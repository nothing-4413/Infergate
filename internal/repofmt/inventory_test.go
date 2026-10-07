package repofmt

import (
	"os"
	"path/filepath"
	"regexp"
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

	configs, err := filepath.Glob(filepath.Join(root, "configs", "*.yaml"))
	if err != nil {
		t.Fatalf("globbing configs/*.yaml: %v", err)
	}
	// A walk that stopped early and a glob that matched nothing would make every
	// sentence below look wrong, so both are checked before they are used.
	if len(packages) < 10 || len(configs) < 5 {
		t.Fatalf("found %d packages and %d configs; the walk or the glob is wrong",
			len(packages), len(configs))
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
