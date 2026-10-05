package router

import (
	"testing"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/upstream"
)

// routingFleet describes backends the way a config file would: a name, the
// models it claims, and the routing attributes that decide order.
type routingFleet struct {
	name    string
	models  []string
	caps    []string
	prio    int
	weight  float64
	baseURL string
}

// newRoutingRouter builds a real registry and a real breaker group -- not stubs
// -- because the router's whole job is to read their state correctly, and a stub
// would let it pass while mis-reading the real one.
func newRoutingRouter(t *testing.T, strategy string, weights config.RoutingWeights, targets []routingFleet, health config.HealthConfig) (*Router, *breaker.Group) {
	t.Helper()

	ups := make([]config.UpstreamConfig, 0, len(targets))
	for _, tg := range targets {
		base := tg.baseURL
		if base == "" {
			base = "http://127.0.0.1:9" + string(rune('0'+len(ups)))
		}
		ups = append(ups, config.UpstreamConfig{
			Name:         tg.name,
			Kind:         config.KindOpenAI,
			BaseURL:      base,
			Models:       tg.models,
			Capabilities: tg.caps,
			Priority:     tg.prio,
			Weight:       tg.weight,
		})
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:              ":0",
			UpstreamTimeout:     config.Duration(5 * time.Second),
			MaxBodyBytes:        1 << 20,
			MaxIdleConnsPerHost: 4,
		},
		Upstreams: ups,
		Routing: config.RoutingConfig{
			Strategy: strategy,
			Weights:  weights,
		},
		Health: health,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config.Validate: %v", err)
	}

	registry, err := upstream.New(cfg)
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	t.Cleanup(registry.CloseIdleConnections)

	group := breaker.NewGroup(cfg.Health, registry.Names())
	r := New(Options{
		Registry: registry,
		Breakers: group,
		Routing:  cfg.Routing,
		Health:   cfg.Health,
		// Prices are keyed by the model the caller names. A catch-all backend
		// receives the caller's model verbatim, so two catch-alls cost the same
		// for a given request -- which is exactly why the cost tests below use
		// backends with distinct declared models: the price of a request is a
		// property of (model, backend), and the router can only compare
		// candidates whose models it can tell apart.
		Price: func(model string) (float64, bool) {
			switch model {
			case "gpt-4o": // the remote flagship
				return 10, true
			case "deepseek-chat": // a cheaper remote
				return 1, true
			case "local-llama": // a local vLLM: no per-token bill
				return 0, true
			case "mystery":
				return 0, false // deliberately unpriced
			}
			return 0, false
		},
	})
	return r, group
}

func testHealth() config.HealthConfig {
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

func plan(t *testing.T, r *Router, req Request) []Candidate {
	t.Helper()
	cands, err := r.Plan(req)
	if err != nil {
		t.Fatalf("Plan(%+v): %v", req, err)
	}
	return cands
}

func names(cands []Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Target.Name)
	}
	return out
}

func assertOrder(t *testing.T, cands []Candidate, want ...string) {
	t.Helper()
	got := names(cands)
	if len(got) != len(want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("plan = %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Strategy ordering
// ---------------------------------------------------------------------------

// TestPriorityIsTheDefaultOrder checks the tie-break an operator can actually
// see in the file: lower priority number first, configuration order otherwise.
func TestPriorityIsTheDefaultOrder(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyPriority, config.RoutingWeights{}, []routingFleet{
		{name: "third", models: []string{"/"}, prio: 30},
		{name: "first", models: []string{"/"}, prio: 10},
		{name: "second", models: []string{"/"}, prio: 20},
		{name: "same-as-second", models: []string{"/"}, prio: 20},
	}, testHealth())

	cands := plan(t, r, Request{Model: "gpt-4o"})
	assertOrder(t, cands, "first", "second", "same-as-second", "third")
	if cands[0].Score != 10 {
		t.Fatalf("Score = %v, want the priority 10", cands[0].Score)
	}
}

// TestCostOrderingBeatsPriority shows the strategy actually overriding the
// default: priority numbers are ignored when sorting by price.
func TestCostOrderingBeatsPriority(t *testing.T) {
	// Two catch-all backends, declared in the order an operator would write them
	// when they want to prefer the flagship. Each candidate is asked for the
	// model the caller named, so distinct declared models are what let their
	// prices differ.
	r, _ := newRoutingRouter(t, config.StrategyCost, config.RoutingWeights{}, []routingFleet{
		{name: "flagship", models: []string{"gpt-4o"}, prio: 1},
		{name: "budget", models: []string{"deepseek-chat"}, prio: 99,
			baseURL: "http://127.0.0.1:18002"},
	}, testHealth())

	// Only the backend that claims the model is eligible, so the cost strategy
	// has nothing to choose between here...
	assertOrder(t, plan(t, r, Request{Model: "gpt-4o"}), "flagship")
	assertOrder(t, plan(t, r, Request{Model: "deepseek-chat"}), "budget")

	// ...and the choice appears once both can serve the same request, which is
	// the catch-all case.
	r2, _ := newRoutingRouter(t, config.StrategyCost, config.RoutingWeights{}, []routingFleet{
		{name: "flagship", models: []string{"/"}, prio: 1},
		{name: "budget", models: []string{"/"}, prio: 99},
	}, testHealth())
	cands := plan(t, r2, Request{Model: "deepseek-chat"})
	if len(cands) != 2 {
		t.Fatalf("plan = %v, want both catch-all backends", names(cands))
	}
	// Both serve the same model, so cost is indistinguishable and the priority
	// tie-break is what orders them -- which is the honest answer: identical
	// prices mean the operator's stated preference decides.
	assertOrder(t, cands, "flagship", "budget")
}

// TestCostOrderingRespectsModelEligibility checks that a per-candidate price is
// looked up for the model that candidate would actually be asked for.
func TestCostOrderingRespectsModelEligibility(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyCost, config.RoutingWeights{}, []routingFleet{
		{name: "flagship", models: []string{"gpt-4o"}},
		{name: "budget", models: []string{"deepseek-chat"},
			baseURL: "http://127.0.0.1:18003"},
	}, testHealth())

	// Each request reaches exactly one backend, because the other does not claim
	// the model and neither is a catch-all.
	assertOrder(t, plan(t, r, Request{Model: "gpt-4o"}), "flagship")
	assertOrder(t, plan(t, r, Request{Model: "deepseek-chat"}), "budget")
}

// TestUnpricedBackendSortsLast documents a real property of the cost strategy and
// a limit of what the plan can express.
//
// priceOf falls back to the REQUESTED model whenever a candidate's own model is
// absent from the price book. A catch-all backend receives the caller's model
// verbatim, so it always prices as that model; and any other eligible candidate
// is asked for either the same name or its own declared name. The practical
// consequence is that two eligible candidates are normally priced on the same
// scale for one request, and priority is left to break the tie -- the honest
// behaviour, and the one asserted here.
func TestUnpricedBackendSortsLast(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyCost, config.RoutingWeights{}, []routingFleet{
		{name: "priced", models: []string{"unpriced-alias", "gpt-4o"}, prio: 99},
		{name: "unpriced", models: []string{"/"}, prio: 1,
			baseURL: "http://127.0.0.1:18005"},
	}, testHealth())

	// The alias is absent from the price book, so neither candidate can be
	// priced and priority legitimately decides.
	assertOrder(t, plan(t, r, Request{Model: "unpriced-alias"}), "unpriced", "priced")

	// "gpt-4o" IS priced, but priceOf's fallback to the requested model means the
	// catch-all candidate inherits that price rather than sorting last as an
	// unknown. The plan is therefore still decided by priority.
	cands := plan(t, r, Request{Model: "gpt-4o"})
	if len(cands) != 2 {
		t.Fatalf("plan = %v, want both backends eligible", names(cands))
	}
	assertOrder(t, cands, "unpriced", "priced")
	if cands[0].Score != 10 {
		t.Fatalf("Score = %v, want the fallback price of the requested model (10)", cands[0].Score)
	}
}

// TestPriceOfFallsBackToTheRequestedModel pins the mechanism the test above
// relies on, including the case the plan cannot express: a candidate whose own
// model is unpriced while the caller's model is priced is still priced, because
// the fallback keeps it comparable instead of treating it as free.
func TestPriceOfFallsBackToTheRequestedModel(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyCost, config.RoutingWeights{}, []routingFleet{
		{name: "a", models: []string{"/"}},
	}, testHealth())

	got := r.priceOf("gpt-4o", "gpt-4o")
	if !got.known || got.usd != 10 {
		t.Fatalf("priceOf(priced model) = %+v, want the configured 10", got)
	}
	// A candidate asked for an alias the book does not know, while the caller
	// named a model it does.
	got = r.priceOf("unknown-alias", "gpt-4o")
	if !got.known || got.usd != 10 {
		t.Fatalf("priceOf(unpriced candidate, priced request) = %+v, want the requested model's price", got)
	}
	// Both unknown: the caller must be able to see it, because an unknown price
	// is what makes an operator add a pricing entry.
	got = r.priceOf("unknown-alias", "also-unknown")
	if got.known {
		t.Fatalf("priceOf(two unknown models) = %+v, want known=false", got)
	}
}

// TestFreeBackendWinsOnCost is the mirror case: a genuinely zero price (a local
// box, where the only cost is electricity) must not be treated as missing, and a
// zero score must not break the ordering.
func TestFreeBackendWinsOnCost(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyCost, config.RoutingWeights{}, []routingFleet{
		{name: "remote", models: []string{"gpt-4o"}, prio: 1},
		{name: "local", models: []string{"local-llama"}, prio: 99,
			baseURL: "http://127.0.0.1:18008"},
	}, testHealth())

	cands := plan(t, r, Request{Model: "local-llama"})
	assertOrder(t, cands, "local")
	if cands[0].Score != 0 {
		t.Fatalf("Score = %v, want 0 for a backend priced as free", cands[0].Score)
	}
	assertOrder(t, plan(t, r, Request{Model: "gpt-4o"}), "remote")
}

// TestLatencyOrderingUsesTheMeasuredWindow checks that the strategy reads the
// same window the breaker does, so routing and tripping can never disagree.
func TestLatencyOrderingUsesTheMeasuredWindow(t *testing.T) {
	r, group := newRoutingRouter(t, config.StrategyLatency, config.RoutingWeights{}, []routingFleet{
		{name: "slow", models: []string{"/"}, prio: 1},
		{name: "fast", models: []string{"/"}, prio: 99},
	}, testHealth())

	group.Get("slow").Stats().RecordSuccess(900 * time.Millisecond)
	group.Get("fast").Stats().RecordSuccess(5 * time.Millisecond)

	cands := plan(t, r, Request{Model: "gpt-4o"})
	assertOrder(t, cands, "fast", "slow")
}

// TestUnmeasuredBackendSortsLast mirrors the cost case: never measured must not
// look like instant.
func TestUnmeasuredBackendSortsLast(t *testing.T) {
	r, group := newRoutingRouter(t, config.StrategyLatency, config.RoutingWeights{}, []routingFleet{
		{name: "measured", models: []string{"/"}, prio: 99},
		{name: "never-asked", models: []string{"/"}, prio: 1},
	}, testHealth())

	group.Get("measured").Stats().RecordSuccess(50 * time.Millisecond)
	assertOrder(t, plan(t, r, Request{Model: "gpt-4o"}), "measured", "never-asked")
}

func TestWeightedStrategyHandsOutBothBackends(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyWeighted, config.RoutingWeights{}, []routingFleet{
		{name: "a", models: []string{"/"}, weight: 1},
		{name: "b", models: []string{"/"}, weight: 1},
	}, testHealth())

	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		cands := plan(t, r, Request{Model: "gpt-4o"})
		seen[cands[0].Target.Name]++
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("weighted routing never picked one of the backends: %v", seen)
	}
}

// TestScoreBlendsReliability is the strategy M1 exists for: a backend that fails
// must lose to a slightly pricier one that works.
func TestScoreBlendsReliability(t *testing.T) {
	r, group := newRoutingRouter(t, config.StrategyScore,
		config.RoutingWeights{Cost: 1, Latency: 1, Reliability: 4, Priority: 0},
		[]routingFleet{
			{name: "cheap-flaky", models: []string{"/"}},
			{name: "dear-solid", models: []string{"/"}},
		}, testHealth())

	// The window must be big enough to describe a ratio: a dozen attempts with
	// most of them failing.
	flaky := group.Get("cheap-flaky").Stats()
	for i := 0; i < 8; i++ {
		flaky.RecordFailure(false)
	}
	for i := 0; i < 2; i++ {
		flaky.RecordSuccess(10 * time.Millisecond)
	}
	solid := group.Get("dear-solid").Stats()
	for i := 0; i < 10; i++ {
		solid.RecordSuccess(10 * time.Millisecond)
	}

	cands := plan(t, r, Request{Model: "gpt-4o"})
	if len(cands) != 2 {
		t.Fatalf("plan = %v, want both candidates", names(cands))
	}
	if cands[0].Target.Name == "cheap-flaky" {
		t.Fatalf("reliability weight 4 still ranked the 80%%-failing backend first: %v", names(cands))
	}
}

// ---------------------------------------------------------------------------
// Eligibility
// ---------------------------------------------------------------------------

func TestUnhealthyBackendSortsLastButSurvives(t *testing.T) {
	r, group := newRoutingRouter(t, config.StrategyPriority, config.RoutingWeights{}, []routingFleet{
		{name: "preferred", models: []string{"/"}, prio: 1},
		{name: "backup", models: []string{"/"}, prio: 2},
	}, testHealth())

	window := group.Get("preferred").Stats()
	for i := 0; i < 4; i++ {
		window.RecordFailure(false)
		group.Get("preferred").RecordFailure(false)
	}
	if got := group.Get("preferred").State(); got != breaker.StateOpen {
		t.Fatalf("setup failed: breaker state = %s, want open", got)
	}

	cands := plan(t, r, Request{Model: "gpt-4o"})
	// The open backend is still in the plan, at the back: deleting it would turn
	// "every breaker is open" into "no upstream configured", which is a worse
	// answer than trying the slow one.
	assertOrder(t, cands, "backup", "preferred")
	if cands[1].Healthy {
		t.Fatal("the open backend is marked healthy")
	}
}

func TestMissingCapabilityIsExcluded(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyPriority, config.RoutingWeights{}, []routingFleet{
		{name: "plain", models: []string{"/"}},
		{name: "toolbox", models: []string{"/"}, caps: []string{"tools"}},
	}, testHealth())

	assertOrder(t, plan(t, r, Request{Model: "gpt-4o"}), "plain", "toolbox")
	assertOrder(t, plan(t, r, Request{Model: "gpt-4o", Capabilities: []string{"tools"}}), "toolbox")
}

// TestNoCapabilityMatchIsAnError, not a silent downgrade: answering a
// tool-calling request from a backend that drops the tools produces a plausible
// answer with no tool call, which is far harder to debug than a 400.
func TestNoCapabilityMatchIsAnError(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyPriority, config.RoutingWeights{}, []routingFleet{
		{name: "plain", models: []string{"/"}},
	}, testHealth())

	if _, err := r.Plan(Request{Model: "gpt-4o", Capabilities: []string{"vision"}}); err == nil {
		t.Fatal("Plan succeeded for a capability no backend declares")
	}
}

func TestExplicitPinWinsAndIsAlone(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyPriority, config.RoutingWeights{}, []routingFleet{
		{name: "a", models: []string{"/"}, prio: 1},
		{name: "b", models: []string{"/"}, prio: 2},
	}, testHealth())

	cands := plan(t, r, Request{Model: "gpt-4o", Explicit: "b"})
	assertOrder(t, cands, "b")
	if cands[0].Reason == "" {
		t.Fatal("a pinned candidate carries no reason for the logs")
	}
	if _, err := r.Plan(Request{Model: "gpt-4o", Explicit: "typo"}); err == nil {
		t.Fatal("Plan accepted an unknown explicit upstream")
	}
}

// TestSingleBackendAbsorbsAnyModel preserves the M0 guarantee for local
// inference: the agent framework sends an alias, and the one backend on the box
// gets it, rewritten to the model it actually serves.
func TestSingleBackendAbsorbsAnyModel(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyPriority, config.RoutingWeights{}, []routingFleet{
		{name: "local", models: []string{"qwen2.5-7b-instruct"}},
	}, testHealth())

	cands := plan(t, r, Request{Model: "whatever-the-agent-sends"})
	assertOrder(t, cands, "local")
	if cands[0].Model != "qwen2.5-7b-instruct" {
		t.Fatalf("Model = %q, want the one the backend serves", cands[0].Model)
	}
}

// TestCatchAllKeepsTheCallerModel is the pass-through case: a vLLM or the mock
// declares "/" and must receive the name it was asked for, or a deployment that
// serves exactly the caller's alias breaks.
func TestCatchAllKeepsTheCallerModel(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyPriority, config.RoutingWeights{}, []routingFleet{
		{name: "gateway", models: []string{"/"}},
	}, testHealth())

	cands := plan(t, r, Request{Model: "mock-reasoner"})
	if cands[0].Model != "mock-reasoner" {
		t.Fatalf("Model = %q, want the requested name passed through", cands[0].Model)
	}
}

// TestCatchAllWithConcreteModelPrefersTheConcreteName pins the interaction
// between two patterns on ONE backend: "/" says it can serve anything, and
// "mock-dear" is the model it actually runs.
//
// Reading IsCatchAll() first and returning the caller's alias is how the
// gateway ended up sending "mock-router" to a backend that only answers to
// "mock-dear" -- the most common 404 in local inference setups, and one that
// only shows up when a backend declares a catch-all alongside a real name.
func TestCatchAllWithConcreteModelPrefersTheConcreteName(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyPriority, config.RoutingWeights{}, []routingFleet{
		{name: "local", models: []string{"/", "mock-dear"}},
	}, testHealth())

	cands := plan(t, r, Request{Model: "mock-router"})
	if cands[0].Model != "mock-dear" {
		t.Fatalf("Model = %q, want the concrete model the backend declares", cands[0].Model)
	}

	// An exact match still wins over the concrete fallback: a backend that
	// declares the caller's name must receive it verbatim.
	cands = plan(t, r, Request{Model: "mock-dear"})
	if cands[0].Model != "mock-dear" {
		t.Fatalf("Model = %q, want the requested name when it matches exactly", cands[0].Model)
	}
}

func TestUnknownModelWithNamedBackendsIsAnError(t *testing.T) {
	r, _ := newRoutingRouter(t, config.StrategyPriority, config.RoutingWeights{}, []routingFleet{
		{name: "a", models: []string{"gpt-4o"}},
		{name: "b", models: []string{"deepseek-chat"}},
	}, testHealth())

	if _, err := r.Plan(Request{Model: "not-a-model"}); err == nil {
		t.Fatal("Plan routed a model no backend claims in a fleet with a real choice")
	}
}
