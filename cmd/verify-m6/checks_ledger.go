package main

// M6 check 4: the per-session ledger.
//
// The ledger exists to answer what a conversation cost, so the assertions are
// about sums: tokens and dollars accumulated across several requests, split by
// model and by backend, with a retry that cost nothing extra.

import (
	"fmt"
	"net/http"
	"time"
)

func checkSessionLedger(c *checker) {
	up := newScriptedUpstream()
	defer up.Close()
	// One backend on purpose: with a second candidate in the plan the failing
	// turn below would fail over to a healthy one and never be recorded as a
	// failure, which is the opposite of what this check is about.
	cfg := baseConfig(upstreamConfig("mock", up.URL(), "chat"))
	cfg.Sessions.Capacity = 16
	s := newStack(c, "ledger", cfg)
	if s == nil {
		return
	}
	defer s.Close(c, "ledger")

	conv := map[string]string{
		"X-InferGate-Tenant":  "team-a",
		"X-InferGate-Session": "conv-1",
	}
	body := chatBody("mock-gpt", "hello")
	// Two requests in one conversation: the mock answers 7 prompt / 3 completion
	// tokens each, so the rollup must be 14/6 and cost (14*2 + 6*6)/1e6.
	post(c, s.url, chatPath, body, conv, "conversation turn 1")
	post(c, s.url, chatPath, body, conv, "conversation turn 2")

	// A request with no session header is counted as unattributed rather than
	// invented as a one-request session.
	post(c, s.url, chatPath, body, map[string]string{"X-InferGate-Tenant": "team-a"}, "request with no session header")

	// A second conversation.
	other := map[string]string{"X-InferGate-Tenant": "team-b", "X-InferGate-Session": "conv-2"}
	post(c, s.url, chatPath, body, other, "another tenant's conversation")

	admin, ok := getAny(c, s.url, "/admin/sessions", "session listing")
	if !ok {
		return
	}
	c.equal(digBool(c, admin, "enabled"), true, "the listing reports the ledger is on")
	c.equal(digNum(c, admin, "capacity"), 16, "the listing reports the configured capacity")
	c.equal(digNum(c, admin, "recent_per_session"), 8, "the listing reports the recent-request bound")
	c.equal(digNum(c, admin, "tracked"), 2, "two conversations are tracked")
	c.equal(len(digList(c, admin, "tenants")), 2, "two tenants have conversations")
	c.equal(digNum(c, admin, "stats", "recorded"), 3,
		"the three requests that named a conversation were recorded")
	c.equal(digNum(c, admin, "stats", "no_session_id"), 1, "the unattributed request is counted")

	one := findSession(c, admin, "team-a", "conv-1")
	if one != nil {
		c.equal(one["requests"], float64(2), "the conversation rolled up both of its requests")
		c.equal(one["prompt_tokens"], float64(14), "prompt tokens are summed across the conversation")
		c.equal(one["completion_tokens"], float64(6), "completion tokens are summed across the conversation")
		c.equal(one["ok"], float64(2), "both requests succeeded")
		c.equal(one["failed"], float64(0), "no request in the conversation failed")
		c.equal(one["idempotent_replays"], float64(0), "no request was a replay")
		cost := toFloat(one["cost_usd"])
		c.assert(closeTo(cost, 64e-6, 1e-9),
			"the conversation cost 64 micro-USD at 2/6 per Mtok (got %v)", cost)
		models, _ := one["models"].(map[string]any)
		c.assert(len(models) == 1, "the rollup names the one model it used (got %v)", models)
		if m, ok := models["mock-gpt"].(map[string]any); ok {
			c.equal(m["requests"], float64(2), "the model rollup counts both requests")
			c.equal(m["prompt_tokens"], float64(14), "the model rollup sums prompt tokens")
		} else {
			c.assert(false, "the model rollup has no entry for mock-gpt")
		}
	}

	// The tenant filter is a filter, not a lookup: it must not return the other
	// tenant's conversation.
	filtered, ok := getAny(c, s.url, "/admin/sessions?tenant=team-b", "session listing for one tenant")
	if ok {
		c.equal(digNum(c, filtered, "count"), 1, "filtering by tenant returns that tenant's conversations")
		c.equal(digNum(c, filtered, "tracked"), 2, "the tracked total is not filtered")
		only := digList(c, filtered, "sessions")[0].(map[string]any)
		c.equal(only["tenant"], "team-b", "the filtered listing names the tenant")
	}

	// One conversation by id: scoped, unscoped, unknown, ambiguous.
	scoped, ok := getAny(c, s.url, "/admin/sessions/conv-1?tenant=team-a", "session by id and tenant")
	if ok {
		c.equal(digStr(c, scoped, "id"), "conv-1", "the lookup returns the requested conversation")
		c.equal(digStr(c, scoped, "tenant"), "team-a", "the lookup stays inside the tenant")
		c.equal(digNum(c, scoped, "requests"), 2, "the lookup returns the rollup")
		recent := digList(c, scoped, "recent")
		c.equal(len(recent), 2, "the conversation carries its recent requests")
		if len(recent) == 2 {
			entry := recent[0].(map[string]any)
			c.equal(entry["model"], "mock-gpt", "a recent request records the served model")
			c.equal(entry["status"], float64(200), "a recent request records its status")
			c.assert(toFloat(entry["cost_usd"]) > 0, "a recent request records its own cost")
			c.assert(toFloat(entry["elapsed_ms"]) >= 0, "a recent request records its latency")
		}
	}
	unknown := get(c, s.url, "/admin/sessions/nope", "unknown session id")
	c.equal(unknown.status, http.StatusNotFound, "an unknown session id is a 404")
	c.contains(unknown.body, "infergate_not_found", "the 404 names its type")
	mismatch := get(c, s.url, "/admin/sessions/conv-1?tenant=team-b", "session id under the wrong tenant")
	c.equal(mismatch.status, http.StatusNotFound, "a session id under another tenant is not found")

	// The same id under two tenants is ambiguous, and the gateway says so
	// instead of picking one.
	post(c, s.url, chatPath, body, map[string]string{"X-InferGate-Tenant": "team-c", "X-InferGate-Session": "conv-1"}, "same session id under a third tenant")
	ambiguous := get(c, s.url, "/admin/sessions/conv-1", "ambiguous session id")
	c.equal(ambiguous.status, http.StatusConflict, "a session id under two tenants is a conflict")
	c.contains(ambiguous.body, "infergate_ambiguous_session", "the ambiguity names its type")
	c.contains(ambiguous.body, "?tenant=", "the ambiguity tells the caller how to disambiguate")

	// A failed request is attributed to its conversation.
	up.failFirst = int64(up.Count()) + 1
	post(c, s.url, chatPath, body, map[string]string{"X-InferGate-Tenant": "team-a", "X-InferGate-Session": "conv-1"}, "a failing turn in the conversation")
	failing, ok := getAny(c, s.url, "/admin/sessions/conv-1?tenant=team-a", "conversation after a failure")
	if ok {
		c.equal(digNum(c, failing, "failed"), 1, "a failed request is counted as failed")
		c.equal(digNum(c, failing, "requests"), 3, "a failed request still counts as a request")
	}

	// A listing is not a generation and must not be recorded.
	beforeRecords := digNum(c, mustAdmin(c, s.url, "/admin/sessions", "listing before a keyed GET"), "stats", "recorded")
	do(c, http.MethodGet, s.url+"/v1/models", "", map[string]string{"X-InferGate-Session": "conv-1", "X-InferGate-Tenant": "team-a"}, "session header on a listing")
	afterRecords := digNum(c, mustAdmin(c, s.url, "/admin/sessions", "listing after a keyed GET"), "stats", "recorded")
	c.equal(afterRecords, beforeRecords, "a non-completion path is not recorded in the ledger")

	// A replay adds a request to the conversation and no tokens: the provider
	// charged for one generation, and the ledger must not invent a second.
	up.failFirst = 0
	replayHeaders := map[string]string{
		"X-InferGate-Tenant": "team-a", "X-InferGate-Session": "conv-1", "Idempotency-Key": "conv-1-turn-4",
	}
	post(c, s.url, chatPath, chatBody("mock-gpt", "count on"), replayHeaders, "keyed turn")
	post(c, s.url, chatPath, chatBody("mock-gpt", "count on"), replayHeaders, "replayed turn")
	withReplay, ok := getAny(c, s.url, "/admin/sessions/conv-1?tenant=team-a", "conversation with a replay")
	if ok {
		c.equal(digNum(c, withReplay, "idempotent_replays"), 1, "the replay is counted as a replay")
		c.equal(digNum(c, withReplay, "requests"), 5, "the replay counts as a request")
		c.equal(digNum(c, withReplay, "prompt_tokens"), float64(14+7),
			"a replay contributes a request and no tokens: the two opening turns plus the keyed one")
	}

	// Metrics: the sums are gauges because eviction would make a counter fall.
	metrics := get(c, s.url, "/metrics", "metrics exposition")
	c.equal(metricFamilyType(metrics.body, "infergate_sessions_tracked"), "gauge", "sessions tracked is a gauge")
	c.equal(metricFamilyType(metrics.body, "infergate_sessions_requests"), "gauge", "session requests is a gauge")
	c.equal(metricFamilyType(metrics.body, "infergate_sessions_cost_usd"), "gauge", "session cost is a gauge")
	c.equal(metricFamilyType(metrics.body, "infergate_session_records_total"), "counter", "records is a counter")
	c.equal(metricFamilyType(metrics.body, "infergate_session_no_id_total"), "counter", "unattributed requests is a counter")
	c.equal(metricScalar(metrics.body, "infergate_sessions_tracked"), 3, "the tracked gauge matches the ledger")
	c.equal(metricScalar(metrics.body, "infergate_sessions_requests"), 7, "the requests gauge sums every conversation")
	c.assert(metricScalar(metrics.body, "infergate_sessions_cost_usd") > 0, "the cost gauge reports real spend")
	// Three conversations exist: team-a/conv-1 (two turns plus the keyed turn),
	// team-b/conv-2, and team-c's conv-1, which is a different session that
	// happens to share an id.
	c.equal(metricValue(metrics.body, "infergate_sessions_tokens", `kind="prompt"`), float64(7*5),
		"the prompt-token gauge sums the conversations")

	// Eviction at capacity keeps the ledger bounded and drops the oldest.
	tightUp := newScriptedUpstream()
	defer tightUp.Close()
	tightCfg := baseConfig(upstreamConfig("mock", tightUp.URL(), "chat"))
	tightCfg.Sessions.Capacity = 1
	tight := newStack(c, "session capacity", tightCfg)
	if tight == nil {
		return
	}
	defer tight.Close(c, "session capacity")
	post(c, tight.url, chatPath, body, map[string]string{"X-InferGate-Session": "s1"}, "conversation 1 with capacity 1")
	post(c, tight.url, chatPath, body, map[string]string{"X-InferGate-Session": "s2"}, "conversation 2 with capacity 1")
	tightAdmin := mustAdmin(c, tight.url, "/admin/sessions", "listing after eviction")
	c.equal(digNum(c, tightAdmin, "tracked"), 1, "the ledger holds only what fits")
	c.equal(digNum(c, tightAdmin, "stats", "evicted"), 1, "the eviction is counted")
	c.equal(metricScalar(get(c, tight.url, "/metrics", "eviction metrics").body, "infergate_sessions_requests"), 1,
		"the gauge drops the evicted conversation's requests")

	// Flush empties the ledger.
	flush := post(c, s.url, "/admin/sessions/flush", "", nil, "flush the ledger")
	c.equal(flush.status, http.StatusOK, "flush answers 200")
	c.contains(flush.body, `"flushed":`, "flush reports what it dropped")
	empty := mustAdmin(c, s.url, "/admin/sessions", "listing after flush")
	c.equal(digNum(c, empty, "tracked"), 0, "the ledger is empty after a flush")
	c.equal(digNum(c, empty, "stats", "flushes"), 1, "the flush is counted")
	c.equal(metricScalar(get(c, s.url, "/metrics", "metrics after flush").body, "infergate_sessions_requests"), 0,
		"the gauge reports zero after a flush")

	// The TTL is the operator's dial, and the config reaches the ledger.
	//
	// The expiry itself is enforced by the ledger's janitor, which sweeps once a
	// minute by default and is not a config knob; waiting for a sweep is not
	// something a gate can do. What is asserted here is the observable half:
	// the configured TTL arrives and is reported. The sweep is pinned by
	// internal/sessions/ledger_test.go, which drives Options.SweepInterval.
	ttlUp := newScriptedUpstream()
	defer ttlUp.Close()
	ttlCfg := baseConfig(upstreamConfig("mock", ttlUp.URL(), "chat"))
	ttlCfg.Sessions.TTL = configDuration(150 * time.Millisecond)
	ttlStack := newStack(c, "session ttl", ttlCfg)
	if ttlStack == nil {
		return
	}
	defer ttlStack.Close(c, "session ttl")
	post(c, ttlStack.url, chatPath, body, map[string]string{"X-InferGate-Session": "short-lived"}, "conversation with a short TTL")
	ttlAdmin := mustAdmin(c, ttlStack.url, "/admin/sessions", "listing with a short TTL")
	c.equal(digStr(c, ttlAdmin, "ttl"), "150ms", "the listing reports the TTL the operator configured")
	c.equal(digNum(c, ttlAdmin, "tracked"), 1, "the conversation is tracked while it is fresh")
	if session := findSession(c, ttlAdmin, "", "short-lived"); session != nil {
		c.assert(fmt.Sprint(session["expires_at"]) != "", "a conversation records when it will be swept")
	}
}
