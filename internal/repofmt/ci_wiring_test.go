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
// THE HISTORY, because it is the reason this test exists at all. The first
// `go test -race` failure on Linux produced one annotation, "Process completed with
// exit code 1", and the job log needs admin rights on this repository. The fix
// looked obvious: pipe `go test` through a `grep` so the FAIL lines land in the
// step log. The second failure then produced the same annotation, because the
// problem was never where the lines went -- it was that the step log is behind
// authentication. The step summary is not: it comes back in check-runs
// `output.summary` to any reader who can see the repository.
//
// So the assertions below are about the three things that make that work, and each
// one has been true-and-broken at some point: the transcript goes to a file, part
// of it is appended to "$GITHUB_STEP_SUMMARY", and the summarizing step runs even
// when the test step failed. Drop any of the three and a red run is unreadable
// again.
func TestRedRunPublishesAReadableTranscript(t *testing.T) {
	root := repoRoot(t)
	workflowBytes, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("reading the workflow: %v", err)
	}
	summaryBytes, err := os.ReadFile(filepath.Join(root, "scripts", "ci-summarize-go-test.sh"))
	if err != nil {
		t.Fatalf("reading the summarizer: %v", err)
	}
	workflow := string(workflowBytes)
	summary := string(summaryBytes)

	// The transcript has to exist as a file for the summarizer to have anything to
	// read. The old shape piped it through grep and never kept it.
	if !strings.Contains(workflow, "/tmp/go-test-race.log") {
		t.Error("ci.yml no longer keeps the race transcript in /tmp/go-test-race.log; " +
			"a summary cannot be built from a pipe")
	}
	if !strings.Contains(workflow, "ci-summarize-go-test.sh") {
		t.Error("ci.yml no longer calls scripts/ci-summarize-go-test.sh")
	}
	if !strings.Contains(workflow, "GITHUB_STEP_SUMMARY") {
		t.Error("ci.yml no longer appends to $GITHUB_STEP_SUMMARY, which is the only " +
			"copy of a red run's reason that a reader without admin rights can get")
	}

	// `if: always()` is what makes the summary appear on the failing run instead of
	// only on the passing one -- the exact mistake that would leave this gate silent
	// precisely when it matters.
	if strings.Count(workflow, "if: always()") < 3 {
		t.Errorf("ci.yml has %d `if: always()` steps; the two summarize steps and the "+
			"artifact upload all need one, or the summary only appears when nothing failed",
			strings.Count(workflow, "if: always()"))
	}

	// A pipe through grep was the previous attempt. It would satisfy the three
	// checks above while still sending the output only to the step log. Only
	// uncommented lines count: ci.yml talks about that attempt at length.
	for _, line := range strings.Split(workflow, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "grep") {
			t.Errorf("ci.yml has an uncommented grep in a step: %q", trimmed)
		}
	}

	// The summarizer has to carry the race report, not just the words "DATA RACE".
	// The frames below the blank line are the reason the report exists.
	if !strings.Contains(summary, "DATA RACE") {
		t.Error("the summarizer no longer looks for the race detector's banner")
	}
	if !strings.Contains(summary, "panic:") {
		t.Error("the summarizer no longer looks for a panic")
	}
	if !strings.Contains(summary, `"$summary_file"`) {
		t.Error("the summarizer no longer writes to its second argument, so the step " +
			"summary would stay empty")
	}
}
