package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// chatRequestFields are the fields InferGate reads from an OpenAI-compatible
// chat completion request. Everything else travels through verbatim.
type chatRequestFields struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages json.RawMessage `json:"messages"`

	// Both completion ceilings are read, because the two spellings are both in
	// the wild: max_tokens is the original and still the most common,
	// max_completion_tokens is what newer OpenAI models require. Whichever the
	// caller sent is the one the quota estimate uses.
	MaxTokens           json.RawMessage `json:"max_tokens"`
	MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
}

// parsedRequest is the outcome of inspecting an inbound body.
type parsedRequest struct {
	// Model is the requested model, empty when the body omitted it.
	Model string

	// Stream is the requested streaming mode.
	Stream bool

	// MaxTokens is the completion ceiling the caller asked for, 0 when it asked
	// for none. It is used to size the quota reservation: a caller that has
	// already named its own upper bound deserves a lease that matches it, in
	// both directions.
	MaxTokens int

	// IsJSON reports whether the body parsed as a JSON object. A non-JSON body
	// is still proxied: the gateway must not become a validator that rejects
	// traffic the upstream would accept.
	IsJSON bool
}

// inspectRequest extracts the fields InferGate needs without disturbing the
// body.
//
// The body is decoded into map[string]json.RawMessage and re-marshalled rather
// than into an interface{}. Both approaches survive a round trip, but only the
// RawMessage form preserves every value's exact representation: large integers,
// high-precision floats and provider-specific nested objects keep their
// original bytes, so a field the gateway never looks at cannot be quietly
// renormalised (a 64-bit token count becoming 1e+06 is the classic symptom).
func inspectRequest(body []byte) parsedRequest {
	if len(bytes.TrimSpace(body)) == 0 {
		return parsedRequest{}
	}
	var fields chatRequestFields
	if err := json.Unmarshal(body, &fields); err != nil {
		return parsedRequest{}
	}
	return parsedRequest{
		Model:     fields.Model,
		Stream:    fields.Stream,
		MaxTokens: completionCeiling(fields.MaxTokens, fields.MaxCompletionTokens),
		IsJSON:    true,
	}
}

// completionCeiling picks the caller's completion cap out of the two spellings.
//
// max_completion_tokens wins when both are present: it is the field the models
// that accept it actually enforce, so honouring the deprecated spelling there
// would reserve against a number the provider ignores.
func completionCeiling(maxTokens, maxCompletionTokens json.RawMessage) int {
	for _, raw := range []json.RawMessage{maxCompletionTokens, maxTokens} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var n int
		if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// rewriteModel returns body with the top-level "model" field set to model.
//
// Returns the input unchanged when nothing needs to change, so the common path
// performs no allocation.
func rewriteModel(body []byte, model string) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, fmt.Errorf("gateway: body is not a JSON object: %w", err)
	}
	current, ok := fields["model"]
	if ok {
		var name string
		if err := json.Unmarshal(current, &name); err == nil && name == model {
			return body, nil
		}
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return body, err
	}
	fields["model"] = encoded
	// json.Marshal orders object keys lexicographically, so the rewritten body
	// is deterministic. OpenAI-compatible servers define no field order, and
	// deterministic output makes request-body assertions in tests stable.
	return json.Marshal(fields)
}

// rewriteMaxTokens returns body with its completion ceiling lowered to cap.
//
// It is the second lever of the degradation ladder, and it only ever lowers:
// a caller that asked for 50 tokens keeps asking for 50, while a caller that
// asked for 4000 (or named no ceiling at all, leaving the provider's own
// default in play) is brought down to the cap. Raising a request's ceiling
// would be the gateway spending money the caller did not ask for.
//
// When the body spells the ceiling max_completion_tokens, that is the field
// rewritten: adding max_tokens alongside it would send a request that two
// different providers interpret in two different ways.
//
// An explicit null, and a ceiling that is not a positive number, are both
// treated as "the caller named no limit" and are therefore capped too - the
// same reading completionCeiling applies when it sizes the reservation, so the
// amount reserved and the amount requested stay the same request.
func rewriteMaxTokens(body []byte, cap int) ([]byte, error) {
	if len(body) == 0 || cap <= 0 {
		return body, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, fmt.Errorf("gateway: body is not a JSON object: %w", err)
	}

	field := "max_tokens"
	raw, ok := fields["max_completion_tokens"]
	if !ok {
		raw, ok = fields["max_tokens"]
	}
	if ok {
		var current int
		if err := json.Unmarshal(raw, &current); err == nil && current > 0 && current <= cap {
			return body, nil
		}
		if _, hasMCT := fields["max_completion_tokens"]; hasMCT {
			field = "max_completion_tokens"
		}
	}
	encoded, err := json.Marshal(cap)
	if err != nil {
		return body, err
	}
	fields[field] = encoded
	return json.Marshal(fields)
}

// isStreamPath reports whether a path can produce an SSE stream. Only chat
// completions stream in M0; completions/embeddings return whole bodies.
func isStreamPath(path string) bool {
	return strings.HasSuffix(strings.TrimSuffix(path, "/"), "/chat/completions")
}

// isCompletionPath reports whether a path is a model-generation call whose body
// carries a "model" field the gateway may need to rewrite.
func isCompletionPath(path string) bool {
	p := strings.TrimSuffix(path, "/")
	return strings.HasSuffix(p, "/chat/completions") ||
		strings.HasSuffix(p, "/completions") ||
		strings.HasSuffix(p, "/embeddings")
}

// routeLabel normalises a request path into a low-cardinality metrics label.
//
// Paths carry identifiers, and using a raw path as a metric label is how a
// metrics backend falls over: one time series per conversation id. Anything
// containing a segment that is not a known endpoint collapses to "other".
func routeLabel(path string) string {
	p := strings.TrimSuffix(path, "/")
	switch {
	case p == "" || p == "/":
		return "/"
	case strings.HasSuffix(p, "/chat/completions"):
		return "/v1/chat/completions"
	case strings.HasSuffix(p, "/completions"):
		return "/v1/completions"
	case strings.HasSuffix(p, "/embeddings"):
		return "/v1/embeddings"
	case strings.HasSuffix(p, "/models"):
		return "/v1/models"
	case strings.HasSuffix(p, "/responses"):
		return "/v1/responses"
	default:
		return "other"
	}
}
