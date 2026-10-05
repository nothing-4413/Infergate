package gateway

// M6 replay acceptance, driven through the REAL proxy so that the ordering of
// admission, the cache and the replay store is exercised rather than assumed.
//
// The failure modes this file is designed to catch are all ordering or state
// bugs that a store-level unit test cannot see: a replay that still reaches the
// provider, a conflict that gets answered from the store anyway, a claim that is
// never released so a failed generation becomes permanent, and a replay that
// double-charges a tenant for tokens the provider never generated.

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/idempotency"
	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/sessions"
)

// newReplayStore builds an enabled store with a controllable clock.
func newReplayStore(t *testing.T, opts idempotency.Options) *idempotency.Store {
	t.Helper()
	if opts.SweepInterval == 0 {
		opts.SweepInterval = -1
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := idempotency.New(opts)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newLedger builds a ledger with no janitor.
func newLedger(t *testing.T, opts sessions.Options) *sessions.Ledger {
	t.Helper()
	if opts.SweepInterval == 0 {
		opts.SweepInterval = -1
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	l := sessions.New(opts)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// withM6 installs the M6 subsystems on a single-attempt fleet pointed at be.
//
// maxAttempts is 1 so that "how many times did the provider get asked" is a
// fact about the replay store rather than about the failover chain.
func withM6(t *testing.T, be *backend, store *idempotency.Store, ledger *sessions.Ledger) *Proxy {
	t.Helper()
	p, _ := newFailoverProxy(t, failoverOptions{
		primary:     be.URL,
		backup:      be.URL,
		maxAttempts: 1,
	}, metrics.NewRecorder())
	p.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	p.idempotency = store
	p.sessions = ledger
	return p
}

// keyedPost sends a POST carrying an Idempotency-Key, optionally with a
// caller-supplied request id so that the replay's origin header is checkable.
func keyedPost(t *testing.T, handler http.Handler, path, body, key string, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{HeaderIdempotencyKey: key}
	for k, v := range extra {
		headers[k] = v
	}
	return mustPostWith(t, handler, path, body, headers)
}

// ---------------------------------------------------------------------------
// Replay
// ---------------------------------------------------------------------------

// TestIdempotentReplayAvoidsASecondUpstreamCall is the M6 headline: an agent
// that retries after a timeout receives the first attempt's answer, and the
// provider is asked exactly once.
func TestIdempotentReplayAvoidsASecondUpstreamCall(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, store, nil)

	first := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-1",
		map[string]string{HeaderRequestID: "req-first"})
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200 (body %s)", first.Code, first.Body)
	}
	// The first attempt says "false": this request did the work. The answer
	// itself cannot say whether it was remembered -- that is only known after
	// the body is written -- so the log and the trace carry it instead.
	if got := first.Header().Get(HeaderIdempotentReplay); got != idempotentFalse {
		t.Errorf("%s = %q on the first request, want %q", HeaderIdempotentReplay, got, idempotentFalse)
	}
	if got := first.Header().Get(HeaderIdempotencyKey); got != "op-1" {
		t.Errorf("%s = %q, want the caller's key echoed", HeaderIdempotencyKey, got)
	}
	if st := store.Stats(); st.Stored != 1 {
		t.Errorf("store stored = %d, want 1", st.Stored)
	}

	second := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-1", nil)
	if second.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", second.Code)
	}
	if got := second.Header().Get(HeaderIdempotentReplay); got != idempotentTrue {
		t.Errorf("%s = %q, want %q", HeaderIdempotentReplay, got, idempotentTrue)
	}
	if got := second.Header().Get(HeaderIdempotentOrigin); got != "req-first" {
		t.Errorf("%s = %q, want req-first", HeaderIdempotentOrigin, got)
	}
	if got := second.Header().Get(HeaderUpstreamName); got != idempotencyUpstream {
		t.Errorf("%s = %q, want %q: a replay is not the backend's traffic", HeaderUpstreamName, got, idempotencyUpstream)
	}
	if got := second.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("replay Content-Type = %q, want the stored content type", got)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("replay body = %q, want the recorded body %q", second.Body.String(), first.Body.String())
	}
	if n := len(be.recorded()); n != 1 {
		t.Fatalf("upstream calls = %d, want 1: a replay must not reach the provider", n)
	}
	if st := store.Stats(); st.Hits != 1 || st.Misses != 1 {
		t.Errorf("store stats = %+v, want 1 hit and 1 miss", st)
	}
}

// TestIdempotentReplayOfAStreamedAnswer checks the recorded transcript is
// replayed byte-for-byte, since a streamed answer is the shape an agent
// framework is most likely to retry.
func TestIdempotentReplayOfAStreamedAnswer(t *testing.T) {
	be := streamBackend(t, cacheStreamChunk1, cacheStreamChunk2)
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, store, nil)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	first := keyedPost(t, p, "/v1/chat/completions", body, "op-stream", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first stream status = %d, want 200", first.Code)
	}
	if !strings.Contains(first.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("first stream Content-Type = %q", first.Header().Get("Content-Type"))
	}

	second := keyedPost(t, p, "/v1/chat/completions", body, "op-stream", nil)
	if got := second.Header().Get(HeaderIdempotentReplay); got != idempotentTrue {
		t.Fatalf("%s = %q, want %q", HeaderIdempotentReplay, got, idempotentTrue)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("replayed stream = %q, want %q", second.Body.String(), first.Body.String())
	}
	if got := second.Body.String(); !strings.Contains(got, cacheStreamChunk2) || !strings.Contains(got, "[DONE]") {
		t.Errorf("replayed stream is missing frames: %q", got)
	}
	if n := len(be.recorded()); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
}

// TestIdempotencyKeyIsScopedByTenant proves two callers that both chose "1" are
// not talking about the same operation.
func TestIdempotencyKeyIsScopedByTenant(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, store, nil)

	first := mustPostWith(t, p, "/v1/chat/completions", chatRequestBody, map[string]string{
		HeaderIdempotencyKey: "1", "Authorization": "Bearer tenant-a",
	})
	second := mustPostWith(t, p, "/v1/chat/completions", chatRequestBody, map[string]string{
		HeaderIdempotencyKey: "1", "Authorization": "Bearer tenant-b",
	})
	if got := second.Header().Get(HeaderIdempotentReplay); got != idempotentFalse {
		t.Errorf("%s = %q across tenants, want %q", HeaderIdempotentReplay, got, idempotentFalse)
	}
	if n := len(be.recorded()); n != 2 {
		t.Fatalf("upstream calls = %d, want 2: one tenant's key must not answer another's request", n)
	}
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d/%d, want 200/200", first.Code, second.Code)
	}
}

// ---------------------------------------------------------------------------
// Conflicts
// ---------------------------------------------------------------------------

// TestIdempotentConflictOnADifferentBody: reusing a key for a different request
// is a mistake, not a cache hit.
func TestIdempotentConflictOnADifferentBody(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, store, nil)

	if got := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-2", nil).Code; got != http.StatusOK {
		t.Fatalf("priming status = %d, want 200", got)
	}
	other := `{"model":"gpt-4o","messages":[{"role":"user","content":"a DIFFERENT question"}]}`
	res := keyedPost(t, p, "/v1/chat/completions", other, "op-2", nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", res.Code, res.Body)
	}
	if got := res.Body.String(); !strings.Contains(got, TypeIdempotencyConflict) {
		t.Errorf("error type missing from %q", got)
	}
	if n := len(be.recorded()); n != 1 {
		t.Fatalf("upstream calls = %d, want 1: a conflict must not be sent upstream", n)
	}
	if st := store.Stats(); st.Conflicts != 1 {
		t.Errorf("store conflicts = %d, want 1", st.Conflicts)
	}
}

// TestIdempotentConflictOnADifferentPath pins the path into the request hash:
// the same body posted to a different endpoint is a different operation.
func TestIdempotentConflictOnADifferentPath(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, store, nil)

	if got := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-3", nil).Code; got != http.StatusOK {
		t.Fatalf("priming status = %d, want 200", got)
	}
	res := keyedPost(t, p, "/v1/completions", chatRequestBody, "op-3", nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", res.Code, res.Body)
	}
}

// TestIdempotentInFlightIsAConflict answers a concurrent duplicate with 409
// rather than starting a second generation, and then replays the first answer.
func TestIdempotentInFlightIsAConflict(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionBody))
	})
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, store, nil)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-4", nil)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the backend")
	}

	res := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-4", nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("concurrent duplicate status = %d, want 409 (body %s)", res.Code, res.Body)
	}
	if got := res.Body.String(); !strings.Contains(got, TypeIdempotencyInFlight) {
		t.Errorf("error type missing from %q", got)
	}
	if got := res.Header().Get("Retry-After"); got == "" {
		t.Error("a 409 that asks the caller to retry should say when")
	}

	close(release)
	first := <-firstDone
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", first.Code)
	}
	if n := len(be.recorded()); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}

	// The claim is completed, so the retry is answered rather than refused.
	again := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-4", nil)
	if got := again.Header().Get(HeaderIdempotentReplay); got != idempotentTrue {
		t.Errorf("after the first attempt finished, %s = %q, want %q", HeaderIdempotentReplay, got, idempotentTrue)
	}
	if n := len(be.recorded()); n != 1 {
		t.Fatalf("upstream calls = %d after the replay, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// What must NOT be remembered
// ---------------------------------------------------------------------------

// TestIdempotentFailureIsNotReplayed: a 5xx is "we do not know", so the caller's
// retry must be allowed to run the work again.
func TestIdempotentFailureIsNotReplayed(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"provider exploded","type":"server_error"}}`))
			return
		}
		_, _ = w.Write([]byte(chatCompletionBody))
	})
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, store, nil)

	first := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-5", nil)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want 500 (body %s)", first.Code, first.Body)
	}
	if got := first.Header().Get(HeaderIdempotentStore); got != "" {
		t.Errorf("%s = %q on a failed generation, want it unset", HeaderIdempotentStore, got)
	}

	second := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-5", nil)
	if got := second.Header().Get(HeaderIdempotentReplay); got == idempotentTrue {
		t.Fatalf("%s = %q: a 5xx must not become the permanent answer for a key", HeaderIdempotentReplay, got)
	}
	if second.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", second.Code)
	}
	if n := len(be.recorded()); n != 2 {
		t.Fatalf("upstream calls = %d, want 2: the retry had to run for real", n)
	}
	if st := store.Stats(); st.Aborted != 1 {
		t.Errorf("store aborted = %d, want 1", st.Aborted)
	}
	// And now it IS replayable, because a 2xx was recorded on the retry.
	third := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-5", nil)
	if got := third.Header().Get(HeaderIdempotentReplay); got != idempotentTrue {
		t.Errorf("%s = %q on the third attempt, want %q", HeaderIdempotentReplay, got, idempotentTrue)
	}
}

// TestIdempotentOversizeIsServedButNotRemembered: a large answer is delivered and
// then deliberately forgotten, and the caller is told which of the two happened.
func TestIdempotentOversizeIsServedButNotRemembered(t *testing.T) {
	big := `{"id":"chatcmpl-big","choices":[{"message":{"content":"` + strings.Repeat("x", 512) + `"}}]}`
	be := jsonBackend(t, http.StatusOK, big)
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour, MaxResponseBytes: 64})
	p := withM6(t, be, store, nil)

	first := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-6", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", first.Code)
	}
	if first.Body.String() != big {
		t.Error("an oversize answer must still be delivered in full")
	}
	if got := first.Header().Get(HeaderIdempotentReplay); got != idempotentFalse {
		t.Errorf("%s = %q, want %q on the request that did the work", HeaderIdempotentReplay, got, idempotentFalse)
	}
	if st := store.Stats(); st.Oversize != 1 || st.Stored != 0 {
		t.Errorf("store stats = %+v, want 1 oversize and 0 stored", st)
	}

	second := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-6", nil)
	if got := second.Header().Get(HeaderIdempotentReplay); got != idempotentFalse {
		t.Errorf("%s = %q, want no replay for an answer that was never stored", HeaderIdempotentReplay, got)
	}
	if n := len(be.recorded()); n != 2 {
		t.Fatalf("upstream calls = %d, want 2: the claim had to be released", n)
	}
}

// TestIdempotentKeyOnANonCompletionPathIsIgnored keeps a client that stamps every
// request with a key working, and says so in the response.
func TestIdempotentKeyOnANonCompletionPathIsIgnored(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, `{"object":"list","data":[]}`)
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, store, nil)

	res := keyedPost(t, p, "/v1/models", "", "op-7", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get(HeaderIdempotentStore); got != idempotentSkip {
		t.Errorf("%s = %q, want %q", HeaderIdempotentStore, got, idempotentSkip)
	}
	if st := store.Stats(); st.Lookups != 0 {
		t.Errorf("store lookups = %d, want 0: a listing has nothing to replay", st.Lookups)
	}
}

// TestIdempotencyIsOffWithoutAStore pins the M0-M5 behaviour: an unconfigured
// gateway ignores the header entirely rather than pretending to honour it.
func TestIdempotencyIsOffWithoutAStore(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p := withM6(t, be, nil, nil)

	first := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-8", nil)
	second := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-8", nil)
	for i, res := range []*httptest.ResponseRecorder{first, second} {
		if res.Header().Get(HeaderIdempotentReplay) != "" || res.Header().Get(HeaderIdempotentStore) != "" {
			t.Errorf("request %d reported idempotency headers with no store configured", i+1)
		}
	}
	if n := len(be.recorded()); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
}

// ---------------------------------------------------------------------------
// Ordering and accounting
// ---------------------------------------------------------------------------

// TestReplaySettlesAsOneRequestNoTokens pins the quota meaning of a replay: the
// slot is consumed, the provider tokens are not billed.
func TestReplaySettlesAsOneRequestNoTokens(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	gate := &fakeQuotaGate{enabled: true, decision: allowedDecision()}
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, store, nil)
	p.quota = gate
	p.quotaCharsPerToken = defaultQuotaCharsPerToken
	p.quotaCompletionTokens = defaultQuotaCompletionTokens

	if got := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-9", nil).Code; got != http.StatusOK {
		t.Fatalf("priming status = %d, want 200", got)
	}
	if got := keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-9", nil).Code; got != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", got)
	}

	if n := gate.admitCount(); n != 2 {
		t.Fatalf("admissions = %d, want 2: a replay consumes a request slot", n)
	}
	settles := gate.settleCalls()
	if len(settles) != 2 {
		t.Fatalf("settles = %d, want 2", len(settles))
	}
	if got := settles[0].Usage.PromptTokens; got != 5 {
		t.Errorf("first settle prompt tokens = %d, want the provider's 5", got)
	}
	if got := settles[1].Usage; got.PromptTokens != 0 || got.CompletionTokens != 0 || got.Requests != 1 {
		t.Errorf("replay settle = %+v, want one request and no tokens", got)
	}
}

// TestReplayIsObservedAsItsOwnUpstream keeps the per-backend numbers describing
// real provider traffic.
func TestReplayIsObservedAsItsOwnUpstream(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	rec := metrics.NewRecorder()
	store := newReplayStore(t, idempotency.Options{Capacity: 8, TTL: time.Hour})
	p, _ := newFailoverProxy(t, failoverOptions{primary: be.URL, backup: be.URL, maxAttempts: 1}, rec)
	p.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	p.idempotency = store

	keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-10", nil)
	keyedPost(t, p, "/v1/chat/completions", chatRequestBody, "op-10", nil)

	var replayed, real int
	for _, h := range rec.RequestDurationHistograms() {
		if h.Upstream == idempotencyUpstream {
			replayed += int(h.Count)
		}
		if h.Upstream == "primary" {
			real += int(h.Count)
		}
	}
	if replayed != 1 || real != 1 {
		t.Fatalf("histogram rows = replay %d / primary %d, want 1/1", replayed, real)
	}
}

// ---------------------------------------------------------------------------
// Session ledger
// ---------------------------------------------------------------------------

// echoModelBackend answers with a completion that echoes the model it was asked
// for, which is what a real provider does.
//
// newBackend drains the request body before the handler runs, so the model is
// read back from what the recorder captured rather than from r.Body.
func echoModelBackend(t *testing.T) *backend {
	t.Helper()
	var be *backend
	be = newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		recorded := be.recorded()
		model := ""
		if n := len(recorded); n > 0 {
			model = recorded[n-1].Model
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-echo","object":"chat.completion","model":%q,`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`, model)
	})
	return be
}

// pricedConfig gives the M6 ledger test a price book, because a cost rollup
// asserted against an empty price book would pass for the wrong reason.
func pricedConfig(addr string) *config.Config {
	cfg := oneUpstreamConfig(addr)
	cfg.Pricing = config.PricingConfig{
		Default: config.ModelPrice{In: 1, Out: 3},
		Models: map[string]config.ModelPrice{
			"gpt-4o":        {In: 2.5, Out: 10},
			"deepseek-chat": {In: 0.27, Out: 1.1},
		},
	}
	return cfg
}

// TestSessionsLedgerRollsUpAConversation is the M6 conversational claim: one
// session id, two models, one answerable total.
func TestSessionsLedgerRollsUpAConversation(t *testing.T) {
	be := echoModelBackend(t)
	ledger := newLedger(t, sessions.Options{Capacity: 8, TTL: time.Hour, RecentPerSession: 4})
	p := newTestProxy(t, pricedConfig(be.URL), metrics.NewRecorder())
	p.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	p.sessions = ledger

	first := `{"model":"gpt-4o","messages":[{"role":"user","content":"one"}]}`
	second := `{"model":"deepseek-chat","messages":[{"role":"user","content":"two"}]}`
	for i, body := range []string{first, second} {
		res := mustPostWith(t, p, "/v1/chat/completions", body, map[string]string{
			HeaderSession: "conv-1", "Authorization": "Bearer caller-key",
		})
		if res.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (body %s)", i+1, res.Code, res.Body)
		}
	}

	if got := ledger.Len(); got != 1 {
		t.Fatalf("ledger sessions = %d, want 1", got)
	}
	// Find by conversation id rather than by tenant: the tenant is the hashed
	// credential, and asserting on the hash's spelling would test the hash.
	found := ledger.Find("conv-1")
	if len(found) != 1 {
		t.Fatalf("Find(conv-1) = %d sessions, want 1", len(found))
	}
	s := found[0]
	if s.Tenant == "" || s.Tenant == "anonymous" {
		t.Errorf("tenant = %q, want the credential-derived namespace", s.Tenant)
	}
	if s.Requests != 2 || s.Ok != 2 {
		t.Fatalf("session counters = %+v, want 2 requests, 2 ok", s)
	}
	if s.PromptTokens != 10 {
		t.Errorf("prompt tokens = %d, want 10 (two requests at 5)", s.PromptTokens)
	}
	if len(s.Models) != 2 {
		t.Errorf("models = %+v, want both models the conversation used", s.Models)
	}
	if got := s.Models["deepseek-chat"].Requests; got != 1 {
		t.Errorf("deepseek-chat requests = %d, want 1", got)
	}
	if len(s.Recent) != 2 {
		t.Errorf("recent = %d entries, want 2", len(s.Recent))
	}
	if s.Recent[1].RequestedModel != "deepseek-chat" || s.Recent[1].Model != "deepseek-chat" {
		t.Errorf("recent[1] models = %q/%q, want the requested and served name",
			s.Recent[1].RequestedModel, s.Recent[1].Model)
	}
	if s.CostUSD <= 0 {
		t.Error("cost was not accumulated (the price book prices gpt-4o and deepseek-chat)")
	}
	if got := s.Models["gpt-4o"].CostUSD; got <= 0 {
		t.Errorf("gpt-4o cost = %v, want a per-model cost", got)
	}
	if got := ledger.Stats().NoSessionID; got != 0 {
		t.Errorf("NoSessionID = %d, want 0", got)
	}
}

// TestSessionsLedgerRecordsARequestWithoutASessionID: the request is counted as
// unattributed rather than filed under an invented conversation.
func TestSessionsLedgerRecordsARequestWithoutASessionID(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	ledger := newLedger(t, sessions.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, nil, ledger)

	if got := mustPost(t, p, "/v1/chat/completions", chatRequestBody).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if got := ledger.Len(); got != 0 {
		t.Errorf("ledger sessions = %d, want 0", got)
	}
	if got := ledger.Stats().NoSessionID; got != 1 {
		t.Errorf("NoSessionID = %d, want 1", got)
	}
}

// TestSessionLedgerIgnoresNonCompletionPaths keeps the rollup free of traffic
// that costs nothing.
func TestSessionLedgerIgnoresNonCompletionPaths(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, `{"object":"list","data":[]}`)
	ledger := newLedger(t, sessions.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, nil, ledger)

	mustPostWith(t, p, "/v1/models", "", map[string]string{HeaderSession: "conv-2"})
	if got := ledger.Stats().Recorded; got != 0 {
		t.Errorf("Recorded = %d, want 0 for a listing", got)
	}
	if got := ledger.Len(); got != 0 {
		t.Errorf("ledger sessions = %d, want 0", got)
	}
}

// TestSessionLedgerCapturesTheFailure: a conversation's failures are part of what
// it cost, so a failed request must be attributed rather than dropped.
func TestSessionLedgerCapturesTheFailure(t *testing.T) {
	be := jsonBackend(t, http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`)
	ledger := newLedger(t, sessions.Options{Capacity: 8, TTL: time.Hour})
	p := withM6(t, be, nil, ledger)

	res := mustPostWith(t, p, "/v1/chat/completions", chatRequestBody, map[string]string{
		HeaderSession: "conv-3",
	})
	if res.Code < 500 {
		t.Fatalf("status = %d, want a 5xx from the failed fleet", res.Code)
	}
	found := ledger.Find("conv-3")
	if len(found) != 1 {
		t.Fatalf("Find(conv-3) = %d sessions, want 1", len(found))
	}
	if found[0].Failed != 1 || found[0].Ok != 0 {
		t.Errorf("session counters = %+v, want 1 failed", found[0])
	}
	if found[0].Recent[0].Outcome == "" || found[0].Recent[0].Reason == "" {
		t.Errorf("recent entry = %+v, want the outcome and the reason", found[0].Recent[0])
	}
}
