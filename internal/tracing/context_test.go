package tracing

import (
	"math/rand"
	"strings"
	"testing"
)

// randomHexID returns n lowercase hex chars from a seeded (deterministic,
// non-crypto) source. Used only to build valid traceparent headers in tests.
func randomHexID(r *rand.Rand, n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[r.Intn(16)]
	}
	return string(b)
}

// TestParseRoundTrip proves that Header() is the inverse of Parse() for every
// shape a Context can take: sampled/unsampled, with and without tracestate.
func TestParseRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	cases := []struct {
		name       string
		traceID    string
		spanID     string
		sampled    bool
		traceState string
	}{
		{"sampled", testTraceID, "00f067aa0ba902b7", true, ""},
		{"not sampled", "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331", false, ""},
		{"with tracestate", "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331", true, "vendor=opaque,other=1"},
		{"random sampled", randomHexID(r, 32), randomHexID(r, 16), true, "a=b"},
		{"random unsampled", randomHexID(r, 32), randomHexID(r, 16), false, ""},
		{"minimal ids", strings.Repeat("0", 31) + "1", strings.Repeat("0", 15) + "1", true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := Context{
				TraceID:    tc.traceID,
				SpanID:     tc.spanID,
				Sampled:    tc.sampled,
				TraceState: tc.traceState,
			}
			header := orig.Header()

			got, ok := Parse(header, tc.traceState)
			if !ok {
				t.Fatalf("Parse(%q) rejected a header this package generated", header)
			}
			if got.TraceID != orig.TraceID {
				t.Errorf("TraceID = %q, want %q", got.TraceID, orig.TraceID)
			}
			if got.SpanID != orig.SpanID {
				t.Errorf("SpanID = %q, want %q", got.SpanID, orig.SpanID)
			}
			if got.Sampled != orig.Sampled {
				t.Errorf("Sampled = %v, want %v", got.Sampled, orig.Sampled)
			}
			if got.TraceState != tc.traceState {
				t.Errorf("TraceState = %q, want %q", got.TraceState, tc.traceState)
			}
			if !got.Remote {
				t.Error("Remote = false after parsing an inbound header, want true")
			}
			if again := got.Header(); again != header {
				t.Errorf("Header() not stable: first %q, second %q", header, again)
			}
		})
	}
}

// TestParseRejects is the table of every malformed traceparent the grammar
// forbids. Each case asserts BOTH that ok is false and that the Context is the
// zero value: a partially filled Context is the failure mode this table exists
// to prevent, because it produces a span with an id that matches nothing.
func TestParseRejects(t *testing.T) {
	const (
		validTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
		validSpan  = "00f067aa0ba902b7"
	)

	cases := []struct {
		name        string
		traceparent string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"leading space", " " + "00-" + validTrace + "-" + validSpan + "-01"},
		{"trailing space", "00-" + validTrace + "-" + validSpan + "-01 "},
		{"newline suffix", "00-" + validTrace + "-" + validSpan + "-01\n"},
		{"no segments", "not-a-traceparent"},
		{"three segments", "00-" + validTrace + "-" + validSpan},
		{"five segments", "00-" + validTrace + "-" + validSpan + "-01-extra"},
		{"version ff", "ff-" + validTrace + "-" + validSpan + "-01"},
		{"future version 01", "01-" + validTrace + "-" + validSpan + "-01"},
		{"version too short", "0-" + validTrace + "-" + validSpan + "-01"},
		{"version too long", "000-" + validTrace + "-" + validSpan + "-01"},
		{"version hex but not 00", "0A-" + validTrace + "-" + validSpan + "-01"},
		{"trace id too short", "00-" + validTrace[:31] + "-" + validSpan + "-01"},
		{"trace id too long", "00-" + validTrace + "a-" + validSpan + "-01"},
		{"trace id not hex", "00-" + "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz" + "-" + validSpan + "-01"},
		{"trace id uppercase", "00-" + "4BF92F3577B34DA6A3CE929D0E0E4736" + "-" + validSpan + "-01"},
		{"trace id all zero", "00-" + strings.Repeat("0", 32) + "-" + validSpan + "-01"},
		{"trace id all ff", "00-" + strings.Repeat("f", 32) + "-" + validSpan + "-01"},
		{"span id too short", "00-" + validTrace + "-" + validSpan[:15] + "-01"},
		{"span id too long", "00-" + validTrace + "-" + validSpan + "0-01"},
		{"span id not hex", "00-" + validTrace + "-zzzzzzzzzzzzzzzz-01"},
		{"span id uppercase", "00-" + validTrace + "-00F067AA0BA902B7-01"},
		{"span id all zero", "00-" + validTrace + "-" + strings.Repeat("0", 16) + "-01"},
		{"span id all ff", "00-" + validTrace + "-" + strings.Repeat("f", 16) + "-01"},
		{"flags empty", "00-" + validTrace + "-" + validSpan + "-"},
		{"flags one char", "00-" + validTrace + "-" + validSpan + "-0"},
		{"flags three chars", "00-" + validTrace + "-" + validSpan + "-010"},
		{"flags not hex", "00-" + validTrace + "-" + validSpan + "-zz"},
		{"flags uppercase", "00-" + validTrace + "-" + validSpan + "-0A"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, ok := Parse(tc.traceparent, "vendor=1")
			if ok {
				t.Fatalf("Parse(%q) accepted a malformed header: %+v", tc.traceparent, ctx)
			}
			if ctx != (Context{}) {
				t.Errorf("rejected header produced a partial Context: %+v", ctx)
			}
		})
	}
}

// TestParseFlagsBit documents that only bit 0 of the flags octet is meaningful
// here: a producer that sets the "random trace id" bit must still be parsed as
// a valid, sampled (or unsampled) trace.
func TestParseFlagsBit(t *testing.T) {
	cases := []struct {
		flags   string
		sampled bool
	}{
		{"00", false},
		{"01", true},
		{"02", false}, // random-trace-id bit, not sampled
		{"03", true},  // random + sampled
	}
	for _, tc := range cases {
		t.Run(tc.flags, func(t *testing.T) {
			ctx, ok := Parse("00-"+testTraceID+"-00f067aa0ba902b7-"+tc.flags, "")
			if !ok {
				t.Fatalf("flags %q rejected", tc.flags)
			}
			if ctx.Sampled != tc.sampled {
				t.Errorf("Sampled = %v for flags %s, want %v", ctx.Sampled, tc.flags, tc.sampled)
			}
		})
	}
}

// TestParseTraceStateOpaque proves tracestate is carried verbatim and never
// validated: it is another vendor's state, including an empty value.
func TestParseTraceStateOpaque(t *testing.T) {
	header := "00-" + testTraceID + "-00f067aa0ba902b7-01"
	for _, ts := range []string{"", "a=b", "vendor=opaque,other=1", "weird value with spaces", "==="} {
		ctx, ok := Parse(header, ts)
		if !ok {
			t.Fatalf("valid traceparent rejected when tracestate = %q", ts)
		}
		if ctx.TraceState != ts {
			t.Errorf("TraceState = %q, want %q (verbatim)", ctx.TraceState, ts)
		}
	}
}

// TestNewRootContextShape pins the shape of a locally started trace.
func TestNewRootContextShape(t *testing.T) {
	c := NewRootContext()

	if !isID(c.TraceID, traceIDLen) {
		t.Errorf("TraceID = %q, want 32 lowercase hex chars", c.TraceID)
	}
	if !isID(c.SpanID, spanIDLen) {
		t.Errorf("SpanID = %q, want 16 lowercase hex chars", c.SpanID)
	}
	if !c.Sampled {
		t.Error("Sampled = false, want true: a gateway that defaults to unsampled produces no traces")
	}
	if c.Remote {
		t.Error("Remote = true, want false: a root context was not received from a peer")
	}
	if c.TraceState != "" {
		t.Errorf("TraceState = %q, want empty", c.TraceState)
	}
	if _, ok := Parse(c.Header(), c.TraceState); !ok {
		t.Errorf("Parse rejected the header of a root context: %q", c.Header())
	}
}

// TestGeneratedIDsAreUnique samples the id generators well past the 10k the
// spec requires and checks the grammar on every single output.
func TestGeneratedIDsAreUnique(t *testing.T) {
	const n = 10000

	traceIDs := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewTraceID()
		if !isID(id, traceIDLen) {
			t.Fatalf("NewTraceID() = %q, not 32 lowercase hex chars", id)
		}
		if _, dup := traceIDs[id]; dup {
			t.Fatalf("NewTraceID() collided after %d generations: %q", i, id)
		}
		traceIDs[id] = struct{}{}
	}

	spanIDs := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewSpanID()
		if !isID(id, spanIDLen) {
			t.Fatalf("NewSpanID() = %q, not 16 lowercase hex chars", id)
		}
		if _, dup := spanIDs[id]; dup {
			t.Fatalf("NewSpanID() collided after %d generations: %q", i, id)
		}
		spanIDs[id] = struct{}{}
	}
}

// TestIsAllZeroOrAllFF covers the reserved-value predicate directly, including
// the mixed case that must NOT be considered reserved.
func TestIsAllZeroOrAllFF(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true}, // vacuously true; callers always check length first
		{"0", true},
		{strings.Repeat("0", 32), true},
		{strings.Repeat("f", 32), true},
		{strings.Repeat("f", 16), true},
		{"00f067aa0ba902b7", false},
		{"0000000000000000", true},
		{"000000000000000a", false},
		{"fffffffffffffffa", false},
	}
	for _, tc := range cases {
		if got := isAllZeroOrAllFF(tc.in); got != tc.want {
			t.Errorf("isAllZeroOrAllFF(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
