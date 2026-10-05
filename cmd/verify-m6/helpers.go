package main

// Small shared helpers for the M6 gate.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/infergate/infergate/internal/config"
)

// configDuration converts a Go duration into the config type, spelling out the
// conversion so a check reads at the same level as the YAML it stands in for.
func configDuration(d time.Duration) config.Duration { return config.Duration(d) }

// mustAdmin fetches an admin surface and decodes it, returning an empty map on
// failure so a caller can keep asserting (the failure itself is already
// recorded by getAny).
func mustAdmin(c *checker, base, path, label string) map[string]any {
	out, _ := getAny(c, base, path, label)
	if out == nil {
		return map[string]any{}
	}
	return out
}

// toFloat reads a number out of decoded JSON, where every number arrives as a
// float64. A non-number is 0, which is what an absent field should compare as.
func toFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

// attrEquals compares one span attribute against an expected value. Attributes
// arrive as decoded JSON, so the comparison is textual -- the same rule every
// other assertion in this gate uses -- and an attribute that is absent is not
// an attribute that happens to equal nil.
func attrEquals(span map[string]any, key string, want any) bool {
	v, ok := spanAttr(span, key)
	return ok && fmt.Sprint(v) == fmt.Sprint(want)
}

// entryByKey returns the idempotency listing entry with this key, or nil after
// recording the failure. The listing is ordered by completion time, which is the
// same clock tick for back-to-back requests, so a caller that wants a specific
// key must ask for it by name rather than by index.
func entryByKey(c *checker, admin map[string]any, key string) map[string]any {
	for _, raw := range digList(c, admin, "entries") {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(entry["key"]) == key {
			return entry
		}
	}
	c.assert(false, "no idempotency entry with key %q in the listing", key)
	return nil
}

// parseTime reads an RFC3339 timestamp out of decoded JSON.
func parseTime(c *checker, value, label string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		c.assert(false, "%s: %q parses as RFC3339 (%v)", label, value, err)
	}
	return parsed
}

// probeBody posts a capability probe and decodes the answer. A nil return means
// the probe itself failed and the failure is already recorded.
func probeBody(c *checker, base string, payload map[string]any) map[string]any {
	res := post(c, base, "/v1/capabilities/probe", marshal(payload), nil, "capability probe")
	if !c.assert(res.status == http.StatusOK, "capability probe: HTTP 200 (got %d: %s)",
		res.status, truncate(res.body, 200)) {
		return nil
	}
	var out map[string]any
	if err := res.json(&out); err != nil {
		c.assert(false, "capability probe: the answer is JSON (%v)", err)
		return nil
	}
	return out
}
