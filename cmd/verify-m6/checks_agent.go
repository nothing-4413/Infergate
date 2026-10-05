package main

// M6 check 6: an agent's tool-calling conversation, end to end.
//
// This is the shape Warden's FunctionCallAgent produces: turn one sends tools and
// receives a tool call, turn two sends the tool's result back and receives the
// final answer. The check is that the gateway is transparent to that
// conversation (the tool call arrives intact, the tool result travels out and
// the answer comes back), that the conversation is accounted as one session, and
// that a retried turn is replayed rather than re-billed.

import (
	"net/http"
	"strings"
	"time"
)

func checkAgentConversation(c *checker) {
	up := newScriptedUpstream()
	defer up.Close()
	cfg := baseConfig(upstreamConfig("agent", up.URL(), "chat", "tools"))
	s := newStack(c, "agent conversation", cfg)
	if s == nil {
		return
	}
	defer s.Close(c, "agent conversation")

	headers := map[string]string{
		"X-InferGate-Tenant":  "team-a",
		"X-InferGate-Session": "agent-1",
	}

	// ---- Turn 1: the caller offers a tool, the model asks for it. ----
	turn1 := post(c, s.url, chatPath, toolsBody("mock-gpt", "what is the weather in Beijing"), headers, "agent turn 1 (tools offered)")
	c.equal(turn1.status, http.StatusOK, "the tool-calling turn succeeds")
	c.equal(turn1.headerValue("X-InferGate-Upstream-Name"), "agent", "the turn names the backend that served it")
	turn1ID := turn1.headerValue("X-InferGate-Request-Id")
	c.assert(turn1ID != "", "the turn carries a request id")
	c.contains(turn1.body, "call_abc123", "the tool call id reaches the caller")
	c.contains(turn1.body, "get_weather", "the tool name reaches the caller")
	c.contains(turn1.body, `"finish_reason":"tool_calls"`, "the caller is told why the model stopped")
	up1 := up.Last()
	c.contains(up1.Body, "get_weather", "the tool schema was forwarded to the model")
	c.contains(up1.Body, `"required":["city"]`, "the tool schema was forwarded unrewritten")
	c.equal(up1.Path, "/v1/chat/completions", "the turn went to the chat path")

	// ---- Turn 2: the tool result goes back, the model answers. ----
	const toolResult = "22C and sunny"
	turn2 := post(c, s.url, chatPath, toolResultBody("mock-gpt", "call_abc123", toolResult), headers, "agent turn 2 (tool result)")
	c.equal(turn2.status, http.StatusOK, "the final turn succeeds")
	c.contains(turn2.body, "the tool said: "+toolResult,
		"the answer proves the tool result reached the model and came back")
	turn2ID := turn2.headerValue("X-InferGate-Request-Id")
	c.assert(turn2ID != turn1ID, "the second turn is its own request")
	up2 := up.Last()
	c.contains(up2.Body, toolResult, "the tool result was forwarded, not dropped")
	c.contains(up2.Body, "call_abc123", "the tool call id was forwarded with the result")
	c.contains(up2.Body, `"role":"tool"`, "the tool message kept its role")
	c.contains(up2.Body, "what is the weather in Beijing", "the whole conversation travelled, not just the last message")

	// ---- The conversation is one session. ----
	session := mustAdmin(c, s.url, "/admin/sessions/agent-1?tenant=team-a", "the agent's session")
	c.equal(digStr(c, session, "tenant"), "team-a", "the session is owned by the tenant that ran it")
	c.equal(digNum(c, session, "requests"), 2, "both turns are in the conversation")
	c.equal(digNum(c, session, "ok"), 2, "both turns succeeded")
	c.equal(digNum(c, session, "prompt_tokens"), float64(11+21), "the session sums the turns' prompt tokens")
	c.equal(digNum(c, session, "completion_tokens"), float64(5+7), "the session sums the turns' completion tokens")
	cost := toFloat(session["cost_usd"])
	c.assert(closeTo(cost, 136e-6, 1e-9),
		"the conversation cost (32 prompt * 2 + 12 completion * 6)/1e6 USD (got %v)", cost)
	models, _ := session["models"].(map[string]any)
	if totals, ok := models["mock-gpt"].(map[string]any); ok {
		c.equal(totals["requests"], float64(2), "both turns are attributed to the served model")
		c.equal(totals["cost_usd"], cost, "the model rollup and the session agree on cost")
	} else {
		c.assert(false, "the session rolls the conversation up by served model")
	}
	recent := digList(c, session, "recent")
	c.equal(len(recent), 2, "the session keeps its recent turns")
	if len(recent) == 2 {
		c.equal(recent[0].(map[string]any)["request_id"], turn1ID, "the first recent turn is the first request")
		c.equal(recent[1].(map[string]any)["request_id"], turn2ID, "the second recent turn is the second request")
	}

	// ---- A retried turn is replayed, not re-billed. ----
	retryHeaders := map[string]string{
		"X-InferGate-Tenant":  "team-a",
		"X-InferGate-Session": "agent-1",
		"Idempotency-Key":     "agent-1-turn-2",
	}
	body := toolResultBody("mock-gpt", "call_abc123", toolResult)
	first := post(c, s.url, chatPath, body, retryHeaders, "the retryable turn (first attempt)")
	c.equal(first.status, http.StatusOK, "the first attempt succeeds")
	c.equal(first.headerValue("X-InferGate-Idempotent-Replay"), "false", "the first attempt is not a replay")
	before := up.Count()

	replay := post(c, s.url, chatPath, body, retryHeaders, "the retryable turn (retried)")
	c.equal(replay.status, http.StatusOK, "the retry answers 200")
	c.equal(replay.body, first.body, "the retry gets the recorded answer byte for byte")
	c.equal(replay.headerValue("X-InferGate-Idempotent-Replay"), "true", "the retry is marked as a replay")
	c.equal(replay.headerValue("X-InferGate-Idempotent-Origin"), first.headerValue("X-InferGate-Request-Id"),
		"the replay names the request that did the work")
	c.equal(replay.headerValue("X-InferGate-Upstream-Name"), "replay", "the replay says it did not call a provider")
	c.equal(up.Count(), before, "the retry did not reach the model")
	c.contains(replay.body, "the tool said: "+toolResult, "the replayed answer is the agent's answer")

	// The same key with a DIFFERENT tool result is a mistake, not a replay: the
	// caller changed the question and must not be handed the old answer.
	changed := post(c, s.url, chatPath, toolResultBody("mock-gpt", "call_abc123", "5C and snow"), retryHeaders,
		"a retried turn whose tool result changed")
	c.equal(changed.status, http.StatusConflict, "a reused key with a different body is a conflict")
	c.contains(changed.body, "infergate_idempotency_conflict", "the conflict names its type")
	c.equal(up.Count(), before, "a conflict does not reach the model")

	// The replay is accounted, and it cost nothing.
	after := mustAdmin(c, s.url, "/admin/sessions/agent-1?tenant=team-a", "the agent's session after a retry")
	c.equal(digNum(c, after, "requests"), 5, "every turn of the exchange is attributed to the session")
	c.equal(digNum(c, after, "idempotent_replays"), 1, "the replayed turn is counted as a replay")
	c.equal(digNum(c, after, "prompt_tokens"), float64(11+21+21), "a replay adds no tokens")
	c.equal(digNum(c, after, "completion_tokens"), float64(5+7+7), "a replay adds no completion tokens")
	c.equal(digNum(c, after, "failed"), 1, "the conflict is recorded as a failed turn")
	c.equal(digNum(c, after, "ok"), 4, "the four turns that produced an answer count as served")

	// ---- The trace says the same thing the headers did. ----
	trace := findTrace(c, s.url, turn2ID, "the final turn")
	if trace != nil {
		span := rootSpan(c, trace, "the final turn")
		if span != nil {
			c.assert(attrEquals(span, "session", "agent-1"), "the span carries the conversation id")
			c.assert(attrEquals(span, "tenant", "team-a"), "the span carries the tenant")
			c.assert(attrEquals(span, "model", "mock-gpt"), "the span carries the served model")
			c.assert(attrEquals(span, "upstream", "agent"), "the span carries the backend")
			c.assert(attrEquals(span, "prompt_tokens", 21), "the span carries the prompt usage")
			c.assert(attrEquals(span, "completion_tokens", 7), "the span carries the completion usage")
		}
	}
	replayTrace := findTrace(c, s.url, replay.headerValue("X-InferGate-Request-Id"), "the replayed turn")
	if replayTrace != nil {
		span := rootSpan(c, replayTrace, "the replayed turn")
		if span != nil {
			c.assert(attrEquals(span, "idempotency", "replay"), "the replay's span records the decision")
			c.assert(attrEquals(span, "idempotency_key", "agent-1-turn-2"), "the replay's span records the key")
			c.assert(hasEvent(span, "idempotency.replay"), "the replay's span records the event")
			c.assert(attrEquals(span, "status", 200), "the replay's span records the status")
		}
	}

	// ---- The conversation works streamed, which is how an agent UI runs. ----
	streamReq := post(c, s.url, chatPath, streamChatBody("mock-gpt", "and tomorrow"), headers, "a streamed agent turn")
	c.equal(streamReq.status, http.StatusOK, "the streamed turn succeeds")
	c.contains(streamReq.body, "data: [DONE]", "the stream is terminated the way the agent runtime expects")
	c.assert(metricValue(get(c, s.url, "/metrics", "stream metrics").body, "infergate_stream_frames_total", `upstream="agent"`) >= 4,
		"every streamed frame was counted")

	// ---- A session that the caller never named is still accounted. ----
	anon := post(c, s.url, chatPath, chatBody("mock-gpt", "who am I"), nil, "an unattributed turn")
	c.equal(anon.status, http.StatusOK, "an unattributed turn still works")
	anonTrace := findTrace(c, s.url, anon.headerValue("X-InferGate-Request-Id"), "an unattributed turn")
	if anonTrace != nil {
		span := rootSpan(c, anonTrace, "an unattributed turn")
		if span != nil {
			_, hasSession := spanAttr(span, "session")
			c.assert(!hasSession, "an unattributed turn has no session attribute")
			c.assert(attrEquals(span, "tenant", "anonymous"), "an unattributed turn is attributed to nobody")
		}
	}
	admin := mustAdmin(c, s.url, "/admin/sessions", "the ledger after an unattributed turn")
	c.equal(digNum(c, admin, "stats", "no_session_id"), 1, "the unattributed turn is counted, not invented into a session")
	c.equal(digNum(c, admin, "tracked"), 1, "only the named conversation is a session")

	// The whole conversation stayed inside one tenant: a second tenant's view
	// does not contain it.
	other := get(c, s.url, "/admin/sessions/agent-1?tenant=team-b", "another tenant's view of the conversation")
	c.equal(other.status, http.StatusNotFound, "a conversation is invisible to another tenant")

	// Helpers used above are also exercised for their failure mode: an unknown
	// trace id is a 404 rather than an empty trace.
	missing := get(c, s.url, "/admin/traces/trace-does-not-exist", "an unknown trace id")
	c.equal(missing.status, http.StatusNotFound, "an unknown trace id is a 404")

	// A little slack for the clock: the spans above are ordered by time and a
	// same-millisecond pair must not invert.
	time.Sleep(2 * time.Millisecond)
	c.assert(strings.Contains(first.body, "the tool said"), "the retried turn's answer is the agent's answer, not a template")
}
