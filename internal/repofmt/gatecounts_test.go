package repofmt

import (
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// TestDocumentedGateTotalsAreTheSumOfTheirRows pins the arithmetic in the two
// tables that tell a reader how much is being asserted, and in every sentence
// that repeats their totals.
//
// WHY IT EXISTS. README.md's 3.3 table and docs/ACCEPTANCE.md's gate table each
// print one count per milestone and a 合计 row underneath. Nothing added those
// rows up, and they did not add up: the curl column listed
// 47+56+157+323+125+211+149 = 1068 under a 合计 of 1060, and four other
// sentences repeated the 1060. 1060 is what the column summed to before M5's
// curl gate grew by the eight parser self-checks that docs/ACCEPTANCE.md
// describes ("M5 的 curl 门从 203 条变成 211 条"): the row was updated, the total
// was not. A number printed next to its addends has to equal their sum.
//
// WHAT IT DOES NOT CHECK. Whether 47 is really how many assertions
// scripts/verify-m0.ps1 runs. Establishing that needs the script, real processes
// and real ports, so it is a measurement rather than a unit test; the
// measurements are the run ids recorded in docs/ACCEPTANCE.md. What is checked
// here is the part arithmetic can settle: the two documents agree row by row,
// each 合计 is the sum of the rows above it, and the sentences that restate a
// total restate the same one.
func TestDocumentedGateTotalsAreTheSumOfTheirRows(t *testing.T) {
	root := repoRoot(t)

	// Two spellings of a milestone row: README.md names the Go gate in the cell,
	// docs/ACCEPTANCE.md follows the count with the raw-data file instead.
	readmeMilestone := regexp.MustCompile("(?m)^\\|\\s*(M\\d)[^|]*\\|\\s*`cmd/verify[^`]*`\\s*—\\s*(\\d+)\\s*条\\s*\\|\\s*(\\d+)\\s*条\\s*\\|")
	acceptanceMilestone := regexp.MustCompile("(?m)^\\|\\s*(M\\d)[^|]*\\|\\s*(\\d+)\\s*\\|\\s*(\\d+)\\s*\\|")
	readmeTotal := regexp.MustCompile("(?m)^\\|\\s*\\*\\*合计\\*\\*\\s*\\|\\s*\\*\\*(\\d+)\\s*条\\*\\*\\s*\\|\\s*\\*\\*(\\d+)\\s*条\\*\\*\\s*\\|")
	acceptanceTotal := regexp.MustCompile("(?m)^\\|\\s*\\*\\*合计\\*\\*\\s*\\|\\s*\\*\\*(\\d+)\\*\\*\\s*\\|\\s*\\*\\*(\\d+)\\*\\*\\s*\\|")

	readmeRows := milestoneCounts(t, "README.md", readFile(t, filepath.Join(root, "README.md")), readmeMilestone)
	acceptanceRows := milestoneCounts(t, "docs/ACCEPTANCE.md",
		readFile(t, filepath.Join(root, "docs", "ACCEPTANCE.md")), acceptanceMilestone)

	for _, name := range milestoneNames {
		got, ok := readmeRows[name]
		if !ok {
			t.Errorf("README.md has no %s row, but docs/ACCEPTANCE.md does", name)
			continue
		}
		want, ok := acceptanceRows[name]
		if !ok {
			t.Errorf("docs/ACCEPTANCE.md has no %s row, but README.md does", name)
			continue
		}
		if got != want {
			t.Errorf("%s: README.md says %d Go / %d curl, docs/ACCEPTANCE.md says %d Go / %d curl; "+
				"the two tables describe the same gates, so one of them is stale",
				name, got[0], got[1], want[0], want[1])
		}
	}

	goSum, curlSum := 0, 0
	for _, name := range milestoneNames {
		goSum += readmeRows[name][0]
		curlSum += readmeRows[name][1]
	}

	checkTotal := func(name, text string, rx *regexp.Regexp) {
		t.Helper()
		found := rx.FindAllStringSubmatch(text, -1)
		if len(found) != 1 {
			t.Fatalf("%s: found %d 合计 rows, want 1; the table changed shape", name, len(found))
		}
		goTotal := atoi(t, name, found[0][1])
		curlTotal := atoi(t, name, found[0][2])
		if goTotal != goSum {
			t.Errorf("%s: the 合计 row says %d Go assertions, but the milestone rows above it add up to %d",
				name, goTotal, goSum)
		}
		if curlTotal != curlSum {
			t.Errorf("%s: the 合计 row says %d curl assertions, but the milestone rows above it add up to %d",
				name, curlTotal, curlSum)
		}
	}
	checkTotal("README.md", readFile(t, filepath.Join(root, "README.md")), readmeTotal)
	checkTotal("docs/ACCEPTANCE.md", readFile(t, filepath.Join(root, "docs", "ACCEPTANCE.md")), acceptanceTotal)

	// The sentences that repeat a total. Each wording is listed rather than
	// derived, and each has to keep matching: a pattern that quietly stops
	// matching would leave the restated total unchecked, which is the failure
	// this check exists to prevent.
	claims := []struct {
		wording   string
		rx        *regexp.Regexp
		goGroup   int
		curlGroup int
		hits      int
	}{
		{"README.md's opening summary (Go half)",
			regexp.MustCompile(`Go 进程内端到端 \*\*(\d+)\*\* 条断言`), 1, 0, 0},
		{"README.md's opening summary (curl half)",
			regexp.MustCompile("真实进程 \\+ 真实 `curl\\.exe` \\*\\*(\\d+)\\*\\* 条断言"), 0, 1, 0},
		{"README.md's CI section and docs/ACCEPTANCE.md's automation table",
			regexp.MustCompile(`(\d+)\s*条 curl\s*(?:端到端\s*)?断言`), 0, 1, 0},
		{"docs/ACCEPTANCE.md's note on why the access gate is a row of its own",
			regexp.MustCompile(`(\d+)[""]?这个从 M0 起就写在 README 里的数字`), 0, 1, 0},
		{"README.md's CI section (Go half)",
			regexp.MustCompile(`(\d+)\s*条进程内 Go 断言`), 1, 0, 0},
		{"docs/RESUME.md's résumé bullet",
			regexp.MustCompile(`Go (\d+) 条 \+ 真实进程 curl (\d+) 条`), 1, 2, 0},
	}

	docs := []string{filepath.Join(root, "README.md")}
	globbed, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatalf("globbing docs: %v", err)
	}
	docs = append(docs, globbed...)
	if len(docs) < 5 {
		t.Fatalf("only %d documents to scan; the check is looking in the wrong place", len(docs))
	}

	restatements := 0
	for _, path := range docs {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relativising %s: %v", path, err)
		}
		rel = filepath.ToSlash(rel)
		text := readFile(t, path)
		for i := range claims {
			claim := &claims[i]
			for _, m := range claim.rx.FindAllStringSubmatch(text, -1) {
				claim.hits++
				restatements++
				if claim.goGroup > 0 {
					if got := atoi(t, rel, m[claim.goGroup]); got != goSum {
						t.Errorf("%s: %s states %d Go assertions, but the gate table adds up to %d",
							rel, claim.wording, got, goSum)
					}
				}
				if claim.curlGroup > 0 {
					if got := atoi(t, rel, m[claim.curlGroup]); got != curlSum {
						t.Errorf("%s: %s states %d curl assertions, but the gate table adds up to %d",
							rel, claim.wording, got, curlSum)
					}
				}
			}
		}
	}
	if restatements < len(claims) {
		t.Fatalf("only %d restatements of the totals found across %d documents, want at least %d (one per wording)",
			restatements, len(docs), len(claims))
	}
	for i := range claims {
		if claims[i].hits == 0 {
			t.Errorf("no document states %s any more; if the wording changed, update the pattern in this test "+
				"rather than leaving the total unstated", claims[i].wording)
		}
	}

	t.Logf("checked %d milestone rows in each gate table (%d Go / %d curl) and %d restatements across %d documents",
		len(milestoneNames), goSum, curlSum, restatements, len(docs))
}

// milestoneNames is the set both tables are expected to describe, in order.
var milestoneNames = []string{"M0", "M1", "M2", "M3", "M4", "M5", "M6"}

// milestoneCounts reads one document's gate table into "M3 -> {Go, curl}".
func milestoneCounts(t *testing.T, name, text string, row *regexp.Regexp) map[string][2]int {
	t.Helper()
	counts := map[string][2]int{}
	for _, m := range row.FindAllStringSubmatch(text, -1) {
		if _, dup := counts[m[1]]; dup {
			t.Errorf("%s: %s appears twice in the gate table", name, m[1])
		}
		counts[m[1]] = [2]int{atoi(t, name, m[2]), atoi(t, name, m[3])}
	}
	// A denominator guard: the row pattern has to keep matching all seven
	// milestones, or a reshaped table would turn this into green assertions about
	// nothing.
	if len(counts) != len(milestoneNames) {
		t.Fatalf("%s: found %d milestone rows, want %d; the table changed shape",
			name, len(counts), len(milestoneNames))
	}
	for _, want := range milestoneNames {
		if _, ok := counts[want]; !ok {
			t.Fatalf("%s: the gate table has no %s row", name, want)
		}
	}
	return counts
}

// atoi parses a count that a regexp has already restricted to digits.
func atoi(t *testing.T, where, digits string) int {
	t.Helper()
	n, err := strconv.Atoi(digits)
	if err != nil {
		t.Fatalf("%s: parsing the count %q: %v", where, digits, err)
	}
	return n
}
