package miniyaml

import (
	"encoding/json"
	"testing"
)

// TestSeqItemMappingIndentationStyles pins the three ways YAML lets you write a
// mapping inside a block sequence. They are semantically identical, and the
// parser must not care which one a human or an editor produced:
//
//	- name: a          # dash and key on one line
//	  kind: openai     #   continuation indented past the dash  <- the common style
//
//	-                 # bare dash
//	  name: a
//
//	-   name: a       # key indented further past the dash
//	    kind: openai
//
// The first style is the one a hand-written config almost always uses, and it
// was rejected with "unexpected indentation 4 in sequence, expected 2" because
// the sequence loop demanded that every line sit at exactly the dash's column.
func TestSeqItemMappingIndentationStyles(t *testing.T) {
	cases := map[string]string{
		"key indented under the dash": `
upstreams:
  - name: "deepseek"
    kind: "openai"
    base_url: "https://api.deepseek.com"
    models:
      - "deepseek-chat"
      - "deepseek-reasoner"
`,
		"bare dash then block": `
upstreams:
  -
    name: "deepseek"
    kind: "openai"
    models:
      - "deepseek-chat"
`,
		"key indented past the dash": `
upstreams:
  -   name: "deepseek"
      kind: "openai"
      models:
        - "deepseek-chat"
`,
	}

	type upstream struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind"`
		BaseURL string   `json:"base_url"`
		Models  []string `json:"models"`
	}
	type doc struct {
		Upstreams []upstream `json:"upstreams"`
	}

	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			var d doc
			if err := Unmarshal([]byte(yaml), &d); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if len(d.Upstreams) != 1 {
				t.Fatalf("got %d upstreams, want 1", len(d.Upstreams))
			}
			u := d.Upstreams[0]
			if u.Name != "deepseek" {
				t.Errorf("name = %q, want deepseek", u.Name)
			}
			if u.Kind != "openai" {
				t.Errorf("kind = %q, want openai", u.Kind)
			}
			if len(u.Models) != 2 && name == "key indented under the dash" {
				t.Errorf("got %d models, want 2", len(u.Models))
			}
			if len(u.Models) > 0 && u.Models[0] != "deepseek-chat" {
				t.Errorf("models[0] = %q, want deepseek-chat", u.Models[0])
			}
		})
	}
}

// TestSeqItemNestedBlockValues covers the harder shape: a mapping item whose
// own value is a nested block (another map, or a sequence), including
// comments and blank lines interleaved at every level. This is exactly the
// shape configs/mock.yaml uses, and a naive fix for the indentation bug above
// would swallow the sibling items that follow.
func TestSeqItemNestedBlockValues(t *testing.T) {
	const yaml = `
# leading comment
server:
  listen: ":8080"

upstreams:
  # a comment between items
  - name: "first"
    kind: "openai"
    base_url: "http://127.0.0.1:9000"
    api_key: ""

    # comment inside an item
    models:
      - "/"

  - name: "second"
    kind: "openai"
    models:
      - "a"
      - "b"

pricing:
  default:
    in: 1.0
    out: 3.0
  models:
    first:
      in: 0.5
      out: 1.5
`
	type upstream struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind"`
		BaseURL string   `json:"base_url"`
		APIKey  string   `json:"api_key"`
		Models  []string `json:"models"`
	}
	type price struct {
		In  float64 `json:"in"`
		Out float64 `json:"out"`
	}
	type doc struct {
		Server struct {
			Listen string `json:"listen"`
		} `json:"server"`
		Upstreams []upstream `json:"upstreams"`
		Pricing   struct {
			Default price            `json:"default"`
			Models  map[string]price `json:"models"`
		} `json:"pricing"`
	}

	var d doc
	if err := Unmarshal([]byte(yaml), &d); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if d.Server.Listen != ":8080" {
		t.Errorf("server.listen = %q, want :8080", d.Server.Listen)
	}
	if len(d.Upstreams) != 2 {
		t.Fatalf("got %d upstreams, want 2: %+v", len(d.Upstreams), d.Upstreams)
	}
	if d.Upstreams[0].Name != "first" || d.Upstreams[1].Name != "second" {
		t.Errorf("upstream order/names wrong: %q, %q", d.Upstreams[0].Name, d.Upstreams[1].Name)
	}
	if got := d.Upstreams[0].Models; len(got) != 1 || got[0] != "/" {
		t.Errorf("first.Models = %v, want [/]", got)
	}
	if got := d.Upstreams[1].Models; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("second.Models = %v, want [a b]", got)
	}
	if d.Pricing.Default.In != 1.0 || d.Pricing.Default.Out != 3.0 {
		t.Errorf("pricing.default = %+v, want {1 3}", d.Pricing.Default)
	}
	if p, ok := d.Pricing.Models["first"]; !ok || p.In != 0.5 || p.Out != 1.5 {
		t.Errorf("pricing.models.first = %+v (present=%v), want {0.5 1.5}", p, ok)
	}

	// Sanity: the normalised JSON must itself be well-formed, since the whole
	// pipeline hands this tree to encoding/json.
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("normalised JSON is invalid: %s", raw)
	}
}

// TestUnterminatedItemIndentationStillRejected guards the fix from being too
// permissive: a line indented deeper than the dash that is NOT a continuation
// key of that item is still a syntax error, because accepting it would silently
// attach a value to the wrong map.
func TestUnterminatedItemIndentationStillRejected(t *testing.T) {
	const yaml = `
upstreams:
  - name: "a"
    - orphaned sequence item
`
	var d struct {
		Upstreams []map[string]string `json:"upstreams"`
	}
	if err := Unmarshal([]byte(yaml), &d); err == nil {
		t.Fatalf("expected an error for an orphaned nested sequence item, got none")
	}
}
