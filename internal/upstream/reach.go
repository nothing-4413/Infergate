package upstream

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/infergate/infergate/internal/config"
)

// ReachResult is the outcome of one startup reachability probe.
//
// It deliberately records the address that was dialled, not just the verdict:
// when a WARN says "unreachable", the operator needs to know whether the
// gateway tried 127.0.0.1:11434 (a local Ollama that was never started) or
// api.deepseek.com:443 (a network/DNS problem), because the two have nothing
// in common as remedies.
type ReachResult struct {
	// Name is the configured upstream name, used as a log field.
	Name string

	// BaseURL is the configured root, verbatim.
	BaseURL string

	// Addr is the host:port the probe dialled. It is derived from BaseURL
	// (defaulting to 80/443 when no port is written) so that the same
	// http.Transport defaults apply at request time.
	Addr string

	// Err is nil when a TCP connection was accepted. Any other error means the
	// backend cannot currently accept traffic, so every request routed to it
	// will fail with a 502 until it comes back.
	Err error
}

// Reachable reports whether the backend accepted a TCP connection.
func (r ReachResult) Reachable() bool { return r.Err == nil }

// reachTimeout bounds one probe; the whole call bounds the set.
//
// The purpose is a startup hint, not a health check: a backend that answers in
// under a second is up, and one that does not will still be tried at request
// time (with the request's own, longer deadline). A gateway must never refuse
// to boot because a provider is slow to complete a handshake.
const reachTimeout = 1500 * time.Millisecond

// ProbeReach dials every configured backend once and reports what answered.
//
// WHY A TCP DIAL AND NOT AN HTTP REQUEST. A probe that sent a real request would
// need a credential (and would spend tokens on a commercial provider), would
// show up as traffic in the provider's dashboards, and would fail on backends
// whose only reachable surface is a path the gateway does not know. A TCP
// connect answers the only question boot actually needs: is anything there to
// accept a request?
//
// Probes run concurrently and results keep configuration order, so a config
// with several dead backends still costs one timeout, not one per backend.
//
// The dial is direct: HTTP(S)_PROXY is not consulted. A deployment that reaches
// its providers only through a proxy will therefore see a false "unreachable"
// warning here. That is a startup diagnostic, not a gate, and the request path
// (http.Transport, which does consult the environment) is unaffected.
func ProbeReach(ctx context.Context, upstreams []config.UpstreamConfig) []ReachResult {
	results := make([]ReachResult, len(upstreams))
	var wg sync.WaitGroup
	for i := range upstreams {
		results[i].Name = upstreams[i].Name
		results[i].BaseURL = upstreams[i].BaseURL
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i].Addr, results[i].Err = dialBaseURL(ctx, upstreams[i].BaseURL)
		}(i)
	}
	wg.Wait()
	return results
}

// dialBaseURL resolves one configured base_url to a host:port and dials it.
func dialBaseURL(ctx context.Context, baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("base_url %q is not a URL: %w", baseURL, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("base_url %q has no host", baseURL)
	}
	addr := u.Host
	if u.Port() == "" {
		port := ""
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return addr, fmt.Errorf("base_url %q has scheme %q, which has no default port", baseURL, u.Scheme)
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}

	dialCtx, cancel := context.WithTimeout(ctx, reachTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return addr, err
	}
	_ = conn.Close()
	return addr, nil
}
