package tracing

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// TestSummaryFieldMapping pins every field of the list row.
//
// Durations: the root span runs 0-150ms, the child spans 2-120ms, so the trace
// is earliest-start to latest-end = 150ms, NOT the root's own 150ms plus the
// 2ms offset it started at relative to the trace. Root DurationMS is 150ms; the
// 1ms difference between the integer path and the cached float is exactly the
// kind of drift this test is here to catch.
func TestSummaryFieldMapping(t *testing.T) {
	tr := testTrace()
	sum := tr.Summary()

	wantString(t, "TraceID", sum.TraceID, testTraceID)
	wantString(t, "RequestID", sum.RequestID, testRequestID)
	wantString(t, "Status", sum.Status, StatusOK)
	wantString(t, "Route", sum.Route, "/v1/chat/completions")
	wantString(t, "Upstream", sum.Upstream, "mock-a")
	wantString(t, "Model", sum.Model, "gpt-oss-120b")
	wantString(t, "StartedAt", sum.StartedAt, baseTime.UTC().Format(time.RFC3339Nano))

	if sum.SpanCount != 2 {
		t.Errorf("SpanCount = %d, want 2", sum.SpanCount)
	}
	wantFloat(t, "DurationMS", sum.DurationMS, 150)

	// StartedAt must round-trip as a real RFC3339Nano timestamp, not just
	// equal the string we happen to format elsewhere.
	parsed, err := time.Parse(time.RFC3339Nano, sum.StartedAt)
	if err != nil {
		t.Fatalf("StartedAt %q is not RFC3339Nano: %v", sum.StartedAt, err)
	}
	if !parsed.Equal(baseTime) {
		t.Errorf("StartedAt parses to %v, want %v", parsed, baseTime)
	}
	if !parsed.UTC().Equal(parsed) {
		t.Errorf("StartedAt %q is not in UTC", sum.StartedAt)
	}
}

// TestSummaryDefaults documents the fallbacks: status from the root span (or
// "unset"), and route/upstream/model from the trace attributes when the root
// span does not carry them.
func TestSummaryDefaults(t *testing.T) {
	start := time.Unix(0, 1_700_000_000_123_456_789).UTC()

	t.Run("no spans", func(t *testing.T) {
		tr := &Trace{TraceID: "t", RequestID: "r", StartedAt: start}
		sum := tr.Summary()
		wantString(t, "Status", sum.Status, StatusUnset)
		wantString(t, "StartedAt", sum.StartedAt, start.Format(time.RFC3339Nano))
		if sum.SpanCount != 0 {
			t.Errorf("SpanCount = %d, want 0", sum.SpanCount)
		}
		wantFloat(t, "DurationMS", sum.DurationMS, 0)
	})

	t.Run("root span status wins", func(t *testing.T) {
		tr := &Trace{TraceID: "t", RequestID: "r", Status: StatusOK}
		tr.Add(Span{Name: "root", StartUnixNano: start.UnixNano(), EndUnixNano: start.Add(time.Millisecond).UnixNano(), Status: StatusError})
		sum := tr.Summary()
		wantString(t, "Status", sum.Status, StatusError)
	})

	t.Run("trace attributes fill the gaps", func(t *testing.T) {
		tr := &Trace{
			TraceID:    "t",
			RequestID:  "r",
			Status:     StatusOK,
			Attributes: map[string]any{AttrRoute: "/v1/embeddings", AttrUpstream: "mock-b", AttrModel: "bge-m3"},
		}
		tr.Add(Span{Name: "root", StartUnixNano: start.UnixNano(), EndUnixNano: start.Add(time.Millisecond).UnixNano(), Status: StatusOK})
		sum := tr.Summary()
		wantString(t, "Route", sum.Route, "/v1/embeddings")
		wantString(t, "Upstream", sum.Upstream, "mock-b")
		wantString(t, "Model", sum.Model, "bge-m3")
	})

	t.Run("root span attributes win over trace attributes", func(t *testing.T) {
		tr := &Trace{
			TraceID:    "t",
			RequestID:  "r",
			Attributes: map[string]any{AttrModel: "trace-level"},
		}
		tr.Add(Span{
			Name: "root", StartUnixNano: start.UnixNano(), EndUnixNano: start.Add(time.Millisecond).UnixNano(),
			Status:     StatusOK,
			Attributes: map[string]any{AttrModel: "span-level"},
		})
		wantString(t, "Model", tr.Summary().Model, "span-level")
	})

	t.Run("nil trace", func(t *testing.T) {
		var tr *Trace
		if got := tr.Summary(); got != (Summary{}) {
			t.Errorf("nil.Summary() = %+v, want zero Summary", got)
		}
	})
}

// TestTraceDuration checks the earliest-start/latest-end rule, including the
// out-of-order case that a root-only computation gets wrong.
func TestTraceDuration(t *testing.T) {
	cases := []struct {
		name  string
		spans []Span
		want  float64
	}{
		{"empty", nil, 0},
		{
			"single span",
			[]Span{{StartUnixNano: 1000, EndUnixNano: 1000 + 250_000_000}},
			250,
		},
		{
			"child starts before root and ends after it",
			[]Span{
				{StartUnixNano: 5_000_000, EndUnixNano: 105_000_000},
				{StartUnixNano: 1_000_000, EndUnixNano: 130_000_000},
			},
			129,
		},
		{
			"span that never ended is ignored, not counted as negative",
			[]Span{
				{StartUnixNano: 1_000_000, EndUnixNano: 1_000_000 + 10_000_000},
				{StartUnixNano: 50_000_000, EndUnixNano: 0},
			},
			10,
		},
		{
			"unstarted spans contribute nothing",
			[]Span{{StartUnixNano: 0, EndUnixNano: 0}},
			0,
		},
		{
			"sub-millisecond rounds to 6 decimals",
			[]Span{{StartUnixNano: 1_000_000, EndUnixNano: 1_000_001}},
			0.000001,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &Trace{Spans: tc.spans}
			wantFloat(t, "Duration()", tr.Duration(), tc.want)
		})
	}

	var nilTrace *Trace
	if got := nilTrace.Duration(); got != 0 {
		t.Errorf("nil.Duration() = %v, want 0", got)
	}
}

// TestAddEnrichesSpan proves Add fills the derived duration and normalizes
// StartedAt to the earliest span start, including when spans arrive newest-first.
func TestAddEnrichesSpan(t *testing.T) {
	later := time.Unix(0, 2_000_000_000).UTC()
	earlier := time.Unix(0, 1_000_000_000).UTC()

	tr := &Trace{TraceID: "t"}
	tr.Add(Span{Name: "later", StartUnixNano: later.UnixNano(), EndUnixNano: later.Add(3 * time.Millisecond).UnixNano()})
	wantString(t, "StartedAt after first span", tr.StartedAt.Format(time.RFC3339Nano), later.Format(time.RFC3339Nano))

	tr.Add(Span{Name: "earlier", StartUnixNano: earlier.UnixNano(), EndUnixNano: earlier.Add(time.Millisecond).UnixNano()})
	wantString(t, "StartedAt moves to the earliest span", tr.StartedAt.Format(time.RFC3339Nano), earlier.Format(time.RFC3339Nano))

	wantFloat(t, "spans[0].DurationMS", tr.Spans[0].DurationMS, 3)
	wantFloat(t, "spans[1].DurationMS", tr.Spans[1].DurationMS, 1)
	wantFloat(t, "trace DurationMS", tr.DurationMS, 1003)

	if tr.StartedAt.UnixNano() != earliestStart(tr.Spans) {
		t.Errorf("StartedAt %d does not equal the earliest span start %d", tr.StartedAt.UnixNano(), earliestStart(tr.Spans))
	}

	// A hand-set StartedAt is overridden by the spans: the nanos are the truth.
	manual := &Trace{TraceID: "t", StartedAt: time.Unix(0, 99_000_000_000).UTC()}
	manual.Add(Span{Name: "root", StartUnixNano: earlier.UnixNano(), EndUnixNano: earlier.Add(time.Millisecond).UnixNano()})
	wantString(t, "StartedAt derived from spans", manual.StartedAt.Format(time.RFC3339Nano), earlier.Format(time.RFC3339Nano))
}

// TestAddDoesNotAliasCallerSpan proves the trace keeps its own copy of the span
// and its maps: the request path writes annotations into the same maps after
// handing the span over.
func TestAddDoesNotAliasCallerSpan(t *testing.T) {
	attrs := map[string]any{"nested": map[string]any{"attempt": 1}, "list": []any{"a"}}
	span := Span{
		Name:          "upstream",
		StartUnixNano: 1_000_000,
		EndUnixNano:   2_000_000,
		Attributes:    attrs,
		Events:        []Event{{Name: "first_chunk", Attributes: map[string]any{"n": 1}}},
	}

	tr := &Trace{TraceID: "t"}
	tr.Add(span)

	// Mutate everything the caller still owns.
	attrs["late"] = "annotation"
	attrs["nested"].(map[string]any)["attempt"] = 99
	attrs["list"].([]any)[0] = "z"
	span.Events[0].Attributes["n"] = 42

	if got := tr.Spans[0].Attributes["late"]; got != nil {
		t.Errorf("span attribute written after Add leaked into the trace: %v", got)
	}
	if got := tr.Spans[0].Attributes["nested"].(map[string]any)["attempt"]; got != 1 {
		t.Errorf("nested map is aliased: attempt = %v, want 1", got)
	}
	if got := tr.Spans[0].Attributes["list"].([]any)[0]; got != "a" {
		t.Errorf("nested slice is aliased: list[0] = %v, want a", got)
	}
	if got := tr.Spans[0].Events[0].Attributes["n"]; got != 1 {
		t.Errorf("event attribute is aliased: n = %v, want 1", got)
	}
}

// TestCloneIsolation proves Clone is deep in BOTH directions: mutating the
// original must not touch the clone, and mutating the clone must not touch the
// original. One direction alone would pass with a shallow copy of the slice.
//
// Each direction gets a FRESH pair of traces. Sharing one pair makes the second
// check depend on the first check's mutations (the original legitimately grew
// when the original was appended to), which is how a real clone bug hides.
func TestCloneIsolation(t *testing.T) {
	checkStructure := func(t *testing.T, orig, cp *Trace) {
		t.Helper()
		if !reflect.DeepEqual(orig, cp) {
			t.Fatalf("Clone is not initially equal to the original:\norig=%+v\ncp  =%+v", orig, cp)
		}
		if len(cp.Spans) != 2 || len(cp.Spans[0].Events) != 1 {
			t.Fatalf("Clone lost structure: %d spans, %d events on the root", len(cp.Spans), len(cp.Spans[0].Events))
		}
	}

	t.Run("mutating the original leaves the clone alone", func(t *testing.T) {
		orig := testTrace()
		cp := orig.Clone()
		checkStructure(t, orig, cp)

		orig.Spans[0].Attributes["route"] = "/mutated"
		orig.Spans[0].Events[0].Name = "mutated"
		orig.Attributes["new"] = true
		orig.Add(Span{Name: "extra", StartUnixNano: 1, EndUnixNano: 2})

		wantString(t, "clone route", cp.Spans[0].Attributes["route"].(string), "/v1/chat/completions")
		wantString(t, "clone event name", cp.Spans[0].Events[0].Name, "router.select")
		if _, ok := cp.Attributes["new"]; ok {
			t.Error("clone trace attributes aliased the original")
		}
		if len(cp.Spans) != 2 {
			t.Errorf("clone grew to %d spans when the original was appended to", len(cp.Spans))
		}
		if cp.DurationMS != 150 {
			t.Errorf("clone DurationMS = %v, want 150", cp.DurationMS)
		}
	})

	t.Run("mutating the clone leaves the original alone", func(t *testing.T) {
		orig := testTrace()
		cp := orig.Clone()
		checkStructure(t, orig, cp)

		cp.Spans[1].Attributes["attempt"] = 99
		cp.Spans[1].Events[0].Name = "mutated"
		cp.Attributes["model"] = "other"
		cp.Add(Span{Name: "extra"})

		if got := orig.Spans[1].Attributes["attempt"]; got != 1 {
			t.Errorf("original span attribute changed to %v via the clone, want 1", got)
		}
		wantString(t, "original event name", orig.Spans[1].Events[0].Name, "first_chunk")
		wantString(t, "original model", orig.Attributes["model"].(string), "gpt-oss-120b")
		if len(orig.Spans) != 2 {
			t.Errorf("original grew to %d spans when the clone was appended to", len(orig.Spans))
		}
	})

	if cp2 := (*Trace)(nil).Clone(); cp2 != nil {
		t.Errorf("nil.Clone() = %+v, want nil", cp2)
	}
}

// TestJSONRoundTrip marshals a full trace with nested attributes and events and
// reads it back, comparing the whole structure.
func TestJSONRoundTrip(t *testing.T) {
	orig := testTrace()

	blob, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("Marshal produced no bytes")
	}

	var back Trace
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// A raw reflect.DeepEqual(orig, &back) can never pass, and its failure
	// would be a lie: JSON has exactly one number type, so every int-typed
	// attribute (attempt, status_code, ttft_ms) legitimately comes back as
	// float64. So compare the two structures AS JSON -- normalize both through
	// a marshal/unmarshal cycle and then deep-compare. That still catches real
	// loss (a dropped event, a reordered span, a stringified nested map) while
	// ignoring the int/float retyping that is inherent to the encoding.
	asJSON := func(v any) any {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var out any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		return out
	}
	if !reflect.DeepEqual(asJSON(orig), asJSON(&back)) {
		t.Errorf("round trip changed the trace:\norig=%+v\nback=%+v", orig, &back)
	}

	// Nothing may be lost on a SECOND trip either: encoding the decoded trace
	// must reproduce the original bytes exactly.
	again, err := json.Marshal(&back)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if string(again) != string(blob) {
		t.Errorf("round trip is not byte-stable:\n  orig=%s\n  back=%s", blob, again)
	}

	// Field names are the OTLP-like contract other tooling maps from, so pin
	// the ones that would silently break a collector.
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, key := range []string{"trace_id", "request_id", "started_at", "duration_ms", "status", "spans"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("marshalled trace has no %q field: %s", key, blob)
		}
	}
	spans := raw["spans"].([]any)
	first := spans[0].(map[string]any)
	for _, key := range []string{"trace_id", "span_id", "name", "kind", "start_unix_nano", "end_unix_nano", "duration_ms", "status", "attributes", "events"} {
		if _, ok := first[key]; !ok {
			t.Errorf("marshalled span has no %q field: %s", key, blob)
		}
	}
	if _, ok := first["parent_span_id"]; ok {
		t.Error("root span marshalled parent_span_id despite omitempty and an empty value")
	}
	// The nested attribute object must survive as an object, not a string.
	attrs := first["attributes"].(map[string]any)
	if _, ok := attrs["status_code"].(float64); !ok {
		t.Errorf("numeric attribute decoded as %T, want float64: %v", attrs["status_code"], attrs["status_code"])
	}
	events := first["events"].([]any)
	ev := events[0].(map[string]any)
	if _, ok := ev["time_unix_nano"].(float64); !ok {
		t.Errorf("event time decoded as %T, want float64", ev["time_unix_nano"])
	}

	// The decoded trace must still summarize correctly: times are nanos, and
	// the summary recomputes duration rather than trusting the cached float.
	sum := back.Summary()
	wantString(t, "decoded Status", sum.Status, StatusOK)
	wantString(t, "decoded Route", sum.Route, "/v1/chat/completions")
	wantFloat(t, "decoded DurationMS", sum.DurationMS, 150)
	wantString(t, "decoded StartedAt", sum.StartedAt, baseTime.UTC().Format(time.RFC3339Nano))
}

// TestJSONRoundTripEmptyAndOmitEmpty checks the zero case: a trace with no
// attributes and no events must not grow empty objects.
func TestJSONRoundTripEmptyAndOmitEmpty(t *testing.T) {
	tr := &Trace{TraceID: "t", RequestID: "r", Status: StatusUnset}
	tr.Add(Span{TraceID: "t", SpanID: "s", Name: "root", Kind: KindServer, StartUnixNano: 1, EndUnixNano: 2, Status: StatusOK})

	blob, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := raw["attributes"]; ok {
		t.Errorf("empty trace attributes were marshalled: %s", blob)
	}
	span := raw["spans"].([]any)[0].(map[string]any)
	if _, ok := span["attributes"]; ok {
		t.Errorf("empty span attributes were marshalled: %s", blob)
	}
	if _, ok := span["events"]; ok {
		t.Errorf("empty span events were marshalled: %s", blob)
	}
	if span["spans"] != nil {
		t.Errorf("unexpected nested spans field: %s", blob)
	}
}

// TestNanosToMS pins the documented rounding: nanoseconds to float milliseconds
// with 6 decimals.
func TestNanosToMS(t *testing.T) {
	cases := []struct {
		nanos int64
		want  float64
	}{
		{0, 0},
		{1, 0.000001},
		{999, 0.000999}, // truncated at 6 decimals would still be 0.000999
		{1_000, 0.001},
		{1_500_000, 1.5},
		{150_000_000, 150},
		{1_000_000_001, 1000.000001},
		{999_999_999_999, 999999.999999},
	}
	for _, tc := range cases {
		if got := nanosToMS(tc.nanos); !sameFloat(got, tc.want) {
			t.Errorf("nanosToMS(%d) = %.10f, want %.10f", tc.nanos, got, tc.want)
		}
	}
}

// TestTimeBaseConsistency is the invariant the package doc claims: integer
// nanoseconds are authoritative, and every derived float agrees with them.
func TestTimeBaseConsistency(t *testing.T) {
	tr := testTrace()
	rootStart := int64(tr.Spans[0].StartUnixNano)

	if tr.StartedAt.UnixNano() != rootStart {
		t.Errorf("Trace.StartedAt = %d, want the root span start %d", tr.StartedAt.UnixNano(), rootStart)
	}
	if got := tr.Summary().StartedAt; got != tr.StartedAt.UTC().Format(time.RFC3339Nano) {
		t.Errorf("Summary.StartedAt = %q, Trace.StartedAt formats to %q", got, tr.StartedAt.UTC().Format(time.RFC3339Nano))
	}

	// The cached DurationMS set by Add must equal the derived one, on a trace
	// that has been through a JSON round trip too.
	for _, candidate := range []*Trace{tr, testTrace()} {
		blob, err := json.Marshal(candidate)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var back Trace
		if err := json.Unmarshal(blob, &back); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if !sameFloat(back.DurationMS, back.Duration()) {
			t.Errorf("cached DurationMS %.9f != derived %.9f", back.DurationMS, back.Duration())
		}
		if !sameFloat(tr.DurationMS, tr.Duration()) {
			t.Errorf("Add cached DurationMS %.9f != derived %.9f", tr.DurationMS, tr.Duration())
		}
	}
}

// TestStrAttrAndEarliestStart cover the two small helpers on their edge inputs.
func TestStrAttrAndEarliestStart(t *testing.T) {
	if got := strAttr(nil, AttrRoute); got != "" {
		t.Errorf("strAttr(nil) = %q, want empty", got)
	}
	if got := strAttr(map[string]any{AttrRoute: 12}, AttrRoute); got != "" {
		t.Errorf("strAttr on a non-string = %q, want empty", got)
	}
	if got := strAttr(map[string]any{AttrRoute: "/x"}, AttrRoute); got != "/x" {
		t.Errorf("strAttr = %q, want /x", got)
	}
	if got := earliestStart(nil); got != 0 {
		t.Errorf("earliestStart(nil) = %d, want 0", got)
	}
	if got := earliestStart([]Span{{StartUnixNano: 0}, {StartUnixNano: 5}, {StartUnixNano: 3}}); got != 3 {
		t.Errorf("earliestStart = %d, want 3", got)
	}
}

// TestNewTraceShape pins the constructor, mainly that a trace starts "unset"
// rather than claiming success before any span has finished.
func TestNewTraceShape(t *testing.T) {
	tr := NewTrace("t", "r", baseTime)
	if tr.TraceID != "t" || tr.RequestID != "r" || !tr.StartedAt.Equal(baseTime) {
		t.Errorf("NewTrace = %+v, want ids and time as given", tr)
	}
	if tr.Status != StatusUnset {
		t.Errorf("Status = %q, want %q", tr.Status, StatusUnset)
	}
	if len(tr.Spans) != 0 {
		t.Errorf("Spans = %v, want empty", tr.Spans)
	}
	if got := fmt.Sprintf("%s", tr.StartedAt.UTC().Format(time.RFC3339Nano)); got != baseTime.Format(time.RFC3339Nano) {
		t.Errorf("StartedAt = %q, want %q", got, baseTime.Format(time.RFC3339Nano))
	}
}
