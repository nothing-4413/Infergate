// Package cache implements the semantic cache: it turns a repeated (or merely
// paraphrased) Agent prompt into a stored answer instead of another paid model
// call.
//
// The design is driven by one asymmetry: a cache MISS is harmless while a WRONG
// HIT is a defect the caller cannot see. An Agent that receives a plausible
// answer to a different question will act on it, so every rule in this package
// prefers "ask the model again" when identity is uncertain:
//
//   - entries are scoped per tenant+model, never shared across them;
//   - a request that asked for randomness (temperature > 0) is not semantically
//     matched at all, because the answer it wants is by definition variable;
//   - a request that carries tools is exact-match only, because two paraphrases
//     can legitimately select different tools;
//   - only successful responses are stored, and error envelopes are never
//     replayed as if they were answers;
//   - the similarity threshold is measured, not guessed (see the M2 section of
//     docs/DESIGN.md for the paraphrase/near-miss sweep).
//
// The package has no HTTP or config dependency: the gateway builds a Request
// from the parsed body and decides how to serve the Entry.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/infergate/infergate/internal/embed"
)

// Defaults. They are values in Config, so an operator can move them per
// deployment; they are constants here so nothing is implicit.
const (
	// DefaultThreshold is the cosine similarity a stored prompt must reach to
	// answer a new one.
	//
	// It is MEASURED, not chosen. `cmd/measure-m2` runs the labelled corpus in
	// internal/evalset (26 hand-labelled pairs: 13 that must hit, 13 that must
	// not) against the offline embedder and sweeps the threshold. At 0.86 that
	// corpus gives a 62% paraphrase hit rate with ZERO wrong answers. The
	// obvious alternative is worse in a way that is easy to miss: 0.84 buys a
	// 77% hit rate, and the three pairs it starts matching are
	// "delete the cache entry for tenant alpha" vs "tenant beta",
	// "write a unit test for the router" vs "the breaker", and
	// "reset my password for the admin console" vs "the user console" - two more
	// hits per thirteen, paid for with a wrong tenant, a wrong package and a
	// wrong console. Misses cost money; wrong answers cost trust.
	//
	// The previous 0.94 came from intuition and scored a 31% hit rate on the
	// same corpus, which is why this constant now cites evidence. The ceiling is
	// a property of the EMBEDDER, not the threshold: five paraphrases in the
	// corpus score BELOW their nearest near-miss, so no threshold separates
	// them. Point cache.embedding at a real model to raise the ceiling, and
	// re-run the sweep to find where the new one is.
	//
	// 0.86 and 0.88 are the whole zero-wrong-answer shelf (both 8/13); the
	// sweep's own recommendation is the 0.88 end, and this constant takes the
	// other edge because the tightest true paraphrase in the corpus measures
	// 0.8819 -- a knife edge, not a margin. 0.90 collapses the hit rate to 6/13.
	DefaultThreshold = 0.86
	// DefaultTTL bounds how stale an answer may be. Agents replay a lot within
	// one conversation and little across days, so the useful window is short.
	DefaultTTL = 15 * time.Minute
	// DefaultMaxEntriesPerScope bounds one tenant+model working set. The store
	// evicts the least recently used entry beyond it.
	DefaultMaxEntriesPerScope = 256
	// DefaultMinPromptChars skips trivial prompts ("hi", "continue"): they are
	// cheap to re-ask and, being short, are the easiest to match wrongly.
	DefaultMinPromptChars = 12
)

// Kind labels how a hit was proven. It is reported to the caller (a header and
// a metric label) so an operator can see whether the semantic path is doing the
// work or the exact path is carrying the cache.
const (
	KindExact    = "exact"
	KindSemantic = "semantic"
)

// ErrDisabled is returned by a Cache that has been switched off, so a caller
// can distinguish "no cache configured" from "cache could not answer".
var ErrDisabled = errors.New("cache: disabled")

// Entry is one cached answer. It is the unit of storage in both backends, so
// the memory and Redis stores cannot drift apart in what they keep.
type Entry struct {
	// Scope names the isolation boundary: tenant and model together. Two
	// requests with different scopes never see each other's entries.
	Scope string `json:"scope"`
	// Key is the exact-match identity: a hash of the canonical request body
	// (model + messages + sampling parameters). Two byte-identical requests
	// share it, and it is also the storage key.
	Key string `json:"key"`
	// Model is the model that produced the answer, recorded because a scope
	// normally covers one model but a fallback may have served another.
	Model string `json:"model"`
	// Prompt is the text that was embedded. Kept for /admin/cache/lookup, which
	// has to explain why two prompts were considered similar.
	Prompt string `json:"prompt"`
	// Signature fingerprints every input that can change the answer other than
	// the prompt itself: temperature, top_p, seed, stop, the tool schema, the
	// response format, and the conversation that preceded the question.
	//
	// A semantic match must agree on this. "Summarise the design document" is a
	// different request at temperature 0 and at temperature 1, and it is a
	// different request with a different tool schema - serving one from the
	// other would be a silent behaviour change that no caller could detect.
	Signature string `json:"signature,omitempty"`
	// Vector is the embedding of Prompt, normalised.
	Vector []float32 `json:"vector,omitempty"`
	// Body is the exact response payload the client would have received,
	// including the OpenAI envelope. It is replayed byte for byte so a cached
	// answer is indistinguishable from a live one.
	Body []byte `json:"body"`
	// Status and ContentType are replayed with the body.
	Status      int               `json:"status"`
	ContentType string            `json:"content_type,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	// Upstream records which backend answered, so a replayed answer can still
	// say where it originally came from (the replay is marked as such).
	Upstream string `json:"upstream,omitempty"`
	// Usage is the token accounting of the original response. Replaying an
	// answer must not replay its cost as a new charge, but the numbers are kept
	// so a cache hit can report the tokens it avoided.
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	// CreatedAt/ExpiresAt drive TTL. ExpiresAt is stored rather than a duration
	// so a Redis entry expires against the same wall clock a memory entry does.
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Hits counts replays of this entry, so an operator can see which prompts
	// actually repeat. It is best-effort: a hit is counted after it is served.
	Hits int64 `json:"hits,omitempty"`
}

// Expired reports whether the entry is past its deadline at now.
func (e Entry) Expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt)
}

// Match is a search result: an entry and how similar its prompt was.
type Match struct {
	Entry      Entry
	Similarity float64
}

// semanticCandidates bounds how many near matches are examined before a
// signature filter is applied. Four is enough to survive a scope holding a few
// different temperatures or tool schemas without turning a lookup into a scan
// of everything the scope holds.
const semanticCandidates = 4

// Store is the persistence interface. Both implementations must be safe for
// concurrent use, and Get/Search must not return expired entries.
//
// Search is intentionally a full scan of one scope rather than an approximate
// nearest-neighbour index: a scope holds hundreds of entries, the vector is
// 512-dimensional, and cosine over a few hundred vectors is tens of
// microseconds. An ANN index would add a dependency and a recall parameter to
// tune before there is evidence it is needed - see docs/DESIGN.md M2.
type Store interface {
	// Get returns the entry stored under an exact key.
	Get(ctx context.Context, scope, key string) (Entry, bool, error)
	// Put stores an entry, replacing any entry with the same key.
	Put(ctx context.Context, e Entry, ttl time.Duration) error
	// Search returns entries in the scope whose similarity to vec is at least
	// threshold, most similar first, at most limit of them.
	Search(ctx context.Context, scope string, vec []float32, threshold float64, limit int) ([]Match, error)
	// Delete removes one entry. It reports whether something was removed.
	Delete(ctx context.Context, scope, key string) (bool, error)
	// Flush removes every entry in a scope, or every scope when scope is empty.
	Flush(ctx context.Context, scope string) (int, error)
	// Len counts entries in a scope (0 means "all scopes" for the memory store;
	// the Redis store reports -1 when it cannot count cheaply).
	Len(ctx context.Context, scope string) (int, error)
	// Stats returns backend counters for /admin/cache.
	Stats() StoreStats
	// Name identifies the backend in logs and admin responses.
	Name() string
	// Close releases resources. It must be idempotent.
	Close() error
}

// StoreStats is the backend's own view of its work.
type StoreStats struct {
	Gets     int64 `json:"gets"`
	Puts     int64 `json:"puts"`
	Deletes  int64 `json:"deletes"`
	Flushes  int64 `json:"flushes"`
	Searches int64 `json:"searches"`
	Scanned  int64 `json:"scanned"` // entries compared during searches
	Evicted  int64 `json:"evicted"` // entries dropped by LRU/FIFO trimming
	Expired  int64 `json:"expired"` // entries dropped because their TTL passed
	Errors   int64 `json:"errors"`  // backend failures (a Redis outage shows here)
}

// Config configures a Cache.
type Config struct {
	// Enabled switches the whole feature. A disabled Cache answers every
	// Lookup with a miss, which is how the gateway runs with no cache: section.
	Enabled bool
	// Threshold is the minimum cosine similarity for a semantic hit.
	Threshold float64
	// TTL is how long an entry stays usable.
	TTL time.Duration
	// MaxEntriesPerScope bounds a single tenant+model working set.
	MaxEntriesPerScope int
	// MinPromptChars skips prompts too short to match safely.
	MinPromptChars int
	// AllowNondeterministic admits temperature > 0 requests to the semantic
	// path. Off by default: replaying one sample of a random request is a
	// behaviour change, not an optimisation.
	AllowNondeterministic bool
	// AllowTools admits tool-carrying requests to semantic matching. Off by
	// default; they are still exact-match cacheable.
	AllowTools bool
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// WithDefaults fills unset fields.
func (c Config) WithDefaults() Config {
	if c.Threshold <= 0 {
		c.Threshold = DefaultThreshold
	}
	if c.TTL <= 0 {
		c.TTL = DefaultTTL
	}
	if c.MaxEntriesPerScope <= 0 {
		c.MaxEntriesPerScope = DefaultMaxEntriesPerScope
	}
	if c.MinPromptChars <= 0 {
		c.MinPromptChars = DefaultMinPromptChars
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Request is what the gateway knows about an inbound call at the point it asks
// the cache. The cache never sees the HTTP request: the gateway has already
// parsed the body and derived the scope, so the policy decisions live in one
// place (this package) and the transport decisions live in the other.
type Request struct {
	// Scope isolates tenants and models. See ScopeFor.
	Scope string
	// Model is the requested model name.
	Model string
	// Prompt is the text to embed. Callers pass the user-visible content only
	// (system + user turns), not the JSON envelope: hashing the envelope would
	// make two semantically identical calls with different key order miss.
	Prompt string
	// ExactKey is the canonical request identity. Two requests that would send
	// the same bytes upstream share it.
	ExactKey string
	// Signature fingerprints the answer-shaping inputs. A semantic match must
	// agree on it; an exact match already does, by construction.
	Signature string
	// HasTools marks a request carrying tool definitions or tool results.
	HasTools bool
	// Nondeterministic marks sampling parameters that ask for variation
	// (temperature > 0, top_p < 1, n > 1).
	Nondeterministic bool
	// Bypass skips reading the cache but still allows storing.
	Bypass bool
	// Refresh reads nothing and replaces the entry after the model answers.
	Refresh bool
}

// Decision explains whether a request may use the cache at all. It is returned
// so the gateway can put the reason in a header and a log field: "why was this
// not cached" is the first question an operator asks.
type Decision struct {
	Lookup bool
	Store  bool
	// Semantic allows similarity matching, not just exact identity.
	Semantic bool
	// Reason is a short machine-readable explanation ("temperature > 0",
	// "prompt too short", "tools present"). Empty means fully cacheable.
	Reason string
}

// Cacheable decides the policy for one request. It is a pure function of the
// request and the config, which makes every rule unit-testable without a store.
func (c *Cache) Cacheable(r Request) Decision {
	cfg := c.cfg
	if !cfg.Enabled {
		return Decision{Reason: "cache disabled"}
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return Decision{Reason: "empty prompt"}
	}
	if len([]rune(r.Prompt)) < cfg.MinPromptChars {
		return Decision{Reason: fmt.Sprintf("prompt shorter than %d characters", cfg.MinPromptChars)}
	}
	if r.ExactKey == "" {
		return Decision{Reason: "request has no canonical identity"}
	}

	// Storing is allowed by default and only ever taken away above: an answer
	// worth returning is worth remembering. Lookup and Store are independent
	// flags, which is what makes "bypass" and "refresh" mean something - a
	// client that knows its answer changed can repair the cache on the way
	// through instead of merely opting out of reading it.
	d := Decision{Lookup: true, Store: true, Semantic: true}
	switch {
	case r.HasTools && !cfg.AllowTools:
		// Tools are cacheable, but only an identical request may hit: two
		// paraphrases of the same question can legitimately choose different
		// tools, and replaying a tool_call the caller did not ask for is worse
		// than paying for the call.
		d.Semantic = false
		d.Reason = "tools present: exact match only"
	case r.Nondeterministic && !cfg.AllowNondeterministic:
		d.Semantic = false
		d.Reason = "temperature > 0: exact match only"
	}
	if r.Bypass {
		// Semantic matching is off as well: a caller that said "do not use the
		// cache" also does not want a paraphrase of someone else's question to
		// answer it while the fresh answer is on its way.
		d.Lookup, d.Semantic = false, false
		d.Reason = "bypass requested"
		return d
	}
	if r.Refresh {
		d.Lookup, d.Semantic = false, false
		d.Reason = "refresh requested"
	}
	return d
}

// Result is a cache answer.
type Result struct {
	// Hit reports whether the cache answered.
	Hit bool
	// Kind is KindExact or KindSemantic when Hit.
	Kind string
	// Similarity is 1.0 for an exact hit, the measured cosine otherwise.
	Similarity float64
	// Entry is the stored answer.
	Entry Entry
	// Age is how long ago the entry was created.
	Age time.Duration
	// Reason explains a miss, matching Decision.Reason where relevant.
	Reason string
}

// Stats counts what the cache did. A gateway reports these as metrics, so the
// effect of the cache is visible next to the traffic it saved.
type Stats struct {
	Lookups        int64
	Hits           int64
	ExactHits      int64
	SemanticHits   int64
	Misses         int64
	Stores         int64
	StoreErrors    int64
	LookupErrors   int64
	StaleEvictions int64
	Collapsed      int64 // requests that waited for an identical in-flight call
	// SavedPromptTokens/SavedCompletionTokens are the provider-reported tokens
	// a hit did NOT have to be generated again. They are counted here and NOT
	// added to the upstream token totals: from the provider's point of view the
	// generation happened once, and counting the replay again would inflate
	// consumption by exactly the amount the cache saved.
	SavedPromptTokens     int64
	SavedCompletionTokens int64
}

// Cache is the semantic cache: a Store plus an Embedder plus policy.
type Cache struct {
	cfg   Config
	store Store
	emb   embed.Embedder
	log   *slog.Logger

	mu    sync.Mutex
	stats Stats
	// inflight collapses identical concurrent misses into one upstream call.
	// Without it a cache warms up worst exactly when it matters most: a burst
	// of first-time requests all miss and all pay.
	inflight map[string]*call
}

type call struct {
	done chan struct{}
	res  any
	err  error
}

// New builds a Cache. A nil embedder means exact-match only, which is a
// legitimate configuration (and the fallback when an embedding endpoint is
// unreachable but the gateway must keep serving).
func New(cfg Config, store Store, emb embed.Embedder, log *slog.Logger) *Cache {
	cfg = cfg.WithDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Cache{cfg: cfg, store: store, emb: emb, log: log, inflight: make(map[string]*call)}
}

// Store exposes the backing store (admin handlers report its stats).
func (c *Cache) Store() Store { return c.store }

// Config returns the effective configuration.
func (c *Cache) Config() Config { return c.cfg }

// Embedder exposes the embedder, which may be nil (an exact-match-only cache).
//
// The admin surface needs it to answer "why did this NOT hit": without it the
// only honest answer is "trust the threshold".
func (c *Cache) Embedder() embed.Embedder { return c.emb }

// Close releases the backing store (a Redis pool, a janitor goroutine). It is
// safe on a nil store and safe to call twice: shutdown may run after a failed
// startup path.
func (c *Cache) Close() error {
	if c == nil || c.store == nil {
		return nil
	}
	return c.store.Close()
}

// Stats returns a snapshot of the counters.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// AddStats merges extra counters (the gateway reports upstream token savings).
func (c *Cache) AddStats(delta Stats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Lookups += delta.Lookups
	c.stats.Hits += delta.Hits
	c.stats.ExactHits += delta.ExactHits
	c.stats.SemanticHits += delta.SemanticHits
	c.stats.Misses += delta.Misses
	c.stats.Stores += delta.Stores
	c.stats.StoreErrors += delta.StoreErrors
	c.stats.LookupErrors += delta.LookupErrors
	c.stats.StaleEvictions += delta.StaleEvictions
	c.stats.Collapsed += delta.Collapsed
}

func (c *Cache) count(f func(*Stats)) {
	c.mu.Lock()
	f(&c.stats)
	c.mu.Unlock()
}

// Lookup answers a request from the cache. A miss is not an error: the caller
// is expected to call the model and then Store.
func (c *Cache) Lookup(ctx context.Context, r Request) (Result, error) {
	d := c.Cacheable(r)
	if !d.Lookup {
		c.count(func(s *Stats) { s.Lookups++; s.Misses++ })
		return Result{Reason: d.Reason}, nil
	}
	c.count(func(s *Stats) { s.Lookups++ })

	now := c.cfg.Now()

	// 1. Exact identity first. It is one hash lookup, it is always correct, and
	//    it is what carries conversational traffic where an Agent re-asks the
	//    same thing verbatim.
	e, ok, err := c.store.Get(ctx, r.Scope, r.ExactKey)
	if err != nil {
		c.count(func(s *Stats) { s.Misses++; s.LookupErrors++ })
		return Result{Reason: "store error: " + err.Error()}, err
	}
	if ok && !e.Expired(now) {
		c.count(func(s *Stats) {
			s.Hits++
			s.ExactHits++
			s.SavedPromptTokens += int64(e.PromptTokens)
			s.SavedCompletionTokens += int64(e.CompletionTokens)
		})
		_ = c.bumpHits(ctx, r.Scope, &e)
		return Result{Hit: true, Kind: KindExact, Similarity: 1, Entry: e, Age: now.Sub(e.CreatedAt)}, nil
	}
	if ok {
		// Expired but still present: drop it so a later Search does not have to
		// re-check it, and so the store's own counters stay honest.
		_, _ = c.store.Delete(ctx, r.Scope, r.ExactKey)
		c.count(func(s *Stats) { s.StaleEvictions++ })
	}

	// 2. Semantic identity. Only for prompts whose policy allows it, and never
	//    for a request that asked for randomness.
	if !d.Semantic || c.emb == nil {
		c.count(func(s *Stats) { s.Misses++ })
		reason := d.Reason
		if reason == "" && c.emb == nil {
			reason = "no embedder configured"
		}
		return Result{Reason: reason}, nil
	}

	vecs, err := c.emb.Embed(ctx, []string{r.Prompt})
	if err != nil || len(vecs) == 0 {
		// An embedding failure degrades the cache to exact-match; it must never
		// fail the request, because the cache is an optimisation.
		c.count(func(s *Stats) { s.Misses++; s.LookupErrors++ })
		reason := "embedder failed"
		if err != nil {
			reason += ": " + err.Error()
		}
		return Result{Reason: reason}, nil
	}
	vec := vecs[0]

	// Ask for a few candidates rather than one. The most similar prompt in the
	// scope may have been asked under a different signature - a different
	// temperature, a different tool schema, a different conversation - and the
	// answer wanted is the most similar prompt that was asked the SAME way, not
	// the most similar prompt, full stop.
	matches, err := c.store.Search(ctx, r.Scope, vec, c.cfg.Threshold, semanticCandidates)
	if err != nil {
		c.count(func(s *Stats) { s.Misses++; s.LookupErrors++ })
		return Result{Reason: "store error: " + err.Error()}, err
	}
	var best Match
	var found, droppedStale bool
	for _, m := range matches {
		if r.Signature != "" && m.Entry.Signature != r.Signature {
			continue
		}
		if m.Entry.Expired(now) {
			if !droppedStale {
				_, _ = c.store.Delete(ctx, r.Scope, m.Entry.Key)
				droppedStale = true
			}
			continue
		}
		best, found = m, true
		break
	}
	if !found {
		c.count(func(s *Stats) {
			s.Misses++
			if droppedStale {
				s.StaleEvictions++
			}
		})
		reason := "no entry above threshold"
		if droppedStale {
			reason = "best match expired"
		}
		return Result{Reason: reason}, nil
	}
	c.count(func(s *Stats) {
		s.Hits++
		s.SemanticHits++
		s.SavedPromptTokens += int64(best.Entry.PromptTokens)
		s.SavedCompletionTokens += int64(best.Entry.CompletionTokens)
	})
	_ = c.bumpHits(ctx, r.Scope, &best.Entry)
	return Result{
		Hit:        true,
		Kind:       KindSemantic,
		Similarity: best.Similarity,
		Entry:      best.Entry,
		Age:        now.Sub(best.Entry.CreatedAt),
	}, nil
}

// bumpHits re-stores an entry with its hit counter incremented. It is
// best-effort: failing to count a hit must not turn a successful hit into an
// error for the caller.
func (c *Cache) bumpHits(ctx context.Context, scope string, e *Entry) error {
	e.Hits++
	ttl := time.Until(e.ExpiresAt)
	if ttl <= 0 {
		ttl = c.cfg.TTL
	}
	return c.store.Put(ctx, *e, ttl)
}

// StoreResponse caches an answer. The caller passes the verdict from the
// gateway (status is 2xx, body is a complete non-streamed answer) and the cache
// re-checks its own policy, so a mis-wired caller cannot store an error or a
// nondeterministic sample.
func (c *Cache) StoreResponse(ctx context.Context, r Request, e Entry) error {
	d := c.Cacheable(r)
	if !d.Store {
		return nil
	}
	now := c.cfg.Now()
	e.Scope = r.Scope
	e.Key = r.ExactKey
	e.Prompt = r.Prompt
	e.Model = r.Model
	e.Signature = r.Signature
	e.CreatedAt = now
	// The deadline is anchored at creation, not refreshed on hit: an answer
	// that is old is old regardless of how popular it is.
	e.ExpiresAt = now.Add(c.cfg.TTL)

	// The vector is computed at store time, not at lookup time: a cache that
	// stores an answer without its vector can never be found semantically, and
	// hashing the prompt on every store is cheaper than embedding on every miss
	// (a miss repeats; the store happens once).
	if e.Vector == nil && c.emb != nil && strings.TrimSpace(r.Prompt) != "" {
		if vecs, err := c.emb.Embed(ctx, []string{r.Prompt}); err == nil && len(vecs) > 0 {
			e.Vector = vecs[0]
		} else if err != nil {
			c.log.Debug("cache: embedding for store failed, storing exact-match only",
				slog.String("error", err.Error()))
		}
	}

	if err := c.store.Put(ctx, e, c.cfg.TTL); err != nil {
		c.count(func(s *Stats) { s.StoreErrors++ })
		return err
	}
	c.count(func(s *Stats) { s.Stores++ })
	return nil
}

// Flush empties a scope, or every scope when scope is empty.
func (c *Cache) Flush(ctx context.Context, scope string) (int, error) {
	n, err := c.store.Flush(ctx, scope)
	if err == nil {
		c.count(func(s *Stats) {})
	}
	return n, err
}

// Do collapses concurrent identical misses. The gateway calls it around the
// upstream call for a cacheable request:
//
//	v, err := c.Do(ctx, req.ExactKey, func() (any, error) { return callUpstream() })
//
// The first caller runs fn; the others wait and receive its result, so a burst
// of N identical requests costs one upstream call. It is deliberately keyed on
// the EXACT identity only: collapsing paraphrases would need the answer to be
// similar, and returning one caller's answer to a different question is the
// defect this package refuses to introduce.
func (c *Cache) Do(ctx context.Context, key string, fn func() (any, error)) (any, bool, error) {
	if key == "" || !c.cfg.Enabled {
		v, err := fn()
		return v, false, err
	}
	c.mu.Lock()
	cl, exists := c.inflight[key]
	if !exists {
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
	}
	c.mu.Unlock()

	if exists {
		select {
		case <-cl.done:
			c.count(func(s *Stats) { s.Collapsed++ })
			return cl.res, true, cl.err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}

	cl.res, cl.err = fn()
	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()
	close(cl.done)
	return cl.res, false, cl.err
}

// ScopeFor builds the isolation key. The tenant comes from the caller's own
// credential (never from a client-supplied header, which would let one tenant
// read another's answers), and the model is part because the same prompt must
// not be answered by a different model's text.
func ScopeFor(tenant, model string, capabilities ...string) string {
	h := sha256.New()
	h.Write([]byte(tenant))
	h.Write([]byte{0})
	h.Write([]byte(model))
	for _, cap := range capabilities {
		h.Write([]byte{0})
		h.Write([]byte(cap))
	}
	// A short prefix keeps Redis keys greppable in redis-cli while the hash
	// keeps them bounded and free of characters that would need escaping.
	sum := hex.EncodeToString(h.Sum(nil))[:16]
	return fmt.Sprintf("%s/%s", sanitiseModel(model), sum)
}

func sanitiseModel(model string) string {
	if model == "" {
		return "any"
	}
	var b strings.Builder
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "any"
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// ExactKeyFor hashes the canonical request identity. Two requests that would
// send identical bytes upstream must produce identical keys, and two that
// differ in any way that could change the answer must not.
func ExactKeyFor(model string, canonicalBody []byte, extra ...string) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write(canonicalBody)
	for _, e := range extra {
		h.Write([]byte{0})
		h.Write([]byte(e))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Rank sorts matches by similarity, most similar first, breaking ties by
// recency so the deterministic answer wins over the nondeterministic one.
func Rank(matches []Match) {
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Similarity != matches[j].Similarity {
			return matches[i].Similarity > matches[j].Similarity
		}
		return matches[i].Entry.CreatedAt.After(matches[j].Entry.CreatedAt)
	})
}
