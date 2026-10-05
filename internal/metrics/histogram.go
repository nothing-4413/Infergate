package metrics

import (
	"math"
	"sort"
	"strconv"
)

// Bucket is one cumulative bucket of a histogram: Count is the number of
// observations that were less than or equal to UpperBound.
//
// Cumulative rather than per-bucket because that is the shape Prometheus
// exposes: the text format has no per-bucket series, only an `le`
// (less-than-or-equal) series per boundary, and a query that wants a range
// subtracts two of them. Handing the caller cumulative counts means the
// arithmetic happens once here rather than once per scrape in the server.
type Bucket struct {
	// UpperBound is the inclusive upper bound, in seconds for a duration
	// histogram or in tokens for a size histogram. It is +Inf for the final
	// catch-all bucket.
	UpperBound float64

	// Count is the cumulative number of observations at or below UpperBound,
	// including every earlier bucket.
	Count int64
}

// IsInf reports whether this is the final +Inf catch-all bucket.
func (b Bucket) IsInf() bool { return math.IsInf(b.UpperBound, 1) }

// Le renders the boundary as a Prometheus `le` label value.
//
// +Inf is spelled exactly as the exposition format requires, and every other
// bound is printed in its shortest round-trippable form so that a bound of
// 0.001 stays "0.001" instead of drifting to "0.0010000000000000001" and
// breaking a dashboard that matches on the label.
func (b Bucket) Le() string {
	if b.IsInf() {
		return "+Inf"
	}
	return strconv.FormatFloat(b.UpperBound, 'g', -1, 64)
}

// Histogram is a fixed-bucket histogram over durations or sizes.
//
// Buckets rather than raw samples because a gateway runs for weeks: keeping
// every request duration so a percentile can be computed later turns a latency
// feature into a memory leak. This is the same argument internal/stats makes
// for its window, applied at the package the /metrics endpoint reads.
//
// A Histogram is NOT safe for concurrent use on its own. Its owner holds the
// lock: Recorder already serialises all of its state behind one mutex, so a
// second lock per histogram would double the critical-section count on the
// request path for no benefit.
type Histogram struct {
	// bounds are the finite, ascending, de-duplicated upper bounds. Everything
	// above bounds[len(bounds)-1] lands in the implicit +Inf bucket.
	bounds []float64

	// counts[i] counts observations in (bounds[i-1], bounds[i]]; the last
	// element is the +Inf bucket. len(counts) == len(bounds)+1 and the values
	// are deliberately NOT cumulative, so one observation touches one element.
	counts []int64

	count int64
	sum   float64
}

// NewHistogram returns an empty histogram over the given finite upper bounds.
//
// The bounds are copied, so a caller may pass a shared package-level slice
// without the histogram seeing later edits to it. Non-finite bounds are
// dropped: +Inf is implicit as the last bucket and cannot also be a finite
// boundary, and NaN would make sort.SearchFloat64s silently misplace
// observations.
func NewHistogram(bounds []float64) *Histogram {
	b := make([]float64, 0, len(bounds))
	for _, v := range bounds {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		b = append(b, v)
	}
	sort.Float64s(b)

	// Collapse duplicates in place. Two identical bounds would create a bucket
	// that can never receive an observation: a silent hole in the output that
	// an operator would read as "no requests in this range" rather than as a
	// bug in the bound list.
	out := b[:0]
	for i, v := range b {
		if i > 0 && v == b[i-1] {
			continue
		}
		out = append(out, v)
	}

	return &Histogram{
		bounds: out,
		counts: make([]int64, len(out)+1),
	}
}

// Observe records one observation.
//
// A negative or NaN value is clamped to 0 rather than rejected. Observations
// come from wall-clock arithmetic (time.Since on the request path, an
// upstream's self-reported duration): an NTP step or a clock read that went
// backwards produces a small negative duration, and the honest reading of that
// is "too fast to measure", not "a request that finished before it started".
// Letting it through would also corrupt the exported sum -- the one value every
// dashboard divides by the count to get a mean -- for the whole process
// lifetime.
func (h *Histogram) Observe(v float64) {
	if math.IsNaN(v) || v < 0 {
		v = 0
	}
	h.count++
	h.sum += v
	// SearchFloat64s returns the first index whose bound is >= v, so an
	// observation exactly on a boundary lands in that boundary's bucket, which
	// is what `le` (less-than-or-equal) promises.
	h.counts[sort.SearchFloat64s(h.bounds, v)]++
}

// HistogramSnapshot is an immutable copy of a histogram's state.
type HistogramSnapshot struct {
	// Bounds are the finite upper bounds, ascending. The implicit +Inf bucket
	// is not listed here.
	Bounds []float64

	// Counts holds one NON-cumulative count per bucket: Counts[i] is the number
	// of observations in (Bounds[i-1], Bounds[i]], and the final element is the
	// +Inf bucket. len(Counts) == len(Bounds)+1, so sum(Counts) == Count.
	Counts []int64

	// Count is the total number of observations and Sum their total, so
	// Sum/Count is the mean over the histogram's whole lifetime.
	Count int64
	Sum   float64
}

// Snapshot copies the histogram. The copy is what lets a reader use the state
// after its owner has released the lock.
func (h *Histogram) Snapshot() HistogramSnapshot {
	return HistogramSnapshot{
		Bounds: append([]float64(nil), h.bounds...),
		Counts: append([]int64(nil), h.counts...),
		Count:  h.count,
		Sum:    h.sum,
	}
}

// Buckets returns the cumulative bucket list, ending with the +Inf catch-all.
//
// The last bucket's Count always equals Snapshot.Count, so an exposition writer
// can emit the slice in order without carrying its own running total.
func (s HistogramSnapshot) Buckets() []Bucket {
	return cumulativeBuckets(s.Bounds, s.Counts)
}

// cumulativeBuckets folds non-cumulative counts into the `le` form. It is
// shared by HistogramSnapshot and the flattened Recorder row types so that the
// +Inf bucket is appended in exactly one place.
func cumulativeBuckets(bounds []float64, counts []int64) []Bucket {
	out := make([]Bucket, 0, len(counts))
	var cum int64
	for i, b := range bounds {
		cum += counts[i]
		out = append(out, Bucket{UpperBound: b, Count: cum})
	}
	if len(counts) > 0 {
		cum += counts[len(counts)-1]
	}
	return append(out, Bucket{UpperBound: math.Inf(1), Count: cum})
}

// DefaultDurationBuckets returns the request- and attempt-duration boundaries,
// in seconds.
//
// The upper part of the set is Prometheus' own default duration buckets (5ms
// through 10s), extended downward with 1ms and 2.5ms: this gateway's own
// overhead -- the number an operator looks at when the upstreams are healthy --
// lives below 5ms and would otherwise hide inside the first bucket. Above 10s a
// request has already lost its user, so a finer split there answers nothing.
//
// The spacing is roughly geometric on purpose. A quantile read off a histogram
// is interpolated within its bucket, so equal-width buckets would put 90% of a
// healthy gateway's traffic in the first one and 90% of an incident's traffic
// in the last, making both readings useless.
func DefaultDurationBuckets() []float64 {
	return append([]float64(nil), defaultDurationBuckets...)
}

// DefaultFirstTokenBuckets returns the time-to-first-token boundaries, in
// seconds.
//
// TTFT for a streamed completion is dominated by the upstream's prefill, which
// is tens to hundreds of milliseconds: it cannot be under 5ms for a real model,
// and above 2.5s the user has already concluded the product is broken. The set
// is therefore tighter and shifted up from the request-duration set.
func DefaultFirstTokenBuckets() []float64 {
	return append([]float64(nil), defaultFirstTokenBuckets...)
}

// DefaultTokensPerRequestBuckets returns the completion-token boundaries for
// one request.
//
// Powers of two because completion length is heavy-tailed and is read in
// magnitudes: an operator asks "is this route returning ~100 tokens or ~2000",
// not "did this request return exactly 96". The top bound is above any
// completion length a configured max_tokens will allow, so the +Inf bucket is
// the visible signal that a provider ignored the cap.
func DefaultTokensPerRequestBuckets() []float64 {
	return append([]float64(nil), defaultTokensPerRequestBuckets...)
}

var (
	defaultDurationBuckets = []float64{
		0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
	}
	defaultFirstTokenBuckets = []float64{
		0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5,
	}
	defaultTokensPerRequestBuckets = []float64{
		1, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096,
	}
)
