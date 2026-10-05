// Command measure-m2 measures the semantic cache's threshold, and later its
// hit rate under load against a running gateway.
//
// Two things are measured, for two different reasons:
//
//   - the THRESHOLD SWEEP runs the labelled corpus in internal/evalset against
//     an embedder and reports, for every candidate threshold, what share of
//     paraphrase pairs would hit and what share of near-miss pairs would be
//     answered with the WRONG cached response. The second number is the one
//     that decides the default; a threshold is not a tuning knob, it is a
//     correctness/benefit trade and it needs evidence.
//   - the LOAD-CLIENT phase (with -url) replays a paraphrase workload against a
//     live gateway and reports the hit rate a real caller sees, including the
//     traffic that arrives while the cache is still cold.
//
// The sweep does not need a gateway, a Redis, or a network: it is pure
// arithmetic over the corpus, which is why it can be the gate that fails a
// build when the default threshold stops being justified.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/embed"
	"github.com/infergate/infergate/internal/evalset"
)

func main() {
	var (
		dims       = flag.Int("dims", embed.DefaultHashingDims, "dims for the offline hashing embedder")
		httpURL    = flag.String("http", "", "use an HTTP embedding endpoint instead of the offline embedder")
		httpModel  = flag.String("http-model", "text-embedding-3-small", "model name for -http")
		httpKey    = flag.String("http-key", "", "API key for -http")
		httpTO     = flag.Duration("http-timeout", 5*time.Second, "timeout for -http")
		thresholds = flag.String("thresholds", "", "comma-separated thresholds (default: the evalset grid)")
		asJSON     = flag.Bool("json", false, "write the report as JSON")
		out        = flag.String("out", "", "write the JSON report to this file")
		pairs      = flag.Bool("pairs", false, "print every pair's similarity, sorted")
		maxFalse   = flag.Int("max-false-hits", 0, "false hits allowed when recommending a threshold")
	)
	flag.Parse()

	ctx := context.Background()

	var emb embed.Embedder
	switch {
	case *httpURL != "":
		h, err := embed.NewHTTPEmbedder(embed.HTTPOptions{
			BaseURL: *httpURL, Model: *httpModel, APIKey: *httpKey, Timeout: *httpTO,
		})
		if err != nil {
			fatal(err)
		}
		emb = h
	default:
		emb = embed.NewHashingEmbedder(*dims)
	}

	grid := evalset.DefaultThresholds()
	if *thresholds != "" {
		grid = nil
		for _, s := range strings.Split(*thresholds, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			var f float64
			if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
				fatal(fmt.Errorf("bad threshold %q: %w", s, err))
			}
			grid = append(grid, f)
		}
	}

	rep, cases, err := evalset.Sweep(ctx, emb, grid, nil)
	if err != nil {
		fatal(err)
	}
	rep.Embedder = fmt.Sprintf("%s(dims=%d)", emb.Name(), emb.Dims())

	if *pairs {
		fmt.Printf("embedder %s, dims %d, %d labelled pairs\n\n", emb.Name(), emb.Dims(), len(cases))
		fmt.Printf("%-6s %-11s %-8s %s\n", "sim", "verdict", "want", "pair")
		for _, c := range cases {
			want := "miss"
			if c.ShouldHit() {
				want = "HIT"
			}
			fmt.Printf("%-6.4f %-11s %-8s %s | %s\n", c.Similarity, c.Kind, want, truncate(c.A, 40), truncate(c.B, 40))
		}
		fmt.Println()
	}

	printCurve(rep)

	if rec, ok := evalset.Recommend(rep, *maxFalse); ok {
		fmt.Printf("\nrecommended threshold %.2f: hit rate %.0f%% (%d/%d), false-hit rate %.0f%% (%d/%d)\n",
			rec.Threshold, rec.HitRate()*100, rec.TrueHits, rec.TrueTotal,
			rec.FalseHitRate()*100, rec.FalseHits, rec.FalseTotal)
		if len(rec.FalseHitIDs) > 0 {
			fmt.Printf("  false hits: %s\n", strings.Join(rec.FalseHitIDs, ", "))
		}
	} else {
		fmt.Printf("\nNO threshold in the swept grid keeps false hits within %d: this embedder cannot separate the corpus\n", *maxFalse)
	}

	if *out != "" {
		if err := writeJSON(*out, rep); err != nil {
			fatal(err)
		}
		fmt.Printf("\nwrote %s\n", *out)
	} else if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fatal(err)
		}
	}
}

func printCurve(rep evalset.Report) {
	fmt.Printf("%-9s %-14s %-14s %s\n", "thresh", "hit rate", "false-hit rate", "false hits")
	for _, t := range rep.Thresholds {
		fmt.Printf("%-9.2f %-14s %-14s %s\n",
			t.Threshold,
			fmt.Sprintf("%.0f%% (%d/%d)", t.HitRate()*100, t.TrueHits, t.TrueTotal),
			fmt.Sprintf("%.0f%% (%d/%d)", t.FalseHitRate()*100, t.FalseHits, t.FalseTotal),
			strings.Join(t.FalseHitIDs, ","))
	}
	fmt.Printf("\n%d pairs: %d should hit, %d should not\n",
		rep.Cases, rep.Thresholds[0].TrueTotal, rep.Thresholds[0].FalseTotal)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "measure-m2: %v\n", err)
	os.Exit(1)
}
