package main

// M6 check 5: the declarative capability surface and the live probe.
//
// The subject here is the difference between what a config SAYS a backend can
// do and what the backend actually does with the request shape. The surface is
// read (GET /v1/capabilities) and then tested (POST /v1/capabilities/probe),
// and the two must agree with the backends' real behaviour.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/infergate/infergate/internal/config"
)

// capUpstream is a backend whose refusals are dialled per capability, which is
// what makes a probe's classification observable.
type capUpstream struct {
	server *httptest.Server

	mu sync.Mutex
	// reject dials a refusal by the substring that appears in the body.
	reject map[string]int
	// status, when non-zero, overrides every answer (how a provider outage is
	// injected).
	status int
	calls  []upstreamCall
}

func newCapUpstream() *capUpstream {
	u := &capUpstream{reject: map[string]int{}}
	u.server = httptest.NewServer(http.HandlerFunc(u.handle))
	return u
}

func (u *capUpstream) URL() string { return u.server.URL }

func (u *capUpstream) Close() {
	if u != nil && u.server != nil {
		u.server.Close()
	}
}

func (u *capUpstream) Count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *capUpstream) Calls() []upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamCall(nil), u.calls...)
}

func (u *capUpstream) Last() upstreamCall {
	calls := u.Calls()
	if len(calls) == 0 {
		return upstreamCall{}
	}
	return calls[len(calls)-1]
}

func (u *capUpstream) setStatus(status int) {
	u.mu.Lock()
	u.status = status
	u.mu.Unlock()
}

func (u *capUpstream) refuse(substring string, status int) {
	u.mu.Lock()
	u.reject[substring] = status
	u.mu.Unlock()
}

func (u *capUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/healthz") {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	body := string(raw)

	u.mu.Lock()
	u.calls = append(u.calls, upstreamCall{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	status := u.status
	refusal := 0
	for sub, code := range u.reject {
		if strings.Contains(body, sub) {
			refusal = code
			break
		}
	}
	u.mu.Unlock()

	if status == 0 {
		status = refusal
	}
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"capability refused","type":"probe_error","status":%d}}`, status)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/embeddings") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
		return
	}

	switch {
	case strings.Contains(body, `"tools"`):
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-probe","object":"chat.completion","model":"probed",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function",`+
			`"function":{"name":"infergate_probe","arguments":"{}"}}]},"finish_reason":"tool_calls"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	case strings.Contains(body, `"stream":true`):
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-probe","object":"chat.completion","model":"probed",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}
}

// capUpstreamEntry finds one backend in a capability report by name.
func capUpstreamEntry(c *checker, rep map[string]any, name string) map[string]any {
	for _, raw := range digList(c, rep, "upstreams") {
		entry, ok := raw.(map[string]any)
		if ok && entry["name"] == name {
			return entry
		}
	}
	c.assert(false, "the capability report lists the backend %q", name)
	return nil
}

// capModelEntry finds one model in a capability report by name.
func capModelEntry(c *checker, rep map[string]any, name string) map[string]any {
	for _, raw := range digList(c, rep, "models") {
		entry, ok := raw.(map[string]any)
		if ok && entry["model"] == name {
			return entry
		}
	}
	c.assert(false, "the capability report lists the model %q", name)
	return nil
}

func checkCapabilities(c *checker) {
	// ---------------------------------------------------------------------
	// The declared surface
	// ---------------------------------------------------------------------
	chatOnly := newCapUpstream()
	defer chatOnly.Close()
	toolsUp := newCapUpstream()
	defer toolsUp.Close()

	cfg := baseConfig(upstreamConfig("chat-only", chatOnly.URL(), "chat"), upstreamConfig("tools-up", toolsUp.URL(), "chat", "tools"))
	cfg.Upstreams[1].Priority = 2
	cfg.Models = map[string]config.ModelInfo{
		"mock-gpt": {
			ContextWindow:   128000,
			MaxOutputTokens: 4096,
			Capabilities:    []string{"chat", "tools"},
			Notes:           "the model the agent runtime is pinned to",
		},
		"mock-vision": {
			ContextWindow: 32000,
			Capabilities:  []string{"vision"},
		},
	}
	s := newStack(c, "capabilities", cfg)
	if s == nil {
		return
	}
	defer s.Close(c, "capabilities")

	rep, ok := getAny(c, s.url, "/v1/capabilities", "capability report")
	if !ok {
		return
	}
	c.equal(digNum(c, rep, "model_count"), 2, "the report counts the declared models")
	c.equal(digNum(c, rep, "upstream_count"), 2, "the report counts the backends")
	c.assert(digStr(c, rep, "generated_at") != "", "the report is timestamped")
	declared := digList(c, rep, "capabilities")
	c.assert(len(declared) > 0, "the report advertises what can be probed")
	for _, want := range []string{"chat", "tools", "vision"} {
		found := false
		for _, got := range declared {
			if got == want {
				found = true
			}
		}
		c.assert(found, "the report advertises %q (got %v)", want, declared)
	}

	gpt := capModelEntry(c, rep, "mock-gpt")
	if gpt != nil {
		c.equal(digNum(c, gpt, "context_window"), 128000, "the model reports its context window")
		c.equal(digNum(c, gpt, "max_output_tokens"), 4096, "the model reports its output bound")
		c.contains(digStr(c, gpt, "notes"), "agent runtime", "the model passes its note through")
		c.equal(digBool(c, gpt, "available"), true, "a model with a healthy backend is available")
		serving := digList(c, gpt, "upstreams")
		c.equal(len(serving), 2, "a catch-all backend serves every model")
	}
	vision := capModelEntry(c, rep, "mock-vision")
	if vision != nil {
		c.equal(digNum(c, vision, "context_window"), 32000, "a second model keeps its own window")
	}

	up := capUpstreamEntry(c, rep, "tools-up")
	if up != nil {
		c.equal(digStr(c, up, "kind"), "openai", "a backend reports its provider kind")
		c.equal(digStr(c, up, "base_url"), toolsUp.URL(), "a backend reports where it is addressed")
		c.equal(digBool(c, up, "catch_all"), true, "the catch-all backend says so")
		c.equal(digNum(c, up, "priority"), 2, "a backend reports its priority")
		c.equal(digStr(c, up, "state"), "closed", "a fresh backend is closed")
	}

	// ---------------------------------------------------------------------
	// The live probe
	// ---------------------------------------------------------------------
	two := probeBody(c, s.url, map[string]any{
		"capabilities": []string{"chat", "embeddings"},
		"model":        "mock-gpt",
		"upstreams":    []string{"tools-up"},
	})
	if two != nil {
		c.equal(digNum(c, two, "count"), 2, "two capabilities were probed")
		c.equal(digNum(c, two, "accepted"), 2, "both are accepted by a permissive backend")
		probes := digList(c, two, "probes")
		c.equal(len(probes), 2, "each probe is reported")
		byCap := map[string]map[string]any{}
		for _, raw := range probes {
			entry := raw.(map[string]any)
			byCap[fmt.Sprint(entry["capability"])] = entry
		}
		chat := byCap["chat"]
		if chat != nil {
			c.equal(chat["accepted"], true, "chat is accepted")
			c.equal(chat["outcome"], "accepted", "an accepted probe says so")
			c.equal(chat["path"], "/v1/chat/completions", "a chat probe posts to the chat path")
			c.equal(chat["model"], "mock-gpt", "the probe sends the model it was told to send")
			c.equal(chat["upstream"], "tools-up", "the probe names the backend it asked")
			c.assert(toFloat(chat["elapsed_ms"]) >= 0, "a probe reports how long it took")
		}
		emb := byCap["embeddings"]
		if emb != nil {
			c.equal(emb["path"], "/v1/embeddings", "an embeddings probe posts to the embeddings path")
		}
		c.contains(digStr(c, two, "note"), "not a statement about answer quality",
			"the answer says what an acceptance does and does not mean")
	}

	// A backend that refuses the shape tells the truth about it.
	rejecting := newCapUpstream()
	defer rejecting.Close()
	rejecting.refuse("image_url", http.StatusBadRequest)
	rejectingCfg := baseConfig(upstreamConfig("picker", rejecting.URL(), "chat"))
	rejectingCfg.Models = map[string]config.ModelInfo{"mock-gpt": {Capabilities: []string{"chat"}}}
	rs := newStack(c, "capability rejection", rejectingCfg)
	if rs == nil {
		return
	}
	defer rs.Close(c, "capability rejection")

	rejected := probeBody(c, rs.url, map[string]any{"capabilities": []string{"vision", "tools", "stream"}, "model": "mock-gpt"})
	if rejected != nil {
		c.equal(digNum(c, rejected, "accepted"), 2, "tools and stream are accepted, vision is not")
		counts, _ := rejected["outcomes"].(map[string]any)
		c.equal(counts["rejected"], float64(1), "the refusal is counted as rejected")
		c.equal(counts["accepted"], float64(2), "the acceptances are counted")
		for _, raw := range digList(c, rejected, "probes") {
			entry := raw.(map[string]any)
			if entry["capability"] == "vision" {
				c.equal(entry["accepted"], false, "a rejected probe is not accepted")
				c.equal(entry["status"], float64(http.StatusBadRequest), "the rejection carries the backend's status")
				c.equal(entry["outcome"], "rejected", "a 4xx is a rejection, not a fault")
			}
			if entry["capability"] == "stream" {
				c.equal(entry["accepted"], true, "a streamed probe that answers SSE is accepted")
			}
		}
	}

	// An upstream that answers 500 is INDETERMINATE: the backend is unhealthy,
	// which is not the same statement as "it cannot do this".
	failing := newCapUpstream()
	defer failing.Close()
	failing.setStatus(http.StatusInternalServerError)
	failCfg := baseConfig(upstreamConfig("sick", failing.URL(), "chat"))
	failCfg.Models = map[string]config.ModelInfo{"mock-gpt": {Capabilities: []string{"chat"}}}
	fs := newStack(c, "capability indeterminate", failCfg)
	if fs == nil {
		return
	}
	defer fs.Close(c, "capability indeterminate")
	sick := probeBody(c, fs.url, map[string]any{"capabilities": []string{"chat"}, "model": "mock-gpt"})
	if sick != nil {
		entry := digList(c, sick, "probes")[0].(map[string]any)
		c.equal(entry["accepted"], false, "a 500 is not an acceptance")
		c.equal(entry["outcome"], "indeterminate", "a 5xx is indistinguishable from an unhealthy provider")
		c.equal(entry["status"], float64(http.StatusInternalServerError), "the 5xx status is reported")
	}

	// An upstream that cannot be reached is its own outcome.
	deadCfg := baseConfig(upstreamConfig("dead", "http://127.0.0.1:1", "chat"))
	deadCfg.Models = map[string]config.ModelInfo{"mock-gpt": {Capabilities: []string{"chat"}}}
	deadCfg.Server.UpstreamTimeout = configDuration(2 * time.Second)
	ds := newStack(c, "capability unreachable", deadCfg)
	if ds == nil {
		return
	}
	defer ds.Close(c, "capability unreachable")
	dead := probeBody(c, ds.url, map[string]any{"capabilities": []string{"chat"}, "model": "mock-gpt"})
	if dead != nil {
		entry := digList(c, dead, "probes")[0].(map[string]any)
		c.equal(entry["outcome"], "unreachable", "a backend that refuses the connection is unreachable")
		c.assert(fmt.Sprint(entry["error"]) != "", "an unreachable probe explains itself")
	}

	// A catch-all backend cannot be probed without a model name, and says so
	// rather than inventing one.
	skipped := probeBody(c, s.url, map[string]any{"capabilities": []string{"chat"}, "upstreams": []string{"chat-only"}})
	if skipped != nil {
		c.equal(digNum(c, skipped, "count"), 1, "the backend restriction is honoured")
		entry := digList(c, skipped, "probes")[0].(map[string]any)
		c.equal(entry["outcome"], "skipped", "a backend with no concrete model name is skipped")
		c.contains(fmt.Sprint(entry["error"]), "concrete model name", "the skip explains why")
	}

	// An empty request means "test everything you can", and everything it
	// cannot test is reported as skipped rather than silently dropped.
	everything := probeBody(c, s.url, map[string]any{})
	if everything != nil {
		c.equal(digNum(c, everything, "count"), 12, "an empty probe asks about all six capabilities per backend")
		c.equal(digNum(c, everything, "accepted"), 0, "no catch-all backend can be probed by name")
		perCapability := map[string]int{}
		for _, raw := range digList(c, everything, "probes") {
			perCapability[fmt.Sprint(raw.(map[string]any)["capability"])]++
		}
		c.equal(len(perCapability), 6, "the fallback covers every capability the gateway can probe (got %v)", perCapability)
		c.equal(perCapability["vision"], 2, "every backend is probed for every capability")
	}

	// The probe is a diagnostic and does not become customer traffic: no
	// ledger record, no idempotency entry, no request metric.
	adminIdem := mustAdmin(c, s.url, "/admin/idempotency", "idempotency after probing")
	c.equal(digNum(c, adminIdem, "stored"), 0, "a probe stores nothing in the replay cache")
	adminSess := mustAdmin(c, s.url, "/admin/sessions", "ledger after probing")
	c.equal(digNum(c, adminSess, "stats", "recorded"), 0, "a probe is not attributed to a session")
	metrics := get(c, s.url, "/metrics", "metrics after probing")
	c.equal(metricValue(metrics.body, "infergate_requests_total", `upstream="tools-up"`), float64(-1),
		"a probe is not counted as a customer request")

	// An unknown capability is the caller's mistake and the message lists the
	// vocabulary.
	bogus := post(c, s.url, "/v1/capabilities/probe", marshal(map[string]any{"capabilities": []string{"telepathy"}}), nil, "unknown capability")
	c.equal(bogus.status, http.StatusBadRequest, "an unknown capability is a 400")
	c.contains(bogus.body, "unknown capability", "the 400 names the mistake")
	c.contains(bogus.body, "embeddings", "the 400 lists what can be probed")

	// ---------------------------------------------------------------------
	// The probe does not respect the breaker, and the report reflects it
	// ---------------------------------------------------------------------
	tripUp := newCapUpstream()
	defer tripUp.Close()
	tripCfg := baseConfig(upstreamConfig("tripper", tripUp.URL(), "chat"))
	tripCfg.Health.MinRequests = 1
	tripCfg.Health.FailureRatio = 0.01
	tripCfg.Health.OpenDuration = configDuration(30 * time.Second)
	tripCfg.Models = map[string]config.ModelInfo{"mock-gpt": {Capabilities: []string{"chat"}}}
	ts := newStack(c, "breaker bypass", tripCfg)
	if ts == nil {
		return
	}
	defer ts.Close(c, "breaker bypass")

	tripUp.setStatus(http.StatusInternalServerError)
	failed := post(c, ts.url, chatPath, chatBody("mock-gpt", "trip it"), nil, "a request that trips the breaker")
	c.equal(failed.status, http.StatusInternalServerError,
		"the only backend answered 500, so the provider's own status is forwarded rather than rewritten")
	states := mustAdmin(c, ts.url, "/admin/upstreams", "breaker state after the failure")
	c.equal(digStr(c, states, "breaker_states", "tripper"), "open", "the breaker opened on the failure")
	openRep := mustAdmin(c, ts.url, "/v1/capabilities", "capability report while open")
	if entry := capUpstreamEntry(c, openRep, "tripper"); entry != nil {
		c.equal(digStr(c, entry, "state"), "open", "the capability report reflects live breaker state")
	}
	model := capModelEntry(c, openRep, "mock-gpt")
	if model != nil {
		c.equal(digBool(c, model, "available"), false, "a model whose only backend is open is unavailable")
	}

	// The provider recovers, but the breaker is still open. A probe reaches the
	// backend anyway because it is a diagnostic: an operator asking "can this
	// backend do vision" must get the backend's answer, not the breaker's.
	tripUp.setStatus(0)
	before := tripUp.Count()
	recovered := probeBody(c, ts.url, map[string]any{"capabilities": []string{"chat"}, "model": "mock-gpt"})
	if recovered != nil {
		entry := digList(c, recovered, "probes")[0].(map[string]any)
		c.equal(entry["outcome"], "accepted", "a probe reaches a healthy backend through an open breaker")
		c.equal(tripUp.Count(), before+1, "the probe really left the gateway")
	}
}
