#!/usr/bin/env python3
"""infergate -- M4 quantization benchmark client (stdlib only).

Measures, for ONE served model variant, the trade-off between latency,
throughput and memory on an OpenAI-compatible chat endpoint (vLLM), and stores
the completions so that output *agreement* with the fp16 reference can be
recomputed later.

Run by ``scripts/bench-vllm-quant.sh``; also runnable on its own::

    python3 scripts/bench_vllm_quant.py \
        --base-url http://127.0.0.1:8000 --model local-chat --variant awq \
        --out tmp/m4-quant-awq.json

Deliberately standard-library only (``urllib.request``, no requests/numpy), so
it also runs under the system python3 before the vLLM venv exists.

HONESTY / LIMITS -- read before quoting any number from the JSON:

* ``agreement`` is NOT accuracy. It measures how far this variant's text drifts
  from the fp16 text for the same prompt. It cannot say whether either answer
  is correct.
* Requests are issued SEQUENTIALLY (concurrency 1) with greedy decoding
  (temperature 0, fixed seed). The numbers are single-stream latency /
  throughput on a laptop GPU, not batched server throughput. A p95 over
  ~12 samples is a hint, not a distribution.
* Wall-clock timing on a laptop: the GPU is shared with the Windows desktop,
  clock-boost behaviour varies, and the card thermally throttles over a long
  run. Variants measured later in a session can therefore look slower for
  reasons that have nothing to do with quantization. Run variants
  back-to-back, repeat the whole script, and treat a single run as indicative.
* TTFT includes this client's own SSE parsing overhead (tiny, but non-zero).
* ``--timeout`` is a per-socket-operation timeout (the time allowed between two
  reads), not a total request budget.
* Warmup requests are excluded from every statistic; only their total wall time
  is reported (``warmup_s``), because the first request pays for CUDA-graph
  capture and kernel autotuning.
* The client is single-shot per prompt: one sample per (variant, prompt), so
  "exact_match_rate" over 12 prompts is coarse.
"""

import argparse
import hashlib
import http.client
import importlib
import json
import math
import os
import platform
import re
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request

SCHEMA = "infergate.m4.quant-bench/1"

# ---------------------------------------------------------------------------
# FROZEN prompt list.
# These 12 strings are intentionally embedded and never generated: all three
# variants must be compared on byte-identical inputs, so that any difference in
# the outputs can only come from the quantization. Do not edit them between
# runs -- if they change, previously recorded results are no longer comparable
# (prompts_sha256 in the output detects that).
#
# Layout: entries 0-5 are SHORT prompts (one line each), entries 6-11 are LONGER
# prompts (multi-constraint / code / reasoning / translation / table / formatting).
# Every prompt stays well under 400 estimated tokens. Note that entry 4 is by far
# the longest of the "short" group (~290 chars) because it has to quote the
# paragraph it summarises; the longer group spans roughly 280-360 chars.
# ---------------------------------------------------------------------------
PROMPTS = [
    # --- short (0-5) -------------------------------------------------------
    # 0: one-line factual question
    "What is the capital of Australia, and in which year did it become the capital? Answer in one line.",
    # 1: two-number arithmetic
    "Add 4873 and 6259, then also report the difference between the two numbers. One line, both results labelled.",
    # 2: name three things
    "Name three primary colours, and for each one give a common object that is usually that colour.",
    # 3: Chinese-language prompt
    "请用一句中文解释什么是模型量化（quantization），并给出一个日常生活中的类比。",
    # 4: one-sentence summary of a supplied short paragraph
    "Summarise the following paragraph in exactly one sentence: The city council approved a new tram line that will connect the northern suburbs to the central station. Construction is expected to take three years and cost 1.2 billion euros, with the first passengers expected on board in 2031.",
    # 5: JSON-output instruction
    'Return only a JSON object with the keys "city", "country" and "population_millions" for Canberra. No prose, no code fences.',
    # --- longer (6-11) -----------------------------------------------------
    # 6: multi-constraint instruction
    "Write a product description for a rechargeable desk lamp aimed at students. It must be at most 120 words, must mention the battery life in hours, must include exactly one price in euros, must avoid the words \"amazing\" and \"revolutionary\", and must end with a single call-to-action sentence.",
    # 7: short code-writing task
    "Write a Python function called chunk(items, size) that splits a list into consecutive sublists of at most size elements. It must not mutate the input, must return a list of lists, and must raise ValueError when size is less than 1. Follow the function with two short usage examples.",
    # 8: step-by-step reasoning task
    "A water tank holds 480 litres and drains at 12 litres per minute while a tap refills it at 5 litres per minute. Work out how long the tank takes to empty from full, showing each step on its own line, and state the assumption you make about the drain and the tap running at the same time.",
    # 9: translation task
    "Translate the following English paragraph into formal Simplified Chinese, keeping the technical terms accurate and the tone professional. Do not add any commentary before or after the translation: A quantized model stores its weights in a lower-precision format. This reduces memory use and often speeds up inference, but it can slightly change the output text.",
    # 10: table-formatting task
    "Produce a Markdown table comparing fp16, int8 and int4 weight formats. Use exactly three columns named Format, Bytes per weight and Typical use case, and one row per format. Fill the cells with concrete values, keep every cell under twelve words, and add nothing outside the table.",
    # 11: explicit "exactly N bullet points" constraint
    "List exactly 4 bullet points explaining when a smaller quantized model is a better choice than a larger full-precision one. Each bullet must be one sentence, must start with a bold keyword, and the answer must contain no introduction, no conclusion and no text outside the four bullets.",
]

PROMPTS_SHA256 = hashlib.sha256("\n".join(PROMPTS).encode("utf-8")).hexdigest()
SSE_DONE = "[DONE]"

# Exceptions that mean "the connection is not healthy yet", worth retrying
# during warmup (the server may still be compiling or binding the port).
RETRYABLE = (urllib.error.URLError, ConnectionError, TimeoutError, OSError,
             http.client.HTTPException)


# ---------------------------------------------------------------------------
# small utilities
# ---------------------------------------------------------------------------
def utc_now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def r3(value):
    """Round for a compact artifact; None stays None."""
    if value is None:
        return None
    try:
        return round(float(value), 3)
    except (TypeError, ValueError):
        return None


def percentile(values, pct):
    """Linear-interpolation percentile (numpy-free)."""
    vals = sorted(v for v in values if v is not None)
    if not vals:
        return None
    if len(vals) == 1:
        return float(vals[0])
    k = (len(vals) - 1) * (pct / 100.0)
    lo = math.floor(k)
    hi = math.ceil(k)
    if lo == hi:
        return float(vals[int(k)])
    return float(vals[lo] * (hi - k) + vals[hi] * (k - lo))


def stat_block(values):
    vals = [v for v in values if v is not None]
    if not vals:
        return {"n": 0, "mean": None, "p50": None, "p95": None,
                "min": None, "max": None}
    return {
        "n": len(vals),
        "mean": r3(statistics.mean(vals)),
        "p50": r3(percentile(vals, 50)),
        "p95": r3(percentile(vals, 95)),
        "min": r3(min(vals)),
        "max": r3(max(vals)),
    }


_WS_RE = re.compile(r"\s+")


def normalise_ws(text):
    """Whitespace-normalised text, for the exact-match comparison."""
    return _WS_RE.sub(" ", text or "").strip()


def word_tokens(text):
    """Lowercased whitespace tokens (a crude common token proxy)."""
    return set(re.findall(r"\S+", (text or "").lower()))


def jaccard(a, b):
    ta, tb = word_tokens(a), word_tokens(b)
    if not ta and not tb:
        return 1.0
    if not ta or not tb:
        return 0.0
    return len(ta & tb) / float(len(ta | tb))


def sha256_text(text):
    return hashlib.sha256((text or "").encode("utf-8")).hexdigest()


def texts_path_for(out_path):
    """m4-quant-awq.json -> m4-quant-awq.texts.json"""
    base = out_path[:-5] if out_path.endswith(".json") else out_path
    return base + ".texts.json"


def is_retryable(exc):
    if isinstance(exc, urllib.error.HTTPError):
        # a 4xx is a real client error; retrying will not help
        return exc.code >= 500
    return isinstance(exc, RETRYABLE)


# ---------------------------------------------------------------------------
# HTTP / SSE
# ---------------------------------------------------------------------------
def chat_url(base_url):
    return base_url.rstrip("/") + "/v1/chat/completions"


def build_payload(args, prompt, stream):
    payload = {
        "model": args.model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": 0,
        "max_tokens": args.max_tokens,
        "seed": args.seed,
    }
    if stream:
        payload["stream"] = True
        payload["stream_options"] = {"include_usage": True}
    else:
        payload["stream"] = False
    return payload


def http_post(url, payload, timeout):
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=data,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
            "User-Agent": "infergate-m4-bench/1 (+stdlib urllib)",
        },
    )
    return urllib.request.urlopen(req, timeout=timeout)


def _absorb_sse_data(data_str, state):
    """Feed one SSE `data:` payload into the running request state."""
    if data_str == SSE_DONE:
        state["done"] = True
        return
    try:
        obj = json.loads(data_str)
    except ValueError:
        state["malformed_lines"] += 1
        return
    if not isinstance(obj, dict):
        return
    usage = obj.get("usage")
    if isinstance(usage, dict):
        # the authoritative token counts; vLLM sends this in the final chunk
        state["usage"] = usage
    choices = obj.get("choices") or []
    if not choices:
        return
    choice = choices[0] or {}
    finish = choice.get("finish_reason")
    if finish:
        state["finish_reason"] = finish
    delta = choice.get("delta")
    if isinstance(delta, dict):
        content = delta.get("content")
        if content:
            if state["ttft_s"] is None:
                # TTFT = arrival of the first non-empty content delta
                state["ttft_s"] = time.perf_counter() - state["t0"]
            state["parts"].append(content)
            state["deltas"] += 1


def request_stream(url, payload, timeout):
    """POST with stream=true and measure TTFT while accumulating text."""
    state = {
        "t0": time.perf_counter(),
        "ttft_s": None,
        "parts": [],
        "usage": None,
        "finish_reason": None,
        "deltas": 0,
        "malformed_lines": 0,
        "done": False,
    }
    with http_post(url, payload, timeout) as resp:
        buf = b""
        read = getattr(resp, "read1", None)
        while not state["done"]:
            chunk = read(65536) if read is not None else resp.read(65536)
            if not chunk:
                break
            buf += chunk
            while b"\n" in buf:
                raw, buf = buf.split(b"\n", 1)
                line = raw.decode("utf-8", "replace").strip()
                if not line or line.startswith(":"):
                    continue          # comment / keep-alive
                if not line.startswith("data:"):
                    continue          # event:/id: lines carry no payload
                _absorb_sse_data(line[5:].strip(), state)
                if state["done"]:
                    break
        # a final line without a trailing newline
        if not state["done"] and buf.strip():
            line = buf.decode("utf-8", "replace").strip()
            if line.startswith("data:"):
                _absorb_sse_data(line[5:].strip(), state)
    total_s = time.perf_counter() - state["t0"]
    text = "".join(state["parts"])
    usage = state["usage"] if isinstance(state["usage"], dict) else {}
    completion_tokens = usage.get("completion_tokens")
    prompt_tokens = usage.get("prompt_tokens")
    usage_source = "api"
    if completion_tokens is None:
        # fall back to counting non-empty content deltas
        completion_tokens = state["deltas"]
        prompt_tokens = None
        usage_source = "estimated"
    return {
        "ttft_ms": None if state["ttft_s"] is None else state["ttft_s"] * 1000.0,
        "ttft_source": "first_content_delta",
        "total_ms": total_s * 1000.0,
        "text": text,
        "prompt_tokens": prompt_tokens,
        "completion_tokens": completion_tokens,
        "finish_reason": state["finish_reason"],
        "usage_source": usage_source,
        "malformed_lines": state["malformed_lines"],
    }


def request_nonstream(url, payload, timeout):
    """POST with stream=false: no TTFT is observable, only the total time."""
    t0 = time.perf_counter()
    with http_post(url, payload, timeout) as resp:
        body = resp.read()
    total_s = time.perf_counter() - t0
    obj = json.loads(body.decode("utf-8", "replace"))
    choices = obj.get("choices") or []
    text = ""
    finish = None
    if choices:
        choice = choices[0] or {}
        message = choice.get("message") or {}
        text = message.get("content") or ""
        finish = choice.get("finish_reason")
    usage = obj.get("usage") or {}
    completion_tokens = usage.get("completion_tokens")
    prompt_tokens = usage.get("prompt_tokens")
    usage_source = "api"
    if completion_tokens is None:
        completion_tokens = len(re.findall(r"\S+", text))
        prompt_tokens = None
        usage_source = "estimated"
    return {
        "ttft_ms": None,
        "ttft_source": "unavailable_non_stream",
        "total_ms": total_s * 1000.0,
        "text": text,
        "prompt_tokens": prompt_tokens,
        "completion_tokens": completion_tokens,
        "finish_reason": finish,
        "usage_source": usage_source,
        "malformed_lines": 0,
    }


def do_request(args, url, prompt):
    payload = build_payload(args, prompt, args.stream)
    if args.stream:
        return request_stream(url, payload, args.timeout)
    return request_nonstream(url, payload, args.timeout)


# ---------------------------------------------------------------------------
# environment facts (collected AFTER the measured phase, so that importing
# torch / calling nvidia-smi cannot perturb the measurement)
# ---------------------------------------------------------------------------
def gpu_facts():
    for exe in ("nvidia-smi", "/usr/lib/wsl/lib/nvidia-smi"):
        try:
            proc = subprocess.run(
                [exe, "--query-gpu=name,driver_version,memory.total",
                 "--format=csv,noheader"],
                capture_output=True, text=True, timeout=30,
            )
        except FileNotFoundError:
            last = "%s: not found" % exe
            continue
        except Exception as exc:                      # noqa: BLE001 - tolerate anything
            last = "%s: %s: %s" % (exe, type(exc).__name__, exc)
            continue
        if proc.returncode == 0:
            return {"available": True, "binary": exe,
                    "raw": (proc.stdout or "").strip()}
        last = "%s: rc=%s stderr=%s" % (exe, proc.returncode,
                                        (proc.stderr or "").strip()[:200])
    return {"available": False, "binary": None, "raw": None, "error": last}


def environment_facts():
    def pkg_version(name):
        try:
            mod = importlib.import_module(name)
        except Exception as exc:                      # noqa: BLE001 - optional deps
            return {"importable": False, "error": "%s: %s" % (type(exc).__name__, exc)}
        return {"importable": True,
                "version": getattr(mod, "__version__", "unknown")}

    return {
        "python_version": platform.python_version(),
        "python_executable": sys.executable,
        "platform": platform.platform(),
        "gpu": gpu_facts(),
        "torch": pkg_version("torch"),
        "vllm": pkg_version("vllm"),
    }


# ---------------------------------------------------------------------------
# agreement vs the fp16 reference texts
# ---------------------------------------------------------------------------
def load_reference(path):
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        doc = json.load(fh)
    by_index = {}
    for entry in doc.get("texts") or []:
        if isinstance(entry, dict) and "prompt_index" in entry:
            by_index[int(entry["prompt_index"])] = entry.get("text") or ""
    return doc, by_index


def agreement_block(args, records, own_texts_path):
    """Compare with the fp16 texts, or explain why that is impossible."""
    ref_path = args.reference
    if not ref_path:
        ref_path = os.path.join(os.path.dirname(os.path.abspath(args.out)),
                                "m4-quant-fp16.texts.json")

    if os.path.abspath(ref_path) == os.path.abspath(own_texts_path):
        return None, (
            "no reference: the reference path is this run's own texts file "
            "(%s); a variant compared with itself says nothing. This is what "
            "happens when --variant fp16 runs first." % ref_path
        )
    if not os.path.isfile(ref_path):
        return None, (
            "no reference: %s not found. Run the fp16 variant first; agreement "
            "cannot be computed without it, and this run may BE the fp16 "
            "reference." % ref_path
        )
    try:
        ref_doc, ref_texts = load_reference(ref_path)
    except (OSError, ValueError) as exc:
        return None, "no reference: could not read %s (%s)" % (ref_path, exc)

    exact = []
    overlaps = []
    chars = []
    ref_chars = []
    unmatched = 0
    for rec in records:
        if not rec.get("ok"):
            continue
        idx = rec.get("prompt_index")
        if idx not in ref_texts:
            unmatched += 1
            continue
        mine = rec.get("text", "")
        theirs = ref_texts[idx] or ""
        exact.append(1.0 if normalise_ws(mine) == normalise_ws(theirs) else 0.0)
        overlaps.append(jaccard(mine, theirs))
        chars.append(len(mine))
        ref_chars.append(len(theirs))
    if not exact:
        return None, (
            "no reference overlap: %s was readable but matched none of this "
            "run's prompt indices (%d unmatched)" % (ref_path, unmatched)
        )
    return {
        "reference_file": ref_path,
        "reference_variant": "fp16",
        "compared_requests": len(exact),
        "unmatched_requests": unmatched,
        "exact_match_rate": r3(statistics.mean(exact)),
        "mean_token_overlap": r3(statistics.mean(overlaps)),
        "mean_completion_chars": r3(statistics.mean(chars)),
        "mean_reference_completion_chars": r3(statistics.mean(ref_chars)),
        "exact_match_definition": "identical after whitespace normalisation",
        "token_overlap_definition": ("mean set-Jaccard over lowercased whitespace "
                                     "tokens: |A and B| / |A or B| with repeated "
                                     "words counted ONCE, not the model's tokenizer"),
        "caveat": ("agreement with fp16 text, NOT accuracy; one greedy sample per "
                   "prompt; a different but equally correct answer counts as drift"),
    }, None


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
def parse_args(argv=None):
    ap = argparse.ArgumentParser(
        description="M4 quantization benchmark client (stdlib only)",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    ap.add_argument("--base-url", required=True,
                    help="e.g. http://127.0.0.1:8000 (no /v1 suffix)")
    ap.add_argument("--model", required=True, help="served model name")
    ap.add_argument("--variant", required=True, help="fp16 | awq | gptq | ...")
    ap.add_argument("--out", required=True, help="result JSON path")
    ap.add_argument("--requests", type=int, default=12,
                    help="measured requests; prompts are cycled if larger than 12")
    ap.add_argument("--max-tokens", type=int, default=128)
    ap.add_argument("--warmup", type=int, default=2,
                    help="warmup requests, excluded from all statistics")
    ap.add_argument("--timeout", type=float, default=180.0,
                    help="per-socket-operation timeout in seconds")
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--stream", dest="stream", action="store_true", default=True,
                    help="use the SSE streaming protocol (default)")
    ap.add_argument("--no-stream", dest="stream", action="store_false",
                    help="non-streaming; TTFT is then unobservable (null)")
    ap.add_argument("--reference", default=None,
                    help="fp16 texts JSON to compare against "
                         "(default: <out dir>/m4-quant-fp16.texts.json)")
    ap.add_argument("--no-texts", dest="write_texts", action="store_false",
                    default=True,
                    help="do not write the sibling .texts.json with full texts")
    ap.add_argument("--warmup-retries", type=int, default=5,
                    help="attempts per warmup request while the server starts")
    ap.add_argument("--retry-sleep", type=float, default=5.0)
    return ap.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    url = chat_url(args.base_url)
    out_dir = os.path.dirname(os.path.abspath(args.out))
    if out_dir:
        os.makedirs(out_dir, exist_ok=True)
    own_texts_path = texts_path_for(os.path.abspath(args.out))
    prompt_count = len(PROMPTS)

    print("[client] variant=%s model=%s url=%s" % (args.variant, args.model, url))
    print("[client] prompts=%d requests=%d max_tokens=%d warmup=%d stream=%s"
          % (prompt_count, args.requests, args.max_tokens, args.warmup, args.stream))

    # --- warmup: excluded from statistics, wall time reported separately ----
    warmup_times = []
    warmup_errors = []
    t_warmup0 = time.perf_counter()
    for i in range(max(0, args.warmup)):
        prompt = PROMPTS[i % prompt_count]
        attempt = 0
        while True:
            attempt += 1
            try:
                res = do_request(args, url, prompt)
                warmup_times.append(res["total_ms"] / 1000.0)
                print("[client] warmup %d/%d ok in %.2fs (%s tokens, %s)"
                      % (i + 1, args.warmup, res["total_ms"] / 1000.0,
                         res["completion_tokens"], res["usage_source"]))
                break
            except Exception as exc:              # noqa: BLE001 - report, never abort
                if attempt < max(1, args.warmup_retries) and is_retryable(exc):
                    print("[client] warmup %d attempt %d failed (%s: %s); "
                          "retrying in %.1fs"
                          % (i + 1, attempt, type(exc).__name__, exc,
                             args.retry_sleep), file=sys.stderr)
                    time.sleep(args.retry_sleep)
                    continue
                warmup_errors.append({
                    "warmup_index": i,
                    "attempts": attempt,
                    "error": "%s: %s" % (type(exc).__name__, exc),
                })
                print("[client] warmup %d FAILED after %d attempt(s): %s: %s"
                      % (i + 1, attempt, type(exc).__name__, exc), file=sys.stderr)
                break
    warmup_s = time.perf_counter() - t_warmup0

    # --- measured phase ----------------------------------------------------
    records = []
    errors = 0
    t_phase0 = time.perf_counter()
    for i in range(max(0, args.requests)):
        idx = i % prompt_count
        prompt = PROMPTS[idx]
        rec = {
            "request_index": i,
            "prompt_index": idx,
            "ok": False,
            "ttft_ms": None,
            "ttft_source": None,
            "total_ms": None,
            "prompt_tokens": None,
            "completion_tokens": None,
            "completion_chars": None,
            "finish_reason": None,
            "text_sha256": None,
            "text_preview": None,
            "usage_source": None,
            "malformed_lines": None,
            "error": None,
        }
        try:
            res = do_request(args, url, prompt)
            text = res["text"] or ""
            rec.update({
                "ok": True,
                "ttft_ms": r3(res["ttft_ms"]),
                "ttft_source": res["ttft_source"],
                "total_ms": r3(res["total_ms"]),
                "prompt_tokens": res["prompt_tokens"],
                "completion_tokens": res["completion_tokens"],
                "completion_chars": len(text),
                "finish_reason": res["finish_reason"],
                "text_sha256": sha256_text(text),
                "text_preview": text[:160],
                "usage_source": res["usage_source"],
                "malformed_lines": res["malformed_lines"],
                "text": text,
            })
            print("[client] %2d/%d prompt=%d ok total=%7.1fms ttft=%s tokens=%s %s"
                  % (i + 1, args.requests, idx, res["total_ms"],
                     "n/a" if res["ttft_ms"] is None else "%.1fms" % res["ttft_ms"],
                     res["completion_tokens"], res["usage_source"]))
        except Exception as exc:                  # noqa: BLE001 - count and continue
            errors += 1
            rec["error"] = "%s: %s" % (type(exc).__name__, exc)
            print("[client] %2d/%d prompt=%d FAILED: %s"
                  % (i + 1, args.requests, idx, rec["error"]), file=sys.stderr)
        records.append(rec)
    wall_clock_s = time.perf_counter() - t_phase0

    # --- aggregate --------------------------------------------------------
    ok_records = [r for r in records if r["ok"]]
    ttfts = [r["ttft_ms"] for r in ok_records]
    totals = [r["total_ms"] for r in ok_records]
    summed_total_s = sum(r["total_ms"] for r in ok_records) / 1000.0
    summed_completion = sum(r["completion_tokens"] or 0 for r in ok_records)
    summed_prompt = sum(r["prompt_tokens"] or 0 for r in ok_records)
    completion_chars = [r["completion_chars"] or 0 for r in ok_records]

    def rate(num, denom):
        if not denom:
            return None
        return r3(num / float(denom))

    usage_sources = sorted({r["usage_source"] for r in ok_records
                            if r["usage_source"]})

    throughput = {
        "summed_completion_tokens": summed_completion,
        "summed_prompt_tokens": summed_prompt,
        "summed_request_wall_s": r3(summed_total_s),
        "wall_clock_s": r3(wall_clock_s),
        "output_tokens_per_s_request_wall": rate(summed_completion, summed_total_s),
        "output_tokens_per_s_wall_clock": rate(summed_completion, wall_clock_s),
        "requests_per_s": rate(len(ok_records), wall_clock_s),
        "note": ("output_tokens_per_s_request_wall divides by the summed "
                 "per-request wall time (excludes client/HTTP gaps); "
                 "output_tokens_per_s_wall_clock divides by the whole measured "
                 "wall clock including those gaps. Sequential requests only."),
    }

    agreement, agreement_note = agreement_block(args, records, own_texts_path)

    # --- full texts sidecar (for redoing the agreement analysis later) ------
    texts_written = None
    if args.write_texts:
        texts_doc = {
            "schema": SCHEMA + ".texts",
            "variant": args.variant,
            "model": args.model,
            "base_url": args.base_url,
            "created_utc": utc_now(),
            "prompts_sha256": PROMPTS_SHA256,
            "prompts": PROMPTS,
            "temperature": 0,
            "seed": args.seed,
            "max_tokens": args.max_tokens,
            "stream": args.stream,
            "texts": [
                {
                    "prompt_index": r["prompt_index"],
                    "request_index": r["request_index"],
                    "text": r.get("text", ""),
                    "completion_tokens": r["completion_tokens"],
                    "finish_reason": r["finish_reason"],
                    "text_sha256": r["text_sha256"],
                }
                for r in ok_records
            ],
        }
        try:
            with open(own_texts_path, "w", encoding="utf-8") as fh:
                json.dump(texts_doc, fh, indent=2, sort_keys=True)
                fh.write("\n")
            texts_written = own_texts_path
        except OSError as exc:
            print("[client] WARNING: could not write %s (%s)"
                  % (own_texts_path, exc), file=sys.stderr)

    # strip the bulky raw text from the per-request records: only the preview
    # and the hash belong in the main artifact
    public_records = []
    for rec in records:
        slim = {k: v for k, v in rec.items() if k != "text"}
        public_records.append(slim)

    result = {
        "schema": SCHEMA,
        "variant": args.variant,
        "model": args.model,
        "base_url": args.base_url,
        "created_utc": utc_now(),
        "protocol": {
            "endpoint": "/v1/chat/completions",
            "stream": args.stream,
            "temperature": 0,
            "seed": args.seed,
            "max_tokens": args.max_tokens,
            "warmup_requests": max(0, args.warmup),
            "concurrency": 1,
            "sequential": True,
        },
        "prompts_sha256": PROMPTS_SHA256,
        "prompt_count": prompt_count,
        "n": len(ok_records),
        "requests_attempted": len(records),
        "errors": len(records) - len(ok_records),
        "error_details": [
            {"request_index": r["request_index"], "prompt_index": r["prompt_index"],
             "error": r["error"]}
            for r in records if not r["ok"]
        ],
        "usage_source": ("none" if not usage_sources
                         else usage_sources[0] if len(usage_sources) == 1
                         else "mixed"),
        "ttft_ms": stat_block(ttfts),
        "total_ms": stat_block(totals),
        "completion_chars": stat_block(completion_chars),
        "throughput": throughput,
        "warmup_s": r3(warmup_s),
        "warmup_requests": max(0, args.warmup),
        "warmup_request_times_s": [r3(t) for t in warmup_times],
        "warmup_errors": warmup_errors,
        "agreement": agreement,
        "agreement_note": agreement_note,
        "texts_file": texts_written,
        "mean_completion_chars_over_ok_requests": r3(
            statistics.mean(completion_chars)) if completion_chars else None,
        "environment": environment_facts(),
        "requests": public_records,
        "caveats": [
            "agreement != accuracy",
            "sequential single-stream measurement (concurrency 1)",
            "greedy decoding, one sample per prompt",
            "laptop GPU: shared clocks, thermal throttling, single-run variance",
            "TTFT is measured client-side and includes SSE parsing overhead",
        ],
    }

    with open(args.out, "w", encoding="utf-8") as fh:
        json.dump(result, fh, indent=2, sort_keys=True)
        fh.write("\n")

    t = result["ttft_ms"]
    thr = result["throughput"]
    if agreement:
        agr_txt = "exact_match=%s mean_token_overlap=%s" % (
            agreement["exact_match_rate"], agreement["mean_token_overlap"])
    else:
        agr_txt = "agreement=null"
    print("SUMMARY variant=%s n=%d errors=%d ttft_p50_ms=%s total_p50_ms=%s "
          "tok_s_request_wall=%s tok_s_wall_clock=%s req_s=%s %s out=%s%s"
          % (args.variant, result["n"], result["errors"], t["p50"],
             result["total_ms"]["p50"],
             thr["output_tokens_per_s_request_wall"],
             thr["output_tokens_per_s_wall_clock"], thr["requests_per_s"],
             agr_txt, args.out,
             "" if texts_written else " (no texts file)"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
