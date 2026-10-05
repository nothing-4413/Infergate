package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReaderParsesFramesCoveringWireDetails walks the formatting details that
// actually appear in the wild: bare data lines, named events, multi-line data,
// comments/heartbeats, CRLF terminators, and fields with leading spaces.
func TestReaderParsesFramesCoveringWireDetails(t *testing.T) {
	raw := strings.Join([]string{
		": keep-alive",
		"",
		"event: message",
		"data: {\"a\":1}",
		"",
		"data: line one",
		"data: line two",
		"",
		"data:[DONE]",
		"",
	}, "\n")

	r := NewReader(strings.NewReader(raw))
	var got []Frame
	for {
		f, err := r.NextFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("NextFrame: %v", err)
		}
		got = append(got, *f)
	}

	if len(got) != 4 {
		t.Fatalf("frame count = %d, want 4 (%v)", len(got), got)
	}
	if !got[0].IsComment() || string(got[0].Comment) != "keep-alive" {
		t.Errorf("frame 0 = %+v, want heartbeat comment", got[0])
	}
	if got[1].Name != "message" || string(got[1].Data) != `{"a":1}` {
		t.Errorf("frame 1 = %+v, want named event with data", got[1])
	}
	if string(got[2].Data) != "line one\nline two" {
		t.Errorf("frame 2 data = %q, want multi-line join with \\n", got[2].Data)
	}
	if !got[3].IsDone() {
		t.Errorf("frame 3 = %+v, want [DONE]", got[3])
	}
	for i, f := range got {
		if len(f.Raw) == 0 {
			t.Errorf("frame %d has empty Raw; replay would not be byte-transparent", i)
		}
	}
}

// TestReaderHandlesCRLF checks the transport-level detail that a proxy must not
// corrupt: HTTP permits CRLF line endings, and a naive reader leaks the '\r'
// into the payload, which then breaks JSON parsing downstream.
func TestReaderHandlesCRLF(t *testing.T) {
	raw := "data: {\"x\":1}\r\n\r\ndata: [DONE]\r\n\r\n"
	r := NewReader(strings.NewReader(raw))

	f, err := r.NextFrame()
	if err != nil {
		t.Fatalf("NextFrame: %v", err)
	}
	if string(f.Data) != `{"x":1}` {
		t.Errorf("data = %q, want no trailing carriage return", f.Data)
	}
}

// TestReaderFrameWithoutTerminator covers a backend that closes the socket
// without a final blank line: the last frame must still be delivered, not
// silently dropped.
func TestReaderFrameWithoutTerminator(t *testing.T) {
	r := NewReader(strings.NewReader("data: {\"x\":1}"))
	f, err := r.NextFrame()
	if err != nil {
		t.Fatalf("NextFrame: %v", err)
	}
	if string(f.Data) != `{"x":1}` {
		t.Errorf("data = %q", f.Data)
	}
	if _, err := r.NextFrame(); !errors.Is(err, io.EOF) {
		t.Errorf("second NextFrame err = %v, want io.EOF", err)
	}
}

// TestReaderRejectsOversizedFrame proves the memory guard works: a backend that
// never sends a frame terminator must not be able to grow the gateway's buffer
// without bound.
func TestReaderRejectsOversizedFrame(t *testing.T) {
	payload := "data: " + strings.Repeat("x", 4096) + "\n\n"
	r := NewReader(strings.NewReader(payload))
	r.SetMaxFrameBytes(1024)

	if _, err := r.NextFrame(); err == nil {
		t.Fatal("NextFrame returned nil error for an oversized frame")
	}
}

// TestReaderAcceptsUnknownFields documents deliberate leniency: a vendor
// extension field must not desynchronise the stream.
func TestReaderAcceptsUnknownFields(t *testing.T) {
	r := NewReader(strings.NewReader("vendor-trace: abc\ndata: hi\n\n"))
	f, err := r.NextFrame()
	if err != nil {
		t.Fatalf("NextFrame: %v", err)
	}
	if string(f.Data) != "hi" {
		t.Errorf("data = %q, want %q", f.Data, "hi")
	}
}
