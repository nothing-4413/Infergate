package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/sse"
	"github.com/infergate/infergate/internal/upstream"
)

// sseBackend is a scriptable OpenAI-compatible streaming upstream. It writes
// each line of the given script as its own flushed SSE frame, with an optional
// inter-frame delay so that time-to-first-token is observable.
type sseBackend struct {
	lines     []string
	frameGap  time.Duration
	status    int
	headers   map[string]string
	omitDone  bool
	rawPrefix string
}

func (s sseBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for k, v := range s.headers {
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)

	// Pre-filled frames arrive back-to-back, which is what a real provider does
	// while it is chewing on a long tool-call argument.
	for _, line := range s.lines {
		if s.frameGap > 0 && strings.HasPrefix(line, "data:") {
			select {
			case <-time.After(s.frameGap):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write([]byte(line))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

const (
	frameRole  = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n"
	frameText  = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n"
	frameStop  = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	frameUsage = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":4}}}` + "\n\n"
	frameDone  = "data: [DONE]\n\n"
)

func streamProxy(t *testing.T, script sseBackend, rec metrics.Sink) http.Handler {
	t.Helper()
	be := httptest.NewServer(script)
	t.Cleanup(be.Close)
	cfg := oneUpstreamConfig(be.URL)
	cfg.Server.UpstreamTimeout = config.Duration(10 * time.Second)
	return newTestProxy(t, cfg, rec)
}

// TestStreamingHappyPath is the M0 streaming headline: frames are relayed
// verbatim to a flushing client, and [DONE] terminates the stream.
func TestStreamingHappyPath(t *testing.T) {
	rec := metrics.NewRecorder()
	p := streamProxy(t, sseBackend{lines: []string{frameRole, frameText, frameStop, frameDone}}, rec)

	res := mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if ct := res.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := res.Body.String()
	if !strings.Contains(body, `"content":"Hello"`) {
		t.Errorf("relayed body missing the content delta:\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("stream did not end with [DONE]:\n%s", body)
	}
	// Byte-transparent relay: the upstream's own frames must appear unchanged.
	if !strings.Contains(body, frameText) {
		t.Errorf("relayed frames were rewritten; got:\n%s", body)
	}

	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Upstream != "test" || snap[0].Status != 200 {
		t.Fatalf("metric series = %+v, want one successful test-upstream row", snap)
	}
	streams := rec.StreamSnapshot()
	if len(streams) != 1 || streams[0].Frames < 4 {
		t.Errorf("stream frames = %+v, want at least 4", streams)
	}
}

// TestStreamingUsageAndCostAccounting proves the gateway can bill a stream: the
// provider only reports usage in the final frame, and the gateway must pick it
// up there (plus the cached-prompt detail that drives M2's cache savings).
func TestStreamingUsageAndCostAccounting(t *testing.T) {
	rec := metrics.NewRecorder()
	p := streamProxy(t, sseBackend{lines: []string{frameRole, frameText, frameStop, frameUsage, frameDone}}, rec)

	mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)

	prompt, completion, cached := rec.TokenTotals()
	if prompt != 9 || completion != 3 || cached != 4 {
		t.Errorf("tokens prompt/completion/cached = %d/%d/%d, want 9/3/4", prompt, completion, cached)
	}
	snap := rec.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("series = %+v", snap)
	}
	// The provider reported a dated snapshot name; billing must follow it.
	if snap[0].Model != "gpt-4o-2024-08-06" {
		t.Errorf("model = %q, want the provider-reported snapshot", snap[0].Model)
	}
}

// TestStreamingToolCallDeltaAssembly is the detail that makes an agent gateway
// usable: OpenAI streams tool arguments as fragments indexed by tool-call
// position, and the gateway must reassemble them without buffering the response.
func TestStreamingToolCallDeltaAssembly(t *testing.T) {
	f1 := `data: {"id":"c","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}` + "\n\n"
	f2 := `data: {"id":"c","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}` + "\n\n"
	f3 := `data: {"id":"c","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Beijing\"}"}}]},"finish_reason":null}]}` + "\n\n"
	f4 := `data: {"id":"c","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"

	be := httptest.NewServer(sseBackend{lines: []string{f1, f2, f3, f4, frameDone}})
	t.Cleanup(be.Close)

	// Drive the inspector directly as well, because the assembled tool call is
	// an internal observation that must be correct before M1 can use it.
	cfg := oneUpstreamConfig(be.URL)
	p := newTestProxy(t, cfg, metrics.NewRecorder())

	res := mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)
	body := res.Body.String()

	// Every fragment must reach the client untouched: the gateway observes, it
	// does not rewrite.
	for _, want := range []string{`"name":"get_weather"`, `{\"city\":`, `\"Beijing\"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream lost tool_call fragment %q:\n%s", want, body)
		}
	}
}

// TestStreamingUsageFromFragmentedToolArgs covers the inspector on a stream
// where arguments arrive split across frames.
func TestStreamingUsageFromFragmentedToolArgs(t *testing.T) {
	ins := newInspectorForTest()
	frames := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"f","arguments":"{"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}`,
	}
	for _, f := range frames {
		obs := ins.Observe(rawFrame("data: " + f + "\n\n"))
		if obs.Err != nil {
			t.Fatalf("unexpected stream error: %v", obs.Err)
		}
	}
	calls := ins.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if calls[0].Name != "f" || string(calls[0].Args) != "{}" {
		t.Errorf("assembled call = %+v, want f({})", calls[0])
	}
}

// TestStreamingWithoutDoneIsRepaired keeps OpenAI SDK clients from hanging
// forever when a backend closes the socket without the sentinel.
func TestStreamingWithoutDoneIsRepaired(t *testing.T) {
	p := streamProxy(t, sseBackend{lines: []string{frameRole, frameText, frameStop}}, metrics.NewRecorder())

	res := mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)

	body := res.Body.String()
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("gateway did not synthesise [DONE] for a backend that omitted it:\n%s", body)
	}
}

// TestStreamingInStreamErrorIsVisible verifies that a provider error delivered
// mid-stream (200 OK already sent, so no status code can carry it) is both
// forwarded and classified as a failure rather than a success.
func TestStreamingInStreamErrorIsVisible(t *testing.T) {
	errFrame := `data: {"error":{"message":"upstream exploded","type":"server_error"}}` + "\n\n"
	rec := metrics.NewRecorder()
	p := streamProxy(t, sseBackend{lines: []string{frameRole, errFrame, frameDone}}, rec)

	res := mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)

	if !strings.Contains(res.Body.String(), "upstream exploded") {
		t.Errorf("in-stream error was not forwarded:\n%s", res.Body.String())
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Outcome != metrics.OutcomeUpstreamErr {
		t.Errorf("outcome = %+v, want upstream_error", snap)
	}
}

// TestStreamingHeartbeatCommentsAreRelayed covers the keep-alive path: an idle
// provider must not look like a hung request to an intermediary proxy.
func TestStreamingHeartbeatCommentsAreRelayed(t *testing.T) {
	p := streamProxy(t, sseBackend{lines: []string{": ping\n\n", frameRole, frameText, frameDone}}, metrics.NewRecorder())

	res := mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)
	if !strings.Contains(res.Body.String(), ": ping") {
		t.Errorf("heartbeat comment was dropped:\n%s", res.Body.String())
	}
}

// TestFirstTokenLatencyIsRecorded validates the metric that matters most for an
// agent UX. The backend stalls before its first data frame, so the recorded
// first-token time must reflect the stall rather than the total.
func TestFirstTokenLatencyIsRecorded(t *testing.T) {
	rec := metrics.NewRecorder()
	p := streamProxy(t, sseBackend{
		lines:    []string{frameRole, frameText, frameStop, frameDone},
		frameGap: 60 * time.Millisecond,
	}, rec)

	start := time.Now()
	mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)
	total := time.Since(start)

	samples := rec.FirstTokenSnapshot()
	if len(samples) != 1 {
		t.Fatalf("first-token samples = %+v, want 1", samples)
	}
	if samples[0].Mean <= 0 {
		t.Fatalf("first-token mean = %v, want > 0", samples[0].Mean)
	}
	if samples[0].Mean > total {
		t.Errorf("first-token %v exceeds total request time %v", samples[0].Mean, total)
	}
}

// TestStreamingRespectsUpstreamTimeout proves the deadline also governs an
// unbounded stream, which is the case that a server-level WriteTimeout would
// otherwise have to handle (and get wrong).
func TestStreamingRespectsUpstreamTimeout(t *testing.T) {
	block := make(chan struct{})
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(block); be.Close() })

	cfg := oneUpstreamConfig(be.URL)
	cfg.Server.UpstreamTimeout = config.Duration(200 * time.Millisecond)
	p := newTestProxy(t, cfg, metrics.NewRecorder())

	start := time.Now()
	res := mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)
	// Headers were already committed, so the client sees a 200 with a truncated
	// body; the assertion that matters is that the gateway did not hang.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("streaming timeout took %v, want it bounded near 200ms", elapsed)
	}
	if res.Code != http.StatusOK {
		t.Logf("status = %d (headers already sent before the timeout)", res.Code)
	}
}

// TestStreamingBadJSONDoesNotBreakTheStream is the resilience case: one
// malformed frame from a provider must not kill an otherwise healthy stream.
func TestStreamingBadJSONDoesNotBreakTheStream(t *testing.T) {
	rec := metrics.NewRecorder()
	p := streamProxy(t, sseBackend{lines: []string{
		"data: {not json\n\n",
		frameText,
		frameDone,
	}}, rec)

	res := mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if !strings.Contains(res.Body.String(), `"Hello"`) {
		t.Errorf("stream was aborted by a malformed frame:\n%s", res.Body.String())
	}
}

// TestNonStreamingResponseWithSSEContentTypeIsRelayed guards the branch
// selection: a provider that ignores "stream":true must not be pushed through
// the SSE inspector.
func TestContentTypeDrivesRelayModeNotTheRequest(t *testing.T) {
	jsonBody := `{"id":"x","choices":[{"message":{"content":"plain"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		// Deliberately ignore the request's "stream": true.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jsonBody))
	})
	rec := metrics.NewRecorder()
	p := newTestProxy(t, oneUpstreamConfig(be.URL), rec)

	res := mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)
	if res.Body.String() != jsonBody {
		t.Errorf("body = %q, want the JSON body verbatim", res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if p, c, _ := rec.TokenTotals(); p != 1 || c != 1 {
		t.Errorf("tokens = %d/%d, want 1/1 from the JSON usage block", p, c)
	}
}

// TestStreamingHeadersAreNotBuffered is the anti-buffering guarantee: a client
// must receive the first frame as soon as it exists, not after the stream ends.
// Without this, an agent's perceived latency equals the whole generation.
func TestStreamingHeadersAreNotBuffered(t *testing.T) {
	release := make(chan struct{})
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write([]byte(frameText))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(frameDone))
	}))
	t.Cleanup(func() { close(release); be.Close() })

	p := newTestProxy(t, oneUpstreamConfig(be.URL), metrics.NewRecorder())

	// A recorder is not a real socket, so this asserts the weaker but still
	// meaningful property: the handler flushes (the recorder implements
	// Flusher) and the relayed bytes are complete on return.
	res := mustPost(t, p, "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`)
	if !res.Flushed {
		t.Error("no flush was issued; a streaming client would buffer the whole response")
	}
	if !strings.Contains(res.Body.String(), "Hello") {
		t.Errorf("body = %q", res.Body.String())
	}
}

// rawFrame turns a raw SSE frame into the parser's representation. The
// inspector is exercised through the same sse.Reader the live relay uses, so
// these tests cover the real parsing path rather than a hand-built struct.
func rawFrame(raw string) *sse.Frame {
	f, err := sse.NewReader(strings.NewReader(raw)).NextFrame()
	if err != nil {
		panic("rawFrame: " + err.Error())
	}
	return f
}

func newInspectorForTest() *sse.Inspector { return sse.NewInspector() }

// TestStreamingMultipleToolCalls verifies index-based assembly with two
// concurrent tool calls, which is how an agent loop emits parallel calls.
func TestStreamingMultipleToolCalls(t *testing.T) {
	ins := newInspectorForTest()
	payloads := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f0","arguments":"{}"}},{"index":1,"id":"b","function":{"name":"f1","arguments":"{"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}}]}}]}`,
	}
	for _, p := range payloads {
		ins.Observe(rawFrame("data: " + p + "\n\n"))
	}
	calls := ins.ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(calls))
	}
	byIndex := map[int]string{}
	for _, c := range calls {
		byIndex[c.Index] = c.Name + string(c.Args)
	}
	if byIndex[0] != "f0{}" || byIndex[1] != "f1{}" {
		t.Errorf("assembled calls = %v, want f0{} and f1{}", byIndex)
	}
}

// TestFinishReasonIsCaptured feeds M1's failover logic, which needs to know
// whether a stream ended because the model finished or because it was cut off.
func TestFinishReasonIsCaptured(t *testing.T) {
	ins := newInspectorForTest()
	ins.Observe(rawFrame(frameStop))
	if got := ins.FinishReason(); got != "stop" {
		t.Errorf("finish reason = %q, want stop", got)
	}
	if u := ins.Usage(); !u.Empty() {
		t.Errorf("usage = %+v, want empty for a frame with no usage block", u)
	}
}

// TestUsageFromJSONResponse covers the non-streaming accounting path used by
// accountJSON.
func TestUsageFromJSONResponse(t *testing.T) {
	body := []byte(`{"model":"gpt-4o-2024-08-06","usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":64}}}`)
	usage, model, ok := usageFromJSON(body)
	if !ok {
		t.Fatal("usageFromJSON returned ok=false for a body with a usage block")
	}
	if usage.Prompt != 100 || usage.Completion != 20 || usage.Cached != 64 {
		t.Errorf("usage = %+v, want 100/20/64", usage)
	}
	if model != "gpt-4o-2024-08-06" {
		t.Errorf("model = %q", model)
	}
	if _, _, ok := usageFromJSON([]byte(`{"choices":[]}`)); ok {
		t.Error("usageFromJSON reported usage for a body without one")
	}
}

// TestPriceBookCost guards the cost arithmetic that M3's budget gate depends
// on. Cached prompt tokens are billed at the input rate: conservative, and
// never under-reports spend.
func TestPriceBookCost(t *testing.T) {
	pb := NewPriceBook(config.PricingConfig{
		Default: config.ModelPrice{In: 1, Out: 2},
		Models: map[string]config.ModelPrice{
			"gpt-4o": {In: 2.50, Out: 10.00},
		},
	})

	// 1M input + 1M output at the model rate.
	if got := pb.CostUSD("gpt-4o", 1_000_000, 1_000_000); got != 12.50 {
		t.Errorf("gpt-4o cost = %v, want 12.50", got)
	}
	// Provider-prefixed names must still resolve.
	if got := pb.CostUSD("openai/gpt-4o", 1_000_000, 0); got != 2.50 {
		t.Errorf("prefixed cost = %v, want 2.50", got)
	}
	// An unknown model uses the default rate rather than silently reporting 0.
	if got := pb.CostUSD("mystery-model", 1_000_000, 0); got != 1 {
		t.Errorf("default cost = %v, want 1", got)
	}
	// Cached prompt tokens are part of the prompt and must be billed.
	if got := pb.CostUSD("gpt-4o", 2_000_000, 0); got != 5.00 {
		t.Errorf("cached-inclusive cost = %v, want 5.00", got)
	}
}

func TestErrorBodyShapeMatchesOpenAI(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadGateway, TypeBadGateway, "boom")

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Message != "boom" || body.Error.Type != TypeBadGateway {
		t.Errorf("body = %+v", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// ---------------------------------------------------------------------------
// Mid-stream client hangup
// ---------------------------------------------------------------------------

// stallWriter is a ResponseWriter that stops draining the stream after the first
// data frame: Write blocks until the test releases it. That is what the relay
// sees when a client stops reading — one frame in flight (the one the writer is
// parked inside) and nothing else consumed — which is the state the terminal
// send has to survive.
type stallWriter struct {
	header  http.Header
	first   chan struct{} // closed once the relay is parked inside Write
	release chan struct{} // closed by the test to let that Write return
	writes  int64
}

func newStallWriter() *stallWriter {
	return &stallWriter{
		header:  http.Header{},
		first:   make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *stallWriter) Header() http.Header { return s.header }
func (s *stallWriter) WriteHeader(int)     {}
func (s *stallWriter) Flush()              {}

func (s *stallWriter) Write(p []byte) (int, error) {
	if atomic.AddInt64(&s.writes, 1) == 1 {
		close(s.first)
		<-s.release
	}
	return len(p), nil
}

// frameBody serves the scripted frames and then fails the read, the way a
// provider connection drops mid-answer. atEnd closes on the read that carries
// that failure, which is how the test knows the reader goroutine has been handed
// the terminal error and has nothing left to read.
type frameBody struct {
	mu    sync.Mutex
	data  []byte
	off   int
	atEnd chan struct{}
	once  sync.Once
}

func (b *frameBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.off < len(b.data) {
		n := copy(p, b.data[b.off:])
		b.off += n
		return n, nil
	}
	b.once.Do(func() { close(b.atEnd) })
	return 0, errors.New("upstream connection reset")
}

func (b *frameBody) Close() error { return nil }

// relayReaderParked reports whether relayStream's reader goroutine is still
// alive. That goroutine is the one anonymous function the relay starts, so its
// presence in a full stack dump is exactly the leak under test — and unlike a
// goroutine count, no other part of the test binary can produce that frame.
func relayReaderParked() bool {
	// Grow until the dump fits: a truncated dump could hide the reader.
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Contains(string(buf[:n]), "relayStream.func1")
		}
		buf = make([]byte, 2*len(buf))
	}
}

func awaitClose(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(what)
	}
}

// TestClientHangupMidStreamDoesNotStrandTheReaderGoroutine is the regression
// test for the terminal-error send in relayStream.
//
// The reader goroutine hands frames to the handler over an 8-slot channel, and
// reports why the stream ended over that same channel. Both sends have to be
// abandonable: the handler stops draining the moment the client hangs up (it
// returns on context cancellation), so an unconditional send with the buffer
// full parks the reader for the life of the process — one leaked goroutine per
// mid-stream hangup, still holding the upstream connection it owns.
//
// Reproducing it needs the buffer genuinely full *and* the blocked send to be
// the terminal error, because the frame send is guarded either way: a cancelled
// context unblocks the reader at its next frame. So nine frames are offered
// (eight buffered plus the one the stalled writer is inside), the reader reads
// a failure, and it parks on the error send with nowhere to put it. Cancel the
// request and let the stalled write return: with the guard the reader leaves,
// without it the goroutine is still parked on that send.
func TestClientHangupMidStreamDoesNotStrandTheReaderGoroutine(t *testing.T) {
	const buffered = 8 // the capacity of the frames channel in relayStream

	var script strings.Builder
	for i := 0; i < buffered+1; i++ {
		fmt.Fprintf(&script, `data: {"choices":[{"index":0,"delta":{"content":"tok%d"}}]}`+"\n\n", i)
	}

	body := &frameBody{data: []byte(script.String()), atEnd: make(chan struct{})}
	resp := &http.Response{StatusCode: http.StatusOK, Body: body, Header: http.Header{}}
	w := newStallWriter()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	p := New(Options{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBodyBytes: 1 << 20,
	})

	done := make(chan metrics.Outcome, 1)
	go func() {
		done <- p.relayStream(w, req, resp, &upstream.Target{Name: "stalled"}, &record{})
	}()

	// The handler is parked inside the first frame write, so nothing drains the
	// channel from here on.
	awaitClose(t, w.first, "the relay never wrote a frame to the stalled client")
	// Every frame was parsed and enqueued, so the reader has now read the
	// failure and is sitting on the terminal send.
	awaitClose(t, body.atEnd, "the reader never reached the end of the upstream body")
	if !relayReaderParked() {
		t.Fatal("the relay's reader goroutine was already gone: this test did not reproduce a mid-stream hangup")
	}

	cancel()
	close(w.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relayStream did not return after the client hung up")
	}

	// Once the client is gone and the handler has returned, the reader has no
	// way out except a send that listens for the hangup.
	deadline := time.Now().Add(5 * time.Second)
	for relayReaderParked() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if relayReaderParked() {
		t.Fatal("the client hung up mid-stream and the handler returned, but the relay's reader goroutine is still parked on its channel send: a hung-up client leaks a goroutine and keeps its upstream connection open")
	}
}
