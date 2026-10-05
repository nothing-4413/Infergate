package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/upstream"
)

// ---------------------------------------------------------------------------
// Test harness
//
// The harness deliberately drives the proxy through httptest and a real
// upstream http.Server rather than mocking http.RoundTripper. Everything M0
// claims to do that is easy to get wrong — flushing, header handling, SSE
// framing, disconnect propagation, connection reuse — only exists in the
// net/http layer, so a RoundTripper stub would test the mock, not the gateway.
// ---------------------------------------------------------------------------

func newTestProxy(t *testing.T, cfg *config.Config, recorder metrics.Sink) *Proxy {
	t.Helper()
	reg, err := upstream.New(cfg)
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	t.Cleanup(reg.CloseIdleConnections)
	return New(Options{
		Upstreams:       reg,
		Pricing:         NewPriceBook(cfg.Pricing),
		Metrics:         recorder,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
		UpstreamTimeout: cfg.Server.UpstreamTimeout.Duration(),
	})
}

// backend is a scriptable OpenAI-compatible upstream.
//
// Requests are recorded under a mutex and the handler runs AFTER the lock is
// released. Holding a lock across the handler would deadlock on t.Cleanup:
// httptest.Server.Close waits for in-flight handlers, a handler waiting on the
// recorder's lock, and the test goroutine waiting on Close.
type backend struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	Path          string
	Authorization string
	Model         string
	RawBody       []byte
	Header        http.Header
}

func newBackend(t *testing.T, handler http.HandlerFunc) *backend {
	t.Helper()
	b := &backend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &fields)

		b.mu.Lock()
		b.requests = append(b.requests, recordedRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			Model:         fields.Model,
			RawBody:       body,
			Header:        r.Header.Clone(),
		})
		b.mu.Unlock()

		handler(w, r)
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *backend) recorded() []recordedRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]recordedRequest, len(b.requests))
	copy(out, b.requests)
	return out
}

// oneUpstreamConfig points a single catch-all upstream at addr.
func oneUpstreamConfig(addr string) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Listen:          ":0",
			UpstreamTimeout: config.Duration(30 * time.Second),
			MaxBodyBytes:    1 << 20,
		},
		Log: config.LogConfig{Level: "error", Format: "text"},
		Upstreams: []config.UpstreamConfig{{
			Name:    "test",
			Kind:    config.KindOpenAI,
			BaseURL: addr,
			APIKey:  "test-key",
			Models:  []string{"/"},
		}},
	}
}

func mustPost(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer caller-key")
	return serve(t, handler, req)
}

func serve(t *testing.T, handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

const chatRequestBody = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`

// ---------------------------------------------------------------------------
// Non-streaming
// ---------------------------------------------------------------------------

// TestPassthroughNonStreaming is the M0 headline test: bytes in, bytes out,
// with the caller's identity replaced by the upstream's credential.
func TestPassthroughNonStreaming(t *testing.T) {
	upstreamBody := `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o-2024-08-06",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`

	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "yes")
		_, _ = w.Write([]byte(upstreamBody))
	})

	rec := metrics.NewRecorder()
	p := newTestProxy(t, oneUpstreamConfig(be.URL), rec)

	recorder := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	if got := recorder.Body.String(); got != upstreamBody {
		t.Errorf("body = %q, want byte-identical upstream body %q", got, upstreamBody)
	}
	if recorder.Header().Get("X-Upstream-Marker") != "yes" {
		t.Error("upstream response header was not forwarded")
	}

	got := be.recorded()
	if len(got) != 1 {
		t.Fatalf("upstream request count = %d, want 1", len(got))
	}
	if got[0].Path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", got[0].Path)
	}
	if got[0].Authorization != "Bearer test-key" {
		t.Errorf("upstream Authorization = %q, want the configured backend credential", got[0].Authorization)
	}

	// The provider's concrete snapshot name ("gpt-4o-2024-08-06") is what gets
	// billed; accounting on the requested alias would silently misprice traffic.
	snap := rec.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("metric series = %d, want 1", len(snap))
	}
	if snap[0].Model != "gpt-4o-2024-08-06" {
		t.Errorf("recorded model = %q, want the provider-reported model", snap[0].Model)
	}
	if snap[0].Status != 200 || snap[0].Outcome != metrics.OutcomeSuccess {
		t.Errorf("recorded status/outcome = %d/%s, want 200/success", snap[0].Status, snap[0].Outcome)
	}
	prompt, completion, cached := rec.TokenTotals()
	if prompt != 11 || completion != 2 || cached != 0 {
		t.Errorf("tokens = %d/%d/%d, want 11/2/0", prompt, completion, cached)
	}
}

// TestCallerAuthorizationFallback documents the credential policy for a backend
// with no configured key (local vLLM, or an operator's own key).
func TestCallerAuthorizationFallback(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})

	cfg := oneUpstreamConfig(be.URL)
	cfg.Upstreams[0].APIKey = ""
	p := newTestProxy(t, cfg, metrics.Nop{})

	mustPost(t, p, "/v1/chat/completions", chatRequestBody)

	got := be.recorded()
	if len(got) != 1 || got[0].Authorization != "Bearer caller-key" {
		t.Fatalf("upstream Authorization = %q, want the caller's key passed through", got[0].Authorization)
	}
}

// TestModelRoutingSendsRequestToOwningUpstream is the core of M0's routing
// table: two providers, one requested model, and only the owner may see it.
func TestModelRoutingSendsRequestToOwningUpstream(t *testing.T) {
	var aHits, bHits int
	beA := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		aHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"a"}`))
	})
	beB := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		bHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"b"}`))
	})

	cfg := &config.Config{
		Server: config.ServerConfig{UpstreamTimeout: config.Duration(30 * time.Second), MaxBodyBytes: 1 << 20},
		Upstreams: []config.UpstreamConfig{
			{Name: "a", Kind: config.KindOpenAI, BaseURL: beA.URL + "/v1", Models: []string{"gpt-4o"}},
			{Name: "b", Kind: config.KindOpenAI, BaseURL: beB.URL + "/v1", Models: []string{"deepseek-chat"}},
		},
	}
	rec := metrics.NewRecorder()
	p := newTestProxy(t, cfg, rec)

	mustPost(t, p, "/v1/chat/completions", `{"model":"deepseek-chat","messages":[]}`)

	if aHits != 0 || bHits != 1 {
		t.Fatalf("hits a=%d b=%d, want a=0 b=1", aHits, bHits)
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Upstream != "b" {
		t.Fatalf("recorded upstream = %+v, want b", snap)
	}
}

// TestUnknownModelWithoutCatchAllReturns400 proves a typo is a client error,
// not a 500 and not a silent retry against a random provider.
//
// Two backends are configured deliberately. The single-backend case is
// ambiguous by design and absorbs any model name (see Registry.Resolve rule 4),
// so the "no upstream for this model" answer only exists once routing genuinely
// has a choice to make. The second backend is never contacted.
func TestUnknownModelWithoutCatchAllReturns400(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be contacted for an unroutable model")
	})
	cfg := &config.Config{
		Server: config.ServerConfig{UpstreamTimeout: config.Duration(time.Second), MaxBodyBytes: 1 << 20},
		Upstreams: []config.UpstreamConfig{
			{Name: "a", Kind: config.KindOpenAI, BaseURL: be.URL, Models: []string{"gpt-4o"}},
			{Name: "b", Kind: config.KindOpenAI, BaseURL: be.URL, Models: []string{"deepseek-chat"}},
		},
	}
	p := newTestProxy(t, cfg, metrics.Nop{})

	res := mustPost(t, p, "/v1/chat/completions", `{"model":"nonexistent","messages":[]}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Type != TypeNoUpstream {
		t.Errorf("error type = %q, want %q", body.Error.Type, TypeNoUpstream)
	}
}

// TestExplicitUpstreamHeaderOverridesModelRouting covers the operator escape
// hatch used to pin traffic during an incident.
func TestExplicitUpstreamHeaderOverridesModelRouting(t *testing.T) {
	var hits int
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	beOther := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})

	cfg := &config.Config{
		Server: config.ServerConfig{UpstreamTimeout: config.Duration(30 * time.Second), MaxBodyBytes: 1 << 20},
		Upstreams: []config.UpstreamConfig{
			{Name: "pinned", Kind: config.KindOpenAI, BaseURL: be.URL + "/v1", Models: []string{"/"}},
			{Name: "other", Kind: config.KindOpenAI, BaseURL: beOther.URL + "/v1", Models: []string{"gpt-4o"}},
		},
	}
	p := newTestProxy(t, cfg, metrics.Nop{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set(HeaderUpstream, "pinned")
	res := serve(t, p, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if hits != 1 {
		t.Errorf("pinned upstream hits = %d, want 1", hits)
	}
}

// TestUpstreamErrorIsForwardedVerbatim matters because OpenAI-compatible SDKs
// parse the provider's error envelope; re-wrapping it would break client
// retry logic and error messages.
func TestUpstreamErrorIsForwardedVerbatim(t *testing.T) {
	upstreamBody := `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(upstreamBody))
	})
	rec := metrics.NewRecorder()
	p := newTestProxy(t, oneUpstreamConfig(be.URL), rec)

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
	if res.Body.String() != upstreamBody {
		t.Errorf("body = %q, want the provider's error envelope verbatim", res.Body.String())
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Outcome != metrics.OutcomeBadRequest {
		t.Errorf("outcome = %+v, want bad_request (a 4xx is the caller's fault)", snap)
	}
}

// TestUpstreamDownReturns502 covers the connection-refused path.
func TestUpstreamDownReturns502(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {})
	addr := be.URL
	be.Close() // nothing is listening now

	cfg := oneUpstreamConfig(addr)
	p := newTestProxy(t, cfg, metrics.Nop{})

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.Code)
	}
}

// TestBodyLimitIsEnforced checks the memory guard on the inbound side.
func TestBodyLimitIsEnforced(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("oversized body must not reach the upstream")
	})
	cfg := oneUpstreamConfig(be.URL)
	cfg.Server.MaxBodyBytes = 128
	p := newTestProxy(t, cfg, metrics.Nop{})

	big := `{"model":"gpt-4o","messages":[{"role":"user","content":"` + strings.Repeat("x", 4096) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))
	res := serve(t, p, req)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", res.Code)
	}
}

// TestUnknownPathReturns404 keeps the gateway from becoming an open proxy: only
// the OpenAI surface is exposed.
func TestUnknownPathReturns404(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unrouted path must not reach the upstream")
	})
	p := newTestProxy(t, oneUpstreamConfig(be.URL), metrics.Nop{})

	req := httptest.NewRequest(http.MethodGet, "/admin/secrets", nil)
	res := serve(t, p, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}
}

// TestHopByHopHeadersAreStripped verifies RFC 7230 §6.1 compliance plus the
// gateway's own control headers, which must never leak upstream and confuse a
// second InferGate instance behind the first.
func TestHopByHopHeadersAreStripped(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Keep-Alive", "timeout=5")
		_, _ = w.Write([]byte(`{}`))
	})
	p := newTestProxy(t, oneUpstreamConfig(be.URL), metrics.Nop{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatRequestBody))
	req.Header.Set("Connection", "close, X-Custom-Hop")
	req.Header.Set("X-Custom-Hop", "leak-me")
	req.Header.Set("X-InferGate-Upstream", "test")
	req.Header.Set("X-Secret-Internal", "keep-me")
	serve(t, p, req)

	got := be.recorded()[0]
	if got.Header.Get("X-Custom-Hop") != "" {
		t.Error("a header named in Connection was forwarded upstream")
	}
	if got.Header.Get("X-InferGate-Upstream") != "" {
		t.Error("gateway control header leaked upstream")
	}
	if got.Header.Get("X-Secret-Internal") != "keep-me" {
		t.Error("an ordinary client header was dropped")
	}
	if res := got.Header.Get("Connection"); res == "close, X-Custom-Hop" {
		t.Error("client Connection header was forwarded verbatim")
	}
}

// TestRequestIDIsPropagated makes log correlation across hops possible.
func TestRequestIDIsPropagated(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	p := newTestProxy(t, oneUpstreamConfig(be.URL), metrics.Nop{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatRequestBody))
	req.Header.Set(HeaderRequestID, "trace-abc")
	serve(t, p, req)

	got := be.recorded()[0]
	if got.Header.Get("X-Request-Id") != "trace-abc" {
		t.Errorf("upstream X-Request-Id = %q, want trace-abc", got.Header.Get("X-Request-Id"))
	}
	// A generated id must also be present when the caller supplied none.
	be2 := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	p2 := newTestProxy(t, oneUpstreamConfig(be2.URL), metrics.Nop{})
	mustPost(t, p2, "/v1/chat/completions", chatRequestBody)
	if id := be2.recorded()[0].Header.Get("X-Request-Id"); id == "" {
		t.Error("no X-Request-Id was generated for a request that supplied none")
	}
}

// TestModelRewriteForSingleModelBackend covers the ergonomic case: the
// caller asks for a made-up alias, the backend serves exactly one model, and
// the gateway speaks the backend's real name on the wire.
func TestModelRewriteForSingleModelBackend(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	cfg := &config.Config{
		Server: config.ServerConfig{UpstreamTimeout: config.Duration(30 * time.Second), MaxBodyBytes: 1 << 20},
		Upstreams: []config.UpstreamConfig{
			{Name: "local", Kind: config.KindOpenAI, BaseURL: be.URL, Models: []string{"Qwen3-8B"}},
		},
	}
	p := newTestProxy(t, cfg, metrics.Nop{})

	res := mustPost(t, p, "/v1/chat/completions", `{"model":"whatever-the-agent-said","messages":[]}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	got := be.recorded()[0]
	if got.Model != "Qwen3-8B" {
		t.Errorf("upstream model = %q, want the backend's concrete model name", got.Model)
	}
	// The rewrite must not disturb the rest of the payload.
	if !strings.Contains(string(got.RawBody), `"messages":[]`) {
		t.Errorf("rewritten body lost fields: %s", got.RawBody)
	}
}

// TestCatchAllBackendPassesModelThrough is the counterpart: a "/"-only backend
// must see the client's model untouched, because the local server may host many
// models and knows better than the gateway which one to load.
func TestCatchAllBackendPassesModelThrough(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	p := newTestProxy(t, oneUpstreamConfig(be.URL), metrics.Nop{})

	mustPost(t, p, "/v1/chat/completions", `{"model":"qwen2.5:7b","messages":[]}`)
	if got := be.recorded()[0].Model; got != "qwen2.5:7b" {
		t.Errorf("upstream model = %q, want pass-through", got)
	}
}

// TestUpstreamTimeoutReturns504 pins the deadline behaviour that keeps a wedged
// provider from pinning gateway goroutines.
func TestUpstreamTimeoutReturns504(t *testing.T) {
	release := make(chan struct{})
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	cfg := oneUpstreamConfig(be.URL)
	cfg.Server.UpstreamTimeout = config.Duration(150 * time.Millisecond)
	rec := metrics.NewRecorder()
	p := newTestProxy(t, cfg, rec)

	start := time.Now()
	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	elapsed := time.Since(start)

	if res.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", res.Code)
	}
	if elapsed > 3*time.Second {
		t.Errorf("timeout took %v, want it bounded near 150ms", elapsed)
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Outcome != metrics.OutcomeTimeout {
		t.Errorf("outcome = %+v, want timeout", snap)
	}
}

// TestClientDisconnectCancelsUpstream proves a hung-up client does not keep a
// provider generating (and billing) tokens.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamDone := make(chan struct{})
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		close(upstreamDone)
	})

	p := newTestProxy(t, oneUpstreamConfig(be.URL), metrics.Nop{})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":true}`)).WithContext(ctx)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	serve(t, p, req)

	select {
	case <-upstreamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not cancelled after the client disconnected")
	}
}

// TestStatusFromUpstreamIsPreserved covers 5xx classification: a provider
// outage is the gateway's problem (upstream_error), not the caller's.
func TestStatusFromUpstreamIsPreserved(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	})
	rec := metrics.NewRecorder()
	p := newTestProxy(t, oneUpstreamConfig(be.URL), rec)

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.Code)
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Outcome != metrics.OutcomeUpstreamErr {
		t.Errorf("outcome = %+v, want upstream_error", snap)
	}
}
