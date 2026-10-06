package repofmt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRaceRecipeStaysRunnable pins the fix for a claim this repository repeated
// in three files for weeks: "this machine cannot run go test -race, there is no
// C compiler".
//
// Half of that was true (gcc is not on PATH) and the conclusion was false:
// C:\msys64\ucrt64\bin\gcc.exe has been there the whole time, and so has the
// MSVC toolset. The cost of the wrong conclusion was not academic -- a data race
// in production code (internal/cache/redis.go's RedisStore.stats) sat behind a
// permanently red CI step whose failure reason is not anonymously readable, so
// "cannot reproduce locally" meant "nobody can see it at all".
//
// The recipe is now a script rather than a paragraph, because a paragraph is
// exactly what drifted. This test holds the three things that make it work:
//
//  1. the script exists and is pure ASCII with no BOM -- Windows PowerShell 5.1
//     reads a BOM-less .ps1 in the local code page, so a stray non-ASCII byte
//     becomes mojibake rather than a syntax error (the same trap cost
//     scripts/verify-m5.ps1 five assertions that were silently vacuous);
//  2. it sets CC, CGO_ENABLED and redirects TMP/TEMP, the last being the part
//     everyone misses: without it cgo writes its input under %LOCALAPPDATA%\Temp
//     and fails with "Access is denied";
//  3. it reports the failing package on stdout and returns non-zero, so a caller
//     who never reads the log still learns which package raced.
func TestRaceRecipeStaysRunnable(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "run-race.ps1")

	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("scripts/run-race.ps1 is gone: %v\n"+
			"That script is the only reason -race is runnable here; the docs point at it.", err)
	}

	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		t.Error("scripts/run-race.ps1 starts with a UTF-8 BOM; PowerShell 5.1 then " +
			"treats the first line as part of the script text")
	}
	for i, b := range raw {
		if b > 127 {
			t.Fatalf("scripts/run-race.ps1 has a non-ASCII byte at offset %d (0x%02X); "+
				"write it through [System.IO.File]::WriteAllText, not Set-Content -Encoding utf8",
				i, b)
		}
	}

	text := string(raw)
	for _, want := range []string{"CGO_ENABLED", "$env:CC", "$env:TMP", "$env:TEMP", "gcc"} {
		if !strings.Contains(text, want) {
			t.Errorf("scripts/run-race.ps1 no longer sets %s; without it `go test -race` "+
				"fails at link time and the recipe reads as impossible again", want)
		}
	}

	// A gate that pipes its own output carries the pipe's exit code, not the
	// tool's -- scripts/lib/run-gate.ps1 records the same lesson from the other
	// direction. This script must capture first, decide, then print.
	if strings.Contains(text, "Tee-Object") {
		t.Error("scripts/run-race.ps1 pipes through Tee-Object; capture the output in a " +
			"variable and branch on $LASTEXITCODE instead, or a failing package reports green")
	}

	// The named-on-the-way-out line is the whole point of the loop in CI too:
	// GET /actions/runs/{id}/jobs exposes step names only, never the package.
	if !strings.Contains(text, "FAILED(running):") {
		t.Error("scripts/run-race.ps1 no longer names the failing package on stdout; " +
			"a reader scrolling history should not have to know which line to grep for")
	}
	if !strings.Contains(text, "exit 1") {
		t.Error("scripts/run-race.ps1 no longer exits non-zero when a package fails")
	}

	// And the docs have to keep pointing at it: the recipe's value is that a
	// reader finds it in one step. This is the drift that started the test.
	for _, doc := range []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "docs", "ACCEPTANCE.md"),
	} {
		body := readFile(t, doc)
		if !strings.Contains(body, "run-race.ps1") {
			t.Errorf("%s does not mention scripts/run-race.ps1; the documented way to run "+
				"-race has drifted away from the script that works", filepath.Base(doc))
		}
	}
}
