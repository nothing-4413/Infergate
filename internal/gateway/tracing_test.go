package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/router"
	"github.com/infergate/infergate/internal/tracing"
	"github.com/infergate/infergate/internal/upstream"
)

// ---------------------------------------------------------------------------
// Tracing harness
// ---------------------------------------------------------------------------

// tracedProxy builds a proxy whose tracer writes into store (never into a file
// or the network, so a test cannot be slowed down by an exporter).
func tracedProxy(t *testing.T, cfg *config.Config, sink metrics.Sink, store *tracing.Store, ratio float64, exporters ...TraceExporter) *Proxy {
	t.Helper()
	if sink == nil {
		sink = metrics.Nop{}
	}
	reg, err := upstream.New(cfg)
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	t.Cleanup(reg.CloseIdleConnections)
	return New(Options{
		Upstreams:       reg,
		Pricing:         NewPriceBook(cfg.Pricing),
		Metrics:         sink,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
		UpstreamTimeout: cfg.Server.UpstreamTimeout.Duration(),
		Tracer:          NewTracer(store, ratio, exporters...),
	})
}

// namedUpstream is one catch-all backend under an explicit name, which is what
// the trace's upstream attribute and its client span name are built from.
func namedUpstream(name, addr string) config.UpstreamConfig {
	return config.UpstreamConfig{
		Name:    name,
		Kind:    config.KindOpenAI,
		BaseURL: addr,
		APIKey:  "test-key",
		Models:  []string{"/"},
	}
}

// postWithHeaders is mustPost plus caller-supplied headers, because the whole
// point of the propagation tests is what the CALLER sent.
func postWithHeaders(t *testing.T, handler http.Handler, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer caller-key")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return serve(t, handler, req)
}

const tracedChatBody = `{"model":"mock-gpt","messages":[{"role":"user","content":"hello"}]}`

const tracedJSONAnswer = `{"id":"chatcmpl-1","object":"chat.completion","model":"mock-gpt",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`

// backendThinkTime is how long the mock upstreams in this file take to answer,
// and the reason they take any time at all.
//
// time.Now() on Windows steps in quanta of roughly 0.5-1.6ms (measured on the
// development host; the numbers are in cmd/verify-m5/harness.go next to
// mockThinkTime), so a backend that answers inside one quantum produces spans
// whose measured duration is exactly 0. That is how
// TestTraceRecordsOneRequestTrace failed intermittently -- root.DurationMS <= 0
// on a request that really happened. 20ms is above the coarsest timer Windows
// offers, so every span measured around this backend covers at least one clock
// step and a positive duration is true by construction rather than by luck.
const backendThinkTime = 20 * time.Millisecond

func jsonAnswerBackend(t *testing.T) *backend {
	t.Helper()
	return newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		// See backendThinkTime: the delay is what makes the durations of the
		// spans around this call measurable at all.
		time.Sleep(backendThinkTime)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, tracedJSONAnswer)
	})
}

// rootSpan returns the stored trace's root span. Spans[0] is the root by
// construction: requestTrace.finish adds it before the attempt spans.
func rootSpan(t *testing.T, tr *tracing.Trace) tracing.Span {
	t.Helper()
	if len(tr.Spans) == 0 {
		t.Fatalf("trace %s has no spans", tr.TraceID)
	}
	return tr.Spans[0]
}

func attrString(t *testing.T, attrs map[string]any, key string) string {
	t.Helper()
	v, ok := attrs[key]
	if !ok {
		t.Fatalf("attribute %q missing from %v", key, attrs)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("attribute %q = %#v, want string", key, v)
	}
	return s
}

// ---------------------------------------------------------------------------
// Shape of a trace
// ---------------------------------------------------------------------------

// TestTraceRecordsOneRequestTrace is the headline of M5's trace surface: one
// request produces one trace with a root span plus one client span per admitted
// upstream attempt, and the summary is derived from the root span rather than
// from anything the request path cached.
func TestTraceRecordsOneRequestTrace(t *testing.T) {
	be := jsonAnswerBackend(t)
	cfg := oneUpstreamConfig(be.URL)
	store := tracing.NewStore(8)
	p := tracedProxy(t, cfg, nil, store, 1)

	res := mustPost(t, p, "/v1/chat/completions", tracedChatBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", res.Code, res.Body.String())
	}
	if store.Len() != 1 || store.Dropped() != 0 {
		t.Fatalf("store len=%d dropped=%d, want 1/0", store.Len(), store.Dropped())
	}

	list := store.List(0)
	if len(list) != 1 {
		t.Fatalf("list = %+v, want one row", list)
	}
	sum := list[0]
	if sum.SpanCount != 2 {
		t.Errorf("summary span count = %d, want 2 (root + one attempt)", sum.SpanCount)
	}
	if sum.Route != "/v1/chat/completions" {
		t.Errorf("summary route = %q", sum.Route)
	}
	if sum.Upstream != "test" {
		t.Errorf("summary upstream = %q, want test", sum.Upstream)
	}
	if sum.Model != "mock-gpt" {
		t.Errorf("summary model = %q, want mock-gpt", sum.Model)
	}
	if sum.Status != tracing.StatusOK {
		t.Errorf("summary status = %q, want ok", sum.Status)
	}
	if sum.RequestID == "" || sum.RequestID != res.Header().Get(HeaderRequestID) {
		t.Errorf("summary request id = %q, response header = %q", sum.RequestID, res.Header().Get(HeaderRequestID))
	}

	tr, ok := store.Get(sum.TraceID)
	if !ok {
		t.Fatalf("Get(%s) missed", sum.TraceID)
	}
	if len(tr.Spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(tr.Spans))
	}

	root := rootSpan(t, tr)
	if root.Name != SpanGateway || root.Kind != tracing.KindServer {
		t.Errorf("root = %q/%s, want %q/server", root.Name, root.Kind, SpanGateway)
	}
	if root.ParentSpanID != "" {
		t.Errorf("root parent span id = %q, want empty for a local root", root.ParentSpanID)
	}
	if root.TraceID != tr.TraceID || root.SpanID == "" {
		t.Errorf("root ids = trace %q span %q", root.TraceID, root.SpanID)
	}
	if root.Status != tracing.StatusOK || root.DurationMS <= 0 {
		t.Errorf("root status=%q duration=%v, want ok and >0", root.Status, root.DurationMS)
	}
	if got := attrString(t, root.Attributes, tracing.AttrRoute); got != "/v1/chat/completions" {
		t.Errorf("root route attr = %q", got)
	}
	if got := attrString(t, root.Attributes, tracing.AttrUpstream); got != "test" {
		t.Errorf("root upstream attr = %q", got)
	}
	if got := attrString(t, root.Attributes, tracing.AttrModel); got != "mock-gpt" {
		t.Errorf("root model attr = %q", got)
	}
	if got := root.Attributes[tracing.AttrRequest]; got != sum.RequestID {
		t.Errorf("root request id attr = %v, want %q", got, sum.RequestID)
	}
	if got := root.Attributes["attempts"]; got != 1 {
		t.Errorf("root attempts attr = %v (%T), want 1", got, got)
	}
	if got := root.Attributes["status"]; got != http.StatusOK {
		t.Errorf("root status attr = %v (%T), want 200", got, got)
	}
	if got := root.Attributes["outcome"]; got != string(metrics.OutcomeSuccess) {
		t.Errorf("root outcome attr = %v, want %q", got, metrics.OutcomeSuccess)
	}
	if got := root.Attributes["prompt_tokens"]; got != 7 {
		t.Errorf("root prompt_tokens attr = %v (%T), want 7", got, got)
	}

	attempt := tr.Spans[1]
	if attempt.Name != SpanUpstreamPrefix+"test" {
		t.Errorf("attempt span name = %q, want %q", attempt.Name, SpanUpstreamPrefix+"test")
	}
	if attempt.Kind != tracing.KindClient {
		t.Errorf("attempt kind = %q, want client", attempt.Kind)
	}
	if attempt.ParentSpanID != root.SpanID {
		t.Errorf("attempt parent = %q, want the root span %q", attempt.ParentSpanID, root.SpanID)
	}
	if attempt.TraceID != root.TraceID {
		t.Errorf("attempt trace id = %q, want %q", attempt.TraceID, root.TraceID)
	}
	if attempt.StartUnixNano < root.StartUnixNano || attempt.EndUnixNano > root.EndUnixNano {
		t.Errorf("attempt span [%d,%d] is not contained in the root span [%d,%d]",
			attempt.StartUnixNano, attempt.EndUnixNano, root.StartUnixNano, root.EndUnixNano)
	}
	if got := attrString(t, attempt.Attributes, tracing.AttrUpstream); got != "test" {
		t.Errorf("attempt upstream attr = %q", got)
	}
	if got := attempt.Attributes["attempt"]; got != 1 {
		t.Errorf("attempt number attr = %v (%T), want 1", got, got)
	}
	if got := attempt.Attributes["status"]; got != http.StatusOK {
		t.Errorf("attempt status attr = %v, want 200", got)
	}
}

// TestTraceExporterReceivesTheTrace pins the exporter contract: a configured
// exporter sees exactly the trace the store holds, which is what makes "ship
// traces to the collector" testable without a collector.
func TestTraceExporterReceivesTheTrace(t *testing.T) {
	be := jsonAnswerBackend(t)
	cfg := oneUpstreamConfig(be.URL)

	var (
		mu   sync.Mutex
		seen []*tracing.Trace
	)
	exp := exporterFunc(func(tr *tracing.Trace) {
		mu.Lock()
		seen = append(seen, tr)
		mu.Unlock()
	})

	p := tracedProxy(t, cfg, nil, tracing.NewStore(8), 1, exp)
	if res := mustPost(t, p, "/v1/chat/completions", tracedChatBody); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("exporter saw %d traces, want 1", len(seen))
	}
	if len(seen[0].Spans) != 2 || seen[0].Spans[0].Name != SpanGateway {
		t.Errorf("exported trace spans = %+v", seen[0].Spans)
	}
	if seen[0].RequestID == "" || seen[0].TraceID == "" {
		t.Errorf("exported trace has empty ids: %+v", seen[0])
	}
}

type exporterFunc func(*tracing.Trace)

func (f exporterFunc) Export(tr *tracing.Trace) { f(tr) }

// ---------------------------------------------------------------------------
// W3C propagation
// ---------------------------------------------------------------------------

// TestTraceContinuesCallerContext is the propagation half of the feature: a
// caller's trace id survives the gateway, the gateway's own span becomes the
// upstream's parent, and tracestate is passed through untouched.
func TestTraceContinuesCallerContext(t *testing.T) {
	be := jsonAnswerBackend(t)
	cfg := oneUpstreamConfig(be.URL)
	store := tracing.NewStore(8)
	p := tracedProxy(t, cfg, nil, store, 1)

	callerTrace, callerSpan := tracing.NewTraceID(), tracing.NewSpanID()
	res := postWithHeaders(t, p, "/v1/chat/completions", tracedChatBody, map[string]string{
		tracing.HeaderTraceparent: "00-" + callerTrace + "-" + callerSpan + "-01",
		tracing.HeaderTracestate:  "vendor=abc",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}

	if store.Len() != 1 {
		t.Fatalf("store len = %d, want 1", store.Len())
	}
	tr, _ := store.Get(callerTrace)
	if tr == nil {
		t.Fatalf("trace %s was not stored under the caller's trace id", callerTrace)
	}
	root := rootSpan(t, tr)
	if root.ParentSpanID != callerSpan {
		t.Errorf("root parent span id = %q, want the caller's %q", root.ParentSpanID, callerSpan)
	}

	sent := be.recorded()[0].Header
	out, ok := tracing.Parse(sent.Get(tracing.HeaderTraceparent), sent.Get(tracing.HeaderTracestate))
	if !ok {
		t.Fatalf("upstream traceparent %q did not parse", sent.Get(tracing.HeaderTraceparent))
	}
	if out.TraceID != callerTrace {
		t.Errorf("upstream trace id = %q, want the caller's %q", out.TraceID, callerTrace)
	}
	if out.SpanID != root.SpanID {
		t.Errorf("upstream parent span = %q, want the gateway root span %q", out.SpanID, root.SpanID)
	}
	if !out.Sampled {
		t.Error("upstream context is not marked sampled although the caller asked for sampling")
	}
	if got := sent.Get(tracing.HeaderTracestate); got != "vendor=abc" {
		t.Errorf("upstream tracestate = %q, want vendor=abc", got)
	}
}

// TestTraceMintsContextForUntracedCaller: a caller that sends no traceparent
// still gets a valid one propagated upstream, so the gateway never breaks a
// trace chain it did not start.
func TestTraceMintsContextForUntracedCaller(t *testing.T) {
	be := jsonAnswerBackend(t)
	cfg := oneUpstreamConfig(be.URL)
	store := tracing.NewStore(8)
	p := tracedProxy(t, cfg, nil, store, 1)

	if res := mustPost(t, p, "/v1/chat/completions", tracedChatBody); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}

	sent := be.recorded()[0].Header
	out, ok := tracing.Parse(sent.Get(tracing.HeaderTraceparent), "")
	if !ok {
		t.Fatalf("upstream traceparent %q did not parse", sent.Get(tracing.HeaderTraceparent))
	}
	tr, ok := store.Get(out.TraceID)
	if !ok {
		t.Fatalf("upstream trace id %q is not the stored trace", out.TraceID)
	}
	if root := rootSpan(t, tr); root.SpanID != out.SpanID {
		t.Errorf("upstream parent span = %q, want the gateway root span %q", out.SpanID, root.SpanID)
	}
}

// TestTraceReplacesMalformedCallerContext: a broken inbound header is replaced,
// never half-reused, because reusing a trace id with an absent parent produces a
// span whose parent does not exist in any viewer.
func TestTraceReplacesMalformedCallerContext(t *testing.T) {
	be := jsonAnswerBackend(t)
	cfg := oneUpstreamConfig(be.URL)
	store := tracing.NewStore(8)
	p := tracedProxy(t, cfg, nil, store, 1)

	bad := []string{
		"",
		"garbage",
		"00-short-short-01",
		"01-" + tracing.NewTraceID() + "-" + tracing.NewSpanID() + "-01",
		"00-00000000000000000000000000000000-" + tracing.NewSpanID() + "-01",
		"00-" + tracing.NewTraceID() + "-" + strings.ToUpper(tracing.NewSpanID()) + "-01",
	}
	for _, tp := range bad {
		be.mu.Lock()
		be.requests = nil
		be.mu.Unlock()
		store.Reset()

		hdr := map[string]string{}
		if tp != "" {
			hdr[tracing.HeaderTraceparent] = tp
		}
		if res := postWithHeaders(t, p, "/v1/chat/completions", tracedChatBody, hdr); res.Code != http.StatusOK {
			t.Fatalf("traceparent %q: status = %d, want 200", tp, res.Code)
		}
		sent := be.recorded()[0].Header.Get(tracing.HeaderTraceparent)
		out, ok := tracing.Parse(sent, "")
		if !ok {
			t.Errorf("traceparent %q: upstream header %q did not parse", tp, sent)
			continue
		}
		if strings.Contains(tp, out.TraceID) && tp != "" {
			t.Errorf("traceparent %q: reused the malformed trace id in %q", tp, sent)
		}
		if store.Len() != 1 {
			t.Errorf("traceparent %q: store len = %d, want 1", tp, store.Len())
		}
	}
}

// TestTraceSamplingZeroStillPropagates is the sampling contract: a gateway that
// is not recording still forwards a usable context, and records nothing.
func TestTraceSamplingZeroStillPropagates(t *testing.T) {
	be := jsonAnswerBackend(t)
	cfg := oneUpstreamConfig(be.URL)
	store := tracing.NewStore(8)
	p := tracedProxy(t, cfg, nil, store, 0)

	if res := mustPost(t, p, "/v1/chat/completions", tracedChatBody); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if store.Len() != 0 {
		t.Errorf("store len = %d, want 0 at ratio 0", store.Len())
	}
	out, ok := tracing.Parse(be.recorded()[0].Header.Get(tracing.HeaderTraceparent), "")
	if !ok {
		t.Fatalf("upstream traceparent did not parse: %q", be.recorded()[0].Header.Get(tracing.HeaderTraceparent))
	}
	if out.Sampled {
		t.Error("upstream context claims sampling although the gateway recorded nothing")
	}
}

// TestTraceHonoursCallerDropDecision: a caller that explicitly marked the trace
// unsampled is not second-guessed, because keeping half a trace is worse than
// keeping none of it.
func TestTraceHonoursCallerDropDecision(t *testing.T) {
	be := jsonAnswerBackend(t)
	cfg := oneUpstreamConfig(be.URL)
	store := tracing.NewStore(8)
	p := tracedProxy(t, cfg, nil, store, 1)

	callerTrace, callerSpan := tracing.NewTraceID(), tracing.NewSpanID()
	res := postWithHeaders(t, p, "/v1/chat/completions", tracedChatBody, map[string]string{
		tracing.HeaderTraceparent: "00-" + callerTrace + "-" + callerSpan + "-00",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if store.Len() != 0 {
		t.Errorf("store len = %d, want 0 for an unsampled caller", store.Len())
	}
	out, ok := tracing.Parse(be.recorded()[0].Header.Get(tracing.HeaderTraceparent), "")
	if !ok {
		t.Fatal("upstream traceparent did not parse")
	}
	if out.TraceID != callerTrace {
		t.Errorf("upstream trace id = %q, want the caller's %q even when dropping", out.TraceID, callerTrace)
	}
	if out.Sampled {
		t.Error("upstream context is marked sampled although the caller's flag was clear")
	}
}

// TestSamplingIsStableForOneRequestID: the sampling decision is a hash of the
// request id, so a retry of the same request lands in the same bucket. A random
// draw would make a retried request appear in one trace but not the other.
func TestSamplingIsStableForOneRequestID(t *testing.T) {
	tr := NewTracer(tracing.NewStore(4), 0.5)
	const id = "ig-0123456789abcdef"
	first := tr.shouldRecord(tracing.NewRootContext(), false, id)
	for i := 0; i < 8; i++ {
		if got := tr.shouldRecord(tracing.NewRootContext(), false, id); got != first {
			t.Fatalf("sampling decision changed on repeat: %v then %v", first, got)
		}
	}
	// And the whole space is not one bucket: at ratio 0.5 a run of distinct
	// ids must contain both verdicts.
	var kept, dropped int
	for i := 0; i < 64; i++ {
		if tr.shouldRecord(tracing.NewRootContext(), false, tracing.NewTraceID()) {
			kept++
		} else {
			dropped++
		}
	}
	if kept == 0 || dropped == 0 {
		t.Errorf("ratio 0.5 sampled %d and dropped %d of 64 distinct ids, want both", kept, dropped)
	}
}

// TestTracerDisabledRecordsNothing: no store and no exporter means no tracer at
// all, which is the configuration every existing test runs in. A disabled
// tracer neither invents a context nor interferes with the caller's — its
// traceparent rides along untouched because it is an ordinary header.
func TestTracerDisabledRecordsNothing(t *testing.T) {
	if tr := NewTracer(nil, 1); tr != nil {
		t.Fatalf("NewTracer(nil, 1) = %+v, want nil", tr)
	}
	be := jsonAnswerBackend(t)
	cfg := oneUpstreamConfig(be.URL)
	p := tracedProxy(t, cfg, nil, nil, 1)
	if p.tracer != nil {
		t.Fatalf("proxy tracer = %+v, want nil", p.tracer)
	}
	if res := mustPost(t, p, "/v1/chat/completions", tracedChatBody); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := be.recorded()[0].Header.Get(tracing.HeaderTraceparent); got != "" {
		t.Errorf("tracing-disabled gateway minted traceparent %q, want none", got)
	}

	// A caller that IS tracing keeps its context across a gateway that is not.
	caller := "00-" + tracing.NewTraceID() + "-" + tracing.NewSpanID() + "-01"
	be.mu.Lock()
	be.requests = nil
	be.mu.Unlock()
	if res := postWithHeaders(t, p, "/v1/chat/completions", tracedChatBody, map[string]string{
		tracing.HeaderTraceparent: caller,
	}); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := be.recorded()[0].Header.Get(tracing.HeaderTraceparent); got != caller {
		t.Errorf("tracing-disabled gateway rewrote the caller's traceparent to %q, want it unchanged", got)
	}
}

// ---------------------------------------------------------------------------
// Request identity
// ---------------------------------------------------------------------------

// TestRequestIDIsConsistentAcrossHops pins the fix for the second M5 defect:
// the id the gateway reports, the id it logs, and the id it sends upstream must
// be ONE value. The M0/M1 behaviour minted a fresh id in buildRequest whenever
// the caller sent none, so the trace and the provider's logs disagreed.
func TestRequestIDIsConsistentAcrossHops(t *testing.T) {
	cases := []struct {
		name    string
		inbound string
	}{
		{name: "minted", inbound: ""},
		{name: "caller supplied", inbound: "caller-chosen-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := jsonAnswerBackend(t)
			cfg := oneUpstreamConfig(be.URL)
			store := tracing.NewStore(8)
			p := tracedProxy(t, cfg, nil, store, 1)

			hdr := map[string]string{}
			if tc.inbound != "" {
				hdr[HeaderRequestID] = tc.inbound
			}
			res := postWithHeaders(t, p, "/v1/chat/completions", tracedChatBody, hdr)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", res.Code)
			}

			downstream := res.Header().Get(HeaderRequestID)
			if downstream == "" {
				t.Fatal("response has no request id header")
			}
			if tc.inbound != "" && downstream != tc.inbound {
				t.Errorf("response request id = %q, want the caller's %q", downstream, tc.inbound)
			}
			upstream := be.recorded()[0].Header.Get("X-Request-Id")
			if upstream != downstream {
				t.Errorf("upstream X-Request-Id = %q, gateway X-InferGate-Request-Id = %q", upstream, downstream)
			}
			tr, ok := store.Get(downstream)
			if !ok {
				t.Fatalf("store.Get(%q) missed: the trace is not reachable by request id", downstream)
			}
			if tr.RequestID != downstream {
				t.Errorf("trace request id = %q, want %q", tr.RequestID, downstream)
			}
			if got := rootSpan(t, tr).Attributes[tracing.AttrRequest]; got != downstream {
				t.Errorf("root request id attr = %v, want %q", got, downstream)
			}
		})
	}
}

// tracedFailoverProxy builds the full M1 stack — registry, breakers, router,
// proxy — with tracing on, so a failover trace is produced by the same code path
// production runs rather than by a two-upstream shortcut around the router.
func tracedFailoverProxy(t *testing.T, store *tracing.Store, primary, backup string) *Proxy {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:              ":0",
			UpstreamTimeout:     config.Duration(5 * time.Second),
			MaxBodyBytes:        1 << 20,
			MaxIdleConnsPerHost: 4,
		},
		Upstreams: []config.UpstreamConfig{
			namedUpstream("first", primary),
			namedUpstream("second", backup),
		},
		Routing: config.RoutingConfig{Strategy: config.StrategyPriority},
		Health:  testHealthConfig(),
	}
	cfg.Upstreams[0].Priority = 1
	cfg.Upstreams[1].Priority = 2

	registry, err := upstream.New(cfg)
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	t.Cleanup(registry.CloseIdleConnections)

	breakers := breaker.NewGroup(cfg.Health, registry.Names())
	priceBook := NewPriceBook(cfg.Pricing)
	rt := router.New(router.Options{
		Registry: registry,
		Breakers: breakers,
		Routing:  cfg.Routing,
		Health:   cfg.Health,
		Price: func(model string) (float64, bool) {
			p, ok := priceBook.Price(model)
			return (p.In + p.Out) / 2, ok
		},
	})

	return New(Options{
		Upstreams:       registry,
		Pricing:         priceBook,
		Metrics:         metrics.Nop{},
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
		UpstreamTimeout: cfg.Server.UpstreamTimeout.Duration(),
		Router:          rt,
		Breakers:        breakers,
		MaxAttempts:     2,
		RetryBackoff:    time.Millisecond,
		Tracer:          NewTracer(store, 1),
	})
}

// ---------------------------------------------------------------------------
// Failover and streaming
// ---------------------------------------------------------------------------

// TestTraceRecordsOneSpanPerFailoverAttempt: a client span per ADMITTED
// attempt, in order, each carrying its own upstream status — the point being
// that "the first backend 500ed" survives in the trace even though the response
// the caller saw was a 200.
func TestTraceRecordsOneSpanPerFailoverAttempt(t *testing.T) {
	bad := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	})
	good := jsonAnswerBackend(t)

	store := tracing.NewStore(8)
	p := tracedFailoverProxy(t, store, bad.URL, good.URL)

	res := mustPost(t, p, "/v1/chat/completions", tracedChatBody)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover (body %s)", res.Code, res.Body.String())
	}

	if store.Len() != 1 {
		t.Fatalf("store len = %d, want 1 trace for the whole request", store.Len())
	}
	tr, _ := store.Get(store.List(0)[0].TraceID)
	if len(tr.Spans) != 3 {
		t.Fatalf("spans = %d, want 3 (root + two attempts)", len(tr.Spans))
	}
	root := rootSpan(t, tr)
	if got := root.Attributes["attempts"]; got != 2 {
		t.Errorf("root attempts attr = %v, want 2", got)
	}
	if got := attrString(t, root.Attributes, tracing.AttrUpstream); got != "second" {
		t.Errorf("root upstream attr = %q, want the serving backend", got)
	}
	tried, ok := root.Attributes["tried"].([]string)
	if !ok || len(tried) != 2 || tried[0] != "first" || tried[1] != "second" {
		t.Errorf("root tried attr = %#v, want [first second]", root.Attributes["tried"])
	}
	if root.Status != tracing.StatusOK {
		t.Errorf("root status = %q, want ok: the caller got an answer", root.Status)
	}

	first, second := tr.Spans[1], tr.Spans[2]
	if first.Name != SpanUpstreamPrefix+"first" || second.Name != SpanUpstreamPrefix+"second" {
		t.Fatalf("attempt span names = %q, %q", first.Name, second.Name)
	}
	if got := first.Attributes["status"]; got != http.StatusInternalServerError {
		t.Errorf("first attempt status attr = %v, want 500", got)
	}
	if first.Status != tracing.StatusError {
		t.Errorf("first attempt span status = %q, want error", first.Status)
	}
	if got := second.Attributes["status"]; got != http.StatusOK {
		t.Errorf("second attempt status attr = %v, want 200", got)
	}
	if second.Status != tracing.StatusOK {
		t.Errorf("second attempt span status = %q, want ok", second.Status)
	}

	var failoverEvent bool
	for _, e := range root.Events {
		if e.Name == "failover" {
			failoverEvent = true
			if got := e.Attributes["failovers"]; got != 1 {
				t.Errorf("failover event count = %v, want 1", got)
			}
			if got, ok := e.Attributes["tried"].([]string); !ok || len(got) != 2 {
				t.Errorf("failover event tried = %#v, want [first second]", e.Attributes["tried"])
			}
		}
	}
	if !failoverEvent {
		t.Errorf("no failover event; events = %+v", root.Events)
	}
}

// TestStreamObservedOncePerRequest is the regression test for the first M5
// defect: the attempt tail and the deferred per-request recorder both reported
// first-token and frame counts, so every streamed request was counted twice.
func TestStreamObservedOncePerRequest(t *testing.T) {
	rec := metrics.NewRecorder()
	p := streamProxy(t, sseBackend{lines: []string{frameRole, frameText, frameStop, frameDone}}, rec)

	res := mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}

	// Count what actually reached the client and compare: the metrics must
	// describe the stream that happened, not twice it.
	relayed := int64(strings.Count(res.Body.String(), "data: "))
	if relayed == 0 {
		t.Fatal("no SSE frames in the response")
	}
	streams := rec.StreamSnapshot()
	if len(streams) != 1 {
		t.Fatalf("stream series = %+v, want one row", streams)
	}
	// The recorder also counts the synthesised [DONE] when the provider omitted
	// it, so compare against the frames the gateway wrote, not the provider's.
	if streams[0].Frames != relayed {
		t.Errorf("recorded frames = %d, response carried %d data frames (double count?)", streams[0].Frames, relayed)
	}

	firsts := rec.FirstTokenSnapshot()
	if len(firsts) != 1 {
		t.Fatalf("first-token series = %+v, want exactly one row", firsts)
	}
	if firsts[0].Count != 1 {
		t.Errorf("first-token observations = %d, want 1 (double count?)", firsts[0].Count)
	}

	// The attempt counter is per admitted attempt and must not have been
	// inflated by the same fix.
	if rows := rec.FailoverSnapshot(); len(rows) != 0 {
		t.Errorf("failover rows = %+v, want none for a one-attempt stream", rows)
	}
}
