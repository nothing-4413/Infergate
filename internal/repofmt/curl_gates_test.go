// The curl-gates job has two properties that no other file can express, and both
// of them were wrong or absent until they were looked up in the docs.
//
// THE FIRST is that a red gate has to be able to fail the job. Every gate step
// carries `continue-on-error: true` so that one red gate does not hide the other
// seven, and GitHub documents exactly what that costs:
//
//	"When a continue-on-error step fails, the outcome is failure, but the final
//	 conclusion is success."
//	-- contexts reference, steps.<step_id>.outcome / .conclusion
//
// With the flag on every gate step and no step that is allowed to fail, all 1060
// curl assertions and all 2353 in-process ones could be red while the job, the run,
// the badge and the anonymous jobs API all reported success -- and those step
// conclusions are the only surface a reader outside the repository can see.
//
// THE SECOND is that the list of gates the check is told to expect has to be the
// list of gates that actually run. A gate renamed on one side only would be
// reported as red forever, or -- worse, in the other direction -- a gate that
// silently stopped being wired up would be reported as green because nobody was
// expecting it.
//
// There are two wrappers now, because there are two acceptance chains: the eight
// curl scripts and the seven in-process `go run .\cmd\verify*` gates. They share the
// marker convention and the counterweight, so they are checked together here rather
// than in two tests that could drift apart.
package repofmt

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	// The gate a step runs, and the name it records itself under. Both wrappers are
	// matched: run-gate.ps1 drives one PowerShell script, run-go-verify.ps1 drives
	// the seven Go milestones.
	gateNameRe = regexp.MustCompile(`run-(?:gate|go-verify)\.ps1[^\n]*?-Name\s+(\S+)`)
	// The step that is told which gates to expect.
	gateExpectRe = regexp.MustCompile(`summarize-gates\.ps1[^\n]*?-Gates\s+(\S+)`)
	// The milestones the Go gate is asked to run.
	gatePackageRe = regexp.MustCompile(`run-go-verify\.ps1[^\n]*?-Package\s+(\S+)`)
)

// goVerifyPackages is the in-process acceptance chain, which used to have no CI step
// at all. The list is spelled out rather than derived from the directory so that
// dropping one of them is a test failure: a milestone that stops being run is
// indistinguishable from a milestone that passes unless something knows it was
// supposed to run.
var goVerifyPackages = []string{
	"./cmd/verify",
	"./cmd/verify-m1",
	"./cmd/verify-m2",
	"./cmd/verify-m3",
	"./cmd/verify-m4",
	"./cmd/verify-m5",
	"./cmd/verify-m6",
}

func TestEveryCurlGateCanFailTheJob(t *testing.T) {
	root := repoRoot(t)
	workflow := readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	steps := curlGateSteps(t, workflow)

	var names []string
	var packages []string
	var expected string
	checkers := 0
	for _, step := range steps {
		guarded := strings.Contains(step, "continue-on-error: true")
		if m := gateNameRe.FindStringSubmatch(step); m != nil {
			names = append(names, m[1])
			if !guarded {
				t.Errorf("gate %q does not carry continue-on-error, so the first red gate stops "+
					"the gates after it and one red run stops reporting the whole picture", m[1])
			}
		}
		if m := gatePackageRe.FindStringSubmatch(step); m != nil {
			for _, pkg := range strings.Split(m[1], ",") {
				if pkg = strings.TrimSpace(pkg); pkg != "" {
					packages = append(packages, pkg)
				}
			}
			if !guarded {
				t.Errorf("the Go gate step does not carry continue-on-error, so the first red "+
					"milestone hides the six after it: %s", m[1])
			}
		}
		if m := gateExpectRe.FindStringSubmatch(step); m != nil {
			checkers++
			expected = m[1]
			if guarded {
				t.Error("the step that checks every gate carries continue-on-error itself, which " +
					"is the one flag that makes it unable to fail the job: no step could then fail it")
			}
			if !strings.Contains(step, "if: always()") {
				t.Error("the gate check has no `if: always()`, so it is skipped exactly in the case " +
					"it exists for -- an earlier step that never ran and left no marker")
			}
		}
	}

	if len(names) < 9 {
		t.Fatalf("only %d gate(s) invoke a gate wrapper; the Windows acceptance chain is eight "+
			"curl scripts plus the seven-milestone Go gate, and a shorter list means this check "+
			"is looking at a truncated job", len(names))
	}
	if checkers != 1 {
		t.Fatalf("%d step(s) check the gates together; exactly one of them has to be able to fail "+
			"the job, and a run is only red if one of them can", checkers)
	}
	if got := strings.Join(names, ","); got != expected {
		t.Errorf("the gates that run are %q but the job expects %q.\nA gate renamed on one side "+
			"only is reported red forever; a gate whose -Gates entry was removed is reported "+
			"green without anyone having checked it.", got, expected)
	}

	// The Go gate runs the whole in-process chain or it verifies a subset nobody
	// chose. Sorted on both sides because the step's own order is the milestone
	// order, which is not alphabetical.
	want := append([]string(nil), goVerifyPackages...)
	gotPkgs := append([]string(nil), packages...)
	sort.Strings(want)
	sort.Strings(gotPkgs)
	if strings.Join(gotPkgs, ",") != strings.Join(want, ",") {
		t.Errorf("the Go gate runs %v but should run %v.\nA milestone missing from this list "+
			"stops being checked while the job stays green, and nothing else in the repository "+
			"knows it was supposed to run.", gotPkgs, want)
	}

	// The channel between the gate steps and the one that can fail: a file,
	// because environment variables do not cross steps and a child process cannot
	// write its parent's $GITHUB_OUTPUT.
	for _, f := range []struct{ path, body string }{
		{filepath.Join(root, "scripts", "lib", "run-gate.ps1"), readFile(t, filepath.Join(root, "scripts", "lib", "run-gate.ps1"))},
		{filepath.Join(root, "scripts", "lib", "run-go-verify.ps1"), readFile(t, filepath.Join(root, "scripts", "lib", "run-go-verify.ps1"))},
		{filepath.Join(root, "scripts", "lib", "summarize-gates.ps1"), readFile(t, filepath.Join(root, "scripts", "lib", "summarize-gates.ps1"))},
	} {
		if !strings.Contains(strings.ToLower(f.body), "gate-$name.exit") {
			t.Errorf("%s no longer uses tmp\\gate-<name>.exit; that marker is the only channel "+
				"that carries a gate's exit code to the step allowed to fail the job", f.path)
		}
	}
}

// TestSummarizeGatesFailsClosed runs the checker itself, because the property that
// matters is an exit code, not a string. It skips where powershell is absent --
// which is every CI run of the Linux job, so this is a local and Windows-runner
// guarantee rather than a Linux one.
//
// Every file it makes lives under tmp/, which is the directory the checker is
// pointed at in ci.yml, and which is inside the repository: on this machine writes
// under %TEMP% come back "Access is denied", and a PowerShell child that cannot
// write where it was told turns into a forty-line debugging session about a script
// that was fine.
func TestSummarizeGatesFailsClosed(t *testing.T) {
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skipf("powershell is not on PATH, so the gate checker cannot be exercised here: %v", err)
	}
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "lib", "summarize-gates.ps1")

	cases := []struct {
		what    string
		markers map[string]string
		expect  string
		want    int
		// wantIn is what the artifact has to say about the failure. The exit code
		// alone tells a reader of the artifact nothing, and the artifact is the
		// only thing that survives the runner.
		wantIn string
	}{
		{"every gate reported zero", map[string]string{"a": "0", "b": "0"}, "a,b", 0, ""},
		{"one gate reported one", map[string]string{"a": "0", "b": "1"}, "a,b", 1, "## gate b : exit 1"},
		{"one gate never reported", map[string]string{"a": "0"}, "a,b", 1, "no marker file"},
		{"a marker is unreadable", map[string]string{"a": "0", "b": "not a number"}, "a,b", 1, "unreadable marker content"},
		// A comma with no names is what a truncated -Gates list looks like, and it
		// has to be red: a check that verifies nothing must not report success.
		{"no gate was listed", map[string]string{"a": "0"}, ",", 1, "no gate was named"},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			// Not t.TempDir(), which resolves to the Go build's own temp area
			// (.gotmp/...). This is the directory the script is aimed at in ci.yml.
			dir, err := os.MkdirTemp(filepath.Join(root, "tmp"), "gate-check-")
			if err != nil {
				t.Fatalf("making a scratch directory under tmp/: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			for name, code := range tc.markers {
				if err := os.WriteFile(filepath.Join(dir, "gate-"+name+".exit"), []byte(code), 0o600); err != nil {
					t.Fatalf("writing marker for %s: %v", name, err)
				}
			}
			summary := filepath.Join(dir, "gate-failures.md")
			out, err := os.Create(filepath.Join(dir, "stdout.txt"))
			if err != nil {
				t.Fatalf("creating the transcript file: %v", err)
			}
			defer out.Close()

			cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script,
				"-Gates", tc.expect, "-MarkerDir", dir, "-SummaryPath", summary)
			// A file, not a pipe: the wrapper in this repository exists because a
			// pipeline carries no exit code, and the same reasoning applies here.
			cmd.Stdout = out
			cmd.Stderr = out
			runErr := cmd.Run()

			got := 0
			if runErr != nil {
				exitErr, ok := runErr.(*exec.ExitError)
				if !ok {
					t.Fatalf("running the checker: %v", runErr)
				}
				got = exitErr.ExitCode()
			}
			if got != tc.want {
				body, _ := os.ReadFile(filepath.Join(dir, "stdout.txt"))
				t.Errorf("the checker exited %d, want %d.\nIts own output:\n%s", got, tc.want, body)
			}
			if tc.want != 0 {
				if _, err := os.Stat(summary); err != nil {
					t.Errorf("a red gate left no summary behind for the artifact: %v", err)
				} else if body, _ := os.ReadFile(summary); !strings.Contains(string(body), tc.wantIn) {
					t.Errorf("the summary does not say %q:\n%s", tc.wantIn, body)
				}
			}
		})
	}
}

// curlGateSteps returns the steps of the curl-gates job, in the order ci.yml
// declares them.
func curlGateSteps(t *testing.T, workflow string) []string {
	t.Helper()
	const header = "\n  curl-gates:"
	start := strings.Index(workflow, header)
	if start < 0 {
		t.Fatal("ci.yml no longer declares a curl-gates job")
	}
	var steps []string
	var current strings.Builder
	for _, line := range strings.Split(workflow[start+len(header):], "\n") {
		// A job ends at the next key at the same or shallower indentation.
		if line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			!strings.HasPrefix(line, "  #") {
			break
		}
		if strings.HasPrefix(line, "      - ") {
			if current.Len() > 0 {
				steps = append(steps, current.String())
			}
			current.Reset()
		}
		current.WriteString(line)
		current.WriteString("\n")
	}
	if current.Len() > 0 {
		steps = append(steps, current.String())
	}
	if len(steps) == 0 {
		t.Fatal("the curl-gates job has no steps; this check would otherwise pass on an empty job")
	}
	return steps
}
