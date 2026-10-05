package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/gateway"
	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/router"
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
	http     *http.Server
	started  time.Time
	version  string
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
// a runnable server. It performs no I/O, so it can be constructed in tests.
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
	})

	s := &Server{
		cfg:      cfg,
		log:      logger,
		proxy:    proxy,
		registry: registry,
		breakers: breakers,
		router:   routerInst,
		recorder: recorder,
		started:  time.Now(),
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

	// Everything else is the OpenAI-compatible surface. The catch-all must not
	// swallow the exact routes above: Go's ServeMux prefers the more specific
	// pattern, so "/" only sees what nothing else claimed.
	mux.Handle("/", proxy)

	s.http = &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           withAccessControl(mux, cfg.Server.MaxBodyBytes),
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
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upstreams":   out,
		"model_index": s.registry.ModelIndex(),
		"routing": map[string]any{
			"strategy":            s.router.Strategy(),
			"weights":             s.cfg.Routing.Weights,
			"fallback_model":      s.cfg.Routing.FallbackModel,
			"default_capabilities": s.cfg.Routing.DefaultCapabilities,
			"max_attempts":        s.cfg.Health.MaxFailuresPerRequest,
			"retry_backoff":       s.cfg.Health.RetryBackoff.String(),
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
			"window":            s.cfg.Health.Window.String(),
			"min_requests":      s.cfg.Health.MinRequests,
			"failure_ratio":     s.cfg.Health.FailureRatio,
			"open_duration":     s.cfg.Health.OpenDuration.String(),
			"half_open_probes":  s.cfg.Health.HalfOpenProbes,
			"max_attempts":      s.cfg.Health.MaxFailuresPerRequest,
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
			"route":          row.Route,
			"upstream":       row.Upstream,
			"model":          row.Model,
			"status":         row.Status,
			"outcome":        string(row.Outcome),
			"count":          row.Count,
			"mean_seconds":   row.MeanSeconds,
			"total_seconds":  row.TotalSeconds,
		})
	}

	samples := s.recorder.RequestLatencySamples()
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
			"p50":   percentile(samples, 0.50).String(),
			"p90":   percentile(samples, 0.90).String(),
			"p95":   percentile(samples, 0.95).String(),
			"p99":   percentile(samples, 0.99).String(),
			"max":   percentile(samples, 1.0).String(),
		},
		"tokens": map[string]any{
			"prompt":     prompt,
			"completion": completion,
			"cached":     cached,
		},
		"first_token_mean": firstToken,
		"streams":          streams,
		"uptime":           time.Since(s.started).Round(time.Second).String(),
	})
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

	b.WriteString("# HELP infergate_request_duration_seconds_sum Total request time by route, upstream and model.\n")
	b.WriteString("# TYPE infergate_request_duration_seconds_sum counter\n")
	for _, row := range s.recorder.Snapshot() {
		fmt.Fprintf(&b, "infergate_request_duration_seconds_sum{route=%q,upstream=%q,model=%q} %g\n",
			row.Route, row.Upstream, row.Model, row.TotalSeconds)
	}

	b.WriteString("# HELP infergate_tokens_total Provider-reported tokens by upstream and model.\n")
	b.WriteString("# TYPE infergate_tokens_total counter\n")
	for _, row := range s.recorder.TokenSnapshotRows() {
		fmt.Fprintf(&b, "infergate_tokens_total{upstream=%q,model=%q,kind=\"prompt\"} %d\n", row.Upstream, row.Model, row.Prompt)
		fmt.Fprintf(&b, "infergate_tokens_total{upstream=%q,model=%q,kind=\"completion\"} %d\n", row.Upstream, row.Model, row.Completion)
		fmt.Fprintf(&b, "infergate_tokens_total{upstream=%q,model=%q,kind=\"cached\"} %d\n", row.Upstream, row.Model, row.Cached)
		fmt.Fprintf(&b, "infergate_tokens_total{upstream=%q,model=%q,kind=\"requests\"} %d\n", row.Upstream, row.Model, row.Requests)
	}

	b.WriteString("# HELP infergate_stream_frames_total SSE frames relayed downstream.\n")
	b.WriteString("# TYPE infergate_stream_frames_total counter\n")
	for _, row := range s.recorder.StreamSnapshot() {
		fmt.Fprintf(&b, "infergate_stream_frames_total{upstream=%q} %d\n", row.Upstream, row.Frames)
		fmt.Fprintf(&b, "infergate_stream_bytes_total{upstream=%q} %d\n", row.Upstream, row.Bytes)
	}

	b.WriteString("# HELP infergate_first_token_seconds_mean Mean time to first streamed token.\n")
	b.WriteString("# TYPE infergate_first_token_seconds_mean gauge\n")
	for _, row := range s.recorder.FirstTokenSnapshot() {
		fmt.Fprintf(&b, "infergate_first_token_seconds_mean{upstream=%q,model=%q} %g\n", row.Upstream, row.Model, row.Mean.Seconds())
	}

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

	_, _ = w.Write([]byte(b.String()))
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

// percentile returns the p-quantile of samples (p in [0,1]) using the
// nearest-rank method.
//
// Nearest-rank over interpolation because the number an operator acts on is
// "how slow was the 99th user", which is one of the observed requests; an
// interpolated value between two samples is a latency that never occurred.
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
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
