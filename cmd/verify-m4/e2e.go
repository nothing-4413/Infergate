package main

// M4 end-to-end checks.
//
// The router checks read the plan; these read the WIRE. A plan that says "the
// local tier first" is worth nothing if the proxy sends the request to the
// cloud anyway, and the only way to know which backend a request really reached
// is to have two backends that count what they were asked for and compare them
// against the X-InferGate-* headers the gateway sets.
//
// Two rules hold throughout:
//
//   - Both tiers are catch-alls in the config, so the tier policy is the only
//     thing that can decide between them (the router's first filter is model
//     eligibility, and a backend that does not serve the model is not a
//     candidate at all).
//   - A "dead" tier is a real closed port, not a mocked failure, so the failover
//     checks exercise the same transport error an outage produces.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/gateway"
	"github.com/infergate/infergate/internal/server"
)

const (
	// tierAlias is the model name every caller uses, as in
	// configs/tiered-local.yaml: one alias, two tiers behind it.
	tierAlias = "local-chat"
	// tierLocalName and tierCloudName are the upstream names the gateway's
	// headers and the recorders agree on.
	tierLocalName = "local-vllm"
	tierCloudName = "cloud-mock"

	tierPromptTokens     = 7
	tierCompletionTokens = 2
)

var (
	// tierCloudPrice is what the caller's alias costs on the paid tier.
	tierCloudPrice = config.ModelPrice{In: 1.0, Out: 3.0}
	// tierLocalFreePrice is what it costs on the box: the GPU is already paid
	// for, so the marginal cost of a locally served request is zero.
	tierLocalFreePrice = config.ModelPrice{In: 0, Out: 0}
)

// tierPriceMicroUSD is what ONE request costs at a price, in micro-USD. Every
// recorder in this file reports 7 prompt and 2 completion tokens, so at the
// cloud price the answer is exactly 7*1.0 + 2*3.0 = 13.
func tierPriceMicroUSD(price config.ModelPrice) float64 {
	return float64(tierPromptTokens)*price.In + float64(tierCompletionTokens)*price.Out
}

// ---------------------------------------------------------------------------
// The two-tier stack
// ---------------------------------------------------------------------------

type tierStackSpec struct {
	policy config.TierPolicyConfig
	// local and cloud are the recording upstreams. A nil one means the tier is
	// declared in the config but its address is a closed port: the cheapest
	// faithful way to say "this tier is down".
	local *recordingUpstream
	cloud *recordingUpstream
	// localModels and cloudModels override the catch-all. An upstream that also
	// declares a concrete model name is served under THAT name (the router's
	// modelFor picks the first non-"/" pattern), which is how the economics
	// check gives the two tiers different list prices.
	localModels []string
	cloudModels []string
	// localCaps defaults to [chat]; cloudCaps defaults to [chat, tools, vision]
	// as the shipped configs do.
	localCaps []string
	cloudCaps []string
	// localTierSpelling, when set, is written into the config verbatim so a
	// check can prove the value is normalised rather than echoed.
	localTierSpelling string
	pricing           config.PricingConfig
	health            config.HealthConfig
}

type tierStack struct {
	url     string
	cfg     *config.Config
	srv     *server.Server
	local   *recordingUpstream
	cloud   *recordingUpstream
	httpSrv *http.Server
	policy  config.TierPolicyConfig
}

func startTierStack(c *checker, e *environment, spec tierStackSpec) *tierStack {
	policy := spec.policy
	health := spec.health
	if health.MinRequests == 0 {
		health = tierGateHealth()
	}
	localModels := spec.localModels
	if len(localModels) == 0 {
		localModels = []string{"/"}
	}
	cloudModels := spec.cloudModels
	if len(cloudModels) == 0 {
		cloudModels = []string{"/"}
	}
	localCaps := spec.localCaps
	if len(localCaps) == 0 {
		localCaps = []string{"chat"}
	}
	cloudCaps := spec.cloudCaps
	if len(cloudCaps) == 0 {
		cloudCaps = []string{"chat", "tools", "vision"}
	}
	localTier := config.TierLocal
	if spec.localTierSpelling != "" {
		localTier = spec.localTierSpelling
	}

	localURL := "http://" + deadAddr(c)
	if spec.local != nil {
		localURL = spec.local.URL()
	}
	cloudURL := "http://" + deadAddr(c)
	if spec.cloud != nil {
		cloudURL = spec.cloud.URL()
	}

	cfg := config.Defaults()
	cfg.Log.Level = "error"
	cfg.Server.Listen = ":0"
	cfg.Pricing = spec.pricing
	cfg.Health = health
	cfg.Routing = config.RoutingConfig{Strategy: config.StrategyTiered, TierPolicy: policy}
	cfg.Upstreams = []config.UpstreamConfig{
		{
			Name: tierLocalName, Kind: config.KindOpenAI, BaseURL: localURL,
			Models: localModels, Capabilities: localCaps, Priority: 1, Weight: 1, Tier: localTier,
		},
		{
			Name: tierCloudName, Kind: config.KindOpenAI, BaseURL: cloudURL,
			Models: cloudModels, Capabilities: cloudCaps, Priority: 2, Weight: 1, Tier: config.TierCloud,
		},
	}
	if err := cfg.Validate(); err != nil {
		c.assert(false, "e2e: build a two-tier config (%v)", err)
		return nil
	}
	srv, err := server.NewServer(&cfg, newLogger(c))
	if err != nil {
		c.assert(false, "e2e: build the gateway (%v)", err)
		return nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if !c.assert(err == nil, "e2e: listen on a free port (%v)", err) {
		return nil
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()

	return &tierStack{
		url:     "http://" + ln.Addr().String(),
		cfg:     &cfg,
		srv:     srv,
		local:   spec.local,
		cloud:   spec.cloud,
		httpSrv: httpSrv,
		policy:  policy,
	}
}

func (st *tierStack) Close() {
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if st.httpSrv != nil {
		_ = st.httpSrv.Shutdown(ctx)
	}
	if st.srv != nil {
		_ = st.srv.Shutdown(ctx)
	}
}

// assertRoute states the two independent claims that a request was served by
// one tier: the header the gateway set, and which recorder counted the call. A
// check that only read the header would pass on a gateway that reported the
// right name and sent the request to the other backend. served is the recorder
// for the upstream named by want, other is the one it must not have reached.
func assertRoute(c *checker, label string, res result, want, wantModel string, served, other *recordingUpstream, servedCalls, otherCalls int) bool {
	ok := c.assert(res.header.Get(gateway.HeaderUpstreamName) == want,
		"%s: X-InferGate-Upstream-Name names %q (got %q)", label, want, res.header.Get(gateway.HeaderUpstreamName))
	if !c.assert(served.Count() == servedCalls,
		"%s: the %s upstream recorded %d request(s) in total, want %d", label, want, served.Count(), servedCalls) {
		ok = false
	}
	if other != nil && !c.assert(other.Count() == otherCalls,
		"%s: the other upstream recorded %d request(s) in total, want %d", label, other.Count(), otherCalls) {
		ok = false
	}
	if last, seen := served.Last(); c.assert(seen, "%s: the %s upstream recorded the call", label, want) {
		ok = c.assert(last.Model == wantModel,
			"%s: the %s upstream was asked for model %q (got %q)", label, want, wantModel, last.Model) && ok
	}
	return ok
}

// tierMessagesBody builds a chat request whose serialised `messages` array is
// exactly messagesBytes long, and asserts that it is.
//
// The router measures len(serialised messages)/4, and the gateway passes the
// client's bytes through untouched, so a byte-exact body is the only way to
// stand exactly on the tier boundary from outside the process. maxTokens is
// omitted when it is zero.
func tierMessagesBody(c *checker, model string, messagesBytes, maxTokens int) string {
	msgs := []map[string]string{{"role": "user", "content": "x"}}
	probe, err := json.Marshal(msgs)
	if !c.assert(err == nil, "harness: encode a probe message array (%v)", err) {
		return ""
	}
	if messagesBytes < len(probe) {
		c.assert(false, "harness: %d bytes is too short for a messages array (the shortest is %d)", messagesBytes, len(probe))
		return ""
	}
	msgs[0]["content"] = strings.Repeat("x", messagesBytes-len(probe)+1)
	packed, err := json.Marshal(msgs)
	if !c.assert(err == nil, "harness: encode the padded messages array (%v)", err) {
		return ""
	}
	c.assert(len(packed) == messagesBytes,
		"harness: the serialised messages array is exactly %d bytes (got %d), which the router estimates as %d prompt tokens",
		messagesBytes, len(packed), messagesBytes/4)

	payload := map[string]any{"model": model, "messages": msgs}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	body, err := json.Marshal(payload)
	if !c.assert(err == nil, "harness: encode the chat body (%v)", err) {
		return ""
	}
	return string(body)
}

func capabilityHdr(need string) map[string]string {
	return map[string]string{gateway.HeaderCapabilities: need}
}

// ---------------------------------------------------------------------------
// 12. A simple request is served by the local tier, over HTTP
// ---------------------------------------------------------------------------

func checkE2ESimple(c *checker, e *environment) {
	local := newRecorder(c, tierLocalName, tierPromptTokens, tierCompletionTokens)
	cloud := newRecorder(c, tierCloudName, tierPromptTokens, tierCompletionTokens)
	defer local.Close()
	defer cloud.Close()

	// 100 prompt and 50 completion tokens: the byte boundary is 400 and 404.
	st := startTierStack(c, e, tierStackSpec{policy: tierPolicySmall(), local: local, cloud: cloud})
	if st == nil {
		return
	}
	defer st.Close()

	// Three ordinary requests. The count assertion is what makes this a check on
	// the gateway rather than on one lucky request: the cloud's counter must
	// stay at zero while the local one climbs.
	for i := 1; i <= 3; i++ {
		res := chat(c, st.url, chatBody(c, tierAlias, fmt.Sprintf("simple prompt number %d", i)), nil)
		if !c.assertLoop(res.status == http.StatusOK, "e2e: simple request %d answers 200 (got %d: %s)", i, res.status, truncate(res.body, 240)) {
			break
		}
		if assertRoute(c, fmt.Sprintf("a simple request (%d)", i), res, tierLocalName, tierAlias, local, cloud, i, 0) {
			c.assertLoop(strings.Contains(res.body, "served by "+tierLocalName),
				"e2e: simple request %d's body is the local upstream's (%s)", i, truncate(res.body, 200))
			c.assertLoop(res.header.Get(gateway.HeaderAttempt) == "1",
				"e2e: simple request %d is served on the first attempt (got %q)", i, res.header.Get(gateway.HeaderAttempt))
		}
	}

	// The boundary, from the outside: 400 bytes is exactly 100 estimated prompt
	// tokens, which is exactly the limit, and the comparison is strictly `>`.
	// 404 bytes is the first byte count that is over it.
	onLimit := chat(c, st.url, tierMessagesBody(c, tierAlias, 400, 0), nil)
	if c.assert(onLimit.status == http.StatusOK, "e2e: a request exactly on the prompt limit answers 200 (got %d)", onLimit.status) {
		assertRoute(c, "a request exactly on the prompt limit", onLimit, tierLocalName, tierAlias, local, cloud, 4, 0)
	}
	// 403 bytes is still 100 tokens after integer division: the boundary is a
	// byte count, and a check written against a token grid would miss it.
	truncated := chat(c, st.url, tierMessagesBody(c, tierAlias, 403, 0), nil)
	if c.assert(truncated.status == http.StatusOK, "e2e: 403 bytes answers 200 (got %d)", truncated.status) {
		assertRoute(c, "403 serialised bytes (100 tokens after truncation)", truncated, tierLocalName, tierAlias, local, cloud, 5, 0)
	}
	// One byte more is a hard request, and a hard request belongs to the cloud.
	overLimit := chat(c, st.url, tierMessagesBody(c, tierAlias, 404, 0), nil)
	if c.assert(overLimit.status == http.StatusOK, "e2e: 404 bytes answers 200 (got %d)", overLimit.status) {
		assertRoute(c, "404 serialised bytes (101 tokens)", overLimit, tierCloudName, tierAlias, cloud, local, 1, 5)
	}

	// A request with no messages at all is the shortest simple request there is.
	noMessages := chat(c, st.url, `{"model":"`+tierAlias+`"}`, nil)
	if c.assert(noMessages.status == http.StatusOK, "e2e: a request with no messages answers 200 (got %d: %s)", noMessages.status, truncate(noMessages.body, 240)) {
		assertRoute(c, "a request with no messages", noMessages, tierLocalName, tierAlias, local, cloud, 6, 1)
	}

	// The ceiling reaches the backend unchanged, so a locally served request is
	// not silently capped by the gate.
	withCeiling := chat(c, st.url, chatBodyWithCeiling(c, tierAlias, "how long is this", 50), nil)
	if c.assert(withCeiling.status == http.StatusOK, "e2e: a ceiling exactly on the completion limit answers 200 (got %d)", withCeiling.status) {
		if assertRoute(c, "a ceiling exactly on the completion limit", withCeiling, tierLocalName, tierAlias, local, cloud, 7, 1) {
			if last, ok := local.Last(); c.assert(ok, "e2e: the local upstream recorded the call") {
				c.assert(last.MaxTokens == 50,
					"e2e: the local upstream was told about the caller's max_tokens ceiling (got %d, want 50)", last.MaxTokens)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 13. A hard request is served by the cloud tier, over HTTP
// ---------------------------------------------------------------------------

func checkE2EHard(c *checker, e *environment) {
	local := newRecorder(c, tierLocalName, tierPromptTokens, tierCompletionTokens)
	cloud := newRecorder(c, tierCloudName, tierPromptTokens, tierCompletionTokens)
	defer local.Close()
	defer cloud.Close()

	st := startTierStack(c, e, tierStackSpec{policy: tierPolicySmall(), local: local, cloud: cloud})
	if st == nil {
		return
	}
	defer st.Close()

	cloudCalls := 0

	// Hard because the completion ceiling is over the limit (51 > 50).
	res := chat(c, st.url, chatBodyWithCeiling(c, tierAlias, "write me an essay", 51), nil)
	if c.assert(res.status == http.StatusOK, "e2e: an over-ceiling request answers 200 (got %d: %s)", res.status, truncate(res.body, 240)) {
		cloudCalls++
		if assertRoute(c, "a request over the completion limit", res, tierCloudName, tierAlias, cloud, local, cloudCalls, 0) {
			c.assert(strings.Contains(res.body, "served by "+tierCloudName),
				"e2e: the body is the cloud upstream's answer (%s)", truncate(res.body, 200))
			if last, ok := cloud.Last(); c.assert(ok, "e2e: the cloud upstream recorded the call") {
				c.assert(last.MaxTokens == 51,
					"e2e: the cloud upstream received the caller's ceiling (got %d, want 51)", last.MaxTokens)
			}
		}
	}
	// Hard because the ceiling is far over it, and there is no prompt at all.
	res = chat(c, st.url, chatBodyWithCeiling(c, tierAlias, "hi", 4096), nil)
	if c.assert(res.status == http.StatusOK, "e2e: a long ceiling answers 200 (got %d)", res.status) {
		cloudCalls++
		assertRoute(c, "a request with a 4096-token ceiling", res, tierCloudName, tierAlias, cloud, local, cloudCalls, 0)
	}
	// Hard because the prompt is over the limit, with no ceiling at all.
	res = chat(c, st.url, tierMessagesBody(c, tierAlias, 404, 0), nil)
	if c.assert(res.status == http.StatusOK, "e2e: an over-limit prompt answers 200 (got %d)", res.status) {
		cloudCalls++
		assertRoute(c, "a request whose prompt is over the limit", res, tierCloudName, tierAlias, cloud, local, cloudCalls, 0)
	}

	// Hard because the request needs a capability the local tier does not
	// declare. The local upstream must not merely be last in the plan: it must
	// not be in the plan, or a cloud hiccup would hand a tool call to a model
	// that cannot serve it.
	for _, need := range []string{"tools", "vision", "tools,vision"} {
		res = chat(c, st.url, chatBody(c, tierAlias, "call a tool"), capabilityHdr(need))
		if !c.assertLoop(res.status == http.StatusOK, "e2e: a request needing %q answers 200 (got %d: %s)", need, res.status, truncate(res.body, 240)) {
			continue
		}
		cloudCalls++
		assertRoute(c, fmt.Sprintf("a request needing %q", need), res, tierCloudName, tierAlias, cloud, local, cloudCalls, 0)
	}

	// A capability the caller asks for that NO tier declares is a routing
	// failure. It must be a 4xx naming the model, not a request quietly served
	// by a backend that cannot do it.
	res = chat(c, st.url, chatBody(c, tierAlias, "embed this"), capabilityHdr("embeddings"))
	if c.assert(res.status == http.StatusBadRequest,
		"e2e: a request needing a capability no upstream declares answers 400 (got %d: %s)", res.status, truncate(res.body, 240)) {
		c.assert(strings.Contains(res.body, tierAlias),
			"e2e: the 400 names the model nobody could serve (%s)", truncate(res.body, 200))
	}

	// A capability BOTH tiers declare does not make the request simple: the
	// policy names tools as a cloud capability, so the request is hard and the
	// cloud leads even though the local tier could have served it. Eligibility
	// and classification are separate rules, and this is the case that shows a
	// talkative local tier does not steal cloud work.
	twinLocal := newRecorder(c, tierLocalName, tierPromptTokens, tierCompletionTokens)
	defer twinLocal.Close()
	twinCloud := newRecorder(c, tierCloudName, tierPromptTokens, tierCompletionTokens)
	defer twinCloud.Close()
	twin := startTierStack(c, e, tierStackSpec{
		policy:    tierPolicySmall(),
		local:     twinLocal,
		cloud:     twinCloud,
		localCaps: []string{"chat", "tools", "vision"},
	})
	if twin != nil {
		defer twin.Close()
		res = chat(c, twin.url, chatBody(c, tierAlias, "who can call tools"), capabilityHdr("tools"))
		if c.assert(res.status == http.StatusOK, "e2e: a tools request both tiers declare answers 200 (got %d)", res.status) {
			assertRoute(c, "a tools request both tiers can serve", res, tierCloudName, tierAlias, twinCloud, twinLocal, 1, 0)
		}
		// ...and a capability only the cloud declares still goes to the cloud,
		// even with the local tier's declaration sitting right there.
		res = chat(c, twin.url, chatBody(c, tierAlias, "embed this"), capabilityHdr("embeddings"))
		c.assert(res.status == http.StatusBadRequest,
			"e2e: an undeclared capability is still a 400 with a talkative local tier (got %d)", res.status)
	}
}

// ---------------------------------------------------------------------------
// 14. Failover crosses the tier boundary in both directions
// ---------------------------------------------------------------------------

func checkE2EFailover(c *checker, e *environment) {
	// A. The cloud is DOWN and the request is HARD. Tiering sent hard requests
	// to the cloud, so this request would be a 502 if the decision had dropped
	// the local tier from the plan instead of putting it behind the cloud.
	local := newRecorder(c, tierLocalName, tierPromptTokens, tierCompletionTokens)
	defer local.Close()
	deadCloud := startTierStack(c, e, tierStackSpec{policy: tierPolicySmall(), local: local, cloud: nil})
	if deadCloud != nil {
		res := chat(c, deadCloud.url, tierMessagesBody(c, tierAlias, 404, 0), nil)
		if c.assert(res.status == http.StatusOK,
			"e2e: a hard request survives a dead cloud tier (got %d: %s)", res.status, truncate(res.body, 240)) {
			if assertRoute(c, "a hard request with the cloud tier down", res, tierLocalName, tierAlias, local, nil, 1, 0) {
				c.assert(res.header.Get(gateway.HeaderAttempt) == "2",
					"e2e: the dead cloud tier was tried first and the local tier was the second attempt (got attempt %q)",
					res.header.Get(gateway.HeaderAttempt))
				c.assert(res.header.Get(gateway.HeaderTried) == tierCloudName+", "+tierLocalName,
					"e2e: the gateway reports the failover chain it walked (got %q)",
					res.header.Get(gateway.HeaderTried))
			}
		}
		deadCloud.Close()
	}

	// B. The local tier is DOWN and the request is SIMPLE. The mirror image: the
	// request the tier policy sent to the local tier must still be served.
	cloud := newRecorder(c, tierCloudName, tierPromptTokens, tierCompletionTokens)
	defer cloud.Close()
	deadLocal := startTierStack(c, e, tierStackSpec{policy: tierPolicySmall(), local: nil, cloud: cloud})
	if deadLocal == nil {
		return
	}
	defer deadLocal.Close()

	res := chat(c, deadLocal.url, chatBody(c, tierAlias, "a simple question"), nil)
	if c.assert(res.status == http.StatusOK,
		"e2e: a simple request survives a dead local tier (got %d: %s)", res.status, truncate(res.body, 240)) {
		if assertRoute(c, "a simple request with the local tier down", res, tierCloudName, tierAlias, cloud, nil, 1, 0) {
			c.assert(res.header.Get(gateway.HeaderAttempt) == "2",
				"e2e: the dead local tier was tried first (got attempt %q)", res.header.Get(gateway.HeaderAttempt))
			c.assert(res.header.Get(gateway.HeaderTried) == tierLocalName+", "+tierCloudName,
				"e2e: the gateway reports the failover chain it walked (got %q)", res.header.Get(gateway.HeaderTried))
		}
	}

	// The breaker is what stops the gateway from paying that failed connection
	// on every subsequent request. It needs a minimum of recorded failures
	// before it can open, so this drives the same path a few times.
	for i := 0; i < 3; i++ {
		_ = chat(c, deadLocal.url, chatBody(c, tierAlias, fmt.Sprintf("simple question %d", i)), nil)
	}
	if !c.assert(deadLocal.srv.Breakers().Get(tierLocalName).State() == breaker.StateOpen,
		"e2e: the local tier's breaker opens once its failures are recorded (state %s)",
		deadLocal.srv.Breakers().Get(tierLocalName).State()) {
		return
	}
	if !c.assert(deadLocal.srv.Breakers().Get(tierCloudName).State() == breaker.StateClosed,
		"e2e: the cloud tier's breaker is still closed (state %s)",
		deadLocal.srv.Breakers().Get(tierCloudName).State()) {
		return
	}

	// With the breaker open, the tier is skipped rather than dialled: the first
	// attempt ALREADY succeeds on the cloud. Without the health rule, every
	// simple request would pay a failed connection to a backend the gateway
	// knows is down.
	skipped := chat(c, deadLocal.url, chatBody(c, tierAlias, "after the breaker opened"), nil)
	if c.assert(skipped.status == http.StatusOK,
		"e2e: a request after the breaker opened answers 200 (got %d)", skipped.status) {
		c.assert(skipped.header.Get(gateway.HeaderUpstreamName) == tierCloudName,
			"e2e: the request was served by the cloud tier (got %q)", skipped.header.Get(gateway.HeaderUpstreamName))
		c.assert(skipped.header.Get(gateway.HeaderAttempt) == "1",
			"e2e: the open tier was skipped, so the cloud tier was the FIRST attempt (got %q)",
			skipped.header.Get(gateway.HeaderAttempt))
		c.assert(skipped.header.Get(gateway.HeaderTried) == "",
			"e2e: no failover chain was walked, because the open tier was never dialled (got %q)",
			skipped.header.Get(gateway.HeaderTried))
	}

	// The admin reset re-arms the tier, and the very next request dials it
	// again -- the observable difference between "skipped" and "forgotten".
	// (The tier is still a closed port, so the request still ends up on the
	// cloud, but it pays the failed connection again.)
	// /admin/breakers/reset is POST-only (server.go route table), so this uses
	// post rather than getJSON.
	resetRes := post(c, deadLocal.url, "/admin/breakers/reset?upstream="+tierLocalName, "", nil)
	if c.assert(resetRes.status == http.StatusOK,
		"e2e: POST /admin/breakers/reset?upstream=%s answers 200 (got %d: %s)",
		tierLocalName, resetRes.status, truncate(resetRes.body, 200)) {
		var reset map[string]any
		if c.assert(json.Unmarshal([]byte(resetRes.body), &reset) == nil,
			"e2e: the admin reset answers JSON (%s)", truncate(resetRes.body, 200)) {
			c.assert(fmt.Sprintf("%v", reset["reset"]) == tierLocalName,
				"e2e: the admin reset reports the upstream it reset (got %v)", reset)
		}
	}
	c.assert(deadLocal.srv.Breakers().Get(tierLocalName).State() == breaker.StateClosed,
		"e2e: after the admin reset the local breaker is closed again (state %s)",
		deadLocal.srv.Breakers().Get(tierLocalName).State())
	rearmed := chat(c, deadLocal.url, chatBody(c, tierAlias, "after the reset"), nil)
	if c.assert(rearmed.status == http.StatusOK, "e2e: a request after the reset answers 200 (got %d)", rearmed.status) {
		c.assert(rearmed.header.Get(gateway.HeaderTried) == tierLocalName+", "+tierCloudName,
			"e2e: the reset tier is dialled first again (got %q)", rearmed.header.Get(gateway.HeaderTried))
	}

	// The breaker state is visible on /admin/upstreams too, which is where an
	// operator would look for this.
	var admin struct {
		BreakerStates map[string]string `json:"breaker_states"`
	}
	if getJSON(c, deadLocal.url+"/admin/upstreams", &admin) {
		c.assert(admin.BreakerStates[tierLocalName] == "closed" && admin.BreakerStates[tierCloudName] == "closed",
			"e2e: /admin/upstreams reports both tiers closed after the reset (got %v)", admin.BreakerStates)
	}

	// C. Both tiers down: the gateway must fail, not answer 200 from the last
	// upstream it tried to dial.
	both := startTierStack(c, e, tierStackSpec{policy: tierPolicySmall()})
	if both != nil {
		defer both.Close()
		res := chat(c, both.url, tierMessagesBody(c, tierAlias, 404, 0), nil)
		c.assert(res.status >= 500, "e2e: a hard request with both tiers down fails (got %d)", res.status)
		c.assert(strings.Contains(res.body, "error"),
			"e2e: the failure is an error envelope, not a truncated success (%s)", truncate(res.body, 240))
	}
}

// ---------------------------------------------------------------------------
// 15. The tier split is where the money is saved
// ---------------------------------------------------------------------------

type tierStats struct {
	Requests int64 `json:"requests"`
	Tokens   struct {
		Prompt     int64 `json:"prompt"`
		Completion int64 `json:"completion"`
	} `json:"tokens"`
	Series []struct {
		Upstream string `json:"upstream"`
		Model    string `json:"model"`
		Status   int    `json:"status"`
		Outcome  string `json:"outcome"`
		Count    int64  `json:"count"`
	} `json:"series"`
}

// tierStatsCounts reads /stats and splits the served requests by tier. This is
// the surface the measurement script reads, and it is the one the gate must
// derive the saving from: the gateway has no savings counter of its own.
func tierStatsCounts(c *checker, base string) (local, cloud, total int64, ok bool) {
	var st tierStats
	if !getJSON(c, base+"/stats", &st) {
		return 0, 0, 0, false
	}
	for _, row := range st.Series {
		switch row.Upstream {
		case tierLocalName:
			local += row.Count
		case tierCloudName:
			cloud += row.Count
		}
	}
	return local, cloud, st.Requests, true
}

func checkTierEconomics(c *checker, e *environment) {
	local := newRecorder(c, tierLocalName, tierPromptTokens, tierCompletionTokens)
	cloud := newRecorder(c, tierCloudName, tierPromptTokens, tierCompletionTokens)
	defer local.Close()
	defer cloud.Close()

	// The two tiers answer to the same alias but are SERVED under different
	// model names: the cloud under the alias, the local box under its own model
	// name (the router's modelFor picks the first non-"/" pattern, and the proxy
	// rewrites the body to it). That is what gives them different list prices --
	// the local model is free, the cloud one is not.
	pricing := config.PricingConfig{
		Default: tierCloudPrice,
		Models: map[string]config.ModelPrice{
			tierAlias:   tierCloudPrice,
			"box-llama": tierLocalFreePrice,
		},
	}
	st := startTierStack(c, e, tierStackSpec{
		policy:      tierPolicySmall(),
		local:       local,
		cloud:       cloud,
		localModels: []string{"box-llama", "/"},
		cloudModels: []string{tierAlias, "/"},
		pricing:     pricing,
	})
	if st == nil {
		return
	}
	defer st.Close()

	// A mixed workload: six chat turns that fit on the box, two that do not.
	const simpleCount, hardCount = 6, 2
	for i := 0; i < simpleCount; i++ {
		res := chat(c, st.url, chatBody(c, tierAlias, fmt.Sprintf("chat turn %d", i)), nil)
		if !c.assertLoop(res.status == http.StatusOK && res.header.Get(gateway.HeaderUpstreamName) == tierLocalName,
			"economics: local request %d was served by %s (status %d, header %q)",
			i, tierLocalName, res.status, res.header.Get(gateway.HeaderUpstreamName)) {
			return
		}
	}
	for i := 0; i < hardCount; i++ {
		res := chat(c, st.url, chatBodyWithCeiling(c, tierAlias, "an essay, please", 4096), nil)
		if !c.assertLoop(res.status == http.StatusOK && res.header.Get(gateway.HeaderUpstreamName) == tierCloudName,
			"economics: hard request %d was served by %s (status %d, header %q)",
			i, tierCloudName, res.status, res.header.Get(gateway.HeaderUpstreamName)) {
			return
		}
	}

	// The billing mechanism, first: the two tiers were asked for two different
	// model names, which is what makes two different prices apply.
	for i, call := range local.Calls() {
		c.assertLoop(call.Model == "box-llama",
			"economics: local call %d was billed under the local model name %q (got %q)", i, "box-llama", call.Model)
	}
	for i, call := range cloud.Calls() {
		c.assertLoop(call.Model == tierAlias,
			"economics: cloud call %d was billed under the caller's alias %q (got %q)", i, tierAlias, call.Model)
	}

	// Then the counts, from /stats -- the gateway's own accounting, not the
	// recorders'.
	localCalls, cloudCalls, totalCalls, ok := tierStatsCounts(c, st.url)
	if !ok {
		return
	}
	c.assert(localCalls == simpleCount,
		"economics: /stats attributes %d request(s) to the local tier, want %d", localCalls, simpleCount)
	c.assert(cloudCalls == hardCount,
		"economics: /stats attributes %d request(s) to the cloud tier, want %d", cloudCalls, hardCount)
	c.assert(totalCalls == simpleCount+hardCount,
		"economics: /stats counts %d request(s) in total, want %d", totalCalls, simpleCount+hardCount)
	c.assert(localCalls == int64(local.Count()) && cloudCalls == int64(cloud.Count()),
		"economics: /stats agrees with what the upstreams recorded (stats %d/%d, recorders %d/%d)",
		localCalls, cloudCalls, local.Count(), cloud.Count())

	// The same counts, through the Prometheus surface, so the number an
	// operator scrapes and the number the gate reads cannot disagree.
	page := get(c, st.url+"/metrics")
	if c.assert(page.status == http.StatusOK, "economics: /metrics answers 200 (got %d)", page.status) {
		c.assert(metricSum(page.body, "infergate_requests_total", `upstream="`+tierLocalName+`"`) == float64(localCalls),
			"economics: infergate_requests_total for the local tier is %d (got %g)",
			localCalls, metricSum(page.body, "infergate_requests_total", `upstream="`+tierLocalName+`"`))
		c.assert(metricSum(page.body, "infergate_requests_total", `upstream="`+tierCloudName+`"`) == float64(cloudCalls),
			"economics: infergate_requests_total for the cloud tier is %d (got %g)",
			cloudCalls, metricSum(page.body, "infergate_requests_total", `upstream="`+tierCloudName+`"`))
	}

	// The arithmetic. The gateway prices a response with its own price book, so
	// the gate derives the saving from the same book and the same /stats counts
	// rather than inventing a second formula.
	book := gateway.NewPriceBook(st.cfg.Pricing)
	localPrice, localKnown := book.Price("box-llama")
	cloudPrice, cloudKnown := book.Price(tierAlias)
	c.assert(localKnown && localPrice.In == 0 && localPrice.Out == 0,
		"economics: the local model's list price is zero (got %v, known=%t)", localPrice, localKnown)
	c.assert(cloudKnown && cloudPrice.In == tierCloudPrice.In && cloudPrice.Out == tierCloudPrice.Out,
		"economics: the cloud model's list price is %v (got %v, known=%t)", tierCloudPrice, cloudPrice, cloudKnown)

	perCallMicro := tierPriceMicroUSD(cloudPrice)
	c.assert(math.Abs(perCallMicro-13) < 1e-9,
		"economics: one request costs %d prompt x %.2f + %d completion x %.2f = %.0f micro-USD at the cloud price",
		tierPromptTokens, cloudPrice.In, tierCompletionTokens, cloudPrice.Out, perCallMicro)

	savedMicro := float64(localCalls) * perCallMicro
	spentMicro := float64(cloudCalls) * perCallMicro
	allCloudMicro := float64(totalCalls) * perCallMicro
	c.assert(savedMicro > 0,
		"economics: serving %d of %d request(s) locally avoided %.0f micro-USD (%.5f USD) of cloud spend",
		localCalls, totalCalls, savedMicro, savedMicro/1e6)
	c.assert(math.Abs(spentMicro+savedMicro-allCloudMicro) < 1e-6,
		"economics: the arithmetic balances -- %d cloud call(s) x %.0f + %d local call(s) x %.0f = %d call(s) x %.0f = %.0f micro-USD",
		cloudCalls, perCallMicro, localCalls, perCallMicro, totalCalls, perCallMicro, allCloudMicro)
	c.assert(math.Abs(book.CostUSD("box-llama", tierPromptTokens, tierCompletionTokens)) < 1e-12,
		"economics: the gateway's own price book charges nothing for a locally served response (got %g USD)",
		book.CostUSD("box-llama", tierPromptTokens, tierCompletionTokens))

	// The whole workload priced at the cloud rate, and the part of it that was
	// actually spent, computed through the gateway's own CostUSD over the
	// aggregate token counts.
	actualUSD := book.CostUSD(tierAlias, int(cloudCalls)*tierPromptTokens, int(cloudCalls)*tierCompletionTokens)
	c.assert(math.Abs(actualUSD-perCallMicro*float64(cloudCalls)/1e6) < 1e-12,
		"economics: the spend was %d cloud request(s) x %.0f micro-USD = %.5f USD (gateway CostUSD says %.5f USD)",
		cloudCalls, perCallMicro, spentMicro/1e6, actualUSD)

	// The same figure the config header quotes: cloud spend avoided per 1000
	// requests at this traffic mix.
	per1000 := 1000 * (float64(localCalls) / float64(totalCalls)) * perCallMicro
	c.assert(math.Abs(per1000-9750) < 1e-6,
		"economics: at %d%% local, 1000 requests avoid %.0f micro-USD = %.5f USD of cloud spend",
		100*localCalls/totalCalls, per1000, per1000/1e6)
	c.info("this figure is derived in the gate from /stats counts and the configured prices: the gateway has no savings counter, and the brief forbids adding product code for one")
}

// ---------------------------------------------------------------------------
// 16. /admin/upstreams reports the tier
// ---------------------------------------------------------------------------

func checkAdminTiers(c *checker, e *environment) {
	local := newRecorder(c, tierLocalName, tierPromptTokens, tierCompletionTokens)
	cloud := newRecorder(c, tierCloudName, tierPromptTokens, tierCompletionTokens)
	defer local.Close()
	defer cloud.Close()

	st := startTierStack(c, e, tierStackSpec{policy: tierPolicyUnderTest(), local: local, cloud: cloud})
	if st == nil {
		return
	}
	defer st.Close()

	type adminUpstream struct {
		Name         string   `json:"name"`
		Kind         string   `json:"kind"`
		BaseURL      string   `json:"base_url"`
		Models       []string `json:"models"`
		CatchAll     bool     `json:"catch_all"`
		HasKey       bool     `json:"has_api_key"`
		Capabilities []string `json:"capabilities"`
		Priority     int      `json:"priority"`
		Weight       float64  `json:"weight"`
		Tier         string   `json:"tier"`
	}
	type adminUpstreams struct {
		Upstreams     []adminUpstream   `json:"upstreams"`
		Routing       map[string]any    `json:"routing"`
		BreakerStates map[string]string `json:"breaker_states"`
	}

	res := get(c, st.url+"/admin/upstreams")
	if !c.assert(res.status == http.StatusOK, "admin: /admin/upstreams answers 200 (got %d)", res.status) {
		return
	}
	var doc adminUpstreams
	if !c.assert(json.Unmarshal([]byte(res.body), &doc) == nil, "admin: the payload decodes (%s)", truncate(res.body, 240)) {
		return
	}
	if !c.assert(len(doc.Upstreams) == 2, "admin: both upstreams are listed (got %d)", len(doc.Upstreams)) {
		return
	}

	byName := map[string]adminUpstream{}
	for _, up := range doc.Upstreams {
		byName[up.Name] = up
	}
	if c.assert(byName[tierLocalName].Name == tierLocalName && byName[tierCloudName].Name == tierCloudName,
		"admin: the listed upstreams are %q and %q (got %v)", tierLocalName, tierCloudName, doc.Upstreams) {
		c.assert(byName[tierLocalName].Tier == config.TierLocal,
			"admin: %s is reported on tier %q (got %q)", tierLocalName, config.TierLocal, byName[tierLocalName].Tier)
		c.assert(byName[tierCloudName].Tier == config.TierCloud,
			"admin: %s is reported on tier %q (got %q)", tierCloudName, config.TierCloud, byName[tierCloudName].Tier)
	}

	// The field is a real member of the JSON object, not an absent key that
	// decodes to the zero string: an operator reading the raw payload has to see
	// it, and a `tier,omitempty` would hide the cloud tier.
	var raw struct {
		Upstreams []map[string]any `json:"upstreams"`
	}
	if c.assert(json.Unmarshal([]byte(res.body), &raw) == nil, "admin: the payload decodes as raw objects") {
		if c.assert(len(raw.Upstreams) == 2, "admin: two raw upstream objects (got %d)", len(raw.Upstreams)) {
			for i, obj := range raw.Upstreams {
				if !c.assertLoop(hasKey(obj, "tier"), "admin: raw upstream %d carries a %q key", i, "tier") {
					continue
				}
				c.assertLoop(fmt.Sprintf("%v", obj["tier"]) == config.TierLocal || fmt.Sprintf("%v", obj["tier"]) == config.TierCloud,
					"admin: raw upstream %d's tier is a tier name (got %v)", i, obj["tier"])
			}
		}
	}

	// The field was inserted without disturbing the rest of the object: the
	// existing keys are still there, in their original order, with tier last.
	order := []string{"name", "kind", "base_url", "models", "catch_all", "has_api_key", "capabilities", "priority", "weight", "tier"}
	start := strings.Index(res.body, `"upstreams"`)
	if c.assert(start >= 0, "admin: the payload has an upstreams member") {
		tail := res.body[start:]
		prev := -1
		for _, key := range order {
			idx := strings.Index(tail, `"`+key+`"`)
			if !c.assertLoop(idx >= 0, "admin: the upstream object still has its %q key", key) {
				prev = -1
				continue
			}
			if prev >= 0 {
				c.assertLoop(idx > prev, "admin: %q keeps its position, after the previous field (offset %d vs %d)", key, idx, prev)
			}
			prev = idx
		}
	}

	// The keys that were already there are unchanged for the consumers that
	// read them, and the surrounding payload still has its routing and breaker
	// members.
	up := byName[tierLocalName]
	c.assert(up.Kind == config.KindOpenAI, "admin: %s still reports its kind %q (got %q)", tierLocalName, config.KindOpenAI, up.Kind)
	c.assert(up.BaseURL == local.URL(), "admin: %s still reports its base URL (%q)", tierLocalName, up.BaseURL)
	c.assert(len(up.Models) == 1 && up.Models[0] == "/",
		"admin: %s still reports its catch-all model pattern (got %v)", tierLocalName, up.Models)
	c.assert(up.CatchAll, "admin: %s is still flagged as a catch-all", tierLocalName)
	c.assert(!up.HasKey, "admin: %s still reports that it has no API key", tierLocalName)
	c.assert(len(up.Capabilities) == 1 && up.Capabilities[0] == "chat",
		"admin: %s still reports its capabilities (got %v)", tierLocalName, up.Capabilities)
	c.assert(up.Priority == 1 && up.Weight == 1, "admin: %s still reports priority and weight (%d, %g)", tierLocalName, up.Priority, up.Weight)
	c.assert(fmt.Sprintf("%v", doc.Routing["strategy"]) == config.StrategyTiered,
		"admin: the routing member still reports the strategy (got %v)", doc.Routing["strategy"])
	c.assert(len(doc.BreakerStates) == 2,
		"admin: the breaker member still reports both upstreams (got %v)", doc.BreakerStates)

	// A tier spelled in another case in the config is reported normalised: the
	// payload describes the routing that is in force, not the file that was
	// read.
	loudLocal := newRecorder(c, tierLocalName, tierPromptTokens, tierCompletionTokens)
	defer loudLocal.Close()
	loudCloud := newRecorder(c, tierCloudName, tierPromptTokens, tierCompletionTokens)
	defer loudCloud.Close()
	loud := startTierStack(c, e, tierStackSpec{
		policy:            tierPolicySmall(),
		local:             loudLocal,
		cloud:             loudCloud,
		localTierSpelling: "  LOCAL  ",
	})
	if loud != nil {
		defer loud.Close()
		doc := adminUpstreams{}
		if getJSON(c, loud.url+"/admin/upstreams", &doc) {
			found := false
			for _, up := range doc.Upstreams {
				if up.Name == tierLocalName {
					found = true
					c.assert(up.Tier == config.TierLocal,
						"admin: a tier spelled %q in the config is reported as %q (got %q)", "  LOCAL  ", config.TierLocal, up.Tier)
				}
			}
			c.assert(found, "admin: the loudly spelled local upstream is listed")
		}
	}
}
