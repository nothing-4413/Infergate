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
// A false second return means the default price applied, which is worth
// surfacing in logs: unlisted models are the ones whose cost is a guess.
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
