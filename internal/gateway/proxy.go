package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/upstream"
)

// Proxy is the OpenAI-compatible reverse proxy that is InferGate's M0 surface.
//
// It is deliberately stateless per request: every field is read-only after
// construction, so the value can be shared by all handler goroutines without
// locking. Mutable per-request state lives in `record` and local variables.
type Proxy struct {
	upstreams *upstream.Registry
	pricing   *PriceBook
	metrics   metrics.Sink
	log       *slog.Logger

	client      *http.Client
	maxBody     int64
	upstreamTTL time.Duration

	// transportFor returns the RoundTripper for a target. It exists so tests
	// can inject a transport (to simulate a dead backend, a slow backend, or a
	// mid-stream disconnect) without standing up a real server.
	transportFor func(*upstream.Target) http.RoundTripper
}

// Options configures a Proxy.
type Options struct {
	// Upstreams resolves a model (and an optional pin) to a backend.
	Upstreams *upstream.Registry

	// Pricing converts tokens to USD. Nil means "do not compute cost".
	Pricing *PriceBook

	// Metrics receives observations. Nil means metrics.Nop.
	Metrics metrics.Sink

	// Logger receives structured request records. Nil means slog.Default().
	Logger *slog.Logger

	// MaxBodyBytes caps the inbound request body.
	MaxBodyBytes int64

	// UpstreamTimeout bounds one upstream exchange, streaming included. Zero
	// disables the gateway-level cap, leaving the client's disconnect as the
	// only terminator.
	UpstreamTimeout time.Duration

	// TransportFor overrides transport selection. Tests use it; production
	// leaves it nil so each backend gets its own pooled transport.
	TransportFor func(*upstream.Target) http.RoundTripper
}

// New builds a Proxy.
func New(opts Options) *Proxy {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	sink := opts.Metrics
	if sink == nil {
		sink = metrics.Nop{}
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 8 << 20
	}
	p := &Proxy{
		upstreams:   opts.Upstreams,
		pricing:     opts.Pricing,
		metrics:     sink,
		log:         logger,
		client:      &http.Client{}, // no Timeout: streaming bodies must not be cut off by the client
		maxBody:     maxBody,
		upstreamTTL: opts.UpstreamTimeout,
	}
	// A nil transportFor must never be reachable: this is a per-request code
	// path, and a nil function value here means every single request panics
	// into a 500. Default to the transport the registry attached to the target
	// so that constructing a Proxy from Options alone is always safe.
	p.transportFor = opts.TransportFor
	if p.transportFor == nil {
		p.transportFor = func(t *upstream.Target) http.RoundTripper {
			return t.Transport
		}
	}
	return p
}

// record accumulates the facts about one request so that exactly one metrics
// observation and one log record are emitted, from a single place, regardless
// of how many returns the handler has.
type record struct {
	start time.Time

	requestID string
	route     string
	path      string
	upstream  string
	model     string
	stream    bool

	status     int
	outcome    metrics.Outcome
	reason     string
	reqBytes   int64
	respBytes  int64
	frames     int64
	firstToken time.Duration
	// firstTokenSet records that a first token was observed, independently of
	// its measured value. A legitimate first token can measure as exactly zero
	// on a coarse clock, and gating on `firstToken > 0` would silently drop
	// that sample.
	firstTokenSet bool
	usage      Usage

	// explicitModel distinguishes "the client asked for this model" from "we
	// substituted the backend's own model", which matters when reading logs.
	explicitModel string
}

// Usage mirrors sse.Usage without importing it here, keeping the record type
// free of the streaming package.
type Usage struct {
	Prompt     int
	Completion int
	Cached     int
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &record{
		start:     time.Now(),
		route:     routeLabel(r.URL.Path),
		path:      r.URL.Path,
		requestID: requestID(r),
		status:    http.StatusOK,
		outcome:   metrics.OutcomeSuccess,
	}

	defer func() {
		// A panic in any handler goroutine would otherwise kill the process.
		// Recover, report, and answer 500 so one malformed request cannot take
		// down a shared gateway.
		if rv := recover(); rv != nil {
			rec.outcome = metrics.OutcomeInternal
			rec.reason = "panic"
			if rec.status == http.StatusOK {
				rec.status = http.StatusInternalServerError
			}
			p.log.Error("panic recovered",
				slog.String("request_id", rec.requestID),
				slog.Any("panic", rv),
				slog.String("path", rec.path),
				slog.String("stack", string(debug.Stack())),
			)
			writeError(w, http.StatusInternalServerError, TypeInternal, "internal gateway error")
		}

		elapsed := time.Since(rec.start)
		p.metrics.ObserveRequest(rec.route, rec.upstream, rec.model, rec.status, rec.outcome, elapsed)
		if !rec.usage.IsEmpty() {
			p.metrics.ObserveTokens(rec.upstream, rec.model, rec.usage.Prompt, rec.usage.Completion, rec.usage.Cached)
			p.metrics.ObserveUpstreamAttempt(rec.upstream, rec.status, rec.outcome, elapsed)
		} else {
			p.metrics.ObserveUpstreamAttempt(rec.upstream, rec.status, rec.outcome, elapsed)
		}
		if rec.frames > 0 {
			p.metrics.ObserveStreamFrames(rec.upstream, rec.frames, rec.respBytes)
		}
		if rec.firstTokenSet {
			p.metrics.ObserveFirstToken(rec.upstream, rec.model, rec.firstToken)
		}

		level := slog.LevelInfo
		switch rec.outcome {
		case metrics.OutcomeSuccess:
		case metrics.OutcomeCanceled:
			level = slog.LevelDebug
		default:
			level = slog.LevelWarn
		}
		attrs := []any{
			slog.String("request_id", rec.requestID),
			slog.String("route", rec.route),
			slog.String("upstream", rec.upstream),
			slog.String("model", rec.model),
			slog.Bool("stream", rec.stream),
			slog.Int("status", rec.status),
			slog.String("outcome", string(rec.outcome)),
			slog.Int64("req_bytes", rec.reqBytes),
			slog.Int64("resp_bytes", rec.respBytes),
			slog.Duration("elapsed", elapsed),
		}
		if rec.firstTokenSet {
			attrs = append(attrs, slog.Duration("first_token", rec.firstToken))
		}
		if rec.frames > 0 {
			attrs = append(attrs, slog.Int64("frames", rec.frames))
		}
		if !rec.usage.IsEmpty() {
			attrs = append(attrs,
				slog.Int("prompt_tokens", rec.usage.Prompt),
				slog.Int("completion_tokens", rec.usage.Completion),
				slog.Int("cached_tokens", rec.usage.Cached),
			)
			if p.pricing != nil {
				attrs = append(attrs, slog.Float64("cost_usd", p.pricing.CostUSD(rec.model, rec.usage.Prompt, rec.usage.Completion)))
			}
		}
		if rec.reason != "" {
			attrs = append(attrs, slog.String("reason", rec.reason))
		}
		p.log.Log(r.Context(), level, "request", attrs...)
	}()

	p.serve(w, r, rec)
}

func (r *Usage) IsEmpty() bool {
	return r.Prompt == 0 && r.Completion == 0 && r.Cached == 0
}

// serve is the request pipeline. The deferred recorder in ServeHTTP wraps it.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, rec *record) {
	if !isCompletionPath(r.URL.Path) {
		// Non-completion endpoints (/v1/models, /health) are proxied too, so a
		// client can point one base_url at the gateway and reach everything.
		if !isProxiablePath(r.URL.Path) {
			rec.status = http.StatusNotFound
			rec.outcome = metrics.OutcomeBadRequest
			rec.reason = "unsupported path"
			writeError(w, http.StatusNotFound, TypeBadRequest,
				"unsupported path "+r.URL.Path+"; InferGate proxies OpenAI-compatible endpoints only")
			return
		}
	}

	body, tooLarge, err := p.readBody(r)
	if err != nil {
		rec.status = http.StatusBadRequest
		rec.outcome = metrics.OutcomeBadRequest
		rec.reason = "read body"
			writeError(w, http.StatusBadRequest, TypeBadRequest, "could not read request body: "+err.Error())
		return
	}
	if tooLarge {
		rec.status = http.StatusRequestEntityTooLarge
		rec.outcome = metrics.OutcomeBadRequest
		rec.reason = "body too large"
		writeError(w, http.StatusRequestEntityTooLarge, TypeBodyTooLarge,
			"request body exceeds the configured limit")
		return
	}
	rec.reqBytes = int64(len(body))

	parsed := inspectRequest(body)
	rec.model = parsed.Model
	rec.explicitModel = parsed.Model
	rec.stream = parsed.Stream && isStreamPath(r.URL.Path)

	target, err := p.upstreams.Resolve(parsed.Model, strings.TrimSpace(r.Header.Get(HeaderUpstream)))
	if err != nil {
		// An unroutable model is the caller's problem, not an upstream outage:
		// answering 502 here would blame the backend and trip circuit breakers
		// for a typo. 400 with the model named is the actionable answer.
		rec.status = http.StatusBadRequest
		rec.outcome = metrics.OutcomeBadRequest
		rec.reason = "no upstream"
		writeError(w, http.StatusBadRequest, TypeNoUpstream, err.Error())
		return
	}
	rec.upstream = target.Name

	// A backend that names a specific model cannot serve an arbitrary alias:
	// rewrite the request so the client may ask for a canonical name (say
	// "gpt-4o") and still be served by the backend that owns that capability.
	if model, changed := p.resolveModelName(target, parsed.Model); changed {
		rewritten, rerr := rewriteModel(body, model)
		if rerr != nil {
			rec.status = http.StatusBadRequest
			rec.outcome = metrics.OutcomeBadRequest
			rec.reason = "rewrite model"
			writeError(w, http.StatusBadRequest, TypeBadRequest, rerr.Error())
			return
		}
		body = rewritten
		rec.model = model
	}

	req, client, cancel, err := p.buildRequest(r, target, body)
	if err != nil {
		rec.status = http.StatusInternalServerError
		rec.outcome = metrics.OutcomeInternal
		rec.reason = "build request"
		writeError(w, http.StatusInternalServerError, TypeInternal, err.Error())
		return
	}
	defer cancel()

	attemptStart := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		p.writeTransportError(w, r, rec, err)
		p.metrics.ObserveUpstreamAttempt(target.Name, 0, rec.outcome, time.Since(attemptStart))
		return
	}
	defer resp.Body.Close()

	rec.status = resp.StatusCode
	forwardResponseHeaders(w.Header(), resp.Header)
	w.Header().Set(HeaderUpstreamName, target.Name)
	w.Header().Set(HeaderAttempt, "1")
	w.Header().Set(HeaderRequestID, rec.requestID)

	if resp.StatusCode >= 400 {
		p.passThroughError(w, resp, rec)
		p.metrics.ObserveUpstreamAttempt(target.Name, resp.StatusCode, metrics.OutcomeUpstreamErr, time.Since(attemptStart))
		return
	}

	if rec.stream && isEventStream(resp.Header.Get("Content-Type")) {
		rec.outcome = p.relayStream(w, r, resp, target, rec)
	} else {
		rec.outcome = p.copyWhole(w, r, resp, rec)
	}
	p.metrics.ObserveUpstreamAttempt(target.Name, resp.StatusCode, rec.outcome, time.Since(attemptStart))
}

// readBody reads the request body up to the configured cap.
//
// The limit is enforced by reading cap+1 bytes rather than trusting
// Content-Length, because a chunked request carries no length and a lying
// Content-Length must not be able to make the gateway allocate arbitrarily.
func (p *Proxy) readBody(r *http.Request) (body []byte, tooLarge bool, err error) {
	if r.Body == nil {
		return nil, false, nil
	}
	limited := io.LimitReader(r.Body, p.maxBody+1)
	body, err = io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > p.maxBody {
		return nil, true, nil
	}
	return body, false, nil
}

// buildRequest clones the inbound request into an outbound one and returns the
// HTTP client that should perform it.
//
// # Why the client's context is not used directly
//
// Go's http.Server cancels the inbound Request.Context when the client
// disconnects, when the handler returns, and when reading the request body
// fails. The first is exactly what we want; the second happens after we are
// done; the third is actively hostile to a streaming gateway, because a client
// that half-sends a body and then waits would abort a generation that is
// already running.
//
// So the outbound context is derived from Background with the gateway's own
// deadline, and a small watcher propagates client disconnects explicitly. The
// result is one place that decides how long an upstream exchange may run, which
// is also what makes the timeout observable and tunable.
func (p *Proxy) buildRequest(r *http.Request, target *upstream.Target, body []byte) (*http.Request, *http.Client, context.CancelFunc, error) {
	base := context.Background()
	// One deadline for the whole upstream exchange. The cancel function is
	// returned to the caller (folded into cleanup) so a context.WithTimeout
	// timer is never leaked for the lifetime of the request.
	deadlineCancel := context.CancelFunc(func() {})
	if p.upstreamTTL > 0 {
		base, deadlineCancel = context.WithTimeout(base, p.upstreamTTL)
	}
	ctx, cancel := context.WithCancel(base)
	release := func() {
		cancel()
		deadlineCancel()
	}

	// Propagate client disconnect and server shutdown. The watcher exits as
	// soon as either side is done, so no goroutine outlives its request.
	clientCtx := r.Context()
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-clientCtx.Done():
			cancel()
		case <-stopWatch:
		}
	}()

	outURL := target.BaseURL + r.URL.Path
	if raw := r.URL.RawQuery; raw != "" {
		outURL += "?" + raw
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, outURL, bytes.NewReader(body))
	if err != nil {
		close(stopWatch)
		release()
		return nil, nil, func() {}, err
	}

	copyHeaders(req.Header, r.Header, true)
	req.Header.Del("Host")
	req.Host = ""

	// Credential policy: a configured backend key always wins (the gateway is
	// the credential holder); otherwise the caller's Authorization is passed
	// through, which is what makes a keyless local vLLM or an operator's own
	// key work without reconfiguration.
	if target.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
	} else if r.Header.Get("Authorization") == "" {
		req.Header.Del("Authorization")
	}
	req.Header.Set("X-Request-Id", requestID(r))

	transport := p.transportFor(target)
	if transport == nil {
		transport = target.Transport
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	// A per-request client is the price of per-backend transports. It is a
	// small value with no allocation beyond itself; the transport underneath
	// (and therefore the connection pool) is shared across requests.
	client := *p.client
	client.Transport = transport

	cleanup := func() {
		close(stopWatch)
		release()
	}
	return req, &client, cleanup, nil
}

// writeTransportError maps a failed attempt onto a client-visible error.
func (p *Proxy) writeTransportError(w http.ResponseWriter, r *http.Request, rec *record, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		rec.outcome = metrics.OutcomeTimeout
		rec.status = http.StatusGatewayTimeout
		rec.reason = "upstream timeout"
		writeError(w, http.StatusGatewayTimeout, TypeTimeout, "upstream did not respond before the gateway deadline")
	case errors.Is(err, context.Canceled) || r.Context().Err() != nil:
		rec.outcome = metrics.OutcomeCanceled
		rec.status = 499 // non-standard, nginx convention: client closed request
		rec.reason = "client canceled"
	default:
		rec.outcome = metrics.OutcomeUpstreamErr
		rec.status = http.StatusBadGateway
		rec.reason = err.Error()
		writeError(w, http.StatusBadGateway, TypeBadGateway, "upstream request failed: "+err.Error())
	}
}

// passThroughError forwards an upstream 4xx/5xx body verbatim.
func (p *Proxy) passThroughError(w http.ResponseWriter, resp *http.Response, rec *record) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, p.maxBody))
	if err != nil {
		rec.outcome = metrics.OutcomeUpstreamErr
		rec.reason = "read upstream error body"
		writeError(w, http.StatusBadGateway, TypeBadGateway, "could not read upstream error body")
		return
	}
	rec.respBytes = int64(len(body))
	rec.outcome = metrics.OutcomeUpstreamErr
	// Client errors (4xx) are the client's fault, not an upstream outage. The
	// distinction drives the error-rate SLO and any circuit breaker in M1.
	if resp.StatusCode < 500 {
		rec.outcome = metrics.OutcomeBadRequest
	} else {
		rec.outcome = metrics.OutcomeUpstreamErr
	}
	rec.reason = "upstream status " + resp.Status
	writeRawError(w, resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// copyWhole streams a non-SSE body to the client, optionally inspecting it for
// usage.
func (p *Proxy) copyWhole(w http.ResponseWriter, r *http.Request, resp *http.Response, rec *record) metrics.Outcome {
	w.WriteHeader(resp.StatusCode)

	// Only JSON responses of a bounded size are buffered for accounting. A
	// non-JSON body (a file download, an audio stream) is copied straight
	// through: the gateway's job is to relay, not to rewrite.
	if isJSON(resp.Header.Get("Content-Type")) {
		body, err := io.ReadAll(io.LimitReader(resp.Body, p.maxBody+1))
		if err != nil {
			rec.respBytes = int64(len(body))
			_, _ = w.Write(body)
			rec.reason = "read upstream body: " + err.Error()
			return metrics.OutcomeUpstreamErr
		}
		rec.respBytes = int64(len(body))
		if _, werr := w.Write(body); werr != nil {
			rec.reason = "write to client: " + werr.Error()
			return metrics.OutcomeCanceled
		}
		p.accountJSON(rec, body)
		return metrics.OutcomeSuccess
	}

	n, err := io.Copy(w, resp.Body)
	rec.respBytes = n
	if err != nil {
		rec.reason = "relay body: " + err.Error()
		if r.Context().Err() != nil {
			return metrics.OutcomeCanceled
		}
		return metrics.OutcomeUpstreamErr
	}
	return metrics.OutcomeSuccess
}

// accountJSON extracts token usage from a non-streaming completion body.
func (p *Proxy) accountJSON(rec *record, body []byte) {
	usage, model, ok := usageFromJSON(body)
	if !ok {
		return
	}
	rec.usage = usage
	// The provider's reported model is authoritative for pricing: an alias may
	// resolve to a differently priced snapshot.
	if model != "" {
		rec.model = model
	}
}

// isJSON reports whether a Content-Type denotes JSON.
func isJSON(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.Contains(strings.ToLower(contentType), "json")
	}
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// isEventStream reports whether a Content-Type is SSE.
func isEventStream(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.Contains(strings.ToLower(contentType), "event-stream")
	}
	return mediaType == "text/event-stream"
}

// usageFromJSON adapts sse.UsageFromResponse into this package's record type.
func usageFromJSON(body []byte) (Usage, string, bool) {
	var env struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Usage{}, "", false
	}
	u := Usage{
		Prompt:     env.Usage.PromptTokens,
		Completion: env.Usage.CompletionTokens,
	}
	if env.Usage.PromptTokensDetails != nil {
		u.Cached = env.Usage.PromptTokensDetails.CachedTokens
	}
	return u, env.Model, true
}

// resolveModelName decides which model name to send to target.
//
// Returns changed=false when the client's own model name is already acceptable.
// A backend whose model list is exactly {"/"} is a pass-through (vLLM, Ollama):
// rewriting would break it, so the client's name is preserved verbatim.
func (p *Proxy) resolveModelName(target *upstream.Target, requested string) (string, bool) {
	patterns := target.ModelPatterns()
	if target.IsCatchAll() && len(patterns) == 1 {
		return requested, false
	}
	for _, pattern := range patterns {
		if strings.EqualFold(pattern, requested) {
			return requested, false
		}
	}
	// No exact match: pick the backend's first concrete model so the request is
	// servable. A request with no model at all is left alone and let upstream
	// reject it, which produces a clearer provider error than a guess.
	if requested == "" {
		return requested, false
	}
	for _, pattern := range patterns {
		if pattern != "/" && pattern != "" {
			return pattern, true
		}
	}
	return requested, false
}

// forwardResponseHeaders copies upstream response headers to the client.
func forwardResponseHeaders(dst, src http.Header) {
	copyHeaders(dst, src, false)
	// The body is re-framed by Go's server, so the upstream framing headers must
	// go: a stale Content-Length on a stream truncates the response, and a
	// forwarded Content-Encoding double-decodes an already-decompressed body.
	dst.Del("Content-Length")
	dst.Del("Content-Encoding")
	dst.Del("Transfer-Encoding")
}

// requestID returns the correlation id for a request, generating one when the
// client did not supply it. A gateway that does not mint ids cannot correlate
// its logs with a client-visible failure.
func requestID(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get(HeaderRequestID)); id != "" {
		return id
	}
	return newRequestID()
}

// isProxiablePath whitelists the OpenAI-compatible surface InferGate relays.
//
// A reverse proxy that forwards any path to a paid API is an open relay: an
// attacker who reaches the gateway can reach every endpoint the backend
// exposes. Enumerating the supported surface is the cheapest effective control.
func isProxiablePath(path string) bool {
	p := strings.TrimSuffix(path, "/")
	switch {
	case p == "", p == "/":
		return true
	case strings.HasSuffix(p, "/chat/completions"),
		strings.HasSuffix(p, "/completions"),
		strings.HasSuffix(p, "/embeddings"),
		strings.HasSuffix(p, "/models"),
		strings.HasSuffix(p, "/responses"):
		return true
	default:
		return false
	}
}
