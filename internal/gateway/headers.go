package gateway

import (
	"net/http"
	"net/textproto"
	"strings"
)

// hopByHopHeaders are removed from both the inbound request and the upstream
// response, per RFC 7230 section 6.1.
//
// Forwarding them is not a style question: Connection carries a list of header
// names that the sending hop promised to consume, so blindly copying it lets a
// client mark an arbitrary header (including Authorization) as hop-by-hop and
// have the gateway strip or duplicate it at the next hop.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Gateway control headers. These are consumed by the gateway and must never
// reach a provider, both to keep provider APIs clean and to prevent a client
// from selecting a backend on a request that is later replayed elsewhere.
const (
	// HeaderUpstream pins the request to a named backend.
	HeaderUpstream = "X-InferGate-Upstream"

	// HeaderRequestID carries a caller-supplied correlation id, echoed back.
	HeaderRequestID = "X-InferGate-Request-Id"

	// HeaderUpstreamName reports which backend served the response.
	HeaderUpstreamName = "X-InferGate-Upstream-Name"

	// HeaderAttempt reports how many upstream attempts were made (M1 retries).
	HeaderAttempt = "X-InferGate-Attempt"

	// HeaderCapabilities declares the feature tags the request requires, as a
	// comma-separated list ("tools", "json", "vision"). The router only offers
	// backends that declared all of them.
	//
	// It is a REQUEST header rather than something inferred from the body,
	// because the same model served by two backends can differ in what it
	// supports, and only the caller knows whether the downstream agent actually
	// needs tool calls. Inferring "tools" from a non-empty tools array is the
	// obvious guess and the wrong one: an agent framework sends the same tool
	// schema whether or not the model will use it.
	HeaderCapabilities = "X-InferGate-Capabilities"

	// HeaderTried lists the backends attempted for this request, in order, on
	// the response. It is response-only: a client that sees two names knows a
	// failover happened without reading the gateway's logs.
	HeaderTried = "X-InferGate-Tried"

	// HeaderTenant names the isolation namespace a request belongs to, and is
	// what keeps one caller's cached answers away from another's. When it is
	// absent the gateway derives a namespace from the credential (hashed, never
	// stored) or falls back to "anonymous".
	HeaderTenant = "X-InferGate-Tenant"

	// HeaderCache is a request directive and a response report, deliberately
	// one header in both directions so that "what did I ask for" and "what did
	// I get" are the same string in a log or a curl transcript.
	//
	// Request values: "bypass" (do not serve from the cache) and "refresh" (do
	// not serve the stored answer, but replace it). Both still STORE the fresh
	// answer unless the request is uncacheable - a bypass that also declined to
	// store would make "force a refresh" a way to permanently disable caching
	// for a scope, one request at a time.
	//
	// Response values: "hit-exact", "hit-semantic", "miss", "skip" (the request
	// itself cannot be cached) and "bypass"/"refresh" echoed back when a
	// directive was honoured.
	HeaderCache = "X-InferGate-Cache"

	// HeaderSession names one logical conversation inside a tenant, and is what
	// a per-session token budget is charged against. It is a REQUEST header
	// only: a client that controls its own session id can start a new budget,
	// which is why the per-session dimension is a fairness control between
	// cooperating callers and never a security boundary. The daily and
	// per-minute dimensions - which no header can reset - are what bound a
	// caller who lies about this one.
	HeaderSession = "X-InferGate-Session"

	// HeaderQuota reports the admission verdict: "allow", "degrade" or
	// "reject". Like HeaderCache it is deliberately meaningful in one
	// direction, and it is sent on EVERY governed request rather than only on
	// a refusal, because a limit whose remaining budget is invisible cannot be
	// planned against.
	HeaderQuota = "X-InferGate-Quota"

	// HeaderQuotaReason names the dimension that decided the verdict
	// ("within-budget", "tokens_per_day", "requests_per_minute", ...).
	HeaderQuotaReason = "X-InferGate-Quota-Reason"

	// HeaderQuotaLimit and HeaderQuotaUsed are the limit that applies and what
	// the tenant had already spent in that window. They are omitted when no
	// single dimension was named, so a caller never reads a zero as a limit.
	HeaderQuotaLimit = "X-InferGate-Quota-Limit"
	HeaderQuotaUsed  = "X-InferGate-Quota-Used"

	// HeaderQuotaModel and HeaderQuotaMaxTokens report what a degraded request
	// was rewritten to, so the caller can see that the answer came from a
	// smaller model or a shortened completion instead of wondering why quality
	// changed.
	HeaderQuotaModel     = "X-InferGate-Quota-Model"
	HeaderQuotaMaxTokens = "X-InferGate-Quota-Max-Tokens"

	// HeaderCacheAge reports the age of a served cached entry in whole
	// milliseconds. An answer with an age is a different thing from an answer,
	// which is why the number is always sent with a hit rather than left for
	// the caller to infer from the body.
	HeaderCacheAge = "X-InferGate-Cache-Age"
)

// Values of HeaderCache in the request direction.
const (
	cacheDirectiveBypass  = "bypass"
	cacheDirectiveRefresh = "refresh"
)

// Values of HeaderCache in the response direction.
const (
	cacheStatusHitExact    = "hit-exact"
	cacheStatusHitSemantic = "hit-semantic"
	cacheStatusMiss        = "miss"
	cacheStatusSkip        = "skip"
	cacheStatusDisabled    = "disabled"
	cacheStatusError       = "error"
)

// cacheStatusUpstream labels a request served from the cache in the per-request
// metrics and logs. The upstream that ORIGINALLY produced the answer is not the
// upstream that served this request, and attributing a cache hit to it would
// make the per-upstream totals claim traffic that never arrived.
const cacheStatusUpstream = "cache"

// removeHopByHop deletes hop-by-hop headers from h, including any header the
// Connection field itself lists.
func removeHopByHop(h http.Header) {
	for _, name := range connectionListed(h) {
		h.Del(name)
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// connectionListed returns the header names named by the Connection field.
//
// This is the subtle half of RFC 7230 section 6.1: Connection is not merely
// itself hop-by-hop, it declares OTHER headers hop-by-hop for this message. A
// gateway that strips only the static list lets a client send
// "Connection: Authorization" and have an arbitrary header handled
// inconsistently at the next hop.
func connectionListed(h http.Header) []string {
	var names []string
	for _, conn := range h.Values("Connection") {
		for _, name := range strings.Split(conn, ",") {
			if name = textproto.TrimString(name); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// copyHeaders copies the headers that survive to the next hop: everything except
// the static hop-by-hop set, anything the Connection field names, and (when
// dropGatewayHeaders is set) InferGate's own control headers. Values are
// preserved as-is so that a provider's response reaches the client
// byte-faithfully (rate-limit headers, request ids, retry hints, vendor
// extensions).
func copyHeaders(dst, src http.Header, dropGatewayHeaders bool) {
	skip := make(map[string]struct{})
	for _, name := range connectionListed(src) {
		skip[http.CanonicalHeaderKey(name)] = struct{}{}
	}
	for name, values := range src {
		if isHopByHop(name) {
			continue
		}
		if _, listed := skip[name]; listed {
			continue
		}
		if dropGatewayHeaders && isGatewayHeader(name) {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
}

func isHopByHop(name string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

func isGatewayHeader(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "x-infergate-")
}
