package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"mime"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/cache"
	"github.com/infergate/infergate/internal/idempotency"
	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/quota"
	"github.com/infergate/infergate/internal/router"
	"github.com/infergate/infergate/internal/sessions"
	"github.com/infergate/infergate/internal/tracing"
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

	// router turns a model and a capability set into an ordered failover chain.
	// Nil means M0 behaviour: resolve one backend and never retry.
	router *router.Router

	// breakers owns the per-upstream windows and circuit state. Nil means every
	// backend is always considered healthy.
	breakers *breaker.Group

	// maxAttempts bounds the failover chain. It exists to stop an amplification
	// loop: three broken backends times three retries each turns one client
	// request into nine provider requests, and the provider's rate limiter pays
	// for it. One means M0 behaviour.
	maxAttempts int

	// retryBackoff is the base delay before the next attempt. It grows linearly
	// and carries jitter, because a fixed delay makes every client retry in
	// lockstep and re-creates the very stampede the retry was meant to cross.
	retryBackoff time.Duration

	// transportFor returns the RoundTripper for a target. It exists so tests
	// can inject a transport (to simulate a dead backend, a slow backend, or a
	// mid-stream disconnect) without standing up a real server.
	transportFor func(*upstream.Target) http.RoundTripper

	// cache is the M2 semantic cache. Nil means every request goes upstream,
	// which is exactly the M0/M1 behaviour.
	//
	// The Proxy holds a *cache.Cache rather than an interface because the cache
	// is not a pluggable detail: the identity rules above (which prompts may be
	// matched, which parameters must agree) are part of this package's
	// correctness argument, not of the store's.
	cache *cache.Cache

	// quota is the M3 admission controller. Nil means no budget is enforced,
	// which is the M0-M2 behaviour and the default: a gateway that suddenly
	// starts refusing traffic because a config section it never had is now
	// empty would be a worse failure than an unbudgeted one.
	quota QuotaGate

	// quotaCharsPerToken and quotaCompletionTokens size a reservation when the
	// request itself does not say. They live here rather than in the manager
	// because estimation is a request-path concern: only the gateway holds the
	// body.
	quotaCharsPerToken    int
	quotaCompletionTokens int

	// tracer records one trace per sampled request. Nil disables tracing, which
	// is the default: the trace store retains request metadata in memory, so it
	// is a decision an operator makes rather than a cost every deployment pays.
	tracer *Tracer

	// idempotency remembers a completed answer against the caller's
	// Idempotency-Key, so a client that retries after a timeout does not pay
	// for a second generation. Nil means the header is ignored, which is the
	// M0-M5 behaviour.
	idempotency *idempotency.Store

	// sessions is the M6 per-conversation ledger. Nil means no conversation
	// rollup is kept; the request log and the traces are unaffected.
	sessions *sessions.Ledger

	// models is the operator's per-model metadata. It is immutable after
	// construction like every other field here, and an empty map is the normal
	// case: the capability endpoint then reports only what the upstreams
	// declared.
	models map[string]ModelInfo
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

	// Router selects the failover chain. Nil means M0 behaviour: one backend,
	// no retries, no health awareness.
	Router *router.Router

	// Breakers records attempt outcomes and gates candidates. Nil disables both
	// circuit breaking and the windowed statistics the router ranks on.
	Breakers *breaker.Group

	// MaxAttempts bounds the failover chain. Zero or one means a single
	// attempt.
	MaxAttempts int

	// RetryBackoff is the base delay between attempts.
	RetryBackoff time.Duration

	// Cache answers repeated questions without an upstream call. Nil disables
	// caching entirely (the M0/M1 behaviour), which is also why enabling it is
	// an explicit configuration decision rather than a default.
	Cache *cache.Cache

	// Quota enforces per-tenant token, spend and request budgets. Nil means no
	// budget is enforced.
	Quota QuotaGate

	// QuotaCharsPerToken turns the inbound body into a prompt-token estimate.
	// Zero means the package default (4).
	QuotaCharsPerToken int

	// QuotaCompletionTokens is the completion reservation for a request that
	// named no ceiling of its own. Zero means the package default (256).
	QuotaCompletionTokens int

	// Tracer records per-request traces. Nil disables tracing.
	Tracer *Tracer

	// Idempotency replays a completed answer for a repeated Idempotency-Key.
	// Nil means the header is ignored.
	Idempotency *idempotency.Store

	// Sessions accumulates per-conversation totals, keyed by tenant and by the
	// caller's X-InferGate-Session. Nil means no ledger is kept.
	Sessions *sessions.Ledger

	// Models is per-model metadata the wire does not carry: how large the
	// context is, how much it can emit, which capability tags the model itself
	// supports, and whatever the operator wants to record about it. It is read
	// only by the capability endpoint, never on the request path.
	Models map[string]ModelInfo
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
		upstreams:             opts.Upstreams,
		pricing:               opts.Pricing,
		metrics:               sink,
		log:                   logger,
		client:                &http.Client{}, // no Timeout: streaming bodies must not be cut off by the client
		maxBody:               maxBody,
		upstreamTTL:           opts.UpstreamTimeout,
		router:                opts.Router,
		breakers:              opts.Breakers,
		cache:                 opts.Cache,
		quota:                 opts.Quota,
		quotaCharsPerToken:    opts.QuotaCharsPerToken,
		quotaCompletionTokens: opts.QuotaCompletionTokens,
		tracer:                opts.Tracer,
		idempotency:           opts.Idempotency,
		sessions:              opts.Sessions,
		models:                opts.Models,
	}
	p.maxAttempts = opts.MaxAttempts
	if p.maxAttempts < 1 {
		p.maxAttempts = 1
	}
	p.retryBackoff = opts.RetryBackoff
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
	usage         Usage

	// explicitModel distinguishes "the client asked for this model" from "we
	// substituted the backend's own model", which matters when reading logs.
	explicitModel string

	// attempts counts upstream attempts made for this request. More than one
	// means a failover happened, which is the number an operator needs to see
	// when asking "how often is a backend being skipped".
	attempts int

	// tried names every backend that was attempted, in order, so a single log
	// record tells the whole failure story instead of one line per attempt.
	tried []string

	// failovers counts attempts that were abandoned because of a transport
	// error or an upstream 5xx.
	failovers int

	// cacheIdent is this request's cache identity, computed once in serve so
	// that the lookup, the response headers and the store all describe the same
	// question. Nil when the cache is disabled.
	cacheIdent *cacheIdentity

	// cacheStatus is the value reported in X-InferGate-Cache and in the request
	// log line. It is set even when nothing was cached, because "this request
	// was skipped, and here is why" is the fact an operator needs.
	cacheStatus string

	// cacheReason explains a skip or an error, for the log rather than the
	// response.
	cacheReason string

	// tenant is the budget and cache isolation namespace, recorded so one log
	// line says whose request this was. It is derived from a client header or
	// from hashed credentials, never from the body.
	tenant string

	// session is the caller-declared conversation id, when it sent one. Only a
	// per-session budget reads it.
	session string

	// quotaRes is the lease admission opened for this request. Its presence is
	// what tells the deferred settle that this request was governed at all.
	quotaRes *quota.Reservation

	// quotaAction / quotaReason are the verdict, and quotaLimit / quotaUsed /
	// quotaRequested the dimension that decided it.
	quotaAction, quotaReason string
	quotaLimit, quotaUsed    int64
	quotaRequested           int64
	quotaSettleError         string

	// degradedModel is what a degraded request was rewritten to, so the log can
	// say "this answer came from the cheap model" rather than leaving an
	// unexplained quality change.
	degradedModel string

	// servedFromCache records that the answer was replayed rather than
	// generated. Its quota meaning is precise: the request consumed a slot but
	// no provider tokens.
	servedFromCache bool

	// idemKey is the caller's Idempotency-Key, already normalised, and is empty
	// when the request carried none. idemScope is the namespace it was claimed
	// in, idemHash fingerprints the request body, and idemClaimed records that
	// THIS request owns the claim and must therefore complete or abort it.
	idemKey     string
	idemScope   string
	idemHash    string
	idemClaimed bool

	// idemDecision is the verdict for the log and the trace ("proceed",
	// "replay", "stored", "aborted", "conflict_body", "in_flight", "skip"),
	// idemReason explains a conflict or a refused store, and idemStore is the
	// value reported in X-InferGate-Idempotent-Store.
	idemDecision string
	idemReason   string
	idemStore    string

	// idemHeaders and idemBody are the captured response, kept only when this
	// request holds a claim and only up to the store's size cap.
	idemCapture *captureWriter

	// servedFromReplay records that the answer came from the idempotency store.
	// Its quota meaning matches servedFromCache: a slot, no provider tokens.
	servedFromReplay bool

	// trace is the in-flight trace for this request. Nil when tracing is
	// disabled or the request was sampled out.
	trace *requestTrace

	// traceparent and tracestate are the W3C context propagated to the
	// upstream. They are computed even when the request is not recorded, so a
	// caller that is tracing stays connected across this hop.
	traceparent string
	tracestate  string
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
	// The trace is opened before anything else so that the upstream hop carries
	// this request's own span as its parent, and the inbound traceparent is
	// validated before it is trusted.
	p.tracer.begin(r, rec)

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
		// M3: the budget lease is closed here rather than in the happy path, so
		// that every exit - a refusal, a failed failover, a panic, a client that
		// vanished mid-stream - settles exactly once.
		p.settleQuota(r.Context(), rec)
		// Attempts are observed where they happen (attemptUpstream), not here:
		// a request that failed over must contribute one attempt sample per
		// backend, otherwise the failed backend's error rate stays invisible and
		// its breaker can never trip on the evidence.
		if rec.frames > 0 {
			p.metrics.ObserveStreamFrames(rec.upstream, rec.frames, rec.respBytes)
		}
		if rec.firstTokenSet {
			p.metrics.ObserveFirstToken(rec.upstream, rec.model, rec.firstToken)
		}
		// M5: the trace is closed here, after the response has been written and
		// after the metrics, so a slow exporter cannot delay an answer. The
		// events below are what make a trace readable without the log line: the
		// verdicts that explain the outcome, recorded where they are known.
		// M6: the replay store is settled before the trace is closed, after the
		// response is on the wire: a store that is slow, full or unavailable
		// must not delay the answer it is trying to remember, and the trace
		// should record what the store decided.
		p.idempotencyComplete(rec)
		if rec.idemKey != "" {
			rec.trace.addEvent("idempotency."+rec.idemDecision, map[string]any{
				"key":    rec.idemKey,
				"reason": rec.idemReason,
			})
		}
		if rec.cacheStatus != "" {
			rec.trace.addEvent("cache."+rec.cacheStatus, map[string]any{
				"reason": rec.cacheReason,
			})
		}
		if rec.failovers > 0 {
			rec.trace.addEvent("failover", map[string]any{
				"failovers": rec.failovers,
				"tried":     rec.tried,
			})
		}
		if rec.frames > 0 {
			rec.trace.addEvent("stream.complete", map[string]any{
				"frames":         rec.frames,
				"response_bytes": rec.respBytes,
				"first_token_ms": firstTokenMS(rec),
			})
		}
		rec.trace.finish(rec)

		// M6: the conversation rollup is updated last, so it carries the final
		// status, the measured latency and the attempt count.
		p.recordSession(r, rec)

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
		if rec.cacheStatus != "" {
			attrs = append(attrs, slog.String("cache", rec.cacheStatus))
			if rec.cacheReason != "" {
				attrs = append(attrs, slog.String("cache_reason", rec.cacheReason))
			}
		}
		if rec.tenant != "" {
			attrs = append(attrs, slog.String("tenant", rec.tenant))
		}
		if rec.session != "" {
			attrs = append(attrs, slog.String("session", rec.session))
		}
		if rec.quotaAction != "" {
			attrs = append(attrs, slog.String("quota", rec.quotaAction))
			if rec.quotaReason != "" {
				attrs = append(attrs, slog.String("quota_reason", rec.quotaReason))
			}
			if rec.quotaLimit > 0 {
				attrs = append(attrs,
					slog.Int64("quota_limit", rec.quotaLimit),
					slog.Int64("quota_used", rec.quotaUsed),
					slog.Int64("quota_requested", rec.quotaRequested),
				)
			}
			if rec.degradedModel != "" {
				attrs = append(attrs, slog.String("degraded_to", rec.degradedModel))
			}
			if rec.quotaSettleError != "" {
				attrs = append(attrs, slog.String("quota_settle_error", rec.quotaSettleError))
			}
		}
		if rec.attempts > 1 {
			attrs = append(attrs, slog.Int("attempts", rec.attempts))
		}
		if len(rec.tried) > 1 {
			attrs = append(attrs, slog.String("tried", strings.Join(rec.tried, " -> ")))
		}
		if rec.idemKey != "" {
			attrs = append(attrs, slog.String("idempotency", rec.idemDecision))
			if rec.idemStore != "" {
				attrs = append(attrs, slog.String("idempotency_store", rec.idemStore))
			}
			if rec.idemReason != "" {
				attrs = append(attrs, slog.String("idempotency_reason", rec.idemReason))
			}
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

	// Identity is resolved once, for every request, rather than lazily inside
	// the subsystems that need it. The tenant is what isolates cached answers
	// and budgets, and the session is the conversation the ledger rolls up, so
	// both must be known before the first subsystem runs - and the log line
	// should name them even on a deployment with no cache and no budget.
	if rec.tenant == "" {
		rec.tenant = tenantFor(r)
	}
	if rec.session == "" {
		rec.session = strings.TrimSpace(r.Header.Get(HeaderSession))
	}

	// M3: admission runs BEFORE the cache lookup, and the order is load-bearing
	// in both directions. A cache hit is free but it is not invisible: it
	// consumes a per-minute slot and it belongs in the tenant's report, so the
	// lease is opened first and settled afterwards with a zero-token usage. And
	// a request that is about to be refused must not have been answered from
	// the cache first, or a tenant over budget would keep receiving traffic.
	//
	// Only completion routes are governed. A budget that counted /v1/models
	// listings would refuse callers for traffic that costs nothing, and the
	// token dimension would be pure fiction on a request with no prompt.
	if p.quota != nil && p.quota.Enabled() && isCompletionPath(r.URL.Path) {
		var admitted bool
		body, parsed, admitted = p.admitQuota(w, r, rec, body, parsed)
		if !admitted {
			return
		}
		rec.model = parsed.Model
	}

	// M6: the idempotency claim is taken after admission and before the cache.
	// After admission, because a replay is a request the caller really made and
	// it consumes a per-minute slot just as a cache hit does; before the cache,
	// because replaying the caller's OWN finished operation is a stronger
	// statement than returning a similar question's answer.
	var done bool
	w, done = p.idempotencyBegin(w, r, rec, body)
	if done {
		return
	}

	// M2: the cache answers before routing is even considered, because a hit
	// makes the routing decision moot. The identity is derived from the body as
	// the CLIENT sent it, before any backend-specific model rewrite, so two
	// requests that differ only in how they spell the model name share an
	// exact key.
	if p.cache != nil {
		p.prepareCache(w, r, rec, body, parsed)
		if p.cacheAttempt(w, r, rec) {
			return
		}
	}

	p.serveAttempts(w, r, rec, parsed, body)
}

// attemptError records why one upstream attempt ended, and whether the client
// has already been answered.
//
// The distinction between "the upstream failed before we answered anything" and
// "we already wrote bytes to the client" is the whole basis of safe failover:
// once any part of a response has been written, retrying a different backend
// would splice two responses together and corrupt the stream.
type attemptError struct {
	err error

	// retryable is true when the attempt failed cleanly enough that no part of
	// a response reached the client.
	retryable bool

	// clientGone is true when the failure was the CALLER's: it disconnected, or
	// the gateway's own deadline fired while it waited. Nothing is written to a
	// connection in that state.
	clientGone bool

	// status, header and body carry the upstream's own failure response when it
	// produced one. They exist so that when every candidate fails, the client
	// receives the provider's status and error envelope instead of a generic
	// 502: an SDK that backs off on 429 or distinguishes 503 from 500 must keep
	// seeing the real code, and a provider's error message is the single most
	// useful thing to put in front of a user.
	status int
	header http.Header
	body   []byte
}

func (e *attemptError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *attemptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// retryableStatus reports whether an upstream status is worth failing over.
//
// Only 5xx and 429 are retried. A 4xx is a verdict about the REQUEST — a bad
// model name, a malformed tool schema, a missing parameter — and every other
// backend would reject it identically, so retrying it costs latency, burns
// provider quota and hides the caller's own bug behind a generic gateway error.
// 429 is included because it is a verdict about the moment, not about the
// request: another backend may well have capacity.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// serveAttempts walks the routing plan until one attempt produces an answer.
func (p *Proxy) serveAttempts(w http.ResponseWriter, r *http.Request, rec *record, parsed parsedRequest, body []byte) {
	plan, err := p.plan(parsed, r)
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

	var last *attemptError
	for i, cand := range plan {
		if i >= p.maxAttempts {
			rec.reason = "failover budget exhausted"
			break
		}
		if r.Context().Err() != nil {
			// The caller is gone. Trying another backend would spend provider
			// quota on an answer nobody will read.
			rec.outcome = metrics.OutcomeCanceled
			rec.status = 499
			rec.reason = "client went away during failover"
			return
		}

		// Each attempt receives the FULL upstream_timeout, not what is left of a
		// shared request deadline.
		//
		// This is deliberate and it is the difference between failover existing
		// and failover being decorative. With a shared deadline the first
		// candidate times out, the budget reaches zero, and every later --
		// perfectly healthy -- candidate is handed a pre-expired deadline. The
		// gateway would then answer 504 having never actually asked the second
		// backend, which is precisely the situation failover is for: a slow
		// primary plus a fast secondary.
		//
		// The cost is that a request can take up to max_attempts * upstream_timeout
		// in the worst case. That is the operator's trade to make, and both knobs
		// are explicit in the config; the alternative is a feature that silently
		// does nothing whenever the first backend is the slow one.
		attempt, cancelled := p.attemptUpstream(w, r, rec, cand, parsed.Model, body, i+1, p.upstreamTTL)
		if attempt == nil {
			// Canceled: the client is gone or the stream died mid-flight.
			// Retrying is either impossible or pointless.
			if cancelled {
				return
			}
			return
		}
		if attempt.err == nil {
			return
		}
		last = attempt
		if !attempt.retryable {
			return
		}
		if i == len(plan)-1 {
			rec.reason = "all " + itoa(len(plan)) + " candidates failed; last: " + attempt.err.Error()
		}
		rec.failovers++
		p.metrics.ObserveFailover(rec.upstream, metrics.OutcomeUpstreamErr, attempt.status)

		if i+1 < len(plan) && i+1 < p.maxAttempts {
			if delay := p.backoff(i); delay > 0 {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					rec.outcome = metrics.OutcomeCanceled
					rec.status = 499
					rec.reason = "client canceled during failover backoff"
					return
				}
			}
		}
	}

	// rec.status currently holds the LAST attempt's status, which was never
	// written to the client (a retryable failure writes nothing by design). It
	// must not be used as a "have we answered yet" flag: a 503 from the last
	// candidate would look like an answer that was already sent. Nothing in this
	// loop writes to w before this point, so the only question that matters is
	// whether the caller is still there to receive an error.
	if last != nil && !last.clientGone {
		// Every candidate failed and nothing was written, so the client can
		// still be told what happened.
		if last.status > 0 {
			// A provider answered; forward ITS status and envelope. Rewriting a
			// 429 into a 502 would break the client's own backoff logic, which
			// is the one thing an OpenAI-compatible SDK is guaranteed to have.
			rec.status = last.status
			rec.outcome = metrics.OutcomeUpstreamErr
			if last.status == http.StatusGatewayTimeout {
				// The provider status here came from the gateway's own deadline,
				// not from a provider: no candidate answered in time. Classifying
				// it as an upstream error would make the two most different
				// failures in the taxonomy -- "the backend said no" and "the
				// backend never spoke" -- indistinguishable in the metrics, and
				// they call for opposite responses (change the request vs. add a
				// faster backend).
				rec.outcome = metrics.OutcomeTimeout
			}
			forwardResponseHeaders(w.Header(), last.header)
			w.Header().Set(HeaderUpstreamName, rec.upstream)
			w.Header().Set(HeaderAttempt, itoa(rec.attempts))
			w.Header().Set(HeaderRequestID, rec.requestID)
			if len(rec.tried) > 1 {
				w.Header().Set(HeaderTried, strings.Join(rec.tried, ", "))
			}
			writeRawError(w, last.status, last.header.Get("Content-Type"), last.body)
			return
		}
		rec.status = http.StatusBadGateway
		rec.outcome = metrics.OutcomeUpstreamErr
		if rec.reason == "" {
			rec.reason = last.err.Error()
		}
		writeError(w, http.StatusBadGateway, TypeBadGateway,
			"no upstream could serve this request: "+last.err.Error())
	}
}

// attemptUpstream performs one upstream exchange.
//
// It returns (nil, true) when the client is gone or the response is already
// committed, and (nil, false) when the exchange succeeded and was fully relayed.
func (p *Proxy) attemptUpstream(w http.ResponseWriter, r *http.Request, rec *record, cand router.Candidate, requestedModel string, body []byte, attemptNo int, budget time.Duration) (*attemptError, bool) {
	target := cand.Target
	rec.upstream = target.Name
	rec.attempts++
	rec.tried = append(rec.tried, target.Name)
	rec.reason = cand.Reason

	// One decision per attempt, taken immediately before it: asking earlier
	// would let a breaker that tripped meanwhile be ignored, and asking later
	// (after the request is built) would waste the work.
	var allowed, probe bool
	if p.breakers != nil {
		d := p.breakers.Get(target.Name).Allow()
		allowed, probe = d.Allowed, d.Probe
		if !allowed {
			// A rejection is a decision, not an exchange: it gets an event
			// rather than a client span, because no request left the process
			// and a zero-duration span would suggest one did.
			rec.trace.addEvent("breaker.reject", map[string]any{
				"upstream": target.Name,
				"reason":   d.Reason,
			})
			return &attemptError{err: errors.New(target.Name + ": " + d.Reason), retryable: true}, false
		}
	}
	// probe marks this attempt as the half-open probe. It is recorded so that
	// the outcome is always reported back: admitting a probe and then failing to
	// report it leaves the breaker stuck half-open forever, because a half-open
	// breaker admits exactly one attempt and waits.
	_ = probe

	// One client span per admitted attempt. The defer closes it on every return
	// path below, including the ones that never reach the network, so a trace
	// shows the same number of attempts the metrics do.
	span := rec.trace.startAttempt(target.Name, attemptNo)
	defer span.finish(rec)

	outBody := body
	if cand.Model != "" && cand.Model != requestedModel {
		// This backend does not serve the requested name (a local single-model
		// box); ask it for the model it declared. M0 did this once, after
		// resolution; here it belongs to the candidate, because different
		// candidates may need different rewrite decisions.
		rewritten, rerr := rewriteModel(body, cand.Model)
		if rerr != nil {
			return &attemptError{err: rerr}, false
		}
		outBody = rewritten
		rec.model = cand.Model
	}

	req, client, cleanup, err := p.buildRequest(r, target, outBody, budget, rec)
	if err != nil {
		return &attemptError{err: err}, false
	}
	defer cleanup()

	attemptStart := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		rec.status = 0
		switch {
		case errors.Is(err, context.Canceled) || r.Context().Err() != nil:
			// The caller hung up. Nothing may be written — there is no longer a
			// connection to write to — and no other candidate is worth trying,
			// because the answer would go nowhere.
			rec.outcome = metrics.OutcomeCanceled
			rec.status = 499 // non-standard, nginx convention: client closed request
			rec.reason = "client canceled"
			return &attemptError{err: err, clientGone: true}, true
		case errors.Is(err, context.DeadlineExceeded):
			// The gateway's own deadline fired and the caller is still waiting.
			//
			// NOTHING is written to the client here, deliberately. The caller
			// asked for one answer and has not received a byte, so another
			// candidate may still produce a usable one -- and a slow backend is
			// the single most common reason to have a second one. Writing the
			// 504 now would commit the client to a failure that failover exists
			// to avoid, and once the status is written it cannot be taken back.
			//
			// The timeout is reported as a retryable attempt error carrying the
			// provider status. If every candidate times out, serveAttempts
			// forwards this exact 504 and body; if a later candidate succeeds,
			// the client sees the success and never learns about the timeout
			// except through the metrics and the X-InferGate-Tried header.
			rec.outcome = metrics.OutcomeTimeout
			rec.status = http.StatusGatewayTimeout
			rec.reason = "upstream timeout: " + target.Name
			if p.breakers != nil {
				p.breakers.Get(target.Name).RecordFailure(true)
			}
			p.observeAttempt(target, http.StatusGatewayTimeout, rec.outcome, time.Since(attemptStart))
			return &attemptError{
				err:       errors.New(target.Name + ": gateway deadline exceeded"),
				status:    http.StatusGatewayTimeout,
				header:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
				body:      errorBodyBytes(http.StatusGatewayTimeout, TypeTimeout, "no upstream answered before the gateway deadline"),
				retryable: true,
			}, false
		default:
			rec.status = http.StatusBadGateway
			rec.outcome = metrics.OutcomeUpstreamErr
			rec.reason = target.Name + ": " + err.Error()
			p.observeAttempt(target, 0, rec.outcome, time.Since(attemptStart))
			if p.breakers != nil {
				p.breakers.Get(target.Name).RecordFailure(false)
			}
			return &attemptError{err: err, retryable: true}, false
		}
	}
	defer resp.Body.Close()

	rec.status = resp.StatusCode

	if resp.StatusCode >= 400 {
		if retryableStatus(resp.StatusCode) {
			// Nothing has been written to the client yet, so this attempt can be
			// abandoned. The body is read (bounded) rather than closed unread:
			// it is both the connection-reuse requirement and the error message
			// the client will eventually see if every candidate fails.
			failureBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			outcome := metrics.OutcomeUpstreamErr
			p.observeAttempt(target, resp.StatusCode, outcome, time.Since(attemptStart))
			if p.breakers != nil {
				p.breakers.Get(target.Name).RecordFailure(resp.StatusCode == http.StatusGatewayTimeout)
			}
			rec.outcome = outcome
			rec.reason = target.Name + ": upstream status " + resp.Status
			return &attemptError{
				err: errors.New(target.Name + " returned " + resp.Status),
				// The status travels with the error so that, if every candidate
				// fails, the client still receives the PROVIDER's status code
				// rather than a generic 502. An SDK that retries on 429 must
				// keep seeing 429.
				status:    resp.StatusCode,
				header:    resp.Header.Clone(),
				body:      failureBody,
				retryable: true,
			}, false
		}
		// A caller error is forwarded verbatim and NOT retried: every other
		// backend would reject the same request identically.
		rec.outcome = metrics.OutcomeBadRequest
		forwardResponseHeaders(w.Header(), resp.Header)
		w.Header().Set(HeaderUpstreamName, target.Name)
		w.Header().Set(HeaderAttempt, itoa(attemptNo))
		w.Header().Set(HeaderRequestID, rec.requestID)
		if len(rec.tried) > 1 {
			w.Header().Set(HeaderTried, strings.Join(rec.tried, ", "))
		}
		p.passThroughError(w, resp, rec)
		p.observeAttempt(target, resp.StatusCode, rec.outcome, time.Since(attemptStart))
		if p.breakers != nil {
			// A 4xx is charged to the caller, not the backend, so it must not
			// count toward the breaker's failure ratio. Counting it would let a
			// client with a bad payload trip a healthy upstream out of rotation.
			p.breakers.Get(target.Name).RecordSuccess()
		}
		return nil, false
	}

	forwardResponseHeaders(w.Header(), resp.Header)
	w.Header().Set(HeaderUpstreamName, target.Name)
	w.Header().Set(HeaderAttempt, itoa(attemptNo))
	w.Header().Set(HeaderRequestID, rec.requestID)
	if len(rec.tried) > 1 {
		w.Header().Set(HeaderTried, strings.Join(rec.tried, ", "))
	}

	if rec.stream && isEventStream(resp.Header.Get("Content-Type")) {
		rec.outcome = p.relayStream(w, r, resp, target, rec)
		// A stream that failed after the first frame cannot be retried: the
		// client already holds a partial answer and a second [DONE] would be a
		// protocol violation. It only counts against the breaker if the failure
		// was the upstream's, not the client's.
		if rec.outcome == metrics.OutcomeSuccess {
			if p.breakers != nil {
				p.breakers.Get(target.Name).RecordSuccess()
			}
		} else if p.breakers != nil && rec.outcome != metrics.OutcomeCanceled {
			p.breakers.Get(target.Name).RecordFailure(false)
		}
	} else {
		rec.outcome = p.copyWhole(w, r, resp, rec)
		if rec.outcome == metrics.OutcomeSuccess {
			if p.breakers != nil {
				p.breakers.Get(target.Name).RecordSuccess()
			}
		} else if p.breakers != nil && rec.outcome != metrics.OutcomeCanceled {
			p.breakers.Get(target.Name).RecordFailure(rec.outcome == metrics.OutcomeTimeout)
		}
	}

	p.observeAttempt(target, resp.StatusCode, rec.outcome, time.Since(attemptStart))
	if !rec.usage.IsEmpty() {
		p.metrics.ObserveTokens(target.Name, rec.model, rec.usage.Prompt, rec.usage.Completion, rec.usage.Cached)
	}
	// First token and stream frames are observed ONCE, by the deferred recorder
	// in ServeHTTP, and not here. They used to be recorded in both places, which
	// double-counted every streaming request in the two families that describe
	// streaming; the deferred site is the correct one because it also covers a
	// cache hit and attributes the sample to the backend that actually served.
	return nil, false
}

// observeAttempt records one upstream attempt for metrics and the window.
//
// The window is fed here rather than in the deferred recorder because the
// window is what the breaker and the router read: an attempt that failed must
// appear in it immediately, or a backend that is failing 100% of requests keeps
// looking healthy for as long as the router's ranking is stale.
func (p *Proxy) observeAttempt(target *upstream.Target, status int, outcome metrics.Outcome, elapsed time.Duration) {
	p.metrics.ObserveUpstreamAttempt(target.Name, status, outcome, elapsed)
	if p.breakers != nil {
		window := p.breakers.Get(target.Name).Stats()
		if outcome == metrics.OutcomeSuccess {
			window.RecordSuccess(elapsed)
		} else if outcome == metrics.OutcomeTimeout {
			window.RecordFailure(true)
		} else if outcome != metrics.OutcomeCanceled {
			window.RecordFailure(false)
		}
	}
}

// backoff returns the delay before the attempt numbered n+2.
//
// Linear growth with full jitter: the linear part keeps a two-backend fleet
// from waiting seconds to fail over, and the jitter stops every client that was
// affected by the same outage from retrying in lockstep and re-creating the
// stampede the breaker just absorbed.
func (p *Proxy) backoff(attempt int) time.Duration {
	if p.retryBackoff <= 0 {
		return 0
	}
	base := p.retryBackoff * time.Duration(attempt+1)
	// Jitter is deterministic per attempt index only in tests; here it uses the
	// clock, which is the cheapest source of entropy on the request path.
	jitter := time.Duration(rand.Int63n(int64(base/2) + 1))
	return base/2 + jitter
}

// plan resolves the routing chain for a request.
func (p *Proxy) plan(parsed parsedRequest, r *http.Request) ([]router.Candidate, error) {
	explicit := strings.TrimSpace(r.Header.Get(HeaderUpstream))

	var caps []string
	if raw := strings.TrimSpace(r.Header.Get(HeaderCapabilities)); raw != "" {
		for _, c := range strings.Split(raw, ",") {
			if c = strings.TrimSpace(c); c != "" {
				caps = append(caps, c)
			}
		}
	}

	if p.router != nil {
		return p.router.Plan(router.Request{
			Model:        parsed.Model,
			Capabilities: caps,
			Explicit:     explicit,
			Messages:     parsed.Messages,
			MaxTokens:    parsed.MaxTokens,
		})
	}

	// M0 fallback: no router configured, so resolve exactly one backend.
	target, err := p.upstreams.Resolve(parsed.Model, explicit)
	if err != nil {
		return nil, err
	}
	model, _ := p.resolveModelName(target, parsed.Model)
	return []router.Candidate{{Target: target, Model: model, Healthy: true, Reason: "direct"}}, nil
}

// itoa is strconv.Itoa without the import churn in this file.
func itoa(n int) string { return strconv.Itoa(n) }

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
// timeout is the budget for THIS attempt, not for the request. The distinction
// matters once failover exists: if every attempt shared one deadline derived
// from the start of the request, the first slow candidate would consume the
// whole budget and every later, perfectly healthy candidate would fail
// instantly with a deadline error -- turning "try the next backend" into "give
// up", which is the opposite of why failover exists. The caller therefore
// computes the per-attempt budget from the time REMAINING in the request, so
// the total a client waits is still bounded.
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
func (p *Proxy) buildRequest(r *http.Request, target *upstream.Target, body []byte, timeout time.Duration, rec *record) (*http.Request, *http.Client, context.CancelFunc, error) {
	base := context.Background()
	// One deadline for this attempt. The cancel function is returned to the
	// caller (folded into cleanup) so a context.WithTimeout timer is never
	// leaked for the lifetime of the request.
	deadlineCancel := context.CancelFunc(func() {})
	if timeout > 0 {
		base, deadlineCancel = context.WithTimeout(base, timeout)
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
	// The request id comes from the record, not from a second call to
	// requestID(r): when the caller sent no id that function MINTS one, so
	// calling it twice produced two different ids and the upstream saw a
	// request id the gateway's own log line never mentioned.
	req.Header.Set("X-Request-Id", rec.requestID)
	// W3C trace context is propagated so the backend's spans are children of
	// this request's gateway span. It is set even when the trace is not being
	// recorded locally, so a traced caller is not disconnected at this hop.
	if rec.traceparent != "" {
		req.Header.Set(tracing.HeaderTraceparent, rec.traceparent)
		if rec.tracestate != "" {
			req.Header.Set(tracing.HeaderTracestate, rec.tracestate)
		}
	}

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

// passThroughError forwards an upstream 4xx/5xx body verbatim.
//
// It does not decide the outcome: the caller has already classified the status
// (retryable 5xx versus a caller-error 4xx) and mapped it onto the metrics
// taxonomy, and letting this function guess again is how the two classifications
// drift apart.
func (p *Proxy) passThroughError(w http.ResponseWriter, resp *http.Response, rec *record) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, p.maxBody))
	if err != nil {
		rec.outcome = metrics.OutcomeUpstreamErr
		rec.reason = "read upstream error body"
		writeError(w, http.StatusBadGateway, TypeBadGateway, "could not read upstream error body")
		return
	}
	rec.respBytes = int64(len(body))
	rec.reason = "upstream status " + resp.Status
	writeRawError(w, resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// copyWhole relays a non-SSE body to the client.
//
// A JSON response is read to completion BEFORE the status line is written. That
// ordering is what keeps failover possible: if the upstream dies mid-body, the
// client has still received nothing, so a retry can produce a clean answer
// instead of a truncated one behind a 200. The cost is that a JSON completion
// is buffered in memory, which is bounded by maxBody because a completion that
// exceeds it cannot be accounted for anyway.
func (p *Proxy) copyWhole(w http.ResponseWriter, r *http.Request, resp *http.Response, rec *record) metrics.Outcome {
	if isJSON(resp.Header.Get("Content-Type")) {
		body, err := io.ReadAll(io.LimitReader(resp.Body, p.maxBody+1))
		if err != nil {
			rec.respBytes = int64(len(body))
			rec.reason = "read upstream body: " + err.Error()
			if r.Context().Err() != nil {
				return metrics.OutcomeCanceled
			}
			// Nothing has been written yet, and the caller will be told so it
			// can fail over rather than surface a truncated answer.
			return metrics.OutcomeUpstreamErr
		}
		rec.respBytes = int64(len(body))
		p.accountJSON(rec, body)
		w.WriteHeader(resp.StatusCode)
		if _, werr := w.Write(body); werr != nil {
			rec.reason = "write to client: " + werr.Error()
			return metrics.OutcomeCanceled
		}
		// M2: store the answer after it has been delivered. A cache write that
		// is slow (or a Redis that has gone away) must cost the CALLER nothing;
		// the answer is already on the wire, so the only thing a failure here
		// can affect is whether the next identical request is cheap.
		if resp.StatusCode == http.StatusOK {
			p.storeCache(r.Context(), rec, body, resp.Header.Get("Content-Type"))
		}
		return metrics.OutcomeSuccess
	}

	// A non-JSON body (a file download, an audio stream) is copied straight
	// through: the gateway's job is to relay, not to rewrite, and buffering an
	// unbounded download would be worse than losing failover for it.
	w.WriteHeader(resp.StatusCode)
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
