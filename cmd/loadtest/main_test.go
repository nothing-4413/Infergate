package main

import (
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// -urls has to accept both spellings the generator promises, keep the caller's
// order and drop duplicates — and it has to do so through the real flag.Value
// path, because "-urls a,b" and "-urls a -urls b" only mean the same thing if
// the flag machinery appends instead of replacing.
func TestURLListParsesCommaListAndRepeats(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"comma separated", []string{"http://a:1,http://b:2"}, []string{"http://a:1", "http://b:2"}},
		{"repeated flags", []string{"http://a:1", "http://b:2"}, []string{"http://a:1", "http://b:2"}},
		{"mixed, order preserved", []string{"http://b:2,http://a:1", "http://c:3"}, []string{"http://b:2", "http://a:1", "http://c:3"}},
		{"duplicates dropped at first position", []string{"http://a:1", "http://b:2", "http://a:1,http://b:2", "http://a:1"}, []string{"http://a:1", "http://b:2"}},
		{"space and trailing slash normalised", []string{" http://a:1/ , http://b:2 "}, []string{"http://a:1", "http://b:2"}},
		{"empty entries ignored", []string{"", ",,", ",http://a:1,"}, []string{"http://a:1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var l urlList
			fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
			fs.Var(&l, "urls", "")
			var argv []string
			for _, a := range tc.args {
				argv = append(argv, "-urls", a)
			}
			if err := fs.Parse(argv); err != nil {
				t.Fatalf("parse %q: %v", argv, err)
			}
			if got := l.list(); !slices.Equal(got, tc.want) {
				t.Errorf("-urls %q: got %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// The spread is round-robin per request, reproducible for a given target list,
// and balanced to within one request however many requests are run.
func TestTargetSetIsRoundRobinStableAndBalanced(t *testing.T) {
	targets := []string{"http://a:1", "http://b:2", "http://c:3"}

	set := newTargetSet(targets)
	for i := 0; i < 11; i++ {
		idx, base := set.pick()
		if want := i % len(targets); idx != want {
			t.Fatalf("pick %d: index %d, want %d", i, idx, want)
		}
		if base != targets[idx] {
			t.Fatalf("pick %d: base %q, want %q", i, base, targets[idx])
		}
	}

	// The assignment of request index to target is a property of the target
	// list alone: two runs of the same command draw the same sequence.
	first, second := newTargetSet(targets), newTargetSet(targets)
	for i := 0; i < 23; i++ {
		a, _ := first.pick()
		b, _ := second.pick()
		if a != b {
			t.Fatalf("pick %d: %d from one run, %d from the next", i, a, b)
		}
	}

	// Including the request counts that are not a multiple of the target count,
	// which is where an off-by-one hand-out would show up as a two-request gap.
	for n := 0; n <= 40; n++ {
		s := newTargetSet(targets)
		counts := make([]int, len(targets))
		for i := 0; i < n; i++ {
			idx, _ := s.pick()
			counts[idx]++
		}
		if hi, lo := slices.Max(counts), slices.Min(counts); hi-lo > 1 {
			t.Errorf("n=%d: counts %v differ by %d", n, counts, hi-lo)
		}
	}
}

// The counter is shared by every worker, so under -race this is also the test
// that says the hand-out is not a per-worker one that would drift as soon as
// one target is slower than its siblings.
func TestTargetSetConcurrentPicksStayBalanced(t *testing.T) {
	targets := []string{"http://a:1", "http://b:2", "http://c:3", "http://d:4"}
	const workers, each = 8, 125

	set := newTargetSet(targets)
	counts := make([]int, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int, len(targets))
			for i := 0; i < each; i++ {
				idx, _ := set.pick()
				local[idx]++
			}
			mu.Lock()
			for i, n := range local {
				counts[i] += n
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	want := workers * each / len(targets)
	for i, got := range counts {
		if got != want {
			t.Errorf("target %d drew %d requests, want %d (all: %v)", i, got, want, counts)
		}
	}
}

// A correct picker that nobody wired up would still be a broken feature, so
// this drives a whole phase against real HTTP backends and checks the per-target
// numbers that land in the record.
func TestRunPhaseSpreadsAcrossRealTargets(t *testing.T) {
	var (
		mu   sync.Mutex
		hits int
	)
	backend := func(status int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			hits++
			mu.Unlock()
			if status != http.StatusOK {
				http.Error(w, "nope", status)
				return
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	targets := []string{backend(http.StatusOK).URL, backend(http.StatusOK).URL, backend(http.StatusOK).URL}

	const requests, warmup = 37, 3
	r, err := runPhase(newClient(10*time.Second), "non-stream", targets, false, 4, requests, warmup, 10*time.Second)
	if err != nil {
		t.Fatalf("runPhase: %v", err)
	}
	if r.Errors != 0 {
		t.Fatalf("errors = %d, want 0", r.Errors)
	}
	if r.Requests != requests {
		t.Errorf("requests = %d, want %d", r.Requests, requests)
	}
	if want := strings.Join(targets, ","); r.Target != want {
		t.Errorf("target = %q, want the comma-joined list %q", r.Target, want)
	}
	if len(r.Targets) != len(targets) {
		t.Fatalf("targets has %d entries, want %d", len(r.Targets), len(targets))
	}

	var sumReq, sumErr int
	var sumQPS float64
	for i, ts := range r.Targets {
		if ts.URL != targets[i] {
			t.Errorf("targets[%d].url = %q, want %q (order must follow the -urls list)", i, ts.URL, targets[i])
		}
		sumReq += ts.Requests
		sumErr += ts.Errors
		sumQPS += ts.QPS
	}
	if sumReq != requests {
		t.Errorf("per-target request counts sum to %d, want %d", sumReq, requests)
	}
	if sumErr != r.Errors {
		t.Errorf("per-target error counts sum to %d, want %d", sumErr, r.Errors)
	}
	if d := sumQPS - r.QPS; d > 1e-6 || d < -1e-6 {
		t.Errorf("per-target qps sum to %f, want the record's %f", sumQPS, r.QPS)
	}
	for i := range r.Targets {
		for j := range r.Targets {
			if d := r.Targets[i].Requests - r.Targets[j].Requests; d > 1 || d < -1 {
				t.Fatalf("targets %d and %d were handed %d and %d requests", i, j, r.Targets[i].Requests, r.Targets[j].Requests)
			}
		}
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != requests+warmup {
		t.Errorf("backends saw %d requests, want %d measured plus %d warmup", got, requests, warmup)
	}
}

// A target that fails must cost its own request and nothing more: the workers
// keep drawing from every target, so the phase finishes and the healthy targets
// still carry their share. (Warmup is 0 here on purpose — a warmup failure is
// still fatal, which the doc comment on runPhase records as a limitation.)
func TestRunPhaseFailingTargetDoesNotWedgeOthers(t *testing.T) {
	var (
		mu   sync.Mutex
		hits int
	)
	backend := func(status int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			hits++
			mu.Unlock()
			if status != http.StatusOK {
				http.Error(w, "nope", status)
				return
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	targets := []string{backend(http.StatusOK).URL, backend(http.StatusInternalServerError).URL, backend(http.StatusOK).URL}

	const requests = 30
	r, err := runPhase(newClient(10*time.Second), "non-stream", targets, false, 4, requests, 0, 10*time.Second)
	if err != nil {
		t.Fatalf("runPhase: %v", err)
	}
	if r.Requests != 20 || r.Errors != 10 {
		t.Errorf("requests/errors = %d/%d, want 20/10", r.Requests, r.Errors)
	}
	for i, ts := range r.Targets {
		if ts.Requests != 10 {
			t.Errorf("targets[%d] was handed %d requests, want 10", i, ts.Requests)
		}
	}
	if r.Targets[1].Errors != 10 {
		t.Errorf("the failing target reports %d errors, want 10", r.Targets[1].Errors)
	}
	if r.Targets[0].Errors != 0 || r.Targets[2].Errors != 0 {
		t.Errorf("healthy targets report errors: %v", r.Targets)
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != requests {
		t.Errorf("backends saw %d requests, want %d", got, requests)
	}
}

// When the temp directory is not writable the mock has no log file, and a
// startup failure must say that rather than print an empty tail that reads as
// "the child was silent".
func TestMockLogTailDegradesWithoutLogFile(t *testing.T) {
	m := &mockProc{}
	if got, want := m.logTail(), "log unavailable, temp dir not writable"; got != want {
		t.Errorf("logTail() with no log = %q, want %q", got, want)
	}
	// A path that was set but has since gone must also degrade, not panic.
	m = &mockProc{logPath: "loadtest-no-such-log-file.log"}
	if got := m.logTail(); !strings.HasPrefix(got, "log unavailable: ") {
		t.Errorf("logTail() with a missing log = %q, want a 'log unavailable' prefix", got)
	}
}

// Repeated rounds keep the per-target breakdown, and its qps is the median of
// the rounds rather than whichever round happened to be first or last.
func TestMedianResultMediansPerTarget(t *testing.T) {
	round := func(targets ...targetStat) result {
		return result{Label: "non-stream", Stream: false, Concurrency: 8, Target: "a,b", Targets: targets}
	}
	group := []result{
		round(targetStat{URL: "a", Requests: 10, QPS: 10}, targetStat{URL: "b", Requests: 10, QPS: 2}),
		round(targetStat{URL: "a", Requests: 12, QPS: 20}, targetStat{URL: "b", Requests: 12, QPS: 4}),
		round(targetStat{URL: "a", Requests: 11, QPS: 30}, targetStat{URL: "b", Requests: 11, QPS: 6}),
	}
	got := medianResult(group)
	if len(got.Targets) != 2 {
		t.Fatalf("targets has %d entries, want 2", len(got.Targets))
	}
	for i, want := range []targetStat{{URL: "a", Requests: 11, QPS: 20}, {URL: "b", Requests: 11, QPS: 4}} {
		if got.Targets[i] != want {
			t.Errorf("targets[%d] = %+v, want %+v", i, got.Targets[i], want)
		}
	}
	if got.Rounds != len(group) {
		t.Errorf("rounds = %d, want %d", got.Rounds, len(group))
	}
}
