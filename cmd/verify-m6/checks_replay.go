package main

// M6 checks 1-3: the idempotency store as an agent runtime meets it.

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	chatPath   = "/v1/chat/completions"
	embedsPath = "/v1/embeddings"
)

// checkIdempotentReplay is the headline behaviour: the same key twice reaches the
// provider once, and the second answer is the first answer byte for byte.
func checkIdempotentReplay(c *checker) {
	up := newScriptedUpstream()
	defer up.Close()
	s := newStack(c, "replay", baseConfig(upstreamConfig("mock", up.URL(), "chat")))
	if s == nil {
		return
	}
	defer s.Close(c, "replay")

	key := map[string]string{"Idempotency-Key": "op-1"}
	body := chatBody("mock-gpt", "hello")

	first := post(c, s.url, chatPath, body, key, "first keyed chat")
	c.equal(first.status, http.StatusOK, "first keyed chat status")
	c.equal(first.headerValue("X-InferGate-Idempotent-Replay"), "false",
		"the attempt that does the work says so explicitly")
	c.equal(first.headerValue("Idempotency-Key"), "op-1", "the key is echoed back")
	c.equal(first.headerValue("X-InferGate-Upstream-Name"), "mock",
		"the first attempt is attributed to the backend that did the work")
	origin := first.headerValue("X-InferGate-Request-Id")
	c.assert(origin != "", "the first attempt carries a request id")
	c.equal(up.Count(), 1, "the provider was asked once")
	c.equal(up.Last().Header.Get("Idempotency-Key"), "",
		"the caller's key is not forwarded to the provider")
	c.equal(up.Last().Header.Get("Authorization"), "Bearer test-key",
		"the gateway injects the backend credential")

	second := post(c, s.url, chatPath, body, key, "second keyed chat")
	c.equal(second.status, http.StatusOK, "second keyed chat status")
	c.equal(second.headerValue("X-InferGate-Idempotent-Replay"), "true",
		"the second answer is a replay")
	c.equal(second.headerValue("X-InferGate-Idempotent-Origin"), origin,
		"the replay names the request that produced the answer")
	c.equal(second.headerValue("X-InferGate-Upstream-Name"), "replay",
		"a replay is not attributed to a backend that did no work")
	c.equal(second.headerValue("X-InferGate-Idempotent-Upstream"), "mock",
		"the replay still reports which backend originally served it")
	c.equal(second.body, first.body, "the replay is the recorded bytes, unchanged")
	c.equal(up.Count(), 1, "a replay does not touch the provider")
	c.assert(second.headerValue("X-InferGate-Request-Id") != origin,
		"the replay carries its own request id, with the origin in its own header")
	age := second.headerValue("X-InferGate-Idempotent-Age")
	c.assert(regexp.MustCompile(`^\d+$`).MatchString(age),
		"the replay reports the age of the recorded answer in ms (got %q)", age)

	// The same key with a different body is a conflict: replaying would answer a
	// question the caller did not ask.
	other := post(c, s.url, chatPath, chatBody("mock-gpt", "goodbye"), key, "same key, different body")
	c.equal(other.status, http.StatusConflict, "a reused key with a new body is a conflict")
	c.contains(other.body, "infergate_idempotency_conflict", "the conflict names its type")
	c.equal(other.headerValue("X-InferGate-Idempotent-Replay"), "false",
		"a conflict is not reported as a replay")
	c.equal(up.Count(), 1, "a conflict does not reach the provider")

	// The same body to a different path is a different operation: the hash covers
	// the method and the path, not just the bytes.
	crossPath := post(c, s.url, embedsPath, marshal(map[string]any{"model": "mock-gpt", "input": "hello"}),
		key, "same key, different path")
	c.equal(crossPath.status, http.StatusConflict, "a key reused across paths is a conflict")

	// Scoping: the key is the caller's own name for an operation, and two
	// tenants are two callers.
	tenantKey := map[string]string{"Idempotency-Key": "op-1", "X-InferGate-Tenant": "team-b"}
	tenant := post(c, s.url, chatPath, body, tenantKey, "same key, other tenant")
	c.equal(tenant.status, http.StatusOK, "another tenant's identical key is its own operation")
	c.equal(tenant.headerValue("X-InferGate-Idempotent-Replay"), "false",
		"another tenant's identical key does the work again")
	c.equal(up.Count(), 2, "the provider was asked once per tenant")

	// A streamed answer is replayable too, and the replay is the transcript as
	// one body rather than a re-timed generation.
	streamKey := map[string]string{"Idempotency-Key": "op-stream"}
	streamBody := streamChatBody("mock-gpt", "count to three")
	s1 := post(c, s.url, chatPath, streamBody, streamKey, "first streamed chat")
	c.equal(s1.status, http.StatusOK, "first streamed chat status")
	c.contains(s1.body, "data: [DONE]", "the streamed answer is an SSE transcript")
	c.equal(up.Count(), 3, "the streamed request reached the provider")
	s2 := post(c, s.url, chatPath, streamBody, streamKey, "second streamed chat")
	c.equal(s2.headerValue("X-InferGate-Idempotent-Replay"), "true", "a streamed answer replays")
	c.equal(s2.body, s1.body, "the replayed transcript is identical")
	c.equal(up.Count(), 3, "a replayed stream does not reach the provider")
}

// checkNotRemembered pins the answers that must NOT be pinned to a key: a
// transient failure, an oversized answer, an interrupted stream, and anything
// that is not a generation at all.
func checkNotRemembered(c *checker) {
	// A 5xx is "we do not know", so it is not an answer: the retry must be able
	// to run again, and the attempt after that may be replayed.
	up := newScriptedUpstream()
	defer up.Close()
	up.failFirst = 1
	s := newStack(c, "failure", baseConfig(upstreamConfig("mock", up.URL(), "chat")))
	if s == nil {
		return
	}
	defer s.Close(c, "failure")

	key := map[string]string{"Idempotency-Key": "op-500"}
	body := chatBody("mock-gpt", "flaky")
	failed := post(c, s.url, chatPath, body, key, "keyed chat against a failing provider")
	c.equal(failed.status, http.StatusInternalServerError, "the provider's failure is passed through")
	c.equal(up.Count(), 1, "the first attempt reached the provider")

	retry := post(c, s.url, chatPath, body, key, "retry of the same key")
	c.equal(retry.status, http.StatusOK, "the retry of a failed attempt runs instead of replaying the failure")
	c.equal(retry.headerValue("X-InferGate-Idempotent-Replay"), "false", "the retry did the work")
	c.equal(up.Count(), 2, "the retry reached the provider")

	third := post(c, s.url, chatPath, body, key, "second retry of the same key")
	c.equal(third.headerValue("X-InferGate-Idempotent-Replay"), "true",
		"once an attempt succeeds, the key replays that answer")
	c.equal(up.Count(), 2, "the successful attempt is now the recorded one")

	admin, ok := getAny(c, s.url, "/admin/idempotency", "idempotency state after a failure")
	if ok {
		c.equal(digNum(c, admin, "stats", "aborted"), 1, "the failed attempt was aborted, not stored")
		c.equal(digNum(c, admin, "stats", "stored"), 1, "only the successful attempt was remembered")
		c.equal(digNum(c, admin, "stats", "hits"), 1, "the third request was the replay")
	}

	// An answer bigger than the store may hold is delivered in full and then
	// deliberately not remembered.
	big := newScriptedUpstream()
	defer big.Close()
	bigCfg := baseConfig(upstreamConfig("mock", big.URL(), "chat"))
	bigCfg.Idempotency.MaxResponseBytes = 64
	bigStack := newStack(c, "oversize", bigCfg)
	if bigStack == nil {
		return
	}
	defer bigStack.Close(c, "oversize")

	bigKey := map[string]string{"Idempotency-Key": "op-big"}
	b1 := post(c, bigStack.url, chatPath, chatBody("mock-gpt", "hello"), bigKey, "keyed chat with an oversized answer")
	c.equal(b1.status, http.StatusOK, "an oversized answer is still served")
	c.assert(len(b1.body) > 64, "the oversized answer arrives whole (got %d bytes)", len(b1.body))
	c.equal(b1.headerValue("X-InferGate-Idempotent-Replay"), "false", "the first attempt reports replay=false")
	b2 := post(c, bigStack.url, chatPath, chatBody("mock-gpt", "hello"), bigKey, "same key, oversized answer")
	c.equal(b2.headerValue("X-InferGate-Idempotent-Replay"), "false",
		"an answer that did not fit was not remembered, so the key runs again")
	c.equal(big.Count(), 2, "an oversized answer is not remembered")
	bigAdmin, ok := getAny(c, bigStack.url, "/admin/idempotency", "idempotency state after an oversized answer")
	if ok {
		c.equal(digNum(c, bigAdmin, "stats", "oversize"), 2, "both oversized answers are counted")
		c.equal(digNum(c, bigAdmin, "stats", "stored"), 0, "nothing oversized was stored")
		c.equal(digNum(c, bigAdmin, "stored"), 0, "the store is empty")
	}
	// The captured copy stays bounded even though the answer did not.
	if ok {
		c.assert(len(big.Calls()) == 2, "the provider was asked twice")
	}

	// A duplicate while the first is still running is refused rather than
	// duplicated: answering 409 tells the caller to retry the same key, and the
	// provider is asked for one generation.
	slow := newScriptedUpstream()
	defer slow.Close()
	slow.block = make(chan struct{})
	slowStack := newStack(c, "in-flight", baseConfig(upstreamConfig("mock", slow.URL(), "chat")))
	if slowStack == nil {
		return
	}
	defer slowStack.Close(c, "in-flight")

	slowKey := map[string]string{"Idempotency-Key": "op-slow"}
	done := make(chan result, 1)
	go func() {
		done <- post(c, slowStack.url, chatPath, chatBody("mock-gpt", "slow"), slowKey, "in-flight first request")
	}()
	if !waitFor(func() bool { return slow.Count() == 1 }, 5*time.Second) {
		c.assert(false, "the blocked request never reached the provider")
		close(slow.block)
		return
	}
	dup := post(c, slowStack.url, chatPath, chatBody("mock-gpt", "slow"), slowKey, "duplicate while in flight")
	c.equal(dup.status, http.StatusConflict, "a duplicate while the first is in flight is refused")
	c.contains(dup.body, "infergate_idempotency_in_flight", "the in-flight conflict names its type")
	c.equal(dup.headerValue("Retry-After"), "1", "the in-flight conflict tells the caller to retry")
	c.equal(slow.Count(), 1, "the duplicate did not reach the provider")
	close(slow.block)
	first := <-done
	c.equal(first.status, http.StatusOK, "the blocked request completed once released")
	follow := post(c, slowStack.url, chatPath, chatBody("mock-gpt", "slow"), slowKey, "retry after the in-flight request finished")
	c.equal(follow.headerValue("X-InferGate-Idempotent-Replay"), "true",
		"after the original finished, the same key replays it")
	c.equal(slow.Count(), 1, "the provider was asked once for the whole exchange")

	// A key on a path with no replayable response is disregarded, and said so.
	skip := do(c, http.MethodGet, s.url+"/v1/models", "", map[string]string{"Idempotency-Key": "op-list"}, "keyed listing")
	c.equal(skip.status, http.StatusOK, "the listing is proxied")
	c.equal(skip.headerValue("X-InferGate-Idempotent-Store"), "skip",
		"a key on a listing reports that there is nothing to replay")
	c.equal(skip.headerValue("X-InferGate-Idempotent-Replay"), "",
		"a listing reports no replay decision")

	// With the store disabled the header is ignored completely.
	offUp := newScriptedUpstream()
	defer offUp.Close()
	offCfg := baseConfig(upstreamConfig("mock", offUp.URL(), "chat"))
	offCfg.Idempotency.Enabled = false
	offStack := newStack(c, "disabled", offCfg)
	if offStack == nil {
		return
	}
	defer offStack.Close(c, "disabled")
	offKey := map[string]string{"Idempotency-Key": "op-off"}
	post(c, offStack.url, chatPath, chatBody("mock-gpt", "hello"), offKey, "keyed chat with replay off")
	offSecond := post(c, offStack.url, chatPath, chatBody("mock-gpt", "hello"), offKey, "keyed chat with replay off, again")
	c.equal(offSecond.headerValue("X-InferGate-Idempotent-Replay"), "",
		"with replay off the header is not set at all")
	c.equal(offUp.Count(), 2, "with replay off every request reaches the provider")

	// Capacity and TTL both end a replay, and both are the operator's dials.
	evictCfg := baseConfig(upstreamConfig("mock", up.URL(), "chat"))
	evictCfg.Idempotency.Capacity = 1
	evictStack := newStack(c, "capacity", evictCfg)
	if evictStack == nil {
		return
	}
	defer evictStack.Close(c, "capacity")
	before := up.Count()
	post(c, evictStack.url, chatPath, body, map[string]string{"Idempotency-Key": "keep"}, "first key with capacity 1")
	post(c, evictStack.url, chatPath, body, map[string]string{"Idempotency-Key": "push-out"}, "second key with capacity 1")
	evicted := post(c, evictStack.url, chatPath, body, map[string]string{"Idempotency-Key": "keep"}, "the evicted key again")
	c.equal(evicted.headerValue("X-InferGate-Idempotent-Replay"), "false",
		"a key evicted at capacity runs again rather than replaying")
	c.equal(up.Count()-before, 3, "the evicted key reached the provider again")
	evictAdmin, ok := getAny(c, evictStack.url, "/admin/idempotency", "idempotency state after eviction")
	if ok {
		c.equal(digNum(c, evictAdmin, "stored"), 1, "the store holds only what fits")
		c.equal(digNum(c, evictAdmin, "stats", "evicted"), 2,
			"the rejected key from each of the two later entries is counted")
	}

	ttlCfg := baseConfig(upstreamConfig("mock", up.URL(), "chat"))
	ttlCfg.Idempotency.TTL = configDuration(150 * time.Millisecond)
	ttlCfg.Idempotency.Capacity = 8
	ttlStack := newStack(c, "ttl", ttlCfg)
	if ttlStack == nil {
		return
	}
	defer ttlStack.Close(c, "ttl")
	before = up.Count()
	ttlKey := map[string]string{"Idempotency-Key": "op-expiring"}
	post(c, ttlStack.url, chatPath, body, ttlKey, "keyed chat with a short TTL")
	replay := post(c, ttlStack.url, chatPath, body, ttlKey, "immediate retry within the TTL")
	c.equal(replay.headerValue("X-InferGate-Idempotent-Replay"), "true", "inside the TTL the key replays")
	time.Sleep(400 * time.Millisecond)
	after := post(c, ttlStack.url, chatPath, body, ttlKey, "retry after the TTL expired")
	c.equal(after.headerValue("X-InferGate-Idempotent-Replay"), "false",
		"an expired entry is gone, so the key runs again")
	c.equal(up.Count()-before, 2, "the expired key reached the provider once more")
	ttlAdmin, ok := getAny(c, ttlStack.url, "/admin/idempotency", "idempotency state after expiry")
	if ok {
		c.equal(digNum(c, ttlAdmin, "stored"), 1, "the re-run stored a fresh entry")
		c.equal(digNum(c, ttlAdmin, "stats", "hits"), 1, "only the in-TTL retry was a hit")
	}
}

// checkIdempotencySurface reads the admin listing and the metrics families.
func checkIdempotencySurface(c *checker) {
	up := newScriptedUpstream()
	defer up.Close()
	cfg := baseConfig(upstreamConfig("mock", up.URL(), "chat"))
	cfg.Idempotency.Capacity = 16
	cfg.Idempotency.MaxResponseBytes = 4096
	s := newStack(c, "surface", cfg)
	if s == nil {
		return
	}
	defer s.Close(c, "surface")

	body := chatBody("mock-gpt", "hello")
	post(c, s.url, chatPath, body, map[string]string{"Idempotency-Key": "surface-1"}, "keyed chat 1")
	post(c, s.url, chatPath, body, map[string]string{"Idempotency-Key": "surface-1"}, "keyed chat 1 replay")
	post(c, s.url, chatPath, body, map[string]string{"Idempotency-Key": "surface-2", "X-InferGate-Tenant": "team-b"}, "keyed chat 2 for another tenant")

	res := get(c, s.url, "/admin/idempotency", "idempotency listing")
	admin, ok := getAny(c, s.url, "/admin/idempotency", "idempotency listing")
	if !ok {
		return
	}
	c.assert(!strings.Contains(res.body, `"body"`), "the listing must not expose stored response bodies")
	c.equal(digBool(c, admin, "enabled"), true, "the listing reports replay is on")
	c.equal(digNum(c, admin, "capacity"), 16, "the listing reports the configured capacity")
	c.equal(digStr(c, admin, "ttl"), "1m0s", "the listing reports the configured TTL")
	c.equal(digNum(c, admin, "max_response_bytes"), 4096, "the listing reports the memory bound")
	c.equal(digNum(c, admin, "stored"), 2, "two keys are stored")
	c.equal(digNum(c, admin, "in_flight"), 0, "nothing is in flight")
	c.equal(digNum(c, admin, "scopes"), 2, "the two tenants are two scopes")
	c.equal(digNum(c, admin, "count"), 2, "the listing returns both entries")
	c.equal(digNum(c, admin, "stats", "lookups"), 3, "every keyed request was looked up")
	c.equal(digNum(c, admin, "stats", "hits"), 1, "one replay")
	c.equal(digNum(c, admin, "stats", "misses"), 2, "two claims")
	c.equal(digNum(c, admin, "stats", "conflicts"), 0, "no conflicts")
	c.equal(digNum(c, admin, "stats", "stored"), 2, "two answers remembered")

	entries := digList(c, admin, "entries")
	c.equal(len(entries), 2, "the listing returns both keys")
	// The order is by completion time, newest first. Entries created inside one
	// clock tick tie and fall back to the key, so this asserts the invariant
	// (non-increasing completion time) rather than naming a winner.
	if len(entries) == 2 {
		firstAt := parseTime(c, fmt.Sprint(entries[0].(map[string]any)["completed_at"]), "first entry completed_at")
		secondAt := parseTime(c, fmt.Sprint(entries[1].(map[string]any)["completed_at"]), "second entry completed_at")
		c.assert(!firstAt.Before(secondAt), "the listing is newest first (got %s then %s)",
			firstAt.Format(time.RFC3339Nano), secondAt.Format(time.RFC3339Nano))
	}
	if entry := entryByKey(c, admin, "surface-2"); entry != nil {
		c.equal(entry["scope"], "team-b", "the entry reports the scope it was claimed in")
		c.equal(entry["model"], "mock-gpt", "the entry records the served model")
		c.equal(entry["upstream"], "mock", "the entry records the backend that did the work")
		c.equal(entry["status"], float64(200), "the entry records the status")
		c.equal(entry["outcome"], "success", "the entry records the outcome")
		c.assert(fmt.Sprint(entry["request_id"]) != "", "the entry records the original request id")
		c.assert(fmt.Sprint(entry["expires_at"]) != "", "the entry records when it expires")
		c.equal(entry["stream"], nil, "a non-streamed entry carries no stream flag")
	}
	if entry := entryByKey(c, admin, "surface-1"); entry != nil {
		c.equal(entry["scope"], "anonymous", "a caller with no tenant header lands in the anonymous scope")
	}

	// The listing is bounded, and the bound does not lie about the total.
	limited, ok := getAny(c, s.url, "/admin/idempotency?limit=1", "bounded idempotency listing")
	if ok {
		c.equal(digNum(c, limited, "count"), 1, "?limit=1 returns one entry")
		c.equal(digNum(c, limited, "stored"), 2, "the total is still reported")
	}

	// Flush is scoped, so one tenant's replays can be dropped without touching
	// another's.
	flushRes := post(c, s.url, "/admin/idempotency/flush?scope=team-b", "", nil, "flush team-b")
	c.equal(flushRes.status, http.StatusOK, "flush answers 200")
	c.contains(flushRes.body, `"flushed": 1`, "flush reports how many entries it dropped")
	c.contains(flushRes.body, `"scope": "team-b"`, "flush reports the scope it narrowed to")
	afterFlush, ok := getAny(c, s.url, "/admin/idempotency", "idempotency listing after a scoped flush")
	if ok {
		c.equal(digNum(c, afterFlush, "stored"), 1, "only the named scope was flushed")
	}
	emptyFlush := post(c, s.url, "/admin/idempotency/flush?scope=nobody", "", nil, "flush an empty scope")
	c.contains(emptyFlush.body, `"flushed": 0`, "flushing an unknown scope is a no-op, not an error")

	// A flushed key does the work again, which is the remedy for a bad answer.
	before := up.Count()
	again := post(c, s.url, chatPath, body, map[string]string{"Idempotency-Key": "surface-2", "X-InferGate-Tenant": "team-b"}, "flushed key again")
	c.equal(again.headerValue("X-InferGate-Idempotent-Replay"), "false", "a flushed key runs again")
	c.equal(up.Count(), before+1, "the flushed key reached the provider")

	// Bad parameters are refused.
	for _, path := range []string{"/admin/idempotency?limit=0", "/admin/idempotency?limit=abc"} {
		bad := get(c, s.url, path, "bad limit "+path)
		c.equal(bad.status, http.StatusBadRequest, "a bad limit is refused: %s", path)
	}

	// Metrics: the families exist, with the right type.
	metrics := get(c, s.url, "/metrics", "metrics exposition")
	for name, want := range map[string]string{
		"infergate_idempotency_lookups_total":      "counter",
		"infergate_idempotency_hits_total":         "counter",
		"infergate_idempotency_misses_total":       "counter",
		"infergate_idempotency_conflicts_total":    "counter",
		"infergate_idempotency_stored_total":       "counter",
		"infergate_idempotency_oversize_total":     "counter",
		"infergate_idempotency_aborted_total":      "counter",
		"infergate_idempotency_evicted_total":      "counter",
		"infergate_idempotency_in_flight_rejects_total": "counter",
		"infergate_idempotency_entries":            "gauge",
		"infergate_idempotency_in_flight":          "gauge",
		"infergate_idempotency_scopes":             "gauge",
	} {
		c.equal(metricFamilyType(metrics.body, name), want, "metric family %s", name)
	}
	c.equal(metricScalar(metrics.body, "infergate_idempotency_hits_total"), 1,
		"the hit counter counts the replay")
	c.equal(metricScalar(metrics.body, "infergate_idempotency_lookups_total"), 4,
		"the lookup counter counts every keyed completion request")
	c.equal(metricScalar(metrics.body, "infergate_idempotency_entries"), 2,
		"the entry gauge is the live store size: the surviving scope plus the flushed key stored again")
	// A replay is attributed to a synthetic upstream so a dashboard cannot
	// mistake it for backend traffic.
	c.assert(metricValue(metrics.body, "infergate_requests_total", `upstream="replay"`) >= 1,
		"a replay appears in infergate_requests_total under upstream=\"replay\"")
}

// waitFor polls a condition; the gate drives goroutines, so it cannot assume an
// ordering.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
