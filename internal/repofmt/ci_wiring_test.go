// CI-wiring checks that have no Go source to attach to. These are deliberately
// shallow -- they read two files and look for strings -- because the property they
// protect is not a behaviour of this module, it is a promise about what a red run
// publishes.
package repofmt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRedRunPublishesAReadableTranscript pins the shape of the fix for a gate
// nobody could read.
//
// THE HISTORY, because every step of it looked like a fix and was not. The first
// `go test -race` failure on Linux produced one annotation, "Process completed with
// exit code 1", and the job log needs admin rights on this repository. The first
// attempt piped `go test` through a `grep` so the FAIL lines landed in the step log
// -- which only helped readers who were already allowed to open it, and the second
// failure produced the same annotation. The second attempt wrote
// $GITHUB_STEP_SUMMARY, which is the documented way and does render in the UI for a
// signed-in reader: it does NOT populate check-runs `output.summary`, so the
// check-run API and the anonymous job page both served nothing.
//
// The third attempt PATCHed the job's own check run, which does work -- `gh api`
// returned 2xx with the token supplied explicitly -- and is then wiped when the job
// completes. A job that ran afterwards PATCHed `output.summary` and read it back as
// null from outside (run 37377940124). Check-runs output is not a channel a workflow
// can hand an anonymous reader anything through, and the assertions below exist so
// that detour does not come back.
//
// So the honest chain is what this test now pins: the transcript lands in a file,
// the summarizer turns it into the job's step summary (readable to any signed-in
// reader, without downloading anything), the raw transcript is uploaded as an
// artifact, and every step that has to run after a failure carries `if: always()`.
//
// And one thing that IS anonymous, which is why the race gate runs package by
// package: the step list of a run comes back from the plain public API. A single
// `go test -race ./...` reports one status for the whole module, which answers "is
// the detector unhappy" and not "where"; a red step named after the package does.
func TestRedRunPublishesAReadableTranscript(t *testing.T) {
	root := repoRoot(t)
	workflow := readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	summarize := readFile(t, filepath.Join(root, "scripts", "ci-summarize-go-test.sh"))

	// Link 1: the transcript has to exist as a file for anything to summarize. The
	// grep attempt piped it and never kept it.
	if !strings.Contains(workflow, "/tmp/go-test-race.log") {
		t.Error("ci.yml no longer keeps the race transcript in /tmp/go-test-race.log; " +
			"a summary cannot be built from a pipe")
	}

	// Only the lines a runner would execute count. A `#` line is a comment and
	// everything inside a `run: |` block is shell -- and this workflow discusses the
	// mistakes it is avoiding in both, which is the point of it.
	var executable []string
	blockIndent := -1
	for _, line := range strings.Split(workflow, "\n") {
		code := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if blockIndent >= 0 {
			if code == "" || indent >= blockIndent {
				continue
			}
			blockIndent = -1
		}
		if code == "" || strings.HasPrefix(code, "#") {
			continue
		}
		if i := strings.Index(code, "run:"); i >= 0 && strings.ContainsAny(code[i:], "|>") {
			blockIndent = indent + 2
		}
		executable = append(executable, code)
	}
	runs := strings.Join(executable, "\n")

	// Link 2: the summary is written, and it is written where the UI reads it. The
	// second argument has to be the runner's own summary file -- writing to a
	// fragment and PATCHing a check run with it is the detour described above.
	// Scanned on the whole file rather than on `runs`: the write happens inside a
	// `run: |` block, which the tokenizer deliberately treats as opaque shell.
	if !strings.Contains(workflow, "GITHUB_STEP_SUMMARY") ||
		!strings.Contains(workflow, "scripts/ci-summarize-go-test.sh") {
		t.Error("ci.yml no longer passes $GITHUB_STEP_SUMMARY to the summarizer; a red " +
			"run's failing test names then reach no reader at all")
	}

	// A pipe through grep was the first attempt.
	for _, line := range executable {
		if strings.Contains(line, "grep") {
			t.Errorf("ci.yml has an executable grep: %q", line)
		}
	}

	// Link 3: the race gate has to fail per package. The step list is the only part
	// of a red run someone without admin rights can read, so `go test -race ./...`
	// would say "somewhere in 23 packages" while a per-package loop names the one.
	if !strings.Contains(workflow, "go list ./...") {
		t.Error("the race step no longer enumerates packages: a red run would then not " +
			"say WHICH package the detector rejected, and the step list is the only " +
			"anonymous channel there is")
	}

	// Link 4: the detour must not return. This is a negative assertion about a
	// channel that was measured: the field is wiped when the owning job completes.
	if strings.Contains(runs, "check-runs") || strings.Contains(runs, "checks: write") {
		t.Error("ci.yml is publishing to check runs again; a job's check-run output is " +
			"wiped when the job completes, so that text reaches nobody (see the comment " +
			"above the summarize steps)")
	}

	// Link 5: the summary has to be produced on the failing run. This is the mistake
	// that leaves the gate silent precisely when it matters.
	if n := strings.Count(workflow, "if: always()"); n < 3 {
		t.Errorf("ci.yml has %d `if: always()` steps; both summarize steps and the "+
			"artifact upload need one, or the summary only appears when nothing "+
			"failed", n)
	}

	// The summarizer has to carry the race report, not just the words "DATA RACE".
	// The frames below the blank line are the reason the report exists, and a
	// line-matching filter is exactly what throws them away.
	if !strings.Contains(summarize, "DATA RACE") {
		t.Error("the summarizer no longer looks for the race detector's banner")
	}
	if !strings.Contains(summarize, "panic:") {
		t.Error("the summarizer no longer looks for a panic")
	}
	if !strings.Contains(summarize, `"$summary_file"`) {
		t.Error("the summarizer no longer writes to its second argument")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}
