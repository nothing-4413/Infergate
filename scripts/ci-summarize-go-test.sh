#!/usr/bin/env bash
# Write the interesting parts of a `go test` transcript into a job's step summary.
#
# WHY THIS EXISTS. A red CI step is useless if its transcript cannot be read. Job
# logs need admin rights on this repository, and its only reader is an
# unauthenticated client that gets 403 on them, so the first `go test -race`
# failure on Linux arrived as nothing but "Process completed with exit code 1" --
# twice, the second time after a `grep` had been added precisely to fix it. The
# grep only helped readers who were allowed to open the log.
#
# The step summary is what a reader sees without downloading anything, and it is
# where this text has to live. Two better-sounding channels were measured and are
# closed: the summary does not populate check-runs `output.summary`, and a check run
# PATCHed by the workflow is wiped when the job completes (run 37377940124). A sign-in
# is therefore required to read the failing test names -- say so in the docs rather
# than claiming otherwise a third time.
#
# The whole DATA RACE report is kept, not just its banner line. A race is only
# actionable as the pair of accesses that raced plus the state they shared, and
# that is exactly the part a line-matching filter throws away.
#
# The same argument applies to an ordinary failure, whose reason is the indented
# line under the FAIL: "script_test.go:455: want a transport error, got status
# 200". A filter on the FAIL line alone publishes which test broke and not why,
# which is what run 37414903323 published -- the reader could see
# TestDropClosesWithoutAResponse fail at 0.00s and nothing else. Both shapes are
# kept now.
#
# Usage: ci-summarize-go-test.sh <logfile> <summaryfile> <title>
set -uo pipefail

log=${1:?usage: ci-summarize-go-test.sh <logfile> <summaryfile> <title>}
summary_file=${2:?usage: ci-summarize-go-test.sh <logfile> <summaryfile> <title>}
title=${3:-go test}

{
    echo "## ${title}"
    echo
    if [ ! -f "$log" ]; then
        echo "The transcript \`${log}\` was never written -- the step died before"
        echo "it produced output. Read the step log for the reason."
        exit 0
    fi

    # Passing/failing tests and package lines, in go test's own vocabulary:
    # `--- FAIL`, `    --- FAIL` for subtests, `^FAIL`, `^ok`, `=== RUN`, `=== CONT`.
    #
    # A report starts at the detector's banner or a panic. It ends at the
    # `==================` rule, NOT at the first blank line: the frames below a
    # blank line ("Goroutine 42 (running) created at:") are the part that says which
    # code created the racing goroutine, and stopping early drops exactly the
    # evidence a race report exists to carry. A panic has no rule, so a blank line
    # ends that block, and the two flags keep the single blank the detector itself
    # prints between the banner and the frames from ending it.
    #
    # `file.go:NNN:` is the reason line go test prints under a FAIL (and under
    # t.Log). It is matched last, after the report block, so it cannot pull an
    # indented frame back in after a report has ended.
    matched=$(
        awk '
            /WARNING: DATA RACE|^DATA RACE/ || /^panic:|panic: / {
                inside = 1; blank = 0; print; next
            }
            /^=+$/ { if (inside) { print; inside = 0; blank = 0; next } }
            inside && /^[[:space:]]*$/ {
                if (blank) { inside = 0 }
                blank = 1; print; next
            }
            inside { blank = 0; print; next }
            /^(FAIL|ok|===|---)[[:space:]]/ || /[[:space:]]--- / { print; next }
            /^[[:space:]]+[A-Za-z0-9_][A-Za-z0-9_./-]*\.go:[0-9]+:/ { print }
        ' "$log"
    )

    if [ -z "$matched" ]; then
        echo "No failing test, race report, or panic in \`${log}\`."
        echo
        echo "The step still exited non-zero, which points at the harness rather"
        echo "than at a test; read the step log."
        exit 0
    fi

    echo '```'
    # Cap it: a race report runs long, and a summary longer than the log it
    # summarizes is not a summary. 400 lines is several reports.
    printf '%s\n' "$matched" | head -n 400
    echo '```'

    total=$(printf '%s\n' "$matched" | wc -l | tr -d ' ')
    if [ "$total" -gt 400 ]; then
        echo
        echo "_Truncated: ${total} lines matched; the uploaded artifact has the rest._"
    fi
} >> "$summary_file"
