#!/usr/bin/env bash
# Give a red CI run's reason to a reader who is not signed in.
#
# WHY THIS FILE EXISTS, because three earlier attempts looked like fixes and were
# not:
#
#   The job log and the artifact download both answer 403 to anyone without admin
#   rights on this repository (verified anonymously against the public API). The
#   first attempt therefore piped each `go test` step through a `grep`, so the FAIL
#   lines landed in the step log -- which only helped readers who could already open
#   it, and the next failure arrived as the same one-line annotation.
#
#   `$GITHUB_STEP_SUMMARY` renders for a signed-in reader and does NOT reach
#   check-runs `output.summary`, so the anonymous API and the anonymous job page
#   both served nothing.
#
#   PATCHing the job's own check run returned 2xx and was then WIPED when the job
#   completed: on run 37377940124 a job that ran afterwards wrote output.summary and
#   read it back as null from outside.
#
# What does work -- measured on run 37379724217 -- is a check run this workflow
# CREATES with `POST /check-runs` rather than one GitHub creates for a job. Under a
# name nobody else owns, with its own output, it survives the job that made it and
# comes back through the plain anonymous API. That is this script.
#
# Publishing must never turn a green run red: every failure path here prints what
# went wrong and exits 0.
#
# Usage: ci-publish-failure-check.sh <title> <fragment.md> [<fragment.md> ...]
set -uo pipefail

title=${1:?usage: ci-publish-failure-check.sh <title> <fragment.md> [<fragment.md> ...]}
shift

if ! command -v gh >/dev/null 2>&1; then
    echo "gh is not on this runner, so the check run cannot be created; the summary is still in the step summary"
    exit 0
fi
if [ -z "${GH_TOKEN:-}" ]; then
    echo "GH_TOKEN is not set, so the check run cannot be created; the summary is still in the step summary"
    exit 0
fi

sha=${GITHUB_SHA:-}
if [ -z "$sha" ]; then
    echo "GITHUB_SHA is not set, so the check run cannot be created; the summary is still in the step summary"
    exit 0
fi

# The summary is capped and the overflow stays in the step summary and the artifact:
# a check-run output longer than this is not read by anybody anyway.
max_bytes=60000
body=/tmp/ci-failure-check.md
: > "$body"
for f in "$@"; do
    [ -s "$f" ] || continue
    cat "$f" >> "$body"
    printf '\n' >> "$body"
done
if [ ! -s "$body" ]; then
    echo "no non-empty fragment was given, so there is nothing to publish"
    exit 0
fi
if [ "$(wc -c < "$body")" -gt "$max_bytes" ]; then
    head -c "$max_bytes" "$body" > "${body}.truncated"
    printf '\n_Truncated to %s bytes; the step summary and the artifact have the rest._\n' "$max_bytes" >> "${body}.truncated"
    mv "${body}.truncated" "$body"
fi

server=${GITHUB_SERVER_URL:-https://github.com}
run_url="${server}/${GITHUB_REPOSITORY:-}/actions/runs/${GITHUB_RUN_ID:-}"
marker='<!-- infergate-ci-failure -->'
summary="$(cat "$body")

${marker}

[Run ${GITHUB_RUN_ID:-?}](${run_url}) -- the step summary has the same text, the artifact has the raw transcript."

# POST, not PATCH: patching the job's own check run is the variant that gets wiped.
# A check run created here is owned by this step and keeps its output.
resp="$(gh api --method POST "repos/${GITHUB_REPOSITORY}/check-runs" \
    -f name="${title}" \
    -f head_sha="${sha}" \
    -f status=completed \
    -f conclusion=failure \
    -f 'output[title]='"${title}" \
    --raw-field 'output[summary]='"${summary}" 2>&1)" && rc=0 || rc=$?
if [ "${rc:-0}" -ne 0 ]; then
    echo "could not create the check run (gh exited $rc); the summary is still in the step summary"
    printf '%s\n' "$resp" | head -n 5
    exit 0
fi

id="$(printf '%s' "$resp" | sed -n 's/.*"id": *\([0-9][0-9]*\).*/\1/p' | head -n 1)"
echo "published $(wc -c < "$body") bytes to check run ${id:-?} '${title}' on ${sha}"
exit 0
