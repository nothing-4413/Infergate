package main

// cmd/verify-m6 is the in-process acceptance gate for M6 (agent-platform
// integration): idempotent replay, the per-session ledger, and the capability
// surface.
//
// It assembles the real gateway (server.NewServer over a real listener) and
// drives it with HTTP, so an assertion here is a statement about the binary, not
// about a helper. The companion gate scripts/verify-m6.ps1 repeats the headline
// paths through curl against real processes; this one can reach the internals
// (a broken pipe mid-stream, a 409 in flight) without a script.

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	verbose := flag.Bool("v", false, "print every assertion detail")
	flag.Parse()
	verboseChecks = *verbose

	c := &checker{}
	fmt.Println("InferGate M6 acceptance gate: agent-platform integration")

	checks := []struct {
		name string
		run  func(*checker)
	}{
		{"idempotent replay: one key, one provider call", checkIdempotentReplay},
		{"what is NOT remembered, and why", checkNotRemembered},
		{"the idempotency admin surface and its metrics", checkIdempotencySurface},
		{"the per-session ledger", checkSessionLedger},
		{"declared capabilities and the live probe", checkCapabilities},
		{"an agent's tool-calling conversation end to end", checkAgentConversation},
	}

	for i, chk := range checks {
		fmt.Printf("\n[%d/%d] %s\n", i+1, len(checks), chk.name)
		chk.run(c)
	}

	fmt.Printf("\nRESULT: %d/%d assertions passed\n", c.passed, c.passed+c.failed)
	if c.failed > 0 {
		fmt.Printf("FAILED: %d assertion(s) failed\n", c.failed)
		for _, n := range c.notes {
			fmt.Printf("  - %s\n", n)
		}
		os.Exit(1)
	}
}
