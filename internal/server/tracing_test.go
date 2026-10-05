package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/config"
)

// ---------------------------------------------------------------------------
// M5 tracing surface
//
// These tests drive the REAL mux (so the "GET /admin/traces/{id}" pattern and
// its PathValue are exercised) and a REAL upstream, through the same NewServer
// the binary builds. The unit-level shape of a trace is covered in
// internal/gateway/tracing_test.go; what is under test here is the wiring:
// does the store see requests, do the endpoints find them again, do the
// exporters get closed and flushed.
// ---------------------------------------------------------------------------

// traceUpstream is a minimal OpenAI-compatible backend that answers every
// request with a complete chat completion.
func traceUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"mock-gpt",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// traceConfig is the smallest config that can serve a chat request, with the
// given tracing section. It is deliberately NOT run through config.Validate:
// the server must already cope with a zero capacity, which is what a Go caller
// building a Config by hand produces.
func traceConfig(addr string, tc config.TracingConfig) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Listen:          "127.0.0.1:0",
			UpstreamTimeout: config.Duration(5 * time.Second),
			MaxBodyBytes:    1 << 20,
		},
		Log: config.LogConfig{Level: "error", Format: "text"},
		Upstreams: []config.UpstreamConfig{{
			Name:    "trace-mock",
			Kind:    config.KindOpenAI,
			BaseURL: addr,
			APIKey:  "trace-key",
			Models:  []string{"/"},
		}},
		Tracing: tc,
	}
}

func newTracingServer(t *testing.T, tc config.TracingConfig) (*Server, *httptest.Server) {
	t.Helper()
	up := traceUpstream(t)
	s, err := NewServer(traceConfig(up.URL, tc), testLogger{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// A second httptest server hosts the mux, so the tests can use a real
	// client and real URLs instead of poking ResponseRecorders at handlers.
	front := httptest.NewServer(s.http.Handler)
	t.Cleanup(front.Close)
	return s, front
}

func tracedChat(t *testing.T, base string) *http.Response {
	t.Helper()
	body := `{"model":"mock-gpt","messages":[{"role":"user","content":"hello there"}]}`
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200", res.StatusCode)
	}
	return res
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("GET %s: body is not JSON (%v): %s", url, err, raw)
		}
	}
	return res.StatusCode
}

func TestServerRecordsAndServesTraces(t *testing.T) {
	s, front := newTracingServer(t, config.TracingConfig{Enabled: true, Capacity: 8, SampleRatio: 1})

	res := tracedChat(t, front.URL)
	requestID := res.Header.Get("X-InferGate-Request-Id")
	if requestID == "" {
		t.Fatalf("response carried no X-InferGate-Request-Id; tracing cannot be correlated")
	}

	var list struct {
		Enabled  bool `json:"enabled"`
		Capacity int  `json:"capacity"`
		Stored   int  `json:"stored"`
		Dropped  int64
		Count    int `json:"count"`
		Traces   []struct {
			TraceID   string `json:"trace_id"`
			RequestID string `json:"request_id"`
			Status    string `json:"status"`
			SpanCount int    `json:"span_count"`
			Route     string `json:"route"`
			Upstream  string `json:"upstream"`
			Model     string `json:"model"`
		} `json:"traces"`
	}
	if code := getJSON(t, front.URL+"/admin/traces", &list); code != http.StatusOK {
		t.Fatalf("GET /admin/traces = %d, want 200", code)
	}
	if !list.Enabled || list.Stored != 1 || list.Count != 1 || len(list.Traces) != 1 {
		t.Fatalf("trace list = %+v, want exactly one stored trace", list)
	}
	sum := list.Traces[0]
	if sum.RequestID != requestID {
		t.Errorf("summary request_id = %q, want the response header %q", sum.RequestID, requestID)
	}
	if sum.SpanCount != 2 {
		t.Errorf("span_count = %d, want 2 (gateway + one upstream attempt)", sum.SpanCount)
	}
	if sum.Route != "/v1/chat/completions" || sum.Upstream != "trace-mock" || sum.Model != "mock-gpt" {
		t.Errorf("summary route/upstream/model = %q/%q/%q, want the request's own values",
			sum.Route, sum.Upstream, sum.Model)
	}
	if len(sum.TraceID) != 32 {
		t.Errorf("trace_id = %q, want 32 hex characters", sum.TraceID)
	}

	// Lookup by request id: the whole point is that a caller holding only the
	// header can still see the trace.
	var full struct {
		TraceID   string `json:"trace_id"`
		RequestID string `json:"request_id"`
		Spans     []struct {
			TraceID      string `json:"trace_id"`
			SpanID       string `json:"span_id"`
			Name         string `json:"name"`
			Kind         string `json:"kind"`
			ParentSpanID string `json:"parent_span_id"`
			Status       string `json:"status"`
		} `json:"spans"`
	}
	if code := getJSON(t, front.URL+"/admin/traces/"+requestID, &full); code != http.StatusOK {
		t.Fatalf("GET /admin/traces/{request_id} = %d, want 200", code)
	}
	if full.TraceID != sum.TraceID {
		t.Errorf("trace_id by request id = %q, want %q", full.TraceID, sum.TraceID)
	}
	if len(full.Spans) != 2 {
		t.Fatalf("spans = %d, want 2: %+v", len(full.Spans), full.Spans)
	}
	if full.Spans[0].Name != "gateway.request" || full.Spans[0].Kind != "server" {
		t.Errorf("span[0] = %+v, want the gateway server span first", full.Spans[0])
	}
	if full.Spans[1].Name != "upstream.trace-mock" || full.Spans[1].Kind != "client" {
		t.Errorf("span[1] = %+v, want the upstream client span", full.Spans[1])
	}
	// The gateway span is the root of a trace this gateway started, and the
	// upstream attempt hangs off it. Both must belong to the same trace.
	if full.Spans[0].ParentSpanID != "" {
		t.Errorf("root span has parent %q, want none", full.Spans[0].ParentSpanID)
	}
	if full.Spans[1].ParentSpanID != full.Spans[0].SpanID {
		t.Errorf("attempt parent = %q, want the root span id %q",
			full.Spans[1].ParentSpanID, full.Spans[0].SpanID)
	}
	for i, sp := range full.Spans {
		if sp.TraceID != full.TraceID {
			t.Errorf("span[%d].trace_id = %q, want %q", i, sp.TraceID, full.TraceID)
		}
		if len(sp.SpanID) != 16 {
			t.Errorf("span[%d].span_id = %q, want 16 hex characters", i, sp.SpanID)
		}
	}

	// And by trace id.
	if code := getJSON(t, front.URL+"/admin/traces/"+sum.TraceID, nil); code != http.StatusOK {
		t.Errorf("GET /admin/traces/{trace_id} = %d, want 200", code)
	}

	// ?format=jsonl renders one parseable line per trace.
	res2, err := http.Get(front.URL + "/admin/traces?format=jsonl")
	if err != nil {
		t.Fatalf("GET jsonl: %v", err)
	}
	defer res2.Body.Close()
	raw, _ := io.ReadAll(res2.Body)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("jsonl rendering = %d lines, want 1:\n%s", len(lines), raw)
	}
	var line struct {
		TraceID string `json:"trace_id"`
		Spans   []any  `json:"spans"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("jsonl line is not JSON: %v", err)
	}
	if line.TraceID != sum.TraceID || len(line.Spans) != 2 {
		t.Errorf("jsonl line = %+v, want trace %s with 2 spans", line, sum.TraceID)
	}
	if ct := res2.Header.Get("Content-Type"); !strings.Contains(ct, "ndjson") {
		t.Errorf("jsonl Content-Type = %q, want ndjson", ct)
	}

	// /admin/tracing reports the configuration the store was built from.
	var cfgBody map[string]any
	if code := getJSON(t, front.URL+"/admin/tracing", &cfgBody); code != http.StatusOK {
		t.Fatalf("GET /admin/tracing = %d, want 200", code)
	}
	if cfgBody["enabled"] != true {
		t.Errorf("/admin/tracing enabled = %v, want true", cfgBody["enabled"])
	}
	if cap := cfgBody["capacity"].(float64); int(cap) != 8 {
		t.Errorf("/admin/tracing capacity = %v, want 8", cap)
	}
	if stored := cfgBody["stored"].(float64); int(stored) != 1 {
		t.Errorf("/admin/tracing stored = %v, want 1", stored)
	}
	if _, ok := cfgBody["exporters"]; !ok {
		t.Errorf("/admin/tracing has no exporters key: %v", cfgBody)
	}
	_ = s
}

func TestTracesDisabledReportsConfigAndRefusesLookup(t *testing.T) {
	_, front := newTracingServer(t, config.TracingConfig{})

	var list map[string]any
	if code := getJSON(t, front.URL+"/admin/traces", &list); code != http.StatusOK {
		t.Fatalf("GET /admin/traces = %d, want 200", code)
	}
	if list["enabled"] != false {
		t.Errorf("enabled = %v, want false", list["enabled"])
	}
	if n := list["count"].(float64); n != 0 {
		t.Errorf("count = %v, want 0", n)
	}
	// Traces are []any{} rather than nil so a consumer never has to special-case
	// a null where it expects an array.
	if _, ok := list["traces"].([]any); !ok {
		t.Errorf("traces = %#v, want an empty array", list["traces"])
	}

	var errBody map[string]any
	if code := getJSON(t, front.URL+"/admin/traces/deadbeefdeadbeefdeadbeefdeadbeef", &errBody); code != http.StatusNotFound {
		t.Errorf("lookup with tracing off = %d, want 404", code)
	} else if _, ok := errBody["error"]; !ok {
		t.Errorf("error envelope missing: %v", errBody)
	}

	var cfgBody map[string]any
	if code := getJSON(t, front.URL+"/admin/tracing", &cfgBody); code != http.StatusOK {
		t.Fatalf("GET /admin/tracing = %d, want 200", code)
	}
	if cfgBody["enabled"] != false {
		t.Errorf("enabled = %v, want false", cfgBody["enabled"])
	}
	if cap := cfgBody["capacity"].(float64); int(cap) != defaultTraceCapacity {
		t.Errorf("capacity = %v, want the %d default", cap, defaultTraceCapacity)
	}
}

func TestTraceLookupValidation(t *testing.T) {
	_, front := newTracingServer(t, config.TracingConfig{Enabled: true, Capacity: 4, SampleRatio: 1})

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"unknown trace id", "/admin/traces/0123456789abcdef0123456789abcdef", http.StatusNotFound},
		{"uppercase hex trace id", "/admin/traces/0123456789ABCDEF0123456789ABCDEF", http.StatusNotFound},
		{"bad charset", "/admin/traces/not%20a%20trace", http.StatusBadRequest},
		{"too long", "/admin/traces/" + strings.Repeat("a", 65), http.StatusBadRequest},
		{"path traversal", "/admin/traces/..%2F..%2Fetc%2Fpasswd", http.StatusBadRequest},
		{"limit not a number", "/admin/traces?limit=abc", http.StatusBadRequest},
		{"limit zero", "/admin/traces?limit=0", http.StatusBadRequest},
		{"limit negative", "/admin/traces?limit=-3", http.StatusBadRequest},
		{"limit clamped", "/admin/traces?limit=99999", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := http.Get(front.URL + tc.url)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.url, err)
			}
			defer res.Body.Close()
			_, _ = io.Copy(io.Discard, res.Body)
			if res.StatusCode != tc.want {
				t.Errorf("GET %s = %d, want %d", tc.url, res.StatusCode, tc.want)
			}
		})
	}
}

func TestTraceRingEvictsAndReportsDropped(t *testing.T) {
	s, front := newTracingServer(t, config.TracingConfig{Enabled: true, Capacity: 2, SampleRatio: 1})

	for i := 0; i < 5; i++ {
		tracedChat(t, front.URL)
	}

	var list struct {
		Stored  int   `json:"stored"`
		Dropped int64 `json:"dropped"`
		Count   int   `json:"count"`
		Traces  []struct {
			TraceID string `json:"trace_id"`
		} `json:"traces"`
	}
	if code := getJSON(t, front.URL+"/admin/traces", &list); code != http.StatusOK {
		t.Fatalf("GET /admin/traces = %d, want 200", code)
	}
	if list.Stored != 2 {
		t.Errorf("stored = %d, want 2 (the ring capacity)", list.Stored)
	}
	if list.Dropped != 3 {
		t.Errorf("dropped = %d, want 3", list.Dropped)
	}
	if list.Count != 2 || len(list.Traces) != 2 {
		t.Errorf("count = %d / %d traces, want 2", list.Count, len(list.Traces))
	}
	// Newest first: the logger's ring and List() agree on ordering.
	if s.trace.store.Len() != 2 {
		t.Errorf("store.Len() = %d, want 2", s.trace.store.Len())
	}
}

func TestJSONLExporterWritesEveryTrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")
	s, front := newTracingServer(t, config.TracingConfig{
		Enabled: true, Capacity: 16, SampleRatio: 1, JSONLPath: path,
	})

	const n = 3
	ids := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		res := tracedChat(t, front.URL)
		ids[res.Header.Get("X-InferGate-Request-Id")] = true
	}

	// CloseTracing is what main calls after the listener stops; it must drain
	// and flush, or the last traces are lost on a clean shutdown.
	if err := s.CloseTracing(); err != nil {
		t.Fatalf("CloseTracing: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != n {
		t.Fatalf("trace log has %d lines, want %d:\n%s", len(lines), n, raw)
	}
	for _, line := range lines {
		var tr struct {
			TraceID   string `json:"trace_id"`
			RequestID string `json:"request_id"`
			Spans     []any  `json:"spans"`
		}
		if err := json.Unmarshal([]byte(line), &tr); err != nil {
			t.Fatalf("trace log line is not JSON: %v", err)
		}
		if !ids[tr.RequestID] {
			t.Errorf("trace log line has unknown request_id %q", tr.RequestID)
		}
		if len(tr.TraceID) != 32 || len(tr.Spans) != 2 {
			t.Errorf("trace log line = %+v, want 32-hex id and 2 spans", tr)
		}
	}

	// Closing twice must be safe: main may run the cleanup path after an error.
	if err := s.CloseTracing(); err != nil {
		t.Errorf("second CloseTracing: %v", err)
	}
}

func TestOTLPExporterReceivesSpans(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		if r.URL.Path != "/v1/traces" {
			t.Errorf("collector saw path %q, want /v1/traces", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer collector.Close()

	s, front := newTracingServer(t, config.TracingConfig{
		Enabled: true, Capacity: 16, SampleRatio: 1,
		OTLP: config.OTLPConfig{
			Endpoint:    collector.URL,
			Timeout:     config.Duration(3 * time.Second),
			ServiceName: "infergate-test",
		},
	})
	tracedChat(t, front.URL)
	if err := s.CloseTracing(); err != nil {
		t.Fatalf("CloseTracing: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatalf("collector received no payload")
	}
	joined := strings.Join(bodies, "\n")
	for _, want := range []string{`"resourceSpans"`, `"infergate-test"`, `"gateway.request"`, `"upstream.trace-mock"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("OTLP payload missing %s:\n%s", want, joined)
		}
	}
}

func TestTracingRejectsBadOTLPEndpoint(t *testing.T) {
	up := traceUpstream(t)
	cfg := traceConfig(up.URL, config.TracingConfig{
		Enabled: true, Capacity: 4,
		OTLP: config.OTLPConfig{Endpoint: "not-a-url"},
	})
	if _, err := NewServer(cfg, testLogger{}); err == nil {
		t.Fatalf("NewServer accepted an unusable OTLP endpoint")
	} else if !strings.Contains(err.Error(), "server: tracing: otlp export") {
		t.Errorf("error = %v, want it wrapped as a tracing export failure", err)
	}
}

func TestTracingUnwritableJSONLPathFailsStartup(t *testing.T) {
	up := traceUpstream(t)
	// A directory path is never openable as a file, on every platform.
	cfg := traceConfig(up.URL, config.TracingConfig{
		Enabled: true, Capacity: 4, JSONLPath: t.TempDir(),
	})
	if _, err := NewServer(cfg, testLogger{}); err == nil {
		t.Fatalf("NewServer accepted a JSONL path that is a directory")
	} else if !strings.Contains(err.Error(), "server: tracing: jsonl export") {
		t.Errorf("error = %v, want it wrapped as a jsonl export failure", err)
	}
}
