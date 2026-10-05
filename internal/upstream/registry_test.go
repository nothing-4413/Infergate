package upstream

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/infergate/infergate/internal/config"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// testConfig is Defaults() plus the given backends. Starting from Defaults
// rather than a hand-built struct means a new required field somewhere in the
// config does not silently turn every test here into "New() failed", which is
// what happens when a test config is written out by hand.
func testConfig(t *testing.T, ups ...config.UpstreamConfig) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Upstreams = ups
	return &cfg
}

// newRegistry builds a registry or fails the test, so no test has to spell out
// the three-line error dance.
func newRegistry(t *testing.T, ups ...config.UpstreamConfig) *Registry {
	t.Helper()
	r, err := New(testConfig(t, ups...))
	if err != nil {
		t.Fatalf("New() = %v, want a registry", err)
	}
	return r
}

// mustResolve resolves or fails, returning the name rather than the *Target
// because most assertions are about WHICH backend answered.
func mustResolve(t *testing.T, r *Registry, model, explicit string) string {
	t.Helper()
	target, err := r.Resolve(model, explicit)
	if err != nil {
		t.Fatalf("Resolve(%q, %q) = %v, want a target", model, explicit, err)
	}
	return target.Name
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// construction
// ---------------------------------------------------------------------------

// TestNewTrimsTrailingSlashOnBaseURL: the proxy builds outbound URLs as
// BaseURL + r.URL.Path, so a configured trailing slash yields
// "//v1/chat/completions" and every request 404s at the provider. Normalising
// once at construction is the fix; this test is what keeps someone from
// "simplifying" it away.
func TestNewTrimsTrailingSlashOnBaseURL(t *testing.T) {
	for _, configured := range []string{
		"http://127.0.0.1:9000/",
		"http://127.0.0.1:9000///",
		"  http://127.0.0.1:9000/  ",
	} {
		r := newRegistry(t, config.UpstreamConfig{
			Name: "mock", BaseURL: configured, Models: []string{"/"},
		})
		target, ok := r.Target("mock")
		if !ok {
			t.Fatalf("Target(mock) missing after configuring base_url %q", configured)
		}
		if target.BaseURL != "http://127.0.0.1:9000" {
			t.Errorf("base_url %q became %q, want the trailing slash trimmed", configured, target.BaseURL)
		}
		if strings.HasSuffix(target.BaseURL, "/") {
			t.Errorf("base_url %q still ends in a slash: the proxy would build %q",
				target.BaseURL, target.BaseURL+"/v1/chat/completions")
		}
	}
}

// TestNewRejectsInvalidConfig: New delegates to cfg.Validate rather than
// trusting its caller, which is why an invalid config fails at construction
// instead of at the first request.
func TestNewRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		ups  []config.UpstreamConfig
		want string
	}{
		{
			name: "no upstreams",
			ups:  nil,
			want: "at least one backend",
		},
		{
			name: "nameless",
			ups:  []config.UpstreamConfig{{BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}},
			want: "name is required",
		},
		{
			name: "duplicate name",
			ups: []config.UpstreamConfig{
				{Name: "a", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}},
				{Name: "a", BaseURL: "http://127.0.0.1:9001", Models: []string{"/"}},
			},
			want: "duplicate name",
		},
		{
			// url.Parse rejects a bare "host:port" as a path with a colon in
			// its first segment, so this never reaches the scheme check. What
			// matters is that it is rejected at all rather than reaching
			// http.NewRequest later.
			name: "scheme missing",
			ups:  []config.UpstreamConfig{{Name: "a", BaseURL: "127.0.0.1:9000", Models: []string{"/"}}},
			want: "base_url invalid",
		},
		{
			name: "wrong scheme",
			ups:  []config.UpstreamConfig{{Name: "a", BaseURL: "ftp://127.0.0.1:9000", Models: []string{"/"}}},
			want: "http or https",
		},
		{
			name: "no host",
			ups:  []config.UpstreamConfig{{Name: "a", BaseURL: "http://", Models: []string{"/"}}},
			want: "no host",
		},
		{
			name: "no models and no catch-all",
			ups:  []config.UpstreamConfig{{Name: "a", BaseURL: "http://127.0.0.1:9000"}},
			want: "at least one model",
		},
		{
			name: "unknown kind",
			ups:  []config.UpstreamConfig{{Name: "a", Kind: "anthropic", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}}},
			want: "unsupported kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(testConfig(t, tc.ups...)); err == nil {
				t.Fatalf("New() accepted an invalid config, want an error mentioning %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestNewGivesEveryTargetItsOwnTransport: transports are per backend, not
// shared, because pool sizing and keep-alive policy are per backend. Sharing
// one would make max_idle_conns_per_host of whichever backend was configured
// first silently govern the others.
func TestNewGivesEveryTargetItsOwnTransport(t *testing.T) {
	r := newRegistry(t,
		config.UpstreamConfig{Name: "a", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}},
		config.UpstreamConfig{Name: "b", BaseURL: "http://127.0.0.1:9001", Models: []string{"/"}},
	)
	targets := r.Targets()
	if len(targets) != 2 {
		t.Fatalf("len(Targets()) = %d, want 2", len(targets))
	}
	if targets[0].Transport == nil || targets[1].Transport == nil {
		t.Fatal("a target has no transport: the proxy would have to build one per request")
	}
	if targets[0].Transport == targets[1].Transport {
		t.Error("both targets share one transport, want one per backend")
	}
}

// TestTransportIsTunedForStreamingLLMTraffic pins the two settings that turn
// into time-to-first-token under concurrency. Neither is the kind of thing a
// failing test would otherwise notice, which is exactly why it is written down.
func TestTransportIsTunedForStreamingLLMTraffic(t *testing.T) {
	cfg := testConfig(t, config.UpstreamConfig{
		Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"},
	})
	cfg.Server.MaxIdleConnsPerHost = 256
	cfg.Server.DisableKeepAlives = true

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	target, _ := r.Target("mock")
	tr, ok := target.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", target.Transport)
	}
	if tr.MaxIdleConnsPerHost != 256 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 256 from server.max_idle_conns_per_host", tr.MaxIdleConnsPerHost)
	}
	if !tr.DisableKeepAlives {
		t.Error("DisableKeepAlives = false, want true from server.disable_keep_alives")
	}
	if tr.MaxConnsPerHost != 0 {
		t.Errorf("MaxConnsPerHost = %d, want 0 (unbounded in flight, bounded by the idle pool)", tr.MaxConnsPerHost)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %v, want 0: a long generation may legitimately take minutes to start, and the deadline belongs to the handler's context", tr.ResponseHeaderTimeout)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConns = %d is below MaxIdleConnsPerHost = %d, so the per-host pool could never fill",
			tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
}

// TestTransportReusesConnections is the behavioural half of the pool sizing
// above: against a real listener, two sequential calls must arrive on one
// connection. Building a transport per request would break exactly this, and
// nothing else in the suite would fail.
func TestTransportReusesConnections(t *testing.T) {
	var opened int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&opened, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	r := newRegistry(t, config.UpstreamConfig{
		Name: "mock", BaseURL: srv.URL, Models: []string{"/"},
	})
	defer r.CloseIdleConnections()
	target, _ := r.Target("mock")
	client := &http.Client{Transport: target.Transport}

	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodPost,
			target.BaseURL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}

	if got := atomic.LoadInt32(&opened); got != 1 {
		t.Errorf("server saw %d connections for 3 sequential requests, want 1: the pool is not being reused", got)
	}
}

// ---------------------------------------------------------------------------
// model resolution
// ---------------------------------------------------------------------------

// TestResolutionOrder pins Resolve's four rules in one place, because each is
// only defensible relative to the others.
func TestResolutionOrder(t *testing.T) {
	r := newRegistry(t,
		config.UpstreamConfig{
			Name: "explicit", BaseURL: "http://127.0.0.1:9001",
			Models: []string{"gpt-4o"}, Capabilities: []string{"tools"},
		},
		config.UpstreamConfig{
			Name: "catchall", BaseURL: "http://127.0.0.1:9002", Models: []string{"/"},
		},
	)

	cases := []struct {
		name     string
		model    string
		explicit string
		want     string
	}{
		{
			name:  "exact model beats catch-all",
			model: "gpt-4o",
			want:  "explicit",
		},
		{
			name:  "unknown model falls to catch-all",
			model: "something-else",
			want:  "catchall",
		},
		{
			name:  "empty model falls to catch-all",
			model: "",
			want:  "catchall",
		},
		{
			name:  "model match is case-insensitive and trimmed",
			model: "  GPT-4O ",
			want:  "explicit",
		},
		{
			name:     "explicit pin beats the model index",
			model:    "gpt-4o",
			explicit: "catchall",
			want:     "catchall",
		},
		{
			name:     "explicit pin is exact, not case-insensitive",
			model:    "gpt-4o",
			explicit: "explicit",
			want:     "explicit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustResolve(t, r, tc.model, tc.explicit); got != tc.want {
				t.Errorf("Resolve(%q, %q) = %q, want %q", tc.model, tc.explicit, got, tc.want)
			}
		})
	}
}

// TestExplicitUnknownUpstreamIsAnError: a pin is an operator instruction. If
// the named backend does not exist, falling back to normal routing would send
// the request somewhere the caller explicitly did not choose, and the caller
// would never know.
func TestExplicitUnknownUpstreamIsAnError(t *testing.T) {
	r := newRegistry(t, config.UpstreamConfig{
		Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"},
	})
	target, err := r.Resolve("any-model", "typo")
	if err == nil {
		t.Fatalf("Resolve with an unknown pin = %q, want an error", target.Name)
	}
	if !errors.Is(err, ErrNoTarget) {
		t.Errorf("error = %v, want it to wrap ErrNoTarget", err)
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("error = %q, want it to name the unknown upstream", err)
	}
}

// TestSingleBackendAbsorbsAnyModel is the rule-4 ergonomic case: a local vLLM
// or Ollama box is told a model alias the gateway has never heard of, and
// rejecting it would make the gateway useless for the most common local
// deployment.
func TestSingleBackendAbsorbsAnyModel(t *testing.T) {
	r := newRegistry(t, config.UpstreamConfig{
		Name: "local", BaseURL: "http://127.0.0.1:8000", Models: []string{"qwen2.5-7b-awq"},
	})
	if got := mustResolve(t, r, "some-agent-alias", ""); got != "local" {
		t.Errorf("Resolve(unknown) = %q, want local: the only backend absorbs it", got)
	}
	if got := mustResolve(t, r, "qwen2.5-7b-awq", ""); got != "local" {
		t.Errorf("Resolve(declared model) = %q, want local", got)
	}
}

// TestTwoBackendsDoNotGuess: with two backends there is no unambiguous answer,
// and guessing would silently send an agent's request to the wrong model -- a
// correctness failure no metric would reveal.
func TestTwoBackendsDoNotGuess(t *testing.T) {
	r := newRegistry(t,
		config.UpstreamConfig{Name: "a", BaseURL: "http://127.0.0.1:9000", Models: []string{"gpt-4o"}},
		config.UpstreamConfig{Name: "b", BaseURL: "http://127.0.0.1:9001", Models: []string{"deepseek-chat"}},
	)
	target, err := r.Resolve("unknown-model", "")
	if err == nil {
		t.Fatalf("Resolve(unknown) = %q, want an error rather than a guess", target.Name)
	}
	if !errors.Is(err, ErrNoTarget) {
		t.Errorf("error = %v, want it to wrap ErrNoTarget", err)
	}
	if !strings.Contains(err.Error(), "unknown-model") {
		t.Errorf("error = %q, want it to name the model that could not be placed", err)
	}
}

// TestFirstConfiguredBackendKeepsAContestedModel: configuration order is the
// tie-breaker, so appending a second backend that also claims a model cannot
// silently steal the traffic that was already flowing.
func TestFirstConfiguredBackendKeepsAContestedModel(t *testing.T) {
	r := newRegistry(t,
		config.UpstreamConfig{Name: "incumbent", BaseURL: "http://127.0.0.1:9000", Models: []string{"gpt-4o"}},
		config.UpstreamConfig{Name: "newcomer", BaseURL: "http://127.0.0.1:9001", Models: []string{"gpt-4o"}},
	)
	if got := mustResolve(t, r, "gpt-4o", ""); got != "incumbent" {
		t.Errorf("Resolve(contested model) = %q, want the first configured backend", got)
	}
	// The pin still reaches the newcomer: nothing is unreachable, the model
	// index just does not move.
	if got := mustResolve(t, r, "gpt-4o", "newcomer"); got != "newcomer" {
		t.Errorf("Resolve(pinned) = %q, want newcomer", got)
	}
}

// TestCatchAllDoesNotEnterTheModelIndex: the catch-all is consulted only after
// an exact match misses, so it must not occupy a model key and shadow the
// backend that explicitly declared that model.
func TestCatchAllDoesNotEnterTheModelIndex(t *testing.T) {
	r := newRegistry(t,
		config.UpstreamConfig{Name: "specific", BaseURL: "http://127.0.0.1:9000", Models: []string{"gpt-4o"}},
		config.UpstreamConfig{Name: "any", BaseURL: "http://127.0.0.1:9001", Models: []string{"/"}},
	)
	index := r.ModelIndex()
	if _, exists := index["/"]; exists {
		t.Errorf("ModelIndex contains %q, want the catch-all excluded", "/")
	}
	if index["gpt-4o"] != "specific" {
		t.Errorf("ModelIndex[gpt-4o] = %q, want specific", index["gpt-4o"])
	}
}

// TestModelIndexIsSortedAndKeyedLowercase: the index feeds /admin/upstreams, so
// its ordering must be stable between two reads of an unchanged config.
func TestModelIndexIsSortedAndKeyedLowercase(t *testing.T) {
	r := newRegistry(t, config.UpstreamConfig{
		Name: "mock", BaseURL: "http://127.0.0.1:9000",
		Models: []string{"Qwen2.5", "gpt-4o", "DeepSeek-Chat"},
	})
	index := r.ModelIndex()
	want := []string{"deepseek-chat", "gpt-4o", "qwen2.5"}
	if len(index) != len(want) {
		t.Fatalf("ModelIndex has %d entries, want %d: %v", len(index), len(want), index)
	}
	for _, k := range want {
		if index[k] != "mock" {
			t.Errorf("ModelIndex[%q] = %q, want mock (keys are lower-cased for lookup)", k, index[k])
		}
	}
	target, _ := r.Target("mock")
	if got := target.ModelPatterns(); !stringSlicesEqual(got, []string{"Qwen2.5", "gpt-4o", "DeepSeek-Chat"}) {
		t.Errorf("ModelPatterns() = %v, want the configured spelling and order preserved", got)
	}
}

// ---------------------------------------------------------------------------
// Target accessors
// ---------------------------------------------------------------------------

func TestTargetAccessorsExposeConfiguration(t *testing.T) {
	r := newRegistry(t, config.UpstreamConfig{
		Name:         "local-vllm",
		Kind:         "openai",
		BaseURL:      "http://127.0.0.1:8000",
		APIKey:       "sk-local",
		Models:       []string{"qwen2.5-7b-awq", "/"},
		Capabilities: []string{"  Tools ", "JSON-MODE", ""},
		Priority:     9,
		Weight:       2.5,
		Tier:         "local",
	})
	target, ok := r.Target("local-vllm")
	if !ok {
		t.Fatal("Target(local-vllm) missing")
	}
	if target.Name != "local-vllm" || target.BaseURL != "http://127.0.0.1:8000" || target.APIKey != "sk-local" {
		t.Errorf("identity = %q / %q / %q, want the configured values", target.Name, target.BaseURL, target.APIKey)
	}
	if target.Kind != config.KindOpenAI {
		t.Errorf("Kind = %q, want %q", target.Kind, config.KindOpenAI)
	}
	if target.Priority() != 9 || target.Weight() != 2.5 || target.Tier() != "local" {
		t.Errorf("Priority/Weight/Tier = %d / %v / %q, want 9 / 2.5 / local", target.Priority(), target.Weight(), target.Tier())
	}
	if !target.IsCatchAll() {
		t.Error("IsCatchAll() = false although \"/\" is configured")
	}
	if got := target.Tags(); got != "local-vllm (catch-all)" {
		t.Errorf("Tags() = %q, want the catch-all marker in logs", got)
	}
	// Capabilities are normalised on construction: lower-cased, trimmed and
	// with empty entries dropped, so matching never has to re-normalise.
	if got := target.Capabilities(); !stringSlicesEqual(got, []string{"tools", "json-mode"}) {
		t.Errorf("Capabilities() = %v, want [tools json-mode]", got)
	}
}

func TestTagsWithoutCatchAllIsJustTheName(t *testing.T) {
	r := newRegistry(t, config.UpstreamConfig{
		Name: "openai", BaseURL: "https://api.openai.com", Models: []string{"gpt-4o"},
	})
	target, _ := r.Target("openai")
	if got := target.Tags(); got != "openai" {
		t.Errorf("Tags() = %q, want the bare name when the backend is not a catch-all", got)
	}
}

// TestServesModel: the model list is an allow-list, but an empty model name is
// "the caller did not say", not "the caller said nothing matches".
func TestServesModel(t *testing.T) {
	r := newRegistry(t,
		config.UpstreamConfig{Name: "specific", BaseURL: "http://127.0.0.1:9000", Models: []string{"GPT-4o"}},
		config.UpstreamConfig{Name: "any", BaseURL: "http://127.0.0.1:9001", Models: []string{"/"}},
	)
	specific, _ := r.Target("specific")
	any, _ := r.Target("any")

	cases := []struct {
		name   string
		target *Target
		model  string
		want   bool
	}{
		{"exact", specific, "GPT-4o", true},
		{"case-insensitive", specific, "gpt-4O", true},
		{"trimmed", specific, "  gpt-4o  ", true},
		{"other model", specific, "deepseek-chat", false},
		{"empty model is not a mismatch", specific, "", true},
		{"catch-all serves anything", any, "literally-anything", true},
		{"catch-all serves empty", any, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.target.ServesModel(tc.model); got != tc.want {
				t.Errorf("%s.ServesModel(%q) = %v, want %v", tc.target.Name, tc.model, got, tc.want)
			}
		})
	}
}

// TestHasCapabilities pins the two deliberate rules: an empty requirement
// always matches, and a non-empty requirement never matches a backend that
// declared nothing. "We don't know" must not be read as "we support it".
func TestHasCapabilities(t *testing.T) {
	r := newRegistry(t,
		config.UpstreamConfig{
			Name: "cloud", BaseURL: "https://api.openai.com", Models: []string{"gpt-4o"},
			Capabilities: []string{"tools", "vision", "long-context"},
		},
		config.UpstreamConfig{
			Name: "silent", BaseURL: "http://127.0.0.1:8000", Models: []string{"local"},
		},
	)
	cloud, _ := r.Target("cloud")
	silent, _ := r.Target("silent")

	cases := []struct {
		name     string
		target   *Target
		required []string
		want     bool
	}{
		{"no requirement matches a capable backend", cloud, nil, true},
		{"no requirement matches a silent backend", silent, nil, true},
		{"one declared capability", cloud, []string{"tools"}, true},
		{"all declared capabilities", cloud, []string{"tools", "vision"}, true},
		{"requirement is normalised like the declaration", cloud, []string{"  TOOLS ", "Vision"}, true},
		{"one missing capability fails the whole set", cloud, []string{"tools", "audio"}, false},
		{"unknown backend supports nothing", silent, []string{"tools"}, false},
		{"an empty tag in the requirement is still a requirement", cloud, []string{"", "tools"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.target.HasCapabilities(tc.required); got != tc.want {
				t.Errorf("%s.HasCapabilities(%v) = %v, want %v", tc.target.Name, tc.required, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// registry accessors and shutdown
// ---------------------------------------------------------------------------

// TestRegistryAccessorsCopyTheirSlices: Targets and Names hand out snapshots.
// A caller that sorted or truncated the live slice would mutate routing for
// every other request in flight.
func TestRegistryAccessorsCopyTheirSlices(t *testing.T) {
	r := newRegistry(t,
		config.UpstreamConfig{Name: "a", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}},
		config.UpstreamConfig{Name: "b", BaseURL: "http://127.0.0.1:9001", Models: []string{"/"}},
	)
	if got := r.Names(); !stringSlicesEqual(got, []string{"a", "b"}) {
		t.Fatalf("Names() = %v, want [a b] in configuration order", got)
	}
	targets := r.Targets()
	first := targets[0]
	targets[0] = targets[1]
	targets = append(targets, nil)

	if got := r.Names(); !stringSlicesEqual(got, []string{"a", "b"}) {
		t.Errorf("Names() = %v after a caller mutated its slice, want the registry unchanged", got)
	}
	if got := r.Targets(); len(got) != 2 || got[0] != first {
		t.Errorf("Targets() = %v after a caller mutated its slice, want the registry unchanged", got)
	}
	if got := mustResolve(t, r, "", ""); got != "a" {
		t.Errorf("Resolve after a caller mutated its slice = %q, want a (the first catch-all)", got)
	}
}

func TestTargetLookupMissesCleanly(t *testing.T) {
	r := newRegistry(t, config.UpstreamConfig{
		Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"},
	})
	if target, ok := r.Target("nope"); ok || target != nil {
		t.Errorf("Target(nope) = %v, %v; want nil, false", target, ok)
	}
}

// TestRegistryIsSafeForConcurrentReaders: routing happens on every request from
// many goroutines, and /admin/upstreams reads the same maps. This is the test
// that pays off under -race.
func TestRegistryIsSafeForConcurrentReaders(t *testing.T) {
	r := newRegistry(t,
		config.UpstreamConfig{Name: "a", BaseURL: "http://127.0.0.1:9000", Models: []string{"gpt-4o"}, Capabilities: []string{"tools"}},
		config.UpstreamConfig{Name: "b", BaseURL: "http://127.0.0.1:9001", Models: []string{"/"}},
	)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				switch (i + j) % 6 {
				case 0:
					r.Resolve("gpt-4o", "")
				case 1:
					r.Resolve("anything", "b")
				case 2:
					r.Targets()
				case 3:
					r.Names()
				case 4:
					r.ModelIndex()
				case 5:
					if target, ok := r.Target("a"); ok {
						target.HasCapabilities([]string{"tools"})
						target.Tags()
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestCloseIdleConnectionsIsIdempotent: shutdown calls it once, but a second
// server Shutdown or a test helper may call it again, and closing an already
// drained pool must not panic.
func TestCloseIdleConnectionsIsIdempotent(t *testing.T) {
	r := newRegistry(t, config.UpstreamConfig{
		Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"},
	})
	r.CloseIdleConnections()
	r.CloseIdleConnections()
}

// ---------------------------------------------------------------------------
// examples (rendered in godoc, and executable, so they cannot rot)
// ---------------------------------------------------------------------------

func ExampleRegistry_Resolve() {
	r, err := New(testConfigForExample())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	target, err := r.Resolve("gpt-4o", "")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(target.Name, target.BaseURL)

	target, err = r.Resolve("an-alias-the-gateway-never-heard-of", "")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(target.Name)
	// Output:
	// openai https://api.openai.com
	// mock
}

// testConfigForExample is testConfig without *testing.T, because an Example
// function has no testing.T to hand.
func testConfigForExample() *config.Config {
	cfg := config.Defaults()
	cfg.Upstreams = []config.UpstreamConfig{
		{Name: "openai", BaseURL: "https://api.openai.com", Models: []string{"gpt-4o", "gpt-4o-mini"}},
		{Name: "mock", BaseURL: "http://127.0.0.1:9000", Models: []string{"/"}},
	}
	return &cfg
}
