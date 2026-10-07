package main

// Tracing checks: the span model, W3C propagation, request-id consistency,
// failover spans, the bounded store with its lookup and validation rules, and
// the two exporters (JSONL, OTLP/HTTP JSON).
//
// Every one of these runs against the assembled gateway over a real listener,
// because the claim is about what an operator sees at /admin/traces -- the span
// tree a package-level test could assert on is not the artifact this milestone
// promises. The recording upstream is what makes "the gateway propagated a
// child context" checkable at all: it reads the headers the gateway actually
// sent, not the ones it says it sent.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/infergate/infergate/internal/gateway"
	"github.com/infergate/infergate/internal/server"
	"github.com/infergate/infergate/internal/tracing"
)

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

type traceListBody struct {
	Enabled  bool              `json:"enabled"`
	Capacity int               `json:"capacity"`
	Stored   int               `json:"stored"`
	Dropped  int64             `json:"dropped"`
	Count    int               `json:"count"`
	Traces   []tracing.Summary `json:"traces"`
}

type tracingConfigBody struct {
	Enabled     bool           `json:"enabled"`
	Capacity    int            `json:"capacity"`
	SampleRatio float64        `json:"sample_ratio"`
	JSONLPath   string         `json:"jsonl_path"`
	Stored      int            `json:"stored"`
	Dropped     int64          `json:"dropped"`
	Exporters   []string       `json:"exporters"`
	ExportStats map[string]any `json:"export_stats"`
	OTLP        struct {
		Endpoint    string `json:"endpoint"`
		Timeout     string `json:"timeout"`
		ServiceName string `json:"service_name"`
		Headers     int    `json:"headers"`
	} `json:"otlp"`
}

// ---------------------------------------------------------------------------
// Decoding helpers
// ---------------------------------------------------------------------------

func getTrace(c *checker, st *stack, id, label string) (tracing.Trace, bool) {
	var tr tracing.Trace
	if !getJSON(c, st.url, "/admin/traces/"+id, label, &tr) {
		return tracing.Trace{}, false
	}
	return tr, true
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func attrNumber(span tracing.Span, key string) (float64, bool) {
	v, ok := span.Attributes[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

func attrString(span tracing.Span, key string) (string, bool) {
	v, ok := span.Attributes[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func attrBool(span tracing.Span, key string) (bool, bool) {
	v, ok := span.Attributes[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func attrStrings(span tracing.Span, key string) ([]string, bool) {
	v, ok := span.Attributes[key]
	if !ok {
		return nil, false
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func eventNamed(span tracing.Span, name string) (tracing.Event, bool) {
	for _, e := range span.Events {
		if e.Name == name {
			return e, true
		}
	}
	return tracing.Event{}, false
}

// ---------------------------------------------------------------------------
// 6. one trace per request
// ---------------------------------------------------------------------------

func checkTracePerRequest(c *checker) {
	section("6. one request produces one trace with a root span and one span per attempt")

	up := newUpstream(upstreamJSON, "traced answer")
	defer up.Close()
	cfg := m5Config(up)
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 64
	st := newStack(c, "trace-basic", &cfg)
	if st == nil {
		return
	}
	defer st.Close(c, "trace-basic")

	const reqID = "m5-trace-0001"
	res := post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "traced"),
		map[string]string{gateway.HeaderRequestID: reqID}, "trace: request")
	if !c.assert(res.status == 200, "6.1 the request is served (got %d: %s)", res.status, truncate(res.body, 160)) {
		return
	}
	c.equal(res.headerValue(gateway.HeaderRequestID), reqID, "6.2 the caller's request id is echoed back")

	var list traceListBody
	if !getJSON(c, st.url, "/admin/traces", "trace: list", &list) {
		return
	}
	c.equal(list.Enabled, true, "6.3 /admin/traces reports tracing enabled")
	c.equal(list.Capacity, 64, "6.4 /admin/traces reports the configured capacity")
	c.equal(list.Count, 1, "6.5 exactly one trace was recorded")
	c.equal(list.Stored, 1, "6.6 the store holds one trace")
	c.equal(list.Dropped, int64(0), "6.7 nothing was evicted")
	if !c.assert(len(list.Traces) == 1, "6.8 the list contains one summary (got %d)", len(list.Traces)) {
		return
	}

	summary := list.Traces[0]
	c.assert(isHex(summary.TraceID, 32), "6.9 the trace id is 32 lowercase hex characters (got %q)", summary.TraceID)
	c.equal(summary.RequestID, reqID, "6.10 the summary carries the request id")
	c.equal(summary.Route, "/v1/chat/completions", "6.11 the summary carries the route label")
	c.equal(summary.Upstream, "m5-mock", "6.12 the summary names the backend that served it")
	c.equal(summary.Model, "mock-gpt", "6.13 the summary carries the model")
	c.equal(summary.Status, tracing.StatusOK, "6.14 a served request summarizes as ok")
	c.equal(summary.SpanCount, 2, "6.15 one root span plus one attempt span")
	c.assert(summary.DurationMS >= 0, "6.16 the summary reports a duration (got %v)", summary.DurationMS)
	c.assert(summary.StartedAt != "", "6.17 the summary carries a start time (got %q)", summary.StartedAt)

	byRequest, ok := getTrace(c, st, reqID, "trace: lookup by request id")
	if !ok {
		return
	}
	c.equal(byRequest.TraceID, summary.TraceID, "6.18 a trace is findable by request id alone")
	byTrace, ok := getTrace(c, st, summary.TraceID, "trace: lookup by trace id")
	if !ok {
		return
	}
	c.equal(byTrace.RequestID, reqID, "6.19 the same trace is findable by trace id")
	c.equal(byTrace.Status, tracing.StatusOK, "6.20 the stored trace carries its status")

	if !c.assert(len(byTrace.Spans) == 2, "6.21 the trace has two spans (got %d)", len(byTrace.Spans)) {
		return
	}
	root := byTrace.Spans[0]
	attempt := byTrace.Spans[1]

	c.equal(root.Name, gateway.SpanGateway, "6.22 the first span is the gateway root span")
	c.equal(root.Kind, tracing.KindServer, "6.23 the root span is a server span")
	c.equal(root.ParentSpanID, "", "6.24 a locally-rooted trace has no phantom parent")
	c.equal(root.Status, tracing.StatusOK, "6.25 the root span is ok")
	c.equal(root.TraceID, byTrace.TraceID, "6.26 the root span belongs to the trace")
	c.assert(isHex(root.SpanID, 16), "6.27 the root span id is 16 lowercase hex characters (got %q)", root.SpanID)
	c.assert(root.EndUnixNano >= root.StartUnixNano, "6.28 the root span ends after it starts")
	c.assert(root.DurationMS > 0, "6.29 the root span reports a derived duration (got %v)", root.DurationMS)
	c.equal(attempt.Name, gateway.SpanUpstreamPrefix+"m5-mock", "6.30 the attempt span is named after the backend")
	c.equal(attempt.Kind, tracing.KindClient, "6.31 the attempt span is a client span")
	c.equal(attempt.ParentSpanID, root.SpanID, "6.32 the attempt span's parent is the root span")
	c.equal(attempt.TraceID, root.TraceID, "6.33 both spans share the trace id")
	c.equal(attempt.Status, tracing.StatusOK, "6.34 a successful attempt is ok")
	c.assert(isHex(attempt.SpanID, 16), "6.35 the attempt span id is 16 lowercase hex characters (got %q)", attempt.SpanID)
	c.assert(attempt.StartUnixNano >= root.StartUnixNano && attempt.EndUnixNano <= root.EndUnixNano,
		"6.36 the attempt span is contained in the root span's time range")

	if v, ok := attrString(root, tracing.AttrRequest); c.assert(ok, "6.37 the root span carries a request_id attribute") {
		c.equal(v, reqID, "6.38 the root request_id attribute is the caller's id")
	}
	if v, ok := attrString(root, tracing.AttrRoute); c.assert(ok, "6.39 the root span carries a route attribute") {
		c.equal(v, "/v1/chat/completions", "6.40 the root route attribute is the route label")
	}
	if v, ok := attrString(root, tracing.AttrUpstream); c.assert(ok, "6.41 the root span names the backend") {
		c.equal(v, "m5-mock", "6.42 the root upstream attribute is the serving backend")
	}
	if v, ok := attrString(root, tracing.AttrModel); c.assert(ok, "6.43 the root span carries the model") {
		c.equal(v, "mock-gpt", "6.44 the root model attribute is the requested model")
	}
	if v, ok := attrNumber(root, "status"); c.assert(ok, "6.45 the root span carries the HTTP status") {
		c.equal(v, 200.0, "6.46 the root status attribute is 200")
	}
	if v, ok := attrString(root, "outcome"); c.assert(ok, "6.47 the root span carries the outcome") {
		c.equal(v, "success", "6.48 the root outcome is success")
	}
	if v, ok := attrNumber(root, "attempts"); c.assert(ok, "6.49 the root span counts its attempts") {
		c.equal(v, 1.0, "6.50 one attempt was made")
	}
	if v, ok := attrNumber(root, "prompt_tokens"); c.assert(ok, "6.51 the root span carries the upstream's prompt tokens") {
		c.equal(v, 7.0, "6.52 the prompt token count is the upstream's")
	}
	if v, ok := attrNumber(root, "completion_tokens"); c.assert(ok, "6.53 the root span carries the completion tokens") {
		c.equal(v, 3.0, "6.54 the completion token count is the upstream's")
	}
	if v, ok := attrBool(root, "stream"); c.assert(ok, "6.55 the root span records whether the request streamed") {
		c.equal(v, false, "6.56 a whole-body request is not marked as a stream")
	}
	if v, ok := attrString(root, "path"); c.assert(ok, "6.57 the root span carries the raw path") {
		c.equal(v, "/v1/chat/completions", "6.58 the raw path is recorded alongside the route label")
	}
	if v, ok := attrNumber(root, "response_bytes"); c.assert(ok, "6.59 the root span records the response size") {
		c.assert(v > 0, "6.60 the response size is positive (got %v)", v)
	}
	if _, present := root.Attributes["tried"]; present {
		c.assert(false, "6.61 a single-attempt trace does not carry a tried list")
	} else {
		c.assert(true, "6.61 a single-attempt trace does not carry a tried list")
	}

	if v, ok := attrString(attempt, tracing.AttrUpstream); c.assert(ok, "6.62 the attempt span names its backend") {
		c.equal(v, "m5-mock", "6.63 the attempt upstream attribute is the backend name")
	}
	if v, ok := attrNumber(attempt, "attempt"); c.assert(ok, "6.64 the attempt span is numbered") {
		c.equal(v, 1.0, "6.65 the first attempt is numbered 1")
	}
	if v, ok := attrNumber(attempt, "status"); c.assert(ok, "6.66 the attempt span carries the upstream status") {
		c.equal(v, 200.0, "6.67 the attempt status is 200")
	}
	if v, ok := attrString(attempt, "outcome"); c.assert(ok, "6.68 the attempt span carries its outcome") {
		c.equal(v, "success", "6.69 the attempt outcome is success")
	}
	if _, present := attempt.Attributes["frames"]; present {
		c.assert(false, "6.70 a whole-body attempt records no stream frames")
	} else {
		c.assert(true, "6.70 a whole-body attempt records no stream frames")
	}

	// The upstream must have seen the gateway's own request id: a trace whose
	// request id does not match what the backend logged is not a trace.
	last := up.Last()
	c.equal(last.Header.Get("X-Request-Id"), reqID, "6.71 the upstream received the same request id the caller sent")
}

// ---------------------------------------------------------------------------
// 7. W3C propagation
// ---------------------------------------------------------------------------

func checkTracePropagation(c *checker) {
	section("7. traceparent is continued, minted, dropped and never trusted")

	const (
		callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
		callerSpan  = "00f067aa0ba902b7"
	)

	up := newUpstream(upstreamJSON, "propagation answer")
	defer up.Close()
	cfg := m5Config(up)
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 64
	st := newStack(c, "trace-propagation", &cfg)
	if st == nil {
		return
	}
	defer st.Close(c, "trace-propagation")

	// 7a: a valid inbound context is continued.
	res := post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "propagate"),
		map[string]string{
			tracing.HeaderTraceparent: "00-" + callerTrace + "-" + callerSpan + "-01",
			tracing.HeaderTracestate:  "vendor=abc",
			gateway.HeaderRequestID:   "m5-prop-a",
		}, "propagation: continued")
	c.assert(res.status == 200, "7.1 a request with a traceparent is served (got %d)", res.status)
	continued, ok := getTrace(c, st, "m5-prop-a", "propagation: continued trace")
	if ok {
		c.equal(continued.TraceID, callerTrace, "7.2 the caller's trace id is reused, not replaced")
		if len(continued.Spans) > 0 {
			root := continued.Spans[0]
			c.equal(root.ParentSpanID, callerSpan, "7.3 the gateway root span's parent is the caller's span")
			c.assert(root.SpanID != callerSpan, "7.4 the gateway mints its own span id")
			got := up.Last().Header.Get(tracing.HeaderTraceparent)
			c.equal(got, "00-"+callerTrace+"-"+root.SpanID+"-01",
				"7.5 the upstream received the gateway's child context, not the caller's")
			c.equal(up.Last().Header.Get(tracing.HeaderTracestate), "vendor=abc",
				"7.6 tracestate rides along unchanged")
		}
	}

	// 7b: no inbound context mints one.
	res = post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "mint"),
		map[string]string{gateway.HeaderRequestID: "m5-prop-b"}, "propagation: minted")
	c.assert(res.status == 200, "7.7 a request without a traceparent is served (got %d)", res.status)
	minted, ok := getTrace(c, st, "m5-prop-b", "propagation: minted trace")
	if ok {
		c.assert(isHex(minted.TraceID, 32), "7.8 a minted trace id is 32 lowercase hex characters (got %q)", minted.TraceID)
		if len(minted.Spans) > 0 {
			c.equal(minted.Spans[0].ParentSpanID, "", "7.9 a minted root span has no parent")
		}
		got := up.Last().Header.Get(tracing.HeaderTraceparent)
		c.assert(strings.HasPrefix(got, "00-"+minted.TraceID+"-"),
			"7.10 the upstream received a traceparent for the minted trace (got %q)", got)
		c.assert(strings.HasSuffix(got, "-01"), "7.11 the gateway's own decision sets the sampled flag (got %q)", got)
		c.equal(len(strings.Split(got, "-")), 4, "7.12 the propagated traceparent has four parts")
	}

	// 7c: a caller that dropped sampling stays dropped here.
	res = post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "drop"),
		map[string]string{
			tracing.HeaderTraceparent: "00-" + callerTrace + "-" + callerSpan + "-00",
			gateway.HeaderRequestID:   "m5-prop-c",
		}, "propagation: dropped")
	c.assert(res.status == 200, "7.13 a request with sampling off is still served (got %d)", res.status)
	lookup := get(c, st.url, "/admin/traces/m5-prop-c", "propagation: dropped lookup")
	c.equal(lookup.status, http.StatusNotFound, "7.14 the caller's drop decision is honoured, so nothing was stored")
	got := up.Last().Header.Get(tracing.HeaderTraceparent)
	c.assert(strings.HasPrefix(got, "00-"+callerTrace+"-"),
		"7.15 the caller's trace id is still propagated when not recording (got %q)", got)
	c.assert(strings.HasSuffix(got, "-00"), "7.16 the sampled flag stays clear (got %q)", got)

	// 7d: a malformed context is replaced, not half-reused.
	res = post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "garbage"),
		map[string]string{
			tracing.HeaderTraceparent: "01-" + strings.ToUpper(callerTrace) + "-" + callerSpan + "-01",
			gateway.HeaderRequestID:   "m5-prop-d",
		}, "propagation: malformed")
	c.assert(res.status == 200, "7.17 a request with a malformed traceparent is served (got %d)", res.status)
	replaced, ok := getTrace(c, st, "m5-prop-d", "propagation: malformed trace")
	if ok {
		c.assert(replaced.TraceID != strings.ToUpper(callerTrace),
			"7.18 a malformed traceparent is replaced rather than half-reused")
		if len(replaced.Spans) > 0 {
			c.equal(replaced.Spans[0].ParentSpanID, "", "7.19 the replaced context leaves the root parentless")
		}
	}

	// 7e: ratio 0 records nothing but still propagates.
	up0 := newUpstream(upstreamJSON, "ratio zero answer")
	defer up0.Close()
	cfg0 := m5Config(up0)
	cfg0.Tracing.Enabled = true
	cfg0.Tracing.SampleRatio = 0.0001
	st0 := newStack(c, "trace-ratio-zero", &cfg0)
	if st0 == nil {
		return
	}
	defer st0.Close(c, "trace-ratio-zero")
	res = post(c, st0.url, "/v1/chat/completions", chatBody("mock-gpt", "ratio zero"),
		map[string]string{
			tracing.HeaderTraceparent: "00-" + callerTrace + "-" + callerSpan + "-01",
			gateway.HeaderRequestID:   "m5-prop-e",
		}, "propagation: ratio near zero")
	c.assert(res.status == 200, "7.20 a request is served with a near-zero sample ratio (got %d)", res.status)
	propagated := up0.Last().Header.Get(tracing.HeaderTraceparent)
	c.assert(strings.HasPrefix(propagated, "00-"+callerTrace+"-"),
		"7.21 an unsampled request still propagates the caller's trace (got %q)", propagated)
	listBody := get(c, st0.url, "/admin/traces", "propagation: ratio near zero list")
	var zeroList traceListBody
	if err := listBody.json(&zeroList); err == nil {
		c.equal(zeroList.Count, 0, "7.22 a near-zero ratio records essentially nothing")
	}
}

// ---------------------------------------------------------------------------
// 8. sampling is deterministic
// ---------------------------------------------------------------------------

func checkTraceSampling(c *checker) {
	section("8. sampling is deterministic per request id and does discriminate")

	up := newUpstream(upstreamJSON, "sampling answer")
	defer up.Close()
	cfg := m5Config(up)
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 256
	cfg.Tracing.SampleRatio = 0.5
	st := newStack(c, "trace-sampling", &cfg)
	if st == nil {
		return
	}
	defer st.Close(c, "trace-sampling")

	const fixed = "m5-sampling-fixed"
	for i := 0; i < 8; i++ {
		res := post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "stable"),
			map[string]string{gateway.HeaderRequestID: fixed}, "sampling: repeated id")
		if res.status != 200 {
			c.assert(false, "8.1 a repeated request is served (got %d)", res.status)
			return
		}
	}
	var list traceListBody
	if !getJSON(c, st.url, "/admin/traces?limit=500", "sampling: list", &list) {
		return
	}
	recorded := 0
	for _, s := range list.Traces {
		if s.RequestID == fixed {
			recorded++
		}
	}
	c.assert(recorded == 0 || recorded == 8,
		"8.2 the same request id is sampled the same way every time (got %d of 8)", recorded)

	for i := 0; i < 40; i++ {
		id := "m5-sampling-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26))
		res := post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "spread"),
			map[string]string{gateway.HeaderRequestID: id}, "sampling: spread")
		if res.status != 200 {
			c.assert(false, "8.3 a spread request is served (got %d)", res.status)
			return
		}
	}
	if !getJSON(c, st.url, "/admin/traces?limit=500", "sampling: list after spread", &list) {
		return
	}
	c.assert(list.Count > 0 && list.Count < 48,
		"8.4 a 0.5 ratio records some requests and not others (recorded %d of at most 48)", list.Count)
	c.assert(list.Stored == list.Count, "8.5 every stored trace is listed while the ring is not full")
	c.equal(list.Dropped, int64(0), "8.6 a 256-trace ring is not full after 48 requests")
}

// ---------------------------------------------------------------------------
// 9. failover spans
// ---------------------------------------------------------------------------

func checkTraceFailover(c *checker) {
	section("9. a failover records one span per attempt and an event")

	primary := newUpstream(upstreamFail, "")
	defer primary.Close()
	backup := newUpstream(upstreamJSON, "backup answer")
	defer backup.Close()

	cfg := baseConfig(upstreamConfig("m5-primary", primary.URL()), upstreamConfig("m5-backup", backup.URL()))
	cfg.Upstreams[1].Priority = 2
	cfg.Health.MinRequests = 1000
	cfg.Health.MaxFailuresPerRequest = 2
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 32
	st := newStack(c, "trace-failover", &cfg)
	if st == nil {
		return
	}
	defer st.Close(c, "trace-failover")

	res := post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "failover"),
		map[string]string{gateway.HeaderRequestID: "m5-failover"}, "failover: request")
	c.assert(res.status == 200, "9.1 the backup serves the request (got %d: %s)", res.status, truncate(res.body, 160))
	c.equal(res.headerValue(gateway.HeaderUpstreamName), "m5-backup", "9.2 the response names the backup")
	c.equal(res.headerValue(gateway.HeaderAttempt), "2", "9.3 the response reports the second attempt")
	tried := res.headerValue(gateway.HeaderTried)
	c.assert(strings.Contains(tried, "m5-primary") && strings.Contains(tried, "m5-backup"),
		"9.4 the response lists both attempts in order (got %q)", tried)
	c.equal(primary.Count(), 1, "9.5 the failing backend was called once")
	c.equal(backup.Count(), 1, "9.6 the backup was called once")

	tr, ok := getTrace(c, st, "m5-failover", "failover: trace")
	if !ok {
		return
	}
	if !c.assert(len(tr.Spans) == 3, "9.7 one root span per request plus one span per attempt (got %d)", len(tr.Spans)) {
		return
	}
	root := tr.Spans[0]
	first := tr.Spans[1]
	second := tr.Spans[2]

	c.equal(root.Name, gateway.SpanGateway, "9.8 the root span comes first")
	if v, ok := attrNumber(root, "attempts"); c.assert(ok, "9.9 the root span counts the attempts") {
		c.equal(v, 2.0, "9.10 two attempts are recorded")
	}
	if v, ok := attrString(root, tracing.AttrUpstream); c.assert(ok, "9.11 the root span names the serving backend") {
		c.equal(v, "m5-backup", "9.12 the root upstream is the backend that actually answered")
	}
	if v, ok := attrStrings(root, "tried"); c.assert(ok, "9.13 the root span lists every backend tried") {
		c.equal(strings.Join(v, ","), "m5-primary,m5-backup", "9.14 the tried list is in attempt order")
	}
	ev, ok := eventNamed(root, "failover")
	if c.assert(ok, "9.15 the root span carries a failover event") {
		if v, ok := ev.Attributes["failovers"]; ok {
			c.equal(v, 1.0, "9.16 the failover event counts one failover")
		} else {
			c.assert(false, "9.16 the failover event counts one failover")
		}
	}

	c.equal(first.Name, gateway.SpanUpstreamPrefix+"m5-primary", "9.17 the first span names the failing backend")
	c.equal(second.Name, gateway.SpanUpstreamPrefix+"m5-backup", "9.18 the second span names the backup")
	c.equal(first.ParentSpanID, root.SpanID, "9.19 both attempt spans are children of the root")
	c.equal(second.ParentSpanID, root.SpanID, "9.20 both attempt spans are children of the root")
	c.equal(first.Status, tracing.StatusError, "9.21 the failed attempt span is marked as an error")
	c.equal(second.Status, tracing.StatusOK, "9.22 the successful attempt span is ok")
	if v, ok := attrNumber(first, "status"); c.assert(ok, "9.23 the failed attempt records the upstream status") {
		c.equal(v, 500.0, "9.24 the failed attempt recorded the 500")
	}
	if v, ok := attrNumber(second, "status"); c.assert(ok, "9.25 the retried attempt records its status") {
		c.equal(v, 200.0, "9.26 the retried attempt recorded the 200")
	}
	if v, ok := attrNumber(first, "attempt"); c.assert(ok, "9.27 the first attempt is numbered") {
		c.equal(v, 1.0, "9.28 the first attempt is 1")
	}
	if v, ok := attrNumber(second, "attempt"); c.assert(ok, "9.29 the second attempt is numbered") {
		c.equal(v, 2.0, "9.30 the second attempt is 2")
	}
}

// ---------------------------------------------------------------------------
// 10. streaming
// ---------------------------------------------------------------------------

func checkStreamTrace(c *checker) {
	section("10. a streamed response is one span with its frame and first-token facts")

	up := newUpstream(upstreamStream, "")
	defer up.Close()
	cfg := m5Config(up)
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 32
	st := newStack(c, "trace-stream", &cfg)
	if st == nil {
		return
	}
	defer st.Close(c, "trace-stream")

	res := post(c, st.url, "/v1/chat/completions", streamChatBody("mock-gpt", "stream"),
		map[string]string{gateway.HeaderRequestID: "m5-stream"}, "stream: request")
	if !c.assert(res.status == 200, "10.1 the stream is served (got %d: %s)", res.status, truncate(res.body, 160)) {
		return
	}
	relayed := strings.Count(res.body, "data: ")
	c.assert(relayed >= 4, "10.2 the relayed body contains the frames (got %d)", relayed)
	c.assert(strings.Contains(res.body, "[DONE]"), "10.3 the terminal frame is relayed")

	tr, ok := getTrace(c, st, "m5-stream", "stream: trace")
	if !ok {
		return
	}
	root := tr.Spans[0]
	frames, hasFrames := attrNumber(root, "frames")
	if c.assert(hasFrames, "10.4 the root span records the frame count") {
		c.equal(frames, float64(relayed), "10.5 the recorded frame count is what the client received")
	}
	if v, ok := attrBool(root, "stream"); c.assert(ok, "10.6 the root span records that it streamed") {
		c.equal(v, true, "10.7 the streamed request is marked as a stream")
	}
	firstToken, hasFirst := attrNumber(root, "first_token_ms")
	c.assert(hasFirst, "10.8 the root span records the first-token latency")
	c.assert(!hasFirst || firstToken > 0, "10.9 the first-token latency is positive (got %v)", firstToken)

	if !c.assert(len(tr.Spans) == 2, "10.10 a streamed request still makes exactly one attempt (got %d)", len(tr.Spans)) {
		return
	}
	attempt := tr.Spans[1]
	if v, ok := attrBool(attempt, "stream"); c.assert(ok, "10.11 the attempt span records that it streamed") {
		c.equal(v, true, "10.12 the attempt span is marked as a stream")
	}
	if v, ok := attrNumber(attempt, "frames"); c.assert(ok, "10.13 the attempt span records the frame count") {
		c.equal(v, frames, "10.14 the attempt and the root agree on the frame count")
	}
	if _, ok := attrNumber(attempt, "first_token_ms"); !c.assert(ok, "10.15 the attempt span records the first-token latency") {
	}
	if v, ok := attrNumber(attempt, "response_bytes"); c.assert(ok, "10.16 the attempt span records the relayed bytes") {
		c.assert(v > 0, "10.17 the relayed byte count is positive (got %v)", v)
	}

	ev, ok := eventNamed(root, "stream.complete")
	if c.assert(ok, "10.18 the root span carries a stream.complete event") {
		if v, ok := ev.Attributes["frames"]; ok {
			c.equal(v, frames, "10.19 the stream event agrees with the span on the frame count")
		} else {
			c.assert(false, "10.19 the stream event agrees with the span on the frame count")
		}
		if _, ok := ev.Attributes["first_token_ms"]; !ok {
			c.assert(false, "10.20 the stream event carries the first-token latency")
		} else {
			c.assert(true, "10.20 the stream event carries the first-token latency")
		}
	}

	// The double-count regression: one streamed request contributes exactly one
	// first-token observation and one frame total, not two of each.
	res = get(c, st.url, "/metrics", "stream: metrics")
	samples := parseSamples(res.body)
	firstGroups := histogramGroups(samples, "infergate_first_token_seconds")
	c.assert(len(firstGroups) == 1, "10.21 one streamed request produces one TTFT series (got %d)", len(firstGroups))
	if len(firstGroups) == 1 {
		c.equal(firstGroups[0].count, 1.0, "10.22 the TTFT histogram counted the stream exactly once")
	}
	if mean, ok := findSample(samples, "infergate_first_token_seconds_mean", map[string]string{"upstream": "m5-mock"}); ok {
		detail("first-token mean gauge: %v", mean.value)
	}
	if frameTotal, ok := findSample(samples, "infergate_stream_frames_total", map[string]string{"upstream": "m5-mock"}); ok {
		c.equal(frameTotal.value, float64(relayed), "10.23 the frame counter counted each relayed frame once")
	}
	failovers, _ := findSample(samples, "infergate_failovers_total", map[string]string{"upstream": "m5-mock"})
	c.equal(failovers.value, 0.0, "10.24 a streamed request recorded no phantom failover")
}

// ---------------------------------------------------------------------------
// 11. the bounded store
// ---------------------------------------------------------------------------

func checkTraceStore(c *checker) {
	section("11. the store is bounded, ordered, validated and can render jsonl")

	up := newUpstream(upstreamJSON, "store answer")
	defer up.Close()
	cfg := m5Config(up)
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 2
	st := newStack(c, "trace-store", &cfg)
	if st == nil {
		return
	}
	defer st.Close(c, "trace-store")

	ids := []string{"m5-store-1", "m5-store-2", "m5-store-3", "m5-store-4", "m5-store-5"}
	for _, id := range ids {
		res := post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "store"),
			map[string]string{gateway.HeaderRequestID: id}, "store: request")
		if res.status != 200 {
			c.assert(false, "11.1 every request is served (got %d)", res.status)
			return
		}
	}

	var list traceListBody
	if !getJSON(c, st.url, "/admin/traces", "store: list", &list) {
		return
	}
	c.equal(list.Stored, 2, "11.2 a capacity-2 ring holds two traces")
	c.equal(list.Dropped, int64(3), "11.3 the three evicted traces are counted as dropped")
	c.equal(list.Count, 2, "11.4 the list returns the stored traces")
	if len(list.Traces) == 2 {
		c.equal(list.Traces[0].RequestID, ids[4], "11.5 the list is newest first")
		c.equal(list.Traces[1].RequestID, ids[3], "11.6 the second row is the one before it")
	}

	evicted := get(c, st.url, "/admin/traces/"+ids[0], "store: evicted lookup")
	c.equal(evicted.status, http.StatusNotFound, "11.7 an evicted trace is a 404, not a lie")
	c.assert(strings.Contains(evicted.body, "trace not found"), "11.8 the 404 says the trace is missing")
	kept := get(c, st.url, "/admin/traces/"+ids[4], "store: kept lookup")
	c.equal(kept.status, http.StatusOK, "11.9 the newest trace is still findable")

	limited := get(c, st.url, "/admin/traces?limit=1", "store: limit=1")
	var one traceListBody
	if err := limited.json(&one); err == nil {
		c.equal(one.Count, 1, "11.10 ?limit=1 returns one summary")
		c.equal(len(one.Traces), 1, "11.11 ?limit=1 returns one element")
	}
	for _, bad := range []string{"limit=0", "limit=-3", "limit=abc"} {
		res := get(c, st.url, "/admin/traces?"+bad, "store: bad limit")
		c.equal(res.status, http.StatusBadRequest, "11.12 /admin/traces?%s is a 400", bad)
	}
	big := get(c, st.url, "/admin/traces?limit=99999", "store: huge limit")
	c.equal(big.status, http.StatusOK, "11.13 an over-large limit is clamped, not rejected")

	// Lookup validation: a request id is a loose charset, a trace id is 32 hex.
	badID := get(c, st.url, "/admin/traces/bad%20id", "store: bad id")
	c.equal(badID.status, http.StatusBadRequest, "11.14 a lookup id with an illegal character is a 400")
	longID := strings.Repeat("a", 65)
	longRes := get(c, st.url, "/admin/traces/"+longID, "store: long id")
	c.equal(longRes.status, http.StatusBadRequest, "11.15 an over-long lookup id is a 400")
	c.assert(strings.Contains(longRes.body, "32 hex"), "11.16 the 400 explains the accepted id shapes")
	missing := get(c, st.url, "/admin/traces/0123456789abcdef0123456789abcdef", "store: missing id")
	c.equal(missing.status, http.StatusNotFound, "11.17 a well-formed unknown trace id is a 404")

	// JSONL rendering of the list: one trace per line, streamed.
	jsonl := get(c, st.url, "/admin/traces?format=jsonl", "store: jsonl list")
	c.equal(jsonl.status, http.StatusOK, "11.18 ?format=jsonl answers 200")
	c.assert(strings.Contains(jsonl.headerValue("Content-Type"), "ndjson"),
		"11.19 ?format=jsonl uses the ndjson content type (got %q)", jsonl.headerValue("Content-Type"))
	lines := nonEmptyLines(jsonl.body)
	c.equal(len(lines), 2, "11.20 one line per stored trace")
	for i, line := range lines {
		var tr tracing.Trace
		if !c.assert(json.Unmarshal([]byte(line), &tr) == nil, "11.21 jsonl line %d parses as a trace", i+1) {
			continue
		}
		c.assert(isHex(tr.TraceID, 32), "11.22 jsonl line %d carries a trace id", i+1)
		c.assert(tr.RequestID != "", "11.23 jsonl line %d carries its request id", i+1)
	}

	single := get(c, st.url, "/admin/traces/"+ids[4]+"?format=jsonl", "store: single jsonl")
	c.equal(single.status, http.StatusOK, "11.24 a single trace renders as jsonl too")
	c.equal(len(nonEmptyLines(single.body)), 1, "11.25 a single-trace jsonl response is one line")
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 12. JSONL export
// ---------------------------------------------------------------------------

func checkJSONLExport(c *checker) {
	section("12. the JSONL exporter writes one line per trace and appends")

	// The temp tree lives under the repo's tmp/ rather than the OS temp dir:
	// this host's sandbox denies writes there, and a gate that cannot create
	// its own scratch file would fail for a reason that has nothing to do with
	// the exporter.
	base := filepath.Join("tmp", "verify-m5")
	if err := os.MkdirAll(base, 0o755); err != nil {
		c.assert(false, "12.1 create the scratch dir %s (%v)", base, err)
		return
	}
	dir, err := os.MkdirTemp(base, "jsonl-")
	if err != nil {
		c.assert(false, "12.1 create a temp dir under %s (%v)", base, err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "traces.jsonl")

	up := newUpstream(upstreamJSON, "jsonl answer")
	defer up.Close()
	cfg := m5Config(up)
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 32
	cfg.Tracing.JSONLPath = path
	st := newStack(c, "trace-jsonl", &cfg)
	if st == nil {
		return
	}

	ids := []string{"m5-jsonl-1", "m5-jsonl-2", "m5-jsonl-3"}
	for _, id := range ids {
		res := post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "jsonl"),
			map[string]string{gateway.HeaderRequestID: id}, "jsonl: request")
		if res.status != 200 {
			c.assert(false, "12.2 every request is served (got %d)", res.status)
			st.Close(c, "trace-jsonl")
			return
		}
	}

	var body tracingConfigBody
	if getJSON(c, st.url, "/admin/tracing", "jsonl: config", &body) {
		c.equal(len(body.Exporters), 1, "12.3 /admin/tracing lists one exporter")
		if len(body.Exporters) == 1 {
			c.equal(body.Exporters[0], "jsonl", "12.4 the exporter is the jsonl writer")
		}
		if stats, ok := body.ExportStats["jsonl"].(map[string]any); ok {
			c.equal(stats["written"], 3.0, "12.5 the exporter reports three traces written")
			c.equal(stats["path"], path, "12.6 the exporter reports the path it writes to")
		} else {
			c.assert(false, "12.7 /admin/tracing reports jsonl export stats")
		}
	}

	// Closing the server must flush: the file is read after Close returns.
	st.Close(c, "trace-jsonl")
	lines := readLines(c, path, "jsonl: file")
	c.equal(len(lines), 3, "12.8 three requests produced three lines")
	seen := map[string]bool{}
	for i, line := range lines {
		var tr tracing.Trace
		if !c.assert(json.Unmarshal([]byte(line), &tr) == nil, "12.9 line %d parses as a trace", i+1) {
			continue
		}
		seen[tr.RequestID] = true
		c.assert(tr.TraceID != "" && len(tr.Spans) > 0, "12.10 line %d carries spans", i+1)
	}
	for _, id := range ids {
		c.assert(seen[id], "12.11 the file contains the trace for %s", id)
	}

	// A second server on the same path appends rather than truncating: that is
	// what makes the file useful across a restart.
	cfg2 := m5Config(up)
	cfg2.Tracing.Enabled = true
	cfg2.Tracing.Capacity = 32
	cfg2.Tracing.JSONLPath = path
	st2 := newStack(c, "trace-jsonl-restart", &cfg2)
	if st2 == nil {
		return
	}
	res := post(c, st2.url, "/v1/chat/completions", chatBody("mock-gpt", "jsonl after restart"),
		map[string]string{gateway.HeaderRequestID: "m5-jsonl-4"}, "jsonl: request after restart")
	c.assert(res.status == 200, "12.12 a request after a restart is served (got %d)", res.status)
	st2.Close(c, "trace-jsonl-restart")
	after := readLines(c, path, "jsonl: file after restart")
	c.equal(len(after), 4, "12.13 a restarted exporter appends to the existing file")
	c.assert(strings.Contains(after[len(after)-1], "m5-jsonl-4"), "12.14 the appended line is the new trace")

	// A path that cannot be opened must fail startup, not silently drop traces.
	bad := m5Config(up)
	bad.Tracing.Enabled = true
	bad.Tracing.JSONLPath = dir // a directory, not a file
	bad.Server.Listen = ":0"
	if err := bad.Validate(); err != nil {
		c.assert(false, "12.15 a config with an unwritable jsonl path still validates (%v)", err)
	} else if _, err := server.NewServer(&bad, newLogger(c)); err == nil {
		c.assert(false, "12.16 an unwritable jsonl path fails server construction")
	} else {
		c.assert(strings.Contains(err.Error(), "server: tracing: jsonl export"),
			"12.17 an unwritable jsonl path fails startup by name (%v)", err)
	}
}

func readLines(c *checker, path, label string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		c.assert(false, "%s: read %s (%v)", label, path, err)
		return nil
	}
	return nonEmptyLines(string(raw))
}

// ---------------------------------------------------------------------------
// 13. OTLP export
// ---------------------------------------------------------------------------

type otlpCollector struct {
	server *httptest.Server
	mu     sync.Mutex
	got    []otlpRequest
}

type otlpRequest struct {
	Path        string
	ContentType string
	Body        string
}

func newCollector() *otlpCollector {
	col := &otlpCollector{}
	col.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		col.mu.Lock()
		col.got = append(col.got, otlpRequest{
			Path:        r.URL.Path,
			ContentType: r.Header.Get("Content-Type"),
			Body:        string(raw),
		})
		col.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"partialSuccess":{}}`)
	}))
	return col
}

func (col *otlpCollector) URL() string { return col.server.URL }

func (col *otlpCollector) Close() { col.server.Close() }

func (col *otlpCollector) Requests() []otlpRequest {
	col.mu.Lock()
	defer col.mu.Unlock()
	return append([]otlpRequest(nil), col.got...)
}

func mapAt(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func sliceAt(v any) []any {
	s, _ := v.([]any)
	return s
}

func strAt(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// numAt reads a JSON number; OTLP/JSON encodes span kind as the enum number
// (2 = SPAN_KIND_SERVER, 3 = SPAN_KIND_CLIENT), not as a name.
func numAt(m map[string]any, key string) (float64, bool) {
	n, ok := m[key].(float64)
	return n, ok
}

// otlpPayload extracts the first scope's spans plus the service name the
// collector saw, the way a collector's own decoder would.
func otlpPayload(body string) (spans []map[string]any, scope, service string, ok bool) {
	var doc map[string]any
	if json.Unmarshal([]byte(body), &doc) != nil {
		return nil, "", "", false
	}
	resourceSpans := sliceAt(doc["resourceSpans"])
	if len(resourceSpans) == 0 {
		return nil, "", "", false
	}
	first := mapAt(resourceSpans[0])
	for _, attr := range sliceAt(mapAt(first["resource"])["attributes"]) {
		a := mapAt(attr)
		if strAt(a, "key") == "service.name" {
			service = strAt(mapAt(a["value"]), "stringValue")
		}
	}
	scopeSpans := sliceAt(first["scopeSpans"])
	if len(scopeSpans) == 0 {
		return nil, "", service, false
	}
	scopeMap := mapAt(scopeSpans[0])
	scope = strAt(mapAt(scopeMap["scope"]), "name")
	for _, raw := range sliceAt(scopeMap["spans"]) {
		spans = append(spans, mapAt(raw))
	}
	return spans, scope, service, true
}

func checkOTLPExport(c *checker) {
	section("13. the OTLP exporter posts a well-formed OTLP/HTTP JSON payload")

	col := newCollector()
	defer col.Close()

	up := newUpstream(upstreamJSON, "otlp answer")
	defer up.Close()
	cfg := m5Config(up)
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 32
	cfg.Tracing.OTLP.Endpoint = col.URL()
	cfg.Tracing.OTLP.ServiceName = "infergate-gate"
	cfg.Tracing.OTLP.Headers = map[string]string{"x-scope-orgid": "infergate-test"}
	st := newStack(c, "trace-otlp", &cfg)
	if st == nil {
		return
	}

	for i := 0; i < 2; i++ {
		res := post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "otlp"),
			map[string]string{gateway.HeaderRequestID: "m5-otlp-" + string(rune('1'+i))}, "otlp: request")
		if res.status != 200 {
			c.assert(false, "13.1 every request is served (got %d)", res.status)
			st.Close(c, "trace-otlp")
			return
		}
	}

	var body tracingConfigBody
	if getJSON(c, st.url, "/admin/tracing", "otlp: config", &body) {
		c.equal(len(body.Exporters), 1, "13.2 /admin/tracing lists one exporter")
		if len(body.Exporters) == 1 {
			c.equal(body.Exporters[0], "otlp", "13.3 the exporter is the OTLP writer")
		}
		c.equal(body.OTLP.Endpoint, col.URL(), "13.4 the configured endpoint is reported")
	}

	// Close drains the exporter; the collector must have seen everything then.
	st.Close(c, "trace-otlp")
	got := col.Requests()
	if !c.assert(len(got) == 2, "13.5 one export per trace reached the collector (got %d)", len(got)) {
		return
	}
	c.equal(got[0].Path, "/v1/traces", "13.6 spans are posted to /v1/traces")
	c.assert(strings.HasPrefix(got[0].ContentType, "application/json"),
		"13.7 the payload is JSON (got %q)", got[0].ContentType)

	spans, scope, service, ok := otlpPayload(got[0].Body)
	if !c.assert(ok, "13.8 the payload has the OTLP envelope (%s)", truncate(got[0].Body, 200)) {
		return
	}
	c.equal(service, "infergate-gate", "13.9 resource service.name is the configured service name")
	c.equal(scope, "infergate.gateway", "13.10 the scope name identifies the gateway instrumentation")
	if !c.assert(len(spans) == 2, "13.11 one trace exports its root and attempt spans (got %d)", len(spans)) {
		return
	}
	root := spans[0]
	attempt := spans[1]
	c.equal(strAt(root, "name"), gateway.SpanGateway, "13.12 the root span name survives the export")
	if kind, ok := numAt(root, "kind"); c.assert(ok, "13.13 a server span exports a numeric kind") {
		c.equal(kind, 2.0, "13.14 a server span exports kind 2 (SPAN_KIND_SERVER)")
	}
	c.equal(strAt(root, "parentSpanId"), "", "13.15 a root span exports no parent")
	c.assert(strAt(root, "traceId") != "", "13.16 the exported span carries its trace id")
	c.assert(strAt(root, "spanId") != "", "13.17 the exported span carries its span id")
	c.assert(isDecimal(strAt(root, "startTimeUnixNano")), "13.18 start time is a decimal string of nanoseconds")
	c.assert(isDecimal(strAt(root, "endTimeUnixNano")), "13.19 end time is a decimal string of nanoseconds")
	if status := mapAt(root["status"]); status != nil {
		c.equal(status["code"], 1.0, "13.20 a successful span exports status code 1 (OK)")
	} else {
		c.assert(false, "13.21 the exported span carries a status")
	}
	c.equal(strAt(attempt, "name"), gateway.SpanUpstreamPrefix+"m5-mock", "13.22 the attempt span name survives the export")
	if kind, ok := numAt(attempt, "kind"); c.assert(ok, "13.23 a client span exports a numeric kind") {
		c.equal(kind, 3.0, "13.24 a client span exports kind 3 (SPAN_KIND_CLIENT)")
	}
	c.equal(strAt(attempt, "parentSpanId"), strAt(root, "spanId"), "13.25 the exported parent link is preserved")
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 14. tracing disabled
// ---------------------------------------------------------------------------

func checkTracingDisabled(c *checker) {
	section("14. a gateway without tracing keeps propagating and records nothing")

	up := newUpstream(upstreamJSON, "no tracing answer")
	defer up.Close()
	cfg := m5Config(up)
	cfg.Tracing.Enabled = false
	st := newStack(c, "trace-disabled", &cfg)
	if st == nil {
		return
	}
	defer st.Close(c, "trace-disabled")

	var list traceListBody
	if !getJSON(c, st.url, "/admin/traces", "disabled: list", &list) {
		return
	}
	c.equal(list.Enabled, false, "14.1 /admin/traces reports tracing disabled")
	c.equal(list.Capacity, 1024, "14.2 a disabled gateway still reports the capacity it would use")
	c.equal(list.Count, 0, "14.3 a disabled gateway lists no traces")
	c.equal(list.Dropped, int64(0), "14.4 a disabled gateway has dropped nothing")

	lookup := get(c, st.url, "/admin/traces/0123456789abcdef0123456789abcdef", "disabled: lookup")
	c.equal(lookup.status, http.StatusNotFound, "14.5 a disabled gateway refuses replay with a 404")
	c.assert(strings.Contains(lookup.body, "tracing is disabled"), "14.6 the 404 says tracing is disabled")

	var body tracingConfigBody
	if getJSON(c, st.url, "/admin/tracing", "disabled: config", &body) {
		c.equal(body.Enabled, false, "14.7 /admin/tracing reports disabled")
		c.equal(len(body.Exporters), 0, "14.8 a disabled gateway has no exporters")
		c.equal(body.Stored, 0, "14.9 a disabled gateway stores nothing")
	}

	const (
		callerTrace = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		callerSpan  = "bbbbbbbbbbbbbbbb"
	)
	res := post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "passthrough"),
		map[string]string{tracing.HeaderTraceparent: "00-" + callerTrace + "-" + callerSpan + "-01"},
		"disabled: request")
	c.assert(res.status == 200, "14.10 a request is served with tracing off (got %d)", res.status)
	c.equal(up.Last().Header.Get(tracing.HeaderTraceparent), "00-"+callerTrace+"-"+callerSpan+"-01",
		"14.11 with tracing off the caller's traceparent is forwarded verbatim, neither replaced nor dropped")
}
