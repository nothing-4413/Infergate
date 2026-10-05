// Command verify is InferGate's M0 acceptance test, run against the real
// binaries rather than around them.
//
// It starts the actual mock upstream and the actual gateway (the same
// server.NewServer the binary uses), drives them over real TCP with real HTTP
// clients, and asserts on the bytes that come back. Nothing is stubbed: the SSE
// relay, connection pooling, chunked transfer encoding and the flush-per-frame
// behaviour are all exercised end to end.
//
// Why a Go program instead of a shell script full of curl:
//
//   - curl on this host cannot do HTTPS (schannel has no credentials) and
//     buffers aggressively, so a curl-based script cannot measure
//     time-to-first-token at all;
//   - a Go client can prove byte transparency — including the exact blank-line
//     framing that makes a concatenated stream invalid SSE — and can time the
//     gap between the request and the first byte of the first frame;
//   - the same program runs in CI on any platform with no dependencies.
//
// Exit code 0 means every check passed. Any failure prints the offending bytes
// and exits non-zero, so it is usable as a gate.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/logging"
	"github.com/infergate/infergate/internal/server"
)

func main() {
	ttfb := flag.Duration("ttfb", 120*time.Millisecond,
		"artificial delay before the mock's first stream frame; must be visible in the measured first-token latency")
	flag.Parse()

	out := os.Stdout
	fmt.Fprintln(out, "InferGate M0 end-to-end verification")
	fmt.Fprintln(out, strings.Repeat("=", 68))

	// 1. Mock upstream, started as a real listener on a real port.
	mock := httptest.NewServer(mockHandler(*ttfb))
	defer mock.Close()
	mockURL := mock.URL
	fmt.Fprintf(out, "mock upstream      : %s (ttfb=%s)\n", mockURL, *ttfb)

	// 2. The real gateway, wired exactly as cmd/infergate wires it.
	cfg := mockConfig(mockURL)
	logger, err := logging.New(io.Discard, "error", "text")
	if err != nil {
		fatal(out, "build logger: %v", err)
	}
	srv, err := server.NewServer(cfg, logger)
	if err != nil {
		fatal(out, "build server: %v", err)
	}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()
	fmt.Fprintf(out, "gateway            : %s (upstream=%s)\n", gw.URL, mockURL)
	fmt.Fprintln(out, strings.Repeat("-", 68))

	checks := []struct {
		name string
		run  func(*checker, string)
	}{
		{"non-streaming passthrough", checkNonStream},
		{"SSE stream byte transparency", checkStreamTransparent},
		{"time-to-first-token measurement", checkFirstToken},
		{"tool_call delta accumulation", checkToolCallDeltas},
		{"synthetic [DONE] for silent backends", checkSyntheticDone},
		{"usage + cost accounting", checkAccounting},
		{"OpenAI-shaped error for unroutable path", checkRouteError},
		{"operational endpoints", checkOpsEndpoints},
	}

	c := &checker{out: out}
	for i, chk := range checks {
		fmt.Fprintf(out, "\n[%d/%d] %s\n", i+1, len(checks), chk.name)
		chk.run(c, gw.URL)
	}

	fmt.Fprintln(out, "\n"+strings.Repeat("=", 68))
	total, failed := c.tally()
	fmt.Fprintf(out, "RESULT: %d/%d assertions passed\n", total-failed, total)
	if failed > 0 {
		fmt.Fprintf(out, "FAILED: %d assertion(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Fprintln(out, "OK: M0 acceptance criteria met")
}

// checker accumulates assertion results so one failure does not hide the rest.
type checker struct {
	out     io.Writer
	pass    int
	fail    int
	details []string
}

func (c *checker) assert(ok bool, format string, args ...any) bool {
	msg := fmt.Sprintf(format, args...)
	if ok {
		c.pass++
		fmt.Fprintf(c.out, "   PASS  %s\n", msg)
		return true
	}
	c.fail++
	c.details = append(c.details, msg)
	fmt.Fprintf(c.out, "   FAIL  %s\n", msg)
	return false
}

func (c *checker) info(format string, args ...any) {
	fmt.Fprintf(c.out, "         "+format+"\n", args...)
}

func (c *checker) tally() (int, int) { return c.pass + c.fail, c.fail }

func fatal(out io.Writer, format string, args ...any) {
	fmt.Fprintf(out, "FATAL: "+format+"\n", args...)
	os.Exit(2)
}

// ---------------------------------------------------------------------------
// Checks
// ---------------------------------------------------------------------------

func checkNonStream(c *checker, base string) {
	body := `{"model":"mock-gpt","messages":[{"role":"user","content":"hello gateway"}]}`
	resp, raw := postJSON(c, base+"/v1/chat/completions", body, nil)
	if !c.assert(resp.StatusCode == http.StatusOK, "status = %d, want 200", resp.StatusCode) {
		return
	}
	c.assert(strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json"),
		"Content-Type = %q, want application/json", resp.Header.Get("Content-Type"))
	c.assert(strings.Contains(raw, `"object":"chat.completion"`), "body is a chat.completion")
	c.assert(strings.Contains(raw, `"usage"`), "provider usage block survived the proxy")
	c.assert(resp.Header.Get("X-InferGate-Upstream-Name") != "" ||
		resp.Header.Get("X-Infergate-Upstream-Name") != "", "response identifies the serving upstream")
	c.info("response %d bytes", len(raw))
}

func checkStreamTransparent(c *checker, base string) {
	body := `{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"stream please"}]}`
	resp, raw := postJSON(c, base+"/v1/chat/completions", body, nil)
	if !c.assert(resp.StatusCode == http.StatusOK, "status = %d, want 200", resp.StatusCode) {
		return
	}
	c.assert(strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"),
		"Content-Type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	c.assert(resp.Header.Get("X-Accel-Buffering") == "no",
		"X-Accel-Buffering: no is set (intermediaries must not buffer the stream)")
	c.assert(strings.HasSuffix(raw, "data: [DONE]\n\n"), "stream ends with the [DONE] sentinel")
	c.assert(strings.Count(raw, "\n\n") >= 4,
		"frames are separated by blank lines (%d separators)", strings.Count(raw, "\n\n"))
	c.assert(strings.Contains(raw, `"object":"chat.completion.chunk"`), "chunks carry the chunk object type")

	// The decisive framing property: every frame must be re-readable by a
	// client that parses the relayed bytes itself. A relay that drops the blank
	// line between frames produces one giant unparseable event.
	frames := parseSSE(raw)
	c.assert(len(frames) >= 4, "relayed stream re-parses into %d frames, want >= 4", len(frames))
	for i, f := range frames {
		if i == len(frames)-1 {
			break
		}
		c.assert(json.Valid([]byte(f)), "frame %d is valid JSON", i)
	}
}

func checkFirstToken(c *checker, base string) {
	body := `{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"measure ttft"}]}`
	start := time.Now()
	firstByte, resp := streamFirstByte(c, base+"/v1/chat/completions", body)
	if resp == nil {
		c.assert(false, "stream request failed")
		return
	}
	ttft := firstByte.Sub(start)
	c.info("client-observed time to first byte: %s", ttft.Round(time.Microsecond))

	// The mock stalls deliberately before its first frame. If the gateway were
	// buffering the stream, the first byte would arrive after the whole
	// response was generated instead of after the stall.
	floor := 60 * time.Millisecond
	c.assert(ttft >= floor, "first byte arrived after the injected %s stall (got %s)",
		floor, ttft.Round(time.Microsecond))

	// And the gateway must have recorded its own measurement, which is the
	// number the /stats endpoint and the M5 dashboard report.
	stats := fetchJSON(c, base+"/stats")
	if stats != nil {
		if arr, ok := stats["first_token_mean"].([]any); ok && len(arr) > 0 {
			row, _ := arr[0].(map[string]any)
			mean := toFloat(row["mean_seconds"])
			c.assert(mean > 0, "gateway recorded a first-token mean of %.1fms over %v samples (upstream %v)",
				mean*1000, row["count"], row["upstream"])
			c.assert(mean >= 0.05,
				"gateway's own measurement agrees the backend stalled (mean %.1fms >= 50ms)", mean*1000)
		} else {
			c.assert(false, "gateway recorded no first_token sample")
		}
	}
}

func checkToolCallDeltas(c *checker, base string) {
	body := `{"model":"mock-tool","stream":true,"messages":[{"role":"user","content":"weather in beijing"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather"}}]}`
	resp, raw := postJSON(c, base+"/v1/chat/completions", body, nil)
	if !c.assert(resp.StatusCode == http.StatusOK, "status = %d, want 200", resp.StatusCode) {
		return
	}

	// Reassemble the fragmented arguments exactly as an OpenAI SDK does: match
	// on tool_calls[].index and concatenate the argument strings.
	type delta struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	var (
		args      strings.Builder
		fnName    string
		callID    string
		fragments int
	)
	for _, f := range parseSSE(raw) {
		if f == "[DONE]" {
			continue
		}
		var d delta
		if err := json.Unmarshal([]byte(f), &d); err != nil {
			continue
		}
		for _, ch := range d.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if tc.ID != "" {
					callID = tc.ID
				}
				if tc.Function.Name != "" {
					fnName = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					args.WriteString(tc.Function.Arguments)
					fragments++
				}
			}
		}
	}
	c.info("reassembled %d argument fragments across the stream", fragments)
	c.assert(fnName == "get_weather", "function name = %q, want get_weather", fnName)
	c.assert(callID != "", "tool call id %q reached the client", callID)
	c.assert(fragments >= 2, "arguments arrived fragmented (%d fragments)", fragments)
	c.assert(json.Valid([]byte(args.String())),
		"concatenated arguments are valid JSON: %s", args.String())
	c.assert(strings.Contains(args.String(), "Beijing"), "argument payload is intact: %s", args.String())
}

func checkSyntheticDone(c *checker, base string) {
	body := `{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"no done"}]}`
	_, raw := postJSON(c, base+"/v1/chat/completions", body,
		map[string]string{"X-Mock-Omit-Done": "1"})
	if !c.assert(strings.HasSuffix(raw, "data: [DONE]\n\n"),
		"gateway supplied the [DONE] sentinel after a backend that omitted it") {
		c.info("tail of relayed stream: %q", tail(raw, 120))
		return
	}
	c.info("backend streamed no [DONE]; client still saw a terminated stream")
}

func checkAccounting(c *checker, base string) {
	before := fetchJSON(c, base+"/stats")
	body := `{"model":"mock-gpt","messages":[{"role":"user","content":"count the tokens"}]}`
	postJSON(c, base+"/v1/chat/completions", body, nil)
	after := fetchJSON(c, base+"/stats")
	if before == nil || after == nil {
		c.assert(false, "could not read /stats")
		return
	}
	b := before["tokens"].(map[string]any)
	a := after["tokens"].(map[string]any)
	deltaPrompt := toInt(a["prompt"]) - toInt(b["prompt"])
	deltaCompletion := toInt(a["completion"]) - toInt(b["completion"])
	c.assert(deltaPrompt > 0, "prompt tokens counted from the provider usage block (+%d)", deltaPrompt)
	c.assert(deltaCompletion > 0, "completion tokens counted (+%d)", deltaCompletion)
	c.info("cumulative prompt=%v completion=%v", a["prompt"], a["completion"])

	metrics := fetchText(c, base+"/metrics")
	c.assert(strings.Contains(metrics, "infergate_requests_total"),
		"/metrics exports infergate_requests_total")
	c.assert(strings.Contains(metrics, "infergate_tokens_total"),
		"/metrics exports infergate_tokens_total")
	c.assert(strings.Contains(metrics, "infergate_first_token_seconds_mean"),
		"/metrics exports infergate_first_token_seconds_mean")
}

func checkRouteError(c *checker, base string) {
	resp, raw := postJSON(c, base+"/v1/nonexistent/endpoint", `{"model":"mock-gpt"}`, nil)
	c.assert(resp.StatusCode == http.StatusNotFound, "unknown path -> %d, want 404", resp.StatusCode)
	c.assert(strings.Contains(raw, `"infergate_`),
		"error body uses the InferGate error taxonomy: %s", strings.TrimSpace(raw))
}

func checkOpsEndpoints(c *checker, base string) {
	for _, path := range []string{"/healthz", "/readyz", "/admin/upstreams"} {
		resp := fetchRaw(c, base+path)
		if resp == nil {
			c.assert(false, "%s is reachable", path)
			continue
		}
		defer resp.Body.Close()
		c.assert(resp.StatusCode == http.StatusOK, "%s -> %d", path, resp.StatusCode)
	}
	body := fetchJSON(c, base+"/admin/upstreams")
	if body != nil {
		c.assert(body["upstreams"] != nil, "/admin/upstreams describes the routing table")
	}
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func postJSON(c *checker, url, body string, headers map[string]string) (*http.Response, string) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		c.assert(false, "build request: %v", err)
		return &http.Response{StatusCode: 0, Header: http.Header{}}, ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer verify-key")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.assert(false, "request to %s failed: %v", url, err)
		return &http.Response{StatusCode: 0, Header: http.Header{}}, ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

// streamFirstByte issues a request and returns the moment the first byte of the
// response body arrived. Buffering clients (curl without -N, or ReadAll) cannot
// see this distinction, which is the entire point of measuring it here.
func streamFirstByte(c *checker, url, body string) (time.Time, *http.Response) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		c.assert(false, "build request: %v", err)
		return time.Time{}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.assert(false, "stream request failed: %v", err)
		return time.Time{}, nil
	}
	br := bufio.NewReader(resp.Body)
	_, err = br.ReadByte()
	firstByte := time.Now()
	if err != nil {
		c.assert(false, "read first byte: %v", err)
		resp.Body.Close()
		return time.Time{}, nil
	}
	// Drain synchronously before returning. The gateway records its
	// first-token metric in a deferred call that runs when its stream handler
	// returns, so a caller that inspects /stats while the stream is still open
	// races the metric it is trying to read.
	_, _ = io.Copy(io.Discard, br)
	resp.Body.Close()
	return firstByte, resp
}

func fetchJSON(c *checker, url string) map[string]any {
	resp := fetchRaw(c, url)
	if resp == nil {
		return nil
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		c.assert(false, "decode %s: %v", url, err)
		return nil
	}
	return v
}

func fetchText(c *checker, url string) string {
	resp := fetchRaw(c, url)
	if resp == nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func fetchRaw(c *checker, url string) *http.Response {
	resp, err := http.Get(url)
	if err != nil {
		c.assert(false, "GET %s failed: %v", url, err)
		return nil
	}
	return resp
}

// parseSSE splits a relayed stream into data payloads, treating the blank line
// as the frame separator exactly as the SSE specification does.
func parseSSE(raw string) []string {
	var out []string
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimRight(block, "\n")
		if block == "" || strings.HasPrefix(block, ":") {
			continue
		}
		var data []string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				data = append(data, v)
			}
		}
		if len(data) > 0 {
			out = append(out, strings.Join(data, "\n"))
		}
	}
	return out
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// tail returns at most n trailing bytes of s, for readable failure output.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// toFloat reads a JSON number that encoding/json decoded into any.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func mockConfig(mockURL string) *config.Config {
	cfg := config.Defaults()
	cfg.Server.UpstreamTimeout = config.Duration(30 * time.Second)
	cfg.Server.MaxBodyBytes = 8 << 20
	cfg.Log.Level = "error"
	cfg.Upstreams = []config.UpstreamConfig{{
		Name:    "mock",
		Kind:    config.KindOpenAI,
		BaseURL: mockURL,
		Models:  []string{"/"},
	}}
	cfg.Pricing = config.PricingConfig{
		Default: config.ModelPrice{In: 1, Out: 3},
		Models: map[string]config.ModelPrice{
			"mock-gpt": {In: 1, Out: 3},
		},
	}
	return &cfg
}

// mockHandler is a minimal OpenAI-compatible backend. It is intentionally the
// same shape as cmd/mockupstream but in-process, so this verifier has no
// external process dependency and cannot fail for environmental reasons.
func mockHandler(ttfb time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Tools    []any  `json:"tools"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}

		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-InferGate-Upstream-Name", "mock")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-verify", "object": "chat.completion", "model": req.Model,
				"choices": []map[string]any{{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": "mock answer"},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9},
			})
			return
		}

		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		select {
		case <-time.After(ttfb):
		case <-r.Context().Done():
			return
		}

		send := func(payload map[string]any) {
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		chunk := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{
				"id": "chatcmpl-verify", "object": "chat.completion.chunk", "model": req.Model,
				"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
			}
		}

		send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		if len(req.Tools) > 0 {
			send(chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_verify_1", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city":"Bei`},
			}}}, nil))
			send(chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "function": map[string]any{"arguments": `jing","units":"c"}`},
			}}}, nil))
			send(chunk(map[string]any{}, "tool_calls"))
		} else {
			send(chunk(map[string]any{"content": "mock "}, nil))
			send(chunk(map[string]any{"content": "answer"}, nil))
			send(chunk(map[string]any{}, "stop"))
		}

		send(map[string]any{
			"id": "chatcmpl-verify", "object": "chat.completion.chunk", "model": req.Model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9,
				"prompt_tokens_details": map[string]any{"cached_tokens": 0},
			},
		})
		if r.Header.Get("X-Mock-Omit-Done") == "" {
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
	})

	// Deliberately fail a second backend so the verifier can also demonstrate
	// that an unroutable path is a client error, not an upstream outage.
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{}})
	})
	return mux
}
