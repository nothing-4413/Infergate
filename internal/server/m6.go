package server

// M6: agent-platform integration.
//
// Three surfaces live here, and they answer three questions an agent runtime
// cannot answer from a response alone:
//
//   - "my client timed out mid-request; if I send it again, will it run twice?"
//     -> the idempotency store, read back through /admin/idempotency.
//   - "what did this conversation cost, and on which backends?"
//     -> the per-session ledger, read through /admin/sessions.
//   - "can any backend actually accept a tool call / an image / JSON mode?"
//     -> /v1/capabilities (declared) and /v1/capabilities/probe (observed).
//
// All three are built here rather than in the gateway, for the same reason the
// cache and the quota manager are: they own state that outlives a request and
// asks the process, not the request path, what it is configured to do.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/gateway"
	"github.com/infergate/infergate/internal/idempotency"
	"github.com/infergate/infergate/internal/sessions"
)

// maxAdminLimit caps what one admin listing may return. The stores are bounded
// anyway; this only stops a caller from turning a debug endpoint into a
// full-memory dump.
const maxAdminLimit = 500

// buildIdempotency constructs the replay store.
//
// A disabled store still exists, so /admin/idempotency can report the
// configuration: "is replay on, and for how long" is the first question after a
// duplicate charge, and answering it must not require reading the config file
// on the host. It is simply never handed to the proxy, so a disabled store
// costs nothing per request.
func buildIdempotency(cfg *config.Config, logger logAdapter) *idempotency.Store {
	ic := cfg.Idempotency
	store := idempotency.New(idempotency.Options{
		Capacity:         ic.Capacity,
		TTL:              ic.TTL.Duration(),
		MaxResponseBytes: ic.MaxResponseBytes,
	})
	if logger != nil {
		state := "disabled"
		if ic.Enabled {
			state = "enabled"
		}
		logger.Info("idempotency replay "+state,
			"capacity", store.Capacity(),
			"ttl", store.TTL().String(),
			"max_response_bytes", store.MaxResponseBytes())
	}
	return store
}

// buildSessions constructs the per-conversation ledger.
func buildSessions(cfg *config.Config, logger logAdapter) *sessions.Ledger {
	sc := cfg.Sessions
	ledger := sessions.New(sessions.Options{
		Capacity:         sc.Capacity,
		TTL:              sc.TTL.Duration(),
		RecentPerSession: sc.RecentPerSession,
	})
	if logger != nil {
		state := "disabled"
		if sc.Enabled {
			state = "enabled"
		}
		logger.Info("session ledger "+state,
			"capacity", ledger.Capacity(),
			"ttl", ledger.TTL().String(),
			"recent_per_session", ledger.RecentPerSession())
	}
	return ledger
}

// modelInfo converts configuration metadata into the gateway's view of it. The
// two types are structurally identical on purpose: the gateway must not depend
// on the config package, and a mapping function is cheaper than a shared type
// that would drag configuration into the request path.
func modelInfo(cfg *config.Config) map[string]gateway.ModelInfo {
	if len(cfg.Models) == 0 {
		return nil
	}
	out := make(map[string]gateway.ModelInfo, len(cfg.Models))
	for name, info := range cfg.Models {
		out[name] = gateway.ModelInfo{
			ContextWindow:   info.ContextWindow,
			MaxOutputTokens: info.MaxOutputTokens,
			Capabilities:    info.Capabilities,
			Notes:           info.Notes,
		}
	}
	return out
}

// adminLimit parses the shared ?limit= parameter of the admin listings.
func adminLimit(r *http.Request, def int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("limit must be an integer, got %q", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("limit must be positive, got %d", n)
	}
	if n > maxAdminLimit {
		n = maxAdminLimit
	}
	return n, nil
}

// handleIdempotency reports the replay configuration, the counters and the
// entries currently held.
//
// The entries are listed because a duplicate charge is diagnosed by finding the
// key that did the work, not by guessing it: the response headers name the
// original request id, and this endpoint is where that id can be looked up
// again after the client has thrown it away.
func (s *Server) handleIdempotency(w http.ResponseWriter, r *http.Request) {
	limit, err := adminLimit(r, 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, "infergate_bad_request", err.Error())
		return
	}
	store := s.idempotency
	entries := store.List(limit)
	if entries == nil {
		entries = []idempotency.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":            s.cfg.Idempotency.Enabled,
		"capacity":           store.Capacity(),
		"ttl":                store.TTL().String(),
		"max_response_bytes": store.MaxResponseBytes(),
		"stored":             store.Len(),
		"in_flight":          store.InFlight(),
		"scopes":             store.ScopeCount(),
		"stats":              store.Stats(),
		"count":              len(entries),
		"entries":            entries,
	})
}

// handleIdempotencyFlush drops stored answers. ?scope= narrows it to one
// tenant, which is what makes the endpoint usable without evicting every other
// tenant's replayable work.
func (s *Server) handleIdempotencyFlush(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	flushed := s.idempotency.Flush(scope)
	target := scope
	if target == "" {
		target = "all"
	}
	writeJSON(w, http.StatusOK, map[string]any{"flushed": flushed, "scope": target})
}

// handleSessions lists the tracked conversations, newest activity first, with
// the per-tenant rollup that a spend review actually starts from.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	limit, err := adminLimit(r, 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, "infergate_bad_request", err.Error())
		return
	}
	ledger := s.sessions
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))

	list := ledger.List(limit)
	if tenant != "" {
		filtered := make([]sessions.Session, 0, len(list))
		for _, sess := range list {
			if sess.Tenant == tenant {
				filtered = append(filtered, sess)
			}
		}
		list = filtered
	}
	if list == nil {
		list = []sessions.Session{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":            s.cfg.Sessions.Enabled,
		"capacity":           ledger.Capacity(),
		"ttl":                ledger.TTL().String(),
		"recent_per_session": ledger.RecentPerSession(),
		"tracked":            ledger.Len(),
		"tenant":             tenant,
		"tenants":            ledger.Tenants(),
		"stats":              ledger.Stats(),
		"count":              len(list),
		"sessions":           list,
	})
}

// handleSessionByID returns one conversation.
//
// The id alone is not a key: the ledger is scoped by tenant, so the same
// session id under two tenants is two different conversations. Passing
// ?tenant= resolves it; omitting it and finding exactly one match resolves it
// too; finding several is reported as ambiguous rather than guessed, because
// guessing would hand an operator another tenant's spend.
func (s *Server) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "infergate_bad_request", "session id is required")
		return
	}
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	if tenant != "" {
		sess, ok := s.sessions.Get(tenant, id)
		if !ok {
			writeError(w, http.StatusNotFound, "infergate_not_found", "session not found: "+id)
			return
		}
		writeJSON(w, http.StatusOK, sess)
		return
	}

	matches := s.sessions.Find(id)
	switch len(matches) {
	case 0:
		writeError(w, http.StatusNotFound, "infergate_not_found", "session not found: "+id)
	case 1:
		writeJSON(w, http.StatusOK, matches[0])
	default:
		tenants := make([]string, 0, len(matches))
		for _, m := range matches {
			tenants = append(tenants, m.Tenant)
		}
		writeError(w, http.StatusConflict, "infergate_ambiguous_session",
			fmt.Sprintf("session id %q exists under %d tenants (%s); pass ?tenant= to choose one",
				id, len(matches), strings.Join(tenants, ", ")))
	}
}

// handleSessionsFlush empties the ledger.
func (s *Server) handleSessionsFlush(w http.ResponseWriter, r *http.Request) {
	flushed := s.sessions.Flush()
	writeJSON(w, http.StatusOK, map[string]any{"flushed": flushed})
}

// handleCapabilities serves the declarative capability surface. It is a POST-
// free endpoint on purpose: an agent runtime asks this before it builds a
// request, and a GET can be cached by a client that is about to send thousands
// of them.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	rep := s.proxy.CapabilityReport()
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at":   rep.GeneratedAt,
		"capabilities":   rep.Capabilities,
		"models":         rep.Models,
		"upstreams":      rep.Upstreams,
		"model_count":    len(rep.Models),
		"upstream_count": len(rep.Upstreams),
	})
}

// handleCapabilityProbe runs live capability probes.
//
// This is a POST because it has a body (which capabilities, which model, which
// backends) and because it is NOT idempotent in the way that matters: it spends
// real provider calls. A GET that a crawler or a prefetcher could trigger would
// be a way to bill the operator by accident.
func (s *Server) handleCapabilityProbe(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "infergate_bad_request", "read request body: "+err.Error())
		return
	}
	req, err := gateway.ProbeJSONReader(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "infergate_bad_request", "decode probe request: "+err.Error())
		return
	}

	probes, err := s.proxy.ProbeCapabilities(r.Context(), req)
	if err != nil {
		// An unknown capability name is the caller's mistake, not a gateway
		// fault, and the message lists what can be probed.
		if errors.Is(err, gateway.ErrUnknownCapability) {
			writeError(w, http.StatusBadRequest, "infergate_bad_request", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "infergate_internal", err.Error())
		return
	}

	counts := map[string]int{}
	accepted := 0
	for _, p := range probes {
		counts[p.Outcome]++
		if p.Accepted {
			accepted++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requested": req,
		"count":     len(probes),
		"accepted":  accepted,
		"outcomes":  counts,
		"probes":    probes,
		"note":      "accepted means the backend did not reject the request shape; it is not a statement about answer quality",
	})
}

// writeM6Metrics renders the idempotency and session families.
//
// Cardinality is the reason these carry no tenant label. A tenant dimension on
// a per-process store turns every tenant id a caller invents into a new time
// series, and a caller controls the tenant header; /admin/idempotency and
// /admin/sessions are where the per-tenant detail lives.
func writeM6Metrics(b *strings.Builder, store *idempotency.Store, ledger *sessions.Ledger) {
	if store != nil {
		is := store.Stats()
		b.WriteString("# HELP infergate_idempotency_lookups_total Idempotency-Key lookups performed.\n")
		b.WriteString("# TYPE infergate_idempotency_lookups_total counter\n")
		b.WriteString("# HELP infergate_idempotency_hits_total Keyed requests answered by replaying a stored answer.\n")
		b.WriteString("# TYPE infergate_idempotency_hits_total counter\n")
		b.WriteString("# HELP infergate_idempotency_misses_total Keyed requests that did the work themselves.\n")
		b.WriteString("# TYPE infergate_idempotency_misses_total counter\n")
		b.WriteString("# HELP infergate_idempotency_conflicts_total Reuses of a key with a different body.\n")
		b.WriteString("# TYPE infergate_idempotency_conflicts_total counter\n")
		b.WriteString("# HELP infergate_idempotency_in_flight_rejects_total Concurrent duplicates refused while the first was running.\n")
		b.WriteString("# TYPE infergate_idempotency_in_flight_rejects_total counter\n")
		b.WriteString("# HELP infergate_idempotency_stored_total Answers remembered against their key.\n")
		b.WriteString("# TYPE infergate_idempotency_stored_total counter\n")
		b.WriteString("# HELP infergate_idempotency_oversize_total Answers served but not remembered because they exceeded max_response_bytes.\n")
		b.WriteString("# TYPE infergate_idempotency_oversize_total counter\n")
		b.WriteString("# HELP infergate_idempotency_aborted_total Claims released because the attempt failed and must be retried for real.\n")
		b.WriteString("# TYPE infergate_idempotency_aborted_total counter\n")
		b.WriteString("# HELP infergate_idempotency_evicted_total Stored answers dropped to stay within capacity.\n")
		b.WriteString("# TYPE infergate_idempotency_evicted_total counter\n")
		b.WriteString("# HELP infergate_idempotency_expired_total Stored answers dropped because their TTL passed.\n")
		b.WriteString("# TYPE infergate_idempotency_expired_total counter\n")
		b.WriteString("# HELP infergate_idempotency_entries Stored answers currently held.\n")
		b.WriteString("# TYPE infergate_idempotency_entries gauge\n")
		b.WriteString("# HELP infergate_idempotency_in_flight Claims currently running.\n")
		b.WriteString("# TYPE infergate_idempotency_in_flight gauge\n")
		b.WriteString("# HELP infergate_idempotency_scopes Distinct tenant scopes holding at least one entry.\n")
		b.WriteString("# TYPE infergate_idempotency_scopes gauge\n")
		fmt.Fprintf(b, "infergate_idempotency_lookups_total %d\n", is.Lookups)
		fmt.Fprintf(b, "infergate_idempotency_hits_total %d\n", is.Hits)
		fmt.Fprintf(b, "infergate_idempotency_misses_total %d\n", is.Misses)
		fmt.Fprintf(b, "infergate_idempotency_conflicts_total %d\n", is.Conflicts)
		fmt.Fprintf(b, "infergate_idempotency_in_flight_rejects_total %d\n", is.InFlightRejects)
		fmt.Fprintf(b, "infergate_idempotency_stored_total %d\n", is.Stored)
		fmt.Fprintf(b, "infergate_idempotency_oversize_total %d\n", is.Oversize)
		fmt.Fprintf(b, "infergate_idempotency_aborted_total %d\n", is.Aborted)
		fmt.Fprintf(b, "infergate_idempotency_evicted_total %d\n", is.Evicted)
		fmt.Fprintf(b, "infergate_idempotency_expired_total %d\n", is.Expired)
		fmt.Fprintf(b, "infergate_idempotency_entries %d\n", store.Len())
		fmt.Fprintf(b, "infergate_idempotency_in_flight %d\n", store.InFlight())
		fmt.Fprintf(b, "infergate_idempotency_scopes %d\n", store.ScopeCount())
	}

	if ledger != nil {
		ls := ledger.Stats()
		b.WriteString("# HELP infergate_session_records_total Requests recorded in a session ledger.\n")
		b.WriteString("# TYPE infergate_session_records_total counter\n")
		b.WriteString("# HELP infergate_session_no_id_total Requests that carried no session id and were therefore not attributed.\n")
		b.WriteString("# TYPE infergate_session_no_id_total counter\n")
		b.WriteString("# HELP infergate_session_evictions_total Sessions dropped to stay within capacity.\n")
		b.WriteString("# TYPE infergate_session_evictions_total counter\n")
		b.WriteString("# HELP infergate_session_expirations_total Sessions dropped because they went idle past the TTL.\n")
		b.WriteString("# TYPE infergate_session_expirations_total counter\n")
		b.WriteString("# HELP infergate_sessions_tracked Sessions currently held.\n")
		b.WriteString("# TYPE infergate_sessions_tracked gauge\n")
		b.WriteString("# HELP infergate_sessions_tenants Distinct tenants with at least one tracked session.\n")
		b.WriteString("# TYPE infergate_sessions_tenants gauge\n")
		// These four are gauges, not counters, and the distinction is load
		// bearing: a session evicted at capacity takes its requests and its
		// spend out of the sum, so a counter here would go DOWN, which is not
		// something a Prometheus counter may do.
		b.WriteString("# HELP infergate_sessions_requests Requests attributed to currently tracked sessions.\n")
		b.WriteString("# TYPE infergate_sessions_requests gauge\n")
		b.WriteString("# HELP infergate_sessions_cost_usd Spend attributed to currently tracked sessions.\n")
		b.WriteString("# TYPE infergate_sessions_cost_usd gauge\n")
		b.WriteString("# HELP infergate_sessions_tokens Tokens attributed to currently tracked sessions.\n")
		b.WriteString("# TYPE infergate_sessions_tokens gauge\n")
		var requests, cost float64
		var prompt, completion, cached int64
		for _, sess := range ledger.List(0) {
			requests += float64(sess.Requests)
			cost += sess.CostUSD
			prompt += sess.PromptTokens
			completion += sess.CompletionTokens
			cached += sess.CachedTokens
		}
		fmt.Fprintf(b, "infergate_session_records_total %d\n", ls.Recorded)
		fmt.Fprintf(b, "infergate_session_no_id_total %d\n", ls.NoSessionID)
		fmt.Fprintf(b, "infergate_session_evictions_total %d\n", ls.Evicted)
		fmt.Fprintf(b, "infergate_session_expirations_total %d\n", ls.Expired)
		fmt.Fprintf(b, "infergate_sessions_tracked %d\n", ledger.Len())
		fmt.Fprintf(b, "infergate_sessions_tenants %d\n", len(ledger.Tenants()))
		fmt.Fprintf(b, "infergate_sessions_requests %g\n", requests)
		fmt.Fprintf(b, "infergate_sessions_cost_usd %g\n", cost)
		fmt.Fprintf(b, "infergate_sessions_tokens{kind=\"prompt\"} %d\n", prompt)
		fmt.Fprintf(b, "infergate_sessions_tokens{kind=\"completion\"} %d\n", completion)
		fmt.Fprintf(b, "infergate_sessions_tokens{kind=\"cached\"} %d\n", cached)
	}
}
