package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/infergate/infergate/internal/metrics"
	"github.com/infergate/infergate/internal/sse"
	"github.com/infergate/infergate/internal/upstream"
)

// heartbeatInterval is how long the relay will let an upstream stay silent
// before writing an SSE comment.
//
// Why this exists: intermediate proxies, load balancers and NAT gateways
// commonly drop a connection that has been idle for 30-60 seconds. A model
// reasoning over a long prompt can easily be silent for longer than that before
// its first token, and the failure mode is a truncated generation rather than a
// clean error. One comment frame every 15 seconds keeps the socket warm at a
// cost of 3 bytes, and every SSE client ignores comments by specification.
const heartbeatInterval = 15 * time.Second

// newRequestID mints a 128-bit random correlation id.
//
// Randomness over a counter because correlation must survive a password-manager
// style threat model: a guessable request id lets an attacker read another
// tenant's logs. 16 bytes is the same width as a UUIDv4 without the formatting
// dependency.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable in practice; fall back to a
		// timestamp so the gateway degrades to "correlatable but guessable"
		// rather than to a panic on the request path.
		return "t" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// relayStream forwards an SSE response frame by frame.
//
// # Design
//
// Frames are produced by a single reader goroutine and delivered over a
// channel, so the select statement can also serve a heartbeat ticker. The
// alternative — a heartbeat goroutine writing to the same ResponseWriter — is a
// data race on the writer and a source of interleaved half-frames. Here exactly
// one goroutine ever writes to the client.
//
// The reader goroutine owns resp.Body by contract: closing the body (by the
// caller's defer, or by context cancellation propagated from the transport)
// unblocks the read, closes frames, and lets the goroutine exit. Nothing is
// leaked even when the client vanishes mid-stream.
func (p *Proxy) relayStream(w http.ResponseWriter, r *http.Request, resp *http.Response, target *upstream.Target, rec *record) metrics.Outcome {
	sw, err := sse.NewWriter(w)
	if err != nil {
		rec.reason = err.Error()
		writeError(w, http.StatusInternalServerError, TypeInternal, "streaming is not supported by this server")
		return metrics.OutcomeInternal
	}
	sw.MirrorSSEHeader()
	if err := sw.WriteHeader(http.StatusOK); err != nil {
		rec.reason = err.Error()
		return metrics.OutcomeCanceled
	}
	// Commit time is not first-token time: the headers are flushed before the
	// model has produced anything. Measuring from here is what makes the metric
	// mean "the user saw a token", not "the socket opened".
	streamStart := time.Now()

	type item struct {
		frame *sse.Frame
		err   error
	}
	frames := make(chan item, 8)
	go func() {
		defer close(frames)
		reader := sse.NewReader(resp.Body)
		for {
			frame, err := reader.NextFrame()
			if err != nil {
				frames <- item{err: err}
				return
			}
			select {
			case frames <- item{frame: frame}:
			case <-r.Context().Done():
				return
			}
		}
	}()

	inspector := sse.NewInspector()
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	var (
		wroteDone bool
		firstData bool
		done      = false

		// firstTokenSet is deliberately separate from rec.firstToken. A
		// legitimate first token can measure as exactly 0 on a coarse clock, so
		// using the value as its own "was it recorded" flag loses that sample.
		firstTokenSet bool
	)

	for !done {
		select {
		case <-r.Context().Done():
			// Client hung up. Stop paying for tokens nobody will read: closing
			// the upstream body below propagates through the transport.
			rec.reason = "client disconnected mid-stream"
			return metrics.OutcomeCanceled

		case <-ticker.C:
			if err := sw.WriteComment("keep-alive"); err != nil {
				rec.reason = "heartbeat failed: " + err.Error()
				return metrics.OutcomeCanceled
			}

		case it, ok := <-frames:
			if !ok {
				done = true
				continue
			}
			if it.err != nil {
				// A clean EOF is the normal end of a well-behaved stream.
				if isStreamEOF(it.err) {
					done = true
					continue
				}
				rec.reason = "stream read: " + it.err.Error()
				p.writeStreamError(sw, "upstream stream failed: "+it.err.Error())
				return metrics.OutcomeUpstreamErr
			}

			frame := it.frame
			obs := inspector.Observe(frame)

			if !firstData && firstVisibleToken(frame) {
				firstData = true
				firstTokenSet = true
				rec.firstToken = time.Since(streamStart)
			}
			if obs.Err != nil {
				// Providers report mid-stream failures inside a 200 OK body.
				// The status is already committed, so the client is told via an
				// in-band error frame, which is exactly how official SDKs
				// detect the condition.
				if werr := sw.WriteUpstream(frame); werr != nil {
					rec.reason = werr.Error()
					return metrics.OutcomeCanceled
				}
				rec.reason = "in-stream error: " + obs.Err.Error()
				rec.frames++
				return metrics.OutcomeUpstreamErr
			}
			if werr := sw.WriteUpstream(frame); werr != nil {
				rec.reason = "client write: " + werr.Error()
				return metrics.OutcomeCanceled
			}
			rec.frames++
			if frame.IsDone() {
				wroteDone = true
			}
		}
	}

	// Some backends close the stream after the last chunk without emitting the
	// [DONE] sentinel. Clients built against the OpenAI SDK treat [DONE] as the
	// end-of-stream signal, so synthesise it rather than leaving a client to
	// hang until its own timeout.
	if !wroteDone {
		if err := sw.WriteDone(); err != nil {
			rec.reason = "client write: " + err.Error()
			return metrics.OutcomeCanceled
		}
	}

	usage := inspector.Usage()
	rec.usage = Usage{Prompt: usage.PromptTokens, Completion: usage.CompletionTokens, Cached: usage.CachedTokens}
	rec.respBytes = sw.BytesWritten()
	// The provider's own model name is authoritative for pricing.
	if m := inspector.Model(); m != "" {
		rec.model = m
	}
	if total, parsed := inspector.Frames(); total > 0 && parsed == 0 {
		rec.reason = "stream carried no parseable frames"
	}
	if streamErr := inspector.Err(); streamErr != nil {
		rec.reason = "in-stream error: " + streamErr.Error()
		return metrics.OutcomeUpstreamErr
	}
	if firstTokenSet {
		rec.firstTokenSet = true
	}
	p.log.Debug("stream complete",
		slog.String("request_id", rec.requestID),
		slog.String("upstream", target.Name),
		slog.Int64("frames", rec.frames),
		slog.Int64("bytes", rec.respBytes),
	)
	return metrics.OutcomeSuccess
}

// firstVisibleToken reports whether a frame is the client-visible beginning of
// the answer, which is what time-to-first-token is supposed to measure.
//
// The naive test — "any frame with a non-empty data payload" — fires on the
// provider's opening role frame, whose delta is `{"role":"assistant"}` with
// empty or absent content. That frame is emitted as soon as the provider
// accepts the request, so measuring it reports queueing time as generation
// latency, and on a fast loopback it measures as zero and looks like a missing
// sample. The honest signal is a frame carrying user-visible output: a content
// delta, a reasoning delta, or a tool-call delta. The terminator never counts,
// and neither does an error frame (that is a failure, not a first token).
func firstVisibleToken(f *sse.Frame) bool {
	if f == nil || len(f.Data) == 0 || f.IsDone() {
		return false
	}
	var payload struct {
		Choices []struct {
			Delta struct {
				Content          *string `json:"content"`
				ReasoningContent *string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    *int    `json:"index"`
					ID       *string `json:"id"`
					Function *struct {
						Name *string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			Message *struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(f.Data), &payload); err != nil {
		// Unparseable body: we cannot tell whether it carried output. Counting
		// it is the safer failure mode for a latency metric — it can only
		// over-report, never hide a real stall.
		return true
	}
	for _, ch := range payload.Choices {
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			return true
		}
		if ch.Delta.ReasoningContent != nil && *ch.Delta.ReasoningContent != "" {
			return true
		}
		if len(ch.Delta.ToolCalls) > 0 {
			return true
		}
		if ch.Message != nil && ch.Message.Content != nil && *ch.Message.Content != "" {
			return true
		}
	}
	return false
}

// writeStreamError emits an OpenAI-shaped error inside an already-committed
// stream. The HTTP status has been sent, so this frame is the only channel left.
func (p *Proxy) writeStreamError(sw *sse.Writer, msg string) {
	payload := ErrorBody{Error: ErrorDetail{Message: msg, Type: TypeBadGateway}}
	if encoded, err := marshalJSON(payload); err == nil {
		_ = sw.WriteData(encoded)
	}
	_ = sw.WriteDone()
}
