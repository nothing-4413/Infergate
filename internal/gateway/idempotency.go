package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/idempotency"
	"github.com/infergate/infergate/internal/metrics"
)

// idempotencyBegin claims the caller's Idempotency-Key and decides what this
// request is: the first attempt, a replay of a finished one, or a conflict.
//
// It returns the writer the rest of the pipeline must use (a recording wrapper
// when this request owns the key) and whether the response has already been
// written in full.
//
// The claim is taken AFTER admission and BEFORE the cache, and both halves of
// that position are deliberate. After admission, because a replay is a request
// the caller made and it consumes a per-minute slot exactly as a cache hit does,
// while consuming no provider tokens. Before the cache, because a replay of a
// FINISHED call is a stronger statement than a cache hit: the caller is asking
// about its own operation, not about a similar question someone already asked.
func (p *Proxy) idempotencyBegin(w http.ResponseWriter, r *http.Request, rec *record, body []byte) (http.ResponseWriter, bool) {
	if p.idempotency == nil {
		return w, false
	}
	raw := strings.TrimSpace(r.Header.Get(HeaderIdempotencyKey))
	if raw == "" {
		return w, false
	}
	key := idempotency.KeyOf(raw)
	if key == "" {
		return w, false
	}

	// A key on a listing or a health check has nothing to replay. Ignoring it
	// rather than refusing keeps a client that stamps every request with a key
	// working; the response says so, so the disregard is not silent.
	if !isCompletionPath(r.URL.Path) {
		rec.idemKey = key
		rec.idemDecision = idempotentSkip
		rec.idemReason = "path has no replayable response"
		w.Header().Set(HeaderIdempotentStore, idempotentSkip)
		return w, false
	}

	scope := rec.tenant
	if scope == "" {
		scope = tenantFor(r)
		rec.tenant = scope
	}
	rec.idemKey = key
	rec.idemScope = scope
	rec.idemHash = requestHash(r.Method, r.URL.Path, body)

	dec, entry := p.idempotency.Begin(scope, key, rec.idemHash)
	switch dec {
	case idempotency.Proceed:
		rec.idemClaimed = true
		rec.idemDecision = idempotency.Proceed.String()
		// Two headers, both knowable before the body. The key is echoed so a
		// caller that reads only the first byte of a stream still knows its
		// retry will be a replay, and the replay header is set to "false" so
		// the caller can tell "this attempt did the work" from "you got the
		// earlier attempt's answer" without inferring it from a missing header.
		w.Header().Set(HeaderIdempotencyKey, key)
		w.Header().Set(HeaderIdempotentReplay, idempotentFalse)
		// One byte OVER the store's limit: the writer has to learn that the
		// answer did not fit, and a buffer capped at the limit would hand the
		// store a truncated body that still looks small enough to remember.
		rec.idemCapture = newCaptureWriter(w, p.idempotency.MaxResponseBytes()+1)
		return rec.idemCapture, false

	case idempotency.Replay:
		rec.idemDecision = idempotency.Replay.String()
		p.serveReplay(w, r, rec, entry)
		return w, true

	case idempotency.ConflictBody:
		rec.idemDecision = idempotency.ConflictBody.String()
		rec.idemReason = "this key is already recorded against a different request body"
		rec.status = http.StatusConflict
		rec.outcome = metrics.OutcomeBadRequest
		rec.reason = "idempotency conflict"
		w.Header().Set(HeaderIdempotentReplay, idempotentFalse)
		writeError(w, http.StatusConflict, TypeIdempotencyConflict,
			"idempotency key "+key+" was already used for a different request body")
		return w, true

	default:
		// ConflictInFlight. Answering 409 rather than waiting or duplicating is
		// the point of the store: the caller can retry the SAME key later and
		// receive the first attempt's answer, and meanwhile the provider was
		// asked for one generation, not two.
		rec.idemDecision = idempotency.ConflictInFlight.String()
		rec.idemReason = "a request with this key is still in flight"
		rec.status = http.StatusConflict
		rec.outcome = metrics.OutcomeBadRequest
		rec.reason = "idempotency in flight"
		w.Header().Set(HeaderIdempotentReplay, idempotentFalse)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusConflict, TypeIdempotencyInFlight,
			"a request with idempotency key "+key+" is already in flight")
		return w, true
	}
}

// serveReplay writes a recorded answer to the client.
//
// The stored bytes go out untouched - that is what makes the replay a replay
// rather than a regeneration - and the provenance headers say where they came
// from. A recorded stream is delivered as one body: the transcript is the
// answer, and re-timing its frames would invent a generation speed the provider
// never had.
func (p *Proxy) serveReplay(w http.ResponseWriter, r *http.Request, rec *record, e idempotency.Entry) {
	// M3: a replay consumed a request slot and no provider tokens, exactly like
	// a cache hit, so quota settles it the same way.
	rec.servedFromReplay = true
	rec.status = e.Status
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	rec.outcome = metrics.OutcomeSuccess
	rec.model = e.Model
	rec.stream = e.Stream
	// The provider that ORIGINALLY produced the answer is not the one that
	// served this request.
	rec.upstream = idempotencyUpstream
	rec.respBytes = int64(len(e.Body))
	w.Header().Set(HeaderIdempotentReplay, idempotentTrue)
	w.Header().Set(HeaderIdempotencyKey, e.Key)
	w.Header().Set(HeaderUpstreamName, idempotencyUpstream)
	if e.RequestID != "" {
		w.Header().Set(HeaderIdempotentOrigin, e.RequestID)
	}
	if e.Upstream != "" {
		// The backend that did the work gets a header of its own, because the
		// upstream-name header above now reports "replay".
		w.Header().Set(HeaderIdempotentUpstream, e.Upstream)
	}
	if !e.CompletedAt.IsZero() {
		age := time.Since(e.CompletedAt)
		if age < 0 {
			age = 0
		}
		w.Header().Set(HeaderIdempotentAge, strconv.FormatInt(age.Milliseconds(), 10))
	}
	if rec.requestID != "" {
		w.Header().Set(HeaderRequestID, rec.requestID)
	}

	contentType := ""
	if e.Headers != nil {
		contentType = strings.TrimSpace(http.Header(e.Headers).Get("Content-Type"))
	}
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(rec.status)
	if len(e.Body) > 0 {
		if _, err := w.Write(e.Body); err != nil {
			rec.reason = "client write: " + err.Error()
			return
		}
	}
	if !e.Stream {
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// idempotencyComplete stores the answer this request produced, or releases the
// claim so the caller's next attempt can make a real one.
//
// It runs from the deferred recorder, after the response is on the wire, so a
// slow store can never delay an answer. The rule about what may be remembered is
// the interesting part: a 5xx or a cancelled stream is NOT replayable, because
// "the last attempt failed" is not an answer a caller should receive forever,
// and the caller's retry is exactly the case that must be allowed to try again.
func (p *Proxy) idempotencyComplete(rec *record) {
	if p.idempotency == nil || !rec.idemClaimed {
		return
	}
	if !replayableResponse(rec) {
		p.idempotency.Abort(rec.idemScope, rec.idemKey)
		rec.idemDecision = "aborted"
		if rec.idemReason == "" {
			rec.idemReason = fmt.Sprintf("status %d is not replayable", rec.status)
		}
		return
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	entry := idempotency.Entry{
		Status:      status,
		Headers:     storedHeaders(rec),
		Body:        storedBody(rec),
		RequestID:   rec.requestID,
		Upstream:    rec.upstream,
		Model:       rec.model,
		Stream:      rec.stream,
		Outcome:     string(rec.outcome),
		CompletedAt: time.Now(),
	}
	stored, reason := p.idempotency.Complete(rec.idemScope, rec.idemKey, entry)
	if stored {
		rec.idemDecision = "stored"
		rec.idemStore = idempotentStored
		return
	}
	rec.idemDecision = "not-stored"
	rec.idemStore = idempotentOversize
	// The claim was released by the store, so the caller's retry really does
	// run again - and it is told so rather than left to guess.
	rec.idemReason = reason
}

// storedHeaders snapshots the response headers for a replay.
//
// Content-Length and Transfer-Encoding are dropped on purpose: the replay's body
// is the recorded bytes written as one write, and a Content-Length copied from a
// chunked generation would describe a framing that no longer exists. Go computes
// the correct framing for what is actually written.
func storedHeaders(rec *record) http.Header {
	if rec.idemCapture == nil || rec.idemCapture.header == nil {
		return nil
	}
	h := rec.idemCapture.header.Clone()
	h.Del("Content-Length")
	h.Del("Transfer-Encoding")
	h.Del(HeaderIdempotentStore)
	return h
}

// storedBody returns the recorded response body.
func storedBody(rec *record) []byte {
	if rec.idemCapture == nil {
		return nil
	}
	return rec.idemCapture.body.Bytes()
}

// replayableResponse reports whether an answer may be remembered and replayed.
//
// 2xx and 4xx are deterministic answers: the same request will produce the same
// thing, and serving the recorded copy is the whole point. 5xx, a timeout and a
// cancelled stream are all "we do not know", and pinning one of those to a key
// would turn a transient failure into a permanent one for that operation.
func replayableResponse(rec *record) bool {
	if rec.outcome == metrics.OutcomeCanceled {
		return false
	}
	return rec.status >= 200 && rec.status < 500
}

// requestHash fingerprints what the caller asked for, so that reusing a key for
// a DIFFERENT request is a conflict rather than a silent replay of an unrelated
// answer. The method and path are part of the hash: the same body posted to
// /v1/embeddings and /v1/chat/completions is not the same operation.
func requestHash(method, path string, body []byte) string {
	sum := sha256.New()
	sum.Write([]byte(method))
	sum.Write([]byte{0})
	sum.Write([]byte(path))
	sum.Write([]byte{0})
	sum.Write(body)
	return hex.EncodeToString(sum.Sum(nil)[:16])
}

// captureWriter records a response as it is written so that a completed answer
// can be replayed byte-for-byte later.
//
// It is a tee, not a buffer: every byte still reaches the client immediately,
// Flush is forwarded, and the copy is capped. A response larger than the cap is
// delivered in full and then deliberately not remembered, because holding an
// unbounded answer in memory to serve a retry that may never come is how a
// gateway turns one large generation into an outage.
type captureWriter struct {
	http.ResponseWriter
	limit   int64
	header  http.Header
	status  int
	body    bytes.Buffer
	total   int64
	written bool
}

func newCaptureWriter(w http.ResponseWriter, limit int64) *captureWriter {
	if limit <= 0 {
		limit = idempotency.DefaultMaxResponseBytes
	}
	return &captureWriter{ResponseWriter: w, limit: limit}
}

func (c *captureWriter) WriteHeader(status int) {
	if c.written {
		return
	}
	c.written = true
	c.status = status
	// The header map is captured BEFORE the first byte is written, because that
	// is the last moment it is still mutable; a replay must reproduce the
	// content type and vendor headers the provider sent.
	c.header = c.ResponseWriter.Header().Clone()
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if !c.written {
		c.WriteHeader(http.StatusOK)
	}
	c.total += int64(len(p))
	// One byte past the limit is enough to know the answer is oversize, so the
	// copy stays bounded by the cap rather than by the response.
	if room := c.limit - int64(c.body.Len()); room > 0 {
		if int64(len(p)) <= room {
			c.body.Write(p)
		} else {
			c.body.Write(p[:room])
		}
	}
	return c.ResponseWriter.Write(p)
}

// Flush forwards to the underlying writer so streaming through a captured
// response keeps its incremental delivery.
func (c *captureWriter) Flush() {
	if !c.written {
		c.WriteHeader(http.StatusOK)
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController, so a handler
// that needs a Hijacker or a deadline can still reach one through the wrapper.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
