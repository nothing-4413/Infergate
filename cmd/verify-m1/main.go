// Command verify-m1 is InferGate's M1 acceptance test: routing strategy and
// failover behaviour, exercised end to end against real backends.
//
// It starts several in-process OpenAI-compatible backends with DIFFERENT
// behaviour (a healthy one, a permanently broken one, a slow one) plus the real
// gateway, then drives traffic through the real wire path. The point of running
// several distinct backends is that routing and failover are only observable
// across a fleet: a single-backend test can prove a request was proxied, but not
// that it reached the RIGHT backend, nor that a second one was tried after the
// first failed.
//
// Every claim is checked against evidence produced by the run rather than the
// gateway's own opinion of itself:
//
//   - each backend records the requests it received, so "the request went to the
//     backup" is proved by the backup's log and the primary's silence;
//   - /metrics is scraped for the attempt and failover counters;
//   - /admin/breakers is read to prove a backend was taken out of rotation and
//     later put back, including the half-open probe.
//
// Exit code 0 means every assertion passed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/breaker"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/logging"
	"github.com/infergate/infergate/internal/mockbackend"
	"github.com/infergate/infergate/internal/server"
)

const chatBody = `{"model":"mock-gpt","messages":[{"role":"user","content":"hi"}]}`

func main() {
	slowDelay := flag.Duration("slow-ttfb", 3*time.Second,
		"how long the deliberately slow backend stalls before its first stream frame")
	flag.Parse()

	out := os.Stdout
	fmt.Fprintln(out, "InferGate M1 end-to-end verification (routing + failover)")
	fmt.Fprintln(out, strings.Repeat("=", 72))

	c := &checker{out: out}
	env := &environment{out: out, slowTTFB: *slowDelay}

	checks := []struct {
		name string
		run  func(*checker, *environment)
	}{
		{"priority routing and failover on 5xx", checkPriorityAndFailover},
		{"no failover on a caller error (4xx)", checkNoFailoverOn4xx},
		{"timeout fails over instead of answering 504", checkTimeoutFailover},
		{"an exhausted fleet answers 504, not a hang", checkExhaustedFleet504},
		{"max_attempts caps the failover budget", checkMaxAttemptsBudget},
		{"capability routing excludes and matches", checkCapabilityRouting},
		{"explicit pin bypasses the fleet", checkExplicitPin},
		{"cost strategy prefers the cheaper backend", checkCostStrategy},
		{"latency strategy prefers the faster backend", checkLatencyStrategy},
		{"breaker takes a dead backend out and puts it back", checkBreakerLifecycle},
		{"admin and metrics surfaces report routing state", checkObservability},
	}

	for i, chk := range checks {
		fmt.Fprintf(out, "\n[%d/%d] %s\n", i+1, len(checks), chk.name)
		chk.run(c, env)
	}

	fmt.Fprintln(out, "\n"+strings.Repeat("=", 72))
	total, failed := c.tally()
	fmt.Fprintf(out, "RESULT: %d/%d assertions passed\n", total-failed, total)
	if failed > 0 {
		fmt.Fprintf(out, "FAILED: %d assertion(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Fprintln(out, "OK: M1 routing and failover acceptance criteria met")
}

// checker accumulates assertion results so one failure does not hide the rest.
type checker struct {
	out     io.Writer
	pass    int
	fail    int
	details []string
}

func (c *checker) assert(ok bool, format string, args ...any) bool {
	msg := fmt.Sprintf(format, args...)
	if ok {
		c.pass++
		fmt.Fprintf(c.out, "   PASS  %s\n", msg)
		return true
	}
	c.fail++
	c.details = append(c.details, msg)
	fmt.Fprintf(c.out, "   FAIL  %s\n", msg)
	return false
}

func (c *checker) info(format string, args ...any) {
	fmt.Fprintf(c.out, "         "+format+"\n", args...)
}

func (c *checker) tally() (int, int) { return c.pass + c.fail, c.fail }

// environment carries the settings shared by every check.
type environment struct {
	out      io.Writer
	slowTTFB time.Duration
}

// ---------------------------------------------------------------------------
// Scenario plumbing
// ---------------------------------------------------------------------------

// fleetEntry describes one backend of a scenario fleet.
type fleetEntry struct {
	name         string
	back         *mockbackend.Backend
	models       []string
	capabilities []string
	priority     int
	priceIn      float64
	priceOut     float64
}

// stack is one running gateway plus the backends behind it.
type stack struct {
	url      string
	breakers *breaker.Group
	close    func()
}

// newStack starts a gateway over the given backends. A fresh stack per check is
// deliberate: breaker state belongs to a gateway, so reusing one stack would let
// an earlier check's failures change a later check's routing decisions.
func (e *environment) newStack(c *checker, cfg *config.Config) *stack {
	// The gateway is built exactly as cmd/infergate builds it, so a passing
	// check is evidence about the shipped wiring, not about a test harness.
	logger, err := logging.New(io.Discard, "error", "text")
	if err != nil {
		c.assert(false, "build logger: %v", err)
		return nil
	}
	srv, err := server.NewServer(cfg, logger)
	if err != nil {
		c.assert(false, "build server: %v", err)
		return nil
	}
	handler := srv.Handler()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.assert(false, "listen: %v", err)
		return nil
	}
	httpSrv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()
	url := "http://" + ln.Addr().String()
	e.infof(c, "gateway %s", url)

	st := &stack{url: url, breakers: srv.Breakers()}
	st.close = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		_ = srv.Shutdown(ctx)
	}
	return st
}

func (e *environment) infof(c *checker, format string, args ...any) { c.info(format, args...) }

// baseConfig builds a gateway config over the entries, with every timeout long
// unless a check overrides it.
func baseConfig(entries []fleetEntry) *config.Config {
	cfg := config.Defaults()
	cfg.Server.UpstreamTimeout = config.Duration(30 * time.Second)
	cfg.Server.MaxBodyBytes = 8 << 20
	cfg.Log.Level = "error"
	cfg.Health = config.HealthConfig{
		Window:                 config.Duration(30 * time.Second),
		Buckets:                6,
		MinRequests:            2,
		FailureRatio:           0.5,
		OpenDuration:           config.Duration(300 * time.Millisecond),
		HalfOpenProbes:         1,
		MaxFailuresPerRequest:  2,
		RetryBackoff:           config.Duration(10 * time.Millisecond),
	}
	cfg.Pricing = config.PricingConfig{Default: config.ModelPrice{In: 1, Out: 3}, Models: map[string]config.ModelPrice{}}

	for _, entry := range entries {
		uc := config.UpstreamConfig{
			Name:         entry.name,
			Kind:         config.KindOpenAI,
			BaseURL:      entry.back.URL,
			Models:       entry.models,
			Capabilities: entry.capabilities,
			Priority:     entry.priority,
		}
		if uc.Models == nil {
			uc.Models = []string{"/"}
		}
		cfg.Upstreams = append(cfg.Upstreams, uc)
		// Price rows are keyed by the CONCRETE model name a backend would serve,
		// because that is what the router prices: for a catch-all candidate the
		// candidate model is the requested one, and priceOf() then falls back to
		// the requested name. A catch-all therefore only carries its own price
		// when it also declares a concrete alias alongside "/" -- which is
		// exactly the multi-model shape (a logical name plus the real model
		// behind it) and the only way two backends can disagree on price for the
		// same request.
		if entry.priceIn > 0 || entry.priceOut > 0 {
			for _, m := range uc.Models {
				if m == "/" {
					continue
				}
				cfg.Pricing.Models[m] = config.ModelPrice{In: entry.priceIn, Out: entry.priceOut}
			}
		}
	}
	return &cfg
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

type result struct {
	status   int
	body     string
	header   http.Header
	firstB   time.Duration
	elapsed  time.Duration
	frames   int
	frameGap time.Duration
}

// post sends one chat request and, for a stream, drains it while timing the
// first data frame. Draining synchronously matters: the gateway records
// first-token state in a deferred call that runs when its handler returns, so a
// test that reads /metrics before the body is consumed races the metric.
func post(c *checker, url, body string, hdr map[string]string, stream bool) result {
	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		c.assert(false, "build request: %v", err)
		return result{}
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		c.assert(false, "request failed: %v", err)
		return result{}
	}
	defer resp.Body.Close()
	res := result{status: resp.StatusCode, header: resp.Header.Clone()}
	if stream {
		buf := make([]byte, 4096)
		var seen []byte
		first := true
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if first && strings.Contains(string(buf[:n]), "data:") {
					res.firstB = time.Since(start)
					first = false
				}
				seen = append(seen, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
		res.body = string(seen)
		res.frames = strings.Count(res.body, "\n\n")
	} else {
		data, _ := io.ReadAll(resp.Body)
		res.body = string(data)
	}
	res.elapsed = time.Since(start)
	return res
}

func getJSON(c *checker, url string, v any) bool {
	resp, err := http.Get(url)
	if err != nil {
		c.assert(false, "GET %s: %v", url, err)
		return false
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, v); err != nil {
		c.assert(false, "GET %s: decode: %v (body=%s)", url, err, truncate(string(data), 300))
		return false
	}
	return true
}

func getText(c *checker, url string) string {
	resp, err := http.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}

// ---------------------------------------------------------------------------
// Metrics helpers
// ---------------------------------------------------------------------------

// metricSum adds up every labelled sample of a counter series. Summing only the
// first sample silently yields a wrong — usually zero — number, because each
// series is labelled per route/upstream/model/status.
func metricSum(text, series string) float64 {
	var total float64
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, series) {
			continue
		}
		rest := strings.TrimPrefix(line, series)
		if !strings.HasPrefix(rest, "{") && !strings.HasPrefix(rest, " ") {
			continue // a longer series name sharing this prefix
		}
		if idx := strings.LastIndex(line, " "); idx > 0 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(line[idx+1:]), 64); err == nil {
				total += v
			}
		}
	}
	return total
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func itoa(n int) string { return strconv.Itoa(n) }
