package main

// Observability checks: the bounded percentile window, the histogram
// invariants, and what an operator can actually see in /metrics and /stats.
//
// The window and histogram checks run against a bare Recorder, because the
// claim is about a data structure and a stack would only add latency noise. The
// exposure checks run against the assembled gateway, because the claim there is
// "this line appears on the endpoint Prometheus scrapes".

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/metrics"
)

// m5Config is the config every M5 stack is built from: one catch-all backend and
// tracing off unless a check turns it on.
func m5Config(up *recordingUpstream) config.Config {
	cfg := baseConfig(upstreamConfig("m5-mock", up.URL()))
	cfg.Health.MinRequests = 1000 // never trip a breaker during an observability check
	return cfg
}

// ---------------------------------------------------------------------------
// 1. the percentile window
// ---------------------------------------------------------------------------

func checkLatencyWindow(c *checker) {
	section("1. the percentile window is bounded and keeps the newest samples")

	rec := metrics.NewRecorderWithWindow(512)
	c.equal(rec.RequestLatencyWindow(), 512, "1.1 a recorder reports the window it was built with")
	c.equal(rec.RequestLatencyDropped(), int64(0), "1.2 a fresh recorder has dropped nothing")

	for _, d := range durations(2000, time.Millisecond) {
		rec.ObserveRequest("/v1/chat/completions", "m5-mock", "mock-gpt", 200, metrics.OutcomeSuccess, d)
	}

	samples := rec.RequestLatencySamples()
	c.equal(len(samples), 512, "1.3 after 2000 observations the window holds exactly 512 samples")
	c.equal(rec.RequestLatencyDropped(), int64(1488), "1.4 the 1488 overwritten samples are counted as dropped, not silently forgotten")
	if len(samples) == 512 {
		c.equal(samples[0], 1489*time.Millisecond, "1.5 the oldest retained sample is the 1489th observation")
		c.equal(samples[511], 2000*time.Millisecond, "1.6 the newest observation is retained")
		c.equal(nearestRank(samples, 0.50), 1744*time.Millisecond, "1.7 p50 is the median of the retained window")
		c.equal(nearestRank(samples, 0.95), 1975*time.Millisecond, "1.8 p95 is the 95th percentile of the retained window")
	}

	// A window that has not wrapped must be indistinguishable from the old
	// unbounded slice: that is what keeps the M2/M3 baselines comparable.
	small := metrics.NewRecorderWithWindow(4)
	for _, d := range []time.Duration{1, 2, 3, 4} {
		small.ObserveRequest("/v1/chat/completions", "m5-mock", "mock-gpt", 200, metrics.OutcomeSuccess, d*time.Millisecond)
	}
	c.equal(fmt.Sprint(small.RequestLatencySamples()), "[1ms 2ms 3ms 4ms]",
		"1.9 an unwrapped window returns every sample in insertion order")
	c.equal(small.RequestLatencyDropped(), int64(0), "1.10 an unwrapped window reports no drops")

	c.equal(metrics.NewRecorder().RequestLatencyWindow(), metrics.DefaultLatencyWindow,
		"1.11 the plain constructor uses the documented default window")
	c.equal(metrics.DefaultLatencyWindow, 65536,
		"1.12 the default window is 65536 samples, a constant 512 KiB of durations")

	small.Reset()
	c.equal(small.RequestLatencyDropped(), int64(0), "1.13 Reset clears the drop counter")
	c.equal(len(small.RequestLatencySamples()), 0, "1.14 Reset empties the window")
	c.equal(small.RequestLatencyWindow(), 4, "1.15 Reset keeps the configured window")
}

// nearestRank is the percentile definition /stats uses: the value at
// ceil(q*n)-1 of the ascending samples.
func nearestRank(samples []time.Duration, q float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ---------------------------------------------------------------------------
// 2. histogram invariants
// ---------------------------------------------------------------------------

func checkHistogramInvariants(c *checker) {
	section("2. the request-duration histogram is a real cumulative histogram")

	rec := metrics.NewRecorder()
	sum := time.Duration(0)
	for i := 1; i <= 10; i++ {
		d := time.Duration(i) * time.Millisecond
		sum += d
		rec.ObserveRequest("/v1/chat/completions", "m5-mock", "mock-gpt", 200, metrics.OutcomeSuccess, d)
	}

	rows := rec.RequestDurationHistograms()
	if !c.assert(len(rows) == 1, "2.1 one label set produced exactly one histogram series (got %d)", len(rows)) {
		return
	}
	row := rows[0]
	c.equal(row.Route, "/v1/chat/completions", "2.2 the series is keyed by route")
	c.equal(row.Upstream, "m5-mock", "2.3 the series is keyed by upstream")
	c.equal(row.Model, "mock-gpt", "2.4 the series is keyed by model")
	c.equal(row.Count, int64(10), "2.5 every observation is counted")
	if !c.assert(math.Abs(row.Sum-sum.Seconds()) < 1e-9,
		"2.6 the sum is the sum of the observed seconds (got %v, want %v)", row.Sum, sum.Seconds()) {
	}

	var total int64
	for _, n := range row.Counts {
		total += n
	}
	c.equal(total, row.Count, "2.7 the non-cumulative bucket counts add up to the total count")

	bounds := row.Bounds()
	c.equal(len(bounds), 13, "2.8 the duration bucket set has 13 finite bounds")
	c.equal(bounds[0], 0.001, "2.9 the first bound is 1ms so gateway overhead is not hidden in the first bucket")
	c.equal(bounds[len(bounds)-1], 10.0, "2.10 the last finite bound is 10s")

	buckets := row.Buckets()
	c.equal(len(buckets), len(bounds)+1, "2.11 there is one cumulative bucket per bound plus +Inf")
	if len(buckets) > 0 {
		c.assert(buckets[len(buckets)-1].IsInf(), "2.12 the final bucket is the +Inf catch-all")
		c.equal(buckets[len(buckets)-1].Count, int64(10), "2.13 the +Inf bucket holds every observation")
		c.equal(buckets[len(buckets)-1].Le(), "+Inf", "2.14 the catch-all renders its le label as +Inf")
		previous := int64(-1)
		monotonic := true
		for _, b := range buckets {
			if b.Count < previous {
				monotonic = false
			}
			previous = b.Count
		}
		c.assert(monotonic, "2.15 cumulative counts never decrease across %s", "le")
	}

	// The `le` promise: an observation exactly on a bound belongs to that
	// bound's bucket, not the one below it.
	exact := metrics.NewRecorder()
	exact.ObserveRequest("/v1/chat/completions", "m5-mock", "mock-gpt", 200, metrics.OutcomeSuccess, 5*time.Millisecond)
	exactBuckets := exact.RequestDurationHistograms()[0].Buckets()
	found := false
	for _, b := range exactBuckets {
		if b.UpperBound == 0.005 {
			found = true
			c.equal(b.Count, int64(1), "2.16 an observation exactly on a bound lands in that bound's bucket (le is inclusive)")
		}
		if b.UpperBound == 0.0025 {
			c.equal(b.Count, int64(0), "2.17 the bucket below the exact bound stays empty")
		}
	}
	c.assert(found, "2.18 the 5ms bound exists in the default bucket set")

	// Negatives and NaN are clamped, not recorded as nonsense.
	clamped := metrics.NewRecorder()
	clamped.ObserveRequest("/v1/chat/completions", "m5-mock", "mock-gpt", 200, metrics.OutcomeSuccess, -time.Second)
	clamped.ObserveRequest("/v1/chat/completions", "m5-mock", "mock-gpt", 200, metrics.OutcomeSuccess, time.Duration(math.NaN()))
	clampedRow := clamped.RequestDurationHistograms()[0]
	c.equal(clampedRow.Count, int64(2), "2.19 a backwards clock still counts as an observation")
	c.equal(clampedRow.Sum, 0.0, "2.20 a negative or NaN observation contributes 0 to the sum, keeping the mean sane")

	// The other three families exist and are keyed as documented.
	attempts := metrics.NewRecorder()
	attempts.ObserveUpstreamAttempt("m5-mock", 200, metrics.OutcomeSuccess, 3*time.Millisecond)
	attemptRows := attempts.AttemptDurationHistograms()
	c.assert(len(attemptRows) == 1, "2.21 an upstream attempt produces one attempt-duration series (got %d)", len(attemptRows))
	if len(attemptRows) == 1 {
		c.equal(attemptRows[0].Upstream, "m5-mock", "2.22 the attempt series is keyed by upstream")
		c.equal(attemptRows[0].Count, int64(1), "2.23 the attempt series counts the attempt")
	}

	first := metrics.NewRecorder()
	first.ObserveFirstToken("m5-mock", "mock-gpt", 120*time.Millisecond)
	firstRows := first.FirstTokenHistograms()
	c.assert(len(firstRows) == 1, "2.24 a first token produces one TTFT series (got %d)", len(firstRows))
	if len(firstRows) == 1 {
		c.equal(len(firstRows[0].Bounds()), 9, "2.25 the TTFT bucket set is the tighter 9-bound set")
		c.equal(firstRows[0].Count, int64(1), "2.26 the TTFT series counts the sample")
	}

	tokens := metrics.NewRecorder()
	tokens.ObserveTokens("m5-mock", "mock-gpt", 7, 3, 0)
	tokens.ObserveRequest("/v1/chat/completions", "m5-mock", "mock-gpt", 200, metrics.OutcomeSuccess, time.Millisecond)
	tokenRows := tokens.CompletionTokensHistograms()
	c.assert(len(tokenRows) == 1, "2.27 completion tokens produce one per-request series (got %d)", len(tokenRows))
	if len(tokenRows) == 1 {
		c.equal(tokenRows[0].Count, int64(1), "2.28 the token histogram counts the observed completion")
	}
}

// ---------------------------------------------------------------------------
// 3. the exposition
// ---------------------------------------------------------------------------

// runtimeFamilies are the process-fact families a dashboard needs to tell "the
// gateway is slow" from "this laptop is out of memory".
var runtimeFamilies = []string{
	"infergate_runtime_goroutines",
	"infergate_runtime_num_cpu",
	"infergate_runtime_memstats_alloc_bytes",
	"infergate_runtime_memstats_heap_alloc_bytes",
	"infergate_runtime_memstats_heap_inuse_bytes",
	"infergate_runtime_memstats_heap_objects",
	"infergate_runtime_memstats_stack_inuse_bytes",
	"infergate_runtime_memstats_sys_bytes",
	"infergate_runtime_memstats_total_alloc_bytes",
	"infergate_runtime_memstats_gc_cycles_total",
	"infergate_runtime_gc_pause_seconds_total",
	"infergate_runtime_gc_last_pause_seconds",
	"infergate_runtime_go_version",
}

// m0ScrapeRegex is the exact regex scripts/verify-m0.ps1 uses to read the
// request-duration total. The M5 work replaced the old counter family with a
// real histogram, whose own _sum line must keep matching it -- otherwise the M0
// gate silently starts asserting nothing.
var m0ScrapeRegex = regexp.MustCompile(`(?m)^infergate_request_duration_seconds_sum\{[^}]*\}\s+([0-9.eE+-]+)\s*$`)

func checkMetricsExposition(c *checker) {
	section("3. /metrics exposes real histograms and the process facts")

	up := newUpstream(upstreamAuto, "m5 answer")
	defer up.Close()
	cfg := m5Config(up)
	cfg.Tracing.Enabled = false
	st := newStack(c, "metrics", &cfg)
	if st == nil {
		return
	}
	defer st.Close(c, "metrics")

	body := chatBody("mock-gpt", "exposition")
	for i := 0; i < 3; i++ {
		res := post(c, st.url, "/v1/chat/completions", body, nil, "metrics: non-stream request")
		c.assert(res.status == 200, "3.1 a non-stream request is served (got %d: %s)", res.status, truncate(res.body, 160))
	}
	stream := post(c, st.url, "/v1/chat/completions", streamChatBody("mock-gpt", "exposition stream"), nil, "metrics: stream request")
	c.assert(stream.status == 200, "3.2 a stream request is served (got %d: %s)", stream.status, truncate(stream.body, 160))

	res := get(c, st.url, "/metrics", "metrics: scrape")
	if !c.assert(res.status == 200, "3.3 /metrics answers 200 (got %d)", res.status) {
		return
	}
	text := res.body
	samples := parseSamples(text)
	present := families(text)

	expected := []string{
		"infergate_requests_total",
		"infergate_request_duration_seconds",
		"infergate_upstream_attempt_duration_seconds",
		"infergate_first_token_seconds",
		"infergate_completion_tokens_per_request",
		"infergate_first_token_seconds_mean",
		"infergate_stream_frames_total",
		"infergate_stream_bytes_total",
		"infergate_tokens_total",
		"infergate_failovers_total",
		"infergate_breaker_state",
	}
	for _, name := range expected {
		c.assert(present[name], "3.4 # TYPE %s is declared", name)
	}
	for _, name := range runtimeFamilies {
		c.assert(present[name], "3.5 # TYPE %s is declared", name)
	}

	// The histogram the M0 gate reads.
	c.assert(m0ScrapeRegex.MatchString(text),
		"3.6 the M0 scrape regex still matches an infergate_request_duration_seconds_sum line")

	// Every family must carry HELP as well as TYPE, even the empty ones.
	helps := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "# HELP ") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				helps[fields[2]] = true
			}
		}
	}
	for _, name := range append(append([]string{}, expected...), runtimeFamilies...) {
		c.assert(helps[name], "3.7 # HELP %s is declared", name)
	}

	// The request-duration histogram is internally consistent and counted. All
	// four requests share one series: a streamed request is still a request, so
	// the count is 4 and the streamed/whole split shows up in TTFT and frames
	// instead.
	groups := histogramGroups(samples, "infergate_request_duration_seconds")
	c.assert(len(groups) >= 1, "3.8 the request-duration histogram has at least one series (got %d)", len(groups))
	nonStream := histGroup{}
	for _, g := range groups {
		if g.count == 4 && g.labels["upstream"] == "m5-mock" {
			nonStream = g
		}
	}
	if c.assert(nonStream.count == 4,
		"3.9 one series counted all four requests, streamed and whole (got %v)", nonStream.count) {
		c.assert(len(nonStream.buckets) == 14, "3.10 the series exposes 13 finite buckets plus +Inf (got %d)", len(nonStream.buckets))
		if len(nonStream.buckets) > 0 {
			c.equal(nonStream.buckets[len(nonStream.buckets)-1].le, "+Inf", "3.11 the last bucket is le=+Inf")
			c.equal(nonStream.buckets[len(nonStream.buckets)-1].count, 4.0, "3.12 the +Inf bucket equals _count")
			previous := -1.0
			monotonic := true
			for _, b := range nonStream.buckets {
				if b.count < previous {
					monotonic = false
				}
				previous = b.count
			}
			c.assert(monotonic, "3.13 the exposed buckets are cumulative")
		}
		c.assert(nonStream.hasSum, "3.14 the series exposes a _sum")
	}

	attemptGroups := histogramGroups(samples, "infergate_upstream_attempt_duration_seconds")
	c.assert(len(attemptGroups) == 1, "3.15 the attempt-duration histogram has one series (got %d)", len(attemptGroups))
	if len(attemptGroups) == 1 {
		c.equal(attemptGroups[0].labels["upstream"], "m5-mock", "3.16 the attempt series is labelled by upstream")
		c.equal(attemptGroups[0].count, 4.0, "3.17 every request produced exactly one attempt")
	}

	firstGroups := histogramGroups(samples, "infergate_first_token_seconds")
	c.assert(len(firstGroups) == 1, "3.18 the TTFT histogram has one series after one stream (got %d)", len(firstGroups))
	if len(firstGroups) == 1 {
		c.equal(firstGroups[0].count, 1.0, "3.19 the TTFT histogram counted the one streamed request")
	}

	tokenGroups := histogramGroups(samples, "infergate_completion_tokens_per_request")
	c.assert(len(tokenGroups) == 1, "3.20 the completion-token histogram has one series (got %d)", len(tokenGroups))
	if len(tokenGroups) == 1 {
		c.equal(tokenGroups[0].count, 4.0, "3.21 every request reported its completion tokens")
	}

	frames, ok := findSample(samples, "infergate_stream_frames_total", map[string]string{"upstream": "m5-mock"})
	if c.assert(ok, "3.22 infergate_stream_frames_total has a series for the upstream") {
		c.assert(frames.value >= 4, "3.23 the four streamed frames were counted (got %v)", frames.value)
	}

	requests, ok := findSample(samples, "infergate_requests_total", map[string]string{"upstream": "m5-mock", "status": "200", "outcome": "success"})
	if c.assert(ok, "3.24 infergate_requests_total has a success series for the upstream") {
		c.equal(requests.value, 4.0, "3.25 all four requests are counted with status 200 and outcome success")
	}

	tokens, ok := findSample(samples, "infergate_tokens_total", map[string]string{"upstream": "m5-mock", "kind": "prompt"})
	if c.assert(ok, "3.26 infergate_tokens_total reports prompt tokens") {
		c.equal(tokens.value, 28.0, "3.27 4 requests x 7 prompt tokens are counted")
	}
}

// ---------------------------------------------------------------------------
// 4. /stats
// ---------------------------------------------------------------------------

func checkStatsEndpoint(c *checker) {
	section("4. /stats reports the window it used")

	up := newUpstream(upstreamJSON, "stats answer")
	defer up.Close()
	cfg := m5Config(up)
	st := newStack(c, "stats", &cfg)
	if st == nil {
		return
	}
	defer st.Close(c, "stats")

	const requests = 12
	for i := 0; i < requests; i++ {
		res := post(c, st.url, "/v1/chat/completions", chatBody("mock-gpt", "stats"), nil, "stats: request")
		if res.status != 200 {
			c.assert(false, "4.1 a request is served (got %d: %s)", res.status, truncate(res.body, 160))
			return
		}
	}

	var stats struct {
		Requests int64 `json:"requests"`
		Latency  struct {
			P50     string `json:"p50"`
			P90     string `json:"p90"`
			P95     string `json:"p95"`
			P99     string `json:"p99"`
			Max     string `json:"max"`
			Window  int    `json:"window"`
			Dropped int64  `json:"dropped"`
		} `json:"latency"`
	}
	if !getJSON(c, st.url, "/stats", "stats: /stats", &stats) {
		return
	}
	c.equal(stats.Requests, int64(requests), "4.2 /stats counts every request")
	c.equal(stats.Latency.Window, metrics.DefaultLatencyWindow, "4.3 /stats reports the percentile window it used")
	c.equal(stats.Latency.Dropped, int64(0), "4.4 a short run has dropped no samples")

	order := []struct {
		name  string
		value string
	}{
		{"p50", stats.Latency.P50},
		{"p90", stats.Latency.P90},
		{"p95", stats.Latency.P95},
		{"p99", stats.Latency.P99},
		{"max", stats.Latency.Max},
	}
	parsed := make([]time.Duration, 0, len(order))
	for _, o := range order {
		d, err := time.ParseDuration(o.value)
		if !c.assert(err == nil, "4.5 latency.%s parses as a duration (got %q: %v)", o.name, o.value, err) {
			return
		}
		parsed = append(parsed, d)
	}
	ascending := true
	for i := 1; i < len(parsed); i++ {
		if parsed[i] < parsed[i-1] {
			ascending = false
		}
	}
	c.assert(ascending, "4.6 p50 <= p90 <= p95 <= p99 <= max (got %v %v %v %v %v)",
		parsed[0], parsed[1], parsed[2], parsed[3], parsed[4])
	c.assert(parsed[4] > 0, "4.7 the maximum is above zero (got %v)", parsed[4])

	// /stats percentiles come from the raw window while /metrics quantiles come
	// from cumulative buckets, so the two are allowed to report different
	// numbers -- but they must agree on how many requests they saw. A request
	// that reached one surface and not the other is the defect this asserts on.
	res := get(c, st.url, "/metrics", "stats: /metrics")
	count := 0.0
	for _, s := range parseSamples(res.body) {
		if s.name == "infergate_request_duration_seconds_count" {
			count += s.value
		}
	}
	c.equal(count, float64(stats.Requests), "4.8 /metrics counts exactly the requests /stats reports")
}

// ---------------------------------------------------------------------------
// 5. /admin/tracing config surface
// ---------------------------------------------------------------------------

func checkTracingConfigSurface(c *checker) {
	section("5. /admin/tracing reports the tracing configuration")

	up := newUpstream(upstreamJSON, "tracing config")
	defer up.Close()
	cfg := m5Config(up)
	cfg.Tracing.Enabled = true
	cfg.Tracing.Capacity = 64
	cfg.Tracing.SampleRatio = 0.25
	cfg.Tracing.OTLP.ServiceName = "infergate-gate"
	st := newStack(c, "tracing-config", &cfg)
	if st == nil {
		return
	}
	defer st.Close(c, "tracing-config")

	var body struct {
		Enabled     bool     `json:"enabled"`
		Capacity    int      `json:"capacity"`
		SampleRatio float64  `json:"sample_ratio"`
		JSONLPath   string   `json:"jsonl_path"`
		Exporters   []string `json:"exporters"`
		Stored      int      `json:"stored"`
		Dropped     int64    `json:"dropped"`
		OTLP        struct {
			Endpoint    string `json:"endpoint"`
			Timeout     string `json:"timeout"`
			ServiceName string `json:"service_name"`
			Headers     int    `json:"headers"`
		} `json:"otlp"`
	}
	if !getJSON(c, st.url, "/admin/tracing", "tracing: /admin/tracing", &body) {
		return
	}
	c.equal(body.Enabled, true, "5.1 /admin/tracing reports tracing enabled")
	c.equal(body.Capacity, 64, "5.2 /admin/tracing reports the configured capacity")
	c.equal(body.SampleRatio, 0.25, "5.3 /admin/tracing reports the configured sample ratio")
	c.equal(body.JSONLPath, "", "5.4 /admin/tracing reports no JSONL path when none is configured")
	c.equal(len(body.Exporters), 0, "5.5 /admin/tracing lists no exporter when none is configured")
	c.equal(body.Stored, 0, "5.6 nothing is stored before the first request")
	c.equal(body.OTLP.ServiceName, "infergate-gate", "5.7 /admin/tracing reports the OTLP service name")
	c.equal(body.OTLP.Timeout, "5s", "5.8 an omitted OTLP timeout defaults to 5s")

	// SampleRatio is clamped rather than trusted, and the clamp is visible.
	out := m5Config(up)
	out.Tracing.Enabled = true
	out.Tracing.SampleRatio = 1.5
	if err := out.Validate(); err == nil {
		c.assert(false, "5.9 a sample ratio above 1 is rejected")
	} else {
		c.assert(strings.Contains(err.Error(), "tracing.sample_ratio"),
			"5.9 a sample ratio above 1 is rejected by name (%v)", err)
	}
	neg := m5Config(up)
	neg.Tracing.Capacity = -1
	if err := neg.Validate(); err == nil {
		c.assert(false, "5.10 a negative capacity is rejected")
	} else {
		c.assert(strings.Contains(err.Error(), "tracing.capacity"),
			"5.10 a negative capacity is rejected by name (%v)", err)
	}

	filled := m5Config(up)
	filled.Tracing.Enabled = true
	if err := filled.Validate(); err == nil {
		c.equal(filled.Tracing.Capacity, 1024, "5.11 an omitted capacity becomes the documented 1024")
		c.equal(filled.Tracing.SampleRatio, 1.0, "5.12 an omitted sample ratio becomes 1")
		c.equal(filled.Tracing.OTLP.Timeout.Duration(), 5*time.Second, "5.13 an omitted OTLP timeout becomes 5s")
		c.equal(filled.Tracing.OTLP.ServiceName, "infergate", "5.14 an omitted OTLP service name becomes infergate")
	} else {
		c.assert(false, "5.15 a tracing config with only enabled set validates (%v)", err)
	}
}
