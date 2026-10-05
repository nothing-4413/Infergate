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
}

// parsedRequest is the outcome of inspecting an inbound body.
type parsedRequest struct {
	// Model is the requested model, empty when the body omitted it.
	Model string

	// Stream is the requested streaming mode.
	Stream bool

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
	return parsedRequest{Model: fields.Model, Stream: fields.Stream, IsJSON: true}
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
