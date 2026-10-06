package gateway

// M2 semantic cache: the part of the pipeline that reads and writes it.
//
// The cache sits between parsing and routing. That order is the whole point:
// a hit means there is nothing to route, no backend to choose, and no token to
// pay for. Everything here is therefore on the critical path and has to be
// cheap, and every failure of it has to be non-fatal - a broken store must
// degrade to "no cache", never to "no answer".

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/infergate/infergate/internal/cache"
	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/sse"
)

// prepareCache computes this request's cache identity and reports the verdict.
//
// It runs before the upstream call so that the response header can describe the
// decision even when the decision was "do not use the cache". A cache that only
// announces its hits looks identical to a cache that is misconfigured.
func (p *Proxy) prepareCache(w http.ResponseWriter, r *http.Request, rec *record, body []byte, parsed parsedRequest) {
	if p.cache == nil {
		return
	}
	ident := cacheIdentityFor(r, body, parsed)
	rec.cacheIdent = &ident

	if !ident.Eligible {
		if isCompletionPath(r.URL.Path) {
			rec.cacheStatus = cacheStatusSkip
			rec.cacheReason = ident.Reason
			w.Header().Set(HeaderCache, cacheStatusSkip)
		}
		return
	}

	// The policy verdict is computed here rather than inside Lookup so that the
	// header can carry it before any store or embedder is touched.
	d := p.cache.Cacheable(cacheRequest(ident))
	switch {
	case ident.Bypass:
		rec.cacheStatus = cacheDirectiveBypass
	case ident.Refresh:
		rec.cacheStatus = cacheDirectiveRefresh
	case !d.Lookup:
		rec.cacheStatus = cacheStatusSkip
	default:
		rec.cacheStatus = cacheStatusMiss
	}
	rec.cacheReason = d.Reason
	if rec.cacheStatus != cacheStatusMiss {
		w.Header().Set(HeaderCache, rec.cacheStatus)
	}
}

// cacheAttempt serves the request from the cache when it can. It reports
// whether the response is already complete, in which case the caller must not
// call an upstream.
func (p *Proxy) cacheAttempt(w http.ResponseWriter, r *http.Request, rec *record) bool {
	if p.cache == nil || rec.cacheIdent == nil || !rec.cacheIdent.Eligible {
		return false
	}
	if rec.cacheStatus != cacheStatusMiss {
		// Skipped by policy or by an explicit directive: nothing to read.
		return false
	}

	res, err := p.cache.Lookup(r.Context(), cacheRequest(*rec.cacheIdent))
	if err != nil {
		// A cache outage is not a request failure. The header says so, because
		// otherwise an operator sees a working gateway and an unused cache with
		// no hint of which of the two is at fault.
		rec.cacheStatus = cacheStatusError
		rec.cacheReason = err.Error()
		w.Header().Set(HeaderCache, cacheStatusError)
		p.log.Debug("cache lookup failed",
			slog.String("request_id", rec.requestID),
			slog.String("error", err.Error()),
		)
		return false
	}
	if !res.Hit {
		rec.cacheReason = res.Reason
		w.Header().Set(HeaderCache, cacheStatusMiss)
		return false
	}

	p.serveCached(w, r, rec, res)
	return true
}

// serveCached writes a stored answer to the client.
func (p *Proxy) serveCached(w http.ResponseWriter, r *http.Request, rec *record, res cache.Result) {
	e := res.Entry

	rec.cacheStatus = cacheStatusHitExact
	if res.Kind == cache.KindSemantic {
		rec.cacheStatus = cacheStatusHitSemantic
	}
	// M3: the quota settle reads this. A replayed answer consumed a request slot
	// and no provider tokens, and the difference matters: charging the stored
	// usage would bill a tenant for tokens the provider never generated, which
	// is the exact opposite of what a cache is for.
	rec.servedFromCache = true
	rec.status = e.Status
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	rec.outcome = metrics.OutcomeSuccess
	rec.model = e.Model
	// The upstream that ORIGINALLY produced this answer is not the upstream
	// that served this request. Labelling the sample "cache" keeps the
	// per-upstream error and latency numbers describing real traffic.
	rec.upstream = cacheStatusUpstream
	rec.respBytes = int64(len(e.Body))
	// The usage the stored answer reported, so the request log still shows what
	// the answer contains (and what it would have cost). It is deliberately NOT
	// added to the upstream token counters: those are provider facts and are
	// observed only by the ObserveTokens call that closes a real attempt in
	// proxy.go, because counting a replay would inflate consumption by exactly
	// what the cache saved.
	rec.usage = Usage{Prompt: e.PromptTokens, Completion: e.CompletionTokens}

	origin := e.Upstream
	if origin == "" {
		origin = "unknown"
	}
	if res.Kind == cache.KindSemantic {
		rec.cacheReason = fmt.Sprintf("semantic match %.4f from %s, age %s",
			res.Similarity, origin, res.Age.Round(time.Millisecond))
	} else {
		rec.cacheReason = fmt.Sprintf("exact match from %s, age %s",
			origin, res.Age.Round(time.Millisecond))
	}

	w.Header().Set(HeaderCache, rec.cacheStatus)
	w.Header().Set(HeaderCacheAge, strconv.FormatInt(res.Age.Milliseconds(), 10))
	w.Header().Set(HeaderUpstreamName, cacheStatusUpstream)
	if rec.requestID != "" {
		w.Header().Set(HeaderRequestID, rec.requestID)
	}

	if isEventStream(e.ContentType) {
		p.replayCachedStream(w, rec, e)
		return
	}

	contentType := e.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(rec.status)
	if _, err := w.Write(e.Body); err != nil {
		rec.reason = "client write: " + err.Error()
	}
}

// replayCachedStream writes a stored stream back to a client that asked for
// one.
//
// The stored bytes are re-parsed into frames and re-emitted one frame at a
// time. Writing the whole body in one call would work at the TCP level (SSE
// framing does not depend on packets) but it would deliver a burst, and the
// point of a stream is that a caller can start reading before the end. The
// frames come out of the same reader and the same writer as a live stream, so
// a replayed answer is indistinguishable from the real one.
//
// This does not re-run the model, so there is no first token to time: the
// first-token histogram is left alone rather than filled with a near-zero
// sample. Mixing a replay into a latency distribution that is supposed to
// describe generation would make the metric improve exactly when the cache is
// used most, which is the opposite of informative.
func (p *Proxy) replayCachedStream(w http.ResponseWriter, rec *record, e cache.Entry) {
	sw, err := sse.NewWriter(w)
	if err != nil {
		// The cache cannot fix a server that cannot stream. Fall back to the
		// stored bytes as a plain body: a caller that gets the frames in one
		// piece can still parse them, whereas a 500 loses the answer entirely.
		p.log.Warn("cache: streaming unavailable for a cached answer",
			slog.String("request_id", rec.requestID), slog.String("error", err.Error()))
		w.Header().Set("Content-Type", sse.ContentType)
		w.WriteHeader(rec.status)
		_, _ = w.Write(e.Body)
		return
	}
	sw.MirrorSSEHeader()
	// There is no live upstream response to mirror here, so the content type is
	// set directly: without it the frames arrive as an untyped body and an SSE
	// client will not treat the answer as a stream at all.
	w.Header().Set("Content-Type", sse.ContentType)
	if err := sw.WriteHeader(rec.status); err != nil {
		rec.reason = "client write: " + err.Error()
		return
	}

	reader := sse.NewReader(bytes.NewReader(e.Body))
	for {
		frame, rerr := reader.NextFrame()
		if rerr != nil {
			// EOF ends the replay; a malformed stored stream is a defect that
			// must be visible, not a reason to fail the caller's request.
			if !isStreamEOF(rerr) {
				rec.reason = "cached stream replay: " + rerr.Error()
			}
			break
		}
		if werr := sw.WriteUpstream(frame); werr != nil {
			rec.reason = "client write: " + werr.Error()
			return
		}
		rec.frames++
	}
	rec.respBytes = sw.BytesWritten()
}

// storeCache writes a delivered answer into the cache.
//
// It is called AFTER the response has been written to the client, so a slow
// store (a Redis round trip, an embedding call) can never delay the caller. The
// answer has already been delivered either way; the only thing at stake is
// whether the next identical request is cheap.
func (p *Proxy) storeCache(ctx context.Context, rec *record, body []byte, contentType string) {
	if p.cache == nil || rec.cacheIdent == nil || !rec.cacheIdent.Eligible {
		return
	}
	if len(body) == 0 {
		return
	}
	ident := *rec.cacheIdent
	entry := cache.Entry{
		Body:        body,
		Status:      http.StatusOK,
		ContentType: contentType,
		// The backend that produced this answer, so a later replay can say
		// where it came from.
		Upstream:         rec.upstream,
		PromptTokens:     rec.usage.Prompt,
		CompletionTokens: rec.usage.Completion,
	}
	if err := p.cache.StoreResponse(ctx, cacheRequest(ident), entry); err != nil {
		rec.cacheReason = "store failed: " + err.Error()
		p.log.Debug("cache store failed",
			slog.String("request_id", rec.requestID),
			slog.String("error", err.Error()),
		)
		return
	}
}

// cacheRequest projects the identity onto the cache's own request type.
func cacheRequest(ident cacheIdentity) cache.Request {
	return cache.Request{
		Scope:     ident.Scope,
		Model:     ident.Model,
		Prompt:    ident.Prompt,
		ExactKey:  ident.ExactKey,
		Signature: ident.Signature,
		HasTools:  ident.HasTools,

		Nondeterministic: ident.Nondeterministic,
		Bypass:           ident.Bypass,
		Refresh:          ident.Refresh,
	}
}
