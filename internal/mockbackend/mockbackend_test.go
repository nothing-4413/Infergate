// Tests for the fault-injection control headers of the in-process backend.
//
// The verifiers use this backend as their measuring instrument, and a header it
// ignores is an instrument that silently measures nothing: a "slow backend"
// scenario built with an unreadable X-Mock-TTFB would answer instantly, the
// gateway would never time out, and the check would report on a run in which the
// thing being checked never happened. An unreadable value is therefore a 400
// that names the header, and these tests pin that plus the order of the knobs
// (header over runtime stall over configured).
package mockbackend

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// postChat sends one chat request to the backend and returns the status and body.
func postChat(t *testing.T, b *Backend, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, b.URL+"/v1/chat/completions", strings.NewReader(`{"model":"mock-gpt"}`))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("posting to %s: %v", b.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestUnreadableTimingHeadersAreRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		value  string
	}{
		{"delay without a unit", HeaderDelay, "300"},
		{"delay that is not a duration", HeaderDelay, "soon"},
		{"negative delay", HeaderDelay, "-1s"},
		{"ttfb without a unit", HeaderTTFB, "300"},
		{"negative ttfb", HeaderTTFB, "-1ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := New(Options{Name: "bad-header"})
			defer b.Close()
			status, body := postChat(t, b, map[string]string{tc.header: tc.value})
			if status != http.StatusBadRequest {
				t.Fatalf("%s = %q: status = %d, want 400 (body %s)", tc.header, tc.value, status, body)
			}
			if !strings.Contains(body, tc.header) {
				t.Fatalf("%s = %q: body = %s, want it to name the header", tc.header, tc.value, body)
			}
		})
	}
}

// The stall is not a streaming-only knob: it applies to a plain JSON answer too,
// which is what makes an upstream-timeout scenario possible without SSE.
func TestReadableTimingHeadersAreHonoured(t *testing.T) {
	b := New(Options{Name: "slow-header"})
	defer b.Close()

	for _, header := range []string{HeaderDelay, HeaderTTFB} {
		start := time.Now()
		status, body := postChat(t, b, map[string]string{header: "40ms"})
		elapsed := time.Since(start)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", header, status, body)
		}
		if elapsed < 30*time.Millisecond {
			t.Fatalf("%s: 40ms: the answer arrived after %v, so the stall was not applied", header, elapsed)
		}
	}
}

// The per-request header wins over the runtime stall, which wins over the
// configured TTFB. The order matters to a verifier that slows one backend for
// one request after having slowed it for a scenario.
func TestHeaderStallWinsOverTheRuntimeStall(t *testing.T) {
	b := New(Options{Name: "precedence", TTFB: 200 * time.Millisecond})
	defer b.Close()
	b.SetStall(300 * time.Millisecond)

	start := time.Now()
	status, body := postChat(t, b, map[string]string{HeaderTTFB: "20ms"})
	elapsed := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("the request took %v: the 20ms header did not win over the 300ms runtime stall", elapsed)
	}

	// With no header the runtime stall is what applies -- this is the half of the
	// order that the refactor from stall(r) to stall(header) could have broken.
	start = time.Now()
	status, body = postChat(t, b, nil)
	elapsed = time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("no header: status = %d, want 200 (body %s)", status, body)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("no header: the request took %v, want the 300ms runtime stall to apply", elapsed)
	}
}
