package gateway

import (
	"hash/fnv"
	"net/http"
	"time"

	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/tracing"
)

// Span names. They are constants rather than literals so that a dashboard or a
// test can refer to a span without repeating a string that a typo would turn
// into a silently different span.
const (
	// SpanGateway is the root span: one per request, covering the whole
	// pipeline including every failover attempt and the response copy.
	SpanGateway = "gateway.request"

	// SpanUpstreamPrefix names a client span per upstream attempt. The backend
	// name is appended, so "which backend was slow" is answerable from the
	// span name alone in a trace viewer.
	SpanUpstreamPrefix = "upstream."
)

// firstTokenMS renders the observed first-token latency in milliseconds, or 0
// when none was observed. A helper rather than an inline expression because the
// same value appears in the root span, the attempt span and the stream event,
// and three copies of a unit conversion is how traces end up disagreeing with
// each other.
func firstTokenMS(rec *record) float64 {
	if rec == nil || !rec.firstTokenSet {
		return 0
	}
	return float64(rec.firstToken) / float64(time.Millisecond)
}

// TraceExporter receives finished traces.
//
// Export is called on the request goroutine and must not block: an
// implementation that writes to a file or the network hands the trace to its
// own goroutine and drops when its buffer is full. A gateway that slows down
// because a collector is unreachable is a worse gateway, not a better
// observability story.
type TraceExporter interface {
	Export(t *tracing.Trace)
}

// Tracer records one trace per sampled request.
//
// Nil is a valid value: every method tolerates a nil receiver, which is what
// lets tracing be optional without branching at each call site.
type Tracer struct {
	store     *tracing.Store
	exporters []TraceExporter
	ratio     float64
}

// NewTracer builds a tracer over a store and zero or more exporters. It returns
// nil when there is nowhere to put a trace, so "tracing enabled but nothing
// configured" costs the request path nothing.
func NewTracer(store *tracing.Store, ratio float64, exporters ...TraceExporter) *Tracer {
	if store == nil && len(exporters) == 0 {
		return nil
	}
	if ratio > 1 {
		ratio = 1
	}
	return &Tracer{store: store, exporters: exporters, ratio: ratio}
}

// begin starts the request's trace and computes the W3C context to propagate
// upstream.
//
// The outbound context is computed even when the request is not recorded,
// because a caller that is tracing must stay connected across a gateway that is
// not: dropping traceparent at the hop is indistinguishable, to the caller,
// from the gateway losing the request.
func (t *Tracer) begin(r *http.Request, rec *record) {
	if t == nil {
		// Tracing is not configured. Nothing is minted, and the caller's own
		// traceparent is already forwarded verbatim by the header copy, so a
		// deployment without tracing neither breaks nor invents a trace chain.
		return
	}
	parent, ok := tracing.Parse(r.Header.Get(tracing.HeaderTraceparent), r.Header.Get(tracing.HeaderTracestate))
	if !ok {
		// An inbound context is never trusted: a malformed one is replaced
		// rather than half-reused, which would produce a span whose parent
		// does not exist.
		parent = tracing.NewRootContext()
	}
	rootID := tracing.NewSpanID()

	record := t.shouldRecord(parent, ok, rec.requestID)
	// A caller that is tracing stays connected even when this gateway is not
	// recording, so the flag is inherited; with no valid inbound context the
	// gateway IS the root and the flag is simply its own decision.
	sampled := record
	if ok {
		sampled = parent.Sampled || record
	}
	outbound := tracing.Context{
		TraceID:    parent.TraceID,
		SpanID:     rootID,
		Sampled:    sampled,
		TraceState: parent.TraceState,
	}
	rec.traceparent = outbound.Header()
	if outbound.TraceState != "" {
		rec.tracestate = outbound.TraceState
	}
	if !record {
		return
	}

	rt := &requestTrace{
		trace:     tracing.NewTrace(parent.TraceID, rec.requestID, rec.start),
		parent:    parent,
		rootID:    rootID,
		store:     t.store,
		exporters: t.exporters,
	}
	rt.trace.Attributes = map[string]any{
		tracing.AttrRoute:   rec.route,
		tracing.AttrRequest: rec.requestID,
	}
	rec.trace = rt
}

// shouldRecord applies the sampling decision.
//
// A caller that sent a valid traceparent with the sampled flag clear is not
// recorded here either: the client has already decided this trace is not being
// kept, and a gateway that keeps half of it produces a trace that is missing
// its own downstream.
func (t *Tracer) shouldRecord(parent tracing.Context, parentOK bool, requestID string) bool {
	if t == nil || (t.store == nil && len(t.exporters) == 0) {
		return false
	}
	if parentOK && !parent.Sampled {
		return false
	}
	if t.ratio >= 1 {
		return true
	}
	if t.ratio <= 0 {
		return false
	}
	// Hash the request id rather than drawing a random number: the decision is
	// then stable for a given request id, so a retry of the same request is
	// sampled the same way and a test can assert on it without seeding.
	h := fnv.New32a()
	_, _ = h.Write([]byte(requestID))
	return float64(h.Sum32()%10000)/10000 < t.ratio
}

// requestTrace is one request's trace under construction. It is only touched by
// the goroutine serving that request, so it needs no lock of its own.
type requestTrace struct {
	trace     *tracing.Trace
	parent    tracing.Context
	rootID    string
	store     *tracing.Store
	exporters []TraceExporter
	spans     []tracing.Span
	events    []tracing.Event
}

// attemptSpan is the handle on one upstream attempt's client span.
//
// Its zero value is a no-op, so the attempt path never branches on whether
// tracing is on; that is the difference between a feature that can be left
// enabled in production and one that cannot.
type attemptSpan struct {
	rt   *requestTrace
	span tracing.Span
}

// startAttempt opens a client span for one upstream attempt.
func (rt *requestTrace) startAttempt(target string, attemptNo int) attemptSpan {
	if rt == nil {
		return attemptSpan{}
	}
	return attemptSpan{
		rt: rt,
		span: tracing.Span{
			TraceID:       rt.trace.TraceID,
			SpanID:        tracing.NewSpanID(),
			ParentSpanID:  rt.rootID,
			Name:          SpanUpstreamPrefix + target,
			Kind:          tracing.KindClient,
			StartUnixNano: time.Now().UnixNano(),
			Status:        tracing.StatusUnset,
			Attributes: map[string]any{
				tracing.AttrUpstream: target,
				"attempt":            attemptNo,
			},
		},
	}
}

// finish closes the attempt span with whatever the attempt reported.
//
// The status and outcome are read from the record rather than passed in,
// because every return path in attemptUpstream already sets them and a
// parameter would have to be threaded through eight of them.
func (a attemptSpan) finish(rec *record) {
	if a.rt == nil {
		return
	}
	a.span.EndUnixNano = time.Now().UnixNano()
	// DurationMS is derived by Trace.Add from the integer nanosecond pair; a
	// second computation here would be free to drift from it.
	a.span.Attributes["status"] = rec.status
	a.span.Attributes["outcome"] = string(rec.outcome)
	if rec.model != "" {
		a.span.Attributes[tracing.AttrModel] = rec.model
	}
	if rec.reason != "" {
		a.span.Attributes["reason"] = rec.reason
	}
	if rec.stream {
		a.span.Attributes["stream"] = true
	}
	if rec.frames > 0 {
		a.span.Attributes["frames"] = rec.frames
		a.span.Attributes["response_bytes"] = rec.respBytes
	}
	if rec.firstTokenSet {
		a.span.Attributes["first_token_ms"] = firstTokenMS(rec)
	}
	if rec.outcome == metrics.OutcomeSuccess {
		a.span.Status = tracing.StatusOK
	} else {
		a.span.Status = tracing.StatusError
	}
	a.rt.spans = append(a.rt.spans, a.span)
}

// addEvent records something that happened during the request but is not a
// timed operation of its own: a cache verdict, a stream summary, a failover.
func (rt *requestTrace) addEvent(name string, attrs map[string]any) {
	if rt == nil {
		return
	}
	rt.events = append(rt.events, tracing.Event{
		Name:         name,
		TimeUnixNano: time.Now().UnixNano(),
		Attributes:   attrs,
	})
}

// finish closes the root span, stores the trace and hands it to the exporters.
//
// It runs from the deferred recorder in ServeHTTP, after the response has been
// written, so nothing here can delay an answer.
func (rt *requestTrace) finish(rec *record) {
	if rt == nil {
		return
	}
	end := time.Now().UnixNano()
	status := tracing.StatusOK
	if rec.outcome != metrics.OutcomeSuccess {
		status = tracing.StatusError
	}
	attrs := map[string]any{
		tracing.AttrRoute:   rec.route,
		tracing.AttrRequest: rec.requestID,
		"path":              rec.path,
		"stream":            rec.stream,
		"status":            rec.status,
		"outcome":           string(rec.outcome),
		"request_bytes":     rec.reqBytes,
		"response_bytes":    rec.respBytes,
		"attempts":          rec.attempts,
	}
	if rec.upstream != "" {
		attrs[tracing.AttrUpstream] = rec.upstream
	}
	if rec.model != "" {
		attrs[tracing.AttrModel] = rec.model
	}
	if rec.tenant != "" {
		attrs["tenant"] = rec.tenant
	}
	if rec.session != "" {
		attrs["session"] = rec.session
	}
	if rec.reason != "" {
		attrs["reason"] = rec.reason
	}
	if rec.cacheStatus != "" {
		attrs["cache"] = rec.cacheStatus
		if rec.cacheReason != "" {
			attrs["cache_reason"] = rec.cacheReason
		}
	}
	if rec.idemKey != "" {
		// The key itself is recorded: an agent that retried after a crash is
		// diagnosed by finding both attempts under one key, and the trace is
		// where an operator looks for that. The decision distinguishes the
		// attempt that did the work from the one that was answered from the
		// store.
		attrs["idempotency_key"] = rec.idemKey
		attrs["idempotency"] = rec.idemDecision
		if rec.idemStore != "" {
			attrs["idempotency_store"] = rec.idemStore
		}
		if rec.idemReason != "" {
			attrs["idempotency_reason"] = rec.idemReason
		}
	}
	if len(rec.tried) > 1 {
		attrs["tried"] = rec.tried
	}
	if rec.frames > 0 {
		attrs["frames"] = rec.frames
	}
	if rec.firstTokenSet {
		attrs["first_token_ms"] = firstTokenMS(rec)
	}
	if !rec.usage.IsEmpty() {
		attrs["prompt_tokens"] = rec.usage.Prompt
		attrs["completion_tokens"] = rec.usage.Completion
		attrs["cached_tokens"] = rec.usage.Cached
	}

	// A locally-minted context has no parent to point at: naming the synthetic
	// span id NewRootContext invented would render as a parent that exists in
	// no other trace, which is worse than having none.
	parentSpanID := rt.parent.SpanID
	if !rt.parent.Remote {
		parentSpanID = ""
	}

	root := tracing.Span{
		TraceID:       rt.trace.TraceID,
		SpanID:        rt.rootID,
		ParentSpanID:  parentSpanID,
		Name:          SpanGateway,
		Kind:          tracing.KindServer,
		StartUnixNano: rec.start.UnixNano(),
		EndUnixNano:   end,
		Status:        status,
		Attributes:    attrs,
		Events:        rt.events,
	}
	rt.trace.Add(root)
	for _, s := range rt.spans {
		rt.trace.Add(s)
	}
	rt.trace.Status = status

	if rt.store != nil {
		rt.store.Add(rt.trace)
	}
	for _, e := range rt.exporters {
		if e != nil {
			e.Export(rt.trace)
		}
	}
}
