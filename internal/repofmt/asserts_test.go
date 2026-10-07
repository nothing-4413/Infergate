package repofmt

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The gate scripts define their own Assert-* helpers at the top of each file,
// and PowerShell does not reject an extra positional argument to a simple
// function: it binds the parameters it has and silently discards the rest. That
// is how six verify-m3.ps1 call sites came to hand Assert-Equal a detail string
// for the failure line -- the helper declared only (Label, Expected, Actual) --
// so "no process started by this script survived it" printed "expected '0', got
// '1'" with no pid list, the four "never reached the provider" assertions lost
// their before/after counters, and the derived-config check lost the list of
// variables it found. The detail is what an operator reads while a gate is red,
// and losing it is invisible in review: nothing errors, the gate still says PASS
// or FAIL exactly as before.
//
// This test resolves every Assert-* call against the helper defined in the same
// file and fails when the call binds more arguments than that helper declares:
// the ones it binds by position, plus the distinct parameters it names. A named
// parameter is taken to consume the token after it unless that token is itself
// named, and the "-Name:value" form consumes none -- the conservative read, since
// believing a token is some parameter's value can only make this test quieter. It
// refuses to pass unless it judged a floor of call sites, so a change to the
// scripts' style cannot turn it into a no-op.
func TestAssertionCallsDoNotOutrunTheirHelpers(t *testing.T) {
	root := repoRoot(t)
	scripts, err := filepath.Glob(filepath.Join(root, "scripts", "*.ps1"))
	if err != nil {
		t.Fatalf("globbing the gate scripts: %v", err)
	}
	if len(scripts) < 8 {
		t.Fatalf("found %d gate scripts under scripts/, want at least 8 (m0..m6 and hardening)", len(scripts))
	}

	judged, skipped, definitions, byName := 0, 0, 0, 0
	for _, path := range scripts {
		src := readFile(t, path)
		helpers := assertHelperArities(src)
		if len(helpers) == 0 {
			continue
		}
		definitions += len(helpers)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for _, call := range findAssertCalls(src) {
			helper, known := helpers[call.name]
			if !known {
				// The name is defined in another gate script (or not at all):
				// comparing it against this file's arity table would be a guess.
				skipped++
				continue
			}
			positional, named := bindingCounts(call.args)
			judged++
			if call.hasNamed {
				byName++
			}
			if bound := positional + named; bound > helper.arity {
				t.Errorf("%s:%d: %s binds %d arguments (%d by position, %d by name), but this file's %s declares %d (%s): an argument PowerShell cannot bind is silently discarded, so the detail it carries never reaches a failing gate",
					rel, call.line, call.name, bound, positional, named, call.name, helper.arity, helper.decl)
			}
		}
	}

	if definitions < 8 {
		t.Fatalf("parsed %d Assert-* definitions, want at least 8: the arity table is what this test compares against", definitions)
	}
	if judged < 400 {
		t.Fatalf("judged only %d assertion calls (%d skipped), want at least 400: the scripts' call style must have changed in a way this parser does not understand", judged, skipped)
	}
	// Calls land in the skipped bucket only when the helper they call is not
	// defined in the file they live in -- today, no call at all. A share this
	// large means the scripts started resolving helpers from somewhere this test
	// does not read, and every one of those calls stops being checked while the
	// test still reports success.
	if skipped > judged/20 {
		t.Fatalf("skipped %d of %d assertion calls: that is more than the cross-file calls this tree has, so this test is no longer looking at the scripts' arguments", skipped, judged+skipped)
	}
	t.Logf("judged %d assertion calls (%d of them bind at least one named parameter, %d skipped as foreign) against %d Assert-* definitions", judged, byName, skipped, definitions)
}

// assertHelper is one Assert-* helper: how many parameters its param(...) block
// declares -- the upper bound on positional arguments PowerShell will bind -- and
// the declaration itself, so a failure can quote it.
type assertHelper struct {
	arity int
	decl  string
}

// assertHelperArities maps every Assert-* helper a script defines to its
// declaration. Every helper in this tree is written as a "function Assert-X {"
// line followed by a line that opens "param(" -- after a comment-based help
// block that may be several lines long, which is why the search skips comment
// lines instead of stopping after a fixed number of them.
func assertHelperArities(src string) map[string]assertHelper {
	lines := strings.Split(src, "\n")
	out := make(map[string]assertHelper, 8)
	for i, line := range lines {
		m := assertHelperLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		inBlockComment := false
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if inBlockComment {
				if strings.Contains(trimmed, "#>") {
					inBlockComment = false
				}
				continue
			}
			if strings.HasPrefix(trimmed, "<#") {
				inBlockComment = !strings.Contains(trimmed, "#>")
				continue
			}
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if !strings.HasPrefix(trimmed, "param") ||
				!strings.HasPrefix(strings.TrimLeft(strings.TrimPrefix(trimmed, "param"), " \t"), "(") {
				// A helper that takes no parameters at all is still a definition:
				// recording it is what keeps its call sites out of the skipped
				// bucket, where nothing is compared.
				out[name] = assertHelper{decl: "no param(...) block"}
				break
			}
			// The parameter list is on one line throughout this tree; join the
			// following lines anyway so a wrapped list keeps its arity.
			open := strings.Index(lines[j], "(")
			text := lines[j]
			inner, _, ok := parenGroup(text[open:], 0)
			for k := j + 1; !ok && k < len(lines) && k <= j+8; k++ {
				text += "\n" + lines[k]
				inner, _, ok = parenGroup(text[open:], 0)
			}
			if ok {
				out[name] = assertHelper{arity: countParamList(inner), decl: collapse(inner)}
			} else {
				out[name] = assertHelper{decl: "unparsed param(...) block"}
			}
			break
		}
	}
	return out
}

var assertHelperLine = regexp.MustCompile(`^function\s+(Assert-[A-Za-z]+)\s*\{\s*$`)
var assertCallName = regexp.MustCompile(`Assert-[A-Za-z]+`)

// collapse flattens a declaration for a one-line error message.
func collapse(decl string) string {
	return strings.Join(strings.Fields(decl), " ")
}

type assertCall struct {
	name     string
	line     int
	args     []string
	hasNamed bool
}

// bindingCounts reports how many arguments a call binds by position and how many
// distinct parameters it binds by name; the two together are what PowerShell has
// to fit into the helper's parameters.
//
// The value of a named parameter is not an argument of its own, so the token
// after "-Name" is skipped unless it is itself named, while the "-Name:value"
// form carries its value inside the same token and skips nothing. A switch
// parameter followed by a positional argument is therefore read as taking that
// argument as its value, which under-counts and can only make this test quieter.
func bindingCounts(args []string) (positional, named int) {
	seen := make(map[string]bool, 4)
	for i := 0; i < len(args); i++ {
		name, isNamed := namedArg(args[i])
		if !isNamed {
			positional++
			continue
		}
		if !seen[name] {
			seen[name] = true
			named++
		}
		if strings.Contains(args[i], ":") || i+1 >= len(args) {
			continue
		}
		if _, nextIsNamed := namedArg(args[i+1]); !nextIsNamed {
			i++
		}
	}
	return positional, named
}

// namedArg reports whether a token is a parameter name ("-Min", "-Detail:3") and
// returns that name without its leading dash or its value. A token that merely
// starts with a dash is not one: a negative number is a value.
func namedArg(tok string) (string, bool) {
	if len(tok) < 2 || tok[0] != '-' || !unicode.IsLetter(rune(tok[1])) {
		return "", false
	}
	name := tok[1:]
	if colon := strings.IndexByte(name, ':'); colon >= 0 {
		name = name[:colon]
	}
	return name, true
}

// TestBindingCountsReadNamedParametersConservatively pins the reading the arity
// guard rests on. Every row that could go either way is pinned on the quieter
// side: a token that cannot be told apart from a parameter's value is counted as
// one, because over-counting is what reports a violation that does not exist.
func TestBindingCountsReadNamedParametersConservatively(t *testing.T) {
	cases := []struct {
		what       string
		args       []string
		positional int
		named      int
	}{
		{"all positional", []string{"'label'", "$ok"}, 2, 0},
		{"named parameters bind their values", []string{"-Label", "$Label", "-Condition", "$ok", "-Detail", "$detail"}, 0, 3},
		{"a value in the name's own token", []string{"-Detail:3", "'x'"}, 1, 1},
		{"repeating a parameter name counts once", []string{"-Min", "0", "-Min", "1"}, 0, 1},
		{"a negative number is a value, not a parameter", []string{"-1", "$x"}, 2, 0},
		{"a switch cannot be told from a value-taking parameter", []string{"-Verbose", "$x"}, 0, 1},
	}
	for _, c := range cases {
		positional, named := bindingCounts(c.args)
		if positional != c.positional || named != c.named {
			t.Errorf("%s: bindingCounts(%q) = %d positional, %d named; want %d, %d",
				c.what, c.args, positional, named, c.positional, c.named)
		}
	}
}

// findAssertCalls returns every call to an Assert-* helper whose argument list
// this parser can reconstruct from text. A call is recognised when the name
// opens a statement: at the start of a line (after indentation), or after an
// opening brace, a semicolon, or an assignment -- which is how these scripts
// write them, including "if ($x) { Assert-Equal ... }".
func findAssertCalls(src string) []assertCall {
	lines := strings.Split(src, "\n")
	calls := make([]assertCall, 0, 256)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		for _, loc := range assertCallName.FindAllStringIndex(line, -1) {
			prefix := line[:loc[0]]
			if strings.Contains(prefix, "#") {
				continue
			}
			trimmed := strings.TrimSpace(prefix)
			if trimmed != "" && !strings.HasSuffix(trimmed, "{") &&
				!strings.HasSuffix(trimmed, ";") && !strings.HasSuffix(trimmed, "=") {
				continue
			}
			text := line[loc[1]:]
			start := i
			// A statement continues while the line ends in a backtick or leaves a
			// quote or a bracket open; both are how these scripts wrap a long
			// assertion. needMoreInput reports both in one call.
			for needMoreInput(text) && i+1 < len(lines) {
				i++
				text += "\n" + lines[i]
			}
			args, named, complete := scanStatement(text)
			if !complete {
				continue
			}
			calls = append(calls, assertCall{
				name:     line[loc[0]:loc[1]],
				line:     start + 1,
				args:     args,
				hasNamed: named,
			})
			// The rest of this logical statement is not scanned again: any further
			// Assert-* name on a consumed continuation line belongs to it.
			break
		}
	}
	return calls
}

// needMoreInput reports whether a partial statement still expects input: it ends
// in a line-continuation backtick, or it leaves a bracket or a quote open.
func needMoreInput(s string) bool {
	if strings.HasSuffix(strings.TrimRight(s, " \t\r\n"), "`") {
		return true
	}
	_, _, complete := scanStatement(s)
	return !complete
}

// scanStatement splits the text that follows an Assert-* name into the arguments
// of that one command. It stops at whatever ends the command at nesting level
// zero -- a semicolon, a pipe, a closing brace, a comment -- and reports whether
// it ran out of text first, in which case the caller appends the next line.
//
// The reader is a three-state machine (code, single-quoted string, double-quoted
// string) because PowerShell's own lexer is what decides where an argument ends,
// and the scripts nest quotes inside "$(if (...) { " >= 1" })" expansions. A
// "$(" inside a double-quoted string pushes a code context, so the quotes and
// braces of the expansion are read as code and the string resumes after its
// closing parenthesis -- reading that expansion as string content splits one
// argument into five and reports an arity violation that does not exist.
func scanStatement(s string) (args []string, hasNamed bool, complete bool) {
	type context struct {
		kind    byte // 'c' code, 's' single-quoted, 'd' double-quoted
		depth   int  // brackets open in this code context
		subexpr bool // pushed by "$(": its closing ) returns to the string
	}
	rs := []rune(s)
	stack := []context{{kind: 'c'}}
	var cur strings.Builder
	flush := func() {
		tok := strings.TrimSpace(cur.String())
		cur.Reset()
		if tok == "" {
			return
		}
		args = append(args, tok)
		if len(tok) > 1 && tok[0] == '-' && unicode.IsLetter(rune(tok[1])) {
			hasNamed = true
		}
	}
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		top := &stack[len(stack)-1]
		switch top.kind {
		case 's':
			if c == '\'' {
				if i+1 < len(rs) && rs[i+1] == '\'' {
					i++
					continue
				}
				stack = stack[:len(stack)-1]
			}
			continue
		case 'd':
			switch {
			case c == '`':
				if i+1 < len(rs) {
					i++
				}
			case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
				// The "(" is consumed here and not counted in depth, so the ")"
				// that balances it is what pops this context.
				i++
				stack = append(stack, context{kind: 'c', subexpr: true})
			case c == '"':
				if i+1 < len(rs) && rs[i+1] == '"' {
					i++
					continue
				}
				stack = stack[:len(stack)-1]
			}
			continue
		}
		// Code: the statement itself, or a $( ) expansion inside a string.
		switch {
		case c == '\'':
			stack = append(stack, context{kind: 's'})
			cur.WriteRune(c)
		case c == '"':
			stack = append(stack, context{kind: 'd'})
			cur.WriteRune(c)
		case c == '`':
			if i+1 >= len(rs) {
				return args, hasNamed, false
			}
			i++
			cur.WriteRune(rs[i])
		case c == '(':
			top.depth++
			cur.WriteRune(c)
		case c == ')':
			switch {
			case top.depth > 0:
				top.depth--
				cur.WriteRune(c)
			case top.subexpr:
				stack = stack[:len(stack)-1]
			default:
				flush()
				return args, hasNamed, true
			}
		case c == '[' || c == '{':
			top.depth++
			cur.WriteRune(c)
		case c == ']' || c == '}':
			if top.depth == 0 && len(stack) == 1 {
				flush()
				return args, hasNamed, true
			}
			if top.depth > 0 {
				top.depth--
			}
			cur.WriteRune(c)
		case (c == '#' || c == '|' || c == ';') && top.depth == 0 && len(stack) == 1:
			flush()
			return args, hasNamed, true
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			// Only the statement's own code context splits arguments: inside a
			// "$( )" expansion that a string embeds, whitespace is string content,
			// and flushing there would cut one argument into several.
			if top.depth == 0 && len(stack) == 1 {
				flush()
			} else {
				cur.WriteRune(c)
			}
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return args, hasNamed, len(stack) == 1 && stack[0].depth == 0
}

// parenGroup returns the text between the parentheses that start at rune offset
// open (s[open] == '(') and the offset just past the matching ')'. Quotes and
// "$( )" expansions are honoured, so a parenthesis inside either does not close
// the group.
func parenGroup(s string, open int) (inner string, end int, ok bool) {
	rs := []rune(s)
	if open < 0 || open >= len(rs) || rs[open] != '(' {
		return "", 0, false
	}
	depth, start := 0, -1
	var quote rune
	for i := open; i < len(rs); i++ {
		c := rs[i]
		if quote != 0 {
			if c == '`' && quote == '"' && i+1 < len(rs) {
				i++
				continue
			}
			if c == quote {
				if i+1 < len(rs) && rs[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(':
			depth++
			if depth == 1 {
				start = i + 1
			}
		case ')':
			depth--
			if depth == 0 {
				return string(rs[start:i]), i + 1, true
			}
		}
	}
	return "", 0, false
}

// countParamList counts the parameters a param(...) block declares. A parameter
// carrying attributes still counts: the count is used as an upper bound on what
// PowerShell can bind, and over-counting can only make this test quieter.
func countParamList(inner string) int {
	count, depth := 0, 0
	var quote rune
	empty := true
	for _, c := range inner {
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			empty = false
		case '(', '[', '{':
			depth++
			empty = false
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				if !empty {
					count++
				}
				empty = true
				continue
			}
			empty = false
		default:
			if !unicode.IsSpace(c) {
				empty = false
			}
		}
	}
	if !empty {
		count++
	}
	return count
}
