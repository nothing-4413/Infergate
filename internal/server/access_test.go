package server

// Tests for the operator-token gate on the operational surfaces.
//
// The interesting properties are the ones a unit test of the policy cannot
// reach: that the gate is actually installed in the handler chain the server
// serves (a correct policy that nothing consults is the failure mode a
// middleware refactor produces), that it leaves the probes and the proxied
// surface alone, and that the answer for a missing token and a wrong token are
// indistinguishable to the caller.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/infergate/infergate/internal/config"
)

// accessTestConfig is a one-backend configuration; the backend is never dialled.
func accessTestConfig() config.Config {
	cfg := config.Defaults()
	cfg.Log.Level = "error"
	cfg.Server.Listen = ":0"
	cfg.Upstreams = []config.UpstreamConfig{{
		Name:    "local",
		Kind:    "openai",
		BaseURL: "http://127.0.0.1:1",
		Models:  []string{"/"},
	}}
	return cfg
}

// accessTestServer builds a server and returns the handler the listener would
// serve, so the test drives the real chain rather than a copy of it.
func accessTestServer(t *testing.T, cfg config.Config) http.Handler {
	t.Helper()
	srv, err := NewServer(&cfg, testLogger{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv.Handler()
}

func doRequest(h http.Handler, method, path string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// TestAccessDisabledMatchesTheM0M6Behaviour pins the compatibility promise: with
// no token configured, every operational endpoint answers exactly as it did
// before the gate existed. Every config and every acceptance script in the
// repository depends on this, so it is asserted rather than assumed.
func TestAccessDisabledMatchesTheM0M6Behaviour(t *testing.T) {
	h := accessTestServer(t, accessTestConfig())

	for _, path := range []string{
		"/healthz", "/readyz", "/metrics", "/stats",
		"/admin/upstreams", "/admin/breakers", "/admin/cache", "/admin/quota",
		"/admin/traces", "/admin/tracing", "/admin/idempotency", "/admin/sessions",
		"/v1/capabilities",
	} {
		if rec := doRequest(h, http.MethodGet, path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s with access disabled = %d, want 200 (body %s)", path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// TestAccessEnabledProtectsTheOperationalSurface walks the whole matrix of
// "which paths, with which credential" in one place, because the bug worth
// catching is an entry that flipped the wrong way.
func TestAccessEnabledProtectsTheOperationalSurface(t *testing.T) {
	cfg := accessTestConfig()
	cfg.Access.Enabled = true
	cfg.Access.Tokens = []string{"s3cret"}
	h := accessTestServer(t, cfg)

	protected := []string{
		"/stats", "/admin/upstreams", "/admin/breakers", "/admin/cache",
		"/admin/cache/lookup", "/admin/quota", "/admin/traces",
		"/admin/tracing", "/admin/idempotency", "/admin/sessions",
	}
	// The 400/404 entries are here on purpose: the point of the loop is that the
	// request reached the HANDLER when the token was right, and a handler that
	// has to be asked a real question (which prompt? which trace?) answers 400
	// or 404 -- not 401. The gate is what is under test, not the handlers.
	okWithToken := []string{
		"/stats", "/admin/upstreams", "/admin/breakers", "/admin/cache",
		"/admin/quota", "/admin/tracing", "/admin/idempotency",
		"/admin/sessions",
	}
	reachesHandler := map[string]int{
		"/admin/cache/lookup": 400, // prompt is required
		"/admin/traces/abc":   404, // tracing is disabled in this config
		"/admin/sessions/abc": 404, // no such session
	}
	for _, path := range protected {
		if rec := doRequest(h, http.MethodGet, path, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, rec.Code)
		}
		if rec := doRequest(h, http.MethodGet, path, bearer("wrong")); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with a wrong token = %d, want 401", path, rec.Code)
		}
	}
	for _, path := range okWithToken {
		if rec := doRequest(h, http.MethodGet, path, bearer("s3cret")); rec.Code != http.StatusOK {
			t.Errorf("GET %s with the right token = %d, want 200 (body %s)", path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
	for path, want := range reachesHandler {
		if rec := doRequest(h, http.MethodGet, path, bearer("s3cret")); rec.Code != want {
			t.Errorf("GET %s with the right token = %d, want %d (body %s)", path, rec.Code, want, strings.TrimSpace(rec.Body.String()))
		}
	}

	// The mutating verbs are the ones that matter most: a POST that flushes the
	// cache or drains the replay store must not be reachable without the token.
	for _, path := range []string{
		"/admin/breakers/reset", "/admin/cache/flush",
		"/admin/idempotency/flush", "/admin/sessions/flush",
	} {
		if rec := doRequest(h, http.MethodPost, path, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without a token = %d, want 401", path, rec.Code)
		}
		if rec := doRequest(h, http.MethodPost, path, bearer("s3cret")); rec.Code != http.StatusOK {
			t.Errorf("POST %s with the right token = %d, want 200 (body %s)", path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// TestAccessLeavesTheProbesAndTheProxyOpen pins what is deliberately NOT behind
// the token. A healthcheck that needs a credential is a healthcheck that marks a
// working deployment unhealthy, and the OpenAI-compatible surface is the product
// -- it is authenticated (or not) by the provider's own key, which the gateway
// forwards.
func TestAccessLeavesTheProbesAndTheProxyOpen(t *testing.T) {
	cfg := accessTestConfig()
	cfg.Access.Enabled = true
	cfg.Access.Tokens = []string{"s3cret"}
	srv, err := NewServer(&cfg, testLogger{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h := srv.Handler()

	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/v1/capabilities"} {
		if rec := doRequest(h, http.MethodGet, path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 without a token", path, rec.Code)
		}
	}

	// /v1/chat/completions must reach the proxy rather than the gate. The
	// backend is unreachable, so the answer is an upstream error -- any 2xx/4xx
	// reply would prove the point too; what must NOT appear is 401.
	rec := doRequest(h, http.MethodPost, "/v1/chat/completions", nil)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("the proxied surface answered 401; the gate must not cover it")
	}
}

// TestAccessRejectionShapeIsUsableAndTellsNothing checks the two things a client
// and an operator need from a 401: a machine-readable type so a caller can
// distinguish it from an upstream 401, and a WWW-Authenticate challenge so an
// interactive client can prompt. It also asserts the body does not echo the
// rejected credential.
func TestAccessRejectionShapeIsUsableAndTellsNothing(t *testing.T) {
	cfg := accessTestConfig()
	cfg.Access.Enabled = true
	cfg.Access.Tokens = []string{"s3cret"}
	h := accessTestServer(t, cfg)

	rec := doRequest(h, http.MethodGet, "/admin/upstreams", bearer("guessed-token-value"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="infergate"` {
		t.Errorf("WWW-Authenticate = %q, want %q", got, `Bearer realm="infergate"`)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	if strings.Contains(rec.Body.String(), "guessed-token-value") {
		t.Errorf("the rejection echoed the presented credential: %s", rec.Body.String())
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("rejection body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Type != "infergate_unauthorized" {
		t.Errorf("error.type = %q, want infergate_unauthorized", body.Error.Type)
	}
	if body.Error.Message == "" {
		t.Error("error.message is empty; a 401 with no explanation is a support ticket")
	}

	// A missing token and a wrong token must be the same answer. If a wrong
	// token produced 403, a prober would learn that it had found a real
	// endpoint whose expected format differs from what it sent.
	missing := doRequest(h, http.MethodGet, "/admin/upstreams", nil)
	if missing.Code != rec.Code {
		t.Errorf("missing token = %d but wrong token = %d; the two must be indistinguishable", missing.Code, rec.Code)
	}
	if missing.Body.String() != rec.Body.String() {
		t.Error("missing-token and wrong-token bodies differ")
	}
}

// TestAccessAcceptsAnyConfiguredToken covers rotation: two tokens are configured
// while callers are moved across, and both must work.
func TestAccessAcceptsAnyConfiguredToken(t *testing.T) {
	cfg := accessTestConfig()
	cfg.Access.Enabled = true
	cfg.Access.Tokens = []string{"old-token", "new-token"}
	h := accessTestServer(t, cfg)

	for _, tok := range []string{"old-token", "new-token"} {
		if rec := doRequest(h, http.MethodGet, "/admin/upstreams", bearer(tok)); rec.Code != http.StatusOK {
			t.Errorf("token %q = %d, want 200", tok, rec.Code)
		}
	}
	// A prefix of a valid token, or a valid token with something appended, must
	// not pass: this is what a comparison that stops at the first difference
	// gets wrong in the other direction.
	//
	// The whitespace cases go through the query parameter rather than the
	// header, and that is not a dodge: Go's header parser strips leading and
	// trailing whitespace from a header value before any handler sees it, so
	// "Bearer  old-token " reaches the gate as "Bearer old-token" no matter what
	// this code does. A test that set that header would be asserting on
	// net/http, not on the gate.
	cfg.Access.AllowQueryToken = true
	hq := accessTestServer(t, cfg)
	for _, tok := range []string{"old-toke", "old-tokenx", " old-token", "old-token "} {
		if rec := doRequest(h, http.MethodGet, "/admin/upstreams", bearer(tok)); rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q = %d, want 401", tok, rec.Code)
		}
		if rec := doRequest(hq, http.MethodGet, "/admin/upstreams?access_token="+url.QueryEscape(tok), nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("query token %q = %d, want 401", tok, rec.Code)
		}
	}
}

// TestAccessHeaderForms covers the accepted spellings of the credential. The
// scheme is case-insensitive per RFC 7235 and the separator is required; a bare
// token in the Authorization header is NOT accepted, because accepting it would
// mean guessing at a format rather than checking one.
func TestAccessHeaderForms(t *testing.T) {
	cfg := accessTestConfig()
	cfg.Access.Enabled = true
	cfg.Access.Tokens = []string{"s3cret"}
	h := accessTestServer(t, cfg)

	accepted := []map[string]string{
		{"Authorization": "Bearer s3cret"},
		{"Authorization": "bearer s3cret"},
		{"Authorization": "BEARER s3cret"},
	}
	for _, header := range accepted {
		if rec := doRequest(h, http.MethodGet, "/admin/upstreams", header); rec.Code != http.StatusOK {
			t.Errorf("header %v = %d, want 200", header, rec.Code)
		}
	}
	rejected := []map[string]string{
		{"Authorization": "s3cret"},
		{"Authorization": "Bearers3cret"},
		{"Authorization": "Basic czNjcmV0"},
		{"X-Operator-Token": "s3cret"},
	}
	for _, header := range rejected {
		if rec := doRequest(h, http.MethodGet, "/admin/upstreams", header); rec.Code != http.StatusUnauthorized {
			t.Errorf("header %v = %d, want 401", header, rec.Code)
		}
	}
}

// TestAccessCustomHeaderAndQueryToken covers the two escape hatches. The query
// form is off unless asked for, which is the important half: a token in a URL
// ends up in access logs.
func TestAccessCustomHeaderAndQueryToken(t *testing.T) {
	// A custom header turns the bearer scheme off: there is no scheme
	// convention for a header that is not Authorization.
	cfg := accessTestConfig()
	cfg.Access.Enabled = true
	cfg.Access.Tokens = []string{"s3cret"}
	cfg.Access.Header = "X-Operator-Token"
	h := accessTestServer(t, cfg)

	if rec := doRequest(h, http.MethodGet, "/admin/upstreams", map[string]string{"X-Operator-Token": "s3cret"}); rec.Code != http.StatusOK {
		t.Errorf("custom header verbatim = %d, want 200", rec.Code)
	}
	if rec := doRequest(h, http.MethodGet, "/admin/upstreams", bearer("s3cret")); rec.Code != http.StatusUnauthorized {
		t.Error("a custom header must stop Authorization from working; otherwise two credentials are accepted")
	}
	if rec := doRequest(h, http.MethodGet, "/admin/upstreams", nil); rec.Header().Get("WWW-Authenticate") != "" {
		t.Error("a non-bearer credential must not advertise a Bearer challenge")
	}

	// The query form is opt-in.
	cfg2 := accessTestConfig()
	cfg2.Access.Enabled = true
	cfg2.Access.Tokens = []string{"s3cret"}
	h2 := accessTestServer(t, cfg2)
	if rec := doRequest(h2, http.MethodGet, "/admin/upstreams?access_token=s3cret", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("query token accepted while allow_query_token is false: %d", rec.Code)
	}

	// cfg3 is built from the same base rather than copied from cfg2, so this
	// case cannot inherit a stray field from the one above it.
	cfg3 := accessTestConfig()
	cfg3.Access.Enabled = true
	cfg3.Access.Tokens = []string{"s3cret"}
	cfg3.Access.AllowQueryToken = true
	h3 := accessTestServer(t, cfg3)
	if rec := doRequest(h3, http.MethodGet, "/admin/upstreams?access_token=s3cret", nil); rec.Code != http.StatusOK {
		t.Errorf("query token rejected while allow_query_token is true: %d", rec.Code)
	}
}

// TestAccessProtectListIsConfigurable covers the operator who wants the scrape
// target closed too, and the prefix boundary that stops /admin from swallowing a
// path that merely starts with the same letters.
func TestAccessProtectListIsConfigurable(t *testing.T) {
	cfg := accessTestConfig()
	cfg.Access.Enabled = true
	cfg.Access.Tokens = []string{"s3cret"}
	cfg.Access.Protect = []string{"/metrics", "/admin"}
	h := accessTestServer(t, cfg)

	if rec := doRequest(h, http.MethodGet, "/metrics", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /metrics without a token = %d, want 401 once it is listed", rec.Code)
	}
	if rec := doRequest(h, http.MethodGet, "/metrics", bearer("s3cret")); rec.Code != http.StatusOK {
		t.Errorf("GET /metrics with the token = %d, want 200", rec.Code)
	}
	// /stats was protected by default but is not in this explicit list, so it
	// must now be open: an explicit list replaces the default rather than
	// adding to it.
	if rec := doRequest(h, http.MethodGet, "/stats", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /stats = %d, want 200 once the explicit list omits it", rec.Code)
	}
}

// TestAccessCoversUsesPathSegments is a direct unit test of the matching rule,
// including the trailing-slash and near-miss cases that a plain strings.HasPrefix
// would get wrong.
func TestAccessCoversUsesPathSegments(t *testing.T) {
	cfg := accessTestConfig()
	cfg.Access.Enabled = true
	cfg.Access.Tokens = []string{"s3cret"}
	cfg.Access.Protect = []string{"/admin/", "/stats"}
	p := newAccessPolicy(cfg.Access)

	// The query string is not part of the path: covers() is handed
	// r.URL.Path, and a policy that had to reason about "?x=1" would be one
	// refactor away from matching on the raw request target.
	covered := []string{"/admin", "/admin/", "/admin/cache", "/admin/cache/flush", "/stats"}
	for _, path := range covered {
		if !p.covers(path) {
			t.Errorf("covers(%q) = false, want true", path)
		}
	}
	notCovered := []string{"/administrator", "/adminish", "/healthz", "/metrics", "/", "/v1/chat/completions"}
	for _, path := range notCovered {
		if p.covers(path) {
			t.Errorf("covers(%q) = true, want false", path)
		}
	}
}

// TestAccessPolicyTreatsEmptyConfigurationAsOff covers the normalisation rules:
// whitespace-only tokens do not arm the gate, and an empty protect list means
// the default set rather than "protect nothing".
func TestAccessPolicyTreatsEmptyConfigurationAsOff(t *testing.T) {
	var zero config.AccessConfig
	p := newAccessPolicy(zero)
	if p.enabled {
		t.Error("an empty access section armed the gate")
	}
	if !p.covers("/admin/cache") || !p.covers("/stats") {
		t.Error("the default protect set is not in effect before validation runs")
	}
	if p.covers("/metrics") {
		t.Error("/metrics is protected by default; it is the scrape target and the README says so")
	}

	blank := config.AccessConfig{Enabled: true, Tokens: []string{"", "   "}}
	if p := newAccessPolicy(blank); p.enabled {
		t.Error("a whitespace-only token armed the gate")
	}
}

// TestAccessPreflightPassesThrough documents the one deliberate hole: a browser
// omits Authorization on an OPTIONS preflight, so rejecting one would make every
// protected endpoint unusable from a browser. No handler logic runs for it.
func TestAccessPreflightPassesThrough(t *testing.T) {
	cfg := accessTestConfig()
	cfg.Access.Enabled = true
	cfg.Access.Tokens = []string{"s3cret"}
	h := accessTestServer(t, cfg)

	if rec := doRequest(h, http.MethodOptions, "/admin/upstreams", nil); rec.Code == http.StatusUnauthorized {
		t.Error("an OPTIONS preflight was rejected; a browser client could never reach a protected endpoint")
	}
}
