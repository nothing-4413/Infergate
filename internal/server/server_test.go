package server

import (
	"encoding/json"
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

// TestNewServerValidatesTheConfigItIsGiven pins the first clause of NewServer's
// contract: an unusable configuration is refused at construction, so every
// caller in this repository may read a nil error as "this config is servable".
// The alternative -- accept whatever is passed and fail on the first request --
// turns a configuration mistake into a runtime mystery, and the tests that build
// a server in one line depend on that not happening.
func TestNewServerValidatesTheConfigItIsGiven(t *testing.T) {
	cases := map[string]func(*config.Config){
		"unnamed upstream": func(cfg *config.Config) {
			cfg.Upstreams = []config.UpstreamConfig{{Kind: "openai", BaseURL: "http://127.0.0.1:1"}}
		},
		"no backends at all": func(cfg *config.Config) { cfg.Upstreams = nil },
		"unusable base_url": func(cfg *config.Config) {
			cfg.Upstreams[0].BaseURL = "ftp://example.invalid"
		},
	}
	for label, breakIt := range cases {
		cfg := accessTestConfig()
		breakIt(&cfg)
		srv, err := NewServer(&cfg, testLogger{})
		if err == nil {
			t.Errorf("%s: NewServer accepted it and returned a server", label)
			continue
		}
		// The stage must be visible: a bare "invalid config" would leave the
		// caller guessing which part of a large file to fix.
		if !strings.Contains(err.Error(), "build upstream registry") {
			t.Errorf("%s: error does not name the stage that rejected it: %v", label, err)
		}
		if srv != nil {
			t.Errorf("%s: NewServer returned a server alongside an error", label)
		}
	}
}

// TestNewServerFailsWhenQuotaRedisIsDown pins the store half of the quota
// section: with `store: redis`, a store that cannot be reached has to stop
// construction instead of letting the process run with budgets it cannot read.
//
// The alternative is worse than a failed start. Coming up anyway would, with the
// default fail-closed setting, refuse every request, and with fail-open on it
// would admit every request -- either way the operator finds out from traffic
// rather than from the boot log. The store-level half of the same claim is
// TestNewRedisStoreRejectsABadAddress.
func TestNewServerFailsWhenQuotaRedisIsDown(t *testing.T) {
	cfg := accessTestConfig()
	cfg.Quota.Enabled = true
	cfg.Quota.Store = config.QuotaStoreRedis
	cfg.Quota.Redis.Addr = "127.0.0.1:1" // nothing listens here

	srv, err := NewServer(&cfg, testLogger{})
	if err == nil {
		t.Fatal("NewServer came up with an unreachable quota store; a budget that cannot be read is not a budget")
	}
	// The stage has to be visible, for the same reason the validation errors
	// above have to name theirs.
	if !strings.Contains(err.Error(), "server: quota: redis") {
		t.Errorf("error does not name the stage that failed: %v", err)
	}
	if srv != nil {
		t.Error("NewServer returned a server alongside an error")
	}
}

// TestNewServerNormalisesTheConfigItIsGiven pins a behaviour that is easy to
// mistake for a bug: NewServer hands the config to cfg.Validate, and Validate is
// a normaliser as well as a checker -- it fills unset defaults in place, on the
// pointer it was handed, so the caller's struct comes back changed.
//
// That is deliberate (see the tier comment in config.Validate: the empty tier
// has to become something, and "cloud" is the direction that costs money, so it
// must not be reached by accident), and it is invisible in production because
// Load validates first: by the time anyone constructs a server the work is done
// and a second pass is a no-op. What is pinned here is the shape, because
// "construction edited my data" is precisely the kind of surprise that should be
// asserted rather than stumbled over:
//
//   - unset fields are filled with the shipped defaults;
//   - explicitly set fields are left alone (a normaliser that overwrites is a
//     different and much worse thing);
//   - explicitly set values ARE normalised (`base_url` loses its trailing
//     slash), which is the one case where a value the caller wrote is changed --
//     and it is the case that silently produces "//v1/chat/completions" if it is
//     ever dropped;
//   - the result is a fixed point, so a second construction changes nothing.
func TestNewServerNormalisesTheConfigItIsGiven(t *testing.T) {
	cfg := accessTestConfig()
	if cfg.Upstreams[0].Tier != "" || cfg.Upstreams[0].Weight != 0 {
		t.Fatalf("the fixture should start unset, got tier=%q weight=%v",
			cfg.Upstreams[0].Tier, cfg.Upstreams[0].Weight)
	}

	if _, err := NewServer(&cfg, testLogger{}); err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if got := cfg.Upstreams[0].Tier; got != config.TierCloud {
		t.Errorf("unset tier: got %q, want %q", got, config.TierCloud)
	}
	if got := cfg.Upstreams[0].Weight; got != 1 {
		t.Errorf("unset weight: got %v, want 1", got)
	}
	if got, want := cfg.Quota.DefaultPolicy.AnomalyRatio, cfg.Quota.AnomalyRatio; got != want {
		t.Errorf("unset quota.default_policy.anomaly_ratio: got %v, want the section value %v", got, want)
	}

	// A fixed point: the defaults that were just written are not written again,
	// and nothing accumulates across constructions.
	first, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := NewServer(&cfg, testLogger{}); err != nil {
		t.Fatalf("second NewServer: %v", err)
	}
	second, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("normalisation is not a fixed point:\nfirst:  %s\nsecond: %s", first, second)
	}

	// Everything the caller set explicitly survives, except the documented
	// trailing-slash trim.
	explicit := accessTestConfig()
	explicit.Upstreams[0].Tier = config.TierLocal
	explicit.Upstreams[0].Weight = 2.5
	explicit.Upstreams[0].BaseURL = "http://127.0.0.1:1/"
	explicit.Quota.AnomalyRatio = 5
	explicit.Quota.DefaultPolicy.AnomalyRatio = 7

	if _, err := NewServer(&explicit, testLogger{}); err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if got := explicit.Upstreams[0].Tier; got != config.TierLocal {
		t.Errorf("explicit tier was overwritten: got %q", got)
	}
	if got := explicit.Upstreams[0].Weight; got != 2.5 {
		t.Errorf("explicit weight was overwritten: got %v", got)
	}
	if got := explicit.Quota.AnomalyRatio; got != 5 {
		t.Errorf("explicit quota.anomaly_ratio was overwritten: got %v", got)
	}
	if got := explicit.Quota.DefaultPolicy.AnomalyRatio; got != 7 {
		t.Errorf("explicit quota.default_policy.anomaly_ratio was overwritten: got %v", got)
	}
	if got := explicit.Upstreams[0].BaseURL; got != "http://127.0.0.1:1" {
		t.Errorf("base_url trailing slash not trimmed: got %q", got)
	}
}

// loggedLine is one call the server made into its logger, kept whole so a test
// can assert on the level, the message and the key/value arguments.
type loggedLine struct {
	level string
	msg   string
	args  []any
}

// recordingLogger captures the server's startup lines. The server logs through
// an interface precisely so a test can substitute one, and this is the test that
// needs to read what an operator would read.
type recordingLogger struct{ lines []loggedLine }

func (l *recordingLogger) Info(msg string, args ...any)  { l.add("info", msg, args) }
func (l *recordingLogger) Warn(msg string, args ...any)  { l.add("warn", msg, args) }
func (l *recordingLogger) Error(msg string, args ...any) { l.add("error", msg, args) }

func (l *recordingLogger) add(level, msg string, args []any) {
	l.lines = append(l.lines, loggedLine{level: level, msg: msg, args: args})
}

// pricingLines returns the startup lines about prices, and nothing else: the
// server is free to log about anything it likes as long as this one report says
// what it claims to.
func (l *recordingLogger) pricingLines() []loggedLine {
	var out []loggedLine
	for _, line := range l.lines {
		if strings.HasPrefix(line.msg, "pricing:") {
			out = append(out, line)
		}
	}
	return out
}

// arg returns the value logged under key, so an assertion can name the field it
// depends on instead of the whole argument list.
func (l loggedLine) arg(key string) (any, bool) {
	for i := 0; i+1 < len(l.args); i += 2 {
		if k, ok := l.args[i].(string); ok && k == key {
			return l.args[i+1], true
		}
	}
	return nil, false
}

// TestNewServerReportsModelsItCannotPrice pins the one place the process admits
// that an unlisted model's cost is a guess.
//
// The distinction it keeps is between a guess and a zero: with a nonzero
// pricing.default the recorded cost is at least the right order of magnitude and
// an info line is enough, but with the default left at 0 the request is summed
// as costing nothing, so no cost budget can ever trip on it. That is the
// direction this repository treats as dangerous, hence a warning that names the
// models and the two knobs.
func TestNewServerReportsModelsItCannotPrice(t *testing.T) {
	upstream := func(models ...string) []config.UpstreamConfig {
		return []config.UpstreamConfig{{
			Name:    "local",
			Kind:    "openai",
			BaseURL: "http://127.0.0.1:1",
			Models:  models,
		}}
	}
	price := func(in, out float64) map[string]config.ModelPrice {
		return map[string]config.ModelPrice{"gpt-4o": {In: in, Out: out}}
	}

	cases := []struct {
		name       string
		upstreams  []config.UpstreamConfig
		pricing    config.PricingConfig
		wantLevel  string // "" means the report must stay silent
		wantModels string
		wantCatch  bool
	}{
		{
			name:       "an unpriced model with a zero default warns",
			upstreams:  upstream("gpt-4o", "bge-m3"),
			pricing:    config.PricingConfig{Models: price(2.5, 10)},
			wantLevel:  "warn",
			wantModels: "bge-m3",
		},
		{
			name:      "every declared model priced says nothing at all",
			upstreams: upstream("gpt-4o"),
			pricing:   config.PricingConfig{Models: price(2.5, 10)},
		},
		{
			name:       "a nonzero default is a guess, so it is info rather than a warning",
			upstreams:  upstream("gpt-4o", "bge-m3"),
			pricing:    config.PricingConfig{Default: config.ModelPrice{In: 1, Out: 3}, Models: price(2.5, 10)},
			wantLevel:  "info",
			wantModels: "bge-m3",
		},
		{
			// The catch-all accepts model names this configuration never
			// mentions, so the report cannot name them; it still has to say that
			// unpriced traffic can arrive.
			name:       "a catch-all alone is still reported",
			upstreams:  upstream("/"),
			wantLevel:  "warn",
			wantModels: "catch-all",
			wantCatch:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := accessTestConfig()
			cfg.Upstreams = tc.upstreams
			cfg.Pricing = tc.pricing
			logger := &recordingLogger{}
			if _, err := NewServer(&cfg, logger); err != nil {
				t.Fatalf("NewServer: %v", err)
			}

			lines := logger.pricingLines()
			if tc.wantLevel == "" {
				if len(lines) != 0 {
					t.Fatalf("a fully priced configuration still reported %d line(s): %+v", len(lines), lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("got %d pricing line(s), want exactly 1: %+v", len(lines), lines)
			}
			line := lines[0]
			if line.level != tc.wantLevel {
				t.Errorf("level = %q, want %q (message: %s)", line.level, tc.wantLevel, line.msg)
			}
			models, ok := line.arg("models")
			if !ok {
				t.Fatalf("the report does not name the models: %+v", line.args)
			}
			if got, want := models.(string), tc.wantModels; !strings.Contains(got, want) {
				t.Errorf("models = %q, want it to contain %q", got, want)
			}
			// The report is only actionable if it says what the fallback price
			// is, because that value is what turns a guess into a silent zero.
			if _, ok := line.arg("default_in_usd_per_mtok"); !ok {
				t.Errorf("the report does not state the default input price: %+v", line.args)
			}
			if got, ok := line.arg("catch_all"); !ok || got.(bool) != tc.wantCatch {
				t.Errorf("catch_all = %v (present=%v), want %v", got, ok, tc.wantCatch)
			}
			if tc.wantLevel == "warn" && !strings.Contains(line.msg, "cost_usd") {
				t.Errorf("the warning does not say what the cost is recorded as: %s", line.msg)
			}
		})
	}
}
