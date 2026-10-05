// Package metrics defines the instrumentation surface the gateway depends on.
//
// M0 ships two implementations: a recording one the binary exposes as
// Prometheus text, and a no-op one used by tests. Instrumentation is expressed
// as an interface so that the dependency points inward: internal/gateway never
// imports a metrics library, and M5 can swap the exporter without touching the
// proxy.
package metrics

import (
	"sort"
	"sync"
	"time"
)

// Outcome classifies how an attempt ended. The distinction matters for SLO
// reporting: a client that hangs up mid-stream is not a gateway failure, but a
// timeout is.
type Outcome string

// Outcome values.
const (
	OutcomeSuccess     Outcome = "success"
	OutcomeUpstreamErr Outcome = "upstream_error"
	OutcomeTimeout     Outcome = "timeout"
	OutcomeCanceled    Outcome = "canceled"
	OutcomeBadRequest  Outcome = "bad_request"
	OutcomeInternal    Outcome = "internal_error"

	// OutcomeRateLimited is a request the gateway itself refused for budget
	// reasons (M3 quota). It is deliberately not folded into bad_request: a
	// tenant over its daily budget is an operational event with an owner, and
	// an operator watching error rates needs to see it as its own series.
	OutcomeRateLimited Outcome = "rate_limited"
)

// Sink receives gateway observations. Implementations must be safe for
// concurrent use and must not block the request path.
type Sink interface {
	// ObserveRequest records one completed request.
	//
	// route is the normalised request path (e.g. "/v1/chat/completions"),
	// upstream is the backend that served it, model is the requested model,
	// status is the downstream HTTP status and elapsed is the wall time of the
	// handler.
	ObserveRequest(route, upstream, model string, status int, outcome Outcome, elapsed time.Duration)

	// ObserveTokens records token consumption for one request.
	//
	// prompt and completion are provider-reported counts; cached is the subset
	// of prompt tokens billed at the discounted rate.
	ObserveTokens(upstream, model string, prompt, completion, cached int)

	// ObserveUpstreamAttempt records one outbound attempt, including retries.
	// M1 uses it to measure failover; M0 records exactly one attempt per
	// request.
	ObserveUpstreamAttempt(upstream string, status int, outcome Outcome, elapsed time.Duration)

	// ObserveFailover records the decision to abandon one backend and try
	// another, with the abandoned attempt's outcome.
	//
	// This is reported separately from ObserveUpstreamAttempt because the two
	// answer different questions: attempts say how often a backend was tried,
	// failovers say how often the gateway decided it was not worth waiting for.
	// A rising failover rate with a flat attempt rate is the signature of a
	// fleet-wide slowdown that no single backend's error rate would show.
	ObserveFailover(upstream string, outcome Outcome, status int)

	// ObserveFirstToken records time-to-first-token for a streamed response.
	ObserveFirstToken(upstream, model string, elapsed time.Duration)

	// ObserveStreamFrames records frame and byte counts for a completed stream.
	ObserveStreamFrames(upstream string, frames, bytes int64)
}

// Nop is a Sink that discards everything.
type Nop struct{}

// ObserveRequest implements Sink.
func (Nop) ObserveRequest(string, string, string, int, Outcome, time.Duration) {}

// ObserveTokens implements Sink.
func (Nop) ObserveTokens(string, string, int, int, int) {}

// ObserveUpstreamAttempt implements Sink.
func (Nop) ObserveUpstreamAttempt(string, int, Outcome, time.Duration) {}

// ObserveFailover implements Sink.
func (Nop) ObserveFailover(string, Outcome, int) {}

// ObserveFirstToken implements Sink.
func (Nop) ObserveFirstToken(string, string, time.Duration) {}

// ObserveStreamFrames implements Sink.
func (Nop) ObserveStreamFrames(string, int64, int64) {}

// DefaultLatencyWindow is the number of raw request durations a Recorder keeps
// for the /stats percentiles.
//
// 65536 samples is bounded memory -- 512KiB at 8 bytes per time.Duration -- and
// roughly a day of traffic at one request per second. That is the point: a
// percentile must describe the traffic an operator is looking at now, not every
// request since the process started, and an unbounded slice is a leak whose
// size the /stats handler pays for again on every scrape (it sorts the whole
// thing). A run shorter than the window is unaffected, which is what keeps the
// 3600-request M2/M3 baselines byte-identical.
const DefaultLatencyWindow = 65536

// Recorder is an in-memory Sink. It backs both tests and the /metrics endpoint,
// so the numbers a test asserts and the numbers an operator scrapes come from
// one implementation rather than two that can drift.
//
// Every field is guarded by the one mutex. Durations are recorded twice on
// purpose: as histogram buckets for /metrics (bounded, exportable) and as raw
// samples in a fixed-size ring for the exact nearest-rank percentiles in
// /stats. The ring is capped at DefaultLatencyWindow, so the recorder's memory
// is a constant of the configuration rather than a function of uptime.
type Recorder struct {
	mu sync.Mutex

	requests   map[requestKey]*requestStat
	attempts   map[attemptKey]*attemptStat
	failovers  map[failoverKey]int64
	tokens     map[tokenKey]*TokenStat
	firstToken map[tokenKey]*latencyStat
	streams    map[string]*streamStat

	requestLatency latencyRing

	requestDuration  map[requestDurationKey]*Histogram
	attemptDuration  map[string]*Histogram
	firstTokenHist   map[tokenKey]*Histogram
	completionTokens map[tokenKey]*Histogram
}

// latencyRing is a fixed-capacity ring of the most recent request durations,
// readable in insertion order starting at the oldest live sample.
//
// It replaces an unbounded append that was the one piece of Recorder state that
// grew with uptime, and it is evicted rather than truncated so that a
// percentile still describes a contiguous recent window instead of the first N
// requests of the process's life.
type latencyRing struct {
	buf []time.Duration

	// next is the slot the next observation overwrites. Once the ring is full
	// that slot holds the oldest sample, which is what makes eviction O(1).
	next int

	// n is how many slots currently hold a sample. next and n diverge only
	// after the first wrap.
	n int

	// dropped counts observations evicted by the window.
	dropped int64
}

// observe records one duration, evicting the oldest sample when full.
func (r *latencyRing) observe(d time.Duration) {
	if len(r.buf) == 0 {
		// A Recorder built by hand instead of by NewRecorder has no window. It
		// must not panic on the request path; it simply records no samples.
		return
	}
	if r.n == len(r.buf) {
		r.dropped++
	} else {
		r.n++
	}
	r.buf[r.next] = d
	r.next++
	if r.next == len(r.buf) {
		r.next = 0
	}
}

// samples copies the live samples oldest-first.
//
// Insertion order (not ring order) is deliberate: a caller that wants
// percentiles sorts anyway, and a caller that is a test wants a value that does
// not change when the ring wraps.
func (r *latencyRing) samples() []time.Duration {
	out := make([]time.Duration, 0, r.n)
	start := r.next
	if r.n < len(r.buf) {
		// Before the first wrap the samples occupy buf[0:n] and next == n.
		start = 0
	}
	for i := 0; i < r.n; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

// reset empties the ring but keeps its buffer, so Reset does not force a
// 512KiB reallocation and a fresh GC cycle.
func (r *latencyRing) reset() {
	r.next = 0
	r.n = 0
	r.dropped = 0
}

// failoverKey groups failover decisions by the backend that was abandoned and
// why. The status is kept because a failover away from a 429 is a quota problem
// while a failover away from a 503 is an outage, and an operator responds to
// those very differently.
type failoverKey struct {
	Upstream string
	Outcome  Outcome
	Status   int
}

type requestKey struct {
	Route    string
	Upstream string
	Model    string
	Status   int
	Outcome  Outcome
}

type attemptKey struct {
	Upstream string
	Status   int
	Outcome  Outcome
}

type tokenKey struct {
	Upstream string
	Model    string
}

// requestDurationKey is the histogram key for request duration. It deliberately
// omits status and outcome, which requestKey keeps: a latency panel split by
// outcome answers "how slow are failures", while the question an SLO asks is
// "how slow were the requests users actually waited for".
type requestDurationKey struct {
	Route    string
	Upstream string
	Model    string
}

type requestStat struct {
	Count   int64
	Seconds float64
}

type attemptStat struct {
	Count   int64
	Seconds float64
}

// TokenStat is the accumulated token usage for one (upstream, model) pair.
type TokenStat struct {
	Prompt     int64
	Completion int64
	Cached     int64
	Requests   int64
}

type latencyStat struct {
	Count   int64
	Seconds float64
}

type streamStat struct {
	Frames int64
	Bytes  int64
}

// NewRecorder returns an empty Recorder with the default latency window.
func NewRecorder() *Recorder { return NewRecorderWithWindow(0) }

// NewRecorderWithWindow returns an empty Recorder whose raw-latency window holds
// at most n samples.
//
// n <= 0 selects DefaultLatencyWindow, so NewRecorder and
// NewRecorderWithWindow(0) build the same recorder. A small n exists for tests
// that need to prove eviction happens; the gateway always takes the default.
func NewRecorderWithWindow(n int) *Recorder {
	if n <= 0 {
		n = DefaultLatencyWindow
	}
	return &Recorder{
		requests:         map[requestKey]*requestStat{},
		attempts:         map[attemptKey]*attemptStat{},
		failovers:        map[failoverKey]int64{},
		tokens:           map[tokenKey]*TokenStat{},
		firstToken:       map[tokenKey]*latencyStat{},
		streams:          map[string]*streamStat{},
		requestLatency:   latencyRing{buf: make([]time.Duration, n)},
		requestDuration:  map[requestDurationKey]*Histogram{},
		attemptDuration:  map[string]*Histogram{},
		firstTokenHist:   map[tokenKey]*Histogram{},
		completionTokens: map[tokenKey]*Histogram{},
	}
}

// ObserveRequest implements Sink.
func (m *Recorder) ObserveRequest(route, upstream, model string, status int, outcome Outcome, elapsed time.Duration) {
	secs := elapsed.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	k := requestKey{Route: route, Upstream: upstream, Model: model, Status: status, Outcome: outcome}
	st := m.requests[k]
	if st == nil {
		st = &requestStat{}
		m.requests[k] = st
	}
	st.Count++
	st.Seconds += secs
	m.requestLatency.observe(elapsed)

	hk := requestDurationKey{Route: route, Upstream: upstream, Model: model}
	h := m.requestDuration[hk]
	if h == nil {
		h = NewHistogram(defaultDurationBuckets)
		m.requestDuration[hk] = h
	}
	h.Observe(secs)
}

// ObserveTokens implements Sink.
func (m *Recorder) ObserveTokens(upstream, model string, prompt, completion, cached int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := tokenKey{Upstream: upstream, Model: model}
	st := m.tokens[k]
	if st == nil {
		st = &TokenStat{}
		m.tokens[k] = st
	}
	st.Prompt += int64(prompt)
	st.Completion += int64(completion)
	st.Cached += int64(cached)
	st.Requests++

	// Completion tokens, not prompt+completion: prompt length is a property of
	// what the client sent and is already fully described by the token
	// counters, while completion length is what the gateway waits for and what
	// a streaming client experiences as duration.
	h := m.completionTokens[k]
	if h == nil {
		h = NewHistogram(defaultTokensPerRequestBuckets)
		m.completionTokens[k] = h
	}
	h.Observe(float64(completion))
}

// ObserveUpstreamAttempt implements Sink.
func (m *Recorder) ObserveUpstreamAttempt(upstream string, status int, outcome Outcome, elapsed time.Duration) {
	secs := elapsed.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	k := attemptKey{Upstream: upstream, Status: status, Outcome: outcome}
	st := m.attempts[k]
	if st == nil {
		st = &attemptStat{}
		m.attempts[k] = st
	}
	st.Count++
	st.Seconds += secs

	h := m.attemptDuration[upstream]
	if h == nil {
		h = NewHistogram(defaultDurationBuckets)
		m.attemptDuration[upstream] = h
	}
	h.Observe(secs)
}

// ObserveFailover implements Sink.
//
// status is the abandoned attempt's HTTP status, or 0 when the attempt never
// produced a response (dial failure, timeout, stream reset).
func (m *Recorder) ObserveFailover(upstream string, outcome Outcome, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failovers[failoverKey{Upstream: upstream, Outcome: outcome, Status: status}]++
}

// FailoverSample is one (upstream, outcome, status) failover counter.
type FailoverSample struct {
	Upstream string
	Outcome  Outcome
	Status   int
	Count    int64
}

// FailoverSnapshot returns the failover counters in deterministic order.
func (m *Recorder) FailoverSnapshot() []FailoverSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]FailoverSample, 0, len(m.failovers))
	for k, v := range m.failovers {
		out = append(out, FailoverSample{Upstream: k.Upstream, Outcome: k.Outcome, Status: k.Status, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Upstream != out[j].Upstream {
			return out[i].Upstream < out[j].Upstream
		}
		if out[i].Outcome != out[j].Outcome {
			return out[i].Outcome < out[j].Outcome
		}
		return out[i].Status < out[j].Status
	})
	return out
}

// ObserveFirstToken implements Sink.
func (m *Recorder) ObserveFirstToken(upstream, model string, elapsed time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := tokenKey{Upstream: upstream, Model: model}
	st := m.firstToken[k]
	if st == nil {
		st = &latencyStat{}
		m.firstToken[k] = st
	}
	st.Count++
	st.Seconds += elapsed.Seconds()

	h := m.firstTokenHist[k]
	if h == nil {
		h = NewHistogram(defaultFirstTokenBuckets)
		m.firstTokenHist[k] = h
	}
	h.Observe(elapsed.Seconds())
}

// ObserveStreamFrames implements Sink.
func (m *Recorder) ObserveStreamFrames(upstream string, frames, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.streams[upstream]
	if st == nil {
		st = &streamStat{}
		m.streams[upstream] = st
	}
	st.Frames += frames
	st.Bytes += bytes
}

// RequestSnapshot is one (route, upstream, model, status, outcome) counter.
type RequestSnapshot struct {
	Route        string
	Upstream     string
	Model        string
	Status       int
	Outcome      Outcome
	Count        int64
	TotalSeconds float64
	MeanSeconds  float64
}

// Snapshot returns a stable copy of the request counters, sorted by key.
func (m *Recorder) Snapshot() []RequestSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RequestSnapshot, 0, len(m.requests))
	for k, st := range m.requests {
		s := RequestSnapshot{
			Route: k.Route, Upstream: k.Upstream, Model: k.Model,
			Status: k.Status, Outcome: k.Outcome,
			Count: st.Count, TotalSeconds: st.Seconds,
		}
		if st.Count > 0 {
			s.MeanSeconds = st.Seconds / float64(st.Count)
		}
		out = append(out, s)
	}
	sortRequests(out)
	return out
}

// TokenSnapshot returns token counters keyed by upstream and model.
func (m *Recorder) TokenSnapshot() map[tokenKey]TokenStat {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[tokenKey]TokenStat, len(m.tokens))
	for k, v := range m.tokens {
		out[k] = *v
	}
	return out
}

// TokenSnapshotRow is one (upstream, model) token counter, with the map key
// flattened into fields so that callers outside this package can read it without
// depending on an unexported key type.
type TokenSnapshotRow struct {
	Upstream   string
	Model      string
	Prompt     int64
	Completion int64
	Cached     int64
	Requests   int64
}

// TokenSnapshotRows returns token counters in deterministic
// (upstream, model) order.
func (m *Recorder) TokenSnapshotRows() []TokenSnapshotRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TokenSnapshotRow, 0, len(m.tokens))
	for k, v := range m.tokens {
		out = append(out, TokenSnapshotRow{
			Upstream:   k.Upstream,
			Model:      k.Model,
			Prompt:     v.Prompt,
			Completion: v.Completion,
			Cached:     v.Cached,
			Requests:   v.Requests,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Upstream != out[j].Upstream {
			return out[i].Upstream < out[j].Upstream
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// TokenTotals sums prompt, completion and cached tokens across every model.
func (m *Recorder) TokenTotals() (prompt, completion, cached int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.tokens {
		prompt += v.Prompt
		completion += v.Completion
		cached += v.Cached
	}
	return
}

// FirstTokenSample is the mean time-to-first-token for one (upstream, model).
type FirstTokenSample struct {
	Upstream string
	Model    string
	Mean     time.Duration
	Count    int64
}

// FirstTokenSnapshot returns mean time-to-first-token per (upstream, model), in
// deterministic order.
func (m *Recorder) FirstTokenSnapshot() []FirstTokenSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]FirstTokenSample, 0, len(m.firstToken))
	for k, v := range m.firstToken {
		if v.Count == 0 {
			continue
		}
		out = append(out, FirstTokenSample{
			Upstream: k.Upstream,
			Model:    k.Model,
			Mean:     time.Duration(v.Seconds / float64(v.Count) * float64(time.Second)),
			Count:    v.Count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Upstream != out[j].Upstream {
			return out[i].Upstream < out[j].Upstream
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// StreamSample is the frame and byte totals for one upstream.
type StreamSample struct {
	Upstream string
	Frames   int64
	Bytes    int64
}

// StreamSnapshot returns frame and byte totals per upstream, in deterministic
// order.
func (m *Recorder) StreamSnapshot() []StreamSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StreamSample, 0, len(m.streams))
	for k, v := range m.streams {
		out = append(out, StreamSample{Upstream: k, Frames: v.Frames, Bytes: v.Bytes})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Upstream < out[j].Upstream })
	return out
}

// RequestLatencySamples returns a copy of the raw request latencies the window
// currently holds, oldest first.
//
// Semantics: the percentiles a caller derives from this are over the most
// recent min(recorded requests, window) requests. A run shorter than the window
// -- every load test in this repo -- sees exactly the samples it always did, so
// its results are unchanged; a longer run has lost its oldest samples instead
// of retaining them forever, and RequestLatencyDropped reports how many.
//
// Ordering is insertion order so the value is deterministic regardless of how
// many times the ring has wrapped.
func (m *Recorder) RequestLatencySamples() []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requestLatency.samples()
}

// RequestLatencyWindow reports the configured capacity of the raw-latency
// window, so /stats can publish the sample population a percentile is computed
// over without duplicating the default.
func (m *Recorder) RequestLatencyWindow() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requestLatency.buf)
}

// RequestLatencyDropped reports how many request durations the window has
// evicted since the last Reset.
//
// It is exported because a percentile over a truncated window is a different
// claim from a percentile over everything: a dashboard that shows p99 without
// this number cannot tell "the last 65536 requests" from "since boot".
func (m *Recorder) RequestLatencyDropped() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requestLatency.dropped
}

// RequestDurationHistogram is the request-duration histogram for one
// (route, upstream, model), with the map key flattened into fields so a caller
// outside this package can read it without depending on an unexported key type.
type RequestDurationHistogram struct {
	Route    string
	Upstream string
	Model    string
	Count    int64
	Sum      float64
	Counts   []int64
}

// Bounds returns the finite bucket boundaries this family was recorded with.
func (RequestDurationHistogram) Bounds() []float64 { return DefaultDurationBuckets() }

// Buckets returns the cumulative buckets, ending with the +Inf catch-all.
func (r RequestDurationHistogram) Buckets() []Bucket {
	return cumulativeBuckets(DefaultDurationBuckets(), r.Counts)
}

// RequestDurationHistograms returns the request-duration histograms in
// deterministic (route, upstream, model) order.
func (m *Recorder) RequestDurationHistograms() []RequestDurationHistogram {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RequestDurationHistogram, 0, len(m.requestDuration))
	for k, h := range m.requestDuration {
		s := h.Snapshot()
		out = append(out, RequestDurationHistogram{
			Route: k.Route, Upstream: k.Upstream, Model: k.Model,
			Count: s.Count, Sum: s.Sum, Counts: s.Counts,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Route != b.Route {
			return a.Route < b.Route
		}
		if a.Upstream != b.Upstream {
			return a.Upstream < b.Upstream
		}
		return a.Model < b.Model
	})
	return out
}

// AttemptDurationHistogram is the upstream-attempt-duration histogram for one
// upstream.
//
// It is the only export of attempt timing: attemptStat carries a count and a sum
// that /metrics has never rendered, so before this a slow backend that the
// gateway retried away was invisible in every scrape.
type AttemptDurationHistogram struct {
	Upstream string
	Count    int64
	Sum      float64
	Counts   []int64
}

// Bounds returns the finite bucket boundaries this family was recorded with.
func (AttemptDurationHistogram) Bounds() []float64 { return DefaultDurationBuckets() }

// Buckets returns the cumulative buckets, ending with the +Inf catch-all.
func (a AttemptDurationHistogram) Buckets() []Bucket {
	return cumulativeBuckets(DefaultDurationBuckets(), a.Counts)
}

// AttemptDurationHistograms returns the attempt-duration histograms in
// deterministic upstream order.
func (m *Recorder) AttemptDurationHistograms() []AttemptDurationHistogram {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AttemptDurationHistogram, 0, len(m.attemptDuration))
	for k, h := range m.attemptDuration {
		s := h.Snapshot()
		out = append(out, AttemptDurationHistogram{Upstream: k, Count: s.Count, Sum: s.Sum, Counts: s.Counts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Upstream < out[j].Upstream })
	return out
}

// FirstTokenHistogram is the time-to-first-token histogram for one
// (upstream, model).
type FirstTokenHistogram struct {
	Upstream string
	Model    string
	Count    int64
	Sum      float64
	Counts   []int64
}

// Bounds returns the finite bucket boundaries this family was recorded with.
func (FirstTokenHistogram) Bounds() []float64 { return DefaultFirstTokenBuckets() }

// Buckets returns the cumulative buckets, ending with the +Inf catch-all.
func (f FirstTokenHistogram) Buckets() []Bucket {
	return cumulativeBuckets(DefaultFirstTokenBuckets(), f.Counts)
}

// FirstTokenHistograms returns the time-to-first-token histograms in
// deterministic (upstream, model) order.
func (m *Recorder) FirstTokenHistograms() []FirstTokenHistogram {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]FirstTokenHistogram, 0, len(m.firstTokenHist))
	for k, h := range m.firstTokenHist {
		s := h.Snapshot()
		out = append(out, FirstTokenHistogram{
			Upstream: k.Upstream, Model: k.Model,
			Count: s.Count, Sum: s.Sum, Counts: s.Counts,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Upstream != out[j].Upstream {
			return out[i].Upstream < out[j].Upstream
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// CompletionTokensHistogram is the completion-tokens-per-request histogram for
// one (upstream, model).
type CompletionTokensHistogram struct {
	Upstream string
	Model    string
	Count    int64
	Sum      float64
	Counts   []int64
}

// Bounds returns the finite bucket boundaries this family was recorded with.
func (CompletionTokensHistogram) Bounds() []float64 { return DefaultTokensPerRequestBuckets() }

// Buckets returns the cumulative buckets, ending with the +Inf catch-all.
func (c CompletionTokensHistogram) Buckets() []Bucket {
	return cumulativeBuckets(DefaultTokensPerRequestBuckets(), c.Counts)
}

// CompletionTokensHistograms returns the completion-token histograms in
// deterministic (upstream, model) order.
func (m *Recorder) CompletionTokensHistograms() []CompletionTokensHistogram {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]CompletionTokensHistogram, 0, len(m.completionTokens))
	for k, h := range m.completionTokens {
		s := h.Snapshot()
		out = append(out, CompletionTokensHistogram{
			Upstream: k.Upstream, Model: k.Model,
			Count: s.Count, Sum: s.Sum, Counts: s.Counts,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Upstream != out[j].Upstream {
			return out[i].Upstream < out[j].Upstream
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Reset clears every counter and every histogram. It exists for tests and for
// benchmark harnesses that measure a single phase.
func (m *Recorder) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = map[requestKey]*requestStat{}
	m.attempts = map[attemptKey]*attemptStat{}
	m.failovers = map[failoverKey]int64{}
	m.tokens = map[tokenKey]*TokenStat{}
	m.firstToken = map[tokenKey]*latencyStat{}
	m.streams = map[string]*streamStat{}
	m.requestLatency.reset()
	m.requestDuration = map[requestDurationKey]*Histogram{}
	m.attemptDuration = map[string]*Histogram{}
	m.firstTokenHist = map[tokenKey]*Histogram{}
	m.completionTokens = map[tokenKey]*Histogram{}
}
