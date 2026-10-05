package gateway

import (
	"net/http"
	"testing"
)

// TestGatewayHeadersDoNotReachProviders pins the outbound header rule.
//
// The gateway copies the caller's headers so that a provider sees the request it
// was written to see. The exception is InferGate's own control surface: those
// headers are instructions to this gateway, and Idempotency-Key is the sharpest
// case because a provider that implements its own idempotency on that header
// would dedupe ACROSS tenants of this gateway. Two tenants picking the same key
// string is not a collision here (keys are scoped per tenant) but it would be
// one there, and the second tenant would be served the first tenant's answer.
func TestGatewayHeadersDoNotReachProviders(t *testing.T) {
	src := http.Header{
		HeaderIdempotencyKey:  {"retry-1"},
		"X-InferGate-Tenant":  {"acme"},
		"X-InferGate-Session": {"conv-1"},
		"Authorization":       {"Bearer caller-key"},
		"Content-Type":        {"application/json"},
		"X-Custom":            {"keep-me"},
		"Connection":          {"X-Drop-Me"},
		"X-Drop-Me":           {"hop-by-hop"},
	}

	dst := http.Header{}
	copyHeaders(dst, src, true)

	for _, name := range []string{
		HeaderIdempotencyKey,
		"X-InferGate-Tenant",
		"X-InferGate-Session",
		"Connection",
		"X-Drop-Me",
	} {
		if got := dst.Get(name); got != "" {
			t.Errorf("copyHeaders forwarded %s = %q; it must stop at the gateway", name, got)
		}
	}
	for name, want := range map[string]string{
		"Authorization": "Bearer caller-key",
		"Content-Type":  "application/json",
		"X-Custom":      "keep-me",
	} {
		if got := dst.Get(name); got != want {
			t.Errorf("copyHeaders copied %s = %q, want %q", name, got, want)
		}
	}

	// The other direction is a response travelling back to the caller, where the
	// same names are the gateway's own answer and must survive.
	upstream := http.Header{HeaderIdempotencyKey: {"echoed-by-provider"}, "X-Ratelimit-Remaining": {"7"}}
	back := http.Header{}
	copyHeaders(back, upstream, false)
	if got := back.Get("X-Ratelimit-Remaining"); got != "7" {
		t.Errorf("a response header was dropped: X-Ratelimit-Remaining = %q", got)
	}
	if got := back.Get(HeaderIdempotencyKey); got != "echoed-by-provider" {
		t.Errorf("the response path dropped %s (%q)", HeaderIdempotencyKey, got)
	}
}
