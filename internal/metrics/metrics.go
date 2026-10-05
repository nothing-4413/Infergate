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

// Recorder is an in-memory Sink. It backs both tests and the /metrics endpoint,
// so the numbers a test asserts and the numbers an operator scrapes come from
// one implementation rather than two that can drift.
type Recorder struct {
	mu sync.Mutex

	requests       map[requestKey]*requestStat
	attempts       map[attemptKey]*attemptStat
	failovers      map[failoverKey]int64
	tokens         map[tokenKey]*TokenStat
	firstToken     map[tokenKey]*latencyStat
	streams        map[string]*streamStat
	requestLatency []time.Duration
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

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder {
	return &Recorder{
		requests:   map[requestKey]*requestStat{},
		attempts:   map[attemptKey]*attemptStat{},
		failovers:  map[failoverKey]int64{},
		tokens:     map[tokenKey]*TokenStat{},
		firstToken: map[tokenKey]*latencyStat{},
		streams:    map[string]*streamStat{},
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
	m.requestLatency = append(m.requestLatency, elapsed)
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
	Route         string
	Upstream      string
	Model         string
	Status        int
	Outcome       Outcome
	Count         int64
	TotalSeconds  float64
	MeanSeconds   float64
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

// RequestLatencySamples returns a copy of the raw request latencies, for
// percentile computation by the caller.
func (m *Recorder) RequestLatencySamples() []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]time.Duration, len(m.requestLatency))
	copy(out, m.requestLatency)
	return out
}

// Reset clears every counter. It exists for tests and for benchmark harnesses
// that measure a single phase.
func (m *Recorder) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = map[requestKey]*requestStat{}
	m.attempts = map[attemptKey]*attemptStat{}
	m.failovers = map[failoverKey]int64{}
	m.tokens = map[tokenKey]*TokenStat{}
	m.firstToken = map[tokenKey]*latencyStat{}
	m.streams = map[string]*streamStat{}
	m.requestLatency = nil
}
