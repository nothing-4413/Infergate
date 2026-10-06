package main

// M3 acceptance checks, part one: the decision path.
//
// The checks below all answer the same question from a different angle: given a
// tenant with a budget, does the gateway do the right thing with the request,
// and can that be shown from something other than the gateway's own words?
//
// The evidence rule this file follows is the one cmd/verify-m2 established:
// every claim is checked against a fact produced somewhere else. "The upstream
// received exactly one request" is a fact (the mock counts them). "The ledger
// holds 18 tokens" is a fact (read back through /admin/quota and, in the Redis
// checks, through the raw key). "The response says allow" is not evidence of
// allowance on its own - it is the thing under test - so it is only ever
// asserted alongside a counter or a request count that could not be right if
// the header were lying.
//
// ---------------------------------------------------------------------------
// The arithmetic every budget check is built on
// ---------------------------------------------------------------------------
//
// A governed request does not charge its real usage up front; it RESERVES an
// estimate and settles to the truth afterwards. The estimate is
// len(body)/chars_per_token + completion, where completion is the caller's own
// max_tokens when the body names one and the configured default (256) when it
// does not (internal/gateway/quotapath.go, quotaEstimate).
//
// That has a consequence that looks like a bug in a naive test and is not one:
// a budget smaller than one reservation can never admit anything. A 10-token
// daily budget refuses its first request immediately - correctly, because the
// request reserved ~280 tokens against a 10-token ceiling. Every check here
// therefore derives its budget from the reservation it just computed, rather
// than hard-coding a "small" number.
//
// The second consequence is that a budget is not exhausted by a fixed number of
// requests. Settling the mock's 9-token usage leaves the day counter at 9, not
// at the 280 that was reserved, so a 300-token budget survives several
// requests. Checks that need a refusal either size the budget to exactly one
// reservation (admit one, refuse the next) or size it to one token (refuse
// everything, deterministically, without depending on settle timing).

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/config"
	igredis "github.com/infergate/infergate/internal/redis"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// quotaHeaders lists the quota response headers actually present. A check that
// only looked at X-InferGate-Quota would miss a stale -Reason or -Model left
// over from another branch, and "no header at all" is a stronger claim than "no
// action header".
func quotaHeaders(res result) []string {
	var out []string
	for name := range res.header {
		if strings.HasPrefix(strings.ToLower(name), "x-infergate-quota") {
			out = append(out, name)
		}
	}
	return out
}

// identityOK is the ledger's own accounting identity:
//
//	reserved - released + overshoot == settled
//
// Every reserved token entry is counted, so `reserved` is what admission
// pre-charged across every budgeted dimension. Settling either charges less than
// was held (the difference is `released`) or more (the difference is
// `overshoot`). So the reservation must always be reconstructible from the three
// numbers that describe where it went. If this drifts, an operator reading the
// admin surface sees budget that cannot be accounted for.
//
// The identity is kept per reserved ENTRY, which is why it holds for a tenant
// metered on two token budgets (a day budget and a session budget) as well as
// for one: each entry either settles short and releases the rest, or settles long
// and overshoots, and the sum of those relations is the relation. A refused
// request is included on both sides - it reserves its entries and releases them
// in the same admission - so `released` also counts the churn of refusals.
func identityOK(d adminQuotaDoc) bool {
	s := d.Stats
	if s.ReleasedTokens > s.ReservedTokens {
		return false
	}
	return s.ReservedTokens-s.ReleasedTokens+s.OvershootTokens == s.SettledTokens
}

func assertIdentity(c *checker, label string, d adminQuotaDoc) {
	s := d.Stats
	c.assert(identityOK(d),
		"%s: reserved - released + overshoot == settled (%d - %d + %d == %d)",
		label, s.ReservedTokens, s.ReleasedTokens, s.OvershootTokens, s.SettledTokens)
}

// awaitStats polls /admin/quota's stats until the predicate holds. Settlement
// happens in a deferred hook after the response is written, so a client that
// already holds its response can still be a few milliseconds ahead of the
// ledger; polling to a predicate is what makes that a non-event instead of a
// flaky assertion. It returns the last document either way so a failed
// assertion can print the numbers it actually saw.
func awaitStats(c *checker, url string, want func(adminQuotaDoc) bool) (adminQuotaDoc, bool) {
	var doc adminQuotaDoc
	for i := 0; i < 250; i++ {
		d, ok := adminQ(c, url, "", "")
		if ok {
			doc = d
			if want(d) {
				return d, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return doc, false
}

// awaitReport is awaitStats for one tenant's report, and is how a check waits
// for a settle to land without guessing an interval.
func awaitReport(c *checker, url, tenant, session string, want func(quotaReport) bool) (quotaReport, bool) {
	var rep quotaReport
	for i := 0; i < 250; i++ {
		doc, ok := adminQ(c, url, tenant, session)
		if ok && doc.Report != nil {
			rep = *doc.Report
			if want(rep) {
				return rep, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return rep, false
}

// awaitTokens waits for a tenant's day-token ledger to reach a value. It is the
// settle barrier the budget checks use: an assertion about the SECOND request
// is only meaningful once the first one has finished settling.
func awaitTokens(c *checker, url, tenant string, want int64) (int64, bool) {
	rep, ok := awaitReport(c, url, tenant, "", func(r quotaReport) bool { return r.TokensToday >= want })
	return rep.TokensToday, ok
}

// awaitCost is awaitTokens for the money ledger.
func awaitCost(c *checker, url, tenant string, want int64) (int64, bool) {
	rep, ok := awaitReport(c, url, tenant, "", func(r quotaReport) bool { return r.CostTodayMicros >= want })
	return rep.CostTodayMicros, ok
}

// dayBucket and minuteBucket are the counter key buckets, and they are UTC
// buckets because internal/quota normalises the clock before it formats one:
// Manager.Admit, Manager.Report and Manager.checkAnomaly each open with
// `now := m.now().UTC()`. Formatting a local time here would
// look for a different key for the hours in which the local and UTC dates
// differ (on a UTC+8 machine, from 00:00 to 08:00 local), so the Redis key
// assertions would pass for most of the day and fail for the rest - the worst
// property an acceptance gate can have. The first version of this check made
// exactly that mistake, and it survived until a run at 16:24 local printed the
// gateway's minute key (202610050824) next to the check's own (202610051624).
//
// The consequence for operators is worth stating plainly: a "daily" budget is
// a UTC day, so on this UTC+8 box it resets at 08:00 local, not at midnight.
func dayBucket(t time.Time) string    { return t.UTC().Format("20060102") }
func minuteBucket(t time.Time) string { return t.UTC().Format("200601021504") }

// secondsToNextUTCMidnight is the lifetime the day bucket's own key implies: the
// key names a UTC day, so a TTL shorter than the time left in that UT day would
// let the counter expire and reset the budget while the day is still running.
//
// internal/quota computes this with untilDayEnd, which builds
// `time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Add(24h)` and
// subtracts the instant. That formula is correct only because its input is
// already UTC - it takes the calendar date from the receiver's own zone and the
// midnight from UTC. Because every caller normalises first, the two agree; this
// helper is the same computation done unambiguously, so a future change that
// drops the `.UTC()` in the manager shows up here as a TTL mismatch.
func secondsToNextUTCMidnight(now time.Time) int64 {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	return int64(next.Sub(utc) / time.Second)
}

// rawRedis is a direct client on the in-process RESP2 server the gateway is
// using. Reading the counters through it rather than through /admin/quota is
// the point of the Redis check: it proves the gateway and an outside reader
// agree on the key layout, which is what makes a shared Redis usable by
// anything but this process.
type rawRedis struct {
	client *igredis.Client
}

func newRawRedis(c *checker, addr string) *rawRedis {
	client := igredis.NewClient(igredis.Options{
		Addr:         addr,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Do(ctx, "PING"); err != nil {
		c.assert(false, "the raw Redis client cannot reach the store at %s: %v", addr, err)
		return nil
	}
	return &rawRedis{client: client}
}

func (r *rawRedis) close() { _ = r.client.Close() }

// get returns a counter's value and whether the key exists at all. The two are
// reported separately because "the gateway never wrote the key" and "the
// gateway wrote a zero" are different findings.
func (r *rawRedis) get(key string) (int64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := r.client.Do(ctx, "GET", key)
	if err != nil || reply.Null {
		return 0, false
	}
	text, err := reply.Text()
	if err != nil {
		return 0, false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (r *rawRedis) set(key, value string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := r.client.Do(ctx, "SET", key, value)
	return err == nil
}

func (r *rawRedis) incrBy(key string, delta int64) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := r.client.Do(ctx, "INCRBY", key, strconv.FormatInt(delta, 10))
	return err == nil
}

func (r *rawRedis) ttl(key string) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := r.client.Do(ctx, "TTL", key)
	if err != nil {
		return -3
	}
	return reply.Int
}

func (r *rawRedis) keys(pattern string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := r.client.Do(ctx, "KEYS", pattern)
	if err != nil {
		return nil
	}
	out, err := reply.Strings()
	if err != nil {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. DISABLED
// ---------------------------------------------------------------------------

// checkDisabled is the switch itself. A governance feature that cannot be
// turned off is not deployable, and the dangerous half-failure is a gateway
// that reports itself disabled while still writing counters or still stamping
// quota headers: clients that learned the headers would act on an action that
// nothing is enforcing, and an operator reading a clean ledger would believe
// traffic was measured when it was not.
//
// So the tenant below is configured with a one-token daily budget. If the
// switch leaks anywhere, the first request is refused; the check asserts all
// three faces of "off" - no header, no ledger, no admin surface - plus the
// request still reaching its upstream, which is the only behaviour that
// actually matters to a caller.
func checkDisabled(c *checker, e *environment) {
	q := config.Defaults().Quota
	q.Enabled = false
	// A budget that would refuse on sight if it were ever consulted.
	q.Tenants = []config.TenantQuotaConfig{{Tenant: "acme", TokensPerDay: 1}}

	st := e.newStack(c, q)
	if st == nil {
		return
	}
	defer st.close()

	body := chatBody(c, "mock-gpt", "hello")

	first := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(first.status == 200, "disabled: the request is admitted (status %d, want 200)", first.status)
	c.assert(len(quotaHeaders(first)) == 0,
		"disabled: no quota header is stamped at all (saw %v)", quotaHeaders(first))
	c.assert(actionOf(first) == "" && reasonOf(first) == "" && limitOf(first) == "" && usedOf(first) == "",
		"disabled: the quota action/reason/limit/used headers are all absent (action=%q reason=%q limit=%q used=%q)",
		actionOf(first), reasonOf(first), limitOf(first), usedOf(first))

	second := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(second.status == 200, "disabled: a second request is admitted too (status %d)", second.status)
	c.assert(st.backend.Count() == 2,
		"disabled: the upstream received both requests, so neither was answered locally (%d)", st.backend.Count())

	doc, ok := adminQ(c, st.url, "", "")
	if c.assert(ok, "disabled: GET /admin/quota answers") {
		c.assert(!doc.Enabled, "disabled: /admin/quota reports enabled=false")
		// The document is still the full shape, and that is the right call: an
		// operator asking "is governance on, and what WOULD it enforce?" gets
		// both answers from one endpoint. What must not happen is a counter
		// moving, so the store is named "disabled" and every statistic is zero.
		c.assert(doc.Store == "disabled", "disabled: /admin/quota names the store as disabled (%q)", doc.Store)
		c.assert(doc.Report == nil, "disabled: /admin/quota returns no per-tenant report")
		c.assert(doc.Config.EstimateCompletionTokens == 256,
			"disabled: /admin/quota still publishes the estimate it would apply (%d)", doc.Config.EstimateCompletionTokens)
		c.assert(doc.Config.EstimateCharsPerToken == 4,
			"disabled: /admin/quota publishes the chars-per-token estimate too (%d)", doc.Config.EstimateCharsPerToken)
		c.assert(doc.Stats.Allowed == 0 && doc.Stats.Rejected == 0 && doc.Stats.ReservedTokens == 0 &&
			doc.Stats.SettledTokens == 0 && doc.Stats.ReleasedTokens == 0,
			"disabled: no decision, reservation or settlement was recorded (allowed=%d rejected=%d reserved=%d settled=%d released=%d)",
			doc.Stats.Allowed, doc.Stats.Rejected, doc.Stats.ReservedTokens, doc.Stats.SettledTokens, doc.Stats.ReleasedTokens)
	}

	block, ok := statsQuotaBlock(c, st.url)
	if c.assert(ok, "disabled: /stats carries a quota block") {
		enabled, has := block["enabled"]
		c.assert(has && enabled == false, "disabled: /stats quota block is exactly {enabled:false} (got %v)", block)
		c.assert(len(block) == 1, "disabled: the /stats quota block has no counters (%d keys: %v)", len(block), block)
	}

	metrics := get(c, st.url+"/metrics").body
	for _, family := range quotaMetricFamilies {
		c.assert(!hasMetricLine(metrics, family),
			"disabled: /metrics carries no %s family", family)
	}
}

// ---------------------------------------------------------------------------
// 2. MEMORY-STORE ALLOW
// ---------------------------------------------------------------------------

// checkMemoryAllow is the calibration for everything that follows: the happy
// path over the in-memory store, asserted against the ledger rather than the
// headers.
//
// The specific failure it guards against is a ledger that echoes configuration
// instead of measuring traffic. A gateway that reports the tenant's budget as
// "usage" would pass a shape check and fail this one, because the assertion is
// that the number CHANGES between two identical requests, that it changes by
// the upstream's own reported usage, and that it agrees across three surfaces
// that are rendered by different code (/admin/quota, /stats, /metrics).
func checkMemoryAllow(c *checker, e *environment) {
	body := chatBody(c, "mock-gpt", "hello")
	reserve := reservedDefault(body)
	actual := int64(mockSettledTokens)
	reserveCost := costMicrosFor(len(body)/charsPerToken, defaultCompletionReservation)
	actualCost := costMicrosFor(mockPromptTokens, mockCompletionTokens)

	q := quotaConfig(tenantPolicy{
		tenant:         "acme",
		tokensPerDay:   1_000_000,
		costPerDayUSD:  100,
		requestsPerMin: 100,
	})
	st := e.newStack(c, q)
	if st == nil {
		return
	}
	defer st.close()

	first := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(first.status == 200, "allow: the first request is admitted (status %d)", first.status)
	c.assert(actionOf(first) == "allow", "allow: X-InferGate-Quota is allow (%q)", actionOf(first))
	c.assert(reasonOf(first) == "within-budget", "allow: the reason is within-budget (%q)", reasonOf(first))
	// Decision.Limit/Used are only populated when a dimension was breached, so
	// an admitted request advertises neither. Asserting their absence is how
	// this gate notices a later change that starts inventing a limit for
	// requests that never hit one.
	c.assert(limitOf(first) == "" && usedOf(first) == "",
		"allow: no limit/used header is invented for a request that breached nothing (limit=%q used=%q)",
		limitOf(first), usedOf(first))

	t1, ok := awaitTokens(c, st.url, "acme", actual)
	c.assert(ok, "allow: the first request settles to the upstream's reported usage (%d tokens, want %d)", t1, actual)
	c1, ok := awaitCost(c, st.url, "acme", actualCost)
	c.assert(ok, "allow: the first request also settles its cost (%d micros, want %d)", c1, actualCost)

	second := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(second.status == 200, "allow: the second request is admitted (status %d)", second.status)
	t2, ok := awaitTokens(c, st.url, "acme", 2*actual)
	c.assert(ok, "allow: the ledger grows by the second request's usage (%d tokens, want %d)", t2, 2*actual)
	c2, ok := awaitCost(c, st.url, "acme", 2*actualCost)
	c.assert(ok, "allow: the cost ledger grows as well (%d micros, want %d)", c2, 2*actualCost)
	c.assert(t2 > t1, "allow: the token ledger moved between the two requests (%d -> %d)", t1, t2)
	c.assert(c2 > c1, "allow: the cost ledger moved between the two requests (%d -> %d)", c1, c2)

	c.assert(st.backend.Count() == 2,
		"allow: the upstream received both admitted requests (%d)", st.backend.Count())

	doc, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.SettledTokens >= uint64(2*actual) })
	if !c.assert(ok, "allow: the settle lands in the admin stats (settled=%d)", doc.Stats.SettledTokens) {
		return
	}
	c.assert(doc.Stats.Allowed == 2, "allow: exactly two requests were allowed (%d)", doc.Stats.Allowed)
	c.assert(doc.Stats.Rejected == 0, "allow: nothing was refused (%d)", doc.Stats.Rejected)
	c.assert(doc.Stats.ReservedTokens == uint64(2*reserve),
		"allow: reserved_tokens holds two reservations of %d (%d)", reserve, doc.Stats.ReservedTokens)
	c.assert(doc.Stats.SettledTokens == uint64(2*actual),
		"allow: settled_tokens holds two settlements of %d (%d)", actual, doc.Stats.SettledTokens)
	c.assert(doc.Stats.ReleasedTokens == uint64(2*(reserve-actual)),
		"allow: the hold each request did not use is released (%d, want %d)",
		doc.Stats.ReleasedTokens, 2*(reserve-actual))
	c.assert(doc.Stats.OvershootTokens == 0,
		"allow: hold %d exceeded usage %d, so nothing overshot (%d)", reserve, actual, doc.Stats.OvershootTokens)
	assertIdentity(c, "allow", doc)

	// The money side of the same release: the reserved spend that was not
	// charged. Before this counter existed the token counters had to carry it,
	// which meant a released dollar was invisible to a cost dashboard.
	c.assert(doc.Stats.ReleasedCostMicro == 2*(reserveCost-actualCost),
		"allow: released_cost_micros returns the unspent hold (%d, want %d)",
		doc.Stats.ReleasedCostMicro, 2*(reserveCost-actualCost))

	rep, ok := awaitReport(c, st.url, "acme", "", func(r quotaReport) bool { return r.RequestsThisMinute >= 2 })
	if c.assert(ok, "allow: the tenant report tracks the minute window (%d requests)", rep.RequestsThisMinute) {
		c.assert(rep.RequestsThisMinute == 2, "allow: exactly two requests are in this minute (%d)", rep.RequestsThisMinute)
		c.assert(rep.Day == dayBucket(time.Now()), "allow: the report names today's day bucket (%q)", rep.Day)
		c.assert(rep.TokensToday == 2*actual, "allow: the report agrees with the ledger (%d, want %d)", rep.TokensToday, 2*actual)
		c.assert(rep.Policy.TokensPerDay == 1_000_000,
			"allow: the report carries the tenant's policy, not the default (%d)", rep.Policy.TokensPerDay)
	}

	block, ok := statsQuotaBlock(c, st.url)
	if c.assert(ok, "allow: /stats carries the quota block") {
		c.assert(mapInt(block, "allowed") == 2, "allow: /stats agrees on the allowed count (%v)", block["allowed"])
		c.assert(mapInt(block, "settled_tokens") == 2*actual,
			"allow: /stats agrees on settled_tokens (%v, want %d)", block["settled_tokens"], 2*actual)
		c.assert(mapInt(block, "released_cost_micros") == 2*(reserveCost-actualCost),
			"allow: /stats agrees on released_cost_micros (%v, want %d)",
			block["released_cost_micros"], 2*(reserveCost-actualCost))
	}

	metrics := get(c, st.url+"/metrics").body
	c.assert(metricValue(metrics, "infergate_quota_decisions_total", `action="allow"`) == 2,
		"allow: /metrics agrees on the allowed count (%v)",
		metricValue(metrics, "infergate_quota_decisions_total", `action="allow"`))
	c.assert(metricValue(metrics, "infergate_quota_tokens_total", `kind="settled"`) == float64(2*actual),
		"allow: /metrics agrees on settled tokens (%v)", metricValue(metrics, "infergate_quota_tokens_total", `kind="settled"`))
	releasedCost, present := scalarMetric(metrics, "infergate_quota_released_cost_micros_total")
	c.assert(present && releasedCost == float64(2*(reserveCost-actualCost)),
		"allow: /metrics carries released spend (%v, present=%v, want %d)",
		releasedCost, present, 2*(reserveCost-actualCost))
}

// ---------------------------------------------------------------------------
// 3. DAILY TOKEN BUDGET
// ---------------------------------------------------------------------------

// checkDailyTokenBudget is the refusal path, and the assertion that matters
// most in it is the one about the upstream: a refused request must not be
// forwarded. A gateway that returns 429 but proxies anyway has told the caller
// a lie and charged the provider for it, and the caller has no way to tell.
//
// The budget is sized to exactly one reservation, which makes the sequence
// deterministic without waiting for a day boundary: request one reserves the
// whole ceiling and is admitted, settles to the mock's real usage, and request
// two then crosses the line. The refusal must also explain itself - the reason
// header, the limit and used headers, a Retry-After a client can act on, and an
// error type a client can switch on - because a bare 429 with no retry hint is
// how a well-behaved client turns into a hot loop.
func checkDailyTokenBudget(c *checker, e *environment) {
	body := chatBody(c, "mock-gpt", "hello")
	budget := reservedDefault(body)
	actual := int64(mockSettledTokens)

	q := quotaConfig(tenantPolicy{tenant: "acme", tokensPerDay: budget})
	st := e.newStack(c, q)
	if st == nil {
		return
	}
	defer st.close()

	first := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(first.status == 200, "daily tokens: the request that exactly fits the budget is admitted (status %d)", first.status)
	c.assert(actionOf(first) == "allow", "daily tokens: the first request is allow (%q)", actionOf(first))
	settled, ok := awaitTokens(c, st.url, "acme", actual)
	c.assert(ok, "daily tokens: the first request settles to %d (saw %d)", actual, settled)

	second := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(second.status == 429, "daily tokens: the next request is refused with 429 (status %d)", second.status)
	c.assert(errorType(second.body) == "infergate_quota_exceeded",
		"daily tokens: the error type is infergate_quota_exceeded (%q)", errorType(second.body))
	c.assert(actionOf(second) == "reject", "daily tokens: X-InferGate-Quota is reject (%q)", actionOf(second))
	c.assert(reasonOf(second) == "tokens_per_day",
		"daily tokens: the reason names the token dimension (%q)", reasonOf(second))
	c.assert(limitOf(second) == strconv.FormatInt(budget, 10),
		"daily tokens: the response names the limit that was hit (%q, want %d)", limitOf(second), budget)
	c.assert(usedOf(second) == strconv.FormatInt(settled, 10),
		"daily tokens: the response names the settled usage it counted (%q, want %d)", usedOf(second), settled)
	retry, err := strconv.Atoi(retryAfterOf(second))
	c.assert(err == nil && retry >= 1,
		"daily tokens: Retry-After is at least one second (%q)", retryAfterOf(second))
	c.assert(strings.Contains(errorMessage(second.body), "daily token budget"),
		"daily tokens: the message names the dimension in words (%q)", errorMessage(second.body))
	// The retry hint has to be actionable AND consistent with the window the
	// refusal is about: it must not expire so early that the caller retries into
	// the same refusal, and it must not claim a window longer than the counter it
	// belongs to. untilDayEnd adds one hour of slack on top of the time to the
	// next UTC midnight, so the window is bounded by that midnight plus the slack
	// - and, because the retry hint is what a client will sleep on, the lower
	// bound is the one with teeth: a day bucket that expires before its own day is
	// over resets a daily budget mid-day.
	remaining := secondsToNextUTCMidnight(time.Now())
	c.assert(err == nil && retry >= 1,
		"daily tokens: Retry-After is at least one second (%q)", retryAfterOf(second))
	c.assert(err == nil && int64(retry) >= remaining,
		"daily tokens: Retry-After outlives the day its counter is named for (%q, %ds left in the UTC day)",
		retryAfterOf(second), remaining)
	c.assert(err == nil && int64(retry) <= remaining+maxDayTTLSeconds,
		"daily tokens: Retry-After does not overrun that day by more than a day bucket (%q)", retryAfterOf(second))

	c.assert(st.backend.Count() == 1,
		"daily tokens: the refused request never reached the upstream (%d calls)", st.backend.Count())

	doc, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.Rejected >= 1 })
	if c.assert(ok, "daily tokens: the refusal is counted (rejected=%d)", doc.Stats.Rejected) {
		c.assert(doc.Stats.Rejected == 1, "daily tokens: exactly one request was refused (%d)", doc.Stats.Rejected)
		c.assert(doc.Stats.Allowed == 1, "daily tokens: exactly one request was allowed (%d)", doc.Stats.Allowed)
		// The refused request reserved its entry and gave it straight back, so it
		// appears on both sides of the ledger: one admitted request holds
		// `budget`, the refusal adds and releases another `budget`.
		c.assert(doc.Stats.ReservedTokens == uint64(2*budget),
			"daily tokens: the refused request's reservation is recorded and released (%d, want %d)",
			doc.Stats.ReservedTokens, 2*budget)
		c.assert(doc.Stats.SettledTokens == uint64(actual),
			"daily tokens: only the admitted request settled (%d, want %d)", doc.Stats.SettledTokens, actual)
		assertIdentity(c, "daily tokens", doc)
	}

	rep, ok := awaitReport(c, st.url, "acme", "", func(r quotaReport) bool { return true })
	if c.assert(ok, "daily tokens: the tenant report is readable after a refusal") {
		c.assert(rep.TokensToday == actual,
			"daily tokens: the refused request charged nothing (%d, want %d)", rep.TokensToday, actual)
	}
}

// ---------------------------------------------------------------------------
// 4. PER-MINUTE RATE LIMIT
// ---------------------------------------------------------------------------

// checkMinuteRateLimit covers the one dimension that is not measured in tokens.
// It is deliberately paired with a token budget in exactly the way a real tenant
// would be configured, but the assertions are about requests: two admitted, the
// third and fourth refused, and the minute counter staying at two.
//
// The fourth request is the interesting one. A refusal must not consume
// anything, because a limiter that decrements on rejection turns its own error
// response into the thing that extends the block: a client that retries during
// an outage would never recover, and an operator watching a rate that stays
// pinned after traffic stops has no way to tell that from a stuck counter. It is
// also the cheap place to catch a decision path that writes the reservation
// before it decides.
//
// The retry hint is bounded from both ends here: at least a second (so a client
// does not hot-loop) and at most a minute (the window it belongs to).
func checkMinuteRateLimit(c *checker, e *environment) {
	body := chatBody(c, "mock-gpt", "hello")
	reserve := reservedDefault(body)
	actual := int64(mockSettledTokens)

	q := quotaConfig(tenantPolicy{tenant: "acme", tokensPerDay: 1_000_000, requestsPerMin: 2})
	st := e.newStack(c, q)
	if st == nil {
		return
	}
	defer st.close()

	first := chat(c, st.url, body, tenantHdr("acme"))
	second := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(first.status == 200 && second.status == 200,
		"rpm: the first two requests are admitted (%d, %d)", first.status, second.status)
	c.assert(actionOf(first) == "allow" && actionOf(second) == "allow",
		"rpm: both admitted requests are allow (%q, %q)", actionOf(first), actionOf(second))

	third := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(third.status == 429, "rpm: the third request is refused with 429 (status %d)", third.status)
	c.assert(reasonOf(third) == "requests_per_minute",
		"rpm: the reason names the request-rate dimension (%q)", reasonOf(third))
	c.assert(errorType(third.body) == "infergate_quota_exceeded",
		"rpm: the error type is infergate_quota_exceeded (%q)", errorType(third.body))
	c.assert(limitOf(third) == "2", "rpm: the response names the limit that was hit (%q)", limitOf(third))
	c.assert(usedOf(third) == "2", "rpm: the response names the two requests already counted (%q)", usedOf(third))
	c.assert(strings.Contains(errorMessage(third.body), "per-minute request"),
		"rpm: the message names the dimension in words (%q)", errorMessage(third.body))
	retry, err := strconv.Atoi(retryAfterOf(third))
	c.assert(err == nil && retry >= 1 && retry <= 60,
		"rpm: Retry-After points inside the minute window (%q)", retryAfterOf(third))

	fourth := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(fourth.status == 429, "rpm: a fourth request is still refused (status %d)", fourth.status)
	c.assert(usedOf(fourth) == "2",
		"rpm: the refusal did not consume budget, so used is still 2 (%q)", usedOf(fourth))

	c.assert(st.backend.Count() == 2,
		"rpm: only the two admitted requests reached the upstream (%d calls)", st.backend.Count())

	doc, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.SettledTokens >= uint64(2*actual) })
	if c.assert(ok, "rpm: the two admitted requests settle (settled=%d)", doc.Stats.SettledTokens) {
		c.assert(doc.Stats.Allowed == 2, "rpm: two requests were allowed (%d)", doc.Stats.Allowed)
		c.assert(doc.Stats.Rejected == 2, "rpm: both refusals were counted (%d)", doc.Stats.Rejected)
		// Two admitted requests plus the two refusals, each reserving and then
		// releasing one token entry: 4 reservations of `reserve`.
		c.assert(doc.Stats.ReservedTokens == uint64(4*reserve),
			"rpm: admitted and refused requests both reserve and release a token entry (%d, want %d)",
			doc.Stats.ReservedTokens, 4*reserve)
		assertIdentity(c, "rpm", doc)
	}

	rep, ok := awaitReport(c, st.url, "acme", "", func(r quotaReport) bool { return r.RequestsThisMinute >= 2 })
	if c.assert(ok, "rpm: the tenant report counts the minute window") {
		c.assert(rep.RequestsThisMinute == 2,
			"rpm: the refused requests are not counted in the minute (%d)", rep.RequestsThisMinute)
	}
}

// ---------------------------------------------------------------------------
// 5. SESSION BUDGET
// ---------------------------------------------------------------------------

// checkSessionBudget is the isolation of two sessions behind one tenant. A
// session budget that is only keyed by tenant is worse than no session budget:
// it looks like a per-conversation cap while every conversation shares it, so
// the first long conversation locks out the tenant's other users.
//
// The check also pins the tenancy of the report: the session ledger is read
// back with the session identified, and a report that omits the session must not
// silently show the tenant's day total in its place.
func checkSessionBudget(c *checker, e *environment) {
	body := chatBody(c, "mock-gpt", "hello")
	perSession := reservedDefault(body)
	actual := int64(mockSettledTokens)

	q := quotaConfig(tenantPolicy{
		tenant:        "acme",
		tokensPerDay:  1_000_000,
		tokensPerSess: perSession,
	})
	st := e.newStack(c, q)
	if st == nil {
		return
	}
	defer st.close()

	a1 := chat(c, st.url, body, sessionHdr("acme", "a"))
	c.assert(a1.status == 200, "session: session a's first request is admitted (status %d)", a1.status)
	c.assert(actionOf(a1) == "allow", "session: session a is allow (%q)", actionOf(a1))
	settledA, ok := awaitReport(c, st.url, "acme", "a", func(r quotaReport) bool { return r.SessionTokens >= actual })
	c.assert(ok, "session: session a's own ledger settles to %d (saw %d)", actual, settledA.SessionTokens)

	a2 := chat(c, st.url, body, sessionHdr("acme", "a"))
	c.assert(a2.status == 429, "session: session a's second request is refused (status %d)", a2.status)
	c.assert(reasonOf(a2) == "tokens_per_session",
		"session: the reason names the session dimension (%q)", reasonOf(a2))
	c.assert(strings.Contains(errorMessage(a2.body), "per-session token"),
		"session: the message names the dimension in words (%q)", errorMessage(a2.body))
	c.assert(limitOf(a2) == strconv.FormatInt(perSession, 10),
		"session: the response names the per-session limit (%q, want %d)", limitOf(a2), perSession)

	// Session b is unaffected: the counter is keyed by the session, not by the
	// tenant, even though both arrive under the same tenant header.
	b1 := chat(c, st.url, body, sessionHdr("acme", "b"))
	c.assert(b1.status == 200, "session: session b is still admitted while a is exhausted (status %d)", b1.status)
	c.assert(actionOf(b1) == "allow", "session: session b is allow (%q)", actionOf(b1))
	settledB, ok := awaitReport(c, st.url, "acme", "b", func(r quotaReport) bool { return r.SessionTokens >= actual })
	c.assert(ok, "session: session b keeps its own ledger at %d (saw %d)", actual, settledB.SessionTokens)
	c.assert(settledB.Session == "b", "session: the report echoes the session it was asked about (%q)", settledB.Session)
	c.assert(settledB.SessionTokens == settledA.SessionTokens,
		"session: the two sessions hold the same amount, independently (%d vs %d)",
		settledA.SessionTokens, settledB.SessionTokens)

	c.assert(st.backend.Count() == 2,
		"session: only the admitted requests reached the upstream (%d calls)", st.backend.Count())

	doc, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.Rejected >= 1 })
	if c.assert(ok, "session: the refusal is counted (rejected=%d)", doc.Stats.Rejected) {
		c.assert(doc.Stats.Allowed == 2, "session: two requests were allowed (%d)", doc.Stats.Allowed)
		c.assert(doc.Stats.Rejected == 1, "session: exactly one was refused (%d)", doc.Stats.Rejected)
		// Three requests (two admitted, one refused) x two budgeted token
		// dimensions = six reserved token entries, each either settling short and
		// releasing the rest or settling whole.
		c.assert(doc.Stats.ReservedTokens == uint64(6*perSession),
			"session: reserved counts every budgeted token dimension (%d, want %d = 3 requests x 2 dimensions x %d)",
			doc.Stats.ReservedTokens, uint64(6*perSession), perSession)
		c.assert(doc.Stats.SettledTokens == uint64(2*2*actual),
			"session: settled counts one entry per budgeted token dimension (%d, want %d = 2 requests x 2 dimensions x %d)",
			doc.Stats.SettledTokens, uint64(4*actual), actual)
		c.assert(doc.Stats.ReleasedTokens == uint64(4*(perSession-actual)+2*perSession),
			"session: released is the unspent hold of both admitted requests plus the refused reservation (%d, want %d = 4 x (%d - %d) + 2 x %d)",
			doc.Stats.ReleasedTokens, uint64(4*(perSession-actual)+2*perSession), perSession, actual, perSession)
		// With every reserved entry counted on both sides, the identity holds for
		// a two-dimension tenant as well - it used to be short by exactly one
		// dimension's worth, which is why this check could only pin the arithmetic
		// and report the deviation.
		assertIdentity(c, "session", doc)
	}

	// The tenant total is the sum of what was really served, across sessions.
	rep, ok := awaitReport(c, st.url, "acme", "", func(r quotaReport) bool { return r.TokensToday >= 2*actual })
	if c.assert(ok, "session: the tenant ledger sums both sessions (%d)", rep.TokensToday) {
		c.assert(rep.TokensToday == 2*actual,
			"session: the day ledger is the sum of the two sessions (%d, want %d)", rep.TokensToday, 2*actual)
		c.assert(rep.Session == "", "session: a report asked without a session names none (%q)", rep.Session)
		c.assert(rep.SessionTokens == 0,
			"session: a report asked without a session does not invent session usage (%d)", rep.SessionTokens)
	}
}

// ---------------------------------------------------------------------------
// 6. COST BUDGET
// ---------------------------------------------------------------------------

// checkCostBudget is the money dimension, and it is the check that catches a
// price book that is applied for display but not for enforcement. A tenant cap
// expressed in dollars has to be enforced against the estimate at admission,
// not reconciled after the fact: an operator who sets a 100-dollar cap and finds
// it overshot by whatever the traffic happened to cost has been given a report,
// not a limit.
//
// The limit is derived from the reservation rather than chosen: one reservation
// plus a sliver is admitted, and the second request - which now carries the
// first one's settled spend in front of it - crosses. Both the limit and the
// used figure are echoed back in the refusal so a client can see the arithmetic
// it was refused by.
func checkCostBudget(c *checker, e *environment) {
	body := chatBody(c, "mock-gpt", "hello")
	reserveCost := costMicrosFor(len(body)/charsPerToken, defaultCompletionReservation)
	actualCost := costMicrosFor(mockPromptTokens, mockCompletionTokens)
	// One reservation fits; a second does not, because the second request is
	// evaluated against the first one's SETTLED spend plus its own reservation.
	// The sliver therefore has to be smaller than that settled spend - a limit
	// of "one reservation plus half of (reserve+settled)" is comfortably large
	// enough to admit the second request, which is how this check passed a
	// gateway that never enforced spend at all.
	limitMicros := reserveCost + actualCost/2

	q := quotaConfig(tenantPolicy{
		tenant:        "acme",
		tokensPerDay:  1_000_000,
		costPerDayUSD: float64(limitMicros) / 1_000_000,
	})
	st := e.newStack(c, q)
	if st == nil {
		return
	}
	defer st.close()

	first := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(first.status == 200, "cost: the first request fits the spend cap (status %d)", first.status)
	spent, ok := awaitCost(c, st.url, "acme", actualCost)
	c.assert(ok, "cost: the first request's spend settles to %d micros (saw %d)", actualCost, spent)

	second := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(second.status == 429, "cost: the second request is refused (status %d)", second.status)
	c.assert(reasonOf(second) == "cost_per_day_usd",
		"cost: the reason names the spend dimension (%q)", reasonOf(second))
	c.assert(errorType(second.body) == "infergate_quota_exceeded",
		"cost: the error type is infergate_quota_exceeded (%q)", errorType(second.body))
	c.assert(limitOf(second) == strconv.FormatInt(limitMicros, 10),
		"cost: the response names the spend limit in micro-dollars (%q, want %d)", limitOf(second), limitMicros)
	c.assert(usedOf(second) == strconv.FormatInt(actualCost, 10),
		"cost: the response names the spend already counted (%q, want %d)", usedOf(second), actualCost)
	c.assert(strings.Contains(errorMessage(second.body), "daily spend"),
		"cost: the message names the dimension in words (%q)", errorMessage(second.body))
	retry, err := strconv.Atoi(retryAfterOf(second))
	c.assert(err == nil && retry >= 1, "cost: Retry-After is at least one second (%q)", retryAfterOf(second))

	c.assert(st.backend.Count() == 1,
		"cost: the refused request never reached the upstream (%d calls)", st.backend.Count())

	rep, ok := awaitReport(c, st.url, "acme", "", func(r quotaReport) bool { return true })
	if c.assert(ok, "cost: the tenant report is readable") {
		c.assert(rep.CostTodayMicros == actualCost,
			"cost: the refused request spent nothing (%d, want %d)", rep.CostTodayMicros, actualCost)
		c.assert(rep.TokensToday == int64(mockSettledTokens),
			"cost: the token ledger is unaffected by the spend refusal (%d)", rep.TokensToday)
		c.assert(rep.CostTodayUSD > 0,
			"cost: the dollar view of the spend is populated (%v)", rep.CostTodayUSD)
	}

	doc, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.Rejected >= 1 })
	if c.assert(ok, "cost: the refusal is counted (rejected=%d)", doc.Stats.Rejected) {
		assertIdentity(c, "cost", doc)
	}

	metrics := get(c, st.url+"/metrics").body
	c.assert(metricValue(metrics, "infergate_quota_decisions_total", `action="reject"`) == 1,
		"cost: /metrics counts the refusal (%v)",
		metricValue(metrics, "infergate_quota_decisions_total", `action="reject"`))
}

// ---------------------------------------------------------------------------
// 7. DEGRADE BY MODEL
// ---------------------------------------------------------------------------

// checkDegradeByModel is the first of the two "don't refuse, rewrite" paths.
// Degrading is the option an operator picks when availability matters more than
// margin, so the two things worth proving are that the caller is NOT refused
// (a 429 here would defeat the whole point of configuring it) and that the
// rewrite reaches the upstream.
//
// That second claim cannot be made from a header. A gateway that advertises the
// downgrade model in X-InferGate-Quota-Model while forwarding the original is
// reporting a policy it is not applying, and every dashboard would agree with
// it. So this check uses the recording upstream and asserts the model it
// actually received in the request body.
//
// The tenant's budget is one token, which means the very first request breaches.
// That is deliberate: it makes the degrade deterministic instead of dependent
// on how much of a previous reservation had settled by the time the next
// request arrived. A second, well-funded tenant in the same stack is the
// control - an allowed request must be passed through untouched.
func checkDegradeByModel(c *checker, e *environment) {
	body := chatBody(c, "mock-gpt", "hello")

	q := quotaConfig(
		tenantPolicy{tenant: "limited", tokensPerDay: 1, onExceed: "degrade", downgradeModel: "mock-gpt-mini"},
		tenantPolicy{tenant: "plain", tokensPerDay: 1_000_000},
	)
	st, rec := e.newRecorderStack(c, q, mockPromptTokens, mockCompletionTokens)
	if st == nil {
		return
	}
	defer st.close()

	control := chat(c, st.url, body, tenantHdr("plain"))
	c.assert(control.status == 200 && actionOf(control) == "allow",
		"degrade(model): the well-funded control request is simply allowed (status %d, action %q)",
		control.status, actionOf(control))
	if call, ok := rec.Last(); c.assert(ok, "degrade(model): the control request reached the upstream") {
		c.assert(call.Model == "mock-gpt",
			"degrade(model): an allowed request keeps its own model (%q)", call.Model)
	}

	res := chat(c, st.url, body, tenantHdr("limited"))
	c.assert(res.status == 200,
		"degrade(model): a breached request is served, not refused (status %d)", res.status)
	c.assert(actionOf(res) == "degrade", "degrade(model): X-InferGate-Quota is degrade (%q)", actionOf(res))
	c.assert(reasonOf(res) == "tokens_per_day",
		"degrade(model): the reason still names the dimension that breached (%q)", reasonOf(res))
	c.assert(modelOf(res) == "mock-gpt-mini",
		"degrade(model): the response advertises the downgrade model (%q)", modelOf(res))
	c.assert(limitOf(res) == "1" && usedOf(res) == "0",
		"degrade(model): the response reports the breached limit and the usage before this request (%q/%q)",
		limitOf(res), usedOf(res))

	if call, ok := rec.Last(); c.assert(ok, "degrade(model): the breached request reached the upstream") {
		c.assert(call.Model == "mock-gpt-mini",
			"degrade(model): the UPSTREAM was sent the downgrade model (%q)", call.Model)
		c.assert(strings.Contains(call.Raw, `"mock-gpt-mini"`),
			"degrade(model): the recorded request body carries the downgrade model (%s)", truncate(call.Raw, 200))
	}
	c.assert(rec.Count() == 2, "degrade(model): both requests were forwarded once each (%d)", rec.Count())

	doc, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.Degraded >= 1 })
	if c.assert(ok, "degrade(model): the degrade is counted (degraded=%d)", doc.Stats.Degraded) {
		c.assert(doc.Stats.Degraded == 1, "degrade(model): exactly one request was degraded (%d)", doc.Stats.Degraded)
		c.assert(doc.Stats.Rejected == 0,
			"degrade(model): nothing was refused, which is the point of degrading (%d)", doc.Stats.Rejected)
		assertIdentity(c, "degrade(model)", doc)
	}
}

// ---------------------------------------------------------------------------
// 8. DEGRADE BY CAP
// ---------------------------------------------------------------------------

// checkDegradeByCap is the other rewrite, and the one with a real trap in it.
// Capping the completion ceiling is how an operator bounds the worst case of a
// single request without changing which model answers. Two ways to get it wrong
// are invisible from the response:
//
//   - advertising the cap in X-InferGate-Quota-Max-Tokens while forwarding the
//     caller's original ceiling, so the gateway's own accounting reserves 128
//     tokens and the upstream is free to generate 4096; and
//   - raising a ceiling that was already BELOW the cap, which would be a
//     rewrite that makes a request more expensive than the caller asked for.
//
// Both are only observable in the outgoing body, so the recording upstream
// captures max_tokens for the allowed control (unchanged), a breaching request
// that asked for far more than the cap (lowered), and a breaching request that
// asked for less (left alone).
func checkDegradeByCap(c *checker, e *environment) {
	generous := chatBodyWithCeiling(c, "mock-gpt", "hello", 4096)
	modest := chatBodyWithCeiling(c, "mock-gpt", "hello", 64)

	q := quotaConfig(
		tenantPolicy{tenant: "capped", tokensPerDay: 1, onExceed: "degrade", maxTokensCap: 128},
		tenantPolicy{tenant: "plain", tokensPerDay: 1_000_000},
	)
	st, rec := e.newRecorderStack(c, q, mockPromptTokens, mockCompletionTokens)
	if st == nil {
		return
	}
	defer st.close()

	control := chat(c, st.url, generous, tenantHdr("plain"))
	c.assert(control.status == 200 && actionOf(control) == "allow",
		"degrade(cap): the control request is allowed (status %d, action %q)", control.status, actionOf(control))
	c.assert(capOf(control) == "" && modelOf(control) == "",
		"degrade(cap): an allowed request carries no rewrite headers (cap=%q model=%q)", capOf(control), modelOf(control))
	call, ok := rec.Last()
	if c.assert(ok, "degrade(cap): the control request reached the upstream") {
		c.assert(call.MaxTokens == 4096,
			"degrade(cap): an allowed request keeps the caller's own ceiling (%d)", call.MaxTokens)
	}

	first := chat(c, st.url, generous, tenantHdr("capped"))
	c.assert(first.status == 200, "degrade(cap): a breached request is served (status %d)", first.status)
	c.assert(actionOf(first) == "degrade", "degrade(cap): X-InferGate-Quota is degrade (%q)", actionOf(first))
	c.assert(capOf(first) == "128",
		"degrade(cap): the response advertises the cap (%q)", capOf(first))
	c.assert(modelOf(first) == "",
		"degrade(cap): a cap-only policy names no downgrade model (%q)", modelOf(first))
	if call, ok := rec.Last(); c.assert(ok, "degrade(cap): the capped request reached the upstream") {
		c.assert(call.MaxTokens == 128,
			"degrade(cap): the UPSTREAM was sent the lowered ceiling (%d, want 128)", call.MaxTokens)
		c.assert(call.Model == "mock-gpt",
			"degrade(cap): a cap-only policy does not change the model (%q)", call.Model)
	}

	second := chat(c, st.url, modest, tenantHdr("capped"))
	c.assert(second.status == 200, "degrade(cap): a second breached request is served (status %d)", second.status)
	c.assert(capOf(second) == "128", "degrade(cap): the cap is advertised again (%q)", capOf(second))
	if call, ok := rec.Last(); c.assert(ok, "degrade(cap): the modest request reached the upstream") {
		c.assert(call.MaxTokens == 64,
			"degrade(cap): a ceiling already below the cap is left alone (%d, want 64)", call.MaxTokens)
	}
	c.assert(rec.Count() == 3, "degrade(cap): every request was forwarded exactly once (%d)", rec.Count())

	doc, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.Degraded >= 2 })
	if c.assert(ok, "degrade(cap): both breaches were counted as degradations (degraded=%d)", doc.Stats.Degraded) {
		c.assert(doc.Stats.Degraded == 2, "degrade(cap): exactly two requests were degraded (%d)", doc.Stats.Degraded)
		c.assert(doc.Stats.Rejected == 0, "degrade(cap): nothing was refused (%d)", doc.Stats.Rejected)
	}
}

// ---------------------------------------------------------------------------
// Small shared readers
// ---------------------------------------------------------------------------

// mapInt reads an integer out of the raw /stats JSON, where every number arrives
// as a float64. A missing key reads as zero, so callers that need to tell
// "absent" from "zero" assert the key separately.
func mapInt(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok {
		return -1
	}
	f, ok := v.(float64)
	if !ok {
		return -2
	}
	return int64(f)
}

// mapBool reads a JSON boolean out of the raw /stats JSON. It exists because the
// two surfaces disagree on the type of the same fact: /admin/quota encodes
// "enabled" as a boolean and /stats encodes it as a boolean too, but a reader
// that only handles float64 sees -2 for both and reports a false failure.
func mapBool(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// repoRoot walks up from the working directory to the module root. The config
// check loads the shipped sample files, and where the gate is run from decides
// whether a relative path to them resolves; anchoring on go.mod makes the check
// work from the repository root and from cmd/verify-m3 alike.
func repoRoot(c *checker) string {
	dir, err := os.Getwd()
	if err != nil {
		c.assert(false, "working directory: %v", err)
		return "."
	}
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	c.assert(false, "could not find the module root above the working directory")
	return "."
}
