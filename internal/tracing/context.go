// Package tracing is the M5 observability core: W3C Trace Context propagation
// plus an in-memory span model and a bounded trace store.
//
// # Why not OpenTelemetry
//
// InferGate has no third-party dependencies, and the Go OpenTelemetry SDK would
// drag in a metric/export dependency tree larger than the gateway itself. What
// M5 actually needs is narrow: a trace id that survives the hop from client to
// gateway to upstream, a span tree that explains where a request spent its
// time, and the ability to replay ONE request by id after the fact.
//
// So this package implements the honest minimum:
//
//   - Propagation is the real W3C Trace Context, version 00 (RFC-adjacent spec
//     "Trace Context", https://www.w3.org/TR/trace-context/). Whether the trace
//     was started by OpenTelemetry, by another gateway or by this process, a
//     valid `traceparent` header is understood.
//
//   - The span and trace shapes are OTLP-LIKE on purpose: snake_case json
//     field names (trace_id, span_id, parent_span_id, time_unix_nano, status),
//     an integer-nanosecond time base, and an Events list that maps onto OTLP
//     span events. A collector mapping is therefore mechanical (rename
//     Status/Kind to enums, wrap in resourceSpans) but the JSON here is NOT
//     OTLP and does not claim to be: there is no resource/scope layer, no
//     proto encoding, and `kind`/`status` are plain lowercase strings rather
//     than integer enums.
//
//   - Storage is a bounded ring. Traces exist to answer "what happened to
//     request X" while it is still interesting, not to be a database; the
//     store drops the oldest trace at capacity and counts the drops so an
//     operator can see that a trace was pushed out before blaming the
//     instrumentation for the gap.
//
// # What this package deliberately does not do
//
// There is no sampling exporter, no background flush, no parent-based sampling
// ratio, and no clock injection. Every one of those is a policy decision that
// belongs to the caller (internal/server, internal/gateway); this package only
// records what it is told, at the time it is told.
//
// The zero value of Trace is usable. Store must be built with NewStore.
package tracing

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// The W3C Trace Context headers. HeaderTraceparent is the propagation carrier;
// HeaderTracestate carries vendor-specific state and is opaque to this package.
//
// Go's http.Header canonicalizes these on Set/Get (Traceparent / Tracestate),
// so wire them through net/http rather than a raw map if you care about the
// exact casing on egress.
const (
	// HeaderTraceparent carries "00-<trace-id>-<span-id>-<flags>".
	HeaderTraceparent = "traceparent"

	// HeaderTracestate carries vendor state. InferGate neither parses nor
	// validates it: a gateway that rewrites another vendor's state corrupts it.
	// It is forwarded byte for byte because the W3C spec says a trace that
	// passes through a system must keep the state of systems it already
	// visited, and only that system may edit its own entry.
	HeaderTracestate = "tracestate"
)

// Context is the propagation state parsed from (or destined for) a traceparent
// header.
//
// SpanID is the CALLER's span, i.e. OUR PARENT, not our own span. That is the
// W3C convention and it is a standing source of off-by-one confusion: when the
// gateway opens its root server span it must record this SpanID as that span's
// ParentSpanID, and then send its own newly minted span id downstream. Sending
// the received SpanID onward unchanged makes every hop in the chain claim the
// same span id and turns the trace into a flat list of siblings.
type Context struct {
	TraceID    string // 32 lowercase hex chars
	SpanID     string // 16 lowercase hex chars = the CALLER's span, i.e. OUR parent
	Sampled    bool
	TraceState string
	Remote     bool // true when it came from an inbound header
}

// Zero Context semantics: Parse returns the zero Context with ok == false.
// Never a partially filled one -- a half-parsed Context handed to a span
// builder produces a span with an id that matches nothing.
const (
	traceIDLen = 32
	spanIDLen  = 16

	// version00 is the only traceparent version this parser accepts. A future
	// version may change the field grammar (the spec reserves the right to add
	// fields after the flags octet), so silently accepting an unknown version
	// would mean forwarding fields we did not understand under a format we
	// claimed to implement.
	version00 = "00"

	// sampledFlag is the low bit of the flags octet: bit 0 == sampled.
	sampledFlag = 0x01
)

// Parse validates a traceparent header and extracts the trace context.
//
// It mirrors the W3C version-00 grammar exactly and is strict on purpose,
// because a lenient parser turns a corrupt header into a plausible-looking
// trace that points nowhere. Rejected (ok == false):
//
//   - empty or whitespace-padded input, or any segment count other than 4;
//   - a version other than "00";
//   - a trace id that is not exactly 32 lowercase hex chars;
//   - a span id that is not exactly 16 lowercase hex chars;
//   - a flags octet that is not exactly 2 hex chars;
//   - UPPERCASE hex (the grammar is lowercase-only, and accepting both would
//     let one trace be addressed by two different ids in the store);
//   - the all-zero trace id or span id ("unspecified" per the spec);
//   - the all-ff trace id or span id ("invalid" per the spec).
//
// On failure the returned Context is the zero value and ok is false. Parse
// never panics and never returns a partial Context.
//
// tracestate is not validated -- it is opaque vendor state with its own spec
// (and its own list members and entropy rules). It is carried through verbatim,
// including when it is empty, whenever the traceparent is valid.
func Parse(traceparent, tracestate string) (Context, bool) {
	if traceparent == "" || traceparent != strings.TrimSpace(traceparent) {
		return Context{}, false
	}

	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 {
		return Context{}, false
	}
	version, traceID, spanID, flags := parts[0], parts[1], parts[2], parts[3]

	if version != version00 {
		return Context{}, false
	}
	if !isID(traceID, traceIDLen) || isAllZeroOrAllFF(traceID) {
		return Context{}, false
	}
	if !isID(spanID, spanIDLen) || isAllZeroOrAllFF(spanID) {
		return Context{}, false
	}
	if len(flags) != 2 {
		return Context{}, false
	}
	fb, err := hex.DecodeString(flags)
	if err != nil || !isLowerHex(flags) {
		return Context{}, false
	}

	return Context{
		TraceID:    traceID,
		SpanID:     spanID,
		Sampled:    fb[0]&sampledFlag != 0,
		TraceState: tracestate,
		Remote:     true,
	}, true
}

// Header renders the context as a version-00 traceparent header.
//
// Header is the inverse of Parse for every Context this package produces:
// Parse(c.Header(), c.TraceState) round-trips TraceID, SpanID, Sampled and
// TraceState. Contexts built by hand with ids that Parse would reject (wrong
// length, uppercase, all-zero) are rendered as-is rather than silently
// repaired -- this method does not validate, because the only caller that
// matters is the egress path, where refusing to send a header is a worse
// failure than sending a malformed one that the next hop will reject.
//
// The flags octet is emitted as "01" when sampled and "00" otherwise. The W3C
// spec allows other bits in the octet; no producer here sets them, and
// re-encoding an unknown flag we did not preserve would be a lie, so they are
// dropped by construction.
func (c Context) Header() string {
	flags := "00"
	if c.Sampled {
		flags = "01"
	}
	return version00 + "-" + c.TraceID + "-" + c.SpanID + "-" + flags
}

// NewRootContext starts a fresh trace: new random ids, Sampled true, Remote
// false. It is what a gateway calls when an inbound request carries no valid
// traceparent.
//
// Sampled defaults to TRUE rather than false. A gateway that defaults to
// unsampled produces traces only when the caller opted in, which in practice
// means "no traces", because the callers that would opt in are exactly the
// ones already running a tracer. M5 wants traces for ordinary traffic, so the
// default is on and dropping that decision up is the caller's job.
func NewRootContext() Context {
	return Context{
		TraceID: NewTraceID(),
		SpanID:  NewSpanID(),
		Sampled: true,
		Remote:  false,
	}
}

// NewTraceID returns 16 random bytes as 32 lowercase hex chars.
//
// Randomness, not a counter or a timestamp: a trace id is handed to clients and
// appears in logs and headers, so a guessable one lets a caller read (or forge)
// another tenant's trace. crypto/rand is the only acceptable source.
func NewTraceID() string { return randomHex(traceIDLen / 2) }

// NewSpanID returns 8 random bytes as 16 lowercase hex chars. Same reasoning as
// NewTraceID: span ids are observable and must not be predictable.
func NewSpanID() string { return randomHex(spanIDLen / 2) }

// randomHex returns n random bytes as 2n lowercase hex chars.
//
// crypto/rand.Read is documented never to fail on the platforms Go supports,
// but the signature forces a decision, and the choices are a panic (on the
// request path, for an id) or degraded uniqueness. Degrade: mix the time, a
// process-global counter and a stack address into a hash. That is not
// cryptographically random, but it keeps ids unique-in-practice and keeps the
// gateway serving, which is the right trade for a correlation id. There is no
// silent correctness bug here because uniqueness -- not unpredictability --
// is what the store and the parser require.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	return fallbackHex(n)
}

// isID reports whether s is exactly n LOWERCASE hex characters.
//
// The lowercase check is not pedantry: W3C defines ids as lowercase hex, and if
// both cases were accepted the same trace could be addressed by two strings,
// which breaks Store.Get lookups and makes two log lines for one request look
// like two requests.
func isID(s string, n int) bool {
	return len(s) == n && isLowerHex(s)
}

// isLowerHex reports whether every byte of s is 0-9 or a-f.
//
// Hand-rolled rather than hex.DecodeString because DecodeString also accepts
// uppercase, and because this runs on every inbound request.
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// isAllZeroOrAllFF rejects both reserved id values: all-zero means
// "unspecified", all-ff means "invalid" in the version-00 grammar.
//
// These are rejected rather than rewritten to fresh ids because silently
// substituting an id makes a broken producer look like a working one, and the
// resulting trace would be filed under an id the client never sent.
func isAllZeroOrAllFF(s string) bool {
	zeros, ones := true, true
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			zeros = false
		}
		if s[i] != 'f' {
			ones = false
		}
		if !zeros && !ones {
			return false
		}
	}
	return true
}
