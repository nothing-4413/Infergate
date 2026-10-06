package breaker

import (
	"testing"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/stats"
)

func testHealthConfig() config.HealthConfig {
	return config.HealthConfig{
		Window:                config.Duration(time.Minute),
		Buckets:               6,
		MinRequests:           4,
		FailureRatio:          0.5,
		OpenDuration:          config.Duration(10 * time.Second),
		HalfOpenProbes:        2,
		MaxFailuresPerRequest: 2,
	}
}

func newTestBreaker() (*Breaker, *stats.Stats, func(time.Duration)) {
	cfg := testHealthConfig()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	window := stats.New(cfg.Window.Duration(), cfg.Buckets)
	window.SetClock(func() time.Time { return now })
	b := New("test", cfg, window)
	b.SetClock(func() time.Time { return now })
	return b, window, func(d time.Duration) { now = now.Add(d) }
}

// TestBelowMinRequestsNeverTrips is the guard against tripping on a sample size
// that means nothing: one failure out of one request is a 100% ratio.
func TestBelowMinRequestsNeverTrips(t *testing.T) {
	b, window, _ := newTestBreaker()
	for i := 0; i < 3; i++ {
		window.RecordFailure(false)
		b.RecordFailure(false)
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("state = %s with only 3 attempts and MinRequests=4, want closed", got)
	}
	if d := b.Allow(); !d.Allowed {
		t.Fatalf("Allow refused while closed: %+v", d)
	}
}

func TestTripsOnFailureRatio(t *testing.T) {
	b, window, _ := newTestBreaker()
	for i := 0; i < 4; i++ {
		window.RecordFailure(false)
		b.RecordFailure(false)
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("state = %s after 4/4 failures with a 0.5 threshold, want open", got)
	}

	// Open means refused, and the refusal is what an operator scrapes.
	d := b.Allow()
	if d.Allowed {
		t.Fatalf("Allow admitted traffic while open: %+v", d)
	}
	if d.Reason == "" {
		t.Fatal("refusal carried no reason for the logs")
	}
	if rep := b.Report(); rep.Rejects != 1 || rep.Trips != 1 {
		t.Fatalf("report = %+v, want rejects=1 trips=1", rep)
	}
}

// TestHalfOpenAdmitsExactlyOneProbe is the anti-stampede property. If every
// waiting request were admitted, the first moment after the cooldown would
// recreate the overload the breaker exists to stop.
func TestHalfOpenAdmitsExactlyOneProbe(t *testing.T) {
	b, window, advance := newTestBreaker()
	for i := 0; i < 4; i++ {
		window.RecordFailure(false)
		b.RecordFailure(false)
	}
	advance(11 * time.Second)

	first := b.Allow()
	if !first.Allowed || !first.Probe {
		t.Fatalf("first call after cooldown = %+v, want an admitted probe", first)
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("state = %s, want half-open", got)
	}
	second := b.Allow()
	if second.Allowed {
		t.Fatalf("second concurrent call was admitted as another probe: %+v", second)
	}
}

// TestReleaseProbeGivesTheHalfOpenSlotBack covers the attempt that ends without
// a verdict. A half-open breaker admits one attempt and waits to be told how it
// went; a caller that hangs up mid-generation says nothing about the backend, so
// the slot must be handed back rather than reported as a failure -- otherwise
// the next caller is refused with "half-open probe already in flight" and the
// backend never gets another chance.
func TestReleaseProbeGivesTheHalfOpenSlotBack(t *testing.T) {
	b, window, advance := newTestBreaker()
	for i := 0; i < 4; i++ {
		window.RecordFailure(false)
		b.RecordFailure(false)
	}
	advance(11 * time.Second)

	if d := b.Allow(); !d.Probe {
		t.Fatalf("first call after cooldown = %+v, want the probe", d)
	}
	if d := b.Allow(); d.Allowed {
		t.Fatalf("a second attempt was admitted as another probe: %+v", d)
	}

	b.ReleaseProbe()

	next := b.Allow()
	if !next.Allowed || !next.Probe {
		t.Fatalf("after ReleaseProbe the next call = %+v, want a fresh probe: the backend would "+
			"otherwise be refused for the life of the process", next)
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("state = %s, want half-open: handing the slot back makes no claim about the backend", got)
	}
	// Releasing is not a verdict, so the window must be exactly as it was.
	if got := window.Snapshot(); got.Attempts != 4 || got.Failures != 4 {
		t.Fatalf("window = %+v after ReleaseProbe, want the 4 failures it already had", got)
	}
}

func TestFailedProbeReopensImmediately(t *testing.T) {
	b, window, advance := newTestBreaker()
	for i := 0; i < 4; i++ {
		window.RecordFailure(false)
		b.RecordFailure(false)
	}
	advance(11 * time.Second)

	if d := b.Allow(); !d.Probe {
		t.Fatalf("expected a probe, got %+v", d)
	}
	b.RecordFailure(false)

	if got := b.State(); got != StateOpen {
		t.Fatalf("state = %s after a failed probe, want open immediately: waiting for the ratio "+
			"to cross the threshold again would require traffic that half-open by design does not admit", got)
	}
	if rep := b.Report(); rep.Trips != 2 {
		t.Fatalf("Trips = %d, want 2 (the original trip plus the failed probe)", rep.Trips)
	}
}

func TestHalfOpenProbesCloseTheBreaker(t *testing.T) {
	b, window, advance := newTestBreaker()
	for i := 0; i < 4; i++ {
		window.RecordFailure(false)
		b.RecordFailure(false)
	}
	advance(11 * time.Second)

	for i := 0; i < 2; i++ {
		if d := b.Allow(); !d.Allowed {
			t.Fatalf("probe %d refused: %+v", i+1, d)
		}
		b.RecordSuccess()
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("state = %s after %d successful probes, want closed", got, 2)
	}
}

// TestResetClearsTheWindow documents the operator escape hatch: after a bad
// deploy is fixed, the old window describes a configuration that no longer
// exists and waiting it out is pure downtime.
func TestResetClearsTheWindow(t *testing.T) {
	b, window, _ := newTestBreaker()
	for i := 0; i < 4; i++ {
		window.RecordFailure(false)
		b.RecordFailure(false)
	}
	b.Reset()
	if got := b.State(); got != StateClosed {
		t.Fatalf("state = %s after Reset, want closed", got)
	}
	if got := window.Snapshot(); got.Attempts != 0 {
		t.Fatalf("Reset left %d attempts in the window", got.Attempts)
	}
}

// TestStateDoesNotConsumeTheProbe pins the fix for a real bug: the router used
// to call Allow() to rank candidates, which ate the single half-open probe and
// left the breaker wedged forever.
func TestStateDoesNotConsumeTheProbe(t *testing.T) {
	b, window, advance := newTestBreaker()
	for i := 0; i < 4; i++ {
		window.RecordFailure(false)
		b.RecordFailure(false)
	}
	advance(11 * time.Second)

	if got := b.State(); got != StateOpen {
		// Time alone does not move the state; the transition happens on the next
		// Allow. Reading must report what is true, not what is about to be.
		t.Fatalf("State = %s before any Allow, want open", got)
	}
	if d := b.Allow(); !d.Probe {
		t.Fatalf("the probe was already consumed: %+v", d)
	}
}

func TestGroupAlwaysReturnsABreaker(t *testing.T) {
	g := NewGroup(testHealthConfig(), nil)
	b := g.Get("never-configured")
	if b == nil {
		t.Fatal("Get returned nil for an unknown upstream")
	}
	if d := b.Allow(); !d.Allowed {
		t.Fatalf("a breaker for an unknown upstream refused traffic: %+v", d)
	}
	if got := g.Names(); len(got) != 1 || got[0] != "never-configured" {
		t.Fatalf("Names = %v, want the lazily created upstream", got)
	}
}
