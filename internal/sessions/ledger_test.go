package sessions

import (
	"sync"
	"testing"
	"time"
)

// newTestLedger builds a ledger with a controllable clock and no janitor, so a
// TTL test advances time instead of sleeping.
func newTestLedger(t *testing.T, opts Options) (*Ledger, func(time.Duration)) {
	t.Helper()
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	clock := struct {
		mu sync.Mutex
		at time.Time
	}{at: start}
	opts.Now = func() time.Time {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		return clock.at
	}
	if opts.SweepInterval == 0 {
		opts.SweepInterval = -1
	}
	l := New(opts)
	t.Cleanup(func() { l.Close() })
	advance := func(d time.Duration) {
		clock.mu.Lock()
		clock.at = clock.at.Add(d)
		clock.mu.Unlock()
	}
	return l, advance
}

func meter(model string, prompt, completion int, cost float64) Request {
	return Request{
		Model:            model,
		Status:           200,
		Outcome:          "success",
		Upstream:         "primary",
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CostUSD:          cost,
	}
}

func TestRecordCreatesAndAccumulatesASession(t *testing.T) {
	l, _ := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour, RecentPerSession: 4})
	l.Record("acme", "s1", meter("gpt-4o", 10, 5, 0.001))
	req := meter("deepseek-chat", 20, 7, 0.002)
	req.Upstream = "secondary"
	l.Record("acme", "s1", req)

	s, ok := l.Get("acme", "s1")
	if !ok {
		t.Fatal("session was not created")
	}
	if s.Tenant != "acme" || s.ID != "s1" {
		t.Fatalf("session identity = %+v", s)
	}
	if s.Requests != 2 || s.Ok != 2 || s.Failed != 0 {
		t.Fatalf("request counters = %+v", s)
	}
	if s.PromptTokens != 30 || s.CompletionTokens != 12 {
		t.Fatalf("tokens = %d/%d, want 30/12", s.PromptTokens, s.CompletionTokens)
	}
	if got := s.CostUSD; got < 0.0029 || got > 0.0031 {
		t.Fatalf("cost = %v, want ~0.003", got)
	}
	if len(s.Models) != 2 {
		t.Fatalf("models = %+v", s.Models)
	}
	if got := s.Models["gpt-4o"]; got.Requests != 1 || got.PromptTokens != 10 || got.CostUSD != 0.001 {
		t.Fatalf("gpt-4o totals = %+v", got)
	}
	if s.Upstreams["primary"] != 1 || s.Upstreams["secondary"] != 1 {
		t.Fatalf("upstreams = %+v", s.Upstreams)
	}
	if len(s.Recent) != 2 || s.Recent[1].Model != "deepseek-chat" {
		t.Fatalf("recent = %+v", s.Recent)
	}
	if s.FirstSeen.IsZero() || s.LastSeen.IsZero() || s.ExpiresAt.IsZero() {
		t.Fatalf("timestamps = %+v", s)
	}
}

func TestRecentWindowIsBoundedAndCounted(t *testing.T) {
	l, _ := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour, RecentPerSession: 2})
	for i := 0; i < 4; i++ {
		req := meter("mock", i, 0, 0)
		req.RequestID = string(rune('a' + i))
		l.Record("acme", "s1", req)
	}
	s, _ := l.Get("acme", "s1")
	if s.Requests != 4 {
		t.Fatalf("Requests = %d, want 4 (the window must not shrink the rollup)", s.Requests)
	}
	if len(s.Recent) != 2 {
		t.Fatalf("Recent has %d entries, want 2", len(s.Recent))
	}
	if s.Recent[0].RequestID != "c" || s.Recent[1].RequestID != "d" {
		t.Fatalf("Recent kept the wrong requests: %+v", s.Recent)
	}
	if s.RecentDropped != 2 {
		t.Fatalf("RecentDropped = %d, want 2", s.RecentDropped)
	}
	if got := l.Stats().RecentDropped; got != 2 {
		t.Fatalf("Stats.RecentDropped = %d, want 2", got)
	}
}

func TestMissingSessionIDIsCountedNotInvented(t *testing.T) {
	l, _ := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour})
	l.Record("acme", "", meter("m", 1, 1, 0))
	l.Record("acme", "   ", meter("m", 1, 1, 0))
	if got := l.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0: a request without a session id must not become a session", got)
	}
	if got := l.Stats().NoSessionID; got != 2 {
		t.Fatalf("NoSessionID = %d, want 2", got)
	}
	if got := l.Stats().Recorded; got != 0 {
		t.Fatalf("Recorded = %d, want 0 (nothing was recorded)", got)
	}
}

func TestSessionsAreScopedByTenant(t *testing.T) {
	l, _ := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour})
	l.Record("acme", "s1", meter("m", 1, 1, 0.5))
	l.Record("globex", "s1", meter("m", 2, 0, 0.25))

	if got := l.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2: the same session id under two tenants is two conversations", got)
	}
	acme, _ := l.Get("acme", "s1")
	globex, _ := l.Get("globex", "s1")
	if acme.PromptTokens != 1 || globex.PromptTokens != 2 {
		t.Fatalf("tenants shared a session: acme %d / globex %d", acme.PromptTokens, globex.PromptTokens)
	}
	found := l.Find("s1")
	if len(found) != 2 {
		t.Fatalf("Find returned %d sessions, want 2", len(found))
	}
}

func TestFindIsNewestFirst(t *testing.T) {
	l, advance := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour})
	l.Record("acme", "same", meter("m", 1, 0, 0))
	advance(time.Minute)
	l.Record("globex", "same", meter("m", 1, 0, 0))
	found := l.Find("same")
	if len(found) != 2 || found[0].Tenant != "globex" {
		t.Fatalf("Find order = %+v", found)
	}
	if got := l.Find("nope"); len(got) != 0 {
		t.Fatalf("Find(unknown) = %+v", got)
	}
}

func TestLRUEvictsTheLeastRecentlyActiveSession(t *testing.T) {
	l, _ := newTestLedger(t, Options{Capacity: 2, TTL: time.Hour})
	l.Record("acme", "s1", meter("m", 1, 0, 0))
	l.Record("acme", "s2", meter("m", 1, 0, 0))
	// Touching s1 makes s2 the least recently active.
	l.Record("acme", "s1", meter("m", 1, 0, 0))
	l.Record("acme", "s3", meter("m", 1, 0, 0))

	if _, ok := l.Get("acme", "s2"); ok {
		t.Fatal("s2 should have been evicted")
	}
	for _, id := range []string{"s1", "s3"} {
		if _, ok := l.Get("acme", id); !ok {
			t.Fatalf("%s should still be tracked", id)
		}
	}
	if got := l.Stats().Evicted; got != 1 {
		t.Fatalf("Evicted = %d, want 1", got)
	}
}

func TestExpiryDropsAnIdleSession(t *testing.T) {
	l, advance := newTestLedger(t, Options{Capacity: 8, TTL: 10 * time.Minute})
	l.Record("acme", "s1", meter("m", 1, 0, 0))

	advance(9 * time.Minute)
	l.sweep()
	if _, ok := l.Get("acme", "s1"); !ok {
		t.Fatal("session expired before its TTL")
	}
	// Activity refreshes the TTL: a conversation that is still going must not
	// vanish mid-conversation.
	advance(time.Minute)
	l.Record("acme", "s1", meter("m", 1, 0, 0))
	advance(9 * time.Minute)
	l.sweep()
	if _, ok := l.Get("acme", "s1"); !ok {
		t.Fatal("activity did not refresh the TTL")
	}

	advance(11 * time.Minute)
	l.sweep()
	if _, ok := l.Get("acme", "s1"); ok {
		t.Fatal("session should have expired")
	}
	if got := l.Stats().Expired; got != 1 {
		t.Fatalf("Expired = %d, want 1", got)
	}
}

func TestTenantTotalsAreRankedByCost(t *testing.T) {
	l, _ := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour})
	l.Record("cheap", "s1", meter("m", 10, 10, 0.01))
	l.Record("cheap", "s2", meter("m", 10, 10, 0.01))
	l.Record("dear", "s1", meter("m", 100, 100, 0.90))

	totals := l.Tenants()
	if len(totals) != 2 {
		t.Fatalf("Tenants = %+v", totals)
	}
	if totals[0].Tenant != "dear" {
		t.Fatalf("Tenants not ranked by cost: %+v", totals)
	}
	if totals[1].Tenant != "cheap" || totals[1].Sessions != 2 || totals[1].Requests != 2 {
		t.Fatalf("cheap rollup = %+v", totals[1])
	}
	if got := totals[1].PromptTokens; got != 20 {
		t.Fatalf("cheap prompt tokens = %d, want 20", got)
	}
}

func TestListIsNewestFirstAndLimited(t *testing.T) {
	l, advance := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour})
	l.Record("acme", "old", meter("m", 0, 0, 0))
	advance(time.Minute)
	l.Record("acme", "new", meter("m", 0, 0, 0))

	list := l.List(0)
	if len(list) != 2 || list[0].ID != "new" || list[1].ID != "old" {
		t.Fatalf("List = %+v", list)
	}
	if got := l.List(1); len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("List(1) = %+v", got)
	}
}

func TestOutcomeCountersAndReplayAndCache(t *testing.T) {
	l, _ := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour})
	fail := meter("m", 0, 0, 0)
	fail.Status = 502
	fail.Outcome = "upstream_error"
	fail.Reason = "no upstream could serve this request"
	l.Record("acme", "s1", fail)

	replayed := meter("m", 5, 5, 0.1)
	replayed.Replay = true
	replayed.Cache = "replay"
	l.Record("acme", "s1", replayed)

	cached := meter("m", 5, 5, 0.0)
	cached.Cache = "hit_exact"
	l.Record("acme", "s1", cached)

	miss := meter("m", 5, 5, 0.0)
	miss.Cache = "miss"
	l.Record("acme", "s1", miss)

	s, _ := l.Get("acme", "s1")
	if s.Requests != 4 || s.Failed != 1 || s.Ok != 3 {
		t.Fatalf("outcome counters = %+v", s)
	}
	if s.Replays != 1 {
		t.Fatalf("Replays = %d, want 1", s.Replays)
	}
	if s.CacheHits != 1 {
		t.Fatalf("CacheHits = %d, want 1 (only the hit_* cache status counts)", s.CacheHits)
	}
	if s.Recent[0].Reason == "" {
		t.Fatal("the failure reason should be kept in the recent window")
	}
}

func TestReturnedSessionsAreCopies(t *testing.T) {
	l, _ := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour, RecentPerSession: 2})
	l.Record("acme", "s1", meter("gpt-4o", 1, 1, 0.1))
	s, _ := l.Get("acme", "s1")
	s.Models["gpt-4o"] = ModelTotals{Requests: 99}
	s.Upstreams["primary"] = 99
	s.Recent[0].Tried = append(s.Recent[0].Tried, "hacked")
	s.Recent[0].Model = "tampered"

	again, _ := l.Get("acme", "s1")
	if again.Models["gpt-4o"].Requests != 1 {
		t.Fatalf("Models map leaked: %+v", again.Models)
	}
	if again.Upstreams["primary"] != 1 {
		t.Fatalf("Upstreams map leaked: %+v", again.Upstreams)
	}
	if again.Recent[0].Model != "gpt-4o" {
		t.Fatalf("Recent slice leaked: %+v", again.Recent)
	}
}

func TestFlushDropsEverything(t *testing.T) {
	l, _ := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour})
	l.Record("acme", "s1", meter("m", 0, 0, 0))
	l.Record("globex", "s2", meter("m", 0, 0, 0))
	if got := l.Flush(); got != 2 {
		t.Fatalf("Flush = %d, want 2", got)
	}
	if got := l.Len(); got != 0 {
		t.Fatalf("Len after flush = %d", got)
	}
	if got := l.Stats().Flushes; got != 1 {
		t.Fatalf("Flushes = %d, want 1", got)
	}
	// The ledger stays usable after a flush.
	l.Record("acme", "s1", meter("m", 0, 0, 0))
	if _, ok := l.Get("acme", "s1"); !ok {
		t.Fatal("ledger unusable after Flush")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	l := New(Options{SweepInterval: -1})
	defer l.Close()
	if l.Capacity() != DefaultCapacity || l.TTL() != DefaultTTL || l.RecentPerSession() != DefaultRecentPerSession {
		t.Fatalf("defaults = %d / %s / %d", l.Capacity(), l.TTL(), l.RecentPerSession())
	}
}

func TestConcurrentRecordsKeepTheRollupConsistent(t *testing.T) {
	l, _ := newTestLedger(t, Options{Capacity: 8, TTL: time.Hour, RecentPerSession: 100})
	const workers, each = 8, 25
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				l.Record("acme", "s1", meter("m", 1, 2, 0.001))
			}
		}()
	}
	wg.Wait()
	s, ok := l.Get("acme", "s1")
	if !ok {
		t.Fatal("session missing after concurrent records")
	}
	want := int64(workers * each)
	if s.Requests != want {
		t.Fatalf("Requests = %d, want %d (a lost update would hide real spend)", s.Requests, want)
	}
	if s.PromptTokens != want || s.CompletionTokens != 2*want {
		t.Fatalf("tokens = %d/%d, want %d/%d", s.PromptTokens, s.CompletionTokens, want, 2*want)
	}
	if got := l.Stats().Recorded; got != uint64(want) {
		t.Fatalf("Recorded = %d, want %d", got, want)
	}
}
