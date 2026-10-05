// Command loadtest measures the gateway's throughput and latency.
//
// Why this is Go and not hey/wrk: this host has no hey or wrk, and curl cannot
// time the first streamed token at all (the shell buffers). A Go generator can
// measure both the first-byte latency of a stream and its full duration, and it
// can compare the SAME workload with and without the gateway in the path —
// which is the only number that answers "what does the gateway cost me?".
//
// What it reports, and why each number matters:
//
//   - QPS          throughput at a fixed concurrency
//   - P50/P95/P99  tail latency; a gateway that averages well but has a fat tail
//     is worse for an agent framework than one with a slightly lower mean
//   - TTFT         time to first streamed token — the metric users feel
//   - stream time  full stream duration, i.e. generation plus relay overhead
//   - gateway mean mean latency reported by the gateway's OWN metrics, compared
//     against the client's measured mean; the difference is what the gateway
//     adds on top of handling the request
//
// Run with -all to spin up the mock upstream and the real gateway in-process on
// ephemeral ports and print a baseline table; run with -url to aim it at any
// already-running deployment.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/logging"
	"github.com/infergate/infergate/internal/server"
	"github.com/infergate/infergate/internal/sse"
	"github.com/infergate/infergate/internal/upstream"
)

func main() {
	var (
		target     = flag.String("url", "", "target base URL; empty means run the in-process baseline with -all")
		all        = flag.Bool("all", false, "run the full in-process baseline (direct and via gateway, stream and non-stream)")
		diag       = flag.Bool("diag", false, "attribute the gateway's cost: compare a bare reverse proxy, a minimal passthrough hop, and the real gateway")
		concurrency = flag.String("c", "8,32,128", "comma-separated concurrency levels")
		requests   = flag.Int("n", 800, "requests per phase (per concurrency level)")
		warmup     = flag.Int("warmup", 100, "warmup requests per phase, excluded from the numbers")
		rounds     = flag.Int("rounds", 3, "repeats per phase; the median is reported, because a single pass cannot separate a real effect from drift")
		timeout    = flag.Duration("timeout", 30*time.Second, "per-request timeout")
		ttfb       = flag.Duration("ttfb", 0, "mock's artificial delay before its first stream frame")
		out        = flag.String("out", "", "write the results as JSON to this path")
	)
	flag.Parse()

	levels, err := parseLevels(*concurrency)
	if err != nil {
		fatalf("%v", err)
	}

	if *diag {
		if err := runDiagnosis(levels[0], *requests, *warmup, *rounds, *timeout); err != nil {
			fatalf("%v", err)
		}
		return
	}
	if *target == "" && !*all {
		fatalf("nothing to do: pass -all for the in-process baseline, -diag to attribute gateway cost, or -url http://host:port for a running deployment")
	}
	if *all && *target != "" {
		fatalf("-all and -url are mutually exclusive")
	}

	if *all {
		if err := runBaseline(levels, *requests, *warmup, *rounds, *timeout, *ttfb, *out); err != nil {
			fatalf("%v", err)
		}
		return
	}

	client := newClient(*timeout)
	results, err := runMatrix(client, *target, levels, *requests, *warmup, *timeout)
	if err != nil {
		fatalf("%v", err)
	}
	printTable(results)
	if *out != "" {
		if err := writeJSON(*out, results); err != nil {
			fatalf("%v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// result types

// result is one measured phase: a workload (stream / non-stream) at one
// concurrency level against one target.
type result struct {
	Label       string  `json:"label"`
	Target      string  `json:"target"`
	Stream      bool    `json:"stream"`
	Concurrency int     `json:"concurrency"`
	Requests    int     `json:"requests"`
	Errors      int     `json:"errors"`
	QPS         float64 `json:"qps"`
	// Rounds and the QPS spread of the repeated runs: a wide spread means the
	// host was too noisy for the number to mean anything.
	Rounds  int     `json:"rounds,omitempty"`
	QPSMin  float64 `json:"qps_min,omitempty"`
	QPSMax  float64 `json:"qps_max,omitempty"`

	P50 time.Duration `json:"p50"`
	P95 time.Duration `json:"p95"`
	P99 time.Duration `json:"p99"`
	Max time.Duration `json:"max"`
	Mean time.Duration `json:"mean"`

	// Stream-only.
	TTFTP50 time.Duration `json:"ttft_p50,omitempty"`
	TTFTP95 time.Duration `json:"ttft_p95,omitempty"`
	TTFTMean time.Duration `json:"ttft_mean,omitempty"`
	BytesMean int64        `json:"bytes_mean,omitempty"`
	FramesMean float64    `json:"frames_mean,omitempty"`

	// Gateway-reported, when the target exposes /metrics. GatewayMean is the
	// gateway's own view of request duration; the gap to Mean is relay overhead
	// plus client/server network time.
	GatewayMean   time.Duration `json:"gateway_mean,omitempty"`
	GatewayCount  int           `json:"gateway_count,omitempty"`
	FirstTokenMean time.Duration `json:"gateway_first_token_mean,omitempty"`

	Wall time.Duration `json:"wall"`
}

type report struct {
	Started time.Time `json:"started"`
	Host    string    `json:"host"`
	Go      string    `json:"go_version"`
	Rounds  int       `json:"rounds"`
	Phases  []result  `json:"phases"`
}

// ---------------------------------------------------------------------------
// in-process baseline

// runBaseline starts the mock upstream and a real gateway in-process, then
// measures four phases that differ in exactly one variable each:
//
//	direct non-stream   the floor: what the backend itself costs
//	via gateway         non-stream adds only proxy-hop cost
//	direct stream       streaming floor, including first-token latency
//	via gateway stream  the number that matters for an agent framework
//
// Ports are ephemeral (httptest) so the baseline never collides with whatever
// else is bound on this host — :8080 is occupied here by an unrelated process.
//
// The ROUND is the outer loop and the phase is the inner loop, and each phase
// is repeated per round with the median taken. Ordering matters at this scale:
// a single pass runs "direct" first, and whatever warms up during it (CPU
// frequency, allocator arenas, TCP stack) flatters or penalises whoever runs
// later. Interleaving phases within a round keeps them in the same thermal and
// heap regime, and the median discards the first-round outlier.
func runBaseline(levels []int, requests, warmup, rounds int, timeout, ttfb time.Duration, out string) error {
	rep := report{Started: time.Now(), Host: hostname(), Go: goVersion(), Rounds: rounds}

	// The mock runs as a REAL child process on a real TCP port rather than as an
	// in-process httptest handler. Two reasons: (a) cmd/mockupstream owns the
	// OpenAI-compatible fixture, so there is exactly one definition of what a
	// backend looks like; (b) a loopback socket keeps the "direct" baseline
	// honest — an in-process handler would skip the kernel round trip that the
	// gateway phase must pay, which would flatter the gateway by the exact
	// amount we are trying to measure.
	//
	// token-delay 0: measuring the mock's per-token sleeps would say nothing
	// about the gateway.
	mock, err := startMock(ttfb)
	if err != nil {
		return err
	}
	defer mock.stop()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:              "127.0.0.1:0",
			ReadHeaderTimeout:   config.Duration(10 * time.Second),
			IdleTimeout:         config.Duration(90 * time.Second),
			UpstreamTimeout:     config.Duration(timeout),
			ShutdownTimeout:     config.Duration(5 * time.Second),
			MaxBodyBytes:        1 << 20,
			MaxIdleConnsPerHost: 256,
		},
		Upstreams: []config.UpstreamConfig{{
			Name:    "mock",
			Kind:    config.KindOpenAI,
			BaseURL: mock.url,
			Models:  []string{"/"},
		}},
		Log: config.LogConfig{Level: "error", Format: "text"},
		Pricing: config.PricingConfig{
			Default: config.ModelPrice{In: 0.15, Out: 0.60},
		},
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("baseline config: %w", err)
	}

	// A gateway with a discarding logger: at 800 requests per phase the log
	// itself becomes a measurable cost, and this command measures the gateway.
	logger, err := logging.New(io.Discard, "error", "text")
	if err != nil {
		return err
	}
	srv, err := server.NewServer(cfg, logger)
	if err != nil {
		return fmt.Errorf("baseline server: %w", err)
	}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	client := newClient(timeout)
	scenarios := []struct {
		label  string
		target string
		stream bool
	}{
		{"direct non-stream", mock.url, false},
		{"gateway non-stream", gw.URL, false},
		{"direct stream", mock.url, true},
		{"gateway stream", gw.URL, true},
	}

	fmt.Printf("mock    %s\n", mock.url)
	fmt.Printf("gateway %s\n", gw.URL)
	fmt.Printf("rounds  %d, n=%d per phase, warmup=%d, ttfb=%s\n\n", rounds, requests, warmup, ttfb)

	type key struct {
		label  string
		stream bool
		c      int
	}
	samples := map[key][]result{}
	for round := 0; round < rounds; round++ {
		for _, sc := range scenarios {
			for _, c := range levels {
				// Baseline for the gateway-delta below.
				beforeMean, beforeCount, _, _ := scrapeMetrics(gw.URL, client)

				r, err := runPhase(client, sc.label, sc.target, sc.stream, c, requests, warmup, timeout)
				if err != nil {
					return err
				}
				// The gateway's counters are cumulative, so a raw scrape would
				// mix every earlier phase and stream into one mean. Snapshot
				// before and after and keep the delta of THIS phase only.
				if strings.Contains(sc.label, "gateway") {
					if after, afterCount, ft, err := scrapeMetrics(gw.URL, client); err == nil {
						delta, deltaCount := after-beforeMean, afterCount-beforeCount
						if deltaCount > 0 {
							r.GatewayMean = time.Duration(float64(delta) / float64(deltaCount))
						}
						r.GatewayCount = deltaCount
						r.FirstTokenMean = ft
					}
				}
				samples[key{sc.label, sc.stream, c}] = append(samples[key{sc.label, sc.stream, c}], r)
				fmt.Printf("  round %d/%d  %-20s c=%-4d qps=%9.1f  p50=%-9s p95=%-9s errs=%d\n",
					round+1, rounds, r.Label, c, r.QPS, ms(r.P50), ms(r.P95), r.Errors)
			}
		}
	}

	// Summarise per phase in a stable order: non-stream first, direct before
	// gateway, concurrency ascending.
	for _, stream := range []bool{false, true} {
		for _, sc := range scenarios {
			if sc.stream != stream {
				continue
			}
			for _, c := range levels {
				group := samples[key{sc.label, stream, c}]
				if len(group) == 0 {
					continue
				}
				r := medianResult(group)
				// GatewayMean and FirstTokenMean were captured as per-phase
				// deltas at run time; nothing to re-scrape here.
				rep.Phases = append(rep.Phases, r)
			}
		}
	}

	fmt.Println()
	printTable(rep.Phases)
	printOverhead(rep.Phases)

	if out != "" {
		if err := writeJSON(out, rep); err != nil {
			return err
		}
		fmt.Printf("\nwrote %s\n", out)
	}
	return nil
}

// medianResult combines repeated runs of the same phase. Percentiles and means
// are medians of the per-round values, which is the honest summary for a host
// that cannot be quietened: it reports what the run typically did rather than
// the best or worst pass.
//
// The QPS spread is computed as well: if the rounds disagree by more than a few
// percent, the number is not trustworthy and the reader should say so instead
// of quoting it.
func medianResult(group []result) result {
	if len(group) == 1 {
		return group[0]
	}
	out := group[0]
	sorted := append([]result(nil), group...)
	med := func(get func(result) time.Duration) time.Duration {
		vals := make([]time.Duration, 0, len(sorted))
		for _, r := range sorted {
			vals = append(vals, get(r))
		}
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		return vals[len(vals)/2]
	}
	qps := make([]float64, 0, len(sorted))
	for _, r := range sorted {
		qps = append(qps, r.QPS)
	}
	sort.Float64s(qps)
	out.QPS = qps[len(qps)/2]
	out.QPSMin = qps[0]
	out.QPSMax = qps[len(qps)-1]
	out.P50 = med(func(r result) time.Duration { return r.P50 })
	out.P95 = med(func(r result) time.Duration { return r.P95 })
	out.P99 = med(func(r result) time.Duration { return r.P99 })
	out.Max = med(func(r result) time.Duration { return r.Max })
	out.Mean = med(func(r result) time.Duration { return r.Mean })
	out.TTFTP50 = med(func(r result) time.Duration { return r.TTFTP50 })
	out.TTFTP95 = med(func(r result) time.Duration { return r.TTFTP95 })
	out.TTFTMean = med(func(r result) time.Duration { return r.TTFTMean })
	out.Rounds = len(group)
	return out
}

// ---------------------------------------------------------------------------
// phase runner

// runMatrix measures an external deployment: both workloads at every
// concurrency level.
func runMatrix(client *http.Client, base string, levels []int, requests, warmup int, timeout time.Duration) ([]result, error) {
	var out []result
	for _, stream := range []bool{false, true} {
		label := "non-stream"
		if stream {
			label = "stream"
		}
		for _, c := range levels {
			r, err := runPhase(client, label, base, stream, c, requests, warmup, timeout)
			if err != nil {
				return nil, err
			}
			if gm, gc, ft, err := scrapeMetrics(base, client); err == nil {
				r.GatewayMean, r.GatewayCount, r.FirstTokenMean = gm, gc, ft
			}
			out = append(out, r)
			fmt.Printf("%-12s c=%-4d qps=%9.1f  p50=%-9s p95=%-9s p99=%-9s errs=%d\n",
				label, c, r.QPS, r.P50.Round(time.Microsecond), r.P95.Round(time.Microsecond),
				r.P99.Round(time.Microsecond), r.Errors)
		}
	}
	return out, nil
}

// runPhase drives `requests` requests at the given concurrency and returns the
// measured distribution.
//
// The dispatch channel is what bounds concurrency: exactly `concurrency`
// workers exist, so no semaphore is needed and the queue depth is 1. Requests
// are submitted as fast as workers retire, which keeps the target saturated for
// the whole measurement window.
func runPhase(client *http.Client, label, target string, stream bool, concurrency, requests, warmup int, timeout time.Duration) (result, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	base := strings.TrimRight(target, "/")

	// Warmup: connection pool establishment, TLS (if any), route table and log
	// setup all happen once. Including them would make the first phase look
	// slow for reasons that never recur.
	for i := 0; i < warmup; i++ {
		if _, err := doRequest(client, base, stream, timeout); err != nil {
			return result{}, fmt.Errorf("warmup against %s: %w", base, err)
		}
	}

	var (
		mu       sync.Mutex
		lat      = make([]time.Duration, 0, requests)
		ttft     = make([]time.Duration, 0, requests)
		bytesN   int64
		framesN  int64
		errs     int
		firstErr string
	)
	frames := make([]int64, requests)

	jobs := make(chan int)
	var wg sync.WaitGroup

	wallStart := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				start := time.Now()
				m, err := doRequest(client, base, stream, timeout)
				elapsed := time.Since(start)

				mu.Lock()
				if err != nil {
					errs++
					if firstErr == "" {
						firstErr = err.Error()
					}
					mu.Unlock()
					continue
				}
				lat = append(lat, elapsed)
				bytesN += m.bytes
				framesN += m.frames
				if idx < len(frames) {
					frames[idx] = m.frames
				}
				if stream && m.firstToken > 0 {
					ttft = append(ttft, m.firstToken)
				}
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < requests; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	wall := time.Since(wallStart)

	if len(lat) == 0 {
		return result{}, fmt.Errorf("phase %s c=%d: every request failed (first error: %s)", label, concurrency, firstErr)
	}

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	sort.Slice(ttft, func(i, j int) bool { return ttft[i] < ttft[j] })

	r := result{
		Label:       label,
		Target:      base,
		Stream:      stream,
		Concurrency: concurrency,
		Requests:    len(lat),
		Errors:      errs,
		QPS:         float64(len(lat)) / wall.Seconds(),
		P50:         pick(lat, 0.50),
		P95:         pick(lat, 0.95),
		P99:         pick(lat, 0.99),
		Max:         lat[len(lat)-1],
		Mean:        mean(lat),
		BytesMean:   bytesN / int64(len(lat)),
		FramesMean:  float64(framesN) / float64(len(lat)),
		Wall:        wall,
	}
	if len(ttft) > 0 {
		r.TTFTP50 = pick(ttft, 0.50)
		r.TTFTP95 = pick(ttft, 0.95)
		r.TTFTMean = mean(ttft)
	}
	if r.Errors > 0 {
		fmt.Printf("  warning: %d/%d requests failed in %s c=%d (%s)\n",
			r.Errors, r.Requests+r.Errors, label, concurrency, firstErr)
	}

	// A loopback load generator is itself a suspect. If each of the c client
	// goroutines completed in less than one scheduling quantum per request, the
	// measured QPS is the client's ceiling, not the target's. That is not a
	// failure — the throughput number is simply not about the server — and it is
	// invisible in the table, so say it in words.
	perGoroutine := wall / time.Duration(max(1, requests/concurrency))
	if perGoroutine < 250*time.Microsecond {
		fmt.Printf("  note: %s c=%d is generator-bound (%s per request per goroutine); QPS here measures this client, not the target\n",
			label, concurrency, perGoroutine.Round(time.Microsecond))
	}
	return r, nil
}

// measurements is what one request tells us.
type measurements struct {
	firstToken time.Duration
	frames     int64
	bytes      int64
}

var chatBody = []byte(`{"model":"mock-gpt","messages":[{"role":"user","content":"hello"}]}`)
var streamBody = []byte(`{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"hello"}]}`)

func doRequest(client *http.Client, base string, stream bool, timeout time.Duration) (measurements, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	body := chatBody
	if stream {
		body = streamBody
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return measurements{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer loadtest-key")

	resp, err := client.Do(req)
	if err != nil {
		return measurements{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return measurements{}, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}

	if !stream {
		n, err := io.Copy(io.Discard, resp.Body)
		if err != nil {
			return measurements{}, err
		}
		return measurements{bytes: n}, nil
	}

	// Streaming: parse with the same frame reader the gateway uses, so the
	// measurement sees exactly the frames a real client sees. A malformed frame
	// is an error, not a silently skipped byte — this is what catches a relay
	// that drops the blank-line separator under load.
	start := time.Now()
	var m measurements
	// One reader, wrapping the counting reader: a second sse.NewReader over the
	// same body would steal bytes from the first and desynchronise framing.
	counting := &countingReader{r: resp.Body}
	reader := sse.NewReader(counting)
	for {
		frame, err := reader.NextFrame()
		if err != nil {
			if err == io.EOF {
				break
			}
			return measurements{}, err
		}
		if frame.IsComment() || len(frame.Data) == 0 {
			continue
		}
		if frame.IsDone() {
			break
		}
		var payload struct {
			Choices []struct {
				Delta struct {
					Content   string          `json:"content"`
					ToolCalls json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(frame.Data, &payload); err != nil {
			return measurements{}, fmt.Errorf("invalid frame payload %q: %w", truncate(string(frame.Data), 80), err)
		}
		m.frames++
		if m.firstToken == 0 && len(payload.Choices) > 0 {
			d := payload.Choices[0].Delta
			if d.Content != "" || len(d.ToolCalls) > 0 {
				m.firstToken = time.Since(start)
			}
		}
	}
	m.bytes = counting.n
	return m, nil
}

// countingReader counts the bytes actually delivered on the wire, which is
// compared against the gateway's own stream_bytes counter.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ---------------------------------------------------------------------------
// diagnosis: where does the gateway's cost actually go?

// runDiagnosis answers a question the baseline table raises but cannot settle:
// the gateway is ~3x slower than talking to the backend directly, and "it is a
// Go HTTP server in the middle" is not an explanation — it is a hypothesis with
// three competing variants. So measure each:
//
//	direct            one HTTP hop (client -> backend)
//	plain reverse     two hops via net/http/httputil with the process-wide
//	                  http.DefaultTransport (MaxIdleConnsPerHost=2)
//	tuned reverse     the same httputil proxy, but given the same tuning the
//	                  gateway applies — that isolates connection pooling from the
//	                  proxy implementation itself
//	minimal passthrough  two hops via a hand-written proxy that still uses the
//	                  real *upstream.Registry and its connection pool, but skips
//	                  body parsing, metrics, logging and error taxonomy
//	infergate         two hops through the real gateway
//
// Whatever plain-vs-direct costs is the price of an extra hop on this host (two
// TCP handshakes and two sets of syscalls per request, since each hop has its
// own connection). Whatever infergate-vs-tuned costs is what the gateway's own
// logic adds — and that is the number worth optimizing.
func runDiagnosis(concurrency, requests, warmup, rounds int, timeout time.Duration) error {
	mock, err := startMock(0)
	if err != nil {
		return err
	}
	defer mock.stop()

	reg, err := upstream.New(&config.Config{
		Server: config.ServerConfig{MaxIdleConnsPerHost: 256},
		Upstreams: []config.UpstreamConfig{{
			Name: "mock", Kind: config.KindOpenAI, BaseURL: mock.url, Models: []string{"/"},
		}},
	})
	if err != nil {
		return err
	}
	defer reg.CloseIdleConnections()
	target, ok := reg.Target("mock")
	if !ok {
		return fmt.Errorf("registry lost the mock target")
	}

	// 1. plain httputil reverse proxy: http.DefaultTransport's idle pool is 2
	// connections per host, so at c=32 most requests pay a fresh TCP handshake.
	plainTarget, _ := url.Parse(mock.url)
	plain := httptest.NewServer(httputil.NewSingleHostReverseProxy(plainTarget))
	defer plain.Close()

	// 2. same proxy, pooled transport
	tunedProxy := httputil.NewSingleHostReverseProxy(plainTarget)
	tunedProxy.Transport = target.Transport
	tuned := httptest.NewServer(tunedProxy)
	defer tuned.Close()

	// 3. minimal passthrough: shares the real connection pool, does no work
	minimal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := r.Clone(context.Background())
		out.URL.Scheme = "http"
		out.URL.Host = strings.TrimPrefix(mock.url, "http://")
		out.RequestURI = ""
		out.Host = out.URL.Host
		out.ContentLength = r.ContentLength
		resp, err := target.Transport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer minimal.Close()

	// 4. the real gateway
	logger, err := logging.New(io.Discard, "error", "text")
	if err != nil {
		return err
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:              "127.0.0.1:0",
			ReadHeaderTimeout:   config.Duration(10 * time.Second),
			IdleTimeout:         config.Duration(90 * time.Second),
			UpstreamTimeout:     config.Duration(timeout),
			ShutdownTimeout:     config.Duration(5 * time.Second),
			MaxBodyBytes:        1 << 20,
			MaxIdleConnsPerHost: 256,
		},
		Upstreams: []config.UpstreamConfig{{
			Name: "mock", Kind: config.KindOpenAI, BaseURL: mock.url, Models: []string{"/"},
		}},
		Log: config.LogConfig{Level: "error", Format: "text"},
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	srv, err := server.NewServer(cfg, logger)
	if err != nil {
		return err
	}
	real := httptest.NewServer(srv.Handler())
	defer real.Close()

	client := newClient(timeout)

	fmt.Printf("diagnosis at c=%d, n=%d per phase, median of %d rounds\n\n", concurrency, requests, rounds)

	rows := []struct {
		label  string
		target string
	}{
		{"direct (1 hop)", mock.url},
		{"plain httputil pool=2", plain.URL},
		{"tuned httputil pool=256", tuned.URL},
		{"minimal passthrough pool=256", minimal.URL},
		{"infergate pool=256", real.URL},
	}

	// Rounds outer, configurations inner: every configuration sees the same
	// thermal and heap conditions within a round, and the median discards the
	// warm-up round.
	collected := make(map[string][]result, len(rows))
	for round := 0; round < rounds; round++ {
		for _, row := range rows {
			r, err := runPhase(client, row.label, row.target, false, concurrency, requests, warmup, timeout)
			if err != nil {
				return err
			}
			collected[row.label] = append(collected[row.label], r)
			fmt.Printf("  round %d/%d  %-32s qps=%9.1f\n", round+1, rounds, row.label, r.QPS)
		}
	}

	var baseline float64
	var baselineSpread float64
	for _, row := range rows {
		r := medianResult(collected[row.label])
		if baseline == 0 {
			baseline = r.QPS
			if r.QPSMin > 0 {
				baselineSpread = (r.QPSMax - r.QPSMin) / r.QPS * 100
			}
		}
		share := 0.0
		if baseline > 0 {
			share = r.QPS / baseline * 100
		}
		spread := 0.0
		if r.QPS > 0 && r.QPSMin > 0 {
			spread = (r.QPSMax - r.QPSMin) / r.QPS * 100
		}
		fmt.Printf("%-32s qps=%9.1f (%5.1f%% of direct, spread %.0f%%)  p50=%-9s p95=%-9s p99=%-9s\n",
			row.label, r.QPS, share, spread, ms(r.P50), ms(r.P95), ms(r.P99))
	}
	fmt.Println()
	if baselineSpread > 5 {
		fmt.Printf("NOTE: the direct baseline itself varied %.0f%% across rounds, so treat differences below that as noise.\n", baselineSpread)
	}
	fmt.Println("read it as: hop cost = plain vs direct; pool effect = tuned vs plain;")
	fmt.Println("            gateway logic cost = infergate vs the better of tuned/minimal.")
	return nil
}

// ---------------------------------------------------------------------------
// mock upstream child process

// mockProc is a running cmd/mockupstream child.
type mockProc struct {
	url  string
	cmd  *exec.Cmd
	log  *os.File
	done chan struct{}
}

// startMock launches cmd/mockupstream on an ephemeral port and waits until it
// answers /healthz, so the load test never races the backend's startup.
//
// The port is chosen by the OS and read back from net.Listen: hard-coding 9000
// would collide with a developer's own mock, and :8080 on this host is already
// taken by an unrelated process.
func startMock(ttfb time.Duration) (*mockProc, error) {
	bin, err := mockBinary()
	if err != nil {
		return nil, err
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := l.Addr().String()
	_ = l.Close()

	args := []string{"-listen", addr, "-name", "loadtest-mock", "-token-delay", "0"}
	if ttfb > 0 {
		args = append(args, "-ttfb", ttfb.String())
	}
	cmd := exec.Command(bin, args...)

	// The child writes to FILES, not pipes. Under this host's sandbox a process
	// cannot open the named pipes that exec.Cmd uses for piped stdio, and a
	// blocked child would look exactly like a slow backend. Files avoid the
	// whole class of problem and double as a diagnostic when startup fails.
	logPath := filepath.Join(os.TempDir(), fmt.Sprintf("infergate-loadtest-mock-%d.log", os.Getpid()))
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create mock log: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}

	m := &mockProc{url: "http://" + addr, cmd: cmd, log: logFile, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(m.done)
	}()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-m.done:
			tail, _ := os.ReadFile(logPath)
			m.stop()
			return nil, fmt.Errorf("mock exited during startup: %s", strings.TrimSpace(string(tail)))
		default:
		}
		resp, err := client.Get(m.url + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return m, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.stop()
	return nil, fmt.Errorf("mock at %s did not become healthy within 15s (log: %s)", m.url, logPath)
}

func (m *mockProc) stop() {
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		select {
		case <-m.done:
		case <-time.After(5 * time.Second):
		}
	}
	if m.log != nil {
		m.log.Close()
	}
}

// mockBinary finds a prebuilt mockupstream next to the gateway, or builds one
// into the OS temp directory if the repo has not been built yet.
func mockBinary() (string, error) {
	for _, candidate := range []string{
		filepath.Join("bin", "mockupstream.exe"),
		filepath.Join("bin", "mockupstream"),
	} {
		if abs, err := filepath.Abs(candidate); err == nil {
			if st, err := os.Stat(abs); err == nil && !st.IsDir() {
				return abs, nil
			}
		}
	}
	out := filepath.Join(os.TempDir(), "infergate-mockupstream.exe")
	if os.PathSeparator != '\\' {
		out = filepath.Join(os.TempDir(), "infergate-mockupstream")
	}
	build := exec.Command("go", "build", "-o", out, "./cmd/mockupstream")
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("mockupstream not built and no `go` on PATH to build it (%v); run tools/go.cmd build -o bin/mockupstream.exe ./cmd/mockupstream", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// gateway metrics scrape

var (
	reRequests = regexp.MustCompile(`infergate_requests_total\{[^}]*\}\s+(\d+)`)
	reDuration = regexp.MustCompile(`infergate_request_duration_seconds_sum\{[^}]*\}\s+([0-9.eE+-]+)`)
	reFirstTok = regexp.MustCompile(`infergate_first_token_seconds_mean\{[^}]*\}\s+([0-9.eE+-]+)`)
)

// scrapeMetrics reads the gateway's own counters. It is best-effort: a target
// that is not an InferGate exposes no such endpoint and the phase still counts.
func scrapeMetrics(base string, client *http.Client) (mean time.Duration, count int, firstToken time.Duration, err error) {
	resp, err := client.Get(strings.TrimRight(base, "/") + "/metrics")
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, 0, 0, err
	}
	text := string(raw)

	var totalCount int
	var totalSeconds float64
	for _, m := range reRequests.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[1])
		totalCount += n
	}
	for _, m := range reDuration.FindAllStringSubmatch(text, -1) {
		f, _ := strconv.ParseFloat(m[1], 64)
		totalSeconds += f
	}
	if totalCount > 0 && totalSeconds > 0 {
		mean = time.Duration(totalSeconds / float64(totalCount) * float64(time.Second))
	}
	if m := reFirstTok.FindStringSubmatch(text); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		firstToken = time.Duration(f * float64(time.Second))
	}
	return mean, totalCount, firstToken, nil
}

// ---------------------------------------------------------------------------
// reporting

// shortTarget renders a base URL as host:port so the per-phase table stays
// narrow; "direct" is reserved for the in-process backend baseline.
func shortTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

func printTable(phases []result) {
	fmt.Println("| workload | target | c | QPS | P50 | P95 | P99 | TTFT P50 | TTFT P95 | errors |")
	fmt.Println("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, r := range phases {
		// Name the target from the label when the label says what it is, and
		// from the URL otherwise. The old rule ("no 'gateway' in the label ->
		// direct") printed the caller's own endpoint as "direct" in -url mode,
		// which reads as "this is the no-gateway baseline" in a table that is
		// often pasted into a report.
		target := "direct"
		switch {
		case strings.HasPrefix(r.Label, "direct"):
			target = "direct"
		case strings.Contains(r.Label, "gateway"):
			target = "gateway"
		case r.Target != "":
			target = shortTarget(r.Target)
		}
		ttft50, ttft95 := "-", "-"
		if r.Stream {
			ttft50 = ms(r.TTFTP50)
			ttft95 = ms(r.TTFTP95)
		}
		fmt.Printf("| %s | %s | %d | %.1f | %s | %s | %s | %s | %s | %d |\n",
			strings.ReplaceAll(r.Label, "gateway ", ""), target, r.Concurrency, r.QPS,
			ms(r.P50), ms(r.P95), ms(r.P99), ttft50, ttft95, r.Errors)
	}
}

// printOverhead pairs each direct phase with its gateway phase at the same
// concurrency and prints the delta. This is the number to quote: "the gateway
// costs X ms of P95", not "the gateway does Y QPS" (which depends entirely on
// the backend behind it).
func printOverhead(phases []result) {
	fmt.Println("gateway overhead vs direct:")
	for _, stream := range []bool{false, true} {
		for _, g := range phases {
			if g.Stream != stream || !strings.Contains(g.Label, "gateway") {
				continue
			}
			for _, d := range phases {
				if d.Stream != stream || strings.Contains(d.Label, "gateway") || d.Concurrency != g.Concurrency {
					continue
				}
				fmt.Printf("  %-9s c=%-4d  QPS %8.1f -> %8.1f (%+.1f%%)   P95 %8s -> %8s (%+s)   P99 %+s\n",
					streamLabel(stream), g.Concurrency,
					d.QPS, g.QPS, pct(g.QPS, d.QPS),
					ms(d.P95), ms(g.P95), ms(g.P95-d.P95), ms(g.P99-d.P99))
			}
		}
	}
	if len(phases) > 0 {
		for _, r := range phases {
			if r.GatewayMean > 0 {
				// When the gateway's own span is far below what the client
				// measured, the gap is NOT gateway overhead -- the gateway
				// finished and went back to waiting. On this host the client
				// (curl-equivalent Go HTTP + SSE parse + JSON decode) is the
				// slower half once the mock has no injected delay, so the
				// honest reading is "most of this is the client", and the line
				// says so instead of implying the gateway burned the time.
				share := 100 * float64(r.GatewayMean) / float64(r.Mean)
				if share < 50 {
					fmt.Printf("  client-observed mean %s at c=%d, of which the gateway's own processing span is only %s (%.1f%%): the load generator is the slower half here, not the gateway\n",
						ms(r.Mean), r.Concurrency, ms(r.GatewayMean), share)
				} else {
					fmt.Printf("  gateway-internal mean %s vs client-observed mean %s at c=%d (delta %s: hop + relay overhead)\n",
						ms(r.GatewayMean), ms(r.Mean), r.Concurrency, ms(r.Mean-r.GatewayMean))
				}
			}
		}
	}
}

func streamLabel(stream bool) string {
	if stream {
		return "stream"
	}
	return "non-stream"
}

func pct(newV, oldV float64) float64 {
	if oldV == 0 {
		return 0
	}
	return (newV - oldV) / oldV * 100
}

func ms(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.0fus", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.3fs", d.Seconds())
	}
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// ---------------------------------------------------------------------------
// helpers

func newClient(timeout time.Duration) *http.Client {
	// Keep-alive matters more than usual here: with the default transport this
	// process would behave like every other Go client, so the comparison is
	// fair. MaxIdleConnsPerHost is raised because the load generator itself
	// would otherwise become the bottleneck at c=128 (Go's default is 2 idle
	// conns per host, which forces reconnect churn under concurrency).
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

func parseLevels(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("-c %q: %q is not an integer", s, part)
		}
		if n < 1 {
			return nil, fmt.Errorf("-c %q: concurrency must be >= 1", s)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-c: no concurrency levels given")
	}
	sort.Ints(out)
	return out, nil
}

// pick returns the nearest-rank percentile of a sorted slice. Nearest-rank (not
// interpolation) because it always reports a latency that was actually
// observed — an interpolated P99 can quote a number no request ever saw.
func pick(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func mean(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	var total time.Duration
	for _, s := range samples {
		total += s
	}
	return total / time.Duration(len(samples))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func goVersion() string {
	return runtime.Version()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "loadtest: "+format+"\n", args...)
	os.Exit(1)
}
