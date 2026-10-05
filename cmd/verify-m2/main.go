// Command verify-m2 is InferGate's M2 acceptance test: the semantic cache,
// exercised end to end through the real gateway binary wiring.
//
// The unit tests in internal/cache and internal/gateway already prove the policy
// and the request path in isolation. This program exists to prove the things
// that are only observable when the whole server is assembled: that the cache
// is reached from HTTP at all, that its admin and metrics surfaces report what
// the store actually holds, that a hit performs NO upstream call, that the same
// policy holds over a real Redis protocol peer, and that a dead store degrades
// to a miss instead of an error.
//
// Every claim is checked against evidence rather than the gateway's opinion of
// itself: each backend counts the requests it received, so "the cache answered"
// is proved by the backend's silence; /metrics and /admin/cache are scraped for
// the counters; and the store's own /admin/breakers-style report is read.
//
// Exit code 0 means every assertion passed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/cache"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/evalset"
	"github.com/infergate/infergate/internal/logging"
	"github.com/infergate/infergate/internal/mockbackend"
	"github.com/infergate/infergate/internal/mockredis"
	"github.com/infergate/infergate/internal/server"
)

// The prompt pair is the measured pp-en-5 pair from internal/evalset (cosine
// 0.8819, above the shipped threshold of 0.86). Using a measured pair instead
// of an invented one is what makes the semantic assertion deterministic: an
// arbitrary rewrite may sit anywhere below the threshold and the test would
// then be asserting the embedder's luck rather than the cache's behaviour.
const (
	promptText     = "summarise the design document"
	paraphraseText = "please summarise the design document"
	trivialText    = "hi"
)

func main() {
	verbose := flag.Bool("v", false, "log every request the gateway serves")
	flag.Parse()

	out := os.Stdout
	fmt.Fprintln(out, "InferGate M2 end-to-end verification (semantic cache)")
	fmt.Fprintln(out, strings.Repeat("=", 72))

	c := &checker{out: out}
	env := &environment{out: out, verbose: *verbose}

	checks := []struct {
		name string
		run  func(*checker, *environment)
	}{
		{"a miss is stored and the next identical request hits", checkMissThenExactHit},
		{"a paraphrase hits semantically, and a near miss never does", checkSemanticHitAndFalseHits},
		{"tenant isolation, and where the embeddings come from", checkTenantIsolation},
		{"bypass and refresh directives", checkDirectives},
		{"policy limits: sampling, tools, trivial prompts, answer shape", checkPolicyLimits},
		{"a streamed answer is replayed frame for frame", checkStreamReplay},
		{"expiry and the per-scope entry bound", checkExpiryAndEviction},
		{"the Redis store, and a dead store degrades to a miss", checkRedisStore},
		{"admin, metrics and /stats describe the cache", checkAdminAndMetrics},
	}

	for i, chk := range checks {
		fmt.Fprintf(out, "\n[%d/%d] %s\n", i+1, len(checks), chk.name)
		chk.run(c, env)
	}

	fmt.Fprintln(out, "\n"+strings.Repeat("=", 72))
	total, failed := c.tally()
	fmt.Fprintf(out, "RESULT: %d/%d assertions passed\n", total-failed, total)
	if failed > 0 {
		fmt.Fprintf(out, "FAILED: %d assertion(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Fprintln(out, "OK: M2 semantic cache acceptance criteria met")
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type checker struct {
	out     io.Writer
	pass    int
	fail    int
	details []string
}

func (c *checker) assert(ok bool, format string, args ...any) bool {
	msg := fmt.Sprintf(format, args...)
	if ok {
		c.pass++
		fmt.Fprintf(c.out, "   PASS  %s\n", msg)
		return true
	}
	c.fail++
	c.details = append(c.details, msg)
	fmt.Fprintf(c.out, "   FAIL  %s\n", msg)
	return false
}

func (c *checker) info(format string, args ...any) {
	fmt.Fprintf(c.out, "         "+format+"\n", args...)
}
func (c *checker) tally() (int, int) { return c.pass + c.fail, c.fail }

type environment struct {
	out     io.Writer
	verbose bool
}

// stack is one running gateway, its backend, and an optional Redis protocol
// peer. A fresh stack per check is deliberate: cache contents belong to a
// gateway, so reusing a stack would let an earlier check's entries answer a
// later check's request.
type stack struct {
	url     string
	srv     *server.Server
	backend *mockbackend.Backend
	redis   *mockredis.Server
	close   func()
}

// cacheConfig returns the shipped shape of the cache section: the same policy
// configs/cache-local.yaml describes, with a shorter TTL for the expiry check.
func cacheConfig() config.CacheConfig {
	return config.CacheConfig{
		Enabled:            true,
		Store:              config.CacheStoreMemory,
		Threshold:          cache.DefaultThreshold,
		TTL:                config.Duration(15 * time.Minute),
		MaxEntriesPerScope: 64,
		MinPromptChars:     12,
		Embedding: config.EmbeddingConfig{
			Provider: config.EmbedProviderHashing,
			Dims:     512,
			Timeout:  config.Duration(5 * time.Second),
		},
		Redis: config.RedisConfig{
			Addr:         "127.0.0.1:6379",
			Prefix:       "ig:cache",
			PoolSize:     4,
			DialTimeout:  config.Duration(2 * time.Second),
			ReadTimeout:  config.Duration(2 * time.Second),
			WriteTimeout: config.Duration(2 * time.Second),
		},
	}
}

// newStack starts a gateway over one healthy mock backend, with the cache
// configured exactly as cmd/infergate would build it from a config file.
func (e *environment) newStack(c *checker, mutate func(*config.Config)) *stack {
	back := mockbackend.New(mockbackend.Options{Name: "primary"})

	cfg := config.Defaults()
	cfg.Server.UpstreamTimeout = config.Duration(30 * time.Second)
	cfg.Server.MaxBodyBytes = 8 << 20
	cfg.Log.Level = "error"
	if !e.verbose {
		cfg.Log.Level = "error"
	}
	cfg.Upstreams = []config.UpstreamConfig{{
		Name:         "primary",
		Kind:         config.KindOpenAI,
		BaseURL:      back.URL,
		Models:       []string{"/"},
		Capabilities: []string{"chat"},
		Priority:     1,
	}}
	cfg.Pricing = config.PricingConfig{
		Default: config.ModelPrice{In: 1, Out: 3},
		Models:  map[string]config.ModelPrice{"mock-gpt": {In: 1, Out: 3}},
	}
	cfg.Cache = cacheConfig()
	if mutate != nil {
		mutate(&cfg)
	}

	level := "error"
	logger, err := logging.New(io.Discard, level, "text")
	if err != nil {
		c.assert(false, "build logger: %v", err)
		return nil
	}
	srv, err := server.NewServer(&cfg, logger)
	if err != nil {
		c.assert(false, "build server: %v", err)
		return nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.assert(false, "listen: %v", err)
		return nil
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()

	st := &stack{url: "http://" + ln.Addr().String(), srv: srv, backend: back}
	st.close = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		_ = srv.Shutdown(ctx)
		_ = srv.CloseCache()
	}
	return st
}

// newRedisStack starts the gateway against an in-process RESP2 server. The
// Redis path is not a different cache: it is the same policy behind a different
// Store, which is exactly why it is asserted here through the same wire path.
func (e *environment) newRedisStack(c *checker, mutate func(*config.Config)) *stack {
	mini := mockredis.New(mockredis.Options{Addr: "127.0.0.1:0"})
	if err := mini.Start(); err != nil {
		c.assert(false, "start the in-repo RESP2 server: %v", err)
		return nil
	}
	st := e.newStack(c, func(cfg *config.Config) {
		cfg.Cache.Store = config.CacheStoreRedis
		cfg.Cache.Redis.Addr = mini.Addr()
		if mutate != nil {
			mutate(cfg)
		}
	})
	if st == nil {
		_ = mini.Close()
		return nil
	}
	st.redis = mini
	inner := st.close
	st.close = func() {
		inner()
		_ = mini.Close()
	}
	return st
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

type result struct {
	status int
	body   string
	header http.Header
}

func post(c *checker, base, path, body string, hdr map[string]string) result {
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		c.assert(false, "build request: %v", err)
		return result{}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		c.assert(false, "request %s: %v", path, err)
		return result{}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, body: string(data), header: resp.Header.Clone()}
}

func chat(c *checker, base, body string, hdr map[string]string) result {
	return post(c, base, "/v1/chat/completions", body, hdr)
}

func get(c *checker, url string) result {
	resp, err := http.Get(url)
	if err != nil {
		c.assert(false, "GET %s: %v", url, err)
		return result{}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, body: string(data), header: resp.Header.Clone()}
}

func getJSON(c *checker, url string, v any) bool {
	res := get(c, url)
	if res.status != http.StatusOK {
		c.assert(false, "GET %s: status %d", url, res.status)
		return false
	}
	if err := json.Unmarshal([]byte(res.body), v); err != nil {
		c.assert(false, "GET %s: decode: %v (body=%s)", url, err, truncate(res.body, 300))
		return false
	}
	return true
}

// chatBodyFor builds a one-message chat request. Marshalling instead of
// formatting matters for the Chinese pairs in the corpus, whose text must stay
// byte-identical between the request that stores and the request that looks up.
func chatBodyFor(c *checker, prompt string) string {
	body, err := json.Marshal(map[string]any{
		"model":    "mock-gpt",
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		c.assert(false, "marshal a chat body: %v", err)
		return ""
	}
	return string(body)
}

func cacheStatus(res result) string { return res.header.Get("X-InferGate-Cache") }
func upstreamName(res result) string {
	return res.header.Get("X-InferGate-Upstream-Name")
}

// metricValue sums a counter series whose label set contains the given regex
// fragment, in ANY label order.
//
// Matching a series by name prefix is a silent way to read zero: the exposition
// orders labels per family (infergate_tokens_total starts with upstream,
// infergate_cache_hits_total starts with kind), so a prefix that assumes one
// order matches nothing and a counter of zero looks exactly like a feature that
// did nothing.
func metricValue(text, series, labels string) float64 {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + `\{[^}]*` + labels + `[^}]*\}\s+([0-9.eE+-]+)\s*$`)
	var total float64
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			total += v
		}
	}
	return total
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------------------------------------------------------------------
// Checks
// ---------------------------------------------------------------------------

// checkMissThenExactHit is the base contract: the first request goes upstream
// and is stored, the second is answered from the store without touching it.
func checkMissThenExactHit(c *checker, e *environment) {
	st := e.newStack(c, nil)
	if st == nil {
		return
	}
	defer st.close()

	body := chatBodyFor(c, promptText)
	first := chat(c, st.url, body, nil)
	if !c.assert(first.status == http.StatusOK, "the first request is answered (%d)", first.status) {
		return
	}
	c.assert(cacheStatus(first) == "miss", "the first request reports %q (miss)", cacheStatus(first))
	c.assert(upstreamName(first) == "primary", "the first request names the backend that produced it (%q)", upstreamName(first))
	firstCalls := st.backend.Count()
	c.assert(firstCalls == 1, "the backend received the first request (calls=%d)", firstCalls)

	second := chat(c, st.url, body, nil)
	c.assert(second.status == http.StatusOK, "the repeat is answered (%d)", second.status)
	c.assert(cacheStatus(second) == "hit-exact", "the repeat reports an exact hit, got %q", cacheStatus(second))
	c.assert(upstreamName(second) == "cache", "the repeat is attributed to the cache, not to a backend (%q)", upstreamName(second))
	c.assert(second.body == first.body, "the replayed body is byte-identical to the stored answer")
	if age := second.header.Get("X-InferGate-Cache-Age"); age != "" {
		_, err := strconv.Atoi(age)
		c.assert(err == nil, "the cache age header is milliseconds (%q)", age)
	}
	c.assert(st.backend.Count() == 1, "a hit performs no upstream call (calls=%d)", st.backend.Count())

	// A hit must not be counted as provider consumption: the token families are
	// attributed per upstream, and inflating them would make the gateway claim
	// traffic that never happened.
	upstreamPrompt := metricValue(get(c, st.url+"/metrics").body, "infergate_tokens_total",
		`upstream="primary"[^}]*kind="prompt"`)
	c.assert(upstreamPrompt == 7, "the backend's prompt tokens are counted once (%.0f)", upstreamPrompt)
}

// checkSemanticHitAndFalseHits primes one wording and asks another. Both halves
// matter: a cache that never matches semantically is dead weight, and a cache
// that matches two different questions is worse than no cache at all, because
// the caller cannot tell.
func checkSemanticHitAndFalseHits(c *checker, e *environment) {
	st := e.newStack(c, nil)
	if st == nil {
		return
	}
	defer st.close()

	if !c.assert(chat(c, st.url, chatBodyFor(c, promptText), nil).status == http.StatusOK, "prime the cache with the first wording") {
		return
	}
	res := chat(c, st.url, chatBodyFor(c, paraphraseText), nil)
	c.assert(cacheStatus(res) == "hit-semantic", "a paraphrase is answered semantically, got %q", cacheStatus(res))
	c.assert(st.backend.Count() == 1, "the paraphrase did not reach the backend (calls=%d)", st.backend.Count())

	// The near-miss and unrelated pairs of the measured corpus must never be
	// answered from a stored entry: this is the false-hit bound the threshold
	// was chosen for, re-asserted on the real request path.
	var falseHits, checked int
	var examples []string
	for _, tc := range evalset.Cases() {
		if tc.ShouldHit() {
			continue
		}
		checked++
		prime := chat(c, st.url, chatBodyFor(c, tc.A), nil)
		if prime.status != http.StatusOK {
			c.assert(false, "prime %s: status %d", tc.ID, prime.status)
			continue
		}
		got := chat(c, st.url, chatBodyFor(c, tc.B), nil)
		if strings.HasPrefix(cacheStatus(got), "hit") {
			falseHits++
			if len(examples) < 3 {
				examples = append(examples, fmt.Sprintf("%s (%s)", tc.ID, cacheStatus(got)))
			}
		}
	}
	c.assert(falseHits == 0, "%d near-miss/unrelated pairs produced no wrong answer (false hits=%d %v)",
		checked, falseHits, examples)
}

// checkTenantIsolation proves the scope key is per caller: two tenants asking
// the same question must not share an answer, because the cache cannot know
// whether the answer was confidential.
func checkTenantIsolation(c *checker, e *environment) {
	st := e.newStack(c, nil)
	if st == nil {
		return
	}
	defer st.close()

	body := chatBodyFor(c, promptText)
	alpha := chat(c, st.url, body, map[string]string{"X-InferGate-Tenant": "alpha"})
	beta := chat(c, st.url, body, map[string]string{"X-InferGate-Tenant": "beta"})
	c.assert(cacheStatus(alpha) == "miss", "tenant alpha's first request misses (%q)", cacheStatus(alpha))
	c.assert(cacheStatus(beta) == "miss", "tenant beta is not served alpha's answer (%q)", cacheStatus(beta))
	c.assert(st.backend.Count() == 2, "both tenants reached the backend (calls=%d)", st.backend.Count())

	again := chat(c, st.url, body, map[string]string{"X-InferGate-Tenant": "alpha"})
	c.assert(cacheStatus(again) == "hit-exact", "tenant alpha's repeat hits (%q)", cacheStatus(again))
	c.assert(st.backend.Count() == 2, "the tenant-scoped hit performed no upstream call (calls=%d)", st.backend.Count())

	// The scope is derived, not guessed: the same derivation the gateway uses
	// must be reproducible from outside, or /admin/cache could not be used.
	scope := cache.ScopeFor("alpha", "mock-gpt")
	admin := get(c, st.url+"/admin/cache")
	var doc struct {
		Enabled bool           `json:"enabled"`
		Store   string         `json:"store"`
		Scopes  map[string]int `json:"scopes"`
		Stats   map[string]any `json:"stats"`
		Config  map[string]any `json:"config"`
	}
	if c.assert(json.Unmarshal([]byte(admin.body), &doc) == nil, "/admin/cache answers JSON") {
		c.assert(doc.Enabled, "/admin/cache reports the cache as enabled")
		c.assert(doc.Store == "memory", "/admin/cache names the store (%q)", doc.Store)
		c.assert(doc.Scopes[scope] >= 1, "the derived scope %q holds an entry (scopes=%v)", scope, doc.Scopes)
	}
}

// checkDirectives covers the two per-request escape hatches. Both still store:
// bypass exists to skip a stale answer for one call, not to poison the cache.
func checkDirectives(c *checker, e *environment) {
	st := e.newStack(c, nil)
	if st == nil {
		return
	}
	defer st.close()

	body := chatBodyFor(c, promptText)
	first := chat(c, st.url, body, nil)
	if !c.assert(cacheStatus(first) == "miss", "prime the cache (%q)", cacheStatus(first)) {
		return
	}
	c.assert(cacheStatus(chat(c, st.url, body, nil)) == "hit-exact", "the primed answer hits")

	bypass := chat(c, st.url, body, map[string]string{"X-InferGate-Cache": "bypass"})
	c.assert(cacheStatus(bypass) == "bypass", "bypass is reported (%q)", cacheStatus(bypass))
	c.assert(st.backend.Count() == 2, "bypass calls the backend (calls=%d)", st.backend.Count())

	refresh := chat(c, st.url, body, map[string]string{"X-InferGate-Cache": "refresh"})
	c.assert(cacheStatus(refresh) == "refresh", "refresh is reported (%q)", cacheStatus(refresh))
	c.assert(st.backend.Count() == 3, "refresh calls the backend (calls=%d)", st.backend.Count())

	after := chat(c, st.url, body, nil)
	c.assert(cacheStatus(after) == "hit-exact", "a bypassed/refreshed answer is still stored (%q)", cacheStatus(after))
	c.assert(st.backend.Count() == 3, "the answer stored by refresh is served without a call (calls=%d)", st.backend.Count())
}

// checkPolicyLimits asserts the rules that keep a hit from being a wrong answer.
func checkPolicyLimits(c *checker, e *environment) {
	st := e.newStack(c, nil)
	if st == nil {
		return
	}
	defer st.close()

	// (a) sampling: the same body twice is a retry of one sampling call and may
	// be replayed, but a paraphrase asked for variation and must not be.
	sampling := `{"model":"mock-gpt","temperature":0.9,"messages":[{"role":"user","content":"` + promptText + `"}]}`
	c.assert(cacheStatus(chat(c, st.url, sampling, nil)) == "miss", "a sampling request is not cached as a topic, it misses first")
	samplingPara := `{"model":"mock-gpt","temperature":0.9,"messages":[{"role":"user","content":"` + paraphraseText + `"}]}`
	c.assert(cacheStatus(chat(c, st.url, samplingPara, nil)) == "miss",
		"a paraphrase at temperature 0.9 is never answered from a stored sample")
	c.assert(cacheStatus(chat(c, st.url, sampling, nil)) == "hit-exact",
		"the identical sampling retry is answered exactly")

	// (b) tools: two paraphrases can legitimately choose different tools.
	tools := `{"model":"mock-gpt","messages":[{"role":"user","content":"` + promptText + `"}],` +
		`"tools":[{"type":"function","function":{"name":"lookup"}}]}`
	c.assert(cacheStatus(chat(c, st.url, tools, nil)) == "miss", "a tool request misses first")
	toolsPara := `{"model":"mock-gpt","messages":[{"role":"user","content":"` + paraphraseText + `"}],` +
		`"tools":[{"type":"function","function":{"name":"lookup"}}]}`
	c.assert(cacheStatus(chat(c, st.url, toolsPara, nil)) == "miss", "a paraphrase with tools is never matched semantically")

	// (c) a prompt too short to be worth embedding is skipped entirely.
	trivial := chat(c, st.url, chatBodyFor(c, trivialText), nil)
	c.assert(cacheStatus(trivial) == "skip", "a trivial prompt is skipped (%q)", cacheStatus(trivial))
	c.assert(st.backend.Count() >= 1, "a skipped request still reaches the backend")

	// (d) the answer shape is part of the request: a different max_tokens is a
	// different question even with an identical prompt.
	shapeBody := func(maxTokens int) string {
		return fmt.Sprintf(`{"model":"mock-gpt","max_tokens":%d,"messages":[{"role":"user","content":%q}]}`,
			maxTokens, promptText)
	}
	c.assert(cacheStatus(chat(c, st.url, shapeBody(900), nil)) == "miss", "a new max_tokens misses")
	c.assert(cacheStatus(chat(c, st.url, shapeBody(50), nil)) == "miss",
		"the same prompt with a different max_tokens is a different request")

	// (e) a route the cache does not answer is not decorated at all.
	models := get(c, st.url+"/v1/models")
	c.assert(models.header.Get("X-InferGate-Cache") == "", "a non-completion route carries no cache header (%q)",
		models.header.Get("X-InferGate-Cache"))
}

// checkStreamReplay proves a stored stream is replayed as frames, not as one
// blob: a client that reads incrementally must still see an increment.
func checkStreamReplay(c *checker, e *environment) {
	st := e.newStack(c, nil)
	if st == nil {
		return
	}
	defer st.close()

	streamBody := `{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"` + promptText + `"}]}`
	live := chat(c, st.url, streamBody, nil)
	if !c.assert(live.status == http.StatusOK, "the live stream is answered (%d)", live.status) {
		return
	}
	liveData := strings.Count(live.body, "data: ")

	replay := chat(c, st.url, streamBody, nil)
	c.assert(cacheStatus(replay) == "hit-exact", "the repeated stream is a cache hit (%q)", cacheStatus(replay))
	c.assert(strings.HasPrefix(replay.header.Get("Content-Type"), "text/event-stream"),
		"the replay declares an event stream (%q)", replay.header.Get("Content-Type"))
	c.assert(replay.body == live.body, "the replay is byte-identical to the live stream")
	c.assert(strings.Count(replay.body, "data: ") == liveData, "the replay has the same frame count (%d)", liveData)
	c.assert(strings.Contains(replay.body, "chat.completion.chunk"), "the replayed frames are still chat chunks")
	c.assert(strings.HasSuffix(strings.TrimSpace(replay.body), "data: [DONE]"), "the replay ends with the sentinel")
	c.assert(st.backend.Count() == 1, "the stream replay performed no upstream call (calls=%d)", st.backend.Count())
}

// checkExpiryAndEviction covers the two ways a stored answer stops being
// served: its TTL, and the per-scope bound. They are measured on separate
// stacks because a short TTL would make the bound untestable (entries expiring
// on their own would look like eviction).
func checkExpiryAndEviction(c *checker, e *environment) {
	expiring := e.newStack(c, func(cfg *config.Config) {
		cfg.Cache.TTL = config.Duration(250 * time.Millisecond)
	})
	if expiring == nil {
		return
	}
	body := chatBodyFor(c, promptText)
	c.assert(cacheStatus(chat(c, expiring.url, body, nil)) == "miss", "store an entry with a 250ms TTL")
	c.assert(cacheStatus(chat(c, expiring.url, body, nil)) == "hit-exact", "the entry is served before it expires")
	time.Sleep(400 * time.Millisecond)
	c.assert(cacheStatus(chat(c, expiring.url, body, nil)) == "miss", "the expired entry is not served")
	expiring.close()

	// The bound is 3 and the prompts are deliberately unrelated, so the fourth
	// insert must evict the oldest rather than grow the scope.
	bounded := e.newStack(c, func(cfg *config.Config) {
		cfg.Cache.MaxEntriesPerScope = 3
	})
	if bounded == nil {
		return
	}
	defer bounded.close()

	first := "explain the token bucket rate limiter algorithm"
	prompts := []string{
		first,
		"write a SQL query that finds duplicate rows",
		"describe how TCP congestion control works",
		"list the phases of the moon in order",
	}
	for _, p := range prompts {
		chat(c, bounded.url, chatBodyFor(c, p), nil)
	}
	scope := cache.ScopeFor("anonymous", "mock-gpt")
	var doc struct {
		Scopes map[string]int `json:"scopes"`
	}
	admin := get(c, bounded.url+"/admin/cache")
	if c.assert(json.Unmarshal([]byte(admin.body), &doc) == nil, "/admin/cache answers JSON after eviction") {
		c.assert(doc.Scopes[scope] == 3, "the scope holds exactly max_entries_per_scope entries (%d)", doc.Scopes[scope])
	}
	c.assert(cacheStatus(chat(c, bounded.url, chatBodyFor(c, first), nil)) == "miss",
		"the oldest entry was evicted, so it misses")
}

// checkRedisStore runs the same contract against a real RESP2 peer, then takes
// the peer away. A cache is an optimisation: losing it must cost a miss, never
// an answer.
func checkRedisStore(c *checker, e *environment) {
	st := e.newRedisStack(c, nil)
	if st == nil {
		return
	}
	defer st.close()

	admin := get(c, st.url+"/admin/cache")
	var doc struct {
		Store  string         `json:"store"`
		Scopes map[string]int `json:"scopes"`
	}
	if !c.assert(json.Unmarshal([]byte(admin.body), &doc) == nil, "/admin/cache answers JSON") {
		return
	}
	c.assert(doc.Store == "redis", "the store is reported as redis (%q)", doc.Store)

	body := chatBodyFor(c, promptText)
	c.assert(cacheStatus(chat(c, st.url, body, nil)) == "miss", "the first request through Redis misses")
	c.assert(cacheStatus(chat(c, st.url, body, nil)) == "hit-exact", "the identical request is served from Redis")
	c.assert(cacheStatus(chat(c, st.url, chatBodyFor(c, paraphraseText), nil)) == "hit-semantic",
		"a paraphrase is served semantically from Redis")
	c.assert(st.backend.Count() == 1, "both Redis hits performed no upstream call (calls=%d)", st.backend.Count())

	after := get(c, st.url+"/admin/cache")
	doc.Scopes = nil
	_ = json.Unmarshal([]byte(after.body), &doc)
	c.assert(len(doc.Scopes) >= 1, "Redis reports at least one scope (%v)", doc.Scopes)

	// Take the cache away. The next request must still be answered by the
	// backend, with the failure visible in the header rather than in the body.
	if err := st.redis.Close(); err != nil {
		c.assert(false, "close the RESP2 server: %v", err)
	}
	dead := chat(c, st.url, body, nil)
	c.assert(dead.status == http.StatusOK, "the request is still answered with a dead cache (%d)", dead.status)
	c.assert(!strings.HasPrefix(cacheStatus(dead), "hit"),
		"a dead store cannot produce a hit (%q)", cacheStatus(dead))
	c.assert(strings.Contains(dead.body, "mock answer from primary"),
		"the answer came from the backend after the cache died")
	c.assert(st.backend.Count() == 2, "the backend served the request the cache could not (calls=%d)", st.backend.Count())

	adminDead := get(c, st.url+"/admin/cache")
	c.assert(adminDead.status == http.StatusOK, "the admin surface still answers with a dead store (%d)", adminDead.status)
	metrics := get(c, st.url+"/metrics")
	c.assert(metrics.status == http.StatusOK, "/metrics still answers with a dead store (%d)", metrics.status)
	c.assert(strings.Contains(metrics.body, "infergate_cache_errors_total"),
		"/metrics reports cache errors by kind")
}

// checkAdminAndMetrics is the M2 observability contract: a semantic cache whose
// decisions cannot be inspected from outside is impossible to operate, because
// "it missed" and "it matched the wrong entry" look identical in a response.
func checkAdminAndMetrics(c *checker, e *environment) {
	st := e.newStack(c, nil)
	if st == nil {
		return
	}
	defer st.close()

	body := chatBodyFor(c, promptText)
	chat(c, st.url, body, nil)
	chat(c, st.url, body, nil)
	chat(c, st.url, chatBodyFor(c, paraphraseText), nil)

	var doc struct {
		Enabled bool `json:"enabled"`
		Config  struct {
			Threshold float64 `json:"threshold"`
			TTL       string  `json:"ttl"`
		} `json:"config"`
		Stats struct {
			Lookups          int64   `json:"lookups"`
			Hits             int64   `json:"hits"`
			ExactHits        int64   `json:"exact_hits"`
			SemanticHits     int64   `json:"semantic_hits"`
			Misses           int64   `json:"misses"`
			Stores           int64   `json:"stores"`
			SavedPromptToken int64   `json:"saved_prompt_tokens"`
			SavedCompTokens  int64   `json:"saved_completion_tokens"`
			HitRate          float64 `json:"hit_rate"`
		} `json:"stats"`
	}
	if !c.assert(getJSON(c, st.url+"/admin/cache", &doc), "/admin/cache answers JSON") {
		return
	}
	c.assert(doc.Enabled, "the cache reports itself enabled")
	c.assert(doc.Config.Threshold == cache.DefaultThreshold,
		"the effective threshold matches the shipped default (%.2f)", doc.Config.Threshold)
	c.assert(doc.Stats.Lookups == 3, "three lookups were served from the cache path (%d)", doc.Stats.Lookups)
	c.assert(doc.Stats.ExactHits == 1, "one exact hit was counted (%d)", doc.Stats.ExactHits)
	c.assert(doc.Stats.SemanticHits == 1, "one semantic hit was counted (%d)", doc.Stats.SemanticHits)
	c.assert(doc.Stats.Misses == 1, "one miss was counted (%d)", doc.Stats.Misses)
	c.assert(doc.Stats.Stores == 1, "one answer was stored (%d)", doc.Stats.Stores)
	c.assert(doc.Stats.HitRate > 0.66 && doc.Stats.HitRate < 0.67,
		"the hit rate is 2 of 3 (%.3f)", doc.Stats.HitRate)

	// The saved-token counters are the cost story: a hit replays the answer but
	// the provider was never billed for it. Both hits count -- the semantic hit
	// saves exactly as much as the exact one, which is the whole reason the
	// embedder is on the request path.
	c.assert(doc.Stats.SavedPromptToken == 14, "both hits' prompt tokens are recorded as saved (%d)", doc.Stats.SavedPromptToken)
	c.assert(doc.Stats.SavedCompTokens == 4, "both hits' completion tokens are recorded as saved (%d)", doc.Stats.SavedCompTokens)

	// /admin/cache/lookup explains a miss: it answers with the features and the
	// nearest stored entries, which is the only way to tell "no entry" from
	// "wrong entry" from outside.
	scope := cache.ScopeFor("anonymous", "mock-gpt")
	lookupURL := st.url + "/admin/cache/lookup?prompt=" + url.QueryEscape(promptText) + "&scope=" + url.QueryEscape(scope)
	var lookup struct {
		Prompt   string   `json:"prompt"`
		Features []string `json:"features"`
		Matches  []struct {
			Key        string  `json:"key"`
			Similarity float64 `json:"similarity"`
		} `json:"matches"`
	}
	if c.assert(getJSON(c, lookupURL, &lookup), "/admin/cache/lookup answers JSON") {
		c.assert(len(lookup.Features) > 0, "the lookup reports the tokens behind the similarity (%d features)", len(lookup.Features))
		c.assert(len(lookup.Matches) >= 1, "the lookup finds the stored entry (%d matches)", len(lookup.Matches))
		if len(lookup.Matches) > 0 {
			c.assert(lookup.Matches[0].Similarity >= cache.DefaultThreshold,
				"the nearest match is above the threshold (%.4f)", lookup.Matches[0].Similarity)
		}
	}

	// Flushing is a POST: letting a GET clear a shared cache would turn a link
	// prefetch or a crawler into an outage.
	badFlush := get(c, st.url+"/admin/cache/flush?scope="+url.QueryEscape(scope))
	c.assert(badFlush.status != http.StatusOK, "a GET cannot flush the cache (%d)", badFlush.status)

	flush := post(c, st.url, "/admin/cache/flush?scope="+url.QueryEscape(scope), "", nil)
	var flushDoc struct {
		Flushed int    `json:"flushed"`
		Scope   string `json:"scope"`
	}
	if c.assert(json.Unmarshal([]byte(flush.body), &flushDoc) == nil, "flush answers JSON (%d)", flush.status) {
		c.assert(flushDoc.Flushed == 1, "one entry was flushed (%d)", flushDoc.Flushed)
		c.assert(flushDoc.Scope == scope, "the flushed scope is echoed (%q)", flushDoc.Scope)
	}
	after := chat(c, st.url, body, nil)
	c.assert(cacheStatus(after) == "miss", "the flushed entry is gone, so the next request misses (%q)", cacheStatus(after))

	// /stats and /metrics must describe the cache with the same numbers.
	var stats struct {
		Cache struct {
			Enabled bool             `json:"enabled"`
			Hits    int64            `json:"hits"`
			Saved   map[string]int64 `json:"saved_tokens"`
		} `json:"cache"`
	}
	if c.assert(getJSON(c, st.url+"/stats", &stats), "/stats answers JSON") {
		c.assert(stats.Cache.Enabled, "/stats reports the cache as enabled")
		c.assert(stats.Cache.Hits == 2, "/stats agrees on the hit count (%d)", stats.Cache.Hits)
		c.assert(stats.Cache.Saved["total"] == 18, "/stats reports the saved token total (%d)", stats.Cache.Saved["total"])
	}

	metrics := get(c, st.url+"/metrics").body
	families := []string{
		"infergate_cache_lookups_total",
		"infergate_cache_hits_total",
		"infergate_cache_misses_total",
		"infergate_cache_stores_total",
		"infergate_cache_hit_ratio",
		"infergate_cache_saved_tokens_total",
		"infergate_cache_entries",
	}
	for _, fam := range families {
		c.assert(strings.Contains(metrics, fam), "/metrics exposes %s", fam)
	}
	c.assert(metricValue(metrics, "infergate_cache_hits_total", `kind="exact"`) == 1,
		"the exact-hit counter is labelled by kind")
	c.assert(metricValue(metrics, "infergate_cache_saved_tokens_total", `kind="prompt"`) == 14,
		"the saved-prompt-token counter is labelled by kind")
}
