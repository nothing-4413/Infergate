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
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
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
	// Scripted mode. Empty (the default) leaves this binary byte-for-byte what
	// the M0-M5 gates curl; a path turns it into a deterministic tool-calling
	// model so an M6 agent run can be exercised offline.
	scriptPath := flag.String("script", "", "path to a JSON script file that makes responses deterministic (empty = today's behaviour)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var sc *script
	if *scriptPath != "" {
		loaded, err := loadScript(*scriptPath)
		if err != nil {
			// Refusing to start is the point: a mock that serves the wrong
			// answer because its script was unreadable would turn an agent-run
			// failure into a mystery instead of a startup error.
			logger.Error("scripted mode unavailable", "err", err)
			os.Exit(2)
		}
		sc = loaded
		logger.Info("scripted mode enabled", "script", *scriptPath, "rules", len(sc.Rules), "has_default", sc.Default != nil)
	}

	srv := &server{name: *name, ttfb: *ttfb, tokenDelay: *tokenDelay, log: logger, script: sc}

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
	// The counter endpoint. It is how a gate proves an idempotent replay caused
	// ZERO extra upstream calls: nothing else can witness a request that was
	// answered and thrown away.
	mux.HandleFunc("/calls", srv.callsHandler)
	mux.HandleFunc("/stats/calls", srv.statsCallsHandler)

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
	// script is nil unless -script was given, in which case the chat path
	// consults it before answering. Everything else in this file is unaware of
	// scripted mode.
	script *script
	// mux is the route table this server is mounted on. Tests build one and
	// drive the real handlers through it instead of starting the process.
	mux *http.ServeMux

	// Counters behind GET /calls. They are updated once per chat request, after
	// the response is written, so the number is "requests really handled".
	callCount        atomic.Int64
	turn             atomic.Int64
	toolCallsEmitted atomic.Int64
	dropped          atomic.Int64
	failed           atomic.Int64
	byPath           countSink
	byRule           countSink
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

// countOpts identifies which chat request is being answered. It is filled in
// before the response starts so the counter can be attributed to the script
// rule (or the built-in default) that produced the answer.
type countOpts struct {
	path string
	rule string
}

// countingWriter wraps the response writer for chat requests and updates the
// /calls counters once the response has been produced. Doing this in one place
// instead of at the end of each response path is what makes the counters
// trustworthy: a gate uses them to prove that an idempotent replay caused zero
// extra upstream calls, so a path that forgot to count would be a false pass.
type countingWriter struct {
	http.ResponseWriter
	server   *server
	opts     countOpts
	code     int
	written  bool
	dropped  bool
	toolCall int
}

// wantsDrop lets answerScripted mark a hijacked connection as a deliberate
// drop; without the flag a drop would be indistinguishable from a handler that
// simply wrote nothing.
func (w *countingWriter) wantsDrop() { w.dropped = true }

// countToolCalls records how many tool calls the answer carried, which is what
// a gate asserts stayed at zero across an idempotent replay.
func (w *countingWriter) countToolCalls(n int) { w.toolCall += n }

func (w *countingWriter) WriteHeader(status int) {
	if !w.written {
		w.written = true
		w.code = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.written = true
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps Server-Sent Events streaming through the wrapper: without it the
// flushed frames would buffer and the stream would stop being incremental.
func (w *countingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack keeps the drop path able to abandon the connection instead of
// answering with a status.
func (w *countingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// record is called with the writer's final state; it counts one served chat
// request (and, when the script emitted tool calls, those too).
func (w *countingWriter) record() {
	// A chat request that produced nothing at all is not a served request.
	if !w.written && !w.dropped {
		return
	}
	failed := w.written && w.code >= http.StatusBadRequest
	w.server.record(w.opts.path, w.opts.rule, w.toolCall, w.dropped, failed)
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

	// The timing headers are read first, before a response shape is chosen, so
	// that an unreadable value is a 400 naming the header instead of a silent
	// "no delay". That distinction is the point: an injected stall is how a gate
	// creates the window it then measures, and a mock that answers an unreadable
	// "300" (rather than "300ms") in microseconds would leave the gate measuring
	// a provider that was never slowed. A malformed request is answered by name
	// even when it also carries X-Mock-Status; internal/mockbackend orders the
	// same two checks the same way. X-Mock-TTFB used to be parsed after the SSE
	// headers had been committed, where a 400 is no longer possible.
	delay, err := durationHeader(r, "X-Mock-Delay")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody(err.Error(), "invalid_request_error"))
		return
	}
	ttfb, err := durationHeader(r, "X-Mock-TTFB")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody(err.Error(), "invalid_request_error"))
		return
	}

	// Fault injection, driven by headers so a client can exercise error paths
	// without recompiling the mock. This is what M1's failover tests point at.
	// An injected status is answered immediately, before the delay below: the
	// delay is there to widen a window around a real answer, while a status IS
	// the answer. (internal/mockbackend deliberately does the opposite with its
	// ttfb stall, because there the slow failure itself is what is measured.)
	if v := r.Header.Get("X-Mock-Status"); v != "" {
		code, err := strconv.Atoi(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("X-Mock-Status must be an integer", "invalid_request_error"))
			return
		}
		writeJSON(w, code, errorBody(fmt.Sprintf("injected status %d", code), "mock_error"))
		return
	}
	if delay > 0 {
		time.Sleep(delay)
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

	// Scripted mode, when enabled, picks the answer for this request. Selection
	// happens after the log line above so the log keeps the same shape as the
	// non-scripted one, and after the counting writer exists so a scripted
	// answer is counted exactly like a built-in one.
	var match *scriptMatch
	if s.script != nil {
		m := s.script.selectResponse(req, req.Stream, r.URL.Path, s.nextTurn())
		match = &m
	}

	// Every chat answer -- scripted or built in -- is served through a counting
	// writer so the /calls counters are exact by construction: the counter is
	// incremented once the response is actually written, on every return path.
	// Incrementing inside each response path instead would need a matching
	// increment in six places, and the one that gets forgotten would make an
	// idempotent replay look as if it had really called upstream.
	opts := countOpts{path: r.URL.Path}
	if match != nil {
		opts.rule = match.RuleName
		if match.Rule != nil {
			opts.rule = match.Rule.Name
		}
	}
	cw := &countingWriter{ResponseWriter: w, server: s, opts: opts}
	// Counting from a defer means the /calls numbers are derived from what was
	// actually written, so a new return path cannot silently stop counting.
	defer cw.record()

	// A scripted response is written here. A legacy match (script present but no
	// rule and no default) leaves match.Respond nil and falls through to the
	// built-in answer below, which keeps today's content byte-for-byte. So does
	// the success path of a scripted answer: answering "how" is this function's
	// job, not answerScripted's, which only handles failure and drop.
	if match != nil && !match.Legacy {
		m := *match
		s.log.Info("script match", "rule", m.RuleName, "turn", m.Turn, "legacy_default", m.Legacy)
		if s.answerScripted(cw, r, req, m) {
			return
		}
	}

	if req.Stream {
		s.streamChat(cw, r, req, match, ttfb)
	} else {
		s.wholeChat(cw, req, match)
	}
}

// wholeChat answers with a single JSON completion, the non-streaming shape.
// match is the scripted response to honour, or nil in today's behaviour.
func (s *server) wholeChat(w http.ResponseWriter, req chatRequest, match *scriptMatch) {
	var content string
	var calls []map[string]any
	finishReason := "stop"
	usage := map[string]any{
		"prompt_tokens":     tokenCount(req),
		"completion_tokens": len(strings.Fields(completionFor(req))),
		"total_tokens":      tokenCount(req) + len(strings.Fields(completionFor(req))),
	}
	if match != nil && !match.Legacy {
		content, calls, finishReason = match.respond(req)
		usage = match.usageFor(req, content, len(calls))
	} else {
		content = completionFor(req)
	}

	message := map[string]any{"role": "assistant", "content": content}
	if len(calls) > 0 {
		message["tool_calls"] = calls
		if c, ok := w.(interface{ countToolCalls(int) }); ok {
			c.countToolCalls(len(calls))
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-mock-1",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		// usage on the non-streaming path is the whole point of M3: the gateway
		// must read the provider's numbers rather than estimate from text.
		"usage": usage,
	})
}

// streamChat writes a real SSE response: one flushed chunk per token-sized
// delta, then a usage frame, then the [DONE] sentinel. match is the scripted
// response to honour, or nil in today's behaviour. ttfb is the caller-supplied
// stall, already parsed by chat: it is passed in rather than read again here
// because by this point the SSE headers are committed and a bad value could no
// longer be answered with a 400.
func (s *server) streamChat(w http.ResponseWriter, r *http.Request, req chatRequest, match *scriptMatch, ttfb time.Duration) {
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
	// A caller-supplied stall is how the streaming timeout path is tested. It was
	// parsed in chat, before the header above went out: an unreadable value is a
	// 400 there, because answering it with "no stall" here would be the silent
	// no-op this mock must not have.
	if ttfb > 0 {
		select {
		case <-time.After(ttfb):
		case <-r.Context().Done():
			return
		}
	}

	send := chunkSender(w, flusher, req.Model, "chatcmpl-mock-stream")

	// Tool-call mode: the argument arrives fragmented across frames, which is
	// the case that breaks naive proxies. A client must accumulate
	// choices[0].delta.tool_calls[0].function.arguments by index.
	//
	// Only the non-scripted path uses the fragmented shape: a script declares
	// whole tool calls, and those are emitted in one opening frame (see
	// streamScripted) because a scripted call is meant to be read back verbatim
	// by the agent runtime, not to stress a proxy's buffering.
	if match == nil && len(req.Tools) > 0 {
		s.streamToolCall(w, r, req, send)
		return
	}

	if match != nil && !match.Legacy {
		s.streamScripted(w, r, req, *match, send)
		return
	}

	// Opening role frame with EMPTY content, exactly as OpenAI emits it. The
	// gateway must not treat this as the first token: it arrives before the
	// model has produced anything, so counting it would report queueing time as
	// generation latency.
	if !send(map[string]any{"role": "assistant", "content": ""}, nil) {
		return
	}

	content := completionFor(req)
	for _, word := range strings.Fields(content) {
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
	prompt := tokenCount(req)
	completion := len(strings.Fields(content))
	writeUsageAndDone(w, r, req.Model, "chatcmpl-mock-stream", map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 0,
		},
	})
}

// streamScripted writes the SSE form of a scripted response.
//
// The frame shape is deliberately identical to the non-scripted one, because
// the gateway's stream handling must not need a second code path for a scripted
// upstream:
//
//	data: {...,"choices":[{"index":0,"delta":{"role":"assistant","content":"",
//	       "tool_calls":[{...whole call...}]},"finish_reason":null}]}   <- opening
//	data: {...,"delta":{"content":"<word> "}}                          <- one per word
//	data: {...,"delta":{},"finish_reason":"tool_calls"|"stop"}          <- finish
//	data: {...,"choices":[],"usage":{...}}                              <- usage
//	data: [DONE]
//
// A scripted tool call is emitted WHOLE in the opening frame's delta
// (id/type/function.name/function.arguments all at once) rather than fragmented
// the way the non-scripted tool-call path emits it. The script declares a
// complete call, and the agent runtime is supposed to read it back verbatim; a
// scripted stream exists to prove the runtime's tool loop works, not to prove a
// proxy can reassemble fragments (the non-scripted path already proves that).
func (s *server) streamScripted(w http.ResponseWriter, r *http.Request, req chatRequest, match scriptMatch, send func(map[string]any, any) bool) {
	content, calls, finishReason := match.respond(req)

	opening := map[string]any{"role": "assistant", "content": ""}
	if len(calls) > 0 {
		opening["tool_calls"] = calls
		if c, ok := w.(interface{ countToolCalls(int) }); ok {
			c.countToolCalls(len(calls))
		}
	}
	if !send(opening, nil) {
		return
	}

	for _, word := range strings.Fields(content) {
		select {
		case <-r.Context().Done():
			s.log.Info("client disconnected mid-stream")
			return
		default:
		}
		if !send(map[string]any{"content": word + " "}, nil) {
			return
		}
		if s.tokenDelay > 0 {
			time.Sleep(s.tokenDelay)
		}
	}

	if !send(map[string]any{}, finishReason) {
		return
	}
	writeUsageAndDone(w, r, req.Model, "chatcmpl-mock-stream", match.usageFor(req, content, len(calls)))
}

// writeUsageAndDone writes the tail of every SSE answer: the usage frame, then
// the [DONE] sentinel unless the caller asked for a stream that truncates.
//
// Both the scripted and the non-scripted stream share it so the two paths cannot
// drift apart: a gateway that handles one must handle the other identically,
// which is the whole reason scripted mode reuses today's frame shape.
func writeUsageAndDone(w http.ResponseWriter, r *http.Request, model, id string, usage map[string]any) {
	flusher, _ := w.(http.Flusher)
	if payload, err := json.Marshal(chunkUsageFrame(model, id, usage)); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// A mock asked to omit [DONE] reproduces a real integration hazard: some
	// OpenAI-compatible servers close the stream without the sentinel and naive
	// clients hang until their own timeout. The gateway must synthesise it.
	if r.Header.Get("X-Mock-Omit-Done") != "" {
		return
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// chunkSender builds the closure that writes one chat.completion.chunk frame.
func chunkSender(w http.ResponseWriter, flusher http.Flusher, model, id string) func(map[string]any, any) bool {
	return func(delta map[string]any, finish any) bool {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
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
}

// chunkUsageFrame is the tail frame that carries usage with an empty choices
// list, which is where the real API puts it.
func chunkUsageFrame(model, id string, usage map[string]any) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{},
		"usage":   usage,
	}
}

// streamToolCall emits a fragmented tool call. Arguments are split mid-JSON on
// purpose: a client that parses each delta independently sees invalid JSON and
// must buffer by index instead.
func (s *server) streamToolCall(w http.ResponseWriter, r *http.Request, req chatRequest, send func(map[string]any, any) bool) {
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
			return
		default:
		}
		if !send(delta, nil) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !send(map[string]any{}, "tool_calls") {
		return
	}
	if r.Header.Get("X-Mock-Omit-Done") == "" {
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// completionFor produces a deterministic answer that depends on the question,
// so a test can assert causality rather than just "some text came back".
func completionFor(req chatRequest) string {
	last := lastUserContent(req.Messages)
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

// durationHeader parses one of the mock's timing headers. An unreadable or
// negative value is an error rather than a zero, because "no delay" is a
// perfectly plausible reading of an ignored header: the request would succeed,
// the gate would pass, and the only thing that never happened is the effect the
// gate meant to inject.
func durationHeader(r *http.Request, name string) (time.Duration, error) {
	v := r.Header.Get(name)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as \"250ms\": %q", name, v)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must not be negative: %q", name, v)
	}
	return d, nil
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
