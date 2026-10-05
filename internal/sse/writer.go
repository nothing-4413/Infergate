package sse

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
)

// ContentType is the media type for SSE. The "; charset=utf-8" parameter is not
// decorative: an SSE stream carrying non-ASCII (any CJK response) is decoded
// with the wrong charset by some clients when the parameter is absent.
const ContentType = "text/event-stream; charset=utf-8"

// Writer relays frames to an http.ResponseWriter, flushing each one.
//
// Flushing per frame is the whole point: it converts "the model produced a
// token" into "the client can render a token", which is what first-token
// latency actually measures.
//
// Writes are serialised by a mutex even though the relay is designed so that a
// single goroutine writes, because the response writer is the one shared
// resource in the streaming path and an accidental second writer (a heartbeat
// goroutine, a future metrics side-channel) would otherwise interleave
// half-frames. The uncontended mutex costs nanoseconds; a corrupted stream
// costs a debugging session.
type Writer struct {
	mu            sync.Mutex
	w             http.ResponseWriter
	rc            *http.ResponseController
	wroteHeader   bool
	mirroredSSE   bool
	bytesWritten  int64
	framesWritten int64
}

// NewWriter prepares w for an SSE response. It does not write the status line;
// call WriteHeader (or let the first frame do it).
func NewWriter(w http.ResponseWriter) (*Writer, error) {
	if _, ok := w.(http.Flusher); !ok {
		// Streaming an unbuffered stream through a writer that cannot flush
		// would silently degrade to a buffered response. Fail loudly instead.
		return nil, fmt.Errorf("sse: ResponseWriter does not support flushing (cannot stream)")
	}
	return &Writer{w: w, rc: http.NewResponseController(w)}, nil
}

// WriteHeader writes and flushes the response status, applying the response
// headers the gateway wants on every stream.
func (s *Writer) WriteHeader(status int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeHeaderLocked(status)
}

func (s *Writer) writeHeaderLocked(status int) error {
	if s.wroteHeader {
		return nil
	}
	h := s.w.Header()
	if !s.mirroredSSE {
		h.Set("Content-Type", ContentType)
		h.Set("Cache-Control", "no-cache")
	}
	// Tell nginx and friends not to buffer this response. Without it a reverse
	// proxy in front of the gateway reintroduces exactly the buffering that
	// per-frame flushing exists to avoid.
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(status)
	s.wroteHeader = true
	return s.rc.Flush()
}

// MirrorSSEHeader records that the caller copied the upstream Content-Type, so
// WriteHeader must not overwrite it.
func (s *Writer) MirrorSSEHeader() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mirroredSSE = true
}

// WriteUpstream replays a frame taken verbatim from the upstream response.
//
// Replaying Raw makes the relay byte-transparent: field order, spacing, vendor
// fields, comments and the terminator all reach the client exactly as the
// provider sent them, while the gateway still inspected Data for accounting.
func (s *Writer) WriteUpstream(f *Frame) error {
	if len(f.Raw) == 0 {
		return s.WriteData(f.Data)
	}
	return s.write(f.Raw)
}

// WriteData writes a synthesised unnamed "data:" frame. Use it for frames the
// gateway originates, such as an error delivered inside an already-committed
// stream.
func (s *Writer) WriteData(data []byte) error {
	buf := make([]byte, 0, len(data)+16)
	buf = append(buf, "data: "...)
	buf = append(buf, data...)
	buf = append(buf, '\n', '\n')
	return s.write(buf)
}

// WriteComment writes a heartbeat comment frame. Comments are the correct
// keep-alive primitive for SSE: every spec-compliant client ignores them, while
// idle-timeout proxies still see traffic.
func (s *Writer) WriteComment(text string) error {
	buf := make([]byte, 0, len(text)+8)
	buf = append(buf, ": "...)
	buf = append(buf, text...)
	buf = append(buf, '\n', '\n')
	return s.write(buf)
}

// WriteDone writes the canonical OpenAI stream terminator.
func (s *Writer) WriteDone() error { return s.WriteData([]byte("[DONE]")) }

func (s *Writer) write(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.wroteHeader {
		// A stream must commit 200 before any frame; the relay errors earlier
		// if upstream returned a non-2xx status.
		if err := s.writeHeaderLocked(http.StatusOK); err != nil {
			return err
		}
	}
	n, err := s.w.Write(p)
	s.bytesWritten += int64(n)
	if err != nil {
		return err
	}
	s.framesWritten++
	// Flush may return http.ErrNotSupported on exotic writers; NewWriter
	// already verified Flusher support, so treat a failure as a transport
	// error and let the caller stop.
	return s.rc.Flush()
}

// BytesWritten reports how many bytes have been flushed downstream.
func (s *Writer) BytesWritten() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytesWritten
}

// FramesWritten reports how many frames have been flushed downstream.
func (s *Writer) FramesWritten() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.framesWritten
}

// SetHeader sets a header before the status line is written.
func (s *Writer) SetHeader(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wroteHeader {
		return
	}
	s.w.Header().Set(key, value)
}

// SetRetryHint advertises a client reconnect delay. It is part of the SSE
// grammar, so clients honour it without application-level agreement.
func (s *Writer) SetRetryHint(ms int) {
	s.SetHeader("Retry-After", strconv.Itoa(ms/1000))
}
