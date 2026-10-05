package metrics

import (
	"math"
	"sort"
	"sync"
	"testing"
	"time"
)

// closeEnough compares floats that were produced by different orders of
// addition. Durations that are stored as seconds are not exactly representable,
// so an exact comparison would fail for reasons unrelated to the code under
// test.
func closeEnough(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestHistogramObservePlacement pins the bucket arithmetic: the bucket is
// inclusive of its upper bound (which is what `le` promises a scraper), values
// above the largest finite bound land in +Inf, and a negative or NaN value is
// clamped to 0 rather than corrupting the sum.
func TestHistogramObservePlacement(t *testing.T) {
	h := NewHistogram([]float64{1, 2, 4})

	for _, v := range []float64{
		0.5,        // bucket 0, below the first bound
		1,          // bucket 0, exactly on the bound
		1.5,        // bucket 1, between bounds
		2,          // bucket 1, exactly on the bound
		5,          // +Inf, above every finite bound
		-3,         // clamped to 0, bucket 0
		math.NaN(), // clamped to 0, bucket 0
	} {
		h.Observe(v)
	}

	snap := h.Snapshot()

	// counts are per-bucket and non-cumulative: [0,1] [1,2] [2,4] (+Inf].
	// Four values land in the first bucket: 0.5, 1, and the two clamped zeroes.
	wantCounts := []int64{4, 2, 0, 1}
	if len(snap.Counts) != len(wantCounts) {
		t.Fatalf("Counts length = %d, want %d (%v)", len(snap.Counts), len(wantCounts), snap.Counts)
	}
	for i, want := range wantCounts {
		if snap.Counts[i] != want {
			t.Errorf("Counts[%d] = %d, want %d (all: %v)", i, snap.Counts[i], want, snap.Counts)
		}
	}

	if snap.Count != 7 {
		t.Errorf("Count = %d, want 7", snap.Count)
	}
	// 0.5 + 1 + 1.5 + 2 + 5, plus two clamped zeroes.
	if want := 10.0; !closeEnough(snap.Sum, want) {
		t.Errorf("Sum = %v, want %v; a negative or NaN observation leaked into the sum", snap.Sum, want)
	}

	buckets := snap.Buckets()
	if len(buckets) != len(wantCounts) {
		t.Fatalf("Buckets length = %d, want %d", len(buckets), len(wantCounts))
	}
	// Cumulative form, ending with the catch-all.
	wantCumulative := []int64{4, 6, 6, 7}
	for i, want := range wantCumulative {
		if buckets[i].Count != want {
			t.Errorf("Buckets[%d].Count = %d, want %d", i, buckets[i].Count, want)
		}
	}
	if !buckets[len(buckets)-1].IsInf() {
		t.Errorf("last bucket %v is not the +Inf catch-all", buckets[len(buckets)-1].UpperBound)
	}
	if got := buckets[len(buckets)-1].Le(); got != "+Inf" {
		t.Errorf("catch-all Le() = %q, want \"+Inf\"", got)
	}
}

// TestHistogramInvariants states the two identities the exposition writer and
// every downstream query rely on: the buckets partition the observations, and
// the exported sum is the sum of what was observed.
func TestHistogramInvariants(t *testing.T) {
	h := NewHistogram(DefaultDurationBuckets())

	var (
		want     float64
		observed int
	)
	for i := 0; i < 1000; i++ {
		// Sweep across every bucket and past the end of the last one.
		v := float64(i) * 0.0125
		h.Observe(v)
		want += v
		observed++
	}
	h.Observe(-1) // clamped, contributes 0

	snap := h.Snapshot()
	if snap.Count != int64(observed+1) {
		t.Fatalf("Count = %d, want %d", snap.Count, observed+1)
	}
	if !closeEnough(snap.Sum, want) {
		t.Errorf("Sum = %v, want %v", snap.Sum, want)
	}

	var total int64
	for _, c := range snap.Counts {
		total += c
	}
	if total != snap.Count {
		t.Errorf("sum(Counts) = %d, but Count = %d", total, snap.Count)
	}
	if got := len(snap.Counts); got != len(snap.Bounds)+1 {
		t.Errorf("len(Counts) = %d, want len(Bounds)+1 = %d", got, len(snap.Bounds)+1)
	}

	buckets := snap.Buckets()
	if last := buckets[len(buckets)-1]; last.Count != snap.Count {
		t.Errorf("final +Inf bucket = %d, want Count = %d", last.Count, snap.Count)
	}
	var prev int64
	for i, b := range buckets {
		if b.Count < prev {
			t.Errorf("bucket %d is not cumulative: %d after %d", i, b.Count, prev)
		}
		prev = b.Count
	}
}

// TestHistogramBoundsAreCopiedAndSanitised covers the two ways a shared bound
// slice could silently poison a histogram: later mutation of the caller's
// slice, and a bound list containing values that cannot be a finite boundary.
func TestHistogramBoundsAreCopiedAndSanitised(t *testing.T) {
	bounds := []float64{3, 1, math.NaN(), 2, 3, math.Inf(1), math.Inf(-1)}
	h := NewHistogram(bounds)

	// The caller keeps its slice; a later edit must not move a boundary.
	bounds[0] = 100

	snap := h.Snapshot()
	want := []float64{1, 2, 3}
	if len(snap.Bounds) != len(want) {
		t.Fatalf("Bounds = %v, want %v (NaN/+-Inf dropped, duplicates collapsed)", snap.Bounds, want)
	}
	for i, w := range want {
		if snap.Bounds[i] != w {
			t.Fatalf("Bounds = %v, want %v", snap.Bounds, want)
		}
	}

	// A bound on the second boundary must be reported in its shortest
	// round-trippable form; a dashboard matches on this label text, and 0.001
	// drifting to "0.0010000000000000001" would silently break it.
	h.Observe(1.5)
	if got := h.Snapshot().Buckets()[1].Le(); got != "2" {
		t.Errorf("Le() for the bound 2 = %q, want %q", got, "2")
	}
	ms := Bucket{UpperBound: 0.001}
	if got := ms.Le(); got != "0.001" {
		t.Errorf("Le() for 0.001 = %q, want %q", got, "0.001")
	}
}

// TestHistogramEmptyBuckets documents the empty case: a family with no series
// must still produce the +Inf bucket so the exposition writer never emits a
// histogram whose buckets do not add up.
func TestHistogramEmptyBuckets(t *testing.T) {
	buckets := NewHistogram(DefaultDurationBuckets()).Snapshot().Buckets()
	if len(buckets) != len(DefaultDurationBuckets())+1 {
		t.Fatalf("empty histogram has %d buckets, want %d", len(buckets), len(DefaultDurationBuckets())+1)
	}
	for i, b := range buckets {
		if b.Count != 0 {
			t.Errorf("empty histogram bucket %d = %d, want 0", i, b.Count)
		}
	}

	// A zero-bound histogram is degenerate but must not panic: every
	// observation lands in +Inf.
	z := NewHistogram(nil)
	z.Observe(1.5)
	if snap := z.Snapshot(); snap.Count != 1 || len(snap.Counts) != 1 {
		t.Fatalf("bound-less histogram = %+v, want one observation in the +Inf bucket", snap)
	}
}

// TestRecorderLatencyWindow checks the leak fix directly: the ring keeps the
// most recent N samples in insertion order, counts what it evicted, and Reset
// clears both the samples and the drop counter.
func TestRecorderLatencyWindow(t *testing.T) {
	const window = 4
	r := NewRecorderWithWindow(window)

	if got := r.RequestLatencyWindow(); got != window {
		t.Fatalf("RequestLatencyWindow() = %d, want %d", got, window)
	}

	// Fewer observations than the window: nothing is dropped and the samples
	// come back in the order they were observed.
	for i := 1; i <= 3; i++ {
		r.ObserveRequest("/v1/chat/completions", "local", "m", 200, OutcomeSuccess, time.Duration(i)*time.Millisecond)
	}
	assertSamples(t, r.RequestLatencySamples(), millis(1, 2, 3))
	if got := r.RequestLatencyDropped(); got != 0 {
		t.Errorf("dropped = %d before the window filled, want 0", got)
	}

	// Push past capacity: the oldest samples go and the newest window survives.
	for i := 4; i <= 10; i++ {
		r.ObserveRequest("/v1/chat/completions", "local", "m", 200, OutcomeSuccess, time.Duration(i)*time.Millisecond)
	}
	assertSamples(t, r.RequestLatencySamples(), millis(7, 8, 9, 10))
	if got := r.RequestLatencyDropped(); got != 6 {
		t.Errorf("dropped = %d, want 6", got)
	}

	// The ring must stay in insertion order across more than one wrap, which is
	// the case a naive start=next implementation gets wrong.
	for i := 11; i <= 14; i++ {
		r.ObserveRequest("/v1/chat/completions", "local", "m", 200, OutcomeSuccess, time.Duration(i)*time.Millisecond)
	}
	assertSamples(t, r.RequestLatencySamples(), millis(11, 12, 13, 14))
	if got := r.RequestLatencyDropped(); got != 10 {
		t.Errorf("dropped = %d, want 10", got)
	}

	// The returned slice is a copy: mutating it must not corrupt the ring.
	samples := r.RequestLatencySamples()
	samples[0] = 999 * time.Millisecond
	assertSamples(t, r.RequestLatencySamples(), millis(11, 12, 13, 14))

	r.Reset()
	if got := r.RequestLatencySamples(); len(got) != 0 {
		t.Errorf("after Reset samples = %v, want empty", got)
	}
	if got := r.RequestLatencyDropped(); got != 0 {
		t.Errorf("after Reset dropped = %d, want 0", got)
	}
	if got := r.RequestLatencyWindow(); got != window {
		t.Errorf("after Reset window = %d, want %d (the buffer is reused, not reallocated)", got, window)
	}
	// And the ring still works after a Reset.
	r.ObserveRequest("/v1/chat/completions", "local", "m", 200, OutcomeSuccess, time.Second)
	assertSamples(t, r.RequestLatencySamples(), []time.Duration{time.Second})
}

// TestRecorderDefaultWindow checks the constructor contract: a non-positive
// window means the default, and NewRecorder keeps its old behaviour.
func TestRecorderDefaultWindow(t *testing.T) {
	for _, r := range []*Recorder{NewRecorder(), NewRecorderWithWindow(0), NewRecorderWithWindow(-1)} {
		if got := r.RequestLatencyWindow(); got != DefaultLatencyWindow {
			t.Errorf("RequestLatencyWindow() = %d, want %d", got, DefaultLatencyWindow)
		}
	}
}

// TestRecorderWindowIsBounded is the regression test for the leak: the amount
// of memory a Recorder retains for latency samples must not grow with the
// number of requests it has seen.
func TestRecorderWindowIsBounded(t *testing.T) {
	const window = 16
	r := NewRecorderWithWindow(window)
	for i := 0; i < 10_000; i++ {
		r.ObserveRequest("/v1/chat/completions", "local", "m", 200, OutcomeSuccess, time.Millisecond)
	}
	if got := len(r.RequestLatencySamples()); got != window {
		t.Errorf("retained %d samples after 10000 requests, want %d", got, window)
	}
	if got, want := r.RequestLatencyDropped(), int64(10_000-window); got != want {
		t.Errorf("dropped = %d, want %d", got, want)
	}
}

// TestRecorderHistogramFamilies drives every Observe method through the Sink
// interface and checks that each one feeds the histogram family it is supposed
// to, with deterministic ordering and a copied Counts slice.
func TestRecorderHistogramFamilies(t *testing.T) {
	r := NewRecorderWithWindow(8)

	var _ Sink = r // the interface must not have grown a method for this

	r.ObserveRequest("/v1/chat/completions", "local", "llama", 200, OutcomeSuccess, 50*time.Millisecond)
	r.ObserveRequest("/v1/chat/completions", "local", "llama", 200, OutcomeSuccess, 3*time.Second)
	r.ObserveRequest("/v1/embeddings", "cloud", "small", 200, OutcomeSuccess, 7*time.Millisecond)
	r.ObserveUpstreamAttempt("local", 200, OutcomeSuccess, 45*time.Millisecond)
	r.ObserveUpstreamAttempt("local", 429, OutcomeRateLimited, 2*time.Millisecond)
	r.ObserveFirstToken("local", "llama", 120*time.Millisecond)
	r.ObserveTokens("local", "llama", 100, 300, 0)
	r.ObserveTokens("local", "llama", 100, 300, 0)

	// Request duration is keyed (route, upstream, model), sorted in that order.
	reqDur := r.RequestDurationHistograms()
	if len(reqDur) != 2 {
		t.Fatalf("RequestDurationHistograms() has %d rows, want 2: %+v", len(reqDur), reqDur)
	}
	if reqDur[0].Route != "/v1/chat/completions" || reqDur[1].Route != "/v1/embeddings" {
		t.Errorf("rows are not sorted by route: %q then %q", reqDur[0].Route, reqDur[1].Route)
	}
	// The local/llama series saw 50ms and 3s; the attempt histogram is keyed by
	// upstream alone and must therefore merge both attempts under "local".
	if got, want := reqDur[0].Count, int64(2); got != want {
		t.Errorf("request duration Count = %d, want %d", got, want)
	}
	if got, want := reqDur[0].Sum, 3.05; !closeEnough(got, want) {
		t.Errorf("request duration Sum = %v, want %v", got, want)
	}
	if got, want := reqDur[1].Sum, 0.007; !closeEnough(got, want) {
		t.Errorf("embeddings duration Sum = %v, want %v", got, want)
	}

	attempts := r.AttemptDurationHistograms()
	if len(attempts) != 1 {
		t.Fatalf("AttemptDurationHistograms() has %d rows, want 1 (keyed by upstream): %+v", len(attempts), attempts)
	}
	if attempts[0].Upstream != "local" || attempts[0].Count != 2 {
		t.Errorf("attempt row = %+v, want upstream=local count=2", attempts[0])
	}
	if got, want := attempts[0].Sum, 0.047; !closeEnough(got, want) {
		t.Errorf("attempt Sum = %v, want %v", got, want)
	}

	first := r.FirstTokenHistograms()
	if len(first) != 1 || first[0].Upstream != "local" || first[0].Model != "llama" {
		t.Fatalf("FirstTokenHistograms() = %+v, want one local/llama row", first)
	}
	if got, want := first[0].Sum, 0.12; !closeEnough(got, want) {
		t.Errorf("first token Sum = %v, want %v", got, want)
	}

	tokens := r.CompletionTokensHistograms()
	if len(tokens) != 1 || tokens[0].Upstream != "local" || tokens[0].Model != "llama" {
		t.Fatalf("CompletionTokensHistograms() = %+v, want one local/llama row", tokens)
	}
	if got, want := tokens[0].Count, int64(2); got != want {
		t.Errorf("completion token Count = %d, want %d", got, want)
	}
	if got, want := tokens[0].Sum, 600.0; !closeEnough(got, want) {
		t.Errorf("completion token Sum = %v, want %v (two requests of 300 tokens)", got, want)
	}
	// 300 tokens falls in the (256, 512] bucket, i.e. index 7 of the token set.
	assertBucketCount(t, tokens[0].Buckets(), 512, 2)

	// Every family's bucket list ends at +Inf and partitions its count.
	for _, row := range reqDur {
		assertBucketsConsistent(t, row.Buckets(), row.Count)
	}
	for _, row := range attempts {
		assertBucketsConsistent(t, row.Buckets(), row.Count)
	}
	for _, row := range first {
		assertBucketsConsistent(t, row.Buckets(), row.Count)
	}
	for _, row := range tokens {
		assertBucketsConsistent(t, row.Buckets(), row.Count)
	}

	// Everything above must have been a copy: a caller editing Counts cannot
	// race a concurrent Observe. This runs last because it deliberately
	// corrupts the returned slice.
	tokens[0].Counts[7] = 999
	again := r.CompletionTokensHistograms()
	if again[0].Counts[7] != 2 {
		t.Errorf("Counts was not copied: editing the returned slice changed the recorder (%v)", again[0].Counts)
	}

	// Reset clears the families, not just the counters.
	r.Reset()
	if got := r.RequestDurationHistograms(); len(got) != 0 {
		t.Errorf("after Reset request duration rows = %+v, want none", got)
	}
	if got := r.AttemptDurationHistograms(); len(got) != 0 {
		t.Errorf("after Reset attempt rows = %+v, want none", got)
	}
	if got := r.FirstTokenHistograms(); len(got) != 0 {
		t.Errorf("after Reset first token rows = %+v, want none", got)
	}
	if got := r.CompletionTokensHistograms(); len(got) != 0 {
		t.Errorf("after Reset completion token rows = %+v, want none", got)
	}
}

// TestRecorderHistogramOrderingIsDeterministic guards the contract the
// exposition writer depends on: two snapshots of the same state must render
// identically, because a series that moves between scrapes looks like a new
// series to a time-series database.
func TestRecorderHistogramOrderingIsDeterministic(t *testing.T) {
	r := NewRecorder()
	for _, route := range []string{"/v1/embeddings", "/v1/chat/completions", "/v1/models"} {
		r.ObserveRequest(route, "local", "m", 200, OutcomeSuccess, time.Millisecond)
	}
	first := r.RequestDurationHistograms()
	for i := 0; i < 20; i++ {
		next := r.RequestDurationHistograms()
		if len(next) != len(first) {
			t.Fatalf("row count changed between snapshots: %d then %d", len(first), len(next))
		}
		for j := range first {
			if first[j].Route != next[j].Route {
				t.Fatalf("snapshot %d reordered rows: %q at index %d, want %q", i, next[j].Route, j, first[j].Route)
			}
		}
	}
}

// TestRecorderConcurrentObserveAndSnapshot is the -race target: many writers
// against a reader that copies the state out, exactly as /metrics and /stats do
// while request handlers are running.
func TestRecorderConcurrentObserveAndSnapshot(t *testing.T) {
	r := NewRecorderWithWindow(64)

	const (
		writers          = 8
		perWriter        = 500
		snapshotterTicks = 200
	)
	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			upstream := []string{"local", "cloud"}[w%2]
			for i := 0; i < perWriter; i++ {
				r.ObserveRequest("/v1/chat/completions", upstream, "m", 200, OutcomeSuccess, time.Duration(i)*time.Microsecond)
				r.ObserveUpstreamAttempt(upstream, 200, OutcomeSuccess, time.Millisecond)
				r.ObserveFirstToken(upstream, "m", 100*time.Millisecond)
				r.ObserveTokens(upstream, "m", 10, 20, 0)
			}
		}(w)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < snapshotterTicks; i++ {
			_ = r.RequestLatencySamples()
			_ = r.RequestLatencyWindow()
			_ = r.RequestLatencyDropped()
			_ = r.RequestDurationHistograms()
			_ = r.AttemptDurationHistograms()
			_ = r.FirstTokenHistograms()
			_ = r.CompletionTokensHistograms()
		}
	}()

	wg.Wait()

	wantRequests := int64(writers * perWriter)
	var got int64
	for _, row := range r.RequestDurationHistograms() {
		got += row.Count
	}
	if got != wantRequests {
		t.Errorf("total request-duration observations = %d, want %d (observations were lost under contention)", got, wantRequests)
	}
	if got := len(r.RequestLatencySamples()); got != 64 {
		t.Errorf("ring holds %d samples, want the window capacity 64", got)
	}
	if got, want := r.RequestLatencyDropped(), wantRequests-64; got != want {
		t.Errorf("dropped = %d, want %d", got, want)
	}
}

// millis builds the expected sample lists so the assertions read as the values
// that were observed rather than as arithmetic on a slice literal.
func millis(vs ...int) []time.Duration {
	out := make([]time.Duration, len(vs))
	for i, v := range vs {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

func assertSamples(t *testing.T, got, want []time.Duration) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("samples = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("samples = %v, want %v (index %d differs)", got, want, i)
		}
	}
}

func assertBucketCount(t *testing.T, buckets []Bucket, upperBound float64, want int64) {
	t.Helper()
	for _, b := range buckets {
		if b.UpperBound == upperBound {
			if b.Count != want {
				t.Errorf("bucket le=%v = %d, want %d", upperBound, b.Count, want)
			}
			return
		}
	}
	t.Errorf("no bucket with upper bound %v in %+v", upperBound, buckets)
}

func assertBucketsConsistent(t *testing.T, buckets []Bucket, count int64) {
	t.Helper()
	if len(buckets) == 0 {
		t.Fatal("empty bucket list")
	}
	if !buckets[len(buckets)-1].IsInf() {
		t.Errorf("last bucket is %v, want +Inf", buckets[len(buckets)-1].UpperBound)
	}
	if got := buckets[len(buckets)-1].Count; got != count {
		t.Errorf("final bucket = %d, want Count = %d", got, count)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks.
//
// These are the evidence for the bottleneck this change fixes. The two
// "Observe" benchmarks measure the whole Sink call -- a map update, a ring
// write and a histogram update behind one mutex -- because that is the unit the
// request path pays for, and the Legacy pair below is the shape it replaced so
// the delta is priced rather than asserted.
//
// legacyRecorder is benchmark-only code. It is never linked into the gateway;
// it exists so that "the histogram made observation slower" is a number instead
// of an opinion.
// ---------------------------------------------------------------------------

// legacyRecorder is the pre-change Recorder's request path: one mutex, one map,
// and an unbounded append of every duration. It deliberately keeps the old
// flaw, because that flaw is what makes a long-running process's memory grow.
type legacyRecorder struct {
	mu      sync.Mutex
	rows    map[requestKey]*requestStat
	latency []time.Duration
}

func newLegacyRecorder() *legacyRecorder {
	return &legacyRecorder{rows: map[requestKey]*requestStat{}}
}

func (l *legacyRecorder) observe(route, upstream, model string, status int, outcome Outcome, elapsed time.Duration) {
	secs := elapsed.Seconds()
	l.mu.Lock()
	defer l.mu.Unlock()
	k := requestKey{Route: route, Upstream: upstream, Model: model, Status: status, Outcome: outcome}
	st := l.rows[k]
	if st == nil {
		st = &requestStat{}
		l.rows[k] = st
	}
	st.Count++
	st.Seconds += secs
	l.latency = append(l.latency, elapsed)
}

func BenchmarkLegacyRecorderObserveRequest(b *testing.B) {
	r := newLegacyRecorder()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.observe("/v1/chat/completions", "local", "llama-3-8b", 200, OutcomeSuccess, 42*time.Millisecond)
	}
}

func BenchmarkLegacyRecorderObserveRequestParallel(b *testing.B) {
	r := newLegacyRecorder()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.observe("/v1/chat/completions", "local", "llama-3-8b", 200, OutcomeSuccess, 42*time.Millisecond)
		}
	})
}

func BenchmarkRecorderObserveRequest(b *testing.B) {
	r := NewRecorder()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.ObserveRequest("/v1/chat/completions", "local", "llama-3-8b", 200, OutcomeSuccess, 42*time.Millisecond)
	}
}

func BenchmarkRecorderObserveRequestParallel(b *testing.B) {
	r := NewRecorder()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.ObserveRequest("/v1/chat/completions", "local", "llama-3-8b", 200, OutcomeSuccess, 42*time.Millisecond)
		}
	})
}

// BenchmarkRecorderObserveFullRequest measures the whole observation of one
// request, which is what the gateway actually emits: duration, attempt, first
// token and tokens.
func BenchmarkRecorderObserveFullRequest(b *testing.B) {
	r := NewRecorder()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.ObserveRequest("/v1/chat/completions", "local", "llama-3-8b", 200, OutcomeSuccess, 42*time.Millisecond)
		r.ObserveUpstreamAttempt("local", 200, OutcomeSuccess, 40*time.Millisecond)
		r.ObserveFirstToken("local", "llama-3-8b", 30*time.Millisecond)
		r.ObserveTokens("local", "llama-3-8b", 100, 300, 0)
	}
}

// BenchmarkHistogramObserve isolates the work the histogram adds once a label
// set exists: one binary search over thirteen bounds plus two counter writes.
// It is unlocked on purpose, because the Recorder already holds its mutex; this
// is the number to quote when asking what a bucket costs per observation.
func BenchmarkHistogramObserve(b *testing.B) {
	h := NewHistogram(DefaultDurationBuckets())
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.Observe(0.042)
	}
}

// BenchmarkRequestDurationMapLookup is benchmark-only: it pays the mutex and
// the second map lookup (the histogram key, which repeats the three label
// strings the request map already hashed) without observing anything into the
// histogram. Subtracting it from BenchmarkRecorderObserveRequest splits the
// delta between "one more map probe" and "one more bucket".
func BenchmarkRequestDurationMapLookup(b *testing.B) {
	m := map[requestDurationKey]*Histogram{}
	hk := requestDurationKey{Route: "/v1/chat/completions", Upstream: "local", Model: "llama-3-8b"}
	m[hk] = NewHistogram(defaultDurationBuckets)
	var mu sync.Mutex
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		mu.Lock()
		_ = m[hk]
		mu.Unlock()
	}
}

// BenchmarkRequestLatencySamples measures one scrape's copy of the raw window.
// It is the cost the /stats endpoint pays per request, and with the old
// unbounded slice it grew with process uptime.
func BenchmarkRequestLatencySamples(b *testing.B) {
	r := NewRecorder()
	for i := 0; i < DefaultLatencyWindow; i++ {
		r.ObserveRequest("/v1/chat/completions", "local", "llama-3-8b", 200, OutcomeSuccess, time.Duration(i)*time.Microsecond)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.RequestLatencySamples()
	}
}

// BenchmarkRequestLatencySamplesSmall is the same scrape on a run shorter than
// the window, so the numbers isolate the copy from the sample count.
func BenchmarkRequestLatencySamplesSmall(b *testing.B) {
	r := NewRecorder()
	for i := 0; i < 3600; i++ {
		r.ObserveRequest("/v1/chat/completions", "local", "llama-3-8b", 200, OutcomeSuccess, time.Duration(i)*time.Microsecond)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.RequestLatencySamples()
	}
}

// benchmarkPercentileSamples is a 3600-sample set, the size of the M2/M3
// baseline runs.
func benchmarkPercentileSamples() []time.Duration {
	samples := make([]time.Duration, 3600)
	for i := range samples {
		samples[i] = time.Duration(i+1) * 100 * time.Microsecond
	}
	return samples
}

// BenchmarkPercentilesUnsortedPerCall is the OLD shape of handleStats: every
// quantile call copies and sorts the whole sample set, so one /stats request
// pays for five sorts. It is benchmark-only; the production path uses
// BenchmarkPercentilesFromSorted.
func BenchmarkPercentilesUnsortedPerCall(b *testing.B) {
	samples := benchmarkPercentileSamples()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, p := range []float64{0.50, 0.90, 0.95, 0.99, 1.0} {
			_ = benchmarkPercentile(samples, p)
		}
	}
}

// BenchmarkPercentilesFromSorted is the new shape: sort once, then read five
// quantiles off the sorted slice.
func BenchmarkPercentilesFromSorted(b *testing.B) {
	samples := benchmarkPercentileSamples()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sorted := append([]time.Duration(nil), samples...)
		sortDurations(sorted)
		for _, p := range []float64{0.50, 0.90, 0.95, 0.99, 1.0} {
			_ = benchmarkPercentileSorted(sorted, p)
		}
	}
}

// benchmarkPercentile mirrors the pre-change internal/server.percentile: it
// copies and sorts on every call. Benchmark-only.
func benchmarkPercentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sortDurations(sorted)
	return benchmarkPercentileSorted(sorted, p)
}

// benchmarkPercentileSorted mirrors internal/server.percentileSorted, nearest
// rank, no copy and no sort. Benchmark-only: the duplicated body exists so the
// metrics package can measure the difference without importing the server.
func benchmarkPercentileSorted(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// sortDurations is the same call internal/server makes, kept in one place so
// both benchmark shapes sort identically.
func sortDurations(s []time.Duration) {
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
}
