package server

// M6 tests: the replay store, the session ledger and the capability surface.
//
// The end-to-end tests here drive the real handler through httptest rather than
// calling the handlers directly, because the interesting property is the one
// that crosses the proxy boundary: "the second identical request returned the
// remembered bytes and did not reach the provider" is a claim about the whole
// path, and a unit test of the store cannot support it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/infergate/infergate/internal/config"
)

// m6ChatBody is a minimal OpenAI-shaped request and response pair.
const m6ChatBody = `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"stream":false}`

func m6Response(model, answer string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`, model, answer)
}

// m6Upstream is a stub provider: it counts calls and answers chat completions,
// rejecting an image content part and embeddings so that a probe has something
// to reject.
type m6Upstream struct {
	server *httptest.Server
	count  int64
}

func newM6Upstream(t *testing.T) *m6Upstream {
	t.Helper()
	up := &m6Upstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&up.count, 1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		switch {
		case r.URL.Path == "/v1/embeddings":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"no embeddings here","type":"mock"}}`))
		case bytes.Contains(body, []byte("image_url")):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"vision is not supported by this backend","type":"mock"}}`))
		case bytes.Contains(body, []byte(`"stream":true`)):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(m6Response("gpt-4o", "mock answer")))
		}
	}))
	t.Cleanup(up.server.Close)
	return up
}

func (u *m6Upstream) calls() int64 { return atomic.LoadInt64(&u.count) }

// m6Config is a one-backend configuration with M6 enabled.
func m6Config(baseURL string) config.Config {
	cfg := config.Defaults()
	cfg.Log.Level = "error"
	cfg.Server.Listen = ":0"
	cfg.Upstreams = []config.UpstreamConfig{{
		Name:         "local",
		Kind:         "openai",
		BaseURL:      baseURL,
		APIKey:       "test-key",
		Models:       []string{"/"},
		Capabilities: []string{"chat", "tools", "stream", "json"},
	}}
	cfg.Idempotency.Enabled = true
	cfg.Sessions.Enabled = true
	return cfg
}

func newM6Server(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	srv, err := NewServer(&cfg, testLogger{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.closeM6)
	return srv
}

// getJSON performs a GET against the real handler and decodes the body.
func m6GetJSON(t *testing.T, srv *Server, path string, header map[string]string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s: decode %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, out
}

func m6At(t *testing.T, out map[string]any, path ...string) any {
	t.Helper()
	var cur any = out
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("value at %v is not an object", path)
		}
		cur, ok = m[key]
		if !ok {
			t.Fatalf("key %q missing from %v", key, m)
		}
	}
	return cur
}

// TestM6AdminSurfacesReportConfigurationWhenDisabled pins the shape an operator
// sees before turning either feature on: the configuration is answered, and the
// counters read zero rather than being absent.
func TestM6AdminSurfacesReportConfigurationWhenDisabled(t *testing.T) {
	up := newM6Upstream(t)
	cfg := m6Config(up.server.URL)
	cfg.Idempotency.Enabled = false
	cfg.Sessions.Enabled = false
	srv := newM6Server(t, cfg)

	code, body := m6GetJSON(t, srv, "/admin/idempotency", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /admin/idempotency = %d, want 200", code)
	}
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
	if body["capacity"] != float64(2048) {
		t.Errorf("capacity = %v, want the default 2048", body["capacity"])
	}
	if body["stored"] != float64(0) || body["in_flight"] != float64(0) {
		t.Errorf("stored/in_flight = %v/%v, want 0/0", body["stored"], body["in_flight"])
	}
	if _, ok := body["entries"].([]any); !ok {
		t.Errorf("entries = %T, want a list even when empty", body["entries"])
	}

	code, body = m6GetJSON(t, srv, "/admin/sessions", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /admin/sessions = %d, want 200", code)
	}
	if body["enabled"] != false || body["tracked"] != float64(0) {
		t.Errorf("enabled/tracked = %v/%v, want false/0", body["enabled"], body["tracked"])
	}
	if _, ok := body["sessions"].([]any); !ok {
		t.Errorf("sessions = %T, want a list even when empty", body["sessions"])
	}

	// The shared limit parameter is validated on both listings, because a bad
	// limit is a caller bug rather than an empty result.
	for _, path := range []string{"/admin/idempotency?limit=0", "/admin/sessions?limit=abc"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, rec.Code)
		}
	}
}

// TestIdempotentReplayAvoidsASecondUpstreamCallThroughTheHandler is the M6
// headline behaviour, checked across the real handler: the second request with
// the same key is answered from the store and the provider sees ONE call.
func TestIdempotentReplayAvoidsASecondUpstreamCallThroughTheHandler(t *testing.T) {
	up := newM6Upstream(t)
	srv := newM6Server(t, m6Config(up.server.URL))

	first := m6PostChat(t, srv, m6ChatBody, map[string]string{"Idempotency-Key": "op-1"})
	if first.Code != http.StatusOK {
		t.Fatalf("first POST = %d, body %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get("X-InferGate-Idempotent-Replay"); got != "false" {
		t.Errorf("first response X-InferGate-Idempotent-Replay = %q, want \"false\"", got)
	}
	origin := first.Header().Get("X-InferGate-Request-Id")
	if origin == "" {
		t.Fatal("first response carried no X-InferGate-Request-Id")
	}

	second := m6PostChat(t, srv, m6ChatBody, map[string]string{"Idempotency-Key": "op-1"})
	if second.Code != http.StatusOK {
		t.Fatalf("second POST = %d, body %s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("X-InferGate-Idempotent-Replay"); got != "true" {
		t.Errorf("second response X-InferGate-Idempotent-Replay = %q, want \"true\"", got)
	}
	if got := second.Header().Get("X-InferGate-Idempotent-Origin"); got != origin {
		t.Errorf("X-InferGate-Idempotent-Origin = %q, want the original request id %q", got, origin)
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("replay body differs:\n first %s\nsecond %s", first.Body.String(), second.Body.String())
	}
	if got := up.calls(); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (the replay must not re-run the work)", got)
	}

	// The same key with a different body is a conflict, not a hit: replaying
	// would answer a question the caller did not ask.
	other := m6PostChat(t, srv, `{"model":"gpt-4o","messages":[{"role":"user","content":"goodbye"}]}`,
		map[string]string{"Idempotency-Key": "op-1"})
	if other.Code != http.StatusConflict {
		t.Errorf("same key, different body = %d, want 409", other.Code)
	}

	code, body := m6GetJSON(t, srv, "/admin/idempotency", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /admin/idempotency = %d, want 200", code)
	}
	if body["stored"] != float64(1) {
		t.Errorf("stored = %v, want 1", body["stored"])
	}
	stats := m6At(t, body, "stats").(map[string]any)
	if stats["hits"] != float64(1) {
		t.Errorf("stats.hits = %v, want 1", stats["hits"])
	}
	if stats["misses"] != float64(1) {
		t.Errorf("stats.misses = %v, want 1", stats["misses"])
	}
	if stats["conflicts"] != float64(1) {
		t.Errorf("stats.conflicts = %v, want 1", stats["conflicts"])
	}
	entries := m6At(t, body, "entries").([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	entry := entries[0].(map[string]any)
	if entry["key"] != "op-1" || entry["scope"] == "" {
		t.Errorf("entry key/scope = %v/%v", entry["key"], entry["scope"])
	}
	if entry["body"] != nil {
		t.Errorf("entry exposed its body in the admin listing: %v", entry["body"])
	}

	// Flushing makes the key usable again, which is the operator's remedy after
	// a buggy caller stored an answer it should not have.
	req := httptest.NewRequest(http.MethodPost, "/admin/idempotency/flush", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /admin/idempotency/flush = %d, want 200", rec.Code)
	}
	var flushed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &flushed)
	if flushed["flushed"] != float64(1) {
		t.Errorf("flushed = %v, want 1", flushed["flushed"])
	}
}

// TestIdempotentReplayIsScopedByTenant pins that two tenants choosing the same
// key are not talking about the same request.
func TestIdempotentReplayIsScopedByTenant(t *testing.T) {
	up := newM6Upstream(t)
	srv := newM6Server(t, m6Config(up.server.URL))

	first := m6PostChat(t, srv, m6ChatBody, map[string]string{
		"Idempotency-Key": "op-1", "X-InferGate-Tenant": "team-a",
	})
	second := m6PostChat(t, srv, m6ChatBody, map[string]string{
		"Idempotency-Key": "op-1", "X-InferGate-Tenant": "team-b",
	})
	if second.Header().Get("X-InferGate-Idempotent-Replay") != "false" {
		t.Error("a different tenant's identical key must run its own work, not replay")
	}
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("codes = %d/%d, want 200/200", first.Code, second.Code)
	}
	if got := up.calls(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (one per tenant)", got)
	}
}

// TestSessionLedgerRollsUpThroughTheHandler records two requests of one
// conversation and reads the rollup back, including the tenant-scoped lookup
// and the ambiguity that an unscoped id can produce.
func TestSessionLedgerRollsUpThroughTheHandler(t *testing.T) {
	up := newM6Upstream(t)
	srv := newM6Server(t, m6Config(up.server.URL))

	headers := map[string]string{
		"X-InferGate-Tenant":  "team-a",
		"X-InferGate-Session": "conv-1",
	}
	if rec := m6PostChat(t, srv, m6ChatBody, headers); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, body %s", rec.Code, rec.Body.String())
	}
	// A request with no session header at all is counted, not invented.
	if rec := m6PostChat(t, srv, m6ChatBody, map[string]string{"X-InferGate-Tenant": "team-a"}); rec.Code != http.StatusOK {
		t.Fatalf("POST without session = %d", rec.Code)
	}
	// A second tenant that happens to use the same session id.
	sameID := map[string]string{"X-InferGate-Tenant": "team-b", "X-InferGate-Session": "conv-1"}
	if rec := m6PostChat(t, srv, m6ChatBody, sameID); rec.Code != http.StatusOK {
		t.Fatalf("POST for team-b = %d", rec.Code)
	}

	code, body := m6GetJSON(t, srv, "/admin/sessions", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /admin/sessions = %d, want 200", code)
	}
	if body["tracked"] != float64(2) {
		t.Errorf("tracked = %v, want 2 sessions (one per tenant)", body["tracked"])
	}
	stats := m6At(t, body, "stats").(map[string]any)
	if stats["no_session_id"] != float64(1) {
		t.Errorf("stats.no_session_id = %v, want 1", stats["no_session_id"])
	}

	code, body = m6GetJSON(t, srv, "/admin/sessions/conv-1?tenant=team-a", nil)
	if code != http.StatusOK {
		t.Fatalf("GET scoped session = %d, want 200", code)
	}
	if body["tenant"] != "team-a" || body["id"] != "conv-1" {
		t.Errorf("session = %v/%v, want team-a/conv-1", body["tenant"], body["id"])
	}
	if body["requests"] != float64(1) {
		t.Errorf("requests = %v, want 1", body["requests"])
	}
	if body["prompt_tokens"] != float64(5) || body["completion_tokens"] != float64(7) {
		t.Errorf("tokens = %v/%v, want 5/7", body["prompt_tokens"], body["completion_tokens"])
	}
	if body["models"] == nil {
		t.Error("session carried no per-model rollup")
	}

	// Without a tenant the id is ambiguous, and guessing would hand an operator
	// another tenant's spend.
	code, body = m6GetJSON(t, srv, "/admin/sessions/conv-1", nil)
	if code != http.StatusConflict {
		t.Fatalf("GET ambiguous session = %d, want 409 (body %v)", code, body)
	}

	code, _ = m6GetJSON(t, srv, "/admin/sessions/nope", nil)
	if code != http.StatusNotFound {
		t.Errorf("GET unknown session = %d, want 404", code)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/sessions/flush", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /admin/sessions/flush = %d, want 200", rec.Code)
	}
	if _, body := m6GetJSON(t, srv, "/admin/sessions", nil); body["tracked"] != float64(0) {
		t.Errorf("tracked after flush = %v, want 0", body["tracked"])
	}
}

// TestCapabilitiesReportsTheDeclaredSurface checks the declarative endpoint
// against a fleet whose metadata is deliberately richer than the wire carries.
func TestCapabilitiesReportsTheDeclaredSurface(t *testing.T) {
	up := newM6Upstream(t)
	cfg := m6Config(up.server.URL)
	cfg.Models = map[string]config.ModelInfo{
		"gpt-4o": {ContextWindow: 128000, MaxOutputTokens: 16384, Capabilities: []string{"vision"}, Notes: "primary"},
	}
	srv := newM6Server(t, cfg)

	code, body := m6GetJSON(t, srv, "/v1/capabilities", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/capabilities = %d, want 200", code)
	}
	caps := m6At(t, body, "capabilities").([]any)
	got := map[string]bool{}
	for _, c := range caps {
		got[c.(string)] = true
	}
	// The union of what the backend declares and what the model adds.
	for _, want := range []string{"chat", "tools", "stream", "json", "vision"} {
		if !got[want] {
			t.Errorf("capability %q missing from %v", want, caps)
		}
	}

	models := m6At(t, body, "models").([]any)
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	model := models[0].(map[string]any)
	if model["model"] != "gpt-4o" {
		t.Errorf("model = %v", model["model"])
	}
	if model["context_window"] != float64(128000) {
		t.Errorf("context_window = %v, want 128000", model["context_window"])
	}
	if model["notes"] != "primary" {
		t.Errorf("notes = %v, want primary", model["notes"])
	}
	if model["available"] != true {
		t.Errorf("available = %v, want true", model["available"])
	}
	if ups := model["upstreams"].([]any); len(ups) != 1 || ups[0] != "local" {
		t.Errorf("model upstreams = %v, want [local]", ups)
	}

	ups := m6At(t, body, "upstreams").([]any)
	if len(ups) != 1 {
		t.Fatalf("upstreams = %d, want 1", len(ups))
	}
	entry := ups[0].(map[string]any)
	if entry["name"] != "local" || entry["state"] != "closed" {
		t.Errorf("upstream name/state = %v/%v, want local/closed", entry["name"], entry["state"])
	}
}

// TestCapabilityProbeClassifiesAcceptance drives the live probe against a stub
// that accepts chat, tools, streaming and JSON mode but refuses vision and
// embeddings, and answers one capability with a 500 to prove that an upstream
// failure is reported as indeterminate rather than as "not supported".
func TestCapabilityProbeClassifiesAcceptance(t *testing.T) {
	var flaky int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		switch {
		case r.URL.Path == "/v1/embeddings":
			w.WriteHeader(http.StatusNotFound)
		case bytes.Contains(body, []byte("image_url")):
			w.WriteHeader(http.StatusBadRequest)
		case bytes.Contains(body, []byte(`"tools"`)):
			// One tools probe fails at the transport level.
			if atomic.AddInt64(&flaky, 1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		case bytes.Contains(body, []byte(`"stream":true`)):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer backend.Close()

	cfg := m6Config(backend.URL)
	srv := newM6Server(t, cfg)

	// The backend declares only the catch-all model pattern, which is not a
	// model name a provider accepts: probing it without being told what to send
	// is reported as skipped rather than guessed.
	skipped := m6Probe(t, srv, `{"capabilities":["chat"]}`)
	if len(skipped) != 1 || skipped[0]["outcome"] != "skipped" {
		t.Fatalf("catch-all-only probe = %v, want one skipped probe", skipped)
	}
	if skipped[0]["error"] == nil {
		t.Error("a skipped probe must say why it was skipped")
	}

	probes := m6Probe(t, srv, `{"capabilities":["chat","vision","embeddings","tools"],"model":"gpt-4o"}`)
	byCap := map[string]map[string]any{}
	for _, p := range probes {
		byCap[p["capability"].(string)] = p
	}
	if byCap["chat"]["outcome"] != "accepted" {
		t.Errorf("chat outcome = %v, want accepted", byCap["chat"]["outcome"])
	}
	if byCap["vision"]["outcome"] != "rejected" || byCap["vision"]["accepted"] != false {
		t.Errorf("vision outcome = %v, want rejected", byCap["vision"]["outcome"])
	}
	if byCap["embeddings"]["outcome"] != "rejected" {
		t.Errorf("embeddings outcome = %v, want rejected", byCap["embeddings"]["outcome"])
	}
	if byCap["tools"]["outcome"] != "indeterminate" {
		t.Errorf("tools outcome = %v, want indeterminate (a 500 says nothing about capability)", byCap["tools"]["outcome"])
	}
	if byCap["chat"]["elapsed_ms"] == nil {
		t.Error("probe reported no elapsed time")
	}
	if byCap["chat"]["model"] != "gpt-4o" {
		t.Errorf("probe model = %v, want the model it was told to send", byCap["chat"]["model"])
	}
	if byCap["chat"]["path"] != "/v1/chat/completions" {
		t.Errorf("chat probe path = %v", byCap["chat"]["path"])
	}
	if byCap["embeddings"]["path"] != "/v1/embeddings" {
		t.Errorf("embeddings probe path = %v", byCap["embeddings"]["path"])
	}

	// An unknown capability is a caller error, and the message lists what can
	// be probed.
	req := httptest.NewRequest(http.MethodPost, "/v1/capabilities/probe",
		strings.NewReader(`{"capabilities":["telepathy"]}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown capability = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "chat") {
		t.Errorf("error message does not list the probeable capabilities: %s", rec.Body.String())
	}
}

// TestM6MetricsExposed checks that the new families reach /metrics, and that
// the families derived from a bounded map are gauges rather than counters: a
// session evicted at capacity takes its requests out of the sum, and a
// Prometheus counter may not decrease.
func TestM6MetricsExposed(t *testing.T) {
	up := newM6Upstream(t)
	srv := newM6Server(t, m6Config(up.server.URL))

	if rec := m6PostChat(t, srv, m6ChatBody, map[string]string{
		"Idempotency-Key": "op-1", "X-InferGate-Session": "conv-1", "X-InferGate-Tenant": "team-a",
	}); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d", rec.Code)
	}
	if rec := m6PostChat(t, srv, m6ChatBody, map[string]string{
		"Idempotency-Key": "op-1", "X-InferGate-Session": "conv-1", "X-InferGate-Tenant": "team-a",
	}); rec.Code != http.StatusOK {
		t.Fatalf("replay POST = %d", rec.Code)
	}

	rec := httptest.NewRecorder()
	srv.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := rec.Body.String()

	types := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, typ, _ := strings.Cut(rest, " ")
			types[name] = typ
		}
	}
	counters := []string{
		"infergate_idempotency_lookups_total",
		"infergate_idempotency_hits_total",
		"infergate_idempotency_misses_total",
		"infergate_idempotency_stored_total",
		"infergate_session_records_total",
		"infergate_session_no_id_total",
	}
	for _, name := range counters {
		if types[name] != "counter" {
			t.Errorf("%s type = %q, want counter", name, types[name])
		}
	}
	gauges := []string{
		"infergate_idempotency_entries",
		"infergate_idempotency_in_flight",
		"infergate_idempotency_scopes",
		"infergate_sessions_tracked",
		"infergate_sessions_requests",
		"infergate_sessions_cost_usd",
	}
	for _, name := range gauges {
		if types[name] != "gauge" {
			t.Errorf("%s type = %q, want gauge", name, types[name])
		}
	}
	if !strings.Contains(out, "infergate_idempotency_hits_total 1\n") {
		t.Error("expected exactly one idempotency hit in the exposition")
	}
	if !strings.Contains(out, "infergate_session_no_id_total 0\n") {
		t.Error("expected no unattributed requests in the exposition")
	}
}

// closeM6 releases the stores' janitor goroutines when a test finishes.
func (s *Server) closeM6() {
	if s.idempotency != nil {
		s.idempotency.Close()
	}
	if s.sessions != nil {
		s.sessions.Close()
	}
}

// postChat sends one chat completion through the real handler.
func m6PostChat(t *testing.T, srv *Server, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// probe runs a capability probe and returns the decoded probe list.
func m6Probe(t *testing.T, srv *Server, body string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/capabilities/probe", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/capabilities/probe = %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Probes []map[string]any `json:"probes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode probe response: %v", err)
	}
	return out.Probes
}
