// Package evalset holds the labelled question pairs used to calibrate the
// semantic cache's similarity threshold.
//
// A threshold is a bet. Guess it too high and the cache never hits, so the
// feature is dead weight; guess it too low and the cache answers a different
// question than the one that was asked, which is worse than being slow because
// the caller cannot tell. The only way to place that bet honestly is to measure
// it against pairs whose correct verdict is known in advance, which is what
// this package is for.
//
// The corpus is deliberately balanced between pairs that MUST hit (identical
// and paraphrase) and pairs that MUST NOT (near-miss and unrelated). A corpus of
// only paraphrases would justify any threshold at all; the near-miss pairs
// (password/username, France/Germany, token bucket/leaky bucket, 北京/上海) are
// what push back and bound the threshold from below.
//
// It is also deliberately small and hand-written: these are pairs a human has
// ruled on, not model output, so a disagreement between the embedder and the
// label is a real disagreement and not a labelling artefact.
package evalset

import (
	"context"
	"fmt"
	"sort"

	"github.com/infergate/infergate/internal/embed"
)

// Kinds of pair, and their expected verdicts.
const (
	// KindIdentical is the same text twice: a hit is mandatory, and it is what
	// the exact-match path is for.
	KindIdentical = "identical"
	// KindParaphrase asks the same question in different words: a hit is the
	// entire point of semantic caching.
	KindParaphrase = "paraphrase"
	// KindNearMiss shares most of its vocabulary but asks for something else.
	// A hit here is a WRONG ANSWER, and it is the failure this calibration
	// exists to prevent.
	KindNearMiss = "near-miss"
	// KindUnrelated shares a topic at most.
	KindUnrelated = "unrelated"
)

// Case is one labelled pair.
type Case struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	A    string `json:"a"`
	B    string `json:"b"`
	// Note records why the label is what it is, so a later reader can disagree
	// with the label instead of having to reverse-engineer it.
	Note string `json:"note,omitempty"`
}

// ShouldHit reports whether the pair is expected to match at any threshold the
// cache would ship with.
func (c Case) ShouldHit() bool { return c.Kind == KindIdentical || c.Kind == KindParaphrase }

// CaseResult is one pair measured against one embedder.
type CaseResult struct {
	Case
	Similarity float64 `json:"similarity"`
	Hit        bool    `json:"hit"`
}

// Correct reports whether the pair matched its label at the threshold used.
func (r CaseResult) Correct() bool { return r.Hit == r.ShouldHit() }

// Cases returns the labelled corpus.
func Cases() []Case {
	return []Case{
		// ---------------------------------------------------------------
		// Identical: a hit is not a judgement call.
		// ---------------------------------------------------------------
		{ID: "id-en-1", Kind: KindIdentical,
			A: "how do I reset my password", B: "how do I reset my password",
			Note: "byte-identical, the exact-match path"},
		{ID: "id-en-2", Kind: KindIdentical,
			A:    "Explain the sliding window breaker used by the gateway, including how the half-open state is entered.",
			B:    "Explain the sliding window breaker used by the gateway, including how the half-open state is entered.",
			Note: "long text, so term-frequency weighting has something to do"},
		{ID: "id-zh-1", Kind: KindIdentical,
			A: "如何重置我的密码", B: "如何重置我的密码",
			Note: "CJK path: character bigrams, no whitespace to split on"},

		// ---------------------------------------------------------------
		// Paraphrase: the same intent in different words. These are the pairs
		// that set the UPPER bound on a usable threshold.
		// ---------------------------------------------------------------
		{ID: "pp-en-1", Kind: KindParaphrase,
			A: "how do I reset my password", B: "how can I reset my password",
			Note: "one inserted word"},
		{ID: "pp-en-2", Kind: KindParaphrase,
			A: "how do I reset my password for the admin console", B: "how can I reset my password for the admin console",
			Note: "one inserted word in a longer sentence"},
		{ID: "pp-en-3", Kind: KindParaphrase,
			A: "what is the capital of France", B: "what's the capital of France",
			Note: "contraction: hardest lexical case, 'is' becomes \"'s\""},
		{ID: "pp-en-4", Kind: KindParaphrase,
			A: "explain the token bucket algorithm", B: "can you explain the token bucket algorithm",
			Note: "polite prefix"},
		{ID: "pp-en-5", Kind: KindParaphrase,
			A: "summarise the design document", B: "please summarise the design document",
			Note: "leading politeness only"},
		{ID: "pp-en-6", Kind: KindParaphrase,
			A: "write a unit test for the router", B: "write a unit test for the router please",
			Note: "trailing politeness only"},
		{ID: "pp-en-7", Kind: KindParaphrase,
			A: "what does the failover header mean", B: "what does the failover header mean?",
			Note: "punctuation only"},
		{ID: "pp-en-8", Kind: KindParaphrase,
			A: "list the environment variables the gateway reads", B: "which environment variables does the gateway read",
			Note: "reordered question, same intent"},
		{ID: "pp-zh-1", Kind: KindParaphrase,
			A: "如何重置我的密码", B: "我该如何重置我的密码",
			Note: "CJK: inserted characters"},
		{ID: "pp-zh-2", Kind: KindParaphrase,
			A: "解释一下令牌桶算法", B: "请解释一下令牌桶算法",
			Note: "CJK: politeness prefix"},

		// ---------------------------------------------------------------
		// Near-miss: most of the vocabulary, a different request. A hit here is
		// a wrong answer. These set the LOWER bound.
		// ---------------------------------------------------------------
		{ID: "nm-en-1", Kind: KindNearMiss,
			A: "how do I reset my password", B: "how do I reset my username",
			Note: "one noun differs; the answer is a different procedure"},
		{ID: "nm-en-2", Kind: KindNearMiss,
			A: "how do I reset my password for the admin console", B: "how do I reset my password for the user console",
			Note: "one noun differs in a long shared frame - the hardest near-miss"},
		{ID: "nm-en-3", Kind: KindNearMiss,
			A: "what is the capital of France", B: "what is the capital of Germany",
			Note: "one country differs; the answer is a different city"},
		{ID: "nm-en-4", Kind: KindNearMiss,
			A: "explain the token bucket algorithm", B: "explain the leaky bucket algorithm",
			Note: "adjacent but distinct rate-limiting algorithms"},
		{ID: "nm-en-5", Kind: KindNearMiss,
			A: "summarise the design document", B: "summarise the deployment document",
			Note: "one noun differs"},
		{ID: "nm-en-6", Kind: KindNearMiss,
			A: "list the environment variables the gateway reads", B: "list the command line flags the gateway reads",
			Note: "one noun phrase differs"},
		{ID: "nm-en-7", Kind: KindNearMiss,
			A: "write a unit test for the router", B: "write a unit test for the breaker",
			Note: "one package differs"},
		{ID: "nm-en-8", Kind: KindNearMiss,
			A: "delete the cache entry for tenant alpha", B: "delete the cache entry for tenant beta",
			Note: "one identifier differs; acting on the wrong tenant is data loss"},
		{ID: "nm-zh-1", Kind: KindNearMiss,
			A: "如何重置我的密码", B: "如何重置我的邮箱",
			Note: "CJK: one noun differs"},
		{ID: "nm-zh-2", Kind: KindNearMiss,
			A: "北京有多少人口", B: "上海有多少人口",
			Note: "CJK: one city differs"},

		// ---------------------------------------------------------------
		// Unrelated: no honest threshold should match these.
		// ---------------------------------------------------------------
		{ID: "un-en-1", Kind: KindUnrelated,
			A: "how do I reset my password", B: "what is the weather in Beijing tomorrow",
			Note: "shares only 'my'/'the'"},
		{ID: "un-en-2", Kind: KindUnrelated,
			A: "explain the token bucket algorithm", B: "write a haiku about autumn",
			Note: "no shared content words"},
		{ID: "un-zh-1", Kind: KindUnrelated,
			A: "如何重置我的密码", B: "推荐几本科幻小说",
			Note: "CJK: no shared bigrams"},
	}
}

// ThresholdResult is the corpus measured at one threshold.
type ThresholdResult struct {
	Threshold float64 `json:"threshold"`
	// TrueHits counts pairs that should hit and did.
	TrueHits int `json:"true_hits"`
	// TrueTotal counts pairs that should hit.
	TrueTotal int `json:"true_total"`
	// FalseHits counts pairs that should NOT hit but did. This is the number
	// that decides whether a threshold is safe to ship.
	FalseHits int `json:"false_hits"`
	// FalseTotal counts pairs that should not hit.
	FalseTotal int `json:"false_total"`
	// MissedIDs names the pairs that should have hit but did not, so the
	// trade-off is inspectable rather than a single number.
	MissedIDs []string `json:"missed_ids,omitempty"`
	// FalseHitIDs names the pairs that were wrongly matched. These are the
	// wrong answers a caller would have received without knowing.
	FalseHitIDs []string `json:"false_hit_ids,omitempty"`
}

// HitRate is the share of should-hit pairs that hit.
func (t ThresholdResult) HitRate() float64 {
	if t.TrueTotal == 0 {
		return 0
	}
	return float64(t.TrueHits) / float64(t.TrueTotal)
}

// FalseHitRate is the share of should-not-hit pairs that hit.
func (t ThresholdResult) FalseHitRate() float64 {
	if t.FalseTotal == 0 {
		return 0
	}
	return float64(t.FalseHits) / float64(t.FalseTotal)
}

// Report is a full sweep.
type Report struct {
	Embedder   string            `json:"embedder"`
	Dims       int               `json:"dims"`
	Cases      int               `json:"cases"`
	Thresholds []ThresholdResult `json:"thresholds"`
}

// Measure scores every case against an embedder and reports the raw
// similarities, sorted so the near-misses and the paraphrases land next to each
// other - the ordering IS the finding: if a near-miss sits above a paraphrase,
// no threshold can separate them.
func Measure(ctx context.Context, e embed.Embedder, cs []Case) ([]CaseResult, error) {
	if len(cs) == 0 {
		cs = Cases()
	}
	out := make([]CaseResult, 0, len(cs))
	for _, c := range cs {
		vecs, err := e.Embed(ctx, []string{c.A, c.B})
		if err != nil {
			return nil, fmt.Errorf("evalset: embed %s: %w", c.ID, err)
		}
		if len(vecs) != 2 {
			return nil, fmt.Errorf("evalset: embed %s: want 2 vectors, got %d", c.ID, len(vecs))
		}
		out = append(out, CaseResult{
			Case:       c,
			Similarity: embed.Cosine(vecs[0], vecs[1]),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Sweep measures the corpus at every threshold and returns the trade-off curve.
// A threshold is inclusive: similarity >= threshold hits.
func Sweep(ctx context.Context, e embed.Embedder, thresholds []float64, cs []Case) (Report, []CaseResult, error) {
	results, err := Measure(ctx, e, cs)
	if err != nil {
		return Report{}, nil, err
	}
	rep := Report{
		Embedder: e.Name(),
		Dims:     e.Dims(),
		Cases:    len(results),
	}
	for _, th := range thresholds {
		tr := ThresholdResult{Threshold: th}
		for _, r := range results {
			hit := r.Similarity >= th
			if r.ShouldHit() {
				tr.TrueTotal++
				if hit {
					tr.TrueHits++
				} else {
					tr.MissedIDs = append(tr.MissedIDs, r.ID)
				}
				continue
			}
			tr.FalseTotal++
			if hit {
				tr.FalseHits++
				tr.FalseHitIDs = append(tr.FalseHitIDs, r.ID)
			}
		}
		rep.Thresholds = append(rep.Thresholds, tr)
	}
	return rep, results, nil
}

// Recommend picks the highest-hit-rate threshold whose false-hit count stays
// within budget. Safety wins over hit rate on ties, because a miss costs money
// and a wrong answer costs trust.
//
// It returns false when no swept threshold satisfies the budget, which is a real
// possible outcome and one the caller must not paper over: it means this
// embedder cannot separate the corpus, and the honest response is to say so
// rather than to ship a threshold that trades wrong answers for a nicer number.
func Recommend(rep Report, maxFalseHits int) (ThresholdResult, bool) {
	var best ThresholdResult
	found := false
	for _, tr := range rep.Thresholds {
		if tr.FalseHits > maxFalseHits {
			continue
		}
		if !found || tr.TrueHits > best.TrueHits ||
			(tr.TrueHits == best.TrueHits && tr.Threshold > best.Threshold) {
			best, found = tr, true
		}
	}
	return best, found
}

// DefaultThresholds is the grid the measurement scripts sweep: fine enough
// around the 0.8-0.95 region where a lexical embedder actually decides.
func DefaultThresholds() []float64 {
	return []float64{
		0.60, 0.65, 0.70, 0.75, 0.80, 0.82, 0.84, 0.86, 0.88,
		0.90, 0.91, 0.92, 0.93, 0.94, 0.95, 0.96, 0.97, 0.98, 0.99,
	}
}
