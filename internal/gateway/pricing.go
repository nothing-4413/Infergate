package gateway

import (
	"strings"

	"github.com/infergate/infergate/internal/config"
)

// pricingUnitTokens is the denominator for the price book. Providers quote
// prices per million tokens; keeping that convention here means the numbers in
// infergate.yaml can be pasted straight from a provider's pricing page with no
// unit conversion to get wrong.
const pricingUnitTokens = 1_000_000

// PriceBook converts token counts into USD using configured per-model prices.
//
// It is read-only after construction, so no locking is needed on the request
// path.
type PriceBook struct {
	defaultPrice config.ModelPrice
	models       map[string]config.ModelPrice
}

// NewPriceBook builds a PriceBook from configuration.
//
// Model keys are lower-cased for lookup because providers are inconsistent
// about case in the model name they report back; a case mismatch would
// otherwise silently fall through to the default price and misreport spend.
func NewPriceBook(pc config.PricingConfig) *PriceBook {
	models := make(map[string]config.ModelPrice, len(pc.Models))
	for name, price := range pc.Models {
		models[strings.ToLower(strings.TrimSpace(name))] = price
	}
	return &PriceBook{defaultPrice: pc.Default, models: models}
}

// Price returns the price entry for model and whether it was an explicit match.
// A false second return means the default price applied: unlisted models are the
// ones whose cost is a guess. Unpriced exists so that fact can be said out loud
// once at startup instead of per request.
func (p *PriceBook) Price(model string) (config.ModelPrice, bool) {
	if p == nil {
		return config.ModelPrice{}, false
	}
	key := strings.ToLower(strings.TrimSpace(model))
	// Strip a provider route prefix such as "openai/gpt-4o" so that a gateway
	// header naming the backend does not change which price applies.
	if i := strings.LastIndex(key, "/"); i >= 0 && i+1 < len(key) {
		if price, ok := p.models[key[i+1:]]; ok {
			return price, true
		}
	}
	if price, ok := p.models[key]; ok {
		return price, true
	}
	return p.defaultPrice, false
}

// Unpriced returns the models from the given list that have no explicit entry
// in the book, in the order given, with duplicates and the "/" catch-all
// dropped. Those are precisely the models CostUSD multiplies by the default
// price.
//
// It exists because the two consumers of a price disagree about what an
// unlisted model means. The router sorts it last, so a forgotten price line can
// never make a backend look free and therefore preferred (see
// internal/router's cost strategy); CostUSD has no such guard, so the same
// forgotten line makes that model's traffic cost the default price -- which is
// $0 in a configuration that never set one, and thus invisible to a cost
// budget. Reporting the list once at startup is the only place the second half
// can be said without putting a configuration gap on the request path as log
// noise.
func (p *PriceBook) Unpriced(models []string) []string {
	var out []string
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if m == "" || m == "/" {
			continue
		}
		if _, ok := p.Price(m); ok {
			continue
		}
		// Case and surrounding space do not make a second model: Price folds
		// both, so a duplicate that differs only there would otherwise be
		// reported twice.
		key := strings.ToLower(strings.TrimSpace(m))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}

// CostUSD returns the USD cost of a response.
//
// Cached prompt tokens are billed at the input rate here. Providers that
// discount them report the discount separately; overstating cost slightly is
// the safe direction for a budget gate, because it trips the budget before the
// real spend exceeds it.
func (p *PriceBook) CostUSD(model string, prompt, completion int) float64 {
	price, _ := p.Price(model)
	return (float64(prompt)*price.In + float64(completion)*price.Out) / pricingUnitTokens
}
