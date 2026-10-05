package server

import (
	"crypto/subtle"
	"net/http"
	"sort"
	"strings"

	"github.com/infergate/infergate/internal/config"
)

// authRealm is the realm advertised in WWW-Authenticate. A realm is the name a
// client shows a human when it wants a token, so it names the service rather
// than the scheme.
const authRealm = "infergate"

// accessPolicy decides which inbound requests must carry an operator token.
//
// WHY THIS EXISTS AT ALL. Every operational surface on this gateway was
// unauthenticated through M6: /admin/* can flush caches, reset breakers, drain
// the replay store and the session ledger, and /stats exposes per-tenant cost
// and quota detail. On a loopback-only development box that is a convenience;
// the moment the listener is bound anywhere else it is an open door, and the
// README had to carry a paragraph warning about it. This turns that paragraph
// into a knob.
//
// WHAT IS DELIBERATELY NOT PROTECTED, and why the list is configurable rather
// than hard-coded:
//
//   - /healthz and /readyz are probes. A kubelet, a compose healthcheck or a
//     load balancer does not hold credentials, and requiring one turns a
//     working deployment into a permanently unhealthy one. Neither says
//     anything an attacker does not already know by connecting.
//   - /metrics is the Prometheus scrape target. The convention is an open
//     endpoint on a private network; anyone who wants it closed can list it in
//     access.protect.
//
// So the default protect list is exactly the set that can MUTATE state or
// expose tenant data: the /admin surface and /stats.
type accessPolicy struct {
	// enabled is false when no token is configured. The gateway then behaves
	// exactly as it did before this file existed -- which is what keeps every
	// config written for M0-M6 valid and every acceptance script green.
	enabled bool

	// protects is the normalised list of path prefixes behind the token.
	protects []string

	// header is the request header carrying the credential, "Authorization" by
	// default. Configurable because a deployment may sit behind something that
	// already claims that header (an OAuth proxy, an ingress) and needs the
	// operator token somewhere else.
	header string

	// bearer is true when the header value is compared as `Bearer <token>`
	// rather than verbatim. It only applies to the
	// Authorization header, because every other header has no scheme
	// convention to speak of.
	bearer bool

	// tokens are the accepted credentials. More than one so that a rotation is
	// a config edit rather than a hard cutover: add the new token, roll the
	// callers, remove the old one.
	tokens []string

	// allowQuery accepts the token as ?access_token= for the case a browser
	// address bar is the client (Grafana's datasource probe, a curl one-liner
	// in a runbook). Off by default: a token in a URL lands in access logs,
	// proxy logs and browser history.
	allowQuery bool
}

// newAccessPolicy normalises configuration into a decision procedure.
func newAccessPolicy(cfg config.AccessConfig) accessPolicy {
	// An empty configured list means "the default set", not "protect nothing".
	// Protecting nothing while a token is configured is never what an operator
	// means, and it is exactly the mistake that would look like it worked.
	protect := cfg.Protect
	if len(protect) == 0 {
		protect = config.DefaultAccessProtect()
	}
	protects := make([]string, 0, len(protect))
	for _, p := range protect {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			protects = append(protects, p)
		}
	}
	// Longest first, so a policy that names both /admin and /admin/cache
	// matches the more specific entry and stays readable in a log line.
	sort.Slice(protects, func(i, j int) bool { return len(protects[i]) > len(protects[j]) })

	header := strings.TrimSpace(cfg.Header)
	if header == "" {
		header = "Authorization"
	}

	tokens := make([]string, 0, len(cfg.Tokens))
	for _, tok := range cfg.Tokens {
		if tok = strings.TrimSpace(tok); tok != "" {
			tokens = append(tokens, tok)
		}
	}

	return accessPolicy{
		enabled:    cfg.Enabled && len(tokens) > 0,
		protects:   protects,
		header:     header,
		bearer:     strings.EqualFold(header, "Authorization"),
		tokens:     tokens,
		allowQuery: cfg.AllowQueryToken,
	}
}

// covers reports whether path is behind the token.
//
// Prefix semantics stop at a path separator: a policy naming /admin must cover
// /admin/cache but not a hypothetical /administrator, which a plain
// strings.HasPrefix would quietly include.
func (p accessPolicy) covers(path string) bool {
	for _, prefix := range p.protects {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// reject writes the "you did not present a usable token" answer.
//
// Both failure modes are 401 rather than distinguishing 403, because the
// distinction would tell a prober whether it guessed a real token and only got
// the header format wrong. The body deliberately does not echo what was
// received: the rejected value may be a valid token for a different service,
// and a response body is the easiest thing in the system to end up in a log.
func (p accessPolicy) reject(w http.ResponseWriter) {
	if p.bearer {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+authRealm+`"`)
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"error": map[string]any{
			"message": "this endpoint requires an operator token",
			"type":    "infergate_unauthorized",
		},
	})
}

// authorized compares a presented credential against every accepted token.
//
// subtle.ConstantTimeCompare rather than ==: Go's string comparison returns as
// soon as it finds a differing byte, which leaks the length of the matching
// prefix and, over enough requests, enough of the token to reconstruct it.
// Comparing every token even after a match keeps the work independent of which
// entry matched.
func (p accessPolicy) authorized(presented string) bool {
	match := 0
	for _, want := range p.tokens {
		match |= subtle.ConstantTimeCompare([]byte(presented), []byte(want))
	}
	return match == 1
}

// credential extracts the presented token from the request.
//
// The scheme prefix is STRIPPED ONLY FOR THE HEADER. A query parameter carries
// the token itself -- ?access_token=abc, not ?access_token=Bearer%20abc -- and
// applying the header's grammar to it would reject every well-formed query
// token while looking correct.
//
// Everything except that one separator space is compared verbatim and WITHOUT
// trimming: the space is part of the bearer grammar, any other leading or
// trailing whitespace is part of what the caller sent, and a configured token
// never contains it. Accepting " token " would make the accepted set wider than
// the configured set, which is the wrong direction to be generous in.
func (p accessPolicy) credential(r *http.Request) string {
	raw, fromHeader := r.Header.Get(p.header), true
	if raw == "" && p.allowQuery {
		raw, fromHeader = r.URL.Query().Get("access_token"), false
	}
	if raw == "" {
		return ""
	}
	if !p.bearer || !fromHeader {
		return raw
	}
	// Scheme match is case-insensitive per RFC 7235.
	const prefix = "bearer "
	if len(raw) < len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return ""
	}
	return raw[len(prefix):]
}

// middleware wraps next with the token check.
//
// It runs AFTER the body-size check (it is applied inside withAccessControl) and
// before any handler: an unauthenticated request to a mutating endpoint should
// not reach the handler at all, and should not pay for a body read either.
func (p accessPolicy) middleware(next http.Handler) http.Handler {
	if !p.enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Preflight carries no credentials by specification: a browser omits
		// Authorization on an OPTIONS preflight, so rejecting one would make
		// every protected endpoint unusable from a browser client (the Grafana
		// datasource test being the concrete case). The preflight itself
		// executes no handler logic.
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if !p.covers(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		cred := p.credential(r)
		if !p.authorized(cred) {
			p.reject(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}
