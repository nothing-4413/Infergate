package main

// M4 router-level checks.
//
// These read the router's plan directly. That is not a shortcut around the
// assembled gateway: the plan IS the decision, and the gateway never exposes
// it. What the gateway does with the decision -- and what the backends actually
// received -- is e2e.go's job.

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/router"
	"github.com/infergate/infergate/internal/upstream"
)

const (
	// tierFleetBaseURL is where the router-level fleets point. Nothing dials
	// it: those checks read the plan and never send the request.
	tierFleetBaseURL = "http://127.0.0.1:9101"

	// reasonSimple and reasonHard are the two classification words the tiered
	// reason strings use for the PREFERRED tier.
	reasonSimple = "simple request prefers this tier"
	reasonHard   = "hard request prefers this tier"
)

// tierFleet is one backend for a router-level check: a name, what it declares,
// and which tier it is on. The zero values are the common case -- a catch-all
// that declares "chat" -- so a fleet reads as the difference from that.
type tierFleet struct {
	name string
	tier string
	prio int
	caps []string
}

// tierGateHealth is deliberately not config.Defaults().Health: a breaker that
// needs twenty recorded requests before it can trip cannot be tripped inside a
// gate run, and the whole point of the unhealthy-tier check is to observe what
// the router does when one HAS tripped.
func tierGateHealth() config.HealthConfig {
	return config.HealthConfig{
		Window:                config.Duration(time.Minute),
		Buckets:               6,
		MinRequests:           2,
		FailureRatio:          0.5,
		OpenDuration:          config.Duration(30 * time.Second),
		HalfOpenProbes:        1,
		MaxFailuresPerRequest: 2,
		RetryBackoff:          config.Duration(10 * time.Millisecond),
	}
}

// newTierRouter assembles a real router over a real registry and breaker group,
// through the same config validation the server uses. It returns nil when the
// fleet itself is invalid, which is a failure of the check rather than of the
// feature.
func newTierRouter(c *checker, strategy string, policy config.TierPolicyConfig, fleets []tierFleet) (*router.Router, *breaker.Group) {
	upstreams := make([]config.UpstreamConfig, 0, len(fleets))
	for _, f := range fleets {
		caps := f.caps
		if len(caps) == 0 {
			caps = []string{"chat"}
		}
		upstreams = append(upstreams, config.UpstreamConfig{
			Name:         f.name,
			Kind:         config.KindOpenAI,
			BaseURL:      tierFleetBaseURL,
			Models:       []string{"/"},
			Capabilities: caps,
			Priority:     f.prio,
			Weight:       1,
			Tier:         f.tier,
		})
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:              ":0",
			UpstreamTimeout:     config.Duration(5 * time.Second),
			MaxBodyBytes:        1 << 20,
			MaxIdleConnsPerHost: 4,
		},
		Routing:   config.RoutingConfig{Strategy: strategy, TierPolicy: policy},
		Health:    tierGateHealth(),
		Upstreams: upstreams,
	}
	if err := cfg.Validate(); err != nil {
		c.assert(false, "router: build a %s fleet of %d upstream(s) (%v)", strategy, len(fleets), err)
		return nil, nil
	}
	registry, err := upstream.New(cfg)
	if err != nil {
		c.assert(false, "router: build the upstream registry (%v)", err)
		return nil, nil
	}
	group := breaker.NewGroup(cfg.Health, registry.Names())
	return router.New(router.Options{
		Registry: registry,
		Breakers: group,
		Routing:  cfg.Routing,
		Health:   cfg.Health,
	}), group
}

func planTier(c *checker, r *router.Router, label string, req router.Request) ([]router.Candidate, bool) {
	if r == nil {
		return nil, false
	}
	plan, err := r.Plan(req)
	if err != nil {
		c.assert(false, "router: %s produces a plan (%v)", label, err)
		return nil, false
	}
	return plan, true
}

func tierNames(plan []router.Candidate) []string {
	out := make([]string, 0, len(plan))
	for _, cand := range plan {
		out = append(out, cand.Target.Name)
	}
	return out
}

// describePlan renders a plan for an assertion message: the order, the tier and
// the score are what a reader needs to tell "wrong order" from "wrong tier".
func describePlan(plan []router.Candidate) string {
	parts := make([]string, 0, len(plan))
	for _, cand := range plan {
		parts = append(parts, fmt.Sprintf("%s(tier=%s score=%g healthy=%t)", cand.Target.Name, cand.Target.Tier(), cand.Score, cand.Healthy))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func assertTierOrder(c *checker, label string, plan []router.Candidate, want ...string) bool {
	if !c.assert(len(plan) == len(want),
		"router: %s has %d candidate(s), want %d -- %s", label, len(plan), len(want), describePlan(plan)) {
		return false
	}
	ok := true
	for i, name := range want {
		if !c.assert(plan[i].Target.Name == name,
			"router: %s candidate %d is %q, want %q -- %s", label, i, plan[i].Target.Name, name, describePlan(plan)) {
			ok = false
		}
	}
	return ok
}

func assertTierScores(c *checker, label string, plan []router.Candidate, want ...float64) bool {
	if !c.assert(len(plan) == len(want), "router: %s has %d candidate(s), want %d scores", label, len(plan), len(want)) {
		return false
	}
	ok := true
	for i, score := range want {
		if !c.assert(plan[i].Score == score,
			"router: %s candidate %d (%s) scores %g, want %g -- %s", label, i, plan[i].Target.Name, plan[i].Score, score, describePlan(plan)) {
			ok = false
		}
	}
	return ok
}

func tierIndex(plan []router.Candidate, name string) int {
	for i, cand := range plan {
		if cand.Target.Name == name {
			return i
		}
	}
	return -1
}

func sortedTierNames(plan []router.Candidate) string {
	names := tierNames(plan)
	sort.Strings(names)
	return strings.Join(names, ",")
}

// tierPolicyUnderTest mirrors configs/tiered-local.yaml: a normal chat turn is
// simple, a request asking for a long completion is not.
func tierPolicyUnderTest() config.TierPolicyConfig {
	return config.TierPolicyConfig{LocalMaxPromptTokens: 400, LocalMaxCompletionTokens: 256}
}

// tierPolicySmall is the same policy at numbers small enough to write the
// arithmetic into an assertion message without counting bytes by hand: 100
// tokens is exactly 400 bytes, and 50 completion tokens is one more than the 51
// that crosses it.
func tierPolicySmall() config.TierPolicyConfig {
	return config.TierPolicyConfig{LocalMaxPromptTokens: 100, LocalMaxCompletionTokens: 50}
}

// tierClassificationFleet gives the cloud tier the BETTER priority on purpose.
// If the local tier still wins for a simple request, the only thing that can
// have put it there is the tier.
func tierClassificationFleet() []tierFleet {
	return []tierFleet{
		{name: "cloud", tier: config.TierCloud, prio: 1, caps: []string{"chat", "tools", "vision"}},
		{name: "box", tier: config.TierLocal, prio: 2, caps: []string{"chat"}},
	}
}

// ---------------------------------------------------------------------------
// 1. A simple request prefers the local tier
// ---------------------------------------------------------------------------

func checkSimplePrefersLocal(c *checker, e *environment) {
	r, _ := newTierRouter(c, config.StrategyTiered, tierPolicySmall(), tierClassificationFleet())
	if r == nil {
		return
	}

	plan, ok := planTier(c, r, "a simple request", router.Request{Model: tierAlias})
	if !ok {
		return
	}
	c.info("a simple request plans %s", describePlan(plan))

	if assertTierOrder(c, "a simple request", plan, "box", "cloud") {
		c.assert(plan[0].Target.Priority() == 2 && plan[1].Target.Priority() == 1,
			"router: the local tier leads although the cloud tier has the better priority (%d vs %d), so the tier decided and the priority did not",
			plan[0].Target.Priority(), plan[1].Target.Priority())
	}
	c.assert(len(plan) == 2, "router: the local decision keeps both tiers in the plan (%d candidate(s))", len(plan))
	for i, cand := range plan {
		c.assertLoop(cand.Healthy, "router: a simple request's candidate %d (%s) is healthy", i, cand.Target.Name)
		c.assert(plan[i].Model == tierAlias,
			"router: candidate %d (%s) is asked for the caller's alias %q (%q)", i, cand.Target.Name, tierAlias, cand.Model)
	}

	// A request with nothing in it, and a request with a short prompt, are both
	// simple: the guard is against classifying everything hard and quietly
	// sending the local tier's work to the paid provider.
	for _, messages := range []string{"", "hi"} {
		req := router.Request{Model: tierAlias, Messages: []byte(messages)}
		short, ok := planTier(c, r, fmt.Sprintf("a %d-byte prompt", len(messages)), req)
		if !ok {
			continue
		}
		assertTierOrder(c, fmt.Sprintf("a %d-byte prompt", len(messages)), short, "box", "cloud")
	}

	// Classification is a property of the request, not of the router: a hard
	// request must not leave the next simple one on the cloud.
	if hard, ok := planTier(c, r, "a hard request", router.Request{Model: tierAlias, MaxTokens: 4096}); ok {
		assertTierOrder(c, "a hard request", hard, "cloud", "box")
	}
	after, ok := planTier(c, r, "a simple request after a hard one", router.Request{Model: tierAlias})
	if ok {
		assertTierOrder(c, "a simple request after a hard one", after, "box", "cloud")
	}
}

// ---------------------------------------------------------------------------
// 2. The tier limits
// ---------------------------------------------------------------------------

func checkTierLimits(c *checker, e *environment) {
	policy := tierPolicySmall()
	r, _ := newTierRouter(c, config.StrategyTiered, policy, tierClassificationFleet())
	if r == nil {
		return
	}

	// The estimator is len(serialised messages)/4 with INTEGER division, so the
	// boundary is a byte count, not a token count. The grid below is chosen to
	// straddle it: 400 bytes is exactly the limit, 403 is still 100 tokens
	// after truncation, and 404 is the first byte count that is over.
	grid := []int{0, 1, 3, 4, 5, 399, 400, 401, 403, 404, 405, 4096}
	for _, n := range grid {
		plan, err := r.Plan(router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), n)})
		if !c.assertLoop(err == nil, "limits: a %d-byte prompt plans (%v)", n, err) {
			continue
		}
		simple := n/4 <= policy.LocalMaxPromptTokens
		want := config.TierCloud
		word := "hard"
		relation := "over"
		if simple {
			want = config.TierLocal
			word = "simple"
			relation = "at or under"
		}
		c.assertLoop(plan[0].Target.Tier() == want,
			"limits: %d bytes / 4 = %d estimated prompt tokens is %s the limit of %d, so the %s tier leads -- %s",
			n, n/4, relation, policy.LocalMaxPromptTokens, want, describePlan(plan))
		c.assertLoop(strings.Contains(plan[0].Reason, word),
			"limits: the %d-byte prompt is called %q by its own reason -- %s", n, word, describePlan(plan))
	}

	// The completion axis, with a prompt short enough that only the ceiling can
	// classify the request.
	for _, maxTokens := range []int{0, 1, 49, 50, 51, 256, 4096} {
		plan, err := r.Plan(router.Request{Model: tierAlias, Messages: []byte("hi"), MaxTokens: maxTokens})
		if !c.assertLoop(err == nil, "limits: a %d-token ceiling plans (%v)", maxTokens, err) {
			continue
		}
		simple := maxTokens <= policy.LocalMaxCompletionTokens
		want := config.TierCloud
		relation := "over"
		if simple {
			want = config.TierLocal
			relation = "at or under"
		}
		c.assertLoop(plan[0].Target.Tier() == want,
			"limits: a ceiling of %d is %s the limit of %d, so the %s tier leads -- %s",
			maxTokens, relation, policy.LocalMaxCompletionTokens, want, describePlan(plan))
	}

	// 0 disables that half of the rule. This is the difference between a
	// deployment that only cares about capability and one that cares about
	// length, and getting it backwards is the expensive direction.
	cases := []struct {
		name   string
		policy config.TierPolicyConfig
		req    router.Request
		want   []string
		why    string
	}{
		{
			name:   "only the completion limit is set",
			policy: config.TierPolicyConfig{LocalMaxCompletionTokens: 50},
			req:    router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), 4096), MaxTokens: 50},
			want:   []string{"box", "cloud"},
			why:    "a zero prompt limit disables the prompt rule, so a 4096-byte prompt (1024 estimated tokens) is still simple -- and the completion rule is satisfied exactly at its limit, the strict > comparison's other half",
		},
		{
			name:   "only the prompt limit is set",
			policy: config.TierPolicyConfig{LocalMaxPromptTokens: 100},
			req:    router.Request{Model: tierAlias, Messages: []byte("hi"), MaxTokens: 4096},
			want:   []string{"box", "cloud"},
			why:    "a zero completion limit disables the completion rule, so a 4096-token ceiling is still simple",
		},
		{
			name:   "only the prompt limit is set, and the prompt is over it",
			policy: config.TierPolicyConfig{LocalMaxPromptTokens: 100},
			req:    router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), 4096)},
			want:   []string{"cloud", "box"},
			why:    "the half that IS set still classifies",
		},
		{
			name:   "neither limit is set",
			policy: config.TierPolicyConfig{},
			req:    router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), 4096), MaxTokens: 4096},
			want:   []string{"box", "cloud"},
			why:    "an unset policy must not send the whole fleet to the paid tier",
		},
		{
			name:   "both limits are explicitly zero",
			policy: config.TierPolicyConfig{LocalMaxPromptTokens: 0, LocalMaxCompletionTokens: 0},
			req:    router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), 4096), MaxTokens: 4096},
			want:   []string{"box", "cloud"},
			why:    "zero is a value, not an omission",
		},
		{
			name:   "the prompt is over the limit and the ceiling is not",
			policy: tierPolicySmall(),
			req:    router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), 404), MaxTokens: 10},
			want:   []string{"cloud", "box"},
			why:    "the two axes are independent",
		},
	}
	for _, tc := range cases {
		r, _ := newTierRouter(c, config.StrategyTiered, tc.policy, tierClassificationFleet())
		if r == nil {
			continue
		}
		plan, err := r.Plan(tc.req)
		if !c.assertLoop(err == nil, "limits(%s): plans (%v)", tc.name, err) {
			continue
		}
		if assertTierOrder(c, tc.name, plan, tc.want...) {
			c.info("%s: %s", tc.name, tc.why)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Capability requests are eligibility-filtered, not merely ordered
// ---------------------------------------------------------------------------

func checkCapabilityEligibility(c *checker, e *environment) {
	fleet := []tierFleet{
		{name: "box", tier: config.TierLocal, prio: 1, caps: []string{"chat", "audio"}},
		{name: "cloud", tier: config.TierCloud, prio: 2, caps: []string{"chat", "tools", "vision"}},
	}
	// An unset policy: Validate fills cloud_capabilities with tools and vision,
	// and the behaviour below is what proves it did.
	r, _ := newTierRouter(c, config.StrategyTiered, config.TierPolicyConfig{}, fleet)
	if r == nil {
		return
	}

	// A request that needs a capability the local box does not declare must not
	// have the local box in the plan AT ALL. Ordering it last would let it be
	// chosen the moment the cloud hiccups, and a local model that silently
	// ignores a tool call is worse than a 502.
	for _, need := range []string{"tools", "vision", "TOOLS ", "vision"} {
		plan, err := r.Plan(router.Request{Model: tierAlias, Capabilities: []string{need}})
		if !c.assertLoop(err == nil, "capability: a request needing %q plans (%v)", need, err) {
			continue
		}
		if assertTierOrder(c, fmt.Sprintf("a request needing %q", need), plan, "cloud") {
			c.assertLoop(tierIndex(plan, "box") == -1,
				"capability: %q leaves the local tier OUT of the plan rather than last in it -- %s", need, describePlan(plan))
		}
	}

	// A capability that is not in cloud_capabilities is not a reason to leave
	// the local tier -- but the request is still filtered by eligibility, so
	// the cloud leaves the plan instead: it never declared audio. The two rules
	// are independent, and this is the case where they point opposite ways.
	plan, ok := planTier(c, r, "a request needing audio", router.Request{Model: tierAlias, Capabilities: []string{"audio"}})
	if ok {
		if assertTierOrder(c, "a request needing audio", plan, "box") {
			c.assert(strings.Contains(plan[0].Reason, "simple"),
				"capability: audio is not a cloud capability, so the request stayed simple -- %s", describePlan(plan))
			c.assert(tierIndex(plan, "cloud") == -1,
				"capability: the cloud does not declare audio, so it is absent from the plan -- %s", describePlan(plan))
		}
	}

	// Two capabilities at once: the local box declares one of them and the
	// cloud declares the other, so no single backend can serve the request and
	// there is no plan at all. The gateway would rather refuse than hand a tool
	// call to a model that cannot make one.
	if _, err := r.Plan(router.Request{Model: tierAlias, Capabilities: []string{"audio", "tools"}}); c.assert(err != nil,
		"capability: capabilities no single backend declares are a refusal, not a downgrade (%v)", err) {
		// The refusal is correct but its wording is not: a capability problem
		// is reported as if the model were unknown. Recorded here rather than
		// fixed, because product code is outside this gate's remit.
		c.assert(strings.Contains(err.Error(), "no upstream serves model"),
			"capability: the refusal blames the model rather than the capabilities (got %q)", err.Error())
	}
	// ...and the other way round: a capability the CLOUD lacks removes the
	// cloud, even though the request is not hard at all.
	if plan, ok := planTier(c, r, "audio and chat together", router.Request{Model: tierAlias, Capabilities: []string{"audio", "chat"}}); ok {
		assertTierOrder(c, "audio and chat together", plan, "box")
	}

	// A capability nobody declares is a routing failure, not a silent fallback.
	if _, err := r.Plan(router.Request{Model: tierAlias, Capabilities: []string{"embeddings"}}); c.assert(err != nil,
		"capability: a request no upstream can serve is an error, not a plan (%v)", err) {
		if err != nil {
			c.assert(strings.Contains(err.Error(), tierAlias),
				"capability: the error names the model nobody could serve (%q)", err.Error())
		}
	}

	// A handful of unrelated capability sets, checked as a loop: every one of
	// them must plan, and the tier that leads is decided by whether the set
	// reaches into the cloud capabilities -- not by which backend happens to
	// declare what, which is the eligibility filter's separate job.
	sets := [][]string{
		{}, {"chat"}, {"audio"}, {"chat", "audio"}, {"tools"}, {"vision"},
		{"tools", "vision"}, {"AUDIO"}, {"chat", "vision"},
	}
	for _, caps := range sets {
		plan, err := r.Plan(router.Request{Model: tierAlias, Capabilities: caps})
		if !c.assertLoop(err == nil, "capability: the set %v is servable (%v)", caps, err) {
			continue
		}
		hard := false
		for _, need := range caps {
			if strings.EqualFold(strings.TrimSpace(need), "tools") || strings.EqualFold(strings.TrimSpace(need), "vision") {
				hard = true
			}
		}
		want := "box"
		if hard {
			want = "cloud"
		}
		c.assertLoop(plan[0].Target.Name == want,
			"capability: the set %v leads with %s -- %s", caps, want, describePlan(plan))
	}

	// The complement of the loop above: sets that SPLIT the fleet. Each one
	// names a capability the box has and one the cloud has, so no single
	// backend is eligible and the router must refuse rather than pick the
	// lesser evil. A plan here would be the silent-downgrade bug this feature
	// exists to prevent.
	for _, caps := range [][]string{{"audio", "vision"}, {"audio", "tools"}} {
		if _, err := r.Plan(router.Request{Model: tierAlias, Capabilities: caps}); c.assertLoop(err != nil,
			"capability: the split set %v has no eligible backend, so it is refused (%v)", caps, err) {
			c.assertLoop(strings.Contains(err.Error(), tierAlias),
				"capability: the refusal for %v names the model (%q)", caps, err.Error())
		}
	}

	// cloud_capabilities is configuration, not a constant: declaring audio as a
	// cloud capability moves audio requests to the cloud, and the match folds
	// case and surrounding space on both sides.
	audioFleet := []tierFleet{
		{name: "box", tier: config.TierLocal, prio: 1, caps: []string{"chat", "audio"}},
		{name: "cloud", tier: config.TierCloud, prio: 2, caps: []string{"chat", "audio"}},
	}
	for _, declared := range []string{"audio", "AUDIO", " audio "} {
		r, _ := newTierRouter(c, config.StrategyTiered, config.TierPolicyConfig{CloudCapabilities: []string{declared}}, audioFleet)
		if r == nil {
			continue
		}
		plan, err := r.Plan(router.Request{Model: tierAlias, Capabilities: []string{"audio"}})
		if !c.assertLoop(err == nil, "capability: cloud_capabilities %q plans (%v)", declared, err) {
			continue
		}
		if assertTierOrder(c, fmt.Sprintf("cloud_capabilities %q", declared), plan, "cloud", "box") {
			c.assertLoop(strings.Contains(plan[0].Reason, "hard"),
				"capability: %q makes an audio request hard -- %s", declared, describePlan(plan))
		}
	}

	// The capability rule is not a length rule, and a huge length limit does
	// not switch it off.
	wide, _ := newTierRouter(c, config.StrategyTiered, config.TierPolicyConfig{
		LocalMaxPromptTokens:     100000,
		LocalMaxCompletionTokens: 100000,
	}, fleet)
	if wide != nil {
		if plan, ok := planTier(c, wide, "tools under a huge length limit", router.Request{Model: tierAlias, Capabilities: []string{"tools"}, Messages: []byte("hi")}); ok {
			assertTierOrder(c, "tools under a huge length limit", plan, "cloud")
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Order inside a tier
// ---------------------------------------------------------------------------

func checkIntraTierOrder(c *checker, e *environment) {
	// The local tier's three members are named so that CONFIG order and
	// ALPHABETICAL order disagree: boxZ is declared first with the worse
	// priority, and boxY/boxX tie on priority 1. A plan of [boxY boxX boxZ] is
	// therefore config order, and [boxX boxY boxZ] would be a sort somebody did
	// not mean.
	fleet := []tierFleet{
		{name: "boxZ", tier: config.TierLocal, prio: 2, caps: []string{"chat"}},
		{name: "boxY", tier: config.TierLocal, prio: 1},
		{name: "boxX", tier: config.TierLocal, prio: 1},
		{name: "cloud", tier: config.TierCloud, prio: 0, caps: []string{"chat", "tools", "vision"}},
	}
	r, _ := newTierRouter(c, config.StrategyTiered, config.TierPolicyConfig{}, fleet)
	if r == nil {
		return
	}

	plan, ok := planTier(c, r, "a simple request", router.Request{Model: tierAlias})
	if !ok {
		return
	}
	c.info("a simple request over four backends plans %s", describePlan(plan))
	if assertTierOrder(c, "a simple request", plan, "boxY", "boxX", "boxZ", "cloud") {
		c.assert(tierIndex(plan, "boxY") < tierIndex(plan, "boxX"),
			"router: boxY is declared before boxX and they tie on priority, so config order breaks the tie (%d before %d)",
			tierIndex(plan, "boxY"), tierIndex(plan, "boxX"))
		c.assert(tierIndex(plan, "boxX") < tierIndex(plan, "boxZ"),
			"router: priority 1 sorts before priority 2 inside the tier (%d before %d)",
			tierIndex(plan, "boxX"), tierIndex(plan, "boxZ"))
		c.assert(tierIndex(plan, "boxZ") < tierIndex(plan, "cloud"),
			"router: the preferred tier outranks a cloud backend with priority 0 (%d before %d)",
			tierIndex(plan, "boxZ"), tierIndex(plan, "cloud"))
	}
	for _, cand := range plan {
		wantScore := 1.0
		if cand.Target.Tier() == config.TierLocal {
			wantScore = 0
		}
		c.assertLoop(cand.Score == wantScore,
			"router: %s is on the %s tier with the better priority, so its score is %g (got %g)",
			cand.Target.Name, cand.Target.Tier(), wantScore, cand.Score)
	}

	// A hard request leads with the cloud and leaves the local tier's own order
	// untouched behind it -- the intra-tier rule is not affected by which tier
	// is preferred.
	hard, _ := newTierRouter(c, config.StrategyTiered, config.TierPolicyConfig{LocalMaxPromptTokens: 1}, fleet)
	if hard != nil {
		plan, ok := planTier(c, hard, "a hard request", router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), 400)})
		if ok {
			if assertTierOrder(c, "a hard request", plan, "cloud", "boxY", "boxX", "boxZ") {
				c.assert(tierIndex(plan, "boxY") < tierIndex(plan, "boxX") && tierIndex(plan, "boxX") < tierIndex(plan, "boxZ"),
					"router: the local tier keeps its priority-then-config order behind the cloud -- %s", describePlan(plan))
			}
			for _, cand := range plan {
				wantScore := 1.0
				if cand.Target.Tier() == config.TierCloud {
					wantScore = 0
				}
				c.assertLoop(cand.Score == wantScore,
					"router: on a hard request %s scores %g (got %g)", cand.Target.Name, wantScore, cand.Score)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 5. An open breaker yields, in either direction
// ---------------------------------------------------------------------------

func checkUnhealthyTierYields(c *checker, e *environment) {
	fleet := []tierFleet{
		{name: "box", tier: config.TierLocal, prio: 1, caps: []string{"chat"}},
		{name: "cloud", tier: config.TierCloud, prio: 2, caps: []string{"chat", "tools", "vision"}},
	}
	// 1 token is the prompt limit, so the 4-byte message below sits exactly ON
	// it (simple) and the 400-byte one is over it (hard). Both directions of
	// the health rule need one of each.
	r, group := newTierRouter(c, config.StrategyTiered, config.TierPolicyConfig{LocalMaxPromptTokens: 1}, fleet)
	if r == nil || group == nil {
		return
	}
	onLimit := router.Request{Model: tierAlias, Messages: []byte("hmmm")}
	overLimit := router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), 400)}

	plan, ok := planTier(c, r, "a simple request", onLimit)
	if ok {
		if assertTierOrder(c, "a simple request", plan, "box", "cloud") {
			c.assert(plan[0].Healthy && plan[1].Healthy, "breaker: both tiers start healthy -- %s", describePlan(plan))
		}
	}
	if plan, ok := planTier(c, r, "a hard request", overLimit); ok {
		assertTierOrder(c, "a hard request", plan, "cloud", "box")
	}

	// Trip the local tier the way production does: recorded failures. The
	// breaker reads its decision out of the stats window and leaves the window
	// to the proxy, so filling only one of the two leaves it closed.
	window := group.Get("box").Stats()
	for i := 0; i < 4; i++ {
		window.RecordFailure(false)
		group.Get("box").RecordFailure(false)
	}
	if !c.assert(group.Get("box").State() == breaker.StateOpen,
		"breaker: the local tier's breaker opens after 8 failures in a minimum-2 window (state %s)", group.Get("box").State()) {
		return
	}

	plan, ok = planTier(c, r, "a simple request with an open local breaker", onLimit)
	if ok {
		c.info("with the local breaker open: %s", describePlan(plan))
		if assertTierOrder(c, "a simple request with an open local breaker", plan, "cloud", "box") {
			c.assert(!plan[1].Healthy, "breaker: the local tier is still in the plan, marked unhealthy rather than deleted -- %s", describePlan(plan))
			c.assert(plan[0].Healthy, "breaker: the cloud tier is the healthy one -- %s", describePlan(plan))
		}
		c.assert(plan[1].Score == 0,
			"breaker: an unhealthy local tier still scores 0, because Score ranks the tier and health is reported separately (got %g)",
			plan[1].Score)
		c.assert(strings.Contains(plan[1].Reason, "circuit open, only used as a last resort"),
			"breaker: the local tier's reason says it is a last resort (%q)", plan[1].Reason)
		c.assert(strings.Contains(plan[0].Reason, "falls back"),
			"breaker: the cloud tier's reason says it is the fallback the request got (%q)", plan[0].Reason)
	}

	// A hard request while the LOCAL tier is unhealthy: the cloud was already
	// preferred, so this only pins that health did not reshuffle the rest.
	if plan, ok := planTier(c, r, "a hard request with an open local breaker", overLimit); ok {
		assertTierOrder(c, "a hard request with an open local breaker", plan, "cloud", "box")
	}

	// The other direction: health outranks the tier even when the tier says
	// otherwise, so a hard request whose cloud tier is down goes to the box.
	group.Get("box").Reset()
	if plan, ok := planTier(c, r, "a hard request after the local reset", overLimit); ok {
		assertTierOrder(c, "a hard request after the local reset", plan, "cloud", "box")
	}
	window = group.Get("cloud").Stats()
	for i := 0; i < 4; i++ {
		window.RecordFailure(false)
		group.Get("cloud").RecordFailure(false)
	}
	if !c.assert(group.Get("cloud").State() == breaker.StateOpen,
		"breaker: the cloud tier's breaker opens too (state %s)", group.Get("cloud").State()) {
		return
	}
	if plan, ok := planTier(c, r, "a hard request with an open cloud breaker", overLimit); ok {
		if assertTierOrder(c, "a hard request with an open cloud breaker", plan, "box", "cloud") {
			c.assert(plan[0].Target.Tier() == config.TierLocal,
				"breaker: health outranks the tier, so a hard request falls to the local tier when the cloud is open -- %s", describePlan(plan))
			c.assert(!plan[1].Healthy, "breaker: the cloud tier is marked unhealthy rather than deleted -- %s", describePlan(plan))
		}
	}
	if plan, ok := planTier(c, r, "a simple request with an open cloud breaker", onLimit); ok {
		assertTierOrder(c, "a simple request with an open cloud breaker", plan, "box", "cloud")
	}
}

// ---------------------------------------------------------------------------
// 6. A hard request keeps the local tier behind the cloud
// ---------------------------------------------------------------------------

func checkHardKeepsFallback(c *checker, e *environment) {
	r, _ := newTierRouter(c, config.StrategyTiered, tierPolicySmall(), tierClassificationFleet())
	if r == nil {
		return
	}

	simple, okSimple := planTier(c, r, "a simple request", router.Request{Model: tierAlias})
	hard, okHard := planTier(c, r, "a hard request", router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), 404)})
	if !okSimple || !okHard {
		return
	}
	c.info("simple: %s", describePlan(simple))
	c.info("hard:   %s", describePlan(hard))

	// The decision reorders the plan; it never shortens it. If a hard request
	// dropped the local tier, a cloud outage would be an outage.
	c.assert(sortedTierNames(simple) == sortedTierNames(hard),
		"router: the tier decision reorders the plan without changing its membership (simple %s, hard %s)",
		sortedTierNames(simple), sortedTierNames(hard))
	if assertTierOrder(c, "a hard request", hard, "cloud", "box") {
		c.assert(hard[1].Target.Tier() == config.TierLocal,
			"router: the local tier is still the hard request's fallback (%s at position 1)", hard[1].Target.Name)
		c.assert(strings.Contains(hard[1].Reason, "hard request falls back to this tier"),
			"router: the fallback says why it is second (%q)", hard[1].Reason)
	}
	assertTierScores(c, "a hard request", hard, 0, 1)

	// The same must hold for each way of being hard: by the prompt, by the
	// completion ceiling, and by a capability the local tier cannot serve --
	// except that the last one is a different rule (an upstream that cannot
	// serve a capability is not a candidate at all), so it is checked with a
	// fleet where the local tier DOES declare the capability.
	for _, tc := range []struct {
		name string
		req  router.Request
	}{
		{"a hard prompt", router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), 404)}},
		{"a long completion", router.Request{Model: tierAlias, MaxTokens: 51}},
		{"a very long completion", router.Request{Model: tierAlias, MaxTokens: 100000}},
	} {
		plan, ok := planTier(c, r, tc.name, tc.req)
		if !ok {
			continue
		}
		if assertTierOrder(c, tc.name, plan, "cloud", "box") {
			c.assertLoop(tierIndex(plan, "box") >= 0,
				"router: %s keeps the local tier in the plan -- %s", tc.name, describePlan(plan))
		}
	}

	toolsFleet := []tierFleet{
		{name: "cloud", tier: config.TierCloud, prio: 1, caps: []string{"chat", "tools"}},
		{name: "box", tier: config.TierLocal, prio: 2, caps: []string{"chat", "tools"}},
	}
	tr, _ := newTierRouter(c, config.StrategyTiered, tierPolicySmall(), toolsFleet)
	if tr != nil {
		plan, ok := planTier(c, tr, "a request needing tools", router.Request{Model: tierAlias, Capabilities: []string{"tools"}})
		if ok {
			if assertTierOrder(c, "a request needing tools", plan, "cloud", "box") {
				c.assert(strings.Contains(plan[0].Reason, "hard"),
					"router: a capability request the local box could serve is still classified hard (%q)", plan[0].Reason)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 7. Reason strings and scores
// ---------------------------------------------------------------------------

func checkReasonAndScore(c *checker, e *environment) {
	r, _ := newTierRouter(c, config.StrategyTiered, tierPolicySmall(), tierClassificationFleet())
	if r == nil {
		return
	}

	simple, okSimple := planTier(c, r, "a simple request", router.Request{Model: tierAlias})
	hard, okHard := planTier(c, r, "a hard request", router.Request{Model: tierAlias, Messages: bytes.Repeat([]byte("a"), 404)})
	if !okSimple || !okHard {
		return
	}

	cases := []struct {
		name    string
		plan    []router.Candidate
		reasons []string
		scores  []float64
	}{
		{
			name: "a simple request",
			plan: simple,
			reasons: []string{
				"strategy=tiered tier=local reason=simple request prefers this tier",
				"strategy=tiered tier=cloud reason=simple request falls back to this tier",
			},
			scores: []float64{0, 1},
		},
		{
			name: "a hard request",
			plan: hard,
			reasons: []string{
				"strategy=tiered tier=cloud reason=hard request prefers this tier",
				"strategy=tiered tier=local reason=hard request falls back to this tier",
			},
			scores: []float64{0, 1},
		},
	}
	for _, tc := range cases {
		if !c.assertLoop(len(tc.plan) == len(tc.reasons),
			"reason(%s): the plan has %d candidate(s), one reason each (%d)", tc.name, len(tc.plan), len(tc.reasons)) {
			continue
		}
		for i, want := range tc.reasons {
			c.assertLoop(tc.plan[i].Reason == want,
				"reason(%s): candidate %d (%s) reports %q (got %q)", tc.name, i, tc.plan[i].Target.Name, want, tc.plan[i].Reason)
			c.assertLoop(tc.plan[i].Score == tc.scores[i],
				"reason(%s): candidate %d (%s) scores %g (got %g)", tc.name, i, tc.plan[i].Target.Name, tc.scores[i], tc.plan[i].Score)
		}
		for i, cand := range tc.plan {
			c.assertLoop(strings.HasPrefix(cand.Reason, "strategy=tiered tier="+cand.Target.Tier()+" "),
				"reason(%s): candidate %d's reason opens with the strategy and its own tier (%q)", tc.name, i, cand.Reason)
			c.assertLoop(strings.Contains(cand.Reason, "reason="),
				"reason(%s): candidate %d's reason explains itself (%q)", tc.name, i, cand.Reason)
			c.assertLoop(!strings.Contains(cand.Reason, "priority"),
				"reason(%s): candidate %d's reason does not borrow the priority strategy's vocabulary (%q)", tc.name, i, cand.Reason)
			c.assertLoop(cand.Score == 0 || cand.Score == 1,
				"reason(%s): candidate %d's score is a tier rank, 0 or 1 (got %g)", tc.name, i, cand.Score)
		}
		prefers := 0
		for _, cand := range tc.plan {
			if strings.Contains(cand.Reason, "prefers this tier") {
				prefers++
			}
		}
		c.assertLoop(prefers == 1,
			"reason(%s): exactly one candidate claims the request prefers its tier (%d) -- %s", tc.name, prefers, describePlan(tc.plan))
		c.assertLoop(fmt.Sprintf("%v", tierNames(tc.plan)) != "" && strings.Contains(tc.plan[0].Reason, "prefers this tier"),
			"reason(%s): the tier that prefers the request is the first candidate -- %s", tc.name, describePlan(tc.plan))
	}

	// The two canonical "preferred" strings, spelled out once more as whole
	// lines, so a change to the wording cannot pass unnoticed by both this
	// check and the loop above.
	c.assert(simple[0].Reason == "strategy=tiered tier=local reason=simple request prefers this tier",
		"reason: a simple request's first reason is the documented local one (%q)", simple[0].Reason)
	c.assert(hard[0].Reason == "strategy=tiered tier=cloud reason=hard request prefers this tier",
		"reason: a hard request's first reason is the documented cloud one (%q)", hard[0].Reason)
}
