// Tests for the fault-injection control headers.
//
// The headers are how a gate injects a status or a stall without recompiling the
// mock, so their failure mode matters more than their success mode: a header the
// mock ignores leaves a green check standing over a scenario that never happened.
// These tests pin the answer to an unreadable value -- a 400 that names the
// header -- and the two orderings that decide what happens when a request
// carries more than one of them.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postChatWithHeaders sends one chat request through the handler with control
// headers attached.
func postChatWithHeaders(t *testing.T, s *server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestUnreadableTimingHeadersAreRejected(t *testing.T) {
	const body = `{"model":"mock","stream":true}`
	for _, tc := range []struct {
		name   string
		header string
		value  string
	}{
		{"delay without a unit", "X-Mock-Delay", "300"},
		{"delay that is not a duration", "X-Mock-Delay", "soon"},
		{"negative delay", "X-Mock-Delay", "-1s"},
		{"ttfb without a unit", "X-Mock-TTFB", "300"},
		{"negative ttfb", "X-Mock-TTFB", "-1ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, nil)
			rec := postChatWithHeaders(t, s, body, map[string]string{tc.header: tc.value})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %q: status = %d, want 400 (body %s)", tc.header, tc.value, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.header) {
				t.Fatalf("%s = %q: body = %s, want it to name the header", tc.header, tc.value, rec.Body.String())
			}
			if got := callSnapshotBody(t, s); got.Calls != 0 {
				t.Fatalf("%s = %q: %d call(s) counted, want 0: a rejected request must not look like an answer", tc.header, tc.value, got.Calls)
			}
		})
	}
}

func TestReadableTimingHeadersAreHonoured(t *testing.T) {
	s := testServer(t, nil)

	// The delay applies to every chat answer, streaming or not.
	start := time.Now()
	rec := postChatWithHeaders(t, s, `{"model":"mock"}`, map[string]string{"X-Mock-Delay": "40ms"})
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("X-Mock-Delay: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if elapsed < 30*time.Millisecond {
		t.Fatalf("X-Mock-Delay: 40ms: the answer arrived after %v, so the delay was not applied", elapsed)
	}

	// The ttfb header stalls the streaming path. A ResponseRecorder buffers, so
	// this observes that the stall happened, not where the flush boundary was.
	start = time.Now()
	rec = postChatWithHeaders(t, s, `{"model":"mock","stream":true}`, map[string]string{"X-Mock-TTFB": "40ms"})
	elapsed = time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("X-Mock-TTFB: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Body.String(), "data: ") {
		t.Fatalf("X-Mock-TTFB: body = %q, want SSE frames", rec.Body.String())
	}
	if elapsed < 30*time.Millisecond {
		t.Fatalf("X-Mock-TTFB: 40ms: the stream finished in %v, so the stall was not applied", elapsed)
	}
}

// An injected status is still the whole answer: the request never waits for the
// delay, and no content frames are produced.
func TestInjectedStatusIsAnsweredBeforeTheDelay(t *testing.T) {
	s := testServer(t, nil)
	start := time.Now()
	rec := postChatWithHeaders(t, s, `{"model":"mock","stream":true}`, map[string]string{
		"X-Mock-Status": "429",
		"X-Mock-Delay":  "400ms",
	})
	elapsed := time.Since(start)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("the injected status waited %v: the delay ran before the status", elapsed)
	}
}

// A malformed request is answered by name even when it also asks for a status:
// the timing headers are validated before a response shape is chosen, so a typo
// cannot be swallowed by the branch that would have answered it.
func TestAMalformedTimingHeaderBeatsAnInjectedStatus(t *testing.T) {
	s := testServer(t, nil)
	rec := postChatWithHeaders(t, s, `{"model":"mock"}`, map[string]string{
		"X-Mock-Status": "429",
		"X-Mock-Delay":  "300",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "X-Mock-Delay") {
		t.Fatalf("body = %s, want it to name X-Mock-Delay", rec.Body.String())
	}
}
