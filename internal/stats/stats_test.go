package stats

import (
	"testing"
	"time"
)

// newTestStats pins the clock so the window can be advanced without sleeping:
// a test that sleeps for its window is slow and, on a loaded machine, flaky.
func newTestStats(span time.Duration, buckets int) (*Stats, func(time.Duration)) {
	s := New(span, buckets)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	return s, func(d time.Duration) { now = now.Add(d) }
}

func TestFailureRatioAndCounts(t *testing.T) {
	s, _ := newTestStats(time.Minute, 6)

	for i := 0; i < 3; i++ {
		s.RecordSuccess(10 * time.Millisecond)
	}
	s.RecordFailure(false)
	s.RecordFailure(true)

	snap := s.Snapshot()
	if snap.Attempts != 5 || snap.Successes != 3 || snap.Failures != 2 || snap.Timeouts != 1 {
		t.Fatalf("counts = %+v, want attempts=5 successes=3 failures=2 timeouts=1", snap)
	}
	if got, want := snap.FailureRatio, 0.4; got != want {
		t.Fatalf("FailureRatio = %v, want %v", got, want)
	}
	if !snap.HasLatency {
		t.Fatal("HasLatency = false after recording successes")
	}
	if got := snap.LatencyMean; got != 10*time.Millisecond {
		t.Fatalf("LatencyMean = %v, want 10ms", got)
	}
}

// TestEmptyWindowReportsNoLatency is the distinction the router depends on: a
// backend that has never answered must not look like the fastest one.
func TestEmptyWindowReportsNoLatency(t *testing.T) {
	s, _ := newTestStats(time.Minute, 6)
	snap := s.Snapshot()
	if snap.HasLatency {
		t.Fatal("HasLatency = true on an empty window")
	}
	if snap.FailureRatio != 0 {
		t.Fatalf("FailureRatio = %v on an empty window, want 0 (unknown is not broken)", snap.FailureRatio)
	}
	if snap.Attempts != 0 {
		t.Fatalf("Attempts = %d on an empty window", snap.Attempts)
	}
}

// TestWindowSlidesAway is the whole point of a ring of buckets: yesterday's
// failures must stop counting, or a breaker built on the window can never close.
func TestWindowSlidesAway(t *testing.T) {
	s, advance := newTestStats(time.Minute, 6)

	s.RecordFailure(false)
	s.RecordFailure(false)
	if got := s.Snapshot().Failures; got != 2 {
		t.Fatalf("Failures = %d before advancing, want 2", got)
	}

	advance(2 * time.Minute)

	snap := s.Snapshot()
	if snap.Attempts != 0 || snap.Failures != 0 {
		t.Fatalf("window did not slide: %+v", snap)
	}
	if snap.FailureRatio != 0 {
		t.Fatalf("FailureRatio = %v after the window slid, want 0", snap.FailureRatio)
	}
}

// TestSlowSampleIsClamped guards the window against one hung request poisoning
// the mean for every request that follows.
func TestSlowSampleIsClamped(t *testing.T) {
	s, _ := newTestStats(time.Minute, 6)
	s.RecordSuccess(time.Hour)
	if got := s.Snapshot().LatencyMean; got != maxSample {
		t.Fatalf("LatencyMean = %v, want it clamped to %v", got, maxSample)
	}
}

// TestFirstTokenIsAveragedSeparately documents why TTFB has its own accumulator:
// a stream that dribbles tokens has an unremarkable TTFB and a terrible total.
func TestFirstTokenIsAveragedSeparately(t *testing.T) {
	s, _ := newTestStats(time.Minute, 6)
	s.RecordSuccess(2 * time.Second)
	s.RecordFirstToken(100 * time.Millisecond)

	snap := s.Snapshot()
	if got := snap.LatencyMean; got != 2*time.Second {
		t.Fatalf("LatencyMean = %v, want 2s", got)
	}
	if got := snap.TTFBMean; got != 100*time.Millisecond {
		t.Fatalf("TTFBMean = %v, want 100ms", got)
	}
}

func TestResetClearsEverything(t *testing.T) {
	s, _ := newTestStats(time.Minute, 6)
	s.RecordFailure(true)
	s.Reset()
	if got := s.Snapshot(); got.Attempts != 0 || got.Failures != 0 {
		t.Fatalf("Reset left %+v", got)
	}
}

func TestNewClampsPathologicalWindow(t *testing.T) {
	s := New(time.Nanosecond, 0)
	s.RecordSuccess(time.Millisecond)
	if got := s.Snapshot(); got.Attempts != 1 {
		t.Fatalf("a clamped window lost its sample: %+v", got)
	}
}
