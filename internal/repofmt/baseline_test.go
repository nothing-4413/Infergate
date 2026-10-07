package repofmt

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestHeadlineClaimsMatchTheBaseline pins the numbers README.md leads with
// against the measured records they come from.
//
// WHY IT EXISTS. The first screen of the README invites the reader to check
// seven quantified claims, and docs/baseline/ is the check: those files are the
// record of runs that already happened, exempt from every other prose rewrite
// here for exactly that reason. Nothing tied the two together, so a number
// re-typed from 58.97 to 58.9, or a summary regenerated with a different verdict,
// would leave the README asserting what the record no longer says -- and the
// record is the only reason to believe the sentence.
//
// WHAT IT CHECKS. Each entry reads one number out of a named baseline file and
// requires the string a document prints to be that number formatted the way that
// document formats it (the scale and the number of decimals are part of the entry,
// because "58.97%" is stored as 0.5897 and "2.42x" as 2.423). An entry names the
// documents that have to print it -- README.md when it names none -- because the
// same numbers carry the first screen of the README and the one-page RESUME, and
// the two are edited at different times by whoever is in a hurry. A path may start
// with "*", which searches the whole file for that key: it succeeds only when the
// key carries the same value everywhere it appears, so a second copy with a
// different number is a failure rather than a coin flip. When a key is not unique
// by name alone (m4-summary.json has one ratio per variant, m2-summary.json a
// hit_rate per sweep step) the entry names the object it means with a marker, so
// adding a variant or a step cannot silently move which number is read. An entry
// may also name several files and ask for their sum, which is how a total spread
// over the three load runs is read.
//
// WHAT IT DOES NOT CHECK. Whether the run was any good, or whether the baseline
// still describes today's code: a baseline is one afternoon's measurement on one
// machine, and the prose says so. This only settles that the sentence and the
// record are about the same number. Nor does it follow a number nobody quotes: a
// reading that only lives in the JSON is the JSON's business.
func TestHeadlineClaimsMatchTheBaseline(t *testing.T) {
	root := repoRoot(t)
	readme := readFile(t, filepath.Join(root, "README.md"))

	// The numbers below lead the README and the RESUME alike; the ones the RESUME
	// does not carry are the README's alone, and the measurements of the local
	// inference story are the RESUME's alone.
	readmeAndResume := []claimPrint{{}, {file: "docs/RESUME.md"}}
	resumeOnly := []claimPrint{{file: "docs/RESUME.md"}}

	claims := []baselineClaim{
		{
			quote: "420.42", file: "m1-summary.json", docs: readmeAndResume,
			find: map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"},
			path: []string{"median_ms_while_breaker_closed"}, digits: 2,
		},
		{
			quote: "17.81", file: "m1-summary.json", docs: readmeAndResume,
			find: map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"},
			path: []string{"median_ms_once_breaker_open"}, digits: 2,
		},
		{
			quote: "58.97", file: "m2-summary.json", docs: readmeAndResume,
			path: []string{"hit_rate", "hit_rate"}, scale: 100, digits: 2,
		},
		{
			quote: "56.39", file: "m2-summary.json", docs: readmeAndResume,
			path: []string{"*", "reduction_percent"}, digits: 2,
		},
		{
			// The cached path and the uncached one, as the M2 row of the README
			// table and the cache paragraph of the RESUME both quote them.
			quote: "5.50", file: "m2-summary.json", docs: readmeAndResume,
			path: []string{"latency", "gateway_stats", "cache_on_pure_hit", "stats", "p95_ms"}, digits: 2,
		},
		{
			quote: "47.31", file: "m2-summary.json", docs: readmeAndResume,
			path: []string{"latency", "gateway_stats", "cache_off_pure_miss", "stats", "p95_ms"}, digits: 2,
		},
		{
			quote: "100.7", file: "m4-summary.json", docs: readmeAndResume,
			find: map[string]any{"variant": "awq"},
			path: []string{"output_tokens_per_s", "variant"}, digits: 1,
		},
		{
			quote: "2.42", file: "m4-summary.json", docs: readmeAndResume,
			find: map[string]any{"variant": "awq"},
			path: []string{"output_tokens_per_s", "ratio"}, digits: 2,
		},
		{
			quote: "34.52", file: "m4-summary.json", docs: readmeAndResume,
			path: []string{"*", "avoided_percent"}, digits: 2,
		},
		{
			// The RESUME tells the local-inference story as an end-to-end P50 that
			// fell from the fp16 run to the quantized one; the README does not.
			quote: "1758", phrase: "1758ms", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "fp16"},
			path: []string{"total_ms", "p50"}, digits: 0,
		},
		{
			quote: "700", phrase: "700ms", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "awq"},
			path: []string{"total_ms", "p50"}, digits: 0,
		},
		{
			quote: "59400", file: "m5-summary.json", docs: readmeAndResume,
			path: []string{"*", "otlp_exported"}, digits: 0,
		},
		{
			quote: "22.07", file: "m5-summary.json", docs: readmeAndResume,
			find: map[string]any{"workload": "non-stream", "concurrency": 128.0},
			path: []string{"direct_minus_gateway_pct"}, digits: 2,
		},
		{
			// The idempotency payoff of the RESUME is B_replay_payoff in the M6
			// record: fresh generation against the replayed answer over 20 pairs.
			// The fresh column is paced by the mock, which the RESUME says out
			// loud next to the ratio, so the ratio is quoted rather than sold.
			quote: "5.7", phrase: "5.7ms", file: "m6-summary.json", docs: resumeOnly,
			path: []string{"*", "fresh_median_ms"}, digits: 1,
		},
		{
			quote: "8.1", phrase: "8.1ms", file: "m6-summary.json", docs: resumeOnly,
			path: []string{"*", "fresh_p95_ms"}, digits: 1,
		},
		{
			quote: "4.5", phrase: "4.5ms", file: "m6-summary.json", docs: resumeOnly,
			path: []string{"*", "replay_median_ms"}, digits: 1,
		},
		{
			quote: "24.9", phrase: "24.9ms", file: "m6-summary.json", docs: resumeOnly,
			path: []string{"*", "replay_p95_ms"}, digits: 1,
		},
		{
			quote: "1.267", file: "m6-summary.json", docs: resumeOnly,
			path: []string{"*", "latency_ratio_fresh_over_replay"}, digits: 3,
		},
		{
			quote: "13", phrase: "13 对近似语料", file: "m2-corpus.json",
			path: []string{"should_hit"}, digits: 0,
		},
		{
			quote: "0", phrase: "0 误命中", file: "m2-summary.json",
			find: map[string]any{"should_not_hit_requests": 26.0},
			path: []string{"false_hits"}, digits: 0,
		},
		{
			quote: "16", phrase: "16 个请求", file: "m4-summary.json",
			path: []string{"tiering", "mix", "requests_total"}, digits: 0,
		},
		{
			quote: "20", phrase: "20/20", file: "m3-summary.json",
			path: []string{"fail_closed", "requests_during_outage"}, digits: 0,
		},
		{
			quote: "20", phrase: "20/20", file: "m3-summary.json",
			path: []string{"fail_closed", "outage_status_codes", "503"}, digits: 0,
		},
		{
			quote: "0", phrase: "上游 0 调用", file: "m3-summary.json",
			path: []string{"fail_closed", "upstream_calls_during_outage"}, digits: 0,
		},
		{
			// The two documents word this one differently: the README counts
			// requests, the RESUME just says how many there were.
			quote: "18000", file: "m1-summary.json", sum: true,
			docs: []claimPrint{
				{phrase: "18000 个请求"},
				{file: "docs/RESUME.md", phrase: "18000 请求"},
			},
			files: []string{"m1-load-faulted-r1.json", "m1-load-faulted-r2.json", "m1-load-faulted-r3.json"},
			path:  []string{"*", "requests"}, digits: 0,
		},
		{
			quote: "0", phrase: "0 错误", sum: true,
			files: []string{"m1-load-faulted-r1.json", "m1-load-faulted-r2.json", "m1-load-faulted-r3.json"},
			path:  []string{"*", "errors"}, digits: 0,
		},
	}
	if len(claims) < 24 {
		t.Fatalf("only %d claims; this check has been hollowed out", len(claims))
	}

	parsed := map[string]any{}
	docFor := func(name string) any {
		doc, ok := parsed[name]
		if !ok {
			doc = parseBaseline(t, root, name)
			parsed[name] = doc
		}
		return doc
	}

	texts := map[string]string{"README.md": readme}
	textOf := func(name string) string {
		text, ok := texts[name]
		if !ok {
			text = readFile(t, filepath.Join(root, name))
			texts[name] = text
		}
		return text
	}

	for _, claim := range claims {
		names := claim.files
		if len(names) == 0 {
			names = []string{claim.file}
		}

		var values []float64
		for _, name := range names {
			found := numbersAt(docFor(name), claim.path)
			if len(claim.find) > 0 {
				found = nil
				for _, object := range findObjects(docFor(name), claim.find) {
					found = append(found, numbersAt(object, claim.path)...)
				}
			}
			if len(found) == 0 {
				t.Errorf("docs/baseline/%s has no value at %s%s; the record moved",
					name, marker(claim.find), strings.Join(claim.path, "."))
			}
			values = append(values, found...)
		}
		if len(values) == 0 {
			continue
		}

		total := values[0]
		if claim.sum {
			total = 0
			for _, value := range values {
				total += value
			}
		} else {
			distinct := map[float64]bool{}
			for _, value := range values {
				distinct[value] = true
			}
			if len(distinct) > 1 {
				t.Errorf("docs/baseline/%s carries %d different values for %s%s; name the one this claim means",
					strings.Join(names, ", "), len(distinct), marker(claim.find), strings.Join(claim.path, "."))
				continue
			}
		}

		factor := claim.scale
		if factor == 0 {
			factor = 1
		}
		got := strconv.FormatFloat(total*factor, 'f', claim.digits, 64)
		if got != claim.quote {
			t.Errorf("docs/baseline/%s%s reads %s, but %s prints %s",
				strings.Join(names, ", "), marker(claim.find), got,
				strings.Join(claimDocs(claim), " and "), claim.quote)
		}
		for _, want := range claim.prints() {
			doc, phrase := want.document(), want.wording(claim)
			if !strings.Contains(textOf(doc), phrase) {
				t.Errorf("%s no longer prints %s anywhere; if the claim moved, move this entry too", doc, phrase)
			}
		}
	}

	// The 23.6x in the M1 bullet is arithmetic on the two readings above rather
	// than a field of its own, so it is computed here from the same record.
	closed := singleNumber(t, docFor("m1-summary.json"),
		map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"}, []string{"median_ms_while_breaker_closed"})
	open := singleNumber(t, docFor("m1-summary.json"),
		map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"}, []string{"median_ms_once_breaker_open"})
	factor := strconv.FormatFloat(closed/open, 'f', 1, 64)
	if factor != "23.6" {
		t.Errorf("m1-summary.json's two medians give %s, but the prose prints 23.6", factor)
	}
	for _, doc := range readmeAndResume {
		if !strings.Contains(textOf(doc.document()), factor+"×") {
			t.Errorf("%s does not print %s next to the two medians", doc.document(), factor+"×")
		}
	}

	// "16 requests split 8:8" is a total plus how it divides, so the split is
	// checked as a shape rather than as one more number.
	mix := docFor("m4-summary.json")
	simple := singleNumber(t, mix, nil, []string{"tiering", "mix", "simple_per_kind"})
	hard := singleNumber(t, mix, nil, []string{"tiering", "mix", "hard_per_kind"})
	split := strconv.FormatFloat(simple, 'f', 0, 64) + ":" + strconv.FormatFloat(hard, 'f', 0, 64)
	if simple != hard {
		t.Errorf("m4-summary.json splits %v simple and %v hard requests, so it is not the even split README.md prints",
			simple, hard)
	} else if !strings.Contains(readme, split) {
		t.Errorf("m4-summary.json's split is %s, but README.md does not print it", split)
	}

	t.Logf("checked %d headline claims against docs/baseline", len(claims)+2)
}

// baselineClaim is one number the prose prints, and where its record keeps it.
type baselineClaim struct {
	quote  string         // exactly what the document prints, already scaled and rounded
	file   string         // file under docs/baseline/
	files  []string       // several records, when the claim is their total
	sum    bool           // add the values instead of requiring them to agree
	phrase string         // wording that carries the number; the quote itself when empty
	docs   []claimPrint   // documents that must print it; README.md when it names none
	find   map[string]any // key/value pairs naming the object to read, when the path alone is ambiguous
	path   []string       // keys from that object down to the number; a leading "*" searches the file
	scale  float64        // 100 to turn a stored ratio into the percent the prose prints; 0 means none
	digits int            // decimals the document prints
}

// claimPrint is one document that has to carry a claim, and the wording it uses:
// the same measurement is "18000 个请求" in the README and "18000 请求" in the
// RESUME, and what matters is that both still say the number at all.
type claimPrint struct {
	file   string // document; README.md when empty
	phrase string // the claim's phrase, then the quote itself, when empty
}

// prints is where a claim has to appear: the documents it names, or the README,
// which is what an entry means when it names none.
func (claim baselineClaim) prints() []claimPrint {
	if len(claim.docs) > 0 {
		return claim.docs
	}
	return []claimPrint{{}}
}

func (want claimPrint) document() string {
	if want.file != "" {
		return want.file
	}
	return "README.md"
}

func (want claimPrint) wording(claim baselineClaim) string {
	if want.phrase != "" {
		return want.phrase
	}
	if claim.phrase != "" {
		return claim.phrase
	}
	return claim.quote
}

// claimDocs names the documents a claim has to appear in, for error messages.
func claimDocs(claim baselineClaim) []string {
	prints := claim.prints()
	names := make([]string, 0, len(prints))
	for _, want := range prints {
		names = append(names, want.document())
	}
	return names
}

func marker(find map[string]any) string {
	if len(find) == 0 {
		return ""
	}
	parts := make([]string, 0, len(find))
	for key, value := range find {
		parts = append(parts, key+"="+strconv.Quote(strings.TrimSpace(toString(value))))
	}
	return " where " + strings.Join(parts, " ") + ":"
}

func toString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func parseBaseline(t *testing.T, root, name string) any {
	t.Helper()

	var doc any
	raw := readFile(t, filepath.Join(root, "docs", "baseline", name))
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("docs/baseline/%s is not JSON: %v", name, err)
	}
	return doc
}

// findObjects returns every object in the document that carries all the marker's
// key/value pairs, so a claim can name its block instead of counting array
// positions that a new variant would shift.
func findObjects(node any, find map[string]any) []map[string]any {
	var found []map[string]any
	var walk func(any)
	walk = func(n any) {
		switch typed := n.(type) {
		case map[string]any:
			match := true
			for key, want := range find {
				if got, ok := typed[key]; !ok || got != want {
					match = false
					break
				}
			}
			if match {
				found = append(found, typed)
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(node)
	return found
}

// numbersAt walks a path to a number, collecting every value it finds: a leading
// "*" searches the whole document for the next key name, and an integer segment
// indexes an array.
func numbersAt(node any, path []string) []float64 {
	if len(path) == 0 {
		if value, ok := node.(float64); ok {
			return []float64{value}
		}
		return nil
	}
	if path[0] == "*" {
		if len(path) < 2 {
			return nil
		}
		var out []float64
		var walk func(any)
		walk = func(n any) {
			switch typed := n.(type) {
			case map[string]any:
				for key, child := range typed {
					if key == path[1] {
						out = append(out, numbersAt(child, path[2:])...)
					}
					walk(child)
				}
			case []any:
				for _, child := range typed {
					walk(child)
				}
			}
		}
		walk(node)
		return out
	}
	switch typed := node.(type) {
	case map[string]any:
		if child, ok := typed[path[0]]; ok {
			return numbersAt(child, path[1:])
		}
	case []any:
		if index, err := strconv.Atoi(path[0]); err == nil && index >= 0 && index < len(typed) {
			return numbersAt(typed[index], path[1:])
		}
	}
	return nil
}

func singleNumber(t *testing.T, doc any, find map[string]any, path []string) float64 {
	t.Helper()

	var values []float64
	if len(find) == 0 {
		values = numbersAt(doc, path)
	} else {
		for _, object := range findObjects(doc, find) {
			values = append(values, numbersAt(object, path)...)
		}
	}
	if len(values) != 1 {
		t.Fatalf("expected exactly one value at %s, found %d", strings.Join(path, "."), len(values))
	}
	return values[0]
}
