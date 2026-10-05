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

	// HeaderRoute reasons about routing decisions in M1.
	HeaderRoute = "X-InferGate-Route"
)

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
