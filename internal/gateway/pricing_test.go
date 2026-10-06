package gateway

import (
	"reflect"
	"testing"

	"github.com/infergate/infergate/internal/config"
)

// TestPriceBookUnpriced pins the list a startup uses to say which models it
// cannot price. The property that matters is that it agrees with Price: a model
// is reported here exactly when Price falls back to the default, because that
// fallback is what makes the cost a guess (or, with a zero default, a silent
// $0).
func TestPriceBookUnpriced(t *testing.T) {
	book := NewPriceBook(config.PricingConfig{
		Default: config.ModelPrice{In: 1, Out: 2},
		Models: map[string]config.ModelPrice{
			"gpt-4o":      {In: 2.5, Out: 10},
			"bge-m3":      {In: 0.1, Out: 0},
			"local/llama": {In: 0, Out: 0},
		},
	})

	cases := []struct {
		name   string
		models []string
		want   []string
	}{
		{
			name:   "a listed model is not reported",
			models: []string{"gpt-4o", "bge-m3"},
		},
		{
			// Price folds case and strips a provider prefix, so these are
			// matches. Reporting them would send the operator looking for a
			// price line that is already there.
			name:   "case and provider prefix still match",
			models: []string{"OpenAI/gpt-4o", "BGE-M3", " gpt-4o "},
		},
		{
			name:   "a prefixed name can be listed in full",
			models: []string{"local/llama"},
		},
		{
			name:   "unlisted models keep the caller's order",
			models: []string{"mystery", "gpt-4o", "other"},
			want:   []string{"mystery", "other"},
		},
		{
			// Two entries that Price folds to the same model are one model, not
			// two, or the warning would name it twice.
			name:   "folding duplicates are reported once",
			models: []string{"Mystery", " mystery ", "mystery"},
			want:   []string{"Mystery"},
		},
		{
			name:   "the catch-all and the empty string name nothing",
			models: []string{"/", "", "mystery"},
			want:   []string{"mystery"},
		},
		{
			name:   "nothing declared, nothing reported",
			models: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := book.Unpriced(tc.models)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Unpriced(%q) = %q, want %q", tc.models, got, tc.want)
			}
		})
	}
}

// TestPriceBookUnpricedOfNilBook keeps the helper safe on the path where a
// server was built without a price book: every model is unlisted, and saying so
// is better than panicking.
func TestPriceBookUnpricedOfNilBook(t *testing.T) {
	var book *PriceBook
	if got := book.Unpriced([]string{"anything", "/"}); !reflect.DeepEqual(got, []string{"anything"}) {
		t.Fatalf("nil book Unpriced = %q, want [anything]", got)
	}
}
