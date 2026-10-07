package repofmt

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/infergate/infergate/internal/config"
)

// TestM4RecordPricesMatchTheShippedConfig holds the record's price block to the
// config it was copied from.
//
// The tiering cost paragraph in docs/DESIGN.md quotes the list price it priced
// the mix with (in 0.27 / out 1.1 per 1e6 tokens). scripts/measure-m4.ps1 does
// not invent those numbers: Get-PricingFromConfig parses them out of
// configs/tiered-local.yaml at measure time and writes them into
// tiering.cost.prices_usd_per_1e6_tokens. That makes the sentence, the record and
// the shipped config three copies of one number. The claim chain holds the first
// two together; this holds the third, which nothing was watching: editing the
// config's price would leave the record and the prose describing a run nobody can
// reproduce, and no gate would say so.
func TestM4RecordPricesMatchTheShippedConfig(t *testing.T) {
	root := repoRoot(t)

	cfg, err := config.Load(filepath.Join(root, "configs", "tiered-local.yaml"))
	if err != nil {
		t.Fatalf("configs/tiered-local.yaml does not load: %v", err)
	}

	var record struct {
		Tiering struct {
			Cost struct {
				Prices map[string]config.ModelPrice `json:"prices_usd_per_1e6_tokens"`
			} `json:"cost"`
		} `json:"tiering"`
	}
	raw := readFile(t, filepath.Join(root, "docs", "baseline", "m4-summary.json"))
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		t.Fatalf("docs/baseline/m4-summary.json is not JSON: %v", err)
	}
	prices := record.Tiering.Cost.Prices
	if len(prices) == 0 {
		t.Fatal("m4-summary.json carries no tiering.cost.prices_usd_per_1e6_tokens block; the record moved")
	}

	if got, want := prices["default"], cfg.Pricing.Default; got != want {
		t.Errorf("m4-summary.json prices the default model at %+v, but configs/tiered-local.yaml declares %+v", got, want)
	}
	for name, want := range cfg.Pricing.Models {
		got, ok := prices[name]
		if !ok {
			t.Errorf("configs/tiered-local.yaml prices %q at %+v, but m4-summary.json carries no entry for it", name, want)
			continue
		}
		if got != want {
			t.Errorf("m4-summary.json prices %q at %+v, but configs/tiered-local.yaml declares %+v", name, got, want)
		}
	}
	for name := range prices {
		if name == "default" {
			continue
		}
		if _, ok := cfg.Pricing.Models[name]; !ok {
			t.Errorf("m4-summary.json prices %q, but configs/tiered-local.yaml no longer declares that model", name)
		}
	}
}
