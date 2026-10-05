// CI-wiring checks that have no Go source to attach to. These are deliberately
// shallow -- they read three files and look for strings -- because the property they
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
// $GITHUB_STEP_SUMMARY, which is the documented way and does render in the UI: it
// does NOT populate check-runs `output.summary`, so the check-run API and the job
// page both served an anonymous client nothing.
//
// What works is PATCHing the check run, because `PATCH /check-runs/{id}`'s
// `output.summary` is the one field a public API reader can get. So the assertions
// below are about the four links in that chain, each of which has been missing at
// some point: the transcript exists as a file, something summarizes it into a
// fragment, something publishes the fragments to the check run, and the publishing
// step runs when a previous step failed.
func TestRedRunPublishesAReadableTranscript(t *testing.T) {
	root := repoRoot(t)
	workflow := readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	summarize := readFile(t, filepath.Join(root, "scripts", "ci-summarize-go-test.sh"))
	publish := readFile(t, filepath.Join(root, "scripts", "ci-publish-failure-summary.sh"))

	// Link 1: the transcript has to exist as a file for anything to summarize. The
	// grep attempt piped it and never kept it.
	if !strings.Contains(workflow, "/tmp/go-test-race.log") {
		t.Error("ci.yml no longer keeps the race transcript in /tmp/go-test-race.log; " +
			"a summary cannot be built from a pipe")
	}

	// Link 2: it goes to a fragment, not into $GITHUB_STEP_SUMMARY directly. The
	// indirection is what lets one step publish all of them together, and doing it
	// the direct way was the attempt that looked right and was unreadable.
	for _, frag := range []string{"/tmp/ci-summary/race.md", "/tmp/ci-summary/test.md"} {
		if !strings.Contains(workflow, frag) {
			t.Errorf("ci.yml no longer writes a summary fragment to %s", frag)
		}
	}
	// Two checks below read only the lines that actually run. A `#` line is a
	// comment, and everything inside a `run: |` block is shell -- and this workflow
	// discusses the mistakes it is avoiding in both, which is the point of it. So
	// what is scanned is the YAML a runner would execute, not the prose around it.
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

	if strings.Contains(strings.Join(executable, "\n"), "GITHUB_STEP_SUMMARY") {
		t.Error("ci.yml runs a step that writes to $GITHUB_STEP_SUMMARY, which renders " +
			"in the UI but does not populate check-runs output.summary -- i.e. not " +
			"readable by an anonymous client. Publish through " +
			"scripts/ci-publish-failure-summary.sh")
	}

	// A pipe through grep was the first attempt.
	for _, line := range executable {
		if strings.Contains(line, "grep") {
			t.Errorf("ci.yml has an executable grep: %q", line)
		}
	}

	// Link 3: the fragments reach the check run.
	if !strings.Contains(workflow, "ci-publish-failure-summary.sh") {
		t.Error("ci.yml no longer calls scripts/ci-publish-failure-summary.sh, so the " +
			"failure output never reaches check-runs output.summary")
	}
	if !strings.Contains(publish, "check-runs/${id}") || !strings.Contains(publish, "output.summary") {
		t.Error("the publisher no longer PATCHes a check run's output.summary")
	}
	if !strings.Contains(publish, "--rawfile") {
		t.Error("the publisher builds its JSON without --rawfile; an interpolated " +
			"transcript would break on the first quote or backslash in a Go stack trace")
	}
	// jq is optional and must stay optional. The publisher's first real run reported
	// success while leaving output.summary empty: it demanded jq, jq was not there,
	// and the step exited 0 without saying so publicly. The gh-only fallback is what
	// makes it work on such a runner; dropping it empties the summary again.
	if !strings.Contains(publish, "command -v jq") {
		t.Error("the publisher no longer treats jq as optional; on a runner without " +
			"it the step still exits 0 and publishes nothing")
	}
	if !strings.Contains(publish, "--raw-field") {
		t.Error("the publisher lost the gh-only path that builds the JSON without jq")
	}

	// Link 4: publishing has to happen on the failing run. This is the mistake that
	// leaves the gate silent precisely when it matters.
	if strings.Count(workflow, "if: always()") < 4 {
		t.Errorf("ci.yml has %d `if: always()` steps; both summarize steps, the publish "+
			"step and the artifact upload need one, or the summary only appears when "+
			"nothing failed", strings.Count(workflow, "if: always()"))
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
