// Command verify-m5 is InferGate's M5 acceptance test: the observability plane
// -- bounded latency windows, real histograms, W3C trace context, per-request
// traces with one span per upstream attempt, and the two trace exporters.
//
// The unit tests in internal/metrics and internal/tracing already prove the
// data structures in isolation. This program exists to prove the things that
// are only observable when a whole gateway is assembled from a config: that
// /metrics really exposes cumulative buckets whose +Inf equals _count, that a
// request really produces a root span plus one span per attempt with a parent
// link a collector can stitch, that the caller's traceparent really reaches the
// upstream (read off the upstream's own recorded headers, not the gateway's
// opinion), that an evicting ring really forgets the oldest trace, and that a
// JSONL path that cannot be opened really stops startup instead of quietly
// dropping traces.
//
// Every claim is checked against evidence: the upstream records the headers and
// bodies it received, the traces are read back through /admin/traces, and the
// exports are captured by a real collector listening on a socket.
//
// Exit code 0 means every assertion passed.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	verbose := flag.Bool("v", false, "log each observation the checks record, not only the failures")
	flag.Parse()

	out := os.Stdout
	fmt.Fprintln(out, "InferGate M5 end-to-end verification (observability: histograms, tracing, export)")
	fmt.Fprintln(out, strings.Repeat("=", 72))

	c := &checker{}
	if *verbose {
		verboseChecks = true
	}

	checks := []struct {
		name string
		run  func(*checker)
	}{
		{"the request latency window is bounded and drops the oldest", checkLatencyWindow},
		{"histograms are cumulative, and +Inf equals the count", checkHistogramInvariants},
		{"/metrics exposes the new families and keeps the M0 contract", checkMetricsExposition},
		{"/stats reports the latency window it uses", checkStatsEndpoint},
		{"/admin/tracing reports the tracing configuration", checkTracingConfigSurface},
		{"one request produces one trace with a root span per attempt", checkTracePerRequest},
		{"traceparent is continued, minted, dropped and never trusted", checkTracePropagation},
		{"sampling is deterministic per request id and does discriminate", checkTraceSampling},
		{"a failover records one span per attempt and an event", checkTraceFailover},
		{"a streamed response is one span with its frame facts", checkStreamTrace},
		{"the trace store is bounded, ordered and validated", checkTraceStore},
		{"the JSONL exporter writes one line per trace and appends", checkJSONLExport},
		{"the OTLP exporter posts a well-formed payload", checkOTLPExport},
		{"tracing off records nothing and propagates verbatim", checkTracingDisabled},
	}

	for i, chk := range checks {
		fmt.Fprintf(out, "\n[%d/%d] %s\n", i+1, len(checks), chk.name)
		chk.run(c)
	}

	fmt.Fprintln(out, "\n"+strings.Repeat("=", 72))
	total := c.passed + c.failed
	fmt.Fprintf(out, "RESULT: %d/%d assertions passed\n", c.passed, total)
	if c.failed > 0 {
		fmt.Fprintf(out, "FAILED: %d assertion(s) failed\n", c.failed)
		for _, note := range c.notes {
			fmt.Fprintf(out, "  - %s\n", note)
		}
		os.Exit(1)
	}
	fmt.Fprintln(out, "OK: M5 observability acceptance criteria met")
}
