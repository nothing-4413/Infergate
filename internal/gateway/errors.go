package gateway

import (
	"encoding/json"
	"net/http"
)

// ErrorBody is the OpenAI-compatible error envelope.
//
// InferGate reproduces this shape for its own failures rather than returning
// bare text: clients that already parse provider errors (every LLM SDK) keep
// working unchanged, and a gateway-specific error is distinguishable by its
// "type" prefix.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the inner error object.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    any    `json:"code"`
}

// Error type identifiers. The "infergate_" prefix keeps gateway-local classes
// from colliding with provider error types such as "invalid_request_error".
const (
	TypeBadRequest   = "infergate_bad_request"
	TypeNoUpstream   = "infergate_no_upstream"
	TypeBadGateway   = "infergate_upstream_unavailable"
	TypeTimeout      = "infergate_upstream_timeout"
	TypeInternal     = "infergate_internal_error"
	TypeBodyTooLarge = "infergate_request_too_large"

	// TypeQuotaExceeded is a budget refusal (HTTP 429). It is distinct from a
	// provider's own rate limit because the caller's remedy differs: a provider
	// 429 says "this backend is busy, retry or fail over", while this one says
	// "this tenant has spent its allowance, and no other backend will help".
	TypeQuotaExceeded = "infergate_quota_exceeded"

	// TypeQuotaStore is a refusal because the budget could not be read (HTTP
	// 503). It is a gateway fault, not the caller's, and it is reported
	// separately so an operator can tell a broken Redis from an exhausted
	// tenant.
	TypeQuotaStore = "infergate_quota_unavailable"
)

// writeError emits an OpenAI-shaped error response.
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorBody{Error: ErrorDetail{Message: msg, Type: typ}})
}

// writeRawError passes through an upstream error body untouched.
//
// Re-encoding a provider's error would discard vendor fields that clients use
// for automated remediation (retry hints, quota reset timestamps), so the bytes
// are forwarded verbatim.
func writeRawError(w http.ResponseWriter, status int, contentType string, body []byte) {
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// errorBodyBytes renders the same envelope writeError would send, as bytes.
//
// It exists for the failover path: an attempt that fails in a way the caller
// cannot see yet must carry its response with it, so that a later candidate can
// still answer. Producing the body here means the "every candidate failed" case
// and the "write it immediately" case emit byte-identical errors.
func errorBodyBytes(status int, typ, msg string) []byte {
	_ = status // the status travels separately, with the header
	body, err := json.Marshal(ErrorBody{Error: ErrorDetail{Message: msg, Type: typ}})
	if err != nil {
		// json.Marshal cannot fail for a struct of strings and nils; the
		// fallback keeps the function total rather than returning no body.
		return []byte(`{"error":{"message":"internal gateway error","type":"infergate_internal_error","param":null,"code":null}}`)
	}
	return append(body, '\n')
}
