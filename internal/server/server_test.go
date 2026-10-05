package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/metrics"
)

// TestWriteRuntimeMetricsFamilies pins the process-metric block: every family
// must be present with its HELP and TYPE lines, and the values must be real
// readings rather than placeholders. The goroutine gauge is the one that is
// always non-zero in a running test binary, which makes it the cheapest proof
// that the block is actually calling the runtime.
func TestWriteRuntimeMetricsFamilies(t *testing.T) {
	var b strings.Builder
	writeRuntimeMetrics(&b)
	out := b.String()

	// name -> expected TYPE.
	families := map[string]string{
		"infergate_runtime_goroutines":                 "gauge",
		"infergate_runtime_num_cpu":                    "gauge",
		"infergate_runtime_memstats_alloc_bytes":       "gauge",
		"infergate_runtime_memstats_heap_alloc_bytes":  "gauge",
		"infergate_runtime_memstats_heap_inuse_bytes":  "gauge",
		"infergate_runtime_memstats_heap_objects":      "gauge",
		"infergate_runtime_memstats_stack_inuse_bytes": "gauge",
		"infergate_runtime_memstats_sys_bytes":         "gauge",
		"infergate_runtime_memstats_total_alloc_bytes": "counter",
		"infergate_runtime_memstats_gc_cycles_total":   "counter",
		"infergate_runtime_gc_pause_seconds_total":     "counter",
		"infergate_runtime_gc_last_pause_seconds":      "gauge",
		"infergate_runtime_go_version":                 "gauge",
	}
	for name, typ := range families {
		if !strings.Contains(out, "# HELP "+name+" ") {
			t.Errorf("missing HELP line for %s", name)
		}
		if want := "# TYPE " + name + " " + typ + "\n"; !strings.Contains(out, want) {
			t.Errorf("missing or wrong TYPE line for %s, want %q", name, want)
		}
	}

	// The unlabelled families must be a bare series, and go_version must carry
	// exactly the one label the parent asked for.
	if !strings.Contains(out, "infergate_runtime_goroutines ") {
		t.Error("infergate_runtime_goroutines has no sample line")
	}
	if strings.Contains(out, "infergate_runtime_goroutines{") {
		t.Error("infergate_runtime_goroutines must be unlabelled")
	}
	if !strings.Contains(out, `infergate_runtime_go_version{version="go`) {
		t.Errorf("infergate_runtime_go_version is missing its version label; output:\n%s", out)
	}

	// A running test binary always has at least one goroutine, so a zero here
	// means the gauge is wired to the wrong thing.
	if strings.Contains(out, "infergate_runtime_goroutines 0\n") {
		t.Error("infergate_runtime_goroutines reported 0 in a live process")
	}
}

// TestWriteHistogramFamilyShape checks the wire format of one histogram family:
// a bucket per boundary in ascending order ending in +Inf, then _count and
// _sum, with the label appended to every series and le added only to buckets.
func TestWriteHistogramFamilyShape(t *testing.T) {
	// A real histogram rather than hand-built counts, so this also covers the
	// cumulative fold that produces what is rendered here.
	h := metrics.NewHistogram([]float64{0.1, 1})
	h.Observe(0.05)
	h.Observe(0.5)
	h.Observe(2)
	snap := h.Snapshot()

	var b strings.Builder
	writeHistogramFamily(&b, "infergate_test_seconds", "Test help.",
		[]string{"upstream", "model"},
		[]histogramSeries{{
			labels:  []string{"local", "llama"},
			buckets: snap.Buckets(),
			count:   snap.Count,
			sum:     snap.Sum,
		}})
	out := b.String()

	want := []string{
		"# HELP infergate_test_seconds Test help.\n",
		"# TYPE infergate_test_seconds histogram\n",
		`infergate_test_seconds_bucket{upstream="local",model="llama",le="0.1"} 1` + "\n",
		`infergate_test_seconds_bucket{upstream="local",model="llama",le="1"} 2` + "\n",
		`infergate_test_seconds_bucket{upstream="local",model="llama",le="+Inf"} 3` + "\n",
		`infergate_test_seconds_count{upstream="local",model="llama"} 3` + "\n",
		`infergate_test_seconds_sum{upstream="local",model="llama"} 2.55` + "\n",
	}
	for _, line := range want {
		if !strings.Contains(out, line) {
			t.Errorf("missing line %q in output:\n%s", line, out)
		}
	}

	// An empty family still has to declare itself: a scraper learns the metric
	// exists from HELP/TYPE, and a bare bucket line with no labels would be an
	// invalid series.
	var empty strings.Builder
	writeHistogramFamily(&empty, "infergate_empty_seconds", "Empty help.", []string{"upstream"}, nil)
	if got := empty.String(); got != "# HELP infergate_empty_seconds Empty help.\n# TYPE infergate_empty_seconds histogram\n" {
		t.Errorf("empty family rendered as %q", got)
	}
}

// TestLabelSuffix covers the unlabelled case, which is what every process
// metric uses.
func TestLabelSuffix(t *testing.T) {
	if got := labelSuffix(nil); got != "" {
		t.Errorf("labelSuffix(nil) = %q, want empty", got)
	}
	if got := labelSuffix([]string{`upstream="local"`}); got != `{upstream="local"}` {
		t.Errorf("labelSuffix = %q", got)
	}
}

// TestPercentileSortedNearestRank locks the values /stats reported before this
// change: the same nearest-rank definition, now read off an already-sorted
// slice. A change here would silently move every published percentile.
func TestPercentileSortedNearestRank(t *testing.T) {
	sorted := make([]time.Duration, 100)
	for i := range sorted {
		sorted[i] = time.Duration(i+1) * time.Millisecond
	}

	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0.50, 51 * time.Millisecond},
		{0.90, 91 * time.Millisecond},
		{0.95, 96 * time.Millisecond},
		{0.99, 100 * time.Millisecond},
		{1.0, 100 * time.Millisecond},
		{0, 1 * time.Millisecond},
	}
	for _, c := range cases {
		if got := percentileSorted(sorted, c.p); got != c.want {
			t.Errorf("percentileSorted(p=%v) = %v, want %v", c.p, got, c.want)
		}
	}

	if got := percentileSorted(nil, 0.5); got != 0 {
		t.Errorf("percentileSorted(empty) = %v, want 0", got)
	}
	if got := percentileSorted([]time.Duration{7 * time.Millisecond}, 0.99); got != 7*time.Millisecond {
		t.Errorf("single-sample percentile = %v, want 7ms", got)
	}
}

// BenchmarkPercentileSorted is the real hot path behind /stats: five of these
// per scrape over the whole latency window.
func BenchmarkPercentileSorted(b *testing.B) {
	sorted := make([]time.Duration, 65536)
	for i := range sorted {
		sorted[i] = time.Duration(i) * time.Microsecond
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, p := range []float64{0.50, 0.90, 0.95, 0.99, 1.0} {
			_ = percentileSorted(sorted, p)
		}
	}
}

// testLogger swallows the server's log lines so a test can construct one.
type testLogger struct{}

func (testLogger) Info(string, ...any)  {}
func (testLogger) Error(string, ...any) {}
func (testLogger) Warn(string, ...any)  {}

// promSample is one non-comment line of an exposition.
type promSample struct {
	name   string
	labels string
	value  string
}

// TestMetricsExpositionIsValid scrapes the real handler and checks the
// properties Prometheus requires of the text format. The interesting one is the
// duplicate-family check: the endpoint used to export
// infergate_request_duration_seconds_sum as its own counter *and* now exports it
// as part of the histogram, and shipping both would make the whole scrape
// invalid rather than merely wrong.
func TestMetricsExpositionIsValid(t *testing.T) {
	cfg := config.Defaults()
	// A registry needs at least one backend; the address is never dialled
	// because the test only renders the endpoint.
	cfg.Upstreams = []config.UpstreamConfig{{
		Name:    "local",
		Kind:    "openai",
		BaseURL: "http://127.0.0.1:1",
		Models:  []string{"/"},
	}}
	srv, err := NewServer(&cfg, testLogger{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Populate every family: two series of request duration so there is
	// something to group by, plus an attempt, a first token and tokens.
	srv.recorder.ObserveRequest("/v1/chat/completions", "local", "llama", 200, metrics.OutcomeSuccess, 30*time.Millisecond)
	srv.recorder.ObserveRequest("/v1/chat/completions", "local", "llama", 200, metrics.OutcomeSuccess, 4*time.Second)
	srv.recorder.ObserveRequest("/v1/embeddings", "cloud", "small", 200, metrics.OutcomeSuccess, 7*time.Millisecond)
	srv.recorder.ObserveUpstreamAttempt("local", 200, metrics.OutcomeSuccess, 25*time.Millisecond)
	srv.recorder.ObserveFirstToken("local", "llama", 80*time.Millisecond)
	srv.recorder.ObserveTokens("local", "llama", 120, 400, 0)

	rec := httptest.NewRecorder()
	srv.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("exposition does not end with a newline")
	}

	help := map[string]bool{}
	types := map[string]string{}
	var samples []promSample
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP "):
			rest := strings.TrimPrefix(line, "# HELP ")
			name, _, ok := strings.Cut(rest, " ")
			if !ok || name == "" {
				t.Errorf("malformed HELP line: %q", line)
				continue
			}
			help[name] = true
		case strings.HasPrefix(line, "# TYPE "):
			rest := strings.TrimPrefix(line, "# TYPE ")
			name, typ, ok := strings.Cut(rest, " ")
			if !ok || name == "" || typ == "" {
				t.Errorf("malformed TYPE line: %q", line)
				continue
			}
			if prev, dup := types[name]; dup {
				t.Errorf("family %s declared twice: %q then %q", name, prev, typ)
			}
			types[name] = typ
		case strings.HasPrefix(line, "#"):
			t.Errorf("unexpected comment line: %q", line)
		default:
			name, labels, value, ok := splitSample(line)
			if !ok {
				t.Errorf("malformed sample line: %q", line)
				continue
			}
			if labels == "{}" {
				t.Errorf("series %s has an empty label set", name)
			}
			samples = append(samples, promSample{name: name, labels: labels, value: value})
		}
	}

	// Every declared family needs HELP, and no family may collide with a
	// histogram's own _sum/_count series.
	for name := range types {
		if !help[name] {
			t.Errorf("family %s has no HELP line", name)
		}
	}
	for name, typ := range types {
		if typ != "histogram" {
			continue
		}
		for _, suffix := range []string{"_sum", "_count"} {
			if other, clash := types[name+suffix]; clash {
				t.Errorf("histogram %s also declares %s%s as a %s family: duplicate series",
					name, name, suffix, other)
			}
		}
	}
	if typ, ok := types["infergate_request_duration_seconds"]; !ok || typ != "histogram" {
		t.Errorf("infergate_request_duration_seconds is %q, want a histogram", typ)
	}

	// Per label set, a histogram's buckets must be cumulative, end at +Inf, and
	// agree with its _count.
	type bucketSeries struct{ bounds, counts []string }
	grouped := map[string]*bucketSeries{}
	var familyOrder []string
	for _, s := range samples {
		family, suffix := histogramFamilyOf(s.name, types)
		if family == "" {
			continue
		}
		base := s.name[:len(s.name)-len(suffix)]
		key := base + "|" + stripLe(s.labels)
		switch suffix {
		case "_bucket":
			g := grouped[key]
			if g == nil {
				g = &bucketSeries{}
				grouped[key] = g
				familyOrder = append(familyOrder, key)
			}
			le := leValue(s.labels)
			if le == "" {
				t.Errorf("bucket series %s has no le label", s.name)
			}
			g.bounds = append(g.bounds, le)
			g.counts = append(g.counts, s.value)
		}
	}

	counts := map[string]string{}
	for _, s := range samples {
		if _, suffix := histogramFamilyOf(s.name, types); suffix == "_count" {
			base := s.name[:len(s.name)-len("_count")]
			counts[base+"|"+stripLe(s.labels)] = s.value
		}
	}

	for _, key := range familyOrder {
		g := grouped[key]
		if len(g.bounds) == 0 {
			continue
		}
		if last := g.bounds[len(g.bounds)-1]; last != "+Inf" {
			t.Errorf("%s: last bucket is le=%q, want +Inf", key, last)
		}
		if g.bounds[0] != "+Inf" {
			for i := 1; i < len(g.bounds); i++ {
				if g.bounds[i] == "+Inf" {
					continue
				}
				if atof(t, g.counts[i]) < atof(t, g.counts[i-1]) {
					t.Errorf("%s: bucket le=%s (%s) is below le=%s (%s); buckets must be cumulative",
						key, g.bounds[i], g.counts[i], g.bounds[i-1], g.counts[i-1])
				}
			}
		}
		if want, ok := counts[key]; ok {
			got := g.counts[len(g.counts)-1]
			if got != want {
				t.Errorf("%s: +Inf bucket = %s but _count = %s; the histogram does not partition its observations",
					key, got, want)
			}
		} else {
			t.Errorf("%s: buckets without a _count series", key)
		}
	}

	// The families added by this change, and the popped-in runtime block, must
	// all be present in a real scrape.
	for _, name := range []string{
		"infergate_request_duration_seconds",
		"infergate_upstream_attempt_duration_seconds",
		"infergate_first_token_seconds",
		"infergate_first_token_seconds_mean",
		"infergate_completion_tokens_per_request",
		"infergate_runtime_goroutines",
	} {
		if _, ok := types[name]; !ok {
			t.Errorf("family %s is missing from the scrape", name)
		}
	}
	if !strings.Contains(out, `infergate_completion_tokens_per_request_bucket{upstream="local",model="llama",le="512"} 1`) {
		t.Errorf("400 completion tokens did not land in the (256,512] bucket; scrape:\n%s", out)
	}

	// scripts/verify-m0.ps1 measures a request's duration by scraping this
	// endpoint with the regex `(?m)^infergate_request_duration_seconds_sum\{[^}]*\}\s+([0-9.eE+-]+)\s*$`.
	// Replacing the standalone `_sum` counter with a histogram is only safe
	// because the histogram's own `_sum` series still matches it; this asserts
	// that contract from inside the repo that owns the endpoint.
	m0Sum := regexp.MustCompile(`(?m)^infergate_request_duration_seconds_sum\{[^}]*\}\s+([0-9.eE+-]+)\s*$`)
	if got := len(m0Sum.FindAllStringSubmatch(out, -1)); got != 2 {
		t.Errorf("scripts/verify-m0.ps1 would find %d request-duration sum series, want 2", got)
	}
}

// histogramFamilyOf resolves a sample name to the histogram family that owns it
// and the suffix that identified it, or ("","") for a non-histogram series.
func histogramFamilyOf(name string, types map[string]string) (string, string) {
	for _, suffix := range []string{"_bucket", "_count", "_sum"} {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		if base := strings.TrimSuffix(name, suffix); types[base] == "histogram" {
			return base, suffix
		}
	}
	return "", ""
}

// stripLe removes the le label so samples differing only by boundary group
// together.
func stripLe(labels string) string {
	if labels == "" {
		return ""
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(labels, "{"), "}")
	parts := strings.Split(inner, ",")
	kept := parts[:0]
	for _, p := range parts {
		if strings.HasPrefix(p, "le=") {
			continue
		}
		kept = append(kept, p)
	}
	return "{" + strings.Join(kept, ",") + "}"
}

func leValue(labels string) string {
	for _, p := range strings.Split(strings.Trim(labels, "{}"), ",") {
		if v, ok := strings.CutPrefix(p, "le="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// splitSample parses `name{labels} value` or `name value`.
func splitSample(line string) (name, labels, value string, ok bool) {
	series, value, found := strings.Cut(line, " ")
	if !found {
		return "", "", "", false
	}
	if i := strings.Index(series, "{"); i >= 0 {
		if !strings.HasSuffix(series, "}") {
			return "", "", "", false
		}
		return series[:i], series[i:], value, true
	}
	return series, "", value, true
}

func atof(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("value %q is not a number: %v", s, err)
	}
	return v
}
