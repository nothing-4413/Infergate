// Package stats keeps the sliding-window health and latency picture that the
// router uses to rank upstreams and that the circuit breaker uses to trip.
//
// Design notes worth knowing before changing anything here:
//
//   - The window is a ring of time buckets, not a list of raw samples. A busy
//     gateway produces millions of samples an hour, and keeping them to compute
//     a percentile would turn a latency feature into a memory leak. Buckets give
//     O(1) recording and O(buckets) reporting.
//
//   - Within a bucket we keep an EXPONENTIAL MOVING AVERAGE rather than
//     sum/count. A bucket that spans many requests must not let one 30-second
//     timeout dominate its mean: the router wants "how fast is this backend
//     right now", and an EMA answers that. It also means the value decays
//     smoothly instead of jumping when a bucket rolls out of the window.
//
//   - Every sample is clamped to maxSample. A single request that hung for ten
//     minutes (a killed client, a suspended laptop) would otherwise poison the
//     average for the whole window and make a healthy backend look terrible.
//
// The zero value is not usable; construct with New.
package stats

import (
	"sync"
	"time"
)

// maxSample bounds a single observation. Above this a measurement stops being
// information about the backend and starts being information about the host:
// process suspension, GC pause, laptop sleep. Clamping keeps such an event from
// permanently skewing the window while still counting as a slow request.
const maxSample = 2 * time.Minute

// EWMAAlpha is the weight of a new sample in a bucket's moving average.
//
// 0.2 was chosen so that a bucket needs roughly five requests before its average
// is meaningfully stable, and so that a step change in backend latency is
// reflected within a few requests without the value ever fully forgetting the
// past inside the bucket. Larger values chase noise; smaller values hide a
// regression for the whole bucket.
const EWMAAlpha = 0.2

// bucket accumulates one slice of the window.
type bucket struct {
	start time.Time

	attempts  int64
	successes int64
	failures  int64
	timeouts  int64

	// latency is the EMA of the whole request duration in seconds, sampled on
	// success only. Failures are counted separately: mixing them in would make
	// a backend that fails instantly look like the fastest one available.
	latency  float64
	latencyN int64
	ttft     float64
	ttftN    int64
}

func (b *bucket) reset(start time.Time) {
	*b = bucket{start: start}
}

func (b *bucket) addAvg(dst *float64, n *int64, seconds float64) {
	if *n == 0 {
		*dst = seconds
	} else {
		*dst += EWMAAlpha * (seconds - *dst)
	}
	*n++
}

// Stats is the per-upstream sliding window. It is safe for concurrent use.
type Stats struct {
	mu      sync.Mutex
	buckets []bucket
	span    time.Duration
	step    time.Duration
	now     func() time.Time
}

// Snapshot is a consistent read of the window.
type Snapshot struct {
	// Attempts, Successes, Failures and Timeouts are windowed counts.
	Attempts  int64
	Successes int64
	Failures  int64
	Timeouts  int64

	// FailureRatio is Failures/Attempts in the window, 0 when there were none.
	// A backend with no traffic reports 0 rather than 1 so that "unknown" never
	// looks like "broken".
	FailureRatio float64

	// LatencyMean is the windowed mean request duration over successful
	// attempts. Zero means no successful attempt was observed.
	LatencyMean time.Duration

	// TTFBMean is the windowed mean time-to-first-byte over streams that
	// produced at least one visible token.
	TTFBMean time.Duration

	// HasLatency distinguishes "measured zero" from "never measured". A router
	// that treats unmeasured as zero would route all traffic to the backend
	// that has never answered.
	HasLatency bool

	// LastFailure is the time of the most recent recorded failure, zero if the
	// window holds none. The breaker uses it to decide when to allow a probe
	// even if nothing has been recorded since the breaker opened.
	LastFailure time.Time
}

// New builds a Stats covering span, divided into buckets slices.
//
// buckets is clamped to at least 1 and at most 1024, and span to at least one
// second. A caller asking for a 1ns window would create a ring that resets on
// every request, which silently disables every statistic it feeds.
func New(span time.Duration, buckets int) *Stats {
	if span < time.Second {
		span = time.Second
	}
	if buckets < 1 {
		buckets = 1
	}
	if buckets > 1024 {
		buckets = 1024
	}
	s := &Stats{
		buckets: make([]bucket, buckets),
		span:    span,
		step:    span / time.Duration(buckets),
		now:     time.Now,
	}
	return s
}

// SetClock replaces the time source. Tests use it to advance the window without
// sleeping; production never calls it.
func (s *Stats) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// bucketFor returns the bucket responsible for now, rolling the ring forward
// and clearing the buckets that time has passed over. Clearing (rather than
// leaving stale data) is what makes the window actually slide.
func (s *Stats) bucketFor(now time.Time) *bucket {
	idx := int(now.UnixNano()/int64(s.step)) % len(s.buckets)
	if idx < 0 {
		idx += len(s.buckets)
	}
	b := &s.buckets[idx]
	// A bucket whose start is not within one bucket-width of this moment is
	// either freshly reused after a full lap or never written. Either way its
	// contents describe a different slice of time and must be discarded.
	if b.start.IsZero() || now.Sub(b.start) >= s.step || b.start.After(now) {
		b.reset(now.Truncate(s.step))
	}
	return b
}

func clampSample(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	if d > maxSample {
		return maxSample.Seconds()
	}
	return d.Seconds()
}

// RecordSuccess records one completed request that produced a usable answer.
func (s *Stats) RecordSuccess(latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bucketFor(s.now())
	b.attempts++
	b.successes++
	b.addAvg(&b.latency, &b.latencyN, clampSample(latency))
}

// RecordFailure records one request that ended without an answer. A timeout is
// also a failure; it additionally carries the timeout flag, because a timeout
// means "slow" while a connection error means "gone", and the two are worth
// distinguishing when reading the fleet's health.
func (s *Stats) RecordFailure(timeout bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	b := s.bucketFor(now)
	b.attempts++
	b.failures++
	if timeout {
		b.timeouts++
	}
}

// RecordFirstToken records the time-to-first-byte of one stream. It is counted
// separately from latency because a streaming request that dribbles tokens for
// a minute has an unremarkable TTFB and a terrible latency, and only the first
// number predicts how the user experienced the start of the answer.
func (s *Stats) RecordFirstToken(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bucketFor(s.now())
	b.addAvg(&b.ttft, &b.ttftN, clampSample(d))
}

// Snapshot sums the live buckets.
//
// Buckets are summed, not averaged, because a quiet slice of the window must
// not carry the same weight as a busy one, and because failures have no
// meaningful average. The latency figures inside a bucket are already averages,
// so the returned mean weights each bucket by its sample count.
func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.bucketFor(now) // force the ring to the current slice

	var out Snapshot
	var latencyWeighted, ttftWeighted float64
	for i := range s.buckets {
		b := &s.buckets[i]
		if b.start.IsZero() || now.Sub(b.start) >= s.span {
			continue
		}
		out.Attempts += b.attempts
		out.Successes += b.successes
		out.Failures += b.failures
		out.Timeouts += b.timeouts
		if b.failures > 0 {
			out.LastFailure = b.start
		}
		if b.latencyN > 0 {
			latencyWeighted += b.latency * float64(b.latencyN)
			out.HasLatency = true
		}
		if b.ttftN > 0 {
			ttftWeighted += b.ttft * float64(b.ttftN)
		}
	}
	if out.Attempts > 0 {
		out.FailureRatio = float64(out.Failures) / float64(out.Attempts)
	}
	if out.HasLatency {
		out.LatencyMean = time.Duration(latencyWeighted / float64(out.Successes) * float64(time.Second))
	}
	if out.Successes > 0 && ttftWeighted > 0 {
		// TTFB is normalised by successes rather than by its own sample count,
		// so a single measured stream among many non-streaming requests still
		// reports the latency it actually saw instead of being diluted to
		// near-zero. It is a property of the backend, not of the mix.
		out.TTFBMean = time.Duration(ttftWeighted / float64(out.Successes) * float64(time.Second))
	}
	return out
}

// Reset clears every bucket. Used when an operator deliberately changes the
// fleet, where old failures describe a configuration that no longer exists.
func (s *Stats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.buckets {
		s.buckets[i] = bucket{}
	}
}
