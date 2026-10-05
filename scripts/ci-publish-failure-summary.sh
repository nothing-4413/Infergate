#!/usr/bin/env bash
# Publish a job's step summaries into the check run's `output.summary`.
#
# WHY THIS EXISTS, AND WHY IT IS NOT JUST $GITHUB_STEP_SUMMARY. Writing the step
# summary is the documented way to make a run explain itself, and it does render in
# the Actions UI -- for a reader who is signed in. What it does NOT do is populate
# check-runs `output.summary`: on the run that added it, both the check-run API and
# the job page HTML served to an anonymous client showed nothing, because the step
# summary lives in a separate store behind the session. The job log and the artifact
# have the same problem (`GET /actions/jobs/{id}/logs` needs admin rights on this
# repository, and it is 403 for everyone else).
#
# `PATCH /check-runs/{id}` with `output.summary` is the one place a red run can put
# its reason that comes back over the plain public API -- so that is where the
# failure output goes, and the step summary is written as well for the UI.
#
# Usage: ci-publish-failure-summary.sh <dir-with-per-step-summaries> <job-name> [publish|digest]
#
# publish (the default) PATCHes the check run: use it for a job whose summaries
# explain a failure. digest just prints the first few KB to the step log: use it for
# a job where the first red step stops everything, so a green job has nothing to
# explain and only the package lines to show.
set -uo pipefail
dir=${1:?usage: ci-publish-failure-summary.sh <dir> <job-name> [publish|digest]}
job=${2:?usage: ci-publish-failure-summary.sh <dir> <job-name> [publish|digest]}
mode=${3:-publish}

combined=/tmp/ci-published-summary.md
truncated=/tmp/ci-published-summary-truncated.md
rm -f "$combined" "$truncated"

# The aggregating steps write one file per step, named after it. Order matters to a
# reader (test before race), so it is by prefix rather than directory order. A step
# that ran again overwrites its file, so the newest version of it wins.
for part in vet gofmt test race; do
    if [ -s "$dir/${part}.md" ]; then
        # The fragment's own "## <title>" heading is the reader-facing label, so no
        # extra one is added here.
        cat "$dir/${part}.md" >> "$combined"
        printf '\n' >> "$combined"
    fi
done

if [ ! -s "$combined" ]; then
    echo "no step summary was written; nothing to publish"
    exit 0
fi

if [ "$mode" != "publish" ]; then
    # A green job needs no explanation, and a reader who wants one can open the
    # artifact. The digest still goes to the step log so the pipeline is visible.
    head -c 4000 "$combined"
    exit 0
fi

# A checks API payload is not the place for a megabyte of Go transcript. The
# summary is meant to name the failure, and the artifact keeps the whole thing.
# head -c cuts by bytes, so a multi-byte character can be split; that is harmless
# here because the tail is replaced by a note.
if [ "$(wc -c < "$combined")" -gt 60000 ]; then
    head -c 60000 "$combined" > "$truncated"
    printf '\n\n_Truncated to 60000 bytes; the uploaded artifact has the full transcript._\n' \
        >> "$truncated"
else
    cp "$combined" "$truncated"
fi

# This step publishes; it does not gate. Everything below therefore reports and
# exits 0 rather than failing the job -- a broken publisher must not be able to turn
# a passing test run red.
if ! command -v gh > /dev/null; then
    echo "gh is not on this runner, so the check run cannot be updated; the"
    echo "failure output is still in the step summary and the artifact ($(wc -c < "$combined") bytes)"
    exit 0
fi

# The check run for THIS job. head_sha narrows it to this commit; if the API returns
# more than one match, the name breaks the tie. The two variables are defaulted
# because `set -u` would otherwise abort on a missing one.
repo=${GITHUB_REPOSITORY:-}
sha=${GITHUB_SHA:-}
id=$(gh api "repos/${repo}/commits/${sha}/check-runs" \
    --jq ".check_runs[] | select(.name == \"${job}\") | .id" | head -n 1)
if [ -z "$id" ]; then
    echo "could not find the check run named '${job}' for ${sha}; the step"
    echo "summary is still in the job UI ($(wc -c < "$combined") bytes)"
    # This step's stdout is not readable without admin rights either, so put the
    # facts where they are: an annotation on the check run, which the public API
    # does serve. The first real run of this publisher reported success and left
    # output.summary empty, and there was no way to tell whether it had published
    # at all -- this is that way.
    diag="repo=${repo} sha=${sha} job=${job} token=${GITHUB_TOKEN:+set}"
    echo "diag: $diag"
    gh api --method POST "repos/${repo}/check-runs/${id}/annotations" \
        -f "path=.github/workflows/ci.yml" \
        -f "start_line=1" -f "end_line=1" \
        -f "annotation_level=warning" \
        -f "message=publisher did not find its check run (${diag})" > /dev/null || true
    exit 0
fi

if command -v jq > /dev/null; then
    # jq is the reliable way to put a multi-megabyte, quote-riddled Go stack trace
    # into JSON. It is NOT a given: this publisher first ran green on ubuntu-24.04
    # without it, because the emptiness check above only looked for gh.
    jq -n --rawfile summary "$truncated" --arg title "${job}" \
        '{output: {title: $title, summary: $summary}}' \
        | gh api --method PATCH "repos/${repo}/check-runs/${id}" --input - > /dev/null
else
    # gh builds the JSON itself here. --raw-field handles newlines; it does not
    # escape, so a transcript that jq would have encoded is still sent intact.
    gh api --method PATCH "repos/${repo}/check-runs/${id}" \
        -f "output[title]=${job}" \
        --raw-field "output[summary]=$(cat "$truncated")" > /dev/null
fi

echo "published $(wc -c < "$truncated") bytes to check run ${id} (${job})"
