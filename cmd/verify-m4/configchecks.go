package main

// M4 config checks.
//
// Tiering is configured, so the failure mode that matters most is a
// configuration that LOOKS tiered and is not: a typo'd tier that quietly means
// cloud, a negative limit that a comparison silently treats as "no limit", an
// empty capability list that disables the capability rule, or a tiered config
// with no local tier at all. Each of those routes the whole fleet to the paid
// provider, and each of them is a one-line config edit away, so each gets an
// assertion rather than a code review.

import (
	"fmt"
	"strings"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/miniyaml"
)

// tierUpstreamCfg is one upstream, as boring as possible, so that a validation
// failure in these checks is about the tier and nothing else.
func tierUpstreamCfg(name, tier string) config.UpstreamConfig {
	return config.UpstreamConfig{
		Name:         name,
		Kind:         config.KindOpenAI,
		BaseURL:      tierFleetBaseURL,
		Models:       []string{"/"},
		Capabilities: []string{"chat"},
		Priority:     1,
		Weight:       1,
		Tier:         tier,
	}
}

func tierValidateConfig(strategy string, policy config.TierPolicyConfig, ups ...config.UpstreamConfig) *config.Config {
	cfg := config.Defaults()
	cfg.Server.Listen = ":0"
	cfg.Routing = config.RoutingConfig{Strategy: strategy, TierPolicy: policy}
	cfg.Upstreams = ups
	return &cfg
}

// ---------------------------------------------------------------------------
// 8. The tier vocabulary
// ---------------------------------------------------------------------------

func checkTierValidation(c *checker, e *environment) {
	// The two names are the whole vocabulary, in YAML and in JSON and in the
	// /admin payload. Pinning the literals here means a rename cannot land
	// without one of these lines failing.
	c.assert(config.TierLocal == "local", "config: the local tier is spelled %q (got %q)", "local", config.TierLocal)
	c.assert(config.TierCloud == "cloud", "config: the cloud tier is spelled %q (got %q)", "cloud", config.TierCloud)

	cases := []struct {
		name string
		tier string
		want string
	}{
		{name: "tier local", tier: "local"},
		{name: "tier cloud", tier: "cloud"},
		{name: "tier omitted", tier: ""},
		{name: "tier in title case", tier: "Local"},
		{name: "tier in upper case", tier: "CLOUD"},
		{name: "tier with surrounding space", tier: "  local  "},
		{name: "tier locale", tier: "locale", want: `upstream box: unsupported tier "locale" (want local or cloud)`},
		{name: "tier localhost", tier: "localhost", want: `upstream box: unsupported tier "localhost" (want local or cloud)`},
		{name: "tier edge", tier: "edge", want: `upstream box: unsupported tier "edge" (want local or cloud)`},
		{name: "tier true", tier: "true", want: `upstream box: unsupported tier "true" (want local or cloud)`},
		{name: "tier 0", tier: "0", want: `upstream box: unsupported tier "0" (want local or cloud)`},
	}
	for _, tc := range cases {
		cfg := tierValidateConfig(config.StrategyPriority, config.TierPolicyConfig{}, tierUpstreamCfg("box", tc.tier))
		err := cfg.Validate()
		if tc.want == "" {
			if !c.assertLoop(err == nil, "config: %s is accepted (%v)", tc.name, err) {
				continue
			}
			want := config.TierCloud
			if strings.EqualFold(strings.TrimSpace(tc.tier), config.TierLocal) {
				want = config.TierLocal
			}
			c.assertLoop(cfg.Upstreams[0].Tier == want,
				"config: %s normalises to tier %q (got %q)", tc.name, want, cfg.Upstreams[0].Tier)
			continue
		}
		if !c.assertLoop(err != nil, "config: %s is rejected (%v)", tc.name, err) {
			continue
		}
		c.assertLoop(strings.Contains(err.Error(), tc.want),
			"config: %s reports %q (got %q)", tc.name, tc.want, err.Error())
	}

	// An omitted tier is cloud, not "unclassified": a config written before
	// tiers existed must keep routing exactly where it did.
	for _, spelling := range []string{"", "  ", "cloud", "CLOUD"} {
		cfg := tierValidateConfig(config.StrategyPriority, config.TierPolicyConfig{}, tierUpstreamCfg("legacy", spelling))
		if !c.assertLoop(cfg.Validate() == nil, "config: the spelling %q validates", spelling) {
			continue
		}
		c.assertLoop(cfg.Upstreams[0].Tier == config.TierCloud,
			"config: the spelling %q means cloud (got %q)", spelling, cfg.Upstreams[0].Tier)
	}

	// The error names the upstream, because a fleet of thirty backends with one
	// bad tier is a search problem without it -- and it names the FIRST bad one,
	// so validation cannot hide a second mistake behind a fixed first.
	cfg := tierValidateConfig(config.StrategyPriority, config.TierPolicyConfig{},
		tierUpstreamCfg("good", "local"),
		tierUpstreamCfg("bad", "fargate"),
		tierUpstreamCfg("also-bad", "gpu"),
	)
	err := cfg.Validate()
	if c.assert(err != nil, "config: a bad tier is rejected even when a good local upstream is present") {
		c.assert(strings.Contains(err.Error(), "upstream bad:"),
			"config: the first bad upstream is the one reported (%q)", err.Error())
		c.assert(!strings.Contains(err.Error(), "also-bad"),
			"config: validation stops at the first bad upstream (%q)", err.Error())
	}

	// Tier validation is not conditional on the strategy: a bad tier is a bad
	// config whatever the router would have done with it.
	for _, strategy := range []string{"priority", "cost", "latency", "weighted", "score", "tiered", ""} {
		// "box" carries a good local tier, so tiered is satisfied and the only
		// reason to fail is the bad one.
		ups := []config.UpstreamConfig{tierUpstreamCfg("box", "LOCAL"), tierUpstreamCfg("bad", "fargate")}
		cfg := tierValidateConfig(strategy, config.TierPolicyConfig{}, ups...)
		c.assertLoop(cfg.Validate() != nil,
			"config: the strategy %q still rejects a bad tier", strategy)
	}

	// An all-local fleet is legal: a deployment can be entirely on-premises,
	// and nothing in the schema demands a cloud tier.
	cfg = tierValidateConfig(config.StrategyTiered, config.TierPolicyConfig{},
		tierUpstreamCfg("box", "local"), tierUpstreamCfg("box2", "local"))
	if c.assert(cfg.Validate() == nil, "config: a tiered fleet with no cloud upstream is legal (%v)", cfg.Validate()) {
		for i, up := range cfg.Upstreams {
			c.assertLoop(up.Tier == config.TierLocal,
				"config: all-local fleet upstream %d normalises to local (got %q)", i, up.Tier)
		}
	}
}

// ---------------------------------------------------------------------------
// 9. tier_policy
// ---------------------------------------------------------------------------

func checkTierPolicyValidation(c *checker, e *environment) {
	tieredFleet := func() []config.UpstreamConfig {
		return []config.UpstreamConfig{tierUpstreamCfg("box", config.TierLocal), tierUpstreamCfg("cloud", config.TierCloud)}
	}

	// A negative limit is rejected rather than treated as "no limit": the
	// router's guard is `limit > 0`, so a negative value would silently disable
	// the rule that keeps traffic off the paid tier.
	cases := []struct {
		name   string
		policy config.TierPolicyConfig
		want   string
	}{
		{
			name:   "a negative prompt limit",
			policy: config.TierPolicyConfig{LocalMaxPromptTokens: -1},
			want:   "routing.tier_policy.local_max_prompt_tokens: must not be negative",
		},
		{
			name:   "a very negative prompt limit",
			policy: config.TierPolicyConfig{LocalMaxPromptTokens: -4096},
			want:   "routing.tier_policy.local_max_prompt_tokens: must not be negative",
		},
		{
			name:   "a negative completion limit",
			policy: config.TierPolicyConfig{LocalMaxCompletionTokens: -1},
			want:   "routing.tier_policy.local_max_completion_tokens: must not be negative",
		},
		{
			name:   "both limits negative",
			policy: config.TierPolicyConfig{LocalMaxPromptTokens: -1, LocalMaxCompletionTokens: -1},
			want:   "must not be negative",
		},
	}
	for _, tc := range cases {
		cfg := tierValidateConfig(config.StrategyTiered, tc.policy, tieredFleet()...)
		err := cfg.Validate()
		if !c.assertLoop(err != nil, "config: %s is rejected (%v)", tc.name, err) {
			continue
		}
		c.assertLoop(strings.Contains(err.Error(), tc.want),
			"config: %s reports %q (got %q)", tc.name, tc.want, err.Error())
	}

	// ...and the limits are validated whatever the strategy is, because the
	// same policy struct is reachable from a config that is not tiered yet.
	for _, strategy := range []string{"priority", "cost", "latency", "weighted", "score", "tiered", ""} {
		cfg := tierValidateConfig(strategy, config.TierPolicyConfig{LocalMaxPromptTokens: -1}, tieredFleet()...)
		c.assertLoop(cfg.Validate() != nil,
			"config: the strategy %q rejects a negative prompt limit too", strategy)
	}

	// Zero is a value: it disables that one rule, and it must not be an error.
	for _, policy := range []config.TierPolicyConfig{
		{LocalMaxPromptTokens: 0},
		{LocalMaxCompletionTokens: 0},
		{LocalMaxPromptTokens: 0, LocalMaxCompletionTokens: 0},
	} {
		cfg := tierValidateConfig(config.StrategyTiered, policy, tieredFleet()...)
		if !c.assertLoop(cfg.Validate() == nil, "config: the limits %d/%d are accepted (%v)",
			policy.LocalMaxPromptTokens, policy.LocalMaxCompletionTokens, cfg.Validate()) {
			continue
		}
		c.assertLoop(cfg.Routing.TierPolicy.LocalMaxPromptTokens == policy.LocalMaxPromptTokens,
			"config: a zero prompt limit is preserved as zero (got %d)", cfg.Routing.TierPolicy.LocalMaxPromptTokens)
		c.assertLoop(cfg.Routing.TierPolicy.LocalMaxCompletionTokens == policy.LocalMaxCompletionTokens,
			"config: a zero completion limit is preserved as zero (got %d)", cfg.Routing.TierPolicy.LocalMaxCompletionTokens)
	}

	// An empty cloud_capabilities means "the defaults", not "no capabilities":
	// an empty list would make every request simple and hand the tool calls to
	// a local model that cannot serve them.
	defaults := config.Defaults().Routing.TierPolicy.CloudCapabilities
	if c.assert(len(defaults) == 2, "config: the shipped default cloud capabilities are two entries (got %v)", defaults) {
		for _, want := range []string{"tools", "vision"} {
			found := false
			for _, got := range defaults {
				if got == want {
					found = true
				}
			}
			c.assertLoop(found, "config: the shipped defaults include %q (got %v)", want, defaults)
		}
	}
	for _, policy := range []config.TierPolicyConfig{
		{},
		{LocalMaxPromptTokens: 100},
		{CloudCapabilities: []string{}},
		{CloudCapabilities: nil},
	} {
		cfg := tierValidateConfig(config.StrategyTiered, policy, tieredFleet()...)
		if !c.assertLoop(cfg.Validate() == nil, "config: an empty capability list validates (%v)", cfg.Validate()) {
			continue
		}
		got := cfg.Routing.TierPolicy.CloudCapabilities
		if !c.assertLoop(len(got) == len(defaults), "config: an empty capability list becomes the default %v (got %v)", defaults, got) {
			continue
		}
		for i := range defaults {
			c.assertLoop(got[i] == defaults[i],
				"config: default capability %d is %q (got %q)", i, defaults[i], got[i])
		}
	}

	// A NON-empty list is configuration and is preserved verbatim -- including
	// its case, which the matcher folds at match time rather than at load time.
	explicit := []string{"Audio", " tools ", "VISION"}
	cfg := tierValidateConfig(config.StrategyTiered, config.TierPolicyConfig{CloudCapabilities: explicit}, tieredFleet()...)
	if c.assert(cfg.Validate() == nil, "config: an explicit capability list validates (%v)", cfg.Validate()) {
		got := cfg.Routing.TierPolicy.CloudCapabilities
		if c.assert(len(got) == len(explicit), "config: an explicit capability list keeps its length (got %v)", got) {
			for i := range explicit {
				c.assert(got[i] == explicit[i],
					"config: explicit capability %d is preserved verbatim, case and space included (want %q, got %q)", i, explicit[i], got[i])
			}
		}
	}

	// A fully specified policy survives validation unchanged, so the router
	// sees the numbers the operator wrote.
	policy := config.TierPolicyConfig{LocalMaxPromptTokens: 1234, LocalMaxCompletionTokens: 567, CloudCapabilities: []string{"tools"}}
	cfg = tierValidateConfig(config.StrategyTiered, policy, tieredFleet()...)
	if c.assert(cfg.Validate() == nil, "config: a full policy validates (%v)", cfg.Validate()) {
		c.assert(cfg.Routing.TierPolicy.LocalMaxPromptTokens == 1234,
			"config: the prompt limit round-trips (got %d)", cfg.Routing.TierPolicy.LocalMaxPromptTokens)
		c.assert(cfg.Routing.TierPolicy.LocalMaxCompletionTokens == 567,
			"config: the completion limit round-trips (got %d)", cfg.Routing.TierPolicy.LocalMaxCompletionTokens)
		c.assert(len(cfg.Routing.TierPolicy.CloudCapabilities) == 1 && cfg.Routing.TierPolicy.CloudCapabilities[0] == "tools",
			"config: the capability list round-trips (got %v)", cfg.Routing.TierPolicy.CloudCapabilities)
	}
}

// ---------------------------------------------------------------------------
// 10. strategy: tiered needs somewhere local to send the simple traffic
// ---------------------------------------------------------------------------

func checkTieredRequiresLocal(c *checker, e *environment) {
	want := `routing: strategy "tiered" requires at least one upstream with tier: local`

	cases := []struct {
		name string
		ups  []config.UpstreamConfig
	}{
		{name: "one cloud upstream", ups: []config.UpstreamConfig{tierUpstreamCfg("cloud", config.TierCloud)}},
		{name: "two cloud upstreams", ups: []config.UpstreamConfig{tierUpstreamCfg("cloud", config.TierCloud), tierUpstreamCfg("cloud2", config.TierCloud)}},
		{name: "an upstream that never mentions a tier", ups: []config.UpstreamConfig{tierUpstreamCfg("legacy", "")}},
		{name: "a wrong-case tier that means cloud", ups: []config.UpstreamConfig{tierUpstreamCfg("legacy", "CLOUD")}},
	}
	for _, tc := range cases {
		cfg := tierValidateConfig(config.StrategyTiered, config.TierPolicyConfig{}, tc.ups...)
		err := cfg.Validate()
		if !c.assertLoop(err != nil, "config: tiered with %s is rejected (%v)", tc.name, err) {
			continue
		}
		c.assertLoop(strings.Contains(err.Error(), want),
			"config: tiered with %s reports %q (got %q)", tc.name, want, err.Error())
	}
	cfg := tierValidateConfig(config.StrategyTiered, config.TierPolicyConfig{}, tierUpstreamCfg("cloud", config.TierCloud))
	if err := cfg.Validate(); c.assert(err != nil && err.Error() == want,
		"config: the tiered-without-local error is exactly %q (got %v)", want, err) {
		c.info("the message is documented in configs/tiered.yaml, so it is part of the contract")
	}

	// A local tier spelled in another case still counts, because validation
	// normalises before it looks.
	for _, spelling := range []string{"local", "Local", "LOCAL", "  local "} {
		cfg := tierValidateConfig(config.StrategyTiered, config.TierPolicyConfig{},
			tierUpstreamCfg("cloud", "cloud"), tierUpstreamCfg("box", spelling))
		if !c.assertLoop(cfg.Validate() == nil, "config: tiered accepts a local tier spelled %q (%v)", spelling, cfg.Validate()) {
			continue
		}
		c.assertLoop(cfg.Upstreams[1].Tier == config.TierLocal,
			"config: the local tier spelled %q normalises to local (got %q)", spelling, cfg.Upstreams[1].Tier)
	}

	// The strategy name itself is case-insensitive, and once normalised the
	// rule applies to it.
	cfg = tierValidateConfig("Tiered", config.TierPolicyConfig{}, tierUpstreamCfg("cloud", "cloud"))
	err := cfg.Validate()
	if c.assert(err != nil, "config: the strategy %q is normalised and then needs a local tier (%v)", "Tiered", err) {
		c.assert(cfg.Routing.Strategy == config.StrategyTiered,
			"config: the strategy is stored normalised as %q (got %q)", config.StrategyTiered, cfg.Routing.Strategy)
	}

	// The requirement belongs to the tiered strategy alone: a cloud-only fleet
	// is fine on every other strategy, and on the default.
	for _, strategy := range []string{"priority", "cost", "latency", "weighted", "score", ""} {
		cfg := tierValidateConfig(strategy, config.TierPolicyConfig{}, tierUpstreamCfg("cloud", config.TierCloud))
		c.assertLoop(cfg.Validate() == nil,
			"config: the strategy %q does not require a local tier (%v)", strategy, cfg.Validate())
	}

	// The matrix: for every strategy, a fleet with no local tier is accepted
	// unless the strategy is tiered.
	fleets := []struct {
		name  string
		ups   []config.UpstreamConfig
		local bool
	}{
		{name: "cloud only", ups: []config.UpstreamConfig{tierUpstreamCfg("cloud", "cloud")}, local: false},
		{name: "local only", ups: []config.UpstreamConfig{tierUpstreamCfg("box", "local")}, local: true},
		{name: "both tiers", ups: []config.UpstreamConfig{tierUpstreamCfg("box", "local"), tierUpstreamCfg("cloud", "cloud")}, local: true},
	}
	for _, strategy := range []string{"priority", "cost", "latency", "weighted", "score", "tiered", ""} {
		for _, fleet := range fleets {
			cfg := tierValidateConfig(strategy, config.TierPolicyConfig{}, fleet.ups...)
			err := cfg.Validate()
			tiered := strategy == config.StrategyTiered
			wantErr := tiered && !fleet.local
			c.assertLoop((err != nil) == wantErr,
				"config: strategy %q with %s %s (%v)", strategy, fleet.name, map[bool]string{true: "is rejected", false: "is accepted"}[wantErr], err)
			if wantErr && err != nil {
				c.assertLoop(strings.Contains(err.Error(), want),
					"config: strategy %q with %s reports the documented error (got %q)", strategy, fleet.name, err.Error())
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 11. A tiered config makes the round trip
// ---------------------------------------------------------------------------

func checkTieredConfigRoundTrip(c *checker, e *environment) {
	// miniyaml decodes YAML through encoding/json, so this is the same path
	// config.Load takes -- minus the ${ENV} expansion, which needs a file and a
	// variable that must not be required just to run a gate.
	doc := `
server:
  listen: ":0"
routing:
  strategy: "tiered"
  tier_policy:
    local_max_prompt_tokens: 400
    local_max_completion_tokens: 256
    cloud_capabilities:
      - tools
      - vision
pricing:
  default:
    in: 1.0
    out: 3.0
  models:
    local-chat:
      in: 0.27
      out: 1.1
upstreams:
  - name: "local-vllm"
    kind: "openai"
    base_url: "http://127.0.0.1:8000"
    tier: "local"
    models:
      - "/"
    capabilities:
      - chat
    priority: 1
  - name: "cloud-mock"
    kind: "openai"
    base_url: "http://127.0.0.1:9100"
    tier: "cloud"
    models:
      - "/"
    capabilities:
      - chat
      - tools
      - vision
    priority: 2
`
	cfg := config.Defaults()
	if !c.assert(miniyaml.Unmarshal([]byte(doc), &cfg) == nil, "config: the tiered YAML document decodes") {
		return
	}

	c.assert(cfg.Routing.Strategy == config.StrategyTiered,
		"config: routing.strategy round-trips as %q (got %q)", config.StrategyTiered, cfg.Routing.Strategy)
	c.assert(cfg.Routing.TierPolicy.LocalMaxPromptTokens == 400,
		"config: local_max_prompt_tokens round-trips (got %d)", cfg.Routing.TierPolicy.LocalMaxPromptTokens)
	c.assert(cfg.Routing.TierPolicy.LocalMaxCompletionTokens == 256,
		"config: local_max_completion_tokens round-trips (got %d)", cfg.Routing.TierPolicy.LocalMaxCompletionTokens)
	caps := cfg.Routing.TierPolicy.CloudCapabilities
	if c.assert(len(caps) == 2, "config: cloud_capabilities round-trips as a two-entry block sequence (got %v)", caps) {
		for i, want := range []string{"tools", "vision"} {
			c.assert(caps[i] == want,
				"config: cloud_capabilities[%d] is %q (got %q)", i, want, caps[i])
		}
	}

	if !c.assert(len(cfg.Upstreams) == 2, "config: two upstreams round-trip (got %d)", len(cfg.Upstreams)) {
		return
	}
	c.assert(cfg.Upstreams[0].Name == "local-vllm" && cfg.Upstreams[1].Name == "cloud-mock",
		"config: the upstream order is the order in the file (%q, %q)", cfg.Upstreams[0].Name, cfg.Upstreams[1].Name)
	c.assert(cfg.Upstreams[0].Tier == config.TierLocal,
		"config: the first upstream's tier round-trips as local (got %q)", cfg.Upstreams[0].Tier)
	c.assert(cfg.Upstreams[1].Tier == config.TierCloud,
		"config: the second upstream's tier round-trips as cloud (got %q)", cfg.Upstreams[1].Tier)
	if c.assert(len(cfg.Upstreams[0].Models) == 1, "config: the models block sequence round-trips (got %v)", cfg.Upstreams[0].Models) {
		c.assert(cfg.Upstreams[0].Models[0] == "/",
			"config: the catch-all model round-trips as %q (got %q)", "/", cfg.Upstreams[0].Models[0])
	}
	c.assert(len(cfg.Upstreams[1].Capabilities) == 3,
		"config: the cloud capabilities round-trip as three entries (got %v)", cfg.Upstreams[1].Capabilities)
	c.assert(cfg.Upstreams[0].Priority == 1 && cfg.Upstreams[1].Priority == 2,
		"config: the priorities round-trip (%d, %d)", cfg.Upstreams[0].Priority, cfg.Upstreams[1].Priority)
	price, ok := cfg.Pricing.Models["local-chat"]
	c.assert(ok && price.In == 0.27 && price.Out == 1.1,
		"config: the nested model price round-trips (%v, present=%t)", price, ok)

	// The decoded document is a usable config, not merely a populated struct:
	// this is the difference between "the YAML parsed" and "the YAML works".
	if c.assert(cfg.Validate() == nil, "config: the decoded tiered document validates (%v)", cfg.Validate()) {
		c.assert(cfg.Upstreams[0].Tier == config.TierLocal && cfg.Upstreams[1].Tier == config.TierCloud,
			"config: the tiers survive validation unchanged (%q, %q)", cfg.Upstreams[0].Tier, cfg.Upstreams[1].Tier)
	}

	// The three indentation styles miniyaml accepts for a block sequence all
	// produce the same tiers. This is the shape the shipped configs use, and it
	// is the shape an operator will edit.
	styles := []struct {
		name string
		doc  string
	}{
		{
			name: "dash and key on one line",
			doc:  "upstreams:\n  - name: \"box\"\n    tier: \"local\"\n    base_url: \"http://127.0.0.1:8000\"\n    models:\n      - \"/\"\n  - name: \"cloud\"\n    tier: \"cloud\"\n    base_url: \"http://127.0.0.1:9100\"\n    models:\n      - \"/\"\n",
		},
		{
			name: "bare dash then the mapping",
			doc:  "upstreams:\n  -\n    name: \"box\"\n    tier: \"local\"\n    base_url: \"http://127.0.0.1:8000\"\n    models:\n      - \"/\"\n  -\n    name: \"cloud\"\n    tier: \"cloud\"\n    base_url: \"http://127.0.0.1:9100\"\n    models:\n      - \"/\"\n",
		},
		{
			name: "keys indented past the dash",
			doc:  "upstreams:\n  -   name: \"box\"\n      tier: \"local\"\n      base_url: \"http://127.0.0.1:8000\"\n      models:\n        - \"/\"\n  -   name: \"cloud\"\n      tier: \"cloud\"\n      base_url: \"http://127.0.0.1:9100\"\n      models:\n        - \"/\"\n",
		},
	}
	for _, style := range styles {
		var got config.Config
		if !c.assertLoop(miniyaml.Unmarshal([]byte(style.doc), &got) == nil, "config: the %q style decodes", style.name) {
			continue
		}
		if !c.assertLoop(len(got.Upstreams) == 2, "config: the %q style yields two upstreams (got %d)", style.name, len(got.Upstreams)) {
			continue
		}
		c.assertLoop(got.Upstreams[0].Tier == "local" || got.Upstreams[0].Tier == "cloud",
			"config: the %q style carries a tier on the first upstream (got %q)", style.name, got.Upstreams[0].Tier)
		c.assertLoop(got.Upstreams[1].Tier == "cloud",
			"config: the %q style carries the cloud tier on the second upstream (got %q)", style.name, got.Upstreams[1].Tier)
	}

	// Controls. Without these, every assertion above could pass because the
	// decoder is ignoring the whole document: the keys below are spelled the
	// way a Go field is, not the way its json tag is, so they must NOT land.
	var camel config.Config
	if c.assert(miniyaml.Unmarshal([]byte("routing:\n  tierPolicy:\n    localMaxPromptTokens: 400\n"), &camel) == nil,
		"config: a camelCase document still decodes (unknown keys are ignored)") {
		c.assert(camel.Routing.TierPolicy.LocalMaxPromptTokens == 0,
			"config: the key is the json tag, not the Go field name (camelCase localMaxPromptTokens left %d)",
			camel.Routing.TierPolicy.LocalMaxPromptTokens)
	}

	// ...and the tier really is a string field: a number in that position must
	// be a decode error rather than a quietly zero-valued tier.
	var numeric config.Config
	err := miniyaml.Unmarshal([]byte("upstreams:\n  - name: \"box\"\n    tier: 3\n    base_url: \"http://127.0.0.1:8000\"\n    models:\n      - \"/\"\n"), &numeric)
	if c.assert(err != nil, "config: a numeric tier is a type error (%v)", err) {
		c.info("the decoder reports: %v", err)
	}

	// An unquoted tier is a YAML scalar, and it must reach the struct as the
	// same string a quoted one does.
	var plain config.Config
	if c.assert(miniyaml.Unmarshal([]byte("upstreams:\n  - name: \"box\"\n    tier: local\n    base_url: \"http://127.0.0.1:8000\"\n    models:\n      - \"/\"\n"), &plain) == nil,
		"config: an unquoted tier decodes") {
		c.assert(plain.Upstreams[0].Tier == "local",
			"config: an unquoted tier arrives as %q (got %q)", "local", plain.Upstreams[0].Tier)
	}

	// The shipped default policy is part of the contract too: an operator who
	// omits tier_policy gets these numbers.
	def := config.Defaults()
	c.assert(def.Routing.TierPolicy.LocalMaxPromptTokens == 0 && def.Routing.TierPolicy.LocalMaxCompletionTokens == 0,
		"config: the default limits are zero, which disables both length rules (%d/%d)",
		def.Routing.TierPolicy.LocalMaxPromptTokens, def.Routing.TierPolicy.LocalMaxCompletionTokens)
	c.assert(fmt.Sprintf("%v", def.Routing.TierPolicy.CloudCapabilities) == "[tools vision]",
		"config: the default cloud capabilities are tools and vision (got %v)", def.Routing.TierPolicy.CloudCapabilities)
}
