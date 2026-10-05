// Package upstream models the OpenAI-compatible backends InferGate talks to.
//
// M0 resolves exactly one backend per request (an explicit X-InferGate-Upstream
// header, or the model-to-backend map). The value of the abstraction is that
// the resolution step is already isolated behind an interface, so M1 can put a
// cost/latency/health policy behind the same call without touching the proxy.
package upstream

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/infergate/infergate/internal/config"
)

// Target is one resolved backend, ready to receive a request.
type Target struct {
	// Name is the backend name from configuration.
	Name string

	// BaseURL is the backend root without a trailing slash.
	BaseURL string

	// APIKey is the bearer credential, or empty for a keyless local backend.
	APIKey string

	// Kind is the provider dialect; only config.KindOpenAI exists in M0.
	Kind string

	// Transport is this target's HTTP transport. Transports are per target
	// rather than global because connection-pool sizing and TLS settings are
	// per backend: a local vLLM wants many idle connections, a remote provider
	// wants few and a longer handshake budget.
	Transport http.RoundTripper

	// catchAll records whether this target declared the "/" catch-all in its
	// model list. It is bookkeeping for diagnostics; resolution uses the
	// registry indexes, never this flag, so that a request costs one map lookup
	// rather than a scan.
	catchAll bool

	// patterns is the configured model list, retained for introspection.
	patterns []string
}

// IsCatchAll reports whether the backend accepts any model name.
func (t *Target) IsCatchAll() bool { return t.catchAll }

// ModelPatterns lists the model names this backend can serve, "/" included.
func (t *Target) ModelPatterns() []string { return t.patterns }

// Registry resolves inbound requests to backends.
type Registry struct {
	mu       sync.RWMutex
	targets  []*Target
	byName   map[string]*Target
	byModel  map[string]*Target
	catchAll []*Target
}

// ErrNoTarget is returned when no backend can serve a model.
var ErrNoTarget = errors.New("no upstream is configured for this model")

// New builds a Registry from validated configuration.
//
// Transport construction happens once, here, instead of per request. Creating
// an http.Transport per request is the single most common reverse-proxy
// performance bug: it defeats connection pooling, leaks idle connections and
// burns a file descriptor per in-flight request.
func New(cfg *config.Config) (*Registry, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	r := &Registry{
		byName:  make(map[string]*Target, len(cfg.Upstreams)),
		byModel: make(map[string]*Target),
	}

	for i := range cfg.Upstreams {
		uc := cfg.Upstreams[i]
		// Normalise the base URL once, here. The proxy builds the outbound URL
		// as BaseURL + r.URL.Path, so a configured trailing slash would produce
		// "//v1/chat/completions" and every request would 404 at the provider.
		// Fixing it at construction means no per-request string surgery.
		baseURL := strings.TrimRight(strings.TrimSpace(uc.BaseURL), "/")
		t := &Target{
			Name:      uc.Name,
			BaseURL:   baseURL,
			APIKey:    uc.APIKey,
			Kind:      uc.Kind,
			Transport: newTransport(cfg.Server),
		}
		r.targets = append(r.targets, t)
		r.byName[t.Name] = t

		for _, m := range uc.Models {
			m = strings.TrimSpace(m)
			switch {
			case m == "/":
				t.catchAll = true
				t.patterns = append(t.patterns, m)
				r.catchAll = append(r.catchAll, t)
			case m != "":
				t.patterns = append(t.patterns, m)
				// First writer wins: an earlier backend that explicitly claims
				// a model keeps it, so configuration order is the tie-breaker
				// and adding a catch-all backend later cannot silently steal
				// traffic.
				key := strings.ToLower(m)
				if _, exists := r.byModel[key]; !exists {
					r.byModel[key] = t
				}
			}
		}
	}
	return r, nil
}

// newTransport builds an HTTP transport tuned for streaming LLM traffic.
func newTransport(sc config.ServerConfig) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			// A 10s dial budget is long for a datacentre and short for the
			// public internet; it is a deliberate middle ground that keeps a
			// dead provider from consuming a client's whole timeout.
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,

		// Pool sizing. MaxIdleConnsPerHost is the setting that matters under
		// concurrency: Go's default of 2 forces a new TCP (and TLS) handshake
		// for most parallel requests, which for an LLM gateway shows up
		// directly in time-to-first-token.
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: sc.MaxIdleConnsPerHost,
		MaxConnsPerHost:     0, // unlimited in flight, bounded by idle pool
		IdleConnTimeout:     90 * time.Second,

		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     sc.DisableKeepAlives,

		// ResponseHeaderTimeout is intentionally unset: a long generation can
		// legitimately take minutes to produce its first byte, and the real
		// deadline is owned by the handler via context.
	}
}

// Resolve returns the target that should serve model.
//
// Resolution order, highest priority first:
//
//  1. explicit — the caller named a backend (X-InferGate-Upstream). Operators
//     and tests use this to pin traffic, and M1 uses it for shadow traffic.
//  2. exact model match from configuration.
//  3. first configured catch-all.
//  4. the only configured backend, when there is exactly one.
//
// Rule 4 is the single-backend ergonomic case: a local vLLM or Ollama box
// serves one quantisation and the caller (an agent framework) is told a model
// alias that the gateway has never heard of. Rejecting that request would make
// the gateway useless for the most common local deployment, so an unambiguous
// single backend absorbs any model name and the proxy rewrites it to the one
// model that backend actually serves.
//
// It is deliberately the LAST rule. With two or more backends there is no
// unambiguous answer, and guessing would silently send an agent's request to
// the wrong model — a correctness failure that no metric would reveal.
//
// Model matching is case-insensitive because providers are inconsistent about
// it and a case difference silently routing to a different (or no) backend is a
// far worse failure than a false match.
func (r *Registry) Resolve(model, explicit string) (*Target, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if explicit != "" {
		if t, ok := r.byName[explicit]; ok {
			return t, nil
		}
		return nil, fmt.Errorf("%w: unknown upstream %q", ErrNoTarget, explicit)
	}
	if t, ok := r.byModel[strings.ToLower(strings.TrimSpace(model))]; ok {
		return t, nil
	}
	if len(r.catchAll) > 0 {
		return r.catchAll[0], nil
	}
	if len(r.targets) == 1 {
		return r.targets[0], nil
	}
	return nil, fmt.Errorf("%w: model %q", ErrNoTarget, model)
}

// Target returns a backend by name.
func (r *Registry) Target(name string) (*Target, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// Names returns every configured backend name in configuration order, which is
// also the ordering guarantee that makes model resolution deterministic.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.targets))
	for _, t := range r.targets {
		out = append(out, t.Name)
	}
	return out
}

// ModelIndex returns the resolved model-to-backend map, for the /admin/upstreams
// introspection endpoint. The value is the backend name.
func (r *Registry) ModelIndex() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.byModel))
	keys := make([]string, 0, len(r.byModel))
	for k := range r.byModel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = r.byModel[k].Name
	}
	return out
}

// CloseIdleConnections releases pooled connections on every backend. The server
// calls it during shutdown so that the process exits promptly instead of
// waiting on idle keep-alive sockets.
func (r *Registry) CloseIdleConnections() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.targets {
		if tr, ok := t.Transport.(interface{ CloseIdleConnections() }); ok {
			tr.CloseIdleConnections()
		}
	}
}
