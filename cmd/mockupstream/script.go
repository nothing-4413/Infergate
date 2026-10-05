// Scripted-response mode for the mock upstream.
//
// Why it exists: M6 wires the gateway into a Python agent runtime (Warden) that
// uses native function calling. Proving that an agent loop "reached the answer"
// on a paid provider is slow, non-deterministic and unavailable in CI, and
// proving that an idempotent replay caused ZERO extra upstream calls cannot be
// done at all from the gateway side alone. A script file turns the mock into a
// deterministic tool-calling model ("first turn: call get_weather; any turn with
// a tool result: echo it back") and GET /calls gives a gate an exact, external
// witness of how many times the upstream was really asked.
//
// The whole feature is dormant when -script is empty, so every M0-M5 gate that
// curls this binary keeps seeing byte-for-byte the old behaviour.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// script is the parsed contents of the -script file.
type script struct {
	// Default answers a chat request that no rule matched. When it is absent the
	// built-in answer ("mock answer to ...") is kept, so adding a script with
	// only rules does not silently change unmatched traffic.
	Default *scriptRespond `json:"default"`
	// Rules are evaluated top to bottom; the first match wins.
	Rules []scriptRule `json:"rules"`
	// path is the file this script came from, for startup logging and errors.
	path string
}

// scriptRule is one "when X, answer Y" pair.
type scriptRule struct {
	// Name identifies the rule in /calls and in logs. Unnamed rules are still
	// usable; they are reported as "" so a test can still tell them apart from
	// the default path.
	Name    string        `json:"name"`
	When    scriptWhen    `json:"when"`
	Respond scriptRespond `json:"respond"`

	// hits counts requests that matched this rule. It drives fail_first and is
	// atomic because every chat request of a running server may reach it.
	hits atomic.Int64
}

// scriptWhen is a conjunction: every field that is set must match. A zero value
// (or an omitted "when") therefore matches every chat request.
type scriptWhen struct {
	// HasToolResult requires the request to contain (true) or to lack (false) a
	// message with role "tool". It is a two-valued predicate that is always
	// applied, so unlike Contains/Turn/Path there is no "do not care" setting:
	// {"has_tool_result": false} really means "no tool result yet".
	HasToolResult bool `json:"has_tool_result"`
	// ToolsPresent requires a non-empty "tools" array in the request body.
	ToolsPresent bool `json:"tools_present"`
	// Contains is a case-insensitive substring test against the LAST user-role
	// message. Empty means "do not care".
	Contains string `json:"contains"`
	// Turn matches the 1-based count of CHAT requests this process has already
	// handled (evaluated before the counter advances, so 1 is the first chat
	// request). 0 means "any turn".
	Turn int `json:"turn"`
	// Stream matches the request's "stream" flag. It is a pointer so that
	// "stream": false is distinguishable from an omitted field: the dispatch
	// happens before the body is inspected otherwise.
	Stream *bool `json:"stream"`
	// Path matches r.URL.Path exactly (e.g. "/v1/chat/completions").
	Path string `json:"path"`
}

// scriptRespond is what a matched rule answers with. Every field has a defined
// default so a minimal rule ("respond": {}) stays a plain mock answer.
type scriptRespond struct {
	// Content is the assistant text. Empty means "no text"; it is deliberately
	// not the same as "use the legacy default", because a tool-call answer
	// normally carries empty content.
	Content string `json:"content"`
	// EchoToolResult, when true, replaces Content with the concatenation of the
	// contents of every role:"tool" message in request order. This is what makes
	// a deterministic two-turn agent loop possible.
	EchoToolResult bool `json:"echo_tool_result"`
	// ToolCalls emits an assistant message carrying OpenAI-shaped tool_calls.
	ToolCalls []scriptToolCall `json:"tool_calls"`
	// Usage overrides the token numbers. Nil keeps the numbers this binary
	// computes today for the equivalent response shape. TotalTokens is filled in
	// when the script omits it.
	Usage *scriptUsage `json:"usage"`
	// Status is the HTTP status to answer with. 0 and 200 both mean "success".
	Status int `json:"status"`
	// ErrorBody, when set, is used verbatim as the JSON body of a non-2xx
	// answer. Otherwise the OpenAI-shaped envelope is emitted.
	ErrorBody json.RawMessage `json:"error_body"`
	// DelayMs sleeps before the response is written. The -ttfb stall, which
	// happens first, still applies.
	DelayMs int `json:"delay_ms"`
	// Drop closes the connection without writing anything, so the gateway sees a
	// broken hop rather than an HTTP status. Drop wins over Status.
	Drop bool `json:"drop"`
	// FailFirst: the first N requests that match this rule fail with Status
	// (default 502) and only from request N+1 on does the rule behave normally.
	FailFirst int `json:"fail_first"`
}

// scriptToolCall is one entry of respond.tool_calls.
type scriptToolCall struct {
	// ID is the tool_call id echoed to the client. Omitted ids become call_1,
	// call_2, ... in list order. An explicit id must stay stable across retries;
	// a scripted failure followed by a replay must produce the same id or the
	// agent runtime cannot correlate the tool result with the request.
	ID string `json:"id"`
	// Name is the function name the runtime dispatches on.
	Name string `json:"name"`
	// Arguments is the function's argument object. It is serialised into the
	// OpenAI wire form (a JSON *string*) when it is marshalled.
	Arguments json.RawMessage `json:"arguments"`
}

// scriptUsage mirrors the OpenAI usage block. CachedTokens exists because the
// gateway's accounting distinguishes cached from fresh prompt tokens.
type scriptUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

// loadScript reads and parses a script file. Every failure names the problem
// (missing file, unreadable file, bad JSON, unknown field) so an operator can
// fix the file instead of guessing why the mock served the wrong answer.
func loadScript(path string) (*script, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read script %s: %w", path, err)
	}
	sc, err := parseScript(raw)
	if err != nil {
		return nil, fmt.Errorf("script %s: %w", path, err)
	}
	sc.path = path
	return sc, nil
}

// parseScript decodes a script body. Unknown fields are rejected on purpose: a
// typo in "tools_present" would otherwise silently turn a tool-calling rule
// into a match-everything rule, and the failure would show up only as an agent
// run that behaved strangely.
func parseScript(raw []byte) (*script, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var sc script
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	// Reject trailing content: a truncated edit that leaves two JSON objects in
	// the file is a real operator mistake and decoding would hide it.
	if dec.More() {
		return nil, errors.New("parse: trailing content after the script object")
	}
	if sc.Default == nil && len(sc.Rules) == 0 {
		return nil, errors.New("parse: script has neither rules nor a default response")
	}
	for i := range sc.Rules {
		if sc.Rules[i].Respond.FailFirst < 0 {
			return nil, fmt.Errorf("parse: rules[%d] (%s): fail_first must not be negative", i, sc.Rules[i].Name)
		}
		if sc.Rules[i].When.Turn < 0 {
			return nil, fmt.Errorf("parse: rules[%d] (%s): when.turn must not be negative", i, sc.Rules[i].Name)
		}
		if sc.Rules[i].Respond.Status < 0 || sc.Rules[i].Respond.Status > 599 {
			return nil, fmt.Errorf("parse: rules[%d] (%s): status %d is not a valid HTTP status", i, sc.Rules[i].Name, sc.Rules[i].Respond.Status)
		}
	}
	return &sc, nil
}

// answerScripted applies the failure and drop side of a scripted response
// BEFORE the success shape is built: a rule that fails or drops must never write
// the success envelope, because an agent runtime that parsed a 200 would treat
// the injected failure as a real answer and the gate would prove nothing.
//
// It returns true when it has already written (or deliberately abandoned) the
// response. The success path returns false: applying a delay is not writing an
// answer, so the caller still has to build the success shape.
func (s *server) answerScripted(w http.ResponseWriter, r *http.Request, req chatRequest, m scriptMatch) bool {
	// A legacy match (no rule, no script default) has no scripted response at
	// all: the caller falls through to the built-in answer.
	if m.Respond == nil {
		return false
	}
	rule := m.RuleName
	if m.Rule != nil {
		rule = m.Rule.Name
	}
	hits := int64(0)
	if m.Rule != nil {
		hits = m.Rule.hits.Load()
	}
	status, failing := effectiveStatus(m.Respond, hits)

	// drop wins over status: the point is a broken connection, not a status code.
	if m.Respond.Drop {
		s.log.Info("script dropping connection", "rule", rule, "turn", m.Turn)
		// Tell the counting writer this was a deliberate abandonment rather than
		// a handler that forgot to answer, so /calls reports it as dropped.
		if d, ok := w.(interface{ wantsDrop() }); ok {
			d.wantsDrop()
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		} else {
			// Without a Hijacker there is no way to close the hop from here, and
			// http.ErrAbortHandler is exactly the escape hatch net/http documents
			// for "I cannot serve this request": it aborts the connection for the
			// client while suppressing the panic log.
			panic(http.ErrAbortHandler)
		}
		return true
	}

	if failing {
		s.log.Info("script failing request", "rule", rule, "turn", m.Turn, "status", status)
		if m.Respond.DelayMs > 0 {
			time.Sleep(time.Duration(m.Respond.DelayMs) * time.Millisecond)
		}
		writeScriptedError(w, status, m.Respond.ErrorBody)
		return true
	}

	if m.Respond.DelayMs > 0 {
		time.Sleep(time.Duration(m.Respond.DelayMs) * time.Millisecond)
	}
	return false
}

// writeScriptedError answers with the rule's own body when one was given, and
// with the OpenAI error envelope otherwise. The body is written verbatim, so a
// gate can assert the exact bytes a provider-style failure carries.
func writeScriptedError(w http.ResponseWriter, status int, body json.RawMessage) {
	if len(body) == 0 {
		writeJSON(w, status, map[string]any{
			"error": map[string]any{
				"message": "scripted failure",
				"type":    "mock_error",
			},
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := body
	if payload[len(payload)-1] != '\n' {
		payload = append(payload, '\n')
	}
	_, _ = w.Write(payload)
}

// scriptMatch is the outcome of evaluating a request against a script.
type scriptMatch struct {
	// Rule is the first matching rule, or nil when the default response applies.
	Rule *scriptRule
	// Respond is the response to build (the matched rule's, or the script's
	// default when that is present).
	Respond *scriptRespond
	// RuleName labels the request in /calls ("default" when no rule matched).
	RuleName string
	// Turn is the 1-based chat-request counter this match was evaluated against.
	Turn int
	// Legacy means "use today's built-in answer": no rule matched and the script
	// declares no default.
	Legacy bool
}

// selectResponse picks the response for one chat request. It is called only
// after the request body has been validated, so the turn counter counts requests
// the mock really answered rather than malformed ones.
func (sc *script) selectResponse(req chatRequest, stream bool, path string, turn int) scriptMatch {
	for i := range sc.Rules {
		if sc.Rules[i].matches(req, stream, path, turn) {
			rule := &sc.Rules[i]
			rule.hits.Add(1)
			return scriptMatch{Rule: rule, Respond: &rule.Respond, RuleName: rule.Name, Turn: turn}
		}
	}
	if sc.Default != nil {
		return scriptMatch{Respond: sc.Default, RuleName: "default", Turn: turn}
	}
	return scriptMatch{RuleName: "default", Turn: turn, Legacy: true}
}

func (r *scriptRule) matches(req chatRequest, stream bool, path string, turn int) bool {
	if r.When.HasToolResult != hasToolResult(req.Messages) {
		return false
	}
	if r.When.ToolsPresent != (len(req.Tools) > 0) {
		return false
	}
	if r.When.Contains != "" && !strings.Contains(strings.ToLower(lastUserContent(req.Messages)), strings.ToLower(r.When.Contains)) {
		return false
	}
	if r.When.Turn != 0 && r.When.Turn != turn {
		return false
	}
	if r.When.Stream != nil && *r.When.Stream != stream {
		return false
	}
	if r.When.Path != "" && r.When.Path != path {
		return false
	}
	return true
}

func hasToolResult(msgs []chatMessage) bool {
	for _, m := range msgs {
		if m.Role == "tool" {
			return true
		}
	}
	return false
}

// lastUserContent returns the content of the last user-role message, which is
// the same message the legacy answer and the "contains" predicate reason about.
func lastUserContent(msgs []chatMessage) string {
	last := ""
	for _, m := range msgs {
		if m.Role == "user" {
			last = m.Content
		}
	}
	return last
}

// toolResultText concatenates the contents of every tool-role message in
// request order, with no prefix or separator. The agent runtime compares the
// answer with what its own tool returned, so inventing decoration here would
// make the assertion depend on the mock's formatting.
func toolResultText(msgs []chatMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "tool" {
			b.WriteString(m.Content)
		}
	}
	return b.String()
}

// effectiveStatus resolves the status a response should be answered with,
// applying fail_first and the "zero means 200" rule.
func effectiveStatus(r *scriptRespond, hits int64) (status int, failing bool) {
	status = r.Status
	if status == 0 {
		status = http.StatusOK
	}
	if r.FailFirst > 0 && hits <= int64(r.FailFirst) {
		// fail_first must fail even when the rule omits "status"; 502 is the
		// documented default for an injected upstream failure.
		if r.Status == 0 {
			status = http.StatusBadGateway
		}
		return status, true
	}
	return status, status < 200 || status > 299
}

// respond builds the assistant turn a match describes. The returned
// finishReason is "tool_calls" when tool calls are present, else "stop".
func (m scriptMatch) respond(req chatRequest) (content string, calls []map[string]any, finishReason string) {
	r := m.Respond
	if r == nil {
		return completionFor(req), nil, "stop"
	}
	content = r.Content
	if r.EchoToolResult {
		content = toolResultText(req.Messages)
	}
	calls = wireToolCalls(r.ToolCalls)
	if len(calls) > 0 {
		return content, calls, "tool_calls"
	}
	return content, nil, "stop"
}

// wireToolCalls converts the script's tool calls into the OpenAI wire shape.
// The arguments object is re-encoded as a JSON *string*, which is what every
// OpenAI-compatible client expects and what makes the streaming and
// non-streaming shapes identical.
func wireToolCalls(specs []scriptToolCall) []map[string]any {
	if len(specs) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(specs))
	for i, tc := range specs {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		args := "{}"
		if len(tc.Arguments) > 0 {
			args = string(tc.Arguments)
		}
		out = append(out, map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      tc.Name,
				"arguments": args,
			},
		})
	}
	return out
}

// usageFor returns the usage block for a response: the script's numbers when it
// supplied any, and otherwise exactly the numbers the non-scripted mock has
// always computed for that shape (so adding -script does not change the
// accounting the M3 gate reconciles).
func (m scriptMatch) usageFor(req chatRequest, content string, toolCalls int) map[string]any {
	prompt := tokenCount(req)
	completion := len(strings.Fields(content))
	if toolCalls > 0 {
		completion += toolCalls
	}
	if u := m.Respond; u != nil && u.Usage != nil {
		prompt = u.Usage.PromptTokens
		completion = u.Usage.CompletionTokens
		total := u.Usage.TotalTokens
		if total == 0 {
			total = prompt + completion
		}
		return map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      total,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": u.Usage.CachedTokens,
			},
		}
	}
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
}

// callsSnapshot is the body of GET /calls. It exists so a gate can prove an
// idempotent replay produced ZERO extra upstream calls, and so a human can see
// which rule answered what while debugging an agent run.
type callsSnapshot struct {
	Calls            int64            `json:"calls"`
	ByPath           map[string]int64 `json:"by_path"`
	ByRule           map[string]int64 `json:"by_rule"`
	ToolCallsEmitted int64            `json:"tool_calls_emitted"`
	Dropped          int64            `json:"dropped"`
	Failed           int64            `json:"failed"`
}

// callsHandler serves the counter snapshot. It is deliberately not counted
// itself: a gate polls it and must not perturb the number it is measuring.
func (s *server) callsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.calls())
}

// statsCallsHandler is the same snapshot under a path that groups with any
// future /stats/* endpoints.
func (s *server) statsCallsHandler(w http.ResponseWriter, r *http.Request) {
	s.callsHandler(w, r)
}

// calls snapshots the counters. Reading them is a plain load per counter: no
// lock is held, so a gate polling /calls cannot stall an in-flight request.
func (s *server) calls() callsSnapshot {
	return callsSnapshot{
		Calls:            s.callCount.Load(),
		ByPath:           s.byPath.snapshot(),
		ByRule:           s.byRule.snapshot(),
		ToolCallsEmitted: s.toolCallsEmitted.Load(),
		Dropped:          s.dropped.Load(),
		Failed:           s.failed.Load(),
	}
}

// countSink is a small concurrency-safe counter map. The keys are known paths
// and rule names, so it is a map guarded by a mutex rather than anything
// cleverer.
type countSink struct {
	mu sync.Mutex
	m  map[string]int64
}

func (c *countSink) add(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int64{}
	}
	c.m[key]++
}

func (c *countSink) snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

// record books one handled chat request. It is called once per chat request,
// AFTER the response body has been written, so "calls" means "upstream requests
// the mock really answered" -- which is exactly the number an idempotent replay
// must not increase.
func (s *server) record(path, rule string, toolCalls int, dropped, failed bool) {
	// An unnamed rule means "no script answered this one" (the mock ran without
	// -script, or no rule matched). Counting it under the empty string would put
	// a "" key in the /calls JSON: legal, but PowerShell 5.1's ConvertFrom-Json
	// throws on an empty property name ("the value of argument \"name\" is not
	// valid"), so every /calls reader written in PowerShell would lose the whole
	// snapshot and quietly skip its provider-call assertions. Name the built-in
	// answer instead.
	if rule == "" {
		rule = "default"
	}
	s.callCount.Add(1)
	s.byPath.add(path)
	s.byRule.add(rule)
	if toolCalls > 0 {
		s.toolCallsEmitted.Add(int64(toolCalls))
	}
	if dropped {
		s.dropped.Add(1)
	}
	if failed {
		s.failed.Add(1)
	}
}

// nextTurn reserves the turn number for one chat request. Matching is evaluated
// against the value BEFORE it advances, so when.turn == 1 means "the first chat
// request this process sees" rather than "the second".
func (s *server) nextTurn() int {
	return int(s.turn.Add(1))
}
