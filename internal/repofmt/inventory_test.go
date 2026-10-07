package repofmt

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestCitedTreeCountsMatchTheTree pins the two numbers the prose uses to
// describe the size of the tree: how many packages it holds and how many shipped
// configs.
//
// WHY IT EXISTS. These are the numbers nobody recomputes. README.md told a
// reader the suite covers "约 90 个 Go 文件" while the tree held 137, and no gate
// saw it because it was not a gate total (605e4b5 removed that one instead of
// pinning it, since a count of files is not a claim worth maintaining). "36 个包"
// and "14 份配置" are the same kind of sentence, written in four and three
// places, and adding a package or a config file is a normal thing to do here.
// Unlike the file count, both have a denominator that is cheap to compute and
// hard to argue with: a directory listing and a glob.
//
// WHAT IT CHECKS. A package is a directory under cmd/ or internal/ that holds at
// least one Go file, which is what `go list ./...` reports apart from the
// directories Go itself ignores; a shipped config is a configs/*.yaml. Every
// `N 个包` and every `N 份…配置` on a prose surface has to agree with those. What
// it does not check is the history in between: sentences recording a past run
// ("36 个包、35 绿、1 红") are pinned like any other, and that is deliberate,
// because the tree those runs describe is this tree.
//
// WHY IT READS FENCED BLOCKS TOO. The other prose checks blank fenced code blocks
// first, on the grounds that a fence quotes something that happened rather than
// claiming something now. The count a reader meets first here is the opposite: it
// is README.md's directory listing, a claim about today's tree dressed as
// sample output. Blanking fences would leave exactly that one unpinned (it did,
// at first: six claims checked instead of seven), so this check reads them, and
// the seven claims it finds are all true.
func TestCitedTreeCountsMatchTheTree(t *testing.T) {
	root := repoRoot(t)
	packages := packageDirs(t, root)

	configs, err := filepath.Glob(filepath.Join(root, "configs", "*.yaml"))
	if err != nil {
		t.Fatalf("globbing configs/*.yaml: %v", err)
	}
	// A glob that matched nothing would make every sentence below look wrong, so
	// it is checked before it is used.
	if len(configs) < 5 {
		t.Fatalf("found %d configs; the glob is wrong", len(configs))
	}

	packageClaim := regexp.MustCompile(`(\d+)\s*个包`)
	configClaim := regexp.MustCompile(`(\d+)\s*份[^\n]{0,10}配置`)

	claims := 0
	for _, file := range proseSurfaces(t, root) {
		body := readFile(t, file)
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatalf("relativising %s: %v", file, err)
		}
		rel = filepath.ToSlash(rel)

		for _, m := range packageClaim.FindAllStringSubmatchIndex(body, -1) {
			claims++
			if atoi(t, rel, body[m[2]:m[3]]) == len(packages) {
				continue
			}
			t.Errorf("%s:%d: says %s packages; cmd/ and internal/ hold %d directories with Go files",
				rel, lineOf(body, m[0]), body[m[2]:m[3]], len(packages))
		}
		for _, m := range configClaim.FindAllStringSubmatchIndex(body, -1) {
			claims++
			if atoi(t, rel, body[m[2]:m[3]]) == len(configs) {
				continue
			}
			t.Errorf("%s:%d: says %s configs; configs/ holds %d yaml files",
				rel, lineOf(body, m[0]), body[m[2]:m[3]], len(configs))
		}
	}

	t.Logf("checked %d tree counts (%d packages, %d configs) across the prose surfaces",
		claims, len(packages), len(configs))
	if claims < 4 {
		t.Fatalf("found %d tree counts; the pattern or the surface list is wrong", claims)
	}
}

// packageDirs returns the directories under cmd/ and internal/, relative to root
// and slash-separated, that hold at least one Go file. Both the walk and the
// count are checked, so a walk that stopped early cannot make a check that uses
// this look like it agrees with the prose.
func packageDirs(t *testing.T, root string) map[string]bool {
	t.Helper()
	packages := map[string]bool{}
	for _, top := range []string{"cmd", "internal"} {
		dir := filepath.Join(root, top)
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s is gone, so this check no longer covers it: %v", top, err)
		}
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return nil
			}
			if path != dir {
				name := info.Name()
				if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" {
					return filepath.SkipDir
				}
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
					rel, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					packages[filepath.ToSlash(rel)] = true
					break
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", top, err)
		}
	}
	if len(packages) < 10 {
		t.Fatalf("found %d packages under cmd/ and internal/; the walk is wrong", len(packages))
	}
	return packages
}

// TestReadmeTreeNamesEveryPackage pins the other half of README.md's directory
// listing: the test above pins the numbers beside it, this one pins the
// enumeration itself. Adding a package is a normal thing to do here, and
// internal/repofmt was absent from the listing while the other twenty-two
// internal packages were named.
//
// WHAT IT CHECKS. Every directory under cmd/ or internal/ that holds Go files has
// its name in the tree block of README.md, either written out ("repofmt/") or
// covered by a range ("verify-m1..m6", which names verify-m1 through verify-m6).
// Comments are dropped before the names are read: a name in a comment is not a
// name in the tree.
func TestReadmeTreeNamesEveryPackage(t *testing.T) {
	root := repoRoot(t)
	packages := packageDirs(t, root)

	const rel = "README.md"
	block := fencedBlockHolding(readFile(t, filepath.Join(root, rel)), "\u251C\u2500\u2500 cmd/")
	if block == "" {
		t.Fatalf("%s no longer holds a cmd/ listing, so this check no longer covers it", rel)
	}
	named := treeNames(block)

	missing := make([]string, 0, len(packages))
	for path := range packages {
		if name := filepath.Base(filepath.FromSlash(path)); !named[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s: the cmd/ and internal/ listing does not name %s; name each package where it belongs in the listing, or cover it with a range like \"verify-m1..m6\"",
			rel, strings.Join(missing, ", "))
	}

	if len(named) < 20 {
		t.Fatalf("found %d names in the %s listing; the block or the pattern is wrong", len(named), rel)
	}
	t.Logf("the %s listing names %d names; cmd/ and internal/ hold %d packages", rel, len(named), len(packages))
}

// fencedBlockHolding returns the body of the first fenced block in body that
// holds marker, or "" when there is none. A fence opens with any run of
// backticks, which is how a block that names its language ("```powershell")
// stays a fence instead of flipping the parity of every block after it.
func fencedBlockHolding(body, marker string) string {
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		if !isFence(lines[i]) {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if !isFence(lines[j]) {
				continue
			}
			block := strings.Join(lines[i+1:j], "\n")
			if strings.Contains(block, marker) {
				return block
			}
			i = j
			break
		}
	}
	return ""
}

// isFence reports whether a line opens or closes a fenced code block.
func isFence(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "```")
}

// treeNames collects the names a directory listing mentions: every word in the
// structure part of the block, with the comments dropped, plus the names a range
// like "verify-m1..m6" stands for.
func treeNames(block string) map[string]bool {
	structure := make([]string, 0, strings.Count(block, "\n")+1)
	for _, line := range strings.Split(block, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		structure = append(structure, line)
	}
	text := strings.Join(structure, "\n")

	names := map[string]bool{}
	for _, name := range treeName.FindAllString(text, -1) {
		names[name] = true
	}
	for _, m := range treeRange.FindAllStringSubmatch(text, -1) {
		prefix, suffix := m[1], m[3]
		from, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		to, err := strconv.Atoi(m[4])
		if err != nil || to <= from || to-from > 100 {
			continue
		}
		if suffix != "" && !strings.HasSuffix(prefix, suffix) {
			continue
		}
		for n := from; n <= to; n++ {
			names[prefix+strconv.Itoa(n)] = true
		}
	}
	return names
}

var (
	// treeName is one word of a directory listing, as in "repofmt/".
	treeName = regexp.MustCompile(`[a-z0-9][a-z0-9-]*`)
	// treeRange is the shorthand a listing uses for a run of packages, as in
	// "verify-m1..m6" for verify-m1 through verify-m6.
	treeRange = regexp.MustCompile(`([a-z][a-z0-9-]*?)(\d+)\.\.([a-z]*)(\d+)`)
)
