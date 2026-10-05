package tracing

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// helpers_test.go holds the fixtures shared by the three _test.go files in this
// package. The fixtures exist so that the store tests and the trace tests agree
// on what a "trace" is: a root server span, a child upstream/client span, and
// an event on each. A test that only ever builds one span cannot catch an
// ordering bug in Duration or a dropped field in Clone.

// baseTime is a fixed wall clock. Fixed, not time.Now(), so that a failure
// prints a diff that is about the code and not about how long the test took.
var baseTime = time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)

// testTraceID and testRequestID are 32 lowercase hex chars, which is what both
// the traceparent grammar and the gateway's X-InferGate-Request-Id produce.
const (
	testTraceID   = "4bf92f3577b34da6a3ce929d0e0e4736"
	testRequestID = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
)

// testTrace builds a complete two-span trace with attributes and events, with
// deterministic ids and times so tests can assert exact values.
func testTrace() *Trace {
	tr := &Trace{
		TraceID:    testTraceID,
		RequestID:  testRequestID,
		Attributes: map[string]any{"route": "/v1/chat/completions", "model": "gpt-oss-120b"},
		Status:     StatusOK,
	}
	tr.Add(Span{
		TraceID:       testTraceID,
		SpanID:        "00f067aa0ba902b7",
		ParentSpanID:  "",
		Name:          "POST /v1/chat/completions",
		Kind:          KindServer,
		StartUnixNano: baseTime.UnixNano(),
		EndUnixNano:   baseTime.Add(150 * time.Millisecond).UnixNano(),
		Status:        StatusOK,
		Attributes: map[string]any{
			AttrRoute:     "/v1/chat/completions",
			AttrUpstream:  "mock-a",
			AttrModel:     "gpt-oss-120b",
			"status_code": 200,
		},
		Events: []Event{
			{Name: "router.select", TimeUnixNano: baseTime.UnixNano(), Attributes: map[string]any{"upstream": "mock-a"}},
		},
	})
	tr.Add(Span{
		TraceID:       testTraceID,
		SpanID:        "b7ad6b7169203331",
		ParentSpanID:  "00f067aa0ba902b7",
		Name:          "upstream.request",
		Kind:          KindClient,
		StartUnixNano: baseTime.Add(2 * time.Millisecond).UnixNano(),
		EndUnixNano:   baseTime.Add(120 * time.Millisecond).UnixNano(),
		Status:        StatusOK,
		Attributes:    map[string]any{"attempt": 1},
		Events: []Event{
			{Name: "first_chunk", TimeUnixNano: baseTime.Add(40 * time.Millisecond).UnixNano(), Attributes: map[string]any{"ttft_ms": 38}},
		},
	})
	return tr
}

// testTraceWith builds a trace with n spans and an explicit trace id, so store
// tests can address each entry.
func testTraceWith(id string, n int) *Trace {
	tr := &Trace{
		TraceID:   id,
		RequestID: "req-" + id,
		Status:    StatusOK,
	}
	for i := 0; i < n; i++ {
		start := baseTime.Add(time.Duration(i) * time.Millisecond)
		tr.Add(Span{
			TraceID:       id,
			SpanID:        fmt.Sprintf("%016x", i+1),
			Name:          fmt.Sprintf("span-%d", i),
			Kind:          KindInternal,
			StartUnixNano: start.UnixNano(),
			EndUnixNano:   start.Add(5 * time.Millisecond).UnixNano(),
			Status:        StatusOK,
		})
	}
	return tr
}

// sameFloat compares two floating point durations. Durations come out of
// different paths (cached field vs recomputed from nanos) and must agree.
func sameFloat(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// wantFloat fails the test unless got is within 1e-9 of want.
func wantFloat(t *testing.T, label string, got, want float64) {
	t.Helper()
	if !sameFloat(got, want) {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

// wantString fails the test unless got equals want.
func wantString(t *testing.T, label, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", label, got, want)
	}
}
