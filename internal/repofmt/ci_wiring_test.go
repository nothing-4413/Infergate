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
// null from outside (run 37377940124). Patching GitHub's own check run for a job is
// not a channel a workflow can hand an anonymous reader anything through.
//
// What that leaves, and what this test pins, is two channels plus one invariant:
//
//   - the transcript lands in a file, the summarizer turns it into a fragment, and
//     that fragment is APPENDED to $GITHUB_STEP_SUMMARY (any signed-in reader, no
//     download) -- and published, unchanged, into a check run the workflow CREATES
//     (any anonymous reader via the public API). Creating is the difference that
//     matters: run 37379724217 created a probe check run whose summary was still
//     readable from outside after the run had finished.
//   - the raw transcript is uploaded as an artifact.
//   - every step that has to run after a failure carries `if: always()`.
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
	// summarizer writes a fragment file, and ci.yml appends that same file to the
	// runner's summary, so the signed-in copy and the anonymous copy cannot drift.
	// Scanned on the whole file rather than on `runs`: both writes happen inside a
	// `run: |` block, which the tokenizer deliberately treats as opaque shell.
	if !strings.Contains(workflow, "GITHUB_STEP_SUMMARY") ||
		!strings.Contains(workflow, "scripts/ci-summarize-go-test.sh") {
		t.Error("ci.yml no longer passes $GITHUB_STEP_SUMMARY to the summarizer; a red " +
			"run's failing test names then reach no reader at all")
	}
	if !strings.Contains(workflow, "/tmp/ci-summary-race.md") ||
		!strings.Contains(workflow, "ci-publish-failure-check.sh") {
		t.Error("ci.yml no longer publishes the race fragment into a check run; that is " +
			"the only copy of the failure text an anonymous reader can fetch")
	}

	// A pipe through grep was the first attempt.
	for _, line := range executable {
		if strings.Contains(line, "grep") {
			t.Errorf("ci.yml has an executable grep: %q", line)
		}
	}

	// Link 3: the race gate enumerates the packages it runs. This buys a complete
	// picture in one run for a reader who signs in -- every rejected package rather
	// than the first one -- and NOT an anonymous package name, which the step list
	// does not expose. See the comment on the step.
	if !strings.Contains(workflow, "go list ./...") {
		t.Error("the race step no longer enumerates packages, so one run stops at the " +
			"first rejected package instead of reporting all of them")
	}

	// Link 4: the wiped channel must not come back, and the working one must keep
	// using POST. PATCHing GitHub's own check run for this job returns 2xx and is
	// erased when the job ends (run 37377940124); creating a check run of our own
	// is what survives (run 37379724217).
	publisher := readFile(t, filepath.Join(root, "scripts", "ci-publish-failure-check.sh"))
	if !strings.Contains(publisher, "--method POST") {
		t.Error("the publisher no longer CREATES its check run; patching an existing " +
			"check run is the variant that gets wiped when the job completes")
	}
	if strings.Contains(runs, "--method PATCH") || strings.Contains(runs, "ci-publish-failure-summary.sh") {
		t.Error("ci.yml is patching a check run again; the output of GitHub's own check " +
			"run for a job is wiped when that job completes (run 37377940124)")
	}

	// Link 5: the summary has to be produced on the failing run. This is the mistake
	// that leaves the gate silent precisely when it matters.
	if n := strings.Count(workflow, "if: always()"); n < 4 {
		t.Errorf("ci.yml has %d `if: always()` steps; both summarize steps, the artifact "+
			"upload and the publish step need one, or the summary only appears when "+
			"nothing failed", n)
	}
	if !strings.Contains(workflow, "if: failure()") {
		t.Error("the publish step no longer runs on failure; that is the only case in " +
			"which there is anything to publish")
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
