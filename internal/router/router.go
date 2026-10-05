// Package router turns "this request needs model X with capabilities Y" into an
// ordered list of backends to try.
//
// The router never performs I/O and never mutates its inputs: it reads the
// windowed health statistics, ranks the candidates and hands back a plan. The
// proxy executes the plan. Keeping selection separate from execution is what
// makes the routing policy testable without a network, and it is why the
// failover chain can be inspected in the admin API before it is used.
//
// Two invariants matter more than any particular strategy:
//
//   - An unhealthy backend is never first. Ranking decides the order, but the
//     circuit breaker decides eligibility, so no weight configuration can route
//     traffic into an upstream that is currently failing.
//
//   - Routing is deterministic given the same configuration and the same
//     statistics, except for the "weighted" strategy, which is explicitly
//     random. A random tie-break in the other strategies would make an incident
//     unreproducible: the same request to the same gateway would take a
//     different path on every retry.
package router

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/stats"
	"github.com/infergate/infergate/internal/upstream"
)

// Candidate is one backend that could serve a request, with the model name to
// ask it for.
type Candidate struct {
	// Target is the backend.
	Target *upstream.Target

	// Model is the model name to send upstream. It differs from the requested
	// model whenever the chosen backend does not serve that exact name (a local
	// single-model box, for example).
	Model string

	// Score is the ranking value for the configured strategy; lower is better.
	// It is filled in for every strategy, including the ones that do not use
	// it, because the admin endpoint and the logs need a comparable number.
	Score float64

	// Healthy reports whether the circuit breaker currently admits traffic.
	Healthy bool

	// Reason explains why this candidate is in the plan, for logs and the admin
	// endpoint. It is written for a human debugging an incident, not for a
	// machine.
	Reason string
}

// Request is the router's view of an inbound request.
type Request struct {
	// Model is the model name the caller asked for.
	Model string

	// Capabilities are the feature tags the request requires.
	Capabilities []string

	// Explicit, when set, pins the request to one named backend. It is the
	// X-InferGate-Upstream escape hatch: operators use it in production to
	// drain a backend, and tests use it to make routing irrelevant.
	Explicit string
}

// PriceFunc reports the blended cost of one request for a model. The router
// only ever compares costs between candidates for the SAME requested model, so
// a single blended number is enough and avoids pretending to know the prompt
// and completion split before the response exists.
type PriceFunc func(model string) (blendedUSD float64, known bool)

// Router selects candidates.
type Router struct {
	registry *upstream.Registry
	breakers *breaker.Group
	cfg      config.RoutingConfig
	health   config.HealthConfig
	price    PriceFunc

	// now and rand are injectable so that the weighted strategy and the
	// half-open timing can be tested deterministically.
	now  func() time.Time
	rand *rand.Rand
}

// Options configures a Router.
type Options struct {
	Registry *upstream.Registry
	Breakers *breaker.Group
	Routing  config.RoutingConfig
	Health   config.HealthConfig

	// Price reports the blended per-request cost of a model. Optional: without
	// it the cost strategy has nothing to sort by and the router falls back to
	// priority order rather than pretending every backend costs the same.
	Price PriceFunc
}

// New builds a Router.
func New(opts Options) *Router {
	r := &Router{
		registry: opts.Registry,
		breakers: opts.Breakers,
		cfg:      opts.Routing,
		health:   opts.Health,
		price:    opts.Price,
		now:      time.Now,
		rand:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	if r.cfg.Strategy == "" {
		r.cfg.Strategy = config.StrategyPriority
	}
	return r
}

// Strategy returns the active strategy name.
func (r *Router) Strategy() string { return r.cfg.Strategy }

// Plan returns the ordered candidates for req.
//
// Unless explicit is set, the whole eligible fleet is returned, best first, and
// the caller walks it until one attempt succeeds. Returning the full chain
// instead of a single choice is what makes failover a property of the plan
// rather than a special case bolted onto the request path.
func (r *Router) Plan(req Request) ([]Candidate, error) {
	if req.Explicit != "" {
		t, ok := r.registry.Target(req.Explicit)
		if !ok {
			return nil, fmt.Errorf("unknown upstream %q", req.Explicit)
		}
		return []Candidate{{
			Target:  t,
			Model:   modelFor(t, req.Model),
			Healthy: true,
			Reason:  "pinned by X-InferGate-Upstream",
		}}, nil
	}

	targets := r.eligible(req)
	if len(targets) == 0 {
		return nil, fmt.Errorf("no upstream serves model %q", req.Model)
	}

	var cands []Candidate
	for _, t := range targets {
		cands = append(cands, Candidate{
			Target:  t,
			Model:   modelFor(t, req.Model),
			Healthy: r.healthy(t),
		})
	}

	switch r.cfg.Strategy {
	case config.StrategyWeighted:
		return r.orderWeighted(cands), nil
	case config.StrategyCost:
		return r.orderByCost(cands, req.Model), nil
	case config.StrategyLatency:
		return r.orderByLatency(cands), nil
	case config.StrategyScore:
		return r.orderByScore(cands, req.Model), nil
	default:
		return r.orderByPriority(cands), nil
	}
}

// eligible filters the fleet to backends that can actually serve the request.
//
// Model matching, not health, is the first filter: asking a backend that does
// not serve the model is a configuration error, not a transient failure, and
// retrying it on every request would waste an attempt per request forever.
func (r *Router) eligible(req Request) []*upstream.Target {
	all := r.registry.Targets()
	out := make([]*upstream.Target, 0, len(all))
	for _, t := range all {
		if !t.ServesModel(req.Model) {
			// A single-backend deployment serves one quantisation and the agent
			// framework sends an alias the gateway has never seen. M0 absorbed
			// that case in the registry; the router must preserve it, or the
			// most common local deployment breaks the moment M1 lands.
			if !(t.IsCatchAll() || len(all) == 1) {
				continue
			}
		}
		if !t.HasCapabilities(req.Capabilities) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// healthy reports whether the breaker admits traffic for a target.
//
// This reads the state, it does not ask for permission. Allow() is a state
// transition that also claims the single half-open probe; calling it here to
// sort candidates would consume the probe before the proxy ever attempts the
// request, and the proxy's own Allow() would then be refused -- leaving the
// breaker stuck half-open with no traffic able to close it.
//
// Half-open counts as unhealthy for ranking purposes: an upstream on probation
// has not yet proved it recovered, so it must not be preferred over one that is
// known good. It is still returned in the plan (the proxy decides eligibility),
// just ordered behind the healthy candidates.
func (r *Router) healthy(t *upstream.Target) bool {
	if r.breakers == nil {
		return true
	}
	return r.breakers.Get(t.Name).State() == breaker.StateClosed
}

// modelFor decides which model name to send to a backend.
//
// A catch-all backend receives the caller's model verbatim: it is the
// pass-through case, and rewriting it would break a vLLM deployment that serves
// exactly the alias the caller used. A backend that lists concrete models
// receives the one it declared, because asking a single-model backend for a
// name it never heard of is the most common 404 in local inference setups.
func modelFor(t *upstream.Target, requested string) string {
	pat := t.ModelPatterns()
	lowered := strings.ToLower(strings.TrimSpace(requested))
	for _, p := range pat {
		if p != "/" && strings.ToLower(p) == lowered {
			return requested
		}
	}
	// A backend that declares concrete models alongside a catch-all still knows
	// its own names, and asking it for a name it never heard of is the most
	// common 404 in local inference setups. The catch-all pattern means "I can
	// also serve anything else", not "prefer the caller's alias over the model I
	// actually run" -- checking IsCatchAll() FIRST sent the caller's alias to a
	// backend that only answers to "mock-dear".
	for _, p := range pat {
		if p != "/" {
			return p
		}
	}
	return requested
}

// orderByPriority is the default: explicit priority first, then configuration
// order. Configuration order is the tie-break because it is the one thing an
// operator can see and predict from the file.
func (r *Router) orderByPriority(cands []Candidate) []Candidate {
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Healthy != cands[j].Healthy {
			return cands[i].Healthy
		}
		return cands[i].Target.Priority() < cands[j].Target.Priority()
	})
	for i := range cands {
		cands[i].Score = float64(cands[i].Target.Priority())
		cands[i].Reason = reasonFor(cands[i], "priority")
	}
	return cands
}

// orderByCost sorts cheapest first, using the price book.
func (r *Router) orderByCost(cands []Candidate, model string) []Candidate {
	prices := make(map[string]pricedCost, len(cands))
	for _, c := range cands {
		prices[c.Target.Name] = r.priceOf(c.Model, model)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Healthy != cands[j].Healthy {
			return cands[i].Healthy
		}
		a, b := prices[cands[i].Target.Name], prices[cands[j].Target.Name]
		// Unknown prices sort last rather than as zero: a model missing from
		// the price book is an unknown, and treating it as free would route
		// every request to the one backend nobody priced.
		if a.known != b.known {
			return a.known
		}
		if a.usd != b.usd {
			return a.usd < b.usd
		}
		return cands[i].Target.Priority() < cands[j].Target.Priority()
	})
	for i := range cands {
		cands[i].Score = prices[cands[i].Target.Name].usd
		cands[i].Reason = reasonFor(cands[i], "cost")
	}
	return cands
}

// orderByLatency sorts fastest observed first, unmeasured last.
func (r *Router) orderByLatency(cands []Candidate) []Candidate {
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Healthy != cands[j].Healthy {
			return cands[i].Healthy
		}
		a, b := r.latencyOf(cands[i].Target.Name), r.latencyOf(cands[j].Target.Name)
		if a != b {
			return a < b
		}
		return cands[i].Target.Priority() < cands[j].Target.Priority()
	})
	for i := range cands {
		cands[i].Score = float64(r.latencyOf(cands[i].Target.Name))
		cands[i].Reason = reasonFor(cands[i], "latency")
	}
	return cands
}

// orderByScore computes the normalised weighted sum.
//
// Every term is normalised across the candidate set to 0..1 before weighting,
// because the raw units are incompatible: USD per million tokens is ~1e-5 while
// a latency is ~1e-2 seconds. Without normalisation the cost term would be
// numerically invisible no matter what weight an operator wrote.
func (r *Router) orderByScore(cands []Candidate, model string) []Candidate {
	w := r.cfg.Weights
	n := len(cands)

	// The configured weights are normalised against their own total, so an
	// operator can write "cost: 3, latency: 1" without having to make them sum
	// to 1 — and so that raising one weight lowers the influence of the others
	// rather than pushing every score outside 0..1.
	total := w.Cost + w.Latency + w.Reliability + w.Priority
	if total <= 0 {
		total = 1
	}
	w.Cost, w.Latency, w.Reliability, w.Priority =
		w.Cost/total, w.Latency/total, w.Reliability/total, w.Priority/total

	costs := make([]float64, n)
	knownCost := make([]bool, n)
	latencies := make([]float64, n)
	knownLat := make([]bool, n)
	failures := make([]float64, n)

	for i, c := range cands {
		pc := r.priceOf(c.Model, model)
		costs[i], knownCost[i] = pc.usd, pc.known
		snap := r.snapshot(c.Target.Name)
		if snap != nil && snap.HasLatency {
			latencies[i], knownLat[i] = float64(snap.LatencyMean.Nanoseconds()), true
		}
		if snap != nil {
			failures[i] = snap.FailureRatio
		}
	}

	normCost := normalise(costs, knownCost, len(cands))
	normLat := normalise(latencies, knownLat, len(cands))

	// Reliability is a ratio already in 0..1, so it needs no normalisation and
	// deliberately keeps its absolute meaning: a backend failing 60% of
	// requests must score badly even if it is the only backend failing.
	for i := range cands {
		prio := float64(cands[i].Target.Priority())
		cands[i].Score = w.Cost*normCost[i] + w.Latency*normLat[i] +
			w.Reliability*failures[i] + w.Priority*prio
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Healthy != cands[j].Healthy {
			return cands[i].Healthy
		}
		if cands[i].Score != cands[j].Score {
			return cands[i].Score < cands[j].Score
		}
		return cands[i].Target.Priority() < cands[j].Target.Priority()
	})
	for i := range cands {
		cands[i].Reason = reasonFor(cands[i], "score")
	}
	return cands
}

// normalise maps values to 0..1 across the candidate set. Unmeasured values
// receive the neutral midpoint: treating them as 0 would make an unmeasured
// backend look perfect and starve the measured ones, and treating them as 1
// would remove it from rotation before it ever served a request.
func normalise(values []float64, known []bool, _ int) []float64 {
	out := make([]float64, len(values))
	min, max := 0.0, 0.0
	first := true
	for i, v := range values {
		if !known[i] {
			continue
		}
		if first || v < min {
			min = v
		}
		if first || v > max {
			max = v
		}
		first = false
	}
	span := max - min
	for i := range values {
		switch {
		case !known[i]:
			out[i] = 0.5
		case span <= 0:
			// All candidates are equal: no signal, so contribute nothing
			// rather than an arbitrary 1 for the first one in the slice.
			out[i] = 0
		default:
			out[i] = (values[i] - min) / span
		}
	}
	return out
}

// orderWeighted picks a random permutation proportional to weight.
//
// Sampling without replacement (rather than independent draws) is what makes
// the result a usable failover chain: every backend appears exactly once, so a
// weighted 90/10 split still has the 10% backend available as a fallback.
func (r *Router) orderWeighted(cands []Candidate) []Candidate {
	remaining := make([]Candidate, len(cands))
	copy(remaining, cands)
	out := make([]Candidate, 0, len(cands))

	for len(remaining) > 0 {
		total := 0.0
		for _, c := range remaining {
			total += c.Target.Weight()
		}
		pick := 0
		if total > 0 {
			roll := r.rand.Float64() * total
			acc := 0.0
			for i, c := range remaining {
				acc += c.Target.Weight()
				if roll < acc {
					pick = i
					break
				}
			}
		} else {
			pick = r.rand.Intn(len(remaining))
		}
		out = append(out, remaining[pick])
		remaining = append(remaining[:pick], remaining[pick+1:]...)
	}
	for i := range out {
		out[i].Reason = reasonFor(out[i], "weighted")
	}
	return out
}

// reasonFor renders a one-line explanation for the plan.
func reasonFor(c Candidate, strategy string) string {
	if !c.Healthy {
		return fmt.Sprintf("%s: circuit open, only used as a last resort", strategy)
	}
	if c.Target.Priority() == 0 {
		return strategy
	}
	return fmt.Sprintf("%s (priority %d)", strategy, c.Target.Priority())
}

// snapshot returns the windowed statistics for a backend, or nil when no
// breaker group was supplied.
func (r *Router) snapshot(name string) *stats.Snapshot {
	if r.breakers == nil {
		return nil
	}
	s := r.breakers.Get(name).Stats().Snapshot()
	return &s
}

// latencyOf returns the measured mean latency in nanoseconds, or a value that
// sorts unmeasured backends last.
func (r *Router) latencyOf(name string) time.Duration {
	if s := r.snapshot(name); s != nil && s.HasLatency {
		return s.LatencyMean
	}
	return time.Duration(1<<62 - 1)
}

type pricedCost struct {
	usd   float64
	known bool
}

// priceOf returns the blended cost for a candidate, falling back to the caller's
// model name when the backend serves it under a different name.
func (r *Router) priceOf(candidateModel, requestedModel string) pricedCost {
	if r.price == nil {
		return pricedCost{}
	}
	if usd, known := r.price(candidateModel); known {
		return pricedCost{usd: usd, known: true}
	}
	if candidateModel != requestedModel {
		if usd, known := r.price(requestedModel); known {
			return pricedCost{usd: usd, known: true}
		}
	}
	return pricedCost{}
}
