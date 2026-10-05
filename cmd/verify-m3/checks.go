package main

// M3 acceptance checks.
//
// The harness below is deliberately the same shape as cmd/verify-m2's: a
// checker that records one line per assertion, an environment that can start a
// real server on a free port over a real in-process upstream, and a stack whose
// Close unwinds everything. Keeping the two gates shaped alike is what makes
// "run the acceptance test" a single habit rather than two.
//
// The one structural difference is that every check here needs a tenant with a
// budget, so the stacks are configured per check through a quota section rather
// than through one shared config.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/logging"
	"github.com/infergate/infergate/internal/mockbackend"
	"github.com/infergate/infergate/internal/mockredis"
	"github.com/infergate/infergate/internal/quota"
	"github.com/infergate/infergate/internal/server"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

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

func (c *checker) info(format string, args ...any) { fmt.Fprintf(c.out, "         "+format+"\n", args...) }
func (c *checker) tally() (int, int)               { return c.pass + c.fail, c.fail }

type environment struct {
	out     io.Writer
	verbose bool
}

// stack is one running gateway, the upstream behind it, and the counter store
// when it lives outside the process. A fresh stack per check is deliberate:
// budgets are per tenant and per day, so reusing a stack would let an earlier
// check's spend refuse a later check's request.
type stack struct {
	url     string
	srv     *server.Server
	backend *mockbackend.Backend
	quota   *quota.Manager
	redis   *mockredis.Server
	close   func()
}

// The numbers every governed request settles to. They are the in-repo mock
// upstream's usage block (internal/mockbackend), and the ledger arithmetic of
// this gate is derived from them: an assertion that hard-codes a token count is
// asserting the mock's contract, so it is named once here and referred to
// everywhere else.
const (
	mockPromptTokens     = 7
	mockCompletionTokens = 2
	mockSettledTokens    = mockPromptTokens + mockCompletionTokens
)

// defaultCompletionReservation mirrors the gateway's estimator: a request that
// names no completion ceiling reserves this many completion tokens.
const defaultCompletionReservation = 256

// charsPerToken is the shipped quota.estimate_chars_per_token.
const charsPerToken = 4

// maxDayTTLSeconds bounds any plausible day-window TTL: the window is the time
// to the next UTC midnight with an hour of slack, so this is the ceiling of
// "about a day" without pinning the assertion to a wall clock.
const maxDayTTLSeconds = 90000

// ---------------------------------------------------------------------------
// Configuration builders
// ---------------------------------------------------------------------------

// tenantPolicy is the shape every check starts from. A zero on any field means
// "this dimension is unbudgeted", which is why each check sets only what it is
// about: a check that means to exercise the token budget must not accidentally
// be exercising the request rate as well.
type tenantPolicy struct {
	tenant          string
	tokensPerDay    int64
	costPerDayUSD   float64
	requestsPerMin  int64
	tokensPerSess   int64
	onExceed        string
	downgradeModel  string
	maxTokensCap    int
	anomalyRatio    float64
	hasAnomalyRatio bool
}

func (t tenantPolicy) toConfig() config.TenantQuotaConfig {
	out := config.TenantQuotaConfig{
		Tenant:            t.tenant,
		TokensPerDay:      t.tokensPerDay,
		CostPerDayUSD:     t.costPerDayUSD,
		RequestsPerMinute: t.requestsPerMin,
		TokensPerSession:  t.tokensPerSess,
		OnExceed:          t.onExceed,
		DowngradeModel:    t.downgradeModel,
		MaxTokensCap:      t.maxTokensCap,
	}
	if t.hasAnomalyRatio {
		out.AnomalyRatio = t.anomalyRatio
	}
	return out
}

// quotaConfig turns policies into an enabled quota section. Store defaults to
// memory; the Redis checks override it.
func quotaConfig(policies ...tenantPolicy) config.QuotaConfig {
	q := config.Defaults().Quota
	q.Enabled = true
	q.FailOpen = false
	for _, p := range policies {
		q.Tenants = append(q.Tenants, p.toConfig())
	}
	return q
}

// ---------------------------------------------------------------------------
// Stacks
// ---------------------------------------------------------------------------

// baseConfig is the shipped shape of a config that reaches one in-process
// upstream. It mirrors cmd/verify-m2's stack so the two gates exercise the same
// assembly, and it leaves quota disabled for the caller to turn on.
//
// The upstream is named by URL rather than by a *mockbackend.Backend because
// two of the M3 checks need an upstream that is not the in-repo mock: one whose
// usage block can be dialled up past the reservation (to make an overshoot
// observable, which the mock's fixed 7/2 usage can never produce) and one that
// keeps the raw request body (to prove a cap was actually rewritten into it).
func baseConfig(baseURL string) config.Config {
	return baseConfigModels(baseURL, []string{"/"})
}

// baseConfigModels is baseConfig with an explicit upstream model list. The
// catch-all "/" is what every other check wants; the list matters for the one
// check that has to make routing itself fail, which cannot happen while the
// upstream claims to serve everything.
func baseConfigModels(baseURL string, models []string) config.Config {
	cfg := config.Defaults()
	cfg.Server.UpstreamTimeout = config.Duration(30 * time.Second)
	cfg.Server.MaxBodyBytes = 8 << 20
	cfg.Log.Level = "error"
	cfg.Upstreams = []config.UpstreamConfig{{
		Name:         "primary",
		Kind:         config.KindOpenAI,
		BaseURL:      baseURL,
		Models:       models,
		Capabilities: []string{"chat"},
		Priority:     1,
	}}
	// The price book is what the cost dimension is computed from. The numbers
	// are the shipped ones: 1 USD per million prompt tokens, 3 per million
	// completion tokens. A check that asserts a cost figure derives it from
	// these rather than hard-coding a micro-dollar amount.
	cfg.Pricing = config.PricingConfig{
		Default: config.ModelPrice{In: 1, Out: 3},
		Models: map[string]config.ModelPrice{
			"mock-gpt":      {In: 1, Out: 3},
			"mock-gpt-mini": {In: 0.15, Out: 0.6},
		},
	}
	return cfg
}

func newLogger(c *checker) *slog.Logger {
	logger, err := logging.New(io.Discard, "error", "text")
	if err != nil {
		c.assert(false, "build logger: %v", err)
		return nil
	}
	return logger
}

// stackSpec describes the upstream a stack should reach. An empty baseURL means
// "start the in-repo mock upstream", which is what every check that only counts
// requests wants. A recorder is an upstream the gate itself can read back: its
// usage block is configurable, and it keeps the raw request bodies it was sent.
type stackSpec struct {
	quota    config.QuotaConfig
	baseURL  string
	models   []string
	recorder *recordingUpstream
}

// newStack starts a gateway with the given quota section, over its own mock
// upstream and its own free port. Every check builds a fresh one, so no check
// can be made to pass by another check's traffic.
func (e *environment) newStack(c *checker, q config.QuotaConfig) *stack {
	return e.start(c, stackSpec{quota: q})
}

// newRecorderStack starts a gateway whose upstream is the recording one. Two
// checks need it and they need it for opposite reasons: the degrade-by-cap check
// cannot see a rewritten max_tokens through the in-repo mock (whose Call record
// keeps the model and nothing else), and the overshoot check cannot make the
// ledger overshoot at all while the upstream insists on reporting 9 tokens.
func (e *environment) newRecorderStack(c *checker, q config.QuotaConfig, promptTokens, completionTokens int) (*stack, *recordingUpstream) {
	rec := newRecorder(c, promptTokens, completionTokens)
	st := e.start(c, stackSpec{quota: q, baseURL: rec.URL(), recorder: rec})
	return st, rec
}

func (e *environment) start(c *checker, spec stackSpec) *stack {
	var back *mockbackend.Backend
	baseURL := spec.baseURL
	if spec.recorder == nil && baseURL == "" {
		back = mockbackend.New(mockbackend.Options{Name: "primary"})
		baseURL = back.URL
	}
	models := spec.models
	if len(models) == 0 {
		models = []string{"/"}
	}
	cfg := baseConfigModels(baseURL, models)
	cfg.Quota = spec.quota

	logger := newLogger(c)
	if logger == nil {
		if back != nil {
			back.Close()
		}
		if spec.recorder != nil {
			spec.recorder.Close()
		}
		return nil
	}
	srv, err := server.NewServer(&cfg, logger)
	if err != nil {
		c.assert(false, "build server: %v", err)
		if back != nil {
			back.Close()
		}
		if spec.recorder != nil {
			spec.recorder.Close()
		}
		return nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.assert(false, "listen: %v", err)
		if back != nil {
			back.Close()
		}
		if spec.recorder != nil {
			spec.recorder.Close()
		}
		_ = srv.Shutdown(context.Background())
		return nil
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()

	return &stack{
		url:     "http://" + ln.Addr().String(),
		srv:     srv,
		backend: back,
		close: func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(ctx)
			_ = srv.Shutdown(ctx)
			_ = srv.CloseCache()
			_ = srv.CloseQuota()
			if back != nil {
				back.Close()
			}
			if spec.recorder != nil {
				spec.recorder.Close()
			}
		},
	}
}

// newRedisStack starts the gateway against an in-process RESP2 server. The
// Redis path is not a different policy: it is the same policy behind a
// different Store, which is exactly why it is asserted through the same HTTP
// path and the same admin surface as the memory store.
func (e *environment) newRedisStack(c *checker, q config.QuotaConfig) *stack {
	mini := mockredis.New(mockredis.Options{Addr: "127.0.0.1:0"})
	if err := mini.Start(); err != nil {
		c.assert(false, "start the in-repo RESP2 server: %v", err)
		return nil
	}
	q.Store = config.QuotaStoreRedis
	q.Redis.Addr = mini.Addr()
	if q.Redis.Prefix == "" {
		q.Redis.Prefix = config.Defaults().Quota.Redis.Prefix
	}

	st := e.newStack(c, q)
	if st == nil {
		_ = mini.Close()
		return nil
	}
	st.redis = mini
	inner := st.close
	st.close = func() {
		inner()
		_ = mini.Close()
	}
	return st
}

// ---------------------------------------------------------------------------
// Recording upstream
// ---------------------------------------------------------------------------

// recordedCall is one request as the recording upstream saw it. MaxTokens is the
// field the degrade-by-cap check turns on: a cap that the gateway advertises in
// a header but never writes into the outgoing body is a promise it does not
// keep, and only the body can tell the two apart.
type recordedCall struct {
	Model     string
	MaxTokens int
	Raw       string
}

// recordingUpstream answers like the in-repo mock but keeps what it was sent and
// reports a usage block the check chooses. Both are deliberate departures from
// internal/mockbackend, whose Call record is model-only and whose usage is fixed
// at 7 prompt + 2 completion tokens; neither would let this gate observe a
// rewrite or an overshoot.
type recordingUpstream struct {
	srv        *httptest.Server
	mu         sync.Mutex
	seen       []recordedCall
	prompt     int
	completion int
}

func newRecorder(c *checker, promptTokens, completionTokens int) *recordingUpstream {
	u := &recordingUpstream{prompt: promptTokens, completion: completionTokens}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", u.chat)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	u.srv = httptest.NewServer(mux)
	return u
}

func (u *recordingUpstream) chat(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.Model == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"model is required","type":"invalid_request_error"}}`))
		return
	}
	u.mu.Lock()
	u.seen = append(u.seen, recordedCall{Model: req.Model, MaxTokens: req.MaxTokens, Raw: string(raw)})
	u.mu.Unlock()

	p, comp := u.prompt, u.completion
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-recorder",
		"object":  "chat.completion",
		"model":   req.Model,
		"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "recorded answer"}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": p, "completion_tokens": comp, "total_tokens": p + comp},
	})
}

func (u *recordingUpstream) URL() string { return u.srv.URL }
func (u *recordingUpstream) Close()      { u.srv.Close() }

func (u *recordingUpstream) Calls() []recordedCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]recordedCall, len(u.seen))
	copy(out, u.seen)
	return out
}

func (u *recordingUpstream) Count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.seen)
}

func (u *recordingUpstream) Last() (recordedCall, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.seen) == 0 {
		return recordedCall{}, false
	}
	return u.seen[len(u.seen)-1], true
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

type result struct {
	status int
	body   string
	header http.Header
}

func post(c *checker, base, path, body string, hdr map[string]string) result {
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		c.assert(false, "build request: %v", err)
		return result{header: http.Header{}}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		c.assert(false, "request %s: %v", path, err)
		return result{header: http.Header{}}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, body: string(data), header: resp.Header.Clone()}
}

func chat(c *checker, base, body string, hdr map[string]string) result {
	return post(c, base, "/v1/chat/completions", body, hdr)
}

func get(c *checker, url string) result {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		c.assert(false, "GET %s: %v", url, err)
		return result{header: http.Header{}}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, body: string(data), header: resp.Header.Clone()}
}

func getJSON(c *checker, url string, v any) bool {
	res := get(c, url)
	if res.status != http.StatusOK {
		c.assert(false, "GET %s: status %d", url, res.status)
		return false
	}
	if err := json.Unmarshal([]byte(res.body), v); err != nil {
		c.assert(false, "GET %s: decode: %v (body=%s)", url, err, truncate(res.body, 300))
		return false
	}
	return true
}

// chatBody builds a governed chat request. It never names max_tokens, so the
// reservation is the gateway's own default completion estimate - the shape a
// caller that does not think about budgets sends.
func chatBody(c *checker, model, prompt string) string {
	body, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		c.assert(false, "marshal a chat body: %v", err)
		return ""
	}
	return string(body)
}

// chatBodyWithCeiling builds a request that names its own completion ceiling.
// It is what makes the reservation predictable: the gateway reserves the
// caller's ceiling, so a check can compute the exact reservation instead of
// reading it back from the thing under test.
func chatBodyWithCeiling(c *checker, model, prompt string, maxTokens int) string {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		c.assert(false, "marshal a chat body: %v", err)
		return ""
	}
	return string(body)
}

func tenantHdr(tenant string) map[string]string {
	return map[string]string{"X-InferGate-Tenant": tenant}
}

func sessionHdr(tenant, session string) map[string]string {
	return map[string]string{"X-InferGate-Tenant": tenant, "X-InferGate-Session": session}
}

// pinnedHdr asks the gateway to use one named backend. Pinning to a name that
// does not exist is the only deterministic way to reach the request path that
// reserves a budget and then finds no candidate at all, i.e. the path where the
// reservation must be given back rather than settled.
func pinnedHdr(tenant, upstream string) map[string]string {
	return map[string]string{"X-InferGate-Tenant": tenant, "X-InferGate-Upstream": upstream}
}

func actionOf(res result) string      { return res.header.Get("X-InferGate-Quota") }
func reasonOf(res result) string      { return res.header.Get("X-InferGate-Quota-Reason") }
func limitOf(res result) string       { return res.header.Get("X-InferGate-Quota-Limit") }
func usedOf(res result) string        { return res.header.Get("X-InferGate-Quota-Used") }
func modelOf(res result) string       { return res.header.Get("X-InferGate-Quota-Model") }
func capOf(res result) string         { return res.header.Get("X-InferGate-Quota-Max-Tokens") }
func retryAfterOf(res result) string  { return res.header.Get("Retry-After") }

// errorType reads the OpenAI-shaped error envelope's type field. The type is
// what a client switches on, so it is asserted instead of the message text.
func errorType(body string) string {
	var doc struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return ""
	}
	return doc.Error.Type
}

func errorMessage(body string) string {
	var doc struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return ""
	}
	return doc.Error.Message
}

// ---------------------------------------------------------------------------
// Admin surface helpers
// ---------------------------------------------------------------------------

// quotaReport is the per-tenant report that /admin/quota adds when it is asked
// about one tenant. It is its own type so a check can hold on to a report after
// the document it came from is out of scope.
type quotaReport struct {
	Tenant             string  `json:"tenant"`
	Day                string  `json:"day"`
	TokensToday        int64   `json:"tokens_today"`
	CostTodayMicros    int64   `json:"cost_today_micros"`
	CostTodayUSD       float64 `json:"cost_today_usd"`
	Minute             string  `json:"minute"`
	RequestsThisMinute int64   `json:"requests_this_minute"`
	Session            string  `json:"session"`
	SessionTokens      int64   `json:"session_tokens"`
	BaselineTokens     float64 `json:"baseline_tokens"`
	Ratio              float64 `json:"ratio"`
	Alerting           bool    `json:"alerting"`
	Policy             struct {
		Tenant       string `json:"tenant"`
		TokensPerDay int64  `json:"tokens_per_day"`
	} `json:"policy"`
}

// adminQuotaDoc is the wire shape of GET /admin/quota. It is declared here
// rather than reusing internal/server's own struct on purpose: the acceptance
// test has to notice a field that was renamed or removed, which it cannot do by
// sharing the producer's type.
type adminQuotaDoc struct {
	Enabled  bool   `json:"enabled"`
	Store    string `json:"store"`
	FailOpen bool   `json:"fail_open"`
	Config   struct {
		EstimateCompletionTokens int     `json:"estimate_completion_tokens"`
		EstimateCharsPerToken    int     `json:"estimate_chars_per_token"`
		AnomalyRatio             float64 `json:"anomaly_ratio"`
		DefaultPolicy            struct {
			Tenant         string `json:"tenant"`
			TokensPerDay   int64  `json:"tokens_per_day"`
			RequestsPerMin int64  `json:"requests_per_minute"`
			OnExceed       string `json:"on_exceed"`
			Unbounded      bool   `json:"unbounded"`
		} `json:"default_policy"`
		Tenants []struct {
			Tenant         string  `json:"tenant"`
			TokensPerDay   int64   `json:"tokens_per_day"`
			CostPerDayUSD  float64 `json:"cost_per_day_usd"`
			RequestsPerMin int64   `json:"requests_per_minute"`
			TokensPerSess  int64   `json:"tokens_per_session"`
			OnExceed       string  `json:"on_exceed"`
			DowngradeModel string  `json:"downgrade_model"`
			MaxTokensCap   int     `json:"max_tokens_cap"`
			Unbounded      bool    `json:"unbounded"`
			Limits         struct {
				TokensPerDay     bool `json:"tokens_per_day"`
				CostPerDay       bool `json:"cost_per_day"`
				RequestsPerMin   bool `json:"requests_per_minute"`
				TokensPerSession bool `json:"tokens_per_session"`
			} `json:"limits"`
		} `json:"tenants"`
	} `json:"config"`
	Stats struct {
		Allowed            uint64 `json:"allowed"`
		Degraded           uint64 `json:"degraded"`
		Rejected           uint64 `json:"rejected"`
		StoreErrors        uint64 `json:"store_errors"`
		Alerts             uint64 `json:"alerts"`
		ReservedTokens     uint64 `json:"reserved_tokens"`
		SettledTokens      uint64 `json:"settled_tokens"`
		ReleasedTokens     uint64 `json:"released_tokens"`
		OvershootTokens    uint64 `json:"overshoot_tokens"`
		OvershootCostMicro int64  `json:"overshoot_cost_micros"`
		ReleasedCostMicro  int64  `json:"released_cost_micros"`
	} `json:"stats"`
	Report *quotaReport `json:"report"`
}

func adminQ(c *checker, url, tenant, session string) (adminQuotaDoc, bool) {
	q := url + "/admin/quota"
	if tenant != "" {
		q += "?tenant=" + tenant
		if session != "" {
			q += "&session=" + session
		}
	}
	var doc adminQuotaDoc
	if !getJSON(c, q, &doc) {
		return doc, false
	}
	return doc, true
}

// statsQuotaBlock reads the quota block of GET /stats as raw JSON, so a key
// that disappeared is a missing entry rather than a zero.
func statsQuotaBlock(c *checker, url string) (map[string]any, bool) {
	var doc struct {
		Quota map[string]any `json:"quota"`
	}
	if !getJSON(c, url+"/stats", &doc) {
		return nil, false
	}
	return doc.Quota, doc.Quota != nil
}

// ---------------------------------------------------------------------------
// Ledger arithmetic
//
// Every assertion about a ledger number is derived from the estimator the
// gateway documents, not from a magic constant. If the estimator changes these
// fail; if a hand-computed number was pasted in, the assertion would keep
// passing against a ledger that no longer adds up.
// ---------------------------------------------------------------------------

// reservedFor is the reservation the gateway makes for a body that names its own
// completion ceiling: the prompt is the body length divided by chars-per-token
// (never zero), plus the ceiling itself.
func reservedFor(body string, maxTokens int) int64 {
	prompt := int64(len(body) / charsPerToken)
	if prompt < 1 {
		prompt = 1
	}
	return prompt + int64(maxTokens)
}

// reservedDefault is the same for a body that names no ceiling.
func reservedDefault(body string) int64 {
	return reservedFor(body, defaultCompletionReservation)
}

// costMicrosFor is the gateway's pricing.CostUSD rounded to micro-dollars. It
// is duplicated here rather than imported because importing the producer's
// arithmetic would make the assertion agree with the code by construction.
func costMicrosFor(prompt, completion int) int64 {
	usd := (float64(prompt)*1.0 + float64(completion)*3.0) / 1_000_000
	return int64(roundHalfUp(usd * 1_000_000))
}

func roundHalfUp(f float64) float64 {
	if f < 0 {
		return float64(int64(f - 0.5))
	}
	return float64(int64(f + 0.5))
}

// ---------------------------------------------------------------------------
// Settlement helpers
//
// Settlement runs in a deferred hook AFTER the response has been written, so a
// client that reads its own response can still be a hair ahead of the ledger.
// These helpers read a counter until it holds still instead of sleeping for a
// guessed interval: a fixed sleep is either slower than it needs to be or
// flaky, and on a loaded machine it is both.
// ---------------------------------------------------------------------------

// stableTokens polls a tenant's day-token counter until it reports the same
// value several times in a row, and returns the last reading.
func stableTokens(c *checker, url, tenant string) int64 {
	last := int64(-1)
	seen := 0
	for i := 0; i < 200; i++ {
		doc, ok := adminQ(c, url, tenant, "")
		if !ok {
			return 0
		}
		got := int64(-2)
		if doc.Report != nil {
			got = doc.Report.TokensToday
		}
		if got == last {
			seen++
			if seen >= 6 {
				return got
			}
		} else {
			seen = 0
			last = got
		}
		time.Sleep(20 * time.Millisecond)
	}
	return last
}

// awaitMetric polls /metrics until a counter reaches at least want, and returns
// the last reading either way. It is used only where the value is produced by
// the deferred settle, so "not yet" is the normal first reading.
func awaitMetric(c *checker, url, series, labels string, want float64) float64 {
	var got float64
	for i := 0; i < 200; i++ {
		got = metricValue(get(c, url+"/metrics").body, series, labels)
		if got >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	return got
}

// awaitScalarMetric is awaitMetric for a family that carries no labels.
//
// It exists because of a trap this gate fell into once: metricValue builds a
// regex that requires a brace after the series name, so asking it for an
// unlabelled counter matches nothing and returns 0 - which looks exactly like a
// counter that never moved. The two families that report store errors and
// alerts are both unlabelled, so reading them the wrong way hid a real
// behaviour behind a false failure.
func awaitScalarMetric(c *checker, url, series string, want float64) (float64, bool) {
	var got float64
	var present bool
	for i := 0; i < 200; i++ {
		got, present = scalarMetric(get(c, url+"/metrics").body, series)
		if present && got >= want {
			return got, true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return got, present
}

// deadAddr returns a loopback address that refuses connections, by binding a
// port and immediately releasing it. A refused connection is what a dead
// counter store looks like from the client side, and it is deterministic: no
// timeouts, no retries, no clock.
func deadAddr(c *checker) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.assert(false, "bind a scratch port: %v", err)
		return ""
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// ---------------------------------------------------------------------------
// Metrics helpers
// ---------------------------------------------------------------------------

// metricValue sums a counter series whose label set contains the given regex
// fragment, in ANY label order.
//
// Matching a series by name prefix is a silent way to read zero: the exposition
// orders labels per family, so a prefix that assumes one order matches nothing
// and a counter of zero looks exactly like a feature that did nothing. This is
// the same reason cmd/verify-m2 reads its counters this way.
func metricValue(text, series, labels string) float64 {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + `\{[^}]*` + labels + `[^}]*\}\s+([0-9.eE+-]+)\s*$`)
	var total float64
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			total += v
		}
	}
	return total
}

// hasSeries reports whether a single-label counter series is exposed, whatever
// its numeric value.
func hasSeries(text, series, labels string) bool {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + `\{[^}]*` + labels + `[^}]*\}`)
	return re.MatchString(text)
}

// scalarMetric returns a counter with no labels, and whether it was present at
// all. The two are different findings: an absent family is a missing feature,
// and a zero is a feature that did nothing.
func scalarMetric(text, name string) (float64, bool) {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s+([0-9.eE+-]+)\s*$`)
	m := re.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

// hasMetricLine reports whether any line of a series carries a parseable
// number, which is what makes a scrape safe to graph.
func hasMetricLine(text, series string) bool {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + `(\{[^}]*\})?\s+[0-9.eE+-]+\s*$`)
	return re.MatchString(text)
}

// quotaMetricFamilies is every M3 family the exposition must carry when
// governance is on. A dashboard built on a family that silently disappeared
// shows a flat line, which is indistinguishable from "no traffic".
//
// The list is longer than the check names because a family here is an
// exposition FAMILY, not a series: infergate_quota_tokens_total alone carries
// three kinds. The released-spend family is the money side of a released
// reservation and is deliberately its own family rather than a label on the
// token counter, because a token that was never charged and a dollar that was
// never charged are different operational facts.
var quotaMetricFamilies = []string{
	"infergate_quota_decisions_total",
	"infergate_quota_store_errors_total",
	"infergate_quota_alerts_total",
	"infergate_quota_tokens_total",
	"infergate_quota_overshoot_tokens_total",
	"infergate_quota_overshoot_cost_micros_total",
	"infergate_quota_released_cost_micros_total",
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
