# Fails the curl-gates job when any gate in it failed.
#
# WHY THIS FILE EXISTS. Each of the nine gate steps carries
# `continue-on-error: true`, and GitHub documents the price of that flag exactly:
#
#   "The result of a completed step after continue-on-error is applied. ...
#    When a continue-on-error step fails, the outcome is failure, but the final
#    conclusion is success."
#   -- contexts reference, steps.<step_id>.outcome / .conclusion
#
# Those gates are 1068 curl assertions, 44 operator-token ones and the 2353
# in-process Go ones, and they are the only Windows coverage this repository has.
# With the flag on every gate step and no step that is allowed to fail, all nine
# could be red while the job, the run, the badge and the anonymous jobs API
# reported success -- and for a reader outside the repository the step conclusions
# are the only thing there is to read. The flag is still the right choice for the
# gate steps, because it is what makes one red run report every gate instead of
# stopping at the first. This step is the counterweight: it is
# deliberately the only gate-related step WITHOUT the flag, so it is the one that
# can fail the job.
#
# WHY A MARKER FILE, NOT AN ENV VAR OR A STEP OUTPUT. Environment variables do not
# travel between steps, and a step output has to be written to $GITHUB_OUTPUT by
# that step's own shell -- run-gate.ps1 runs as a child process, so it cannot set
# its parent's output either. A file in tmp\ is the only channel that survives
# from the last gate to this step.
#
# FAIL CLOSED. -Gates names the gates that must report. A missing marker is red,
# not "not applicable": the case it catches is a wrapper that died before it could
# write anything, a step that failed before reaching the wrapper at all, and a gate
# renamed on one side only. The expected list is written in ci.yml next to the
# steps it describes, and internal/repofmt pins the two lists to each other.
#
# WHY -Gates IS ONE STRING AND NOT A [string[]], AND WHY IT IS SPLIT ON COMMAS OR
# SPACES. ci.yml invokes this file the way every gate is invoked, `powershell -File
# ... -Gates m0,m1,...`, and what reaches $Gates depends on who built that command
# line: a parent that quoted the argument hands over "m0,m1" verbatim, while a
# caller that passed it bare -- Go's exec, which is what internal/repofmt does --
# ends up with PowerShell reading the comma as a list and joining the elements with
# the output field separator, "m0 m1". A [string[]] parameter does not help: in the
# quoted case -File binds the whole thing as one element, which is how the first
# version of this file came to check a single gate called "a,b". Splitting on both
# separators reads the list the same way whoever sent it, and the fail-closed rule
# still catches the case where the list arrives empty.
#
# WHY THIS PARAMETER IS NOT CALLED -Expect. It was, and that name made
# internal/repofmt's behavioural test hang: a PowerShell grandchild whose command
# line carries an argument starting with -Ex and whose stdout is redirected to a
# file comes back killed after exactly 30 seconds with an empty transcript, while
# the same command line as a direct child of the shell finishes in 0.2s. -Ex, -Expec,
# -Expect and a literal -ExecutionPolicy all reproduce it; -Gates, -Out and
# -MarkerDir do not. The runner is not affected (run 37410701867 ran this file with
# -Expect and passed in seconds), but a parameter whose name powershell.exe can read
# as an abbreviation of its own -ExecutionPolicy switch is not worth keeping.
#
# Usage:  scripts/lib/summarize-gates.ps1 -Gates m0,m1,m2,m3,m4,m5,m6,hardening,go-verify
param(
    [Parameter(Mandatory = $true)][string]$Gates,
    [string]$MarkerDir,
    [string]$LogDir,
    [string]$SummaryPath
)

$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
if (-not $MarkerDir) { $MarkerDir = Join-Path $repo 'tmp' }
if (-not $LogDir) { $LogDir = $MarkerDir }
if (-not $SummaryPath) { $SummaryPath = Join-Path $repo 'tmp\gate-failures.md' }

# WHY THIS LOCAL IS NOT CALLED $gates. PowerShell variable names are case
# insensitive, so `$gates = @($Gates -split ...)` assigns to the parameter it is
# reading from and silently keeps the unsplit value: the list then has one entry,
# "a b", and the step reports a gate nobody named. The name of this local is
# load-bearing, not cosmetic.
$gateList = @($Gates -split '[,\s]+' | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne '' })

$red = @()
$rows = @()
$body = New-Object System.Text.StringBuilder
[void]$body.AppendLine('# curl gates (M0-M6, operator token)')
[void]$body.AppendLine('')

# Fail closed: a job that names no gates is a job where this step proves nothing,
# and a green run from it would be a claim nobody checked. It still writes the
# summary, because "why is this red" is the one question the artifact has to
# answer -- the first version of this file exited before writing it.
$noGates = $gateList.Count -eq 0
if ($noGates) {
    [void]$body.AppendLine('## no gate was named')
    [void]$body.AppendLine('')
    [void]$body.AppendLine("-Gates '$Gates' named no gate, so this step verified nothing.")
    [void]$body.AppendLine('')
}

foreach ($name in $gateList) {
    $marker = Join-Path $MarkerDir "gate-$name.exit"
    $row = '{0,-12} ok' -f $name
    $reason = ''
    $code = 0

    if (-not (Test-Path -LiteralPath $marker)) {
        $reason = "no marker file at $marker -- the gate never reported"
    } else {
        $raw = ''
        try { $raw = (Get-Content -LiteralPath $marker -Raw).Trim() } catch { $raw = '' }
        if (-not [int]::TryParse($raw, [ref]$code)) {
            $reason = "unreadable marker content '$raw'"
        } elseif ($code -ne 0) {
            $reason = "exit $code"
        }
    }

    if ($reason -eq '') {
        $rows += $row
        continue
    }

    $red += $name
    $rows += ('{0,-12} RED   {1}' -f $name, $reason)
    Write-Host "::error title=gate $name::$reason"

    [void]$body.AppendLine("## gate $name : $reason")
    [void]$body.AppendLine('')

    $log = Join-Path $LogDir "$name.log"
    if (Test-Path -LiteralPath $log) {
        $all = @(Get-Content -LiteralPath $log)
        $fails = @($all | Where-Object { $_ -match '(?i)fail' } | Select-Object -First 20)
        if ($fails.Count -gt 0) {
            [void]$body.AppendLine('lines mentioning a failure:')
            [void]$body.AppendLine('')
            [void]$body.AppendLine('```')
            foreach ($line in $fails) { [void]$body.AppendLine($line) }
            [void]$body.AppendLine('```')
            [void]$body.AppendLine('')
        }
        $tail = @($all | Select-Object -Last 40)
        [void]$body.AppendLine("last $($tail.Count) line(s) of tmp\$name.log:")
        [void]$body.AppendLine('')
        [void]$body.AppendLine('```')
        foreach ($line in $tail) { [void]$body.AppendLine($line) }
        [void]$body.AppendLine('```')
        [void]$body.AppendLine('')
    } else {
        [void]$body.AppendLine("no transcript at $log")
        [void]$body.AppendLine('')
    }
}

Write-Host ""
foreach ($row in $rows) { Write-Host "  $row" }
Write-Host ""

$text = $body.ToString()
try {
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $SummaryPath) | Out-Null
    [System.IO.File]::WriteAllText($SummaryPath, $text, [System.Text.UTF8Encoding]::new($false))
    Write-Host "gate summary written to $SummaryPath"
} catch {
    # The summary is a convenience for the artifact; the exit code is the product.
    Write-Host "could not write $SummaryPath : $_"
}

if ($noGates) {
    Write-Host "::error title=gates not listed::-Gates '$Gates' named no gate"
    Write-Host "gates failed: 0 of 0 -- -Gates named no gate"
    exit 1
}

if ($red.Count -gt 0) {
    Write-Host "gates failed: $($red.Count) of $($gateList.Count) -- $($red -join ', ')"
    exit 1
}
Write-Host "gates passed: all $($gateList.Count) reported exit 0"
exit 0
