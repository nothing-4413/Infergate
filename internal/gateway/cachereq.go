package gateway

// M2 semantic cache: what the cache needs to know about a request.
//
// The identity of a request is a bigger question than routing asks. Routing
// needs the model and the capabilities; the cache needs to know which question
// is being asked (so a paraphrase can be recognised) and which answer would be
// legitimate to hand back (so a paraphrase is not confused with a different
// question). Both are computed here, additively: parsedRequest keeps its three
// fields and the routing path is untouched.
//
// The two failure modes are not symmetric, and every rule below follows from
// that. A MISS costs an upstream call, which is exactly what would have
// happened without a cache. A WRONG HIT is invisible to the caller: it looks
// like a fast, confident answer to a question nobody asked. So when the
// identity is uncertain the request is not cacheable, and when a stored answer
// might belong to a different question it is not returned.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/infergate/infergate/internal/cache"
)

// chatMessage is the subset of a message the cache reasons about. Content is
// kept raw because an OpenAI message content can be a string or an array of
// parts, and a tool message carries a tool_call_id instead of prose.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
}

// cacheFields is the wider, cache-only decode of a request body.
//
// Everything that can change the answer is listed, including the fields this
// gateway never interprets for any other purpose (temperature, top_p, seed,
// stop). A parameter that is missing from this struct is a parameter that two
// genuinely different requests would share - that is how a wrong hit happens.
type cacheFields struct {
	Model               string          `json:"model"`
	Stream              bool            `json:"stream"`
	Prompt              json.RawMessage `json:"prompt"`
	Messages            []chatMessage   `json:"messages"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	N                   *int            `json:"n"`
	Seed                *int            `json:"seed"`
	Stop                json.RawMessage `json:"stop"`
	Tools               json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ResponseFormat      json.RawMessage `json:"response_format"`
	User                string          `json:"user"`
}

// cacheIdentity is everything the cache needs about one request, computed once
// so that the lookup, the store and the response headers cannot disagree.
type cacheIdentity struct {
	// Eligible is false when this request must never touch the cache, with
	// Reason saying why. The reason is reported (header, log) rather than
	// swallowed: "why was this not cached" is the first question an operator
	// asks, and a silent skip is indistinguishable from a broken cache.
	Eligible bool
	Reason   string

	Scope     string
	ExactKey  string
	Signature string
	Prompt    string
	Model     string
	Tenant    string

	HasTools         bool
	Nondeterministic bool
	Bypass           bool
	Refresh          bool
}

// cacheIdentityFor derives the identity of a request. It is a pure function of
// the request line, the headers it reads and the body, which is what makes the
// rules testable without a store.
//
// The body passed here must be the body the CLIENT sent, before any model
// rewrite: rewriteModel re-marshals the JSON with sorted keys, so an exact key
// taken afterwards would differ between two identical requests that merely
// spelled the model name differently.
func cacheIdentityFor(r *http.Request, body []byte, parsed parsedRequest) cacheIdentity {
	ident := cacheIdentity{Model: parsed.Model}

	if !isCompletionPath(r.URL.Path) {
		// Embedding and model-list responses are cheap to recompute and a
		// vector is not an answer - caching them would add a store and a
		// lookup to save less than they cost.
		ident.Reason = "path is not a completion route"
		return ident
	}
	if !parsed.IsJSON {
		ident.Reason = "body is not JSON"
		return ident
	}

	var f cacheFields
	if err := json.Unmarshal(body, &f); err != nil {
		ident.Reason = "body did not parse: " + err.Error()
		return ident
	}
	if f.Model == "" {
		f.Model = parsed.Model
	}

	prompt, prefix, ok := conversationPrompt(f)
	if !ok {
		ident.Reason = "no user prompt to key on"
		return ident
	}

	ident.Prompt = prompt
	ident.Model = f.Model
	ident.Tenant = tenantFor(r)
	ident.HasTools = jsonPresent(f.Tools)
	// temperature 0 means "as deterministic as the backend can be", which is
	// the only regime where a stored answer is still the answer. top_p below 1
	// is the same statement made a different way. Anything else is sampling,
	// and a sampled answer is one draw, not the answer.
	ident.Nondeterministic = (f.Temperature != nil && *f.Temperature > 0) ||
		(f.TopP != nil && *f.TopP < 1)

	ident.Signature = answerSignature(f, prefix)
	ident.Scope = cache.ScopeFor(ident.Tenant, f.Model, capabilitiesFor(r)...)
	ident.ExactKey = cache.ExactKeyFor(f.Model, body, r.URL.Path, ident.Tenant)

	switch directive := strings.ToLower(strings.TrimSpace(r.Header.Get(HeaderCache))); directive {
	case cacheDirectiveBypass:
		ident.Bypass = true
	case cacheDirectiveRefresh:
		ident.Refresh = true
	case "":
	default:
		// An unrecognised directive is ignored rather than guessed at: a typo
		// must not silently become "bypass everything" or "serve stale".
	}

	ident.Eligible = true
	return ident
}

// answerSignature fingerprints every input that can change the answer, so that
// a semantically similar prompt is only served from a stored entry that was
// produced under the same conditions.
//
// The conversation prefix is hashed in full, not just the system message: in an
// agent loop the same question means something different after different tool
// results, and matching only on the last user message would happily hand back a
// plan computed from a different state of the world.
func answerSignature(f cacheFields, prefix string) string {
	canonical := struct {
		Model               string          `json:"model"`
		Stream              bool            `json:"stream"`
		Temperature         *float64        `json:"temperature"`
		TopP                *float64        `json:"top_p"`
		MaxTokens           *int            `json:"max_tokens"`
		MaxCompletionTokens *int            `json:"max_completion_tokens"`
		N                   *int            `json:"n"`
		Seed                *int            `json:"seed"`
		Stop                json.RawMessage `json:"stop"`
		Tools               json.RawMessage `json:"tools"`
		ToolChoice          json.RawMessage `json:"tool_choice"`
		ResponseFormat      json.RawMessage `json:"response_format"`
		Prefix              string          `json:"prefix"`
	}{
		Model: f.Model, Stream: f.Stream, Temperature: f.Temperature, TopP: f.TopP,
		MaxTokens: f.MaxTokens, MaxCompletionTokens: f.MaxCompletionTokens,
		N: f.N, Seed: f.Seed, Stop: f.Stop, Tools: f.Tools,
		ToolChoice: f.ToolChoice, ResponseFormat: f.ResponseFormat,
		Prefix: prefix,
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		// Marshalling a struct of raw JSON can only fail if a field holds
		// invalid JSON, which the request decoder would have rejected. A
		// signature that cannot be computed must not collide with a real one,
		// so return something no honest signature equals.
		return "uncomputable:" + err.Error()
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// conversationPrompt extracts the question the model is being asked and a hash
// of everything that came before it.
//
// The prompt is the last message the model must respond TO, which in a chat is
// the last user message and in a tool loop is the last tool result. Earlier
// messages are context, not the question: hashing them into the prefix keeps
// two identical questions asked in different conversations apart, while the
// lookup itself is free to match a paraphrase of the question.
func conversationPrompt(f cacheFields) (prompt, prefix string, ok bool) {
	if idx := lastQuestionIndex(f.Messages); idx >= 0 {
		prompt = messageText(f.Messages[idx].Content)
		if prompt == "" {
			return "", "", false
		}
		prefix = messagesFingerprint(f.Messages[:idx])
		return prompt, prefix, true
	}
	if jsonPresent(f.Prompt) {
		prompt = messageText(f.Prompt)
		if prompt == "" {
			return "", "", false
		}
		return prompt, "", true
	}
	return "", "", false
}

// lastQuestionIndex finds the newest message whose content is the thing being
// answered: a user turn, or a tool result in an agent loop. A trailing
// assistant message counts too - it is how a caller asks for a continuation -
// but only when nothing else is present.
func lastQuestionIndex(msgs []chatMessage) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		switch msgs[i].Role {
		case "user", "tool":
			if messageText(msgs[i].Content) != "" {
				return i
			}
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && messageText(msgs[i].Content) != "" {
			return i
		}
	}
	return -1
}

// messagesFingerprint hashes the messages exactly as they were sent: roles,
// content, tool calls and tool call ids. Serialising rather than interpreting
// means a field this code does not know about still changes the fingerprint,
// which errs towards a miss and never towards a wrong hit.
func messagesFingerprint(msgs []chatMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	h := sha256.New()
	for _, m := range msgs {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write(m.Content)
		h.Write([]byte{0})
		h.Write([]byte(m.Name))
		h.Write([]byte{0})
		h.Write([]byte(m.ToolCallID))
		h.Write([]byte{0})
		h.Write(m.ToolCalls)
		h.Write([]byte{0xff})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// messageText flattens a message content into the text a human asked. A content
// array is the multi-part form (text, image_url, input_text); only the text
// parts are joined, because a cache key built from an image's URL is not the
// text of the question.
func messageText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	if trimmed[0] != '[' {
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text == "" {
			continue
		}
		switch p.Type {
		case "", "text", "input_text", "output_text":
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// tenantFor decides the isolation namespace of a request.
//
// Two callers with different credentials must not share cached answers by
// accident: the same prompt can be confidential, and a shared scope turns one
// tenant's successful request into another tenant's data leak. The credential
// itself is hashed rather than stored, because a cache key is a thing people
// paste into issues and dashboards.
func tenantFor(r *http.Request) string {
	if t := strings.TrimSpace(r.Header.Get(HeaderTenant)); t != "" {
		return t
	}
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		sum := sha256.Sum256([]byte(normaliseCredential(auth)))
		return "key-" + hex.EncodeToString(sum[:8])
	}
	return "anonymous"
}

// normaliseCredential folds the case of an authentication scheme.
//
// "Bearer X" and "bearer X" are the same credential, and HTTP treats the scheme
// as case-insensitive while treating the token as opaque; hashing the header
// verbatim would hand one caller two cache scopes and - since M3 - two separate
// budgets, depending only on how its HTTP client capitalised a word. Only a
// case-variant of a known scheme is rewritten, so the canonical spelling keeps
// deriving exactly the key it derived before this existed.
func normaliseCredential(auth string) string {
	scheme, rest, found := strings.Cut(auth, " ")
	if !found || scheme == "Bearer" {
		return auth
	}
	if strings.EqualFold(scheme, "Bearer") {
		return "Bearer " + rest
	}
	return auth
}

// capabilitiesFor reads the capability constraint off the request. Two requests
// with different capability constraints can be routed to different backends and
// therefore get different answers, so capabilities belong in the scope.
func capabilitiesFor(r *http.Request) []string {
	raw := strings.TrimSpace(r.Header.Get(HeaderCapabilities))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// jsonPresent reports whether a raw JSON field was actually supplied. The
// decoder leaves absent fields as nil and explicit nulls as the four bytes
// "null"; both mean "not given".
func jsonPresent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}
