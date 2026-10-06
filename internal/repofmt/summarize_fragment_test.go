// The channel a red run uses to explain itself, exercised as a program rather
// than as a string. ci_wiring_test.go can only assert that the summarizer still
// mentions "DATA RACE"; the filter's actual behaviour is what decides whether a
// reader of the published check run learns why the run went red.
package repofmt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// transcript is shaped like what run 37414903323 published, plus a race report
// and one line that should not survive the filter. The first failure is the real
// one: `--- FAIL: TestDropClosesWithoutAResponse (0.00s)` reached the check run
// and the reason under it did not, so the publication named the test and not why.
const transcript = `=== RUN   TestDropClosesWithoutAResponse
    script_test.go:462: snapshot = {Calls:1 Dropped:0 Failed:0}, want calls=1 dropped=1 failed=0
--- FAIL: TestDropClosesWithoutAResponse (0.00s)
=== RUN   TestTraceRecordsOneRequestTrace
==================
WARNING: DATA RACE
Write at 0x00c0000b4018 by goroutine 8:
  github.com/infergate/infergate/internal/cache.(*RedisStore).stats()
      /home/runner/work/Infergate/Infergate/internal/cache/redis.go:88 +0x44

Previous read at 0x00c0000b4018 by goroutine 7:
  github.com/infergate/infergate/internal/cache.(*RedisStore).Get()
      /home/runner/work/Infergate/Infergate/internal/cache/redis.go:52 +0x30

Goroutine 8 (running) created at:
  github.com/infergate/infergate/internal/cache.TestStats()
      /home/runner/work/Infergate/Infergate/internal/cache/redis_test.go:31 +0x90
==================
    redis_test.go:31: the lock is missing
--- FAIL: TestTraceRecordsOneRequestTrace (0.11s)
2026/10/06 04:43:58 chatter from the harness that is neither a result nor a reason
FAIL
FAIL	github.com/infergate/infergate/cmd/mockupstream	0.038s
ok  	github.com/infergate/infergate/internal/gateway	33.422s
`

// TestSummarizeKeepsWhyATestFailed runs the summarizer on a real transcript,
// because the property that matters is what ends up in the fragment, not which
// words the script contains.
//
// It skips where bash or awk is missing. The Linux `go test` job has both, so
// this is a Linux-runner guarantee as well as a local one -- unlike the gate
// checker itself, which needs powershell and therefore never runs in CI on Linux.
func TestSummarizeKeepsWhyATestFailed(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is not on PATH, so the summarizer cannot be run here: %v", err)
	}
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skipf("awk is not on PATH, so the summarizer cannot be run here: %v", err)
	}
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "ci-summarize-go-test.sh")

	// tmp/ is ignored by git and nothing tracks it, so a fresh checkout does not
	// have it and MkdirTemp cannot make a child of a directory that is not there.
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatalf("creating %s: %v", tmp, err)
	}
	dir, err := os.MkdirTemp(tmp, "summarize-check-")
	if err != nil {
		t.Fatalf("making a scratch directory under %s: %v", tmp, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	logPath := filepath.Join(dir, "go-test.log")
	if err := os.WriteFile(logPath, []byte(transcript), 0o600); err != nil {
		t.Fatalf("writing the transcript: %v", err)
	}
	fragment := filepath.Join(dir, "ci-summary-test.md")

	cmd := exec.Command(bash, script, logPath, fragment, "go test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running the summarizer: %v\n%s", err, out)
	}
	body, err := os.ReadFile(fragment)
	if err != nil {
		t.Fatalf("reading the fragment: %v", err)
	}
	got := string(body)

	for _, want := range []struct{ what, text string }{
		{"the failing test", "--- FAIL: TestDropClosesWithoutAResponse (0.00s)"},
		{"the reason under it", "    script_test.go:462: snapshot = {Calls:1 Dropped:0 Failed:0}"},
		{"the package that failed", "FAIL\tgithub.com/infergate/infergate/cmd/mockupstream\t0.038s"},
		{"the race report's first access", "redis.go:88 +0x44"},
		{"the state the two accesses shared", "Previous read at 0x00c0000b4018 by goroutine 7:"},
		{"the frame that created the goroutine", "Goroutine 8 (running) created at:"},
		{"a reason a race report needs", "redis_test.go:31: the lock is missing"},
		{"the passing package", "ok  \tgithub.com/infergate/infergate/internal/gateway\t33.422s"},
	} {
		if !strings.Contains(got, want.text) {
			t.Errorf("the fragment is missing %s:\n\t%q\nfragment:\n%s", want.what, want.text, got)
		}
	}

	// And the filter still filters: a summary that keeps everything is a log.
	if strings.Contains(got, "chatter from the harness") {
		t.Errorf("the fragment carried a line with no result in it:\n%s", got)
	}
}
