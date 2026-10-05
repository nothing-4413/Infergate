// Package config defines InferGate's process-level configuration and the loader
// that turns it into a validated, immutable value.
//
// M0 keeps configuration intentionally small: one listening address, a set of
// upstream OpenAI-compatible backends, logging options and a price book. Later
// milestones extend this schema (routing policy, cache policy, quota policy)
// without changing the loader contract.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/miniyaml"
)

// unmarshalYAML decodes YAML into v.
//
// M0 uses a small hand-written YAML subset parser rather than a third-party
// dependency, so the gateway builds from a bare toolchain with no module
// downloads. The parser normalises YAML to JSON and lets encoding/json do the
// typing, which means the json struct tags on the types below are the single
// source of truth for the config schema — and `-print-config` round-trips
// exactly what the loader understood.
func unmarshalYAML(raw []byte, v any) error {
	return miniyaml.Unmarshal(raw, v)
}

// Config is the complete process configuration.
type Config struct {
	Server    ServerConfig     `json:"server"`
	Upstreams []UpstreamConfig `json:"upstreams"`
	Routing   RoutingConfig    `json:"routing"`
	Health    HealthConfig     `json:"health"`
	Log       LogConfig        `json:"log"`
	Pricing   PricingConfig    `json:"pricing"`
	Cache     CacheConfig      `json:"cache"`
	Quota     QuotaConfig      `json:"quota"`
}

// HealthConfig parameterises the per-upstream circuit breaker.
//
// Every threshold here is expressed against a SLIDING WINDOW rather than a
// lifetime counter. A lifetime counter is the classic breaker bug: an upstream
// that failed a thousand times yesterday can never be trusted again, because the
// ratio never recovers no matter how healthy it is now.
type HealthConfig struct {
	// Window is the observation window for failure and latency statistics.
	Window Duration `json:"window"`

	// Buckets is how many slices the window is divided into. More buckets make
	// the window slide more smoothly at the cost of a little memory. A window
	// of 60s with 6 buckets means statistics fall out of scope in 10s steps.
	Buckets int `json:"buckets"`

	// MinRequests is how many attempts must be observed in the window before
	// the breaker may open. Below it a single unlucky failure would trip a
	// breaker on a healthy-but-quiet backend.
	MinRequests int `json:"min_requests"`

	// FailureRatio opens the breaker when the windowed error ratio reaches it
	// (0.5 = half the attempts failed).
	FailureRatio float64 `json:"failure_ratio"`

	// OpenDuration is how long the breaker stays open before it lets a single
	// probe request through (half-open).
	OpenDuration Duration `json:"open_duration"`

	// HalfOpenProbes is how many consecutive successes in the half-open state
	// are needed to close the breaker again. One success is enough for a mock,
	// but too few for a flapping provider, so the default is 2.
	HalfOpenProbes int `json:"half_open_probes"`

	// MaxFailuresPerRequest bounds the failover chain length. It exists to stop
	// an amplification loop: three broken backends multiplied by three retries
	// each would turn one client request into nine upstream requests, and the
	// cost of that is paid by the provider's rate limiter.
	MaxFailuresPerRequest int `json:"max_failures_per_request"`

	// RetryBackoff is the base delay before the next attempt. Successive
	// attempts multiply it (linear growth) and jitter is added, because a fixed
	// delay makes every client retry in lockstep and re-creates the stampede.
	RetryBackoff Duration `json:"retry_backoff"`
}

// RoutingConfig selects and weights the routing strategy.
type RoutingConfig struct {
	// Strategy is one of "priority" (config order / explicit priority), "cost"
	// (cheapest first), "latency" (fastest observed first), "weighted" (random
	// by weight) or "score" (normalised weighted sum of cost, latency and
	// failure ratio). Unknown values are rejected at load time rather than
	// silently degrading to the first backend.
	Strategy string `json:"strategy"`

	// Weights tune the "score" strategy. They do not need to sum to 1: they are
	// normalised against their own total, so an operator can write
	// "cost: 3, latency: 1" without doing arithmetic.
	Weights RoutingWeights `json:"weights"`

	// DefaultCapabilities is applied to an upstream that does not declare its
	// own capability tags.
	DefaultCapabilities []string `json:"default_capabilities"`

	// FallbackModel, when set, is the model name to ask for on a retry where
	// the caller's model does not exist on the candidate backend.
	FallbackModel string `json:"fallback_model"`

	// TierPolicy tunes the "tiered" strategy: which requests are cheap enough
	// for the local tier, and which capabilities only the cloud tier has.
	TierPolicy TierPolicyConfig `json:"tier_policy"`
}

// TierPolicyConfig describes the local/cloud split for the "tiered" strategy.
//
// A zero limit means "no limit", not "everything is too big": a deployment
// that only wants to gate on capabilities writes no numbers at all, and a
// limit of zero must never send every request to the cloud.
type TierPolicyConfig struct {
	// LocalMaxPromptTokens is the largest estimated prompt the local tier is
	// asked to serve. Zero disables the rule.
	LocalMaxPromptTokens int `json:"local_max_prompt_tokens"`

	// LocalMaxCompletionTokens is the largest completion ceiling the local
	// tier is asked to serve. Zero disables the rule.
	LocalMaxCompletionTokens int `json:"local_max_completion_tokens"`

	// CloudCapabilities are the capability tags that force a request to the
	// cloud tier. They default to the two tags a small local model is most
	// likely to lack outright (see Defaults).
	CloudCapabilities []string `json:"cloud_capabilities"`
}

// RoutingWeights are the score-strategy coefficients.
type RoutingWeights struct {
	// Cost is the weight of normalised price per 1M tokens.
	Cost float64 `json:"cost"`

	// Latency is the weight of normalised recent latency.
	Latency float64 `json:"latency"`

	// Reliability is the weight of the windowed failure ratio. It is the only
	// term that can take a backend out of rotation on its own.
	Reliability float64 `json:"reliability"`

	// Priority is the weight of the configured priority (lower wins).
	Priority float64 `json:"priority"`
}

// Duration is a time.Duration that reads the human-readable form from YAML.
//
// encoding/json cannot decode "90s" into a bare time.Duration: a Duration is an
// int64 underneath, so a JSON string is a type error, and writing 90000000000
// in a config file is unusable. Implemented in UnmarshalJSON here — rather than
// by pre-processing the YAML — so the conversion is testable on its own and the
// schema stays a plain struct with json tags.
//
// Accepted: "90s", "10m", "1h30m" (YAML's own duration syntax, which is
// time.ParseDuration) and a bare integer, read as nanoseconds so JSON produced
// by other tools still loads.
type Duration time.Duration

// UnmarshalJSON accepts a duration string or a nanosecond count.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*d = 0
		return nil
	}
	if s[0] == '"' {
		var text string
		if err := json.Unmarshal(b, &text); err != nil {
			return err
		}
		parsed, err := time.ParseDuration(strings.TrimSpace(text))
		if err != nil {
			return fmt.Errorf("invalid duration %q (use a Go duration such as \"90s\", \"10m\", \"1h30m\")", text)
		}
		*d = Duration(parsed)
		return nil
	}
	ns, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid duration %s (use a quoted duration such as \"90s\")", s)
	}
	*d = Duration(ns)
	return nil
}

// MarshalJSON emits the human-readable form so a config can be round-tripped
// and logged without exposing raw nanosecond counts.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Duration returns the value as a standard library duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the human-readable form, so logs and error messages read
// "15s" rather than a nanosecond count.
func (d Duration) String() string { return time.Duration(d).String() }

// ServerConfig holds HTTP listener settings and the deadlines that bound a
// single proxied attempt. The deadlines exist so a wedged upstream can never
// pin a goroutine forever.
type ServerConfig struct {
	// Listen is the TCP address the gateway binds, e.g. ":8080".
	Listen string `json:"listen"`

	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers. It is the primary Slowloris defence.
	ReadHeaderTimeout Duration `json:"read_header_timeout"`

	// IdleTimeout bounds keep-alive connections with no in-flight request.
	IdleTimeout Duration `json:"idle_timeout"`

	// UpstreamTimeout bounds the whole upstream exchange, including the
	// streaming body. Zero means "no gateway-level cap" (the client's own
	// disconnect still cancels the request).
	UpstreamTimeout Duration `json:"upstream_timeout"`

	// ShutdownTimeout bounds how long a graceful shutdown waits for in-flight
	// requests before closing connections.
	ShutdownTimeout Duration `json:"shutdown_timeout"`

	// MaxBodyBytes caps the size of an inbound request body. LLM requests can
	// carry large prompts, so the default is generous but still finite.
	MaxBodyBytes int64 `json:"max_body_bytes"`

	// DisableKeepAlives, when true, asks Go's HTTP client not to reuse
	// upstream connections. Useful for debugging, harmful for throughput.
	DisableKeepAlives bool `json:"disable_keep_alives"`

	// MaxIdleConnsPerHost sizes the upstream connection pool per backend.
	MaxIdleConnsPerHost int `json:"max_idle_conns_per_host"`
}

// UpstreamConfig describes one OpenAI-compatible backend.
type UpstreamConfig struct {
	// Name is the stable identity used in logs, metrics labels and headers.
	Name string `json:"name"`

	// Kind is the dialect of the backend. Only "openai" is supported in M0.
	Kind string `json:"kind"`

	// BaseURL is the upstream root, e.g. "https://api.deepseek.com".
	// The inbound request path is appended verbatim, so the gateway stays a
	// transparent reverse proxy rather than a path-rewriting shim.
	BaseURL string `json:"base_url"`

	// APIKey is the bearer credential for this backend. It is expanded from
	// ${ENV} during load and must never be committed.
	APIKey string `json:"api_key"`

	// Models is the routing allow-list. An entry "/" terminates the list and
	// acts as a catch-all, matching any model name (this mirrors the classic
	// nginx location-prefix idiom and is deliberate, not an accident).
	Models []string `json:"models"`

	// Capabilities are the feature tags this backend supports, matched against
	// the capabilities a request requires (function calling, JSON mode, long
	// context, vision...). A request that requires a missing capability skips
	// this backend rather than failing on it.
	Capabilities []string `json:"capabilities"`

	// Priority orders candidates in the "priority" strategy: lower wins. All
	// else being equal it is also the final tie-break, which keeps routing
	// deterministic when the score strategy produces equal scores.
	Priority int `json:"priority"`

	// Weight is the relative share for the "weighted" strategy. Zero means
	// "unweighted"; negative values are rejected.
	Weight float64 `json:"weight"`

	// Tier places this backend in the "local" or "cloud" half of a tiered
	// deployment. Empty means "cloud", so a config written before tiers
	// existed keeps routing where it always did instead of silently becoming
	// local.
	Tier string `json:"tier"`
}

// LogConfig selects the logging surface.
type LogConfig struct {
	// Level is one of debug, info, warn, error.
	Level string `json:"level"`

	// Format is "text" (human) or "json" (machine).
	Format string `json:"format"`
}

// PricingConfig is the USD price book used for token cost accounting.
//
// Prices are USD per one million tokens, which is how every commercial
// provider quotes them; storing them that way avoids a lossy unit convention.
type PricingConfig struct {
	// Default applies to any model without an explicit entry.
	Default ModelPrice `json:"default"`

	// Models overrides the default per model name.
	Models map[string]ModelPrice `json:"models"`
}

// ModelPrice is the price of one million tokens in USD for one model.
type ModelPrice struct {
	// In is USD per 1M prompt (input) tokens.
	In float64 `json:"in"`

	// Out is USD per 1M completion (output) tokens.
	Out float64 `json:"out"`
}

// CacheConfig parameterises the M2 semantic cache.
//
// The cache is OFF by default, and that is a deliberate default rather than
// caution: a cache changes what a caller can observe - an upstream that would
// have been called is not called, and the answer may be minutes old - so it is
// an operational decision an operator makes explicitly, not something a version
// upgrade turns on underneath them. Every other field still has a considered
// default, so enabling it is one line.
type CacheConfig struct {
	// Enabled turns the cache on. While false the section is still validated
	// (a typo in an enum is worth an error even when the feature is off) but it
	// does not have to be complete.
	Enabled bool `json:"enabled"`

	// Store is "memory" or "redis". Memory is the single-replica default because
	// it needs nothing and has no failure mode; redis is what makes a cache
	// shared across replicas, and costs a network hop plus a dependency.
	Store string `json:"store"`

	// Threshold is the cosine similarity a stored prompt must reach. It is
	// measured, not guessed: internal/cache.DefaultThreshold records the corpus
	// and the numbers behind the default. Lowering it raises the hit rate and
	// starts answering questions that were not asked.
	Threshold float64 `json:"threshold"`

	// TTL bounds staleness. It is anchored at creation and NOT refreshed by
	// hits: refreshing on hit means one hot question can keep an answer alive
	// forever, which is exactly how a cache silently serves last week's data.
	TTL Duration `json:"ttl"`

	// MaxEntriesPerScope bounds the working set per tenant+model. The memory
	// store evicts least-recently-used; the redis store evicts oldest-first
	// because Redis cannot report read order (see internal/cache/redis.go).
	MaxEntriesPerScope int `json:"max_entries_per_scope"`

	// MinPromptChars is the shortest prompt worth caching. One-word prompts
	// collide semantically with almost anything, and their answers are cheap.
	MinPromptChars int `json:"min_prompt_chars"`

	// AllowNondeterministic permits semantic hits for temperature > 0. Off by
	// default: the caller explicitly asked for variety, and a cached answer
	// silently overrides that. Exact hits still happen when it is off - a
	// repeated identical request is a repetition, not a variance.
	AllowNondeterministic bool `json:"allow_nondeterministic"`

	// AllowTools permits semantic hits for requests carrying tools. Off by
	// default because two paraphrases can legitimately choose different tools,
	// and replaying a tool_call the caller did not ask for is worse than paying
	// for the call.
	AllowTools bool `json:"allow_tools"`

	// Embedding selects how prompts are vectorised.
	Embedding EmbeddingConfig `json:"embedding"`

	// Redis configures the shared store.
	Redis RedisConfig `json:"redis"`
}

// EmbeddingConfig selects the embedding provider.
type EmbeddingConfig struct {
	// Provider is "hashing", "http" or "none".
	//
	// "hashing" is the offline default: signed feature hashing over word
	// unigrams and bigrams (CJK character bigrams), no network, deterministic
	// across processes so replicas agree on a vector. It is lexical, which caps
	// how much paraphrase it can catch - the measured ceiling is in
	// internal/cache.DefaultThreshold.
	//
	// "http" calls an OpenAI-compatible /embeddings endpoint and raises that
	// ceiling, at the cost of a network dependency on the request path.
	//
	// "none" disables semantic matching entirely: the cache still answers
	// byte-identical requests, which is the cheap half of the benefit.
	Provider string `json:"provider"`

	// Dims is the vector width for the hashing provider. It is stored with each
	// entry, so changing it invalidates old entries rather than mixing
	// dimensions (a length mismatch scores 0, i.e. a miss).
	Dims int `json:"dims"`

	// BaseURL, Model and APIKey configure the http provider. BaseURL is the
	// service root (the /embeddings path is appended), matching how an upstream
	// base_url is written.
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`

	// Timeout bounds one embedding call. It is short on purpose: the embedding
	// is on the request path, and the cache must not be able to make a request
	// slower than not having a cache - past this the lookup degrades to a miss
	// and the request proceeds.
	Timeout Duration `json:"timeout"`

	// MaxInputChars truncates the text sent to an embedding endpoint. Prompts
	// are unbounded, and an embeddings API has a context limit that would
	// otherwise turn a long prompt into a 400 and a lost cache lookup.
	MaxInputChars int `json:"max_input_chars"`
}

// RedisConfig configures the shared cache store.
type RedisConfig struct {
	// Addr is host:port. There is no URL form: the RESP2 client in
	// internal/redis speaks one protocol to one address, and a fake that
	// accepted redis:// would only hide which parts are supported.
	Addr string `json:"addr"`

	// Password is empty for an unauthenticated instance. It is expanded from
	// ${ENV} by the loader like an upstream API key.
	Password string `json:"password"`

	// DB selects the Redis logical database.
	DB int `json:"db"`

	// Prefix namespaces every key the gateway writes, so a shared Redis can
	// hold more than one gateway's data. Keys are
	// <prefix>:<scope>:meta|vec|idx|exp plus <prefix>:scopes.
	Prefix string `json:"prefix"`

	// PoolSize bounds concurrent connections. Each cache lookup is a couple of
	// round trips, so this is a small number even under load.
	PoolSize int `json:"pool_size"`

	// DialTimeout, ReadTimeout and WriteTimeout bound one round trip. They are
	// short for the same reason the embedding timeout is: a cache is never
	// allowed to be the reason a request is slow, so a timeout must surface as a
	// miss rather than as latency.
	DialTimeout  Duration `json:"dial_timeout"`
	ReadTimeout  Duration `json:"read_timeout"`
	WriteTimeout Duration `json:"write_timeout"`
}

// QuotaConfig configures multi-tenant token and cost budgets.
//
// This is the one section that can REFUSE traffic, so its defaults are chosen
// to be inert: disabled, and (when enabled) a tenant with no entry is unlimited
// rather than blocked. A gateway that silently stops serving every caller the
// operator forgot to list is a worse failure than an unbudgeted caller.
type QuotaConfig struct {
	// Enabled turns quota enforcement on. While false the section is still
	// validated but does not have to be complete.
	Enabled bool `json:"enabled"`

	// Store is "memory" or "redis". Memory counts per process, which is only
	// correct for a single replica; a shared fleet must use redis or each
	// replica hands out the full budget (the same argument as the cache's).
	Store string `json:"store"`

	// FailOpen decides what a store failure means. Closed by default: a budget
	// that cannot be read is not a budget, and admitting on error turns one
	// Redis outage into an unbounded bill. Operators who prefer availability to
	// spend can say so explicitly.
	FailOpen bool `json:"fail_open"`

	// EstimateCompletionTokens is the completion size reserved before the call,
	// when the request does not itself ask for one. It is released on settle.
	EstimateCompletionTokens int `json:"estimate_completion_tokens"`

	// EstimateCharsPerToken estimates prompt tokens from the prompt length. Four
	// is the usual English rule of thumb; it is only used for RESERVATION, never
	// for billing, because the provider's own usage is authoritative once it
	// arrives.
	EstimateCharsPerToken int `json:"estimate_chars_per_token"`

	// AnomalyRatio flags a tenant whose cost in a window exceeds this multiple of
	// the same window's previous period. 0 disables alerting. Alerting never
	// blocks a request: a spike is worth a human, not a 4xx.
	AnomalyRatio float64 `json:"anomaly_ratio"`

	// DefaultPolicy applies to tenants with no entry of their own. Its Tenant
	// field is ignored.
	DefaultPolicy TenantQuotaConfig `json:"default_policy"`

	// Tenants holds one policy per tenant. Order does not matter; a duplicate
	// tenant is a config error rather than a silent last-one-wins.
	Tenants []TenantQuotaConfig `json:"tenants"`

	// Redis configures the shared counter store. Keys are
	// <prefix>:<tenant>:<window>:<bucket>.
	Redis RedisConfig `json:"redis"`
}

// TenantQuotaConfig is one tenant's budget and the ladder to follow when it runs
// out. A zero limit means "no limit on this dimension" rather than "zero", so an
// entry can cap cost without also capping tokens.
type TenantQuotaConfig struct {
	// Tenant is the identity the quota is keyed by: the X-InferGate-Tenant
	// header when present, otherwise a hash of the credential. It is the same
	// identity the cache scopes by, so a tenant is one concept across the
	// gateway.
	Tenant string `json:"tenant"`

	// TokensPerDay, CostPerDayUSD and RequestsPerMinute are the day and minute
	// windows. Cost is computed from the configured price book, which is why the
	// gateway needs pricing to enforce a budget honestly.
	TokensPerDay      int64   `json:"tokens_per_day"`
	CostPerDayUSD     float64 `json:"cost_per_day_usd"`
	RequestsPerMinute int64   `json:"requests_per_minute"`

	// TokensPerSession bounds one conversation, keyed by X-InferGate-Session.
	// Zero disables the session dimension.
	TokensPerSession int64 `json:"tokens_per_session"`

	// OnExceed is "reject" or "degrade". Degrade walks the ladder in
	// TenantQuotaConfig order: a cheaper model first, then a shorter answer, and
	// only then a refusal - a budget should buy less, not nothing.
	OnExceed string `json:"on_exceed"`

	// DowngradeModel is the model to switch to when the ladder starts. It is
	// named explicitly rather than guessed from the price book because the cheap
	// model must still be one the caller's request shape works with.
	DowngradeModel string `json:"downgrade_model"`

	// MaxTokensCap caps max_tokens on a degraded request. It is a ceiling, not a
	// value: a request that already asks for fewer tokens keeps its own number.
	MaxTokensCap int `json:"max_tokens_cap"`

	// AnomalyRatio overrides the section-level ratio for this tenant.
	AnomalyRatio float64 `json:"anomaly_ratio"`
}

// Supported cache stores and embedding providers.
const (
	// CacheStoreMemory keeps entries in this process. It is the default: it has
	// no dependency and no failure mode.
	CacheStoreMemory = "memory"

	// CacheStoreRedis shares entries between replicas through Redis.
	CacheStoreRedis = "redis"

	// EmbedProviderHashing is the offline, deterministic lexical embedder.
	EmbedProviderHashing = "hashing"

	// EmbedProviderHTTP calls an OpenAI-compatible embeddings endpoint.
	EmbedProviderHTTP = "http"

	// EmbedProviderNone disables semantic matching.
	EmbedProviderNone = "none"
)

// Supported quota stores and exhaustion actions.
const (
	// QuotaStoreMemory counts in this process. Correct only for one replica.
	QuotaStoreMemory = "memory"

	// QuotaStoreRedis shares counters between replicas through Redis.
	QuotaStoreRedis = "redis"

	// QuotaActionReject refuses the request with the gateway's quota error.
	QuotaActionReject = "reject"

	// QuotaActionDegrade spends less instead of refusing: a cheaper model
	// and/or a shorter answer.
	QuotaActionDegrade = "degrade"
)

// Supported upstream dialects.
const (
	KindOpenAI = "openai"
)

// Supported backend tiers. The tier only matters to the "tiered" strategy; an
// unknown value is rejected rather than defaulted, because a misspelled
// "tier: locale" would silently leave the whole local fleet in the cloud tier
// and the operator would learn about it from the bill.
const (
	// TierLocal marks a backend on the operator's own hardware: cheap to run,
	// limited in capability and in how much prompt it is worth asking it to
	// handle.
	TierLocal = "local"

	// TierCloud marks a backend behind a paid API. It is also the tier of an
	// upstream that declares none.
	TierCloud = "cloud"
)

// Supported routing strategies.
const (
	// StrategyPriority orders by configured priority, then config order. It is
	// the default because it is the only strategy whose behaviour an operator
	// can predict from the file alone.
	StrategyPriority = "priority"

	// StrategyCost orders by the configured price book, cheapest first.
	StrategyCost = "cost"

	// StrategyLatency orders by a sliding-window latency EWMA.
	StrategyLatency = "latency"

	// StrategyWeighted picks randomly according to the configured weights,
	// which is how traffic is split deliberately (canary, quota smoothing).
	StrategyWeighted = "weighted"

	// StrategyScore orders by a normalised weighted sum of cost, latency,
	// reliability and priority.
	StrategyScore = "score"

	// StrategyTiered orders the local tier ahead of the cloud tier for
	// requests that look simple, and the cloud tier ahead of the local one for
	// the rest. Both tiers stay in the plan either way, so failover still
	// reaches the other half when one is down.
	StrategyTiered = "tiered"
)

// Defaults mirrors configs/infergate.yaml. Load applies them before overlaying
// the file, so a partially specified file is still a valid configuration.
func Defaults() Config {
	return Config{
		Server: ServerConfig{
			Listen:              ":8080",
			ReadHeaderTimeout:   Duration(10 * time.Second),
			IdleTimeout:         Duration(90 * time.Second),
			UpstreamTimeout:     Duration(10 * time.Minute),
			ShutdownTimeout:     Duration(15 * time.Second),
			MaxBodyBytes:        8 << 20,
			MaxIdleConnsPerHost: 64,
		},
		Log: LogConfig{Level: "info", Format: "text"},
		Routing: RoutingConfig{
			Strategy: StrategyPriority,
			// Weights are a starting point, not a truth: cost dominates because
			// it is the only term an operator controls exactly, reliability is
			// next because a failing backend costs a retry, and latency is
			// deliberately last because a shared host makes it noisy (see the
			// M0 baseline in docs/RESUME.md).
			Weights: RoutingWeights{Cost: 3, Latency: 1, Reliability: 2, Priority: 1},
			TierPolicy: TierPolicyConfig{
				// The two tags a small local model is most likely to lack, and
				// the two whose absence is worst: a backend that ignores tools
				// or images returns a confident answer about neither, which
				// reads as a model failure rather than a routing mistake.
				CloudCapabilities: []string{"tools", "vision"},
			},
		},
		Health: HealthConfig{
			Window:                Duration(60 * time.Second),
			Buckets:               6,
			MinRequests:           20,
			FailureRatio:          0.5,
			OpenDuration:          Duration(15 * time.Second),
			HalfOpenProbes:        2,
			MaxFailuresPerRequest: 3,
			RetryBackoff:          Duration(50 * time.Millisecond),
		},
		Pricing: PricingConfig{
			Default: ModelPrice{In: 0, Out: 0},
			Models:  map[string]ModelPrice{},
		},
		Cache: CacheConfig{
			Enabled: false,
			Store:   CacheStoreMemory,
			// 0.86 is the measured value documented on
			// internal/cache.DefaultThreshold: on a 26-pair labelled corpus it
			// holds a 0% wrong-answer rate at a 62% paraphrase hit rate. The
			// intuitive 0.94 scored 31% on the same corpus.
			Threshold:          0.86,
			TTL:                Duration(15 * time.Minute),
			MaxEntriesPerScope: 256,
			MinPromptChars:     12,
			Embedding: EmbeddingConfig{
				Provider: EmbedProviderHashing,
				// 512 dims is ~2.7 KiB per entry once base64-encoded, so a full
				// 256-entry scope is ~700 KiB read per semantic lookup. Higher
				// dims reduce hash collisions; 512 keeps the scan cheap enough
				// to run inline on the request path.
				Dims:          512,
				Timeout:       Duration(5 * time.Second),
				MaxInputChars: 8000,
			},
			Redis: RedisConfig{
				Addr:     "127.0.0.1:6379",
				Prefix:   "ig:cache",
				PoolSize: 16,
				// Short timeouts: a slow cache must become a miss, not latency.
				DialTimeout:  Duration(2 * time.Second),
				ReadTimeout:  Duration(2 * time.Second),
				WriteTimeout: Duration(2 * time.Second),
			},
		},
		Quota: QuotaConfig{
			Enabled: false,
			Store:   QuotaStoreMemory,
			// Closed by default: see FailOpen's comment. A budget that cannot be
			// read is not a budget.
			FailOpen: false,
			// 256 is not a guess about the answer's length; it is the hold placed
			// while the call is in flight. Too small a hold lets a tenant
			// overshoot the limit by one request; too large a hold makes
			// concurrent callers trip a limit they have not spent. It is
			// released in full on settle.
			EstimateCompletionTokens: 256,
			EstimateCharsPerToken:    4,
			// 3x the previous period is the point where a human should look. It
			// alerts, it never blocks.
			AnomalyRatio: 3,
			DefaultPolicy: TenantQuotaConfig{
				OnExceed: QuotaActionReject,
			},
			Redis: RedisConfig{
				Addr:     "127.0.0.1:6379",
				Prefix:   "ig:quota",
				PoolSize: 16,
				// Tighter than the cache's: a quota check is on the request path
				// exactly once, and a slow budget check is a slow gateway.
				DialTimeout:  Duration(2 * time.Second),
				ReadTimeout:  Duration(2 * time.Second),
				WriteTimeout: Duration(2 * time.Second),
			},
		},
	}
}

// Load reads a YAML file, overlays environment variables, fills defaults and
// validates the result. A missing file is an error: silently starting with no
// upstream is a worse failure mode than refusing to start.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	// Strip a UTF-8 byte-order mark. Notepad and several editors add one by
	// default, and it is an encoding artifact rather than content: left in
	// place, the first line reads as "\ufeffserver:" and the parser reports a
	// confusing "expected key: value" on whatever the first line happens to be
	// (usually a comment, which makes the message actively misleading).
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	cfg := Defaults()
	if err := unmarshalYAML(raw, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	// Expand ${ENV} references (typically API keys) after parsing so that a
	// missing secret produces a precise error naming the variable.
	if err := expandEnv(&cfg); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	if err := applyEnvOverrides(&cfg); err != nil {
		return nil, fmt.Errorf("config: env: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}

// Validate checks cross-field invariants and normalises the configuration in
// place. It is idempotent, so it is safe to call on a hand-built Config in
// tests as well as from Load.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		c.Server.Listen = Defaults().Server.Listen
	}
	if c.Server.ReadHeaderTimeout < 0 || c.Server.IdleTimeout < 0 ||
		c.Server.UpstreamTimeout < 0 || c.Server.ShutdownTimeout < 0 {
		return errors.New("server: timeouts must not be negative")
	}
	if c.Server.MaxBodyBytes <= 0 {
		c.Server.MaxBodyBytes = Defaults().Server.MaxBodyBytes
	}
	if c.Server.MaxIdleConnsPerHost <= 0 {
		c.Server.MaxIdleConnsPerHost = Defaults().Server.MaxIdleConnsPerHost
	}

	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
		c.Log.Level = strings.ToLower(c.Log.Level)
	case "":
		c.Log.Level = Defaults().Log.Level
	default:
		return fmt.Errorf("log: unsupported level %q", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "text", "json":
		c.Log.Format = strings.ToLower(c.Log.Format)
	case "":
		c.Log.Format = Defaults().Log.Format
	default:
		return fmt.Errorf("log: unsupported format %q", c.Log.Format)
	}

	if len(c.Upstreams) == 0 {
		return errors.New("upstreams: at least one backend is required")
	}
	seen := make(map[string]struct{}, len(c.Upstreams))
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if strings.TrimSpace(u.Name) == "" {
			return fmt.Errorf("upstreams[%d]: name is required", i)
		}
		if _, dup := seen[u.Name]; dup {
			return fmt.Errorf("upstreams[%d]: duplicate name %q", i, u.Name)
		}
		seen[u.Name] = struct{}{}

		if u.Kind == "" {
			u.Kind = KindOpenAI
		}
		if u.Kind != KindOpenAI {
			return fmt.Errorf("upstream %s: unsupported kind %q", u.Name, u.Kind)
		}

		u.BaseURL = strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
		parsed, err := url.Parse(u.BaseURL)
		if err != nil {
			return fmt.Errorf("upstream %s: base_url invalid: %w", u.Name, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("upstream %s: base_url must be http or https, got %q", u.Name, u.BaseURL)
		}
		if parsed.Host == "" {
			return fmt.Errorf("upstream %s: base_url has no host", u.Name)
		}
		if !hasCatchAll(u.Models) && len(u.Models) == 0 {
			return fmt.Errorf("upstream %s: models must list at least one model or the \"/\" catch-all", u.Name)
		}

		// The tier is normalised here, once, so the router never has to decide
		// what an empty tier means and a typo cannot degrade into "cloud" --
		// the silent direction that costs money.
		switch tier := strings.ToLower(strings.TrimSpace(u.Tier)); tier {
		case TierLocal, TierCloud:
			u.Tier = tier
		case "":
			u.Tier = TierCloud
		default:
			return fmt.Errorf("upstream %s: unsupported tier %q (want local or cloud)", u.Name, u.Tier)
		}
	}

	if c.Pricing.Models == nil {
		c.Pricing.Models = map[string]ModelPrice{}
	}
	for model, price := range c.Pricing.Models {
		if price.In < 0 || price.Out < 0 {
			return fmt.Errorf("pricing: model %q has negative price", model)
		}
	}
	if c.Pricing.Default.In < 0 || c.Pricing.Default.Out < 0 {
		return errors.New("pricing: default price must not be negative")
	}

	// Cache. Structural checks always run, even while the cache is switched off:
	// a silently ignored `store: rediz` becomes a redis-shaped deployment that
	// quietly uses process memory, and the operator finds out from a hit rate
	// rather than from an error. Only the cross-field completeness checks are
	// gated on Enabled, so a section can be written before the feature is
	// turned on without having to be finished first.
	dc := Defaults().Cache
	switch store := strings.ToLower(strings.TrimSpace(c.Cache.Store)); store {
	case CacheStoreMemory, CacheStoreRedis:
		c.Cache.Store = store
	case "":
		c.Cache.Store = dc.Store
	default:
		return fmt.Errorf("cache.store: unsupported store %q (want memory or redis)", c.Cache.Store)
	}
	if c.Cache.Threshold == 0 {
		c.Cache.Threshold = dc.Threshold
	}
	if c.Cache.Threshold < 0 || c.Cache.Threshold > 1 {
		return fmt.Errorf("cache.threshold: must be within (0, 1], got %v", c.Cache.Threshold)
	}
	// Zero means "unset" throughout this section, matching the health block, so
	// every default lives in Defaults() and nowhere else. Negative is a mistake
	// worth an error rather than a silent substitution.
	if c.Cache.TTL < 0 {
		return errors.New("cache.ttl: must not be negative")
	}
	if c.Cache.TTL == 0 {
		c.Cache.TTL = dc.TTL
	}
	if c.Cache.MaxEntriesPerScope < 0 {
		return errors.New("cache.max_entries_per_scope: must not be negative")
	}
	if c.Cache.MaxEntriesPerScope == 0 {
		c.Cache.MaxEntriesPerScope = dc.MaxEntriesPerScope
	}
	if c.Cache.MinPromptChars < 0 {
		return errors.New("cache.min_prompt_chars: must not be negative")
	}
	if c.Cache.MinPromptChars == 0 {
		c.Cache.MinPromptChars = dc.MinPromptChars
	}

	demb := dc.Embedding
	switch provider := strings.ToLower(strings.TrimSpace(c.Cache.Embedding.Provider)); provider {
	case EmbedProviderHashing, EmbedProviderHTTP, EmbedProviderNone:
		c.Cache.Embedding.Provider = provider
	case "":
		c.Cache.Embedding.Provider = demb.Provider
	default:
		return fmt.Errorf("cache.embedding.provider: unsupported provider %q (want hashing, http or none)", c.Cache.Embedding.Provider)
	}
	e := &c.Cache.Embedding
	e.BaseURL = strings.TrimRight(strings.TrimSpace(e.BaseURL), "/")
	if e.Dims < 0 {
		return errors.New("cache.embedding.dims: must not be negative")
	}
	if e.Dims == 0 {
		e.Dims = demb.Dims
	}
	if e.Timeout < 0 {
		return errors.New("cache.embedding.timeout: must not be negative")
	}
	if e.Timeout == 0 {
		e.Timeout = demb.Timeout
	}
	if e.MaxInputChars < 0 {
		return errors.New("cache.embedding.max_input_chars: must not be negative")
	}
	if e.MaxInputChars == 0 {
		e.MaxInputChars = demb.MaxInputChars
	}
	if c.Cache.Enabled && e.Provider == EmbedProviderHTTP {
		// An embeddings endpoint without an address or a model name cannot work,
		// and the failure would otherwise appear as "the cache never hits".
		if e.BaseURL == "" {
			return errors.New("cache.embedding.base_url: required when provider is http")
		}
		if strings.TrimSpace(e.Model) == "" {
			return errors.New("cache.embedding.model: required when provider is http")
		}
	}

	dr := dc.Redis
	r := &c.Cache.Redis
	r.Addr = strings.TrimSpace(r.Addr)
	if r.Addr == "" {
		r.Addr = dr.Addr
	}
	if r.DB < 0 {
		return fmt.Errorf("cache.redis.db: must not be negative, got %d", r.DB)
	}
	r.Prefix = strings.TrimSpace(r.Prefix)
	if r.Prefix == "" {
		r.Prefix = dr.Prefix
	}
	// Keys are built as <prefix>:<scope>:<part>, so an interior colon in the
	// prefix is fine (the default has one, and "ig:cache:tenant/x:meta" reads
	// better than "igcache_tenant_x_meta"). What cannot be allowed is whitespace
	// - a key that is hard to type into redis-cli is hard to inspect - or a
	// trailing colon, which would silently produce a doubled separator.
	if strings.ContainsAny(r.Prefix, " \t\r\n") {
		return fmt.Errorf("cache.redis.prefix: must not contain whitespace, got %q", r.Prefix)
	}
	if strings.HasSuffix(r.Prefix, ":") {
		return fmt.Errorf("cache.redis.prefix: must not end with %q, got %q", ":", r.Prefix)
	}
	if r.PoolSize < 0 {
		return errors.New("cache.redis.pool_size: must not be negative")
	}
	if r.PoolSize == 0 {
		r.PoolSize = dr.PoolSize
	}
	for name, d := range map[string]Duration{
		"dial_timeout":  r.DialTimeout,
		"read_timeout":  r.ReadTimeout,
		"write_timeout": r.WriteTimeout,
	} {
		if d < 0 {
			return fmt.Errorf("cache.redis.%s: must not be negative", name)
		}
	}
	if r.DialTimeout == 0 {
		r.DialTimeout = dr.DialTimeout
	}
	if r.ReadTimeout == 0 {
		r.ReadTimeout = dr.ReadTimeout
	}
	if r.WriteTimeout == 0 {
		r.WriteTimeout = dr.WriteTimeout
	}

	dq := Defaults().Quota
	switch strings.ToLower(c.Quota.Store) {
	case QuotaStoreMemory, QuotaStoreRedis:
		c.Quota.Store = strings.ToLower(c.Quota.Store)
	case "":
		c.Quota.Store = dq.Store
	default:
		return fmt.Errorf("quota: unsupported store %q (want memory or redis)", c.Quota.Store)
	}
	if c.Quota.EstimateCompletionTokens < 0 {
		return fmt.Errorf("quota.estimate_completion_tokens: must not be negative, got %d", c.Quota.EstimateCompletionTokens)
	}
	if c.Quota.EstimateCompletionTokens == 0 {
		c.Quota.EstimateCompletionTokens = dq.EstimateCompletionTokens
	}
	if c.Quota.EstimateCharsPerToken < 0 {
		return fmt.Errorf("quota.estimate_chars_per_token: must not be negative, got %d", c.Quota.EstimateCharsPerToken)
	}
	if c.Quota.EstimateCharsPerToken == 0 {
		c.Quota.EstimateCharsPerToken = dq.EstimateCharsPerToken
	}
	if c.Quota.AnomalyRatio < 0 {
		return fmt.Errorf("quota.anomaly_ratio: must not be negative, got %v", c.Quota.AnomalyRatio)
	}
	if c.Quota.AnomalyRatio == 0 {
		c.Quota.AnomalyRatio = dq.AnomalyRatio
	}
	if c.Quota.AnomalyRatio < 1 {
		// A ratio below 1 would alert whenever a tenant spent LESS than the
		// previous period, which is noise, not an anomaly.
		return fmt.Errorf("quota.anomaly_ratio: must be at least 1, got %v", c.Quota.AnomalyRatio)
	}
	qr := &c.Quota.Redis
	if qr.Prefix == "" {
		qr.Prefix = dq.Redis.Prefix
	}
	if strings.ContainsAny(qr.Prefix, " \t") {
		return fmt.Errorf("quota.redis.prefix: must not contain whitespace, got %q", qr.Prefix)
	}
	if strings.HasSuffix(qr.Prefix, ":") {
		// Otherwise every key would start with a doubled separator and two
		// prefixes that look different would collide.
		return fmt.Errorf("quota.redis.prefix: must not end with %q, got %q", ":", qr.Prefix)
	}
	if qr.PoolSize < 0 {
		return fmt.Errorf("quota.redis.pool_size: must not be negative, got %d", qr.PoolSize)
	}
	if qr.PoolSize == 0 {
		qr.PoolSize = dq.Redis.PoolSize
	}
	for name, d := range map[string]Duration{
		"dial_timeout":  qr.DialTimeout,
		"read_timeout":  qr.ReadTimeout,
		"write_timeout": qr.WriteTimeout,
	} {
		if d < 0 {
			return fmt.Errorf("quota.redis.%s: must not be negative", name)
		}
	}
	if qr.DialTimeout == 0 {
		qr.DialTimeout = dq.Redis.DialTimeout
	}
	if qr.ReadTimeout == 0 {
		qr.ReadTimeout = dq.Redis.ReadTimeout
	}
	if qr.WriteTimeout == 0 {
		qr.WriteTimeout = dq.Redis.WriteTimeout
	}
	if c.Quota.Enabled && c.Quota.Store == QuotaStoreRedis && strings.TrimSpace(qr.Addr) == "" {
		// The same argument as the cache's: a fleet that believes it is sharing
		// a budget while each replica counts alone hands out N times the limit.
		return errors.New("quota.redis.addr: required when store is redis and quota is enabled")
	}
	// The shipped default_policy carries no ratio of its own, so the section
	// value is what an unset per-tenant ratio inherits - via the default policy.
	dq.DefaultPolicy.AnomalyRatio = c.Quota.AnomalyRatio
	if err := validateTenantQuota("quota.default_policy", &c.Quota.DefaultPolicy, dq.DefaultPolicy); err != nil {
		return err
	}
	tenantDef := c.Quota.DefaultPolicy
	if tenantDef.AnomalyRatio == 0 {
		tenantDef.AnomalyRatio = c.Quota.AnomalyRatio
	}
	quotaSeen := make(map[string]bool, len(c.Quota.Tenants))
	for i := range c.Quota.Tenants {
		t := &c.Quota.Tenants[i]
		name := strings.TrimSpace(t.Tenant)
		if name == "" {
			if c.Quota.Enabled {
				return fmt.Errorf("quota.tenants[%d]: tenant is required", i)
			}
			continue
		}
		if quotaSeen[name] {
			// Last-one-wins would hide the typo that produced two entries for
			// the same tenant, and the two entries usually disagree.
			return fmt.Errorf("quota.tenants: duplicate tenant %q", name)
		}
		quotaSeen[name] = true
		t.Tenant = name
		if err := validateTenantQuota(fmt.Sprintf("quota.tenants[%s]", name), t, tenantDef); err != nil {
			return err
		}
	}

	switch strings.ToLower(c.Routing.Strategy) {
	case StrategyPriority, StrategyCost, StrategyLatency, StrategyWeighted, StrategyScore, StrategyTiered:
		c.Routing.Strategy = strings.ToLower(c.Routing.Strategy)
	case "":
		c.Routing.Strategy = Defaults().Routing.Strategy
	default:
		return fmt.Errorf("routing: unsupported strategy %q (want priority, cost, latency, weighted, score or tiered)", c.Routing.Strategy)
	}
	if w := c.Routing.Weights; w.Cost < 0 || w.Latency < 0 || w.Reliability < 0 || w.Priority < 0 {
		return errors.New("routing.weights: weights must not be negative")
	}
	if c.Routing.Weights == (RoutingWeights{}) {
		c.Routing.Weights = Defaults().Routing.Weights
	}

	dtp := Defaults().Routing.TierPolicy
	tp := &c.Routing.TierPolicy
	if tp.LocalMaxPromptTokens < 0 {
		return errors.New("routing.tier_policy.local_max_prompt_tokens: must not be negative")
	}
	if tp.LocalMaxCompletionTokens < 0 {
		return errors.New("routing.tier_policy.local_max_completion_tokens: must not be negative")
	}
	if len(tp.CloudCapabilities) == 0 {
		tp.CloudCapabilities = dtp.CloudCapabilities
	}
	if c.Routing.Strategy == StrategyTiered {
		// Checked against the already-normalised upstream tiers, so `tier: ""`
		// counts as cloud exactly as it does for the router.
		local := false
		for i := range c.Upstreams {
			if c.Upstreams[i].Tier == TierLocal {
				local = true
				break
			}
		}
		if !local {
			return errors.New(`routing: strategy "tiered" requires at least one upstream with tier: local`)
		}
	}

	dh := Defaults().Health
	if c.Health.Window <= 0 {
		c.Health.Window = dh.Window
	}
	if c.Health.OpenDuration <= 0 {
		c.Health.OpenDuration = dh.OpenDuration
	}
	if c.Health.Buckets <= 0 {
		c.Health.Buckets = dh.Buckets
	}
	if c.Health.MinRequests <= 0 {
		c.Health.MinRequests = dh.MinRequests
	}
	if c.Health.HalfOpenProbes <= 0 {
		c.Health.HalfOpenProbes = dh.HalfOpenProbes
	}
	if c.Health.MaxFailuresPerRequest <= 0 {
		c.Health.MaxFailuresPerRequest = dh.MaxFailuresPerRequest
	}
	if c.Health.RetryBackoff < 0 {
		return errors.New("health: retry_backoff must not be negative")
	}
	if c.Health.FailureRatio == 0 {
		c.Health.FailureRatio = dh.FailureRatio
	}
	if c.Health.FailureRatio < 0 || c.Health.FailureRatio > 1 {
		return fmt.Errorf("health: failure_ratio must be within (0, 1], got %v", c.Health.FailureRatio)
	}

	weights := 0.0
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.Weight < 0 {
			return fmt.Errorf("upstream %s: weight must not be negative", u.Name)
		}
		weights += u.Weight
	}
	if weights == 0 {
		// An unweighted fleet is treated as equally weighted. Doing it here
		// rather than in the picker means the picker never divides by zero and
		// the meaning of "weight: 0" is defined in exactly one place: default.
		for i := range c.Upstreams {
			c.Upstreams[i].Weight = 1
		}
	}
	return nil
}

// validateTenantQuota checks one tenant's budget and the ladder to follow when
// it runs out. `def` supplies the values a zero field inherits - the section's
// default_policy for the tenant list, the shipped default for default_policy
// itself.
//
// The ladder check is the interesting one: "on_exceed: degrade" with neither a
// downgrade model nor a token cap is an empty ladder, and an empty ladder that
// silently behaves like "reject" is exactly the kind of config that looks
// harmless in review and refuses production traffic.
func validateTenantQuota(name string, t *TenantQuotaConfig, def TenantQuotaConfig) error {
	if t.TokensPerDay < 0 {
		return fmt.Errorf("%s.tokens_per_day: must not be negative", name)
	}
	if t.CostPerDayUSD < 0 {
		return fmt.Errorf("%s.cost_per_day_usd: must not be negative", name)
	}
	if t.RequestsPerMinute < 0 {
		return fmt.Errorf("%s.requests_per_minute: must not be negative", name)
	}
	if t.TokensPerSession < 0 {
		return fmt.Errorf("%s.tokens_per_session: must not be negative", name)
	}
	if t.MaxTokensCap < 0 {
		return fmt.Errorf("%s.max_tokens_cap: must not be negative", name)
	}
	switch strings.ToLower(t.OnExceed) {
	case QuotaActionReject, QuotaActionDegrade:
		t.OnExceed = strings.ToLower(t.OnExceed)
	case "":
		t.OnExceed = def.OnExceed
		if t.OnExceed == "" {
			t.OnExceed = QuotaActionReject
		}
	default:
		return fmt.Errorf("%s.on_exceed: unsupported action %q (want reject or degrade)", name, t.OnExceed)
	}
	// The ladder a tenant degrades along inherits like every other field: a
	// tenant that says only "degrade" uses the default policy's rungs. The
	// effective values are written back so a consumer never has to merge the
	// two levels itself, and so MaxTokensCap == 0 has exactly one meaning
	// (no cap) once validation is done.
	if t.DowngradeModel == "" {
		t.DowngradeModel = def.DowngradeModel
	}
	if t.MaxTokensCap == 0 {
		t.MaxTokensCap = def.MaxTokensCap
	}
	if t.OnExceed == QuotaActionDegrade && t.DowngradeModel == "" && t.MaxTokensCap == 0 {
		return fmt.Errorf("%s: on_exceed is degrade but neither downgrade_model nor max_tokens_cap is set, so there is nothing to degrade to", name)
	}
	if t.AnomalyRatio < 0 {
		return fmt.Errorf("%s.anomaly_ratio: must not be negative", name)
	}
	if t.AnomalyRatio == 0 {
		t.AnomalyRatio = def.AnomalyRatio
	}
	if t.AnomalyRatio > 0 && t.AnomalyRatio < 1 {
		return fmt.Errorf("%s.anomaly_ratio: must be at least 1, got %v", name, t.AnomalyRatio)
	}
	return nil
}

// hasCatchAll reports whether the model list contains the "/" terminator that
// makes an upstream eligible for any model.
func hasCatchAll(models []string) bool {
	for _, m := range models {
		if strings.TrimSpace(m) == "/" {
			return true
		}
	}
	return false
}

// expandEnv resolves ${VAR} and $VAR inside every string field that can carry a
// secret. An unset variable is an error rather than an empty string: a backend
// that silently loses its credential produces 401s that look like upstream
// outages.
func expandEnv(c *Config) error {
	var missing []string
	expand := func(field, in string) string {
		out := os.Expand(in, func(key string) string {
			if v, ok := os.LookupEnv(key); ok {
				return v
			}
			missing = append(missing, fmt.Sprintf("%s -> $%s", field, key))
			return ""
		})
		return out
	}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		u.BaseURL = expand(fmt.Sprintf("upstreams[%d].base_url", i), u.BaseURL)
		u.APIKey = expand(fmt.Sprintf("upstreams[%d].api_key", i), u.APIKey)
	}
	// The cache carries secrets too: an embeddings API key, and a Redis password
	// in any deployment that is not a throwaway local instance.
	c.Cache.Embedding.APIKey = expand("cache.embedding.api_key", c.Cache.Embedding.APIKey)
	c.Cache.Redis.Password = expand("cache.redis.password", c.Cache.Redis.Password)
	c.Quota.Redis.Password = expand("quota.redis.password", c.Quota.Redis.Password)
	if len(missing) > 0 {
		return fmt.Errorf("undefined environment variable(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// Environment overrides that are useful in containers, where editing the file
// is awkward. They intentionally cover only scalars.
const (
	envListen       = "INFERGATE_LISTEN"
	envLogLevel     = "INFERGATE_LOG_LEVEL"
	envMaxBodyBytes = "INFERGATE_MAX_BODY_BYTES"
)

func applyEnvOverrides(c *Config) error {
	if v := strings.TrimSpace(os.Getenv(envListen)); v != "" {
		c.Server.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv(envLogLevel)); v != "" {
		c.Log.Level = v
	}
	if v := strings.TrimSpace(os.Getenv(envMaxBodyBytes)); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s must be a positive integer, got %q", envMaxBodyBytes, v)
		}
		c.Server.MaxBodyBytes = n
	}
	return nil
}
