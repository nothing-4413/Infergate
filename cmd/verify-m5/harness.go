package main

// Shared harness for the M5 acceptance gate: assertion bookkeeping, an HTTP
// client, a recording upstream that can answer whole, stream and fail, the
// in-process gateway stack, and a small Prometheus text parser.
//
// Two deliberate choices echo verify-m4's harness because they are the same
// problem: the logger is built through internal/logging (so the gate exercises
// the same construction path the binary uses) and every stack hosts the REAL
// assembled gateway from server.NewServer over its exported Handler(), not a
// hand-built proxy. A gate that wired its own subset would pass on code the
// binary never runs.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
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

// section prints a heading for one group of checks; info prints a measurement
// worth seeing in the output even when it is not itself an assertion.
func section(format string, args ...any) {
	fmt.Printf("\n== %s ==\n", fmt.Sprintf(format, args...))
}

func info(format string, args ...any) {
	fmt.Printf("   %s\n", fmt.Sprintf(format, args...))
}

// verboseChecks gates the per-observation logging (-v). The checks that record
// hundreds of samples would otherwise bury the assertions they exist to prove.
var verboseChecks bool

func detail(format string, args ...any) {
	if verboseChecks {
		fmt.Printf("   - %s\n", fmt.Sprintf(format, args...))
	}
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

// getJSON fetches a path and decodes it, asserting the status first so a
// failure names the endpoint rather than a JSON syntax error.
func getJSON(c *checker, base, path, label string, out any) bool {
	res := get(c, base, path, label)
	if !c.assert(res.status == http.StatusOK, "%s: HTTP 200 from %s (got %d: %s)",
		label, path, res.status, truncate(res.body, 160)) {
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

const (
	upstreamJSON   = "json"
	upstreamStream = "stream"
	upstreamFail   = "fail"
	// upstreamAuto answers the way the request asked: SSE for a body carrying
	// "stream":true, a whole JSON body otherwise. That is what an OpenAI-
	// compatible backend does, so one check can mix streamed and whole requests
	// against a single backend and still see realistic metrics.
	upstreamAuto = "auto"
)

type upstreamCall struct {
	Path   string
	Header http.Header
	Body   string
}

type recordingUpstream struct {
	server  *httptest.Server
	mode    string
	content string

	mu    sync.Mutex
	calls []upstreamCall
}

func newUpstream(mode, content string) *recordingUpstream {
	if content == "" {
		content = "mock answer"
	}
	u := &recordingUpstream{mode: mode, content: content}
	u.server = httptest.NewServer(http.HandlerFunc(u.handle))
	return u
}

func (u *recordingUpstream) URL() string { return u.server.URL }

func (u *recordingUpstream) Close() {
	if u != nil && u.server != nil {
		u.server.Close()
	}
}

func (u *recordingUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/healthz") || r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.calls = append(u.calls, upstreamCall{Path: r.URL.Path, Header: r.Header.Clone(), Body: string(raw)})
	u.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	mode := u.mode
	if mode == upstreamAuto {
		mode = upstreamJSON
		if strings.Contains(string(raw), `"stream":true`) || strings.Contains(string(raw), `"stream": true`) {
			mode = upstreamStream
		}
	}
	switch mode {
	case upstreamFail:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"backend is down","type":"server_error"}}`)
	case upstreamStream:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		frames := []string{
			`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"mock-gpt","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"mock-gpt","choices":[{"index":0,"delta":{"content":"mock "},"finish_reason":null}]}`,
			`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"mock-gpt","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
			`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"mock-gpt","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
		}
		for _, f := range frames {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", f); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	default:
		_, _ = fmt.Fprintf(w,
			`{"id":"mock-1","object":"chat.completion","created":1,"model":"mock-gpt",`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`, u.content)
	}
}

func (u *recordingUpstream) Count() int {
	if u == nil {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *recordingUpstream) Calls() []upstreamCall {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamCall(nil), u.calls...)
}

func (u *recordingUpstream) Last() upstreamCall {
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

// newStack assembles the real gateway over a real listener.
//
// Listen is forced to :0 and the address is taken from the listener, so two
// stacks in one run never collide and nothing depends on a config file.
func newStack(c *checker, label string, cfg *config.Config) *stack {
	cfg.Server.Listen = ":0"
	if err := cfg.Validate(); err != nil {
		c.assert(false, "%s: config validates (%v)", label, err)
		return nil
	}
	srv, err := server.NewServer(cfg, newLogger(c))
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
	return &stack{url: "http://" + ln.Addr().String(), cfg: cfg, srv: srv, httpSrv: httpSrv}
}

// Close shuts the listener down and then closes tracing, which is the order
// cmd/infergate uses and the only way a JSONL exporter is guaranteed to have
// flushed by the time a check reads the file.
func (s *stack) Close(c *checker, label string) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if s.httpSrv != nil {
		_ = s.httpSrv.Shutdown(ctx)
	}
	if s.srv != nil {
		_ = s.srv.Shutdown(ctx)
		if err := s.srv.CloseTracing(); err != nil {
			c.assert(false, "%s: close tracing (%v)", label, err)
		}
	}
}

// upstreamConfig is a catch-all backend: Models ["/"] makes it eligible for
// every model name, so a check that wants to see which backend served a request
// reads the response header rather than relying on model routing.
func upstreamConfig(name, url string) config.UpstreamConfig {
	return config.UpstreamConfig{
		Name:         name,
		Kind:         config.KindOpenAI,
		BaseURL:      url,
		APIKey:       "test-key",
		Models:       []string{"/"},
		Capabilities: []string{"chat"},
		Priority:     1,
		Weight:       1,
	}
}

func baseConfig(upstreams ...config.UpstreamConfig) config.Config {
	cfg := config.Defaults()
	cfg.Log.Level = "error"
	cfg.Server.Listen = ":0"
	cfg.Routing = config.RoutingConfig{Strategy: config.StrategyPriority}
	cfg.Upstreams = upstreams
	return cfg
}

// ---------------------------------------------------------------------------
// Prometheus text parsing
// ---------------------------------------------------------------------------

type sample struct {
	name   string
	labels map[string]string
	value  float64
}

func parseSamples(text string) []sample {
	var out []sample
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var name, labelText, valueText string
		if open := strings.IndexByte(line, '{'); open >= 0 {
			close := strings.IndexByte(line, '}')
			if close < open {
				continue
			}
			name = line[:open]
			labelText = line[open+1 : close]
			fields := strings.Fields(line[close+1:])
			if len(fields) != 1 {
				continue
			}
			valueText = fields[0]
		} else {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			name, valueText = fields[0], fields[1]
		}
		value, err := strconv.ParseFloat(valueText, 64)
		if err != nil {
			continue
		}
		out = append(out, sample{name: name, labels: parseLabels(labelText), value: value})
	}
	return out
}

// parseLabels reads a Prometheus label list, honouring quoted values and the
// three escapes the exposition format defines (\\, \" and \n).
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(s) {
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(s[i : i+eq])
		j := i + eq + 1
		if j >= len(s) || s[j] != '"' {
			break
		}
		j++
		var b strings.Builder
		for j < len(s) {
			ch := s[j]
			if ch == '\\' && j+1 < len(s) {
				switch s[j+1] {
				case 'n':
					b.WriteByte('\n')
				case '\\':
					b.WriteByte('\\')
				case '"':
					b.WriteByte('"')
				default:
					b.WriteByte(s[j+1])
				}
				j += 2
				continue
			}
			if ch == '"' {
				j++
				break
			}
			b.WriteByte(ch)
			j++
		}
		out[key] = b.String()
		i = j
		if i < len(s) && s[i] == ',' {
			i++
		}
	}
	return out
}

func families(text string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			out[fields[2]] = true
		}
	}
	return out
}

func findSample(samples []sample, name string, want map[string]string) (sample, bool) {
	for _, s := range samples {
		if s.name != name {
			continue
		}
		match := true
		for k, v := range want {
			if s.labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return s, true
		}
	}
	return sample{}, false
}

// histGroup is one histogram series set: every `le` bucket plus _count and _sum.
type histGroup struct {
	labels  map[string]string
	buckets []histBucket
	count   float64
	sum     float64
	hasSum  bool
}

type histBucket struct {
	le    string
	bound float64
	count float64
}

// histogramGroups folds the exposure text into one group per label set, which is
// what lets a check assert the cumulative invariant and `+Inf == _count` the way
// Prometheus itself would read it.
func histogramGroups(samples []sample, family string) []histGroup {
	byKey := map[string]*histGroup{}
	for _, s := range samples {
		var key string
		var g *histGroup
		switch s.name {
		case family + "_bucket":
			labels := copyLabels(s.labels)
			le := labels["le"]
			delete(labels, "le")
			key = labelKey(labels)
			g = byKey[key]
			if g == nil {
				g = &histGroup{labels: labels}
				byKey[key] = g
			}
			bound := math.Inf(1)
			if le != "+Inf" {
				parsed, err := strconv.ParseFloat(le, 64)
				if err != nil {
					continue
				}
				bound = parsed
			}
			g.buckets = append(g.buckets, histBucket{le: le, bound: bound, count: s.value})
		case family + "_count":
			labels := copyLabels(s.labels)
			key = labelKey(labels)
			g = byKey[key]
			if g == nil {
				g = &histGroup{labels: labels}
				byKey[key] = g
			}
			g.count = s.value
		case family + "_sum":
			labels := copyLabels(s.labels)
			key = labelKey(labels)
			g = byKey[key]
			if g == nil {
				g = &histGroup{labels: labels}
				byKey[key] = g
			}
			g.sum = s.value
			g.hasSum = true
		}
	}
	out := make([]histGroup, 0, len(byKey))
	for _, g := range byKey {
		sort.Slice(g.buckets, func(i, j int) bool { return g.buckets[i].bound < g.buckets[j].bound })
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return labelKey(out[i].labels) < labelKey(out[j].labels) })
	return out
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func labelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(',')
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Timed observation helper
// ---------------------------------------------------------------------------

// durations builds n strictly increasing durations so a percentile check knows
// the exact expected answer instead of merely that the numbers are ordered.
func durations(n int, step time.Duration) []time.Duration {
	out := make([]time.Duration, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, time.Duration(i)*step)
	}
	return out
}
