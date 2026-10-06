// Tests for scripted-response mode.
//
// The handlers and the parser are exercised directly rather than by starting the
// binary: a scripted mock exists to make an M6 agent run deterministic, and a
// test that shells out to a child process would trade that determinism for a
// port and a sleep. The few HTTP tests use httptest, and the one test that needs
// a real connection (drop) uses a real local listener because a hijack cannot be
// observed on a recorder.
package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testServer builds the same route table main() builds, minus the process
// lifecycle, so the tests exercise the real handlers.
func testServer(t *testing.T, sc *script) *server {
	t.Helper()
	s := &server{
		name:   "mock",
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		script: sc,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/models", s.models)
	mux.HandleFunc("/v1/models", s.models)
	mux.HandleFunc("/chat/completions", s.chat)
	mux.HandleFunc("/v1/chat/completions", s.chat)
	mux.HandleFunc("/embeddings", s.embeddings)
	mux.HandleFunc("/v1/embeddings", s.embeddings)
	mux.HandleFunc("/calls", s.callsHandler)
	mux.HandleFunc("/stats/calls", s.statsCallsHandler)
	s.mux = mux
	return s
}

// mustScript parses a script body in place of a file. Parsing is the same code
// path loadScript uses, so a broken script in a test fails the same way a broken
// script on disk does.
func mustScript(t *testing.T, body string) *script {
	t.Helper()
	sc, err := parseScript([]byte(body))
	if err != nil {
		t.Fatalf("parseScript(%s): %v", body, err)
	}
	return sc
}

// postChat sends one chat request through the handler and returns the recorder.
func postChat(t *testing.T, s *server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// get sends one GET through the handler.
func get(t *testing.T, s *server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// callSnapshotBody reads GET /calls back.
func callSnapshotBody(t *testing.T, s *server) callsSnapshot {
	t.Helper()
	rec := get(t, s, "/calls")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /calls: status = %d, want 200", rec.Code)
	}
	var got callsSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET /calls: decode %q: %v", rec.Body.String(), err)
	}
	return got
}

// completionEnvelope is the subset of the non-streaming answer the tests assert.
type completionEnvelope struct {
	Choices []struct {
		Message struct {
			Role      string          `json:"role"`
			Content   string          `json:"content"`
			ToolCalls []toolCallEnvel `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		PromptDetails    struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

type toolCallEnvel struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func decodeCompletion(t *testing.T, rec *httptest.ResponseRecorder) completionEnvelope {
	t.Helper()
	var out completionEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode completion %q: %v", rec.Body.String(), err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1 (%s)", len(out.Choices), rec.Body.String())
	}
	return out
}

// TestDefaultContentWithoutScript pins today's non-streamed answer, which every
// M0-M5 curl gate depends on.
func TestDefaultContentWithoutScript(t *testing.T) {
	s := testServer(t, nil)
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"mock-gpt","messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeCompletion(t, rec)
	if want := `mock answer to "ping"`; got.Choices[0].Message.Content != want {
		t.Fatalf("content = %q, want %q", got.Choices[0].Message.Content, want)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got.Choices[0].FinishReason)
	}
	if got.Choices[0].Message.ToolCalls != nil {
		t.Fatalf("unexpected tool_calls: %+v", got.Choices[0].Message.ToolCalls)
	}
}

// TestUnscriptedCallsAreCountedUnderDefaultName pins the rule name the /calls
// snapshot gives an answer the built-in path produced. It must not be the empty
// string: PowerShell 5.1's ConvertFrom-Json throws on an empty property name
// ("the value of argument \"name\" is not valid"), so a snapshot carrying
// `by_rule: {"":1}` costs every PowerShell gate the whole body -- and a body
// that failed to decode turns "the replay caused no provider call" into an
// assertion that silently never ran.
func TestUnscriptedCallsAreCountedUnderDefaultName(t *testing.T) {
	s := testServer(t, nil)
	postChat(t, s, "/v1/chat/completions", `{"model":"mock-gpt","messages":[{"role":"user","content":"ping"}]}`)

	rec := get(t, s, "/calls")
	raw := rec.Body.String()
	if strings.Contains(raw, `"":`) {
		t.Fatalf("GET /calls carries an empty JSON key, which PowerShell 5.1 cannot parse: %s", raw)
	}
	var got callsSnapshot
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("GET /calls: decode %q: %v", raw, err)
	}
	if got.Calls != 1 || got.ByRule["default"] != 1 {
		t.Fatalf("calls = %d by_rule = %v, want calls:1 default:1", got.Calls, got.ByRule)
	}
}

// TestScriptedDefaultContentWithoutRuleApplies proves a script with only a
// default keeps the legacy answer for unmatched traffic instead of inventing an
// empty one.
func TestScriptedDefaultContentWithoutRuleApplies(t *testing.T) {
	s := testServer(t, mustScript(t, `{"default":{"content":"fallback answer"}}`))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"mock-gpt","messages":[{"role":"user","content":"ping"}]}`)
	got := decodeCompletion(t, rec)
	if want := "fallback answer"; got.Choices[0].Message.Content != want {
		t.Fatalf("content = %q, want %q", got.Choices[0].Message.Content, want)
	}
	if snap := callSnapshotBody(t, s); snap.ByRule["default"] != 1 {
		t.Fatalf("by_rule = %v, want default:1", snap.ByRule)
	}
}

// TestLegacyDefaultWhenScriptHasNoDefault proves that a script which only
// declares rules leaves unmatched requests byte-for-byte alone.
func TestLegacyDefaultWhenScriptHasNoDefault(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"never","when":{"contains":"zzz"},"respond":{"content":"nope"}}]}`))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"mock-gpt","messages":[{"role":"user","content":"ping"}]}`)
	got := decodeCompletion(t, rec)
	if want := `mock answer to "ping"`; got.Choices[0].Message.Content != want {
		t.Fatalf("content = %q, want %q", got.Choices[0].Message.Content, want)
	}
}

const toolScript = `{
  "rules": [
    { "name": "tool-step",
      "when": { "has_tool_result": false, "tools_present": true, "contains": "weather" },
      "respond": {
        "tool_calls": [
          { "id": "call_1", "name": "get_weather", "arguments": {"city": "Beijing"} },
          { "name": "get_units", "arguments": {"system": "metric"} }
        ],
        "content": "",
        "usage": { "prompt_tokens": 11, "completion_tokens": 3 }
      } },
    { "name": "final-answer",
      "when": { "has_tool_result": true },
      "respond": { "echo_tool_result": true, "usage": { "prompt_tokens": 21, "completion_tokens": 7 } } }
  ]
}`

// TestToolsPresentEmitsToolCalls covers OpenAI-shaped tool_calls, the generated
// call_N ids, the JSON-string arguments and the tool_calls finish reason.
func TestToolsPresentEmitsToolCalls(t *testing.T) {
	s := testServer(t, mustScript(t, toolScript))
	body := `{"model":"mock-tool","tools":[{"type":"function","function":{"name":"get_weather"}}],
	           "messages":[{"role":"user","content":"What is the weather in Beijing?"}]}`
	rec := postChat(t, s, "/v1/chat/completions", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeCompletion(t, rec)
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
	calls := got.Choices[0].Message.ToolCalls
	if len(calls) != 2 {
		t.Fatalf("tool_calls = %d, want 2 (%s)", len(calls), rec.Body.String())
	}
	if calls[0].ID != "call_1" {
		t.Fatalf("calls[0].id = %q, want the script's explicit call_1", calls[0].ID)
	}
	// An omitted id must become call_2, not an empty string: the agent runtime
	// correlates a tool result with the call that asked for it.
	if calls[1].ID != "call_2" {
		t.Fatalf("calls[1].id = %q, want generated call_2", calls[1].ID)
	}
	for i, want := range []string{"get_weather", "get_units"} {
		if calls[i].Type != "function" {
			t.Fatalf("calls[%d].type = %q, want function", i, calls[i].Type)
		}
		if calls[i].Function.Name != want {
			t.Fatalf("calls[%d].function.name = %q, want %q", i, calls[i].Function.Name, want)
		}
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments %q is not a JSON object: %v", calls[0].Function.Arguments, err)
	}
	if args["city"] != "Beijing" {
		t.Fatalf("arguments = %v, want city=Beijing", args)
	}
	if got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 3 || got.Usage.TotalTokens != 14 {
		t.Fatalf("usage = %+v, want 11/3/14 (script totals for a response with no total)", got.Usage)
	}
	if snap := callSnapshotBody(t, s); snap.ToolCallsEmitted != 2 {
		t.Fatalf("tool_calls_emitted = %d, want 2", snap.ToolCallsEmitted)
	}
}

// TestToolCallIDIsStableAcrossRetries is why the id field exists: an agent
// runtime re-sends the same request after a transient failure and must be able
// to match the tool result to the identical call id.
func TestToolCallIDIsStableAcrossRetries(t *testing.T) {
	s := testServer(t, mustScript(t, toolScript))
	body := `{"model":"mock-tool","tools":[{"type":"function"}],"messages":[{"role":"user","content":"weather?"}]}`
	first := decodeCompletion(t, postChat(t, s, "/v1/chat/completions", body))
	second := decodeCompletion(t, postChat(t, s, "/v1/chat/completions", body))
	if len(first.Choices[0].Message.ToolCalls) == 0 || len(second.Choices[0].Message.ToolCalls) == 0 {
		t.Fatal("expected tool calls on both attempts")
	}
	a := first.Choices[0].Message.ToolCalls[0]
	b := second.Choices[0].Message.ToolCalls[0]
	if a.ID != b.ID || a.Function.Name != b.Function.Name || a.Function.Arguments != b.Function.Arguments {
		t.Fatalf("tool call changed across retries: %+v vs %+v", a, b)
	}
}

// TestEchoToolResult closes the loop: a request carrying a tool result gets that
// result back verbatim, which is what makes a deterministic two-turn agent run
// possible.
func TestEchoToolResult(t *testing.T) {
	s := testServer(t, mustScript(t, toolScript))
	body := `{"model":"mock-tool","messages":[
	  {"role":"user","content":"weather in Beijing?"},
	  {"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":"22C and sunny"},
	  {"role":"tool","tool_call_id":"call_2","content":"; metric"}]}`
	rec := postChat(t, s, "/v1/chat/completions", body)
	got := decodeCompletion(t, rec)
	if want := "22C and sunny; metric"; got.Choices[0].Message.Content != want {
		t.Fatalf("content = %q, want %q", got.Choices[0].Message.Content, want)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got.Choices[0].FinishReason)
	}
	if got.Choices[0].Message.ToolCalls != nil {
		t.Fatalf("unexpected tool_calls on the echo turn: %+v", got.Choices[0].Message.ToolCalls)
	}
	if got.Usage.PromptTokens != 21 || got.Usage.CompletionTokens != 7 {
		t.Fatalf("usage = %+v, want the script's 21/7", got.Usage)
	}
}

// TestTurnMatchesOnlyFirstChatRequest pins the counter semantics: turn is the
// value BEFORE the request is counted, so turn 1 is the first chat request and
// /healthz, /models and /embeddings never advance it.
func TestTurnMatchesOnlyFirstChatRequest(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[
	  {"name":"first","when":{"turn":1},"respond":{"content":"first turn"}},
	  {"name":"later","when":{"turn":2},"respond":{"content":"second turn"}}
	]}`))
	get(t, s, "/healthz")
	get(t, s, "/v1/models")
	first := decodeCompletion(t, postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"a"}]}`))
	if first.Choices[0].Message.Content != "first turn" {
		t.Fatalf("first content = %q, want \"first turn\"", first.Choices[0].Message.Content)
	}
	second := decodeCompletion(t, postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"b"}]}`))
	if second.Choices[0].Message.Content != "second turn" {
		t.Fatalf("second content = %q, want \"second turn\"", second.Choices[0].Message.Content)
	}
	// Turn 3 matches no rule, and this script has no default, so the legacy
	// answer applies.
	third := decodeCompletion(t, postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"c"}]}`))
	if want := `mock answer to "c"`; third.Choices[0].Message.Content != want {
		t.Fatalf("third content = %q, want %q", third.Choices[0].Message.Content, want)
	}
}

// TestContainsIsCaseInsensitive matches a mixed-case last user message against a
// lowercase predicate and vice versa.
func TestContainsIsCaseInsensitive(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"weather","when":{"contains":"WeAtHeR"},"respond":{"content":"matched"}}]}`))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"what is the WEATHER like"}]}`)
	if got := decodeCompletion(t, rec).Choices[0].Message.Content; got != "matched" {
		t.Fatalf("content = %q, want matched", got)
	}
	rec = postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"nothing to see"}]}`)
	if got := decodeCompletion(t, rec).Choices[0].Message.Content; got != `mock answer to "nothing to see"` {
		t.Fatalf("content = %q, want the legacy answer", got)
	}
}

// TestStatusWithErrorBody checks that a non-2xx rule writes the script's body
// verbatim and NEVER the success envelope.
func TestStatusWithErrorBody(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"boom","when":{},"respond":{
	  "status":502, "error_body":{"error":{"message":"upstream on fire","type":"mock_error"}}}}]}`))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"a"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("error body %q is not JSON: %v", rec.Body.String(), err)
	}
	if got["error"] == nil {
		t.Fatalf("error body = %v, want an error envelope", got)
	}
	if strings.Contains(rec.Body.String(), "chat.completion") {
		t.Fatalf("failure wrote a success envelope: %q", rec.Body.String())
	}
	if snap := callSnapshotBody(t, s); snap.Failed != 1 || snap.Calls != 1 {
		t.Fatalf("snapshot = %+v, want calls=1 failed=1", snap)
	}
}

// TestStatusWithoutErrorBodyUsesTheEnvelope covers the documented default body.
func TestStatusWithoutErrorBodyUsesTheEnvelope(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"boom","when":{},"respond":{"status":500}}]}`))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"a"}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if got.Error.Message != "scripted failure" || got.Error.Type != "mock_error" {
		t.Fatalf("error = %+v, want the mock_error envelope", got.Error)
	}
}

// TestFailFirstFailsOnceThenSucceeds is the transient-failure injection a gate
// uses to make the gateway retry, and it works even when the rule omits
// "status" (502 is the documented default).
func TestFailFirstFailsOnceThenSucceeds(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"flaky","when":{},"respond":{"fail_first":1,"content":"recovered"}}]}`))
	first := postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"a"}]}`)
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first status = %d, want the implicit 502", first.Code)
	}
	second := postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"a"}]}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", second.Code)
	}
	if got := decodeCompletion(t, second).Choices[0].Message.Content; got != "recovered" {
		t.Fatalf("second content = %q, want recovered", got)
	}
	// A second rule instance keeps its own count: fail_first is per rule, not
	// global, so one broken step cannot disarm the rest of the script.
	if snap := callSnapshotBody(t, s); snap.Calls != 2 || snap.Failed != 1 {
		t.Fatalf("snapshot = %+v, want calls=2 failed=1", snap)
	}
}

// TestDelayMs sleeps before the reply. The assertion is on elapsed time because
// a delay's whole observable effect is that the client waits.
func TestDelayMs(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"slow","when":{},"respond":{"delay_ms":25,"content":"late"}}]}`))
	start := time.Now()
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"a"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("elapsed = %v, want >= 20ms of delay_ms", elapsed)
	}
}

// TestDropClosesWithoutAResponse needs a real connection: a hijack has no
// meaning on a recorder, and the point of drop is that the client sees a
// transport error rather than an HTTP status.
func TestDropClosesWithoutAResponse(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"drop","when":{},"respond":{"drop":true}}]}`))
	srv := httptest.NewServer(s.mux)
	// Without DisableKeepAlives the client may pick a pooled connection and a
	// hijacked-and-closed socket can surface as a hang instead of an error.
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   5 * time.Second,
	}
	defer srv.Close()
	defer client.CloseIdleConnections()

	resp, err := client.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"a"}]}`))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("want a transport error, got status %d", resp.StatusCode)
	}
	// A closed hop must not be reported as an HTTP failure either: the gateway
	// retries a broken connection differently from a 502.
	if resp != nil {
		t.Fatalf("want no response, got %+v", resp.Status)
	}
	// The counters move in the handler's deferred record(), which runs after the
	// hijacked socket is already closed: coming back from the failed POST only
	// proves the client saw the close, not that the server has counted it yet.
	// Reading /calls once therefore tests the machine's scheduling as much as the
	// mock -- run 37414903323 failed on this line at 0.00s on Linux while thirty
	// runs of it pass here. Wait for the count; the values asserted are unchanged.
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := callSnapshotBody(t, s)
		if snap.Calls == 1 && snap.Dropped == 1 && snap.Failed == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot = %+v, want calls=1 dropped=1 failed=0 after 2s", snap)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCallsCountsChatsOnlyAndGroupsByPathAndRule is the counter a gate reads to
// prove an idempotent replay produced zero extra upstream calls.
func TestCallsCountsChatsOnlyAndGroupsByPathAndRule(t *testing.T) {
	s := testServer(t, mustScript(t, toolScript))

	// Nothing but chat requests may move any counter.
	get(t, s, "/healthz")
	get(t, s, "/models")
	get(t, s, "/v1/models")
	get(t, s, "/calls")
	get(t, s, "/stats/calls")
	if snap := callSnapshotBody(t, s); snap.Calls != 0 || len(snap.ByPath) != 0 {
		t.Fatalf("non-chat traffic was counted: %+v", snap)
	}

	postChat(t, s, "/v1/chat/completions", `{"model":"mock-tool","tools":[{"type":"function"}],"messages":[{"role":"user","content":"weather?"}]}`)
	postChat(t, s, "/chat/completions", `{"model":"mock-tool","tools":[{"type":"function"}],"messages":[{"role":"user","content":"weather?"}]}`)
	postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"plain"},{"role":"tool","content":"x"}]}`)

	snap := callSnapshotBody(t, s)
	if snap.Calls != 3 {
		t.Fatalf("calls = %d, want 3", snap.Calls)
	}
	if snap.ByPath["/v1/chat/completions"] != 2 || snap.ByPath["/chat/completions"] != 1 {
		t.Fatalf("by_path = %v, want /v1:2 /:1", snap.ByPath)
	}
	if snap.ByRule["tool-step"] != 2 || snap.ByRule["final-answer"] != 1 {
		t.Fatalf("by_rule = %v, want tool-step:2 final-answer:1", snap.ByRule)
	}
	if snap.ToolCallsEmitted != 4 {
		t.Fatalf("tool_calls_emitted = %d, want 4 (two calls on each of two turns)", snap.ToolCallsEmitted)
	}
	if snap.Dropped != 0 || snap.Failed != 0 {
		t.Fatalf("snapshot = %+v, want no drops or failures", snap)
	}
}

// TestPathPredicate checks that a rule can be pinned to one path shape, which is
// how a gate distinguishes a direct call from a gateway-forwarded one.
func TestPathPredicate(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[
	  {"name":"bare","when":{"path":"/chat/completions"},"respond":{"content":"bare path"}},
	  {"name":"v1","when":{"path":"/v1/chat/completions"},"respond":{"content":"v1 path"}}
	]}`))
	if got := decodeCompletion(t, postChat(t, s, "/chat/completions", `{"model":"m","messages":[]}`)).Choices[0].Message.Content; got != "bare path" {
		t.Fatalf("content = %q, want \"bare path\"", got)
	}
	if got := decodeCompletion(t, postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[]}`)).Choices[0].Message.Content; got != "v1 path" {
		t.Fatalf("content = %q, want \"v1 path\"", got)
	}
}

// TestStreamPredicate checks that "stream": false in a rule is honoured, which is
// only possible because the flag is a pointer rather than a bool.
func TestStreamPredicate(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[
	  {"name":"non-stream","when":{"stream":false},"respond":{"content":"non-streamed"}},
	  {"name":"stream","when":{"stream":true},"respond":{"content":"streamed"}}
	]}`))
	if got := decodeCompletion(t, postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[]}`)).Choices[0].Message.Content; got != "non-streamed" {
		t.Fatalf("non-streamed content = %q", got)
	}
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	chunks := parseSSE(t, rec.Body.String())
	if len(chunks) < 3 {
		t.Fatalf("frames = %d, want at least opening/content/finish (%q)", len(chunks), rec.Body.String())
	}
	if got := chunks[1]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"]; got != "streamed " {
		t.Fatalf("first content frame = %v, want \"streamed \"", got)
	}
}

// TestStreamedScriptedContent covers the frame-by-frame contract: an opening
// frame with empty content, one frame per word, the finish frame, the usage
// frame and the [DONE] terminator.
func TestStreamedScriptedContent(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"words","when":{},"respond":{
	  "content":"hello brave world","usage":{"prompt_tokens":5,"completion_tokens":3}}}]}`))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	raw := rec.Body.String()
	if !strings.Contains(raw, "data: ") {
		t.Fatalf("body is not SSE: %q", raw)
	}
	if !strings.HasSuffix(strings.TrimRight(raw, "\n"), "data: [DONE]") {
		t.Fatalf("stream does not end with [DONE]: %q", raw)
	}

	chunks := parseSSE(t, raw)
	if len(chunks) != 6 {
		t.Fatalf("frames = %d, want 6 (opening + 3 words + finish + usage) (%q)", len(chunks), raw)
	}
	opening := chunkDelta(t, chunks[0])
	if opening["content"] != "" {
		t.Fatalf("opening content = %v, want empty", opening["content"])
	}
	if opening["role"] != "assistant" {
		t.Fatalf("opening role = %v, want assistant", opening["role"])
	}
	for i, want := range []string{"hello ", "brave ", "world "} {
		if got := chunkDelta(t, chunks[i+1])["content"]; got != want {
			t.Fatalf("frame %d content = %v, want %q", i+1, got, want)
		}
	}
	if got := chunkChoice(t, chunks[4])["finish_reason"]; got != "stop" {
		t.Fatalf("finish_reason = %v, want stop", got)
	}
	usageFrame := chunks[5]
	if usageFrame["usage"] == nil {
		t.Fatalf("usage frame missing usage: %v", usageFrame)
	}
	usage := usageFrame["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(5) || usage["completion_tokens"] != float64(3) {
		t.Fatalf("usage = %v, want the script's 5/3", usage)
	}
}

// TestStreamedScriptedToolCalls documents the design decision: a scripted tool
// call is emitted WHOLE in the opening frame's delta, not fragmented, because
// the script declares a complete call and the non-scripted path already proves a
// proxy can reassemble fragments.
func TestStreamedScriptedToolCalls(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"call","when":{"tools_present":true},"respond":{
	  "tool_calls":[{"id":"call_1","name":"get_weather","arguments":{"city":"Beijing"}}]}}]}`))
	rec := postChat(t, s, "/v1/chat/completions",
		`{"model":"m","stream":true,"tools":[{"type":"function"}],"messages":[{"role":"user","content":"weather"}]}`)
	chunks := parseSSE(t, rec.Body.String())
	if len(chunks) < 3 {
		t.Fatalf("frames = %d, want opening/finish/usage (%q)", len(chunks), rec.Body.String())
	}
	opening := chunkDelta(t, chunks[0])
	calls, ok := opening["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("opening tool_calls = %v, want one whole call", opening["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" {
		t.Fatalf("call id = %v, want call_1", call["id"])
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("function.name = %v, want get_weather", fn["name"])
	}
	if fn["arguments"] != `{"city":"Beijing"}` {
		t.Fatalf("function.arguments = %v, want the JSON string", fn["arguments"])
	}
	if got := chunkChoice(t, chunks[1])["finish_reason"]; got != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", got)
	}
	if !strings.HasSuffix(strings.TrimRight(rec.Body.String(), "\n"), "data: [DONE]") {
		t.Fatalf("stream does not end with [DONE]: %q", rec.Body.String())
	}
}

// TestStreamedEchoToolResult checks the streamed half of the agent loop.
func TestStreamedEchoToolResult(t *testing.T) {
	s := testServer(t, mustScript(t, toolScript))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[
	  {"role":"user","content":"weather?"},
	  {"role":"tool","tool_call_id":"call_1","content":"22C sunny"}]}`)
	chunks := parseSSE(t, rec.Body.String())
	if len(chunks) != 5 {
		t.Fatalf("frames = %d, want 5 (opening + 2 words + finish, plus usage) (%q)", len(chunks), rec.Body.String())
	}
	if got := chunkDelta(t, chunks[1])["content"]; got != "22C " {
		t.Fatalf("frame 1 = %v, want \"22C \"", got)
	}
	if got := chunkDelta(t, chunks[2])["content"]; got != "sunny " {
		t.Fatalf("frame 2 = %v, want \"sunny \"", got)
	}
}

// TestOmitDoneStillHonoured proves the scripted stream keeps the truncation
// hazard the non-scripted stream exposes: the gateway must synthesise [DONE].
func TestOmitDoneStillHonoured(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"words","when":{},"respond":{"content":"hi there"}}]}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	req.Header.Set("X-Mock-Omit-Done", "1")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("stream should have omitted [DONE]: %q", rec.Body.String())
	}
}

// TestFirstMatchingRuleWins checks the documented order: rules are a list, not a
// set, and the first match short-circuits the rest.
func TestFirstMatchingRuleWins(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[
	  {"name":"first","when":{},"respond":{"content":"first"}},
	  {"name":"second","when":{},"respond":{"content":"second"}}
	]}`))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[]}`)
	if got := decodeCompletion(t, rec).Choices[0].Message.Content; got != "first" {
		t.Fatalf("content = %q, want first", got)
	}
	if snap := callSnapshotBody(t, s); snap.ByRule["first"] != 1 || snap.ByRule["second"] != 0 {
		t.Fatalf("by_rule = %v, want only first to have fired", snap.ByRule)
	}
}

// TestNoScriptKeepsStreamShape pins the non-scripted SSE shape so adding the
// scripted path cannot have changed it.
func TestNoScriptKeepsStreamShape(t *testing.T) {
	s := testServer(t, nil)
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"one two"}]}`)
	chunks := parseSSE(t, rec.Body.String())
	if len(chunks) != 8 {
		t.Fatalf("frames = %d, want 8 (opening + 5 words of %q + finish + usage) (%q)", len(chunks), `mock answer to "one two"`, rec.Body.String())
	}
	if got := chunkDelta(t, chunks[1])["content"]; got != "mock " {
		t.Fatalf("first word frame = %v, want \"mock \"", got)
	}
	if got := chunkChoice(t, chunks[6])["finish_reason"]; got != "stop" {
		t.Fatalf("finish_reason = %v, want stop", got)
	}
	if chunks[7]["usage"] == nil {
		t.Fatalf("last frame has no usage: %v", chunks[7])
	}
	if !strings.HasSuffix(strings.TrimRight(rec.Body.String(), "\n"), "data: [DONE]") {
		t.Fatalf("stream does not end with [DONE]: %q", rec.Body.String())
	}
}

// TestParseScriptRejects covers every startup rejection a broken file can hit,
// which is what keeps a misconfigured mock from serving the wrong answer.
func TestParseScriptRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json", `{`, "parse"},
		{"trailing object", `{"default":{"content":"a"}}{"default":{"content":"b"}}`, "trailing"},
		{"empty object", `{}`, "neither rules nor a default"},
		{"unknown field", `{"rulez":[]}`, "unknown field"},
		{"negative fail_first", `{"rules":[{"name":"x","when":{},"respond":{"fail_first":-1}}]}`, "fail_first"},
		{"impossible status", `{"rules":[{"name":"x","when":{},"respond":{"status":900}}]}`, "status 900"},
		// A malformed error_body is caught by the decoder itself, before any rule
		// validation runs, so the message names the JSON syntax problem.
		{"malformed error_body", `{"rules":[{"name":"x","when":{},"respond":{"error_body":{oops}}}]}`, "parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, err := parseScript([]byte(tc.body))
			if err == nil {
				t.Fatalf("parseScript(%s) = %+v, want an error", tc.body, sc)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestLoadScriptUnreadableErrors proves an unreadable path is a startup error
// rather than a mock that quietly falls back to the legacy answer.
func TestLoadScriptUnreadableErrors(t *testing.T) {
	if _, err := loadScript(t.TempDir()); err == nil {
		t.Fatal("loadScript(directory) = nil error, want a read failure")
	}
	if _, err := loadScript(t.TempDir() + "/missing.json"); err == nil {
		t.Fatal("loadScript(missing file) = nil error, want a read failure")
	}
}

// TestToolCallArgumentsDefaultToEmptyObject covers a script that declares a call
// without arguments: the wire field must still be valid JSON, because a runtime
// that json.loads it would otherwise crash on the tool step.
func TestToolCallArgumentsDefaultToEmptyObject(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"c","when":{},"respond":{"tool_calls":[{"name":"bare"}]}}]}`))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[]}`)
	calls := decodeCompletion(t, rec).Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
	if calls[0].Function.Arguments != "{}" {
		t.Fatalf("arguments = %q, want {}", calls[0].Function.Arguments)
	}
	if calls[0].ID != "call_1" {
		t.Fatalf("id = %q, want generated call_1", calls[0].ID)
	}
}

// TestUsageDefaultsWhenScriptOmitsIt proves adding -script does not silently
// change the accounting the M3 gate reconciles: a rule without "usage" keeps the
// numbers this binary has always computed.
func TestUsageDefaultsWhenScriptOmitsIt(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"plain","when":{},"respond":{"content":"alpha beta"}}]}`))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"one two"}]}`)
	env := decodeCompletion(t, rec)
	// tokenCount: 2 words + 4 per message = 6; completion: 2 words.
	if env.Usage.PromptTokens != 6 || env.Usage.CompletionTokens != 2 || env.Usage.TotalTokens != 8 {
		t.Fatalf("usage = %+v, want the legacy 6/2/8", env.Usage)
	}

	// And a rule that omits usage entirely on the echo/default path is the same.
	s2 := testServer(t, mustScript(t, `{"default":{"content":"gamma"}}`))
	env2 := decodeCompletion(t, postChat(t, s2, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"one two"}]}`))
	if env2.Usage.PromptTokens != 6 || env2.Usage.CompletionTokens != 1 || env2.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want 6/1/7", env2.Usage)
	}
}

// TestUsageScriptTotalIsFilledIn covers a script that gives prompt and
// completion but no total.
func TestScriptedUsageWithoutTotal(t *testing.T) {
	s := testServer(t, mustScript(t, `{"rules":[{"name":"u","when":{},"respond":{
	  "content":"x","usage":{"prompt_tokens":11,"completion_tokens":3}}}]}`))
	rec := postChat(t, s, "/v1/chat/completions", `{"model":"m","messages":[]}`)
	env := decodeCompletion(t, rec)
	if env.Usage.PromptTokens != 11 || env.Usage.CompletionTokens != 3 || env.Usage.TotalTokens != 14 {
		t.Fatalf("usage = %+v, want 11/3/14", env.Usage)
	}
}

// --- small helpers -------------------------------------------------------

// parseSSE decodes every "data: {...}" frame of an SSE body, stopping at the
// [DONE] sentinel. The raw bytes are asserted separately where the terminator
// itself matters.
func parseSSE(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, frame := range strings.Split(body, "\n\n") {
		line := strings.TrimSpace(frame)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("frame is not an SSE data line: %q", line)
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("frame %q is not JSON: %v", payload, err)
		}
		out = append(out, chunk)
	}
	return out
}

// chunkChoice returns choices[0] of one frame.
func chunkChoice(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("chunk has no choices: %v", chunk)
	}
	return choices[0].(map[string]any)
}

// chunkDelta returns choices[0].delta of one frame.
func chunkDelta(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	delta, ok := chunkChoice(t, chunk)["delta"].(map[string]any)
	if !ok {
		t.Fatalf("chunk has no delta: %v", chunk)
	}
	return delta
}
