// Package miniyaml is a deliberately small YAML reader for InferGate's
// configuration files.
//
// Why not a full YAML library? Because M0 ships with zero third-party
// dependencies: every line of the gateway is standard-library Go, which keeps
// the dependency surface auditable and the build reproducible without a module
// proxy. The cost is that this package implements the subset InferGate needs
// and rejects the rest loudly rather than guessing.
//
// Supported (matching configs/infergate.yaml):
//
//	key: value                      # scalar
//	key:                            # nested mapping (2-space indent)
//	  child: value
//	key:                            # block sequence of scalars
//	  - a
//	  - b
//	key:                            # block sequence of mappings
//	  - name: a
//	    url: b
//	key: [a, b]                     # flow sequence of scalars
//	key: {}                         # empty flow mapping (ignored, keeps defaults)
//	key: []                         # empty flow sequence
//	# comment                       # whole-line comment
//	key: value  # trailing comment
//
// Rejected with a precise error: tabs used for indentation, anchors/aliases,
// block scalars (| and >), multi-document streams, duplicate keys. Unmarshal
// additionally ignores keys the target has no field for; UnmarshalStrict
// rejects them by name, because a configuration file that silently drops a
// misspelled key is a configuration file that does not say what it does.
//
// Scalars are typed the way YAML types them — a bare 0.27 or 8388608 becomes a
// JSON number, a quoted "0.27" stays a string — and the typed tree is then
// handed to encoding/json, which maps it onto the target struct's field types.
// That keeps one source of truth for the schema (the json tags) while still
// letting an unquoted number reach a float64 or int field, and a quoted one
// reach a time.Duration or a model name that merely looks numeric.
package miniyaml

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Kind discriminates the three node shapes this parser produces.
type Kind uint8

const (
	// KindMap is a mapping node.
	KindMap Kind = iota
	// KindSeq is a sequence node.
	KindSeq
	// KindScalar is a leaf node holding a string.
	KindScalar
)

// Node is a parsed YAML value.
type Node struct {
	Kind Kind
	Map  map[string]*Node
	Seq  []*Node
	Str  string
}

// Unmarshal parses YAML bytes and decodes them into v using encoding/json, so
// the struct's json tags are the single source of truth for field names and
// types. A key the target has no field for is ignored; use UnmarshalStrict
// where that would hide a mistake.
func Unmarshal(data []byte, v any) error {
	return decode(data, v, false)
}

// UnmarshalStrict is Unmarshal with unknown keys rejected: a mapping key the
// target struct has no field for is an error naming the key instead of a
// no-op.
//
// This exists because an ignored key is the worst possible fate for a
// configuration file. `quota:` misspelled `quotas:` leaves every quota at its
// default, so the gateway runs and spends with the limits the operator thought
// they had set nowhere in effect -- a silent downgrade of exactly the safety
// net the file was written to install. The same mistake in a normal decode is
// invisible by construction: encoding/json has nowhere to report it and no way
// to know whether the key was a typo or a field from another version.
//
// internal/config loads through this function, so a config that starts is a
// config the loader understood.
func UnmarshalStrict(data []byte, v any) error {
	return decode(data, v, true)
}

func decode(data []byte, v any, strict bool) error {
	node, err := Parse(data)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(node.toJSONValue())
	if err != nil {
		return fmt.Errorf("miniyaml: re-encode: %w", err)
	}
	if strict {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return fmt.Errorf("miniyaml: decode: %w", err)
		}
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("miniyaml: decode: %w", err)
	}
	return nil
}

// Parse turns YAML bytes into a Node tree.
func Parse(data []byte) (*Node, error) {
	lines, err := lexLines(string(data))
	if err != nil {
		return nil, err
	}
	p := &parser{lines: lines}
	if len(p.lines) == 0 {
		return &Node{Kind: KindMap, Map: map[string]*Node{}}, nil
	}
	if p.lines[0].indent != 0 {
		return nil, p.errorf(0, "document must start at column 0, got indent %d", p.lines[0].indent)
	}
	node, err := p.parseNode(0)
	if err != nil {
		return nil, err
	}
	return node, nil
}

type line struct {
	no     int // 1-based source line number
	indent int
	text   string
}

type parser struct {
	lines []line
	pos   int
}

func (p *parser) errorf(idx int, format string, args ...any) error {
	no := 0
	if idx < len(p.lines) {
		no = p.lines[idx].no
	}
	msg := fmt.Sprintf(format, args...)
	if no == 0 {
		return fmt.Errorf("miniyaml: %s", msg)
	}
	return fmt.Errorf("miniyaml: line %d: %s", no, msg)
}

// parseNode consumes the block that begins at the current position, whose
// members are all indented by exactly `indent`.
func (p *parser) parseNode(indent int) (*Node, error) {
	if p.pos >= len(p.lines) {
		return nil, p.errorf(p.pos, "unexpected end of input, expected a mapping or sequence")
	}
	if strings.HasPrefix(p.lines[p.pos].text, "- ") || p.lines[p.pos].text == "-" {
		return p.parseSeq(indent)
	}
	return p.parseMap(indent)
}

func (p *parser) parseMap(indent int) (*Node, error) {
	node := &Node{Kind: KindMap, Map: map[string]*Node{}}
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		if ln.indent < indent {
			break
		}
		if ln.indent > indent {
			return nil, p.errorf(p.pos, "unexpected indentation %d, expected %d", ln.indent, indent)
		}
		if strings.HasPrefix(ln.text, "- ") || ln.text == "-" {
			return nil, p.errorf(p.pos, "found a sequence item where a mapping entry was expected")
		}

		key, rest, ok := splitKey(ln.text)
		if !ok {
			return nil, p.errorf(p.pos, "expected \"key: value\", got %q", ln.text)
		}
		if _, dup := node.Map[key]; dup {
			return nil, p.errorf(p.pos, "duplicate key %q", key)
		}
		p.pos++

		value, err := p.parseValue(rest, indent, p.pos)
		if err != nil {
			return nil, err
		}
		node.Map[key] = value
	}
	return node, nil
}

func (p *parser) parseSeq(indent int) (*Node, error) {
	node := &Node{Kind: KindSeq}
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		if ln.indent < indent {
			break
		}
		// Continuation keys of a "- key: value" item are indented past the dash,
		// so a deeper line is normal and is appended to the mapping opened by the
		// item itself (see the inline-mapping branch below). YAML allows both of
		// these, and they mean the same thing:
		//
		//	- name: a        - name: a
		//	  kind: openai     kind: openai
		//
		// A deeper line that is NOT part of that mapping is a genuine
		// indentation error, because any block nested under a dash is consumed by
		// parseValue before the loop sees it.
		if ln.indent > indent && !strings.HasPrefix(ln.text, "- ") && ln.text != "-" {
			return nil, p.errorf(p.pos, "unexpected indentation %d in sequence, expected %d", ln.indent, indent)
		}
		if !strings.HasPrefix(ln.text, "- ") && ln.text != "-" {
			return nil, p.errorf(p.pos, "expected a sequence item (\"- ...\"), got %q", ln.text)
		}

		rest := strings.TrimSpace(strings.TrimPrefix(ln.text, "-"))
		p.pos++

		// "- " alone (or "-" followed by a deeper block) introduces a nested
		// block. Otherwise the remainder is either an inline mapping entry
		// ("- name: a") or a scalar.
		if rest == "" {
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				child, err := p.parseNode(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				node.Seq = append(node.Seq, child)
				continue
			}
			node.Seq = append(node.Seq, &Node{Kind: KindScalar})
			continue
		}

		if key, value, ok := splitKey(rest); ok {
			// Inline mapping entry: "- name: value" opens a mapping whose
			// continuation keys line up under the KEY (the "- " prefix is not
			// part of the mapping's indentation). ln.text is already trimmed, so
			// inner is the key's column relative to the line's own indent.
			//
			//	- name: a          inner = 0 + 2 = 2
			//	  kind: openai       indent 2 == inner, so it joins the mapping
			//
			// Getting this from the dash column instead produces 0 and the
			// sibling key looks like a stray over-indented line.
			item := &Node{Kind: KindMap, Map: map[string]*Node{}}
			inner := indent + strings.Index(ln.text, rest)
			child, err := p.parseValue(value, inner, p.pos)
			if err != nil {
				return nil, err
			}
			item.Map[key] = child
			for p.pos < len(p.lines) && p.lines[p.pos].indent == inner &&
				!strings.HasPrefix(p.lines[p.pos].text, "- ") && p.lines[p.pos].text != "-" {
				cln := p.lines[p.pos]
				ck, crem, cok := splitKey(cln.text)
				if !cok {
					return nil, p.errorf(p.pos, "expected \"key: value\", got %q", cln.text)
				}
				if _, dup := item.Map[ck]; dup {
					return nil, p.errorf(p.pos, "duplicate key %q", ck)
				}
				p.pos++
				cv, err := p.parseValue(crem, inner, p.pos)
				if err != nil {
					return nil, err
				}
				item.Map[ck] = cv
			}
			node.Seq = append(node.Seq, item)
			continue
		}

		node.Seq = append(node.Seq, &Node{Kind: KindScalar, Str: rest})
	}
	return node, nil
}

// parseValue interprets the text following "key:". `ownerIndent` is the indent
// of the line that owns the key; `next` is the index of the following line.
func (p *parser) parseValue(rest string, ownerIndent, next int) (*Node, error) {
	if rest != "" {
		if rest == "{}" {
			return &Node{Kind: KindMap, Map: map[string]*Node{}}, nil
		}
		if rest == "[]" {
			return &Node{Kind: KindSeq}, nil
		}
		if strings.HasPrefix(rest, "[") {
			return parseFlowSeq(rest)
		}
		if rest == "|" || rest == ">" || strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, ">") {
			return nil, p.errorf(next, "block scalars (%q) are not supported", rest)
		}
		if strings.HasPrefix(rest, "&") || strings.HasPrefix(rest, "*") {
			return nil, p.errorf(next, "anchors and aliases are not supported")
		}
		return &Node{Kind: KindScalar, Str: rest}, nil
	}

	// Empty value: the value is the following block, if it is indented deeper
	// than the key. Otherwise the key is an empty scalar.
	if next < len(p.lines) && p.lines[next].indent > ownerIndent {
		return p.parseNode(p.lines[next].indent)
	}
	return &Node{Kind: KindScalar}, nil
}

// parseFlowSeq handles the single-line "[a, b, c]" form.
func parseFlowSeq(s string) (*Node, error) {
	if !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("miniyaml: unterminated flow sequence %q", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	node := &Node{Kind: KindSeq}
	if inner == "" {
		return node, nil
	}
	for _, part := range splitTopLevelCommas(inner) {
		node.Seq = append(node.Seq, &Node{Kind: KindScalar, Str: part})
	}
	return node, nil
}

func splitTopLevelCommas(s string) []string {
	var (
		out   []string
		depth int
		quote byte
		start int
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			out = append(out, unquote(strings.TrimSpace(s[start:i])))
			start = i + 1
		}
	}
	out = append(out, unquote(strings.TrimSpace(s[start:])))
	return out
}

// splitKey splits "key: value" into its two halves, honouring quoted keys.
func splitKey(s string) (key, rest string, ok bool) {
	if s == "" {
		return "", "", false
	}
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ':':
			// A colon only terminates the key when it is followed by a space
			// or the end of line, so "http://x" stays a scalar.
			if i+1 == len(s) || s[i+1] == ' ' {
				return unquote(strings.TrimSpace(s[:i])), strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			inner := s[1 : len(s)-1]
			if s[0] == '"' {
				if v, err := strconv.Unquote(s); err == nil {
					return v
				}
			}
			return strings.ReplaceAll(inner, "''", "'")
		}
	}
	return s
}

// lexLines drops blank and comment-only lines, strips trailing comments and
// measures indentation. Tabs are rejected because YAML forbids them for
// indentation and a silent conversion hides real mistakes.
func lexLines(src string) ([]line, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	raw := strings.Split(src, "\n")

	out := make([]line, 0, len(raw))
	for i, original := range raw {
		no := i + 1
		if strings.ContainsRune(original, '\t') {
			return nil, fmt.Errorf("miniyaml: line %d: tab characters are not allowed for indentation, use spaces", no)
		}
		trimmed := stripComment(original)
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " "))
		out = append(out, line{no: no, indent: indent, text: strings.TrimSpace(trimmed)})
	}
	return out, nil
}

// stripComment removes a trailing "# ..." comment that is not inside quotes.
func stripComment(s string) string {
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return s[:i]
			}
		}
	}
	return s
}

// toJSONValue converts the node tree into the shape encoding/json marshals.
func (n *Node) toJSONValue() any {
	switch n.Kind {
	case KindMap:
		out := make(map[string]any, len(n.Map))
		for k, v := range n.Map {
			out[k] = v.toJSONValue()
		}
		return out
	case KindSeq:
		out := make([]any, 0, len(n.Seq))
		for _, item := range n.Seq {
			out = append(out, item.toJSONValue())
		}
		return out
	default:
		return scalarJSONValue(n.Str)
	}
}

// scalarJSONValue gives a scalar the JSON type YAML would have given it.
//
// The rules matter more than they look:
//
//   - A QUOTED scalar is always a string. YAML makes quoting authoritative, and
//     the config relies on it: read_header_timeout: "10s" must reach a
//     time.Duration field, and a model name is never coerced to a number.
//   - A bare token that parses as a number becomes a json.Number, not a
//     float64. encoding/json marshals json.Number verbatim, so a large
//     max_body_bytes or a price with many significant digits survives the
//     round trip instead of being reformatted through float64.
//   - Only tokens that fully match strconv's number syntax are converted, so
//     "1.2.3" and "v2" stay strings rather than becoming garbage numbers.
func scalarJSONValue(s string) any {
	if len(s) < 2 || (s[0] != '"' && s[0] != '\'') {
		switch s {
		case "", "~", "null":
			return nil
		case "true", "yes", "on":
			return true
		case "false", "no", "off":
			return false
		}
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			return json.Number(s)
		}
		return s
	}
	quote := s[0]
	if s[len(s)-1] != quote {
		// Unterminated quote: hand the raw text to the caller rather than
		// silently inventing a value.
		return s
	}
	inner := s[1 : len(s)-1]
	if quote == '"' {
		// YAML double-quoted scalars support escapes; unescaping here keeps the
		// JSON round trip honest instead of double-escaping a backslash.
		var out string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
		return inner
	}
	// Single-quoted YAML is literal, with '' as the only escape.
	return strings.ReplaceAll(inner, "''", "'")
}
