package gateway

// M2 cache acceptance, driven through the REAL proxy (router, breakers and
// accounting included) rather than against the cache package directly.
//
// The reason is the same one that shaped the M1 tests: every interesting bug in
// this layer lives in the seam. A cache that returns the right bytes but the
// wrong X-InferGate-Upstream-Name, or one that stops the breaker from seeing
// the upstream call it replaced, would pass a cache-package unit test and still
// be wrong.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/cache"
	"github.com/infergate/infergate/internal/embed"
	"github.com/infergate/infergate/internal/metrics"
)

// cachePromptBody and cacheParaphraseBody are the corpus pair pp-en-5 from
// internal/evalset, measured at 0.8819 similarity -- comfortably above the
// shipped 0.86 threshold. They are used verbatim so that a threshold change
// that breaks the semantic path is caught here with the measurement to hand.
const (
	cachePromptBody     = `{"model":"gpt-4o","messages":[{"role":"user","content":"summarise the design document"}]}`
	cacheParaphraseBody = `{"model":"gpt-4o","messages":[{"role":"user","content":"please summarise the design document"}]}`
)

// newTestCache builds an enabled cache with the shipped defaults and the
// offline lexical embedder, which is what a host with no embedding endpoint
// actually runs.
func newTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	c := cache.New(cache.Config{
		Enabled:            true,
		Threshold:          cache.DefaultThreshold,
		TTL:                15 * time.Minute,
		MaxEntriesPerScope: 64,
		MinPromptChars:     12,
		Now:                time.Now,
	}, cache.NewMemoryStore(cache.MemoryOptions{MaxEntriesPerScope: 64}),
		embed.NewHashingEmbedder(embed.DefaultHashingDims),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// mustPostWith sends a POST with extra gateway headers.
func mustPostWith(t *testing.T, handler http.Handler, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer caller-key")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return serve(t, handler, req)
}

// streamBackend answers with a minimal but well-formed SSE completion.
func streamBackend(t *testing.T, chunks ...string) *backend {
	t.Helper()
	return newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, c := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})
}

const (
	cacheStreamChunk1 = `{"id":"chatcmpl-s","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`
	cacheStreamChunk2 = `{"id":"chatcmpl-s","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`
)

// cacheHeadersOf returns the cache-related response headers, which is the
// contract a caller (and the curl script) sees.
func cacheHeadersOf(res *httptest.ResponseRecorder) (code int, cache, upstream, age string) {
	h := res.Header()
	return res.Code, h.Get(HeaderCache), h.Get(HeaderUpstreamName), h.Get(HeaderCacheAge)
}

// ---------------------------------------------------------------------------
// Exact match
// ---------------------------------------------------------------------------

// TestCacheMissThenExactHit is the M2 headline: the same request twice must
// reach the upstream once.
func TestCacheMissThenExactHit(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	first := mustPost(t, p, "/v1/chat/completions", cachePromptBody)
	if _, got, upstream, _ := cacheHeadersOf(first); got != cacheStatusMiss {
		t.Fatalf("%s = %q on the first request, want %q", HeaderCache, got, cacheStatusMiss)
	} else if upstream != "primary" {
		t.Errorf("%s = %q on a miss, want the backend that answered", HeaderUpstreamName, upstream)
	}

	second := mustPost(t, p, "/v1/chat/completions", cachePromptBody)
	_, got, upstream, _ := cacheHeadersOf(second)
	if got != cacheStatusHitExact {
		t.Errorf("%s = %q on the second identical request, want %q", HeaderCache, got, cacheStatusHitExact)
	}
	if upstream != "cache" {
		t.Errorf("%s = %q on a hit, want %q: a cached answer was not produced by a backend now", HeaderUpstreamName, upstream, "cache")
	}
	if second.Body.String() != chatCompletionBody {
		t.Errorf("replayed body = %q, want the stored completion", second.Body.String())
	}
	if n := len(primary.recorded()); n != 1 {
		t.Errorf("the upstream saw %d requests, want 1: the second was answered from cache", n)
	}
}

// TestCacheSemanticHitAcrossWording proves the embedding path is wired, not
// just the exact-key path: a paraphrase reaches the upstream once.
func TestCacheSemanticHitAcrossWording(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	if res := mustPost(t, p, "/v1/chat/completions", cachePromptBody); res.Header().Get(HeaderCache) != cacheStatusMiss {
		t.Fatalf("priming request: %s = %q, want a miss", HeaderCache, res.Header().Get(HeaderCache))
	}
	res := mustPost(t, p, "/v1/chat/completions", cacheParaphraseBody)
	if got := res.Header().Get(HeaderCache); got != cacheStatusHitSemantic {
		t.Errorf("%s = %q for a paraphrase, want %q", HeaderCache, got, cacheStatusHitSemantic)
	}
	if res.Body.String() != chatCompletionBody {
		t.Errorf("replayed body = %q, want the stored completion", res.Body.String())
	}
	if n := len(primary.recorded()); n != 1 {
		t.Errorf("the upstream saw %d requests, want 1", n)
	}
}

// TestCacheHitIsNotAttributedToAnUpstream is the accounting rule: a replay must
// not inflate the backend's token totals, because a dashboard that showed "5
// prompt tokens" for every replay would understate the saving the cache exists
// to produce. The saving is reported by the cache instead.
func TestCacheHitIsNotAttributedToAnUpstream(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	c := newTestCache(t)
	rec := metrics.NewRecorder()
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: c}, rec)

	mustPost(t, p, "/v1/chat/completions", cachePromptBody)
	promptAfterMiss, completionAfterMiss, _ := rec.TokenTotals()

	res := mustPost(t, p, "/v1/chat/completions", cachePromptBody)
	if got := res.Header().Get(HeaderCache); got != cacheStatusHitExact {
		t.Fatalf("%s = %q, want an exact hit before this can be judged", HeaderCache, got)
	}

	promptAfterHit, completionAfterHit, _ := rec.TokenTotals()
	if promptAfterHit != promptAfterMiss || completionAfterHit != completionAfterMiss {
		t.Errorf("upstream tokens changed on a cache hit: %d/%d -> %d/%d, want no change",
			promptAfterMiss, completionAfterMiss, promptAfterHit, completionAfterHit)
	}

	// chatCompletionBody reports 5 prompt and 1 completion token.
	st := c.Stats()
	if st.SavedPromptTokens != 5 || st.SavedCompletionTokens != 1 {
		t.Errorf("saved tokens = %d/%d, want 5/1 from the stored usage",
			st.SavedPromptTokens, st.SavedCompletionTokens)
	}
}

// ---------------------------------------------------------------------------
// Scope and eligibility
// ---------------------------------------------------------------------------

// TestCacheIsScopedToTenantAndModel: the same words from a different tenant (or
// a different model) are a different question, and answering them from one
// entry is the failure mode a semantic cache is most likely to ship with.
func TestCacheIsScopedToTenantAndModel(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	mustPostWith(t, p, "/v1/chat/completions", cachePromptBody, map[string]string{HeaderTenant: "alpha"})

	other := mustPostWith(t, p, "/v1/chat/completions", cachePromptBody, map[string]string{HeaderTenant: "beta"})
	if got := other.Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Errorf("a request from another tenant was answered %q, want a miss", got)
	}

	otherModel := strings.Replace(cachePromptBody, "gpt-4o", "mock-cheap", 1)
	third := mustPost(t, p, "/v1/chat/completions", otherModel)
	if got := third.Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Errorf("a request for another model was answered %q, want a miss", got)
	}

	if n := len(primary.recorded()); n != 3 {
		t.Errorf("the upstream saw %d requests, want 3 (a hit here would be cross-tenant contamination)", n)
	}
}

// TestCacheSamplingRequestsMatchExactlyOnly: temperature > 0 means the same
// prompt is expected to give different answers, so a PARAPHRASE must never be
// served -- but a byte-identical request is a retry of the same sampling call,
// and answering it from cache is the point (a client that retries a request
// should not pay twice, and should not silently get a second sample).
func TestCacheSamplingRequestsMatchExactlyOnly(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	body := `{"model":"gpt-4o","temperature":0.9,"messages":[{"role":"user","content":"write me a haiku about routers"}]}`
	first := mustPost(t, p, "/v1/chat/completions", body)
	if got := first.Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Fatalf("first sampling request: %s = %q, want %q", HeaderCache, got, cacheStatusMiss)
	}

	// A paraphrase with the same temperature must not be answered.
	para := `{"model":"gpt-4o","temperature":0.9,"messages":[{"role":"user","content":"write me a haiku about a router please"}]}`
	if got := mustPost(t, p, "/v1/chat/completions", para).Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Errorf("%s = %q for a paraphrase of a sampling request, want a miss", HeaderCache, got)
	}

	// The identical retry does hit, exactly.
	third := mustPost(t, p, "/v1/chat/completions", body)
	if got := third.Header().Get(HeaderCache); got != cacheStatusHitExact {
		t.Errorf("%s = %q for an identical retry, want %q", HeaderCache, got, cacheStatusHitExact)
	}
	if n := len(primary.recorded()); n != 2 {
		t.Errorf("the upstream saw %d requests, want 2 (the retry must not be re-sent)", n)
	}
}

// TestCacheToolsAreExactMatchOnly: two paraphrases of the same question can
// legitimately choose different tools, so a stored tool_call must only be
// replayed for the identical request.
func TestCacheToolsAreExactMatchOnly(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	toolFor := func(question string) string {
		return `{"model":"gpt-4o","messages":[{"role":"user","content":"` + question + `"}],` +
			`"tools":[{"type":"function","function":{"name":"get_weather"}}]}`
	}
	body := toolFor("what is the weather in beijing today")
	if got := mustPost(t, p, "/v1/chat/completions", body).Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Fatalf("%s = %q on the first tool request, want %q", HeaderCache, got, cacheStatusMiss)
	}
	paraphrase := toolFor("what is the weather in beijing at the moment")
	if got := mustPost(t, p, "/v1/chat/completions", paraphrase).Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Errorf("%s = %q for a paraphrase with tools, want a miss", HeaderCache, got)
	}
	third := mustPost(t, p, "/v1/chat/completions", body)
	if got := third.Header().Get(HeaderCache); got != cacheStatusHitExact {
		t.Errorf("%s = %q for the identical tool request, want %q", HeaderCache, got, cacheStatusHitExact)
	}
	if n := len(primary.recorded()); n != 2 {
		t.Errorf("the upstream saw %d requests, want 2", n)
	}
}

// TestCacheSkipsTrivialPrompts: a short prompt is cheap to answer and far more
// likely to collide with an unrelated short prompt.
func TestCacheSkipsTrivialPrompts(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	res := mustPost(t, p, "/v1/chat/completions", chatRequestBody) // "hi"
	if got := res.Header().Get(HeaderCache); got != cacheStatusSkip {
		t.Errorf("%s = %q for a two-character prompt, want %q", HeaderCache, got, cacheStatusSkip)
	}
	if n := len(primary.recorded()); n != 1 {
		t.Errorf("the upstream saw %d requests, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Directives
// ---------------------------------------------------------------------------

// TestCacheBypassSkipsTheLookupButStillStores: bypass is "give me a fresh
// answer", not "do not learn from this". An operator who had to disable the
// cache to force a fresh answer would never re-enable it.
func TestCacheBypassSkipsTheLookupButStillStores(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	bypass := mustPostWith(t, p, "/v1/chat/completions", cachePromptBody, map[string]string{HeaderCache: "bypass"})
	if got := bypass.Header().Get(HeaderCache); got != "bypass" {
		t.Fatalf("%s = %q with a bypass directive, want %q", HeaderCache, got, "bypass")
	}
	if n := len(primary.recorded()); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}

	// The bypassed answer must have been stored, so a normal follow-up hits.
	res := mustPost(t, p, "/v1/chat/completions", cachePromptBody)
	if got := res.Header().Get(HeaderCache); got != cacheStatusHitExact {
		t.Errorf("%s = %q after a bypass, want the answer it produced to have been stored", HeaderCache, got)
	}
	if n := len(primary.recorded()); n != 1 {
		t.Errorf("the upstream saw %d requests, want 1", n)
	}
}

// TestCacheRefreshReplacesTheStoredAnswer: refresh is for the case where the
// operator knows the cached answer is stale.
func TestCacheRefreshReplacesTheStoredAnswer(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	mustPost(t, p, "/v1/chat/completions", cachePromptBody) // prime
	refreshed := mustPostWith(t, p, "/v1/chat/completions", cachePromptBody, map[string]string{HeaderCache: "refresh"})
	if got := refreshed.Header().Get(HeaderCache); got != "refresh" {
		t.Errorf("%s = %q with a refresh directive, want %q", HeaderCache, got, "refresh")
	}
	if n := len(primary.recorded()); n != 2 {
		t.Errorf("the upstream saw %d requests, want 2: refresh must not be answered from cache", n)
	}

	// And the refreshed entry is what a later request gets.
	third := mustPost(t, p, "/v1/chat/completions", cachePromptBody)
	if got := third.Header().Get(HeaderCache); got != cacheStatusHitExact {
		t.Errorf("%s = %q after a refresh, want a hit", HeaderCache, got)
	}
	if n := len(primary.recorded()); n != 2 {
		t.Errorf("the upstream saw %d requests, want 2", n)
	}
}

// ---------------------------------------------------------------------------
// Hit validation
// ---------------------------------------------------------------------------

// TestCacheSignatureBlocksADifferentAnswerShape is the rule that makes semantic
// matching safe: the same words with a different max_tokens (or tool schema)
// produce a different expected answer, so the near-match must NOT be served even
// though the prompt is similar.
func TestCacheSignatureBlocksADifferentAnswerShape(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	long := `{"model":"gpt-4o","max_tokens":900,"messages":[{"role":"user","content":"summarise the design document"}]}`
	if res := mustPost(t, p, "/v1/chat/completions", long); res.Header().Get(HeaderCache) != cacheStatusMiss {
		t.Fatalf("priming request: %s = %q, want a miss", HeaderCache, res.Header().Get(HeaderCache))
	}

	// The paraphrase is above the threshold, but it asks for a different length.
	short := `{"model":"gpt-4o","max_tokens":50,"messages":[{"role":"user","content":"please summarise the design document"}]}`
	res := mustPost(t, p, "/v1/chat/completions", short)
	if got := res.Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Errorf("%s = %q for a different max_tokens, want a miss: the answer shape differs", HeaderCache, got)
	}
	if n := len(primary.recorded()); n != 2 {
		t.Errorf("the upstream saw %d requests, want 2", n)
	}
}

// TestCacheSignatureSeparatesConversationPrefixes: in an agent loop the same
// question after different tool results is a different question.
func TestCacheSignatureSeparatesConversationPrefixes(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	withHistory := func(toolResult string) string {
		return `{"model":"gpt-4o","messages":[` +
			`{"role":"user","content":"what is the deployment status of the gateway service"},` +
			`{"role":"tool","tool_call_id":"c1","content":"` + toolResult + `"},` +
			`{"role":"user","content":"summarise the design document"}]}`
	}
	mustPost(t, p, "/v1/chat/completions", withHistory("all replicas healthy"))

	res := mustPost(t, p, "/v1/chat/completions", withHistory("two replicas down in the beijing region"))
	if got := res.Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Errorf("%s = %q across different tool results, want a miss", HeaderCache, got)
	}
	if n := len(primary.recorded()); n != 2 {
		t.Errorf("the upstream saw %d requests, want 2", n)
	}
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// TestCacheReplaysStreamingAnswers: a streamed answer must be replayable
// frame-for-frame, because a client that gets a non-stream body where it
// expected frames shows an empty bubble.
func TestCacheReplaysStreamingAnswers(t *testing.T) {
	primary := streamBackend(t, cacheStreamChunk1, cacheStreamChunk2)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	stream := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"summarise the design document"}]}`
	first := mustPost(t, p, "/v1/chat/completions", stream)
	if got := first.Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Fatalf("priming stream: %s = %q, want a miss", HeaderCache, got)
	}
	if !strings.Contains(first.Body.String(), "[DONE]") {
		t.Fatalf("priming stream body = %q, want the relayed frames", first.Body.String())
	}
	wantBody := first.Body.String()

	second := mustPost(t, p, "/v1/chat/completions", stream)
	if got := second.Header().Get(HeaderCache); got != cacheStatusHitExact {
		t.Fatalf("%s = %q on the replayed stream, want %q", HeaderCache, got, cacheStatusHitExact)
	}
	if ct := second.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q on a replayed stream, want text/event-stream", ct)
	}
	if second.Body.String() != wantBody {
		t.Errorf("replayed stream body = %q, want the same frames the client saw first (%q)", second.Body.String(), wantBody)
	}
	if n := len(primary.recorded()); n != 1 {
		t.Errorf("the upstream saw %d requests, want 1", n)
	}
}

// TestCacheReplayKeepsFramePayloadsValid guards the shape a client actually
// parses: every replayed chunk must still be a chat.completion.chunk object.
func TestCacheReplayKeepsFramePayloadsValid(t *testing.T) {
	primary := streamBackend(t, cacheStreamChunk1, cacheStreamChunk2)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	stream := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"summarise the design document"}]}`
	mustPost(t, p, "/v1/chat/completions", stream)
	replay := mustPost(t, p, "/v1/chat/completions", stream)

	var payloads []string
	for _, line := range strings.Split(replay.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			payloads = append(payloads, strings.TrimPrefix(line, "data: "))
		}
	}
	if len(payloads) != 3 { // two chunks plus [DONE]
		t.Fatalf("replay carried %d data lines, want 3: %q", len(payloads), replay.Body.String())
	}
	if payloads[2] != "[DONE]" {
		t.Errorf("last data line = %q, want [DONE]", payloads[2])
	}
	for i, p := range payloads[:2] {
		var obj struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(p), &obj); err != nil {
			t.Fatalf("replayed frame %d is not JSON: %v (%q)", i, err, p)
		}
		if obj.Object != "chat.completion.chunk" {
			t.Errorf("replayed frame %d object = %q, want chat.completion.chunk", i, obj.Object)
		}
	}
}

// ---------------------------------------------------------------------------
// Degradation
// ---------------------------------------------------------------------------

// TestCacheDisabledLeavesM1Behaviour: with no cache installed, nothing about the
// M1 surface changes -- no header, and every request reaches the upstream.
func TestCacheDisabledLeavesM1Behaviour(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, chatCompletionBody)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL}, nil)

	for i := 0; i < 2; i++ {
		res := mustPost(t, p, "/v1/chat/completions", cachePromptBody)
		if got := res.Header().Get(HeaderCache); got != "" {
			t.Errorf("request %d: %s = %q with no cache configured, want no header", i+1, HeaderCache, got)
		}
	}
	if n := len(primary.recorded()); n != 2 {
		t.Errorf("the upstream saw %d requests, want 2", n)
	}
}

// TestCacheDoesNotAnswerNonCompletionRoutes: /v1/models is proxied, and a
// cached model list would be a stale catalogue served from a path nobody
// expects to be cached.
func TestCacheDoesNotAnswerNonCompletionRoutes(t *testing.T) {
	primary := jsonBackend(t, http.StatusOK, `{"object":"list","data":[]}`)
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		res := serve(t, p, req)
		if got := res.Header().Get(HeaderCache); got != "" {
			t.Errorf("request %d: %s = %q on /v1/models, want no cache header", i+1, HeaderCache, got)
		}
	}
	if n := len(primary.recorded()); n != 2 {
		t.Errorf("the upstream saw %d requests, want 2", n)
	}
}

// TestCacheStoresOnlySuccessfulAnswers: a 4xx is passed through to the caller
// and must not become the cached answer for that prompt.
func TestCacheStoresOnlySuccessfulAnswers(t *testing.T) {
	primary := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad payload","type":"invalid_request_error"}}`))
	})
	p, _ := newFailoverProxy(t, failoverOptions{primary: primary.URL, backup: primary.URL, cache: newTestCache(t)}, nil)

	first := mustPost(t, p, "/v1/chat/completions", cachePromptBody)
	if first.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the provider's 400", first.Code)
	}
	if got := first.Header().Get(HeaderCache); got != cacheStatusMiss {
		t.Errorf("%s = %q on a 400, want %q", HeaderCache, got, cacheStatusMiss)
	}
	second := mustPost(t, p, "/v1/chat/completions", cachePromptBody)
	if got := second.Header().Get(HeaderCache); got == cacheStatusHitExact {
		t.Errorf("%s = %q: an error response must never be served from cache", HeaderCache, got)
	}
	if n := len(primary.recorded()); n != 2 {
		t.Errorf("the upstream saw %d requests, want 2", n)
	}
}
