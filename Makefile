# InferGate -- M0 build/test entry points.
#
# Why this file exists at all: the host has no `go` on PATH, and the module-cache
# toolchain copy is truncated (missing src/unsafe and src/runtime), so every Go
# command must go through tools/go.cmd, which pins GOROOT/GOCACHE/GOMODCACHE to
# the repo.  Run these from the repo root with `make` (or copy the commands).
#
# On Windows PowerShell, `make` may not be installed; the equivalent one-liners
# are in README.md section 3.1.  GNU make is only a convenience wrapper here.

GO ?= tools/go.cmd

.PHONY: all build vet test verify verify-curl verify-m1 verify-m1-curl verify-m2 verify-m2-curl measure-m2 verify-m3 verify-m3-curl measure-m3 verify-m4 verify-m4-curl measure-m4 verify-m5 verify-m5-curl measure-m5 verify-m6 verify-m6-curl measure-m6 loadtest diag run-mock run-gateway run-fleet run-cache run-cache-redis run-miniredis run-quota run-quota-redis run-miniredis-quota run-tiered run-observability run-agent clean help

all: build vet test

help:
	@echo "targets: build vet test verify verify-curl verify-m1 verify-m1-curl verify-m2 verify-m2-curl measure-m2 verify-m3 verify-m3-curl measure-m3 verify-m4 verify-m4-curl measure-m4 verify-m5 verify-m5-curl measure-m5 verify-m6 verify-m6-curl measure-m6 loadtest diag run-mock run-gateway run-fleet run-cache run-cache-redis run-miniredis run-quota run-quota-redis run-miniredis-quota run-tiered run-observability run-agent clean"

## build: compile every package and emit the two binaries.
build:
	$(GO) build ./...
	$(GO) build -o bin/infergate$(EXE) ./cmd/infergate
	$(GO) build -o bin/mockupstream$(EXE) ./cmd/mockupstream

## vet: static checks (nil deref, shadowed vars, printf verbs).
vet:
	$(GO) vet ./...

## test: unit + integration suite.
##
## -race is NOT in this target because it needs cgo and a C compiler, which this
## host does not put on PATH by default -- NOT because it is unavailable.  It
## works with the ucrt64 gcc that ships with msys64, from PowerShell:
##   $env:CGO_ENABLED='1'; $env:CC='C:\msys64\ucrt64\bin\gcc.exe'
##   $env:PATH='C:\msys64\ucrt64\bin;'+$env:PATH
##   $env:TMP=$env:TEMP=(Resolve-Path .\.gotmp).Path   # cgo cannot write %TEMP%
##   .\tools\go.cmd test ./internal/tracing/ -race -count=1
## The %TEMP% override is load-bearing: without it cgo fails with
## "open ...\AppData\Local\Temp\cgo-gcc-input-<n>: Access is denied."
test:
	$(GO) test ./...

## verify: in-process Go end-to-end acceptance (38 assertions, CI gate).
verify:
	$(GO) run ./cmd/verify

## verify-curl: real processes + real curl.exe (47 assertions).
## Requires Windows PowerShell; `pwsh` does not exist on this host.
verify-curl:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/verify-m0.ps1

## verify-m1: multi-provider routing + failover acceptance (64 assertions, the M1
## CI gate).  Runs three mock replicas and really kills one: a failover test that
## never loses a backend is a test of the happy path.
verify-m1:
	$(GO) run ./cmd/verify-m1

## verify-m1-curl: the same claims through the real binary and real curl.exe,
## over a real three-replica fleet on :19100-19102 (the M0 lesson: the in-process
## gate proves the gateway is correct, a real fleet proves it is operable).
## 61 assertions.
verify-m1-curl:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/verify-m1.ps1

## run-fleet: the M1 sample fleet (configs/routing-local.yaml) on :8080 with the
## three replicas the routing config names.  Run in three more shells:
##   $(GO) run ./cmd/mockupstream -listen :9100 -name primary
##   $(GO) run ./cmd/mockupstream -listen :9101 -name secondary
##   $(GO) run ./cmd/mockupstream -listen :9102 -name tools
run-fleet:
	$(GO) run ./cmd/infergate -config configs/routing-local.yaml

## verify-m2: semantic cache acceptance (103 assertions, the M2 CI gate).  Every
## claim comes from the assembled server -- router, breakers and accounting
## included -- because a cache that returns the right bytes while attributing
## them to an upstream that was never called would pass a cache-unit test.
verify-m2:
	$(GO) run ./cmd/verify-m2

## verify-m2-curl: the same claims through the real binary and real curl.exe,
## against both the memory store (the default) and a real Redis protocol server
## (cmd/miniredis), on :18280/:18281 with mocks on :19500/:19501.  158 assertions.
verify-m2-curl:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/verify-m2.ps1

## measure-m2: what the cache is worth -- hit rate and false-hit rate over the
## labelled corpus (internal/evalset), token and cost saving, and end-to-end
## latency for a hit versus a miss.  Writes docs/baseline/m2-summary.json.
measure-m2:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/measure-m2.ps1

## run-cache: the M2 sample (configs/cache-local.yaml) on :8082 with a memory
## store.  Needs one mock in another shell:
##   $(GO) run ./cmd/mockupstream -listen :9200 -name local
run-cache:
	$(GO) run ./cmd/infergate -config configs/cache-local.yaml

## run-miniredis: the in-repo RESP2 server on :6399 -- deliberately not 6379, so
## a real Redis installed on this host is never shadowed.
run-miniredis:
	$(GO) run ./cmd/miniredis -listen :6399

## run-cache-redis: the shared-cache sample (configs/cache-redis.yaml) on :8083.
## Start `make run-miniredis` and a mock on :9201 in other shells first.
run-cache-redis:
	$(GO) run ./cmd/infergate -config configs/cache-redis.yaml

## verify-m3: token and cost governance acceptance (470 assertions, the M3 CI
## gate).  Budgets are checked against the counters they produce, not against the
## gateway's opinion: a refusal is proved by the upstream never being called
## again, and a degrade is proved by the body the backend actually received.
verify-m3:
	$(GO) run ./cmd/verify-m3

## verify-m3-curl: the same claims through the real binary and real curl.exe,
## against both the memory store and a real Redis protocol server (cmd/miniredis),
## because a budget that is only enforced in one process is not a budget.
## 323 assertions.
verify-m3-curl:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/verify-m3.ps1

## measure-m3: what governance costs and what it catches -- admission overhead on
## the governed versus ungoverned path, the accuracy of the reservation against
## real usage, and how much of a runaway tenant's spend a budget actually stops.
## Writes docs/baseline/m3-summary.json.  57 assertions of its own.
measure-m3:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/measure-m3.ps1

## verify-m4: tiered local/cloud routing acceptance (879 assertions, the M4 CI
## gate).  Classification is asserted against the router's own decisions and then
## end to end through the assembled server, because a tier policy that is right in
## the router but lost on the way to the proxy saves nothing.
verify-m4:
	$(GO) run ./cmd/verify-m4

## verify-m4-curl: the same claims through the real binary and real curl.exe over
## two mock tiers (a "local" one and a "cloud" one), so the gate needs no GPU and
## runs anywhere.  The REAL vLLM local tier is measured by measure-m4 instead.
## 128 assertions.
verify-m4-curl:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/verify-m4.ps1

## measure-m4: what the local tier is worth -- the fp16 vs AWQ vs GPTQ comparison
## on the 8 GB card (latency, throughput, VRAM, and how far the quantized text
## drifts from fp16), then a real tiered run against the real model with the split
## and the cloud spend it displaced.  Needs vLLM inside the WSL distro.
## Writes docs/baseline/m4-summary.json.
measure-m4:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/measure-m4.ps1

## verify-m5: observability acceptance (420 assertions, the M5 CI gate).  The
## exposition is asserted against the Prometheus text contract itself (HELP/TYPE,
## cumulative buckets, +Inf == _count) and each trace against the span tree it
## produced, because a metric that is merely present is not observable.
verify-m5:
	$(GO) run ./cmd/verify-m5

## verify-m5-curl: the same claims through the real binary and real curl.exe --
## /metrics, /stats, /admin/traces, /admin/tracing, the JSONL sink, and OTLP read
## back from a real HTTP collector across a process boundary.  211 assertions.
verify-m5-curl:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/verify-m5.ps1

## measure-m5: what observability costs -- the gateway's throughput and tail
## latency at several concurrency levels against a mock upstream, the overhead of
## tracing at sample_ratio 1.0 (OTLP alone, then OTLP + JSONL), and two gateway
## instances versus one behind a single round-robin client.  Writes
## docs/baseline/m5-summary.json.
measure-m5:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/measure-m5.ps1

## verify-m6: idempotent replay, the per-session ledger, capability discovery and
## an agent's tool-calling conversation end to end (381 assertions, the M6 CI
## gate).  The claims that matter are negative -- the provider was NOT called a
## second time, the replayed body is byte-identical, a second tenant does NOT see
## the first tenant's conversation -- so the gate asserts on the store's own
## counters and on the trace of the replayed request, not just on the response.
verify-m6:
	$(GO) run ./cmd/verify-m6

## verify-m6-curl: the same claims through the real binary and real curl.exe over
## a real scripted upstream process, with the mock's /calls counter as the
## external witness that a replay caused zero extra provider calls.  153
## assertions; the seven gates' curl rows add up to 1081 assertions.
verify-m6-curl:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/verify-m6.ps1

## measure-m6: what the M6 features cost and what they buy -- throughput and tail
## latency with idempotency+sessions+tracing off versus on, the latency and tokens
## a replayed turn saves against a fresh one, the store's bytes per entry at its
## configured capacity, and the provider calls one key produces under concurrency.
## Writes docs/baseline/m6-summary.json.
measure-m6:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/measure-m6.ps1

## run-agent: the M6 sample (configs/agent-local.yaml) on 127.0.0.1:18909, an
## agent platform behind one gateway with replay, the session ledger and tracing
## on.  Start the scripted upstream first (the -script file is optional: without
## it the mock answers a fixed sentence):
##   $(GO) run ./cmd/mockupstream -listen 127.0.0.1:19910 -name scripted-mock -script tmp/m6-script.json
run-agent:
	$(GO) run ./cmd/infergate -config configs/agent-local.yaml

## run-tiered: the M4 sample (configs/tiered-local.yaml) on :8080, local tier
## served by vLLM inside WSL on :8000 and cloud tier by a mock on :9100.
## Start the mock first:  $(GO) run ./cmd/mockupstream -listen :9100 -name cloud-mock
run-tiered:
	$(GO) run ./cmd/infergate -config configs/tiered-local.yaml

## run-observability: the M5 sample (configs/observability.yaml) on :18999 with
## tracing on (bounded in-memory store, JSONL export).  Needs one mock first:
##   $(GO) run ./cmd/mockupstream -listen :19900 -name observability-mock
## Then:  curl -s http://127.0.0.1:18999/admin/traces
## Prometheus + Grafana for this port are in deploy/ (see deploy/README.md).
run-observability:
	$(GO) run ./cmd/infergate -config configs/observability.yaml

## run-quota: the M3 sample (configs/quota-local.yaml) on :8084, budgets counted
## in process memory.  Needs one mock in another shell:
##   $(GO) run ./cmd/mockupstream -listen :9300 -name local
run-quota:
	$(GO) run ./cmd/infergate -config configs/quota-local.yaml

## run-miniredis-quota: the in-repo RESP2 server on :6398, so a quota run and a
## cache run can be started side by side without sharing counters.
run-miniredis-quota:
	$(GO) run ./cmd/miniredis -listen :6398

## run-quota-redis: the shared-budget sample (configs/quota-redis.yaml) on :8085.
## Start `make run-miniredis-quota` and a mock on :9301 in other shells first.
run-quota-redis:
	$(GO) run ./cmd/infergate -config configs/quota-redis.yaml

## loadtest: the M0 baseline.  Medians of 3 rounds, spreads included, because a
## single pass on a shared laptop is not reproducible (see docs/RESUME.md).
loadtest:
	$(GO) run ./cmd/loadtest -all -c 8,32 -n 1500 -warmup 300 -rounds 3 -out docs/baseline/m0-baseline.json

## diag: attribute the gateway's cost per layer (direct / plain proxy / tuned
## proxy / minimal passthrough / InferGate) instead of just blaming the gateway.
diag:
	$(GO) run ./cmd/loadtest -diag -c 8,32 -n 1500 -warmup 300 -rounds 3

## run-mock: fake OpenAI-compatible upstream on :9000 with a 250ms first-token stall.
run-mock:
	$(GO) run ./cmd/mockupstream -listen :9000 -ttfb 250ms

## run-gateway: gateway on the port in configs/mock.yaml (default :8080).
run-gateway:
	$(GO) run ./cmd/infergate -config configs/mock.yaml

clean:
	rm -rf bin
