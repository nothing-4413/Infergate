package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/mockbackend"
)

// The cost-strategy fleet serves one logical model under two concrete names, so
// the two backends can carry two different prices. That is how a real fleet
// looks: "the cheap way to answer" and "the good way to answer" are different
// upstream models, not one model sold at two prices.
const (
	cheapModel = "mock-cheap"
	dearModel  = "mock-dear"
)

// routerBody asks for a model no backend declares by that name, which makes the
// fleet resolve it through model patterns rather than an exact match.
var routerBody = `{"model":"mock-router","messages":[{"role":"user","content":"hi"}]}`


// checkPriorityAndFailover: with priority ordering the FIRST eligible backend is
// tried, and a 5xx takes the gateway to the next candidate instead of surfacing
// the failure -- unless the failing backend is the only one, in which case the
// provider's own status must reach the client.
func checkPriorityAndFailover(c *checker, e *environment) {
	primary := mockbackend.New(mockbackend.Options{Name: "primary"})
	defer primary.Close()
	backup := mockbackend.New(mockbackend.Options{Name: "backup"})
	defer backup.Close()

	cfg := baseConfig([]fleetEntry{
		{name: "primary", back: primary, priority: 1, models: []string{"mock-gpt"}},
		{name: "backup", back: backup, priority: 2, models: []string{"mock-gpt"}},
	})
	cfg.Routing.Strategy = config.StrategyPriority
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	// The healthy path: lowest priority number wins and the backup is untouched.
	res := post(c, st.url, chatBody, nil, false)
	c.assert(res.status == 200, "healthy request answers 200 (got %d, body=%s)", res.status, truncate(res.body, 160))
	c.assert(res.header.Get("X-InferGate-Upstream-Name") == "primary",
		"healthy request goes to the highest-priority backend (X-InferGate-Upstream-Name=%q)",
		res.header.Get("X-InferGate-Upstream-Name"))
	c.assert(primary.Count() == 1 && backup.Count() == 0,
		"exactly one backend was contacted (primary=%d backup=%d)", primary.Count(), backup.Count())

	// The failure path: a 503 from the first candidate is retried on the second.
	//
	// The failure is injected on the BACKEND (SetFailStatus), not per request via
	// X-Mock-Status. That distinction is the whole point of this check: a
	// per-request header is forwarded by the gateway to every candidate, so the
	// backup would fail with the same 503 and the "failover" would look broken
	// while actually working. A permanently broken primary is also the realistic
	// case -- real providers do not fail once and then recover on demand.
	primary.Reset()
	backup.Reset()
	primary.SetFailStatus(http.StatusServiceUnavailable)
	res = post(c, st.url, chatBody, nil, false)
	c.assert(res.status == 200, "a 503 from the preferred backend is hidden by failover (got %d, body=%s)",
		res.status, truncate(res.body, 160))
	c.assert(res.header.Get("X-InferGate-Upstream-Name") == "backup",
		"the retry landed on the backup (X-InferGate-Upstream-Name=%q)", res.header.Get("X-InferGate-Upstream-Name"))
	c.assert(res.header.Get("X-InferGate-Tried") == "primary, backup",
		"the response names every backend that was tried in order (%s=%q)",
		"X-InferGate-Tried", res.header.Get("X-InferGate-Tried"))
	c.assert(res.header.Get("X-InferGate-Attempt") == "2",
		"the attempt counter reports 2 (%s=%q)", "X-InferGate-Attempt", res.header.Get("X-InferGate-Attempt"))
	c.assert(primary.Count() == 1 && backup.Count() == 1,
		"each backend saw exactly one request (primary=%d backup=%d)", primary.Count(), backup.Count())
	primary.SetFailStatus(0)

	// Every candidate fails: the client must receive the PROVIDER's status, not
	// a generic gateway error, because an SDK's retry policy keys off it.
	//
	// This runs on a FRESH stack with BOTH backends permanently broken. Reusing
	// the stack above would instead exercise the breaker: the successful failover
	// already proved the mechanism, and two more failed attempts would trip both
	// breakers, at which point the correct answer is a gateway 502 ("no upstream
	// could serve this request") -- not the provider envelope this check is
	// about. Mixing the two premises is how a passing check gets misread as a
	// product bug.
	brokenA := mockbackend.New(mockbackend.Options{Name: "primary", FailStatus: http.StatusBadGateway})
	defer brokenA.Close()
	brokenB := mockbackend.New(mockbackend.Options{Name: "backup", FailStatus: http.StatusServiceUnavailable})
	defer brokenB.Close()
	downCfg := baseConfig([]fleetEntry{
		{name: "primary", back: brokenA, priority: 1},
		{name: "backup", back: brokenB, priority: 2},
	})
	downSt := e.newStack(c, downCfg)
	if downSt == nil {
		return
	}
	defer downSt.close()

	res = post(c, downSt.url, chatBody, nil, false)
	c.assert(res.status == http.StatusServiceUnavailable,
		"when every candidate fails the LAST provider status is forwarded (got %d)", res.status)
	c.assert(strings.Contains(res.body, "is permanently failing with 503"),
		"the provider's own error envelope is forwarded verbatim (body=%s)", truncate(res.body, 160))
	c.assert(strings.Contains(res.header.Get("X-InferGate-Tried"), "primary"),
		"the tried list still records the attempts (%s=%q)",
		"X-InferGate-Tried", res.header.Get("X-InferGate-Tried"))
}

// ---------------------------------------------------------------------------
// 2. No failover on a caller error
// ---------------------------------------------------------------------------

// checkNoFailoverOn4xx: a 4xx is a verdict about the REQUEST. Every other
// backend would reject it identically, so retrying it burns an attempt per
// request, spends provider quota and hides the caller's own bug behind a generic
// gateway error.
func checkNoFailoverOn4xx(c *checker, e *environment) {
	primary := mockbackend.New(mockbackend.Options{Name: "primary"})
	defer primary.Close()
	backup := mockbackend.New(mockbackend.Options{Name: "backup"})
	defer backup.Close()

	cfg := baseConfig([]fleetEntry{
		{name: "primary", back: primary, priority: 1},
		{name: "backup", back: backup, priority: 2},
	})
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	res := post(c, st.url, chatBody, map[string]string{"X-Mock-Status": "400"}, false)
	c.assert(res.status == 400, "a 400 from the first backend is returned as 400 (got %d)", res.status)
	c.assert(backup.Count() == 0, "the 4xx did not reach the second backend (backup=%d)", backup.Count())
	c.assert(primary.Count() == 1, "the first backend saw exactly one request (primary=%d)", primary.Count())
	c.assert(res.header.Get("X-InferGate-Tried") == "",
		"no failover marker is set when nothing failed over (%s=%q)",
		"X-InferGate-Tried", res.header.Get("X-InferGate-Tried"))

	// 429 is the exception: it is a verdict about the MOMENT, so it is retried.
	// Injected on the backend so the backup is genuinely healthy -- a
	// per-request header would ride along to the retry and fail it too.
	primary.Reset()
	backup.Reset()
	primary.SetFailStatus(http.StatusTooManyRequests)
	res = post(c, st.url, chatBody, nil, false)
	c.assert(res.status == 200 && backup.Count() == 1,
		"429 is retried on the next backend (status=%d backup=%d)", res.status, backup.Count())
	primary.SetFailStatus(0)
}

// ---------------------------------------------------------------------------
// 3. Timeout fails over instead of answering 504
// ---------------------------------------------------------------------------

// checkTimeoutFailover: this is the case a second backend exists for. The
// preferred backend is slow (not broken), so the gateway's deadline fires while
// the client is still waiting; the client must end up with the fast backend's
// answer, and must never see a 504 that a later attempt could have prevented.
func checkTimeoutFailover(c *checker, e *environment) {
	slow := mockbackend.New(mockbackend.Options{Name: "slow"})
	defer slow.Close()
	fast := mockbackend.New(mockbackend.Options{Name: "fast"})
	defer fast.Close()

	cfg := baseConfig([]fleetEntry{
		{name: "slow", back: slow, priority: 1},
		{name: "fast", back: fast, priority: 2},
	})
	// The stall is set on the BACKEND, not broadcast as a request header. A
	// per-request header is forwarded to every candidate, so the fast backend
	// would stall for the same 1.2s and the failover target would be as slow as
	// the thing it is supposed to rescue.
	slow.SetStall(e.slowTTFB)
	defer slow.SetStall(0)

	// The gateway's per-attempt budget is a third of the stall, so the slow
	// candidate is abandoned well before it answers.
	cfg.Server.UpstreamTimeout = config.Duration(e.slowTTFB / 3)
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	// The slow backend stalls and is abandoned; the fast one answers. The client
	// must never see the 504 the first attempt produced.
	res := post(c, st.url, chatBody, nil, false)
	c.assert(res.status == 200, "a slow preferred backend is failed over, not surfaced (got %d, body=%s)",
		res.status, truncate(res.body, 200))
	c.assert(res.header.Get("X-InferGate-Tried") == "slow, fast",
		"the slow backend was tried first, then abandoned (%s=%q)",
		"X-InferGate-Tried", res.header.Get("X-InferGate-Tried"))
	c.assert(strings.Contains(res.body, "fast"), "the answer came from the fast backend (body=%s)", truncate(res.body, 120))
	e.infof(c, "per-attempt budget %.0fms, client total %.0fms",
		float64(cfg.Server.UpstreamTimeout)/float64(time.Millisecond), float64(res.elapsed)/float64(time.Millisecond))

	// A streaming request must fail over too: the SSE path is inside the attempt
	// loop, not a parallel implementation with its own error handling.
	res = post(c, st.url, `{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		nil, true)
	c.assert(res.status == 200, "a slow preferred backend is failed over for a stream too (got %d)", res.status)
	c.assert(strings.HasSuffix(strings.TrimRight(res.body, "\r\n"), "data: [DONE]"),
		"the streamed failover still ends with the sentinel (tail=%q)", truncate(tailOf(res.body), 60))
}

// ---------------------------------------------------------------------------
// 4. An exhausted fleet answers 504, not a hang
// ---------------------------------------------------------------------------

// checkExhaustedFleet504: when every candidate is slow the client must be told.
// A gateway that keeps retrying past its own deadline leaves an SDK waiting on a
// 200 with no body, which is the worst possible failure mode because it looks
// like success.
func checkExhaustedFleet504(c *checker, e *environment) {
	slowOne := mockbackend.New(mockbackend.Options{Name: "slow-one"})
	defer slowOne.Close()
	slowTwo := mockbackend.New(mockbackend.Options{Name: "slow-two"})
	defer slowTwo.Close()

	cfg := baseConfig([]fleetEntry{
		{name: "slow-one", back: slowOne, priority: 1},
		{name: "slow-two", back: slowTwo, priority: 2},
	})
	slowOne.SetStall(e.slowTTFB)
	slowTwo.SetStall(e.slowTTFB)
	defer slowOne.SetStall(0)
	defer slowTwo.SetStall(0)

	cfg.Server.UpstreamTimeout = config.Duration(e.slowTTFB / 3)
	// Two attempts, so BOTH candidates are given their own budget and the request
	// as a whole spends two of them. With max_attempts=1 the check would only
	// prove that a single slow backend times out -- which is the boring half --
	// and would never exercise the exhausted-fleet accounting that has to survive
	// failover.
	cfg.Health.MaxFailuresPerRequest = 2
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	res := post(c, st.url, chatBody, nil, false)
	c.assert(res.status == 504, "an unanswerable fleet answers 504 rather than hanging (got %d)", res.status)
	c.assert(strings.Contains(res.body, "infergate_upstream_timeout"),
		"the 504 uses the gateway's timeout error type (body=%s)", truncate(res.body, 200))
	c.assert(res.elapsed < 3*time.Second,
		"the deadline is enforced instead of waiting for the backend (%.0fms)",
		float64(res.elapsed)/float64(time.Millisecond))
	c.assert(res.body != "", "the 504 carries a body an SDK can parse")

	// /stats must classify it as a timeout, not as an upstream error: "the
	// backend said no" and "the backend never spoke" need opposite responses.
	var stats statsDoc
	if getJSON(c, st.url+"/stats", &stats) {
		found := false
		for _, row := range stats.Series {
			if row.Outcome == "timeout" {
				found = true
			}
		}
		c.assert(found, "the request is classified as outcome=timeout in /stats (series=%s)", truncate(fmt.Sprint(stats.Series), 300))
	}
}

// ---------------------------------------------------------------------------
// 5. max_attempts caps the budget
// ---------------------------------------------------------------------------

// checkMaxAttemptsBudget: the failover chain is bounded, so a fleet of N broken
// backends costs at most max_attempts upstream calls per client request. Without
// the cap, one unhealthy fleet multiplies load on the healthy remainder exactly
// when it can least afford it.
func checkMaxAttemptsBudget(c *checker, e *environment) {
	one := mockbackend.New(mockbackend.Options{Name: "one"})
	defer one.Close()
	two := mockbackend.New(mockbackend.Options{Name: "two"})
	defer two.Close()
	three := mockbackend.New(mockbackend.Options{Name: "three"})
	defer three.Close()

	cfg := baseConfig([]fleetEntry{
		{name: "one", back: one, priority: 1},
		{name: "two", back: two, priority: 2},
		{name: "three", back: three, priority: 3},
	})
	cfg.Health.MaxFailuresPerRequest = 2
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	res := post(c, st.url, chatBody, map[string]string{"X-Mock-Status": "503"}, false)
	total := one.Count() + two.Count() + three.Count()
	c.assert(total == 2, "max_attempts=2 stops after two backends (attempts=%d one=%d two=%d three=%d)",
		total, one.Count(), two.Count(), three.Count())
	c.assert(three.Count() == 0, "the third backend was never contacted (three=%d)", three.Count())
	c.assert(res.status == 503, "the client still receives a real status (got %d)", res.status)
	c.assert(res.header.Get("X-InferGate-Tried") == "one, two",
		"the tried list names exactly the attempted backends (%q)", res.header.Get("X-InferGate-Tried"))
}

// ---------------------------------------------------------------------------
// 6. Capability routing
// ---------------------------------------------------------------------------

// checkCapabilityRouting: a request that declares a required capability must go
// to a backend that has it, and must FAIL rather than silently downgrade when
// none does. A silent downgrade is the expensive kind of bug: the answer looks
// fine and only the caller knows a feature was missing.
func checkCapabilityRouting(c *checker, e *environment) {
	plain := mockbackend.New(mockbackend.Options{Name: "plain"})
	defer plain.Close()
	tools := mockbackend.New(mockbackend.Options{Name: "tools"})
	defer tools.Close()

	cfg := baseConfig([]fleetEntry{
		{name: "plain", back: plain, priority: 1},
		{name: "tools", back: tools, priority: 2, capabilities: []string{"tools"}},
	})
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	res := post(c, st.url, chatBody, nil, false)
	c.assert(res.header.Get("X-InferGate-Upstream-Name") == "plain",
		"without a capability requirement priority wins (got %q)", res.header.Get("X-InferGate-Upstream-Name"))

	plain.Reset()
	tools.Reset()
	res = post(c, st.url, chatBody, map[string]string{"X-InferGate-Capabilities": "tools"}, false)
	c.assert(res.status == 200 && res.header.Get("X-InferGate-Upstream-Name") == "tools",
		"a required capability selects the backend that has it (status=%d upstream=%q)",
		res.status, res.header.Get("X-InferGate-Upstream-Name"))
	c.assert(plain.Count() == 0, "the backend without the capability was never contacted (plain=%d)", plain.Count())

	plain.Reset()
	tools.Reset()
	res = post(c, st.url, chatBody, map[string]string{"X-InferGate-Capabilities": "vision"}, false)
	c.assert(res.status == 400, "an unservable capability fails loudly with 400 (got %d)", res.status)
	c.assert(plain.Count()+tools.Count() == 0,
		"an unservable capability contacts no backend at all (plain=%d tools=%d)", plain.Count(), tools.Count())
}

// ---------------------------------------------------------------------------
// 7. Explicit pin
// ---------------------------------------------------------------------------

// checkExplicitPin: an operator debugging one backend needs to reach it even
// when the router would prefer another, and the pin must be exclusive -- if the
// router still considered alternatives, the traffic being investigated would
// silently land elsewhere.
func checkExplicitPin(c *checker, e *environment) {
	preferred := mockbackend.New(mockbackend.Options{Name: "preferred"})
	defer preferred.Close()
	pinned := mockbackend.New(mockbackend.Options{Name: "pinned"})
	defer pinned.Close()

	cfg := baseConfig([]fleetEntry{
		{name: "preferred", back: preferred, priority: 1},
		{name: "pinned", back: pinned, priority: 9},
	})
	// With the pinned backend out of the model map, only the pin can route to it.
	cfg.Upstreams[1].Models = []string{"mock-gpt"}
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	res := post(c, st.url, chatBody, map[string]string{"X-InferGate-Upstream": "pinned"}, false)
	c.assert(res.status == 200, "a pinned upstream serves the request (got %d)", res.status)
	c.assert(res.header.Get("X-InferGate-Upstream-Name") == "pinned",
		"the pin overrides priority (%q)", res.header.Get("X-InferGate-Upstream-Name"))
	c.assert(preferred.Count() == 0, "the preferred backend was bypassed entirely (preferred=%d)", preferred.Count())

	res = post(c, st.url, chatBody, map[string]string{"X-InferGate-Upstream": "nope"}, false)
	c.assert(res.status == 400, "an unknown pinned upstream is a 400, not a silent fallback (got %d)", res.status)
}

// ---------------------------------------------------------------------------
// 8. Cost strategy
// ---------------------------------------------------------------------------

// checkCostStrategy: with strategy=cost the cheap backend must win even though
// the other one has better priority, and the comparison must use the SAME price
// scale as the request's own accounting -- a router that prices differently from
// the cost report sends traffic to the backend the report calls priciest.
func checkCostStrategy(c *checker, e *environment) {
	dear := mockbackend.New(mockbackend.Options{Name: "dear"})
	defer dear.Close()
	cheap := mockbackend.New(mockbackend.Options{Name: "cheap"})
	defer cheap.Close()

	cfg := baseConfig([]fleetEntry{
		// Both are catch-all, and their price rows describe the ALIAS they would
		// rewrite the request to (see baseConfig). This is the only shape in
		// which two backends can answer the same request at two different
		// prices: a backend that declares the requested model with an exact
		// match is the only eligible candidate for it, so there is nothing to
		// order. The router's own unit tests cover the same mechanism directly.
		{name: "dear", back: dear, priority: 1, models: []string{"/", dearModel}, priceIn: 10, priceOut: 30},
		{name: "cheap", back: cheap, priority: 2, models: []string{"/", cheapModel}, priceIn: 1, priceOut: 3},
	})
	cfg.Routing.Strategy = config.StrategyCost
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	// "mock-router" is priced, so both candidates fall back to the REQUESTED
	// model's price and the only difference is the alias each one declares. The
	// cheap backend must win even though priority says otherwise.
	res := post(c, st.url, routerBody, nil, false)
	c.assert(res.header.Get("X-InferGate-Upstream-Name") == "cheap",
		"strategy=cost prefers the cheaper backend over the better priority (%q)",
		res.header.Get("X-InferGate-Upstream-Name"))
	c.assert(dear.Count() == 0, "the dear backend was not contacted (dear=%d)", dear.Count())
	c.assert(strings.Contains(res.body, cheapModel),
		"the upstream receives the model name IT serves, not the logical one (%s)",
		truncate(res.body, 200))

	// The cheap backend now fails for real. The re-planned chain must still be
	// cost-ordered rather than falling back to configuration order -- with the
	// dear backend first, a request would have to fail twice before reaching the
	// cheap one.
	cheap.SetFailStatus(http.StatusServiceUnavailable)
	res = post(c, st.url, routerBody, nil, false)
	c.assert(res.status == 200 && res.header.Get("X-InferGate-Upstream-Name") == "dear",
		"the cost order also drives failover (status=%d upstream=%q)",
		res.status, res.header.Get("X-InferGate-Upstream-Name"))
	c.assert(strings.Contains(res.body, dearModel),
		"the fallback receives its own model name too (%s)", truncate(res.body, 200))
	cheap.SetFailStatus(0)
}

// ---------------------------------------------------------------------------
// 9. Latency strategy
// ---------------------------------------------------------------------------

// checkLatencyStrategy: with strategy=latency the ordering must follow measured
// latency, not configuration. The windows are fed by real traffic first, so the
// routing decision is based on evidence the gateway collected itself.
func checkLatencyStrategy(c *checker, e *environment) {
	slow := mockbackend.New(mockbackend.Options{Name: "slow", TTFB: 60 * time.Millisecond})
	defer slow.Close()
	fast := mockbackend.New(mockbackend.Options{Name: "fast"})
	defer fast.Close()

	cfg := baseConfig([]fleetEntry{
		{name: "slow", back: slow, priority: 1, models: []string{"mock-gpt"}},
		{name: "fast", back: fast, priority: 2, models: []string{"mock-gpt"}},
	})
	cfg.Routing.Strategy = config.StrategyLatency
	cfg.Health.MinRequests = 1
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	// Pin each backend once so both windows have a measurement; the pins are
	// exclusive, so this is the only way to teach the router about both without
	// the strategy itself deciding who gets measured.
	post(c, st.url, chatBody, map[string]string{"X-InferGate-Upstream": "slow"}, false)
	post(c, st.url, chatBody, map[string]string{"X-InferGate-Upstream": "fast"}, false)

	slowSnap := st.breakers.Stats("slow").Snapshot()
	fastSnap := st.breakers.Stats("fast").Snapshot()
	c.assert(slowSnap.HasLatency && fastSnap.HasLatency,
		"both backends have a measured window (slow=%v fast=%v)", slowSnap.HasLatency, fastSnap.HasLatency)
	c.assert(slowSnap.LatencyMean > fastSnap.LatencyMean,
		"the delayed backend measures slower (slow=%v fast=%v)", slowSnap.LatencyMean, fastSnap.LatencyMean)

	slow.Reset()
	fast.Reset()
	for i := 0; i < 3; i++ {
		post(c, st.url, chatBody, nil, false)
	}
	c.assert(fast.Count() >= 2, "strategy=latency prefers the measured-faster backend (fast=%d slow=%d)",
		fast.Count(), slow.Count())
}

// ---------------------------------------------------------------------------
// 10. Breaker lifecycle
// ---------------------------------------------------------------------------

// checkBreakerLifecycle: repeated failures must take a backend out of rotation
// (so a dead replica stops consuming attempts), traffic must keep flowing to the
// healthy one, and the tripped backend must be allowed back through a single
// probe after its cooldown. A breaker that never closes is an outage the
// operator has to fix by hand.
func checkBreakerLifecycle(c *checker, e *environment) {
	dead := mockbackend.New(mockbackend.Options{Name: "dead", FailStatus: 503})
	defer dead.Close()
	alive := mockbackend.New(mockbackend.Options{Name: "alive"})
	defer alive.Close()

	cfg := baseConfig([]fleetEntry{
		{name: "dead", back: dead, priority: 1},
		{name: "alive", back: alive, priority: 2},
	})
	cfg.Health.MinRequests = 3
	cfg.Health.FailureRatio = 0.5
	cfg.Health.OpenDuration = config.Duration(400 * time.Millisecond)
	cfg.Health.HalfOpenProbes = 1
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	// Two requests fail over (2 failed attempts observed), the third trips the
	// breaker and must be served WITHOUT the dead backend being contacted at all.
	for i := 0; i < 3; i++ {
		post(c, st.url, chatBody, nil, false)
	}
	deadAfterTrip := dead.Count()
	c.assert(deadAfterTrip == 3, "the dead backend was contacted before tripping (attempts=%d)", deadAfterTrip)

	res := post(c, st.url, chatBody, nil, false)
	c.assert(res.status == 200, "traffic still succeeds once the dead backend is tripped (got %d)", res.status)
	c.assert(dead.Count() == deadAfterTrip,
		"a tripped backend is skipped entirely: no attempt was spent on it (%d -> %d)", deadAfterTrip, dead.Count())
	c.assert(st.breakers.Get("dead").State() == breaker.StateOpen,
		"the breaker reports open after the failure ratio crossed the threshold (state=%s)", st.breakers.Get("dead").State())

	// Half-open: after the cooldown exactly one probe is admitted, and a FAILED
	// probe must re-open immediately rather than pretending the backend is
	// usable.
	time.Sleep(500 * time.Millisecond)
	post(c, st.url, chatBody, map[string]string{"X-InferGate-Upstream": "dead"}, false)
	probes := dead.Count() - deadAfterTrip
	c.assert(probes == 1, "exactly one half-open probe was admitted after the cooldown (probes=%d)", probes)
	c.assert(alive.Count() >= 4,
		"the healthy backend kept serving while the dead one was out (alive=%d)", alive.Count())
	c.assert(st.breakers.Get("dead").State() != breaker.StateClosed,
		"a failed probe leaves the breaker out of rotation (state=%s)", st.breakers.Get("dead").State())

	// Recovery: repairing the backend and letting the next probe succeed must
	// close the breaker and return the backend to the fleet. A breaker that
	// never closes is a manual outage the operator has to fix by hand.
	dead.SetFailStatus(0)
	time.Sleep(500 * time.Millisecond)
	post(c, st.url, chatBody, map[string]string{"X-InferGate-Upstream": "dead"}, false)
	c.assert(st.breakers.Get("dead").State() == breaker.StateClosed,
		"a successful probe closes the breaker and the backend returns to the fleet (state=%s)",
		st.breakers.Get("dead").State())
	e.infof(c, "dead backend: %d pre-trip attempts, 1 half-open probe, recovered", deadAfterTrip)
}

// ---------------------------------------------------------------------------
// 11. Observability surfaces
// ---------------------------------------------------------------------------

// checkObservability: routing decisions must be visible to an operator. The
// admin endpoints are the interface for "why did traffic go there", and the
// metrics are what an alert fires on.
func checkObservability(c *checker, e *environment) {
	dead := mockbackend.New(mockbackend.Options{Name: "dead", FailStatus: 503})
	defer dead.Close()
	alive := mockbackend.New(mockbackend.Options{Name: "alive"})
	defer alive.Close()

	cfg := baseConfig([]fleetEntry{
		{name: "dead", back: dead, priority: 1},
		{name: "alive", back: alive, priority: 2},
	})
	cfg.Routing.Strategy = config.StrategyPriority
	st := e.newStack(c, cfg)
	if st == nil {
		return
	}
	defer st.close()

	post(c, st.url, chatBody, nil, false)

	var up adminUpstreams
	if getJSON(c, st.url+"/admin/upstreams", &up) {
		c.assert(len(up.Upstreams) == 2, "the admin surface lists both backends (%d)", len(up.Upstreams))
		c.assert(up.Routing.Strategy == config.StrategyPriority,
			"the admin surface reports the active strategy (%q)", up.Routing.Strategy)
		c.assert(len(up.BreakerStates) == 2, "the admin surface reports breaker state per backend (%v)", up.BreakerStates)
	}

	var br adminBreakers
	if getJSON(c, st.url+"/admin/breakers", &br) {
		c.assert(len(br.Reports) == 2, "the breaker endpoint reports every backend (%d)", len(br.Reports))
		found := false
		for _, r := range br.Reports {
			if r.Name == "dead" && r.Failures > 0 {
				found = true
				c.info("dead breaker: state=%s attempts=%d failures=%d ratio=%.2f", r.State, r.Attempts, r.Failures, r.FailureRatio)
			}
		}
		c.assert(found, "the failed backend's breaker shows real evidence (reports=%s)", truncate(fmt.Sprint(br.Reports), 300))
	}

	metrics := getText(c, st.url+"/metrics")
	c.assert(metricSum(metrics, "infergate_failovers_total") >= 1,
		"the failover counter recorded the failover (sum=%.0f)", metricSum(metrics, "infergate_failovers_total"))
	c.assert(metricSum(metrics, "infergate_requests_total") >= 1,
		"the request counter recorded the request (sum=%.0f)", metricSum(metrics, "infergate_requests_total"))
	c.assert(strings.Contains(metrics, "infergate_breaker_state{"),
		"the breaker state gauge is exported so a dashboard can alert on state=open")

	var stats statsDoc
	if getJSON(c, st.url+"/stats", &stats) {
		c.assert(stats.Requests >= 1, "the request count is reported (%d)", stats.Requests)
	}
}

// adminUpstreams mirrors the JSON document served by /admin/upstreams and
// /admin/breakers. Decoding into a typed struct rather than substring-matching
// the text means a renamed field fails the check instead of silently passing.
type adminUpstreams struct {
	Upstreams []struct {
		Name         string   `json:"name"`
		Models       []string `json:"models"`
		Capabilities []string `json:"capabilities"`
		Priority     int      `json:"priority"`
		CatchAll     bool     `json:"catch_all"`
	} `json:"upstreams"`
	Routing struct {
		Strategy    string  `json:"strategy"`
		MaxAttempts int     `json:"max_attempts"`
		Weights     weights `json:"weights"`
	} `json:"routing"`
	BreakerStates map[string]string `json:"breaker_states"`
}

type weights struct {
	Cost        float64 `json:"cost"`
	Latency     float64 `json:"latency"`
	Reliability float64 `json:"reliability"`
	Priority    float64 `json:"priority"`
}

type adminBreakers struct {
	Reports []struct {
		Name         string  `json:"name"`
		State        string  `json:"state"`
		Trips        int64   `json:"trips"`
		Rejections   int64   `json:"rejections"`
		Attempts     int64   `json:"attempts"`
		Failures     int64   `json:"failures"`
		FailureRatio float64 `json:"failure_ratio"`
	} `json:"upstreams"`
	Summary map[string]int `json:"summary"`
}

type statsDoc struct {
	Requests int `json:"requests"`
	Series   []struct {
		Route    string `json:"route"`
		Upstream string `json:"upstream"`
		Outcome  string `json:"outcome"`
		Count    int64  `json:"count"`
	} `json:"series"`
}

func tailOf(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if len(s) <= 80 {
		return s
	}
	return s[len(s)-80:]
}
