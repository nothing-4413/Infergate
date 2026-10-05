package idempotency

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// newTestStore builds a store with a controllable clock and no janitor.
func newTestStore(t *testing.T, opts Options) (*Store, func(time.Duration)) {
	t.Helper()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	clock := struct {
		mu sync.Mutex
		at time.Time
	}{at: now}
	opts.Now = func() time.Time {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		return clock.at
	}
	if opts.SweepInterval == 0 {
		opts.SweepInterval = -1
	}
	s := New(opts)
	t.Cleanup(func() { s.Close() })
	advance := func(d time.Duration) {
		clock.mu.Lock()
		clock.at = clock.at.Add(d)
		clock.mu.Unlock()
	}
	return s, advance
}

func TestBeginClaimsThenReplaysTheSameResponse(t *testing.T) {
	s, _ := newTestStore(t, Options{Capacity: 8, TTL: time.Minute})

	if d, _ := s.Begin("acme", "k1", "hash-a"); d != Proceed {
		t.Fatalf("first Begin = %v, want Proceed", d)
	}
	if got := s.InFlight(); got != 1 {
		t.Fatalf("InFlight after claim = %d, want 1", got)
	}
	stored := Entry{
		Status:    200,
		Headers:   map[string][]string{"Content-Type": {"application/json"}, "X-InferGate-Upstream-Name": {"primary"}},
		Body:      []byte(`{"answer":"42"}`),
		RequestID: "req-original",
		Upstream:  "primary",
		Model:     "mock-gpt",
		Outcome:   "success",
	}
	if ok, reason := s.Complete("acme", "k1", stored); !ok {
		t.Fatalf("Complete = false (%s), want stored", reason)
	}

	d, e := s.Begin("acme", "k1", "hash-a")
	if d != Replay {
		t.Fatalf("second Begin = %v, want Replay", d)
	}
	if e.Status != 200 || string(e.Body) != `{"answer":"42"}` || e.RequestID != "req-original" {
		t.Fatalf("replay entry = %+v", e)
	}
	if got := e.Headers["X-InferGate-Upstream-Name"]; len(got) != 1 || got[0] != "primary" {
		t.Fatalf("replay headers = %v", e.Headers)
	}
	if e.CompletedAt.IsZero() {
		t.Fatal("CompletedAt was not stamped")
	}

	// The store hands out copies: a caller that rewrites the body it received
	// must not corrupt what the next replay sees.
	e.Body[0] = 'X'
	e.Headers["Content-Type"][0] = "text/plain"
	_, again := s.Begin("acme", "k1", "hash-a")
	if string(again.Body) != `{"answer":"42"}` {
		t.Fatalf("stored body was mutated through the returned entry: %q", again.Body)
	}
	if again.Headers["Content-Type"][0] != "application/json" {
		t.Fatalf("stored headers were mutated through the returned entry: %v", again.Headers)
	}

	st := s.Stats()
	if st.Lookups != 3 || st.Hits != 2 || st.Misses != 1 || st.Stored != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestDifferentBodyUnderTheSameKeyIsAConflict(t *testing.T) {
	s, _ := newTestStore(t, Options{Capacity: 8, TTL: time.Minute})
	s.Begin("acme", "k1", "hash-a")
	s.Complete("acme", "k1", Entry{Status: 200, Body: []byte("a")})

	d, e := s.Begin("acme", "k1", "hash-b")
	if d != ConflictBody {
		t.Fatalf("Begin with a different body = %v, want ConflictBody", d)
	}
	// The conflicting entry is returned so the error can name what it holds.
	if e.RequestHash != "hash-a" {
		t.Fatalf("conflict entry = %+v", e)
	}
	if got := s.Stats().Conflicts; got != 1 {
		t.Fatalf("Conflicts = %d, want 1", got)
	}
}

func TestConcurrentDuplicateIsRefusedWhileTheFirstRuns(t *testing.T) {
	s, _ := newTestStore(t, Options{Capacity: 8, TTL: time.Minute})
	s.Begin("acme", "k1", "hash-a")

	if d, _ := s.Begin("acme", "k1", "hash-a"); d != ConflictInFlight {
		t.Fatalf("Begin while in flight = %v, want ConflictInFlight", d)
	}
	if got := s.Stats().InFlightRejects; got != 1 {
		t.Fatalf("InFlightRejects stat = %d, want 1", got)
	}
}

func TestAbortReleasesTheClaim(t *testing.T) {
	s, _ := newTestStore(t, Options{Capacity: 8, TTL: time.Minute})
	s.Begin("acme", "k1", "hash-a")
	s.Abort("acme", "k1")

	if d, _ := s.Begin("acme", "k1", "hash-a"); d != Proceed {
		t.Fatalf("Begin after Abort = %v, want Proceed", d)
	}
	// Aborting a completed entry is a no-op: a stored answer must not be lost
	// because a late caller decided it was not going to store one.
	s.Complete("acme", "k1", Entry{Status: 200, Body: []byte("a")})
	s.Abort("acme", "k1")
	if d, _ := s.Begin("acme", "k1", "hash-a"); d != Replay {
		t.Fatalf("Begin after Abort-of-completed = %v, want Replay", d)
	}
}

func TestExpiryMakesTheKeyUsableAgain(t *testing.T) {
	s, advance := newTestStore(t, Options{Capacity: 8, TTL: 30 * time.Second})
	s.Begin("acme", "k1", "hash-a")
	s.Complete("acme", "k1", Entry{Status: 200, Body: []byte("a")})

	advance(29 * time.Second)
	if d, _ := s.Begin("acme", "k1", "hash-a"); d != Replay {
		t.Fatalf("Begin before TTL = %v, want Replay", d)
	}
	advance(2 * time.Second)
	if d, _ := s.Begin("acme", "k1", "hash-a"); d != Proceed {
		t.Fatalf("Begin after TTL = %v, want Proceed", d)
	}
	if got := s.Stats().Expired; got != 1 {
		t.Fatalf("Expired = %d, want 1", got)
	}
	// The key is claimed again (in flight), but nothing is stored yet: Len
	// counts replayable answers, and an in-flight request has none.
	if got := s.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0 (the old entry expired and the new key is only claimed)", got)
	}
	if got := s.InFlight(); got != 1 {
		t.Fatalf("InFlight = %d, want 1", got)
	}
}

func TestClaimIsNotSweptWhileInFlight(t *testing.T) {
	s, advance := newTestStore(t, Options{Capacity: 8, TTL: time.Second})
	s.Begin("acme", "k1", "hash-a")
	advance(time.Hour)
	s.sweep()

	if got := s.InFlight(); got != 1 {
		t.Fatalf("InFlight after sweep = %d, want 1: a running request must keep its claim", got)
	}
}

func TestLRUEvictsTheLeastRecentlyUsedKey(t *testing.T) {
	s, _ := newTestStore(t, Options{Capacity: 2, TTL: time.Minute})
	for _, k := range []string{"k1", "k2"} {
		s.Begin("acme", k, "h")
		s.Complete("acme", k, Entry{Status: 200, Body: []byte(k)})
	}
	// Touch k1 so k2 becomes the least recently used.
	if d, _ := s.Begin("acme", "k1", "h"); d != Replay {
		t.Fatalf("touch k1 = %v, want Replay", d)
	}
	s.Begin("acme", "k3", "h")
	s.Complete("acme", "k3", Entry{Status: 200, Body: []byte("k3")})

	if _, ok := s.Get("acme", "k2"); ok {
		t.Fatal("k2 should have been evicted")
	}
	for _, k := range []string{"k1", "k3"} {
		if _, ok := s.Get("acme", k); !ok {
			t.Fatalf("%s should still be stored", k)
		}
	}
	if got := s.Stats().Evicted; got != 1 {
		t.Fatalf("Evicted = %d, want 1", got)
	}
}

func TestOversizeResponseIsServedButNotRemembered(t *testing.T) {
	s, _ := newTestStore(t, Options{Capacity: 8, TTL: time.Minute, MaxResponseBytes: 8})
	s.Begin("acme", "k1", "hash-a")
	ok, reason := s.Complete("acme", "k1", Entry{Status: 200, Body: bytes.Repeat([]byte("x"), 9)})
	if ok || reason != "oversize" {
		t.Fatalf("Complete = (%v, %q), want (false, oversize)", ok, reason)
	}
	if got := s.InFlight(); got != 0 {
		t.Fatalf("InFlight after oversize = %d, want 0: the claim must be released", got)
	}
	if d, _ := s.Begin("acme", "k1", "hash-a"); d != Proceed {
		t.Fatalf("Begin after oversize = %v, want Proceed", d)
	}
	if got := s.Stats().Oversize; got != 1 {
		t.Fatalf("Oversize = %d, want 1", got)
	}
}

func TestScopesAreIsolated(t *testing.T) {
	s, _ := newTestStore(t, Options{Capacity: 8, TTL: time.Minute})
	s.Begin("acme", "k1", "hash-a")
	s.Complete("acme", "k1", Entry{Status: 200, Body: []byte("acme")})

	if d, _ := s.Begin("globex", "k1", "hash-b"); d != Proceed {
		t.Fatalf("same key in another tenant = %v, want Proceed", d)
	}
	e, ok := s.Get("acme", "k1")
	if !ok || string(e.Body) != "acme" {
		t.Fatalf("acme entry = (%q, %v)", e.Body, ok)
	}
	if got := s.Scopes(); len(got) != 2 {
		t.Fatalf("Scopes = %v, want two tenants", got)
	}
}

func TestFlushAndLen(t *testing.T) {
	s, _ := newTestStore(t, Options{Capacity: 8, TTL: time.Minute})
	for _, spec := range []struct{ scope, key string }{{"acme", "k1"}, {"acme", "k2"}, {"globex", "k1"}} {
		s.Begin(spec.scope, spec.key, "h")
		s.Complete(spec.scope, spec.key, Entry{Status: 200, Body: []byte(spec.key)})
	}
	if got := s.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	if got := s.Flush("acme"); got != 2 {
		t.Fatalf("Flush(acme) = %d, want 2", got)
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("Len after flush = %d, want 1", got)
	}
	if got := s.Flush(""); got != 1 {
		t.Fatalf("Flush(all) = %d, want 1", got)
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("Len after flush-all = %d, want 0", got)
	}
}

func TestListIsNewestFirst(t *testing.T) {
	s, advance := newTestStore(t, Options{Capacity: 8, TTL: time.Hour})
	s.Begin("acme", "old", "h")
	s.Complete("acme", "old", Entry{Status: 200, Body: []byte("old")})
	advance(time.Minute)
	s.Begin("globex", "new", "h")
	s.Complete("globex", "new", Entry{Status: 201, Body: []byte("new")})

	list := s.List(0)
	if len(list) != 2 || list[0].Key != "new" || list[1].Key != "old" {
		t.Fatalf("List = %+v", list)
	}
	if got := s.List(1); len(got) != 1 || got[0].Key != "new" {
		t.Fatalf("List(1) = %+v", got)
	}
	// An in-flight claim is not a replayable entry.
	s.Begin("acme", "pending", "h")
	if got := len(s.List(0)); got != 2 {
		t.Fatalf("List with a pending claim = %d entries, want 2", got)
	}
}

func TestExactlyOneConcurrentBeginProceeds(t *testing.T) {
	s, _ := newTestStore(t, Options{Capacity: 8, TTL: time.Minute})
	const workers = 16
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		proceeds int
		inflight int
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, _ := s.Begin("acme", "k1", "hash-a")
			mu.Lock()
			defer mu.Unlock()
			switch d {
			case Proceed:
				proceeds++
			case ConflictInFlight:
				inflight++
			}
		}()
	}
	wg.Wait()
	if proceeds != 1 {
		t.Fatalf("Proceed counted %d times, want exactly 1", proceeds)
	}
	if inflight != workers-1 {
		t.Fatalf("ConflictInFlight counted %d times, want %d", inflight, workers-1)
	}
}

func TestKeyOfTrimsWhitespace(t *testing.T) {
	if got := KeyOf("  abc  "); got != "abc" {
		t.Fatalf("KeyOf = %q", got)
	}
	// Keys stay case sensitive: two callers that distinguish K1 from k1 mean it.
	if KeyOf("K1") == KeyOf("k1") {
		t.Fatal("KeyOf must not fold case")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	s := New(Options{SweepInterval: -1})
	defer s.Close()
	if s.Capacity() != DefaultCapacity || s.TTL() != DefaultTTL || s.MaxResponseBytes() != DefaultMaxResponseBytes {
		t.Fatalf("defaults = %d / %s / %d", s.Capacity(), s.TTL(), s.MaxResponseBytes())
	}
}

func TestDecisionStrings(t *testing.T) {
	for d, want := range map[Decision]string{
		Proceed:          "proceed",
		Replay:           "replay",
		ConflictBody:     "conflict_body",
		ConflictInFlight: "in_flight",
	} {
		if got := d.String(); got != want {
			t.Fatalf("Decision(%d).String() = %q, want %q", d, got, want)
		}
	}
	if got := Decision(99).String(); got != "unknown" {
		t.Fatalf("unknown Decision = %q", got)
	}
}

func TestAgeAndExpired(t *testing.T) {
	now := time.Now()
	e := Entry{CompletedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute)}
	if got := e.Age(now); got != time.Second {
		t.Fatalf("Age = %s", got)
	}
	if e.Expired(now) {
		t.Fatal("entry should not be expired")
	}
	e.ExpiresAt = now
	if !e.Expired(now) {
		t.Fatal("an entry whose TTL has arrived is expired")
	}
	if (Entry{}).Expired(now) {
		t.Fatal("a zero ExpiresAt means no expiry")
	}
	if got := (Entry{}).Age(now); got != 0 {
		t.Fatalf("Age of an uncompleted entry = %s, want 0", got)
	}
	if got := (Entry{Body: []byte("abc")}).BodyLen(); got != 3 {
		t.Fatalf("BodyLen = %d", got)
	}
}

func TestStatsRenderAsAnAdminSurface(t *testing.T) {
	s, _ := newTestStore(t, Options{Capacity: 8, TTL: time.Minute})
	s.Begin("acme", "k1", "h")
	s.Complete("acme", "k1", Entry{Status: 200, Body: []byte("a")})
	s.Begin("acme", "k1", "h")
	s.Begin("acme", "k2", "h")
	st := s.Stats()
	if st.Lookups != 3 || st.Hits != 1 || st.Misses != 2 || st.Stored != 1 {
		t.Fatalf("stats = %+v", st)
	}
	// k2 was claimed and never completed: the gauge must show it and Len must
	// not, because a claim is not a replayable answer.
	if got := s.InFlight(); got != 1 {
		t.Fatalf("InFlight gauge = %d, want 1", got)
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1 (only the completed k1 counts)", got)
	}
}
