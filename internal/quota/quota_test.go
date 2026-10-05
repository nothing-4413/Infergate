package quota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test scaffolding
// ---------------------------------------------------------------------------

// testClock is a wall clock the test drives by hand, so a day rollover or a
// throttle window is a clock change instead of a sleep.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(at time.Time) *testClock { return &testClock{now: at} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// stubCall is one recorded Store.Add.
type stubCall struct {
	key   string
	delta int64
	ttl   time.Duration
}

// stubStore is an in-process Store that records every write and can be told to
// fail. It is how the fail-open and fail-closed paths are reached without
// breaking a real backend, and how the exact key layout is observed.
type stubStore struct {
	mu     sync.Mutex
	calls  []stubCall
	values map[string]int64
	addErr error
	getErr error
	closes int
}

func newStubStore() *stubStore {
	return &stubStore{values: make(map[string]int64)}
}

func (s *stubStore) Add(_ context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{key: key, delta: delta, ttl: ttl})
	if s.addErr != nil {
		return 0, s.addErr
	}
	s.values[key] += delta
	return s.values[key], nil
}

func (s *stubStore) Get(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return 0, s.getErr
	}
	return s.values[key], nil
}

func (s *stubStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}

func (s *stubStore) callsSnapshot() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall(nil), s.calls...)
}

func (s *stubStore) addCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubStore) addKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.calls))
	for _, c := range s.calls {
		out = append(out, c.key)
	}
	return out
}

var errStoreDown = errors.New("stub store is down")

// newManager builds a manager and closes its store when the test ends.
func newManager(t *testing.T, cfg Config, store Store) *Manager {
	t.Helper()
	m := New(cfg, store)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func storeValue(t *testing.T, ctx context.Context, s Store, key string) int64 {
	t.Helper()
	v, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	return v
}

func assertKeys(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("store saw %d writes %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("store write %d = %q, want %q (all writes: %v)", i, got[i], want[i], got)
		}
	}
}

// ---------------------------------------------------------------------------
// 1. A disabled manager
// ---------------------------------------------------------------------------

func TestDisabledManagerIsAllowingAndInert(t *testing.T) {
	ctx := context.Background()
	store := newStubStore()
	m := newManager(t, Config{
		Enabled:       false,
		DefaultPolicy: Policy{Tenant: "acme", TokensPerDay: 10, RequestsPerMinute: 1},
	}, store)

	if m.Enabled() {
		t.Fatal("Enabled() = true for a manager built with Enabled: false")
	}

	dec, err := m.Admit(ctx, "acme", "s1", Estimate{PromptTokens: 5000, Requests: 1})
	if err != nil {
		t.Fatalf("Admit on a disabled manager: %v", err)
	}
	if dec == nil {
		t.Fatal("Admit returned a nil decision")
	}
	if !dec.Allowed || dec.Action != ActionAllow || dec.Reason != ReasonDisabled {
		t.Fatalf("disabled Admit = allowed %v action %q reason %q, want true/%q/%q",
			dec.Allowed, dec.Action, dec.Reason, ActionAllow, ReasonDisabled)
	}
	if dec.Reservation != nil {
		t.Fatalf("a disabled manager handed out a reservation: %+v", dec.Reservation)
	}
	if n := store.addCalls(); n != 0 {
		t.Fatalf("a disabled manager wrote to the store %d times", n)
	}
	if got := m.Stats(); got.Allowed != 0 || got.ReservedTokens != 0 || got.StoreErrors != 0 {
		t.Fatalf("a disabled manager touched its own counters: %+v", got)
	}

	// The disabled path hands back a nil reservation, so settling and releasing
	// it must be safe. A panic here would take out a live request.
	if err := m.Settle(ctx, nil, Usage{PromptTokens: 10, Requests: 1}); err != nil {
		t.Fatalf("Settle(nil): %v", err)
	}
	if err := m.Release(ctx, nil); err != nil {
		t.Fatalf("Release(nil): %v", err)
	}
	if err := m.Settle(ctx, dec.Reservation, Usage{PromptTokens: 10, Requests: 1}); err != nil {
		t.Fatalf("Settle of the disabled decision's nil reservation: %v", err)
	}
	if err := m.Release(ctx, dec.Reservation); err != nil {
		t.Fatalf("Release of the disabled decision's nil reservation: %v", err)
	}
	if n := store.addCalls(); n != 0 {
		t.Fatalf("Settle/Release of a nil reservation wrote to the store %d times", n)
	}
}

// ---------------------------------------------------------------------------
// 2. Daily token budget
// ---------------------------------------------------------------------------

func TestDailyTokenBudget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	clock := newTestClock(now)
	store := NewMemoryStore()
	m := newManager(t, Config{
		Enabled:       true,
		Now:           clock.Now,
		DefaultPolicy: Policy{Tenant: "acme", TokensPerDay: 100},
	}, store)
	key := m.key("acme", WindowDay, dayBucket(now), CounterTokens)

	est := Estimate{PromptTokens: 40, Requests: 1}
	for i := 1; i <= 2; i++ {
		dec, err := m.Admit(ctx, "acme", "", est)
		if err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
		if !dec.Allowed || dec.Action != ActionAllow || dec.Reason != ReasonWithinBudget {
			t.Fatalf("admit %d = allowed %v action %q reason %q, want true/%q/%q",
				i, dec.Allowed, dec.Action, dec.Reason, ActionAllow, ReasonWithinBudget)
		}
		if dec.Reservation == nil {
			t.Fatalf("admit %d was allowed without a reservation", i)
		}
		if err := m.Settle(ctx, dec.Reservation, Usage{PromptTokens: 40, Requests: 1}); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
		if got := storeValue(t, ctx, store, key); got != int64(40*i) {
			t.Fatalf("after admit %d the day counter is %d, want %d", i, got, 40*i)
		}
	}

	dec, err := m.Admit(ctx, "acme", "", est)
	if err != nil {
		t.Fatalf("the rejected admit returned an error: %v", err)
	}
	if dec.Allowed {
		t.Fatal("the third estimate (120 tokens) must be rejected against a 100 token day")
	}
	if dec.Action != ActionReject {
		t.Fatalf("Action = %q, want %q", dec.Action, ActionReject)
	}
	if dec.Reason != ReasonDailyTokens {
		t.Fatalf("Reason = %q, want %q", dec.Reason, ReasonDailyTokens)
	}
	if dec.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want a positive wait until the day bucket resets", dec.RetryAfter)
	}
	if dec.Reservation != nil {
		t.Fatalf("a rejected request must not hand out a reservation: %+v", dec.Reservation)
	}
	if dec.Limit != 100 || dec.Used != 80 || dec.Requested != 40 {
		t.Fatalf("rejection = limit %d used %d requested %d, want 100/80/40",
			dec.Limit, dec.Used, dec.Requested)
	}
	if got := storeValue(t, ctx, store, key); got != 80 {
		t.Fatalf("the rejected request was charged: the day counter is %d, want 80", got)
	}
	if got := m.Stats(); got.Allowed != 2 || got.Rejected != 1 {
		t.Fatalf("stats = allowed %d rejected %d, want 2 and 1", got.Allowed, got.Rejected)
	}
}

// ---------------------------------------------------------------------------
// 3. Daily cost budget
// ---------------------------------------------------------------------------

func TestDailyCostBudget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	clock := newTestClock(now)
	store := NewMemoryStore()
	m := newManager(t, Config{
		Enabled:       true,
		Now:           clock.Now,
		DefaultPolicy: Policy{Tenant: "acme", CostPerDayMicros: 1000},
	}, store)
	key := m.key("acme", WindowDay, dayBucket(now), CounterCost)

	dec, err := m.Admit(ctx, "acme", "", Estimate{CostMicros: 600, Requests: 1})
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if !dec.Allowed || dec.Reason != ReasonWithinBudget {
		t.Fatalf("first admit = allowed %v reason %q, want true/%q", dec.Allowed, dec.Reason, ReasonWithinBudget)
	}
	if err := m.Settle(ctx, dec.Reservation, Usage{CostMicros: 600, Requests: 1}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := storeValue(t, ctx, store, key); got != 600 {
		t.Fatalf("after the first request the day cost is %d, want 600", got)
	}

	dec, err = m.Admit(ctx, "acme", "", Estimate{CostMicros: 600, Requests: 1})
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if dec.Allowed {
		t.Fatal("1200 micros of cost must be rejected against a 1000 micro day")
	}
	if dec.Reason != ReasonDailyCost {
		t.Fatalf("Reason = %q, want %q (the cost dimension, not the token one)", dec.Reason, ReasonDailyCost)
	}
	if dec.Action != ActionReject || dec.Reservation != nil || dec.RetryAfter <= 0 {
		t.Fatalf("rejection = action %q reservation %v retryAfter %v, want %q/nil/positive",
			dec.Action, dec.Reservation, dec.RetryAfter, ActionReject)
	}
	if dec.Limit != 1000 || dec.Used != 600 || dec.Requested != 600 {
		t.Fatalf("rejection = limit %d used %d requested %d, want 1000/600/600",
			dec.Limit, dec.Used, dec.Requested)
	}
	if got := storeValue(t, ctx, store, key); got != 600 {
		t.Fatalf("the rejected request was charged: the day cost is %d, want 600", got)
	}
}

// ---------------------------------------------------------------------------
// 4. Requests per minute
// ---------------------------------------------------------------------------

func TestRequestsPerMinute(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 6, 7, 8, 30, 0, time.UTC)
	clock := newTestClock(now)
	store := NewMemoryStore()
	m := newManager(t, Config{
		Enabled:       true,
		Now:           clock.Now,
		DefaultPolicy: Policy{Tenant: "acme", RequestsPerMinute: 2},
	}, store)
	key := m.key("acme", WindowMinute, minuteBucket(now), CounterRequests)

	first, err := m.Admit(ctx, "acme", "", Estimate{Requests: 1})
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if !first.Allowed {
		t.Fatalf("the first of two requests in a minute must be allowed: %+v", first)
	}
	if err := m.Settle(ctx, first.Reservation, Usage{Requests: 1}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := storeValue(t, ctx, store, key); got != 1 {
		t.Fatalf("the minute counter is %d after one request, want 1", got)
	}

	second, err := m.Admit(ctx, "acme", "", Estimate{Requests: 1})
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if !second.Allowed {
		t.Fatalf("the second of two requests in a minute must be allowed: %+v", second)
	}

	third, err := m.Admit(ctx, "acme", "", Estimate{Requests: 1})
	if err != nil {
		t.Fatalf("third admit: %v", err)
	}
	if third.Allowed {
		t.Fatal("the third request in a minute must be rejected when the limit is 2")
	}
	if third.Reason != ReasonMinuteRPM {
		t.Fatalf("Reason = %q, want %q", third.Reason, ReasonMinuteRPM)
	}
	if third.Reservation != nil {
		t.Fatalf("a rejected request must not hand out a reservation: %+v", third.Reservation)
	}
	if third.RetryAfter <= 0 || third.RetryAfter > time.Minute {
		t.Fatalf("RetryAfter = %v, want (0, 1m]: the wait is the rest of the minute", third.RetryAfter)
	}
	if got := storeValue(t, ctx, store, key); got != 2 {
		t.Fatalf("the rejected request was charged: the minute counter is %d, want 2", got)
	}

	// Release gives the request slot back, which matters because the request
	// count is a reservation like any other.
	if err := m.Release(ctx, second.Reservation); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := storeValue(t, ctx, store, key); got != 1 {
		t.Fatalf("after Release the minute counter is %d, want 1", got)
	}
	fourth, err := m.Admit(ctx, "acme", "", Estimate{Requests: 1})
	if err != nil {
		t.Fatalf("admit after Release: %v", err)
	}
	if !fourth.Allowed {
		t.Fatalf("releasing a reservation must free its request slot: %+v", fourth)
	}
	if got := storeValue(t, ctx, store, key); got != 2 {
		t.Fatalf("the minute counter is %d after the re-admit, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// 5. Session budget
// ---------------------------------------------------------------------------

func TestSessionTokenBudget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)

	t.Run("a session is capped", func(t *testing.T) {
		clock := newTestClock(now)
		store := NewMemoryStore()
		m := newManager(t, Config{
			Enabled:       true,
			Now:           clock.Now,
			DefaultPolicy: Policy{Tenant: "acme", TokensPerSession: 100},
		}, store)
		key := m.key("acme", WindowSession, "sess-1", CounterTokens)

		dec, err := m.Admit(ctx, "acme", "sess-1", Estimate{PromptTokens: 60})
		if err != nil {
			t.Fatalf("first admit: %v", err)
		}
		if !dec.Allowed {
			t.Fatalf("60 tokens against a 100 token session must be allowed: %+v", dec)
		}
		if err := m.Settle(ctx, dec.Reservation, Usage{PromptTokens: 60}); err != nil {
			t.Fatalf("settle: %v", err)
		}
		if got := storeValue(t, ctx, store, key); got != 60 {
			t.Fatalf("the session counter is %d, want 60", got)
		}

		dec, err = m.Admit(ctx, "acme", "sess-1", Estimate{PromptTokens: 60})
		if err != nil {
			t.Fatalf("second admit: %v", err)
		}
		if dec.Allowed {
			t.Fatal("120 tokens against a 100 token session must be rejected")
		}
		if dec.Reason != ReasonSessionTokens {
			t.Fatalf("Reason = %q, want %q", dec.Reason, ReasonSessionTokens)
		}
		if dec.Reservation != nil {
			t.Fatalf("a rejected request must not hand out a reservation: %+v", dec.Reservation)
		}
		if got := storeValue(t, ctx, store, key); got != 60 {
			t.Fatalf("the rejected request was charged: the session counter is %d, want 60", got)
		}

		// Another session has its own budget.
		other, err := m.Admit(ctx, "acme", "sess-2", Estimate{PromptTokens: 60})
		if err != nil {
			t.Fatalf("admit for a second session: %v", err)
		}
		if !other.Allowed {
			t.Fatalf("a second session must not inherit the first session's usage: %+v", other)
		}
	})

	t.Run("an empty session skips the counter", func(t *testing.T) {
		clock := newTestClock(now)
		store := newStubStore()
		m := newManager(t, Config{
			Enabled:       true,
			Now:           clock.Now,
			DefaultPolicy: Policy{Tenant: "acme", TokensPerSession: 100},
		}, store)

		dec, err := m.Admit(ctx, "acme", "", Estimate{PromptTokens: 10_000})
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if !dec.Allowed || dec.Reason != ReasonWithinBudget {
			t.Fatalf("a request with no session has no session budget to blow: %+v", dec)
		}
		if dec.Reservation == nil {
			t.Fatal("an allowed admit must carry a reservation even when no dimension needed one")
		}
		if n := len(dec.Reservation.entries); n != 0 {
			t.Fatalf("the reservation holds %d entries, want 0: an empty session must not be counted", n)
		}
		if n := store.addCalls(); n != 0 {
			t.Fatalf("an empty session wrote to the store %d times", n)
		}

		// The session that IS named still gets its own bucket, with the default
		// session ttl.
		named, err := m.Admit(ctx, "acme", "sess-1", Estimate{PromptTokens: 50})
		if err != nil {
			t.Fatalf("Admit for a named session: %v", err)
		}
		if !named.Allowed {
			t.Fatalf("50 tokens against a 100 token session must be allowed: %+v", named)
		}
		assertKeys(t, store.addKeys(), []string{m.key("acme", WindowSession, "sess-1", CounterTokens)})
		if calls := store.callsSnapshot(); calls[0].ttl != DefaultSessionTTL {
			t.Fatalf("session ttl = %v, want the default %v", calls[0].ttl, DefaultSessionTTL)
		}
	})
}

// ---------------------------------------------------------------------------
// 6. The degrade ladder
// ---------------------------------------------------------------------------

func TestDegradeWithDowngradeModel(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 13, 14, 15, 0, time.UTC)
	clock := newTestClock(now)
	store := NewMemoryStore()
	m := newManager(t, Config{
		Enabled: true,
		Now:     clock.Now,
		DefaultPolicy: Policy{
			Tenant: "acme", TokensPerDay: 100,
			OnExceed: ActionDegrade, DowngradeModel: "cheap",
		},
	}, store)
	key := m.key("acme", WindowDay, dayBucket(now), CounterTokens)

	dec, err := m.Admit(ctx, "acme", "s1", Estimate{PromptTokens: 150})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("a degrade policy must still admit the request: %+v", dec)
	}
	if dec.Action != ActionDegrade {
		t.Fatalf("Action = %q, want %q", dec.Action, ActionDegrade)
	}
	if dec.Reason != ReasonDailyTokens {
		t.Fatalf("Reason = %q, want the dimension that overflowed (%q)", dec.Reason, ReasonDailyTokens)
	}
	if dec.DowngradeModel != "cheap" {
		t.Fatalf("DowngradeModel = %q, want %q", dec.DowngradeModel, "cheap")
	}
	if dec.Limit != 100 || dec.Requested != 150 || dec.Used != 0 {
		t.Fatalf("degrade = limit %d requested %d used %d, want 100/150/0",
			dec.Limit, dec.Requested, dec.Used)
	}
	if dec.Reservation == nil {
		t.Fatal("a degraded request is still served, so its reservation must survive to be settled")
	}
	if got := storeValue(t, ctx, store, key); got != 150 {
		t.Fatalf("a degraded request must still be counted: the day counter is %d, want 150", got)
	}
	if got := m.Stats(); got.Degraded != 1 || got.Allowed != 0 || got.ReservedTokens != 150 {
		t.Fatalf("stats = degraded %d allowed %d reserved %d, want 1/0/150",
			got.Degraded, got.Allowed, got.ReservedTokens)
	}

	// The cheaper model consumed far less, so settlement hands the rest back
	// instead of booking an overshoot.
	if err := m.Settle(ctx, dec.Reservation, Usage{PromptTokens: 10}); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := m.Stats(); got.OvershootTokens != 0 {
		t.Fatalf("OvershootTokens = %d, want 0: the settlement is below the reservation", got.OvershootTokens)
	}
	if got := m.Stats(); got.ReleasedTokens != 140 {
		t.Fatalf("ReleasedTokens = %d, want 140 (150 reserved, 10 used)", got.ReleasedTokens)
	}
	if got := storeValue(t, ctx, store, key); got != 10 {
		t.Fatalf("the day counter is %d after settling a 150 reservation at 10, want 10", got)
	}
}

func TestDegradeWithMaxTokensCapOnly(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 12, 13, 14, 15, 16, 0, time.UTC)
	clock := newTestClock(now)
	store := NewMemoryStore()
	m := newManager(t, Config{
		Enabled: true,
		Now:     clock.Now,
		DefaultPolicy: Policy{
			Tenant: "acme", TokensPerDay: 100,
			OnExceed: ActionDegrade, MaxTokensCap: 128,
		},
	}, store)

	dec, err := m.Admit(ctx, "acme", "s1", Estimate{PromptTokens: 150})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !dec.Allowed || dec.Action != ActionDegrade {
		t.Fatalf("OnExceed degrade with a token cap must admit with ActionDegrade, got %+v", dec)
	}
	if dec.MaxTokensCap != 128 {
		t.Fatalf("MaxTokensCap = %d, want 128", dec.MaxTokensCap)
	}
	if dec.DowngradeModel != "" {
		t.Fatalf("DowngradeModel = %q, want empty: this policy only caps the reply", dec.DowngradeModel)
	}
	if dec.Reservation == nil {
		t.Fatal("the degraded request must carry a reservation")
	}
	if got := storeValue(t, ctx, store, m.key("acme", WindowDay, dayBucket(now), CounterTokens)); got != 150 {
		t.Fatalf("the degraded request was not counted: %d, want 150", got)
	}
}

// ---------------------------------------------------------------------------
// 7. Settlement and release
// ---------------------------------------------------------------------------

func TestSettleIsIdempotent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	clock := newTestClock(now)
	store := NewMemoryStore()
	m := newManager(t, Config{
		Enabled:       true,
		Now:           clock.Now,
		DefaultPolicy: Policy{Tenant: "acme", TokensPerDay: 1000},
	}, store)
	key := m.key("acme", WindowDay, dayBucket(now), CounterTokens)

	dec, err := m.Admit(ctx, "acme", "", Estimate{PromptTokens: 40})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("Admit denied: %+v", dec)
	}
	if got := storeValue(t, ctx, store, key); got != 40 {
		t.Fatalf("the reserved counter is %d, want 40", got)
	}

	// The request outgrew its estimate.
	if err := m.Settle(ctx, dec.Reservation, Usage{PromptTokens: 60}); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := storeValue(t, ctx, store, key); got != 60 {
		t.Fatalf("the settled counter is %d, want 60", got)
	}
	if got := m.Stats(); got.SettledTokens != 60 || got.OvershootTokens != 20 || got.ReleasedTokens != 0 {
		t.Fatalf("stats = settled %d overshoot %d released %d, want 60/20/0 (60 really charged, 20 of it above the estimate)",
			got.SettledTokens, got.OvershootTokens, got.ReleasedTokens)
	}

	// A deferred settle can run on a path that already settled, so repeating it
	// must change nothing at all.
	for i := 1; i <= 2; i++ {
		if err := m.Settle(ctx, dec.Reservation, Usage{PromptTokens: 9999}); err != nil {
			t.Fatalf("repeated Settle %d: %v", i, err)
		}
	}
	if got := storeValue(t, ctx, store, key); got != 60 {
		t.Fatalf("a repeated Settle moved the counter to %d, want 60", got)
	}
	if got := m.Stats(); got.SettledTokens != 60 || got.OvershootTokens != 20 || got.ReleasedTokens != 0 {
		t.Fatalf("a repeated Settle double-counted: settled %d overshoot %d released %d, want 60/20/0",
			got.SettledTokens, got.OvershootTokens, got.ReleasedTokens)
	}
}

func TestReleaseReturnsEveryDimension(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 11, 12, 13, 0, time.UTC)
	clock := newTestClock(now)
	store := NewMemoryStore()
	m := newManager(t, Config{
		Enabled: true,
		Now:     clock.Now,
		DefaultPolicy: Policy{
			Tenant: "acme", TokensPerDay: 1000, CostPerDayMicros: 1_000_000,
			RequestsPerMinute: 10, TokensPerSession: 1000,
		},
	}, store)

	dec, err := m.Admit(ctx, "acme", "s1", Estimate{PromptTokens: 40, CostMicros: 500, Requests: 1})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("Admit denied: %+v", dec)
	}

	keys := []string{
		m.key("acme", WindowDay, dayBucket(now), CounterTokens),
		m.key("acme", WindowDay, dayBucket(now), CounterCost),
		m.key("acme", WindowMinute, minuteBucket(now), CounterRequests),
		m.key("acme", WindowSession, "s1", CounterTokens),
	}
	for _, k := range keys {
		if got := storeValue(t, ctx, store, k); got == 0 {
			t.Fatalf("the admit did not charge %s", k)
		}
	}

	if err := m.Release(ctx, dec.Reservation); err != nil {
		t.Fatalf("Release: %v", err)
	}
	for _, k := range keys {
		if got := storeValue(t, ctx, store, k); got != 0 {
			t.Fatalf("Release left %s at %d, want 0", k, got)
		}
	}
	// 80, not 40: the tenant is metered on two token budgets (day and session),
	// so one request consumes both and releasing it gives both back. The ledger
	// counts budgets consumed rather than requests admitted, which is what keeps
	// reserved - released + overshoot == settled true for such a tenant.
	if got := m.Stats().ReleasedTokens; got != 80 {
		t.Fatalf("Stats().ReleasedTokens = %d, want 80 (40 per token budget)", got)
	}
	// The money side must be refunded in the audit too, not just in the store.
	if got := m.Stats().ReleasedCostMicro; got != 500 {
		t.Fatalf("Stats().ReleasedCostMicro = %d, want 500", got)
	}

	// Releasing twice must not give the budget back twice.
	if err := m.Release(ctx, dec.Reservation); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if got := m.Stats().ReleasedTokens; got != 80 {
		t.Fatalf("the second Release moved Stats().ReleasedTokens to %d, want 80", got)
	}
	// And a settle after a release must not re-charge anything.
	if err := m.Settle(ctx, dec.Reservation, Usage{PromptTokens: 40, CostMicros: 500, Requests: 1}); err != nil {
		t.Fatalf("Settle after Release: %v", err)
	}
	for _, k := range keys {
		if got := storeValue(t, ctx, store, k); got != 0 {
			t.Fatalf("Settle after Release re-charged %s to %d", k, got)
		}
	}
}

// TestSettleKeepsTokensAndMoneyInDifferentCounters is the regression test for a
// unit bug a live run exposed: the cost entry's credit was added to the token
// counter, so a request that reserved 279 tokens reported 1014 released ones
// (1014 = 260 real tokens + 754 micro-dollars).
func TestSettleKeepsTokensAndMoneyInDifferentCounters(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	clock := newTestClock(now)
	store := NewMemoryStore()
	m := newManager(t, Config{
		Enabled: true,
		Now:     clock.Now,
		DefaultPolicy: Policy{
			Tenant: "acme", TokensPerDay: 1000, CostPerDayMicros: 1_000_000,
		},
	}, store)

	// Reserve the shape of a real request - the completion estimate dominates -
	// and settle far below both dimensions.
	dec, err := m.Admit(ctx, "acme", "", Estimate{PromptTokens: 23, CompletionTokens: 256, CostMicros: 765, Requests: 1})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("Admit denied: %+v", dec)
	}
	if err := m.Settle(ctx, dec.Reservation, Usage{PromptTokens: 7, CompletionTokens: 4, CostMicros: 11, Requests: 1}); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	got := m.Stats()
	if got.ReservedTokens != 279 {
		t.Fatalf("ReservedTokens = %d, want 279", got.ReservedTokens)
	}
	if got.SettledTokens != 11 {
		t.Fatalf("SettledTokens = %d, want 11 (what was really charged)", got.SettledTokens)
	}
	if got.ReleasedTokens != 268 {
		t.Fatalf("ReleasedTokens = %d, want 268 (279 reserved - 11 used): the cost credit must not land in a token counter", got.ReleasedTokens)
	}
	if got.ReleasedCostMicro != 754 {
		t.Fatalf("ReleasedCostMicro = %d, want 754 (765 reserved - 11 spent)", got.ReleasedCostMicro)
	}
	if got.OvershootTokens != 0 || got.OvershootCostMicro != 0 {
		t.Fatalf("overshoot = %d tokens / %d micros, want 0/0", got.OvershootTokens, got.OvershootCostMicro)
	}
	// The identity the three audit counters have to satisfy.
	if got.ReservedTokens-got.ReleasedTokens+got.OvershootTokens != got.SettledTokens {
		t.Fatalf("reserved %d - released %d + overshoot %d != settled %d",
			got.ReservedTokens, got.ReleasedTokens, got.OvershootTokens, got.SettledTokens)
	}
	// And the store agrees with the audit counters, in both units.
	if v := storeValue(t, ctx, store, m.key("acme", WindowDay, dayBucket(now), CounterTokens)); v != 11 {
		t.Fatalf("day token counter = %d, want 11", v)
	}
	if v := storeValue(t, ctx, store, m.key("acme", WindowDay, dayBucket(now), CounterCost)); v != 11 {
		t.Fatalf("day cost counter = %d, want 11", v)
	}
}

// ---------------------------------------------------------------------------
// 8. Store failures
// ---------------------------------------------------------------------------

func TestStoreErrorsFailClosedAndFailOpen(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	cfg := func(failOpen bool) Config {
		return Config{
			Enabled:  true,
			FailOpen: failOpen,
			Now:      newTestClock(now).Now,
			DefaultPolicy: Policy{
				Tenant: "acme", TokensPerDay: 100, RequestsPerMinute: 10,
			},
		}
	}

	t.Run("fail closed", func(t *testing.T) {
		store := newStubStore()
		store.addErr = errStoreDown
		m := newManager(t, cfg(false), store)

		dec, err := m.Admit(ctx, "acme", "", Estimate{PromptTokens: 40, Requests: 1})
		if err == nil {
			t.Fatal("a broken store must surface an error so the caller can apply its own policy")
		}
		if !errors.Is(err, errStoreDown) {
			t.Fatalf("Admit error = %v, want it to wrap %v", err, errStoreDown)
		}
		if dec == nil {
			t.Fatal("Admit returned a nil decision alongside the error")
		}
		if dec.Allowed {
			t.Fatal("fail-closed Admit must deny when the store errors")
		}
		if dec.Action != ActionReject || dec.Reason != ReasonStoreError {
			t.Fatalf("decision = action %q reason %q, want %q/%q",
				dec.Action, dec.Reason, ActionReject, ReasonStoreError)
		}
		if dec.Reservation != nil {
			t.Fatalf("a denied request must not hand out a reservation: %+v", dec.Reservation)
		}
		if got := m.Stats(); got.StoreErrors != 1 {
			t.Fatalf("Stats().StoreErrors = %d, want 1", got.StoreErrors)
		}
	})

	t.Run("fail open", func(t *testing.T) {
		store := newStubStore()
		store.addErr = errStoreDown
		m := newManager(t, cfg(true), store)

		dec, err := m.Admit(ctx, "acme", "", Estimate{PromptTokens: 40, Requests: 1})
		if err == nil {
			t.Fatal("fail-open also returns the error: the caller still has to log it")
		}
		if !errors.Is(err, errStoreDown) {
			t.Fatalf("Admit error = %v, want it to wrap %v", err, errStoreDown)
		}
		if dec == nil {
			t.Fatal("Admit returned a nil decision alongside the error")
		}
		if !dec.Allowed {
			t.Fatalf("fail-open Admit must allow when the store errors: %+v", dec)
		}
		if dec.Action != ActionAllow || dec.Reason != ReasonStoreError {
			t.Fatalf("decision = action %q reason %q, want %q/%q",
				dec.Action, dec.Reason, ActionAllow, ReasonStoreError)
		}
		if dec.Reservation != nil {
			t.Fatalf("a fail-open decision has nothing to settle, so it must not hand out a reservation: %+v", dec.Reservation)
		}
		if got := m.Stats(); got.StoreErrors != 1 {
			t.Fatalf("Stats().StoreErrors = %d, want 1", got.StoreErrors)
		}
	})

	t.Run("a read error surfaces from Report", func(t *testing.T) {
		store := newStubStore()
		store.getErr = errStoreDown
		m := newManager(t, cfg(true), store)
		if _, err := m.Report(ctx, "acme", "sess-1"); !errors.Is(err, errStoreDown) {
			t.Fatalf("Report error = %v, want %v", err, errStoreDown)
		}
	})
}

// ---------------------------------------------------------------------------
// 9. An unbounded policy
// ---------------------------------------------------------------------------

func TestUnboundedPolicySkipsTheStore(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC)

	cases := []struct {
		name   string
		policy Policy
	}{
		{name: "no limits", policy: Policy{Tenant: "acme", OnExceed: ActionReject}},
		{name: "zero policy", policy: Policy{Tenant: "acme"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newTestClock(now)
			store := newStubStore()
			m := newManager(t, Config{Enabled: true, Now: clock.Now, DefaultPolicy: tc.policy}, store)

			dec, err := m.Admit(ctx, "acme", "sess-1", Estimate{
				PromptTokens: 1 << 20, CompletionTokens: 1 << 20,
				CostMicros: 1 << 30, Requests: 1000,
			})
			if err != nil {
				t.Fatalf("Admit: %v", err)
			}
			if !dec.Allowed || dec.Action != ActionAllow || dec.Reason != ReasonWithinBudget {
				t.Fatalf("unbounded Admit = allowed %v action %q reason %q, want true/%q/%q",
					dec.Allowed, dec.Action, dec.Reason, ActionAllow, ReasonWithinBudget)
			}
			if dec.Reservation != nil {
				t.Fatalf("an unlimited tenant needs no reservation: %+v", dec.Reservation)
			}
			if n := store.addCalls(); n != 0 {
				t.Fatalf("an unlimited tenant wrote to the store %d times", n)
			}
			if got := m.Stats(); got.Allowed != 1 || got.ReservedTokens != 0 {
				t.Fatalf("stats = allowed %d reserved %d, want 1/0", got.Allowed, got.ReservedTokens)
			}
			if got := m.Policy("acme"); got != tc.policy {
				t.Fatalf("Policy(acme) = %+v, want %+v", got, tc.policy)
			}
		})
	}

	// Budgeted() is the single decision: a policy whose dimensions are all zero
	// has nothing to reserve against. There is deliberately no separate
	// "unbounded" flag to contradict it, because such a flag would be a second
	// source of truth for the same question, and the one an operator would set
	// wrongly.
	t.Run("a limit is what makes a policy bounded", func(t *testing.T) {
		clock := newTestClock(now)
		store := newStubStore()
		m := newManager(t, Config{Enabled: true, Now: clock.Now,
			DefaultPolicy: Policy{Tenant: "acme", TokensPerDay: 100}}, store)

		dec, err := m.Admit(ctx, "acme", "", Estimate{PromptTokens: 500})
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if dec.Allowed {
			t.Fatal("TokensPerDay: 100 must be enforced")
		}
		if dec.Reason != ReasonDailyTokens {
			t.Fatalf("Reason = %q, want %q", dec.Reason, ReasonDailyTokens)
		}
	})
}

// ---------------------------------------------------------------------------
// 10. Key layout
// ---------------------------------------------------------------------------

func TestKeyLayoutAndBucketFormats(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	clock := newTestClock(now)
	store := newStubStore()
	m := newManager(t, Config{
		Enabled: true,
		Prefix:  "ig:quota",
		Now:     clock.Now,
		DefaultPolicy: Policy{
			Tenant: "acme:inc/eu", TokensPerDay: 1000,
			RequestsPerMinute: 10, TokensPerSession: 500,
		},
	}, store)

	dec, err := m.Admit(ctx, "acme:inc/eu", "sess/1", Estimate{PromptTokens: 7, Requests: 1})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("Admit denied: %+v", dec)
	}

	// One write per limited dimension, in the order checks() builds them. Both
	// the tenant and the session id are escaped so neither can introduce a
	// separator, and the window and the bucket are literal.
	//
	// The escape is injective and fixed-width: ":" encodes as "_x00003a" and "/"
	// as "_x00002f", so this tenant cannot be spelled into another tenant's keys
	// by a crafted header ("acme_inc_eu" stays a different key).
	assertKeys(t, store.addKeys(), []string{
		"ig:quota:acme_x00003ainc_x00002feu:day:20260102:tokens",
		"ig:quota:acme_x00003ainc_x00002feu:minute:202601021504:requests",
		"ig:quota:acme_x00003ainc_x00002feu:session:sess_x00002f1:tokens",
	})

	calls := store.callsSnapshot()
	if len(calls) != 3 {
		t.Fatalf("the admit made %d writes, want 3", len(calls))
	}
	if want := untilDayEnd(now); calls[0].ttl != want {
		t.Fatalf("the day bucket ttl = %v, want %v", calls[0].ttl, want)
	}
	if calls[1].ttl != minuteTTL {
		t.Fatalf("the minute bucket ttl = %v, want %v (it must outlive its bucket)", calls[1].ttl, minuteTTL)
	}
	if calls[2].ttl != DefaultSessionTTL {
		t.Fatalf("the session ttl = %v, want %v", calls[2].ttl, DefaultSessionTTL)
	}
	if calls[0].delta != 7 || calls[1].delta != 1 || calls[2].delta != 7 {
		t.Fatalf("reserved deltas = %d/%d/%d, want 7/1/7",
			calls[0].delta, calls[1].delta, calls[2].delta)
	}

	sanitiseCases := []struct {
		in   string
		want string
	}{
		{in: "acme", want: "acme"},
		{in: "acme:inc/eu", want: "acme_x00003ainc_x00002feu"},
		{in: "", want: "_"},
		{in: "a-b_c.d", want: "a-b__c.d"},
		{in: "tenant:*", want: "tenant_x00003a_x00002a"},
		{in: "日本", want: "_x0065e5_x00672c"},
	}
	for _, tc := range sanitiseCases {
		if got := sanitise(tc.in); got != tc.want {
			t.Fatalf("sanitise(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// The encoding must be injective, which is the property that stops a crafted
	// header from charging another tenant's budget. The last pair is why the hex
	// is a fixed six digits: with a variable width, U+10FFF followed by "ff" and
	// U+10FFFF followed by "f" would both encode to "_x10fffff".
	injective := [][2]string{
		{"acme_inc", "acme:inc"},
		{"acme:inc/eu", "acme/inc:eu"},
		{"ab", "ab"},
		{"a__b", "a_x005f_b"},
		{"\U0010FFFff", "\U0010FFFFf"},
	}
	for _, pair := range injective {
		left, right := sanitise(pair[0]), sanitise(pair[1])
		if pair[0] == pair[1] {
			continue
		}
		if left == right {
			t.Fatalf("sanitise is not injective: %q and %q both encode to %q", pair[0], pair[1], left)
		}
	}

	// A crafted tenant cannot forge another tenant's bucket: the separators it
	// relies on are inside the value that gets rewritten.
	forged := m.key("victim:day:20261005", WindowDay, "20261005", CounterTokens)
	if forged != "ig:quota:victim_x00003aday_x00003a20261005:day:20261005:tokens" {
		t.Fatalf("crafted tenant key = %q", forged)
	}
	if forged == m.key("victim", WindowDay, "20261005", CounterTokens) {
		t.Fatal("a crafted tenant collided with another tenant's key")
	}

	custom := newManager(t, Config{Enabled: true, Prefix: "tenant-x", Now: clock.Now,
		DefaultPolicy: Policy{Tenant: "acme", TokensPerDay: 1000}}, newStubStore())
	if got := custom.key("acme", WindowDay, "20260102", CounterTokens); got != "tenant-x:acme:day:20260102:tokens" {
		t.Fatalf("custom prefix key = %q", got)
	}
	def := newManager(t, Config{Enabled: true, Now: clock.Now,
		DefaultPolicy: Policy{Tenant: "acme", TokensPerDay: 1000}}, newStubStore())
	if got := def.key("acme", WindowDay, "20260102", CounterTokens); got != "ig:quota:acme:day:20260102:tokens" {
		t.Fatalf("default prefix key = %q", got)
	}
	if got := def.key("", WindowDay, "20260102", CounterTokens); got != "ig:quota:_:day:20260102:tokens" {
		t.Fatalf("empty tenant key = %q", got)
	}

	bucketCases := []struct {
		name string
		at   time.Time
		day  string
		min  string
	}{
		{
			name: "mid afternoon",
			at:   now,
			day:  "20260102",
			min:  "202601021504",
		},
		{
			name: "last day of the year",
			at:   time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC),
			day:  "20261231",
			min:  "202612312359",
		},
	}
	for _, tc := range bucketCases {
		if got := dayBucket(tc.at); got != tc.day {
			t.Fatalf("%s: dayBucket(%v) = %q, want %q", tc.name, tc.at, got, tc.day)
		}
		if got := minuteBucket(tc.at); got != tc.min {
			t.Fatalf("%s: minuteBucket(%v) = %q, want %q", tc.name, tc.at, got, tc.min)
		}
	}

	untilCases := []struct {
		name string
		at   time.Time
	}{
		{name: "midday", at: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)},
		{name: "just before midnight", at: time.Date(2026, 1, 2, 23, 59, 59, 0, time.UTC)},
		{name: "midnight itself", at: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range untilCases {
		got := untilDayEnd(tc.at)
		if got <= 0 {
			t.Fatalf("%s: untilDayEnd = %v, want a positive ttl", tc.name, got)
		}
		if got < time.Minute {
			t.Fatalf("%s: untilDayEnd = %v, want at least a minute", tc.name, got)
		}
	}
	if got := untilDayEnd(time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)); got != 13*time.Hour {
		t.Fatalf("untilDayEnd(midday) = %v, want 13h: 12h to midnight plus an hour of slack", got)
	}
}

// ---------------------------------------------------------------------------
// 11. Report
// ---------------------------------------------------------------------------

func TestReportMatchesSettledUsage(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 2, 3, 10, 20, 0, 0, time.UTC)
	clock := newTestClock(now)
	store := NewMemoryStore()
	policy := Policy{
		Tenant: "acme", TokensPerDay: 1000, CostPerDayMicros: 5000,
		RequestsPerMinute: 10, TokensPerSession: 1000,
	}
	m := newManager(t, Config{
		Enabled: true,
		Now:     clock.Now,
		Tenants: map[string]Policy{"acme": policy},
	}, store)

	rounds := []struct {
		est   Estimate
		usage Usage
	}{
		{
			est:   Estimate{PromptTokens: 30, CompletionTokens: 10, CostMicros: 250, Requests: 1},
			usage: Usage{PromptTokens: 30, CompletionTokens: 10, CostMicros: 250, Requests: 1},
		},
		{
			// Reserved 20 tokens and 100 micros, actually used 5 and 0.
			est:   Estimate{PromptTokens: 20, CostMicros: 100, Requests: 1},
			usage: Usage{PromptTokens: 5, Requests: 0},
		},
	}
	for i, r := range rounds {
		dec, err := m.Admit(ctx, "acme", "s1", r.est)
		if err != nil {
			t.Fatalf("round %d admit: %v", i+1, err)
		}
		if !dec.Allowed {
			t.Fatalf("round %d denied: %+v", i+1, dec)
		}
		if err := m.Settle(ctx, dec.Reservation, r.usage); err != nil {
			t.Fatalf("round %d settle: %v", i+1, err)
		}
	}

	rep, err := m.Report(ctx, "acme", "s1")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Tenant != "acme" {
		t.Fatalf("Report.Tenant = %q, want %q", rep.Tenant, "acme")
	}
	if rep.Policy != policy {
		t.Fatalf("Report.Policy = %+v, want %+v", rep.Policy, policy)
	}
	if rep.Day != "20260203" || rep.Minute != "202602031020" {
		t.Fatalf("Report buckets = %q/%q, want 20260203/202602031020", rep.Day, rep.Minute)
	}
	if rep.Session != "s1" {
		t.Fatalf("Report.Session = %q, want %q", rep.Session, "s1")
	}
	// Round 1 booked 40 tokens and 250 micros; round 2 reserved 20 tokens and
	// 100 micros but settled at 5 and 0, so the counters hold the settled
	// amounts, not the estimates.
	if rep.TokensToday != 45 {
		t.Fatalf("TokensToday = %d, want 45", rep.TokensToday)
	}
	if rep.CostTodayMicro != 250 {
		t.Fatalf("CostTodayMicro = %d, want 250", rep.CostTodayMicro)
	}
	if rep.RequestsThisMinute != 2 {
		t.Fatalf("RequestsThisMinute = %d, want 2", rep.RequestsThisMinute)
	}
	if rep.SessionTokens != 45 {
		t.Fatalf("SessionTokens = %d, want 45", rep.SessionTokens)
	}
	if rep.BaselineTokens != 0 || rep.Ratio != 0 || rep.Alerting {
		t.Fatalf("with no AnomalyRatio Report must report no baseline: %+v", rep)
	}

	// No session id: the session counter is skipped rather than read.
	empty, err := m.Report(ctx, "acme", "")
	if err != nil {
		t.Fatalf("Report without a session: %v", err)
	}
	if empty.Session != "" || empty.SessionTokens != 0 {
		t.Fatalf("session-less report = %q/%d, want empty/0", empty.Session, empty.SessionTokens)
	}
	if empty.TokensToday != 45 || empty.RequestsThisMinute != 2 || empty.Day != rep.Day {
		t.Fatalf("the session id changed the other counters: %+v", empty)
	}

	// An unknown tenant falls back to the default policy and an untouched set
	// of counters.
	other, err := m.Report(ctx, "nobody", "s1")
	if err != nil {
		t.Fatalf("Report for an unknown tenant: %v", err)
	}
	if other.Policy != (Policy{}) {
		t.Fatalf("unknown tenant policy = %+v, want the zero default", other.Policy)
	}
	if other.TokensToday != 0 || other.CostTodayMicro != 0 || other.RequestsThisMinute != 0 || other.SessionTokens != 0 {
		t.Fatalf("unknown tenant counters = %+v, want all zero", other)
	}
}

// ---------------------------------------------------------------------------
// 12. Anomaly alerting
// ---------------------------------------------------------------------------

func TestAnomalyAlertsWithoutBlocking(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	clock := newTestClock(base)
	store := NewMemoryStore()
	policy := Policy{Tenant: "acme", TokensPerDay: 1_000_000, AnomalyRatio: 2}
	m := newManager(t, Config{
		Enabled: true,
		Now:     clock.Now,
		Tenants: map[string]Policy{"acme": policy},
	}, store)

	dayKey := m.key("acme", WindowDay, dayBucket(base), CounterTokens)
	for i := 1; i <= anomalyBaselineDays; i++ {
		key := m.key("acme", WindowDay, dayBucket(base.AddDate(0, 0, -i)), CounterTokens)
		if _, err := store.Add(ctx, key, 1000, 0); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	baseline, err := m.baseline(ctx, "acme", base, policy)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if baseline != 1000 {
		t.Fatalf("baseline = %v, want 1000 (the mean of three 1000 token days)", baseline)
	}

	spend := func(reserve, actual int) {
		t.Helper()
		dec, err := m.Admit(ctx, "acme", "s1", Estimate{PromptTokens: reserve})
		if err != nil {
			t.Fatalf("Admit(%d): %v", reserve, err)
		}
		if !dec.Allowed {
			t.Fatalf("an anomaly must never block a request: %+v", dec)
		}
		if dec.Reason != ReasonWithinBudget {
			t.Fatalf("Reason = %q, want %q: anomaly detection is alerting only",
				dec.Reason, ReasonWithinBudget)
		}
		if err := m.Settle(ctx, dec.Reservation, Usage{PromptTokens: actual}); err != nil {
			t.Fatalf("Settle(%d): %v", actual, err)
		}
	}

	// 5000 tokens against a 1000 baseline is 5x the usual day.
	spend(5000, 5000)
	if got := m.Stats().Alerts; got != 1 {
		t.Fatalf("Alerts = %d after the first anomalous settle, want 1", got)
	}
	rep, err := m.Report(ctx, "acme", "s1")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.BaselineTokens != 1000 || !rep.Alerting || rep.TokensToday != 5000 {
		t.Fatalf("Report = baseline %v alerting %v today %d, want 1000/true/5000",
			rep.BaselineTokens, rep.Alerting, rep.TokensToday)
	}

	// A settle inside the throttle window must not alert again.
	clock.Advance(10 * time.Second)
	spend(100, 100)
	if got := m.Stats().Alerts; got != 1 {
		t.Fatalf("Alerts = %d; a settle inside the same minute interval must not alert again", got)
	}

	// Past the interval it re-evaluates, but the alert is latched rather than
	// repeated on every check.
	clock.Advance(61 * time.Second)
	spend(100, 100)
	if got := m.Stats().Alerts; got != 1 {
		t.Fatalf("Alerts = %d; a still-anomalous day must not repeat the alert", got)
	}

	// Recovery: the day falls back under the threshold and the latch clears.
	clock.Advance(61 * time.Second)
	if _, err := store.Add(ctx, dayKey, -5000, 0); err != nil {
		t.Fatalf("rewrite the day counter: %v", err)
	}
	spend(0, 0)
	if got := m.Stats().Alerts; got != 1 {
		t.Fatalf("Alerts = %d on recovery, want 1: coming back to normal is not an alert", got)
	}
	rep, err = m.Report(ctx, "acme", "s1")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Alerting {
		t.Fatalf("Report still alerting after recovery: %+v", rep)
	}

	// So the next spike is a new alert, one interval later.
	clock.Advance(61 * time.Second)
	if _, err := store.Add(ctx, dayKey, 5000, 0); err != nil {
		t.Fatalf("re-inflate the day counter: %v", err)
	}
	spend(0, 0)
	if got := m.Stats().Alerts; got != 2 {
		t.Fatalf("Alerts = %d after a second spike, want 2: the alert must re-fire after recovery", got)
	}
	if got := m.Stats().Rejected; got != 0 {
		t.Fatalf("Stats().Rejected = %d: anomaly detection must never reject a request", got)
	}
}
