// Command mockupstream is a local, dependency-free OpenAI-compatible backend.
//
// Why it exists: InferGate's M0 exit criterion is "a real SSE stream reaches a
// real client". Verifying that against a paid provider makes the test slow,
// expensive, non-deterministic and impossible to run in CI. This mock is a real
// net/http server speaking the real wire protocol — chunked SSE frames, a
// provider-style usage block, fragmented tool_call deltas — so the gateway is
// exercised by genuine HTTP rather than by a mock RoundTripper.
//
// It is also the reference for M1: every upstream behaviour the router has to
// survive (slow first byte, 429, 500, a stream that truncates without [DONE])
// is reachable here through request headers, which is what makes failover and
// circuit-breaking testable without a provider account.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	listen := flag.String("listen", ":9000", "address to listen on")
	name := flag.String("name", "mock", "upstream name reported in /healthz, to identify which replica answered")
	ttfb := flag.Duration("ttfb", 0, "artificial delay before the first stream frame (exercises time-to-first-token)")
	// Inter-token pacing. The default makes generation latency observable by eye
	// and by the acceptance script; set it to 0 for load tests, where a 15ms
	// sleep per token would measure the mock rather than the gateway.
	tokenDelay := flag.Duration("token-delay", 15*time.Millisecond, "delay between streamed content frames (0 for load tests)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv := &server{name: *name, ttfb: *ttfb, tokenDelay: *tokenDelay, log: logger}

	mux := http.NewServeMux()
	// Both path shapes are registered on purpose. When a gateway fronts this
	// server it forwards the caller's path, which is usually "/v1/..."; when a
	// client talks to it directly the same path applies. Registering both means
	// the mock works in either position without configuration.
	mux.HandleFunc("/healthz", srv.healthz)
	mux.HandleFunc("/models", srv.models)
	mux.HandleFunc("/v1/models", srv.models)
	mux.HandleFunc("/chat/completions", srv.chat)
	mux.HandleFunc("/v1/chat/completions", srv.chat)
	mux.HandleFunc("/embeddings", srv.embeddings)
	mux.HandleFunc("/v1/embeddings", srv.embeddings)

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cut off a deliberately slow stream.
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		logger.Info("shutting down")
		_ = httpSrv.Close()
	}()

	logger.Info("mockupstream listening", "addr", *listen, "name", *name)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("listen failed", "err", err)
		os.Exit(1)
	}
}

type server struct {
	name       string
	ttfb       time.Duration
	tokenDelay time.Duration
	log        *slog.Logger
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "upstream": s.name})
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "mock-gpt", "object": "model", "owned_by": "infergate-mock"},
			{"id": "mock-reasoner", "object": "model", "owned_by": "infergate-mock"},
			{"id": "mock-tool", "object": "model", "owned_by": "infergate-mock"},
		},
	})
}

func (s *server) embeddings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"model":  "mock-embed",
		"data": []map[string]any{
			{"object": "embedding", "index": 0, "embedding": []float64{0.01, -0.02, 0.03}},
		},
		"usage": map[string]any{"prompt_tokens": 3, "total_tokens": 3},
	})
}

// chatRequest is the subset of the OpenAI chat body the mock reasons about.
type chatRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages []chatMessage   `json:"messages"`
	Tools    json.RawMessage `json:"tools"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method not allowed", "invalid_request_error"))
		return
	}

	// Fault injection, driven by headers so a client can exercise error paths
	// without recompiling the mock. This is what M1's failover tests point at.
	if v := r.Header.Get("X-Mock-Status"); v != "" {
		code, err := strconv.Atoi(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("X-Mock-Status must be an integer", "invalid_request_error"))
			return
		}
		writeJSON(w, code, errorBody(fmt.Sprintf("injected status %d", code), "mock_error"))
		return
	}
	if d := r.Header.Get("X-Mock-Delay"); d != "" {
		if dur, err := time.ParseDuration(d); err == nil {
			time.Sleep(dur)
		}
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid JSON body: "+err.Error(), "invalid_request_error"))
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("you must provide a model parameter", "invalid_request_error"))
		return
	}

	s.log.Info("chat request",
		"model", req.Model, "stream", req.Stream, "messages", len(req.Messages),
		"tools", len(req.Tools) > 0, "path", r.URL.Path)

	if req.Stream {
		s.streamChat(w, r, req)
		return
	}
	s.wholeChat(w, req)
}

// wholeChat answers with a single JSON completion, the non-streaming shape.
func (s *server) wholeChat(w http.ResponseWriter, req chatRequest) {
	completion := completionFor(req)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-mock-1",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": completion},
			"finish_reason": "stop",
		}},
		// usage on the non-streaming path is the whole point of M3: the gateway
		// must read the provider's numbers rather than estimate from text.
		"usage": map[string]any{
			"prompt_tokens":     tokenCount(req),
			"completion_tokens": len(strings.Fields(completion)),
			"total_tokens":      tokenCount(req) + len(strings.Fields(completion)),
		},
	})
}

// streamChat writes a real SSE response: one flushed chunk per token-sized
// delta, then a usage frame, then the [DONE] sentinel.
func (s *server) streamChat(w http.ResponseWriter, r *http.Request, req chatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Mock-Upstream", s.name)

	// The stall happens BEFORE the header is committed and flushed.
	//
	// This ordering is the whole point of the mock. If the response headers go
	// out first, the gateway forwards them immediately and every client
	// measurement of "time to first byte" reads ~2ms even though no token has
	// arrived — the stall becomes unobservable and the check silently proves
	// nothing. Holding the header back until the first token is what real
	// providers do and what makes time-to-first-token measurable.
	if s.ttfb > 0 {
		select {
		case <-time.After(s.ttfb):
		case <-r.Context().Done():
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	// A caller-supplied stall is how the streaming timeout path is tested.
	if d := r.Header.Get("X-Mock-TTFB"); d != "" {
		if dur, err := time.ParseDuration(d); err == nil {
			select {
			case <-time.After(dur):
			case <-r.Context().Done():
				return
			}
		}
	}

	id := "chatcmpl-mock-stream"
	send := func(delta map[string]any, finish any) bool {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		payload, err := json.Marshal(chunk)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Opening role frame with EMPTY content, exactly as OpenAI emits it. The
	// gateway must not treat this as the first token: it arrives before the
	// model has produced anything, so counting it would report queueing time as
	// generation latency.
	if !send(map[string]any{"role": "assistant", "content": ""}, nil) {
		return
	}

	// Tool-call mode: the argument arrives fragmented across frames, which is
	// the case that breaks naive proxies. A client must accumulate
	// choices[0].delta.tool_calls[0].function.arguments by index.
	if len(req.Tools) > 0 {
		if s.streamToolCall(w, r, req, send) {
			return
		}
		return
	}

	for _, word := range strings.Fields(completionFor(req)) {
		select {
		case <-r.Context().Done():
			// The client vanished. A well-behaved backend stops generating here
			// rather than burning tokens nobody will read.
			s.log.Info("client disconnected mid-stream")
			return
		default:
		}
		if !send(map[string]any{"content": word + " "}, nil) {
			return
		}
		// Configurable pacing: a fixed 15ms here would cap streaming throughput
		// at the mock's own speed instead of the gateway's, making the load test
		// measure the wrong process.
		if s.tokenDelay > 0 {
			time.Sleep(s.tokenDelay)
		}
	}

	if !send(map[string]any{}, "stop") {
		return
	}

	// Usage arrives only in the final frame on the real API. Accounting that
	// ignores the tail of the stream therefore reports zero cost, which is the
	// bug this frame exists to catch.
	usage := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     tokenCount(req),
			"completion_tokens": len(strings.Fields(completionFor(req))),
			"total_tokens":      tokenCount(req) + len(strings.Fields(completionFor(req))),
			"prompt_tokens_details": map[string]any{
				"cached_tokens": 0,
			},
		},
	}
	if payload, err := json.Marshal(usage); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}

	// A mock asked to omit [DONE] reproduces a real integration hazard: some
	// OpenAI-compatible servers close the stream without the sentinel and naive
	// clients hang until their own timeout. The gateway must synthesise it.
	if r.Header.Get("X-Mock-Omit-Done") == "" {
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

// streamToolCall emits a fragmented tool call. Arguments are split mid-JSON on
// purpose: a client that parses each delta independently sees invalid JSON and
// must buffer by index instead.
func (s *server) streamToolCall(w http.ResponseWriter, r *http.Request, req chatRequest, send func(map[string]any, any) bool) bool {
	id := "call_mock_1"
	fragments := []string{`{"city":"Bei`, `jing","units":"met`, `ric"}`}
	frames := []map[string]any{
		{"tool_calls": []map[string]any{{
			"index": 0,
			"id":    id,
			"type":  "function",
			"function": map[string]any{
				"name":      "get_weather",
				"arguments": fragments[0],
			},
		}}},
		{"tool_calls": []map[string]any{{
			"index":    0,
			"function": map[string]any{"arguments": fragments[1]},
		}}},
		{"tool_calls": []map[string]any{{
			"index":    0,
			"function": map[string]any{"arguments": fragments[2]},
		}}},
	}
	for _, delta := range frames {
		select {
		case <-r.Context().Done():
			return true
		default:
		}
		if !send(delta, nil) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !send(map[string]any{}, "tool_calls") {
		return true
	}
	if r.Header.Get("X-Mock-Omit-Done") == "" {
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	return true
}

// completionFor produces a deterministic answer that depends on the question,
// so a test can assert causality rather than just "some text came back".
func completionFor(req chatRequest) string {
	last := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			last = m.Content
		}
	}
	if last == "" {
		last = "nothing"
	}
	return fmt.Sprintf("mock answer to %q", truncate(last, 120))
}

// tokenCount is a deliberately crude stand-in for a tokenizer. It only has to
// be deterministic and non-zero; M3 reconciles these numbers against the real
// provider's usage block.
func tokenCount(req chatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += len(strings.Fields(m.Content)) + 4
	}
	if n == 0 {
		n = 1
	}
	return n
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func errorBody(message, kind string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
			"param":   nil,
			"code":    nil,
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
