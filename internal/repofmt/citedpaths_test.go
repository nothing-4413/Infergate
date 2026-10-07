package repofmt

import (
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// TestEveryCitedRepoPathExists pins the file paths this repository's prose
// points readers at.
//
// WHY IT EXISTS. A path in a document is a claim about the tree, and nothing was
// checking it. The instance that prompted this check was small and telling:
// docs/USAGE.md told the reader to run
//
//	.\bin\infergate.exe -check -config .\configs\typo.yaml
//
// and quoted the loader's stderr for it, but no configs/typo.yaml has ever been
// in the repository -- the file was created in tmp/ for the run and the document
// recorded the wrong directory. The claim was true of a run nobody could repeat,
// which is the one thing evidence in this repository is not allowed to be. The
// same scan found the two placeholders below (`verify-mN.ps1`,
// `mN-summary.json`), where pointing at nothing is the point, and it found
// configs/infergate.yaml's "copy to configs/local.yaml", where the file is
// deliberately absent because .gitignore keeps it out of the tree.
//
// WHY THE SURFACE IS A LIST. Paths are cited in Go comments and shell scripts
// too, but those are code: a script may legitimately name the file it is about
// to write, and a comment there is read next to the statement that uses it. The
// surfaces below are the ones a reader trusts as a description of the tree.
func TestEveryCitedRepoPathExists(t *testing.T) {
	root := repoRoot(t)
	surfaces := proseSurfaces(t, root)

	// WHY THE PRECEDING CHARACTER IS PART OF THE MATCH. Go's regexp is RE2 and
	// has no lookbehind, so the boundary is matched and then discarded instead of
	// asserted. The boundary is what keeps a URL such as
	// https://github.com/.../docs/x.md from being read as a repository path.
	cited := regexp.MustCompile(`(?:^|[^\w./\\-])((?:\.\\)?(?:cmd|internal|scripts|configs|deploy|docs|tools|packages|\.github)[\\/][\w.\\/-]*\.(?:yaml|yml|ps1|sh|go|json|md|py|txt|conf|exe))`)

	// WHY AN EXPLICIT LIST RATHER THAN A PLACEHOLDER RULE. A rule for "a segment
	// that looks like a milestone" would have to guess, and it would silently
	// absorb the next real typo inside a milestone-shaped name. Three entries
	// with a reason each is a smaller thing to keep honest, and each one is
	// checked in both directions below: the file must still be missing, and the
	// citation must still be there.
	exempt := map[string]string{
		"scripts/verify-mN.ps1":         "README.md writes the milestone gate as mN; no such file is meant to exist",
		"docs/baseline/mN-summary.json": "the same placeholder shape in docs/RESUME.md",
		"configs/local.yaml":            ".gitignore:17 keeps it out of the tree, and configs/infergate.yaml tells the reader to create it",
	}

	used := map[string]bool{}
	total := 0
	var missing []string
	for _, file := range surfaces {
		text, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		body := string(text)
		if strings.EqualFold(filepath.Ext(file), ".md") {
			body = withoutFencedCodeBlocks(body)
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatalf("relativising %s: %v", file, err)
		}
		rel = filepath.ToSlash(rel)

		for _, m := range cited.FindAllStringSubmatchIndex(body, -1) {
			// Group 1 is the path; the boundary character is group 0's first rune
			// and is deliberately not part of either submatch.
			path := body[m[2]:m[3]]
			path = strings.TrimPrefix(strings.ReplaceAll(path, `\`, "/"), "./")
			total++
			if _, ok := exempt[path]; ok {
				used[path] = true
				continue
			}
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err == nil {
				continue
			}
			line := 1 + strings.Count(body[:m[2]], "\n")
			missing = append(missing, filepath.ToSlash(rel)+":"+
				strconv.Itoa(line)+": cites "+path+", which is not in the repository")
		}
	}

	// A check that matches nothing passes for the wrong reason -- the same guard
	// the documented-defaults check carries. The number is well under what the
	// tree holds today (300+), so it only fires when the scan is broken.
	t.Logf("checked %d path citations across %d surfaces", total, len(surfaces))
	if total < 100 {
		t.Fatalf("found only %d path citations across %d surfaces; the pattern or the surface list is wrong",
			total, len(surfaces))
	}
	for _, entry := range missing {
		t.Errorf("%s", entry)
	}
	for path, why := range exempt {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err == nil {
			t.Errorf("the exemption for %s is stale: the file exists now (%s)", path, why)
		}
		if !used[path] {
			t.Errorf("the exemption for %s is never cited any more (%s)", path, why)
		}
	}
}

// proseSurfaces lists the files whose text describes the tree.
//
// Every entry is checked to exist: a renamed document must not drop out of this
// check silently, which is exactly how a repository-wide property turns back
// into an assumption.
func proseSurfaces(t *testing.T, root string) []string {
	t.Helper()

	surfaces := make([]string, 0, 8)
	for _, name := range []string{"README.md", "Makefile", "docker-compose.yml", "Dockerfile"} {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s is gone, so this check no longer covers it: %v", name, err)
		}
		surfaces = append(surfaces, path)
	}

	// docs/baseline/ is exempt from the walks below: those JSON files are the
	// record of a run that already happened, not a description of today's tree,
	// and rewriting a citation in one would falsify the record.
	walks := []struct {
		dir  string
		exts []string
		skip []string
	}{
		{".github", []string{".yml", ".yaml"}, nil},
		{"docs", []string{".md"}, []string{"baseline"}},
		{"deploy", []string{".md"}, nil},
		{"configs", []string{".yaml"}, nil},
	}
	for _, walk := range walks {
		dir := filepath.Join(root, walk.dir)
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("walking %s: %v", walk.dir, err)
		}
		found := 0
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				for _, skip := range walk.skip {
					if info.Name() == skip {
						return filepath.SkipDir
					}
				}
				return nil
			}
			for _, ext := range walk.exts {
				if strings.EqualFold(filepath.Ext(path), ext) {
					surfaces = append(surfaces, path)
					found++
					return nil
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", walk.dir, err)
		}
		if found == 0 {
			t.Fatalf("no %v files under %s; this check is looking in the wrong place", walk.exts, walk.dir)
		}
	}

	sort.Strings(surfaces)
	return surfaces
}

// withoutFencedCodeBlocks blanks the body of ``` fences while keeping every
// line break, so offsets and line numbers still line up.
//
// WHY. A fenced block is quoted output or an example, not a description of the
// tree: a `go test` FAIL line that names the source line it came from, and a
// sample config naming an illustrative file, both belong there. Prose is where a
// path claims to exist.
func withoutFencedCodeBlocks(text string) string {
	lines := strings.Split(text, "\n")
	var out strings.Builder
	inFence := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			out.WriteString("\n")
			continue
		}
		if inFence {
			out.WriteString("\n")
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}

// withoutHereStrings blanks the body of PowerShell here-strings while keeping
// every line break, so offsets and line numbers still line up.
//
// WHY. A here-string holds output that was quoted -- a test transcript, a
// generated config, a shell script written out for a container -- and the same
// exemption a fenced markdown block gets applies: a `go test` FAIL line inside it
// names the source line it came from, which is the quote, not a claim about the
// tree. A double-quoted string does not get that exemption: it is text the script
// author wrote, and in a gate that text is printed for the reader.
//
// Only `@"` or `@'` at the end of a line opens one, and the @ has to start a
// word, so bash's `"$@"` inside a body does not.
func withoutHereStrings(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, len(lines))
	var terminator string
	for i, line := range lines {
		if terminator != "" {
			if strings.HasPrefix(strings.TrimSpace(line), terminator) {
				terminator = ""
			}
			continue
		}
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmed, `@"`) || strings.HasSuffix(trimmed, `@'`) {
			at := len(trimmed) - 2
			if at == 0 || strings.ContainsAny(trimmed[at-1:at], " \t=") {
				if trimmed[at+1] == '"' {
					terminator = `"@`
				} else {
					terminator = `'@`
				}
				continue
			}
		}
		out[i] = line
	}
	return strings.Join(out, "\n")
}

// fileMention matches any repository file name, cited by line or not. It is the
// denominator in the checks below and the file half of a spelled-out citation.
var fileMention = regexp.MustCompile(`[\w.\\/-]+\.(?:go|md|ps1|sh|yaml|yml|json)`)

// fileLineCitation matches the `path/file.go:NNN` form of a citation, the shape
// this file has always read. The leading character is part of the match because
// RE2 has no lookbehind; it is what stops a URL path from being read as one.
var fileLineCitation = regexp.MustCompile(`(?:^|[^\w./\\-])([\w.\\/-]*[\w-]\.(?:go|md|ps1|sh|yaml|yml|json)):\d+`)

// spelledOutLineNumber matches a line number written in words, the shape the
// `file.go:NNN` pattern cannot see.
var spelledOutLineNumber = regexp.MustCompile(`\bat lines? \d+`)

// linePinPhrase returns the spelled-out line number on a line that also names a
// repository file, or "".
//
// WHY THE FILE HAS TO BE NAMED. "line 55: unexpected indentation 4 in sequence"
// is quoted parser output, and a fenced block or a here-string already exempts
// it. A line that names a file *and* a line number is a citation of that file,
// and saying it in words rather than as `file.go:NNN` is the one shape the
// patterns above and below cannot see -- which is how
// scripts/verify-m4.ps1's note about X-InferGate-Capabilities kept naming
// `internal/gateway/proxy.go` "at line N" while the read moved to
// (*Proxy).plan, in front of both checks, until `f1f26b6`.
func linePinPhrase(line string) string {
	if !fileMention.MatchString(line) {
		return ""
	}
	return spelledOutLineNumber.FindString(line)
}

// TestProseCitesSymbolsNotLineNumbers forbids a line number in a citation the
// prose makes about this repository.
//
// WHY. A line number is a claim that another commit can silently falsify, and
// this repository has the receipts: configs/docker.yaml cited
// internal/config/config.go for the cache bound, the idempotency ledger and the
// session ledger at :356, :710 and :733. By the time anyone checked, those three
// lines held a `Threshold float64` field, an access token slice and the OTLP
// endpoint -- three citations, three of them wrong, and nothing could have
// reported it, because nothing compares a document to the file it points at. A
// symbol (`config.CacheConfig.MaxEntriesPerScope`) or a section
// (configs/docker.yaml 里 `idempotency:` 的注释) cannot rot that way.
//
// WHY FENCES ARE EXEMPT. Quoted command output is a record of what a tool
// printed once; its line numbers are part of the quote, not a claim about where
// something lives now. A sample that has to keep its line number therefore
// belongs in a fenced block, and prose is where the rule applies.
//
// WHY NOT SIMPLY CHECK THAT THE LINE EXISTS. That was the first idea and it
// catches almost nothing: a file grows, so an off-by-fifty citation still lands
// inside it, and the failure above (356 -> a Threshold field) is exactly the
// case an existence check calls fine.
//
// WHY IT ALSO READS THE SPELLED-OUT FORM. The pattern above only understands
// `file.go:NNN`. scripts/verify-m4.ps1's note about X-InferGate-Capabilities said
// `internal/gateway/proxy.go` "reads it at line N" instead, and prose that
// spells a line number out is the same claim in a shape nothing here matched
// (`f1f26b6`). A line number in words is now reported the same way, on the one
// condition that the line also names a file -- see linePinPhrase.
func TestProseCitesSymbolsNotLineNumbers(t *testing.T) {
	root := repoRoot(t)
	surfaces := proseSurfaces(t, root)

	// Both shapes live above: fileLineCitation is the `file.go:NNN` form, and
	// fileMention is every file the prose names at all -- the denominator, so a
	// pattern this test stops understanding cannot turn it into a no-op that
	// passes.
	total := 0
	var offenders []string
	for _, file := range surfaces {
		text, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		body := string(text)
		if strings.EqualFold(filepath.Ext(file), ".md") {
			body = withoutFencedCodeBlocks(body)
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatalf("relativising %s: %v", file, err)
		}
		rel = filepath.ToSlash(rel)

		total += len(fileMention.FindAllString(body, -1))
		for _, m := range fileLineCitation.FindAllStringSubmatchIndex(body, -1) {
			// m[2]..m[1] is the citation itself: the boundary character sits
			// before m[2] and is not part of the claim being reported.
			citation := body[m[2]:m[1]]
			line := 1 + strings.Count(body[:m[2]], "\n")
			offenders = append(offenders, rel+":"+strconv.Itoa(line)+": cites "+citation+
				"; name the symbol or the section instead, or put the quoted output in a fenced block")
		}
		for i, bodyLine := range strings.Split(body, "\n") {
			phrase := linePinPhrase(bodyLine)
			if phrase == "" {
				continue
			}
			offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": cites the line number spelled out as "+strconv.Quote(phrase)+
				"; name the symbol or the section instead, or put the quoted output in a fenced block")
		}
	}

	t.Logf("looked at %d file mentions across %d surfaces", total, len(surfaces))
	if total < 50 {
		t.Fatalf("only %d file mentions found; the pattern is not matching and this check cannot fail", total)
	}
	for _, offender := range offenders {
		t.Errorf("%s", offender)
	}
}

// TestCommentsCiteSymbolsNotLineNumbers applies the rule above to the comments of
// this repository's Go and PowerShell files.
//
// WHY IT IS A SEPARATE CHECK. TestEveryCitedRepoPathExists deliberately leaves
// code out of its surface list: a script may legitimately name a file it is about
// to write, and a comment there is read next to the statement that uses it. The
// rule here is narrower, and it is not about paths: it is about a comment that
// locates another file's behaviour by line number, which nothing in a comment can
// keep true. Four such citations had already gone wrong by the time the prose
// checks landed -- the token-key comment in cmd/verify-m3/governance.go named
// three lines of internal/quota/quota.go and all three had moved or been
// replaced, and internal/gateway/cachepath.go pointed into the middle of
// proxy.go's retry loop at a line that is now a bare return. Every one of them
// was invisible to every other check in this file, because comments are not a
// surface any of those checks reads.
//
// WHAT IT LOOKS AT. Every line of the PowerShell files under cmd/, internal/ and
// scripts/ -- outside here-string bodies, which hold quoted output -- and the
// lines of the Go files there whose first non-space characters are `//`. Block
// comments stay out of scope, as they were.
//
// WHY WHOLE POWERSHELL LINES AND NOT JUST ITS COMMENTS. A gate's notes are
// strings it prints, and a stale one is read exactly like a stale comment:
// scripts/verify-m4.ps1's note about X-InferGate-Capabilities named
// `internal/gateway/proxy.go` "at line N" from inside an Add-Note string. That
// line was not a comment this check read, and the spelled-out shape was not one
// the pattern below could match either, so the note stayed wrong in front of both
// checks until `f1f26b6`. Strings count now; linePinPhrase reads the words.
func TestCommentsCiteSymbolsNotLineNumbers(t *testing.T) {
	root := repoRoot(t)
	files := commentSurfaces(t, root)
	if len(files) < 100 {
		t.Fatalf("walked %d Go/PowerShell files under cmd/, internal/ and scripts/, want at least 100: the walk is not reaching the tree", len(files))
	}

	// fileLineCitation and fileMention are the two patterns the prose check
	// above uses: one shape, one denominator, so the two checks cannot drift
	// apart and leave a citation only one of them can see.
	total := 0
	var offenders []string
	for _, file := range files {
		text, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatalf("relativising %s: %v", file, err)
		}
		rel = filepath.ToSlash(rel)
		script := strings.EqualFold(filepath.Ext(file), ".ps1")
		body := string(text)
		if script {
			body = withoutHereStrings(body)
		}

		for i, line := range strings.Split(body, "\n") {
			if !script && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			total += len(fileMention.FindAllString(line, -1))
			for _, m := range fileLineCitation.FindAllStringSubmatch(line, -1) {
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": cites "+m[1]+
					" by line; name the symbol instead, or say which lines moved and why")
			}
			if phrase := linePinPhrase(line); phrase != "" {
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": cites the line number spelled out as "+strconv.Quote(phrase)+
					"; name the symbol instead, or say which lines moved and why")
			}
		}
	}

	t.Logf("read %d files; their comments and script text name %d files", len(files), total)
	if total < 100 {
		t.Fatalf("only %d file mentions found in comments and script text; the pattern is not matching and this check cannot fail", total)
	}
	for _, offender := range offenders {
		t.Errorf("%s", offender)
	}
}

// commentSurfaces returns every Go and PowerShell file under the three trees that
// hold the comments and script text citing code, sorted so a failure reads the
// same every run.
func commentSurfaces(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"cmd", "internal", "scripts"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".go", ".ps1":
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}
	sort.Strings(files)
	return files
}

// TestEveryMarkdownLinkResolvesInTheTree pins the links the prose makes to its
// own documents.
//
// WHY THE CHECK ABOVE IS NOT ENOUGH. That one matches cited paths that carry a
// directory (`docs/USAGE.md`); a link written as a bare filename -- the shape
// the four docs/ pages use to point at each other, `[USAGE.md](USAGE.md)` --
// never carries one, so a typo in it is invisible to it. Verified rather than
// assumed: renaming the target in docs/ACCEPTANCE.md to `[USAGE.md](USAG.md)`
// leaves TestEveryCitedRepoPathExists green. A renamed heading is invisible to
// every check that existed, because nothing read a fragment at all.
//
// WHAT IT CHECKS. Every `[text](target)` in a markdown surface, with fenced
// blocks blanked out first (a sample is an example, not a claim about the
// tree). A target resolves relative to the document that makes the claim, and a
// fragment has to match an anchor that the target document's headings actually
// produce -- a link to `#3-快速开始` is a claim about a heading, and renaming
// the heading falsifies it silently everywhere except here.
//
// WHY THE ANCHOR RULE IS SPELLED OUT. GitHub gives a heading its fragment by
// lower-casing it, dropping every character that is not a letter, a digit, an
// underscore or a hyphen, and turning spaces into hyphens; `## 3. 快速开始`
// becomes `#3-快速开始`, the one fragment link this repository makes. Checking
// only that the heading exists would have been the weaker claim: the link points
// at the anchor, not at the heading text.
func TestEveryMarkdownLinkResolvesInTheTree(t *testing.T) {
	root := repoRoot(t)

	link := regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

	documents, total, fragments := 0, 0, 0
	for _, file := range proseSurfaces(t, root) {
		if !strings.EqualFold(filepath.Ext(file), ".md") {
			continue
		}
		documents++
		body := withoutFencedCodeBlocks(readFile(t, file))
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatalf("relativising %s: %v", file, err)
		}
		rel = filepath.ToSlash(rel)

		for _, m := range link.FindAllStringSubmatchIndex(body, -1) {
			target := body[m[2]:m[3]]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			total++
			where := rel + ":" + strconv.Itoa(1+strings.Count(body[:m[0]], "\n"))

			path, fragment := target, ""
			if i := strings.IndexByte(target, '#'); i >= 0 {
				path, fragment = target[:i], target[i+1:]
			}
			dest := file
			if path != "" {
				resolved, ok := resolveLinkTarget(filepath.Dir(file), path)
				if !ok {
					t.Errorf("%s: links to %s, which is not in the repository", where, path)
					continue
				}
				dest = resolved
			}
			if fragment == "" {
				continue
			}

			fragments++
			if !headingAnchors(readFile(t, dest))[fragment] {
				destRel, err := filepath.Rel(root, dest)
				if err != nil {
					t.Fatalf("relativising %s: %v", dest, err)
				}
				t.Errorf("%s: links to #%s, but no heading in %s produces that anchor",
					where, fragment, filepath.ToSlash(destRel))
			}
		}
	}

	t.Logf("resolved %d markdown links (%d of them into a heading anchor) across %d documents",
		total, fragments, documents)
	if total < 20 || documents < 5 {
		t.Fatalf("found %d links across %d markdown surfaces; the pattern or the surface list is wrong",
			total, documents)
	}
	if fragments == 0 {
		t.Errorf("no link points at a heading any more; if the anchor moved, point this check at the new one " +
			"rather than dropping fragment checking")
	}
}

// TestCitedTestNamesExist pins the fourth kind of citation the prose makes: the
// name of a test it offers as evidence.
//
// WHY IT EXISTS. The M3 table in docs/ACCEPTANCE.md cited three tests that had
// never existed under the names it used -- TestDailyTokenBudgetRejects,
// TestDegradeKeepsTheReservation and TestNewServerFailsWhenQuotaRedisIsDown --
// while the tests that do carry those claims are called TestDailyTokenBudget,
// TestDegradeWithDowngradeModel and TestNewRedisStoreRejectsABadAddress. A
// renamed test leaves the sentence behind, still reading like evidence, and the
// reader who greps for the name finds nothing: a citation to a test rots the
// same way a line number does, one rename at a time.
//
// WHAT IT CHECKS. Every `Test...` name inside backticks on a prose surface has
// to be defined by a func in cmd/ or internal/. Fenced blocks are blanked first:
// a transcript quotes a name, it does not claim one exists.
func TestCitedTestNamesExist(t *testing.T) {
	root := repoRoot(t)
	cited := regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")
	defined := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)

	names := map[string]bool{}
	for _, file := range commentSurfaces(t, root) {
		if !strings.HasSuffix(file, "_test.go") {
			continue
		}
		for _, m := range defined.FindAllStringSubmatch(readFile(t, file), -1) {
			names[m[1]] = true
		}
	}
	if len(names) < 200 {
		t.Fatalf("found %d tests under cmd/ and internal/; this check is reading the wrong tree", len(names))
	}

	quoted := map[string]bool{}
	total := 0
	for _, file := range proseSurfaces(t, root) {
		body := withoutFencedCodeBlocks(readFile(t, file))
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatalf("relativising %s: %v", file, err)
		}
		rel = filepath.ToSlash(rel)

		for _, m := range cited.FindAllStringSubmatchIndex(body, -1) {
			name := body[m[2]:m[3]]
			total++
			quoted[name] = true
			if names[name] {
				continue
			}
			t.Errorf("%s:%d: cites %s, which no test in the tree defines; name the test that covers it, or rename this one",
				rel, 1+strings.Count(body[:m[0]], "\n"), name)
		}
	}

	t.Logf("checked %d quoted test names (%d distinct) against %d defined tests", total, len(quoted), len(names))
	if len(quoted) < 30 {
		t.Fatalf("found %d distinct test names quoted in the prose; the pattern or the surface list is wrong", len(quoted))
	}
}

// resolveLinkTarget turns a markdown link target into the file it names,
// relative to the document making the claim. A target may be percent-encoded
// (a link to a file with a space in its name), so the decoded form is tried
// before the link is called broken.
func resolveLinkTarget(fromDir, target string) (string, bool) {
	candidates := []string{target}
	if decoded, err := url.PathUnescape(target); err == nil && decoded != target {
		candidates = append(candidates, decoded)
	}
	for _, candidate := range candidates {
		path := filepath.Join(fromDir, filepath.FromSlash(candidate))
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
	}
	return "", false
}

// TestCitedSectionsExist pins the fifth kind of citation this prose makes: a
// section number.
//
// WHY IT EXISTS. A section number survives a rename and dies in a renumbering,
// and §-references are scattered through the documents and through the comments
// in the configs, the compose file and the Dockerfile -- 36 of them. Nothing read
// them, so a section that moves takes every sentence pointing at it out of reach
// while leaving those sentences standing.
//
// WHAT IT CHECKS. Every §N.M has to be a number that some document in this
// repository carries as an ATX heading. Which document a citation means is read
// from the label in front of it (`docs/DESIGN.md`, `DESIGN`, `deploy/README`);
// with no label the number is accepted if any of the documents has it, because
// this prose writes "§12.5" on a line whose paragraph named DESIGN a sentence
// earlier, and a check that demanded the label be repeated would be a check
// about style. A line naming an RFC is skipped: `RFC 7230 §6.1` is someone
// else's document.
func TestCitedSectionsExist(t *testing.T) {
	root := repoRoot(t)

	documents := []struct {
		rel    string
		labels []string
	}{
		{"README.md", []string{"README.md", "README"}},
		{"docs/DESIGN.md", []string{"docs/DESIGN.md", "DESIGN"}},
		{"docs/USAGE.md", []string{"docs/USAGE.md", "USAGE"}},
		{"docs/RESUME.md", []string{"docs/RESUME.md", "RESUME"}},
		{"docs/ACCEPTANCE.md", []string{"docs/ACCEPTANCE.md", "ACCEPTANCE"}},
		{"deploy/README.md", []string{"deploy/README.md", "deploy/README"}},
	}

	heading := regexp.MustCompile(`(?m)^#{1,6}[ \t]+§?(\d+(?:\.\d+)*)[.\s]`)
	headings := make([]map[string]bool, len(documents))
	headingTotal := 0
	for i, doc := range documents {
		path := filepath.Join(root, filepath.FromSlash(doc.rel))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s is gone, so this check no longer covers it: %v", doc.rel, err)
		}
		headings[i] = map[string]bool{}
		for _, m := range heading.FindAllStringSubmatch(readFile(t, path), -1) {
			headings[i][m[1]] = true
			headingTotal++
		}
	}
	// A heading set that came back empty or half-read would make every citation
	// below look wrong, so it is checked before it is used.
	if headingTotal < 60 {
		t.Fatalf("found %d numbered headings across %d documents; the pattern or the document list is wrong",
			headingTotal, len(documents))
	}

	section := regexp.MustCompile(`§\s*(\d+(?:\.\d+)*)`)
	rfc := regexp.MustCompile(`(?i)\bRFC[ \t]*\d`)

	total := 0
	for _, file := range proseSurfaces(t, root) {
		body := readFile(t, file)
		if strings.EqualFold(filepath.Ext(file), ".md") {
			body = withoutFencedCodeBlocks(body)
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatalf("relativising %s: %v", file, err)
		}
		rel = filepath.ToSlash(rel)

		for _, m := range section.FindAllStringSubmatchIndex(body, -1) {
			line := lineAround(body, m[0])
			if rfc.MatchString(line) {
				continue
			}
			total++

			// The label is looked for in the line itself and in the couple of
			// hundred characters before it, which is enough to reach the
			// sentence that named the document without reaching the last one.
			context := line
			if start := m[0] - 200; start > 0 {
				context = body[start:m[0]] + " " + line
			}

			var candidates []int
			for i, doc := range documents {
				for _, label := range doc.labels {
					if strings.Contains(context, label) {
						candidates = append(candidates, i)
						break
					}
				}
			}
			if len(candidates) == 0 {
				for i := range documents {
					candidates = append(candidates, i)
				}
			}

			number := body[m[2]:m[3]]
			named := make([]string, 0, len(candidates))
			found := false
			for _, i := range candidates {
				named = append(named, documents[i].rel)
				if headings[i][number] {
					found = true
				}
			}
			if found {
				continue
			}
			t.Errorf("%s:%d: cites §%s, but no heading in %s has that number",
				rel, 1+strings.Count(body[:m[0]], "\n"), number, strings.Join(named, ", "))
		}
	}

	t.Logf("checked %d section citations against %d numbered headings across %d documents",
		total, headingTotal, len(documents))
	if total < 20 {
		t.Fatalf("found %d section citations; the pattern or the surface list is wrong", total)
	}
}

// lineAround returns the text of the line that contains offset.
func lineAround(text string, offset int) string {
	start := strings.LastIndexByte(text[:offset], '\n') + 1
	end := strings.IndexByte(text[offset:], '\n')
	if end < 0 {
		return text[start:]
	}
	return text[start : offset+end]
}

// headingAnchors returns the fragments the ATX headings in text produce:
// lower-cased, every character that is not a letter, a digit, an underscore or a
// hyphen dropped, spaces turned into hyphens.
func headingAnchors(text string) map[string]bool {
	headings := regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.*)$`)

	anchors := map[string]bool{}
	for _, m := range headings.FindAllStringSubmatch(text, -1) {
		var anchor strings.Builder
		for _, r := range strings.ToLower(strings.TrimSpace(m[1])) {
			switch {
			case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-':
				anchor.WriteRune(r)
			case r == ' ':
				anchor.WriteByte('-')
			}
		}
		anchors[anchor.String()] = true
	}
	return anchors
}
