# Runs the in-process Go acceptance gates (cmd/verify*) and leaves the same
# tmp\gate-<name>.exit marker the curl gates leave, so the one step in ci.yml that
# is allowed to fail can fail on these too.
#
# WHY THIS FILE EXISTS. cmd/verify* is 2353 assertions -- the larger of the two
# acceptance chains -- and it was the only one with no CI step at all. It needs no
# curl, no PowerShell and no spawned binary: it is a Go program that talks to the
# gateway in-process. Nothing about it is platform-specific; it simply had never
# been wired up. It runs in the Windows job because that job already exists, it
# already sets up Go, and the transcript has somewhere to go (the gate log
# artifact).
#
# WHY IT RUNS ALL SEVEN AND REPORTS AT THE END. The same reason the eight curl
# gates carry continue-on-error: a run that stops at the first red milestone
# cannot tell you whether the other six are red as well, and "one known failure"
# is a much cheaper thing to reason about than a queue of them.
#
# WHY -Package IS A COMMA-SEPARATED STRING AND NOT A [string[]]. ci.yml invokes
# this the way every gate is invoked, `powershell -File ... `, and in that form
# `-Package a,b` binds one literal element "a,b" -- it does not split on the
# comma. summarize-gates.ps1 learned this the same way: a fail-closed check
# reported the bogus gate name "a,b" instead of quietly checking nothing.
#
# Usage:  scripts/lib/run-go-verify.ps1 -Name go-verify -Package ./cmd/verify,./cmd/verify-m1
param(
    [Parameter(Mandatory = $true)][string]$Name,
    [Parameter(Mandatory = $true)][string]$Package,
    [string]$GoCmd = 'go'
)

$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$tmp = Join-Path $repo 'tmp'
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$logPath = Join-Path $tmp "$Name.log"
$markerPath = Join-Path $tmp "gate-$Name.exit"

$packages = @($Package -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne '' })

$code = 0
$passed = 0
$report = New-Object System.Text.StringBuilder

# Fail closed: a gate that was told to run nothing must not report success.
if ($packages.Count -eq 0) {
    [void]$report.AppendLine("no package was named, so this gate verified nothing")
    $code = 1
}

foreach ($pkg in $packages) {
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $out = & $GoCmd run $pkg 2>&1 | Out-String
    $rc = $LASTEXITCODE
    if ($null -eq $rc) { $rc = 0 }
    $sw.Stop()

    # The verdict line is the one that says how many assertions ran; it is also
    # what goes into the annotation, because "exit 1" alone tells a reader of the
    # checks page nothing. When the run died before printing one -- a package that
    # does not exist, a compile error -- the useful line is the first real line of
    # the output, not the last: PowerShell 5.1 appends its own NativeCommandError
    # noise (the "+ CategoryInfo" block) after the actual message.
    $verdict = ''
    foreach ($line in ($out -split "`n")) {
        if ($line -match 'RESULT:') { $verdict = $line.Trim() }
    }
    if (-not $verdict) {
        foreach ($line in ($out -split "`n")) {
            $t = $line.Trim()
            if ($t -eq '') { continue }
            if ($t -match '^(\+|At line:|CategoryInfo|FullyQualifiedErrorId|RemoteException|NativeCommandError)') { continue }
            $verdict = $t
            break
        }
    }

    [void]$report.AppendLine(('=' * 78))
    [void]$report.AppendLine(("{0}  exit={1}  {2:N1}s" -f $pkg, $rc, $sw.Elapsed.TotalSeconds))
    [void]$report.AppendLine(("  {0}" -f $verdict))
    [void]$report.AppendLine(('=' * 78))
    [void]$report.AppendLine($out.TrimEnd())
    [void]$report.AppendLine('')

    if ($rc -ne 0) {
        if ($code -eq 0) { $code = $rc }
        Write-Host "::error title=go gate $pkg::$verdict"
        Write-Host "$pkg exit=$rc  $verdict"
    } else {
        $passed++
        Write-Host "$pkg exit=0  $verdict"
    }
}

$text = $report.ToString()
[System.IO.File]::WriteAllText($logPath, $text, [System.Text.UTF8Encoding]::new($false))

# Written before the exit code is reported, and not swallowed: an absent marker
# counts as red in summarize-gates.ps1, which is the safe direction.
[System.IO.File]::WriteAllText($markerPath, "$code", [System.Text.UTF8Encoding]::new($false))

if ($code -ne 0) {
    Write-Host "go gates failed: $passed of $($packages.Count) milestone(s) passed, marker says $code"
} else {
    Write-Host "go gates passed: all $($packages.Count) milestone(s) reported exit 0"
}
exit $code
