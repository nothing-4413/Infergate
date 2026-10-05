package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/embed"
	"github.com/infergate/infergate/internal/mockredis"
)

// ---------------------------------------------------------------------------
// A shared conformance suite
//
// The memory and Redis stores back the same policy, so a behaviour that only
// one of them has is a bug that appears when an operator switches backends.
// Every test below runs against BOTH; the only store-specific test is the one
// about eviction order, because that is the one place they are documented to
// differ (true LRU vs FIFO).
// ---------------------------------------------------------------------------

type storeFactory struct {
	name string
	new  func(t *testing.T) Store
}

func storeFactories() []storeFactory {
	return []storeFactory{
		{"memory", func(t *testing.T) Store {
			s := NewMemoryStore(MemoryOptions{MaxEntriesPerScope: 4, SweepInterval: -1})
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
		{"redis", func(t *testing.T) Store {
			srv := mockredis.New(mockredis.Options{Addr: "127.0.0.1:0"})
			if err := srv.Start(); err != nil {
				t.Fatalf("mockredis.Start: %v", err)
			}
			t.Cleanup(func() { _ = srv.Close() })
			s, err := NewRedisStore(RedisOptions{
				Addr:               srv.Addr(),
				MaxEntriesPerScope: 4,
				DialTimeout:        2 * time.Second,
				ReadTimeout:        2 * time.Second,
			})
			if err != nil {
				t.Fatalf("NewRedisStore: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
	}
}

func entry(scope, key, prompt string, dims int, createdAt time.Time, ttl time.Duration) Entry {
	emb := embed.NewHashingEmbedder(dims)
	vecs, _ := emb.Embed(context.Background(), []string{prompt})
	e := Entry{
		Scope:     scope,
		Key:       key,
		Model:     "mock-gpt",
		Prompt:    prompt,
		Body:      []byte(`{"id":"chatcmpl-1"}`),
		Status:    200,
		Upstream:  "primary",
		CreatedAt: createdAt,
		ExpiresAt: createdAt.Add(ttl),
		Headers:   map[string]string{"content-type": "application/json"},
	}
	if len(vecs) > 0 {
		e.Vector = vecs[0]
	}
	return e
}

func TestStoreConformance(t *testing.T) {
	ctx := context.Background()
	for _, f := range storeFactories() {
		t.Run(f.name, func(t *testing.T) {
			t.Run("get put delete", func(t *testing.T) {
				s := f.new(t)
				now := time.Now()
				e := entry("scope-a", "k1", "how do I reset a password", 128, now, time.Hour)
				if err := s.Put(ctx, e, time.Hour); err != nil {
					t.Fatalf("Put: %v", err)
				}
				got, ok, err := s.Get(ctx, "scope-a", "k1")
				if err != nil || !ok {
					t.Fatalf("Get: ok=%v err=%v", ok, err)
				}
				if string(got.Body) != string(e.Body) || got.Status != 200 || got.Prompt != e.Prompt {
					t.Fatalf("round-tripped entry lost data: %+v", got)
				}
				if got.Upstream != "primary" || got.Headers["content-type"] != "application/json" {
					t.Fatalf("replay metadata lost: upstream=%q headers=%v", got.Upstream, got.Headers)
				}
				// The vector must survive, otherwise the Redis backend would
				// silently degrade to exact-match only and the measured hit rate
				// would depend on which store was configured.
				if len(got.Vector) != 128 {
					t.Fatalf("vector length %d, want 128", len(got.Vector))
				}
				if _, ok, _ := s.Get(ctx, "scope-a", "nope"); ok {
					t.Fatal("Get of an unknown key must miss")
				}
				// Scopes are the isolation boundary: the same key in another
				// tenant scope must not be visible.
				if _, ok, _ := s.Get(ctx, "scope-b", "k1"); ok {
					t.Fatal("an entry leaked across scopes")
				}
				removed, err := s.Delete(ctx, "scope-a", "k1")
				if err != nil || !removed {
					t.Fatalf("Delete: removed=%v err=%v", removed, err)
				}
				if _, ok, _ := s.Get(ctx, "scope-a", "k1"); ok {
					t.Fatal("the entry survived Delete")
				}
				if removed, _ := s.Delete(ctx, "scope-a", "k1"); removed {
					t.Fatal("deleting a missing key must report false")
				}
			})

			t.Run("expiry", func(t *testing.T) {
				s := f.new(t)
				now := time.Now()
				expired := entry("s", "old", "an old prompt that should be gone", 128, now.Add(-2*time.Hour), time.Hour)
				if err := s.Put(ctx, expired, time.Hour); err != nil {
					t.Fatalf("Put: %v", err)
				}
				if _, ok, err := s.Get(ctx, "s", "old"); ok || err != nil {
					t.Fatalf("an expired entry must miss: ok=%v err=%v", ok, err)
				}
				// Search must also skip it; a semantic hit on a stale answer is
				// the same defect as an exact hit on one.
				vec := expired.Vector
				matches, err := s.Search(ctx, "s", vec, 0.1, 10)
				if err != nil {
					t.Fatalf("Search: %v", err)
				}
				if len(matches) != 0 {
					t.Fatalf("Search returned %d expired entries", len(matches))
				}
			})

			t.Run("search ranks and gates", func(t *testing.T) {
				s := f.new(t)
				now := time.Now()
				prompts := []string{
					"how do I reset my password",
					"how can I reset my password",
					"what is the capital of France",
				}
				emb := embed.NewHashingEmbedder(512)
				vecs, _ := emb.Embed(ctx, prompts)
				for i, p := range prompts {
					e := entry("s", fmt.Sprintf("k%d", i), p, 512, now, time.Hour)
					e.Vector = vecs[i]
					if err := s.Put(ctx, e, time.Hour); err != nil {
						t.Fatalf("Put: %v", err)
					}
				}
				// The paraphrase must rank above the unrelated prompt.
				matches, err := s.Search(ctx, "s", vecs[0], 0.5, 10)
				if err != nil {
					t.Fatalf("Search: %v", err)
				}
				if len(matches) == 0 {
					t.Fatal("Search found nothing above 0.5 for an exact self-match")
				}
				if matches[0].Entry.Key != "k0" {
					t.Fatalf("best match = %q (similarity %.3f), want k0", matches[0].Entry.Key, matches[0].Similarity)
				}
				if matches[0].Similarity < 0.999 {
					t.Fatalf("a prompt should match itself at ~1.0, got %.4f", matches[0].Similarity)
				}
				// A high threshold must exclude the paraphrase that a low one
				// accepts: this is the knob the M2 measurement sweeps.
				loose, _ := s.Search(ctx, "s", vecs[0], 0.5, 10)
				tight, _ := s.Search(ctx, "s", vecs[0], 0.999, 10)
				if len(tight) > len(loose) {
					t.Fatal("raising the threshold admitted more entries")
				}
				// The limit must be honoured.
				limited, _ := s.Search(ctx, "s", vecs[0], 0.0, 2)
				if len(limited) != 2 {
					t.Fatalf("limit 2 returned %d entries", len(limited))
				}
				// A zero vector can never match: it has no direction.
				zero, err := s.Search(ctx, "s", make([]float32, 512), 0.0, 10)
				if err != nil {
					t.Fatalf("Search with a zero vector: %v", err)
				}
				if len(zero) != 0 {
					t.Fatalf("a zero vector matched %d entries", len(zero))
				}
			})

			t.Run("eviction beyond the bound", func(t *testing.T) {
				s := f.new(t) // bound is 4
				now := time.Now()
				for i := 0; i < 7; i++ {
					e := entry("s", fmt.Sprintf("k%d", i), fmt.Sprintf("prompt number %d about caching", i), 128,
						now.Add(time.Duration(i)*time.Second), time.Hour)
					if err := s.Put(ctx, e, time.Hour); err != nil {
						t.Fatalf("Put %d: %v", i, err)
					}
				}
				n, err := s.Len(ctx, "s")
				if err != nil {
					t.Fatalf("Len: %v", err)
				}
				if n != 4 {
					t.Fatalf("Len = %d after 7 puts into a bound of 4", n)
				}
				// The NEWEST entries must survive in both backends: evicting the
				// entry just written would make a cache that never warms up.
				for _, key := range []string{"k3", "k4", "k5", "k6"} {
					if _, ok, _ := s.Get(ctx, "s", key); !ok {
						t.Fatalf("%s was evicted; the newest entries must survive", key)
					}
				}
				if s.Name() == "memory" {
					// True LRU: touching k0 must protect it from the next insert.
					if _, ok, _ := s.Get(ctx, "s", "k0"); ok {
						t.Fatal("k0 was already evicted before the touch")
					}
				}
			})

			t.Run("flush", func(t *testing.T) {
				s := f.new(t)
				now := time.Now()
				for _, scope := range []string{"a", "b"} {
					for i := 0; i < 2; i++ {
						e := entry(scope, fmt.Sprintf("k%d", i), fmt.Sprintf("prompt %d in %s about flushing", i, scope), 128, now, time.Hour)
						if err := s.Put(ctx, e, time.Hour); err != nil {
							t.Fatalf("Put: %v", err)
						}
					}
				}
				n, err := s.Flush(ctx, "a")
				if err != nil {
					t.Fatalf("Flush: %v", err)
				}
				if n != 2 {
					t.Fatalf("Flush(a) removed %d entries, want 2", n)
				}
				if got, _ := s.Len(ctx, "a"); got != 0 {
					t.Fatalf("scope a still has %d entries", got)
				}
				if got, _ := s.Len(ctx, "b"); got != 2 {
					t.Fatalf("flushing one scope removed %d entries from another", 2-got)
				}
				if n, err := s.Flush(ctx, ""); err != nil || n != 2 {
					t.Fatalf("Flush(all) removed %d entries, err=%v, want 2", n, err)
				}
				if got, _ := s.Len(ctx, ""); got != 0 {
					t.Fatalf("Flush(all) left %d entries", got)
				}
			})

			t.Run("concurrent use", func(t *testing.T) {
				s := f.new(t)
				now := time.Now()
				var wg sync.WaitGroup
				var failures atomic.Int64
				for w := 0; w < 8; w++ {
					wg.Add(1)
					go func(w int) {
						defer wg.Done()
						for i := 0; i < 20; i++ {
							key := fmt.Sprintf("w%d-%d", w, i)
							e := entry("scope", key, fmt.Sprintf("worker %d prompt %d about concurrency", w, i), 128, now, time.Hour)
							if err := s.Put(ctx, e, time.Hour); err != nil {
								failures.Add(1)
								return
							}
							if _, ok, err := s.Get(ctx, "scope", key); err != nil {
								failures.Add(1)
								return
							} else if !ok {
								// Legitimate: a concurrent writer may have evicted it.
								continue
							}
							if _, err := s.Search(ctx, "scope", e.Vector, 0.9, 5); err != nil {
								failures.Add(1)
								return
							}
						}
					}(w)
				}
				wg.Wait()
				if n := failures.Load(); n != 0 {
					t.Fatalf("%d concurrent operations failed", n)
				}
			})
		})
	}
}

// The one documented divergence: the memory store is LRU, the Redis store is
// FIFO by creation time. Pinning both makes the difference a decision rather
// than a surprise.
func TestMemoryStoreIsLRU(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(MemoryOptions{MaxEntriesPerScope: 3, SweepInterval: -1})
	defer func() { _ = s.Close() }()
	now := time.Now()
	for i := 0; i < 3; i++ {
		e := entry("s", fmt.Sprintf("k%d", i), fmt.Sprintf("prompt %d about eviction order", i), 64, now.Add(time.Duration(i)*time.Second), time.Hour)
		if err := s.Put(ctx, e, time.Hour); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Touch the oldest so it becomes the most recently used, then insert one.
	if _, ok, _ := s.Get(ctx, "s", "k0"); !ok {
		t.Fatal("k0 missing before the touch test")
	}
	e := entry("s", "k3", "a new prompt about eviction order", 64, now.Add(9*time.Second), time.Hour)
	if err := s.Put(ctx, e, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok, _ := s.Get(ctx, "s", "k0"); !ok {
		t.Fatal("the most recently used entry was evicted; eviction is not LRU")
	}
	if _, ok, _ := s.Get(ctx, "s", "k1"); ok {
		t.Fatal("the least recently used entry survived; eviction is not LRU")
	}
}

func TestRedisStoreIsFIFOAndDocumentedSo(t *testing.T) {
	ctx := context.Background()
	srv := mockredis.New(mockredis.Options{Addr: "127.0.0.1:0"})
	if err := srv.Start(); err != nil {
		t.Fatalf("mockredis.Start: %v", err)
	}
	defer func() { _ = srv.Close() }()
	s, err := NewRedisStore(RedisOptions{Addr: srv.Addr(), MaxEntriesPerScope: 3, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	now := time.Now()
	for i := 0; i < 3; i++ {
		e := entry("s", fmt.Sprintf("k%d", i), fmt.Sprintf("prompt %d about redis eviction", i), 64, now.Add(time.Duration(i)*time.Second), time.Hour)
		if err := s.Put(ctx, e, time.Hour); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Touch the oldest. Redis cannot report read order, so this must NOT save it.
	if _, ok, _ := s.Get(ctx, "s", "k0"); !ok {
		t.Fatal("k0 missing before the touch test")
	}
	e := entry("s", "k3", "a new prompt about redis eviction", 64, now.Add(9*time.Second), time.Hour)
	if err := s.Put(ctx, e, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok, _ := s.Get(ctx, "s", "k0"); ok {
		t.Fatal("the touched entry survived; the Redis store is documented as FIFO, not LRU")
	}
}

// A Redis outage must be a miss, never a failed request: the cache is an
// optimisation and the gateway has to keep serving.
func TestRedisStoreDegradesWhenTheServerGoesAway(t *testing.T) {
	ctx := context.Background()
	srv := mockredis.New(mockredis.Options{Addr: "127.0.0.1:0"})
	if err := srv.Start(); err != nil {
		t.Fatalf("mockredis.Start: %v", err)
	}
	s, err := NewRedisStore(RedisOptions{Addr: srv.Addr(), DialTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, err := s.Get(ctx, "s", "k"); err == nil {
		t.Fatal("Get against a dead Redis must return an error for the caller to log")
	}
	if err := s.Put(ctx, entry("s", "k", "a prompt about degradations", 64, time.Now(), time.Hour), time.Hour); err == nil {
		t.Fatal("Put against a dead Redis must return an error")
	}
	if _, err := s.Len(ctx, "s"); err == nil {
		t.Fatal("Len against a dead Redis must return an error")
	}
	if s.Stats().Errors == 0 {
		t.Fatal("the error counter must record the outage")
	}
}

func TestNewRedisStoreRejectsAnEmptyAddress(t *testing.T) {
	if _, err := NewRedisStore(RedisOptions{}); err == nil {
		t.Fatal("an empty address must fail at construction, not silently disable the cache")
	}
}

func TestNewRedisStoreFailsOnAnUnreachableAddress(t *testing.T) {
	if _, err := NewRedisStore(RedisOptions{Addr: "127.0.0.1:1", DialTimeout: 300 * time.Millisecond, ReadTimeout: 300 * time.Millisecond}); err == nil {
		t.Fatal("an unreachable address must fail at startup with a clear error")
	}
}

// ---------------------------------------------------------------------------
// Policy and orchestration
// ---------------------------------------------------------------------------

func newTestCache(t *testing.T, mutate ...func(*Config)) (*Cache, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore(MemoryOptions{MaxEntriesPerScope: 64, SweepInterval: -1})
	t.Cleanup(func() { _ = store.Close() })
	cfg := Config{Enabled: true, Threshold: DefaultThreshold, TTL: time.Minute, MaxEntriesPerScope: 64}
	for _, m := range mutate {
		m(&cfg)
	}
	emb := embed.NewHashingEmbedder(512)
	return New(cfg, store, emb, nil), store
}

func TestCacheablePolicy(t *testing.T) {
	c, _ := newTestCache(t)
	long := strings.Repeat("how do I reset a password ", 3)

	cases := []struct {
		name       string
		req        Request
		wantLookup bool
		wantSem    bool
		wantReason string
	}{
		{"plain request", Request{Scope: "s", Prompt: long, ExactKey: "k"}, true, true, ""},
		{"short prompt", Request{Scope: "s", Prompt: "hi", ExactKey: "k"}, false, false, "prompt shorter than 12 characters"},
		{"empty prompt", Request{Scope: "s", Prompt: "   ", ExactKey: "k"}, false, false, "empty prompt"},
		{"no identity", Request{Scope: "s", Prompt: long}, false, false, "request has no canonical identity"},
		{"temperature > 0", Request{Scope: "s", Prompt: long, ExactKey: "k", Nondeterministic: true}, true, false, "temperature > 0: exact match only"},
		{"tools", Request{Scope: "s", Prompt: long, ExactKey: "k", HasTools: true}, true, false, "tools present: exact match only"},
		{"bypass", Request{Scope: "s", Prompt: long, ExactKey: "k", Bypass: true}, false, false, "bypass requested"},
		{"refresh", Request{Scope: "s", Prompt: long, ExactKey: "k", Refresh: true}, false, false, "refresh requested"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := c.Cacheable(tc.req)
			if d.Lookup != tc.wantLookup || d.Semantic != tc.wantSem {
				t.Fatalf("decision = %+v, want lookup=%v semantic=%v", d, tc.wantLookup, tc.wantSem)
			}
			if tc.wantReason != "" && d.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", d.Reason, tc.wantReason)
			}
		})
	}

	// A refresh still stores: that is the point of it.
	if d := c.Cacheable(Request{Scope: "s", Prompt: long, ExactKey: "k", Refresh: true}); !d.Store {
		t.Fatal("a refresh request must still store the fresh answer")
	}
	// A bypass still stores, so a client that knows its answer changed can
	// repair the cache.
	if d := c.Cacheable(Request{Scope: "s", Prompt: long, ExactKey: "k", Bypass: true}); !d.Store {
		t.Fatal("a bypassed request must still store the fresh answer")
	}
	// A disabled cache stores nothing.
	off, _ := newTestCache(t, func(c *Config) { c.Enabled = false })
	if d := off.Cacheable(Request{Scope: "s", Prompt: long, ExactKey: "k"}); d.Lookup || d.Store {
		t.Fatalf("a disabled cache decided %+v", d)
	}
	// The opt-ins work.
	loose, _ := newTestCache(t, func(c *Config) { c.AllowNondeterministic, c.AllowTools = true, true })
	if d := loose.Cacheable(Request{Scope: "s", Prompt: long, ExactKey: "k", Nondeterministic: true, HasTools: true}); !d.Semantic {
		t.Fatalf("with both opt-ins the request should be semantically cacheable, got %+v", d)
	}
}

func TestExactHitAndSemanticHit(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)
	prompt := "summarise the design document"
	req := Request{Scope: "tenant-a/mock-gpt", Model: "mock-gpt", Prompt: prompt, ExactKey: ExactKeyFor("mock-gpt", []byte(prompt))}
	body := []byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"See section 4."}}]}`)

	if res, err := c.Lookup(ctx, req); err != nil || res.Hit {
		t.Fatalf("a cold lookup must miss: %+v err=%v", res, err)
	}
	if err := c.StoreResponse(ctx, req, Entry{Body: body, Status: 200, ContentType: "application/json", Upstream: "primary"}); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}

	// 1. An identical request hits exactly.
	res, err := c.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !res.Hit || res.Kind != KindExact || res.Similarity != 1 {
		t.Fatalf("exact lookup = %+v", res)
	}
	if string(res.Entry.Body) != string(body) {
		t.Fatal("the replayed body differs from the stored one")
	}
	if res.Entry.Upstream != "primary" {
		t.Fatalf("provenance lost: %q", res.Entry.Upstream)
	}
	if res.Age < 0 || res.Age > time.Minute {
		t.Fatalf("age = %v", res.Age)
	}

	// 2. A paraphrase hits semantically, with a DIFFERENT exact key.
	//
	// This pair is the corpus case pp-en-5 from internal/evalset, measured at
	// 0.8819 against the offline embedder: comfortably above the measured
	// default of 0.86, so the test exercises the shipped default rather than a
	// threshold invented for the test. A "harder" paraphrase (say pp-en-1,
	// "how do I reset my password" vs "how can I reset my password", 0.7273)
	// would fail here, and that is a real property of a lexical embedder - see
	// the DefaultThreshold comment - not a bug in this path.
	para := "please summarise the design document"
	res2, err := c.Lookup(ctx, Request{Scope: req.Scope, Model: req.Model, Prompt: para, ExactKey: ExactKeyFor("mock-gpt", []byte(para))})
	if err != nil {
		t.Fatalf("Lookup paraphrase: %v", err)
	}
	if !res2.Hit || res2.Kind != KindSemantic {
		t.Fatalf("paraphrase lookup = %+v (threshold %v)", res2, c.Config().Threshold)
	}
	if res2.Similarity < c.Config().Threshold {
		t.Fatalf("hit below threshold: %.4f", res2.Similarity)
	}
	if res2.Entry.Key != req.ExactKey {
		t.Fatalf("semantic hit returned key %q, want the stored one %q", res2.Entry.Key, req.ExactKey)
	}

	// 3. An unrelated prompt misses even though the cache is warm.
	unrelated := "what is the weather in Beijing tomorrow morning"
	res3, err := c.Lookup(ctx, Request{Scope: req.Scope, Model: req.Model, Prompt: unrelated, ExactKey: ExactKeyFor("mock-gpt", []byte(unrelated))})
	if err != nil {
		t.Fatalf("Lookup unrelated: %v", err)
	}
	if res3.Hit {
		t.Fatalf("an unrelated prompt hit (%s, similarity %.4f)", res3.Kind, res3.Similarity)
	}

	// 4. Scope isolation: the same prompt in another tenant must miss.
	res4, err := c.Lookup(ctx, Request{Scope: "tenant-b/mock-gpt", Model: "mock-gpt", Prompt: para,
		ExactKey: ExactKeyFor("mock-gpt", []byte(para))})
	if err != nil {
		t.Fatalf("Lookup other tenant: %v", err)
	}
	if res4.Hit {
		t.Fatal("a tenant read another tenant's cached answer")
	}

	st := c.Stats()
	if st.Hits != 2 || st.ExactHits != 1 || st.SemanticHits != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if st.Misses < 2 {
		t.Fatalf("misses = %d", st.Misses)
	}
}

func TestNondeterministicRequestsAreNeverSemanticallyMatched(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)
	prompt := "write me a poem about distributed systems"
	req := Request{Scope: "s/gpt", Model: "gpt", Prompt: prompt, ExactKey: ExactKeyFor("gpt", []byte(prompt))}
	if err := c.StoreResponse(ctx, req, Entry{Body: []byte(`{"ok":true}`), Status: 200}); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}
	// A near-identical rewrite with temperature > 0 must NOT be answered by the
	// stored sample: the caller asked for variation.
	para := "write me a poem about distributed systems please"
	res, err := c.Lookup(ctx, Request{Scope: "s/gpt", Model: "gpt", Prompt: para,
		ExactKey: ExactKeyFor("gpt", []byte(para)), Nondeterministic: true})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if res.Hit {
		t.Fatalf("a temperature > 0 request was answered from the cache (%s, %.4f)", res.Kind, res.Similarity)
	}
	// ...but the byte-identical request still hits: replaying the same question
	// with the same seed parameters is what "exact match only" promises.
	res2, err := c.Lookup(ctx, Request{Scope: "s/gpt", Model: "gpt", Prompt: prompt,
		ExactKey: ExactKeyFor("gpt", []byte(prompt)), Nondeterministic: true})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !res2.Hit || res2.Kind != KindExact {
		t.Fatalf("an identical nondeterministic request must still hit exactly: %+v", res2)
	}
}

func TestToolRequestsAreExactMatchOnly(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)
	prompt := "what is the weather in Shanghai right now"
	req := Request{Scope: "s/gpt", Model: "gpt", Prompt: prompt, ExactKey: ExactKeyFor("gpt", []byte(prompt)), HasTools: true}
	if err := c.StoreResponse(ctx, req, Entry{Body: []byte(`{"tool_calls":[]}`), Status: 200}); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}
	para := "what is the weather in Shanghai at the moment"
	res, err := c.Lookup(ctx, Request{Scope: "s/gpt", Model: "gpt", Prompt: para,
		ExactKey: ExactKeyFor("gpt", []byte(para)), HasTools: true})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if res.Hit {
		t.Fatal("a tool-carrying paraphrase was answered from the cache; the tool set can legitimately differ")
	}
}

func TestStoreResponseSkipsUncacheableRequests(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)
	short := Request{Scope: "s", Model: "m", Prompt: "hi", ExactKey: "k"}
	if err := c.StoreResponse(ctx, short, Entry{Body: []byte("x"), Status: 200}); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}
	if n, _ := c.Store().Len(ctx, "s"); n != 0 {
		t.Fatalf("a too-short prompt was stored (%d entries)", n)
	}
}

func TestTTLIsAnchoredAtCreation(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	clock := base
	c, store := newTestCache(t, func(cfg *Config) {
		cfg.TTL = time.Minute
		cfg.Now = func() time.Time { return clock }
	})
	_ = store
	prompt := "explain the token bucket algorithm in detail"
	req := Request{Scope: "s/m", Model: "m", Prompt: prompt, ExactKey: "k1"}
	if err := c.StoreResponse(ctx, req, Entry{Body: []byte(`{"a":1}`), Status: 200}); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}
	clock = base.Add(30 * time.Second)
	if res, _ := c.Lookup(ctx, req); !res.Hit {
		t.Fatal("an entry inside its TTL must hit")
	}
	// A hit must NOT extend the deadline: an old answer stays old however
	// popular it is.
	clock = base.Add(61 * time.Second)
	if res, _ := c.Lookup(ctx, req); res.Hit {
		t.Fatal("an entry past its TTL still hit; the hit refreshed the deadline")
	}
}

func TestDoCollapsesIdenticalConcurrentMisses(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)
	var calls atomic.Int64
	release := make(chan struct{})
	fn := func() (any, error) {
		calls.Add(1)
		<-release
		return "answer", nil
	}
	const workers = 8
	var wg sync.WaitGroup
	results := make([]any, workers)
	collapsed := make([]bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, dup, err := c.Do(ctx, "same-key", fn)
			if err != nil {
				t.Errorf("Do: %v", err)
				return
			}
			results[i], collapsed[i] = v, dup
		}(i)
	}
	// Give the goroutines time to register, then let the winner finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("the upstream function ran %d times for %d identical requests", n, workers)
	}
	dups := 0
	for i := range collapsed {
		if collapsed[i] {
			dups++
		}
		if results[i] != "answer" {
			t.Fatalf("worker %d got %v", i, results[i])
		}
	}
	if dups != workers-1 {
		t.Fatalf("%d workers were collapsed, want %d", dups, workers-1)
	}
	if st := c.Stats(); st.Collapsed != int64(workers-1) {
		t.Fatalf("collapsed counter = %d", st.Collapsed)
	}
	// A different key must not be collapsed into the first.
	if _, dup, err := c.Do(ctx, "other-key", func() (any, error) { return "other", nil }); err != nil || dup {
		t.Fatalf("a distinct key was collapsed: dup=%v err=%v", dup, err)
	}
}

func TestDoPropagatesTheError(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)
	want := errors.New("upstream exploded")
	if _, _, err := c.Do(ctx, "k", func() (any, error) { return nil, want }); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	// After a failure the key must be free again, not permanently poisoned.
	if _, dup, err := c.Do(ctx, "k", func() (any, error) { return "recovered", nil }); err != nil || dup {
		t.Fatalf("a failed call blocked the next one: dup=%v err=%v", dup, err)
	}
}

func TestScopeAndKeyDerivation(t *testing.T) {
	// Determinism across processes is what makes a shared Redis useful: the
	// same tenant+model must produce the same scope in two gateways.
	a := ScopeFor("tenant-1", "gpt-4o", "chat", "tools")
	b := ScopeFor("tenant-1", "gpt-4o", "chat", "tools")
	if a != b {
		t.Fatalf("scope is not deterministic: %q vs %q", a, b)
	}
	if a == ScopeFor("tenant-2", "gpt-4o", "chat", "tools") {
		t.Fatal("two tenants share a scope")
	}
	if a == ScopeFor("tenant-1", "gpt-4o-mini", "chat", "tools") {
		t.Fatal("two models share a scope")
	}
	if a == ScopeFor("tenant-1", "gpt-4o", "chat") {
		t.Fatal("different capability sets share a scope")
	}
	if !strings.HasPrefix(a, "gpt-4o/") {
		t.Fatalf("the scope should lead with the model for readability in redis-cli: %q", a)
	}
	// A model name with characters that would break a Redis key is sanitised.
	weird := ScopeFor("t", "org/model:v1 beta")
	if strings.ContainsAny(weird, ": ") {
		t.Fatalf("scope %q contains characters that need escaping in a key", weird)
	}
	if k1, k2 := ExactKeyFor("m", []byte(`{"a":1}`)), ExactKeyFor("m", []byte(`{"a":1}`)); k1 != k2 {
		t.Fatal("ExactKeyFor is not deterministic")
	}
	if ExactKeyFor("m", []byte(`{"a":1}`)) == ExactKeyFor("m", []byte(`{"a":2}`)) {
		t.Fatal("different bodies share an exact key")
	}
	if ExactKeyFor("m", []byte(`{}`)) == ExactKeyFor("other", []byte(`{}`)) {
		t.Fatal("different models share an exact key")
	}
}

func TestDisabledCacheNeverHitsAndNeverStores(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t, func(cfg *Config) { cfg.Enabled = false })
	req := Request{Scope: "s", Model: "m", Prompt: "a prompt long enough to cache", ExactKey: "k"}
	if err := c.StoreResponse(ctx, req, Entry{Body: []byte("x"), Status: 200}); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}
	if n, _ := c.Store().Len(ctx, "s"); n != 0 {
		t.Fatal("a disabled cache stored an entry")
	}
	res, err := c.Lookup(ctx, req)
	if err != nil || res.Hit {
		t.Fatalf("disabled lookup = %+v err=%v", res, err)
	}
	if res.Reason != "cache disabled" {
		t.Fatalf("reason = %q", res.Reason)
	}
}

func TestNilEmbedderMeansExactMatchOnly(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(MemoryOptions{MaxEntriesPerScope: 8, SweepInterval: -1})
	defer func() { _ = store.Close() }()
	c := New(Config{Enabled: true, TTL: time.Minute}, store, nil, nil)
	prompt := "summarise the architecture decision record"
	req := Request{Scope: "s", Model: "m", Prompt: prompt, ExactKey: "k"}
	if err := c.StoreResponse(ctx, req, Entry{Body: []byte(`{"ok":1}`), Status: 200}); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}
	if res, _ := c.Lookup(ctx, req); !res.Hit || res.Kind != KindExact {
		t.Fatalf("exact hit without an embedder = %+v", res)
	}
	para := Request{Scope: "s", Model: "m", Prompt: "summarise the architecture decision record please", ExactKey: "k2"}
	res, err := c.Lookup(ctx, para)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if res.Hit {
		t.Fatal("without an embedder only exact identity may hit")
	}
	if res.Reason != "no embedder configured" {
		t.Fatalf("reason = %q", res.Reason)
	}
}

// An embedder outage must degrade the cache to exact matching, not fail the
// caller: the cache is an optimisation and an embedding endpoint is a network
// dependency.
func TestEmbedderFailureDegradesToAMiss(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(MemoryOptions{MaxEntriesPerScope: 8, SweepInterval: -1})
	defer func() { _ = store.Close() }()
	c := New(Config{Enabled: true, TTL: time.Minute}, store, failingEmbedder{}, nil)
	res, err := c.Lookup(ctx, Request{Scope: "s", Model: "m", Prompt: "a long enough prompt to embed", ExactKey: "k"})
	if err != nil {
		t.Fatalf("an embedder failure must not surface as an error: %v", err)
	}
	if res.Hit {
		t.Fatal("a hit without a working embedder")
	}
	if !strings.Contains(res.Reason, "embedder failed") {
		t.Fatalf("reason = %q", res.Reason)
	}
	if c.Stats().LookupErrors == 0 {
		t.Fatal("the lookup error counter must record the degradation")
	}
}

type failingEmbedder struct{}

func (failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embedding endpoint is down")
}
func (failingEmbedder) Dims() int    { return 0 }
func (failingEmbedder) Name() string { return "failing" }

func TestThresholdGatesSemanticHits(t *testing.T) {
	ctx := context.Background()
	prompt := "how do I rotate the database credentials safely"
	para := "how can I rotate the database credentials safely"

	for _, tc := range []struct {
		threshold float64
		wantHit   bool
	}{
		{0.5, true},
		{0.999, false},
	} {
		t.Run(fmt.Sprintf("threshold %.3f", tc.threshold), func(t *testing.T) {
			c, _ := newTestCache(t, func(cfg *Config) { cfg.Threshold = tc.threshold })
			req := Request{Scope: "s", Model: "m", Prompt: prompt, ExactKey: ExactKeyFor("m", []byte(prompt))}
			if err := c.StoreResponse(ctx, req, Entry{Body: []byte(`{"ok":1}`), Status: 200}); err != nil {
				t.Fatalf("StoreResponse: %v", err)
			}
			res, err := c.Lookup(ctx, Request{Scope: "s", Model: "m", Prompt: para, ExactKey: ExactKeyFor("m", []byte(para))})
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if res.Hit != tc.wantHit {
				t.Fatalf("hit = %v (similarity %.4f) want %v", res.Hit, res.Similarity, tc.wantHit)
			}
		})
	}
}

func TestStoreResponseSetsDeadlinesAndScopes(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	c, _ := newTestCache(t, func(cfg *Config) {
		cfg.TTL = 2 * time.Minute
		cfg.Now = func() time.Time { return base }
	})
	req := Request{Scope: "tenant/m", Model: "m", Prompt: "a reasonably long prompt for the store", ExactKey: "k"}
	if err := c.StoreResponse(ctx, req, Entry{Body: []byte(`{"ok":1}`), Status: 200}); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}
	got, ok, err := c.Store().Get(ctx, "tenant/m", "k")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Scope != "tenant/m" || got.Key != "k" || got.Model != "m" || got.Prompt != req.Prompt {
		t.Fatalf("entry identity not filled in: %+v", got)
	}
	if !got.CreatedAt.Equal(base) || !got.ExpiresAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("deadlines: created=%v expires=%v", got.CreatedAt, got.ExpiresAt)
	}
	if len(got.Vector) == 0 {
		t.Fatal("the vector was not computed at store time; the entry can never be found semantically")
	}
}

func TestFlushThroughTheCache(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)
	for i := 0; i < 3; i++ {
		req := Request{Scope: "s", Model: "m", Prompt: fmt.Sprintf("prompt number %d about flushing", i), ExactKey: fmt.Sprintf("k%d", i)}
		if err := c.StoreResponse(ctx, req, Entry{Body: []byte(`{}`), Status: 200}); err != nil {
			t.Fatalf("StoreResponse: %v", err)
		}
	}
	n, err := c.Flush(ctx, "s")
	if err != nil || n != 3 {
		t.Fatalf("Flush = %d, %v", n, err)
	}
	if n, _ := c.Store().Len(ctx, "s"); n != 0 {
		t.Fatalf("%d entries survived the flush", n)
	}
}

// A cache whose entries outlive their TTL is a correctness problem, not a
// memory problem: the caller gets an answer to a question it may no longer be
// asking. The janitor is what bounds it without any traffic.
func TestMemoryJanitorDropsExpiredEntries(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(MemoryOptions{MaxEntriesPerScope: 8, SweepInterval: 5 * time.Millisecond})
	defer func() { _ = store.Close() }()
	now := time.Now()
	if err := store.Put(ctx, entry("s", "k", "a prompt that will expire shortly", 64, now.Add(-time.Hour), time.Minute), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n, _ := store.Len(ctx, "s"); n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the janitor never dropped the expired entry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if store.Stats().Expired == 0 {
		t.Fatal("the expired counter was not incremented")
	}
}

func TestMemoryStoreStatsAreCounted(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(MemoryOptions{MaxEntriesPerScope: 8, SweepInterval: -1})
	defer func() { _ = s.Close() }()
	e := entry("s", "k", "a prompt about statistics", 64, time.Now(), time.Hour)
	if err := s.Put(ctx, e, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, _, err := s.Get(ctx, "s", "k"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := s.Search(ctx, "s", e.Vector, 0.5, 1); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, err := s.Delete(ctx, "s", "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Flush(ctx, "s"); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	st := s.Stats()
	if st.Gets != 1 || st.Puts != 1 || st.Searches != 1 || st.Deletes != 1 || st.Flushes != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if st.Scanned == 0 {
		t.Fatal("Scanned was not counted; the scan cost would be invisible")
	}
}
