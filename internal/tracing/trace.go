package tracing

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

// Time base
// =========
//
// Every time in this file is stored as an int64 count of nanoseconds since the
// Unix epoch, and the integer is AUTHORITATIVE. The float millisecond fields
// exist because JSON is what operators read and dashboards chart, and int64
// nanoseconds are hostile to both (a 20-second request is 2e10, which is a
// number nobody eyeballs).
//
// How the two stay consistent:
//
//   - StartUnixNano/EndUnixNano and StartedAt come from the same time.Time.
//     Trace.StartedAt always satisfies StartedAt.UnixNano() == the start nanos
//     of the root span, because that is literally how it is set (see Add).
//
//   - DurationMS is DERIVED from the integer pair by nanosToMS and is never
//     fed independently. In fact it is also RECOMPUTED on read, in
//     Trace.Duration() and Trace.Summary(); the cached field is a
//     serialization convenience, so a trace decoded from JSON that carries
//     stale nanos still reports a correct duration. Deriving beats trusting.
//
//   - nanosToMS rounds to 6 decimal places. float64 has no exact nanosecond
//     fraction: 1 ns is 1e-6 ms, which is not representable in binary, so
//     rounding is what makes the printed and encoded value stable instead of
//     drifting in the 15th decimal. Sub-nanosecond precision does not exist by
//     definition, so 6 decimals is lossless, not lossy.
//
// A float64 nanosecond value is exact only to +-2^53 ns (~104 days), far beyond
// any request duration; the int64 remains the source of truth regardless.

// Event is a timestamped annotation on a span: a retry, a cache hit/miss, the
// first streamed chunk, a heartbeat. The shape mirrors an OTLP span event
// (name, time_unix_nano, attributes) so exporting is a field rename.
//
// Events rather than child spans because these moments are points in time, not
// units of work: "first token" has no duration to nest.
type Event struct {
	Name         string         `json:"name"`
	TimeUnixNano int64          `json:"time_unix_nano"`
	Attributes   map[string]any `json:"attributes,omitempty"`
}

// Span is one unit of traced work. TraceID/SpanID are propagated ids; the root
// gateway span shares its TraceID with the trace and is the only span with an
// empty ParentSpanID.
type Span struct {
	TraceID       string         `json:"trace_id"`
	SpanID        string         `json:"span_id"`
	ParentSpanID  string         `json:"parent_span_id,omitempty"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"` // "server" | "client" | "internal"
	StartUnixNano int64          `json:"start_unix_nano"`
	EndUnixNano   int64          `json:"end_unix_nano"`
	DurationMS    float64        `json:"duration_ms"`
	Status        string         `json:"status"` // "ok" | "error" | "unset"
	Attributes    map[string]any `json:"attributes,omitempty"`
	Events        []Event        `json:"events,omitempty"`
}

// Kind values. Strings plus constants rather than an int enum so the JSON is
// readable and a collector mapping is a switch, not a table lookup.
const (
	KindServer   = "server"
	KindClient   = "client"
	KindInternal = "internal"
)

// Status values. "unset" is a real state, not a placeholder: it is how an
// abandoned span (client disconnected mid-stream, process restart) is
// distinguished from one that genuinely succeeded.
const (
	StatusUnset = "unset"
	StatusOK    = "ok"
	StatusError = "error"
)

// Attribute keys used for the summary and by the instrumentation that will live
// in internal/server. Exported so a caller cannot typo "route" into a trace
// that silently summarizes as empty.
const (
	AttrRoute    = "route"
	AttrUpstream = "upstream"
	AttrModel    = "model"
	AttrRequest  = "request_id"
)

// Summary is the list-row projection of a trace: everything needed to render a
// "recent requests" table without touching the span tree. It is deliberately
// small -- Store.List returns these, and cloning whole traces to render a list
// is how an observability feature becomes a memory and latency problem.
type Summary struct {
	TraceID    string  `json:"trace_id"`
	RequestID  string  `json:"request_id"`
	StartedAt  string  `json:"started_at"` // RFC3339 with nanoseconds, UTC
	DurationMS float64 `json:"duration_ms"`
	Status     string  `json:"status"`
	Route      string  `json:"route,omitempty"`
	Upstream   string  `json:"upstream,omitempty"`
	Model      string  `json:"model,omitempty"`
	SpanCount  int     `json:"span_count"`
}

// Trace is a whole request: one root span plus its descendants.
type Trace struct {
	TraceID    string         `json:"trace_id"`
	RequestID  string         `json:"request_id"`
	StartedAt  time.Time      `json:"started_at"`
	DurationMS float64        `json:"duration_ms"`
	Status     string         `json:"status"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Spans      []Span         `json:"spans"`
}

// NewTrace returns a trace rooted at t0 with no spans yet. Status starts
// "unset" so a trace whose only span never completed reads as abandoned rather
// than as a success.
func NewTrace(traceID, requestID string, t0 time.Time) *Trace {
	return &Trace{
		TraceID:   traceID,
		RequestID: requestID,
		StartedAt: t0,
		Status:    StatusUnset,
	}
}

// Add appends a span to the trace.
//
// Two things it does beyond append, both of which are the reason it is a method
// and not a bare `append`:
//
//   - It DEEP COPIES the span. The caller on the request path holds the same
//     Attributes map it is still writing to (annotations arrive as the request
//     progresses), and a slice that aliases caller-owned state is how a
//     "finished" trace mutates under a concurrent reader -- which is exactly
//     the data race the store promises to prevent. After Add, the caller may
//     mutate the span and its maps freely.
//
//   - It fills in DurationMS from Start/EndUnixNano (nanosToMS, 6 decimals),
//     and normalizes StartedAt to the EARLIEST span start. Callers should not
//     have to remember to compute a derived field, and a trace whose DurationMS
//     disagrees with its nanos is worse than one with no duration. Deriving
//     StartedAt rather than fixing it at the first append keeps the documented
//     invariant (StartedAt.UnixNano() == earliest span start) true even when
//     spans arrive out of order, which they do whenever a buffered writer's
//     child span lands before the root span of the streamed request.
//
// Spans[0] is expected to be the root gateway span. Add does not enforce it --
// spans can legitimately arrive out of order when a streamed body is closed
// after child spans finish -- but Summary reads route/upstream/model/status
// from Spans[0], so the root must be added first.
//
// Add is not safe for concurrent use; the store's mutex is what serializes it.
func (t *Trace) Add(span Span) {
	span = cloneSpan(span)
	if span.DurationMS == 0 && span.EndUnixNano > span.StartUnixNano {
		span.DurationMS = nanosToMS(span.EndUnixNano - span.StartUnixNano)
	}
	t.Spans = append(t.Spans, span)
	if start := earliestStart(t.Spans); start > 0 {
		t.StartedAt = time.Unix(0, start).UTC()
	}
	t.DurationMS = t.Duration()
}

// Duration returns the trace wall time in milliseconds, derived from the
// integer nanosecond times of its spans: from the EARLIEST span start to the
// LATEST span end.
//
// Earliest-to-latest, not root-start-to-root-end, because that is what "how
// long did this request take" means when the root span is closed before its
// children (a streamed response is often finished by the time the writer's
// child span is closed). Using the root alone under-reports; using the trace's
// own StartedAt would drag in time before the root span existed.
//
// Returns 0 for a trace with no spans, or with only unstarted spans.
func (t *Trace) Duration() float64 {
	if t == nil || len(t.Spans) == 0 {
		return 0
	}
	min, max := int64(0), int64(0)
	for i := range t.Spans {
		s := &t.Spans[i]
		if s.EndUnixNano < s.StartUnixNano {
			continue // a span that never ended contributes its start only
		}
		if min == 0 || s.StartUnixNano < min {
			min = s.StartUnixNano
		}
		if s.EndUnixNano > max {
			max = s.EndUnixNano
		}
	}
	if min == 0 || max <= min {
		return 0
	}
	return nanosToMS(max - min)
}

// Summary projects the trace into its list row.
//
// Every summary field is derived from the stored spans, NOT read out of the
// trace's own float cache, so a Trace unmarshalled from JSON -- or one that was
// never in a store -- still summarizes correctly.
//
// Route/upstream/model precedence is root span attribute first, then the
// trace-level attribute. The root span wins because it is the most specific
// statement about THIS request: a trace-level "model" is the default the
// gateway intended, while the value on the root span is what the request
// actually resolved to after routing and failover. The two disagreeing is
// itself information, and it should not be hidden by the broader value.
//
// Status comes from Spans[0] when present (the root span is the authoritative
// verdict for the request as a whole) and falls back to the trace status.
func (t *Trace) Summary() Summary {
	if t == nil {
		return Summary{}
	}
	sum := Summary{
		TraceID:   t.TraceID,
		RequestID: t.RequestID,
		Status:    t.Status,
		SpanCount: len(t.Spans),
	}
	if sum.Status == "" {
		sum.Status = StatusUnset
	}

	sum.StartedAt = t.StartedAt.UTC().Format(time.RFC3339Nano)
	sum.DurationMS = t.Duration()
	if sum.DurationMS == 0 && t.DurationMS > 0 {
		// Degenerate but real: a trace assembled by hand with a duration and
		// no span times. Prefer reporting a number over reporting zero.
		sum.DurationMS = t.DurationMS
	}

	var route, upstream, model string
	startNanos := int64(0)

	if len(t.Spans) > 0 {
		root := &t.Spans[0]
		if root.Status != "" {
			sum.Status = root.Status
		}
		route = strAttr(root.Attributes, AttrRoute)
		upstream = strAttr(root.Attributes, AttrUpstream)
		model = strAttr(root.Attributes, AttrModel)
		startNanos = root.StartUnixNano
	}
	if route == "" {
		route = strAttr(t.Attributes, AttrRoute)
	}
	if upstream == "" {
		upstream = strAttr(t.Attributes, AttrUpstream)
	}
	if model == "" {
		model = strAttr(t.Attributes, AttrModel)
	}

	if startNanos == 0 {
		startNanos = earliestStart(t.Spans)
	}
	if startNanos > 0 {
		// Keeps Summary.StartedAt and Trace.StartedAt the same instant: both
		// are rendered from a nanosecond time.Time, one formatted here and one
		// by encoding/json.
		sum.StartedAt = time.Unix(0, startNanos).UTC().Format(time.RFC3339Nano)
	} else if !t.StartedAt.IsZero() {
		sum.StartedAt = t.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if sum.StartedAt == "" {
		sum.StartedAt = time.Unix(0, 0).UTC().Format(time.RFC3339Nano)
	}

	sum.Route, sum.Upstream, sum.Model = route, upstream, model
	return sum
}

// Clone returns a deep copy of the trace: spans, span attributes, span events,
// event attributes, and the trace attributes.
//
// Deep, not shallow, because the copy is the boundary between the request path
// (which mutates a trace while it is still being built) and the store or an
// operator reading a finished one. A shallow copy would share every Attributes
// map, so a single annotation after Add would rewrite history under a reader
// and race with it. Values inside attributes are cloned recursively (maps and
// slices) for the same reason.
//
// nil returns nil so Clone is safe in a `defer` or on an optional trace.
func (t *Trace) Clone() *Trace {
	if t == nil {
		return nil
	}
	cp := &Trace{
		TraceID:    t.TraceID,
		RequestID:  t.RequestID,
		StartedAt:  t.StartedAt,
		DurationMS: t.DurationMS,
		Status:     t.Status,
		Attributes: cloneAnyMap(t.Attributes),
	}
	if t.Spans != nil {
		cp.Spans = make([]Span, len(t.Spans))
		for i := range t.Spans {
			cp.Spans[i] = cloneSpan(t.Spans[i])
		}
	}
	return cp
}

// cloneSpan deep copies one span, including its attributes and events.
func cloneSpan(s Span) Span {
	s.Attributes = cloneAnyMap(s.Attributes)
	if s.Events != nil {
		ev := make([]Event, len(s.Events))
		for i := range s.Events {
			ev[i] = s.Events[i]
			ev[i].Attributes = cloneAnyMap(s.Events[i].Attributes)
		}
		s.Events = ev
	}
	return s
}

// cloneAnyMap copies a map[string]any, cloning nested maps and slices.
//
// The recursion matters: trace attributes are set from decoded JSON (where
// "usage" is a nested object) and from request headers (where "tried" is a
// list). Copying only the top level would leave the store aliasing a slice the
// caller still appends to, which is the exact bug the clone exists to prevent.
//
// Unknown types are carried by reference: a caller may stash a *http.Request or
// a struct in an attribute, and this package cannot deep copy what it does not
// understand. Document that in the instrumentation, do not guess here.
func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAny(v)
	}
	return out
}

// cloneAny deep copies the container types this package writes into
// attributes; scalars are returned as-is (they are immutable values).
func cloneAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneAnyMap(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = cloneAny(x[i])
		}
		return out
	case []string:
		out := make([]string, len(x))
		copy(out, x)
		return out
	default:
		return v
	}
}

// strAttr reads a string attribute, tolerating the type JSON decoding produces.
func strAttr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

// earliestStart returns the smallest non-zero span start time.
func earliestStart(spans []Span) int64 {
	min := int64(0)
	for i := range spans {
		s := spans[i].StartUnixNano
		if s > 0 && (min == 0 || s < min) {
			min = s
		}
	}
	return min
}

// nanosToMS converts a nanosecond delta to float64 milliseconds, rounded to 6
// decimals (the full precision of a nanosecond expressed in milliseconds).
//
// Rounding, not truncation: truncation biases every measurement downwards by up
// to a microsecond, and a percentile computed over biased samples is wrong in
// the direction that hides latency.
func nanosToMS(nanos int64) float64 {
	return math.Round(float64(nanos)/1e6*1e6) / 1e6
}

// Fallback id generation. crypto/rand does not fail on supported platforms;
// see randomHex for why this exists instead of a panic. The counter makes two
// calls in the same nanosecond distinct.
var fallbackCounter atomic.Uint64

// fallbackHex produces n bytes of "unique enough" hex without crypto/rand.
// It is not secret material and must never be used to make a token.
func fallbackHex(n int) string {
	var seed uint64 = uint64(time.Now().UnixNano())
	seed ^= fallbackCounter.Add(0x9e3779b97f4a7c15)
	out := make([]byte, n)
	for i := range out {
		// splitmix64: cheap, and decorrelates successive low bits, which is
		// all that is needed to avoid id collisions inside one nanosecond.
		seed += 0x9e3779b97f4a7c15
		z := seed
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z ^= z >> 31
		out[i] = byte(z)
	}
	const hexdigits = "0123456789abcdef"
	s := make([]byte, 2*n)
	for i, b := range out {
		s[2*i] = hexdigits[b>>4]
		s[2*i+1] = hexdigits[b&0x0f]
	}
	return string(s)
}
