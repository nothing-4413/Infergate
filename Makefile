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

.PHONY: all build vet test verify verify-curl loadtest diag run-mock run-gateway clean help

all: build vet test

help:
	@echo "targets: build vet test verify verify-curl loadtest diag run-mock run-gateway clean"

## build: compile every package and emit the two binaries.
build:
	$(GO) build ./...
	$(GO) build -o bin/infergate$(EXE) ./cmd/infergate
	$(GO) build -o bin/mockupstream$(EXE) ./cmd/mockupstream

## vet: static checks (nil deref, shadowed vars, printf verbs).
vet:
	$(GO) vet ./...

## test: unit + integration suite.  -race is NOT available on this host (no gcc).
test:
	$(GO) test ./...

## verify: in-process Go end-to-end acceptance (38 assertions, CI gate).
verify:
	$(GO) run ./cmd/verify

## verify-curl: real processes + real curl.exe (47 assertions).
## Requires Windows PowerShell; `pwsh` does not exist on this host.
verify-curl:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/verify-m0.ps1

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
