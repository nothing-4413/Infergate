// Command verify-m4 is InferGate's M4 acceptance test: tiered local/cloud
// routing, exercised against both the router's own plan and the assembled
// gateway.
//
// The unit tests in internal/router and internal/config already prove the
// ordering and the validation rules in isolation. This program exists to prove
// the things that are only observable when the whole gateway is assembled from
// a config: that a `tier: local` upstream declared in YAML really becomes the
// preferred candidate for a simple request over HTTP, that a hard request
// really reaches the paid provider while keeping the local box behind it for
// failover, that the tier boundary survives an upstream that is DOWN rather
// than merely deprioritised, that the money the boundary saves is readable off
// /stats, and that /admin/upstreams tells an operator which tier each backend
// is on.
//
// Every claim is checked against evidence rather than the gateway's opinion of
// itself: each tier is a recording upstream that counts the requests it
// received and names the model it was asked for, and the router's plan is read
// directly where the response headers cannot show it.
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
	verbose := flag.Bool("v", false, "log every request the gateway serves, and the loop/call-site assertion split")
	flag.Parse()

	out := os.Stdout
	fmt.Fprintln(out, "InferGate M4 end-to-end verification (tiered local/cloud routing)")
	fmt.Fprintln(out, strings.Repeat("=", 72))

	c := &checker{out: out}
	env := &environment{out: out, verbose: *verbose}

	checks := []struct {
		name string
		run  func(*checker, *environment)
	}{
		{"a simple request prefers the local tier", checkSimplePrefersLocal},
		{"the tier limits are strict, and zero disables one", checkTierLimits},
		{"a cloud capability leaves the local tier out of the plan", checkCapabilityEligibility},
		{"inside a tier the order is priority, then config order", checkIntraTierOrder},
		{"an open local breaker yields to a healthy cloud", checkUnhealthyTierYields},
		{"a hard request still carries the local tier behind the cloud", checkHardKeepsFallback},
		{"the decision reason and score name the tier", checkReasonAndScore},
		{"upstream tier validation", checkTierValidation},
		{"tier_policy validation and defaults", checkTierPolicyValidation},
		{"the tiered strategy requires a local upstream", checkTieredRequiresLocal},
		{"a tiered config loads and round-trips", checkTieredConfigRoundTrip},
		{"a simple request is served by the local tier end to end", checkE2ESimple},
		{"a hard request is served by the cloud tier end to end", checkE2EHard},
		{"failover crosses the tier boundary in both directions", checkE2EFailover},
		{"the tier split is where the money is saved", checkTierEconomics},
		{"/admin/upstreams reports the tier of every upstream", checkAdminTiers},
	}

	for i, chk := range checks {
		fmt.Fprintf(out, "\n[%d/%d] %s\n", i+1, len(checks), chk.name)
		chk.run(c, env)
	}

	fmt.Fprintln(out, "\n"+strings.Repeat("=", 72))
	total, failed := c.tally()
	if *verbose {
		fmt.Fprintf(out, "   %d of the assertions were issued from loops; %d from call sites\n", c.loops, total-c.loops)
	}
	fmt.Fprintf(out, "RESULT: %d/%d assertions passed\n", total-failed, total)
	if failed > 0 {
		fmt.Fprintf(out, "FAILED: %d assertion(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Fprintln(out, "OK: M4 tiered local/cloud routing acceptance criteria met")
}
