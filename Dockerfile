# InferGate container image.
#
# One Dockerfile, two binaries: CMD selects which command to compile, so
# `docker build --build-arg CMD=infergate` and `--build-arg CMD=mockupstream`
# share this stage instead of duplicating a second file. docker-compose.yml uses
# exactly that for its two services.
#
# Local build (Docker Desktop on Windows, or any Linux host):
#
#   docker build --build-arg CMD=infergate    -t infergate:local .
#   docker build --build-arg CMD=mockupstream -t infergate-mock:local .
#   docker run --rm infergate:local -config /etc/infergate/configs/mock.yaml -check
#
# WHY THE DEFAULT BASE TAG IS A VARIABLE. The module targets Go 1.26 (see go.mod)
# and this project pins its toolchain from the local module cache rather than the
# PATH (README §4). Image tags for a brand-new Go release appear on Docker Hub
# before or after they appear here, so the tag is an ARG you can override without
# editing this file:
#
#   docker build --build-arg GO_IMAGE=golang:1.26-alpine --build-arg CMD=infergate .
#
# There is no vendor/ and no go.sum because the module has NO third-party
# dependencies (stdlib only, README §2), so the build needs no module download
# beyond what the toolchain image ships. If you ever add a dependency, add
# `go mod download` after the COPY of go.mod/go.sum below.

ARG GO_IMAGE=golang:1.26-alpine

# ---------- build ----------
FROM ${GO_IMAGE} AS build

ARG CMD=infergate
# -trimpath drops the build machine's absolute paths; -s -w drops the symbol
# table and DWARF. Both are about not shipping a 9 MB binary when 6 MB does.
ARG LDFLAGS="-s -w"

WORKDIR /src

# go.mod alone is enough to resolve the module path (no external deps).
COPY go.mod ./

COPY cmd ./cmd
COPY internal ./internal

# CGO_ENABLED=0 is not a size trick: the whole point of this project is a
# dependency-free static binary, and it is what makes the alpine runtime stage
# (musl) viable at all. The local machine cannot run `go test -race` for exactly
# the opposite reason (race needs cgo and there is no gcc — README §4/§10).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "${LDFLAGS}" -o /out/app ./cmd/${CMD}

# ---------- runtime ----------
FROM alpine:3.20 AS runtime

# ca-certificates is the one package that matters: talking to a real provider
# (https://api.deepseek.com and friends) needs a trust store. wget comes from
# busybox and is what the compose healthchecks use.
RUN apk add --no-cache ca-certificates

# Non-root. The gateway needs no writable path of its own: the cache and the
# quota counters live in Redis (or process memory), and traces go to stdout, an
# OTLP endpoint or a JSONL file that an operator mounts.
RUN adduser -D -H -u 10001 infergate

WORKDIR /app
COPY --from=build /out/app /app/infergate
# Configs are baked in so `docker run` works with no bind mount; in compose the
# whole configs/ directory is mounted over this, which is the more useful shape
# for experimenting without rebuilding.
COPY configs /app/configs

USER infergate

# 8080: gateway + /metrics + /healthz /readyz /stats /admin/*
# 9000: mockupstream's OpenAI-compatible surface (only used by the mock image)
EXPOSE 8080 9000

# Liveness only. /readyz is the stronger probe (it looks at the routing table and
# the stores) and is what compose uses for dependents.
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/infergate"]
# Default config is the self-contained mock profile. Override with:
#   docker run ... infergate:local -config /app/configs/infergate.yaml
CMD ["-config", "/app/configs/mock.yaml"]
