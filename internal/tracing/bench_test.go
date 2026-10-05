package tracing

import (
	"strings"
	"testing"
	"time"
)

// benchTrace is the trace used by the store benchmarks. Built once so the
// measured path is Add/Get/Summary, not fixture construction.
var benchTrace = func() *Trace {
	tr := &Trace{
		TraceID:    testTraceID,
		RequestID:  testRequestID,
		Attributes: map[string]any{AttrRoute: "/v1/chat/completions", AttrModel: "gpt-oss-120b"},
		Status:     StatusOK,
	}
	tr.Add(Span{
		TraceID: testTraceID, SpanID: "00f067aa0ba902b7", Name: "POST /v1/chat/completions",
		Kind: KindServer, StartUnixNano: baseTime.UnixNano(), EndUnixNano: baseTime.Add(150 * time.Millisecond).UnixNano(),
		Status:     StatusOK,
		Attributes: map[string]any{AttrRoute: "/v1/chat/completions", AttrUpstream: "mock-a", AttrModel: "gpt-oss-120b", "status_code": 200},
		Events:     []Event{{Name: "router.select", TimeUnixNano: baseTime.UnixNano(), Attributes: map[string]any{"upstream": "mock-a"}}},
	})
	tr.Add(Span{
		TraceID: testTraceID, SpanID: "b7ad6b7169203331", ParentSpanID: "00f067aa0ba902b7",
		Name: "upstream.request", Kind: KindClient,
		StartUnixNano: baseTime.Add(2 * time.Millisecond).UnixNano(), EndUnixNano: baseTime.Add(120 * time.Millisecond).UnixNano(),
		Status: StatusOK, Attributes: map[string]any{"attempt": 1},
	})
	return tr
}()

// BenchmarkParse measures the inbound propagation path: one header on every
// request, so this is on the hot path.
func BenchmarkParse(b *testing.B) {
	header := "00-" + testTraceID + "-00f067aa0ba902b7-01"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := Parse(header, "vendor=opaque"); !ok {
			b.Fatal("valid header rejected")
		}
	}
}

// BenchmarkContextHeader measures the egress path: rendering the header for
// every upstream call.
func BenchmarkContextHeader(b *testing.B) {
	c := Context{TraceID: testTraceID, SpanID: "00f067aa0ba902b7", Sampled: true}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if c.Header() == "" {
			b.Fatal("empty header")
		}
	}
}

// BenchmarkStoreAdd measures the request-path insert: clone, index, summarize.
func BenchmarkStoreAdd(b *testing.B) {
	s := NewStore(1024)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Add(benchTrace)
	}
}

// BenchmarkStoreAddParallel measures the same insert under contention, which is
// what an actual gateway does across concurrent requests.
func BenchmarkStoreAddParallel(b *testing.B) {
	s := NewStore(1024)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Add(benchTrace)
		}
	})
}

// BenchmarkStoreGet measures the operator replay path: index lookup plus clone.
func BenchmarkStoreGet(b *testing.B) {
	s := NewStore(1024)
	s.Add(benchTrace)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := s.Get(testTraceID); !ok {
			b.Fatal("stored trace not found")
		}
	}
}

// BenchmarkTraceSummary measures the list-row projection, which List does not
// do per call (it caches summaries at Add time) but which the JSON/status paths
// and the tests call directly.
func BenchmarkTraceSummary(b *testing.B) {
	tr := benchTrace
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if sum := tr.Summary(); sum.TraceID == "" {
			b.Fatal("empty summary")
		}
	}
}

// BenchmarkTraceClone is not required by the task; it is here because Get and
// Add both pay for a clone and it is the number that explains BenchmarkStoreGet.
func BenchmarkTraceClone(b *testing.B) {
	tr := benchTrace
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if tr.Clone() == nil {
			b.Fatal("nil clone")
		}
	}
}

// BenchmarkWriteJSONL measures the export path, including the JSON encoding
// that must happen outside any lock. /dev/null equivalent: io.Discard.
func BenchmarkWriteJSONL(b *testing.B) {
	s := NewStore(4)
	var sb strings.Builder
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sb.Reset()
		if err := s.WriteJSONL(&sb, benchTrace); err != nil {
			b.Fatal(err)
		}
	}
}
