package repofmt

import (
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
// tree: `go test` output naming script_test.go:462 and a sample config naming an
// illustrative file both belong there. Prose is where a path claims to exist.
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
func TestProseCitesSymbolsNotLineNumbers(t *testing.T) {
	root := repoRoot(t)
	surfaces := proseSurfaces(t, root)

	// The leading character is part of the match because RE2 has no lookbehind;
	// it is what stops a URL path from being read as a citation.
	lineNumber := regexp.MustCompile(`(?:^|[^\w./\\-])([\w.\\/-]*[\w-]\.(?:go|md|ps1|sh|yaml|yml|json)):\d+`)
	// Every file the prose names at all, cited by line or not. This is the
	// denominator: if it collapses, the pattern above broke and the check is
	// passing because it is looking at nothing.
	mentions := regexp.MustCompile(`[\w.\\/-]+\.(?:go|md|ps1|sh|yaml|yml|json)`)

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

		total += len(mentions.FindAllString(body, -1))
		for _, m := range lineNumber.FindAllStringSubmatchIndex(body, -1) {
			// m[2]..m[1] is the citation itself: the boundary character sits
			// before m[2] and is not part of the claim being reported.
			citation := body[m[2]:m[1]]
			line := 1 + strings.Count(body[:m[2]], "\n")
			offenders = append(offenders, rel+":"+strconv.Itoa(line)+": cites "+citation+
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
