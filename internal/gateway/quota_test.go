package gateway

// M3 quota admission, driven through the REAL proxy (router, breakers, cache
// and accounting included) rather than against quotapath.go's helpers in
// isolation.
//
// The reason is the same one that shaped the M1 and M2 suites: every decision
// this layer makes is a decision about ORDER. Admission must run before the
// cache lookup, so a tenant that is over budget cannot keep being answered from
// a stored completion. The lease must be opened on a cache hit, so a "free"
// answer still consumes a per-minute slot. Release, not Settle, must run when
// no provider was ever billed. None of those are visible in a unit test of
// admitQuota; all of them are visible through ServeHTTP.
//
// The gate itself is a fake, because a real quota.Manager would put a token
// bucket, a clock and a store between the test and the thing being tested. The
// fake records what it was asked and hands back a scripted Decision, which is
// exactly the seam the proxy is supposed to honour.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/quota"
)

// ---------------------------------------------------------------------------
// Fake gate
// ---------------------------------------------------------------------------

// fakeQuotaGate is a scriptable QuotaGate.
//
// It exists to answer one question per case -- was Admit called, what did it
// see, what did Settle/Release receive -- so its methods deliberately do no
// arithmetic. The zero value is a disabled gate that admits nothing, which is
// the right default for a test that only wants to prove the gate was NOT
// consulted.
type fakeQuotaGate struct {
	enabled bool

	// admitFn, when set, wins over the static fields below. It is the escape
	// hatch for multi-request tests (a cache miss followed by a hit) where a
	// single scripted decision cannot describe both calls.
	admitFn func(tenant, session string, est quota.Estimate) (*quota.Decision, error)

	// decision and admitErr are what a single-shot gate returns.
	decision *quota.Decision
	admitErr error

	mu       sync.Mutex
	admits   []quotaAdmitCall
	settles  []quotaSettleCall
	releases []*quota.Reservation
}

// quotaAdmitCall is one Admit invocation, flattened.
type quotaAdmitCall struct {
	Tenant  string
	Session string
	Est     quota.Estimate
}

// quotaSettleCall is one Settle invocation, flattened.
//
// The reservation pointer is kept so a test can prove Settle received the SAME
// reservation Admit handed out: settling a lease the gate never issued would
// silently credit the wrong bucket.
type quotaSettleCall struct {
	Res   *quota.Reservation
	Usage quota.Usage
}

func (g *fakeQuotaGate) Enabled() bool { return g.enabled }

func (g *fakeQuotaGate) Admit(_ context.Context, tenant, session string, est quota.Estimate) (*quota.Decision, error) {
	g.mu.Lock()
	g.admits = append(g.admits, quotaAdmitCall{Tenant: tenant, Session: session, Est: est})
	fn := g.admitFn
	dec, err := g.decision, g.admitErr
	g.mu.Unlock()

	if fn != nil {
		return fn(tenant, session, est)
	}
	return dec, err
}

func (g *fakeQuotaGate) Settle(_ context.Context, res *quota.Reservation, usage quota.Usage) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settles = append(g.settles, quotaSettleCall{Res: res, Usage: usage})
	return nil
}

func (g *fakeQuotaGate) Release(_ context.Context, res *quota.Reservation) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releases = append(g.releases, res)
	return nil
}

func (g *fakeQuotaGate) admitCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.admits)
}

func (g *fakeQuotaGate) admitCalls() []quotaAdmitCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]quotaAdmitCall, len(g.admits))
	copy(out, g.admits)
	return out
}

func (g *fakeQuotaGate) settleCalls() []quotaSettleCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]quotaSettleCall, len(g.settles))
	copy(out, g.settles)
	return out
}

func (g *fakeQuotaGate) releaseCalls() []*quota.Reservation {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*quota.Reservation, len(g.releases))
	copy(out, g.releases)
	return out
}

// errQuotaScripted stands in for a real store outage. The VALUE never reaches
// the client (the gateway writes its own envelope), so it only has to be
// non-nil for the fail-open/fail-closed branches that key off it.
var errQuotaScripted = errors.New("scripted quota store outage")

// allowedDecision is the shape quota.Manager.Admit returns on the happy path.
// The Reservation must be non-nil: settleQuota decides between Settle and
// Release partly on `rec.quotaRes != nil`, so a fake that returned nil here
// would silently skip accounting and make every settle test vacuous.
func allowedDecision() *quota.Decision {
	return &quota.Decision{
		Allowed:     true,
		Action:      quota.ActionAllow,
		Reason:      quota.ReasonWithinBudget,
		Reservation: &quota.Reservation{},
	}
}

// withQuota installs a fake gate on a freshly built M1 harness proxy.
//
// It reuses newFailoverProxy so the requests travel the real router, breakers
// and accounting path. The gate is assigned after construction because
// failoverOptions predates M3; the proxy's own defaults are then restored
// explicitly so an estimate assertion cannot pass just because some option
// happened to be left at its zero value.
func withQuota(t *testing.T, gate *fakeQuotaGate, mutate func(*failoverOptions)) *Proxy {
	t.Helper()
	if mutate == nil {
		mutate = func(*failoverOptions) {}
	}
	opts := failoverOptions{maxAttempts: 1}
	mutate(&opts)

	p, _ := newFailoverProxy(t, opts, nil)
	p.quota = gate
	p.quotaCharsPerToken = defaultQuotaCharsPerToken
	p.quotaCompletionTokens = defaultQuotaCompletionTokens
	return p
}

// postWithHeader sends a POST plus one extra gateway header. mustPostWith takes
// a whole map, which reads badly for the single-header cases below.
func postWithHeader(t *testing.T, handler http.Handler, path, body, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	return mustPostWith(t, handler, path, body, map[string]string{header: value})
}

// errorTypeOf pulls the machine-readable type out of a gateway error envelope.
// Asserting on the type rather than the message is deliberate: the message is
// prose meant for a human and may be reworded, the type is the contract an SDK
// switches on.
func errorTypeOf(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var env ErrorBody
	if err := json.Unmarshal(res.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not the gateway envelope: %v (body %q)", err, res.Body.String())
	}
	return env.Error.Type
}

// bodyField reads one top-level field out of a JSON object body, so a test can
// assert on what the UPSTREAM received (max_tokens as a number, model as a
// string) without pinning the whole serialised request.
func bodyField(t *testing.T, body []byte, field string) (json.RawMessage, bool) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("recorded request body is not a JSON object: %v (body %q)", err, body)
	}
	raw, ok := fields[field]
	return raw, ok
}

// ---------------------------------------------------------------------------
// 1-2. The gate is absent, or present but switched off
// ---------------------------------------------------------------------------

// TestQuotaNilGateIsInvisible is the compatibility case: an operator who never
// configured quota must not be able to tell M3 shipped. A stray
// X-InferGate-Quota: allow on every response would be a header clients could
// start depending on, and then disabling the feature would look like a
// regression.
func TestQuotaNilGateIsInvisible(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	// Quota deliberately left as the zero value: nil.
	p, _ := newFailoverProxy(t, failoverOptions{primary: be.URL, backup: be.URL, maxAttempts: 1}, nil)

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get(HeaderQuota); got != "" {
		t.Errorf("%s = %q with no gate configured, want no header at all", HeaderQuota, got)
	}
	if n := len(be.recorded()); n != 1 {
		t.Errorf("upstream saw %d requests, want 1: a nil gate must not stop traffic", n)
	}
}

// TestQuotaGateDisabledIsNotConsulted pins the Enabled() short-circuit, which
// is what lets a host ship the quota code switched off without paying for a
// store round trip on every request. Asserting only on the header would pass
// even if Admit were still being called and its answer ignored.
func TestQuotaGateDisabledIsNotConsulted(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: false, decision: allowedDecision()}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get(HeaderQuota); got != "" {
		t.Errorf("%s = %q with a disabled gate, want no header", HeaderQuota, got)
	}
	if n := gate.admitCount(); n != 0 {
		t.Errorf("Admit called %d times for a disabled gate, want 0: a disabled gate must be free", n)
	}
}

// ---------------------------------------------------------------------------
// 3. Allow
// ---------------------------------------------------------------------------

// TestQuotaAllowAdvertisesBudget covers the ordinary admitted request, both
// shapes of the budget headers. Limit/Used are only meaningful when the store
// knows a limit (Limit > 0); emitting "-Limit: 0" on an unlimited tenant would
// read as "your budget is zero", which is the opposite of the truth.
func TestQuotaAllowAdvertisesBudget(t *testing.T) {
	cases := []struct {
		name        string
		limit, used int64
		wantHeaders bool
	}{
		{name: "with a known limit", limit: 1000, used: 250, wantHeaders: true},
		{name: "without a configured limit", limit: 0, used: 0, wantHeaders: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := jsonBackend(t, http.StatusOK, chatCompletionBody)
			dec := allowedDecision()
			dec.Limit, dec.Used, dec.Requested = tc.limit, tc.used, 10
			gate := &fakeQuotaGate{enabled: true, decision: dec}
			p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

			res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", res.Code, res.Body)
			}
			if got := res.Header().Get(HeaderQuota); got != quota.ActionAllow {
				t.Errorf("%s = %q, want %q", HeaderQuota, got, quota.ActionAllow)
			}
			if got := res.Header().Get(HeaderQuotaReason); got != quota.ReasonWithinBudget {
				t.Errorf("%s = %q, want %q", HeaderQuotaReason, got, quota.ReasonWithinBudget)
			}

			limit, used := res.Header().Get(HeaderQuotaLimit), res.Header().Get(HeaderQuotaUsed)
			if tc.wantHeaders {
				if limit == "" || used == "" {
					t.Errorf("%s/%s = %q/%q, want both present when the decision carries a limit",
						HeaderQuotaLimit, HeaderQuotaUsed, limit, used)
				}
			} else if limit != "" || used != "" {
				t.Errorf("%s/%s = %q/%q, want neither when the decision carries no limit",
					HeaderQuotaLimit, HeaderQuotaUsed, limit, used)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4-5. Reject
// ---------------------------------------------------------------------------

// TestQuotaRejectIsAnsweredWithoutUpstream is the headline M3 property: a
// tenant over budget is refused by the GATEWAY. Asserting on the 429 alone
// would miss the expensive half of a bug here -- a rejected request that still
// reached a provider would spend the very budget the rejection was protecting.
func TestQuotaRejectIsAnsweredWithoutUpstream(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: true, decision: &quota.Decision{
		Allowed:     false,
		Action:      quota.ActionReject,
		Reason:      quota.ReasonDailyTokens,
		Limit:       100,
		Used:        90,
		Requested:   5,
		RetryAfter:  90 * time.Second,
		Reservation: nil, // a refusal issues no lease
	}}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d (body %s)", res.Code, http.StatusTooManyRequests, res.Body)
	}
	if got := res.Header().Get(HeaderQuota); got != quota.ActionReject {
		t.Errorf("%s = %q, want %q", HeaderQuota, got, quota.ActionReject)
	}
	if got := errorTypeOf(t, res); got != TypeQuotaExceeded {
		t.Errorf("error type = %q, want %q", got, TypeQuotaExceeded)
	}
	if got := res.Header().Get("Retry-After"); got != "90" {
		t.Errorf("Retry-After = %q, want %q: the decision's window must reach the client verbatim", got, "90")
	}
	if n := len(be.recorded()); n != 0 {
		t.Errorf("upstream saw %d requests for a rejected request, want 0", n)
	}
	if n := len(gate.settleCalls()); n != 0 {
		t.Errorf("Settle called %d times for a rejected request, want 0: no lease was issued", n)
	}
	if n := len(gate.releaseCalls()); n != 0 {
		t.Errorf("Release called %d times for a rejected request, want 0: no lease was issued", n)
	}
}

// TestQuotaRejectRetryAfterNeverZero covers the sub-second window.
//
// A Retry-After of "0" tells a client to retry immediately, which produces a
// tight loop against a gateway that is still over budget -- the worst possible
// answer. The ceiling is clamped to at least one second; anything less is not a
// backoff.
func TestQuotaRejectRetryAfterNeverZero(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: true, decision: &quota.Decision{
		Allowed:    false,
		Action:     quota.ActionReject,
		Reason:     quota.ReasonMinuteRPM,
		Limit:      100,
		Used:       100,
		Requested:  1,
		RetryAfter: 200 * time.Millisecond,
	}}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusTooManyRequests)
	}
	got := res.Header().Get("Retry-After")
	if got != "1" {
		t.Errorf("Retry-After = %q for a 200ms window, want %q: a sub-second window must still mean one second", got, "1")
	}
	if n := len(be.recorded()); n != 0 {
		t.Errorf("upstream saw %d requests, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// 6-9. Degrade
// ---------------------------------------------------------------------------

// TestQuotaDegradeRewritesModel checks that a degrade verdict actually reaches
// the provider as a cheaper model name. The client must still see a normal
// completion -- degrade is a silent downgrade, not an error -- carrying the
// headers that let an operator explain why the answer looks different.
func TestQuotaDegradeRewritesModel(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	dec := allowedDecision()
	dec.Action = quota.ActionDegrade
	dec.Reason = quota.ReasonDailyCost
	dec.DowngradeModel = "gpt-4o-mini"
	dec.Limit, dec.Used = 500, 480
	gate := &fakeQuotaGate{enabled: true, decision: dec}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a degrade is not an error (body %s)", res.Code, res.Body)
	}
	if got := res.Header().Get(HeaderQuota); got != quota.ActionDegrade {
		t.Errorf("%s = %q, want %q", HeaderQuota, got, quota.ActionDegrade)
	}
	if got := res.Header().Get(HeaderQuotaModel); got != "gpt-4o-mini" {
		t.Errorf("%s = %q, want %q", HeaderQuotaModel, got, "gpt-4o-mini")
	}

	got := be.recorded()
	if len(got) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(got))
	}
	if got[0].Model != "gpt-4o-mini" {
		t.Errorf("upstream received model = %q, want %q: the degrade must be applied to the body", got[0].Model, "gpt-4o-mini")
	}
}

// TestQuotaDegradeModelMatchIsCaseInsensitive guards an idempotence property:
// a backend that already names the downgrade target must not have its body
// rewritten when the only difference is spelling.
//
// The gateway compares with strings.EqualFold but rewrites with a byte-for-byte
// model assignment, so a naive comparison would re-serialise the whole body
// (reordering every key, changing the bytes the cache key and provider both
// see) for a request that was already on the cheap model.
func TestQuotaDegradeModelMatchIsCaseInsensitive(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	dec := allowedDecision()
	dec.Action = quota.ActionDegrade
	dec.Reason = quota.ReasonDailyCost
	dec.DowngradeModel = "GPT-4O-MINI"
	gate := &fakeQuotaGate{enabled: true, decision: dec}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	const body = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
	res := mustPost(t, p, "/v1/chat/completions", body)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}

	got := be.recorded()
	if len(got) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(got))
	}
	if string(got[0].RawBody) != body {
		t.Errorf("upstream body = %s, want the original bytes %s: an already-cheap model must be left alone",
			got[0].RawBody, body)
	}
}

// TestQuotaDegradeTokenCapLowersCeiling covers the other degrade lever: the
// gateway caps the answer length when the tenant is close to its budget.
//
// Two directions matter and they are different bugs. A cap that does not reach
// the provider does nothing; a cap applied to a request that already asked for
// less would silently REWRITE a caller's conservative choice upward, which is
// the one outcome nobody expects from a budget guard.
func TestQuotaDegradeTokenCapLowersCeiling(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantMaxToken string
	}{
		{
			name:         "lowers an oversized ceiling",
			body:         `{"model":"gpt-4o","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`,
			wantMaxToken: "128",
		},
		{
			name:         "leaves a smaller ceiling alone",
			body:         `{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			wantMaxToken: "64",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := jsonBackend(t, http.StatusOK, chatCompletionBody)
			dec := allowedDecision()
			dec.Action = quota.ActionDegrade
			dec.Reason = quota.ReasonSessionTokens
			dec.MaxTokensCap = 128
			gate := &fakeQuotaGate{enabled: true, decision: dec}
			p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

			res := mustPost(t, p, "/v1/chat/completions", tc.body)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", res.Code)
			}
			if got := res.Header().Get(HeaderQuotaMaxTokens); got != "128" {
				t.Errorf("%s = %q, want %q", HeaderQuotaMaxTokens, got, "128")
			}

			got := be.recorded()
			if len(got) != 1 {
				t.Fatalf("upstream saw %d requests, want 1", len(got))
			}
			raw, ok := bodyField(t, got[0].RawBody, "max_tokens")
			if !ok {
				t.Fatalf("upstream body has no max_tokens: %s", got[0].RawBody)
			}
			if string(raw) != tc.wantMaxToken {
				t.Errorf("upstream max_tokens = %s, want %s (body %s)", raw, tc.wantMaxToken, got[0].RawBody)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 10-12. Settling the lease
// ---------------------------------------------------------------------------

// TestQuotaSettleChargesProviderUsage pins the numbers. A settle that reported
// anything other than the provider's own accounting would let the ledger drift
// away from reality in whichever direction the bug happened to point, and the
// per-minute request count is the dimension that must never be dropped: it is
// the only one that is knowable even when the provider reports no usage.
func TestQuotaSettleChargesProviderUsage(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, `{"id":"chatcmpl-q","object":"chat.completion","model":"gpt-4o",`+
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":37,"completion_tokens":11,"total_tokens":48}}`)
	gate := &fakeQuotaGate{enabled: true, decision: allowedDecision()}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	if res := mustPost(t, p, "/v1/chat/completions", chatRequestBody); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}

	settles := gate.settleCalls()
	if len(settles) != 1 {
		t.Fatalf("Settle called %d times, want exactly 1", len(settles))
	}
	got := settles[0].Usage
	if got.PromptTokens != 37 || got.CompletionTokens != 11 {
		t.Errorf("settled usage = %d/%d prompt/completion tokens, want 37/11 from the provider's usage object",
			got.PromptTokens, got.CompletionTokens)
	}
	if got.Requests != 1 {
		t.Errorf("settled Requests = %d, want 1: every admitted request consumes a slot even when tokens are unknown", got.Requests)
	}
	if settles[0].Res == nil {
		t.Error("Settle received a nil reservation: the lease Admit issued was not the one settled")
	}
	if n := len(gate.releaseCalls()); n != 0 {
		t.Errorf("Release called %d times after a successful attempt, want 0", n)
	}
}

// TestQuotaSettleCacheHitChargesNoTokens drives a REAL cache hit (see
// newTestCache) rather than asserting on the usage shape directly, because the
// interesting bug is in the ordering: settleQuota branches on servedFromCache
// BEFORE it branches on attempts == 0, and a cache hit has attempts == 0. If
// that order were reversed, a replayed answer would be Released instead of
// Settled -- the per-minute slot a hit consumed would vanish from the report --
// and only an end-to-end test through the cache can see it.
//
// A cache hit is free in tokens but not in capacity: no provider was billed, so
// the ledger must record zero tokens and one request.
func TestQuotaSettleCacheHitChargesNoTokens(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: true, decision: allowedDecision()}
	p := withQuota(t, gate, func(o *failoverOptions) {
		o.primary, o.backup = be.URL, be.URL
		o.cache = newTestCache(t)
	})

	first := mustPost(t, p, "/v1/chat/completions", cachePromptBody)
	if got := first.Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Fatalf("%s = %q on the priming request, want %q (body %s)", HeaderCache, got, cacheStatusMiss, first.Body)
	}

	second := mustPost(t, p, "/v1/chat/completions", cachePromptBody)
	if got := second.Header().Get(HeaderCache); got == cacheStatusMiss || got == "" {
		t.Fatalf("%s = %q on the repeated request, want a hit: this case is vacuous otherwise", HeaderCache, got)
	}
	if n := len(be.recorded()); n != 1 {
		t.Fatalf("upstream saw %d requests, want 1: the second request must have been replayed", n)
	}

	settles := gate.settleCalls()
	if len(settles) != 2 {
		t.Fatalf("Settle called %d times, want 2 (one per admitted request, hit included)", len(settles))
	}
	hit := settles[1].Usage
	if hit.PromptTokens != 0 || hit.CompletionTokens != 0 || hit.CostMicros != 0 {
		t.Errorf("cache-hit usage = %d/%d tokens, %d micros, want all zero: a replayed answer bills no provider",
			hit.PromptTokens, hit.CompletionTokens, hit.CostMicros)
	}
	if hit.Requests != 1 {
		t.Errorf("cache-hit Requests = %d, want 1: a hit still occupies the per-minute dimension", hit.Requests)
	}
	if n := len(gate.releaseCalls()); n != 0 {
		t.Errorf("Release called %d times across a miss-plus-hit, want 0: both requests were served", n)
	}
}

// TestQuotaReleaseWhenNoUpstreamAttempted covers the aborted path.
//
// When routing never picks a backend, no provider was asked and no tokens can
// ever be charged, so the lease must be RELEASED. Settling it instead would
// charge the tenant for a request the gateway refused on its own, and never
// settling it would leak the reservation until its window expired.
//
// The unroutable case is reached here with an explicit upstream name that does
// not exist, which makes plan() fail before any attempt is counted -- attempts
// stays 0, which is exactly the condition settleQuota branches on.
func TestQuotaReleaseWhenNoUpstreamAttempted(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: true, decision: allowedDecision()}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	res := postWithHeader(t, p, "/v1/chat/completions", chatRequestBody, HeaderUpstream, "no-such-backend")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: an explicit unknown upstream is the caller's error (body %s)", res.Code, res.Body)
	}
	if n := len(be.recorded()); n != 0 {
		t.Errorf("upstream saw %d requests, want 0", n)
	}

	releases := gate.releaseCalls()
	if len(releases) != 1 {
		t.Fatalf("Release called %d times, want exactly 1", len(releases))
	}
	if releases[0] == nil {
		t.Error("Release received a nil reservation: the lease Admit issued was not the one released")
	}
	if n := len(gate.settleCalls()); n != 0 {
		t.Errorf("Settle called %d times with no upstream attempt, want 0: nothing was billed", n)
	}
}

// ---------------------------------------------------------------------------
// 13-14. Store outages
// ---------------------------------------------------------------------------

// TestQuotaStoreErrorFailsClosed pins the safe answer for a metering outage:
// refuse. A gateway that cannot count cannot prove a request is within budget,
// and admitting traffic it cannot bill is how an unprotected provider account
// gets drained -- the operator opts into the other behaviour explicitly.
func TestQuotaStoreErrorFailsClosed(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: true, admitErr: errQuotaScripted, decision: &quota.Decision{
		Allowed: false,
		Action:  quota.ActionReject,
		Reason:  quota.ReasonStoreError,
	}}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (body %s)", res.Code, http.StatusServiceUnavailable, res.Body)
	}
	if got := errorTypeOf(t, res); got != TypeQuotaStore {
		t.Errorf("error type = %q, want %q", got, TypeQuotaStore)
	}
	if n := len(be.recorded()); n != 0 {
		t.Errorf("upstream saw %d requests while the store was down, want 0: fail-closed must not forward", n)
	}
}

// TestQuotaStoreErrorFailsOpen is the same outage under the opposite policy.
//
// Note the shape: Allowed is TRUE and err is non-nil at the same time. That is
// precisely what quota.Manager returns in fail-open mode, and the gateway must
// read the boolean rather than treat any non-nil error as a refusal -- getting
// this backwards turns a fail-open configuration into a total outage, which is
// the failure mode operators enable fail-open to avoid.
func TestQuotaStoreErrorFailsOpen(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: true, admitErr: errQuotaScripted, decision: &quota.Decision{
		Allowed:     true,
		Action:      quota.ActionAllow,
		Reason:      quota.ReasonStoreError,
		Reservation: &quota.Reservation{},
	}}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: fail-open admits under a store outage (body %s)", res.Code, res.Body)
	}
	if n := len(be.recorded()); n != 1 {
		t.Errorf("upstream saw %d requests, want 1: a fail-open request must reach a provider", n)
	}
}

// ---------------------------------------------------------------------------
// 15. Routes that are not completions
// ---------------------------------------------------------------------------

// TestQuotaSkipsNonCompletionPaths pins the scope of the budget.
//
// A capability listing costs no tokens. Metering it would refuse a caller for
// traffic that is free, and would fill the token dimension with fiction, since
// a models listing carries no prompt.
func TestQuotaSkipsNonCompletionPaths(t *testing.T) {
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})
	gate := &fakeQuotaGate{enabled: true, decision: allowedDecision()}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	res := serve(t, p, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", res.Code, res.Body)
	}
	if n := gate.admitCount(); n != 0 {
		t.Errorf("Admit called %d times for a models listing, want 0", n)
	}
	if got := res.Header().Get(HeaderQuota); got != "" {
		t.Errorf("%s = %q on a non-completion path, want no header", HeaderQuota, got)
	}
	if n := len(be.recorded()); n != 1 {
		t.Errorf("upstream saw %d requests, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// 16. What the gate is asked to admit
// ---------------------------------------------------------------------------

// TestQuotaEstimateSizing checks the numbers the gate sees, which is the whole
// basis of its decision.
//
// The completion side deliberately prefers the client's own ceiling over the
// configured default: a request that says max_tokens: 77 cannot consume more
// than 77 output tokens, so budgeting the default (256) would refuse callers
// for tokens they never asked for. The prompt side is an estimate from the
// body's size, and it must never be zero -- a zero-token estimate makes every
// token budget look infinite -- nor may Requests be anything but 1.
func TestQuotaEstimateSizing(t *testing.T) {
	const ceiling = 77

	withCeiling := `{"model":"gpt-4o","max_tokens":77,"messages":[{"role":"user","content":"hi"}]}`
	withCompletionField := `{"model":"gpt-4o","max_completion_tokens":77,"messages":[{"role":"user","content":"hi"}]}`

	cases := []struct {
		name             string
		body             string
		completionTokens int
	}{
		{name: "max_tokens ceiling wins", body: withCeiling, completionTokens: ceiling},
		{name: "max_completion_tokens ceiling wins", body: withCompletionField, completionTokens: ceiling},
		{name: "no ceiling falls back to the configured default", body: chatRequestBody, completionTokens: defaultQuotaCompletionTokens},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := jsonBackend(t, http.StatusOK, chatCompletionBody)
			gate := &fakeQuotaGate{enabled: true, decision: allowedDecision()}
			p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

			if res := mustPost(t, p, "/v1/chat/completions", tc.body); res.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", res.Code)
			}

			calls := gate.admitCalls()
			if len(calls) != 1 {
				t.Fatalf("Admit called %d times, want 1", len(calls))
			}
			est := calls[0].Est
			if est.CompletionTokens != tc.completionTokens {
				t.Errorf("estimate CompletionTokens = %d, want %d for body %s",
					est.CompletionTokens, tc.completionTokens, tc.body)
			}
			if want := len(tc.body) / defaultQuotaCharsPerToken; est.PromptTokens != want {
				t.Errorf("estimate PromptTokens = %d, want %d (%d body bytes / %d chars per token)",
					est.PromptTokens, want, len(tc.body), defaultQuotaCharsPerToken)
			}
			if est.PromptTokens < 1 {
				t.Errorf("estimate PromptTokens = %d, want at least 1: a zero estimate makes every budget infinite", est.PromptTokens)
			}
			if est.Requests != 1 {
				t.Errorf("estimate Requests = %d, want 1", est.Requests)
			}
			if est.CostMicros < 0 {
				t.Errorf("estimate CostMicros = %d, want non-negative", est.CostMicros)
			}
		})
	}
}

// TestQuotaAdmitSeesTenantAndSession covers the identity handed to the store.
//
// Without it a request could be admitted against the wrong tenant's ledger, and
// the per-session dimension would be evaluated against an empty session. The
// X-InferGate-Tenant header is the operator's explicit override and the session
// header is what makes a conversation-scoped budget possible.
func TestQuotaAdmitSeesTenantAndSession(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: true, decision: allowedDecision()}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	res := mustPostWith(t, p, "/v1/chat/completions", chatRequestBody, map[string]string{
		HeaderTenant:  "acme",
		HeaderSession: "sess-42",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}

	calls := gate.admitCalls()
	if len(calls) != 1 {
		t.Fatalf("Admit called %d times, want 1", len(calls))
	}
	if calls[0].Tenant != "acme" {
		t.Errorf("Admit tenant = %q, want %q", calls[0].Tenant, "acme")
	}
	if calls[0].Session != "sess-42" {
		t.Errorf("Admit session = %q, want %q", calls[0].Session, "sess-42")
	}
}

// ---------------------------------------------------------------------------
// 17-18. Pure helpers
// ---------------------------------------------------------------------------

// TestRewriteMaxTokens exercises the ladder directly.
//
// The rewrites are pure functions on a serialised body, which makes them easy
// to test and easy to get subtly wrong: the field to rewrite depends on which
// spelling the caller used, and adding max_tokens next to an existing
// max_completion_tokens would send a body that some providers reject outright.
func TestRewriteMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		cap  int
		want string
	}{
		{
			name: "lowers max_tokens",
			body: `{"model":"gpt-4o","max_tokens":4096}`,
			cap:  128,
			want: `{"max_tokens":128,"model":"gpt-4o"}`,
		},
		{
			name: "uses max_completion_tokens and does not add max_tokens",
			body: `{"model":"gpt-4o","max_completion_tokens":4096}`,
			cap:  128,
			want: `{"max_completion_tokens":128,"model":"gpt-4o"}`,
		},
		{
			name: "leaves an existing ceiling at or below the cap",
			body: `{"model":"gpt-4o","max_tokens":64}`,
			cap:  128,
			want: `{"model":"gpt-4o","max_tokens":64}`,
		},
		{
			name: "leaves an existing ceiling exactly at the cap",
			body: `{"model":"gpt-4o","max_tokens":128}`,
			cap:  128,
			want: `{"model":"gpt-4o","max_tokens":128}`,
		},
		{
			name: "ignores a non-positive cap",
			body: `{"model":"gpt-4o","max_tokens":4096}`,
			cap:  0,
			want: `{"model":"gpt-4o","max_tokens":4096}`,
		},
		{
			name: "ignores a negative cap",
			body: `{"model":"gpt-4o","max_tokens":4096}`,
			cap:  -1,
			want: `{"model":"gpt-4o","max_tokens":4096}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteMaxTokens([]byte(tc.body), tc.cap)
			if err != nil {
				t.Fatalf("rewriteMaxTokens(%s, %d) = %v, want no error", tc.body, tc.cap, err)
			}
			if string(got) != tc.want {
				t.Errorf("rewriteMaxTokens(%s, %d) = %s, want %s", tc.body, tc.cap, got, tc.want)
			}
		})
	}

	t.Run("empty body is left alone", func(t *testing.T) {
		got, err := rewriteMaxTokens(nil, 128)
		if err != nil {
			t.Fatalf("rewriteMaxTokens(nil, 128) error = %v, want none", err)
		}
		if len(got) != 0 {
			t.Errorf("rewriteMaxTokens(nil, 128) = %s, want empty", got)
		}
	})

	t.Run("non-object body is an error", func(t *testing.T) {
		for _, body := range []string{`[1,2,3]`, `"a string"`, `17`, `not json`} {
			if _, err := rewriteMaxTokens([]byte(body), 128); err == nil {
				t.Errorf("rewriteMaxTokens(%s, 128) = no error, want an error: the body cannot be capped", body)
			} else if !strings.Contains(err.Error(), "not a JSON object") {
				t.Errorf("rewriteMaxTokens(%s, 128) error = %q, want it to name the JSON object requirement", body, err)
			}
		}
	})
}

// TestQuotaEstimatePromptTokensFloor checks the divisor and the floor.
//
// charsPerToken is a configured knob, so the estimate is asserted against
// len(body)/charsPerToken rather than a literal: a hard-coded number here would
// freeze the tokenizer guess and fail the moment an operator tuned it. The
// floor matters because a one-byte body would otherwise estimate zero prompt
// tokens, and a zero-cost request is indistinguishable from a free one.
func TestQuotaEstimatePromptTokensFloor(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: true, decision: allowedDecision()}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })

	const body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	if res := mustPost(t, p, "/v1/chat/completions", body); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}

	calls := gate.admitCalls()
	if len(calls) != 1 {
		t.Fatalf("Admit called %d times, want 1", len(calls))
	}
	want := len(body) / defaultQuotaCharsPerToken
	if want < 1 {
		want = 1
	}
	if got := calls[0].Est.PromptTokens; got != want {
		t.Errorf("estimate PromptTokens = %d, want %d (len(body)=%d / charsPerToken=%d, floored at 1)",
			got, want, len(body), defaultQuotaCharsPerToken)
	}
}

// TestQuotaCharsPerTokenIsHonoured proves the configured divisor is actually
// used. A test that only ever ran with the default 4 could not tell a wired
// knob from a constant, and the knob is how an operator trades estimate
// accuracy against accidental over-budget refusals.
func TestQuotaCharsPerTokenIsHonoured(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: true, decision: allowedDecision()}
	p := withQuota(t, gate, func(o *failoverOptions) { o.primary, o.backup = be.URL, be.URL })
	p.quotaCharsPerToken = 1

	const body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	if res := mustPost(t, p, "/v1/chat/completions", body); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}

	calls := gate.admitCalls()
	if len(calls) != 1 {
		t.Fatalf("Admit called %d times, want 1", len(calls))
	}
	if got := calls[0].Est.PromptTokens; got != len(body) {
		t.Errorf("estimate PromptTokens = %d, want %d: chars_per_token was ignored", got, len(body))
	}
}

// TestTenantForFoldsSchemeCase pins the identity a budget is charged against.
//
// The Authorization header doubles as the tenant identity when no explicit
// tenant header is set, so a caller whose HTTP client spells the scheme
// differently would otherwise be charged twice and would miss its own cache
// entries. The canonical spelling must keep deriving the key it derived before
// the folding existed, or every deployed cache scope and ledger would split.
func TestTenantForFoldsSchemeCase(t *testing.T) {
	request := func(auth string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		return r
	}

	canonical := tenantFor(request("Bearer sk-abc"))
	for _, variant := range []string{"bearer sk-abc", "BEARER sk-abc", "BeArEr sk-abc"} {
		if got := tenantFor(request(variant)); got != canonical {
			t.Errorf("tenantFor(%q) = %q, want %q: the scheme case split one credential into two tenants",
				variant, got, canonical)
		}
	}

	// The token itself stays opaque: two different credentials are two tenants
	// no matter how the scheme is spelled, and a token that happens to contain
	// spaces is still hashed whole.
	if got := tenantFor(request("Bearer sk-abd")); got == canonical {
		t.Errorf("tenantFor with a different token = %q, want a different tenant", got)
	}
	if got, want := tenantFor(request("sk-abc")), tenantFor(request("sk-abc")); got != want {
		t.Errorf("tenantFor with no scheme is not stable: %q vs %q", got, want)
	}
	if got := tenantFor(request("")); got != "anonymous" {
		t.Errorf("tenantFor with no credential = %q, want %q", got, "anonymous")
	}
}
