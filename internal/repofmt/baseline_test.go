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
// requires the string README.md prints to be that number formatted the way the
// README formats it (the scale and the number of decimals are part of the entry,
// because "58.97%" is stored as 0.5897 and "2.42x" as 2.423). A path may start
// with "*", which searches the whole file for that key: it succeeds only when the
// key carries the same value everywhere it appears, so a second copy with a
// different number is a failure rather than a coin flip. When a key is not unique
// by name alone (m4-summary.json has one ratio per variant, m2-summary.json a
// hit_rate per sweep step) the entry names the object it means with a marker, so
// adding a variant or a step cannot silently move which number is read.
//
// WHAT IT DOES NOT CHECK. Whether the run was any good, or whether the baseline
// still describes today's code: a baseline is one afternoon's measurement on one
// machine, and the prose says so. This only settles that the sentence and the
// record are about the same number.
func TestHeadlineClaimsMatchTheBaseline(t *testing.T) {
	root := repoRoot(t)
	readme := readFile(t, filepath.Join(root, "README.md"))

	claims := []baselineClaim{
		{
			quote: "420.42", file: "m1-summary.json",
			find: map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"},
			path: []string{"median_ms_while_breaker_closed"}, digits: 2,
		},
		{
			quote: "17.81", file: "m1-summary.json",
			find: map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"},
			path: []string{"median_ms_once_breaker_open"}, digits: 2,
		},
		{
			quote: "58.97", file: "m2-summary.json",
			path: []string{"hit_rate", "hit_rate"}, scale: 100, digits: 2,
		},
		{
			quote: "56.39", file: "m2-summary.json",
			path: []string{"*", "reduction_percent"}, digits: 2,
		},
		{
			quote: "100.7", file: "m4-summary.json",
			find: map[string]any{"variant": "awq"},
			path: []string{"output_tokens_per_s", "variant"}, digits: 1,
		},
		{
			quote: "2.42", file: "m4-summary.json",
			find: map[string]any{"variant": "awq"},
			path: []string{"output_tokens_per_s", "ratio"}, digits: 2,
		},
		{
			quote: "34.52", file: "m4-summary.json",
			path: []string{"*", "avoided_percent"}, digits: 2,
		},
		{
			quote: "59400", file: "m5-summary.json",
			path: []string{"*", "otlp_exported"}, digits: 0,
		},
		{
			quote: "22.07", file: "m5-summary.json",
			find: map[string]any{"workload": "non-stream", "concurrency": 128.0},
			path: []string{"direct_minus_gateway_pct"}, digits: 2,
		},
	}
	if len(claims) < 8 {
		t.Fatalf("only %d claims; this check has been hollowed out", len(claims))
	}

	parsed := map[string]any{}
	for _, claim := range claims {
		doc, ok := parsed[claim.file]
		if !ok {
			doc = parseBaseline(t, root, claim.file)
			parsed[claim.file] = doc
		}

		values := numbersAt(doc, claim.path)
		if len(claim.find) > 0 {
			values = nil
			for _, object := range findObjects(doc, claim.find) {
				values = append(values, numbersAt(object, claim.path)...)
			}
		}
		if len(values) == 0 {
			t.Errorf("docs/baseline/%s has no value at %s%s; the record moved",
				claim.file, marker(claim.find), strings.Join(claim.path, "."))
			continue
		}
		distinct := map[float64]bool{}
		for _, value := range values {
			distinct[value] = true
		}
		if len(distinct) > 1 {
			t.Errorf("docs/baseline/%s carries %d different values for %s%s; name the one this claim means",
				claim.file, len(distinct), marker(claim.find), strings.Join(claim.path, "."))
			continue
		}

		factor := claim.scale
		if factor == 0 {
			factor = 1
		}
		got := strconv.FormatFloat(values[0]*factor, 'f', claim.digits, 64)
		if got != claim.quote {
			t.Errorf("docs/baseline/%s%s reads %s, but README.md prints %s",
				claim.file, marker(claim.find), got, claim.quote)
		}
		if !strings.Contains(readme, claim.quote) {
			t.Errorf("README.md no longer prints %s anywhere; if the claim moved, move this entry too", claim.quote)
		}
	}

	// The 23.6x in the M1 bullet is arithmetic on the two readings above rather
	// than a field of its own, so it is computed here from the same record.
	closed := singleNumber(t, parsed["m1-summary.json"],
		map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"}, "median_ms_while_breaker_closed")
	open := singleNumber(t, parsed["m1-summary.json"],
		map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"}, "median_ms_once_breaker_open")
	factor := strconv.FormatFloat(closed/open, 'f', 1, 64)
	if factor != "23.6" {
		t.Errorf("m1-summary.json's two medians give %s, but README.md prints 23.6", factor)
	}
	if !strings.Contains(readme, factor+"×") {
		t.Errorf("README.md does not print %s next to the two medians", factor+"×")
	}

	t.Logf("checked %d headline claims against docs/baseline", len(claims)+1)
}

// baselineClaim is one number the README prints, and where its record keeps it.
type baselineClaim struct {
	quote  string         // exactly what README.md prints, already scaled and rounded
	file   string         // file under docs/baseline/
	find   map[string]any // key/value pairs naming the object to read, when the path alone is ambiguous
	path   []string       // keys from that object down to the number; a leading "*" searches the file
	scale  float64        // 100 to turn a stored ratio into the percent the README prints; 0 means none
	digits int            // decimals the README prints
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

func singleNumber(t *testing.T, doc any, find map[string]any, key string) float64 {
	t.Helper()

	var values []float64
	for _, object := range findObjects(doc, find) {
		values = append(values, numbersAt(object, []string{key})...)
	}
	if len(values) != 1 {
		t.Fatalf("expected exactly one %s in docs/baseline, found %d", key, len(values))
	}
	return values[0]
}
