// Package mockbackend is an in-process OpenAI-compatible backend used by the
// acceptance verifiers.
//
// It exists as a library rather than only as cmd/mockupstream so that a verifier
// can start several DIFFERENT backends in one process (a healthy one, a broken
// one, a slow one) and inspect exactly what each of them was asked. A verifier
// that needs two backends with different behaviour cannot use a single global
// mock binary without starting and configuring several child processes, and the
// per-backend request log is what proves routing and failover actually happened
// instead of inferring it from the response.
package mockbackend

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Fault-injection headers understood by every backend. They are per-request, so
// a single backend can be healthy for one call and broken for the next; that is
// what lets a test exercise a breaker's transition without restarting anything.
const (
	HeaderStatus  = "X-Mock-Status"   // reply with this HTTP status instead
	HeaderDelay   = "X-Mock-Delay"    // sleep this long before answering
	HeaderTTFB    = "X-Mock-TTFB"     // sleep this long before the first stream frame
	HeaderOmitEnd = "X-Mock-Omit-Done" // stream without the [DONE] sentinel
)

// Options configures one backend.
type Options struct {
	// Name is reported in the X-Mock-Upstream header and in /healthz, so a
	// verifier can tell which replica answered.
	Name string
	// TTFB delays the first stream frame by default. The header overrides it.
	TTFB time.Duration
	// TokenDelay paces streamed content frames. Zero means as fast as possible.
	TokenDelay time.Duration
	// FailStatus, when non-zero, makes every chat request fail with that status
	// unless the request carries an explicit X-Mock-Status. This is how a
	// permanently broken backend is modelled.
	FailStatus int
	// Methods, when true, appends an extra "methods" capability to the model
	// list; capability routing is configured by the gateway, not by the backend.
	Methods bool
}

// Call is one recorded request.
type Call struct {
	Path          string
	Model         string
	Stream        bool
	Authorization string
	Header        http.Header
}

// Backend is a running mock upstream plus its request log.
type Backend struct {
	*httptest.Server

	name  string
	ttfb  time.Duration
	delay time.Duration

	failState atomic.Int32
	stallFor  atomic.Int64

	mu    sync.Mutex
	calls []Call
}

// New starts a backend on an ephemeral port. Closing is the caller's job via the
// embedded *httptest.Server.
func New(opts Options) *Backend {
	if opts.Name == "" {
		opts.Name = "mock"
	}
	b := &Backend{name: opts.Name, ttfb: opts.TTFB, delay: opts.TokenDelay}
	b.failState.Store(int32(opts.FailStatus))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "upstream": b.name})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "mock-gpt", "object": "model"}},
		})
	})
	// The request log is exposed over HTTP so a child process cannot be the only
	// witness. Verifiers read it directly; a human can curl it while debugging.
	mux.HandleFunc("/calls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			b.Reset()
			writeJSON(w, http.StatusOK, map[string]any{"reset": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"calls": b.Calls(), "count": len(b.Calls())})
	})

	handler := func(w http.ResponseWriter, r *http.Request) {
		// The /v1 prefix is normalised away before dispatch. Matching on the
		// full path is how an earlier version of this mock answered 404 for
		// every request: the gateway forwards the caller's path verbatim, so a
		// client posting /v1/chat/completions arrives here as exactly that, not
		// as the bare route the mux pattern was written for.
		switch strings.TrimPrefix(r.URL.Path, "/v1") {
		case "/chat/completions":
			b.chat(w, r)
		case "/embeddings":
			writeJSON(w, http.StatusOK, map[string]any{
				"object": "list", "model": "mock-embed",
				"data":  []map[string]any{{"object": "embedding", "index": 0, "embedding": []float64{0.01, -0.02, 0.03}}},
				"usage": map[string]any{"prompt_tokens": 3, "total_tokens": 3},
			})
		default:
			http.NotFound(w, r)
		}
	}
	// Both path shapes are registered because the gateway forwards the caller's
	// path verbatim: a client posting /v1/chat/completions reaches the backend as
	// /v1/chat/completions, and a base_url that already ends in /v1 produces the
	// bare shape.
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/v1/embeddings", handler)

	b.Server = httptest.NewServer(mux)
	return b
}

// Calls returns a copy of the request log.
func (b *Backend) Calls() []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Call, len(b.calls))
	copy(out, b.calls)
	return out
}

// Count returns how many chat requests this backend has seen.
func (b *Backend) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// Reset clears the request log.
func (b *Backend) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = nil
}

// SetFailStatus changes the permanently-injected status at runtime, so one
// backend can be broken and then repaired inside a single test. The alternative
// -- restarting a backend to repair it -- would also reset its connection pool
// and its breaker window, which is the state under test.
func (b *Backend) SetFailStatus(status int) { b.failState.Store(int32(status)) }

// SetStall makes this backend slow at runtime, or fast again with zero.
//
// Runtime control is deliberate: restarting a backend to change its speed would
// also reset its connection pool and its breaker window, and the whole point of
// a timeout scenario is what the gateway does to state that already exists.
func (b *Backend) SetStall(d time.Duration) { b.stallFor.Store(int64(d)) }

// stall returns how long this backend should wait before answering. The
// runtime value wins over the configured one, and the per-request header wins
// over both so a single request can be slowed without disturbing the others.
func (b *Backend) stall(r *http.Request) time.Duration {
	if d := durationHeader(r, HeaderTTFB); d > 0 {
		return d
	}
	if d := time.Duration(b.stallFor.Load()); d > 0 {
		return d
	}
	return b.ttfb
}

func (b *Backend) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method not allowed", "invalid_request_error"))
		return
	}

	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []any  `json:"tools"`
	}
	body, _ := readAll(r)
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid JSON body: "+err.Error(), "invalid_request_error"))
		return
	}

	b.mu.Lock()
	b.calls = append(b.calls, Call{
		Path: r.URL.Path, Model: req.Model, Stream: req.Stream,
		Authorization: r.Header.Get("Authorization"), Header: r.Header.Clone(),
	})
	b.mu.Unlock()

	// The stall applies to EVERY answer, streaming or not, and happens before
	// either the injected status or the real response is written. Tying it to
	// the stream path only made a non-streaming "slow backend" answer in
	// microseconds, so an upstream-timeout scenario proved nothing at all: the
	// gateway never had a reason to give up on it.
	//
	// Sleeping before the injected status matters too -- a backend that fails
	// slowly is what makes failover cost measurable, and a failing-fast backend
	// hides that cost.
	if stall := b.stall(r); stall > 0 {
		time.Sleep(stall)
	}

	// A permanently broken backend still honours an explicit per-request status,
	// so one instance can serve "always 503" for the breaker test and "503 once"
	// for the failover test.
	if v := r.Header.Get(HeaderStatus); v != "" {
		code, err := strconv.Atoi(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("X-Mock-Status must be an integer", "invalid_request_error"))
			return
		}
		writeJSON(w, code, errorBody(fmt.Sprintf("injected status %d", code), "mock_error"))
		return
	}
	if b.failState.Load() != 0 {
		code := int(b.failState.Load())
		writeJSON(w, code, errorBody(fmt.Sprintf("%s is permanently failing with %d", b.name, code), "mock_error"))
		return
	}
	if d := durationHeader(r, HeaderDelay); d > 0 {
		time.Sleep(d)
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("you must provide a model parameter", "invalid_request_error"))
		return
	}

	if req.Stream {
		b.streamChat(w, r, req.Model)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion", "created": time.Now().Unix(),
		"model": req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "mock answer from " + b.name},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9},
	})
}

// streamChat writes a real SSE response: a role frame, two content frames, a
// finish frame, a usage frame and the sentinel.
func (b *Backend) streamChat(w http.ResponseWriter, r *http.Request, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set(HeaderUpstreamName, b.name)

	// No stall here: the delay is applied once, in chat(), before any header or
	// status is chosen. Sleeping in both places would double the configured
	// latency and make a time-to-first-token measurement depend on how many
	// times the backend happened to check the clock.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emit := func(payload map[string]any) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		if b.delay > 0 {
			time.Sleep(b.delay)
		}
	}
	emit(map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}}},
	})
	emit(map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "mock "}}},
	})
	emit(map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "answer"}}},
	})
	emit(map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	})
	emit(map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{},
		"usage":   map[string]any{"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9},
	})
	if r.Header.Get(HeaderOmitEnd) == "" {
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

// HeaderUpstreamName is the header a backend sets so a client can see which
// replica answered without reading the body.
const HeaderUpstreamName = "X-Mock-Upstream"

func durationHeader(r *http.Request, name string) time.Duration {
	v := r.Header.Get(name)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
}

func readAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errorBody(message, typ string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": typ, "param": nil, "code": nil}}
}
