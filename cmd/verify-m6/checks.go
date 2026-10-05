package main

// Shared readers for the JSON the admin surface returns.
//
// The gate reads responses as generic maps rather than as typed structs: an
// admin payload is a view over live state, and a struct would make this gate
// agree with a field name instead of with the value. Every path is asserted, so
// a renamed or missing field is a failure and not a zero.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var verboseChecks bool

func detail(format string, args ...any) {
	if verboseChecks {
		info(format, args...)
	}
}

func getAny(c *checker, base, path, label string) (map[string]any, bool) {
	res := get(c, base, path, label)
	if !c.assert(res.status == 200, "%s: HTTP 200 from %s (got %d: %s)",
		label, path, res.status, truncate(res.body, 200)) {
		return nil, false
	}
	var out map[string]any
	if err := res.json(&out); err != nil {
		c.assert(false, "%s: %s is a JSON object (%v)", label, path, err)
		return nil, false
	}
	detail("%s: %s", label, truncate(res.body, 400))
	return out, true
}

// dig walks a decoded JSON value by key or list index. It reports a missing path
// as a failed assertion and returns nil, so a caller can keep going without a
// nil dereference.
func dig(c *checker, root any, path ...string) any {
	cur := root
	for i, key := range path {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[key]
			if !ok {
				c.assert(false, "missing JSON key %q at %s", key, strings.Join(path[:i+1], "."))
				return nil
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(key)
			if err != nil || idx < 0 || idx >= len(node) {
				c.assert(false, "JSON index %q out of range at %s (len %d)", key, strings.Join(path[:i+1], "."), len(node))
				return nil
			}
			cur = node[idx]
		default:
			c.assert(false, "JSON path %s: %s is not an object or array",
				strings.Join(path[:i], "."), strings.Join(path[i:], "."))
			return nil
		}
	}
	return cur
}

func digStr(c *checker, root any, path ...string) string {
	v := dig(c, root, path...)
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		c.assert(false, "JSON %s is a string (got %T)", strings.Join(path, "."), v)
		return ""
	}
	return s
}

func digNum(c *checker, root any, path ...string) float64 {
	v := dig(c, root, path...)
	switch n := v.(type) {
	case float64:
		return n
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			c.assert(false, "JSON %s is numeric (got %q)", strings.Join(path, "."), n)
		}
		return f
	default:
		c.assert(false, "JSON %s is numeric (got %T)", strings.Join(path, "."), v)
		return 0
	}
}

func digBool(c *checker, root any, path ...string) bool {
	v := dig(c, root, path...)
	b, ok := v.(bool)
	if !ok {
		c.assert(false, "JSON %s is a boolean (got %T)", strings.Join(path, "."), v)
		return false
	}
	return b
}

func digList(c *checker, root any, path ...string) []any {
	v := dig(c, root, path...)
	if v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		c.assert(false, "JSON %s is a list (got %T)", strings.Join(path, "."), v)
		return nil
	}
	return list
}

// findSession returns the session object with a matching id and tenant.
func findSession(c *checker, admin map[string]any, tenant, id string) map[string]any {
	for _, raw := range digList(c, admin, "sessions") {
		sess, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(sess["id"]) == id && (tenant == "" || fmt.Sprint(sess["tenant"]) == tenant) {
			return sess
		}
	}
	c.assert(false, "no session %q under tenant %q in /admin/sessions", id, tenant)
	return nil
}

// findTrace returns the trace detail whose request id matches.
func findTrace(c *checker, base, requestID, label string) map[string]any {
	list, ok := getAny(c, base, "/admin/traces?limit=100", label+" trace list")
	if !ok {
		return nil
	}
	if !digBool(c, list, "enabled") {
		c.assert(false, "%s: tracing is not enabled, so no trace can be inspected", label)
		return nil
	}
	for _, raw := range digList(c, list, "traces") {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(entry["request_id"]) != requestID {
			continue
		}
		traceID := fmt.Sprint(entry["trace_id"])
		got, ok := getAny(c, base, "/admin/traces/"+traceID, label+" trace detail")
		if !ok {
			return nil
		}
		return got
	}
	c.assert(false, "%s: no trace recorded for request id %s", label, requestID)
	return nil
}

// rootSpan picks the server span out of a trace.
func rootSpan(c *checker, trace map[string]any, label string) map[string]any {
	spans := digList(c, trace, "spans")
	if len(spans) == 0 {
		c.assert(false, "%s: the trace has at least one span", label)
		return nil
	}
	for _, raw := range spans {
		span, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(span["kind"]) == "server" {
			return span
		}
	}
	c.assert(false, "%s: the trace has a server span", label)
	return nil
}

// spanAttr reads one span attribute, tolerating a missing map.
func spanAttr(span map[string]any, key string) (any, bool) {
	if span == nil {
		return nil, false
	}
	attrs, ok := span["attributes"].(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := attrs[key]
	return v, ok
}

// hasEvent reports whether a span carries an event with this name.
func hasEvent(span map[string]any, name string) bool {
	if span == nil {
		return false
	}
	events, ok := span["events"].([]any)
	if !ok {
		return false
	}
	for _, raw := range events {
		if e, ok := raw.(map[string]any); ok && fmt.Sprint(e["name"]) == name {
			return true
		}
	}
	return false
}

// closeTo compares floats with a tolerance, which is what a cost in micro-USD
// requires.
func closeTo(got, want, tol float64) bool { return math.Abs(got-want) <= tol }
