package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The loader normalises YAML to JSON, so the json tags ARE the config schema.
// This test exists because a cache section that parses into the wrong struct
// fields would be invisible: the gateway would start, and simply never cache.
func TestLoadCacheSectionFromYAML(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":8080"
log:
  level: "info"
upstreams:
  - name: "mock"
    base_url: "http://127.0.0.1:9000"
    models:
      - "/"
cache:
  enabled: true
  store: "redis"
  threshold: 0.9
  ttl: "5m"
  max_entries_per_scope: 32
  min_prompt_chars: 20
  allow_nondeterministic: true
  allow_tools: true
  embedding:
    provider: "http"
    dims: 256
    base_url: "http://127.0.0.1:9001/"
    model: "text-embedding-3-small"
    timeout: "2s"
    max_input_chars: 4000
  redis:
    addr: "127.0.0.1:6399"
    db: 2
    prefix: "ig:test"
    pool_size: 4
    read_timeout: "1s"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Cache
	if !c.Enabled || c.Store != CacheStoreRedis {
		t.Fatalf("enabled/store = %v/%q", c.Enabled, c.Store)
	}
	if c.Threshold != 0.9 {
		t.Fatalf("threshold = %v", c.Threshold)
	}
	if c.TTL.Duration() != 5*time.Minute {
		t.Fatalf("ttl = %v", c.TTL.Duration())
	}
	if c.MaxEntriesPerScope != 32 || c.MinPromptChars != 20 {
		t.Fatalf("bounds = %d/%d", c.MaxEntriesPerScope, c.MinPromptChars)
	}
	if !c.AllowNondeterministic || !c.AllowTools {
		t.Fatal("the opt-ins did not survive the load")
	}
	if c.Embedding.Provider != EmbedProviderHTTP || c.Embedding.Dims != 256 {
		t.Fatalf("embedding = %+v", c.Embedding)
	}
	if c.Embedding.BaseURL != "http://127.0.0.1:9001" {
		t.Fatalf("base_url = %q (the trailing slash should be trimmed)", c.Embedding.BaseURL)
	}
	if c.Embedding.Timeout.Duration() != 2*time.Second || c.Embedding.MaxInputChars != 4000 {
		t.Fatalf("embedding limits = %+v", c.Embedding)
	}
	if c.Redis.Addr != "127.0.0.1:6399" || c.Redis.DB != 2 || c.Redis.Prefix != "ig:test" || c.Redis.PoolSize != 4 {
		t.Fatalf("redis = %+v", c.Redis)
	}
	if c.Redis.ReadTimeout.Duration() != time.Second {
		t.Fatalf("read_timeout = %v", c.Redis.ReadTimeout.Duration())
	}
	// Unset durations in the same section keep their defaults rather than
	// becoming zero, which would mean "no timeout" to the RESP2 client.
	if c.Redis.DialTimeout.Duration() != Defaults().Cache.Redis.DialTimeout.Duration() {
		t.Fatalf("dial_timeout = %v, want the default", c.Redis.DialTimeout.Duration())
	}
}

func TestCacheSectionIsOptionalAndOffByDefault(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":8080"
upstreams:
  - name: "mock"
    base_url: "http://127.0.0.1:9000"
    models:
      - "/"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cache.Enabled {
		t.Fatal("the cache must default to off: enabling it changes what a caller can observe")
	}
	// Off is not the same as unset: the defaults are filled in so that
	// `enabled: true` alone is a working configuration.
	want := Defaults().Cache
	want.Enabled = false
	if !reflect.DeepEqual(cfg.Cache, want) {
		t.Fatalf("cache defaults = %+v, want %+v", cfg.Cache, want)
	}
}

// Validate is called on both the file path and hand-built configs in code, so it
// must be safe to call twice and must not keep re-normalising.
func TestValidateIsIdempotent(t *testing.T) {
	cfg := Defaults()
	cfg.Upstreams = []UpstreamConfig{{Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}}
	cfg.Cache.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("first Validate: %v", err)
	}
	first := cfg.Cache
	if err := cfg.Validate(); err != nil {
		t.Fatalf("second Validate: %v", err)
	}
	if !reflect.DeepEqual(first, cfg.Cache) {
		t.Fatalf("Validate is not idempotent:\n first %+v\nsecond %+v", first, cfg.Cache)
	}
}

func TestCacheValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"unknown store", func(c *Config) { c.Cache.Store = "rediz" },
			`cache.store: unsupported store "rediz"`},
		{"threshold too high", func(c *Config) { c.Cache.Threshold = 1.5 },
			"cache.threshold: must be within (0, 1]"},
		{"negative threshold", func(c *Config) { c.Cache.Threshold = -0.5 },
			"cache.threshold: must be within (0, 1]"},
		{"negative ttl", func(c *Config) { c.Cache.TTL = Duration(-time.Minute) },
			"cache.ttl: must not be negative"},
		{"negative bound", func(c *Config) { c.Cache.MaxEntriesPerScope = -1 },
			"cache.max_entries_per_scope: must not be negative"},
		{"negative min prompt", func(c *Config) { c.Cache.MinPromptChars = -1 },
			"cache.min_prompt_chars: must not be negative"},
		{"unknown provider", func(c *Config) { c.Cache.Embedding.Provider = "bert" },
			`cache.embedding.provider: unsupported provider "bert"`},
		{"negative dims", func(c *Config) { c.Cache.Embedding.Dims = -8 },
			"cache.embedding.dims: must not be negative"},
		{"negative embedding timeout", func(c *Config) { c.Cache.Embedding.Timeout = Duration(-time.Second) },
			"cache.embedding.timeout: must not be negative"},
		{"negative max input chars", func(c *Config) { c.Cache.Embedding.MaxInputChars = -1 },
			"cache.embedding.max_input_chars: must not be negative"},
		{"negative redis db", func(c *Config) { c.Cache.Redis.DB = -1 },
			"cache.redis.db: must not be negative"},
		{"prefix with a space", func(c *Config) { c.Cache.Redis.Prefix = "ig cache" },
			"cache.redis.prefix: must not contain whitespace"},
		{"prefix ending in a colon", func(c *Config) { c.Cache.Redis.Prefix = "ig:cache:" },
			`cache.redis.prefix: must not end with ":"`},
		{"negative pool size", func(c *Config) { c.Cache.Redis.PoolSize = -1 },
			"cache.redis.pool_size: must not be negative"},
		{"negative read timeout", func(c *Config) { c.Cache.Redis.ReadTimeout = Duration(-time.Second) },
			"cache.redis.read_timeout: must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Upstreams = []UpstreamConfig{{Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}}
			cfg.Cache.Enabled = true
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// A section that is switched off is still checked for enum typos, but it does
// not have to be complete. Those two rules pull in opposite directions and both
// matter: a typo silently ignored is a broken deployment, while forcing an
// unfinished section to be complete makes the feature hard to stage.
func TestCacheValidationWhileDisabled(t *testing.T) {
	base := func() Config {
		cfg := Defaults()
		cfg.Upstreams = []UpstreamConfig{{Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}}
		return cfg
	}

	// Incomplete but structurally valid: accepted while off.
	cfg := base()
	cfg.Cache.Embedding.Provider = EmbedProviderHTTP // no base_url, no model
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a disabled cache must not require a complete embedding section: %v", err)
	}

	// Still an error while off, because it would be an error the moment the
	// feature was switched on and nobody would connect the two events.
	cfg = base()
	cfg.Cache.Embedding.Provider = "htpp"
	if err := cfg.Validate(); err == nil {
		t.Fatal("a misspelled provider must be rejected even while the cache is disabled")
	}

	// And complete-validation applies once enabled.
	cfg = base()
	cfg.Cache.Enabled = true
	cfg.Cache.Embedding.Provider = EmbedProviderHTTP
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "cache.embedding.base_url: required when provider is http") {
		t.Fatalf("err = %v, want the base_url requirement", err)
	}
}

// Secrets in the cache section expand from the environment exactly like an
// upstream API key, and an unset variable is an error rather than an empty
// string.
func TestCacheSecretsExpandFromEnv(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":8080"
upstreams:
  - name: "mock"
    base_url: "http://127.0.0.1:9000"
    models:
      - "/"
cache:
  enabled: true
  store: "redis"
  embedding:
    provider: "http"
    base_url: "http://127.0.0.1:9001"
    model: "text-embedding-3-small"
    api_key: "${IG_TEST_EMBED_KEY}"
  redis:
    password: "${IG_TEST_REDIS_PASS}"
`)

	t.Setenv("IG_TEST_EMBED_KEY", "embed-secret")
	t.Setenv("IG_TEST_REDIS_PASS", "redis-secret")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cache.Embedding.APIKey != "embed-secret" {
		t.Fatalf("embedding key = %q", cfg.Cache.Embedding.APIKey)
	}
	if cfg.Cache.Redis.Password != "redis-secret" {
		t.Fatalf("redis password = %q", cfg.Cache.Redis.Password)
	}

	// Unset: the error must name the variable, not produce an empty credential.
	t.Setenv("IG_TEST_EMBED_KEY", "")
	os.Unsetenv("IG_TEST_EMBED_KEY")
	if _, err := Load(path); err == nil {
		t.Fatal("an undefined environment variable must be an error")
	} else if !strings.Contains(err.Error(), "IG_TEST_EMBED_KEY") {
		t.Fatalf("error = %q, want it to name the variable", err.Error())
	}
}

// The quota section is validated by the same two rules as the cache: structural
// mistakes are always errors, completeness is only required once enabled.
func TestQuotaValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"unknown store", func(c *Config) { c.Quota.Store = "rediz" },
			`quota: unsupported store "rediz"`},
		{"negative completion estimate", func(c *Config) { c.Quota.EstimateCompletionTokens = -1 },
			"quota.estimate_completion_tokens: must not be negative"},
		{"negative chars per token", func(c *Config) { c.Quota.EstimateCharsPerToken = -4 },
			"quota.estimate_chars_per_token: must not be negative"},
		{"negative anomaly ratio", func(c *Config) { c.Quota.AnomalyRatio = -2 },
			"quota.anomaly_ratio: must not be negative"},
		// A ratio under 1 would fire whenever a tenant spent less than last
		// period, which is the opposite of an anomaly.
		{"sub-unit anomaly ratio", func(c *Config) { c.Quota.AnomalyRatio = 0.5 },
			"quota.anomaly_ratio: must be at least 1"},
		{"prefix with a space", func(c *Config) { c.Quota.Redis.Prefix = "ig quota" },
			"quota.redis.prefix: must not contain whitespace"},
		{"prefix ending in a colon", func(c *Config) { c.Quota.Redis.Prefix = "ig:quota:" },
			`quota.redis.prefix: must not end with ":"`},
		{"negative pool size", func(c *Config) { c.Quota.Redis.PoolSize = -1 },
			"quota.redis.pool_size: must not be negative"},
		{"negative read timeout", func(c *Config) { c.Quota.Redis.ReadTimeout = Duration(-time.Second) },
			"quota.redis.read_timeout: must not be negative"},
		{"empty tenant while enabled", func(c *Config) {
			c.Quota.Enabled = true
			c.Quota.Tenants = []TenantQuotaConfig{{Tenant: "  "}}
		}, "quota.tenants[0]: tenant is required"},
		{"duplicate tenant", func(c *Config) {
			c.Quota.Tenants = []TenantQuotaConfig{{Tenant: "acme"}, {Tenant: "acme"}}
		}, `quota.tenants: duplicate tenant "acme"`},
		{"unknown action", func(c *Config) {
			c.Quota.Tenants = []TenantQuotaConfig{{Tenant: "acme", OnExceed: "block"}}
		}, `quota.tenants[acme].on_exceed: unsupported action "block"`},
		{"negative tenant tokens", func(c *Config) {
			c.Quota.Tenants = []TenantQuotaConfig{{Tenant: "acme", TokensPerDay: -1}}
		}, "quota.tenants[acme].tokens_per_day: must not be negative"},
		{"negative tenant cost", func(c *Config) {
			c.Quota.Tenants = []TenantQuotaConfig{{Tenant: "acme", CostPerDayUSD: -0.5}}
		}, "quota.tenants[acme].cost_per_day_usd: must not be negative"},
		{"negative tenant rpm", func(c *Config) {
			c.Quota.Tenants = []TenantQuotaConfig{{Tenant: "acme", RequestsPerMinute: -1}}
		}, "quota.tenants[acme].requests_per_minute: must not be negative"},
		{"negative tenant session budget", func(c *Config) {
			c.Quota.Tenants = []TenantQuotaConfig{{Tenant: "acme", TokensPerSession: -1}}
		}, "quota.tenants[acme].tokens_per_session: must not be negative"},
		{"negative token cap", func(c *Config) {
			c.Quota.Tenants = []TenantQuotaConfig{{Tenant: "acme", MaxTokensCap: -1}}
		}, "quota.tenants[acme].max_tokens_cap: must not be negative"},
		// The dangerous one: a ladder with no rungs would silently behave like
		// "reject", which is not what "degrade" promised.
		{"degrade with an empty ladder", func(c *Config) {
			c.Quota.Tenants = []TenantQuotaConfig{{Tenant: "acme", OnExceed: QuotaActionDegrade}}
		}, "on_exceed is degrade but neither downgrade_model nor max_tokens_cap is set"},
		{"degrade with an empty ladder on the default policy", func(c *Config) {
			c.Quota.DefaultPolicy.OnExceed = QuotaActionDegrade
		}, "quota.default_policy: on_exceed is degrade but neither downgrade_model nor max_tokens_cap is set"},
		{"negative tenant anomaly ratio", func(c *Config) {
			c.Quota.Tenants = []TenantQuotaConfig{{Tenant: "acme", AnomalyRatio: -1}}
		}, "quota.tenants[acme].anomaly_ratio: must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Upstreams = []UpstreamConfig{{Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}}
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// A disabled quota section still rejects enum typos, and the redis address is
// only required once the section is both enabled and redis-backed.
func TestQuotaValidationWhileDisabled(t *testing.T) {
	base := func() Config {
		cfg := Defaults()
		cfg.Upstreams = []UpstreamConfig{{Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}}
		return cfg
	}

	// An incomplete store is fine while the feature is off.
	cfg := base()
	cfg.Quota.Store = QuotaStoreRedis
	cfg.Quota.Redis.Addr = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a disabled quota store must not require an address: %v", err)
	}
	// The shipped defaults carry an address, so a config that simply omits the
	// field keeps one; only an explicit "" leaves it empty, and that is what
	// the enabled check below catches.
	if Defaults().Quota.Redis.Addr == "" {
		t.Fatal("the defaults must carry a redis address")
	}

	cfg = base()
	cfg.Quota.Store = "rediz"
	if err := cfg.Validate(); err == nil {
		t.Fatal("a misspelled store must be rejected even while the quota is disabled")
	}

	cfg = base()
	cfg.Quota.Enabled = true
	cfg.Quota.Store = QuotaStoreRedis
	cfg.Quota.Redis.Addr = ""
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "quota.redis.addr: required when store is redis and quota is enabled") {
		t.Fatalf("err = %v, want the addr requirement", err)
	}
}

// Inheritance runs section -> default_policy -> tenant, and an unset action
// defaults to reject rather than to something that lets traffic through.
func TestQuotaInheritance(t *testing.T) {
	cfg := Defaults()
	cfg.Upstreams = []UpstreamConfig{{Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}}
	cfg.Quota.AnomalyRatio = 2.5
	cfg.Quota.DefaultPolicy.OnExceed = QuotaActionDegrade
	cfg.Quota.DefaultPolicy.MaxTokensCap = 128
	cfg.Quota.Tenants = []TenantQuotaConfig{
		{Tenant: "inherits"},
		{Tenant: "override", OnExceed: QuotaActionReject},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.Quota.Tenants[0].OnExceed; got != QuotaActionDegrade {
		t.Fatalf("tenant action = %q, want it to inherit degrade", got)
	}
	if got := cfg.Quota.Tenants[1].OnExceed; got != QuotaActionReject {
		t.Fatalf("tenant action = %q, want the explicit reject", got)
	}
	// The section ratio is what a tenant with none of its own lands on.
	if got := cfg.Quota.Tenants[0].AnomalyRatio; got != 2.5 {
		t.Fatalf("tenant anomaly ratio = %v, want 2.5", got)
	}
	if got := cfg.Quota.DefaultPolicy.AnomalyRatio; got != 2.5 {
		t.Fatalf("default policy anomaly ratio = %v, want 2.5", got)
	}
}

// The redis password in the quota section expands from the environment exactly
// like the cache's, and an unset variable is an error that names it.
func TestQuotaRedisPasswordExpandsFromEnv(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":8080"
upstreams:
  - name: "mock"
    base_url: "http://127.0.0.1:9000"
    models:
      - "/"
quota:
  enabled: true
  store: "redis"
  redis:
    addr: "127.0.0.1:6379"
    password: "${IG_TEST_QUOTA_PASS}"
`)

	t.Setenv("IG_TEST_QUOTA_PASS", "quota-secret")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Quota.Redis.Password != "quota-secret" {
		t.Fatalf("quota redis password = %q", cfg.Quota.Redis.Password)
	}

	os.Unsetenv("IG_TEST_QUOTA_PASS")
	if _, err := Load(path); err == nil {
		t.Fatal("an undefined environment variable must be an error")
	} else if !strings.Contains(err.Error(), "IG_TEST_QUOTA_PASS") {
		t.Fatalf("error = %q, want it to name the variable", err.Error())
	}
}

// A tier is normalised at load time, so neither the router nor the registry has
// to decide what an empty tier means. A typo is rejected rather than defaulted:
// "tier: locale" rounding to cloud is the direction that silently spends money.
func TestUpstreamTierValidation(t *testing.T) {
	tests := []struct {
		name    string
		tier    string
		want    string
		wantErr string
	}{
		{name: "explicit local", tier: "local", want: TierLocal},
		{name: "explicit cloud", tier: "cloud", want: TierCloud},
		{name: "omitted means cloud", tier: "", want: TierCloud},
		{name: "case is not significant", tier: "LoCaL", want: TierLocal},
		{name: "surrounding space is trimmed", tier: "  cloud  ", want: TierCloud},
		{
			name:    "unknown tier is an error",
			tier:    "locale",
			wantErr: `upstream mock: unsupported tier "locale" (want local or cloud)`,
		},
		{
			name:    "a tier that is only close is still an error",
			tier:    "edge",
			wantErr: `upstream mock: unsupported tier "edge" (want local or cloud)`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Upstreams = []UpstreamConfig{{
				Name:    "mock",
				BaseURL: "http://127.0.0.1:9000",
				Models:  []string{"/"},
				Tier:    tc.tier,
			}}
			err := cfg.Validate()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := cfg.Upstreams[0].Tier; got != tc.want {
				t.Fatalf("tier = %q, want %q", got, tc.want)
			}
		})
	}
}

// The tier policy's limits are "disabled at zero", so a negative can only be a
// mistake -- and a mistake that would, if accepted, send every request to the
// cloud.
func TestTierPolicyValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*TierPolicyConfig)
		wantErr string
	}{
		{
			name:    "negative prompt limit",
			mutate:  func(tp *TierPolicyConfig) { tp.LocalMaxPromptTokens = -1 },
			wantErr: "routing.tier_policy.local_max_prompt_tokens: must not be negative",
		},
		{
			name:    "negative completion limit",
			mutate:  func(tp *TierPolicyConfig) { tp.LocalMaxCompletionTokens = -1 },
			wantErr: "routing.tier_policy.local_max_completion_tokens: must not be negative",
		},
		{
			name:   "zero disables both limits",
			mutate: func(tp *TierPolicyConfig) { *tp = TierPolicyConfig{} },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Routing.Strategy = StrategyTiered
			cfg.Upstreams = []UpstreamConfig{
				{Name: "local", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}, Tier: TierLocal},
				{Name: "cloud", BaseURL: "https://api.example.com", Models: []string{"/"}},
			}
			tc.mutate(&cfg.Routing.TierPolicy)
			err := cfg.Validate()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			// An unset capability list gets the defaults rather than an empty
			// one, which would make every request look simple.
			if got := cfg.Routing.TierPolicy.CloudCapabilities; !reflect.DeepEqual(got, []string{"tools", "vision"}) {
				t.Fatalf("cloud capabilities = %v, want the defaults", got)
			}
		})
	}

	// An explicit list is the operator's, and is not merged with the defaults.
	cfg := Defaults()
	cfg.Upstreams = []UpstreamConfig{{Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}}
	cfg.Routing.TierPolicy.CloudCapabilities = []string{"audio"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.Routing.TierPolicy.CloudCapabilities; !reflect.DeepEqual(got, []string{"audio"}) {
		t.Fatalf("cloud capabilities = %v, want the configured list", got)
	}
}

// "tiered" with no local backend is a typo, not a deployment: nothing would
// ever sort ahead of the cloud tier, so the strategy would silently behave
// exactly like "priority" while reading as if the local tier were in use.
func TestTieredRequiresLocalUpstream(t *testing.T) {
	withUpstreams := func(tiers ...string) *Config {
		cfg := Defaults()
		cfg.Routing.Strategy = StrategyTiered
		for i, tier := range tiers {
			cfg.Upstreams = append(cfg.Upstreams, UpstreamConfig{
				Name:    "u" + string(rune('0'+i)),
				BaseURL: "http://127.0.0.1:9000",
				Models:  []string{"/"},
				Tier:    tier,
			})
		}
		return &cfg
	}

	if err := withUpstreams("cloud", "").Validate(); err == nil {
		t.Fatal("tiered with no local upstream must be rejected")
	} else if !strings.Contains(err.Error(), `routing: strategy "tiered" requires at least one upstream with tier: local`) {
		t.Fatalf("err = %v, want the local-tier requirement", err)
	}

	if err := withUpstreams("cloud", "local").Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// The same fleet is fine under a strategy that does not use tiers.
	cfg := withUpstreams("cloud")
	cfg.Routing.Strategy = StrategyPriority
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// The point of defaulting an omitted tier to cloud is that a config written
// before tiers existed loads with its routing unchanged.
func TestOmittedTierLoadsAsCloud(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":8080"
routing:
  strategy: "tiered"
upstreams:
  - name: "legacy"
    base_url: "http://127.0.0.1:9000"
    models:
      - "/"
  - name: "box"
    base_url: "http://127.0.0.1:9001"
    models:
      - "/"
    tier: "local"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Upstreams[0].Tier; got != TierCloud {
		t.Fatalf("omitted tier = %q, want %q", got, TierCloud)
	}
	if got := cfg.Upstreams[1].Tier; got != TierLocal {
		t.Fatalf("declared tier = %q, want %q", got, TierLocal)
	}
	if got := cfg.Routing.TierPolicy.CloudCapabilities; !reflect.DeepEqual(got, []string{"tools", "vision"}) {
		t.Fatalf("cloud capabilities = %v, want the defaults", got)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// Tracing is off by default, and an omitted section must still leave the
// store's parameters usable so that turning it on is a one-line change.
func TestTracingDefaultsWhenOmitted(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":8080"
upstreams:
  - name: "mock"
    base_url: "http://127.0.0.1:9000"
    models:
      - "/"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tracing.Enabled {
		t.Fatal("tracing is on by default; it retains request metadata and must be opt-in")
	}
	if cfg.Tracing.Capacity != 1024 {
		t.Fatalf("capacity = %d, want 1024", cfg.Tracing.Capacity)
	}
	if cfg.Tracing.SampleRatio != 1 {
		t.Fatalf("sample_ratio = %v, want 1", cfg.Tracing.SampleRatio)
	}
	if cfg.Tracing.JSONLPath != "" {
		t.Fatalf("jsonl_path = %q, want empty", cfg.Tracing.JSONLPath)
	}
	if got := cfg.Tracing.OTLP.Timeout.Duration(); got != 5*time.Second {
		t.Fatalf("otlp.timeout = %v, want 5s", got)
	}
	if got := cfg.Tracing.OTLP.ServiceName; got != "infergate" {
		t.Fatalf("otlp.service_name = %q, want infergate", got)
	}
	if cfg.Tracing.OTLP.Headers == nil {
		t.Fatal("otlp.headers is nil; an exporter that ranges over it should not need a nil check")
	}
}

// The section must round-trip from YAML into the struct the wiring reads.
func TestTracingSectionParses(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":8080"
upstreams:
  - name: "mock"
    base_url: "http://127.0.0.1:9000"
    models:
      - "/"
tracing:
  enabled: true
  capacity: 512
  sample_ratio: 0.25
  jsonl_path: "tmp/traces.jsonl"
  otlp:
    endpoint: "http://127.0.0.1:4318"
    timeout: "2s"
    service_name: "infergate-m5"
    headers:
      "x-api-key": "secret"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tc := cfg.Tracing
	if !tc.Enabled || tc.Capacity != 512 || tc.SampleRatio != 0.25 {
		t.Fatalf("tracing = %+v, want enabled capacity 512 ratio 0.25", tc)
	}
	if tc.JSONLPath != "tmp/traces.jsonl" {
		t.Fatalf("jsonl_path = %q", tc.JSONLPath)
	}
	if tc.OTLP.Endpoint != "http://127.0.0.1:4318" || tc.OTLP.ServiceName != "infergate-m5" {
		t.Fatalf("otlp = %+v", tc.OTLP)
	}
	if got := tc.OTLP.Timeout.Duration(); got != 2*time.Second {
		t.Fatalf("otlp.timeout = %v, want 2s", got)
	}
	if tc.OTLP.Headers["x-api-key"] != "secret" {
		t.Fatalf("otlp.headers = %v", tc.OTLP.Headers)
	}
}

func TestTracingValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"negative capacity", func(c *Config) { c.Tracing.Capacity = -1 },
			"tracing.capacity: must not be negative"},
		{"negative sample ratio", func(c *Config) { c.Tracing.SampleRatio = -0.1 },
			"tracing.sample_ratio: must be between 0 and 1"},
		{"sample ratio above one", func(c *Config) { c.Tracing.SampleRatio = 1.5 },
			"tracing.sample_ratio: must be between 0 and 1"},
		{"negative otlp timeout", func(c *Config) { c.Tracing.OTLP.Timeout = Duration(-time.Second) },
			"tracing.otlp.timeout: must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Upstreams = []UpstreamConfig{{Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}}
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// Zero means "unset" for capacity and sample_ratio, never "record nothing":
// a store of capacity 0 would drop every trace while the config claims tracing
// is on, which is the one misreading that looks like a bug in the gateway.
func TestTracingZeroMeansUnset(t *testing.T) {
	cfg := Defaults()
	cfg.Upstreams = []UpstreamConfig{{Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}}
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 0
	cfg.Tracing.SampleRatio = 0
	cfg.Tracing.OTLP.Timeout = 0
	cfg.Tracing.OTLP.ServiceName = "   "
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Tracing.Capacity != 1024 {
		t.Fatalf("capacity = %d, want the default 1024", cfg.Tracing.Capacity)
	}
	if cfg.Tracing.SampleRatio != 1 {
		t.Fatalf("sample_ratio = %v, want the default 1", cfg.Tracing.SampleRatio)
	}
	if got := cfg.Tracing.OTLP.Timeout.Duration(); got != 5*time.Second {
		t.Fatalf("otlp.timeout = %v, want the default 5s", got)
	}
	if cfg.Tracing.OTLP.ServiceName != "infergate" {
		t.Fatalf("otlp.service_name = %q, want the default", cfg.Tracing.OTLP.ServiceName)
	}
}
