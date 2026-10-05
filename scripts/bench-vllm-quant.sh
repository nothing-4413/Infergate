#!/usr/bin/env bash
# =============================================================================
# infergate -- M4 quantization benchmark (orchestrator, runs INSIDE the WSL distro)
#
# Compares three quantizations of the SAME 1.5B instruct chat model
# (fp16 / awq / gptq) served by vLLM on ONE machine (RTX 4070 Laptop, 8 GB) and
# records throughput, latency, VRAM and output AGREEMENT.
#
#   usage:  scripts/bench-vllm-quant.sh [variant ...]
#           scripts/bench-vllm-quant.sh                 # fp16 awq gptq
#           scripts/bench-vllm-quant.sh awq             # just one
#           PORT=8001 BENCH_ARGS='--requests 24' scripts/bench-vllm-quant.sh fp16
#
# Everything is configurable by environment (see the CONFIG block below).
#
# ---------------------------------------------------------------------------
# HONESTY: read this before quoting any number this script produces
# ---------------------------------------------------------------------------
#   * "agreement" is NOT accuracy. It only says how far a quantized model's
#     text drifts from the fp16 text for the SAME prompt on the SAME server.
#     It cannot say whether either answer is correct.
#   * The client issues requests SEQUENTIALLY (concurrency 1) with greedy
#     decoding (temperature 0, fixed seed). These are single-stream latency /
#     throughput figures on a laptop GPU, not batched datacenter throughput.
#     A p95 over 12 samples is a hint, not a distribution.
#   * Laptop GPU: clocks are shared with the Windows desktop, the card
#     thermally throttles, and the VRAM budget is 8188 MiB total with ~1 GB
#     already consumed by the desktop. Run-to-run variance is real, so one run
#     per variant is an indication, not a precise measurement. Compare variants
#     measured back-to-back in the same session, and say so.
#   * Load time and first-request time include compile / CUDA-graph capture, so
#     the client does warmup requests that are excluded from the statistics
#     (their wall time is reported separately as warmup_s).
#   * Models must already exist under $MODELS_ROOT. This script never downloads
#     and never (re)configures /opt/vllm or /opt/models.
#   * `set -e` is deliberately NOT used: a variant that fails must not abort the
#     remaining variants.
# =============================================================================

set -uo pipefail

# ---------------------------------------------------------------------------
# CRLF tolerance
# The Windows repo is checked out with core.autocrlf=true, so this file can
# arrive with CRLF line endings, which would turn every `$VAR\r` into a literal
# carriage return and break the script. If CR is detected we re-exec from a
# sanitized copy in /tmp. (The caller usually strips CR first; we do not rely
# on that.) Note: if the *shebang line* itself has CRLF and the file is exec'd
# directly, the kernel fails before any bash code runs -- invoke it as
# `bash scripts/bench-vllm-quant.sh`, or strip CR first.
# ---------------------------------------------------------------------------
if [ "${BENCH_VLLM_QUANT_REEXEC:-0}" != "1" ]; then
  _self="${BASH_SOURCE[0]:-$0}"
  if [ -f "$_self" ] && grep -q $'\r' "$_self" 2>/dev/null; then
    _san="$(mktemp "${TMPDIR:-/tmp}/bench-vllm-quant.XXXXXX" 2>/dev/null || echo "/tmp/bench-vllm-quant.$$")"
    sed 's/\r$//' "$_self" >"$_san"
    chmod +x "$_san" 2>/dev/null || true
    echo "[bench] CRLF detected in $_self -> re-exec from $_san" >&2
    BENCH_VLLM_QUANT_REEXEC=1 SANITIZED_COPY="$_san" exec bash "$_san" "$@"
  fi
fi
SANITIZED_COPY="${SANITIZED_COPY:-}"

# ---------------------------------------------------------------------------
# CONFIG (environment overridable)
# ---------------------------------------------------------------------------
VLLM_BIN=${VLLM_BIN:-/opt/vllm/bin/vllm}
VLLM_PY=${VLLM_PY:-/opt/vllm/bin/python}
MODELS_ROOT=${MODELS_ROOT:-/opt/models}
PORT=${PORT:-8000}
HOST=${HOST:-127.0.0.1}
OUT_DIR=${OUT_DIR:-/mnt/c/Users/20106/Desktop/infergate/tmp}
MAX_MODEL_LEN=${MAX_MODEL_LEN:-4096}
GPU_UTIL=${GPU_UTIL:-0.85}
SERVED_NAME=${SERVED_NAME:-local-chat}
READY_TIMEOUT=${READY_TIMEOUT:-420}
BENCH_ARGS=${BENCH_ARGS:-}
BENCH_SERVE_EXTRA=${BENCH_SERVE_EXTRA:-}

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]:-$0}")" >/dev/null 2>&1 && pwd)"
CLIENT_PY="$SCRIPT_DIR/bench_vllm_quant.py"

# An interpreter for the script's own tiny JSON helpers. $VLLM_PY is preferred,
# but it may not exist yet while the venv is still being built, so fall back to
# system python3 -- the helpers are stdlib-only on purpose.
HELPER_PY="$VLLM_PY"
if [ ! -x "$HELPER_PY" ]; then
  HELPER_PY="$(command -v python3 || true)"
fi
[ -n "$HELPER_PY" ] || HELPER_PY=/usr/bin/python3

# nvidia-smi location: normally on PATH inside WSL, occasionally only at the
# WSL driver shim path.
NVIDIA_SMI=""
if command -v nvidia-smi >/dev/null 2>&1; then
  NVIDIA_SMI="$(command -v nvidia-smi)"
elif [ -x /usr/lib/wsl/lib/nvidia-smi ]; then
  NVIDIA_SMI=/usr/lib/wsl/lib/nvidia-smi
fi

# BENCH_ARGS / BENCH_SERVE_EXTRA are whitespace-split into arrays (no shell
# quoting/expansion is interpreted, and no glob expansion happens). They are
# deliberately NOT interpolated into a quoted string.
SERVE_EXTRA_ARGS=()
if [ -n "$BENCH_SERVE_EXTRA" ]; then
  read -r -a SERVE_EXTRA_ARGS <<<"$BENCH_SERVE_EXTRA"
fi
CLIENT_EXTRA_ARGS=()
if [ -n "$BENCH_ARGS" ]; then
  read -r -a CLIENT_EXTRA_ARGS <<<"$BENCH_ARGS"
fi

DEFAULT_VARIANTS=(fp16 awq gptq)

# ---------------------------------------------------------------------------
# state
# ---------------------------------------------------------------------------
SERVER_PID=""                 # only ever set to a pid THIS script started
RESULT_LINES=()
ANY_OK=0
ALL_FAILED=1

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------
log() { echo "[bench] $*"; }

gpu_state() {
  # "name, memory.total, memory.used, utilization.gpu" or a placeholder.
  if [ -z "$NVIDIA_SMI" ]; then echo "nvidia-smi-unavailable"; return 0; fi
  local out
  out="$("$NVIDIA_SMI" --query-gpu=name,memory.total,memory.used,utilization.gpu \
        --format=csv,noheader 2>/dev/null | head -n 1 | tr -d '\r')"
  if [ -z "$out" ]; then echo "nvidia-smi-failed"; else echo "$out"; fi
}

gpu_used_mb() {
  # Used VRAM in MiB as a bare number, or empty.
  if [ -z "$NVIDIA_SMI" ]; then echo ""; return 0; fi
  "$NVIDIA_SMI" --query-gpu=memory.used --format=csv,noheader,nounits 2>/dev/null \
    | head -n 1 | tr -d ' \r'
}

port_is_busy() {
  # True if something is already listening on PORT. Tries `ss -ltn` first and
  # falls back to a python socket-bind probe when ss is missing or unusable.
  # Never signals anything: a foreign listener must be left alone.
  local out rc
  if command -v ss >/dev/null 2>&1; then
    out="$(ss -ltnH "sport = :$PORT" 2>&1)"
    rc=$?
    if [ "$rc" -eq 0 ]; then
      # ss answered authoritatively: any listener line means busy
      [ -n "$out" ] && return 0
      return 1
    fi
    log "ss probe failed (rc=$rc), falling back to a socket bind test" >&2
  fi
  if "$HELPER_PY" - "$HOST" "$PORT" <<'PY' >/dev/null 2>&1
import socket, sys
host, port = sys.argv[1], int(sys.argv[2])
sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
try:
    sock.bind((host, port))
except OSError:
    sys.exit(1)  # busy
finally:
    sock.close()
sys.exit(0)      # free
PY
  then
    return 1     # free
  fi
  return 0       # busy
}

wait_http_ok() {
  # Poll /v1/models until HTTP 200. Echoes the number of seconds it took.
  # Returns 0 on ready, 1 on timeout, 2 if the server process died.
  local pid="$1" deadline code now start
  start="$(date +%s)"
  deadline=$((start + READY_TIMEOUT))
  while :; do
    code="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "http://$HOST:$PORT/v1/models" 2>/dev/null || true)"
    if [ "$code" = "200" ]; then
      echo $(( $(date +%s) - start ))
      return 0
    fi
    if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then
      echo $(( $(date +%s) - start ))
      return 2
    fi
    now="$(date +%s)"
    if [ "$now" -ge "$deadline" ]; then
      echo $((now - start))
      return 1
    fi
    sleep 2
  done
}

stop_server() {
  # Kill ONLY the pid this script recorded, wait up to 30s, then kill -9.
  # Never pkill/pgrep by name: a foreign vLLM belongs to somebody else.
  local pid="$SERVER_PID" i
  [ -n "$pid" ] || return 0
  if ! kill -0 "$pid" 2>/dev/null; then SERVER_PID=""; return 0; fi
  log "stopping server pid=$pid (SIGTERM)"
  kill "$pid" 2>/dev/null || true
  for i in $(seq 1 30); do
    if ! kill -0 "$pid" 2>/dev/null; then SERVER_PID=""; return 0; fi
    sleep 1
  done
  log "pid=$pid still alive after 30s -> SIGKILL"
  kill -9 "$pid" 2>/dev/null || true
  sleep 1
  if kill -0 "$pid" 2>/dev/null; then
    log "WARNING: pid=$pid survived SIGKILL (uninterruptible?); VRAM may stay allocated"
  fi
  SERVER_PID=""
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    log "cleanup: terminating server pid=$SERVER_PID that this script started"
    kill "$SERVER_PID" 2>/dev/null || true
  fi
  [ -n "$SANITIZED_COPY" ] && rm -f "$SANITIZED_COPY" 2>/dev/null
  exit "$rc"
}
trap cleanup EXIT INT TERM

tail_of_log() {
  local f="$1"
  [ -f "$f" ] || { echo "(no server log at $f)"; return 0; }
  tail -n 30 "$f" 2>/dev/null || echo "(could not read $f)"
}

write_error_json() {
  # $1 variant  $2 stage  $3 message  $4 detail  $5 server-log path (or "")
  local variant="$1" stage="$2" message="$3" detail="${4:-}" logf="${5:-}"
  ERR_OUT="$OUT_DIR/m4-quant-$variant.error.json" \
  ERR_VARIANT="$variant" ERR_STAGE="$stage" ERR_MESSAGE="$message" \
  ERR_DETAIL="$detail" ERR_LOG="$logf" ERR_HOST="$HOST" ERR_PORT="$PORT" \
  ERR_VLLM_BIN="$VLLM_BIN" ERR_MODELS_ROOT="$MODELS_ROOT" \
  "$HELPER_PY" - <<'PY' 2>/dev/null || echo "[bench] WARNING: could not write error json for $variant"
import json, os, time
def tail(path, n=30):
    if not path or not os.path.isfile(path):
        return []
    try:
        with open(path, "r", errors="replace") as fh:
            return [ln.rstrip("\n") for ln in fh.readlines()[-n:]]
    except OSError as exc:
        return ["<could not read %s: %s>" % (path, exc)]
obj = {
    "schema": "infergate.m4.quant-bench.error/1",
    "ok": False,
    "variant": os.environ["ERR_VARIANT"],
    "stage": os.environ["ERR_STAGE"],
    "error": os.environ["ERR_MESSAGE"],
    "detail": os.environ["ERR_DETAIL"],
    "server_log": os.environ["ERR_LOG"] or None,
    "server_log_tail": tail(os.environ["ERR_LOG"]),
    "host": os.environ["ERR_HOST"],
    "port": int(os.environ["ERR_PORT"]),
    "vllm_bin": os.environ["ERR_VLLM_BIN"],
    "models_root": os.environ["ERR_MODELS_ROOT"],
    "recorded_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
}
with open(os.environ["ERR_OUT"], "w") as fh:
    json.dump(obj, fh, indent=2, sort_keys=True)
    fh.write("\n")
print("[bench] wrote %s" % os.environ["ERR_OUT"])
PY
}

write_run_record() {
  # Orchestration facts for one variant (the client writes the measurements).
  RR_OUT="$OUT_DIR/m4-quant-$1.run.json" RR_VARIANT="$1" RR_OK="$2" \
  RR_STATUS="$3" RR_LOAD_S="$4" RR_GPU_BEFORE="$5" RR_GPU_AFTER="$6" \
  RR_GPU_PEAK="$7" RR_GPU_UNLOADED="$8" RR_VRAM_MB="$9" \
  RR_RESULT="${10}" RR_LOG="${11}" RR_CLIENT_OUT="${12}" RR_RC="${13}" \
  RR_VRAM_BEFORE="${14:-}" RR_VRAM_UNLOADED="${15:-}" \
  RR_HOST="$HOST" RR_PORT="$PORT" RR_SERVED_NAME="$SERVED_NAME" \
  RR_MAXLEN="$MAX_MODEL_LEN" RR_GPUUTIL="$GPU_UTIL" RR_BIN="$VLLM_BIN" \
  "$HELPER_PY" - <<'PY' 2>/dev/null || echo "[bench] WARNING: could not write run record for $RR_VARIANT"
import json, os, time
def num(name):
    v = os.environ.get(name, "")
    try:
        return float(v) if v not in ("", None) else None
    except ValueError:
        return None
def integer(name):
    v = os.environ.get(name, "")
    try:
        return int(v) if v not in ("", None) else None
    except ValueError:
        return None
# vram_used_after_load_mb is TOTAL GPU memory (it includes whatever the Windows
# desktop already holds), so the model's own footprint is the delta.
before = num("RR_VRAM_BEFORE")
after = num("RR_VRAM_MB")
unloaded = num("RR_VRAM_UNLOADED")
delta = None
released = None
if before is not None and after is not None:
    delta = round(after - before, 1)
if after is not None and unloaded is not None:
    released = round(after - unloaded, 1)
obj = {
    "schema": "infergate.m4.quant-bench.run/1",
    "variant": os.environ["RR_VARIANT"],
    "ok": os.environ["RR_OK"] == "1",
    "status": os.environ["RR_STATUS"],
    "load_seconds": num("RR_LOAD_S"),
    "gpu_before_load": os.environ["RR_GPU_BEFORE"] or None,
    "gpu_after_load": os.environ["RR_GPU_AFTER"] or None,
    "gpu_after_bench": os.environ["RR_GPU_PEAK"] or None,
    "gpu_after_unload": os.environ["RR_GPU_UNLOADED"] or None,
    "vram_used_after_load_mb": num("RR_VRAM_MB"),
    "vram_baseline_before_load_mb": before,
    "vram_delta_after_load_mb": delta,
    "vram_after_unload_mb": unloaded,
    "vram_released_mb": released,
    "vram_note": ("memory.used is TOTAL GPU memory: this GPU is shared with the "
                  "Windows desktop, which already holds its own share (~1.7-1.9 GiB "
                  "observed). The model's footprint is vram_delta_after_load_mb. "
                  "That desktop allocation drifts by tens of MiB between samples, so "
                  "a single-shot delta below ~50 MiB is measurement noise, not a model."),
    "result_json": os.environ["RR_RESULT"] or None,
    "client_log": os.environ["RR_CLIENT_OUT"] or None,
    "server_log": os.environ["RR_LOG"] or None,
    "client_exit_code": integer("RR_RC"),
    "serve": {
        "vllm_bin": os.environ["RR_BIN"],
        "host": os.environ["RR_HOST"],
        "port": int(os.environ["RR_PORT"]),
        "served_model_name": os.environ["RR_SERVED_NAME"],
        "max_model_len": int(os.environ["RR_MAXLEN"]),
        "gpu_memory_utilization": num("RR_GPUUTIL"),
    },
    "recorded_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
}
with open(os.environ["RR_OUT"], "w") as fh:
    json.dump(obj, fh, indent=2, sort_keys=True)
    fh.write("\n")
PY
}

summary_line() {
  # Compact one-liner: variant=.. ok=.. ttft_p50_ms=.. tok_per_s=..
  # vram_mb= is TOTAL GPU memory (it includes the Windows desktop's share) and
  # vram_delta_mb= is the loaded model's own footprint; they are kept separate.
  SUM_JSON="$OUT_DIR/m4-quant-$1.json" SUM_VARIANT="$1" SUM_OK="$2" \
  SUM_STATUS="$3" SUM_VRAM_MB="$4" SUM_LOAD_S="$5" SUM_VRAM_DELTA="${6:-}" \
  "$HELPER_PY" - <<'PY' 2>/dev/null || echo "variant=$1 ok=0 status=$3 (summary unavailable)"
import json, os
variant = os.environ["SUM_VARIANT"]
ok = os.environ["SUM_OK"] == "1"
status = os.environ["SUM_STATUS"]
vram = os.environ["SUM_VRAM_MB"]
load_s = os.environ["SUM_LOAD_S"]
vram_delta = os.environ.get("SUM_VRAM_DELTA", "")
doc = None
path = os.environ["SUM_JSON"]
if os.path.isfile(path):
    try:
        with open(path) as fh:
            doc = json.load(fh)
    except (OSError, ValueError):
        doc = None
def g(*keys, default=None):
    cur = doc
    for k in keys:
        if not isinstance(cur, dict) or k not in cur:
            return default
        cur = cur[k]
    return cur
def fmt(v, nd=1):
    if v is None:
        return "na"
    try:
        return ("%." + str(nd) + "f") % float(v)
    except (TypeError, ValueError):
        return "na"
parts = [
    "variant=%s" % variant,
    "ok=%d" % (1 if ok else 0),
    "status=%s" % status,
    "ttft_p50_ms=%s" % fmt(g("ttft_ms", "p50")),
    "tok_per_s=%s" % fmt(g("throughput", "output_tokens_per_s_request_wall")),
    "tok_per_s_wall=%s" % fmt(g("throughput", "output_tokens_per_s_wall_clock")),
    "req_per_s=%s" % fmt(g("throughput", "requests_per_s"), 3),
    "vram_mb=%s" % (vram if vram not in ("", None) else "na"),
    "vram_delta_mb=%s" % (vram_delta if vram_delta not in ("", None) else "na"),
    "load_s=%s" % (load_s if load_s not in ("", None) else "na"),
    "n=%s" % (g("n", default="na")),
    "errors=%s" % (g("errors", default="na")),
]
agr = g("agreement")
if isinstance(agr, dict):
    parts.append("exact_match=%s" % fmt(agr.get("exact_match_rate"), 3))
    parts.append("mean_token_overlap=%s" % fmt(agr.get("mean_token_overlap"), 3))
print(" ".join(parts))
PY
}

fail_variant() {
  # record an error object, print the summary line, update the tallies
  local variant="$1" stage="$2" message="$3" detail="${4:-}" logf="${5:-}"
  write_error_json "$variant" "$stage" "$message" "$detail" "$logf"
  local line
  line="$(summary_line "$variant" 0 "$stage" "" "")"
  echo "$line"
  RESULT_LINES+=("$line")
}

clear_stale_artifacts() {
  # Drop this variant's artifacts from any earlier run. Without this, a rerun
  # that fails can sit next to the previous run's measurements, and the summary
  # line would quote numbers this run never produced. Consequence (deliberate):
  # a failed <variant> run also discards that variant's older .texts.json, so a
  # missing fp16 reference is reported honestly instead of silently reused.
  local variant="$1" f
  for f in "$OUT_DIR/m4-quant-$variant.json" \
           "$OUT_DIR/m4-quant-$variant.texts.json" \
           "$OUT_DIR/m4-quant-$variant.run.json" \
           "$OUT_DIR/m4-quant-$variant.error.json"; do
    if [ -e "$f" ]; then
      rm -f "$f" 2>/dev/null && log "cleared stale artifact: $f"
    fi
  done
}

# ---------------------------------------------------------------------------
# one variant, start to finish, with unambiguous cleanup
# ---------------------------------------------------------------------------
run_variant() {
  local variant="$1"
  local model_dir="$MODELS_ROOT/$variant"
  local log="$OUT_DIR/vllm-$variant.log"
  local result_json="$OUT_DIR/m4-quant-$variant.json"
  local client_out="$OUT_DIR/bench-$variant.out"
  local gpu_before gpu_after gpu_peak gpu_unloaded
  local vram_before="" vram_after="" vram_delta="" vram_unloaded=""
  local load_seconds="" rc="" status ok ready_rc

  echo
  echo "=============================================================="
  log "variant=$variant  model_dir=$model_dir"
  log "port=$HOST:$PORT  max_model_len=$MAX_MODEL_LEN  gpu_util=$GPU_UTIL"
  echo "=============================================================="

  # A rerun must never mix this run's verdict with the previous run's numbers.
  clear_stale_artifacts "$variant"

  # --- 1. model directory must exist and be non-empty ---------------------
  if [ ! -d "$model_dir" ]; then
    fail_variant "$variant" "model_dir_missing" \
      "model directory $model_dir does not exist" \
      "nothing to serve; run the download/export step first" ""
    return 1
  fi
  if [ -z "$(ls -A "$model_dir" 2>/dev/null)" ]; then
    fail_variant "$variant" "model_dir_empty" \
      "model directory $model_dir is empty" \
      "a partially downloaded or aborted export leaves an empty dir" ""
    return 1
  fi

  # --- 2. the port must be free: never kill a foreign process -------------
  if port_is_busy; then
    fail_variant "$variant" "port_in_use" \
      "something is already listening on $HOST:$PORT" \
      "refusing to kill it; rerun with PORT=<free port> if it is not yours" ""
    return 1
  fi

  # --- 3. GPU state BEFORE load ------------------------------------------
  # NOTE: nvidia-smi memory.used is TOTAL memory in use, and on this laptop the
  # Windows desktop already holds ~1.7-1.9 GiB of the 8188 MiB. The loaded
  # model's own footprint is the DELTA, never the absolute reading.
  gpu_before="$(gpu_state)"
  vram_before="$(gpu_used_mb)"
  log "gpu before load: $gpu_before (used=${vram_before:-na} MiB)"

  # --- 4. start the server ------------------------------------------------
  # A cold load of these models takes tens of seconds (weights to VRAM, KV
  # cache allocation, torch.compile / CUDA-graph capture), and the FIRST
  # request additionally pays for graph capture and kernel autotuning. That is
  # exactly why the client issues warmup requests that are excluded from the
  # statistics.
  : >"$log" 2>/dev/null || true
  log "starting: $VLLM_BIN serve $model_dir (log: $log)"
  nohup "$VLLM_BIN" serve "$model_dir" \
      --served-model-name "$SERVED_NAME" \
      --host "$HOST" \
      --port "$PORT" \
      --max-model-len "$MAX_MODEL_LEN" \
      --gpu-memory-utilization "$GPU_UTIL" \
      "${SERVE_EXTRA_ARGS[@]}" \
      >"$log" 2>&1 &
  SERVER_PID=$!
  log "server pid=$SERVER_PID"

  # --- 5. wait until /v1/models answers 200 -------------------------------
  ready_rc=0
  load_seconds="$(wait_http_ok "$SERVER_PID")" || ready_rc=$?
  if [ "$ready_rc" -ne 0 ]; then
    local why="timed out after ${READY_TIMEOUT}s waiting for http://$HOST:$PORT/v1/models"
    if [ "$ready_rc" -eq 2 ]; then
      why="server process pid=$SERVER_PID exited during startup"
    fi
    echo "[bench] $variant NOT READY: $why" >&2
    echo "[bench] last 30 lines of $log:" >&2
    tail_of_log "$log" >&2
    stop_server
    fail_variant "$variant" "server_not_ready" "$why" \
      "waited ${load_seconds}s; see server_log_tail" "$log"
    return 1
  fi
  log "server ready after ${load_seconds}s"

  # --- 6. GPU state AFTER load (VRAM of the loaded model = headline) ------
  gpu_after="$(gpu_state)"
  vram_after="$(gpu_used_mb)"
  if [ -n "$vram_after" ] && [ -n "$vram_before" ]; then
    vram_delta=$((vram_after - vram_before))
  fi
  log "gpu after load: $gpu_after (used=${vram_after:-na} MiB, model delta=${vram_delta:-na} MiB)"

  # --- 7. run the measurement client --------------------------------------
  log "client: $HELPER_PY $CLIENT_PY ... --variant $variant (log: $client_out)"
  : >"$client_out" 2>/dev/null || true
  if [ ! -f "$CLIENT_PY" ]; then
    stop_server
    fail_variant "$variant" "client_missing" \
      "measurement client not found at $CLIENT_PY" "" "$log"
    return 1
  fi
  "$HELPER_PY" "$CLIENT_PY" \
      --base-url "http://$HOST:$PORT" \
      --model "$SERVED_NAME" \
      --variant "$variant" \
      --out "$result_json" \
      "${CLIENT_EXTRA_ARGS[@]}" 2>&1 | tee -a "$client_out"
  rc="${PIPESTATUS[0]}"
  log "client exit code=$rc"
  if [ "$rc" -ne 0 ]; then
    log "client failed; last 30 lines of $client_out:"
    tail -n 30 "$client_out" 2>/dev/null || true
  fi

  # --- 8. GPU at peak, then unload ---------------------------------------
  gpu_peak="$(gpu_state)"
  log "gpu after bench (peak-ish): $gpu_peak"
  stop_server
  sleep 2
  gpu_unloaded="$(gpu_state)"
  vram_unloaded="$(gpu_used_mb)"
  log "gpu after unload: $gpu_unloaded (used=${vram_unloaded:-na} MiB)"

  # --- 9. record + compact one-line summary ------------------------------
  status="ok"
  ok=1
  if [ "$rc" -ne 0 ]; then
    status="client_failed"
    ok=0
  fi
  if [ ! -s "$result_json" ]; then
    status="no_result_json"
    ok=0
  fi
  write_run_record "$variant" "$ok" "$status" "$load_seconds" \
    "$gpu_before" "$gpu_after" "$gpu_peak" "$gpu_unloaded" "$vram_after" \
    "$result_json" "$log" "$client_out" "$rc" "$vram_before" "$vram_unloaded"
  if [ "$ok" -eq 0 ]; then
    write_error_json "$variant" "bench_client" \
      "benchmark client exited with code $rc or wrote no result json" \
      "status=$status result=$result_json" "$log"
  fi
  local line
  line="$(summary_line "$variant" "$ok" "$status" "$vram_after" "$load_seconds" "$vram_delta")"
  echo "$line"
  RESULT_LINES+=("$line")
  if [ "$ok" -eq 1 ]; then ANY_OK=1; ALL_FAILED=0; fi
  [ "$ok" -eq 1 ]
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
mkdir -p "$OUT_DIR" 2>/dev/null || true

VARIANTS=("$@")
if [ "${#VARIANTS[@]}" -eq 0 ]; then
  VARIANTS=("${DEFAULT_VARIANTS[@]}")
fi

echo "=============================================================="
log "M4 quantization benchmark"
log "date=$(date -Is)  out_dir=$OUT_DIR"
log "variants=${VARIANTS[*]}"
log "gpu: $(gpu_state)"
echo "=============================================================="

# Preflight: warn about missing tools, but keep the per-variant error records
# authoritative (a missing binary simply fails every variant).
if [ ! -x "$VLLM_BIN" ]; then
  echo "[bench] WARNING: vLLM binary not executable at $VLLM_BIN (override with VLLM_BIN=...)" >&2
fi
if [ -z "$NVIDIA_SMI" ]; then
  echo "[bench] WARNING: nvidia-smi not found; VRAM numbers will be unavailable" >&2
fi
if [ ! -f "$CLIENT_PY" ]; then
  echo "[bench] WARNING: client not found at $CLIENT_PY" >&2
fi
if [ -z "$HELPER_PY" ] || [ ! -x "$HELPER_PY" ]; then
  echo "[bench] WARNING: no usable python interpreter (VLLM_PY=$VLLM_PY)" >&2
fi

for v in "${VARIANTS[@]}"; do
  # variant names become directory names, so keep them boring
  if ! printf '%s' "$v" | grep -qE '^[A-Za-z0-9][A-Za-z0-9_.-]*$'; then
    # Do NOT write an artifact here: the name is unusable as a filename
    # component (it may hold spaces, slashes or ".."), so there is no safe path
    # to write it to. Report it on the console and in the summary instead.
    echo "[bench] WARNING: variant name '$v' is not allowed (expected ^[A-Za-z0-9][A-Za-z0-9_.-]*\$); skipping" >&2
    badline="variant=$v ok=0 status=bad_variant_name ttft_p50_ms=na tok_per_s=na vram_mb=na load_s=na n=na errors=na"
    echo "$badline"
    RESULT_LINES+=("$badline")
    continue
  fi
  run_variant "$v" || true
done

echo
echo "=============================================================="
echo "M4 quantization benchmark -- final summary"
echo "  out_dir: $OUT_DIR"
echo "  gpu    : $(gpu_state)"
if [ "${#RESULT_LINES[@]}" -eq 0 ]; then
  echo "  (no variants were run)"
else
  for line in "${RESULT_LINES[@]}"; do
    echo "  $line"
  done
fi
echo "  artifacts: m4-quant-<variant>.json (measurements),"
echo "             m4-quant-<variant>.texts.json (full completions),"
echo "             m4-quant-<variant>.run.json (load time + GPU states),"
echo "             m4-quant-<variant>.error.json (only on failure),"
echo "             vllm-<variant>.log (server log), bench-<variant>.out (client log)"
echo "  REMINDER: 'agreement' is drift from fp16 text, NOT accuracy; single-stream"
echo "            sequential requests on a thermally-throttling 8 GB laptop GPU."
echo "            vram_mb= is TOTAL GPU memory (the Windows desktop holds its own"
echo "            ~1.7-1.9 GiB); vram_delta_mb= is the model's footprint, and its"
echo "            single-sample noise floor on this shared GPU is ~50 MiB."
echo "=============================================================="

if [ "$ALL_FAILED" -eq 1 ]; then
  echo "[bench] every variant failed"
  exit 1
fi
exit 0
