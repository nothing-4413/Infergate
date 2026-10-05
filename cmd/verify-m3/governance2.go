package main

// M3 acceptance checks, part two: settlement, the counter store, the operational
// surfaces, and the configuration contract.
//
// Part one asserted decisions. These checks assert what the decisions LEFT
// BEHIND: the ledger that settlement writes, the Redis keys an outside reader
// has to agree about, the surfaces an operator watches, and the config
// validation that stands between a typo and a gateway that silently does not
// govern anything.

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/server"
)

// ---------------------------------------------------------------------------
// 9. SETTLE / OVERSHOOT / RELEASE
// ---------------------------------------------------------------------------

// checkSettleOvershootRelease is the accounting check, and it is the one that
// needs an upstream the in-repo mock cannot provide.
//
// Settlement has three outcomes and only two of them are reachable with a mock
// whose usage is fixed at 7 prompt + 2 completion tokens. That usage is always
// far BELOW the reservation a real request body produces (the estimator reserves
// len(body)/4 + 256), so the reserve-then-settle flow always releases; an
// overshoot - the case where the provider bills more than the gateway held back
// - cannot be produced at all. It is also the case that matters most, because it
// is the one where the ledger has to admit it charged more than it promised.
//
// So this check runs three stacks:
//
//	A. a recording upstream that reports 500 + 500 tokens against a reservation
//	   of a few dozen: the overshoot path, asserted in tokens AND micro-dollars;
//	B. the mock upstream told to fail every request: the request was attempted
//	   and billed nothing, so the whole reservation must come back;
//	C. a mock upstream that only claims to serve one model, asked for another:
//	   routing never reaches an upstream, so no attempt is made at all and the
//	   reservation must be released without a settlement.
//
// B and C are different failures - a provider that answered with an error, and
// a request that never left the gateway - and a ledger that handles one but not
// the other leaves a permanent leak: every such request would keep its
// reservation and slowly lock a tenant out of its own budget.
func checkSettleOvershootRelease(c *checker, e *environment) {
	// --- A: overshoot -----------------------------------------------------
	body := chatBodyWithCeiling(c, "mock-gpt", "hello", 2)
	hold := reservedFor(body, 2)
	estPrompt := len(body) / charsPerToken
	holdCost := costMicrosFor(estPrompt, 2)
	usagePrompt, usageCompletion := 500, 500
	billedTokens := int64(usagePrompt + usageCompletion)
	billedCost := costMicrosFor(usagePrompt, usageCompletion)
	overshoot := billedTokens - hold

	q := quotaConfig(tenantPolicy{tenant: "acme", tokensPerDay: 1_000_000, costPerDayUSD: 100})
	st, rec := e.newRecorderStack(c, q, usagePrompt, usageCompletion)
	if st == nil {
		return
	}
	defer st.close()

	res := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(res.status == 200 && actionOf(res) == "allow",
		"settle/overshoot: the request is admitted under a generous budget (status %d, action %q)",
		res.status, actionOf(res))

	doc, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.SettledTokens >= uint64(billedTokens) })
	if !c.assert(ok, "settle/overshoot: settlement lands in the ledger (settled=%d, want %d)",
		doc.Stats.SettledTokens, billedTokens) {
		return
	}
	c.assert(doc.Stats.ReservedTokens == uint64(hold),
		"settle/overshoot: the reservation was the estimate, not the usage (%d, want %d)",
		doc.Stats.ReservedTokens, hold)
	c.assert(doc.Stats.SettledTokens == uint64(billedTokens),
		"settle/overshoot: settled_tokens is what the provider actually billed (%d, want %d)",
		doc.Stats.SettledTokens, billedTokens)
	c.assert(doc.Stats.OvershootTokens == uint64(overshoot),
		"settle/overshoot: the amount billed over the reservation is recorded as overshoot (%d, want %d)",
		doc.Stats.OvershootTokens, overshoot)
	c.assert(doc.Stats.ReleasedTokens == 0,
		"settle/overshoot: nothing was released when the hold was exceeded (%d)", doc.Stats.ReleasedTokens)
	c.assert(doc.Stats.OvershootCostMicro == billedCost-holdCost,
		"settle/overshoot: the money overshoot is the billed spend over the held spend (%d, want %d)",
		doc.Stats.OvershootCostMicro, billedCost-holdCost)
	c.assert(doc.Stats.ReleasedCostMicro == 0,
		"settle/overshoot: no spend was released either (%d)", doc.Stats.ReleasedCostMicro)
	assertIdentity(c, "settle/overshoot", doc)

	rep, ok := awaitReport(c, st.url, "acme", "", func(r quotaReport) bool { return r.CostTodayMicros >= billedCost })
	if c.assert(ok, "settle/overshoot: the tenant report carries the settled spend (%d)", rep.CostTodayMicros) {
		c.assert(rep.TokensToday == billedTokens,
			"settle/overshoot: the day counter holds the billed tokens, not the hold (%d, want %d)",
			rep.TokensToday, billedTokens)
		c.assert(rep.CostTodayMicros == billedCost,
			"settle/overshoot: the day cost counter holds the billed spend (%d, want %d)",
			rep.CostTodayMicros, billedCost)
	}

	metrics := get(c, st.url+"/metrics").body
	metricOvershoot, present := scalarMetric(metrics, "infergate_quota_overshoot_tokens_total")
	c.assert(present && metricOvershoot == float64(overshoot),
		"settle/overshoot: /metrics exposes the token overshoot (%v, present=%v, want %d)",
		metricOvershoot, present, overshoot)
	metricOvershootCost, present := scalarMetric(metrics, "infergate_quota_overshoot_cost_micros_total")
	c.assert(present && metricOvershootCost == float64(billedCost-holdCost),
		"settle/overshoot: /metrics exposes the spend overshoot (%v, present=%v, want %d)",
		metricOvershootCost, present, billedCost-holdCost)
	if call, ok := rec.Last(); c.assert(ok, "settle/overshoot: the upstream saw the request") {
		c.assert(strings.Contains(call.Raw, `"max_tokens":2`),
			"settle/overshoot: the outgoing body kept the caller's ceiling (%s)", truncate(call.Raw, 160))
	}

	// --- B: the upstream failed -------------------------------------------
	bodyB := chatBody(c, "mock-gpt", "hello")
	holdB := reservedDefault(bodyB)
	actual := int64(mockSettledTokens)
	actualCost := costMicrosFor(mockPromptTokens, mockCompletionTokens)
	holdCostB := costMicrosFor(len(bodyB)/charsPerToken, defaultCompletionReservation)

	stB := e.newStack(c, quotaConfig(tenantPolicy{tenant: "acme", tokensPerDay: 1_000_000, costPerDayUSD: 100}))
	if stB == nil {
		return
	}
	defer stB.close()

	good := chat(c, stB.url, bodyB, tenantHdr("acme"))
	c.assert(good.status == 200, "settle/release: the first request succeeds (status %d)", good.status)
	if _, ok := awaitTokens(c, stB.url, "acme", actual); !ok {
		c.assert(false, "settle/release: the first request did not settle in time")
		return
	}

	stB.backend.SetFailStatus(500)
	failed := chat(c, stB.url, bodyB, tenantHdr("acme"))
	c.assert(failed.status != 200,
		"settle/release: a request whose upstream fails is not reported as success (status %d)", failed.status)
	c.assert(stB.backend.Count() == 2,
		"settle/release: the failing request WAS attempted upstream, so it must be released not voided (%d calls)",
		stB.backend.Count())

	docB, ok := awaitStats(c, stB.url, func(d adminQuotaDoc) bool {
		return d.Stats.ReleasedTokens >= uint64((holdB-actual)+holdB)
	})
	if c.assert(ok, "settle/release: the failed request's reservation comes back (%d released, want %d)",
		docB.Stats.ReleasedTokens, (holdB-actual)+holdB) {
		c.assert(docB.Stats.SettledTokens == uint64(actual),
			"settle/release: a failed request settles to nothing, so only the first is charged (%d, want %d)",
			docB.Stats.SettledTokens, actual)
		c.assert(docB.Stats.OvershootTokens == 0,
			"settle/release: nothing overshot (%d)", docB.Stats.OvershootTokens)
		assertIdentity(c, "settle/release(upstream error)", docB)
	}
	repB, ok := awaitReport(c, stB.url, "acme", "", func(r quotaReport) bool { return true })
	if c.assert(ok, "settle/release: the tenant report is readable after the failure") {
		c.assert(repB.TokensToday == actual,
			"settle/release: the failed request left no tokens in the ledger (%d, want %d)", repB.TokensToday, actual)
	}
	releasedCost, present := scalarMetric(get(c, stB.url+"/metrics").body, "infergate_quota_released_cost_micros_total")
	c.assert(present && releasedCost == float64((holdCostB-actualCost)+holdCostB),
		"settle/release: the failed request's reserved spend is released too (%v, present=%v, want %d)",
		releasedCost, present, (holdCostB-actualCost)+holdCostB)

	// --- C: the request never reached an upstream -------------------------
	// Zero attempts is the rarer of the two failure paths, and it is the one
	// that must RELEASE rather than settle: admission already reserved budget, and
	// nothing was spent, so a gateway that settled here would charge the tenant
	// for a request it never sent. Leaning on the router to reject a model it does
	// not serve does not reach it - a single backend with the "/" catch-all
	// happily rewrites an unknown model name to its own - so the request is pinned
	// to an upstream that does not exist, which fails the plan before any attempt.
	stC := e.start(c, stackSpec{
		quota: quotaConfig(tenantPolicy{tenant: "acme", tokensPerDay: 1_000_000, costPerDayUSD: 100}),
	})
	if stC == nil {
		return
	}
	defer stC.close()

	ok1 := chat(c, stC.url, bodyB, tenantHdr("acme"))
	c.assert(ok1.status == 200, "settle/release(no attempt): the routable request succeeds (status %d)", ok1.status)
	if _, ok := awaitTokens(c, stC.url, "acme", actual); !ok {
		c.assert(false, "settle/release(no attempt): the routable request did not settle in time")
		return
	}

	unroutable := chat(c, stC.url, bodyB, pinnedHdr("acme", "no-such-upstream"))
	c.info("a request pinned to a nonexistent upstream answered %d: %s", unroutable.status, truncate(unroutable.body, 160))
	c.assert(unroutable.status == 400,
		"settle/release(no attempt): an unroutable request is the caller's error, not a gateway failure (status %d, want 400)",
		unroutable.status)
	c.assert(errorType(unroutable.body) == "infergate_no_upstream",
		"settle/release(no attempt): the error type names the routing failure (%q)", errorType(unroutable.body))
	c.assert(stC.backend.Count() == 1,
		"settle/release(no attempt): no attempt was made for the unroutable request (%d calls)", stC.backend.Count())

	docC, ok := awaitStats(c, stC.url, func(d adminQuotaDoc) bool {
		return d.Stats.ReleasedTokens >= uint64((holdB-actual)+holdB)
	})
	if c.assert(ok, "settle/release(no attempt): the reservation is released (%d, want %d)",
		docC.Stats.ReleasedTokens, (holdB-actual)+holdB) {
		c.assert(docC.Stats.SettledTokens == uint64(actual),
			"settle/release(no attempt): nothing was settled for a request that never left (%d, want %d)",
			docC.Stats.SettledTokens, actual)
		assertIdentity(c, "settle/release(no attempt)", docC)
	}
	repC, ok := awaitReport(c, stC.url, "acme", "", func(r quotaReport) bool { return true })
	if c.assert(ok, "settle/release(no attempt): the tenant report is readable") {
		c.assert(repC.TokensToday == actual,
			"settle/release(no attempt): the unroutable request left no tokens behind (%d, want %d)",
			repC.TokensToday, actual)
	}

	// The money side of the same reservation comes back too. This is the
	// regression test for an asymmetry a gate run found: Release() credited
	// released_tokens with the whole reservation and rolled the store back entry
	// by entry, but it never touched released_cost_micros, while Settle() credits
	// it for the very same kind of refund (part B above). A tenant's spend audit
	// was therefore short by every zero-attempt request's reserved spend.
	c.assert(docC.Stats.ReleasedCostMicro == (holdCostB-actualCost)+holdCostB,
		"settle/release(no attempt): the unroutable request's reserved spend is released too (%d, want %d = %d settled hold + %d unroutable hold)",
		docC.Stats.ReleasedCostMicro, (holdCostB-actualCost)+holdCostB, holdCostB-actualCost, holdCostB)
}

// ---------------------------------------------------------------------------
// 10. REDIS STORE
// ---------------------------------------------------------------------------

// checkRedisStore asserts the store contract that makes Redis worth using: the
// key layout is a public interface.
//
// A Redis-backed limiter is shared with whatever else an operator runs against
// the same server - a cost report, an alert, a Grafana query, a second gateway
// instance - so the layout is not an implementation detail. This check reads
// every counter back through the raw client on the documented key, compares it
// with what the admin surface reports for the same tenant, and then writes to
// the key from the outside and watches the gateway's own report move. That last
// step is what proves the two are talking about the same counter rather than
// coincidentally agreeing about two numbers that happen to be equal.
//
// Expiry gets the same treatment from both sides: a TTL must exist and be
// roughly the window it belongs to, and a second increment must not extend it.
// The second-increment case is the one that bites: a naive INCRBY-then-EXPIRE
// resets the window on every request, so a steady tenant's counter never
// expires and its budget never resets.
func checkRedisStore(c *checker, e *environment) {
	body := chatBody(c, "mock-gpt", "hello")
	perSession := reservedDefault(body)
	actual := int64(mockSettledTokens)
	actualCost := costMicrosFor(mockPromptTokens, mockCompletionTokens)
	session := "sess-redis-1"

	q := quotaConfig(tenantPolicy{
		tenant:         "acme",
		tokensPerDay:   1_000_000,
		costPerDayUSD:  100,
		requestsPerMin: 100,
		// Two requests must fit in the session budget: a budget of exactly one
		// reservation admits the first request, settles it down to the upstream's
		// real usage, and then refuses the second for the 9 tokens the first one
		// left behind - which is correct behaviour and a useless fixture.
		tokensPerSess: 2*perSession + 100,
	})
	st := e.newRedisStack(c, q)
	if st == nil {
		return
	}
	defer st.close()
	if st.redis == nil {
		c.assert(false, "redis: the in-process Redis server was not started")
		return
	}

	first := chat(c, st.url, body, sessionHdr("acme", session))
	second := chat(c, st.url, body, sessionHdr("acme", session))
	c.assert(first.status == 200 && second.status == 200,
		"redis: both requests are admitted (status %d, %d)", first.status, second.status)
	if _, ok := awaitTokens(c, st.url, "acme", 2*actual); !ok {
		c.assert(false, "redis: the two requests did not settle in time")
		return
	}

	doc, ok := adminQ(c, st.url, "acme", session)
	if !c.assert(ok && doc.Report != nil, "redis: /admin/quota answers for the tenant and session") {
		return
	}
	c.assert(doc.Store == "redis", "redis: /admin/quota names the redis store (%q)", doc.Store)

	rep := *doc.Report
	// The bucket in the key is a UTC bucket: internal/quota formats
	// m.now().UTC(), so deriving it from the local clock would look for the wrong
	// key for the eight hours a day in which the two dates differ here (see
	// dayBucket).
	now := time.Now()
	day := dayBucket(now)
	minute := minuteBucket(now)
	keyDay := "ig:quota:acme:day:" + day + ":tokens"
	keyCost := "ig:quota:acme:day:" + day + ":cost_micros"
	keyMinute := "ig:quota:acme:minute:" + minute + ":requests"
	keySession := "ig:quota:acme:session:" + session + ":tokens"

	rr := newRawRedis(c, st.redis.Addr())
	if rr == nil {
		return
	}
	defer rr.close()

	cases := []struct {
		key  string
		want int64
		what string
	}{
		{keyDay, rep.TokensToday, "the day token counter"},
		{keyCost, rep.CostTodayMicros, "the day spend counter"},
		{keyMinute, rep.RequestsThisMinute, "the minute request counter"},
		{keySession, rep.SessionTokens, "the session token counter"},
	}
	for _, tc := range cases {
		got, present := rr.get(tc.key)
		c.assert(present && got == tc.want,
			"redis: %s at %s holds %d, matching the report's %d (present=%v)",
			tc.what, tc.key, got, tc.want, present)
	}
	c.assert(rep.TokensToday == 2*actual,
		"redis: the day counter accumulated both requests (%d, want %d)", rep.TokensToday, 2*actual)
	c.assert(rep.CostTodayMicros == 2*actualCost,
		"redis: the spend counter accumulated both requests (%d, want %d)", rep.CostTodayMicros, 2*actualCost)
	c.assert(rep.RequestsThisMinute == 2,
		"redis: the minute counter counts requests, not tokens (%d, want 2)", rep.RequestsThisMinute)
	c.assert(rep.SessionTokens == 2*actual,
		"redis: the session counter accumulated both requests (%d, want %d)", rep.SessionTokens, 2*actual)

	keys := rr.keys("ig:quota:acme:*")
	c.assert(len(keys) >= 4, "redis: every budgeted dimension has a key (%d keys: %v)", len(keys), keys)
	for _, want := range []string{keyDay, keyCost, keyMinute, keySession} {
		found := false
		for _, k := range keys {
			if k == want {
				found = true
			}
		}
		c.assert(found, "redis: KEYS ig:quota:acme:* includes %s", want)
	}

	// Expiry: present, and covering the day the key names. The day key must
	// outlive its own UTC day, or a tenant's daily budget would reset while the
	// day it counts is still running; internal/quota adds one hour of slack on
	// top of the time to the next UTC midnight, and that hour is pinned here
	// because it is the difference between "expires with the day" and "expires
	// just after it".
	remainingDay := secondsToNextUTCMidnight(now)
	ttlDay := rr.ttl(keyDay)
	c.assert(ttlDay > 0, "redis: the day counter has an expiry (%d)", ttlDay)
	c.assert(ttlDay >= remainingDay+3590 && ttlDay <= remainingDay+3610,
		"redis: the day counter expires with its own UTC day plus the one-hour slack (%d, want ~%d)",
		ttlDay, remainingDay+3600)
	c.assert(ttlDay >= remainingDay,
		"redis: the day counter's expiry outlives the day it is named for (%d, want >= %d)",
		ttlDay, remainingDay)
	ttlMinute := rr.ttl(keyMinute)
	c.assert(ttlMinute > 0 && ttlMinute <= 120,
		"redis: the minute counter expires with its minute window (%d)", ttlMinute)
	ttlSession := rr.ttl(keySession)
	c.assert(ttlSession > 0 && ttlSession <= 86400,
		"redis: the session counter expires with the session window (%d)", ttlSession)

	// A second increment must not push the expiry out. This is the last thing
	// the check does, because it deliberately writes a value the gateway did not
	// compute.
	before := rr.ttl(keyDay)
	value, present := rr.get(keyDay)
	if c.assert(present, "redis: the day counter is readable before the raw increment") {
		c.assert(rr.incrBy(keyDay, 5), "redis: the raw client can increment the day counter")
		after := rr.ttl(keyDay)
		c.assert(after <= before,
			"redis: a second increment does not extend the TTL (%d -> %d)", before, after)
		got, present := rr.get(keyDay)
		c.assert(present && got == value+5,
			"redis: the raw increment moved the counter by exactly five (%d -> %d)", value, got)
		// And the gateway reads that same key: a report taken now must show the
		// number an outside writer just put there.
		doc, ok := adminQ(c, st.url, "acme", "")
		if c.assert(ok && doc.Report != nil, "redis: /admin/quota answers after the raw increment") {
			c.assert(doc.Report.TokensToday == value+5,
				"redis: the gateway reports the value the raw client wrote (%d, want %d)",
				doc.Report.TokensToday, value+5)
		}
		other, ok := adminQ(c, st.url, "nobody", "")
		if c.assert(ok && other.Report != nil, "redis: /admin/quota answers for an unknown tenant") {
			c.assert(other.Report.TokensToday == 0,
				"redis: an ungoverned tenant reads its own empty ledger (%d)", other.Report.TokensToday)
		}
	}
}

// ---------------------------------------------------------------------------
// 11. KEY ISOLATION
// ---------------------------------------------------------------------------

// checkKeyIsolation is about a tenant name that is not a clean identifier.
//
// The counter key is assembled from the tenant name, so a name containing the
// key separator is the classic way to make one tenant's ledger overlap
// another's - and the tenant name arrives from a request header, which means it
// is caller-controlled. internal/quota escapes every character outside
// [a-zA-Z0-9-.] as "_x" plus six hex digits (and "_" as "__"), so a value cannot
// inject a separator AND two different names cannot collide: the encoding is
// injective. This check asserts the consequence that matters: a request naming
// `acme:inc` cannot touch the ledger of `acme_inc`, and `acme`'s budget cannot be
// spent by it.
//
// The two halves are asserted separately, because they are reached by different
// paths:
//
//   - An unconfigured crafted name is governed by the default policy. On the
//     shipped defaults that policy sets no limit at all, and Admit returns
//     "within budget" without touching the store - so the crafted request charges
//     nobody, not even a tenant whose name it resembles.
//   - A tenant configured under a name the escape rewrites (here `acme:inc`) owns
//     its own counter: the policy is looked up by the raw name and the counter is
//     keyed by the escaped one, and because the escape is injective those are two
//     different keys. An earlier release used a many-to-one mapping that made the
//     crafted name drain `acme_inc`'s budget; a gate run found it, and this check
//     is now the regression test for its absence.
func checkKeyIsolation(c *checker, e *environment) {
	body := chatBody(c, "mock-gpt", "hello")
	hold := reservedDefault(body)
	actual := int64(mockSettledTokens)
	victimBudget := 2*hold + 100

	// --- 1: an unconfigured crafted name charges nobody -------------------
	q := quotaConfig(
		tenantPolicy{tenant: "acme", tokensPerDay: victimBudget},
		tenantPolicy{tenant: "acme_inc", tokensPerDay: hold},
	)
	st := e.newStack(c, q)
	if st == nil {
		return
	}
	defer st.close()

	first := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(first.status == 200 && actionOf(first) == "allow",
		"isolation: the real tenant is admitted (status %d, action %q)", first.status, actionOf(first))
	if _, ok := awaitTokens(c, st.url, "acme", actual); !ok {
		c.assert(false, "isolation: the real tenant's first request did not settle in time")
		return
	}

	// The crafted name is not a configured tenant, so it falls to the default
	// policy - which is unbounded on the shipped defaults.
	crafted := chat(c, st.url, body, tenantHdr("acme:inc"))
	c.assert(crafted.status == 200 && actionOf(crafted) == "allow",
		"isolation: the crafted tenant name is admitted (status %d, action %q)", crafted.status, actionOf(crafted))
	c.assert(reasonOf(crafted) == "within-budget",
		"isolation: the crafted name is judged by the default policy (%q)", reasonOf(crafted))
	c.assert(limitOf(crafted) == "",
		"isolation: the unbounded default policy sets no limit for the crafted name (%q)", limitOf(crafted))

	// An unbounded policy never writes a counter, which is the mechanism that
	// makes a caller-invented name harmless: it cannot charge a key it resembles,
	// because it is not measured at all.
	craftedLedger, ok := awaitReport(c, st.url, "acme:inc", "", func(r quotaReport) bool { return true })
	c.assert(ok && craftedLedger.TokensToday == 0,
		"isolation: the crafted name charged nothing to any ledger (%d tokens, want 0)", craftedLedger.TokensToday)
	configuredLedger, ok := awaitReport(c, st.url, "acme_inc", "", func(r quotaReport) bool { return true })
	c.assert(ok && configuredLedger.TokensToday == 0,
		"isolation: the tenant whose key the crafted name resembles is untouched (%d tokens, want 0)",
		configuredLedger.TokensToday)

	// And so the resembling tenant can still spend: its budget is intact, which
	// is the observable form of "the crafted request could not charge it".
	resembling := chat(c, st.url, body, tenantHdr("acme_inc"))
	c.assert(resembling.status == 200 && actionOf(resembling) == "allow",
		"isolation: the resembling tenant still has its whole budget (status %d, action %q)",
		resembling.status, actionOf(resembling))
	if _, ok := awaitTokens(c, st.url, "acme_inc", actual); !ok {
		c.assert(false, "isolation: the resembling tenant's request did not settle in time")
		return
	}

	// The two ledgers are independent: the resembling tenant holds only its own
	// request while the real tenant holds only its own two.
	second := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(second.status == 200 && actionOf(second) == "allow",
		"isolation: the real tenant still has its budget after the crafted traffic (status %d, action %q)",
		second.status, actionOf(second))
	after, ok := awaitTokens(c, st.url, "acme", 2*actual)
	c.assert(ok, "isolation: the real tenant's ledger grows by its own requests only (%d, want %d)", after, 2*actual)

	resemblingLedger, ok := awaitReport(c, st.url, "acme_inc", "", func(r quotaReport) bool { return r.TokensToday == actual })
	c.assert(ok, "isolation: the resembling tenant's ledger holds only its own request (%d, want %d)",
		resemblingLedger.TokensToday, actual)
	c.assert(after != resemblingLedger.TokensToday,
		"isolation: the two tenants hold different ledgers (%d vs %d)", after, resemblingLedger.TokensToday)
	c.assert(craftedLedger.TokensToday != resemblingLedger.TokensToday,
		"isolation: the crafted name is not a second name for the resembling tenant's ledger (%d vs %d)",
		craftedLedger.TokensToday, resemblingLedger.TokensToday)

	// Four requests were admitted (two for acme, one crafted, one resembling) and
	// every one of them reached the upstream: isolation must not mean refusal.
	c.assert(st.backend.Count() == 4,
		"isolation: every admitted request was forwarded (%d calls, want 4)", st.backend.Count())

	// --- 2: two names that differ only by an escaped character ------------
	// A tenant literally named `acme:inc` next to `acme_inc`. The policy comes
	// from the raw name, the counter from the escaped one, and the escape is
	// injective - so these are two tenants with two budgets.
	q2 := quotaConfig(
		tenantPolicy{tenant: "acme:inc", tokensPerDay: 1_000_000},
		tenantPolicy{tenant: "acme_inc", tokensPerDay: hold},
	)
	st2 := e.newStack(c, q2)
	if st2 == nil {
		return
	}
	defer st2.close()

	spender := chat(c, st2.url, body, tenantHdr("acme:inc"))
	c.assert(spender.status == 200 && actionOf(spender) == "allow",
		"isolation: the configured colon-named tenant is admitted under its own generous budget (status %d)", spender.status)
	if _, ok := awaitTokens(c, st2.url, "acme:inc", actual); !ok {
		c.assert(false, "isolation: the colon-named tenant's usage did not land in its own counter in time")
		return
	}

	// The resembling tenant never saw that request: its counter is empty, so its
	// own request fits its own (much smaller) budget.
	resembling2 := chat(c, st2.url, body, tenantHdr("acme_inc"))
	c.assert(resembling2.status == 200 && actionOf(resembling2) == "allow",
		"isolation: the tenant that differs only by the escaped character is not charged (status %d, action %q)",
		resembling2.status, actionOf(resembling2))
	if _, ok := awaitTokens(c, st2.url, "acme_inc", actual); !ok {
		c.assert(false, "isolation: the resembling tenant's usage did not land in its own counter in time")
		return
	}

	// And the limit it is held to is its OWN: one more request of the same size
	// does not fit inside `hold`, while the colon-named tenant's budget (1e6) is
	// nowhere near its ceiling.
	drained := chat(c, st2.url, body, tenantHdr("acme_inc"))
	c.assert(drained.status == 429,
		"isolation: the resembling tenant is refused by its own budget, not another name's counter (status %d)", drained.status)
	c.assert(limitOf(drained) == strconv.FormatInt(hold, 10),
		"isolation: the refusal enforces the resembling tenant's OWN limit (%q, want %d)",
		limitOf(drained), hold)
	c.assert(limitOf(drained) != "1000000",
		"isolation: the refusal does not borrow the colon-named tenant's larger budget (%q)", limitOf(drained))
	c.assert(st2.backend.Count() == 2,
		"isolation: only the two admitted requests were forwarded (%d calls, want 2)", st2.backend.Count())

	// The two counters are distinct keys, which is the mechanism behind all of
	// the above.
	colonLedger, ok := awaitReport(c, st2.url, "acme:inc", "", func(r quotaReport) bool { return r.TokensToday == actual })
	c.assert(ok && colonLedger.TokensToday == actual,
		"isolation: the colon-named tenant's ledger holds its own request (%d, want %d)", colonLedger.TokensToday, actual)
	underscoreLedger, ok := awaitReport(c, st2.url, "acme_inc", "", func(r quotaReport) bool { return r.TokensToday == actual })
	c.assert(ok && underscoreLedger.TokensToday == actual,
		"isolation: the resembling tenant's ledger holds its own request (%d, want %d)", underscoreLedger.TokensToday, actual)
}

// ---------------------------------------------------------------------------
// 12. FAIL CLOSED vs FAIL OPEN
// ---------------------------------------------------------------------------

// checkFailClosedOpen is the availability-versus-accounting decision, and it is
// asserted at three levels because there are three different ways a counter
// store can be unavailable.
//
//  1. At startup. A Redis-backed limiter that cannot reach its store is not
//     "up but blind": with fail_open false the server refuses to start at all,
//     which is the strongest possible form of closing the gate and the one an
//     operator will actually notice. This is asserted directly against
//     server.NewServer, with no listener.
//  2. At run time with fail_open false. The store dies mid-flight; every
//     governed request must answer 503 (not 200, and not 429, which would send
//     a client to sleep for a window that means nothing) and must not be
//     forwarded, because a request that is neither accounted for nor refused is
//     a request that bypassed governance.
//  3. At run time with fail_open true. The same outage must keep serving: the
//     response is admitted, the request reaches the upstream, and BOTH the admin
//     surface and the metrics record a store error. Without those counters a
//     fail-open gateway is indistinguishable from a healthy one while it
//     accounts for nothing at all.
func checkFailClosedOpen(c *checker, e *environment) {
	// --- 1: a store that is unreachable at startup ------------------------
	dead := deadAddr(c)
	qDead := quotaConfig(tenantPolicy{tenant: "acme", tokensPerDay: 1000})
	qDead.Store = config.QuotaStoreRedis
	qDead.FailOpen = false
	qDead.Redis.Addr = dead
	cfg := baseConfig("http://127.0.0.1:1")
	cfg.Quota = qDead
	srv, err := server.NewServer(&cfg, newLogger(c))
	c.assert(err != nil,
		"fail-closed(startup): a redis-backed gateway with a dead store refuses to start (err=%v)", err)
	if err != nil {
		c.assert(strings.Contains(err.Error(), "quota"),
			"fail-closed(startup): the startup error names the quota subsystem (%v)", err)
		c.assert(strings.Contains(err.Error(), dead),
			"fail-closed(startup): the startup error names the address that failed (%v)", err)
	} else {
		_ = srv.CloseQuota()
		_ = srv.Shutdown(context.Background())
	}

	body := chatBody(c, "mock-gpt", "hello")

	// --- 2: the store dies while the gateway is up, fail_open false -------
	qClosed := quotaConfig(tenantPolicy{tenant: "acme", tokensPerDay: 1_000_000})
	qClosed.FailOpen = false
	stClosed := e.newRedisStack(c, qClosed)
	if stClosed == nil {
		return
	}
	defer stClosed.close()

	if err := stClosed.redis.Close(); err != nil {
		c.assert(false, "fail-closed(runtime): could not stop the in-process store: %v", err)
		return
	}
	closed := chat(c, stClosed.url, body, tenantHdr("acme"))
	c.assert(closed.status == 503,
		"fail-closed(runtime): the request is refused with 503 when the store is gone (status %d)", closed.status)
	c.assert(errorType(closed.body) == "infergate_quota_unavailable",
		"fail-closed(runtime): the error type is infergate_quota_unavailable (%q)", errorType(closed.body))
	c.assert(actionOf(closed) == "reject",
		"fail-closed(runtime): the response's quota action is reject (%q)", actionOf(closed))
	c.assert(reasonOf(closed) == "store-error",
		"fail-closed(runtime): the reason names the store, not a budget (%q)", reasonOf(closed))
	c.assert(stClosed.backend.Count() == 0,
		"fail-closed(runtime): the request was NOT forwarded upstream (%d calls)", stClosed.backend.Count())
	closedDoc, ok := awaitStats(c, stClosed.url, func(d adminQuotaDoc) bool { return d.Stats.StoreErrors >= 1 })
	c.assert(ok, "fail-closed(runtime): the store error is counted on the admin surface (%d)", closedDoc.Stats.StoreErrors)
	// A refusal the gateway made is a refusal it must count: a store outage that
	// produced 503s and left rejected at 0 would make the decision counters
	// undercount exactly when someone is watching them.
	c.assert(closedDoc.Stats.Rejected >= 1,
		"fail-closed(runtime): the refused request is counted as rejected (%d)", closedDoc.Stats.Rejected)
	c.assert(closedDoc.Stats.Allowed == 0,
		"fail-closed(runtime): nothing was admitted while the store was gone (%d)", closedDoc.Stats.Allowed)
	if got, present := awaitScalarMetric(c, stClosed.url, "infergate_quota_store_errors_total", 1); true {
		c.assert(present && got >= 1,
			"fail-closed(runtime): /metrics carries the store error (%v, present=%v)", got, present)
	}

	// --- 3: the same outage with fail_open true ---------------------------
	qOpen := quotaConfig(tenantPolicy{tenant: "acme", tokensPerDay: 1_000_000})
	qOpen.FailOpen = true
	stOpen := e.newRedisStack(c, qOpen)
	if stOpen == nil {
		return
	}
	defer stOpen.close()

	if err := stOpen.redis.Close(); err != nil {
		c.assert(false, "fail-open(runtime): could not stop the in-process store: %v", err)
		return
	}
	open := chat(c, stOpen.url, body, tenantHdr("acme"))
	c.assert(open.status == 200,
		"fail-open(runtime): the request is still served while the store is gone (status %d)", open.status)
	c.assert(actionOf(open) == "allow" && reasonOf(open) == "store-error",
		"fail-open(runtime): the response admits the request and says why (action=%q reason=%q)",
		actionOf(open), reasonOf(open))
	c.assert(stOpen.backend.Count() == 1,
		"fail-open(runtime): the admitted request reached the upstream (%d calls)", stOpen.backend.Count())
	openDoc, ok := awaitStats(c, stOpen.url, func(d adminQuotaDoc) bool { return d.Stats.StoreErrors >= 1 })
	if c.assert(ok, "fail-open(runtime): the store error is counted on the admin surface (%d)", openDoc.Stats.StoreErrors) {
		c.assert(openDoc.Stats.ReservedTokens == 0,
			"fail-open(runtime): nothing was reserved, because nothing was measured (%d)", openDoc.Stats.ReservedTokens)
		c.assert(openDoc.Stats.SettledTokens == 0,
			"fail-open(runtime): nothing was settled (%d)", openDoc.Stats.SettledTokens)
		c.assert(openDoc.Stats.StoreErrors >= 1,
			"fail-open(runtime): store_errors is the signal that this happened (%d)", openDoc.Stats.StoreErrors)
		// A fail-open admission is still an admission, and it has to appear in
		// the decision counters: leaving it out made allowed+degraded+rejected
		// undercount the requests the gateway actually served, exactly while the
		// store was down and those numbers were the only evidence.
		c.assert(openDoc.Stats.Allowed >= 1,
			"fail-open(runtime): the admitted request is counted as allowed (%d)", openDoc.Stats.Allowed)
		c.assert(openDoc.Stats.Rejected == 0,
			"fail-open(runtime): nothing was refused (%d)", openDoc.Stats.Rejected)
		// The decision counter is labelled, so it must be read with its label:
		// a bare series name matches no sample and quietly reads zero.
		openMetrics := get(c, stOpen.url+"/metrics").body
		c.assert(metricValue(openMetrics, "infergate_quota_decisions_total", `action="allow"`) >= 1,
			"fail-open(runtime): /metrics counts the fail-open decision as an allow (%v)",
			metricValue(openMetrics, "infergate_quota_decisions_total", `action="allow"`))
		if got, present := scalarMetric(openMetrics, "infergate_quota_store_errors_total"); true {
			c.assert(present && got >= 1,
				"fail-open(runtime): /metrics carries the store error too (%v, present=%v)", got, present)
		}
	}
}

// ---------------------------------------------------------------------------
// 13. NON-COMPLETION PATH
// ---------------------------------------------------------------------------

// checkNonCompletionPath is about requests that are not billable completions.
//
// Governance is attached to the completion path, and everything else - the
// health probe a load balancer hammers, the model list a client fetches - must
// pass through untouched. Two failures are worth guarding against. A health
// probe that consumes quota will eventually take a tenant's budget offline by
// itself, and it will do it fastest exactly when a supervisor is retrying, which
// is the worst possible moment. A model list that carries a quota header teaches
// clients to expect governance metadata on endpoints that were never governed.
//
// The read is deliberately a comparison of the whole stats document before and
// after rather than of one counter: a non-completion path that moved anything -
// an allow, a reservation, an alert - is a finding, and a whole-document
// comparison cannot miss a surface that was added later.
func checkNonCompletionPath(c *checker, e *environment) {
	q := quotaConfig(tenantPolicy{tenant: "acme", tokensPerDay: 1_000_000})
	st := e.newStack(c, q)
	if st == nil {
		return
	}
	defer st.close()

	before, ok := adminQ(c, st.url, "", "")
	if !c.assert(ok, "non-completion: the stats surface is readable before the probes") {
		return
	}

	health := get(c, st.url+"/healthz")
	c.assert(health.status == 200, "non-completion: /healthz answers 200 (status %d)", health.status)
	c.assert(len(quotaHeaders(health)) == 0,
		"non-completion: /healthz carries no quota header (saw %v)", quotaHeaders(health))

	models := get(c, st.url+"/v1/models")
	c.assert(models.status == 200, "non-completion: /v1/models answers 200 (status %d)", models.status)
	c.assert(len(quotaHeaders(models)) == 0,
		"non-completion: /v1/models carries no quota header (saw %v)", quotaHeaders(models))
	c.info("/v1/models was answered with %d upstream calls, so it did not consume governance or an upstream request",
		st.backend.Count())
	c.assert(st.backend.Count() == 0,
		"non-completion: neither probe was forwarded as a governed request (%d calls)", st.backend.Count())

	rep, ok := adminQ(c, st.url, "acme", "")
	if c.assert(ok && rep.Report != nil, "non-completion: the tenant report is readable") {
		c.assert(rep.Report.TokensToday == 0,
			"non-completion: the probes left no tokens in the ledger (%d)", rep.Report.TokensToday)
		c.assert(rep.Report.RequestsThisMinute == 0,
			"non-completion: the probes left no requests in the minute window (%d)", rep.Report.RequestsThisMinute)
	}

	after, ok := adminQ(c, st.url, "", "")
	if c.assert(ok, "non-completion: the stats surface is readable after the probes") {
		c.assert(after.Stats == before.Stats,
			"non-completion: the probes moved no counter at all (before %+v, after %+v)",
			before.Stats, after.Stats)
	}

	// The control: a completion request DOES move the same counters, which is
	// what makes the comparison above meaningful rather than vacuous.
	ctl := chat(c, st.url, chatBody(c, "mock-gpt", "hello"), tenantHdr("acme"))
	c.assert(ctl.status == 200 && actionOf(ctl) == "allow",
		"non-completion: the control completion request is governed (status %d, action %q)",
		ctl.status, actionOf(ctl))
	control, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.Allowed >= before.Stats.Allowed+1 })
	c.assert(ok, "non-completion: the control request moved the allowed counter (%d -> %d)",
		before.Stats.Allowed, control.Stats.Allowed)
}

// ---------------------------------------------------------------------------
// 14. SURFACES
// ---------------------------------------------------------------------------

// checkSurfaces is the shape contract of the three operational surfaces.
//
// Each one is read by different software: /metrics by a scraper, /stats by a
// dashboard or an agent, /admin/quota by a human or a control plane. A field
// that is renamed or dropped breaks a consumer silently - a Grafana panel with
// no data looks like a quiet period, and a quota dashboard that lost
// released_cost_micros reports unspent money as spent. So the assertions here
// are about NAMES and AGREEMENT: every documented key present with the right
// type, and the three surfaces telling the same story about the same traffic.
//
// The tenant is configured on every dimension at once, because a family that is
// only emitted when a dimension is used would otherwise look like a missing
// family.
func checkSurfaces(c *checker, e *environment) {
	body := chatBody(c, "mock-gpt", "hello")
	hold := reservedDefault(body)
	actual := int64(mockSettledTokens)
	actualCost := costMicrosFor(mockPromptTokens, mockCompletionTokens)
	holdCost := costMicrosFor(len(body)/charsPerToken, defaultCompletionReservation)

	q := quotaConfig(tenantPolicy{
		tenant:         "acme",
		tokensPerDay:   1_000_000,
		costPerDayUSD:  100,
		requestsPerMin: 2,
		tokensPerSess:  1_000_000,
	})
	st := e.newStack(c, q)
	if st == nil {
		return
	}
	defer st.close()

	first := chat(c, st.url, body, tenantHdr("acme"))
	second := chat(c, st.url, body, sessionHdr("acme", "s1"))
	third := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(first.status == 200 && second.status == 200,
		"surfaces: two requests are admitted before the minute limit (%d, %d)", first.status, second.status)
	c.assert(third.status == 429,
		"surfaces: the third request trips the two-per-minute limit (status %d)", third.status)
	if _, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.Rejected >= 1 }); !ok {
		c.assert(false, "surfaces: the refusal did not land in the stats in time")
		return
	}

	// --- /admin/quota: the shape, key by key ------------------------------
	raw, ok := adminQuotaRaw(c, st.url)
	if !c.assert(ok, "surfaces: /admin/quota answers with a JSON object") {
		return
	}
	for _, key := range []string{"enabled", "store", "fail_open", "config", "stats"} {
		_, present := raw[key]
		c.assert(present, "surfaces: /admin/quota carries the %q key", key)
	}
	cfgBlock := nestedMap(raw, "config")
	for _, key := range []string{"estimate_completion_tokens", "estimate_chars_per_token", "anomaly_ratio", "default_policy", "tenants"} {
		_, present := cfgBlock[key]
		c.assert(present, "surfaces: /admin/quota config carries %q", key)
	}
	statsBlock := nestedMap(raw, "stats")
	for _, key := range []string{
		"allowed", "degraded", "rejected", "store_errors", "alerts",
		"reserved_tokens", "settled_tokens", "released_tokens",
		"overshoot_tokens", "overshoot_cost_micros", "released_cost_micros",
	} {
		_, present := statsBlock[key]
		c.assert(present, "surfaces: /admin/quota stats carries %q", key)
	}

	doc, ok := adminQ(c, st.url, "", "")
	if c.assert(ok, "surfaces: /admin/quota decodes into the documented shape") {
		c.assert(doc.Enabled, "surfaces: /admin/quota reports enabled=true")
		c.assert(doc.Store == "memory", "surfaces: /admin/quota names the memory store (%q)", doc.Store)
		c.assert(!doc.FailOpen, "surfaces: /admin/quota reports fail_open=false")
		c.assert(doc.Config.EstimateCompletionTokens == 256,
			"surfaces: the default completion estimate is reported (%d)", doc.Config.EstimateCompletionTokens)
		c.assert(doc.Config.EstimateCharsPerToken == 4,
			"surfaces: the default chars-per-token estimate is reported (%d)", doc.Config.EstimateCharsPerToken)
		c.assert(doc.Config.AnomalyRatio == 3,
			"surfaces: the default anomaly ratio is reported (%v)", doc.Config.AnomalyRatio)
		c.assert(doc.Config.DefaultPolicy.OnExceed == "reject",
			"surfaces: the default policy's action is reported (%q)", doc.Config.DefaultPolicy.OnExceed)
		c.assert(doc.Config.DefaultPolicy.Unbounded,
			"surfaces: the shipped default policy is reported as unbounded")
		if c.assert(len(doc.Config.Tenants) == 1, "surfaces: the configured tenant is listed (%d)", len(doc.Config.Tenants)) {
			t := doc.Config.Tenants[0]
			c.assert(t.Tenant == "acme", "surfaces: the listed tenant is named (%q)", t.Tenant)
			c.assert(t.TokensPerDay == 1_000_000, "surfaces: the tenant's token budget is listed (%d)", t.TokensPerDay)
			c.assert(t.CostPerDayUSD == 100, "surfaces: the tenant's spend budget is listed (%v)", t.CostPerDayUSD)
			c.assert(t.RequestsPerMin == 2, "surfaces: the tenant's rate limit is listed (%d)", t.RequestsPerMin)
			c.assert(t.TokensPerSess == 1_000_000, "surfaces: the tenant's session budget is listed (%d)", t.TokensPerSess)
			c.assert(!t.Unbounded, "surfaces: a budgeted tenant is not reported as unbounded")
			c.assert(t.Limits.TokensPerDay && t.Limits.CostPerDay && t.Limits.RequestsPerMin && t.Limits.TokensPerSession,
				"surfaces: every budgeted dimension is flagged in the tenant's limits (%+v)", t.Limits)
		}
		c.assert(doc.Stats.Allowed == 2, "surfaces: two requests were allowed (%d)", doc.Stats.Allowed)
		c.assert(doc.Stats.Rejected == 1, "surfaces: one was refused (%d)", doc.Stats.Rejected)
		c.assert(doc.Stats.Degraded == 0, "surfaces: none was degraded (%d)", doc.Stats.Degraded)
		c.assert(doc.Stats.ReservedTokens == uint64(4*hold),
			"surfaces: reserved counts every budgeted token entry, refused requests included (%d, want %d = 3 admitted-or-refused requests, one of them on two dimensions)",
			doc.Stats.ReservedTokens, 4*hold)
		// Three token entries on the admitted requests, not two requests: the
		// second request carried a session header AND the tenant has a session
		// budget, so it settled two token dimensions where the first settled one.
		c.assert(doc.Stats.SettledTokens == uint64(3*actual),
			"surfaces: settled counts a day entry per request plus a session entry per session request (%d, want %d)",
			doc.Stats.SettledTokens, 3*actual)
		c.assert(doc.Stats.ReleasedTokens == uint64(3*(hold-actual)+hold),
			"surfaces: released follows the same entries, plus the refusal's reservation (%d, want %d)",
			doc.Stats.ReleasedTokens, 3*(hold-actual)+hold)
		assertIdentity(c, "surfaces", doc)
	}

	// --- /admin/quota?tenant=: the report's shape -------------------------
	reportRaw, ok := adminQuotaReportRaw(c, st.url, "acme", "s1")
	if c.assert(ok, "surfaces: /admin/quota?tenant=&session= answers") {
		for _, key := range []string{
			"tenant", "day", "tokens_today", "cost_today_micros", "cost_today_usd",
			"minute", "requests_this_minute", "session", "session_tokens",
			"baseline_tokens", "ratio", "alerting", "policy",
		} {
			_, present := reportRaw[key]
			c.assert(present, "surfaces: the tenant report carries %q", key)
		}
	}

	// --- /stats: the same story, as raw JSON ------------------------------
	block, ok := statsQuotaBlock(c, st.url)
	if c.assert(ok, "surfaces: /stats carries the quota block") {
		for _, key := range []string{
			"enabled", "store", "allowed", "degraded", "rejected", "store_errors", "alerts",
			"reserved_tokens", "settled_tokens", "released_tokens",
			"overshoot_tokens", "overshoot_cost_micros", "released_cost_micros",
		} {
			_, present := block[key]
			c.assert(present, "surfaces: the /stats quota block carries %q", key)
		}
		enabled, hasEnabled := mapBool(block, "enabled")
		c.assert(hasEnabled && enabled, "surfaces: /stats reports governance as enabled (%v)", block["enabled"])
		c.assert(mapInt(block, "allowed") == 2, "surfaces: /stats agrees on allowed (%v)", block["allowed"])
		c.assert(mapInt(block, "rejected") == 1, "surfaces: /stats agrees on rejected (%v)", block["rejected"])
		c.assert(mapInt(block, "reserved_tokens") == 4*hold,
			"surfaces: /stats agrees on reserved (%v, want %d)", block["reserved_tokens"], 4*hold)
		c.assert(mapInt(block, "settled_tokens") == 3*actual,
			"surfaces: /stats agrees on settled (%v, want %d)", block["settled_tokens"], 3*actual)
		c.assert(mapInt(block, "released_tokens") == 3*(hold-actual)+hold,
			"surfaces: /stats agrees on released (%v, want %d)", block["released_tokens"], 3*(hold-actual)+hold)
		c.assert(mapInt(block, "released_cost_micros") == 2*(holdCost-actualCost)+holdCost,
			"surfaces: /stats agrees on released spend (%v, want %d)",
			block["released_cost_micros"], 2*(holdCost-actualCost)+holdCost)
	}

	// --- /metrics: every family, with parseable numbers -------------------
	metrics := get(c, st.url+"/metrics").body
	for _, family := range quotaMetricFamilies {
		c.assert(hasMetricLine(metrics, family),
			"surfaces: /metrics exposes %s with a parseable number", family)
	}
	c.assert(metricValue(metrics, "infergate_quota_decisions_total", `action="allow"`) == 2,
		"surfaces: /metrics agrees on allowed (%v)",
		metricValue(metrics, "infergate_quota_decisions_total", `action="allow"`))
	c.assert(hasSeries(metrics, "infergate_quota_decisions_total", `action="degrade"`),
		"surfaces: /metrics exposes the degrade series even at zero")
	c.assert(metricValue(metrics, "infergate_quota_decisions_total", `action="reject"`) == 1,
		"surfaces: /metrics agrees on rejected (%v)",
		metricValue(metrics, "infergate_quota_decisions_total", `action="reject"`))
	for _, kind := range []string{"reserved", "settled", "released"} {
		c.assert(hasSeries(metrics, "infergate_quota_tokens_total", `kind="`+kind+`"`),
			"surfaces: /metrics exposes tokens{kind=%q}", kind)
	}
	c.assert(metricValue(metrics, "infergate_quota_tokens_total", `kind="reserved"`) == float64(4*hold),
		"surfaces: /metrics agrees on reserved tokens (%v, want %d)",
		metricValue(metrics, "infergate_quota_tokens_total", `kind="reserved"`), 4*hold)
	c.assert(metricValue(metrics, "infergate_quota_tokens_total", `kind="released"`) == float64(3*(hold-actual)+hold),
		"surfaces: /metrics agrees on released tokens (%v, want %d)",
		metricValue(metrics, "infergate_quota_tokens_total", `kind="released"`), 3*(hold-actual)+hold)
	releasedCost, present := scalarMetric(metrics, "infergate_quota_released_cost_micros_total")
	c.assert(present && releasedCost == float64(2*(holdCost-actualCost)+holdCost),
		"surfaces: /metrics agrees on released spend (%v, want %d)", releasedCost, 2*(holdCost-actualCost)+holdCost)
	if storeErrors, present := scalarMetric(metrics, "infergate_quota_store_errors_total"); c.assert(present, "surfaces: /metrics exposes the store-error counter") {
		c.assert(storeErrors == 0, "surfaces: a healthy stack reports no store errors (%v)", storeErrors)
	}
	if alerts, present := scalarMetric(metrics, "infergate_quota_alerts_total"); c.assert(present, "surfaces: /metrics exposes the alert counter") {
		c.assert(alerts == 0, "surfaces: a tenant under its baseline reports no alerts (%v)", alerts)
	}
}

// adminQuotaRaw is the whole /admin/quota document as an untyped map, so a
// missing key is distinguishable from a zero.
func adminQuotaRaw(c *checker, url string) (map[string]any, bool) {
	var doc map[string]any
	if !getJSON(c, url+"/admin/quota", &doc) {
		return nil, false
	}
	return doc, true
}

// adminQuotaReportRaw is the tenant report as an untyped map, for the same
// reason.
func adminQuotaReportRaw(c *checker, url, tenant, session string) (map[string]any, bool) {
	var doc map[string]any
	q := url + "/admin/quota?tenant=" + tenant
	if session != "" {
		q += "&session=" + session
	}
	if !getJSON(c, q, &doc) {
		return nil, false
	}
	rep, ok := doc["report"].(map[string]any)
	return rep, ok
}

func nestedMap(m map[string]any, key string) map[string]any {
	out, _ := m[key].(map[string]any)
	if out == nil {
		return map[string]any{}
	}
	return out
}

// ---------------------------------------------------------------------------
// 15. ANOMALY
// ---------------------------------------------------------------------------

// checkAnomaly is the alerting path, and the assertion that carries the most
// weight is the negative one: an anomaly alert must not refuse anything.
//
// Alerting exists so a human can look at a spike; a gateway that turns its own
// alert into a refusal has invented a budget nobody configured, and it will do
// it at the worst moment - the spike - which is exactly when the tenant needs
// its requests to go through. So the check asserts that the alert fires AND that
// nothing about the traffic changed.
//
// An anomaly is defined against the previous days' traffic, so the baseline has
// to be seeded. The store is the in-process Redis for that reason: the baseline
// is read out of the same keys an earlier day's traffic would have written, and
// seeding them directly is how the check exercises the real comparison instead
// of a mocked one. The tenant's ratio is set to 1, the smallest value the
// configuration accepts, so a single ordinary request is enough of a spike.
func checkAnomaly(c *checker, e *environment) {
	body := chatBody(c, "mock-gpt", "hello")
	actual := int64(mockSettledTokens)
	baseline := int64(2)
	ratio := float64(actual) / float64(baseline)

	q := quotaConfig(tenantPolicy{
		tenant:          "acme",
		tokensPerDay:    1_000_000,
		anomalyRatio:    1,
		hasAnomalyRatio: true,
	})
	st := e.newRedisStack(c, q)
	if st == nil {
		return
	}
	defer st.close()
	if st.redis == nil {
		c.assert(false, "anomaly: the in-process Redis server was not started")
		return
	}

	rr := newRawRedis(c, st.redis.Addr())
	if rr == nil {
		return
	}
	defer rr.close()

	now := time.Now()
	for i := 1; i <= 3; i++ {
		// Previous UTC days, for the same reason as above: the baseline reads the
		// buckets the gateway would have written, and those are named for the UTC
		// date (internal/quota formats m.now().UTC()).
		key := "ig:quota:acme:day:" + dayBucket(now.AddDate(0, 0, -i)) + ":tokens"
		c.assert(rr.set(key, strconv.FormatInt(baseline, 10)),
			"anomaly: the previous day's traffic is seeded at %s", key)
	}

	first := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(first.status == 200 && actionOf(first) == "allow",
		"anomaly: the spiking request is admitted, not refused (status %d, action %q)", first.status, actionOf(first))

	rep, ok := awaitReport(c, st.url, "acme", "", func(r quotaReport) bool { return r.Alerting })
	if c.assert(ok, "anomaly: the tenant report raises the alert (alerting=%v, ratio=%v)", rep.Alerting, rep.Ratio) {
		c.assert(rep.BaselineTokens == float64(baseline),
			"anomaly: the baseline is the mean of the seeded days (%v, want %d)", rep.BaselineTokens, baseline)
		c.assert(rep.Ratio == ratio,
			"anomaly: the ratio is today's usage over the baseline (%v, want %v)", rep.Ratio, ratio)
		c.assert(rep.Alerting, "anomaly: alerting is true once the ratio crosses the tenant's threshold")
	}

	doc, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.Alerts >= 1 })
	if c.assert(ok, "anomaly: the alert is counted (%d)", doc.Stats.Alerts) {
		c.assert(doc.Stats.Rejected == 0,
			"anomaly: NOTHING was refused because of the alert (%d)", doc.Stats.Rejected)
		c.assert(doc.Stats.Allowed >= 1,
			"anomaly: the request was allowed (%d)", doc.Stats.Allowed)
	}
	alerts, present := awaitScalarMetric(c, st.url, "infergate_quota_alerts_total", 1)
	c.assert(present && alerts >= 1,
		"anomaly: /metrics exposes the alert (%v, present=%v)", alerts, present)

	second := chat(c, st.url, body, tenantHdr("acme"))
	c.assert(second.status == 200,
		"anomaly: traffic keeps flowing after the alert (status %d)", second.status)
	after, ok := awaitStats(c, st.url, func(d adminQuotaDoc) bool { return d.Stats.Allowed >= 2 })
	if c.assert(ok, "anomaly: the second request was allowed too (%d)", after.Stats.Allowed) {
		c.assert(after.Stats.Rejected == 0,
			"anomaly: still nothing refused (%d)", after.Stats.Rejected)
		c.assert(after.Stats.Degraded == 0,
			"anomaly: and nothing silently degraded (%d)", after.Stats.Degraded)
	}
	c.assert(st.backend.Count() == 2,
		"anomaly: both requests reached the upstream (%d calls)", st.backend.Count())
}

// ---------------------------------------------------------------------------
// 16. CONFIG VALIDATION
// ---------------------------------------------------------------------------

// checkConfigValidation is the contract between a configuration file and the
// gateway.
//
// Every bad case below is a mistake a human makes in a YAML file, and the
// requirement is not merely that it is rejected but that the message says which
// key was wrong. A quota section that fails with "invalid configuration" is a
// section an operator will "fix" by deleting it, and a gateway that silently
// normalises an impossible value - a zero rate limit, a negative budget - is
// worse still: it starts up looking healthy and governs nothing.
//
// The messages are asserted as substrings, so wording can be improved without
// rewriting the gate, but the name of the offending key is pinned. The two
// shipped sample files are loaded as well: documentation that does not parse is
// the most common way a feature is first broken.
func checkConfigValidation(c *checker, e *environment) {
	// Defaults() carries no upstream, and Validate() checks that before it
	// reaches the quota section, so every case starts from a config that has
	// one. Without this the check would pass for the wrong reason.
	build := func(mutate func(*config.QuotaConfig)) *config.Config {
		q := config.Defaults().Quota
		q.Enabled = true
		mutate(&q)
		cfg := baseConfig("http://127.0.0.1:1")
		cfg.Quota = q
		return &cfg
	}

	cases := []struct {
		name string
		want string
		make func(*config.QuotaConfig)
	}{
		{"an unsupported store", "unsupported store",
			func(q *config.QuotaConfig) { q.Store = "nope" }},
		{"a negative completion estimate", "quota.estimate_completion_tokens: must not be negative",
			func(q *config.QuotaConfig) { q.EstimateCompletionTokens = -1 }},
		{"a negative chars-per-token estimate", "quota.estimate_chars_per_token: must not be negative",
			func(q *config.QuotaConfig) { q.EstimateCharsPerToken = -1 }},
		{"a negative anomaly ratio", "quota.anomaly_ratio: must not be negative",
			func(q *config.QuotaConfig) { q.AnomalyRatio = -1 }},
		{"an anomaly ratio below one", "quota.anomaly_ratio: must be at least 1",
			func(q *config.QuotaConfig) { q.AnomalyRatio = 0.5 }},
		{"a redis prefix with whitespace", "quota.redis.prefix: must not contain whitespace",
			func(q *config.QuotaConfig) { q.Redis.Prefix = "ig quota" }},
		{"a redis prefix with a trailing separator", "quota.redis.prefix: must not end with",
			func(q *config.QuotaConfig) { q.Redis.Prefix = "ig:quota:" }},
		{"a negative redis pool size", "quota.redis.pool_size: must not be negative",
			func(q *config.QuotaConfig) { q.Redis.PoolSize = -1 }},
		{"a redis store with no address", "quota.redis.addr: required when store is redis and quota is enabled",
			func(q *config.QuotaConfig) { q.Store = config.QuotaStoreRedis; q.Redis.Addr = "" }},
		{"a tenant with no name", "quota.tenants[0]: tenant is required",
			func(q *config.QuotaConfig) { q.Tenants = []config.TenantQuotaConfig{{TokensPerDay: 10}} }},
		{"a duplicated tenant", `quota.tenants: duplicate tenant "acme"`,
			func(q *config.QuotaConfig) {
				q.Tenants = []config.TenantQuotaConfig{{Tenant: "acme", TokensPerDay: 10}, {Tenant: "acme", TokensPerDay: 20}}
			}},
		{"a negative daily token budget", "tokens_per_day: must not be negative",
			func(q *config.QuotaConfig) {
				q.Tenants = []config.TenantQuotaConfig{{Tenant: "acme", TokensPerDay: -1}}
			}},
		{"a negative daily spend budget", "cost_per_day_usd: must not be negative",
			func(q *config.QuotaConfig) {
				q.Tenants = []config.TenantQuotaConfig{{Tenant: "acme", CostPerDayUSD: -1}}
			}},
		{"a negative rate limit", "requests_per_minute: must not be negative",
			func(q *config.QuotaConfig) {
				q.Tenants = []config.TenantQuotaConfig{{Tenant: "acme", RequestsPerMinute: -1}}
			}},
		{"a negative session budget", "tokens_per_session: must not be negative",
			func(q *config.QuotaConfig) {
				q.Tenants = []config.TenantQuotaConfig{{Tenant: "acme", TokensPerSession: -1}}
			}},
		{"a negative completion cap", "max_tokens_cap: must not be negative",
			func(q *config.QuotaConfig) {
				q.Tenants = []config.TenantQuotaConfig{{Tenant: "acme", MaxTokensCap: -1}}
			}},
		{"an unsupported on_exceed action", "on_exceed: unsupported action",
			func(q *config.QuotaConfig) {
				q.Tenants = []config.TenantQuotaConfig{{Tenant: "acme", TokensPerDay: 10, OnExceed: "explode"}}
			}},
		{"degrade with nothing to degrade to", "on_exceed is degrade but neither",
			func(q *config.QuotaConfig) {
				q.Tenants = []config.TenantQuotaConfig{{Tenant: "acme", TokensPerDay: 10, OnExceed: "degrade"}}
			}},
		{"a tenant anomaly ratio below one", "anomaly_ratio: must be at least 1",
			func(q *config.QuotaConfig) {
				q.Tenants = []config.TenantQuotaConfig{{Tenant: "acme", TokensPerDay: 10, AnomalyRatio: 0.5}}
			}},
	}

	for _, tc := range cases {
		cfg := build(tc.make)
		err := cfg.Validate()
		if !c.assert(err != nil, "config: %s is rejected", tc.name) {
			continue
		}
		c.assert(strings.Contains(err.Error(), tc.want),
			"config: %s is rejected with a message naming the key (got %q, want %q)", tc.name, err.Error(), tc.want)
	}

	// The positive control: a section that is fine must validate, or every
	// assertion above could be passing on a Validate() that rejects everything.
	good := build(func(q *config.QuotaConfig) {
		q.Store = config.QuotaStoreRedis
		q.Redis.Addr = "127.0.0.1:6379"
		q.Tenants = []config.TenantQuotaConfig{
			{Tenant: "acme", TokensPerDay: 10, OnExceed: config.QuotaActionReject},
			{Tenant: "globex", RequestsPerMinute: 5, OnExceed: config.QuotaActionDegrade, DowngradeModel: "small"},
		}
	})
	c.assert(good.Validate() == nil, "config: a well-formed quota section validates (%v)", good.Validate())

	root := repoRoot(c)
	samples := []struct {
		file  string
		store string
	}{
		{"configs/quota-local.yaml", config.QuotaStoreMemory},
		{"configs/quota-redis.yaml", config.QuotaStoreRedis},
	}
	for _, s := range samples {
		path := filepath.Join(root, "configs", filepath.Base(s.file))
		cfg, err := config.Load(path)
		if !c.assert(err == nil, "config: the shipped %s loads (%v)", s.file, err) {
			continue
		}
		c.assert(cfg.Quota.Enabled, "config: %s ships with governance enabled", s.file)
		c.assert(cfg.Quota.Store == s.store,
			"config: %s uses the %s store (%q)", s.file, s.store, cfg.Quota.Store)
		c.assert(len(cfg.Quota.Tenants) > 0,
			"config: %s configures at least one tenant (%d)", s.file, len(cfg.Quota.Tenants))
		named := true
		for _, t := range cfg.Quota.Tenants {
			if strings.TrimSpace(t.Tenant) == "" {
				named = false
			}
			if t.OnExceed != config.QuotaActionReject && t.OnExceed != config.QuotaActionDegrade {
				named = false
			}
		}
		c.assert(named,
			"config: %s gives every tenant a name and a valid on_exceed action", s.file)
		if s.store == config.QuotaStoreRedis {
			c.assert(cfg.Quota.Redis.Addr != "",
				"config: %s names the redis address it expects (%q)", s.file, cfg.Quota.Redis.Addr)
			c.assert(cfg.Quota.Redis.Prefix != "",
				"config: %s names the redis key prefix it expects (%q)", s.file, cfg.Quota.Redis.Prefix)
		}
	}

	local, err := config.Load(filepath.Join(root, "configs", "quota-local.yaml"))
	if c.assert(err == nil, "config: the local sample loads for the per-tenant assertions (%v)", err) {
		c.assert(len(local.Quota.Tenants) == 4,
			"config: the local sample documents four tenants (%d)", len(local.Quota.Tenants))
		names := map[string]bool{}
		for _, t := range local.Quota.Tenants {
			names[t.Tenant] = true
		}
		c.assert(names["acme"], "config: the local sample configures acme")
		c.assert(local.Quota.DefaultPolicy.TokensPerDay > 0,
			"config: the local sample sets a default daily token budget (%d)",
			local.Quota.DefaultPolicy.TokensPerDay)
	}
}
