package repofmt

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
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

	readmeTotal := regexp.MustCompile("(?m)^\\|\\s*\\*\\*合计\\*\\*\\s*\\|\\s*\\*\\*(\\d+)\\s*条\\*\\*\\s*\\|\\s*\\*\\*(\\d+)\\s*条\\*\\*\\s*\\|")
	acceptanceTotal := regexp.MustCompile("(?m)^\\|\\s*\\*\\*合计\\*\\*\\s*\\|\\s*\\*\\*(\\d+)\\*\\*\\s*\\|\\s*\\*\\*(\\d+)\\*\\*\\s*\\|")

	readmeRows := milestoneCounts(t, "README.md", readFile(t, filepath.Join(root, "README.md")), readmeMilestoneRow)
	acceptanceRows := milestoneCounts(t, "docs/ACCEPTANCE.md",
		readFile(t, filepath.Join(root, "docs", "ACCEPTANCE.md")), acceptanceMilestoneRow)

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

// TestEveryQuotedGateCountMatchesTheTable extends the arithmetic check to the
// places that name one gate and print its count: the acceptance commands in
// docs/USAGE.md, the per-milestone bullets in docs/DESIGN.md and the résumé
// bullet in docs/RESUME.md.
//
// WHY IT EXISTS. The two tables are only half of where a count is written down.
// docs/DESIGN.md's bullets, docs/USAGE.md's command block and docs/RESUME.md
// all restate individual milestone counts, and nothing compared those against
// the table -- the same shape of defect the table's own 合计 row had, one row
// down: a gate that grows or shrinks leaves every sentence that quotes it
// stale, and a reader cannot tell which of the two numbers to believe.
//
// WHAT IT CHECKS. Every sentence whose wording ties a command or script to a
// count, matched per milestone rather than accumulated: each hit has to equal
// the row the table gives for that milestone and that side (Go gate vs curl
// gate). Counts that belong to something else -- the measure scripts quote
// their own totals in the same sentences -- are not matched, because the
// patterns require the cmd/verify* or scripts/verify-m*.ps1 name.
//
// The Makefile states the same counts in English, one ## comment per target, and
// it is read here for the same reason: nothing compared it against the table, and
// two of its comments had drifted. verify-m5-curl still said 203 where the row
// says 211 (the number M5's curl row had before its eight parser self-checks),
// and verify-m6-curl claimed "over 5,000 curl-level assertions across the seven
// gates" where the seven curl rows add up to 1068.
func TestEveryQuotedGateCountMatchesTheTable(t *testing.T) {
	root := repoRoot(t)
	readme := readFile(t, filepath.Join(root, "README.md"))
	rows := milestoneCounts(t, "README.md", readme, readmeMilestoneRow)

	type quote struct {
		what         string
		rx           *regexp.Regexp
		milestoneGrp int
		countGrp     int
		alsoCountGrp int  // a second number that must equal the same row, or 0
		curl         bool // the fixed side, for the patterns that name it
		fromComment  bool // the side is decided by a capture: group 2 says "curl"
	}
	quotes := []quote{
		{ // "…verify-m2      # M2，103 条断言" / "…# M2 curl，158 条"
			what:         "an acceptance command's trailing comment",
			rx:           regexp.MustCompile(`(?m)#\s*M(\d)\s*(curl)?，(\d+)\s*条`),
			milestoneGrp: 1, countGrp: 3, fromComment: true},
		{ // "`cmd/verify-m1`（Go，64 条断言）"
			what:         "a docs/DESIGN.md bullet about a Go gate",
			rx:           regexp.MustCompile("`cmd/verify(?:-m(\\d))?`（[^）]{0,40}?(\\d+)\\s*条断言"),
			milestoneGrp: 1, countGrp: 2},
		{ // "`scripts/verify-m1.ps1`（curl，58 条断言）"
			what:         "a docs/DESIGN.md bullet about a curl gate",
			rx:           regexp.MustCompile("`scripts/verify-m(\\d)\\.ps1`（[^）]{0,40}?(\\d+)\\s*条断言"),
			milestoneGrp: 1, countGrp: 2, curl: true},
		{ // "`cmd/verify-m5` 的 420 条断言"
			what:         "a sentence pointing at a Go gate's count",
			rx:           regexp.MustCompile("`cmd/verify(?:-m(\\d))?`\\s*的\\s*(\\d+)\\s*条断言"),
			milestoneGrp: 1, countGrp: 2},
		{ // "`scripts/verify-m0.ps1` 会…跑完 47 条断言"
			what:         "a sentence about what a curl gate runs",
			rx:           regexp.MustCompile("`scripts/verify-m(\\d)\\.ps1`[^。\\n]{0,40}?跑完\\s*(\\d+)\\s*条断言"),
			milestoneGrp: 1, countGrp: 2, curl: true},
		{ // "…\cmd\verify-m2   # Go 门禁，103 条断言"  (a command, so a backslash in the path)
			what:         "a command block that labels its own gate",
			rx:           regexp.MustCompile(`(?m)^.*cmd[\\/]verify-m(\d)\b.*#.*?(\d+)\s*条断言`),
			milestoneGrp: 1, countGrp: 2},
		{ // "…\cmd\verify-m3   # Go 门禁，470/470 断言"  (what a run prints, both numbers)
			what:         "a command block quoting a run's own tally",
			rx:           regexp.MustCompile(`(?m)^.*cmd[\\/]verify-m(\d)\b.*#.*?(\d+)/(\d+)\s*断言`),
			milestoneGrp: 1, countGrp: 2, alsoCountGrp: 3},
		{ // "Go `cmd/verify-m5` **420** 条断言" -- any shape between the name and the count
			what:         "a sentence that names a Go gate and then a count",
			rx:           regexp.MustCompile("`cmd/verify(?:-m(\\d))?`[^。\\n]{0,24}?(\\d+)\\s*条断言"),
			milestoneGrp: 1, countGrp: 2},
		{ // "curl `scripts/verify-m5.ps1` **211** 条断言"
			what:         "a sentence that names a curl gate and then a count",
			rx:           regexp.MustCompile("`scripts/verify-m(\\d)\\.ps1`[^。\\n]{0,24}?(\\d+)\\s*条断言"),
			milestoneGrp: 1, countGrp: 2, curl: true},
	}

	// Every tracked prose surface, not just the two tables' own files: the four
	// docs/ pages plus deploy/README.md, which quotes a script's own check count
	// but no milestone gate's.
	docs := []string{filepath.Join(root, "README.md")}
	for _, dir := range []string{"docs", "deploy"} {
		globbed, err := filepath.Glob(filepath.Join(root, dir, "*.md"))
		if err != nil {
			t.Fatalf("globbing %s: %v", dir, err)
		}
		if len(globbed) == 0 {
			t.Fatalf("no markdown files under %s; the glob stopped finding them", dir)
		}
		docs = append(docs, globbed...)
	}

	perPattern := make([]int, len(quotes))
	total := 0
	for _, path := range docs {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relativising %s: %v", path, err)
		}
		rel = filepath.ToSlash(rel)
		text := readFile(t, path)
		for i := range quotes {
			q := &quotes[i]
			for _, m := range q.rx.FindAllStringSubmatchIndex(text, -1) {
				perPattern[i]++
				total++
				name := "M0"
				if g := m[2*q.milestoneGrp]; g >= 0 {
					name = "M" + text[g:m[2*q.milestoneGrp+1]]
				}
				curl := q.curl
				if q.fromComment && m[2*2] >= 0 {
					curl = true
				}
				side := 0
				sideName := "Go"
				if curl {
					side, sideName = 1, "curl"
				}
				nums := []int{atoi(t, rel, text[m[2*q.countGrp]:m[2*q.countGrp+1]])}
				if q.alsoCountGrp != 0 && m[2*q.alsoCountGrp] >= 0 {
					nums = append(nums, atoi(t, rel, text[m[2*q.alsoCountGrp]:m[2*q.alsoCountGrp+1]]))
				}
				want, known := rows[name]
				if !known {
					t.Errorf("%s:%d: %s names %s, which is not a milestone in the gate table",
						rel, lineOf(text, m[0]), q.what, name)
					continue
				}
				for _, got := range nums {
					if got != want[side] {
						t.Errorf("%s:%d: %s quotes %d assertions for %s, but the gate table's %s %s row says %d",
							rel, lineOf(text, m[0]), q.what, got, name, name, sideName, want[side])
					}
				}
			}
		}
	}

	// A denominator guard, then one per pattern: a wording that stops matching
	// would leave that sentence's number unchecked, which is what this test is
	// for.
	if total < 25 {
		t.Fatalf("only %d quoted gate counts found, want at least 25; the patterns stopped matching", total)
	}
	for i := range quotes {
		if perPattern[i] == 0 {
			t.Errorf("no document %s any more; if the wording changed, update the pattern in this test "+
				"rather than leaving the count unstated", quotes[i].what)
		}
	}

	// The one place that quotes the range rather than a single gate.
	span := regexp.MustCompile(`（(\d+) ~ (\d+) 条断言）`).FindStringSubmatch(readme)
	if span == nil {
		t.Errorf("README.md no longer quotes the Go gates as a range; update this pattern rather than dropping it")
	} else {
		low, high := atoi(t, "README.md", span[1]), atoi(t, "README.md", span[2])
		smallest, largest := rows[milestoneNames[0]][0], rows[milestoneNames[0]][0]
		for _, name := range milestoneNames {
			if got := rows[name][0]; got < smallest {
				smallest = got
			} else if got > largest {
				largest = got
			}
		}
		if low != smallest || high != largest {
			t.Errorf("README.md quotes the Go gates as %d ~ %d assertions, but the table spans %d ~ %d",
				low, high, smallest, largest)
		}
	}

	// The curl column's total, for the one comment that states it as well as its
	// own row.
	curlTotal := 0
	for _, name := range milestoneNames {
		curlTotal += rows[name][1]
	}

	// The Makefile states the same counts in English, one ## comment per gate
	// target. The target name anchors the claim, so the block above a target has
	// to quote exactly that target's row -- the two drifted numbers this test
	// grew a Makefile half for (203 for verify-m5-curl, "over 5,000" for
	// verify-m6-curl) each sat next to a target that said which gate it was.
	//
	// measure-* and run-* targets are not covered: their comments quote the
	// totals their own runs print, which are measurements rather than rows of
	// the gate table.
	type makeClaim struct {
		target    string
		milestone string
		curl      bool
		total     bool // the block also states the curl column's total
	}
	makeClaims := []makeClaim{
		{"verify", "M0", false, false},
		{"verify-curl", "M0", true, false},
		{"verify-m1", "M1", false, false},
		{"verify-m1-curl", "M1", true, false},
		{"verify-m2", "M2", false, false},
		{"verify-m2-curl", "M2", true, false},
		{"verify-m3", "M3", false, false},
		{"verify-m3-curl", "M3", true, false},
		{"verify-m4", "M4", false, false},
		{"verify-m4-curl", "M4", true, false},
		{"verify-m5", "M5", false, false},
		{"verify-m5-curl", "M5", true, false},
		{"verify-m6", "M6", false, false},
		{"verify-m6-curl", "M6", true, true},
	}

	quoted := regexp.MustCompile(`(\d+)\s+assertions?`)
	targetLine := regexp.MustCompile(`^([a-z][a-z0-9-]*):$`)
	makefile := readFile(t, filepath.Join(root, "Makefile"))
	makeLines := strings.Split(makefile, "\n")

	// commentBlock renders the run of "##" lines directly above line i as one
	// paragraph: the Makefile wraps a sentence over several of them, so a count
	// at the end of one line and the word it counts on the next only meet once
	// the prefixes are gone.
	commentBlock := func(i int) string {
		from := i
		for from > 0 && strings.HasPrefix(makeLines[from-1], "##") {
			from--
		}
		parts := make([]string, 0, i-from)
		for _, ln := range makeLines[from:i] {
			parts = append(parts, strings.TrimSpace(strings.TrimPrefix(ln, "##")))
		}
		return strings.Join(parts, " ")
	}

	listed := map[string]bool{}
	commented := 0
	for _, c := range makeClaims {
		listed[c.target] = true
		at := -1
		for i, ln := range makeLines {
			if ln == c.target+":" {
				at = i
				break
			}
		}
		if at < 0 {
			t.Errorf("the Makefile has no %s target, so the comment quoting its count cannot be checked", c.target)
			continue
		}
		block := commentBlock(at)
		if block == "" {
			t.Errorf("the Makefile's %s target has no ## comment above it, so nothing states its count", c.target)
			continue
		}
		side, sideName := 0, "Go"
		if c.curl {
			side, sideName = 1, "curl"
		}
		want := []int{rows[c.milestone][side]}
		note := ""
		if c.total {
			want = append(want, curlTotal)
			note = " plus the curl column's total"
		}
		var got []int
		for _, m := range quoted.FindAllStringSubmatch(block, -1) {
			got = append(got, atoi(t, "Makefile", m[1]))
		}
		commented++
		if countsKey(got) != countsKey(want) {
			t.Errorf("the Makefile's %s comment quotes %s assertions, but this test expects %s (M%s's %s row%s)",
				c.target, joinInts(got), joinInts(want), strings.TrimPrefix(c.milestone, "M"), sideName, note)
		}
	}

	// A coverage guard: a verify-* target whose comment quotes a count has to be
	// listed above, or a gate added later would state its count unchecked.
	var unlisted []string
	for i, ln := range makeLines {
		m := targetLine.FindStringSubmatch(ln)
		if m == nil || !strings.HasPrefix(m[1], "verify") {
			continue
		}
		if !listed[m[1]] && quoted.MatchString(commentBlock(i)) {
			unlisted = append(unlisted, m[1])
		}
	}
	if len(unlisted) > 0 {
		t.Errorf("the Makefile quotes an assertion count for %s, but this test does not check %s; add it to makeClaims",
			strings.Join(unlisted, ", "), strings.Join(unlisted, ", "))
	}
	if commented < len(makeClaims) {
		t.Errorf("read the count from only %d of %d Makefile comments; a target or the block above it moved",
			commented, len(makeClaims))
	}

	t.Logf("checked %d quoted gate counts across %d documents and %d Makefile comments",
		total, len(docs), commented)
}

// countsKey renders a small count list as a key, sorted so a comment that states
// the same numbers in another order still matches.
func countsKey(ns []int) string {
	sorted := append([]int(nil), ns...)
	sort.Ints(sorted)
	return joinInts(sorted)
}

// joinInts renders counts for an error message.
func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// readmeMilestoneRow and acceptanceMilestoneRow are the two spellings of a
// milestone row: README.md names the Go gate in the cell, docs/ACCEPTANCE.md
// follows the count with the raw-data file instead.
var (
	readmeMilestoneRow     = regexp.MustCompile("(?m)^\\|\\s*(M\\d)[^|]*\\|\\s*`cmd/verify[^`]*`\\s*—\\s*(\\d+)\\s*条\\s*\\|\\s*(\\d+)\\s*条\\s*\\|")
	acceptanceMilestoneRow = regexp.MustCompile("(?m)^\\|\\s*(M\\d)[^|]*\\|\\s*(\\d+)\\s*\\|\\s*(\\d+)\\s*\\|")
)

// lineOf turns a byte offset into the 1-based line that holds it.
func lineOf(text string, offset int) int {
	return strings.Count(text[:offset], "\n") + 1
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
