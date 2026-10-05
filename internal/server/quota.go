package server

// M3 token and cost governance: construction and the admin surface.
//
// Like the cache, the quota manager is built here rather than inside the
// gateway: it is the component that talks to Redis and owns counters that
// outlive a single request. The gateway receives the manager and calls Admit /
// Settle / Release on it, which keeps the request path free of configuration
// translation and keeps every policy decision testable without a server.

import (
	"fmt"
	"math"
	"net/http"
	"sort"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/quota"
)

// buildQuota constructs the configured quota manager.
//
// A disabled manager still gets an object, so /admin/quota can report what IS
// configured: "is governance on, and what are the budgets" is the first
// question during a spend incident, and answering it must not require reading
// the config file on the host. The manager is simply never handed to the
// gateway, so a disabled one costs nothing per request.
func buildQuota(cfg *config.Config, logger logAdapter) (*quota.Manager, string, error) {
	qc := cfg.Quota

	policies := make(map[string]quota.Policy, len(qc.Tenants))
	for _, t := range qc.Tenants {
		policies[t.Tenant] = quotaPolicy(t)
	}
	def := quotaPolicy(qc.DefaultPolicy)

	cfgQuota := quota.Config{
		Enabled:       qc.Enabled,
		Prefix:        qc.Redis.Prefix,
		FailOpen:      qc.FailOpen,
		DefaultPolicy: def,
		Tenants:       policies,
	}
	if logger != nil {
		cfgQuota.Log = loggerFrom(logger)
	}

	if !qc.Enabled {
		return quota.New(cfgQuota, nil), "disabled", nil
	}

	switch qc.Store {
	case config.QuotaStoreRedis:
		rs, err := quota.NewRedisStore(quota.RedisOptions{
			Addr:         qc.Redis.Addr,
			Password:     qc.Redis.Password,
			DB:           qc.Redis.DB,
			DialTimeout:  qc.Redis.DialTimeout.Duration(),
			ReadTimeout:  qc.Redis.ReadTimeout.Duration(),
			WriteTimeout: qc.Redis.WriteTimeout.Duration(),
			PoolSize:     qc.Redis.PoolSize,
		})
		if err != nil {
			// Same reasoning as the shared cache: a deployment configured for
			// cluster-wide budgets that silently enforced per-process ones
			// would let N replicas each spend the whole budget, and the
			// overspend would be invisible until the invoice arrived.
			return nil, "", fmt.Errorf("server: quota: redis at %s: %w", qc.Redis.Addr, err)
		}
		if logger != nil {
			logger.Info("quota enforcement enabled",
				"store", "redis", "addr", qc.Redis.Addr,
				"tenants", len(policies), "fail_open", qc.FailOpen)
		}
		return quota.New(cfgQuota, rs), "redis", nil
	default:
		if logger != nil {
			logger.Info("quota enforcement enabled",
				"store", "memory", "tenants", len(policies), "fail_open", qc.FailOpen)
		}
		// Memory is the honest store for a single process, and it is the one
		// the test suite uses. It is deliberately reported as such in
		// /admin/quota: budgets enforced in process memory are per-replica
		// budgets, and an operator reading this page needs to know that.
		return quota.New(cfgQuota, quota.NewMemoryStore()), "memory", nil
	}
}

// quotaPolicy translates one configured tenant into a runtime policy.
//
// The config speaks dollars because that is the unit an operator budgets in;
// the manager counts micro-dollars because that is the unit that survives
// integer arithmetic. The conversion happens exactly here.
func quotaPolicy(t config.TenantQuotaConfig) quota.Policy {
	var micros int64
	if t.CostPerDayUSD > 0 {
		micros = int64(math.Round(t.CostPerDayUSD * 1_000_000))
	}
	return quota.Policy{
		Tenant:            t.Tenant,
		TokensPerDay:      t.TokensPerDay,
		CostPerDayMicros:  micros,
		RequestsPerMinute: t.RequestsPerMinute,
		TokensPerSession:  t.TokensPerSession,
		OnExceed:          t.OnExceed,
		DowngradeModel:    t.DowngradeModel,
		MaxTokensCap:      t.MaxTokensCap,
		AnomalyRatio:      t.AnomalyRatio,
	}
}

// handleQuota reports the governance configuration, the counters and the
// per-tenant state.
//
// A budget system is unauditable from the outside otherwise: "the tenant says
// it was refused" and "the tenant says it never got near the limit" are the
// same claim until someone can read the counter that decided it. A ?tenant=
// report is the answer to that, so it is the primary view rather than an extra.
func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	if s.quota == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	stats := s.quota.Stats()
	cfg := s.cfg.Quota

	body := map[string]any{
		"enabled":   s.quota.Enabled(),
		"store":     s.quotaStore,
		"fail_open": cfg.FailOpen,
		"config": map[string]any{
			"estimate_completion_tokens": cfg.EstimateCompletionTokens,
			"estimate_chars_per_token":   cfg.EstimateCharsPerToken,
			"anomaly_ratio":              cfg.AnomalyRatio,
			"default_policy":             quotaPolicyJSON(quotaPolicy(cfg.DefaultPolicy)),
			"tenants":                    quotaTenantsJSON(cfg),
		},
		"stats": map[string]any{
			"allowed":          stats.Allowed,
			"degraded":         stats.Degraded,
			"rejected":         stats.Rejected,
			"store_errors":     stats.StoreErrors,
			"alerts":           stats.Alerts,
			"reserved_tokens":  stats.ReservedTokens,
			"settled_tokens":   stats.SettledTokens,
			"released_tokens":  stats.ReleasedTokens,
			"overshoot_tokens": stats.OvershootTokens,
			// Overshoot is the number that tells an operator how soft the
			// limit really is: a reservation is an estimate, and this is the
			// measured distance between the estimate and what was spent.
			"overshoot_cost_micros": stats.OvershootCostMicro,
			// The money side of the same ledger. Kept out of the token counts
			// because adding micro-dollars to a token total is a unit error.
			"released_cost_micros": stats.ReleasedCostMicro,
		},
	}

	tenant := r.URL.Query().Get("tenant")
	if tenant == "" {
		writeJSON(w, http.StatusOK, body)
		return
	}
	report, err := s.quota.Report(r.Context(), tenant, r.URL.Query().Get("session"))
	if err != nil {
		// The report reads the counters, so a failure here is the same
		// condition that makes admission fail closed: report it as such
		// instead of pretending the tenant has spent nothing.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{
				"message": err.Error(),
				"type":    "infergate_quota_error",
			},
		})
		return
	}
	body["report"] = map[string]any{
		"tenant":               report.Tenant,
		"day":                  report.Day,
		"tokens_today":         report.TokensToday,
		"cost_today_micros":    report.CostTodayMicro,
		"cost_today_usd":       float64(report.CostTodayMicro) / 1_000_000,
		"minute":               report.Minute,
		"requests_this_minute": report.RequestsThisMinute,
		"session":              report.Session,
		"session_tokens":       report.SessionTokens,
		"baseline_tokens":      report.BaselineTokens,
		"ratio":                report.Ratio,
		"alerting":             report.Alerting,
		"policy":               quotaPolicyJSON(report.Policy),
	}
	writeJSON(w, http.StatusOK, body)
}

// quotaTenantsJSON lists the configured tenants in a stable order, so the
// admin page can be diffed between two reads.
func quotaTenantsJSON(cfg config.QuotaConfig) []map[string]any {
	out := make([]map[string]any, 0, len(cfg.Tenants))
	names := make([]string, 0, len(cfg.Tenants))
	for _, t := range cfg.Tenants {
		names = append(names, t.Tenant)
	}
	sort.Strings(names)
	byName := make(map[string]config.TenantQuotaConfig, len(cfg.Tenants))
	for _, t := range cfg.Tenants {
		byName[t.Tenant] = t
	}
	for _, name := range names {
		out = append(out, quotaPolicyJSON(quotaPolicy(byName[name])))
	}
	return out
}

// quotaPolicyJSON renders a policy with the dimensions named, plus a flag for
// each dimension that is actually limited.
//
// The booleans are what make the page readable: a policy of all zeros looks
// identical to a policy that failed to load, and "unbounded" is a legitimate,
// necessary answer for the tenant nobody has budgeted for yet.
func quotaPolicyJSON(p quota.Policy) map[string]any {
	return map[string]any{
		"tenant":              p.Tenant,
		"tokens_per_day":      p.TokensPerDay,
		"cost_per_day_usd":    float64(p.CostPerDayMicros) / 1_000_000,
		"cost_per_day_micros": p.CostPerDayMicros,
		"requests_per_minute": p.RequestsPerMinute,
		"tokens_per_session":  p.TokensPerSession,
		"on_exceed":           p.OnExceed,
		"downgrade_model":     p.DowngradeModel,
		"max_tokens_cap":      p.MaxTokensCap,
		"anomaly_ratio":       p.AnomalyRatio,
		"unbounded":           !p.Budgeted(),
		"limits": map[string]bool{
			"tokens_per_day":      p.TokensPerDay > 0,
			"cost_per_day":        p.CostPerDayMicros > 0,
			"requests_per_minute": p.RequestsPerMinute > 0,
			"tokens_per_session":  p.TokensPerSession > 0,
		},
	}
}

// CloseQuota releases the quota store's connections.
//
// Called after the listener has stopped, for the same reason as CloseCache: a
// counter that is still being written while a request is in flight must not
// have its connection pool torn down underneath it.
func (s *Server) CloseQuota() error {
	if s.quota == nil {
		return nil
	}
	return s.quota.Close()
}
