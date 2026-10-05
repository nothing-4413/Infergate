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
	Log       LogConfig        `json:"log"`
	Pricing   PricingConfig    `json:"pricing"`
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

// Supported upstream dialects.
const (
	KindOpenAI = "openai"
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
		Pricing: PricingConfig{
			Default: ModelPrice{In: 0, Out: 0},
			Models:  map[string]ModelPrice{},
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
