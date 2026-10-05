package gateway

import (
	"net/http"
	"time"

	"github.com/infergate/infergate/internal/sessions"
)

// recordSession appends this request to the per-conversation ledger.
//
// The ledger answers questions the request log cannot: what did this agent
// conversation cost in total, which models did it use, how much of that was
// served from the cache or replayed from the idempotency store, and how many of
// its requests failed. Those are questions about a CONVERSATION, and a
// conversation is several requests spread across several providers, so the
// rollup has to happen here - where every request passes - rather than in any
// single provider path.
//
// Only completion routes are recorded. A budget and a cost rollup that counted
// /v1/models listings would report spend for traffic that is free, and the token
// dimension would be pure fiction on a request with no prompt.
func (p *Proxy) recordSession(r *http.Request, rec *record) {
	if p.sessions == nil || !isCompletionPath(rec.path) {
		return
	}
	req := sessions.Request{
		At:               rec.start,
		RequestID:        rec.requestID,
		Route:            rec.route,
		Upstream:         rec.upstream,
		Model:            rec.model,
		RequestedModel:   rec.explicitModel,
		Status:           rec.status,
		Outcome:          string(rec.outcome),
		Stream:           rec.stream,
		PromptTokens:     rec.usage.Prompt,
		CompletionTokens: rec.usage.Completion,
		CachedTokens:     rec.usage.Cached,
		ElapsedMS:        float64(time.Since(rec.start).Microseconds()) / 1000.0,
		Attempts:         rec.attempts,
		Tried:            append([]string(nil), rec.tried...),
		Cache:            rec.cacheStatus,
		Reason:           rec.reason,
		Replay:           rec.servedFromReplay,
	}
	if p.pricing != nil {
		req.CostUSD = p.pricing.CostUSD(rec.model, rec.usage.Prompt, rec.usage.Completion)
	}
	if rec.firstTokenSet {
		req.FirstTokenMS = float64(rec.firstToken.Microseconds()) / 1000.0
	}
	// An empty session id is not an error here: the ledger counts it separately
	// (Stats.NoSessionID) rather than inventing a one-request pseudo-session,
	// because a ledger full of those would hide the fact that the caller never
	// sent the header.
	p.sessions.Record(rec.tenant, rec.session, req)
}
