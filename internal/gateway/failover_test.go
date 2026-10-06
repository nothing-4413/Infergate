package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/cache"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/router"
	"github.com/infergate/infergate/internal/upstream"
)

// ---------------------------------------------------------------------------
// M1 harness: a two-backend fleet driven through the real router and breakers.
// ---------------------------------------------------------------------------

// chatCompletionBody is a well-formed non-streaming answer a test backend can
// return. It carries usage so the accounting path stays exercised.
const chatCompletionBody = `{"id":"chatcmpl-x","object":"chat.completion","model":"gpt-4o",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`

// failoverOptions describes a fleet for the M1 tests.
type failoverOptions struct {
	// primary and backup are the two backends, primary first in the file.
	primary, backup string
	// strategy defaults to priority.
	strategy string
	// primaryPriority/backupPriority default to 1 and 2 so primary is preferred.
	primaryPriority, backupPriority int
	// primaryModels/backupModels default to the catch-all.
	primaryModels, backupModels []string
	// primaryCapabilities/backupCapabilities default to none declared.
	primaryCapabilities, backupCapabilities []string
	// maxAttempts defaults to 2.
	maxAttempts int
	// health overrides the whole HealthConfig when non-zero.
	health config.HealthConfig
	// cache, when set, is installed on the proxy (M2 tests reuse this fleet so
	// that a cache hit is proven against the real router and breakers, not
	// against a stub that would never have been called anyway).
	cache *cache.Cache
}

func testHealthConfig() config.HealthConfig {
	return config.HealthConfig{
		Window:                config.Duration(time.Minute),
		Buckets:               6,
		MinRequests:           3,
		FailureRatio:          0.5,
		OpenDuration:          config.Duration(30 * time.Second),
		HalfOpenProbes:        1,
		MaxFailuresPerRequest: 2,
		RetryBackoff:          config.Duration(time.Millisecond),
	}
}

// newFailoverProxy builds the full M1 stack: registry, breakers, router and
// proxy. Nothing is stubbed, because the bugs this layer has already produced
// (a consumed half-open probe, a 503 mistaken for an answer already sent) only
// exist in the interaction between those four pieces.
func newFailoverProxy(t *testing.T, opts failoverOptions, rec metrics.Sink) (*Proxy, *breaker.Group) {
	t.Helper()

	models := func(m []string) []string {
		if len(m) == 0 {
			return []string{"/"}
		}
		return m
	}
	prio := func(v, fallback int) int {
		if v == 0 {
			return fallback
		}
		return v
	}
	strategy := opts.strategy
	if strategy == "" {
		strategy = config.StrategyPriority
	}
	health := opts.health
	if health.MinRequests == 0 {
		health = testHealthConfig()
	}
	maxAttempts := opts.maxAttempts
	if maxAttempts == 0 {
		maxAttempts = 2
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:              ":0",
			UpstreamTimeout:     config.Duration(5 * time.Second),
			MaxBodyBytes:        1 << 20,
			MaxIdleConnsPerHost: 4,
		},
		Upstreams: []config.UpstreamConfig{
			{
				Name:         "primary",
				Kind:         config.KindOpenAI,
				BaseURL:      opts.primary,
				APIKey:       "primary-key",
				Models:       models(opts.primaryModels),
				Priority:     prio(opts.primaryPriority, 1),
				Capabilities: opts.primaryCapabilities,
			},
			{
				Name:         "backup",
				Kind:         config.KindOpenAI,
				BaseURL:      opts.backup,
				APIKey:       "backup-key",
				Models:       models(opts.backupModels),
				Priority:     prio(opts.backupPriority, 2),
				Capabilities: opts.backupCapabilities,
			},
		},
		Routing: config.RoutingConfig{Strategy: strategy},
		Health:  health,
	}

	registry, err := upstream.New(cfg)
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	t.Cleanup(registry.CloseIdleConnections)

	breakers := breaker.NewGroup(cfg.Health, registry.Names())
	priceBook := NewPriceBook(cfg.Pricing)
	rt := router.New(router.Options{
		Registry: registry,
		Breakers: breakers,
		Routing:  cfg.Routing,
		Health:   cfg.Health,
		Price: func(model string) (float64, bool) {
			p, ok := priceBook.Price(model)
			return (p.In + p.Out) / 2, ok
		},
	})

	return New(Options{
		Upstreams:       registry,
		Pricing:         priceBook,
		Metrics:         rec,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
		UpstreamTimeout: cfg.Server.UpstreamTimeout.Duration(),
		Router:          rt,
		Breakers:        breakers,
		MaxAttempts:     maxAttempts,
		RetryBackoff:    cfg.Health.RetryBackoff.Duration(),
		Cache:           opts.cache,
	}), breakers
}

// jsonBackend answers every request with the same non-streaming completion,
// optionally after reporting a status first.
func jsonBackend(t *testing.T, status int, body string) *backend {
	t.Helper()
	return newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(body))
	})
}

// ---------------------------------------------------------------------------
// Failover
// ---------------------------------------------------------------------------

// TestFailoverOn5xx is the M1 headline: the preferred backend is down, so the
// request is answered by the next candidate and the answer is a clean one.
func TestFailoverOn5xx(t *testing.T) {
	primary := jsonBackend(t, http.StatusServiceUnavailable,
		`{"error":{"message":"upstream is restarting","type":"server_error"}}`)
	backup := jsonBackend(t, http.StatusOK, chatCompletionBody)

	rec := metrics.NewRecorder()
	p, breakers := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: backup.URL}, rec)

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 from the backup", res.Code, res.Body.String())
	}
	if res.Body.String() != chatCompletionBody {
		t.Errorf("body = %q, want the backup's completion", res.Body.String())
	}
	if got := res.Header().Get(HeaderUpstreamName); got != "backup" {
		t.Errorf("%s = %q, want backup", HeaderUpstreamName, got)
	}
	if got := res.Header().Get("X-InferGate-Tried"); got != "primary, backup" {
		t.Errorf("%s = %q, want the failover path", "X-InferGate-Tried", got)
	}
	if got := res.Header().Get(HeaderAttempt); got != "2" {
		t.Errorf("%s = %q, want 2 attempts", HeaderAttempt, got)
	}

	if n := len(primary.recorded()); n != 1 {
		t.Errorf("primary saw %d requests, want 1", n)
	}
	if n := len(backup.recorded()); n != 1 {
		t.Errorf("backup saw %d requests, want 1", n)
	}

	// One request, two upstream attempts: the failed attempt must be visible
	// per-backend or the tripped breaker's evidence would be missing.
	// One client request must record evidence against EACH backend it tried, and
	// that evidence is what the breaker consumes. Asserting it on the shared
	// window is stronger than counting metrics rows: it proves the failed attempt
	// reached the machinery that decides to stop using the backend.
	primaryWindow := breakers.Stats("primary").Snapshot()
	if primaryWindow.Attempts != 1 || primaryWindow.Failures != 1 || primaryWindow.Successes != 0 {
		t.Errorf("primary window = %+v, want exactly one recorded failure", primaryWindow)
	}
	backupWindow := breakers.Stats("backup").Snapshot()
	if backupWindow.Attempts != 1 || backupWindow.Successes != 1 {
		t.Errorf("backup window = %+v, want exactly one recorded success", backupWindow)
	}
	if fails := rec.FailoverSnapshot(); len(fails) != 1 || fails[0].Upstream != "primary" || fails[0].Status != http.StatusServiceUnavailable {
		t.Errorf("failover metric = %+v, want one entry for primary with status 503", fails)
	}
}

// TestNoFailoverOn4xx is the decision that keeps the gateway from burning a
// second provider's quota on a request that is simply wrong.
func TestNoFailoverOn4xx(t *testing.T) {
	const envelope = `{"error":{"message":"model not found","type":"invalid_request_error"}}`
	primary := jsonBackend(t, http.StatusBadRequest, envelope)
	backup := jsonBackend(t, http.StatusOK, chatCompletionBody)

	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: backup.URL}, metrics.Nop{})

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the provider's 400", res.Code)
	}
	if res.Body.String() != envelope {
		t.Errorf("body = %q, want the provider envelope verbatim", res.Body.String())
	}
	if n := len(backup.recorded()); n != 0 {
		t.Errorf("backup saw %d requests: a 4xx must not be retried", n)
	}
}

// TestFailoverOn429 covers the status that is about the moment, not the request.
func TestFailoverOn429(t *testing.T) {
	primary := jsonBackend(t, http.StatusTooManyRequests,
		`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	backup := jsonBackend(t, http.StatusOK, chatCompletionBody)

	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: backup.URL}, metrics.Nop{})

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the backup after a 429", res.Code)
	}
	if got := res.Header().Get(HeaderUpstreamName); got != "backup" {
		t.Errorf("%s = %q, want backup", HeaderUpstreamName, got)
	}
}

// TestAllCandidatesFailForwardsTheProviderError checks that when nothing works,
// the caller still sees a real provider status and envelope rather than a
// synthesized 502 -- an SDK's backoff logic depends on the status code.
func TestAllCandidatesFailForwardsTheProviderError(t *testing.T) {
	const envelope = `{"error":{"message":"backend melting","type":"server_error"}}`
	primary := jsonBackend(t, http.StatusServiceUnavailable, envelope)
	backup := jsonBackend(t, http.StatusBadGateway, envelope)

	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: backup.URL}, metrics.Nop{})

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want the LAST provider's 502 rather than a generic gateway error", res.Code)
	}
	if res.Body.String() != envelope {
		t.Errorf("body = %q, want the last provider's envelope", res.Body.String())
	}
	if n := len(primary.recorded()); n != 1 {
		t.Errorf("primary saw %d requests, want 1", n)
	}
	if n := len(backup.recorded()); n != 1 {
		t.Errorf("backup saw %d requests, want 1", n)
	}
}

// TestMaxAttemptsCapsTheFailoverBudget: with a fleet of three, maxAttempts=1
// must never try a second backend.
func TestMaxAttemptsCapsTheFailoverBudget(t *testing.T) {
	primary := jsonBackend(t, http.StatusServiceUnavailable, `{"error":{"message":"down"}}`)
	backup := jsonBackend(t, http.StatusOK, chatCompletionBody)

	p, _ := newFailoverProxy(t, failoverOptions{
		primary: primary.URL, backup: backup.URL, maxAttempts: 1,
	}, metrics.Nop{})

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the primary's 503 with no failover budget", res.Code)
	}
	if n := len(backup.recorded()); n != 0 {
		t.Errorf("backup saw %d requests, want 0 with maxAttempts=1", n)
	}
}

// TestTimeoutFailsOverWithoutAnswering504ForTheClient pins the fix for the bug
// that made a gateway deadline look like a sent answer: writing the 504 before
// retrying would leave the client with a 4xx and no body while a healthy backup
// was still available.
func TestTimeoutFailsOverWithoutAnswering504ForTheClient(t *testing.T) {
	primary := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(10 * time.Second):
		case <-r.Context().Done():
		}
	})
	backup := jsonBackend(t, http.StatusOK, chatCompletionBody)

	cfgHealth := testHealthConfig()
	p, _ := newFailoverProxy(t, failoverOptions{
		primary: primary.URL, backup: backup.URL, health: cfgHealth,
	}, metrics.Nop{})
	// The timeout is a proxy option, not a fleet one, so it is set here.
	//
	// It is deliberately GENEROUS. An earlier version used tens of milliseconds
	// and passed alone but failed in the full suite: the deadline has to cover
	// the second attempt's connection setup, and on a loaded host the first
	// dial to a fresh localhost port can take longer than the whole budget. A
	// deadline the backup can only meet on an idle machine tests the laptop, not
	// the gateway. The first attempt stalls for 10 seconds, so the failover path
	// is still the only way this request can succeed.
	p.upstreamTTL = 2 * time.Second

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), ct=%q, hdr=%+v, want 200 from the backup after the primary timed out",
			res.Code, res.Body.String(), res.Header().Get("Content-Type"),
			map[string][]string(res.Header()))
	}
	if got := res.Header().Get(HeaderUpstreamName); got != "backup" {
		t.Errorf("%s = %q, want backup", HeaderUpstreamName, got)
	}
}

// TestLastCandidateTimeoutAnswers504: when the ONLY candidate times out the
// client must be told, not left hanging on a 200 with an empty body.
func TestLastCandidateTimeoutAnswers504(t *testing.T) {
	primary := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
	})

	p, _ := newFailoverProxy(t, failoverOptions{
		primary: primary.URL, backup: primary.URL, maxAttempts: 1,
	}, metrics.Nop{})
	p.upstreamTTL = 250 * time.Millisecond

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d (%s), want 504", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), TypeTimeout) {
		t.Errorf("body = %q, want the %s error type", res.Body.String(), TypeTimeout)
	}
}

// ---------------------------------------------------------------------------
// Breakers
// ---------------------------------------------------------------------------

// TestBreakerStopsRoutingToADeadBackend is the whole point of M1: after enough
// failures the dead backend must stop receiving traffic at all, which is what
// turns a per-request retry cost into a one-time detection cost.
func TestBreakerStopsRoutingToADeadBackend(t *testing.T) {
	var primaryHits, backupHits atomic.Int64
	primary := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	})
	backup := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionBody))
	})

	health := testHealthConfig()
	health.MinRequests = 3
	p, breakers := newFailoverProxy(t, failoverOptions{
		primary: primary.URL, backup: backup.URL, health: health,
	}, metrics.Nop{})

	// Each request contributes two attempts: one failure on primary, one success
	// on backup. Three requests take primary past MinRequests with a 100% failure
	// ratio, which trips it.
	for i := 0; i < 3; i++ {
		res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
		if res.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 from the backup", i+1, res.Code)
		}
	}
	if got := breakers.Get("primary").State(); got != breaker.StateOpen {
		t.Fatalf("primary breaker = %s after 3/3 failures, want open", got)
	}
	hitsAfterTripping := primaryHits.Load()

	// A fourth request must not touch the dead backend at all.
	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the backup with primary tripped", res.Code)
	}
	if got := primaryHits.Load(); got != hitsAfterTripping {
		t.Errorf("primary received %d extra requests while its breaker was open", got-hitsAfterTripping)
	}
	if got := backupHits.Load(); got != 4 {
		t.Errorf("backup served %d requests, want 4", got)
	}
}

// TestBreakerHalfOpenAdmitsOneProbeAndRecovers walks the full recovery path,
// which is where the consumed-probe bug lived: the router must be able to RANK
// a tripped backend without stealing the single probe the proxy then needs.
func TestBreakerHalfOpenAdmitsOneProbeAndRecovers(t *testing.T) {
	var healthy atomic.Bool
	primary := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if healthy.Load() {
			_, _ = w.Write([]byte(chatCompletionBody))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	})
	// The alternative is healthy at first, so primary is the only failing
	// backend; it is then taken down to force the plan to reach the tripped
	// backend again.
	var backupDown atomic.Bool
	backup := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if backupDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"also down"}}`))
			return
		}
		_, _ = w.Write([]byte(chatCompletionBody))
	})

	health := testHealthConfig()
	health.MinRequests = 2
	health.OpenDuration = config.Duration(50 * time.Millisecond)
	health.HalfOpenProbes = 1
	health.RetryBackoff = config.Duration(0)
	p, breakers := newFailoverProxy(t, failoverOptions{
		primary: primary.URL, backup: backup.URL, health: health,
	}, metrics.Nop{})

	for i := 0; i < 2; i++ {
		if res := mustPost(t, p, "/v1/chat/completions", chatRequestBody); res.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 from the backup", i+1, res.Code)
		}
	}
	if got := breakers.Get("primary").State(); got != breaker.StateOpen {
		t.Fatalf("primary breaker = %s, want open", got)
	}

	// The first request after the cooldown must be able to reach primary, and
	// that is only possible when nothing healthier is ahead of it: the router
	// deliberately ranks an open backend LAST rather than deleting it, so a
	// healthy alternative always shadows it. Take the alternative down, which is
	// exactly the production case this guarantee exists for -- the preferred
	// backend is failing, and the one that was tripped earlier gets its chance.
	healthy.Store(true)
	backupDown.Store(true)
	time.Sleep(60 * time.Millisecond)
	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get(HeaderUpstreamName); got != "primary" {
		t.Errorf("%s = %q, want the recovered primary to answer", HeaderUpstreamName, got)
	}
	if got := breakers.Get("primary").State(); got != breaker.StateClosed {
		t.Fatalf("primary breaker = %s after a successful probe, want closed", got)
	}
}

// TestCanceledProbeGivesTheHalfOpenSlotBack is the end-to-end half of the
// release: the probe is admitted, the caller disappears before the backend
// answers, and the NEXT request must still be able to reach that backend.
//
// The path it covers is the one that reports no verdict -- a canceled caller is
// not evidence about the backend -- so the half-open slot has to be handed back
// explicitly. Without that, primary is refused with "half-open probe already in
// flight" for the life of the process and every request is answered by the
// backup: a backend that was slow once is dropped for good.
func TestCanceledProbeGivesTheHalfOpenSlotBack(t *testing.T) {
	const (
		modeFailing int32 = iota
		modeHanging
		modeHealthy
	)
	var mode atomic.Int32
	mode.Store(modeFailing)
	probeStarted := make(chan struct{}, 1)

	primary := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case modeHanging:
			// Announce the attempt, then wait for the caller to vanish. The
			// proxy cancels this context when the client disconnects or the
			// caller's own context is canceled.
			select {
			case probeStarted <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		case modeHealthy:
			_, _ = w.Write([]byte(chatCompletionBody))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
		}
	})
	// The backup is healthy first (so the failing primary can be reached and
	// tripped), then down, so the plan has to fall through to primary.
	var backupDown atomic.Bool
	backup := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if backupDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"also down"}}`))
			return
		}
		_, _ = w.Write([]byte(chatCompletionBody))
	})

	health := testHealthConfig()
	health.MinRequests = 2
	health.OpenDuration = config.Duration(50 * time.Millisecond)
	health.HalfOpenProbes = 1
	health.RetryBackoff = config.Duration(0)
	p, breakers := newFailoverProxy(t, failoverOptions{
		primary: primary.URL, backup: backup.URL, health: health,
	}, metrics.Nop{})

	for i := 0; i < 2; i++ {
		if res := mustPost(t, p, "/v1/chat/completions", chatRequestBody); res.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 from the backup", i+1, res.Code)
		}
	}
	if got := breakers.Get("primary").State(); got != breaker.StateOpen {
		t.Fatalf("primary breaker = %s, want open", got)
	}

	mode.Store(modeHanging)
	backupDown.Store(true)
	time.Sleep(60 * time.Millisecond)

	// The probe: primary is admitted for its single half-open attempt and never
	// answers, because the caller goes away first.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatRequestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer caller-key")
	req = req.WithContext(ctx)
	served := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		served <- rec
	}()

	select {
	case <-probeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never reached primary: the half-open slot was not usable at all")
	}
	cancel()
	res := <-served
	if res.Body.Len() != 0 {
		t.Errorf("a canceled caller received %d bytes of body: %q", res.Body.Len(), res.Body.String())
	}

	// The decisive assertion: primary must still be reachable. If the canceled
	// probe kept the slot, this request is refused and the client gets a 502
	// naming a breaker state the backend never earned.
	mode.Store(modeHealthy)
	res = mustPost(t, p, "/v1/chat/completions", chatRequestBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 from primary", res.Code, strings.TrimSpace(res.Body.String()))
	}
	if got := res.Header().Get(HeaderUpstreamName); got != "primary" {
		t.Fatalf("%s = %q, want primary: a canceled probe must not keep the backend out for good",
			HeaderUpstreamName, got)
	}
	if got := breakers.Get("primary").State(); got != breaker.StateClosed {
		t.Fatalf("primary breaker = %s after a successful probe, want closed", got)
	}
}

// TestBackendReceivesItsOwnModelName checks the per-candidate rewrite: a
// failover to a backend that declares a concrete model must not leak the
// caller's alias, which is the most common 404 in local inference setups.
func TestBackendReceivesItsOwnModelName(t *testing.T) {
	primary := jsonBackend(t, http.StatusServiceUnavailable, `{"error":{"message":"down"}}`)
	backup := jsonBackend(t, http.StatusOK, chatCompletionBody)

	p, _ := newFailoverProxy(t, failoverOptions{
		primary:       primary.URL,
		backup:        backup.URL,
		primaryModels: []string{"gpt-4o"},
		backupModels:  []string{"deepseek-chat"},
	}, metrics.Nop{})

	// Only "gpt-4o" is claimed by a concrete backend, so this isolates the
	// primary. Use the alias both can serve instead: make the request for the
	// backup's model, which only backup claims.
	res := mustPost(t, p, "/v1/chat/completions",
		`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	got := backup.recorded()
	if len(got) != 1 {
		t.Fatalf("backup saw %d requests, want 1", len(got))
	}
	if got[0].Model != "deepseek-chat" {
		t.Errorf("backup received model %q, want its declared name", got[0].Model)
	}
}

// TestExplicitPinSkipsThePreferredBackend checks the operator escape hatch end
// to end: X-InferGate-Upstream must reach the named backend even though the
// routing plan would have preferred another one.
func TestExplicitPinSkipsThePreferredBackend(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	backup := jsonBackend(t, http.StatusOK, chatCompletionBody)

	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: backup.URL}, metrics.Nop{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatRequestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderUpstream, "backup")
	res := serve(t, p, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get(HeaderUpstreamName); got != "backup" {
		t.Errorf("%s = %q, want the pinned backend", HeaderUpstreamName, got)
	}
	if n := len(primary.recorded()); n != 0 {
		t.Errorf("primary saw %d requests, want 0 when the caller pinned a backend", n)
	}
}

// TestCapabilityHeaderExcludesABackend checks that a request declaring a
// capability is not answered by a backend that does not advertise it.
func TestCapabilityHeaderExcludesABackend(t *testing.T) {
	withoutTools := jsonBackend(t, http.StatusOK, chatCompletionBody)
	withTools := jsonBackend(t, http.StatusOK, chatCompletionBody)

	cfg := failoverOptions{
		primary:            withoutTools.URL,
		backup:             withTools.URL,
		backupCapabilities: []string{"tools"},
	}
	p, _ := newFailoverProxy(t, cfg, metrics.Nop{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatRequestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderCapabilities, "tools")
	res := serve(t, p, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", res.Code, res.Body.String())
	}
	if got := res.Header().Get(HeaderUpstreamName); got != "backup" {
		t.Errorf("%s = %q, want the only backend that declares tools", HeaderUpstreamName, got)
	}
	if n := len(withoutTools.recorded()); n != 0 {
		t.Errorf("the backend without the capability saw %d requests", n)
	}
}

// TestUnmatchedCapabilityIs400NotADowngrade: answering anyway would produce a
// plausible answer with no tool call, which is far harder to debug than an error.
func TestUnmatchedCapabilityIs400NotADowngrade(t *testing.T) {
	be := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: be.URL, backup: be.URL}, metrics.Nop{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatRequestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderCapabilities, "vision")
	res := serve(t, p, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a capability no backend declares", res.Code)
	}
	if n := len(be.recorded()); n != 0 {
		t.Errorf("a backend was asked anyway: %d requests", n)
	}
}

// TestStreamingFailoverRelaysTheSecondBackend checks that the SSE path is part
// of the failover logic and not a separate code path that forgot about it.
func TestStreamingFailoverRelaysTheSecondBackend(t *testing.T) {
	primary := jsonBackend(t, http.StatusServiceUnavailable, `{"error":{"message":"down"}}`)
	backup := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, line := range []string{frameRole, frameText, frameStop, frameUsage, frameDone} {
			_, _ = io.WriteString(w, line)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})

	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: backup.URL}, metrics.Nop{})

	res := mustPost(t, p, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 from the backup", res.Code, res.Body.String())
	}
	if !strings.HasSuffix(res.Body.String(), frameDone) {
		t.Errorf("stream did not end with the sentinel: %q", res.Body.String())
	}
	if got := res.Header().Get(HeaderUpstreamName); got != "backup" {
		t.Errorf("%s = %q, want the backend that actually streamed", HeaderUpstreamName, got)
	}
}
