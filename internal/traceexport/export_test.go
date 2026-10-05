package traceexport_test

// Tests for the two exporters in internal/traceexport.
//
// Everything here is hermetic: the only network is an httptest server on the
// loopback interface, the only clock use is a coarse "this returned quickly
// enough" assertion, and the total sleep budget is well under 2s (the waits are
// channel-driven; the two deadlines are 2s ceilings that are never reached on a
// healthy run).
//
// The OTLP payload is asserted byte-for-byte against a hand-written golden
// string. That is the point of the sorted-attributes rule: without it a map
// iteration order would make this test flaky, and a flaky exporter test gets
// deleted rather than fixed.

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/traceexport"
	"github.com/infergate/infergate/internal/tracing"
)

// testTrace builds a finished two-span trace with one event and one attribute
// of every type the encoder distinguishes.
//
// The times are constants, not time.Now(): a golden payload cannot be compared
// against a payload that moves. 1700000000000000000 is
// 2023-11-14T22:13:20Z, comfortably inside float64's exact range so the decimal
// string assertion is testing the encoder and not the test's arithmetic.
func testTrace() *tracing.Trace {
	tr := &tracing.Trace{
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
		RequestID:  "req-1",
		StartedAt:  time.Unix(0, 1700000000000000000).UTC(),
		DurationMS: 12.5,
		Status:     tracing.StatusOK,
	}
	tr.Add(tracing.Span{
		TraceID:       tr.TraceID,
		SpanID:        "00f067aa0ba902b7",
		Name:          "gateway.request",
		Kind:          tracing.KindServer,
		StartUnixNano: 1700000000000000000,
		EndUnixNano:   1700000000012500000,
		Status:        tracing.StatusOK,
	})
	tr.Add(tracing.Span{
		TraceID:       tr.TraceID,
		SpanID:        "a1b2c3d4e5f60718",
		ParentSpanID:  "00f067aa0ba902b7",
		Name:          "upstream.attempt",
		Kind:          tracing.KindClient,
		StartUnixNano: 1700000000001000000,
		EndUnixNano:   1700000000012000000,
		Status:        tracing.StatusError,
		Attributes: map[string]any{
			"attempts": int64(1),
			"route":    "/v1/chat/completions",
		},
		Events: []tracing.Event{{
			Name:         "failover",
			TimeUnixNano: 1700000000001500000,
			Attributes:   map[string]any{"upstream": "b"},
		}},
	})
	return tr
}

// --- JSONL ------------------------------------------------------------------

func TestJSONLExport(t *testing.T) {
	t.Run("writes_one_valid_json_line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "traces.jsonl")
		j, err := traceexport.NewJSONL(traceexport.JSONLOptions{Path: path})
		if err != nil {
			t.Fatalf("NewJSONL: %v", err)
		}
		tr := testTrace()
		j.Export(tr)
		if err := j.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := j.Written(); got != 1 {
			t.Errorf("Written() = %d, want 1", got)
		}
		if got := j.Dropped(); got != 0 {
			t.Errorf("Dropped() = %d, want 0", got)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		text := string(data)
		if !strings.HasSuffix(text, "\n") {
			t.Fatalf("file does not end with a newline: %q", text)
		}
		if n := strings.Count(text, "\n"); n != 1 {
			t.Fatalf("file has %d lines, want 1: %q", n, text)
		}

		var decoded tracing.Trace
		if err := json.Unmarshal([]byte(strings.TrimSuffix(text, "\n")), &decoded); err != nil {
			t.Fatalf("line is not valid JSON: %v (%q)", err, text)
		}
		if decoded.TraceID != tr.TraceID || decoded.RequestID != tr.RequestID {
			t.Errorf("ids = %q/%q, want %q/%q", decoded.TraceID, decoded.RequestID, tr.TraceID, tr.RequestID)
		}
		if len(decoded.Spans) != 2 {
			t.Errorf("span count = %d, want 2", len(decoded.Spans))
		}

		// The line must be exactly what the tracing package renders: a
		// different rendering here would make the file format drift from the
		// in-process dump an operator already knows.
		var want strings.Builder
		if err := tracing.NewStore(1).WriteJSONL(&want, tr); err != nil {
			t.Fatalf("WriteJSONL: %v", err)
		}
		if text != want.String() {
			t.Errorf("line differs from tracing.WriteJSONL rendering:\n got %q\nwant %q", text, want.String())
		}
	})

	t.Run("two_exports_two_lines", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "traces.jsonl")
		j, err := traceexport.NewJSONL(traceexport.JSONLOptions{Path: path})
		if err != nil {
			t.Fatalf("NewJSONL: %v", err)
		}
		a, b := testTrace(), testTrace()
		b.RequestID = "req-2"
		b.TraceID = "11111111111111111111111111111111"
		j.Export(a)
		j.Export(b)
		if err := j.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := j.Written(); got != 2 {
			t.Errorf("Written() = %d, want 2", got)
		}
		lines := readLines(t, path)
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
		}
		for i, line := range lines {
			var tr tracing.Trace
			if err := json.Unmarshal([]byte(line), &tr); err != nil {
				t.Fatalf("line %d is not valid JSON: %v", i, err)
			}
		}
		if !strings.Contains(lines[0], "req-1") || !strings.Contains(lines[1], "req-2") {
			t.Errorf("lines out of order or wrong content: %q", lines)
		}
	})

	t.Run("reopen_appends_and_preserves", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "traces.jsonl")

		first, err := traceexport.NewJSONL(traceexport.JSONLOptions{Path: path})
		if err != nil {
			t.Fatalf("NewJSONL #1: %v", err)
		}
		first.Export(testTrace())
		if err := first.Close(); err != nil {
			t.Fatalf("Close #1: %v", err)
		}
		before := readLines(t, path)
		if len(before) != 1 {
			t.Fatalf("first run wrote %d lines, want 1", len(before))
		}

		second, err := traceexport.NewJSONL(traceexport.JSONLOptions{Path: path})
		if err != nil {
			t.Fatalf("NewJSONL #2: %v", err)
		}
		other := testTrace()
		other.RequestID = "req-restarted"
		second.Export(other)
		if err := second.Close(); err != nil {
			t.Fatalf("Close #2: %v", err)
		}

		after := readLines(t, path)
		if len(after) != 2 {
			t.Fatalf("after reopen got %d lines, want 2: %q", len(after), after)
		}
		if after[0] != before[0] {
			t.Errorf("reopen destroyed the earlier line:\n got %q\nwant %q", after[0], before[0])
		}
		if !strings.Contains(after[1], "req-restarted") {
			t.Errorf("appended line = %q, want the new trace", after[1])
		}
	})

	t.Run("close_flushes_without_draining_delay", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "traces.jsonl")
		j, err := traceexport.NewJSONL(traceexport.JSONLOptions{Path: path, Buffer: 4})
		if err != nil {
			t.Fatalf("NewJSONL: %v", err)
		}
		// Exported and immediately closed: the worker may not have read the
		// channel at all. Close must drain it rather than race it.
		j.Export(testTrace())
		if err := j.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := j.Written(); got != 1 {
			t.Errorf("Written() = %d, want 1", got)
		}
		if lines := readLines(t, path); len(lines) != 1 {
			t.Errorf("got %d lines, want 1: %q", len(lines), lines)
		}
	})

	t.Run("second_close_is_a_noop", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "traces.jsonl")
		j, err := traceexport.NewJSONL(traceexport.JSONLOptions{Path: path})
		if err != nil {
			t.Fatalf("NewJSONL: %v", err)
		}
		j.Export(testTrace())
		if err := j.Close(); err != nil {
			t.Fatalf("Close #1: %v", err)
		}
		if err := j.Close(); err != nil {
			t.Fatalf("Close #2: %v", err)
		}
		if err := j.Close(); err != nil {
			t.Fatalf("Close #3: %v", err)
		}
		if got := j.Written(); got != 1 {
			t.Errorf("Written() = %d, want 1 after repeated closes", got)
		}
		if lines := readLines(t, path); len(lines) != 1 {
			t.Errorf("got %d lines, want 1 after repeated closes", len(lines))
		}
	})

	t.Run("export_after_close_drops", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "traces.jsonl")
		j, err := traceexport.NewJSONL(traceexport.JSONLOptions{Path: path})
		if err != nil {
			t.Fatalf("NewJSONL: %v", err)
		}
		if err := j.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		j.Export(testTrace())
		if got := j.Dropped(); got != 1 {
			t.Errorf("Dropped() = %d, want 1", got)
		}
		if got := j.Written(); got != 0 {
			t.Errorf("Written() = %d, want 0", got)
		}
		if lines := readLines(t, path); len(lines) != 0 {
			t.Errorf("closed exporter wrote %d lines, want 0", len(lines))
		}
	})

	t.Run("unwritable_path_fails_construction", func(t *testing.T) {
		dir := t.TempDir()
		j, err := traceexport.NewJSONL(traceexport.JSONLOptions{Path: dir})
		if err == nil {
			_ = j.Close()
			t.Fatalf("NewJSONL(%q) = nil error, want failure for a directory path", dir)
		}
		if j != nil {
			t.Errorf("NewJSONL returned a non-nil exporter with an error: %v", j)
		}

		if j, err := traceexport.NewJSONL(traceexport.JSONLOptions{Path: filepath.Join(dir, "missing", "traces.jsonl")}); err == nil {
			_ = j.Close()
			t.Errorf("NewJSONL with a missing parent directory = nil error, want failure")
		}
		if j, err := traceexport.NewJSONL(traceexport.JSONLOptions{}); err == nil {
			_ = j.Close()
			t.Errorf("NewJSONL with an empty path = nil error, want failure")
		}
	})
}

// TestJSONLNonBlocking proves the promise that matters: with the writer busy,
// Export returns immediately and loses traces rather than slowing the caller.
func TestJSONLNonBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")
	j, err := traceexport.NewJSONL(traceexport.JSONLOptions{Path: path, Buffer: 4})
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	const n = 40
	start := time.Now()
	for i := 0; i < n; i++ {
		j.Export(testTrace())
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("Export of %d traces with a full queue took %v, want well under the deadline", n, elapsed)
	}
	if got := j.Dropped(); got == 0 {
		t.Errorf("Dropped() = 0 after %d exports into a %d-deep queue, want > 0", n, 4)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// --- OTLP -------------------------------------------------------------------

// collector is a test OTLP/HTTP receiver. It records every request's headers
// and decoded envelopes and counts the spans it was told about.
type collector struct {
	*httptest.Server

	mu      sync.Mutex
	headers []http.Header
	paths   []string
	bodies  [][]byte

	spans atomic.Int64

	status int
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{status: http.StatusOK}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		c.mu.Lock()
		c.headers = append(c.headers, r.Header.Clone())
		c.paths = append(c.paths, r.URL.Path)
		c.bodies = append(c.bodies, body)
		status := c.status
		c.mu.Unlock()

		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"collector said no"}`))
			return
		}
		// Count the spans so a concurrency test can compare against Exported.
		var env struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []json.RawMessage `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(body, &env); err == nil {
			for _, rs := range env.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					c.spans.Add(int64(len(ss.Spans)))
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.Close)
	return c
}

// firstBody returns the first request body the collector saw.
func (c *collector) firstBody(t *testing.T) []byte {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatalf("collector received no requests")
	}
	return c.bodies[0]
}

func (c *collector) requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *collector) setStatus(status int) {
	c.mu.Lock()
	c.status = status
	c.mu.Unlock()
}

// spanCount returns how many spans the collector has decoded so far.
func (c *collector) spanCount() int64 { return c.spans.Load() }

// waitForSpans polls until the collector has decoded at least n spans or the
// deadline passes. The read lag is real: the HTTP response can be written
// before the client's Close returns, so a bare assertion would be flaky.
func (c *collector) waitForSpans(n int64, timeout time.Duration) int64 {
	deadline := time.Now().Add(timeout)
	for {
		got := c.spanCount()
		if got >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// TestOTLPPayload asserts the exact wire shape. It is the single most valuable
// test in this file: every rule in the encoding section of the task shows up
// here as a golden byte or an absence.
func TestOTLPPayload(t *testing.T) {
	c := newCollector(t)
	o, err := traceexport.NewOTLP(traceexport.OTLPOptions{
		Endpoint: c.URL,
		Timeout:  2 * time.Second,
		Headers:  map[string]string{"Authorization": "Bearer test-token"},
	})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	o.Export(testTrace())
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stats := o.Stats()
	if stats.Exported != 1 || stats.Failed != 0 {
		t.Fatalf("Stats() = %+v, want Exported=1 Failed=0", stats)
	}
	if stats.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0", stats.Dropped)
	}
	// LastStatus/LastError are the LAST FAILURE, not a log of every response:
	// a successful run leaves them empty, which is what lets an operator read
	// Stats() and see trouble rather than traffic.
	if stats.LastStatus != 0 {
		t.Errorf("LastStatus = %d after only successful deliveries, want 0", stats.LastStatus)
	}
	if stats.LastError != "" {
		t.Errorf("LastError = %q after only successful deliveries, want empty", stats.LastError)
	}
	if got := c.requests(); got != 1 {
		t.Fatalf("collector saw %d requests, want 1", got)
	}

	c.mu.Lock()
	gotPath := c.paths[0]
	gotHeaders := c.headers[0]
	c.mu.Unlock()
	if gotPath != "/v1/traces" {
		t.Errorf("POST path = %q, want /v1/traces", gotPath)
	}
	if ct := gotHeaders.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if auth := gotHeaders.Get("Authorization"); auth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want the configured extra header", auth)
	}

	got := string(c.firstBody(t))
	const want = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"infergate"}}]},` +
		`"scopeSpans":[{"scope":{"name":"infergate.gateway"},` +
		`"spans":[` +
		`{"traceId":"4bf92f3577b34da6a3ce929d0e0e4736","spanId":"00f067aa0ba902b7",` +
		`"name":"gateway.request","kind":2,` +
		`"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000012500000",` +
		`"status":{"code":1}},` +
		`{"traceId":"4bf92f3577b34da6a3ce929d0e0e4736","spanId":"a1b2c3d4e5f60718",` +
		`"parentSpanId":"00f067aa0ba902b7","name":"upstream.attempt","kind":3,` +
		`"startTimeUnixNano":"1700000000001000000","endTimeUnixNano":"1700000000012000000",` +
		`"attributes":[{"key":"attempts","value":{"intValue":"1"}},{"key":"route","value":{"stringValue":"/v1/chat/completions"}}],` +
		`"events":[{"name":"failover","timeUnixNano":"1700000000001500000",` +
		`"attributes":[{"key":"upstream","value":{"stringValue":"b"}}]}],` +
		`"status":{"code":2}}` +
		`]}]}]}`
	if got != want {
		t.Errorf("payload mismatch\n got: %s\nwant: %s", got, want)
	}

	// The root span must not carry a parentSpanId key at all -- an empty
	// string is not the same as an absent field to an OTLP receiver.
	if strings.Contains(got, `"parentSpanId":""`) {
		t.Errorf("root span emitted an empty parentSpanId: %s", got)
	}
	if n := strings.Count(got, `"parentSpanId"`); n != 1 {
		t.Errorf("payload has %d parentSpanId keys, want exactly 1 (the child span)", n)
	}

	// decimal-string nanos: they must be inside JSON strings, never bare
	// numbers, because a JS/JSON consumer would lose precision on 1.7e18.
	for _, bare := range []string{`:1700000000000000000`, `:1700000000012500000`, `:1700000000001500000`} {
		if strings.Contains(got, bare) {
			t.Errorf("nanos were encoded as a bare number %q: %s", bare, got)
		}
	}
	for _, quoted := range []string{`"1700000000000000000"`, `"1700000000012500000"`, `"1700000000001500000"`} {
		if !strings.Contains(got, quoted) {
			t.Errorf("missing decimal-string nanos %s in %s", quoted, got)
		}
	}
}

// TestOTLPKindMapping is table-driven over every kind the exporter can see.
func TestOTLPKindMapping(t *testing.T) {
	tests := []struct {
		kind string
		want int
	}{
		{tracing.KindServer, 2},
		{tracing.KindClient, 3},
		{tracing.KindInternal, 1},
		{"", 0},
		{"producer", 0},
	}
	for _, tt := range tests {
		t.Run("kind="+tt.kind, func(t *testing.T) {
			tr := &tracing.Trace{TraceID: "abc", Spans: []tracing.Span{{
				TraceID:       "abc",
				SpanID:        "def",
				Name:          "n",
				Kind:          tt.kind,
				StartUnixNano: 1,
				EndUnixNano:   2,
			}}}
			c := newCollector(t)
			o, err := traceexport.NewOTLP(traceexport.OTLPOptions{Endpoint: c.URL, Timeout: 2 * time.Second})
			if err != nil {
				t.Fatalf("NewOTLP: %v", err)
			}
			o.Export(tr)
			if err := o.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			var env struct {
				ResourceSpans []struct {
					ScopeSpans []struct {
						Spans []struct {
							Kind int `json:"kind"`
						} `json:"spans"`
					} `json:"scopeSpans"`
				} `json:"resourceSpans"`
			}
			if err := json.Unmarshal(c.firstBody(t), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := env.ResourceSpans[0].ScopeSpans[0].Spans[0].Kind; got != tt.want {
				t.Errorf("kind %q encoded as %d, want %d", tt.kind, got, tt.want)
			}
		})
	}
}

// TestOTLPSkipsUnendedSpan covers the one span-level rule: a span with no end
// time cannot be represented and is dropped rather than exported as ending at
// the epoch.
func TestOTLPSkipsUnendedSpan(t *testing.T) {
	tr := &tracing.Trace{TraceID: "abc", Spans: []tracing.Span{
		{TraceID: "abc", SpanID: "1", Name: "ended", StartUnixNano: 10, EndUnixNano: 20},
		{TraceID: "abc", SpanID: "2", Name: "abandoned", StartUnixNano: 30},
	}}
	c := newCollector(t)
	o, err := traceexport.NewOTLP(traceexport.OTLPOptions{Endpoint: c.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	o.Export(tr)
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := string(c.firstBody(t))
	if strings.Contains(got, "abandoned") {
		t.Errorf("an unended span was exported: %s", got)
	}
	if !strings.Contains(got, `"spanId":"1"`) {
		t.Errorf("the ended span is missing: %s", got)
	}
	if n := strings.Count(got, `"spanId"`); n != 1 {
		t.Errorf("payload has %d spans, want 1: %s", n, got)
	}
}

// TestOTLPAttributeTypes is table-driven over the dynamic-type rule that
// decides which AnyValue arm is used.
func TestOTLPAttributeTypes(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"string", "hello", `{"stringValue":"hello"}`},
		{"empty string", "", `{"stringValue":""}`},
		{"bool true", true, `{"boolValue":true}`},
		{"bool false", false, `{"boolValue":false}`},
		{"int", 7, `{"intValue":"7"}`},
		{"int negative", -7, `{"intValue":"-7"}`},
		{"int8", int8(1), `{"intValue":"1"}`},
		{"int16", int16(2), `{"intValue":"2"}`},
		{"int32", int32(3), `{"intValue":"3"}`},
		{"int64 large", int64(9007199254740993), `{"intValue":"9007199254740993"}`},
		{"uint", uint(4), `{"intValue":"4"}`},
		{"uint8", uint8(5), `{"intValue":"5"}`},
		{"uint16", uint16(6), `{"intValue":"6"}`},
		{"uint32", uint32(8), `{"intValue":"8"}`},
		{"uint64 large", uint64(18446744073709551615), `{"intValue":"18446744073709551615"}`},
		{"uintptr", uintptr(9), `{"intValue":"9"}`},
		{"float64", 1.5, `{"doubleValue":1.5}`},
		{"float64 whole", float64(2), `{"doubleValue":2}`},
		{"float32", float32(0.5), `{"doubleValue":0.5}`},
		{"[]string", []string{"a", "b"}, `{"arrayValue":{"values":[{"stringValue":"a"},{"stringValue":"b"}]}}`},
		{"[]string empty", []string{}, `{"arrayValue":{"values":[]}}`},
		{"other type", time.Duration(0), `{"stringValue":"0s"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &tracing.Trace{TraceID: "abc", Spans: []tracing.Span{{
				TraceID:       "abc",
				SpanID:        "1",
				Name:          "n",
				StartUnixNano: 1,
				EndUnixNano:   2,
				Attributes:    map[string]any{"k": tt.value},
			}}}
			c := newCollector(t)
			o, err := traceexport.NewOTLP(traceexport.OTLPOptions{Endpoint: c.URL, Timeout: 2 * time.Second})
			if err != nil {
				t.Fatalf("NewOTLP: %v", err)
			}
			o.Export(tr)
			if err := o.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			got := string(c.firstBody(t))
			wantFragment := `"attributes":[{"key":"k","value":` + tt.want + `}]`
			if !strings.Contains(got, wantFragment) {
				t.Errorf("attribute encoding\n got: %s\nwant fragment: %s", got, wantFragment)
			}
		})
	}
}

// TestOTLPAttributeRules covers nil, sorting and the empty-attributes case.
func TestOTLPAttributeRules(t *testing.T) {
	t.Run("nil skipped and keys sorted", func(t *testing.T) {
		tr := &tracing.Trace{TraceID: "abc", Spans: []tracing.Span{{
			TraceID:       "abc",
			SpanID:        "1",
			Name:          "n",
			StartUnixNano: 1,
			EndUnixNano:   2,
			Attributes: map[string]any{
				"zebra":  "z",
				"alpha":  "a",
				"middle": nil,
				"beta":   2,
			},
		}}}
		c := newCollector(t)
		o, err := traceexport.NewOTLP(traceexport.OTLPOptions{Endpoint: c.URL, Timeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("NewOTLP: %v", err)
		}
		o.Export(tr)
		if err := o.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		body := c.firstBody(t)
		got := string(body)
		if strings.Contains(got, "middle") || strings.Contains(got, "<nil>") {
			t.Errorf("nil attribute was emitted: %s", got)
		}
		want := `"attributes":[{"key":"alpha","value":{"stringValue":"a"}},{"key":"beta","value":{"intValue":"2"}},{"key":"zebra","value":{"stringValue":"z"}}]`
		if !strings.Contains(got, want) {
			t.Errorf("attributes are not in sorted order\n got: %s\nwant fragment: %s", got, want)
		}
	})

	t.Run("no attributes omits the field", func(t *testing.T) {
		tr := &tracing.Trace{TraceID: "abc", Spans: []tracing.Span{{
			TraceID: "abc", SpanID: "1", Name: "n", StartUnixNano: 1, EndUnixNano: 2,
			Attributes: map[string]any{"only": nil},
		}}}
		c := newCollector(t)
		o, err := traceexport.NewOTLP(traceexport.OTLPOptions{Endpoint: c.URL, Timeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("NewOTLP: %v", err)
		}
		o.Export(tr)
		if err := o.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Assert on the span object itself: the resource attributes legitimately
		// contain an "attributes" key, so a whole-body substring check would be
		// testing the resource, not the span.
		c.mu.Lock()
		body := string(c.bodies[0])
		c.mu.Unlock()
		const spanObj = `{"traceId":"abc","spanId":"1","name":"n","kind":0,"startTimeUnixNano":"1","endTimeUnixNano":"2"}`
		if !strings.Contains(body, spanObj) {
			t.Errorf("span object is not the expected exact shape\n got: %s\nwant fragment: %s", body, spanObj)
		}
		if strings.Contains(body, `"only"`) {
			t.Errorf("a nil-only attribute map leaked its key: %s", body)
		}
	})

	t.Run("status unset omits the status object", func(t *testing.T) {
		tr := &tracing.Trace{TraceID: "abc", Spans: []tracing.Span{{
			TraceID: "abc", SpanID: "1", Name: "n", StartUnixNano: 1, EndUnixNano: 2,
			Status: tracing.StatusUnset,
		}}}
		c := newCollector(t)
		o, err := traceexport.NewOTLP(traceexport.OTLPOptions{Endpoint: c.URL, Timeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("NewOTLP: %v", err)
		}
		o.Export(tr)
		if err := o.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		got := string(c.firstBody(t))
		if strings.Contains(got, `"status"`) {
			t.Errorf("status unset emitted a status object: %s", got)
		}
	})

	t.Run("service name default and override", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			opt  string
			want string
		}{
			{"default", "", `{"key":"service.name","value":{"stringValue":"infergate"}}`},
			{"override", "checkout-gw", `{"key":"service.name","value":{"stringValue":"checkout-gw"}}`},
		} {
			t.Run(tt.name, func(t *testing.T) {
				c := newCollector(t)
				o, err := traceexport.NewOTLP(traceexport.OTLPOptions{
					Endpoint:    c.URL,
					Timeout:     2 * time.Second,
					ServiceName: tt.opt,
				})
				if err != nil {
					t.Fatalf("NewOTLP: %v", err)
				}
				o.Export(testTrace())
				if err := o.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				got := string(c.firstBody(t))
				if !strings.Contains(got, tt.want) {
					t.Errorf("resource attribute\n got: %s\nwant fragment: %s", got, tt.want)
				}
				if !strings.Contains(got, `"scope":{"name":"infergate.gateway"}`) {
					t.Errorf("scope name missing: %s", got)
				}
			})
		}
	})
}

// TestOTLPFailure covers the non-2xx path: counters, status and the quoted body.
func TestOTLPFailure(t *testing.T) {
	c := newCollector(t)
	c.setStatus(http.StatusInternalServerError)

	o, err := traceexport.NewOTLP(traceexport.OTLPOptions{Endpoint: c.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	o.Export(testTrace())
	// Close reports the last delivery failure, so an operator who only watches
	// the shutdown path still learns that traces were not delivered.
	closeErr := o.Close()
	if closeErr == nil {
		t.Errorf("Close after a failed delivery = nil, want the delivery error")
	}

	stats := o.Stats()
	if stats.Failed != 1 {
		t.Errorf("Failed = %d, want 1", stats.Failed)
	}
	if stats.Exported != 0 {
		t.Errorf("Exported = %d, want 0", stats.Exported)
	}
	if stats.LastStatus != http.StatusInternalServerError {
		t.Errorf("LastStatus = %d, want 500", stats.LastStatus)
	}
	if stats.LastError == "" {
		t.Errorf("LastError is empty, want the status and body")
	}
	if !strings.Contains(stats.LastError, "500") {
		t.Errorf("LastError = %q, want it to mention the status", stats.LastError)
	}
	if !strings.Contains(stats.LastError, "collector said no") {
		t.Errorf("LastError = %q, want it to quote the response body", stats.LastError)
	}

	// Close is idempotent even after a failure: the second call reports the
	// same error rather than losing it or panicking.
	if err := o.Close(); err == nil {
		t.Errorf("second Close = nil, want the same delivery error")
	}
}

// TestOTLPTransportError covers a request that never completes at the HTTP
// level (here: a refused connection). LastStatus must stay 0.
func TestOTLPTransportError(t *testing.T) {
	// Bind then immediately close a server so the port is (almost certainly)
	// nothing: a connection refused, not a slow response.
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := s.URL
	s.Close()

	o, err := traceexport.NewOTLP(traceexport.OTLPOptions{Endpoint: endpoint, Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	o.Export(testTrace())
	if err := o.Close(); err == nil {
		t.Errorf("Close after a transport error = nil, want the transport error")
	}
	stats := o.Stats()
	if stats.Failed != 1 || stats.Exported != 0 {
		t.Errorf("Stats() = %+v, want Failed=1 Exported=0", stats)
	}
	if stats.LastStatus != 0 {
		t.Errorf("LastStatus = %d, want 0 for a transport error", stats.LastStatus)
	}
	if stats.LastError == "" {
		t.Errorf("LastError is empty, want the transport error text")
	}
}

// TestOTLPNewValidation checks endpoint validation.
func TestOTLPNewValidation(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"relative", "collector:4318", true},
		{"no scheme", "127.0.0.1:4318", true},
		{"wrong scheme", "ftp://127.0.0.1:4318", true},
		{"unparsable", "http://[::1", true},
		{"http ok", "http://127.0.0.1:19999", false},
		{"https ok", "https://collector.example.com", false},
		{"trailing slash ok", "http://127.0.0.1:19999/", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := traceexport.NewOTLP(traceexport.OTLPOptions{Endpoint: tt.endpoint})
			if tt.wantErr {
				if err == nil {
					_ = o.Close()
					t.Fatalf("NewOTLP(%q) = nil error, want failure", tt.endpoint)
				}
				if o != nil {
					t.Errorf("NewOTLP(%q) returned a non-nil exporter with an error", tt.endpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewOTLP(%q): %v", tt.endpoint, err)
			}
			if err := o.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
}

// blockingTransport is an http.RoundTripper that parks every request until the
// test releases it. It stands in for "the collector stopped answering", which
// is the condition Export must never propagate to a caller.
type blockingTransport struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
	count   atomic.Int64

	mu       sync.Mutex
	released bool
}

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
}

// releaseAll unblocks every request, now and in the future. Idempotent, so a
// test's failsafe defer can call it after the test already did.
func (b *blockingTransport) releaseAll() {
	b.mu.Lock()
	already := b.released
	b.released = true
	b.mu.Unlock()
	if !already {
		close(b.release)
	}
}

// wasReleased reports whether releaseAll ran.
func (b *blockingTransport) wasReleased() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.released
}

func (b *blockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b.count.Add(1)
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// TestOTLPNonBlocking is the proof for the OTLP exporter: with the network
// deliberately stalled, Export returns immediately and traces are dropped
// rather than the caller being parked behind the collector.
func TestOTLPNonBlocking(t *testing.T) {
	tr := newBlockingTransport()
	client := &http.Client{Transport: tr}

	o, err := traceexport.NewOTLP(traceexport.OTLPOptions{
		Endpoint: "http://127.0.0.1:19999",
		Timeout:  2 * time.Second,
		Buffer:   4,
		Client:   client,
	})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer func() {
		// Failsafe only: the test releases the transport itself. If it failed
		// early, this keeps the worker from staying parked forever.
		if !tr.wasReleased() {
			tr.once.Do(func() { close(tr.started) })
			tr.releaseAll()
		}
	}()

	const n = 40
	start := time.Now()
	for i := 0; i < n; i++ {
		o.Export(testTrace())
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("Export of %d traces against a stalled collector took %v", n, elapsed)
	}
	// The first trace is picked up by the worker (and blocks inside the
	// transport); the rest must be dropped once the 4-deep queue is full.
	if got := o.Stats().Dropped; got == 0 {
		t.Errorf("Dropped = 0 after %d exports against a stalled collector with a 4-deep queue, want > 0", n)
	}
	if got := o.Stats().Exported; got != 0 {
		t.Errorf("Exported = %d before the collector answered, want 0", got)
	}

	// The worker must have reached the transport, and only once: a worker that
	// spawned a goroutine per trace would show up here as many concurrent
	// requests.
	select {
	case <-tr.started:
	case <-time.After(time.Second):
		t.Fatalf("worker never issued a request")
	}
	time.Sleep(20 * time.Millisecond)
	if got := tr.count.Load(); got != 1 {
		t.Errorf("transport saw %d concurrent requests, want 1 (one worker, serial posts)", got)
	}

	// Release the stall: the exporter must now finish its (bounded) drain and
	// stop, not hang. The bound is the POST timeout, so this is generous.
	tr.once.Do(func() { close(tr.started) })
	tr.releaseAll()

	closed := make(chan error, 1)
	go func() { closed <- o.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Close did not return after the collector was released")
	}
	stats := o.Stats()
	if stats.Exported == 0 {
		t.Errorf("Exported = 0 after release, want the queued traces delivered")
	}
	if stats.Exported+stats.Dropped != n {
		t.Errorf("Exported+Dropped = %d+%d, want %d", stats.Exported, stats.Dropped, n)
	}
}

// TestOTLPConcurrency runs the exporter the way the gateway will: many
// producers, one worker, a collector that is up. Every trace is either
// delivered or dropped, and the collector must agree with the counter.
func TestOTLPConcurrency(t *testing.T) {
	c := newCollector(t)
	o, err := traceexport.NewOTLP(traceexport.OTLPOptions{
		Endpoint: c.URL,
		Timeout:  2 * time.Second,
		Buffer:   64,
	})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}

	const goroutines, perGoroutine = 8, 50
	const total = goroutines * perGoroutine

	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perGoroutine; i++ {
				o.Export(testTrace())
			}
		}()
	}
	close(start) // release them together, to actually contend for the queue
	wg.Wait()

	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stats := o.Stats()
	if stats.Exported+stats.Dropped != total {
		t.Errorf("Exported+Dropped = %d+%d = %d, want %d",
			stats.Exported, stats.Dropped, stats.Exported+stats.Dropped, total)
	}
	if stats.Failed != 0 {
		t.Errorf("Failed = %d against a healthy collector, want 0", stats.Failed)
	}
	// Two spans per trace, so the collector's span count is the independent
	// witness: it must see exactly the traces the exporter claims to have sent.
	want := stats.Exported * 2
	if got := c.waitForSpans(want, 2*time.Second); got != want {
		t.Errorf("collector decoded %d spans, want %d (%d traces) after the exporter reported %d exported",
			got, want, stats.Exported, stats.Exported)
	}
}

// TestOTLPHeadersCopied proves the Headers option is copied at construction:
// mutating the caller's map afterwards must not change the requests.
func TestOTLPHeadersCopied(t *testing.T) {
	c := newCollector(t)
	headers := map[string]string{"X-Tenant": "acme"}
	o, err := traceexport.NewOTLP(traceexport.OTLPOptions{
		Endpoint: c.URL,
		Timeout:  2 * time.Second,
		Headers:  headers,
	})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	headers["X-Tenant"] = "mutated"
	headers["X-New"] = "surprise"
	o.Export(testTrace())
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c.mu.Lock()
	got := c.headers[0]
	c.mu.Unlock()
	if v := got.Get("X-Tenant"); v != "acme" {
		t.Errorf("X-Tenant = %q, want the value at construction time (acme)", v)
	}
	if v := got.Get("X-New"); v != "" {
		t.Errorf("X-New = %q, want it absent: headers must be copied, not aliased", v)
	}
}

// TestStatsZeroValue keeps a nil-safety promise: a never-started exporter and a
// nil receiver both answer instead of panicking.
func TestStatsZeroValue(t *testing.T) {
	var nilOTLP *traceexport.OTLP
	nilOTLP.Export(testTrace())
	if got := nilOTLP.Stats(); !reflect.DeepEqual(got, traceexport.OTLPStats{}) {
		t.Errorf("nil.StatS() = %+v, want the zero value", got)
	}
	if err := nilOTLP.Close(); err != nil {
		t.Errorf("nil.Close() = %v, want nil", err)
	}

	var nilJSONL *traceexport.JSONL
	nilJSONL.Export(testTrace())
	if nilJSONL.Written() != 0 || nilJSONL.Dropped() != 0 || nilJSONL.Err() != nil {
		t.Errorf("nil JSONL accessors returned non-zero values")
	}
	if err := nilJSONL.Close(); err != nil {
		t.Errorf("nil.Close() = %v, want nil", err)
	}
}

// --- helpers ----------------------------------------------------------------

// readLines returns the file's lines with the trailing newline removed.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}
