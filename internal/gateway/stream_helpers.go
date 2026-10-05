package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
)

// marshalJSON encodes v without HTML escaping.
//
// encoding/json escapes <, > and & by default. Error messages echoed back
// through an in-band error frame are user-visible text; escaping them would
// produce a response that differs from what the provider would have sent for
// the same message, which breaks clients that match on error strings.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	// Encoder.Encode appends a newline; strip it so the value can be embedded
	// directly in an SSE data field.
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// isStreamEOF reports whether a reader error is the normal end of a stream.
//
// A body that is closed because the caller finished, or because the transport
// tore down the connection after the final frame, surfaces as io.EOF or
// net.ErrClosed. Treating those as failures would report a healthy stream as an
// upstream error and, in M1, trigger a pointless retry of a completed request.
func isStreamEOF(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}
