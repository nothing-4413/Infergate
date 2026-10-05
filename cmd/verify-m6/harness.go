package main

// Shared harness for the M6 acceptance gate: assertion bookkeeping, an HTTP
// client, a scripted upstream that can act like a tool-calling model, and the
// in-process gateway stack.
//
// The choices echo verify-m5's harness because they are the same problem: the
// logger is built through internal/logging (the same construction path the
// binary uses) and every stack hosts the REAL assembled gateway from
// server.NewServer over its exported Handler(), not a hand-built proxy. A gate
// that wired its own subset would pass on code the binary never runs.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/logging"
	"github.com/infergate/infergate/internal/server"
)

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

type checker struct {
	passed int
	failed int
	notes  []string
}

func (c *checker) assert(cond bool, format string, args ...any) bool {
	if cond {
		c.passed++
		return true
	}
	c.failed++
	msg := fmt.Sprintf(format, args...)
	c.notes = append(c.notes, msg)
	fmt.Printf("  FAIL  %s\n", msg)
	return false
}

func (c *checker) equal(got, want any, format string, args ...any) bool {
	return c.assert(fmt.Sprint(got) == fmt.Sprint(want),
		"%s: got %v, want %v", fmt.Sprintf(format, args...), got, want)
}

func (c *checker) contains(haystack, needle, format string, args ...any) bool {
	return c.assert(strings.Contains(haystack, needle),
		"%s: %q does not contain %q", fmt.Sprintf(format, args...), truncate(haystack, 200), needle)
}

func section(format string, args ...any) {
	fmt.Printf("\n== %s ==\n", fmt.Sprintf(format, args...))
}

func info(format string, args ...any) {
	fmt.Printf("   %s\n", fmt.Sprintf(format, args...))
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func newLogger(c *checker) *slog.Logger {
	logger, err := logging.New(io.Discard, "error", "text")
	if !c.assert(err == nil, "harness: build the server logger (%v)", err) {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return logger
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

var httpClient = &http.Client{Timeout: 20 * time.Second}

type result struct {
	status int
	body   string
	header http.Header
}

func (r result) headerValue(name string) string { return r.header.Get(name) }

func (r result) json(out any) error { return json.Unmarshal([]byte(r.body), out) }

func do(c *checker, method, url, body string, hdr map[string]string, label string) result {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		c.assert(false, "%s: build %s %s (%v)", label, method, url, err)
		return result{status: -1}
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		c.assert(false, "%s: send %s %s (%v)", label, method, url, err)
		return result{status: -1}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.assert(false, "%s: read the response body (%v)", label, err)
		return result{status: resp.StatusCode, header: resp.Header}
	}
	return result{status: resp.StatusCode, body: string(raw), header: resp.Header}
}

func post(c *checker, base, path, body string, hdr map[string]string, label string) result {
	return do(c, http.MethodPost, strings.TrimRight(base, "/")+path, body, hdr, label)
}

func get(c *checker, base, path, label string) result {
	return do(c, http.MethodGet, strings.TrimRight(base, "/")+path, "", nil, label)
}

func getJSON(c *checker, base, path, label string, out any) bool {
	res := get(c, base, path, label)
	if !c.assert(res.status == http.StatusOK, "%s: HTTP 200 from %s (got %d: %s)",
		label, path, res.status, truncate(res.body, 200)) {
		return false
	}
	if err := res.json(out); err != nil {
		c.assert(false, "%s: %s is JSON (%v)", label, path, err)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Request bodies
// ---------------------------------------------------------------------------

func chatBody(model, user string) string {
	return marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": user}},
		"max_tokens": 16,
	})
}

func streamChatBody(model, user string) string {
	return marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": user}},
		"max_tokens": 16,
		"stream":     true,
	})
}

// toolsBody is what a function-calling agent sends on the first turn.
func toolsBody(model, user string) string {
	return marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": user}},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "current weather for a city",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
					"required":   []string{"city"},
				},
			},
		}},
		"max_tokens": 32,
	})
}

// toolResultBody is the second turn: the assistant's tool_call plus the tool's
// result, which is the shape Warden's FunctionCallAgent persists.
func toolResultBody(model, callID, result string) string {
	return marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]string{"role": "user", "content": "what is the weather in Beijing"},
			map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []map[string]any{{
					"id":   callID,
					"type": "function",
					"function": map[string]string{
						"name":      "get_weather",
						"arguments": `{"city": "Beijing"}`,
					},
				}},
			},
			map[string]string{"role": "tool", "tool_call_id": callID, "content": result},
		},
		"max_tokens": 32,
	})
}

func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Upstreams
// ---------------------------------------------------------------------------

type upstreamCall struct {
	Path   string
	Header http.Header
	Body   string
}

// scriptedUpstream behaves like a deterministic tool-calling model.
//
// It answers a request that carries a tool RESULT with a final answer that
// echoes the tool's output, and a request that carries tools with a tool call.
// That is the whole shape of an agent turn, and it makes a two-turn conversation
// reproducible without a model: the recomposed final answer proves the tool
// result travelled out and back.
type scriptedUpstream struct {
	server *httptest.Server

	// failFirst makes the first N chat requests fail with 500, which is how a
	// transient provider failure is injected without a second upstream.
	failFirst int64

	// block, when non-nil, holds every request until it is closed.
	block chan struct{}

	mu    sync.Mutex
	calls []upstreamCall
	count int64
}

func newScriptedUpstream() *scriptedUpstream {
	u := &scriptedUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(u.handle))
	return u
}

func (u *scriptedUpstream) URL() string { return u.server.URL }

func (u *scriptedUpstream) Close() {
	if u != nil && u.server != nil {
		u.server.Close()
	}
}

func (u *scriptedUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/healthz") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	raw, _ := io.ReadAll(r.Body)

	u.mu.Lock()
	u.calls = append(u.calls, upstreamCall{Path: r.URL.Path, Header: r.Header.Clone(), Body: string(raw)})
	u.count++
	n := u.count
	fail := u.failFirst > 0 && n <= u.failFirst
	u.mu.Unlock()

	if u.block != nil {
		<-u.block
	}

	if r.URL.Path == "/v1/embeddings" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"provider is restarting","type":"server_error"}}`)
		return
	}

	body := string(raw)
	switch {
	case strings.Contains(body, `"role":"tool"`) || strings.Contains(body, `"role": "tool"`):
		// The final turn: echo the tool output so the caller can verify that
		// the tool result reached the model and came back in the answer.
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-final","object":"chat.completion","model":"mock-gpt",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":21,"completion_tokens":7,"total_tokens":28}}`, finalAnswerOf(body))
	case strings.Contains(body, `"tools"`):
		_, _ = io.WriteString(w, `{"id":"chatcmpl-tool","object":"chat.completion","model":"mock-gpt",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
			`{"id":"call_abc123","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Beijing\"}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`)
	case strings.Contains(body, `"stream":true`):
		// A real SSE generation: the replay of this is one write of the whole
		// transcript, which is the property the gate checks.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`{"id":"chatcmpl-s","object":"chat.completion.chunk","model":"mock-gpt","choices":[{"index":0,"delta":{"role":"assistant","content":"mock "},"finish_reason":null}]}`,
			`{"id":"chatcmpl-s","object":"chat.completion.chunk","model":"mock-gpt","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-s","object":"chat.completion.chunk","model":"mock-gpt","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"chatcmpl-s","object":"chat.completion.chunk","model":"mock-gpt","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	default:
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"mock-gpt",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"mock answer"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	}
}

// finalAnswerOf extracts the tool result from a message array and returns the
// sentence the scripted model "writes" from it.
func finalAnswerOf(body string) string {
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return "the tool said something unreadable"
	}
	for _, m := range payload.Messages {
		if m.Role == "tool" {
			return "the tool said: " + m.Content
		}
	}
	return "no tool result reached the model"
}

func (u *scriptedUpstream) Count() int {
	if u == nil {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *scriptedUpstream) Calls() []upstreamCall {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamCall(nil), u.calls...)
}

func (u *scriptedUpstream) Last() upstreamCall {
	calls := u.Calls()
	if len(calls) == 0 {
		return upstreamCall{}
	}
	return calls[len(calls)-1]
}

// ---------------------------------------------------------------------------
// The gateway under test
// ---------------------------------------------------------------------------

type stack struct {
	url     string
	cfg     *config.Config
	srv     *server.Server
	httpSrv *http.Server
}

// newStack assembles the real gateway over a real listener. The config is taken
// by value because every check mutates the copy it built before handing it over.
func newStack(c *checker, label string, cfg config.Config) *stack {
	cfg.Server.Listen = ":0"
	if err := cfg.Validate(); err != nil {
		c.assert(false, "%s: config validates (%v)", label, err)
		return nil
	}
	srv, err := server.NewServer(&cfg, newLogger(c))
	if err != nil {
		c.assert(false, "%s: build the gateway (%v)", label, err)
		return nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if !c.assert(err == nil, "%s: listen on a free port (%v)", label, err) {
		return nil
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()
	return &stack{url: "http://" + ln.Addr().String(), cfg: &cfg, srv: srv, httpSrv: httpSrv}
}

func (s *stack) Close(c *checker, label string) {
	if s == nil {
		return
	}
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
	if s.srv != nil {
		_ = s.srv.CloseTracing()
	}
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// upstreamConfig is a catch-all backend: Models ["/"] makes it eligible for any
// model name, so a check reads the serving backend from the response header
// rather than from model routing.
func upstreamConfig(name, url string, capabilities ...string) config.UpstreamConfig {
	return config.UpstreamConfig{
		Name:         name,
		Kind:         config.KindOpenAI,
		BaseURL:      url,
		APIKey:       "test-key",
		Models:       []string{"/"},
		Capabilities: capabilities,
		Priority:     1,
		Weight:       1,
	}
}

// baseConfig returns an M6-enabled configuration over one backend.
//
// Both M6 features are ON here, unlike the shipped defaults, because the gate's
// subject IS those features; the "off" behaviour is checked explicitly by
// turning them off in the one check that tests it.
func baseConfig(ups ...config.UpstreamConfig) config.Config {
	cfg := config.Defaults()
	cfg.Log.Level = "error"
	cfg.Server.Listen = ":0"
	cfg.Upstreams = ups
	cfg.Health.MinRequests = 1000 // never trip a breaker unless a check wants it
	cfg.Pricing = config.PricingConfig{
		Default: config.ModelPrice{In: 1, Out: 3},
		Models:  map[string]config.ModelPrice{"mock-gpt": {In: 2, Out: 6}},
	}
	cfg.Idempotency.Enabled = true
	cfg.Idempotency.Capacity = 64
	cfg.Idempotency.TTL = config.Duration(time.Minute)
	cfg.Sessions.Enabled = true
	cfg.Sessions.Capacity = 64
	cfg.Sessions.TTL = config.Duration(time.Hour)
	cfg.Sessions.RecentPerSession = 8
	// Tracing is off in the shipped defaults; the gate turns it on because the
	// idempotency decision and the session id are only visible in the trace, and
	// a check that could not see them would be agreeing with the headers alone.
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 256
	cfg.Tracing.SampleRatio = 1
	return cfg
}

// ---------------------------------------------------------------------------
// Prometheus helpers
// ---------------------------------------------------------------------------

func metricFamilyType(text, name string) string {
	re := regexp.MustCompile(`(?m)^# TYPE ` + regexp.QuoteMeta(name) + ` (\w+)$`)
	if m := re.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// metricValue returns the value of name{labels...} whose label set contains the
// fragment, or -1 when the series is absent.
func metricValue(text, name, labelFragment string) float64 {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\{([^}]*)\}\s+([0-9.eE+-]+)\s*$`)
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if strings.Contains(m[1], labelFragment) {
			v, err := strconv.ParseFloat(m[2], 64)
			if err == nil {
				return v
			}
		}
	}
	return -1
}

// metricScalar returns the value of an unlabelled series, or -1.
func metricScalar(text, name string) float64 {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s+([0-9.eE+-]+)\s*$`)
	if m := re.FindStringSubmatch(text); m != nil {
		v, err := strconv.ParseFloat(m[1], 64)
		if err == nil {
			return v
		}
	}
	return -1
}
