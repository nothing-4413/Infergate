// Package sse implements Server-Sent Events framing.
//
// InferGate relays OpenAI-compatible streams. A naive relay does
// `io.Copy(w, resp.Body)` and appears to work, but it has three defects that
// only show up under load or in production:
//
//  1. No flush. net/http buffers the response; the client sees nothing until
//     ~4 KiB has accumulated. First-token latency then measures the buffer, not
//     the model.
//  2. No TTFB observability. You cannot report time-to-first-token if you never
//     observe the first frame.
//  3. No protocol awareness. Terminating events, upstream errors delivered
//     inside a 200 OK stream, and usage accounting are all invisible.
//
// This package makes frames first-class values so the relay can flush, measure,
// inspect and terminate deliberately.
//
// # Framing rules (WHATWG HTML "Server-sent events")
//
// A frame is a sequence of "field: value" lines terminated by a blank line. The
// spec qualifies the field name as `[A-Za-z]*`, but real providers emit
// "retry:", "ping:" and vendor fields, so this reader accepts any byte before
// the colon rather than silently dropping provider-specific data.
//
// A line starting with ':' is a comment. Providers use it as a keep-alive
// heartbeat, and it must reach the client or idle-timeout proxies will sever
// long-running generations.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// MaxFrameBytes bounds a single frame's length. A stream frame is a small JSON
// object (the largest realistic one is a full tool-call argument delta), so 4
// MiB is far above anything legitimate and still prevents a hostile or broken
// upstream from allocating without limit.
const MaxFrameBytes = 4 << 20

// Frame is one parsed SSE event.
type Frame struct {
	// Name is the value of the optional "event:" line; empty for the common
	// unnamed case.
	Name string

	// ID is the value of the optional "id:" line.
	ID string

	// RetryMillis is the value of the optional "retry:" line, in milliseconds.
	RetryMillis int

	// Data is the concatenation of every "data:" line with the newlines the
	// spec prescribes. It is the JSON payload for OpenAI-style streams.
	Data []byte

	// Comment holds a comment frame's text (without the leading ':').
	Comment []byte

	// Raw preserves the exact bytes read from upstream, terminator included.
	// Replaying Raw is what makes the relay byte-transparent; Data exists for
	// inspection. Raw is nil for a frame the gateway synthesises.
	Raw []byte
}

// IsComment reports whether the frame carried only a comment (keep-alive).
func (f *Frame) IsComment() bool { return len(f.Comment) > 0 && len(f.Data) == 0 }

// IsDone reports whether the frame is OpenAI's stream terminator.
func (f *Frame) IsDone() bool { return string(bytes.TrimSpace(f.Data)) == "[DONE]" }

// Reader parses an SSE byte stream frame by frame.
//
// It chooses frame-oriented parsing over bufio.Scanner's line-oriented default
// for two reasons. First, a data payload may legally straddle a read boundary,
// and splitting on newlines alone invites the classic bug of treating one JSON
// object as two. Second, the relay must be able to replay a frame's exact bytes,
// which requires owning a copy of them rather than a view into a reader's
// buffer. The frame is assembled first and only then inspected, so a payload is
// never partially interpreted.
type Reader struct {
	br       *bufio.Reader
	accum    bytes.Buffer // raw bytes of the frame being assembled
	lineNo   int
	maxFrame int
}

// NewReader wraps r with SSE frame parsing.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 32<<10), maxFrame: MaxFrameBytes}
}

// SetMaxFrameBytes overrides the per-frame size cap. Values <= 0 restore the
// default.
func (r *Reader) SetMaxFrameBytes(n int) {
	if n <= 0 {
		n = MaxFrameBytes
	}
	r.maxFrame = n
}

// NextFrame returns the next frame. It returns io.EOF at a clean end of stream.
//
// On a malformed frame it returns the error but keeps the stream position so
// the caller can decide between aborting and resynchronising; the gateway
// aborts, because a stream it cannot parse is a stream it cannot account for.
func (r *Reader) NextFrame() (*Frame, error) {
	frame := &Frame{}

	for {
		line, err := r.readLine()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		atEOF := errors.Is(err, io.EOF)

		// Trim the terminator before anything else. A line read by ReadSlice
		// includes its '\n' (and a '\r' if the upstream uses CRLF), and this
		// must happen exactly once, in one place:
		//
		//   - blank-line detection depends on the line being EMPTY after the
		//     terminator is removed, otherwise no frame ever terminates and the
		//     whole stream collapses into a single frame;
		//   - field values must not carry the terminator, or event names end up
		//     as "message\n" and multi-line payloads accumulate "\r\n\n".
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case len(trimmed) == 0 && !atEOF:
			// Blank line terminates the frame. Its bytes belong to the frame:
			// Raw must include the terminator, because replaying Raw is how the
			// relay stays byte-transparent. Without this the client receives
			// every frame concatenated with no blank line between them, which
			// is no longer a valid SSE stream — a client that re-parses it sees
			// one giant frame.
			r.accum.Write(line)
			frame.Raw = r.takeRaw()
			return finalizeFrame(frame), nil

		case len(trimmed) == 0 && atEOF:
			// EOF right after a blank line: nothing buffered, so the stream is
			// cleanly finished.
			if r.accum.Len() == 0 {
				return nil, io.EOF
			}
			frame.Raw = r.takeRaw()
			return finalizeFrame(frame), nil

		case len(trimmed) > 0:
			if r.accum.Len()+len(line) > r.maxFrame {
				return nil, fmt.Errorf("sse: frame exceeds %d bytes (line %d)", r.maxFrame, r.lineNo)
			}
			r.accum.Write(line)
			r.parseLine(frame, trimmed)
		}

		if atEOF {
			// Stream ended without a trailing blank line. The spec says to
			// dispatch what we have; OpenAI always terminates with a blank
			// line, but a truncated upstream response should still be reported
			// rather than swallowed.
			if r.accum.Len() == 0 {
				return nil, io.EOF
			}
			frame.Raw = r.takeRaw()
			return finalizeFrame(frame), nil
		}
	}
}

// takeRaw returns and clears the raw bytes buffered for this frame.
func (r *Reader) takeRaw() []byte {
	all := r.accum.Bytes()
	raw := make([]byte, len(all))
	copy(raw, all)
	r.accum.Reset()
	return raw
}

// parseLine folds one "field: value" line into the frame under construction.
func (r *Reader) parseLine(frame *Frame, line []byte) {
	if len(line) == 0 {
		return
	}
	// Comment or heartbeat.
	if line[0] == ':' {
		comment := bytes.TrimPrefix(line, []byte(":"))
		if len(comment) > 0 && comment[0] == ' ' {
			comment = comment[1:]
		}
		frame.Comment = append(frame.Comment, comment...)
		return
	}

	var (
		field []byte
		value []byte
	)
	if i := bytes.IndexByte(line, ':'); i >= 0 {
		field = line[:i]
		value = line[i+1:]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
	} else {
		field = line
	}

	switch string(field) {
	case "event":
		frame.Name = string(value)
	case "id":
		frame.ID = string(value)
	case "retry":
		if n, err := strconv.Atoi(string(bytes.TrimSpace(value))); err == nil {
			frame.RetryMillis = n
		}
	case "data":
		// Spec: append the value plus a newline. The final trailing newline is
		// removed in finalizeFrame, so multi-line payloads concatenate with
		// interior newlines intact.
		frame.Data = append(frame.Data, value...)
		frame.Data = append(frame.Data, '\n')
	default:
		// Unknown or vendor-specific field. Preserve it inside Comment-free
		// Raw replay; keeping the stream byte-transparent matters more than
		// modelling every field.
	}
}

// finalizeFrame strips the spec-mandated trailing newline from the data payload.
func finalizeFrame(f *Frame) *Frame {
	if n := len(f.Data); n > 0 && f.Data[n-1] == '\n' {
		f.Data = f.Data[:n-1]
	}
	return f
}

// readLine reads one line including its terminator.
//
// The returned slice is a COPY. bufio.ReadSlice hands back a view into the
// reader's internal buffer, and NextFrame keeps referencing the line it is
// currently parsing (parseLine appends the value to frame.Data, and Raw replay
// copies the frame's bytes). Two slices into the same buffer therefore alias
// each other, and the second read silently rewrites the first line's content —
// which shows up as data from a later line leaking into an earlier frame. One
// allocation per line is a cheap price for correctness on a path that is
// already dominated by network I/O.
func (r *Reader) readLine() ([]byte, error) {
	line, err := r.br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, fmt.Errorf("sse: line %d exceeds the read buffer", r.lineNo+1)
	}
	if len(line) > 0 {
		r.lineNo++
	}
	out := make([]byte, len(line))
	copy(out, line)
	if err != nil {
		return out, err
	}
	return out, nil
}
