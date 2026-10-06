package repofmt

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocumentedAccessChecksMatchTheSource pins the size of the access gate's
// Go evidence to the source those documents cite.
//
// WHY IT EXISTS. The gate table in docs/ACCEPTANCE.md gives the access row a
// line of its own, and both that row and README.md say how much Go evidence
// backs it. The number was 41, and nothing in the repository produced it: the
// two files the documents name hold 40 and 15 t.Error*/t.Fatal* check sites, so
// 41 was neither their sum nor either half, and it is not an executed-assertion
// count either -- the loops in internal/server/access_test.go run one site per
// path, and the two tests that walk the credential matrix execute 52 checks
// between them. Every other Go number in that table comes from cmd/verify*,
// which prints what it ran; this row is not covered by that counter, so it says
// 检查点 -- check sites in the source -- and this test is what keeps the
// sentence, the table row and the source in step.
//
// WHAT IT COUNTS. Sites, not executions: a table-driven loop runs one site many
// times, and counting executions would need those tests to run somewhere this
// package cannot see. A site is what changes when the tests change, which is
// the drift worth catching.
func TestDocumentedAccessChecksMatchTheSource(t *testing.T) {
	root := repoRoot(t)
	serverPath := filepath.Join(root, "internal", "server", "access_test.go")
	configPath := filepath.Join(root, "internal", "config", "config_test.go")
	serverSrc := readFile(t, serverPath)
	configSrc := readFile(t, configPath)

	// A denominator guard: the documents cite a file of access tests, so the
	// file has to keep looking like one.
	if tests := len(testFuncLine.FindAllString(serverSrc, -1)); tests != 11 {
		t.Fatalf("internal/server/access_test.go has %d test functions, want 11; the file changed shape", tests)
	}
	serverChecks := countCheckSites(serverSrc)

	// The two named cases, counted inside their own bodies: the rest of
	// config_test.go is about other sections.
	configChecks := 0
	for _, name := range []string{"TestAccessSectionParsesAndExpands", "TestAccessValidationErrors"} {
		body, ok := functionBody(configSrc, name)
		if !ok {
			t.Fatalf("internal/config/config_test.go has no %s, but docs/ACCEPTANCE.md cites it", name)
		}
		configChecks += countCheckSites(body)
	}
	total := serverChecks + configChecks

	// Every statement names both files, so a sentence that drops one of them
	// stops matching instead of passing.
	statements := []struct {
		where  string
		rx     *regexp.Regexp
		total  int
		server int
		config int
		curl   int
	}{
		{
			"README.md's note on what /admin/* does not protect",
			regexp.MustCompile("Go 进程内 (\\d+) 个检查点（`internal/server/access_test\\.go` (\\d+) 个 \\+\\s*" +
				"`internal/config/config_test\\.go` 的两个 access 用例 (\\d+) 个"),
			1, 2, 3, 0,
		},
		{
			"docs/ACCEPTANCE.md's gate table row",
			regexp.MustCompile("(?m)^\\|\\s*管理面鉴权[^|]*\\|\\s*(\\d+)\\s*\\|\\s*(\\d+)\\s*\\|"),
			1, 0, 0, 2,
		},
		{
			"docs/ACCEPTANCE.md's evidence-boundary paragraph",
			regexp.MustCompile("Go 进程内（(\\d+) 个检查点，\\s*`internal/server/access_test\\.go` (\\d+) 个 \\+\\s*" +
				"`internal/config/config_test\\.go` 的两个 access 用例 (\\d+) 个）与真进程 \\+ curl（(\\d+) 条"),
			1, 2, 3, 4,
		},
	}

	curlSeen, curlWhere := -1, ""
	check := func(where, text string, rx *regexp.Regexp, totalGrp, serverGrp, configGrp, curlGrp int) {
		t.Helper()
		m := rx.FindStringSubmatch(text)
		if m == nil {
			t.Errorf("%s no longer states the access gate's counts in the form this test reads "+
				"(pattern %s); update the sentence or the pattern rather than dropping the number", where, rx)
			return
		}
		if totalGrp > 0 {
			if got := atoi(t, where, m[totalGrp]); got != total {
				t.Errorf("%s states %d check sites for the access gate, but its source has %d "+
					"(%d in internal/server/access_test.go + %d in internal/config/config_test.go)",
					where, got, total, serverChecks, configChecks)
			}
		}
		if serverGrp > 0 {
			if got := atoi(t, where, m[serverGrp]); got != serverChecks {
				t.Errorf("%s states %d check sites in internal/server/access_test.go, but the file has %d",
					where, got, serverChecks)
			}
		}
		if configGrp > 0 {
			if got := atoi(t, where, m[configGrp]); got != configChecks {
				t.Errorf("%s states %d check sites in the two internal/config/config_test.go cases, but they have %d",
					where, got, configChecks)
			}
		}
		if curlGrp > 0 {
			got := atoi(t, where, m[curlGrp])
			if curlSeen < 0 {
				curlSeen, curlWhere = got, where
				return
			}
			if got != curlSeen {
				t.Errorf("%s says the operator-token chain asserts %d times, %s says %d; "+
					"scripts/verify-hardening.ps1 prints one number", curlWhere, curlSeen, where, got)
			}
		}
	}

	texts := map[string]string{
		"README.md":          readFile(t, filepath.Join(root, "README.md")),
		"docs/ACCEPTANCE.md": readFile(t, filepath.Join(root, "docs", "ACCEPTANCE.md")),
	}
	for _, s := range statements {
		where := s.where
		text, ok := texts[strings.SplitN(where, "'", 2)[0]]
		if !ok {
			t.Fatalf("%s: the test has no text loaded for this document", where)
		}
		check(where, text, s.rx, s.total, s.server, s.config, s.curl)
	}
	if curlSeen < 0 {
		t.Fatalf("no statement of the operator-token assertion count was found; the curl half of the row is unchecked")
	}

	t.Logf("access gate: %d check sites in internal/server/access_test.go + %d in the two "+
		"internal/config/config_test.go cases = %d, against %d assertions on the curl side",
		serverChecks, configChecks, total, curlSeen)
}

// testFuncLine matches a top-level test function declaration.
var testFuncLine = regexp.MustCompile(`(?m)^func Test`)

// checkSite matches the call that makes an assertion in these tests.
var checkSite = regexp.MustCompile(`t\.(?:Error|Errorf|Fatal|Fatalf)\(`)

// countCheckSites counts assertion call sites, skipping comment lines so that a
// commented-out check is not counted as evidence.
func countCheckSites(src string) int {
	n := 0
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		n += len(checkSite.FindAllStringIndex(line, -1))
	}
	return n
}

// functionBody returns one top-level test function, from its declaration to the
// line before the next one.
func functionBody(src, name string) (string, bool) {
	start := strings.Index(src, "\nfunc "+name+"(t *testing.T) {")
	if start < 0 {
		return "", false
	}
	rest := src[start+1:]
	if end := strings.Index(rest[1:], "\nfunc "); end >= 0 {
		return rest[:end+1], true
	}
	return rest, true
}
