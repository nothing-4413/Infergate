package evalset_test

import (
	"context"
	"testing"

	"github.com/infergate/infergate/internal/cache"
	"github.com/infergate/infergate/internal/embed"
	"github.com/infergate/infergate/internal/evalset"
)

// The corpus is the evidence behind cache.DefaultThreshold, so its integrity is
// itself worth a test: a corpus that quietly lost its near-miss pairs would
// justify any threshold at all.
func TestCorpusIsWellFormed(t *testing.T) {
	cases := evalset.Cases()
	if len(cases) < 20 {
		t.Fatalf("corpus has %d pairs; a small corpus makes the calibration arbitrary", len(cases))
	}
	kinds := map[string]int{}
	ids := map[string]bool{}
	for _, c := range cases {
		kinds[c.Kind]++
		if ids[c.ID] {
			t.Fatalf("duplicate case id %q", c.ID)
		}
		ids[c.ID] = true
		if c.A == "" || c.B == "" {
			t.Fatalf("case %s has an empty side", c.ID)
		}
		if c.Note == "" {
			t.Fatalf("case %s has no note explaining its label", c.ID)
		}
		want := c.Kind == evalset.KindIdentical || c.Kind == evalset.KindParaphrase
		if c.ShouldHit() != want {
			t.Fatalf("case %s (%s): ShouldHit()=%v", c.ID, c.Kind, c.ShouldHit())
		}
	}
	for _, k := range []string{evalset.KindIdentical, evalset.KindParaphrase, evalset.KindNearMiss, evalset.KindUnrelated} {
		if kinds[k] == 0 {
			t.Fatalf("corpus has no %s pairs", k)
		}
	}
	// Near-misses and paraphrases must be comparably represented: a corpus with
	// three near-misses and thirty paraphrases would push the threshold down.
	if kinds[evalset.KindNearMiss] < 8 {
		t.Fatalf("only %d near-miss pairs; the lower bound on the threshold is too weak", kinds[evalset.KindNearMiss])
	}
	if kinds[evalset.KindParaphrase] < 8 {
		t.Fatalf("only %d paraphrase pairs; the upper bound on the threshold is too weak", kinds[evalset.KindParaphrase])
	}
}

func sweep(t *testing.T, e embed.Embedder) evalset.Report {
	t.Helper()
	rep, _, err := evalset.Sweep(context.Background(), e, evalset.DefaultThresholds(), nil)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return rep
}

// Raising the threshold can only ever turn hits into misses. If this fails, the
// comparison in Sweep (or in the stores) is not a simple >= gate.
func TestCurveIsMonotonic(t *testing.T) {
	rep := sweep(t, embed.NewHashingEmbedder(embed.DefaultHashingDims))
	if len(rep.Thresholds) < 2 {
		t.Fatal("not enough thresholds swept")
	}
	for i := 1; i < len(rep.Thresholds); i++ {
		prev, cur := rep.Thresholds[i-1], rep.Thresholds[i]
		if cur.Threshold <= prev.Threshold {
			t.Fatalf("thresholds are not ascending: %v then %v", prev.Threshold, cur.Threshold)
		}
		if cur.TrueHits > prev.TrueHits {
			t.Fatalf("true hits rose from %d to %d when the threshold rose %.2f -> %.2f",
				prev.TrueHits, cur.TrueHits, prev.Threshold, cur.Threshold)
		}
		if cur.FalseHits > prev.FalseHits {
			t.Fatalf("false hits rose from %d to %d when the threshold rose %.2f -> %.2f",
				prev.FalseHits, cur.FalseHits, prev.Threshold, cur.Threshold)
		}
	}
}

// The shipped default must be the measured one. This is the gate that stops a
// future edit from lowering the threshold for a nicer hit rate: the failure
// message names the wrong answers that would be served.
func TestShippedThresholdHasNoFalseHits(t *testing.T) {
	rep := sweep(t, embed.NewHashingEmbedder(embed.DefaultHashingDims))
	at := find(t, rep, cache.DefaultThreshold)

	if at.FalseHits != 0 {
		t.Fatalf("cache.DefaultThreshold = %.2f serves %d wrong answers on the corpus: %v",
			at.Threshold, at.FalseHits, at.FalseHitIDs)
	}
	if at.TrueHits < 8 {
		t.Fatalf("cache.DefaultThreshold = %.2f only hits %d/%d paraphrases; it is too strict to be worth shipping",
			at.Threshold, at.TrueHits, at.TrueTotal)
	}

	// The trade-off the DefaultThreshold comment describes must be real: a
	// little lower and the corpus starts answering the wrong question. If this
	// ever stops being true, the comment is stale and the default should be
	// re-measured rather than silently kept.
	lower := find(t, rep, 0.84)
	if lower.FalseHits == 0 {
		t.Skipf("at %.2f the corpus no longer produces false hits; re-measure and lower the default", lower.Threshold)
	}
	t.Logf("at %.2f the threshold would wrongly match %v", lower.Threshold, lower.FalseHitIDs)
}

// A lexical embedder cannot separate every paraphrase from every near-miss. That
// is the structural reason the offline default is not good enough on its own, and
// it is worth asserting so nobody spends an afternoon tuning a threshold to fix
// it: the fix is a real embedding model, not a number.
func TestLexicalEmbedderCannotSeparateEverything(t *testing.T) {
	cases, err := evalset.Measure(context.Background(), embed.NewHashingEmbedder(embed.DefaultHashingDims), nil)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}

	var worstParaphrase, nearestNearMiss float64 = 1, 0
	for _, c := range cases {
		switch c.Kind {
		case evalset.KindParaphrase:
			if c.Similarity < worstParaphrase {
				worstParaphrase = c.Similarity
			}
		case evalset.KindNearMiss:
			if c.Similarity > nearestNearMiss {
				nearestNearMiss = c.Similarity
			}
		}
	}
	if nearestNearMiss < worstParaphrase {
		t.Skip("this embedder separates the corpus; the offline default could be raised")
	}
	t.Logf("a near-miss (%.4f) outscores a paraphrase (%.4f): no threshold separates them with this embedder",
		nearestNearMiss, worstParaphrase)
}

func find(t *testing.T, rep evalset.Report, threshold float64) evalset.ThresholdResult {
	t.Helper()
	for _, tr := range rep.Thresholds {
		if tr.Threshold == threshold {
			return tr
		}
	}
	t.Fatalf("threshold %.2f is not in the swept grid %v", threshold, evalset.DefaultThresholds())
	return evalset.ThresholdResult{}
}
