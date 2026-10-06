package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/cache"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/gateway"
	"github.com/infergate/infergate/internal/idempotency"
	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/quota"
	"github.com/infergate/infergate/internal/router"
	"github.com/infergate/infergate/internal/sessions"
	"github.com/infergate/infergate/internal/upstream"
)

// Version is the build version reported by /healthz and the root endpoint.
// It is a var so a release build can set it with -ldflags.
var Version = "0.1.0-m1"

// Server owns the HTTP listener, the routing table and the graceful shutdown
// sequence.
type Server struct {
	cfg      *config.Config
	log      logAdapter
	proxy    *gateway.Proxy
	registry *upstream.Registry
	breakers *breaker.Group
	router   *router.Router
	recorder *metrics.Recorder
	cache    *cache.Cache

	// quota enforces M3 budgets and quotaStore names the counter backend
	// ("memory", "redis" or "disabled") so /admin/quota can report it without
	// taking the config apart.
	quota      *quota.Manager
	quotaStore string

	// trace owns the M5 trace ring and its exporters. It is always non-nil so
	// /admin/traces and /admin/tracing can report the configuration even when
	// tracing is off.
	trace *tracePlane

	// M6 state. Both exist even when disabled so the admin surfaces can report
	// what is configured; only an enabled one is handed to the proxy, so a
	// disabled one costs nothing per request.
	idempotency *idempotency.Store
	sessions    *sessions.Ledger

	// access decides which paths require an operator token. It is computed once
	// at construction (the answer cannot change while the process runs) and is
	// a no-op unless access.tokens is configured. See access.go.
	access accessPolicy

	http    *http.Server
	started time.Time
	version string
}

// SetVersion overrides the version string reported by /healthz, /readyz and
// /metrics. The binary calls it with the value injected at link time; leaving
// it unset keeps the package default.
func (s *Server) SetVersion(v string) {
	if strings.TrimSpace(v) != "" {
		s.version = v
	}
}

// Version reports the version this server announces.
func (s *Server) Version() string {
	if s.version == "" {
		return Version
	}
	return s.version
}

// logAdapter narrows slog to the two methods the server needs, which keeps the
// server's dependency on logging explicit and makes it trivial to substitute in
// tests.
type logAdapter interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
}

// NewServer wires configuration, upstream resolution, metrics and routing into
// a runnable server.
//
// The contract, spelled out because almost every test in this repository leans
// on it:
//
//   - It validates the configuration. The registry build calls cfg.Validate, so
//     a config that cannot serve fails here, at construction, rather than on the
//     first request — which is what lets a caller treat a nil error as "this
//     config is servable" and what keeps a bad config from turning into a
//     runtime mystery three requests later.
//   - It performs no I/O. Nothing listens and nothing is dialled: a
//     Redis-backed cache or budget store is constructed, not connected, so this
//     stays usable from a unit test with no network.
//   - It does not mutate the configuration *unless* validation does. This is a
//     real distinction, not a hedge: cfg.Validate is a normaliser as well as a
//     checker, and it fills unset defaults in place on the pointer it is handed
//     (an empty upstream tier becomes "cloud", a zero weight becomes 1, a zero
//     quota ratio becomes the section default). So the struct the caller passed
//     comes back changed whenever something in it was left unset. It is
//     invisible in production -- Load validates first, so by the time anything
//     calls this the work is already done and a second pass changes nothing --
//     which is exactly why it is worth stating rather than discovering.
//     TestNewServerNormalisesTheConfigItIsGiven pins the shape: unset fields
//     filled, explicitly set fields untouched, and the result a fixed point.
//   - It KEEPS the pointer it was given (s.cfg). The listener address, the
//     admin surfaces' routing/health reports and the handler chain all read from
//     it later, while the router, breakers and cache were built from the values
//     visible at this moment. Configure first, then construct: mutating the
//     config afterwards changes what the admin endpoints *report* without
//     changing what the server *does*, which is a silent inconsistency rather
//     than a configuration change.
func NewServer(cfg *config.Config, logger logAdapter) (*Server, error) {
	registry, err := upstream.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("server: build upstream registry: %w", err)
	}

	recorder := metrics.NewRecorder()
	priceBook := gateway.NewPriceBook(cfg.Pricing)

	// One circuit breaker per configured backend, sharing this process's health
	// window with the router. They are created even when every upstream is
	// healthy: a breaker that is only built after the first failure would leave
	// the first failing request unroutable.
	breakers := breaker.NewGroup(cfg.Health, registry.Names())

	// The router and the proxy must compare costs on the same scale, so the
	// blended price function is derived from the same price book that bills the
	// request. A second, independently configured notion of "expensive" is how a
	// router ends up sending traffic to the backend the cost report says is the
	// priciest.
	price := func(model string) (float64, bool) {
		entry, known := priceBook.Price(model)
		return (entry.In + entry.Out) / 2, known
	}

	routerInst := router.New(router.Options{
		Registry: registry,
		Breakers: breakers,
		Routing:  cfg.Routing,
		Health:   cfg.Health,
		Price:    price,
	})

	// M2: build the cache before the proxy, because a hit is answered without
	// routing anything. Only an enabled cache is handed to the proxy; a
	// disabled one still exists so /admin/cache can report what is configured.
	semanticCache, err := buildCache(cfg, logger)
	if err != nil {
		return nil, err
	}
	var proxyCache *cache.Cache
	if cfg.Cache.Enabled {
		proxyCache = semanticCache
	}

	// M3: budgets are enforced before anything else can spend money, so the
	// manager is built before the proxy and handed to it. A disabled manager
	// still exists so /admin/quota can report the configured budgets.
	quotaManager, quotaStore, err := buildQuota(cfg, logger)
	if err != nil {
		return nil, err
	}
	var proxyQuota gateway.QuotaGate
	if cfg.Quota.Enabled {
		proxyQuota = quotaManager
	}

	// M5: tracing is built before the proxy because the proxy is handed the
	// tracer at construction, and a nil tracer is what makes tracing free when
	// it is off. Unlike the cache and the quota manager, "enabled" also decides
	// whether the ring exists at all.
	traceplane, err := buildTracing(cfg, logger)
	if err != nil {
		return nil, err
	}

	// M6: the replay store and the session ledger. Like the cache and the
	// budget, both are built before the proxy because the proxy is handed them
	// at construction, and a nil store is what makes replay free when it is off.
	idempotencyStore := buildIdempotency(cfg, logger)
	sessionLedger := buildSessions(cfg, logger)
	var proxyIdempotency *idempotency.Store
	if cfg.Idempotency.Enabled {
		proxyIdempotency = idempotencyStore
	}
	var proxySessions *sessions.Ledger
	if cfg.Sessions.Enabled {
		proxySessions = sessionLedger
	}

	proxy := gateway.New(gateway.Options{
		Upstreams:       registry,
		Pricing:         priceBook,
		Metrics:         recorder,
		Logger:          loggerFrom(logger),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
		UpstreamTimeout: cfg.Server.UpstreamTimeout.Duration(),
		Router:          routerInst,
		Breakers:        breakers,
		MaxAttempts:     cfg.Health.MaxFailuresPerRequest,
		RetryBackoff:    cfg.Health.RetryBackoff.Duration(),
		Cache:           proxyCache,
		Quota:           proxyQuota,
		// The estimator lives here because only the gateway holds the request
		// body; these two numbers are how it turns bytes into tokens.
		QuotaCharsPerToken:    cfg.Quota.EstimateCharsPerToken,
		QuotaCompletionTokens: cfg.Quota.EstimateCompletionTokens,
		Tracer:                traceplane.tracer,
		Idempotency:           proxyIdempotency,
		Sessions:              proxySessions,
		Models:                modelInfo(cfg),
	})

	s := &Server{
		cfg:        cfg,
		log:        logger,
		proxy:      proxy,
		registry:   registry,
		breakers:   breakers,
		router:     routerInst,
		recorder:   recorder,
		cache:      semanticCache,
		quota:      quotaManager,
		quotaStore: quotaStore,
		trace:      traceplane,
		// The admin surfaces read these whether or not replay and the ledger
		// are on, so they are the un-narrowed handles.
		idempotency: idempotencyStore,
		sessions:    sessionLedger,
		access:      newAccessPolicy(cfg.Access),
		started:     time.Now(),
	}

	mux := http.NewServeMux()
	// Gateway-owned endpoints.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /admin/upstreams", s.handleUpstreams)
	mux.HandleFunc("GET /admin/breakers", s.handleBreakers)
	mux.HandleFunc("POST /admin/breakers/reset", s.handleBreakerReset)
	mux.HandleFunc("GET /admin/cache", s.handleCache)
	mux.HandleFunc("POST /admin/cache/flush", s.handleCacheFlush)
	mux.HandleFunc("GET /admin/cache/lookup", s.handleCacheLookup)
	mux.HandleFunc("GET /admin/quota", s.handleQuota)
	mux.HandleFunc("GET /admin/traces", s.handleTraces)
	mux.HandleFunc("GET /admin/traces/{id}", s.handleTraceByID)
	mux.HandleFunc("GET /admin/tracing", s.handleTracingConfig)
	mux.HandleFunc("GET /admin/idempotency", s.handleIdempotency)
	mux.HandleFunc("POST /admin/idempotency/flush", s.handleIdempotencyFlush)
	mux.HandleFunc("GET /admin/sessions", s.handleSessions)
	mux.HandleFunc("POST /admin/sessions/flush", s.handleSessionsFlush)
	mux.HandleFunc("GET /admin/sessions/{id}", s.handleSessionByID)
	// M6 capability discovery. /v1/capabilities is a gateway-owned endpoint on
	// the otherwise proxied /v1 surface, which is why it is registered here
	// rather than relayed: no provider serves it.
	mux.HandleFunc("GET /v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("POST /v1/capabilities/probe", s.handleCapabilityProbe)

	// Everything else is the OpenAI-compatible surface. The catch-all must not
	// swallow the exact routes above: Go's ServeMux prefers the more specific
	// pattern, so "/" only sees what nothing else claimed.
	mux.Handle("/", proxy)

	s.http = &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           s.access.middleware(withAccessControl(mux, cfg.Server.MaxBodyBytes)),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration(),
		IdleTimeout:       cfg.Server.IdleTimeout.Duration(),
		// ReadTimeout and WriteTimeout are deliberately unset. Both would cap
		// the whole exchange, and an LLM response legitimately takes minutes;
		// a WriteTimeout would silently truncate a long stream mid-sentence.
		// The controls that matter here are ReadHeaderTimeout (Slowloris), the
		// per-attempt UpstreamTimeout, and IdleTimeout for keep-alive sockets.
	}
	return s, nil
}

// ListenAndServe blocks until the server stops.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("server: listen on %s: %w", s.cfg.Server.Listen, err)
	}
	s.log.Info("infergate listening",
		"addr", ln.Addr().String(),
		"version", s.Version(),
		"upstreams", strings.Join(s.registry.Names(), ","),
	)
	// Said once, at startup, in both directions. An operator who configured a
	// token wants the confirmation that it took effect; one who did not should
	// not have to read the config back to learn that the admin endpoints are
	// open. The token itself is never logged.
	if s.access.enabled {
		s.log.Info("operator token required",
			"paths", strings.Join(s.access.protects, ","),
			"header", s.access.header,
			"tokens", strconv.Itoa(len(s.access.tokens)),
		)
	} else {
		s.log.Warn("no operator token configured; /admin/* and /stats are open to anyone who can reach this port",
			"paths", strings.Join(s.access.protects, ","),
		)
	}
	if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Addr reports the configured listen address.
func (s *Server) Addr() string { return s.cfg.Server.Listen }

// Shutdown drains in-flight requests, then releases pooled upstream
// connections.
//
// The order matters: http.Server.Shutdown waits for handlers to return, and a
// streaming handler only returns when its upstream call finishes. Releasing the
// pool first would abort exactly the requests being drained.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.http.Shutdown(ctx)
	s.registry.CloseIdleConnections()
	return err
}

// Handler exposes the routing table for tests and for embedding InferGate in
// another server.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Breakers exposes the per-upstream circuit breakers.
//
// It is exported for the acceptance verifiers, which must observe a breaker's
// state transitions directly rather than inferring them from response bodies:
// "the request succeeded" and "the request succeeded because the tripped backend
// was skipped" are the same bytes on the wire, and only the breaker can tell
// them apart.
func (s *Server) Breakers() *breaker.Group { return s.breakers }

// handleHealthz is a liveness probe: it must not depend on any downstream, or a
// provider outage would cause an orchestrator to restart a perfectly healthy
// gateway.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.Version(),
		"uptime":  time.Since(s.started).Round(time.Second).String(),
	})
}

// handleReadyz is a readiness probe: it reports whether the gateway can serve
// traffic, which in M0 means "at least one upstream is configured".
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	names := s.registry.Names()
	if len(names) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":  "not ready",
			"reason":  "no upstream configured",
			"version": s.Version(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ready",
		"version":   s.Version(),
		"upstreams": names,
	})
}

// handleUpstreams describes the resolved routing table. It answers the first
// question in any incident: "which backend did this model go to?"
func (s *Server) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	type upstreamView struct {
		Name         string   `json:"name"`
		Kind         string   `json:"kind"`
		BaseURL      string   `json:"base_url"`
		Models       []string `json:"models"`
		CatchAll     bool     `json:"catch_all"`
		HasKey       bool     `json:"has_api_key"`
		Capabilities []string `json:"capabilities"`
		Priority     int      `json:"priority"`
		Weight       float64  `json:"weight"`
		Tier         string   `json:"tier"`
	}
	out := make([]upstreamView, 0, len(s.registry.Names()))
	for _, name := range s.registry.Names() {
		target, ok := s.registry.Target(name)
		if !ok {
			continue
		}
		out = append(out, upstreamView{
			Name:         target.Name,
			Kind:         target.Kind,
			BaseURL:      target.BaseURL,
			Models:       target.ModelPatterns(),
			CatchAll:     target.IsCatchAll(),
			HasKey:       target.APIKey != "",
			Capabilities: target.Capabilities(),
			Priority:     target.Priority(),
			Weight:       target.Weight(),
			Tier:         target.Tier(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upstreams":   out,
		"model_index": s.registry.ModelIndex(),
		"routing": map[string]any{
			"strategy":             s.router.Strategy(),
			"weights":              s.cfg.Routing.Weights,
			"fallback_model":       s.cfg.Routing.FallbackModel,
			"default_capabilities": s.cfg.Routing.DefaultCapabilities,
			"max_attempts":         s.cfg.Health.MaxFailuresPerRequest,
			"retry_backoff":        s.cfg.Health.RetryBackoff.String(),
		},
		"breaker_states": s.breakerStates(),
	})
}

// handleBreakers reports every circuit breaker's state and the windowed
// statistics it is acting on.
//
// The window is included on purpose. A breaker that has tripped is only half the
// story; an operator needs to see whether it tripped on three failures out of
// three requests (a dead backend) or seventy out of two hundred (a backend that
// is degrading under load), because the first is a restart and the second is a
// capacity decision.
func (s *Server) handleBreakers(w http.ResponseWriter, r *http.Request) {
	reports := s.breakers.Reports()
	filter := strings.TrimSpace(r.URL.Query().Get("upstream"))
	if filter != "" {
		kept := make([]breaker.Report, 0, 1)
		for _, rep := range reports {
			if rep.Name == filter {
				kept = append(kept, rep)
			}
		}
		reports = kept
	}

	states := make(map[string]int, 3)
	for _, rep := range reports {
		states[string(rep.State)]++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"upstreams": reports,
		"summary":   states,
		"health": map[string]any{
			"window":           s.cfg.Health.Window.String(),
			"min_requests":     s.cfg.Health.MinRequests,
			"failure_ratio":    s.cfg.Health.FailureRatio,
			"open_duration":    s.cfg.Health.OpenDuration.String(),
			"half_open_probes": s.cfg.Health.HalfOpenProbes,
			"max_attempts":     s.cfg.Health.MaxFailuresPerRequest,
		},
	})
}

// handleBreakerReset closes one breaker (or all of them) and clears its window.
//
// It is a POST because it mutates state, and it is deliberately not exposed as a
// GET: a monitoring system that crawls links would otherwise reset the very
// breakers the operator is looking at.
func (s *Server) handleBreakerReset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("upstream"))
	if name == "" {
		s.breakers.ResetAll()
		s.log.Info("breaker reset", "scope", "all")
		writeJSON(w, http.StatusOK, map[string]any{"reset": "all", "upstreams": s.registry.Names()})
		return
	}
	if _, ok := s.registry.Target(name); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{
				"message": "unknown upstream " + name,
				"type":    "infergate_unknown_upstream",
			},
		})
		return
	}
	s.breakers.Get(name).Reset()
	s.log.Info("breaker reset", "upstream", name)
	writeJSON(w, http.StatusOK, map[string]any{"reset": name})
}

// breakerStates summarises breaker states for the upstream listing.
func (s *Server) breakerStates() map[string]string {
	out := make(map[string]string, len(s.registry.Names()))
	for _, rep := range s.breakers.Reports() {
		out[rep.Name] = string(rep.State)
	}
	return out
}

// handleStats exposes the in-process counters as JSON. /metrics is for
// Prometheus; this is for a human and for the load-test harness, which needs
// machine-readable latency data without a Prometheus scrape.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	snap := s.recorder.Snapshot()
	requests := make([]map[string]any, 0, len(snap))
	var total int64
	for _, row := range snap {
		total += row.Count
		requests = append(requests, map[string]any{
			"route":         row.Route,
			"upstream":      row.Upstream,
			"model":         row.Model,
			"status":        row.Status,
			"outcome":       string(row.Outcome),
			"count":         row.Count,
			"mean_seconds":  row.MeanSeconds,
			"total_seconds": row.TotalSeconds,
		})
	}

	samples := s.recorder.RequestLatencySamples()
	// Sorted once and read five times. The previous shape called percentile()
	// per quantile, and percentile copied and re-sorted the entire sample set on
	// every call, so a single /stats scrape paid for five sorts of a slice that
	// grew with uptime. Sorting here and extracting from the sorted slice keeps
	// the exact same nearest-rank values at a fifth of the work.
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	prompt, completion, cached := s.recorder.TokenTotals()

	firstToken := make([]map[string]any, 0)
	for _, row := range s.recorder.FirstTokenSnapshot() {
		firstToken = append(firstToken, map[string]any{
			"upstream":     row.Upstream,
			"model":        row.Model,
			"count":        row.Count,
			"mean_seconds": row.Mean.Seconds(),
		})
	}

	streams := make([]map[string]any, 0)
	for _, row := range s.recorder.StreamSnapshot() {
		streams = append(streams, map[string]any{
			"upstream": row.Upstream,
			"frames":   row.Frames,
			"bytes":    row.Bytes,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"series":   requests,
		"requests": total,
		"latency": map[string]any{
			"count": len(samples),
			// window and dropped describe the population, not the latency: a
			// p99 over the last 65536 requests is a different claim from a p99
			// since boot, and only these two keys let a dashboard tell them
			// apart.
			"window":  s.recorder.RequestLatencyWindow(),
			"dropped": s.recorder.RequestLatencyDropped(),
			"p50":     percentileSorted(samples, 0.50).String(),
			"p90":     percentileSorted(samples, 0.90).String(),
			"p95":     percentileSorted(samples, 0.95).String(),
			"p99":     percentileSorted(samples, 0.99).String(),
			"max":     percentileSorted(samples, 1.0).String(),
		},
		"tokens": map[string]any{
			"prompt":     prompt,
			"completion": completion,
			"cached":     cached,
		},
		"first_token_mean": firstToken,
		"streams":          streams,
		"cache":            s.statsCache(r.Context()),
		"quota":            s.statsQuota(r.Context()),
		"uptime":           time.Since(s.started).Round(time.Second).String(),
	})
}

// statsQuota renders the M3 governance block for /stats.
//
// It reports the manager's own counters, not the config: during an incident
// the question is "how many requests did we refuse in the last hour", and a
// config echo cannot answer it. The configured budgets live in /admin/quota.
func (s *Server) statsQuota(_ context.Context) map[string]any {
	if s.quota == nil || !s.quota.Enabled() {
		return map[string]any{"enabled": false}
	}
	st := s.quota.Stats()
	return map[string]any{
		"enabled":               true,
		"store":                 s.quotaStore,
		"allowed":               st.Allowed,
		"degraded":              st.Degraded,
		"rejected":              st.Rejected,
		"store_errors":          st.StoreErrors,
		"alerts":                st.Alerts,
		"reserved_tokens":       st.ReservedTokens,
		"settled_tokens":        st.SettledTokens,
		"released_tokens":       st.ReleasedTokens,
		"released_cost_micros":  st.ReleasedCostMicro,
		"overshoot_tokens":      st.OvershootTokens,
		"overshoot_cost_micros": st.OvershootCostMicro,
	}
}

// statsCache renders the M2 cache block for /stats. A nil or disabled cache
// reports {"enabled": false} rather than omitting the key, so a dashboard can
// tell "off" from "not deployed yet".
func (s *Server) statsCache(ctx context.Context) map[string]any {
	if s.cache == nil || !s.cache.Config().Enabled {
		return map[string]any{"enabled": false}
	}
	cs := s.cache.Stats()
	ss := s.cache.Store().Stats()
	return map[string]any{
		"enabled":         true,
		"store":           s.cache.Store().Name(),
		"lookups":         cs.Lookups,
		"hits":            cs.Hits,
		"exact_hits":      cs.ExactHits,
		"semantic_hits":   cs.SemanticHits,
		"misses":          cs.Misses,
		"stores":          cs.Stores,
		"hit_ratio":       hitRate(cs),
		"stale_evictions": cs.StaleEvictions,
		"entries":         cacheEntries(ctx, s.cache),
		"evictions":       ss.Evicted,
		"errors":          cs.LookupErrors + cs.StoreErrors,
		"saved_tokens": map[string]any{
			"prompt":     cs.SavedPromptTokens,
			"completion": cs.SavedCompletionTokens,
			"total":      cs.SavedPromptTokens + cs.SavedCompletionTokens,
		},
	}
}

// cacheEntries counts stored entries across every scope.
//
// The template is deliberately forgiving: this feeds a gauge, and a store that
// is briefly unavailable (Redis restarting) must not turn /metrics into a 500
// when every other number in the response is still valid.
func cacheEntries(ctx context.Context, c *cache.Cache) int {
	n, err := c.Store().Len(ctx, "")
	if err != nil {
		return -1
	}
	return n
}

// handleMetrics renders the counters in Prometheus text exposition format.
//
// Hand-written rather than pulled from client_golang on purpose: M0 has no
// third-party dependencies, and the exposition format is a stable, small
// contract. M5 replaces this with the OpenTelemetry exporter, which is where
// the real integration value lies (exemplars, OTLP push, resource attributes).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var b strings.Builder
	b.WriteString("# HELP infergate_build_info Build information, always 1.\n")
	b.WriteString("# TYPE infergate_build_info gauge\n")
	fmt.Fprintf(&b, "infergate_build_info{version=%q} 1\n", s.Version())

	b.WriteString("# HELP infergate_requests_total Completed requests by route, upstream, model, status and outcome.\n")
	b.WriteString("# TYPE infergate_requests_total counter\n")
	for _, row := range s.recorder.Snapshot() {
		fmt.Fprintf(&b, "infergate_requests_total{route=%q,upstream=%q,model=%q,status=%q,outcome=%q} %d\n",
			row.Route, row.Upstream, row.Model, strconv.Itoa(row.Status), string(row.Outcome), row.Count)
	}

	// Request duration is a histogram, not the `_sum` counter this endpoint used
	// to export on its own. The mean is the one latency statistic that hides the
	// event an operator is hunting for -- a slow tail -- so the buckets come
	// first and `_sum`/`_count` are part of the same family. Keeping the old
	// standalone `_sum` counter alongside would be an invalid duplicate family.
	writeHistogramFamily(&b, "infergate_request_duration_seconds",
		"Request duration by route, upstream and model.",
		[]string{"route", "upstream", "model"},
		requestDurationSeries(s.recorder.RequestDurationHistograms()))

	// Attempt duration, which no earlier export carried. It is what separates a
	// slow backend from a slow gateway: a request that took four seconds because
	// it was retried three times shows one slow request and three fast attempts,
	// and only this family makes that visible.
	writeHistogramFamily(&b, "infergate_upstream_attempt_duration_seconds",
		"Duration of one outbound upstream attempt, including retries.",
		[]string{"upstream"},
		attemptDurationSeries(s.recorder.AttemptDurationHistograms()))

	b.WriteString("# HELP infergate_tokens_total Provider-reported tokens by upstream and model.\n")
	b.WriteString("# TYPE infergate_tokens_total counter\n")
	for _, row := range s.recorder.TokenSnapshotRows() {
		fmt.Fprintf(&b, "infergate_tokens_total{upstream=%q,model=%q,kind=\"prompt\"} %d\n", row.Upstream, row.Model, row.Prompt)
		fmt.Fprintf(&b, "infergate_tokens_total{upstream=%q,model=%q,kind=\"completion\"} %d\n", row.Upstream, row.Model, row.Completion)
		fmt.Fprintf(&b, "infergate_tokens_total{upstream=%q,model=%q,kind=\"cached\"} %d\n", row.Upstream, row.Model, row.Cached)
		fmt.Fprintf(&b, "infergate_tokens_total{upstream=%q,model=%q,kind=\"requests\"} %d\n", row.Upstream, row.Model, row.Requests)
	}

	// Completion length is a distribution, not a total: a fleet average of 300
	// tokens is consistent with every request asking for 300 and with half the
	// fleet asking for 8 while the other half asks for 600, and only the buckets
	// distinguish them. That difference decides whether the batching and the
	// timeout budget are sized right.
	writeHistogramFamily(&b, "infergate_completion_tokens_per_request",
		"Completion tokens produced per request.",
		[]string{"upstream", "model"},
		completionTokensSeries(s.recorder.CompletionTokensHistograms()))

	// Both stream families declare their metadata before either family's samples:
	// the exposition interleaves frames and bytes per upstream, and a Prometheus
	// reader takes the first line it sees for a metric name as the declaration,
	// so a # TYPE emitted between two sample lines would arrive too late.
	b.WriteString("# HELP infergate_stream_frames_total SSE frames relayed downstream.\n")
	b.WriteString("# TYPE infergate_stream_frames_total counter\n")
	b.WriteString("# HELP infergate_stream_bytes_total SSE bytes relayed downstream.\n")
	b.WriteString("# TYPE infergate_stream_bytes_total counter\n")
	for _, row := range s.recorder.StreamSnapshot() {
		fmt.Fprintf(&b, "infergate_stream_frames_total{upstream=%q} %d\n", row.Upstream, row.Frames)
		fmt.Fprintf(&b, "infergate_stream_bytes_total{upstream=%q} %d\n", row.Upstream, row.Bytes)
	}

	// The mean stays a gauge even though the buckets now exist. They are
	// different family names, so both are valid, and they serve different
	// readers: a cheap "is the first token fast" panel wants one number, while a
	// quantile panel wants the buckets and would otherwise have to scan the
	// whole sample set in the query engine.
	b.WriteString("# HELP infergate_first_token_seconds_mean Mean time to first streamed token.\n")
	b.WriteString("# TYPE infergate_first_token_seconds_mean gauge\n")
	for _, row := range s.recorder.FirstTokenSnapshot() {
		fmt.Fprintf(&b, "infergate_first_token_seconds_mean{upstream=%q,model=%q} %g\n", row.Upstream, row.Model, row.Mean.Seconds())
	}
	writeHistogramFamily(&b, "infergate_first_token_seconds",
		"Time to first streamed token by upstream and model.",
		[]string{"upstream", "model"},
		firstTokenSeries(s.recorder.FirstTokenHistograms()))

	// Failover counters. The status label is what separates "we moved off a
	// backend that was rate limiting us" from "we moved off a backend that was
	// down", and those two lead to opposite capacity decisions.
	b.WriteString("# HELP infergate_failovers_total Attempts abandoned in favour of another backend.\n")
	b.WriteString("# TYPE infergate_failovers_total counter\n")
	for _, row := range s.recorder.FailoverSnapshot() {
		fmt.Fprintf(&b, "infergate_failovers_total{upstream=%q,outcome=%q,status=%q} %d\n",
			row.Upstream, string(row.Outcome), strconv.Itoa(row.Status), row.Count)
	}

	// Breaker state is a gauge, not a counter, because it is a current
	// condition: a dashboard alerting on state==open wants the value to return
	// to 0 by itself once the backend recovers, with no reset step.
	b.WriteString("# HELP infergate_breaker_state Current circuit breaker state, as a one-hot gauge.\n")
	b.WriteString("# TYPE infergate_breaker_state gauge\n")
	b.WriteString("# HELP infergate_breaker_trips_total Times a breaker opened since process start.\n")
	b.WriteString("# TYPE infergate_breaker_trips_total counter\n")
	b.WriteString("# HELP infergate_breaker_rejections_total Attempts refused because a breaker was not closed.\n")
	b.WriteString("# TYPE infergate_breaker_rejections_total counter\n")
	b.WriteString("# HELP infergate_breaker_failure_ratio Windowed failure ratio the breaker is acting on.\n")
	b.WriteString("# TYPE infergate_breaker_failure_ratio gauge\n")
	for _, rep := range s.breakers.Reports() {
		for _, state := range []string{string(breaker.StateClosed), string(breaker.StateHalfOpen), string(breaker.StateOpen)} {
			value := 0
			if string(rep.State) == state {
				value = 1
			}
			fmt.Fprintf(&b, "infergate_breaker_state{upstream=%q,state=%q} %d\n", rep.Name, state, value)
		}
		fmt.Fprintf(&b, "infergate_breaker_trips_total{upstream=%q} %d\n", rep.Name, rep.Trips)
		fmt.Fprintf(&b, "infergate_breaker_rejections_total{upstream=%q} %d\n", rep.Name, rep.Rejects)
		fmt.Fprintf(&b, "infergate_breaker_failure_ratio{upstream=%q} %g\n", rep.Name, rep.FailureRatio)
	}

	// M2 cache counters. The hit/miss split is by far the most useful number
	// here: a rising lookup count with a flat hit count means the cache is
	// running but never matching, which looks exactly like a working cache from
	// a request-rate graph.
	if s.cache != nil {
		cs := s.cache.Stats()
		ss := s.cache.Store().Stats()
		fmt.Fprintf(&b, "# HELP infergate_cache_lookups_total Cache lookups performed.\n")
		fmt.Fprintf(&b, "# TYPE infergate_cache_lookups_total counter\n")
		fmt.Fprintf(&b, "# HELP infergate_cache_hits_total Cache hits, by kind.\n")
		fmt.Fprintf(&b, "# TYPE infergate_cache_hits_total counter\n")
		fmt.Fprintf(&b, "# HELP infergate_cache_misses_total Lookups that fell through to an upstream.\n")
		fmt.Fprintf(&b, "# TYPE infergate_cache_misses_total counter\n")
		fmt.Fprintf(&b, "# HELP infergate_cache_stores_total Responses stored for later reuse.\n")
		fmt.Fprintf(&b, "# TYPE infergate_cache_stores_total counter\n")
		fmt.Fprintf(&b, "# HELP infergate_cache_errors_total Cache operations that failed and degraded to an upstream call.\n")
		fmt.Fprintf(&b, "# TYPE infergate_cache_errors_total counter\n")
		fmt.Fprintf(&b, "# HELP infergate_cache_entries Current entry count.\n")
		fmt.Fprintf(&b, "# TYPE infergate_cache_entries gauge\n")
		fmt.Fprintf(&b, "# HELP infergate_cache_evictions_total Entries dropped to stay within the per-scope bound.\n")
		fmt.Fprintf(&b, "# TYPE infergate_cache_evictions_total counter\n")
		fmt.Fprintf(&b, "# HELP infergate_cache_hit_ratio Share of lookups answered from the cache.\n")
		fmt.Fprintf(&b, "# TYPE infergate_cache_hit_ratio gauge\n")
		fmt.Fprintf(&b, "infergate_cache_lookups_total %d\n", cs.Lookups)
		fmt.Fprintf(&b, "infergate_cache_hits_total{kind=\"exact\"} %d\n", cs.ExactHits)
		fmt.Fprintf(&b, "infergate_cache_hits_total{kind=\"semantic\"} %d\n", cs.SemanticHits)
		fmt.Fprintf(&b, "infergate_cache_misses_total %d\n", cs.Misses)
		fmt.Fprintf(&b, "infergate_cache_stores_total %d\n", cs.Stores)
		fmt.Fprintf(&b, "infergate_cache_errors_total{kind=\"lookup\"} %d\n", cs.LookupErrors)
		fmt.Fprintf(&b, "infergate_cache_errors_total{kind=\"store\"} %d\n", cs.StoreErrors)
		fmt.Fprintf(&b, "infergate_cache_entries{store=%q} %d\n", s.cache.Store().Name(), cacheEntries(r.Context(), s.cache))
		fmt.Fprintf(&b, "infergate_cache_evictions_total %d\n", ss.Evicted)
		fmt.Fprintf(&b, "infergate_cache_hit_ratio %g\n", hitRate(cs))
		fmt.Fprintf(&b, "# HELP infergate_cache_saved_tokens_total Provider tokens a cache hit avoided regenerating.\n")
		fmt.Fprintf(&b, "# TYPE infergate_cache_saved_tokens_total counter\n")
		fmt.Fprintf(&b, "infergate_cache_saved_tokens_total{kind=\"prompt\"} %d\n", cs.SavedPromptTokens)
		fmt.Fprintf(&b, "infergate_cache_saved_tokens_total{kind=\"completion\"} %d\n", cs.SavedCompletionTokens)
	}

	// M3 quota counters. rejected against allowed is the ratio an operator
	// alerts on, and overshoot_tokens is what says whether the budgets are
	// actually holding: a reservation is an estimate, so a nonzero and growing
	// overshoot means the estimates are systematically low.
	if s.quota != nil && s.quota.Enabled() {
		qs := s.quota.Stats()
		b.WriteString("# HELP infergate_quota_decisions_total Admission decisions by outcome.\n")
		b.WriteString("# TYPE infergate_quota_decisions_total counter\n")
		b.WriteString("# HELP infergate_quota_store_errors_total Counter operations that failed.\n")
		b.WriteString("# TYPE infergate_quota_store_errors_total counter\n")
		b.WriteString("# HELP infergate_quota_alerts_total Tenant spend anomalies observed.\n")
		b.WriteString("# TYPE infergate_quota_alerts_total counter\n")
		b.WriteString("# HELP infergate_quota_tokens_total Reserved, settled and released token counts.\n")
		b.WriteString("# TYPE infergate_quota_tokens_total counter\n")
		b.WriteString("# HELP infergate_quota_overshoot_tokens_total Tokens spent beyond the reservation.\n")
		b.WriteString("# TYPE infergate_quota_overshoot_tokens_total counter\n")
		b.WriteString("# HELP infergate_quota_overshoot_cost_micros_total Spend beyond the reservation, in micro-dollars.\n")
		b.WriteString("# TYPE infergate_quota_overshoot_cost_micros_total counter\n")
		b.WriteString("# HELP infergate_quota_released_cost_micros_total Reserved spend returned to the tenant, in micro-dollars.\n")
		b.WriteString("# TYPE infergate_quota_released_cost_micros_total counter\n")
		fmt.Fprintf(&b, "infergate_quota_decisions_total{action=\"allow\"} %d\n", qs.Allowed)
		fmt.Fprintf(&b, "infergate_quota_decisions_total{action=\"degrade\"} %d\n", qs.Degraded)
		fmt.Fprintf(&b, "infergate_quota_decisions_total{action=\"reject\"} %d\n", qs.Rejected)
		fmt.Fprintf(&b, "infergate_quota_store_errors_total %d\n", qs.StoreErrors)
		fmt.Fprintf(&b, "infergate_quota_alerts_total %d\n", qs.Alerts)
		fmt.Fprintf(&b, "infergate_quota_tokens_total{kind=\"reserved\"} %d\n", qs.ReservedTokens)
		fmt.Fprintf(&b, "infergate_quota_tokens_total{kind=\"settled\"} %d\n", qs.SettledTokens)
		fmt.Fprintf(&b, "infergate_quota_tokens_total{kind=\"released\"} %d\n", qs.ReleasedTokens)
		fmt.Fprintf(&b, "infergate_quota_overshoot_tokens_total %d\n", qs.OvershootTokens)
		fmt.Fprintf(&b, "infergate_quota_overshoot_cost_micros_total %d\n", qs.OvershootCostMicro)
		fmt.Fprintf(&b, "infergate_quota_released_cost_micros_total %d\n", qs.ReleasedCostMicro)
	}

	// M6 replay and session counters. The host/miss split answers "is anyone
	// actually sending an idempotency key", and the in-flight gauge is what
	// distinguishes a stuck request from a stream of conflicts.
	writeM6Metrics(&b, s.idempotency, s.sessions)

	// ---------------------------------------------------------------------
	// Process facts, not request facts.
	//
	// Everything above describes traffic; this block describes the process
	// serving it. They are in one place because they are read together: a
	// latency regression that arrives with a rising goroutine count or a rising
	// heap is the gateway's problem, and the same regression with a flat process
	// is the upstream's. Hand-written from package runtime rather than a
	// collector library, because this repo takes no third-party dependencies and
	// the exposition is small enough to read.
	// ---------------------------------------------------------------------
	writeRuntimeMetrics(&b)

	_, _ = w.Write([]byte(b.String()))
}

// histogramSeries is one labelled histogram, flattened out of the Recorder so
// that the writer does not need to know which accessor produced it.
type histogramSeries struct {
	labels  []string
	count   int64
	sum     float64
	buckets []metrics.Bucket
}

func requestDurationSeries(rows []metrics.RequestDurationHistogram) []histogramSeries {
	out := make([]histogramSeries, 0, len(rows))
	for _, r := range rows {
		out = append(out, histogramSeries{
			labels: []string{r.Route, r.Upstream, r.Model},
			count:  r.Count, sum: r.Sum, buckets: r.Buckets(),
		})
	}
	return out
}

func attemptDurationSeries(rows []metrics.AttemptDurationHistogram) []histogramSeries {
	out := make([]histogramSeries, 0, len(rows))
	for _, r := range rows {
		out = append(out, histogramSeries{
			labels: []string{r.Upstream},
			count:  r.Count, sum: r.Sum, buckets: r.Buckets(),
		})
	}
	return out
}

func firstTokenSeries(rows []metrics.FirstTokenHistogram) []histogramSeries {
	out := make([]histogramSeries, 0, len(rows))
	for _, r := range rows {
		out = append(out, histogramSeries{
			labels: []string{r.Upstream, r.Model},
			count:  r.Count, sum: r.Sum, buckets: r.Buckets(),
		})
	}
	return out
}

func completionTokensSeries(rows []metrics.CompletionTokensHistogram) []histogramSeries {
	out := make([]histogramSeries, 0, len(rows))
	for _, r := range rows {
		out = append(out, histogramSeries{
			labels: []string{r.Upstream, r.Model},
			count:  r.Count, sum: r.Sum, buckets: r.Buckets(),
		})
	}
	return out
}

// writeHistogramFamily renders one Prometheus histogram family.
//
// The exposition format is strict here, so the shape is written once rather than
// per family: a `_bucket` series per boundary carrying `le`, then `_count` and
// `_sum` derived from the same series. A family with no series still gets its
// HELP and TYPE lines, which is how a scraper learns the metric exists and is
// merely idle -- emitting a bare bucket line with no labels instead would be a
// malformed series.
func writeHistogramFamily(b *strings.Builder, name, help string, labelNames []string, series []histogramSeries) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s histogram\n", name)
	for _, s := range series {
		base := make([]string, 0, len(labelNames))
		for i, ln := range labelNames {
			base = append(base, fmt.Sprintf("%s=%q", ln, s.labels[i]))
		}
		// le is appended per bucket rather than stored, because it is the one
		// label whose value changes line to line.
		withLe := func(le string) string {
			parts := make([]string, len(base), len(base)+1)
			copy(parts, base)
			parts = append(parts, fmt.Sprintf("le=%q", le))
			return strings.Join(parts, ",")
		}
		for _, bk := range s.buckets {
			fmt.Fprintf(b, "%s_bucket{%s} %d\n", name, withLe(bk.Le()), bk.Count)
		}
		fmt.Fprintf(b, "%s_count%s %d\n", name, labelSuffix(base), s.count)
		fmt.Fprintf(b, "%s_sum%s %g\n", name, labelSuffix(base), s.sum)
	}
}

// labelSuffix renders a Prometheus label set, or nothing at all for the
// unlabelled series that a process-level metric produces.
func labelSuffix(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return "{" + strings.Join(labels, ",") + "}"
}

// writeRuntimeMetrics renders the process's own numbers.
//
// Every value is read once, here, rather than lazily per line: a scrape that
// reported goroutines from one instant and heap from another would be
// internally inconsistent, and the whole point of this block is to correlate
// them.
func writeRuntimeMetrics(b *strings.Builder) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	writeProcessMetric(b, "infergate_runtime_goroutines", "gauge",
		"Live goroutines in this process.", fmt.Sprintf("%d", runtime.NumGoroutine()))
	writeProcessMetric(b, "infergate_runtime_num_cpu", "gauge",
		"Logical CPUs available to this process, as runtime.NumCPU sees them.", fmt.Sprintf("%d", runtime.NumCPU()))
	writeProcessMetric(b, "infergate_runtime_memstats_alloc_bytes", "gauge",
		"Bytes of allocated heap objects, including unreachable ones not yet collected.", fmt.Sprintf("%d", ms.Alloc))
	writeProcessMetric(b, "infergate_runtime_memstats_heap_alloc_bytes", "gauge",
		"Bytes of allocated heap objects.", fmt.Sprintf("%d", ms.HeapAlloc))
	writeProcessMetric(b, "infergate_runtime_memstats_heap_inuse_bytes", "gauge",
		"Heap bytes in spans that have at least one live object.", fmt.Sprintf("%d", ms.HeapInuse))
	writeProcessMetric(b, "infergate_runtime_memstats_heap_objects", "gauge",
		"Allocated heap objects.", fmt.Sprintf("%d", ms.HeapObjects))
	writeProcessMetric(b, "infergate_runtime_memstats_stack_inuse_bytes", "gauge",
		"Bytes in stack spans.", fmt.Sprintf("%d", ms.StackInuse))
	writeProcessMetric(b, "infergate_runtime_memstats_sys_bytes", "gauge",
		"Bytes obtained from the OS for the runtime, including unused spans.", fmt.Sprintf("%d", ms.Sys))
	writeProcessMetric(b, "infergate_runtime_memstats_total_alloc_bytes", "counter",
		"Cumulative bytes allocated over the process lifetime, even if freed.", fmt.Sprintf("%d", ms.TotalAlloc))
	writeProcessMetric(b, "infergate_runtime_memstats_gc_cycles_total", "counter",
		"Completed GC cycles since process start.", fmt.Sprintf("%d", ms.NumGC))
	writeProcessMetric(b, "infergate_runtime_gc_pause_seconds_total", "counter",
		"Cumulative stop-the-world GC pause time.", strconv.FormatFloat(float64(ms.PauseTotalNs)/1e9, 'g', -1, 64))

	// PauseNs is a 256-slot ring of the most recent pauses, newest at
	// (NumGC+255)%256. Before the first collection every slot is zero, and the
	// honest answer there is "no pause yet" rather than whichever slot the
	// arithmetic happens to point at.
	lastPause := 0.0
	if ms.NumGC > 0 {
		lastPause = float64(ms.PauseNs[(ms.NumGC+255)%256]) / 1e9
	}
	writeProcessMetric(b, "infergate_runtime_gc_last_pause_seconds", "gauge",
		"Most recent stop-the-world GC pause; 0 before the first collection.",
		strconv.FormatFloat(lastPause, 'g', -1, 64))

	// The Go version is the one metric here with a label: it is a build fact
	// that a fleet-wide dashboard needs to group by when a runtime upgrade rolls
	// out to half the hosts.
	b.WriteString("# HELP infergate_runtime_go_version Go runtime the binary was built against, always 1.\n")
	b.WriteString("# TYPE infergate_runtime_go_version gauge\n")
	fmt.Fprintf(b, "infergate_runtime_go_version{version=%q} 1\n", runtime.Version())
}

// writeProcessMetric emits one single-valued process metric with its HELP and
// TYPE lines. It takes an already-formatted value so that byte counters stay
// exact integers instead of being rounded through a float64.
func writeProcessMetric(b *strings.Builder, name, typ, help, value string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
	fmt.Fprintf(b, "%s %s\n", name, value)
}

// withAccessControl wraps the router with the cross-cutting concerns that
// belong outside any single handler.
func withAccessControl(next http.Handler, maxBody int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject an oversized declared body before reading a byte of it. The
		// proxy also caps while reading, because Content-Length can lie or be
		// absent under chunked encoding.
		if maxBody > 0 && r.ContentLength > maxBody {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "request body exceeds the configured limit",
					"type":    "infergate_request_too_large",
				},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// percentileSorted returns the p-quantile of an ASCENDING slice (p in [0,1])
// using the nearest-rank method.
//
// The caller sorts. A handler that reports five quantiles of the same population
// must not pay for five sorts of it, which is what the previous
// sort-per-quantile helper did.
//
// Nearest-rank over interpolation because the number an operator acts on is
// "how slow was the 99th user", which is one of the observed requests; an
// interpolated value between two samples is a latency that never occurred.
func percentileSorted(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
