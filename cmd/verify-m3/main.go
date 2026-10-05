// Command verify-m3 is InferGate's M3 acceptance test: token and cost
// governance, exercised end to end through the real server assembly.
//
// The unit tests in internal/quota and internal/gateway already prove the
// policy and the request path in isolation. This program exists to prove the
// things that are only observable when the whole gateway is assembled from a
// config: that a budget configured in YAML actually refuses an HTTP request,
// that a refusal really kept the request away from the provider, that a
// degraded request really reached the provider in its cheaper shape, that the
// counters /admin/quota reports are the same numbers a raw RESP2 client sees
// under the documented key layout, and that a broken counter store fails in
// the direction the operator asked for.
//
// Every claim is checked against evidence rather than the gateway's opinion of
// itself: the upstream counts the requests it received and names the model it
// was asked for, the counter store is read back directly, and /metrics and
// /admin/quota are scraped for the numbers.
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
	verbose := flag.Bool("v", false, "log every request the gateway serves")
	flag.Parse()

	out := os.Stdout
	fmt.Fprintln(out, "InferGate M3 end-to-end verification (token and cost governance)")
	fmt.Fprintln(out, strings.Repeat("=", 72))

	c := &checker{out: out}
	env := &environment{out: out, verbose: *verbose}

	checks := []struct {
		name string
		run  func(*checker, *environment)
	}{
		{"disabled governance is fully inert", checkDisabled},
		{"the memory store admits, ledgers and reports", checkMemoryAllow},
		{"the daily token budget refuses and stops the upstream call", checkDailyTokenBudget},
		{"the per-minute rate limit counts requests, not tokens", checkMinuteRateLimit},
		{"per-session budgets are independent counters", checkSessionBudget},
		{"the daily cost budget refuses on the price book", checkCostBudget},
		{"a breach degrades the model the upstream is asked for", checkDegradeByModel},
		{"a breach degrades the completion ceiling", checkDegradeByCap},
		{"settle, overshoot and release", checkSettleOvershootRelease},
		{"the Redis store: key layout, TTL and agreement with /admin/quota", checkRedisStore},
		{"a crafted tenant name cannot charge another tenant", checkKeyIsolation},
		{"fail-closed refuses, fail-open admits", checkFailClosedOpen},
		{"non-completion paths are not governed", checkNonCompletionPath},
		{"the metrics, stats and admin surfaces", checkSurfaces},
		{"anomaly alerting observes without refusing", checkAnomaly},
		{"quota configuration validation", checkConfigValidation},
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
	fmt.Fprintln(out, "OK: M3 token and cost governance acceptance criteria met")
}
