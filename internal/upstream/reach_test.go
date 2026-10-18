package upstream

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/config"
)

// freeAddr starts a listener and returns its address and a channel that the
// test closes to release it. Using net.Listen rather than httptest keeps the
// probe honest: it only promises a TCP connection, so the test should too.
func freeAddr(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// closedAddr returns an address nothing is listening on: bind, read the port,
// close, and hand the address back. Binding first is what makes the port free
// instead of hoping 1 is unused.
func closedAddr(t *testing.T) string {
	t.Helper()
	addr, release := freeAddr(t)
	release()
	return addr
}

func TestProbeReachReportsAListeningBackendAsReachable(t *testing.T) {
	addr, release := freeAddr(t)
	defer release()

	results := ProbeReach(context.Background(), []config.UpstreamConfig{
		{Name: "local", BaseURL: "http://" + addr},
	})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if !r.Reachable() {
		t.Fatalf("Reachable() = false (%v), want true for a listener at %s", r.Err, addr)
	}
	if r.Name != "local" {
		t.Fatalf("Name = %q, want the configured name", r.Name)
	}
	if r.Addr != addr {
		t.Fatalf("Addr = %q, want the dialled address %q", r.Addr, addr)
	}
}

func TestProbeReachNamesTheAddressItCouldNotReach(t *testing.T) {
	addr := closedAddr(t)

	results := ProbeReach(context.Background(), []config.UpstreamConfig{
		{Name: "ollama-local", BaseURL: "http://" + addr},
	})
	if results[0].Reachable() {
		t.Fatal("Reachable() = true for a closed port")
	}
	// The WARN the process prints quotes this address; without it an operator
	// cannot tell "Ollama was never started" from "DNS is broken".
	if results[0].Addr != addr {
		t.Fatalf("Addr = %q, want %q even though nothing answered", results[0].Addr, addr)
	}
	if results[0].Err == nil {
		t.Fatal("Err = nil for a closed port, want the dial failure")
	}
}

func TestProbeReachFillsInTheSchemeDefaultPort(t *testing.T) {
	// Nothing listens on :80 here, and that is fine: this test is about the
	// address the probe chose, not about what answered on it.
	results := ProbeReach(context.Background(), []config.UpstreamConfig{
		{Name: "plain", BaseURL: "http://127.0.0.1"},
		{Name: "tls", BaseURL: "https://127.0.0.1"},
	})
	if got, want := results[0].Addr, "127.0.0.1:80"; got != want {
		t.Fatalf("http Addr = %q, want %q", got, want)
	}
	if got, want := results[1].Addr, "127.0.0.1:443"; got != want {
		t.Fatalf("https Addr = %q, want %q", got, want)
	}
}

func TestProbeReachRejectsABaseURLWithoutAHost(t *testing.T) {
	results := ProbeReach(context.Background(), []config.UpstreamConfig{
		{Name: "empty", BaseURL: "http://"},
	})
	if results[0].Reachable() {
		t.Fatal("Reachable() = true for a base_url with no host")
	}
	if !strings.Contains(results[0].Err.Error(), "no host") {
		t.Fatalf("Err = %v, want it to say the URL has no host", results[0].Err)
	}
}

func TestProbeReachRejectsASchemeWithNoDefaultPort(t *testing.T) {
	results := ProbeReach(context.Background(), []config.UpstreamConfig{
		{Name: "weird", BaseURL: "ftp://127.0.0.1"},
	})
	if results[0].Reachable() {
		t.Fatal("Reachable() = true for a scheme the gateway cannot speak")
	}
	if !strings.Contains(results[0].Err.Error(), "no default port") {
		t.Fatalf("Err = %v, want it to name the missing default port", results[0].Err)
	}
}

func TestProbeReachKeepsConfigurationOrder(t *testing.T) {
	up, releaseUp := freeAddr(t)
	defer releaseUp()
	down := closedAddr(t)

	// Deliberately mixed and given in this order: results must follow the
	// configuration, not the order the goroutines happened to finish in.
	results := ProbeReach(context.Background(), []config.UpstreamConfig{
		{Name: "first-down", BaseURL: "http://" + down},
		{Name: "second-up", BaseURL: "http://" + up},
	})
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Name != "first-down" || results[1].Name != "second-up" {
		t.Fatalf("names = [%q %q], want configuration order", results[0].Name, results[1].Name)
	}
	if results[0].Reachable() {
		t.Fatal("the closed backend was reported reachable")
	}
	if !results[1].Reachable() {
		t.Fatalf("the listening backend was reported unreachable: %v", results[1].Err)
	}
}

func TestProbeReachOfNoUpstreamsIsEmpty(t *testing.T) {
	if got := ProbeReach(context.Background(), nil); len(got) != 0 {
		t.Fatalf("got %d results, want none", len(got))
	}
}

func TestProbeReachHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	results := ProbeReach(ctx, []config.UpstreamConfig{
		{Name: "local", BaseURL: "http://127.0.0.1:1"},
	})
	if results[0].Reachable() {
		t.Fatal("Reachable() = true under a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > reachTimeout {
		t.Fatalf("probe took %v, want it to stop at the cancelled context rather than the timeout", elapsed)
	}
}
