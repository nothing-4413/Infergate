package gateway

// M3 token quota and cost governance: the request-path half of the budget
// system.
//
// The accounting lives in internal/quota; this file is the seam. It answers
// three questions and nothing else:
//
//  1. May this request proceed, and if it may, in what shape? (admitQuota)
//  2. What did it actually spend? (settleQuota)
//  3. What does the caller get told? (headers and the 429 envelope)
//
// The order in the pipeline is the design: admission runs before the cache
// lookup, because a served-from-cache request still IS a request - it consumes
// a per-minute slot and it must be visible in the tenant's report - even though
// it costs no provider tokens. Settling from the deferred block in ServeHTTP
// means one settle per admitted request on every exit path, including a panic.
//
// The reservation is deliberately an ESTIMATE that is corrected afterwards. A
// gateway that charged only what it could measure would let one tenant open a
// thousand concurrent streams against a budget of one, because nothing is
// measured until the responses come back; reserving the ceiling first and
// returning the unused part at settlement is what makes a budget hold under
// concurrency.

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/quota"
)

// QuotaGate is the admission controller the gateway consults.
//
// It is an interface rather than the concrete *quota.Manager so that the
// gateway's own tests can drive every branch - allowed, degraded, refused,
// store failure - without a Redis, an embedder or a clock.
type QuotaGate interface {
	// Enabled reports whether budgets are enforced at all. A disabled gate is
	// never consulted, so its presence costs nothing per request.
	Enabled() bool

	// Admit reserves budget for a request. A non-nil error always means the
	// store could not be read; the Decision still says what to do about it,
	// because fail-open and fail-closed are both legitimate configurations.
	Admit(ctx context.Context, tenant, session string, est quota.Estimate) (*quota.Decision, error)

	// Settle charges real usage and returns whatever Admit over-reserved.
	Settle(ctx context.Context, res *quota.Reservation, usage quota.Usage) error

	// Release gives a reservation back untouched. It is what a request that
	// never reached a provider settles with.
	Release(ctx context.Context, res *quota.Reservation) error
}

// The manager must satisfy the gateway's seam; if either side drifts, this
// fails to compile rather than at the first request of a deployment.
var _ QuotaGate = (*quota.Manager)(nil)

const (
	// defaultQuotaCharsPerToken converts an inbound body into a prompt-token
	// estimate. Four is the usual rule of thumb for English text and JSON
	// framing; CJK costs more per character, which is why it is a knob.
	defaultQuotaCharsPerToken = 4

	// defaultQuotaCompletionTokens is the completion reservation when the
	// caller named no ceiling of its own.
	defaultQuotaCompletionTokens = 256

	// quotaSettleTimeout bounds the post-response bookkeeping. The client has
	// already been answered by the time this runs, so a slow store must not
	// hold a handler goroutine (or a shutdown) hostage.
	quotaSettleTimeout = 2 * time.Second
)

// admitQuota reserves budget for a request and applies the verdict.
//
// It returns the (possibly rewritten) body and its parse, plus whether the
// caller may continue. A refusal has already been written to w.
func (p *Proxy) admitQuota(w http.ResponseWriter, r *http.Request, rec *record, body []byte, parsed parsedRequest) ([]byte, parsedRequest, bool) {
	if p.quota == nil || !p.quota.Enabled() {
		return body, parsed, true
	}

	tenant := tenantFor(r)
	session := strings.TrimSpace(r.Header.Get(HeaderSession))
	rec.tenant = tenant
	if session != "" {
		rec.session = session
	}

	est := p.quotaEstimate(parsed, body)
	dec, err := p.quota.Admit(r.Context(), tenant, session, est)
	if dec == nil {
		// A gate that returns nothing has failed in the only way this code
		// cannot interpret. Treat it as a store failure rather than as consent.
		dec = &quota.Decision{Allowed: false, Action: quota.ActionReject, Reason: quota.ReasonStoreError}
	}

	if !dec.Allowed && dec.Reason == quota.ReasonStoreError {
		// The budget could not be read. Fail-closed (the default) refuses the
		// request because a limit that cannot be read is not a limit; fail-open
		// arrives here as Allowed with the same reason and is logged loudly.
		if err == nil {
			err = errQuotaStore
		}
		if dec.Allowed {
			p.log.Warn("quota: store unavailable, admitting under fail_open",
				"request_id", rec.requestID, "tenant", tenant, "error", err.Error())
		} else {
			rec.status = http.StatusServiceUnavailable
			rec.outcome = metrics.OutcomeInternal
			rec.reason = "quota store error"
			setQuotaHeaders(w, dec)
			p.log.Error("quota: store unavailable, refusing request",
				"request_id", rec.requestID, "tenant", tenant, "error", err.Error())
			writeError(w, http.StatusServiceUnavailable, TypeQuotaStore,
				"quota accounting is unavailable, so the request was not sent upstream")
			return body, parsed, false
		}
	}

	rec.quotaAction = dec.Action
	rec.quotaReason = dec.Reason
	rec.quotaLimit, rec.quotaUsed, rec.quotaRequested = dec.Limit, dec.Used, dec.Requested
	setQuotaHeaders(w, dec)

	if !dec.Allowed {
		rec.status = http.StatusTooManyRequests
		rec.outcome = metrics.OutcomeRateLimited
		rec.reason = "quota exceeded: " + dec.Reason
		// Always answer with a backoff hint, even when the decision carries no
		// window: a 429 without Retry-After leaves every SDK to invent one, and
		// the invented one is usually "retry immediately".
		seconds := 1
		if dec.RetryAfter > 0 {
			seconds = int(math.Ceil(dec.RetryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeError(w, http.StatusTooManyRequests, TypeQuotaExceeded, quotaMessage(tenant, dec))
		return body, parsed, false
	}

	// The request is admitted; remember the lease so the deferred settle has
	// something to settle. It is stored only for an admitted request: a refusal
	// has already released its reservation inside the manager.
	rec.quotaRes = dec.Reservation

	if dec.Action != quota.ActionDegrade {
		return body, parsed, true
	}

	// Degrade: keep serving, but smaller. Both levers are optional and both are
	// applied to the body the operator is about to send upstream, so the
	// provider - not the gateway - is what actually spends less.
	if dec.DowngradeModel != "" && !strings.EqualFold(dec.DowngradeModel, parsed.Model) {
		rewritten, rerr := rewriteModel(body, dec.DowngradeModel)
		if rerr != nil {
			rec.quotaReason = dec.Reason + "; downgrade failed: " + rerr.Error()
			p.log.Warn("quota: could not rewrite the model for a degraded request",
				"request_id", rec.requestID, "tenant", tenant, "error", rerr.Error())
		} else {
			body = rewritten
			parsed = inspectRequest(body)
			rec.model = parsed.Model
			rec.degradedModel = dec.DowngradeModel
		}
	}
	if dec.MaxTokensCap > 0 {
		rewritten, rerr := rewriteMaxTokens(body, dec.MaxTokensCap)
		if rerr != nil {
			rec.quotaReason = dec.Reason + "; max_tokens cap failed: " + rerr.Error()
			p.log.Warn("quota: could not cap max_tokens for a degraded request",
				"request_id", rec.requestID, "tenant", tenant, "error", rerr.Error())
		} else {
			body = rewritten
			parsed = inspectRequest(body)
		}
	}
	return body, parsed, true
}

// quotaEstimate turns a request into the reservation it needs.
//
// Prompt tokens are estimated from the body length and completion tokens from
// the caller's own max_tokens when it named one - the caller's ceiling is a
// better upper bound than any constant, and reserving less than the request can
// spend would let a burst through a budget that is nominally exhausted. The
// estimate is corrected at settlement; this is a lease, not a charge.
func (p *Proxy) quotaEstimate(parsed parsedRequest, body []byte) quota.Estimate {
	charsPerToken := p.quotaCharsPerToken
	if charsPerToken <= 0 {
		charsPerToken = defaultQuotaCharsPerToken
	}
	prompt := len(body) / charsPerToken
	if prompt < 1 {
		prompt = 1
	}
	completion := p.quotaCompletionTokens
	if completion <= 0 {
		completion = defaultQuotaCompletionTokens
	}
	if parsed.MaxTokens > 0 {
		completion = parsed.MaxTokens
	}

	var costMicros int64
	if p.pricing != nil {
		costMicros = int64(math.Round(p.pricing.CostUSD(parsed.Model, prompt, completion) * 1e6))
	}
	return quota.Estimate{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CostMicros:       costMicros,
		Requests:         1,
	}
}

// settleQuota ends the lease Admit opened, on every exit path.
//
// It runs after the response has been delivered, from a context detached from
// the caller's: a client that disconnected mid-stream must still be accounted
// for, and settling is the one write that must not be skipped because a socket
// closed.
func (p *Proxy) settleQuota(ctx context.Context, rec *record) {
	if p.quota == nil || rec.quotaRes == nil {
		return
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), quotaSettleTimeout)
	defer cancel()

	switch {
	case rec.servedFromCache:
		// A replayed answer costs the provider nothing, so only the request
		// count settles. This is why admission sits before the cache: a hit is
		// free but it is not invisible.
		p.finishQuota(sctx, rec, quota.Usage{Requests: 1})
	case rec.attempts == 0:
		// Nothing reached a provider - an unroutable model, a body the gateway
		// refused, a caller that vanished first. Give everything back,
		// including the slot.
		if err := p.quota.Release(sctx, rec.quotaRes); err != nil {
			rec.quotaSettleError = err.Error()
		}
	default:
		usage := quota.Usage{
			PromptTokens:     rec.usage.Prompt,
			CompletionTokens: rec.usage.Completion,
			Requests:         1,
		}
		if p.pricing != nil {
			usage.CostMicros = int64(math.Round(p.pricing.CostUSD(rec.model, rec.usage.Prompt, rec.usage.Completion) * 1e6))
		}
		// A stream that was cut off mid-flight reports no usage, so the
		// reservation is returned rather than charged. The gateway cannot know
		// what the provider billed for a partial generation, and inventing a
		// number would corrupt the one figure an operator uses to compare
		// spend against the provider's invoice; the release is visible in
		// Stats.ReleasedTokens.
		p.finishQuota(sctx, rec, usage)
	}
}

func (p *Proxy) finishQuota(ctx context.Context, rec *record, usage quota.Usage) {
	if err := p.quota.Settle(ctx, rec.quotaRes, usage); err != nil {
		rec.quotaSettleError = err.Error()
	}
}

// setQuotaHeaders describes the verdict to the caller.
//
// It is set for every governed request, not only for a refusal: "allowed, and
// here is how much of the budget was left" is the difference between a limit an
// operator can plan against and one they discover from a 429.
func setQuotaHeaders(w http.ResponseWriter, dec *quota.Decision) {
	h := w.Header()
	action := dec.Action
	if action == "" {
		action = quota.ActionAllow
	}
	h.Set(HeaderQuota, action)
	if dec.Reason != "" {
		h.Set(HeaderQuotaReason, dec.Reason)
	}
	if dec.Limit > 0 {
		h.Set(HeaderQuotaLimit, strconv.FormatInt(dec.Limit, 10))
		h.Set(HeaderQuotaUsed, strconv.FormatInt(dec.Used, 10))
	}
	if dec.Action == quota.ActionDegrade {
		if dec.DowngradeModel != "" {
			h.Set(HeaderQuotaModel, dec.DowngradeModel)
		}
		if dec.MaxTokensCap > 0 {
			h.Set(HeaderQuotaMaxTokens, strconv.Itoa(dec.MaxTokensCap))
		}
	}
}

// quotaMessage explains a refusal in the terms the caller can act on.
//
// The dimension, the limit and the usage are all named: an operator who sees
// only "quota exceeded" has to go and find out which of four budgets they hit,
// which is exactly the minute they will spend asking the gateway owner instead.
func quotaMessage(tenant string, dec *quota.Decision) string {
	var b strings.Builder
	b.WriteString("quota exceeded for tenant ")
	b.WriteString(strconv.Quote(tenant))
	b.WriteString(": ")
	b.WriteString(dimensionLabel(dec.Reason))
	if dec.Limit > 0 {
		b.WriteString(" budget of ")
		b.WriteString(strconv.FormatInt(dec.Limit, 10))
		b.WriteString(" is already at ")
		b.WriteString(strconv.FormatInt(dec.Used, 10))
		b.WriteString(", and this request asked for ")
		b.WriteString(strconv.FormatInt(dec.Requested, 10))
	}
	if dec.RetryAfter > 0 {
		b.WriteString("; the window resets in ")
		b.WriteString(dec.RetryAfter.Round(time.Second).String())
	}
	return b.String()
}

// dimensionLabel turns a machine reason into the phrase a human reads.
func dimensionLabel(reason string) string {
	switch reason {
	case quota.ReasonDailyTokens:
		return "daily token"
	case quota.ReasonDailyCost:
		return "daily spend"
	case quota.ReasonSessionTokens:
		return "per-session token"
	case quota.ReasonMinuteRPM:
		return "per-minute request"
	case quota.ReasonStoreError:
		return "accounting"
	default:
		return reason
	}
}

// errQuotaStore stands in for a gate that failed without saying why.
var errQuotaStore = quotaStoreError("quota: store unavailable")

type quotaStoreError string

func (e quotaStoreError) Error() string { return string(e) }
