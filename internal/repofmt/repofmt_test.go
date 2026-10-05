// Package repofmt holds the check that has no source to test: it asserts a
// property of the whole repository rather than of a package, and it lives in a
// package of its own so it cannot drag a build dependency into anything.
package repofmt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoFmtClean pins the formatting gate CI enforces, where it is cheapest to
// run and where a failure names the offending file.
//
// WHY IT EXISTS AS A TEST. Adding the gate to CI was not enough to find the
// drift: at the moment it was added, seven files under cmd/ and internal/ were
// not gofmt-clean, and nobody had noticed because gofmt only ever ran on the file
// being edited. A repository-wide property belongs in a repository-wide check,
// and running it locally means the answer arrives in a second rather than after a
// push and a queue.
//
// WHY IT WALKS THE TREE. The obvious implementation shells out to
// "gofmt -l ." from the root, which is wrong HERE in a way that generalises: the
// vendored toolchain and the module cache live under this same directory
// (.gotoolchain, .gomodcache) and are full of Go testdata -- deliberately
// unformatted, some of it not even valid Go. The listing then buries the real
// answer under hundreds of lines of the standard library. The directories below
// that can contain this module's sources are named explicitly instead.
func TestGoFmtClean(t *testing.T) {
	gofmt, err := exec.LookPath("gofmt")
	if err != nil {
		t.Skipf("gofmt is not on PATH, so this check cannot run: %v", err)
	}
	root := repoRoot(t)

	// Only directories that hold this module's own sources. packages/ predates
	// the current layout; it is listed rather than assumed so re-adding one does
	// not silently drop out of the check.
	roots := []string{"cmd", "internal", "packages"}
	existing := make([]string, 0, len(roots))
	for _, dir := range roots {
		if fi, err := os.Stat(filepath.Join(root, dir)); err == nil && fi.IsDir() {
			existing = append(existing, filepath.Join(root, dir))
		}
	}
	if len(existing) == 0 {
		t.Fatalf("none of %v exists under %s; the check is looking in the wrong place", roots, root)
	}

	var unformatted []string
	for _, dir := range existing {
		out, err := exec.Command(gofmt, "-l", dir).CombinedOutput()
		if err != nil {
			t.Fatalf("gofmt -l %s: %v\n%s", dir, err, out)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				if rel, err := filepath.Rel(root, line); err == nil {
					line = rel
				}
				unformatted = append(unformatted, filepath.ToSlash(line))
			}
		}
	}
	if len(unformatted) > 0 {
		t.Errorf("%d file(s) are not gofmt-clean; run: gofmt -w ./cmd ./internal\n%s",
			len(unformatted), strings.Join(unformatted, "\n"))
	}
}

// repoRoot walks up from the test's working directory until it finds go.mod.
//
// The test runs with its own package directory as the working directory, so
// reaching the repository root means walking up; hard-coding ".." would break the
// moment this package moves, and the failure would look like a formatting problem
// rather than a path one.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
