// Package breaker implements a per-upstream circuit breaker over a windowed
// failure ratio.
//
// The state machine is the classic three-state one:
//
//	closed    -- normal operation; trips to open when the windowed failure ratio
//	             reaches the threshold with enough observations behind it
//	open      -- the upstream is skipped entirely; after OpenDuration elapses it
//	             moves to half-open on the next request
//	half-open  -- exactly one probe is allowed through at a time; consecutive
//	             successes close the breaker, any failure re-opens it
//
// Two decisions are worth recording because they are the ones that go wrong in
// production:
//
//  1. The breaker observes WINDOWED statistics, never lifetime counters. A
//     lifetime ratio cannot recover: an upstream that failed a thousand times
//     yesterday stays "mostly failed" forever, so a breaker built on one either
//     never closes again or has to be reset by hand.
//
//  2. Half-open admits one probe at a time, not one per waiting goroutine. If
//     it admitted all of them, the first request after OpenDuration would
//     re-create the stampede that the breaker exists to stop.
package breaker

import (
	"sync"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/stats"
)

// State is the breaker's current state.
type State string

const (
	// StateClosed passes traffic and counts outcomes.
	StateClosed State = "closed"

	// StateOpen rejects traffic without contacting the upstream.
	StateOpen State = "open"

	// StateHalfOpen lets a single probe through.
	StateHalfOpen State = "half-open"
)

// Breaker tracks one upstream's health. Safe for concurrent use.
type Breaker struct {
	name string
	cfg  config.HealthConfig

	mu sync.Mutex

	// stats is the shared window; the breaker reads it to trip and close, and
	// the router reads the same window to rank candidates. One window means the
	// router and the breaker can never disagree about how an upstream is doing.
	stats *stats.Stats

	state State

	// openedAt is when the breaker last opened; the transition to half-open is
	// time-based, so no background timer is needed (a timer per upstream would
	// be a goroutine and a wakeup per upstream for no information gain).
	openedAt time.Time

	// probeInFlight guards the half-open state against admitting more than one
	// concurrent probe.
	probeInFlight bool

	// halfOpenSuccesses counts consecutive successes since the probe started.
	halfOpenSuccesses int

	now func() time.Time

	// trips and rejections are lifetime observability counters. They are kept
	// separately from the window precisely because they must NOT decay: an
	// operator asking "did this breaker ever flap" needs the total.
	trips      int64
	rejections int64
}

// New builds a breaker over the supplied window.
func New(name string, cfg config.HealthConfig, window *stats.Stats) *Breaker {
	if window == nil {
		window = stats.New(cfg.Window.Duration(), cfg.Buckets)
	}
	return &Breaker{
		name:  name,
		cfg:   cfg,
		stats: window,
		state: StateClosed,
		now:   time.Now,
	}
}

// SetClock replaces the time source for tests.
func (b *Breaker) SetClock(now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = now
}

// Stats exposes the shared window so the router and the admin endpoint read the
// same numbers the breaker trips on.
func (b *Breaker) Stats() *stats.Stats { return b.stats }

// State reports the current state without observing a request.
//
// It exists so that a caller which only needs to RANK upstreams can read health
// without consuming a half-open probe. Allow is a state transition, not a
// query: calling it twice for one request -- once to sort candidates, once to
// admit the attempt -- makes the second call reject the very probe the first
// admitted, and permanently wedges the breaker half-open.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Decision is the answer to "may I use this upstream right now".
type Decision struct {
	// Allowed is false when the breaker is open, or when it is half-open and a
	// probe is already in flight.
	Allowed bool

	// State is the state the decision was made in.
	State State

	// Probe is true when this call was admitted AS the half-open probe, so the
	// caller must report the outcome back via RecordSuccess/RecordFailure even
	// if it abandons the attempt for an unrelated reason. Otherwise the breaker
	// stays stuck half-open forever.
	Probe bool

	// Reason is a short human-readable explanation for logs and the admin API.
	Reason string
}

// Allow decides whether one attempt may proceed.
func (b *Breaker) Allow() Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()

	if b.state == StateOpen {
		if now.Sub(b.openedAt) < b.cfg.OpenDuration.Duration() {
			b.rejections++
			return Decision{State: StateOpen, Reason: "circuit open"}
		}
		// Cooldown elapsed: degrade to half-open and let this caller probe.
		b.state = StateHalfOpen
		b.probeInFlight = false
		b.halfOpenSuccesses = 0
	}

	if b.state == StateHalfOpen {
		if b.probeInFlight {
			b.rejections++
			return Decision{State: StateHalfOpen, Reason: "half-open probe already in flight"}
		}
		b.probeInFlight = true
		return Decision{Allowed: true, State: StateHalfOpen, Probe: true, Reason: "half-open probe"}
	}

	return Decision{Allowed: true, State: StateClosed, Reason: "closed"}
}

// RecordSuccess reports a completed, usable answer.
//
// It deliberately does NOT write to the window: the proxy records real latency
// into the shared stats.Stats, and the breaker only reads it. If the breaker
// also recorded a success it would need a latency value it does not have, and
// inventing one (zero) would drag the router's latency estimate toward zero for
// whichever backend it was called for.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeInFlight = false
	if b.state == StateHalfOpen {
		b.halfOpenSuccesses++
		if b.halfOpenSuccesses >= b.cfg.HalfOpenProbes {
			b.state = StateClosed
			b.halfOpenSuccesses = 0
			// Deliberately does NOT clear the window. Clearing it would erase
			// the evidence that the upstream just recovered, and also the
			// failures that made the trip necessary -- both of which the admin
			// endpoint is expected to show. The window expires on its own.
		}
	}
}

// RecordFailure reports an attempt that ended without an answer. timeout
// distinguishes a slow upstream from an unreachable one for observability.
//
// Like RecordSuccess it leaves the window to the proxy and only reads it here.
func (b *Breaker) RecordFailure(timeout bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeInFlight = false

	if b.state == StateHalfOpen {
		// A probe that fails re-opens the breaker immediately. Waiting for the
		// failure ratio to cross the threshold would require enough traffic to
		// produce that ratio, and in half-open there is no traffic by design.
		b.trip()
		return
	}
	if b.state == StateOpen {
		return
	}

	snap := b.stats.Snapshot()
	if snap.Attempts < int64(b.cfg.MinRequests) {
		return
	}
	if snap.FailureRatio >= b.cfg.FailureRatio {
		b.trip()
	}
}

// trip opens the breaker. The caller must hold b.mu.
func (b *Breaker) trip() {
	if b.state != StateOpen {
		b.trips++
	}
	b.state = StateOpen
	b.openedAt = b.now()
	b.halfOpenSuccesses = 0
	b.probeInFlight = false
}

// Reset returns the breaker to closed and clears the window. It exists for the
// admin API: after an operator fixes a backend, waiting out the window is
// pointless because the window describes the broken configuration.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateClosed
	b.openedAt = time.Time{}
	b.probeInFlight = false
	b.halfOpenSuccesses = 0
	b.stats.Reset()
}

// Report is a consistent read of the breaker for logs, metrics and the admin
// endpoint.
type Report struct {
	Name    string `json:"name"`
	State   State  `json:"state"`
	Trips   int64  `json:"trips"`
	Rejects int64  `json:"rejections"`

	// Stats is the windowed picture the breaker is acting on.
	Attempts       int64   `json:"attempts"`
	Failures       int64   `json:"failures"`
	Timeouts       int64   `json:"timeouts"`
	FailureRatio   float64 `json:"failure_ratio"`
	LatencyMeanMS  float64 `json:"latency_mean_ms"`
	TTFBMeanMS     float64 `json:"ttft_mean_ms"`
	HasLatency     bool    `json:"has_latency"`
	OpenedForMS    float64 `json:"opened_for_ms,omitempty"`
}

// Report snapshots the breaker and its window.
func (b *Breaker) Report() Report {
	b.mu.Lock()
	snap := b.stats.Snapshot()
	state := b.state
	openedAt := b.openedAt
	trips := b.trips
	rejects := b.rejections
	now := b.now()
	b.mu.Unlock()

	r := Report{
		Name:         b.name,
		State:        state,
		Trips:        trips,
		Rejects:      rejects,
		Attempts:     snap.Attempts,
		Failures:     snap.Failures,
		Timeouts:     snap.Timeouts,
		FailureRatio: snap.FailureRatio,
		HasLatency:   snap.HasLatency,
	}
	if snap.HasLatency {
		r.LatencyMeanMS = float64(snap.LatencyMean) / float64(time.Millisecond)
	}
	if snap.TTFBMean > 0 {
		r.TTFBMeanMS = float64(snap.TTFBMean) / float64(time.Millisecond)
	}
	if state == StateOpen && !openedAt.IsZero() {
		r.OpenedForMS = float64(now.Sub(openedAt)) / float64(time.Millisecond)
	}
	return r
}
