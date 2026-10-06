package repofmt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkflowYAMLIsParseable catches the failure mode that cost this repository
// six commits of blindness.
//
// WHAT HAPPENED. 760c303 added a step whose `run:` line carried a title
// containing a colon:
//
//	run: bash scripts/ci-publish-failure-check.sh "ci failure: build / vet / test / race" ...
//
// A YAML plain scalar may not contain a colon followed by a space, so the whole
// file stopped being valid YAML:
//
//	go-yaml load error in scanner at L185.C66: mapping values are not allowed
//	in this context
//
// A workflow file GitHub cannot parse produces a run with ZERO JOBS. The run is
// `completed/failure`, its display name falls back to the file path instead of
// `name: ci`, `updated_at` equals `created_at`, and nothing ever executes. So
// for six commits -- 760c303 through 03655b2 -- every push looked red while in
// fact *nothing ran at all*: not the Linux gate, not the eight Windows curl
// gates, not the check-run relay. All the repository's notes about GitHub
// returning `total_count: 0` for these runs were describing its own broken file,
// not a GitHub anomaly.
//
// WHY NOT JUST PARSE THE YAML. This module has no dependencies (no go.sum) and
// is not going to acquire one for a test, and GitHub's YAML dialect is not
// something to hand-roll. What is cheap and exact is the ONE rule that was
// broken: a plain (unquoted) scalar value must not contain a colon-space. That
// rule needs no parser, has no false positives for plain scalars, and covers the
// whole class of mistake rather than this one instance.
//
// Quoted values, block scalars (`|`, `>`, with or without chomping indicators)
// and flow collections are all fine and are skipped, including the lines inside
// a block scalar -- shell code inside `run: |` legitimately contains colons.
func TestWorkflowYAMLIsParseable(t *testing.T) {
	root := repoRoot(t)

	dir := filepath.Join(root, ".github")
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yml", ".yaml":
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatal("no .yml or .yaml files under .github; either the workflows are gone " +
			"or this check is looking in the wrong place")
	}

	for _, path := range files {
		rel, _ := filepath.Rel(root, path)
		body := readFile(t, path)
		for _, problem := range plainScalarColonProblems(body) {
			t.Errorf("%s: %s", filepath.ToSlash(rel), problem)
		}
	}
}

// plainScalarColonProblems returns one message per mapping value that is a plain
// YAML scalar containing ": " (or ending in ":"), which makes the document
// unparseable. It is deliberately a small state machine rather than a parser:
// the only structure it needs to know is where a block scalar starts and ends.
func plainScalarColonProblems(content string) []string {
	var problems []string
	lines := strings.Split(content, "\n")
	// Indentation of the mapping key that opened the block scalar we are inside,
	// or -1 when not inside one.
	blockIndent := -1

	for i, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		if line == "" {
			if blockIndent >= 0 {
				continue
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		if blockIndent >= 0 {
			// A block scalar ends at the first non-blank line that is not
			// indented deeper than its key.
			if indent > blockIndent {
				continue
			}
			blockIndent = -1
		}

		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		body := trimmed
		if strings.HasPrefix(body, "- ") {
			body = strings.TrimSpace(body[2:])
		}

		idx := strings.Index(body, ":")
		if idx <= 0 {
			continue
		}
		key := body[:idx]
		if strings.ContainsAny(key, "\"'") || strings.Contains(key, ":") {
			continue
		}
		// Only "key:" or "key: value" is a mapping; "https://x" is not a key.
		if idx+1 < len(body) && body[idx+1] != ' ' && body[idx+1] != '\t' {
			continue
		}

		rest := strings.TrimLeft(body[idx+1:], " \t")
		if rest == "" {
			continue
		}
		switch rest[0] {
		case '|', '>':
			// Block scalar: remember where it started, skip its contents.
			blockIndent = indent
			continue
		case '"', '\'', '[', '{', '&', '*', '!', '?':
			// Quoted scalar, flow collection, anchor, alias, tag, or explicit
			// key indicator: the colon rule does not apply.
			continue
		}

		if strings.Contains(rest, ": ") || strings.HasSuffix(rest, ":") {
			problems = append(problems, fmt.Sprintf("line %d: a plain YAML scalar cannot "+
				"contain ':' followed by a space, which makes the whole file unparseable and "+
				"the workflow start zero jobs:\n      %s\n    quote the value, or move it out "+
				"of the 'run:' line (an env: variable works)", i+1, trimmed))
		}
	}
	return problems
}

// TestPlainScalarColonDetection proves the check above is not vacuous, using the
// exact line that broke the workflow plus the shapes that must stay legal.
func TestPlainScalarColonDetection(t *testing.T) {
	const broken = `      - name: publish
        env:
          GH_TOKEN: ${{ github.token }}
        run: bash scripts/ci-publish-failure-check.sh "ci failure: build / vet / test / race" /tmp/x.md
`
	if got := plainScalarColonProblems(broken); len(got) == 0 {
		t.Error("the line that made ci.yml invalid YAML is not detected")
	}

	legal := map[string]string{
		"the fixed form (title in env)": `        env:
          CHECK_TITLE: 'ci failure: build / vet / test / race'
        run: bash scripts/x.sh "$CHECK_TITLE" /tmp/x.md
`,
		"a quoted scalar holding a colon-space": `        name: "publish: the summary"
`,
		"shell code inside a block scalar": `        run: |
          echo "failure: build" > /tmp/x
          for pkg in $(go list ./...); do
            echo "=== $pkg (race)"
          done
`,
		"a folded block scalar": `        run: >-
          bash scripts/x.sh
          "ci failure: build"
`,
		"a URL": `        url: https://api.github.com/repos/o/r
`,
		"a flow sequence": `        branches: [main, release]
`,
		"an expression with no colon-space": `        group: ci-${{ github.ref }}
        if: failure()
`,
		"a plain value with no colon": `        name: go build
        run: go build ./...
`,
	}
	for label, content := range legal {
		if got := plainScalarColonProblems(content); len(got) != 0 {
			t.Errorf("%s should be legal, got %v", label, got)
		}
	}
}
