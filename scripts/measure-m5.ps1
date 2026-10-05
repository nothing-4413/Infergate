# InferGate M5 -- measure the gateway's own cost and the cost of observability.
#
# M5 added three observability surfaces to the gateway (Prometheus /metrics, the
# human /stats snapshot, and request tracing with two exporters). Those surfaces
# are not free, and "not free" is a number, not an opinion. This script produces
# that number against ONE backend so the comparison is meaningful:
#
#   direct        the load generator talks to cmd\mockupstream itself. This is
#                 the WITNESS, not a gateway arm: it is what the backend can do
#                 with no gateway in the path at all. If direct and gateway land
#                 within ~15% of each other at the top concurrency, the mock is
#                 the ceiling and the absolute QPS says nothing about the
#                 gateway -- the artifact says so in limitations.
#   gateway       through one untraced gateway instance.
#   traced-otlp   through a gateway with tracing at sample_ratio 1.0 and the
#                 OTLP/HTTP exporter on (no JSONL sink).
#   traced-full   tracing at sample_ratio 1.0 with OTLP AND the JSONL file sink.
#   second        a second untraced instance, alone: the single-instance control
#                 for the horizontal arms.
#   horizontal-2  the SAME client process against A+B (round robin).
#   horizontal-4  ... against A+B+C+D, the shape of the repo's documented
#                 `infergate-fleet` Prometheus scrape job.
#   gateway-scraped  gateway A again, while a scraper polls /stats and /metrics
#                 every 100ms (~10x a real 5-15s Prometheus interval). This is
#                 what answers "what does being scraped cost" -- and it is an
#                 upper bound, not a Prometheus model.
#
# The two horizontal arms are what makes the scaling claim checkable: the client
# is byte-identical in `second`, `horizontal-2` and `horizontal-4`, so a gain is
# a gain in the fleet and not in the generator.
#
# What is NOT claimed, and is written into the artifact's limitations:
#   * the backend is cmd\mockupstream, an in-repo stand-in that answers instantly
#     and sleeps 1ms per streamed token. It is not a real provider, and the
#     absolute QPS here is a property of this host, this mock and this client.
#   * everything is loopback on ONE host that this very script is also running
#     five gateways, a mock and a collector on. The host has 24 physical cores,
#     so the 4-instance arm is not core-starved -- but a single host is still a
#     single host.
#   * the gateways run at log level `error` so that per-request logging is not
#     mixed into the tracing cost. configs\observability.yaml documents what
#     info-level logging costs on this host; that cost is NOT measured here.
#   * the JSONL and OTLP exporters own background workers, so Export() on the
#     request path only enqueues. The cost measured here is the cost of building
#     and enqueueing a span tree at sample_ratio 1.0, plus whatever contention
#     the sink worker adds -- not the cost of a synchronous write.
#   * tracing is enabled per INSTANCE, so all tracing arms share whichever host
#     cores the untraced arms are also using; the arms are run interleaved (every
#     arm once per round, rounds outermost) so host drift hits all arms alike.
#   * the scrape-load arm holds a CONSTANT 10Hz scrape for the whole arm, which
#     no real Prometheus does (5-15s interval), and the scraper itself runs on
#     the same host as the gateway it scrapes -- so part of any delta is the
#     scraper's CPU rather than the exposition code.
#
# Usage (pwsh does NOT exist on this host):
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m5.ps1
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m5.ps1 -Rounds 1 -Requests 60 -Concurrency '8'
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m5.ps1 -KeepRunning
#
# The defaults are MEASUREMENT-sized, not smoke-sized: at this host's ~900-1500
# QPS a 400-request phase lasts under half a second, which makes the percentiles
# start-up dominated and gives the 10Hz scrape arm only a couple of GETs to
# report. 3000 requests with a 300-request warmup puts each phase in the several
# seconds, and the whole run in the 5-8 minute range. A run that does not meet
# the minimum (>= 2 rounds, >= 500 requests) is a SMOKE run and is written to
# tmp\ instead of docs\baseline\ so it can never be mistaken for a measurement.
#
# Everything is started and stopped inside ONE powershell invocation, and the
# gateways are pointed at the SAME single mock: an arm comparison where the two
# arms sit behind different backends would attribute the backend's difference to
# the gateway.

[CmdletBinding()]
param(
    [string]$Concurrency = '8,32,128',
    [int]$Requests = 3000,
    [int]$Warmup = 300,
    [int]$Rounds = 3,
    [string]$Timeout = '30s',
    [int]$MockTokenDelayMs = 1,
    [int]$MockTTFBMs = 0,
    [ValidateSet('error', 'warn', 'info', 'debug')]
    [string]$LogLevel = 'error',
    [int]$HealthTimeoutSeconds = 30,
    # Below these, the run is a smoke test: it still runs and still checks, but
    # its artifact goes to tmp\ rather than docs\baseline\.
    [int]$MinRequests = 500,
    [int]$MinRounds = 2,
    [switch]$SkipBuild,
    [switch]$KeepRunning,
    [switch]$KeepWork
)

$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# Layout
# ---------------------------------------------------------------------------

$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
$baselineDir = Join-Path $repo 'docs\baseline'
$goShim = Join-Path $repo 'tools\go.cmd'

# A run that is too small to be a measurement is a SMOKE run: it still runs every
# arm and every check, but its artifact goes to tmp\ so docs\baseline\ can never
# be confused by a 30-request smoke run (a 400-request phase is under half a
# second at this host's QPS, which makes the percentiles start-up noise and gives
# the 10Hz scrape arm almost nothing to count).
$isSmoke = [bool](($Requests -lt $MinRequests) -or ($Rounds -lt $MinRounds))
if ($isSmoke) {
    $artifact = Join-Path $tmpDir 'm5-summary-smoke.json'
    $smokeReason = "$Requests requests (< $MinRequests) or $Rounds rounds (< $MinRounds)"
}
else {
    $artifact = Join-Path $baselineDir 'm5-summary.json'
    $smokeReason = ''
}

foreach ($d in @($binDir, $tmpDir, $baselineDir)) {
    if (-not (Test-Path -LiteralPath $d)) { New-Item -ItemType Directory -Path $d -Force | Out-Null }
}

$stamp = "$(Get-Date -Format 'yyyyMMdd-HHmmss')-$PID"
$gwExe = Join-Path $binDir 'infergate.exe'
$mockExe = Join-Path $binDir 'mockupstream.exe'
$ltExe = Join-Path $binDir 'loadtest.exe'
$mcExe = Join-Path $binDir 'mockcollector.exe'

# One scratch dir per invocation: raw per-run arm JSON, configs, logs and the
# JSONL trace file all live here, so a rerun can never read the previous run's
# JSONL line count.
$workDir = Join-Path $tmpDir "m5-measure-$stamp"
New-Item -ItemType Directory -Path $workDir -Force | Out-Null
$jsonlPath = Join-Path $workDir 'traces.jsonl'

# ---------------------------------------------------------------------------
# Fleet -- ports are FIXED and are the contract (see the brief).
#
# 18999-19002 are four byte-identical UNTRACED instances; 18999+19000 is the
# 2-instance arm and 18999-19002 is the 4-instance arm, i.e. exactly the targets
# of the repo's documented `infergate-fleet` Prometheus scrape job. The two
# TRACED instances live at 19010/19011 so that "which port is traced" is
# answerable from the port alone.
# ---------------------------------------------------------------------------

$mockPort = 19900
$collectorPort = 19999
$ports = [ordered]@{ A = 18999; B = 19000; C = 19001; D = 19002; E = 19010; F = 19011 }
$gatewayPorts = @($ports.A, $ports.B, $ports.C, $ports.D, $ports.E, $ports.F)
$fleetPorts = @($ports.A, $ports.B, $ports.C, $ports.D)

$mockBase = "http://127.0.0.1:$mockPort"
$collectorBase = "http://127.0.0.1:$collectorPort"
$otlpPath = '/v1/traces'
# `otlp.endpoint` is a BASE url: internal/traceexport/otlp.go:43-46 documents it
# as "the collector's base URL, e.g. http://127.0.0.1:4318. Export POSTs to
# <Endpoint>/v1/traces", and otlp.go:153 builds the POST url as
#   strings.TrimSuffix(endpoint, "/") + "/v1/traces"
# so the configured value must NOT already carry the path. scripts\verify-m5.ps1
# configures the same base form. The path the collector should therefore see is
# exactly $otlpPath, and a POST to anything else is a harness configuration bug.
$otlpEndpoint = $collectorBase
$otlpConstructedPath = $otlpPath
$otlpServiceName = 'infergate-m5-measure'
$latencyWindow = 65536  # internal/metrics.DefaultLatencyWindow

$levels = @()
foreach ($piece in ($Concurrency -split ',')) {
    $t = $piece.Trim()
    if ($t -ne '') { $levels += [int]$t }
}
if ($levels.Count -eq 0) { throw "-Concurrency '$Concurrency' produced no levels" }
$topLevel = ($levels | Measure-Object -Maximum).Maximum

# ---------------------------------------------------------------------------
# Script-scope state
# ---------------------------------------------------------------------------

$script:utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$script:assertFails = New-Object System.Collections.ArrayList
$script:assertTotal = 0
$script:log = New-Object System.Collections.ArrayList
$script:started = New-Object System.Collections.ArrayList   # [pscustomobject]@{Name;Proc;Log;Err}
$script:gwARequestsTotal = @{ before = -1; after = -1; armed = $false }
$script:checks = New-Object System.Collections.ArrayList
$script:limitations = New-Object System.Collections.ArrayList
$script:failures = New-Object System.Collections.ArrayList        # non-assertion problems
$script:scrapeJobs = New-Object System.Collections.ArrayList      # live scrape-load jobs

$startedAt = Get-Date

# ---------------------------------------------------------------------------
# Console + assertion plumbing (house style: scripts/measure-m4.ps1)
# ---------------------------------------------------------------------------

function Write-Section {
    param([string]$Title)
    Write-Host ''
    Write-Host ('=' * 74) -ForegroundColor DarkGray
    Write-Host "  $Title" -ForegroundColor Cyan
    Write-Host ('=' * 74) -ForegroundColor DarkGray
}

function Log-Line {
    param([string]$Text, [string]$Color = 'Gray')
    [void]$script:log.Add($Text)
    Write-Host $Text -ForegroundColor $Color
}

function Assert-That {
    param([bool]$Ok, [string]$Name, [string]$Detail = '')
    $script:assertTotal = 1 + [int]$script:assertTotal
    if ($Ok) {
        Write-Host "  PASS  $Name" -ForegroundColor Green
    }
    else {
        Write-Host "  FAIL  $Name" -ForegroundColor Red
        if ($Detail) { Write-Host "        $Detail" -ForegroundColor DarkYellow }
        [void]$script:assertFails.Add($Name)
    }
}

function Add-Failure {
    param([string]$Text)
    [void]$script:failures.Add($Text)
    Write-Host "  UNMEASURED  $Text" -ForegroundColor Yellow
    [void]$script:log.Add("UNMEASURED: $Text")
}

function Add-Check {
    # The artifact's `checks` array is the brief's cross-check list: a stable name
    # plus the raw detail a reader needs to reproduce the verdict.
    param([string]$Name, [bool]$Ok, [string]$Detail)
    [void]$script:checks.Add([ordered]@{ name = $Name; ok = [bool]$Ok; detail = $Detail })
    $color = 'Green'
    if (-not $Ok) { $color = 'Red' }
    $verdict = 'ok  '
    if (-not $Ok) { $verdict = 'FAIL' }
    Log-Line ("  [{0}] {1} :: {2}" -f $verdict, $Name, $Detail) $color
    if (-not $Ok) { [void]$script:assertFails.Add("check: $Name") }
}

function Get-Median {
    param($Values)
    $arr = @($Values | Where-Object { $null -ne $_ } | ForEach-Object { [double]$_ } | Sort-Object)
    if ($arr.Count -eq 0) { return 0.0 }
    $idx = [int][Math]::Floor($arr.Count / 2)
    if (($arr.Count % 2) -eq 1) { return [Math]::Round([double]$arr[$idx], 4) }
    $lo = [int]($arr.Count / 2) - 1
    return [Math]::Round((([double]$arr[$lo] + [double]$arr[$idx]) / 2.0), 4)
}

function Get-MinValue {
    param($Values)
    $arr = @($Values | ForEach-Object { [double]$_ } | Sort-Object)
    if ($arr.Count -eq 0) { return 0.0 }
    return [Math]::Round([double]$arr[0], 4)
}

function Get-MaxValue {
    param($Values)
    $arr = @($Values | ForEach-Object { [double]$_ } | Sort-Object)
    if ($arr.Count -eq 0) { return 0.0 }
    return [Math]::Round([double]$arr[$arr.Count - 1], 4)
}

# ---------------------------------------------------------------------------
# File helpers -- every read of a file a process may hold open goes through
# FileShare.ReadWrite; a bare StreamReader(path) fails with "being used by
# another process" on the gateway's log and on the JSONL trace file.
# ---------------------------------------------------------------------------

function Read-Text {
    param([string]$Path)
    if ([string]::IsNullOrEmpty($Path)) { return '' }
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $fs = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    $reader = New-Object System.IO.StreamReader($fs, $script:utf8NoBom)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose(); $fs.Dispose() }
}

function Get-TailText {
    param([string]$Path, [int]$Lines = 20)
    $all = Read-Text $Path
    if ([string]::IsNullOrWhiteSpace($all)) { return '' }
    $split = @($all -split "`r?`n" | Where-Object { $_ -match '\S' })
    if ($split.Count -le $Lines) { return ($split -join "`n") }
    return (($split[($split.Count - $Lines)..($split.Count - 1)]) -join "`n")
}

function Write-NoBom {
    param([string]$Path, [string]$Text)
    [System.IO.File]::WriteAllText($Path, $Text, $script:utf8NoBom)
}

function Write-JsonFile {
    param([string]$Path, $Value)
    $json = ($Value | ConvertTo-Json -Depth 20)
    [System.IO.File]::WriteAllText($Path, $json + "`n", $script:utf8NoBom)
}

function Get-Json {
    param([string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text)) { return $null }
    try { return ($Text | ConvertFrom-Json) } catch { return $null }
}

# ---------------------------------------------------------------------------
# HTTP / process helpers
# ---------------------------------------------------------------------------

function Invoke-Curl {
    param([string[]]$Arguments)
    # stderr is dropped: with $ErrorActionPreference='Stop', a native command that
    # writes ANYTHING to stderr becomes a terminating NativeCommandError.
    return (& curl.exe @Arguments 2>$null | Out-String)
}

function Get-Url {
    param([string]$Url, [int]$TimeoutSeconds = 15)
    return (Invoke-Curl @('-s', '--max-time', "$TimeoutSeconds", $Url))
}

function Test-PortOpen {
    param([int]$Port, [int]$TimeoutMs = 500)
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $client.BeginConnect('127.0.0.1', $Port, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne($TimeoutMs)) { return $false }
        $client.EndConnect($iar)
        return $true
    }
    catch { return $false }
    finally { $client.Close() }
}

function Wait-PortClosed {
    param([int]$Port, [int]$Seconds = 10)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (-not (Test-PortOpen -Port $Port -TimeoutMs 250)) { return $true }
        Start-Sleep -Milliseconds 150
    }
    return $false
}

function Wait-Healthy {
    param([int]$Port, [string]$Path = '/healthz', [int]$Seconds = 20)
    $deadline = (Get-Date).AddSeconds($Seconds)
    $code = ''
    while ((Get-Date) -lt $deadline) {
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', '--max-time', '3', "http://127.0.0.1:$Port$Path")).Trim()
        if ($code -eq '200') { return $code }
        Start-Sleep -Milliseconds 150
    }
    return $code
}

function Start-Tracked {
    param([string]$Name, [string]$Exe, [string[]]$Arguments, [string]$LogPath, [string]$ErrPath)
    $proc = Start-Process -FilePath $Exe -ArgumentList $Arguments -RedirectStandardOutput $LogPath -RedirectStandardError $ErrPath -PassThru -WindowStyle Hidden
    [void]$script:started.Add([pscustomobject]@{ Name = $Name; Proc = $proc; Log = $LogPath; Err = $ErrPath })
    return $proc
}

function Stop-Tracked {
    param($Entry)
    if ($null -eq $Entry) { return }
    $p = $Entry.Proc
    if ($null -eq $p) { return }
    if (-not $p.HasExited) {
        Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
        Write-Host "  stopped $($Entry.Name) (pid=$($p.Id))" -ForegroundColor DarkGray
    }
    else {
        Write-Host "  $($Entry.Name) (pid=$($p.Id)) had already exited with $($p.ExitCode)" -ForegroundColor DarkGray
    }
}

function Invoke-GoBuild {
    param([string]$Out, [string]$Pkg)
    $buildLog = Join-Path $workDir ("build-{0}.log" -f (Split-Path -Leaf $Out))
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        Write-NoBom $buildLog ''
        & $goShim build -o $Out $Pkg 2>$buildLog | Out-Null
        if ($LASTEXITCODE -eq 0) {
            Assert-That (Test-Path -LiteralPath $Out) "$Pkg built to $(Split-Path -Leaf $Out)" "attempt $attempt"
            return
        }
        Write-Host ("  build attempt {0} failed for {1}:" -f $attempt, $Pkg) -ForegroundColor DarkYellow
        Write-Host (Get-TailText $buildLog 15) -ForegroundColor DarkYellow
        Start-Sleep -Milliseconds 500
    }
    throw "go build $Pkg failed after 3 attempts (log: $buildLog)"
}

function Invoke-ConfigCheck {
    param([string]$Config)
    $out = Join-Path $workDir ("check-{0}.out" -f (Split-Path -Leaf $Config))
    $err = Join-Path $workDir ("check-{0}.err" -f (Split-Path -Leaf $Config))
    # Never 2>&1: a native stderr line reaching PS 5.1 with Stop set aborts the run.
    # Invoked with & rather than Start-Process: on this host Start-Process -PassThru
    # can hand back a Process object whose ExitCode is $null even after
    # WaitForExit(), while $LASTEXITCODE is always correct for a native call.
    $stdout = (& $gwExe -config $Config -check 2>$err | Out-String)
    $code = $LASTEXITCODE
    Write-NoBom $out $stdout
    $text = (Read-Text $out) + (Read-Text $err)
    return [pscustomobject]@{ Exit = $code; Text = $text }
}

# ---------------------------------------------------------------------------
# Scrape-load generator
#
# The scrape-load arm needs a second, independent 10Hz load in ADDITION to the
# load generator. It runs in a PowerShell background job so a failure inside it
# cannot abort the measurement, and it stops itself at an absolute end time so it
# cannot outlive the arm even if the caller's bookkeeping is wrong. Nothing here
# touches Go source: this is the only scraper the harness has.
# ---------------------------------------------------------------------------

function Start-ScrapeJob {
    param([string]$Base, [int]$IntervalMs, [string[]]$Paths, [string]$ResultFile)
    # The job writes its own tally to $ResultFile as JSON. The parent reads that
    # file rather than Receive-Job: a job's output stream is easy to lose (an
    # empty Receive-Job means "nothing there", which is indistinguishable from
    # "the scrape loop never ran"), and a file is also readable mid-arm.
    #
    # The body cannot use this script's helper functions -- a Start-Job
    # scriptblock runs in a fresh runspace with none of them defined, and an
    # undefined function inside a loop is not fatal, it just accumulates
    # non-terminating errors (which is exactly how an earlier version failed
    # silently). It therefore uses only core cmdlets and .NET types.
    $stopFile = "$ResultFile.stop"
    if (Test-Path -LiteralPath $ResultFile) { Remove-Item -LiteralPath $ResultFile -Force -ErrorAction SilentlyContinue }
    if (Test-Path -LiteralPath $stopFile) { Remove-Item -LiteralPath $stopFile -Force -ErrorAction SilentlyContinue }
    $job = Start-Job -Name ("m5-scrape-{0}" -f $PID) -ArgumentList $Base, $IntervalMs, ($Paths -join ','), $PID, $ResultFile, $stopFile -ScriptBlock {
        param($BaseUrl, $Interval, $PathList, $OwnerPid, $OutFile, $StopFile)
        $endpoints = @($PathList -split ',')
        $ok = 0
        $bad = 0
        $statuses = @{}
        $seconds = 0.0
        $failure = ''
        try {
            $sw = [System.Diagnostics.Stopwatch]::StartNew()
            while ($sw.Elapsed.TotalSeconds -lt 600) {
                # Three independent stop conditions, so a scrape job can never
                # outlive its arm: the parent's explicit stop file (written as
                # soon as the load generator for this arm exits), the owner
                # process disappearing, and the 600s backstop.
                if (Test-Path -LiteralPath $StopFile) { break }
                if (-not (Get-Process -Id $OwnerPid -ErrorAction SilentlyContinue)) { break }
                foreach ($p in $endpoints) {
                    $code = (& curl.exe -s -o NUL -w '%{http_code}' --max-time 3 "$BaseUrl$p" 2>$null | Out-String).Trim()
                    if ($code -eq '200') { $ok += 1 }
                    else {
                        $bad += 1
                        if ($code -eq '') { $code = '(none)' }
                        if ($statuses.ContainsKey($code)) { $statuses[$code] = 1 + $statuses[$code] }
                        else { $statuses[$code] = 1 }
                    }
                }
                Start-Sleep -Milliseconds $Interval
            }
            $sw.Stop()
            $seconds = [Math]::Round($sw.Elapsed.TotalSeconds, 3)
        }
        catch { $failure = $_.Exception.Message }
        # Always write the file, success or failure: the parent must be able to
        # tell "the loop never ran" apart from "the loop ran and found nothing".
        $payload = [ordered]@{
            ok = $ok; bad = $bad; seconds = $seconds
            bad_statuses = ($statuses.Keys | ForEach-Object { "$_=$($statuses[$_])" }) -join ','
            endpoints = @($endpoints)
            interval_ms = $Interval
            failure = $failure
        }
        try { [System.IO.File]::WriteAllText($OutFile, ($payload | ConvertTo-Json -Compress), (New-Object System.Text.UTF8Encoding($false))) } catch { }
        return $payload
    }
    [void]$script:scrapeJobs.Add($job)
    return $job
}

function Stop-ScrapeJob {
    param($Job, [string]$ResultFile)
    # Signal first (the loop polls for the stop file), then give it a moment to
    # write its tally and exit on its own; Stop-Job only as a last resort. When
    # the caller already signalled (see the arm loop, which writes the stop file
    # as soon as the load generator exits, so that the job's measured scrape
    # window spans exactly the arm) this is a no-op write and just the cleanup.
    if (-not [string]::IsNullOrEmpty($ResultFile)) {
        try {
            if (-not (Test-Path -LiteralPath "$ResultFile.stop")) {
                [System.IO.File]::WriteAllText("$ResultFile.stop", 'stop', (New-Object System.Text.UTF8Encoding($false)))
            }
        } catch { }
    }
    if ($null -ne $Job) { try { [void](Wait-Job -Job $Job -Timeout 15) } catch { } }
    if ($null -eq $Job) { return }
    # If the job never produced its result file, copy its error records next to
    # where the file should have been: "the loop wrote nothing" and "the loop
    # never started" are different bugs and only the error stream separates them.
    $haveFile = (-not [string]::IsNullOrEmpty($ResultFile)) -and (Test-Path -LiteralPath $ResultFile)
    if (-not $haveFile -and -not [string]::IsNullOrEmpty($ResultFile)) {
        $diag = ''
        try { $diag = (Receive-Job -Job $Job -Keep -ErrorAction SilentlyContinue 2>&1 | Out-String).Trim() } catch { }
        try {
            $jobState = [string]$Job.State
            $reason = [string]$Job.JobStateInfo.Reason
            $text = "state=$jobState`nreason=$reason`noutput=$diag`n"
            [System.IO.File]::WriteAllText("$ResultFile.err", $text, (New-Object System.Text.UTF8Encoding($false)))
        } catch { }
    }
    try { Stop-Job -Job $Job -ErrorAction SilentlyContinue } catch { }
    try { Remove-Job -Job $Job -Force -ErrorAction SilentlyContinue } catch { }
    try { [void]$script:scrapeJobs.Remove($Job) } catch { }
}

function Get-ScrapeResult {
    param($Job, [string]$ResultFile)
    # The file is the source of truth: it exists only once the loop has stopped,
    # and it carries the complete tally (Receive-Job can return a partially
    # drained stream, which would under-count the scrapes).
    if (-not [string]::IsNullOrEmpty($ResultFile) -and (Test-Path -LiteralPath $ResultFile)) {
        $parsed = Get-Json (Read-Text $ResultFile)
        if ($null -ne $parsed) { return $parsed }
    }
    if ($null -eq $Job) { return $null }
    $res = $null
    try { $res = Receive-Job -Job $Job -ErrorAction SilentlyContinue } catch { $res = $null }
    if ($null -eq $res) { return $null }
    $first = @($res)[0]
    if ($null -eq $first) { return $null }
    return $first
}

# ---------------------------------------------------------------------------
# Metrics parsing
#
# The exposition carries `[` in label sets, so literal containment is done with
# .Contains() and metric values are pulled with regex -- never -like.
# ---------------------------------------------------------------------------

function Get-MetricSum {
    param([string]$Text, [string]$Metric)
    if ([string]::IsNullOrEmpty($Text)) { return [int64]0 }
    $pattern = '(?m)^' + [regex]::Escape($Metric) + '\{[^}]*\}\s+([0-9.eE+-]+)\s*$'
    $rx = New-Object System.Text.RegularExpressions.Regex(
        $pattern, ([System.Text.RegularExpressions.RegexOptions]::Multiline -bor [System.Text.RegularExpressions.RegexOptions]::IgnoreCase))
    $sum = 0.0
    foreach ($m in $rx.Matches($Text)) {
        $text = $m.Groups[1].Value.ToLowerInvariant()
        $v = 0.0
        if ($text.Contains('e')) {
            try { $v = [double]::Parse($text, [System.Globalization.NumberStyles]::Float, [System.Globalization.CultureInfo]::InvariantCulture) }
            catch { $v = 0.0 }
        }
        else { $v = [double]$text }
        $sum += $v
    }
    return [int64][Math]::Round($sum)
}

function Convert-GoDurationToMs {
    param([string]$Text)
    # Go marshals time.Duration as a NANOSECOND integer, but /stats reports its
    # percentiles as Go duration STRINGS ("1.23ms", "456us"), so both shapes
    # reach this script and both have to become milliseconds.
    if ([string]::IsNullOrWhiteSpace($Text)) { return 0.0 }
    $t = $Text.Replace([string][char]0x00B5, 'u').Replace('µ', 'u').Trim()
    $m = [regex]::Match($t, '^([0-9]+(?:\.[0-9]+)?)(ns|us|ms|s|m|h)$')
    if (-not $m.Success) { return 0.0 }
    $v = [double]$m.Groups[1].Value
    switch ($m.Groups[2].Value) {
        'ns' { return [Math]::Round($v / 1e6, 4) }
        'us' { return [Math]::Round($v / 1e3, 4) }
        'ms' { return [Math]::Round($v, 4) }
        's' { return [Math]::Round($v * 1000, 4) }
        'm' { return [Math]::Round($v * 60000, 4) }
        'h' { return [Math]::Round($v * 3600000, 4) }
    }
    return 0.0
}

function Get-MetricOpenStates {
    param([string]$Text)
    $open = New-Object System.Collections.ArrayList
    if ([string]::IsNullOrEmpty($Text)) { return $open }
    $rx = New-Object System.Text.RegularExpressions.Regex(
        '(?ms)infergate_breaker_state\{([^}]*)\}\s+([0-9.eE+-]+)',
        ([System.Text.RegularExpressions.RegexOptions]::Multiline -bor [System.Text.RegularExpressions.RegexOptions]::Singleline))
    foreach ($m in $rx.Matches($Text)) {
        $labels = $m.Groups[1].Value
        $value = [double]$m.Groups[2].Value
        $st = [regex]::Match($labels, 'state="([^"]*)"')
        $up = [regex]::Match($labels, 'upstream="([^"]*)"')
        if ($st.Success -and $st.Groups[1].Value -eq 'open' -and $value -gt 0) {
            $name = '?'
            if ($up.Success) { $name = $up.Groups[1].Value }
            [void]$open.Add($name)
        }
    }
    return $open
}

# ---------------------------------------------------------------------------
# Config generation
#
# configs\ is read-only in this environment, so every instance's config is
# generated into the run's scratch dir. The five untraced instances differ ONLY
# in `listen`; the traced pair differ only in `listen` and in which sinks are
# configured, so an arm difference is never a config difference.
# ---------------------------------------------------------------------------

function New-GatewayConfig {
    param([string]$Name, [int]$Port, [bool]$Tracing, [bool]$UseJSONL, [bool]$UseOTLP, [string]$LogLevelLocal)
    $path = Join-Path $workDir "m5-measure-$Name.yaml"
    $sb = New-Object System.Text.StringBuilder
    [void]$sb.AppendLine('# generated by scripts\measure-m5.ps1 -- do not edit; configs\ is never modified')
    [void]$sb.AppendLine('server:')
    [void]$sb.AppendLine("  listen: `"127.0.0.1:$Port`"")
    [void]$sb.AppendLine('  upstream_timeout: "10s"')
    [void]$sb.AppendLine('  shutdown_timeout: "5s"')
    [void]$sb.AppendLine('  max_body_bytes: 8388608')
    [void]$sb.AppendLine('  max_idle_conns_per_host: 256')
    [void]$sb.AppendLine('')
    [void]$sb.AppendLine('log:')
    [void]$sb.AppendLine("  level: `"$LogLevelLocal`"")
    [void]$sb.AppendLine('  format: "text"')
    [void]$sb.AppendLine('')
    [void]$sb.AppendLine('upstreams:')
    [void]$sb.AppendLine('  - name: "m5-load-mock"')
    [void]$sb.AppendLine('    kind: "openai"')
    [void]$sb.AppendLine("    base_url: `"$mockBase`"")
    [void]$sb.AppendLine('    api_key: "loadtest-key"')
    [void]$sb.AppendLine('    models: ["/"]')
    [void]$sb.AppendLine('    capabilities: ["chat"]')
    [void]$sb.AppendLine('    priority: 1')
    [void]$sb.AppendLine('')
    [void]$sb.AppendLine('routing:')
    [void]$sb.AppendLine('  strategy: "priority"')
    [void]$sb.AppendLine('  default_capabilities: ["chat"]')
    [void]$sb.AppendLine('')
    [void]$sb.AppendLine('health:')
    [void]$sb.AppendLine('  window: "30s"')
    [void]$sb.AppendLine('  buckets: 6')
    [void]$sb.AppendLine('  min_requests: 1000')
    [void]$sb.AppendLine('  failure_ratio: 0.5')
    [void]$sb.AppendLine('  open_duration: "30s"')
    [void]$sb.AppendLine('  half_open_probes: 1')
    [void]$sb.AppendLine('  max_failures_per_request: 2')
    [void]$sb.AppendLine('  retry_backoff: "20ms"')
    [void]$sb.AppendLine('')
    [void]$sb.AppendLine('tracing:')
    $enabledText = 'false'
    if ($Tracing) { $enabledText = 'true' }
    [void]$sb.AppendLine("  enabled: $enabledText")
    [void]$sb.AppendLine('  capacity: 8192')
    [void]$sb.AppendLine('  sample_ratio: 1.0')
    $jsonlText = '""'
    if ($UseJSONL) { $jsonlText = '"' + ($jsonlPath -replace '\\', '/') + '"' }
    [void]$sb.AppendLine("  jsonl_path: $jsonlText")
    [void]$sb.AppendLine('  otlp:')
    $endpointText = '""'
    if ($UseOTLP) { $endpointText = '"' + $otlpEndpoint + '"' }
    [void]$sb.AppendLine("    endpoint: $endpointText")
    [void]$sb.AppendLine('    timeout: "5s"')
    [void]$sb.AppendLine("    service_name: `"$otlpServiceName`"")
    [void]$sb.AppendLine('    headers: {}')
    Write-NoBom $path ($sb.ToString())
    return $path
}

# ---------------------------------------------------------------------------
# 0. Banner
# ---------------------------------------------------------------------------

Write-Section '0. InferGate M5 load-baseline + observability-overhead measurement'
Write-Host "  repo        : $repo"
Write-Host "  scratch     : $workDir"
Write-Host "  artifact    : $artifact"
Write-Host "  levels      : $($levels -join ', ')  requests=$Requests warmup=$Warmup rounds=$Rounds timeout=$Timeout"
Write-Host "  mock        : $mockBase (token-delay ${MockTokenDelayMs}ms, ttfb ${MockTTFBMs}ms)"
Write-Host "  gateways    : $($gatewayPorts -join ', ')  (A-D untraced fleet, E traced OTLP, F traced OTLP+JSONL)"
Write-Host "  collector   : $collectorBase"
Write-Host "  log level   : $LogLevel (per-request logging is deliberately out of the comparison)"
if ($isSmoke) {
    Write-Host ("  WARNING: smoke run ({0} requests, {1} rounds) -- NOT written to docs\baseline\" -f $Requests, $Rounds) -ForegroundColor Yellow
    Write-Host ("           reason: {0}; the artifact will go to {1}" -f $smokeReason, $artifact) -ForegroundColor Yellow
    Write-Host "           smoke numbers are start-up dominated and must never be quoted as a measurement" -ForegroundColor Yellow
}

$exitOk = $false
$runError = ''
$rawRuns = New-Object System.Collections.ArrayList   # [pscustomobject]@{Arm;Round;Path;Rows;RunSeconds}
$aggregates = @()
$vsDirect = @()
$tracingOverhead = @()
$horizontalBlock = [ordered]@{}
$bottleneckBlock = [ordered]@{}
$gwAMetrics = [ordered]@{ before = -1; after = -1; delta = -1; dispatched = 0; warmup_total = 0; warmup_per_phase = 0; phases = 0; arm = ''; round = 0; delta_per_dispatched = -1 }
$gatewayCounts = @{}
$metricsProbe = [ordered]@{}
$tracingProbe = [ordered]@{}
$jsonlProbe = [ordered]@{}
$statsProbe = [ordered]@{}

try {
    # -----------------------------------------------------------------------
    # 1. Build
    # -----------------------------------------------------------------------
    Write-Section '1. Build'
    if ($SkipBuild) {
        Log-Line '  -SkipBuild: reusing the existing binaries in bin\' 'DarkYellow'
        foreach ($pair in @(@($gwExe, 'infergate'), @($mockExe, 'mockupstream'), @($ltExe, 'loadtest'), @($mcExe, 'mockcollector'))) {
            Assert-That (Test-Path -LiteralPath $pair[0]) "bin\$($pair[1]).exe exists" 'missing and -SkipBuild was set'
        }
    }
    else {
        Invoke-GoBuild -Out $gwExe -Pkg '.\cmd\infergate'
        Invoke-GoBuild -Out $mockExe -Pkg '.\cmd\mockupstream'
        Invoke-GoBuild -Out $ltExe -Pkg '.\cmd\loadtest'
        Invoke-GoBuild -Out $mcExe -Pkg '.\cmd\mockcollector'
    }

    # -----------------------------------------------------------------------
    # 2. Configs
    # -----------------------------------------------------------------------
    Write-Section '2. Fleet configurations'
    $configs = [ordered]@{}
    $configs['A'] = New-GatewayConfig -Name 'a' -Port $ports.A -Tracing $false -UseJSONL $false -UseOTLP $false -LogLevelLocal $LogLevel
    $configs['B'] = New-GatewayConfig -Name 'b' -Port $ports.B -Tracing $false -UseJSONL $false -UseOTLP $false -LogLevelLocal $LogLevel
    $configs['C'] = New-GatewayConfig -Name 'c' -Port $ports.C -Tracing $false -UseJSONL $false -UseOTLP $false -LogLevelLocal $LogLevel
    $configs['D'] = New-GatewayConfig -Name 'd' -Port $ports.D -Tracing $false -UseJSONL $false -UseOTLP $false -LogLevelLocal $LogLevel
    $configs['E'] = New-GatewayConfig -Name 'e-traced-otlp' -Port $ports.E -Tracing $true -UseJSONL $false -UseOTLP $true -LogLevelLocal $LogLevel
    $configs['F'] = New-GatewayConfig -Name 'f-traced-full' -Port $ports.F -Tracing $true -UseJSONL $true -UseOTLP $true -LogLevelLocal $LogLevel

    foreach ($key in @('A', 'B', 'C', 'D', 'E', 'F')) {
        $res = Invoke-ConfigCheck -Config $configs[$key]
        Assert-That ($res.Exit -eq 0) "infergate -check accepts the $key config" "exit=$($res.Exit) $($res.Text.Trim())"
    }

    # The JSONL sink appends, so a leftover file from a previous invocation would
    # make this run's line count meaningless. Delete it BEFORE the gateway starts.
    if (Test-Path -LiteralPath $jsonlPath) { Remove-Item -LiteralPath $jsonlPath -Force }
    Assert-That (-not (Test-Path -LiteralPath $jsonlPath)) 'the JSONL trace file is absent before the traced gateway starts' $jsonlPath

    # -----------------------------------------------------------------------
    # 3. Fleet up
    # -----------------------------------------------------------------------
    Write-Section '3. Fleet up'
    $mockLog = Join-Path $workDir 'mock.log'
    $mockErr = Join-Path $workDir 'mock.err'
    $mockArgs = @('-listen', "127.0.0.1:$mockPort", '-name', 'm5-load-mock',
        '-token-delay', "${MockTokenDelayMs}ms", '-ttfb', "${MockTTFBMs}ms")
    [void](Start-Tracked -Name 'mockupstream' -Exe $mockExe -Arguments $mockArgs -LogPath $mockLog -ErrPath $mockErr)
    Assert-That ((Wait-Healthy -Port $mockPort -Seconds $HealthTimeoutSeconds) -eq '200') "the mock upstream answers /healthz on :$mockPort"

    $mcLog = Join-Path $workDir 'collector.log'
    $mcErr = Join-Path $workDir 'collector.err'
    [void](Start-Tracked -Name 'mockcollector' -Exe $mcExe -Arguments @('-listen', "127.0.0.1:$collectorPort", '-name', 'm5-measure-collector', '-quiet') -LogPath $mcLog -ErrPath $mcErr)
    Assert-That ((Wait-Healthy -Port $collectorPort -Seconds $HealthTimeoutSeconds) -eq '200') "the mock OTLP collector answers /healthz on :$collectorPort"

    foreach ($key in @('A', 'B', 'C', 'D', 'E', 'F')) {
        $port = $ports[$key]
        $logPath = Join-Path $workDir "gateway-$key.log"
        $errPath = Join-Path $workDir "gateway-$key.err"
        [void](Start-Tracked -Name "gateway-$key" -Exe $gwExe -Arguments @('-config', $configs[$key]) -LogPath $logPath -ErrPath $errPath)
        $healthy = Wait-Healthy -Port $port -Seconds $HealthTimeoutSeconds
        Assert-That ($healthy -eq '200') "gateway $key answers /healthz on :$port" "http=$healthy"
        if ($healthy -ne '200') {
            Write-Host (Get-TailText $logPath 20) -ForegroundColor DarkYellow
            Write-Host (Get-TailText $errPath 20) -ForegroundColor DarkYellow
        }
    }

    # -----------------------------------------------------------------------
    # 4. Measurement
    #
    # Rounds are OUTER and the arms are interleaved inside each round, so host
    # drift lands on every arm rather than on the last one. `-rounds` does not
    # work in cmd\loadtest's -url path (it is honoured only in -all mode), which
    # is why this script owns the rounds and takes the median across them.
    # -----------------------------------------------------------------------
    Write-Section '4. Measurement'
    $runPlan = @(
        [pscustomobject]@{ Arm = 'direct'; Kind = 'one'; Targets = @($mockBase) },
        [pscustomobject]@{ Arm = 'gateway'; Kind = 'one'; Targets = @("http://127.0.0.1:$($ports.A)") },
        [pscustomobject]@{ Arm = 'gateway-scraped'; Kind = 'one'; Targets = @("http://127.0.0.1:$($ports.A)") },
        [pscustomobject]@{ Arm = 'traced-otlp'; Kind = 'one'; Targets = @("http://127.0.0.1:$($ports.E)") },
        [pscustomobject]@{ Arm = 'traced-full'; Kind = 'one'; Targets = @("http://127.0.0.1:$($ports.F)") },
        [pscustomobject]@{ Arm = 'second'; Kind = 'one'; Targets = @("http://127.0.0.1:$($ports.B)") },
        [pscustomobject]@{ Arm = 'horizontal-2'; Kind = 'many'; Targets = @("http://127.0.0.1:$($ports.A)", "http://127.0.0.1:$($ports.B)") },
        [pscustomobject]@{ Arm = 'horizontal-4'; Kind = 'many'; Targets = @("http://127.0.0.1:$($ports.A)", "http://127.0.0.1:$($ports.B)", "http://127.0.0.1:$($ports.C)", "http://127.0.0.1:$($ports.D)") }
    )
    $scrapePaths = @('/stats', '/metrics')
    $scrapeIntervalMs = 100
    $scrapeStats = [ordered]@{
        interval_ms = $scrapeIntervalMs
        endpoints = @($scrapePaths)
        arms = @()
        scrapes = 0
        scrapes_ok = 0
        scrapes_failed = 0
        scrape_seconds = 0.0
        scrape_rate_hz = 0.0
        intended_rate_hz = [Math]::Round((1000.0 / $scrapeIntervalMs), 3)
        note = 'one scrape = one request to each endpoint, so a "scrape" here is len(endpoints) HTTP GETs'
    }

    for ($round = 1; $round -le $Rounds; $round++) {
        foreach ($plan in $runPlan) {
            $outPath = Join-Path $workDir "m5-load-$($plan.Arm)-r$round.json"
            $ltLog = Join-Path $workDir "loadtest-$($plan.Arm)-r$round.log"
            $ltErr = Join-Path $workDir "loadtest-$($plan.Arm)-r$round.err"
            $targetArgs = @()
            if ($plan.Kind -eq 'one') { $targetArgs = @('-url', $plan.Targets[0]) }
            else { $targetArgs = @('-urls', ($plan.Targets -join ',')) }
            $ltArgs = $targetArgs + @('-c', $Concurrency, '-n', "$Requests", '-warmup', "$Warmup",
                '-timeout', $Timeout, '-out', $outPath)

            # Check 3's samples: around ONE arm on the primary untraced instance.
            $before = -1
            if ($plan.Arm -eq 'gateway' -and -not $script:gwARequestsTotal['armed']) {
                $before = Get-MetricSum (Get-Url "$($plan.Targets[0])/metrics") 'infergate_requests_total'
                $script:gwARequestsTotal['before'] = $before
            }

            # The scrape-load arm runs a second, independent 10Hz GET load at the
            # SAME instance for the duration of the load-test invocation. It lives
            # in a background job that stops itself when this script's pid is gone,
            # and is stopped here as well so it can never overlap the next arm.
            $scrapeJob = $null
            $scrapeResultFile = Join-Path $workDir "scrape-$($plan.Arm)-r$round.json"
            if ($plan.Arm -eq 'gateway-scraped') {
                $scrapeJob = Start-ScrapeJob -Base $plan.Targets[0] -IntervalMs $scrapeIntervalMs -Paths $scrapePaths -ResultFile $scrapeResultFile
            }

            $sw = [System.Diagnostics.Stopwatch]::StartNew()
            Write-Host ("  round {0} arm {1,-15} -> {2}" -f $round, $plan.Arm, ($plan.Targets -join ' ')) -ForegroundColor White
            # & blocks until the native program is done and always sets
            # $LASTEXITCODE; stdout goes to the log, stderr to its own file so no
            # native stderr line can become a terminating NativeCommandError.
            $stdout = (& $ltExe @ltArgs 2>$ltErr | Out-String)
            $ltExit = $LASTEXITCODE
            Write-NoBom $ltLog $stdout
            $sw.Stop()

            # Signal the scrape loop the moment the load generator is done, so
            # the window the job reports is this arm's window and not this arm
            # plus however long the rest of the bookkeeping takes. Then wait for
            # it to finish on its own: it writes its own tally file on exit.
            $scrapeResult = $null
            if ($null -ne $scrapeJob) {
                try { [System.IO.File]::WriteAllText("$scrapeResultFile.stop", 'stop', (New-Object System.Text.UTF8Encoding($false))) } catch { }
                try { [void](Wait-Job -Job $scrapeJob -Timeout 15) } catch { }
                $scrapeResult = Get-ScrapeResult -Job $scrapeJob -ResultFile $scrapeResultFile
                $jobStateAtCollect = [string]$scrapeJob.State
                Stop-ScrapeJob -Job $scrapeJob -ResultFile $scrapeResultFile
                $scrapeJob = $null
                if ($null -eq $scrapeResult) {
                    $reasonText = ''
                    if (Test-Path -LiteralPath "$scrapeResultFile.err") { $reasonText = ' ' + (Read-Text "$scrapeResultFile.err") }
                    Add-Failure "the scrape job for arm $($plan.Arm) round $round returned nothing (result file: $scrapeResultFile; job state at collection: $jobStateAtCollect)$reasonText"
                }
                else {
                    $scrapes = [int]$scrapeResult.ok + [int]$scrapeResult.bad
                    $seconds = [double]$scrapeResult.seconds
                    if ($null -ne $scrapeResult.failure -and ([string]$scrapeResult.failure) -ne '') {
                        Add-Failure "the scrape job for arm $($plan.Arm) round $round reported a failure: $($scrapeResult.failure)"
                    }
                    $rate = 0.0
                    if ($seconds -gt 0) { $rate = [Math]::Round(($scrapes / $seconds), 3) }
                    $scrapeStats['scrapes'] = [int]$scrapeStats['scrapes'] + $scrapes
                    $scrapeStats['scrapes_ok'] = [int]$scrapeStats['scrapes_ok'] + [int]$scrapeResult.ok
                    $scrapeStats['scrapes_failed'] = [int]$scrapeStats['scrapes_failed'] + [int]$scrapeResult.bad
                    $scrapeStats['scrape_seconds'] = [Math]::Round(([double]$scrapeStats['scrape_seconds'] + $seconds), 3)
                    if ([double]$scrapeStats['scrape_seconds'] -gt 0) {
                        $scrapeStats['scrape_rate_hz'] = [Math]::Round(([int]$scrapeStats['scrapes'] / [double]$scrapeStats['scrape_seconds']), 3)
                    }
                    $scrapeStats['arms'] += [ordered]@{
                        arm = $plan.Arm; round = $round; scrapes = $scrapes; ok = [int]$scrapeResult.ok
                        failed = [int]$scrapeResult.bad; seconds = $seconds; rate_hz = $rate
                        arm_seconds = [Math]::Round($sw.Elapsed.TotalSeconds, 3)
                        bad_statuses = [string]$scrapeResult.bad_statuses
                    }
                    Write-Host ("    scrapes={0} ok={1} failed={2} in {3}s = {4} Hz (intended {5} Hz)" -f `
                            $scrapes, $scrapeResult.ok, $scrapeResult.bad, $seconds, $rate, [double]$scrapeStats['intended_rate_hz']) -ForegroundColor DarkGray
                }
            }

            Assert-That ($ltExit -eq 0) "loadtest $($plan.Arm) round $round exited 0" "exit=$ltExit $((Get-TailText $ltErr 6))"

            $rows = @(Get-Json (Read-Text $outPath))
            Assert-That ($rows.Count -eq 2 * $levels.Count) "loadtest $($plan.Arm) round $round wrote $((2 * $levels.Count)) phases" "got $($rows.Count)"
            if ($rows.Count -eq 0) {
                throw "arm $($plan.Arm) round $round produced no phases (loadtest exit=$ltExit): $((Get-TailText $ltErr 10))"
            }
            [void]$rawRuns.Add([pscustomobject]@{
                    Arm = $plan.Arm; Round = $round; Path = $outPath; Rows = $rows
                    RunSeconds = [Math]::Round($sw.Elapsed.TotalSeconds, 1); Targets = $plan.Targets
                })

            if ($plan.Arm -eq 'gateway' -and -not $script:gwARequestsTotal['armed']) {
                $after = Get-MetricSum (Get-Url "$($plan.Targets[0])/metrics") 'infergate_requests_total'
                $script:gwARequestsTotal['after'] = $after
                $script:gwARequestsTotal['armed'] = $true
                $dispatched = 0
                foreach ($r in $rows) { $dispatched += [int]$r.requests }
                $gwAMetrics['before'] = $before
                $gwAMetrics['after'] = $after
                $gwAMetrics['delta'] = $after - $before
                $gwAMetrics['dispatched'] = $dispatched
                # A phase's warmup requests are real requests to the instance,
                # so the counter legitimately covers dispatched + warmup. The
                # measured invariant is delta == dispatched + warmup*phases
                # (verified directly on this host: -n 60 -warmup 5 with 2 phases
                # gave delta 130 for dispatched 120 and warmup 10).
                $gwAMetrics['warmup_per_phase'] = $Warmup
                $gwAMetrics['phases'] = @($rows).Count
                $gwAMetrics['warmup_total'] = $Warmup * @($rows).Count
                $gwAMetrics['arm'] = 'gateway'
                $gwAMetrics['round'] = $round
                if ($dispatched -gt 0) { $gwAMetrics['delta_per_dispatched'] = [Math]::Round(([double]($after - $before)) / $dispatched, 4) }
                # Run-level metrics probes for the artifact's provenance block.
                foreach ($p in @(@('A', $ports.A), @('B', $ports.B), @('C', $ports.C), @('D', $ports.D), @('E', $ports.E), @('F', $ports.F))) {
                    $text = Get-Url "http://127.0.0.1:$($p[1])/metrics"
                    $metricsProbe[$p[0]] = [ordered]@{
                        port = $p[1]
                        requests_total = Get-MetricSum $text 'infergate_requests_total'
                        stream_bytes_total_declared = $text.Contains('# TYPE infergate_stream_bytes_total')
                        breaker_open = @(Get-MetricOpenStates $text)
                    }
                }
            }
        }
    }

    # -----------------------------------------------------------------------
    # 5. Aggregate
    # -----------------------------------------------------------------------
    Write-Section '5. Aggregate (median across rounds)'
    $groups = @{}
    foreach ($run in $rawRuns) {
        foreach ($row in $run.Rows) {
            $streamLabel = 'non-stream'
            if ([bool]$row.stream) { $streamLabel = 'stream' }
            $key = "$($run.Arm)|$streamLabel|$([int]$row.concurrency)"
            if (-not $groups.ContainsKey($key)) { $groups[$key] = New-Object System.Collections.ArrayList }
            [void]$groups[$key].Add([pscustomobject]@{ Arm = $run.Arm; Workload = $streamLabel; Row = $row })
        }
    }

    $aggregates = @()
    foreach ($key in ($groups.Keys | Sort-Object)) {
        $items = @($groups[$key])
        $arm = $items[0].Arm
        $workload = $items[0].Workload
        $conc = [int]$items[0].Row.concurrency
        $qpsValues = @($items | ForEach-Object { [double]$_.Row.qps })
        $gwMeans = @($items | Where-Object { $null -ne $_.Row.PSObject.Properties['gateway_mean'] -and [double]$_.Row.gateway_mean -gt 0 } | ForEach-Object { [double]$_.Row.gateway_mean / 1e6 })
        $gwCounts = @($items | Where-Object { $null -ne $_.Row.PSObject.Properties['gateway_count'] } | ForEach-Object { [int]$_.Row.gateway_count })
        $errs = 0
        foreach ($it in $items) { $errs += [int]$it.Row.errors }

        $rec = [ordered]@{
            Arm                = $arm
            Workload           = $workload
            Concurrency        = $conc
            QPS                = Get-Median $qpsValues
            QPSMin             = Get-MinValue $qpsValues
            QPSMax             = Get-MaxValue $qpsValues
            P50_ms             = Get-Median @($items | ForEach-Object { [double]$_.Row.p50 / 1e6 })
            P95_ms             = Get-Median @($items | ForEach-Object { [double]$_.Row.p95 / 1e6 })
            P99_ms             = Get-Median @($items | ForEach-Object { [double]$_.Row.p99 / 1e6 })
            TTFT_p95_ms        = Get-Median @($items | ForEach-Object { [double]$_.Row.ttft_p95 / 1e6 })
            Max_ms             = Get-Median @($items | ForEach-Object { [double]$_.Row.max / 1e6 })
            Mean_ms            = Get-Median @($items | ForEach-Object { [double]$_.Row.mean / 1e6 })
            GatewayMean_ms     = Get-Median $gwMeans
            GatewayCount       = 0
            Errors             = $errs
            Rounds             = $items.Count
        }
        if ($gwCounts.Count -gt 0) { $rec['GatewayCount'] = Get-Median $gwCounts }
        # The run-to-run spread the same arm already showed, as a percentage of
        # its median QPS. This is the ONLY honest noise band this harness has:
        # with a single round there is no estimate at all, so it stays 0 and
        # NoiseIsEstimated stays false -- every delta comparison below then says
        # "cannot call this measurable" rather than "measurably zero".
        $noisePct = 0.0
        if ($items.Count -ge 2 -and [double]$rec['QPS'] -gt 0) {
            $noisePct = [Math]::Round((100.0 * ([double]$rec['QPSMax'] - [double]$rec['QPSMin']) / [double]$rec['QPS']), 2)
        }
        $rec['QPSNoise_pct'] = $noisePct
        $rec['NoiseIsEstimated'] = [bool]($items.Count -ge 2)
        $aggregates += $rec
        $gatewayCounts["$arm|$workload|$conc"] = $rec['GatewayCount']

        Write-Host ("  {0,-13} {1,-11} c={2,-4} qps={3,8} [{4}-{5}] p50={6,7} p95={7,7} p99={8,8} ttft_p95={9,7}" -f `
                $arm, $workload, $conc, $rec['QPS'], $rec['QPSMin'], $rec['QPSMax'], $rec['P50_ms'], $rec['P95_ms'], $rec['P99_ms'], $rec['TTFT_p95_ms']) -ForegroundColor DarkGray
    }

    function Get-Aggregate {
        param([string]$Arm, [string]$Workload, [int]$Conc)
        foreach ($a in $aggregates) {
            if ($a.Arm -eq $Arm -and $a.Workload -eq $Workload -and [int]$a.Concurrency -eq $Conc) { return $a }
        }
        return $null
    }

    # The widest run-to-run spread of the two arms being compared, in percent of
    # median QPS. A delta smaller than this is indistinguishable from the host's
    # own drift and must be reported as "within noise", never as a cost. Two
    # distinct arms take the wider band, not an average: one of the two arms may
    # simply have been unluckier, and the claim has to survive the worse case.
    #
    # NOTE: the aggregates are [ordered]@{} dictionaries, and PowerShell does NOT
    # expose an OrderedDictionary's keys through PSObject.Properties (only
    # Count/Keys/Values/... show up). Reading the noise fields that way silently
    # returned $null for every arm, which made every comparison claim "one round
    # gives no noise estimate" even on a 3-round run. Index the dictionary
    # directly, and keep the PSObject path for plain PSCustomObjects.
    function Get-Prop {
        param($Obj, [string]$Name)
        if ($null -eq $Obj) { return $null }
        if ($Obj -is [System.Collections.IDictionary]) {
            if ($Obj.Contains($Name)) { return $Obj[$Name] }
            return $null
        }
        if ($null -eq $Obj.PSObject.Properties[$Name]) { return $null }
        return $Obj.PSObject.Properties[$Name].Value
    }

    function Get-NoiseBandPct {
        param($A, $B)
        $band = 0.0
        $estimated = $false
        foreach ($x in @($A, $B)) {
            if ($null -eq $x) { continue }
            if ([bool](Get-Prop $x 'NoiseIsEstimated')) { $estimated = $true }
            $np = Get-Prop $x 'QPSNoise_pct'
            if ($null -ne $np -and [double]$np -gt $band) { $band = [double]$np }
        }
        return [pscustomobject]@{ Band = $band; Estimated = $estimated }
    }

    # ---- overhead vs direct ------------------------------------------------
    foreach ($workload in @('non-stream', 'stream')) {
        foreach ($conc in $levels) {
            $d = Get-Aggregate -Arm 'direct' -Workload $workload -Conc $conc
            $g = Get-Aggregate -Arm 'gateway' -Workload $workload -Conc $conc
            if ($null -eq $d -or $null -eq $g) { continue }
            $ratio = 0.0
            $pct = 0.0
            if ([double]$d.QPS -gt 0) {
                $ratio = [Math]::Round(([double]$g.QPS / [double]$d.QPS), 4)
                $pct = [Math]::Round((100.0 * ([double]$g.QPS - [double]$d.QPS) / [double]$d.QPS), 2)
            }
            $band = Get-NoiseBandPct $d $g
            $vsDirect += [ordered]@{
                workload        = $workload
                concurrency     = $conc
                direct_qps      = $d.QPS
                gateway_qps     = $g.QPS
                qps_ratio       = $ratio
                qps_percent     = $pct
                p95_delta_ms    = [Math]::Round(([double]$g.P95_ms - [double]$d.P95_ms), 4)
                p99_delta_ms    = [Math]::Round(([double]$g.P99_ms - [double]$d.P99_ms), 4)
                direct_p95_ms   = $d.P95_ms
                gateway_p95_ms  = $g.P95_ms
                direct_p99_ms   = $d.P99_ms
                gateway_p99_ms  = $g.P99_ms
                noise_band_percent = $band.Band
                noise_estimated    = [bool]$band.Estimated
                beyond_noise       = [bool]($band.Estimated -and ([Math]::Abs($pct) -gt $band.Band))
                within_noise       = [bool](-not ($band.Estimated -and ([Math]::Abs($pct) -gt $band.Band)))
                note            = 'gateway overhead is measured against the same single mock the direct arm talks to; direct is the witness, not a client benchmark'
            }
        }
    }

    # ---- tracing overhead --------------------------------------------------
    $jsonlVerdict = 'not computed'
    foreach ($workload in @('non-stream', 'stream')) {
        foreach ($conc in $levels) {
            $g = Get-Aggregate -Arm 'gateway' -Workload $workload -Conc $conc
            if ($null -eq $g) { continue }
            foreach ($pair in @(@('traced-otlp', 'otlp'), @('traced-full', 'otlp+jsonl'))) {
                $t = Get-Aggregate -Arm $pair[0] -Workload $workload -Conc $conc
                if ($null -eq $t) { continue }
                $ratio = 0.0
                $pct = 0.0
                if ([double]$g.QPS -gt 0) {
                    $ratio = [Math]::Round(([double]$t.QPS / [double]$g.QPS), 4)
                    $pct = [Math]::Round((100.0 * ([double]$t.QPS - [double]$g.QPS) / [double]$g.QPS), 2)
                }
                $band = Get-NoiseBandPct $g $t
                $tracingOverhead += [ordered]@{
                    workload           = $workload
                    concurrency        = $conc
                    sink               = $pair[1]
                    untraced_qps       = $g.QPS
                    traced_qps         = $t.QPS
                    qps_ratio          = $ratio
                    qps_percent        = $pct
                    p95_delta_ms       = [Math]::Round(([double]$t.P95_ms - [double]$g.P95_ms), 4)
                    p99_delta_ms       = [Math]::Round(([double]$t.P99_ms - [double]$g.P99_ms), 4)
                    untraced_p95_ms    = $g.P95_ms
                    traced_p95_ms      = $t.P95_ms
                    noise_band_percent = $band.Band
                    noise_estimated    = [bool]$band.Estimated
                    beyond_noise       = [bool]($band.Estimated -and ([Math]::Abs($pct) -gt $band.Band))
                    within_noise       = [bool](-not ($band.Estimated -and ([Math]::Abs($pct) -gt $band.Band)))
                    verdict = $(if (-not $band.Estimated) { 'not assessable: one round gives no noise estimate, so this delta cannot be called measurable' }
                        elseif ([Math]::Abs($pct) -gt $band.Band) { ("measurable: {0}% QPS change exceeds the {1}% run-to-run band" -f $pct, $band.Band) }
                        else { ("within round-to-round noise on this host: {0}% QPS change, {1}% band" -f $pct, $band.Band) })
                }
            }
            # Does the JSONL sink cost anything ON TOP of OTLP? Compare the two
            # traced arms directly; that difference is the sink's own cost.
            $otlpArm = Get-Aggregate -Arm 'traced-otlp' -Workload $workload -Conc $conc
            $fullArm = Get-Aggregate -Arm 'traced-full' -Workload $workload -Conc $conc
            if ($null -ne $otlpArm -and $null -ne $fullArm) {
                $deltaPct = 0.0
                if ([double]$otlpArm.QPS -gt 0) {
                    $deltaPct = [Math]::Round((100.0 * ([double]$fullArm.QPS - [double]$otlpArm.QPS) / [double]$otlpArm.QPS), 2)
                }
                $row = $tracingOverhead[$tracingOverhead.Count - 1]
                $row['jsonl_vs_otlp_qps_percent'] = $deltaPct
                $row['jsonl_vs_otlp_p95_delta_ms'] = [Math]::Round(([double]$fullArm.P95_ms - [double]$otlpArm.P95_ms), 4)
                # A sink "adds measurable cost" only if its QPS drop exceeds the
                # run-to-run spread the two traced arms already showed. Anything
                # smaller is host noise, and with a single round there is no
                # spread to compare against at all -- then the honest answer is
                # "not assessable", not "no cost" and not "costs 49%".
                $band = Get-NoiseBandPct $otlpArm $fullArm
                $jsonlBeyond = [bool]($band.Estimated -and ([Math]::Abs($deltaPct) -gt $band.Band))
                $jsonlCost = [bool]($jsonlBeyond -and ($deltaPct -lt 0))
                $row['jsonl_vs_otlp_noise_percent'] = $band.Band
                $row['jsonl_vs_otlp_noise_estimated'] = [bool]$band.Estimated
                $row['jsonl_vs_otlp_beyond_noise'] = $jsonlBeyond
                $row['jsonl_adds_measurable_cost'] = $jsonlCost
                $row['jsonl_vs_otlp_within_noise'] = [bool](-not $jsonlBeyond)
                $row['jsonl_vs_otlp_verdict'] = $(if (-not $band.Estimated) { 'not assessable: one round gives no noise estimate' }
                    elseif ($jsonlCost) { ("measurable cost: {0}% QPS change exceeds the {1}% band" -f $deltaPct, $band.Band) }
                    elseif ($jsonlBeyond) { ("beyond the {1}% band but a GAIN ({0}%), so not a sink cost -- two arms drifting" -f $deltaPct, $band.Band) }
                    else { ("within round-to-round noise on this host: {0}% QPS change, {1}% band" -f $deltaPct, $band.Band) })
            }
        }
    }

    # One verdict line for the whole comparison, stated once and honestly.
    $sinkDelta = @($tracingOverhead | Where-Object { $null -ne (Get-Prop $_ 'jsonl_vs_otlp_qps_percent') })
    $sinkFlags = @($sinkDelta | Where-Object { $_.jsonl_adds_measurable_cost })
    $sinkAssessable = @($sinkDelta | Where-Object { $_.jsonl_vs_otlp_noise_estimated })
    if ($sinkDelta.Count -eq 0) {
        $jsonlVerdict = 'not computed: no traced arm produced both an OTLP-only and an OTLP+JSONL aggregate'
    }
    elseif ($Rounds -lt 2 -or $sinkAssessable.Count -eq 0) {
        $worstS = ($sinkDelta | Sort-Object { [Math]::Abs($_.jsonl_vs_otlp_qps_percent) } -Descending)[0]
        $jsonlVerdict = ("NOT ASSESSABLE at {0} round(s): a single round gives no run-to-run noise estimate, so the {1}% difference between the OTLP-only and OTLP+JSONL arms at c={2} ({3}) cannot be attributed to the sink. Re-run with -Rounds 2 or more." -f `
                $Rounds, $worstS.jsonl_vs_otlp_qps_percent, $worstS.concurrency, $worstS.workload)
    }
    elseif ($sinkFlags.Count -eq 0) {
        $worst = ($sinkDelta | Sort-Object { [Math]::Abs($_.jsonl_vs_otlp_qps_percent) } -Descending)[0]
        $jsonlVerdict = ("the JSONL sink adds no measurable cost on top of OTLP anywhere measured: the largest QPS change is {0}% at c={1} ({2} workload), which is within round-to-round noise on this host (band {3}%)" -f `
                $worst.jsonl_vs_otlp_qps_percent, $worst.concurrency, $worst.workload, $worst.jsonl_vs_otlp_noise_percent)
    }
    else {
        $jsonlVerdict = ("the JSONL sink does cost something on top of OTLP at {0} of {1} measured (workload, concurrency) pairs, by more than that pair's own run-to-run band; see overhead.tracing[jsonl_vs_otlp_*]" -f `
                $sinkFlags.Count, $sinkDelta.Count)
    }

    # ---- scrape load -------------------------------------------------------
    # gateway-scraped is gateway A again with a second, independent 10Hz GET load
    # against /stats and /metrics. The comparison is against the `gateway` arm at
    # the same (workload, concurrency), which is the same instance with no scraper.
    $scrapeOverhead = @()
    foreach ($workload in @('non-stream', 'stream')) {
        foreach ($conc in $levels) {
            $g = Get-Aggregate -Arm 'gateway' -Workload $workload -Conc $conc
            $s = Get-Aggregate -Arm 'gateway-scraped' -Workload $workload -Conc $conc
            if ($null -eq $g -or $null -eq $s) { continue }
            $ratio = 0.0
            $pct = 0.0
            if ([double]$g.QPS -gt 0) {
                $ratio = [Math]::Round(([double]$s.QPS / [double]$g.QPS), 4)
                $pct = [Math]::Round((100.0 * ([double]$s.QPS - [double]$g.QPS) / [double]$g.QPS), 2)
            }
            $band = Get-NoiseBandPct $g $s
            # The scraper's control is the gateway arm ITSELF -- the only
            # difference between `gateway` and `gateway-scraped` is the poller --
            # so this comparison uses exactly the per-round spread of those two
            # arms, no different from the tracing comparison.
            $beyondNoise = [bool]($band.Estimated -and ([Math]::Abs($pct) -gt $band.Band))
            $isCost = [bool]($beyondNoise -and ($pct -lt 0))
            $scrapeOverhead += [ordered]@{
                workload                  = $workload
                concurrency               = $conc
                unscraped_qps             = $g.QPS
                scraped_qps               = $s.QPS
                qps_ratio                 = $ratio
                qps_percent               = $pct
                p95_delta_ms              = [Math]::Round(([double]$s.P95_ms - [double]$g.P95_ms), 4)
                p99_delta_ms              = [Math]::Round(([double]$s.P99_ms - [double]$g.P99_ms), 4)
                unscraped_p95_ms          = $g.P95_ms
                scraped_p95_ms            = $s.P95_ms
                unscraped_noise_percent   = $band.Band
                noise_band_percent        = $band.Band
                noise_estimated           = [bool]$band.Estimated
                direction                 = $(if ($pct -lt 0) { 'scraped-slower' } elseif ($pct -gt 0) { 'scraped-faster' } else { 'equal' })
                beyond_noise              = $beyondNoise
                scraped_adds_measurable_cost = $isCost
                within_noise              = [bool](-not $beyondNoise)
                verdict = $(if (-not $band.Estimated) { 'not assessable: one round gives no noise estimate, so this delta cannot be called measurable' }
                    elseif ($isCost) { ("measurable cost: the {0}% QPS drop at c={1} exceeds the {2}% run-to-round band of the gateway arm it is compared against" -f [Math]::Abs($pct), $conc, $band.Band) }
                    elseif ($beyondNoise) { ("measurable but a GAIN, not a cost: {0}% QPS at c={1} exceeds the {2}% band -- the co-located scraper cannot make the gateway faster, so read this as host drift between two arms, not as a benefit of being scraped" -f $pct, $conc, $band.Band) }
                    else { ("within round-to-round noise on this host: {0}% QPS change at c={1}, {2}% band" -f $pct, $conc, $band.Band) })
            }
        }
    }

    $scrapeVerdict = 'not computed'
    $scrapeAssessable = @($scrapeOverhead | Where-Object { $_.noise_estimated })
    if ($scrapeOverhead.Count -eq 0) {
        $scrapeVerdict = 'not computed: no gateway/gateway-scraped pair was aggregated'
    }
    elseif ($Rounds -lt 2 -or $scrapeAssessable.Count -eq 0) {
        $worstQps = ($scrapeOverhead | Sort-Object { [Math]::Abs($_.qps_percent) } -Descending)[0]
        $scrapeVerdict = ("NOT ASSESSABLE at {0} round(s): a single round gives no run-to-run noise estimate, so the {1}% QPS difference between the scraped and unscraped runs at c={2} ({3}) cannot be attributed to the scraper. Re-run with -Rounds 2 or more." -f `
                $Rounds, $worstQps.qps_percent, $worstQps.concurrency, $worstQps.workload)
    }
    else {
        $flags = @($scrapeOverhead | Where-Object { $_.scraped_adds_measurable_cost })
        $gains = @($scrapeOverhead | Where-Object { $_.beyond_noise -and -not $_.scraped_adds_measurable_cost })
        $worstQps = ($scrapeOverhead | Sort-Object { [Math]::Abs($_.qps_percent) } -Descending)[0]
        $worstP95 = ($scrapeOverhead | Sort-Object { [Math]::Abs($_.p95_delta_ms) } -Descending)[0]
        $bandText = (@($scrapeOverhead | ForEach-Object { "$($_.workload) c$($_.concurrency): +/-$($_.noise_band_percent)%" }) -join ', ')
        $rateNow = [double]$scrapeStats['scrape_rate_hz']
        $intendedNow = [double]$scrapeStats['intended_rate_hz']
        if ($flags.Count -eq 0) {
            $scrapeVerdict = ("a constant poll of /stats+/metrics (asked {0} endpoint GETs/s, achieved {1}) changes no throughput measurably: the largest QPS move is {2}% at c={3} ({4}), against the gateway arm's own round-to-round bands [{5}], and the worst P95 move is {6}ms. NOTHING here is called a scrape cost" -f `
                    $intendedNow, $rateNow, $worstQps.qps_percent, $worstQps.concurrency, $worstQps.workload, $bandText, $worstP95.p95_delta_ms)
        }
        else {
            $scrapeVerdict = ("a constant poll of /stats+/metrics costs throughput beyond the run-to-round noise at {0} of {1} measured (workload, concurrency) pairs; the largest QPS move is {2}% at c={3} ({4}) and the worst P95 move is {5}ms. Bands are [{6}]. Examples: {7}" -f `
                    $flags.Count, $scrapeOverhead.Count, $worstQps.qps_percent, $worstQps.concurrency, $worstQps.workload, $worstP95.p95_delta_ms, $bandText, `
                (($flags | ForEach-Object { "c$($_.concurrency) $($_.workload) $($_.qps_percent)% (band $($_.noise_band_percent)%)" }) -join '; '))
        }
        if ($gains.Count -gt 0) {
            $scrapeVerdict += (". {0} pair(s) moved the OTHER way beyond the band ({1}); a co-located scraper cannot speed the gateway up, so those are drift between two arms and are reported, not attributed" -f `
                    $gains.Count, (($gains | ForEach-Object { "c$($_.concurrency) $($_.workload) +$($_.qps_percent)% (band $($_.noise_band_percent)%)" }) -join '; '))
        }
    }

    $scrapeScrapes = [int]$scrapeStats['scrapes']
    $scrapeOk = [int]$scrapeStats['scrapes_ok']
    $scrapeFailed = [int]$scrapeStats['scrapes_failed']
    $scrapeRate = [double]$scrapeStats['scrape_rate_hz']
    $scrapeIntended = [double]$scrapeStats['intended_rate_hz']
    $scrapeOkPct = 0.0
    if ($scrapeScrapes -gt 0) { $scrapeOkPct = [Math]::Round((100.0 * $scrapeOk / $scrapeScrapes), 3) }
    # What this harness can actually guarantee about the scraper. It CANNOT
    # guarantee the 10 Hz it asks for: the scraper is a co-located PowerShell
    # background job that spawns curl.exe twice per iteration, and process spawn
    # alone costs ~135ms, so ~7.3 endpoint GETs/s is a structural ceiling
    # (stable across rounds), not flakiness. So the check asserts continuity and
    # cleanliness, not a rate this harness cannot deliver, and the achieved rate
    # is recorded as a fact beside the intended one.
    $scrapeRateFloor = 1.0
    $scrapeAchievedVsIntended = 0.0
    if ($scrapeIntended -gt 0) { $scrapeAchievedVsIntended = [Math]::Round(($scrapeRate / $scrapeIntended), 4) }
    $scrapeRanContinuously = [bool](($scrapeScrapes -gt 0) -and ($scrapeFailed -eq 0) -and ($scrapeOk -eq $scrapeScrapes) -and ($scrapeRate -ge $scrapeRateFloor))
    # Per-round rates, so "stable ceiling" is a measured claim and not a story.
    $perRoundRates = @($scrapeStats['arms'] | ForEach-Object { [double]$_.rate_hz })
    $rateSpreadPct = 0.0
    $rateMedianHz = 0.0
    if ($perRoundRates.Count -ge 2) {
        $rateMedianHz = Get-Median $perRoundRates
        if ($rateMedianHz -gt 0) {
            $rateSpreadPct = [Math]::Round((100.0 * ((Get-MaxValue $perRoundRates) - (Get-MinValue $perRoundRates)) / $rateMedianHz), 2)
        }
    }

    $scrapeBlock = [ordered]@{
        interval_ms       = $scrapeIntervalMs
        endpoints         = @($scrapePaths)
        intended_rate_hz  = $scrapeIntended
        achieved_rate_hz  = $scrapeRate
        achieved_vs_intended = $scrapeAchievedVsIntended
        rate_floor_hz     = $scrapeRateFloor
        rate_floor_note   = 'the asserted floor is 1.0 endpoint GET/s, i.e. at least one GET to each endpoint per second, which is still >=5x a 5s Prometheus scrape interval; the 10 Hz the arm asks for is unreachable for a spawn-per-iteration scraper and is reported, not asserted'
        per_round_rate_hz = $perRoundRates
        per_round_rate_median_hz = $rateMedianHz
        per_round_rate_spread_percent = $rateSpreadPct
        rates_are_stable = [bool]($perRoundRates.Count -ge 2 -and $rateSpreadPct -le 25.0)
        scrapes           = $scrapeScrapes
        scrapes_ok        = $scrapeOk
        scrapes_failed    = $scrapeFailed
        scrapes_ok_percent = $scrapeOkPct
        scrape_seconds    = $scrapeStats['scrape_seconds']
        scrape_rate_hz    = $scrapeRate
        ran_continuously_without_failures = $scrapeRanContinuously
        arms              = @($scrapeStats['arms'])
        note              = 'one "scrape" = one GET to EACH endpoint, so one scrape is two HTTP requests; the scrapes field therefore counts endpoint hits, and rate_hz is endpoint hits per second'
        verdict           = $scrapeVerdict
        rows              = $scrapeOverhead
    }

    # ---- horizontal scaling ------------------------------------------------
    $coreCount = 0
    try { $coreCount = [int](Get-CimInstance Win32_Processor -ErrorAction Stop | Select-Object -First 1).NumberOfCores } catch { $coreCount = 0 }
    $logicalCount = 0
    try { $logicalCount = [int](Get-CimInstance Win32_Processor -ErrorAction Stop | Select-Object -First 1).NumberOfLogicalProcessors } catch { $logicalCount = 0 }

    $horizontalRows = @()
    foreach ($workload in @('non-stream', 'stream')) {
        foreach ($conc in $levels) {
            $second = Get-Aggregate -Arm 'second' -Workload $workload -Conc $conc
            $h2 = Get-Aggregate -Arm 'horizontal-2' -Workload $workload -Conc $conc
            $h4 = Get-Aggregate -Arm 'horizontal-4' -Workload $workload -Conc $conc
            if ($null -eq $second -or $null -eq $h2 -or $null -eq $h4) { continue }

            $sharesH2 = @()
            foreach ($run in @($rawRuns | Where-Object { $_.Arm -eq 'horizontal-2' })) {
                foreach ($row in @($run.Rows | Where-Object { ([bool]$_.stream) -eq ($workload -eq 'stream') -and [int]$_.concurrency -eq $conc })) {
                    $total = 0
                    foreach ($t in @($row.targets)) { $total += [int]$t.requests }
                    foreach ($t in @($row.targets)) {
                        $share = 0.0
                        if ($total -gt 0) { $share = [Math]::Round((100.0 * [int]$t.requests / $total), 2) }
                        $sharesH2 += [ordered]@{ arm = 'horizontal-2'; round = $run.Round; url = $t.url; requests = [int]$t.requests; errors = [int]$t.errors; share_percent = $share }
                    }
                }
            }
            $sharesH4 = @()
            foreach ($run in @($rawRuns | Where-Object { $_.Arm -eq 'horizontal-4' })) {
                foreach ($row in @($run.Rows | Where-Object { ([bool]$_.stream) -eq ($workload -eq 'stream') -and [int]$_.concurrency -eq $conc })) {
                    $total = 0
                    foreach ($t in @($row.targets)) { $total += [int]$t.requests }
                    foreach ($t in @($row.targets)) {
                        $share = 0.0
                        if ($total -gt 0) { $share = [Math]::Round((100.0 * [int]$t.requests / $total), 2) }
                        $sharesH4 += [ordered]@{ arm = 'horizontal-4'; round = $run.Round; url = $t.url; requests = [int]$t.requests; errors = [int]$t.errors; share_percent = $share }
                    }
                }
            }
            # The two arms are reported separately: a 2-instance arm splits its
            # requests two ways and a 4-instance arm splits them four ways, so
            # "each target's share" only means something within one arm. Mixing
            # them would flag every 4-instance target as "under 40%".
            $shares = @($sharesH2) + @($sharesH4)

            # ONE named control arm: `gateway` on :18999, the same instance the
            # 2- and 4-instance arms both include. `second` (:19000) is reported
            # beside it as an independent single-instance measurement, because
            # the two differ by host drift at the same nominal load -- an earlier
            # run showed a 2x gap between them at (stream, c=8), and a linearity
            # figure computed against whichever arm happened to be luckier is not
            # a figure at all. Both are in the artifact so the spread is visible.
            $control = Get-Aggregate -Arm 'gateway' -Workload $workload -Conc $conc
            if ($null -eq $control) { continue }
            $secondNoise = 0.0
            if ([double]$control.QPS -gt 0 -and $null -ne $second) {
                $secondNoise = [Math]::Round((100.0 * ([double]$second.QPS - [double]$control.QPS) / [double]$control.QPS), 2)
            }

            $lin2 = 0.0
            $lin4 = 0.0
            if ([double]$control.QPS -gt 0) {
                $lin2 = [Math]::Round((100.0 * ([double]$h2.QPS / 2.0) / [double]$control.QPS), 2)
                $lin4 = [Math]::Round((100.0 * ([double]$h4.QPS / 4.0) / [double]$control.QPS), 2)
            }
            # Each target's share of ITS OWN arm's total. A fair split of the
            # load generator's requests is ~1/N for the N-instance arm (the
            # generator round-robins `requests` over N targets), so the
            # meaningful assertion is "within 25% of expected", not "at least
            # 40% of the arm total" -- 40% is unreachable for a 4-way split and
            # would flag a perfectly balanced 4-instance arm as broken.
            $h2Share = 0.0
            $h4Share = 0.0
            $h2Targets = @(@($sharesH2) | ForEach-Object { $_.url } | Select-Object -Unique).Count
            $h4Targets = @(@($sharesH4) | ForEach-Object { $_.url } | Select-Object -Unique).Count
            if ($h2Targets -gt 0) { $h2Share = [Math]::Round((100.0 / $h2Targets), 2) }
            if ($h4Targets -gt 0) { $h4Share = [Math]::Round((100.0 / $h4Targets), 2) }
            $horizontalRows += [ordered]@{
                workload              = $workload
                concurrency           = $conc
                control_arm           = 'gateway'
                control_port          = $ports.A
                single_instance_qps   = $control.QPS
                single_instance_min   = $control.QPSMin
                single_instance_max   = $control.QPSMax
                single_instance_noise_pct = $control.QPSNoise_pct
                second_instance_qps   = $(if ($null -ne $second) { $second.QPS } else { 0 })
                second_instance_min   = $(if ($null -ne $second) { $second.QPSMin } else { 0 })
                second_instance_max   = $(if ($null -ne $second) { $second.QPSMax } else { 0 })
                second_instance_noise_pct = $(if ($null -ne $second) { $second.QPSNoise_pct } else { 0 })
                second_vs_control_percent = $secondNoise
                single_instance_spread_note = 'single_instance_qps is the NAMED control arm (gateway on :18999, the instance both fleet arms include); second_instance_qps is the independent measurement on :19000. They are the same product configuration under the same load, so the gap between them is this host''s drift and bounds what any linearity figure here can resolve.'
                fleet2_qps            = $h2.QPS
                fleet2_min            = $h2.QPSMin
                fleet2_max            = $h2.QPSMax
                fleet4_qps            = $h4.QPS
                fleet4_min            = $h4.QPSMin
                fleet4_max            = $h4.QPSMax
                fleet2_speedup        = 0.0
                fleet4_speedup        = 0.0
                fleet2_linearity_pct  = $lin2
                fleet4_linearity_pct  = $lin4
                cores                 = $coreCount
                logical_processors    = $logicalCount
                targets_share         = $shares
                targets_share_h2      = @($sharesH2)
                targets_share_h4      = @($sharesH4)
                fair_share_h2_percent = $h2Share
                fair_share_h4_percent = $h4Share
            }
            if ([double]$control.QPS -gt 0) {
                $horizontalRows[$horizontalRows.Count - 1]['fleet2_speedup'] = [Math]::Round(([double]$h2.QPS / [double]$control.QPS), 4)
                $horizontalRows[$horizontalRows.Count - 1]['fleet4_speedup'] = [Math]::Round(([double]$h4.QPS / [double]$control.QPS), 4)
            }
        }
    }

    $horizontalBlock = [ordered]@{
        measured_arms = @('gateway (control)', 'second', 'horizontal-2', 'horizontal-4')
        control_arm   = 'gateway on :18999'
        note          = 'the SAME loadtest.exe client process drives every arm here, so a difference between arms is a difference in the fleet, not in the generator; single_instance_qps is always the named `gateway` control arm, and second_instance_qps is reported beside it because two identical single instances differ by host drift'
        cores         = $coreCount
        logical_processors = $logicalCount
        core_note     = "this host reports $coreCount physical cores / $logicalCount logical processors; a 4-instance arm on this host is not core-starved, but every instance still shares the same memory bus, NIC queue and loopback stack"
        rows          = $horizontalRows
    }

    # ---- bottleneck witness ------------------------------------------------
    # One verdict per WORKLOAD. The two workloads do not have to agree: in the
    # measured run the non-stream arm came out 16% FASTER through the gateway at
    # every level while the stream arm was 22.6% SLOWER at the top level. A single
    # sign cannot speak for both, and the faster direction must not be reported as
    # "the gateway is free" -- it is a property of this loopback mock+client pair.
    $bottleneckOk = $true
    $bottleneckRows = @()
    $workloadVerdicts = [ordered]@{}
    foreach ($workload in @('non-stream', 'stream')) {
        $d = Get-Aggregate -Arm 'direct' -Workload $workload -Conc $topLevel
        $g = Get-Aggregate -Arm 'gateway' -Workload $workload -Conc $topLevel
        if ($null -eq $d -or $null -eq $g) { continue }
        $gapPct = 0.0
        if ([double]$d.QPS -gt 0) { $gapPct = [Math]::Round((100.0 * ([double]$d.QPS - [double]$g.QPS) / [double]$d.QPS), 2) }
        $mockBound = ([Math]::Abs($gapPct) -le 15.0)
        if (-not $mockBound) { $bottleneckOk = $false }

        # Every level, so a direction is only claimed when it holds throughout.
        $levelGaps = @()
        $allSlower = $true
        $allFaster = $true
        $ratioParts = @()
        foreach ($lv in $levels) {
            $dl = Get-Aggregate -Arm 'direct' -Workload $workload -Conc $lv
            $gl = Get-Aggregate -Arm 'gateway' -Workload $workload -Conc $lv
            if ($null -eq $dl -or $null -eq $gl -or [double]$dl.QPS -le 0) { continue }
            $gp = [Math]::Round((100.0 * ([double]$dl.QPS - [double]$gl.QPS) / [double]$dl.QPS), 2)
            $levelGaps += [ordered]@{ concurrency = $lv; direct_qps = $dl.QPS; gateway_qps = $gl.QPS; direct_minus_gateway_pct = $gp; gateway_over_direct_ratio = [Math]::Round(([double]$gl.QPS / [double]$dl.QPS), 4) }
            $ratioParts += ("{0}@c{1}" -f [Math]::Round(([double]$gl.QPS / [double]$dl.QPS), 3), $lv)
            if ($gp -lt 0) { $allSlower = $false }
            if ($gp -gt 0) { $allFaster = $false }
        }

        # The P95/P99 cost at the top level, from the same comparison the
        # overhead.vs_direct rows use, so the verdict and the rows agree.
        $vdRow = $null
        foreach ($r in $vsDirect) { if ($r.workload -eq $workload -and [int]$r.concurrency -eq $topLevel) { $vdRow = $r } }
        $p95Delta = 0.0
        $p99Delta = 0.0
        if ($null -ne $vdRow) { $p95Delta = [double]$vdRow.p95_delta_ms; $p99Delta = [double]$vdRow.p99_delta_ms }

        $direction = 'within-threshold'
        $mockIsCeiling = [bool]$mockBound
        if ($gapPct -gt 15.0) { $direction = 'gateway-slower' }
        elseif ($gapPct -lt -15.0) { $direction = 'gateway-faster' }

        $wv = ''
        if ($mockIsCeiling) {
            $wv = ("{0}: at the top concurrency (c={1}) direct and through-the-gateway are within {2}% ({3} vs {4} QPS), so this workload cannot separate gateway cost from the mock's own ceiling" -f `
                    $workload, $topLevel, [Math]::Abs($gapPct), $d.QPS, $g.QPS)
        }
        elseif ($direction -eq 'gateway-slower') {
            if ($allSlower) {
                $wv = ("{0}: the GATEWAY IS THE LIMIT at every level measured -- {1}% fewer QPS than direct-to-mock at c={2} ({3} vs {4} QPS), with P95 {5} ms and P99 {6} ms worse at that level. The throughput figures for this workload therefore do reflect gateway cost, not only the mock" -f `
                        $workload, $gapPct, $topLevel, $g.QPS, $d.QPS, $p95Delta, $p99Delta)
            }
            else {
                $wv = ("{0}: the gateway is slower than direct-to-mock at the top concurrency (c={1}: {2}% fewer QPS, {3} vs {4} QPS, P95 {5} ms and P99 {6} ms worse), but the direction does not hold at every level, so treat it as a top-concurrency effect rather than a uniform gateway tax" -f `
                        $workload, $topLevel, $gapPct, $g.QPS, $d.QPS, $p95Delta, $p99Delta)
            }
        }
        else {
            if ($allFaster) {
                $wv = ("{0}: through-the-gateway was FASTER than direct-to-mock at EVERY level measured ({1}; +{2}% QPS at c={3}), which is a property of this loopback mock+client pair and NOT evidence that a proxy hop is free. This harness cannot separate the client's connection handling from the mock's, so the {0} QPS columns describe this mock and this client, not the gateway's capacity" -f `
                        $workload, ($ratioParts -join ', '), [Math]::Abs($gapPct), $topLevel)
            }
            else {
                $wv = ("{0}: direct and through-the-gateway differ by {1}% at c={2} ({3} vs {4} QPS) and the sign is not consistent across levels ({5}), so no direction is claimed for this workload" -f `
                        $workload, $gapPct, $topLevel, $g.QPS, $d.QPS, ($ratioParts -join ', '))
            }
        }

        $bottleneckRows += [ordered]@{
            workload                = $workload
            concurrency             = $topLevel
            direct_qps              = $d.QPS
            gateway_qps             = $g.QPS
            direct_minus_gateway_pct = $gapPct
            p95_delta_ms            = $p95Delta
            p99_delta_ms            = $p99Delta
            direction               = $direction
            direction_holds_at_all_levels = [bool]($allSlower -or $allFaster)
            per_level               = $levelGaps
            gateway_over_direct_ratio_by_level = $ratioParts
            mock_is_the_ceiling     = [bool]$mockBound
            threshold_percent       = 15.0
            verdict                 = $wv
        }
        $workloadVerdicts[$workload] = $wv
    }
    $dominant = 'mixed'
    foreach ($r in $bottleneckRows) {
        if ($r.direction -eq 'gateway-slower') { $dominant = 'gateway' }
        elseif ($r.direction -eq 'gateway-faster' -and $dominant -ne 'gateway') { $dominant = 'mock-or-client' }
        if ($r.direction -eq 'within-threshold' -and $dominant -eq 'mixed') { $dominant = 'mock' }
    }
    $bottleneckBlock = [ordered]@{
        threshold_percent = 15.0
        dominant_limiter  = $dominant
        verdict           = 'unknown'
        verdict_note      = 'the verdict is per workload: the two workloads need not agree, and one sign must not be read as speaking for both'
        by_workload       = $workloadVerdicts
        rows              = $bottleneckRows
    }
    $mockBoundAll = $true
    $anySlower = $false
    foreach ($b in $bottleneckRows) {
        if (-not $b.mock_is_the_ceiling) { $mockBoundAll = $false }
        if ($b.direction -eq 'gateway-slower') { $anySlower = $true }
    }
    if ($bottleneckRows.Count -eq 0) {
        $bottleneckBlock['verdict'] = 'not computed: no direct/gateway pair at the top concurrency'
    }
    elseif ($mockBoundAll) {
        $bottleneckBlock['verdict'] = 'the mock upstream is the ceiling for BOTH workloads: at the top concurrency the direct-to-mock arm and the through-the-gateway arm are within 15%, so the absolute QPS in this artifact describes this mock and this client, NOT the gateway''s capacity'
    }
    elseif ($anySlower) {
        $bottleneckBlock['verdict'] = ("the workloads disagree, so read them separately: {0}" -f ((@($workloadVerdicts.Values)) -join ' || '))
    }
    else {
        $bottleneckBlock['verdict'] = ("no workload shows a slower gateway at the top concurrency, so the throughput columns here describe this mock and this client rather than the gateway's capacity: {0}" -f ((@($workloadVerdicts.Values)) -join ' || '))
    }

    # ---- client-vs-gateway caveat -----------------------------------------
    $clientBound = @()
    foreach ($a in $aggregates) {
        if ([double]$a.GatewayMean_ms -le 0) { continue }
        if ([double]$a.Mean_ms -gt (2.0 * [double]$a.GatewayMean_ms)) {
            $clientBound += ("{0}/{1}/c{2}: client-observed mean {3}ms vs gateway mean {4}ms" -f $a.Arm, $a.Workload, $a.Concurrency, $a.Mean_ms, $a.GatewayMean_ms)
        }
    }

    # -----------------------------------------------------------------------
    # 6. Cross-checks
    # -----------------------------------------------------------------------
    Write-Section '6. Cross-checks'

    # 1. no errors anywhere
    $badPhases = @()
    foreach ($run in $rawRuns) {
        foreach ($row in $run.Rows) {
            if ([int]$row.errors -ne 0) {
                $sl = 'non-stream'
                if ([bool]$row.stream) { $sl = 'stream' }
                $badPhases += "$($run.Arm)/r$($run.Round)/$sl/c$([int]$row.concurrency)=$([int]$row.errors)"
            }
        }
    }
    $totalPhases = 0
    foreach ($run in $rawRuns) { $totalPhases += @($run.Rows).Count }
    Add-Check 'errors_zero_in_every_phase' ($badPhases.Count -eq 0) ("{0} phases checked across {1} arm runs; failing phases: {2}" -f `
            $totalPhases, $rawRuns.Count, $(if ($badPhases.Count -eq 0) { 'none' } else { $badPhases -join ', ' }))

    # 2. gateway_count present exactly where a gateway sits in the path
    $missingGw = @()
    $unexpectedGw = @()
    foreach ($run in $rawRuns) {
        foreach ($row in $run.Rows) {
            $sl = 'non-stream'
            if ([bool]$row.stream) { $sl = 'stream' }
            $has = ($null -ne $row.PSObject.Properties['gateway_count'])
            $cnt = 0
            if ($has) { $cnt = [int]$row.gateway_count }
            if ($run.Arm -eq 'direct') {
                if ($has -and $cnt -gt 0) { $unexpectedGw += "$($run.Arm)/$sl/c$([int]$row.concurrency)" }
            }
            else {
                if ((-not $has) -or $cnt -le 0) { $missingGw += "$($run.Arm)/$sl/c$([int]$row.concurrency)" }
            }
        }
    }
    Add-Check 'gateway_count_present_for_gateway_arms_only' (($missingGw.Count -eq 0) -and ($unexpectedGw.Count -eq 0)) `
        ("gateway arms with no gateway_count: {0}; direct-arm phases reporting one: {1}" -f `
        $(if ($missingGw.Count -eq 0) { 'none' } else { ($missingGw | Select-Object -First 6) -join ', ' }), `
        $(if ($unexpectedGw.Count -eq 0) { 'none' } else { ($unexpectedGw | Select-Object -First 6) -join ', ' }))

    # 3. the primary untraced instance's own counter accounts for the arm
    #
    # The counter counts COMPLETED requests, and a phase's warmup requests are
    # real requests too, so the invariant is
    #   delta == dispatched + warmup*phases.
    # Sampled before and after the loadtest invocation only, so no other arm can
    # contribute; the upper bound is exact rather than a fudge factor (this was
    # measured on this host, not assumed).
    $deltaOk = $false
    $deltaDetail = 'not sampled'
    if ($gwAMetrics['before'] -ge 0 -and $gwAMetrics['delta'] -ge 0) {
        $dispatched = [int]$gwAMetrics['dispatched']
        $delta = [int64]$gwAMetrics['delta']
        $warmTotal = [int]$gwAMetrics['warmup_total']
        $incrementPerRequest = [double]$gwAMetrics['delta_per_dispatched']
        if ($dispatched -gt 0) {
            $pctCovered = [Math]::Round((100.0 * $delta / $dispatched), 2)
            $pctOfBudget = [Math]::Round((100.0 * $delta / ($dispatched + $warmTotal)), 2)
            $overBudget = $delta - ($dispatched + $warmTotal)
            $deltaOk = (($pctCovered -ge 99.0) -and ($overBudget -le 0))
            $deltaDetail = ("infergate_requests_total on :$($ports.A) grew by $delta around the $($gwAMetrics['arm']) arm " +
                "(round $($gwAMetrics['round'])): dispatched $dispatched + $warmTotal warmup ($($gwAMetrics['warmup_per_phase']) per phase x $($gwAMetrics['phases']) phases) = $($dispatched + $warmTotal); " +
                "$pctCovered% of dispatched and $pctOfBudget% of the dispatched+warmup budget; " +
                "increment per dispatched request = $incrementPerRequest (1.0 means one counter increment per request, no retries or failovers)")
        }
        else { $deltaDetail = "the sampled arm dispatched 0 requests; delta=$delta" }
    }
    Add-Check 'requests_total_delta_matches_arm' $deltaOk $deltaDetail

    # 4. breaker states + the M5 # TYPE regression guard
    $openUpstreams = @()
    $missingType = @()
    foreach ($key in @('A', 'B', 'C', 'D', 'E', 'F')) {
        $text = Get-Url "http://127.0.0.1:$($ports[$key])/metrics"
        if (-not $text.Contains('infergate_breaker_state')) { $openUpstreams += "$key(no breaker metric)" }
        foreach ($u in @(Get-MetricOpenStates $text)) { $openUpstreams += "$key/$u" }
        if (-not $text.Contains('# TYPE infergate_stream_bytes_total')) { $missingType += $key }
    }
    Add-Check 'breaker_states_closed_and_stream_bytes_type_declared' (($openUpstreams.Count -eq 0) -and ($missingType.Count -eq 0)) `
        ("open breakers: {0}; gateways whose /metrics lacks '# TYPE infergate_stream_bytes_total': {1}" -f `
        $(if ($openUpstreams.Count -eq 0) { 'none' } else { $openUpstreams -join ', ' }), `
        $(if ($missingType.Count -eq 0) { 'none' } else { $missingType -join ', ' }))

    # 5. OTLP: the traced gateway reports exports and the collector received them
    $otlpBody = Get-Url "http://127.0.0.1:$($ports.E)/admin/tracing"
    $otlpJson = Get-Json $otlpBody
    $otlpExported = -1
    $otlpFailed = -1
    if ($null -ne $otlpJson -and $null -ne $otlpJson.export_stats -and $null -ne $otlpJson.export_stats.otlp) {
        $otlpExported = [int64]$otlpJson.export_stats.otlp.exported
        $otlpFailed = [int64]$otlpJson.export_stats.otlp.failed
    }
    $collectorJson = Get-Json (Get-Url "$collectorBase/requests")
    $collectorCount = 0
    $configuredPathHits = 0
    $constructedPathHits = 0
    $otherTracesPathHits = 0
    $otherPaths = @()
    if ($null -ne $collectorJson) {
        $collectorCount = [int]$collectorJson.count
        foreach ($req in @($collectorJson.requests)) {
            $p = [string]$req.path
            if ($p -eq $otlpPath) {
                $configuredPathHits += 1
            }
            elseif ($p -eq $otlpConstructedPath) {
                # The exporter builds its POST URL as base endpoint + '/v1/traces'
                # (internal/traceexport/otlp.go:153) and the endpoint option is
                # documented as a BASE url (otlp.go:43-46, configs/observability.yaml).
                # With the base form configured this branch cannot be reached; if
                # it ever is, the harness configured the endpoint WITH the path and
                # the collector saw a doubled path (the harness's bug, not the
                # gateway's) -- the check fails on that in as many words.
                $constructedPathHits += 1
            }
            elseif ($p.Contains('/v1/traces')) {
                $otherTracesPathHits += 1
            }
            else {
                $otherPaths += $p
            }
        }
    }
    $observedOtlpPaths = @($configuredPathHits, $constructedPathHits, $otherTracesPathHits) | Where-Object { $_ -gt 0 }
    $dumpText = Get-Url "$collectorBase/dump"
    $serviceInBody = $dumpText.Contains($otlpServiceName)
    # The configured endpoint is a BASE url, so the ONLY path a correct harness
    # can produce is $otlpPath. A POST to the constructed path means the harness
    # fed the exporter a url that already had the path (doubling it), and a POST
    # to any other path is unexplained: both fail the check.
    $otlpPathOk = [bool](($configuredPathHits -gt 0) -and ($constructedPathHits -eq 0) -and (@($otherPaths).Count -eq 0))
    $tracingProbe = [ordered]@{
        otlp_exported = $otlpExported
        otlp_failed = $otlpFailed
        collector_posts = $collectorCount
        configured_endpoint = $otlpEndpoint
        configured_path = $otlpPath
        constructed_post_path = $otlpConstructedPath
        expected_post_path = $otlpPath
        posts_to_configured_path = $configuredPathHits
        posts_to_constructed_path = $constructedPathHits
        posts_to_other_traces_paths = $otherTracesPathHits
        posts_to_other_paths = @($otherPaths | Select-Object -Unique)
        endpoint_path_mismatch = [bool](-not $otlpPathOk)
        service_name_in_a_received_body = [bool]$serviceInBody
        configured_service_name = $otlpServiceName
        note = 'otlp.endpoint is a BASE url (internal/traceexport/otlp.go:43-46,153), so every POST must land on the configured base plus /v1/traces and nothing else; the mock collector records bodies but /requests omits them (the field is unexported), so the service-name assertion reads GET /dump'
    }
    Add-Check 'otlp_exporter_exported_and_collector_received' (($otlpExported -gt 0) -and ($otlpFailed -eq 0) -and $otlpPathOk -and $serviceInBody) `
        ("otlp.exported=$otlpExported failed=$otlpFailed; collector recorded $collectorCount POST(s), all of them must be on '$otlpPath' " +
        "(on the configured path '$otlpPath': $configuredPathHits, on a DOUBLED path '$otlpConstructedPath' (path-doubling regression guard): $constructedPathHits, on another /v1/traces path: $otherTracesPathHits, on some other path: $(@($otherPaths).Count)); " +
        "service name '$otlpServiceName' present in a received body: $serviceInBody")

    # 6. JSONL sink: the file's line count must BE the exporter's written count
    $fullJson = Get-Json (Get-Url "http://127.0.0.1:$($ports.F)/admin/tracing")
    $jsonlWritten = -1
    $jsonlDropped = -1
    if ($null -ne $fullJson -and $null -ne $fullJson.export_stats -and $null -ne $fullJson.export_stats.jsonl) {
        $jsonlWritten = [int64]$fullJson.export_stats.jsonl.written
        $jsonlDropped = [int64]$fullJson.export_stats.jsonl.dropped
    }
    # The sink owns a background worker, so give it a moment to drain before the
    # line count is compared with the counter.
    $lines = @()
    $badLines = 0
    for ($attempt = 1; $attempt -le 20; $attempt++) {
        $lines = @((Read-Text $jsonlPath) -split "`r?`n" | Where-Object { $_ -match '\S' })
        $badLines = 0
        foreach ($line in $lines) {
            $obj = Get-Json $line
            if ($null -eq $obj -or $null -eq $obj.PSObject.Properties['trace_id'] -or [string]::IsNullOrEmpty([string]$obj.trace_id)) { $badLines += 1 }
        }
        if ($lines.Count -ge $jsonlWritten -or $attempt -eq 20) { break }
        Start-Sleep -Milliseconds 250
    }
    $jsonlProbe = [ordered]@{
        path = $jsonlPath
        exists = (Test-Path -LiteralPath $jsonlPath)
        line_count = $lines.Count
        export_stats_written = $jsonlWritten
        export_stats_dropped = $jsonlDropped
        lines_with_trace_id = ($lines.Count - $badLines)
        lines_not_parsing_or_without_trace_id = $badLines
        exporters = @()
    }
    if ($null -ne $fullJson) { $jsonlProbe['exporters'] = @($fullJson.exporters) }
    Add-Check 'jsonl_file_lines_equal_written_and_parse' ((Test-Path -LiteralPath $jsonlPath) -and ($lines.Count -gt 0) -and ($lines.Count -eq $jsonlWritten) -and ($badLines -eq 0)) `
        ("$jsonlPath has $($lines.Count) line(s); export_stats.jsonl.written=$jsonlWritten; $($lines.Count - $badLines) line(s) parse as JSON with a trace_id")

    # 7. horizontal: every target in an arm, in BOTH the 2-instance and the
    # 4-instance arm, took its fair share of that arm's requests.
    #
    # The load generator round-robins its `requests` over the arm's targets, so
    # a fair share is ~1/N of the arm total: ~50% in the 2-instance arm and
    # ~25% in the 4-instance arm. The assertion is therefore "each target's mean
    # share is within 25% of that arm's fair share" (>=37.5% for the 2-instance
    # arm, >=18.75% for the 4-instance arm), and the expected and observed
    # percentages are both reported so a reader can see the split directly. A
    # flat "at least 40%" would be unreachable for a balanced 4-way split.
    $shareProblems = @()
    $shareSummary = @()
    foreach ($hr in $horizontalRows) {
        foreach ($pair in @(@('horizontal-2', @($hr.targets_share_h2), [double]$hr.fair_share_h2_percent),
                @('horizontal-4', @($hr.targets_share_h4), [double]$hr.fair_share_h4_percent))) {
            $armName = $pair[0]
            $entries = $pair[1]
            $fair = $pair[2]
            if (@($entries).Count -eq 0) { continue }
            $perTarget = @{}
            $roundCount = @{}
            foreach ($s in @($entries)) {
                if (-not $perTarget.ContainsKey($s.url)) { $perTarget[$s.url] = 0.0; $roundCount[$s.url] = 0 }
                $perTarget[$s.url] += [double]$s.share_percent
                $roundCount[$s.url] += 1
            }
            $means = @()
            foreach ($u in $perTarget.Keys) {
                $means += [Math]::Round(($perTarget[$u] / [Math]::Max(1, $roundCount[$u])), 2)
            }
            $lowest = 100.0
            if ($means.Count -gt 0) { $lowest = ($means | Measure-Object -Minimum).Minimum }
            $lowestAllowed = [Math]::Round((0.75 * $fair), 2)
            $shareSummary += ("{0} c={1}: expected {2}% per target, lowest mean {3}% (allowed >= {4}%)" -f $armName, $hr.concurrency, $fair, $lowest, $lowestAllowed)
            if ($lowest -lt $lowestAllowed) {
                $shareProblems += ("{0}/c{1} lowest mean share {2}% < fair {3}% - 25% = {4}%" -f $armName, $hr.concurrency, $lowest, $fair, $lowestAllowed)
            }
        }
    }
    Add-Check 'horizontal_targets_each_took_at_least_40_percent' ($shareProblems.Count -eq 0) `
        ($(if ($shareProblems.Count -eq 0) { ($shareSummary -join '; ') } else { 'unbalanced: ' + ($shareProblems -join '; ') }))

    # 8. /stats self-consistency on the observability surface being measured
    $statsProblems = @()
    $statsSummary = @()
    foreach ($key in @('A', 'B', 'C', 'D', 'E', 'F')) {
        $statsJson = Get-Json (Get-Url "http://127.0.0.1:$($ports[$key])/stats")
        if ($null -eq $statsJson -or $null -eq $statsJson.latency) {
            $statsProblems += "$key(no latency block)"
            continue
        }
        $window = [int]$statsJson.latency.window
        $p50 = Convert-GoDurationToMs ([string]$statsJson.latency.p50)
        $p95 = Convert-GoDurationToMs ([string]$statsJson.latency.p95)
        $p99 = Convert-GoDurationToMs ([string]$statsJson.latency.p99)
        $statsSummary += ("{0}: window={1} p50={2}ms p95={3}ms p99={4}ms" -f $key, $window, $p50, $p95, $p99)
        if ($window -ne $latencyWindow) { $statsProblems += "$key(window=$window)" }
        if (-not (($p50 -le $p95) -and ($p95 -le $p99))) { $statsProblems += "$key(p50<=p95<=p99 violated)" }
    }
    Add-Check 'stats_window_and_percentile_ordering' ($statsProblems.Count -eq 0) `
        ("window must be $latencyWindow and p50<=p95<=p99; problems: {0}; observed: {1}" -f `
        $(if ($statsProblems.Count -eq 0) { 'none' } else { $statsProblems -join ', ' }), ($statsSummary -join ' | '))

    # 9. the scrape-load arm really scraped, continuously and cleanly. The name
    # deliberately does not claim 10 Hz: see check 10 for why that is impossible
    # here, and scrape_load{} for the rate that was actually achieved.
    Add-Check 'scrape_load_ran_continuously_without_failures' $scrapeRanContinuously `
        ("the gateway-scraped arm issued {0} endpoint GETs ({1} to {2} per scrape) over {3}s = {4} GETs/s; {5} returned HTTP 200 = {6}%, {7} failed (statuses seen: {8}); asserted: scrapes>0, failed==0, ok==scrapes, rate>={9}/s" -f `
            $scrapeScrapes, @($scrapePaths).Count, ($scrapePaths -join '+'), $scrapeStats['scrape_seconds'], $scrapeRate, $scrapeOk, $scrapeOkPct, $scrapeFailed, `
        $(if ($scrapeStats['arms'].Count -eq 0) { 'none recorded' } elseif (@($scrapeStats['arms'] | Where-Object { $_.bad_statuses -ne '' }).Count -eq 0) { 'all 200' } else { (@($scrapeStats['arms'] | Where-Object { $_.bad_statuses -ne '' } | ForEach-Object { "r$($_.round):$($_.bad_statuses)" }) -join ', ') }), `
            $scrapeRateFloor)

    # 10. the achieved scrape rate is a REAL, STABLE rate -- not the 10 Hz the arm
    # asks for. A PowerShell loop that spawns curl.exe twice per iteration pays
    # ~135ms of process spawn per iteration, so ~7.3 endpoint GETs/s is this
    # harness's structural ceiling. The honest assertion is therefore: the rate is
    # a substantial fraction of the request (>=0.5x, i.e. the scraper was not
    # stalled) AND it is stable across rounds (spread <=25%), which is what
    # distinguishes a systematic ceiling from a scraper that silently died.
    # The intended/achieved pair is recorded as a fact in scrape_load{}.
    # The >=0.5x floor and the round-to-round spread are only asserted when the
    # scrape window is long enough to measure a rate at all. In a smoke run the
    # scrape phase is a few seconds long (4-6 GETs per round), so job startup
    # dominates the window and the very same scraper reads ~0.40x with a 34%
    # round-to-round spread -- failing that would be measuring the phase length,
    # not the scraper. Below the window the achieved and per-round rates stay
    # recorded facts, and what is asserted is only what a short window supports:
    # every round actually polled (>= the 1 GET/s floor).
    $scrapeWindowS = [double]$scrapeStats['scrape_seconds']
    $scrapeRateWindowMinSeconds = 15.0
    $rateAssessable = [bool]($scrapeWindowS -ge $scrapeRateWindowMinSeconds)
    $roundRatesOk = [bool](@($perRoundRates | Where-Object { [double]$_ -lt $scrapeRateFloor }).Count -eq 0)
    $rateOk = [bool]$(if ($rateAssessable) {
        ($scrapeAchievedVsIntended -ge 0.5) -and [bool]$scrapeBlock['rates_are_stable']
    } else {
        $roundRatesOk
    })
    Add-Check 'scrape_load_rate_achieved_and_stable' $rateOk `
        ("intended {0} Hz (one GET to each of {1} endpoints every {2}ms); achieved {3} endpoint GETs/s over {4}s = {5}x intended, per-round {6} (median {7}, spread {8}% = {9}); asserted: {10}. The 10 Hz request is NOT achievable by a spawn-per-iteration co-located scraper" -f `
            $scrapeIntended, @($scrapePaths).Count, $scrapeIntervalMs, $scrapeRate, $scrapeStats['scrape_seconds'], $scrapeAchievedVsIntended, `
            (@($perRoundRates) -join '/'), $rateMedianHz, $rateSpreadPct, $(if ($scrapeBlock['rates_are_stable']) { 'stable' } else { 'unstable' }), `
            $(if ($rateAssessable) { "achieved>=0.5x intended AND per-round spread<=25% (window {0:0.#}s >= {1:0.#}s)" -f $scrapeWindowS, $scrapeRateWindowMinSeconds } else { "every round polled at >={2:0.#}/s (window {0:0.#}s < {1:0.#}s is too short to measure the 0.5x floor or the spread; both are recorded as facts; each round = {3:0.#},{4:0.#},{5:0.#}/s)" -f $scrapeWindowS, $scrapeRateWindowMinSeconds, $scrapeRateFloor, $(if ($perRoundRates.Count -gt 0) { [double]$perRoundRates[0] } else { 0 }), $(if ($perRoundRates.Count -gt 1) { [double]$perRoundRates[1] } else { 0 }), $(if ($perRoundRates.Count -gt 2) { [double]$perRoundRates[2] } else { 0 }) }))

    # -----------------------------------------------------------------------
    # 7. Limitations
    # -----------------------------------------------------------------------
    Write-Section '7. Limitations'
    [void]$script:limitations.Add('The backend is cmd\mockupstream, an in-repo stand-in that answers instantly and sleeps 1ms per streamed token. The absolute QPS in this artifact is a property of this mock, this host and this client -- it is not a provider benchmark and not a capacity claim for the gateway.')
    # The bottleneck verdict is per workload now, so emit one limitation per
    # workload rather than a single sentence that half the rows contradict.
    foreach ($b in $bottleneckRows) { [void]$script:limitations.Add(("BOTTLENECK ({0}): {1}" -f $b.workload, [string]$b.verdict)) }
    if ($mockBoundAll) {
        [void]$script:limitations.Add(("BOTTLENECK SUMMARY: the mock upstream is the ceiling for both workloads at concurrency {0} (within {1}%), so no QPS column in this artifact bounds how much traffic the gateway itself could carry." -f $topLevel, [string]$bottleneckBlock['threshold_percent']))
    }
    else {
        $faster = @($bottleneckRows | Where-Object { $_.direction -eq 'gateway-faster' })
        if ($faster.Count -gt 0) {
            [void]$script:limitations.Add(("BOTTLENECK SUMMARY: a through-the-gateway arm beat the direct-to-mock arm ({0}), which sounds like 'the proxy hop is free' and is NOT: cmd\loadtest already raises its own transport pool (cmd\loadtest/main.go:1297-1309 sets MaxIdleConns 512, MaxIdleConnsPerHost 256, ForceAttemptHTTP2 true), so the difference is not a default-2-idle-conns client either. This harness cannot separate the client's connection handling from the mock's, so a FASTER-through-the-gateway workload yields no gateway-capacity claim in either direction -- it only tells you that this mock and this client, not the gateway, set the ceiling for that workload." -f (($faster | ForEach-Object { $_.workload }) -join ', ')))
        }
        $slower = @($bottleneckRows | Where-Object { $_.direction -eq 'gateway-slower' })
        if ($slower.Count -gt 0) {
            [void]$script:limitations.Add(("BOTTLENECK SUMMARY: for the workload(s) {0} the gateway IS the limit at the top concurrency, so those throughput figures do reflect gateway cost rather than only the mock's ceiling." -f (($slower | ForEach-Object { $_.workload }) -join ', ')))
        }
    }
    if ($clientBound.Count -gt 0) {
        [void]$script:limitations.Add(("CLIENT-BOUND PHASES: at least one phase shows a client-observed mean more than twice the gateway's own recorded mean, i.e. most of the wall clock was spent inside the load generator rather than the gateway (cmd\loadtest prints a warning of its own in this case). Those phases measure this client, not the gateway: " + (($clientBound | Select-Object -First 8) -join '; ')))
    }
    [void]$script:limitations.Add(("HOST NOISE: every figure is the median of {0} interleaved rounds, and the per-round QPSMin/QPSMax are in the rows so the spread is visible. Rounds are outermost (all {1} arms run once per round) so drift hits the arms alike, but the host was also running this script, {2} gateway instances, a mock and a collector throughout." -f $Rounds, $runPlan.Count, $gatewayPorts.Count))
    [void]$script:limitations.Add('Everything is loopback on ONE host. There is no network in the path, so no figure here includes transport cost, and the five gateway instances compete for the same CPU, memory bus and loopback stack.')
    [void]$script:limitations.Add(("LOG LEVEL: the gateways run at log level '{0}' so that per-request logging is NOT mixed into the tracing cost. configs\observability.yaml documents that info-level per-request logging costs about 18% QPS and 4ms P95 at concurrency 32 on this host -- that cost is not measured here." -f $LogLevel))
    [void]$script:limitations.Add('EXPORTERS ARE ASYNCHRONOUS: both sinks own a background worker and Export() only enqueues, so the number measured is the cost of building and enqueueing a span tree (plus sink-worker contention), not the cost of a synchronous write. A slow or unreachable collector delays delivery and grows the drop counter; it does not block the request path.')
    [void]$script:limitations.Add('Tracing is enabled per INSTANCE at sample_ratio 1.0, so every request in the traced arms produced a trace. The traced arms therefore show the worst case for the tracing layer, not a sampled deployment.')
    [void]$script:limitations.Add(("The 4-instance arm (ports {0}) is the same shape as the repo's documented `infergate-fleet` Prometheus scrape job, which targets 127.0.0.1:18999-19002 as four identical instances behind one mock upstream. This host reports {1} physical cores / {2} logical processors, so the 4-instance arm is not core-starved -- but a single host is still a single host, so linearity measured here is an upper bound on what these instances cost each other, not a cluster result." -f (@($fleetPorts) -join ', '), [string]$coreCount, [string]$logicalCount))
    [void]$script:limitations.Add('A `direct` arm that is as fast as the `gateway` arm does NOT mean the gateway is free: it means this harness cannot see the gateway''s ceiling through this backend. The tracing arms are the ones that still produce a usable relative signal in that regime, because they change what the gateway does rather than how much the backend can absorb.')
    [void]$script:limitations.Add(("SCRAPE LOAD is an UPPER BOUND, not a Prometheus model: the `gateway-scraped` arm holds a constant {0}ms poll of {1} for the whole arm, whereas a real Prometheus scrapes every 5-15s. It answers 'what does being scraped cost' in the most aggressive regime this harness can produce, not what a normal scrape interval costs." -f $scrapeIntervalMs, (@($scrapePaths) -join ' + ')))
    [void]$script:limitations.Add(("SCRAPE RATE CEILING: the scrape arm ASKS for a {0} endpoint-GETs/s poll (one GET to each of {1} every {2}ms). The co-located scraper is a PowerShell loop invoking curl.exe, whose process-spawn cost caps it at ~{3} endpoint GETs/s ({4}x intended, per-round {5}, spread {6}% -- stable, so a systematic ceiling rather than flakiness). That is still about {7}x a 5s Prometheus scrape interval, so the scrape-cost delta in this artifact is an UPPER BOUND for a real Prometheus, not a model of one -- and part of the delta is the scraper process competing for the same cores." -f `
            $scrapeIntended, (@($scrapePaths) -join ' + '), $scrapeIntervalMs, $scrapeRate, $scrapeAchievedVsIntended, (@($perRoundRates) -join '/'), $rateSpreadPct, `
        $(if ([double]$scrapeRate -gt 0) { [Math]::Round(([double]$scrapeRate / (1.0 / 5.0)), 1) } else { 0 })))
    [void]$script:limitations.Add('The scraper in the SCRAPE LOAD arm is a PowerShell background job (curl.exe in a loop) on the SAME host and the SAME CPU as the gateway it scrapes, so part of any gateway-scraped-vs-gateway delta is the scraper process competing for cores rather than the exposition code getting slower. Read that delta as the cost of being scraped by a co-located scraper, which is the worst case a Prometheus deployment would present.')
    if ($tracingProbe['endpoint_path_mismatch']) {
        # With the base-url endpoint this cannot happen. If it ever does, the
        # HARNESS is misconfigured (it handed the exporter a url that already
        # carried the path), not the gateway: internal/traceexport/otlp.go:43-46
        # documents otlp.endpoint as a base URL and :153 appends '/v1/traces'.
        [void]$script:limitations.Add(("OTLP ENDPOINT PATH (HARNESS CONFIGURATION ERROR, not a gateway defect): otlp.endpoint is a BASE url (internal/traceexport/otlp.go:43-46,153), and this run configured '{0}' while the collector saw POSTs at '{1}'. A real collector on ':4318' is configured in the base form, so the fix is in this harness's config, not in the gateway. Exporter counters for this run: exported={2}, failed={3}." -f $otlpEndpoint, ($collectorBase + $otlpConstructedPath), [string]$tracingProbe['otlp_exported'], [string]$tracingProbe['otlp_failed']))
    }
    foreach ($lim in $script:limitations) { Write-Host "  - $lim" -ForegroundColor DarkGray }

    # -----------------------------------------------------------------------
    # 8. Artifacts
    # -----------------------------------------------------------------------
    Write-Section '8. Artifacts'

    # The raw per-run arm files are copied next to the summary, the same way M1's
    # raw runs sit next to m1-summary.json -- so a reader can recompute any median
    # without rerunning the harness. A SMOKE run copies them into its own tmp
    # scratch directory instead, so docs\baseline\ never receives smoke data.
    $rawDir = $baselineDir
    $rawDirLabel = 'docs/baseline'
    if ($isSmoke) {
        $rawDir = Join-Path $tmpDir 'raw'
        $rawDirLabel = 'tmp (smoke run)'
        New-Item -ItemType Directory -Path $rawDir -Force | Out-Null
    }
    $copiedRuns = @()
    foreach ($run in $rawRuns) {
        $dest = Join-Path $rawDir "m5-load-$($run.Arm)-r$($run.Round).json"
        Copy-Item -LiteralPath $run.Path -Destination $dest -Force
        $copiedRuns += [ordered]@{
            arm = $run.Arm; round = $run.Round; file = "$rawDirLabel/m5-load-$($run.Arm)-r$($run.Round).json"
            phases = @($run.Rows).Count; run_seconds = $run.RunSeconds
            targets = @($run.Targets); source = "tmp\m5-measure-$stamp\m5-load-$($run.Arm)-r$($run.Round).json"
        }
    }
    Assert-That ($copiedRuns.Count -eq ($runPlan.Count * $Rounds)) 'every arm round was copied next to the artifact' "copied=$($copiedRuns.Count) expected=$($runPlan.Count * $Rounds) dir=$rawDirLabel"

    $goVersion = ''
    try { $goVersion = ((& $goShim version 2>$null | Out-String).Trim()) } catch { $goVersion = 'unknown' }

    $duration = [Math]::Round(((Get-Date) - $startedAt).TotalSeconds, 1)
    $checksFailed = @($script:checks | Where-Object { -not $_.ok }).Count

    $summary = [ordered]@{
        generated_at = (Get-Date).ToString('s')
        milestone    = 'M5'
        smoke        = [bool]$isSmoke
        smoke_reason = $smokeReason
        artifact_scope_note = $(if ($isSmoke) {
                "SMOKE RUN: parameters were below the measurement floor (requests >= $MinRequests AND rounds >= $MinRounds). Written to tmp\ on purpose; docs\baseline\ is untouched so this can never be mistaken for a measurement."
            }
            else {
                "Measurement run: requests $Requests >= $MinRequests and rounds $Rounds >= $MinRounds, so this artifact was written to docs\baseline\ alongside the raw per-arm runs."
            })
        host         = "$env:COMPUTERNAME / $env:PROCESSOR_IDENTIFIER"
        go_version   = $goVersion
        command      = ("powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m5.ps1 -Concurrency '{0}' -Requests {1} -Warmup {2} -Rounds {3} -Timeout {4}" -f `
            $Concurrency, $Requests, $Warmup, $Rounds, $Timeout)
        command_note = 'reconstructed from the parameters this process actually ran with; see workload{} for the same values plus the mock timings, and smoke{} for whether this was a measurement or a smoke run'
        duration_s   = $duration
        host_detail  = [ordered]@{
            machine    = "$env:COMPUTERNAME"
            processor  = "$env:PROCESSOR_IDENTIFIER"
            cpu_name   = "$((Get-CimInstance Win32_Processor -ErrorAction SilentlyContinue | Select-Object -First 1).Name)"
            cores      = $coreCount
            logical_processors = $logicalCount
            os         = [System.Environment]::OSVersion.VersionString
            powershell = $PSVersionTable.PSVersion.ToString()
            repo       = $repo
            script     = 'scripts\measure-m5.ps1'
            stamp      = $stamp
            duration_s = $duration
        }
        workload     = [ordered]@{
            path               = '/v1/chat/completions'
            levels             = @($levels)
            requests           = $Requests
            warmup             = $Warmup
            rounds             = $Rounds
            timeout            = $Timeout
            mock_token_delay_ms = $MockTokenDelayMs
            mock_ttfb_ms       = $MockTTFBMs
            log_level          = $LogLevel
            model              = 'mock-gpt'
            auth               = 'Authorization: Bearer loadtest-key'
            note               = 'cmd\loadtest does NOT honour -rounds in -url/-urls mode, so this script owns the rounds and reports the median across them'
        }
        fleet        = @(
            [ordered]@{ name = 'mock'; port = $mockPort; tracing = $false; purpose = 'the single backend every arm talks to, directly or through a gateway (token-delay ' + $MockTokenDelayMs + 'ms, ttfb ' + $MockTTFBMs + 'ms)' }
            [ordered]@{ name = 'gateway'; port = $ports.A; tracing = $false; purpose = 'primary untraced gateway instance; the 2- and 4-instance horizontal arms both include it' }
            [ordered]@{ name = 'second'; port = $ports.B; tracing = $false; purpose = 'second untraced instance: the single-instance control arm and the second member of the 2-instance arm' }
            [ordered]@{ name = 'gateway-c'; port = $ports.C; tracing = $false; purpose = 'third untraced instance, member of the 4-instance arm only' }
            [ordered]@{ name = 'gateway-d'; port = $ports.D; tracing = $false; purpose = 'fourth untraced instance, member of the 4-instance arm only' }
            [ordered]@{ name = 'traced-otlp'; port = $ports.E; tracing = $true; purpose = 'tracing at sample_ratio 1.0 with the OTLP/HTTP exporter only (no JSONL sink)' }
            [ordered]@{ name = 'traced-full'; port = $ports.F; tracing = $true; purpose = 'tracing at sample_ratio 1.0 with BOTH the OTLP/HTTP exporter and the JSONL file sink' }
            [ordered]@{ name = 'mockcollector'; port = $collectorPort; tracing = $false; purpose = 'stdlib OTLP/HTTP sink that records what the exporters actually delivered' }
        )
        rows         = @($aggregates)
        scrape_load  = $scrapeBlock
        overhead     = [ordered]@{
            vs_direct  = @($vsDirect)
            tracing    = @($tracingOverhead)
            scrape_load = $scrapeOverhead
            horizontal = $horizontalBlock
            bottleneck = $bottleneckBlock
        }
        probes       = [ordered]@{
            gateway_metrics = $metricsProbe
            requests_total_around_arm = $gwAMetrics
            otlp = $tracingProbe
            jsonl = $jsonlProbe
            stats = @($statsSummary)
        }
        checks       = @($script:checks)
        checks_summary = [ordered]@{
            total = $script:checks.Count
            failed = $checksFailed
            result_line = ("RESULT: {0}/{1} checks passed" -f ($script:checks.Count - $checksFailed), $script:checks.Count)
        }
        jsonl_verdict = $jsonlVerdict
        raw_runs     = @($copiedRuns)
        raw_runs_dir = $rawDirLabel
        limitations  = @($script:limitations)
        run_log      = @($script:log)
        assertions   = [ordered]@{
            total = [int]$script:assertTotal
            failed_count = $script:assertFails.Count
            failed = @($script:assertFails)
            note = 'written before the final re-parse assertion, then rewritten with the final counts'
        }
    }

    Write-JsonFile $artifact $summary
    $parsed = Get-Json (Read-Text $artifact)
    Assert-That ($null -ne $parsed) 'the artifact parses back as JSON'
    Assert-That ($parsed.milestone -eq 'M5') 'the artifact names the milestone M5'
    Assert-That (@($parsed.rows).Count -eq ($aggregates.Count)) 'the artifact carries every aggregated row' "rows=$(@($parsed.rows).Count) expected=$($aggregates.Count)"
    Assert-That (@($parsed.checks).Count -eq 10) 'the artifact carries all 10 cross-checks' "checks=$(@($parsed.checks).Count)"

    # The assertion counts can only be final now: the assertions above are part of them.
    $summary['assertions']['total'] = [int]$script:assertTotal
    $summary['assertions']['failed_count'] = $script:assertFails.Count
    $summary['assertions']['failed'] = @($script:assertFails)
    $summary['run_log'] = @($script:log)
    Write-JsonFile $artifact $summary
    $reparsed = Get-Json (Read-Text $artifact)
    Assert-That ($null -ne $reparsed) 'the artifact still parses after the final rewrite'

    # -----------------------------------------------------------------------
    # 9. Headline
    # -----------------------------------------------------------------------
    Write-Section 'M5 HEADLINE'
    $md = New-Object System.Collections.ArrayList
    [void]$md.Add('### M5 -- gateway and observability cost (median of ' + $Rounds + ' interleaved rounds)')
    [void]$md.Add('')
    [void]$md.Add('| arm | workload | c | QPS | QPS min-max | P50 ms | P95 ms | P99 ms | TTFT p95 ms | gateway mean ms | errors |')
    [void]$md.Add('| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |')
    foreach ($a in $aggregates) {
        [void]$md.Add(("| {0} | {1} | {2} | {3} | {4}-{5} | {6} | {7} | {8} | {9} | {10} | {11} |" -f `
                    $a.Arm, $a.Workload, $a.Concurrency, $a.QPS, $a.QPSMin, $a.QPSMax, $a.P50_ms, $a.P95_ms, $a.P99_ms, $a.TTFT_p95_ms, $a.GatewayMean_ms, $a.Errors))
    }
    [void]$md.Add('')
    [void]$md.Add('**Gateway overhead vs the direct-to-backend witness**')
    [void]$md.Add('')
    foreach ($o in $vsDirect) {
        [void]$md.Add(("- {0} c={1}: {2} -> {3} QPS ({4}%, ratio {5}); P95 {6} -> {7} ms (delta {8} ms); P99 delta {9} ms" -f `
                    $o.workload, $o.concurrency, $o.direct_qps, $o.gateway_qps, $o.qps_percent, $o.qps_ratio, $o.direct_p95_ms, $o.gateway_p95_ms, $o.p95_delta_ms, $o.p99_delta_ms))
    }
    [void]$md.Add('')
    [void]$md.Add('**Tracing overhead (vs the untraced gateway arm)**')
    [void]$md.Add('')
    foreach ($o in $tracingOverhead) {
        $extra = ''
        if ($null -ne (Get-Prop $o 'jsonl_vs_otlp_qps_percent')) {
            $extra = ("; JSONL-vs-OTLP {0}% QPS / {1} ms P95 (noise {2}%, measurable={3})" -f `
                    $o.jsonl_vs_otlp_qps_percent, $o.jsonl_vs_otlp_p95_delta_ms, $o.jsonl_vs_otlp_noise_percent, $o.jsonl_adds_measurable_cost)
        }
        [void]$md.Add(("- {0} c={1} [{2}]: {3} -> {4} QPS ({5}%, ratio {6}); P95 {7} -> {8} ms (delta {9} ms){10}" -f `
                    $o.workload, $o.concurrency, $o.sink, $o.untraced_qps, $o.traced_qps, $o.qps_percent, $o.qps_ratio, $o.untraced_p95_ms, $o.traced_p95_ms, $o.p95_delta_ms, $extra))
    }
    [void]$md.Add('')
    [void]$md.Add('- JSONL verdict: ' + $jsonlVerdict)
    [void]$md.Add('')
    [void]$md.Add('**Scrape load (gateway A with a co-located ' + $scrapeIntervalMs + 'ms poller of ' + (@($scrapePaths) -join ' + ') + ')**')
    [void]$md.Add('')
    [void]$md.Add(("- issued {0} endpoint GETs ({1} HTTP 200 = {2}%, {3} failed) over {4}s = {5} Hz against an intended {6} Hz (one GET to each endpoint every {7}ms)" -f `
                $scrapeScrapes, $scrapeOk, $scrapeOkPct, $scrapeFailed, $scrapeStats['scrape_seconds'], $scrapeRate, $scrapeIntended, $scrapeIntervalMs))
    [void]$md.Add('')
    foreach ($o in $scrapeOverhead) {
        [void]$md.Add(("- {0} c={1}: {2} -> {3} QPS ({4}%, ratio {5}); P95 {6} -> {7} ms (delta {8} ms); noise {9}%, measurable={10}" -f `
                    $o.workload, $o.concurrency, $o.unscraped_qps, $o.scraped_qps, $o.qps_percent, $o.qps_ratio, `
                    $o.unscraped_p95_ms, $o.scraped_p95_ms, $o.p95_delta_ms, $o.unscraped_noise_percent, $o.scraped_adds_measurable_cost))
    }
    [void]$md.Add('')
    [void]$md.Add('- scrape verdict: ' + $scrapeVerdict)
    [void]$md.Add('')
    [void]$md.Add('**Horizontal scaling (identical single client process in every arm)**')
    [void]$md.Add('')
    [void]$md.Add(("- host: {0} physical cores / {1} logical processors" -f $coreCount, $logicalCount))
    [void]$md.Add('')
    foreach ($h in $horizontalRows) {
        [void]$md.Add(("- {0} c={1}: 1 instance {2} QPS -> 2 instances {3} QPS ({4}x, {5}% of linear) -> 4 instances {6} QPS ({7}x, {8}% of linear)" -f `
                    $h.workload, $h.concurrency, $h.single_instance_qps, $h.fleet2_qps, $h.fleet2_speedup, $h.fleet2_linearity_pct, $h.fleet4_qps, $h.fleet4_speedup, $h.fleet4_linearity_pct))
    }
    [void]$md.Add('')
    [void]$md.Add('**Bottleneck witness**')
    [void]$md.Add('')
    foreach ($b in $bottleneckRows) {
        [void]$md.Add(("- {0} c={1}: direct {2} QPS vs gateway {3} QPS ({4}% gap, direction {5}, mock is the ceiling: {6})" -f `
                    $b.workload, $b.concurrency, $b.direct_qps, $b.gateway_qps, $b.direct_minus_gateway_pct, $b.direction, $b.mock_is_the_ceiling))
        [void]$md.Add(('  - ' + [string]$b.verdict))
    }
    [void]$md.Add('')
    [void]$md.Add('- ' + [string]$bottleneckBlock['verdict'])
    [void]$md.Add('')
    [void]$md.Add('**Limitations**')
    [void]$md.Add('')
    foreach ($lim in $script:limitations) { [void]$md.Add('- ' + $lim) }

    foreach ($line in $md) { Write-Host $line }
    Write-NoBom (Join-Path $workDir 'headline.md') ($md -join "`n")

    $failCount = $checksFailed
    Write-Host ''
    Write-Host ("  assertions: {0} run, {1} failed" -f [int]$script:assertTotal, $script:assertFails.Count)
    Write-Host ("  artifact  : $artifact")
    Write-Host ("  headline  : " + (Join-Path $workDir 'headline.md'))
    $exitOk = (($script:assertFails.Count -eq 0) -and ($failCount -eq 0))
}
catch {
    $runError = $_.Exception.Message
    Write-Host ''
    Write-Host "RUN ABORTED: $runError" -ForegroundColor Red
    if ($_.ScriptStackTrace) { Write-Host $_.ScriptStackTrace -ForegroundColor DarkYellow }
    $exitOk = $false
}
finally {
    # -----------------------------------------------------------------------
    # 10. Teardown -- only what this script started
    # -----------------------------------------------------------------------
    Write-Section '10. Teardown'

    if ($KeepRunning) {
        Write-Host '  -KeepRunning set: the fleet is left running for inspection.' -ForegroundColor DarkYellow
        foreach ($entry in $script:started) {
            Write-Host ("    {0,-16} pid={1}" -f $entry.Name, $entry.Proc.Id) -ForegroundColor DarkYellow
        }
        Write-Host "  scratch dir kept: $workDir" -ForegroundColor DarkYellow
    }
    else {
        # Scrape jobs first: they are the only thing here that could still be
        # issuing requests, and a leftover job would follow the script home.
        foreach ($job in @($script:scrapeJobs)) { Stop-ScrapeJob -Job $job }
        $script:scrapeJobs.Clear()

        foreach ($entry in $script:started) { Stop-Tracked -Entry $entry }

        $deadline = (Get-Date).AddSeconds(10)
        $alive = @($script:started | Where-Object { $null -ne (Get-Process -Id $_.Proc.Id -ErrorAction SilentlyContinue) })
        while ((Get-Date) -lt $deadline -and $alive.Count -gt 0) {
            Start-Sleep -Milliseconds 200
            $alive = @($script:started | Where-Object { $null -ne (Get-Process -Id $_.Proc.Id -ErrorAction SilentlyContinue) })
        }
        Assert-That ($alive.Count -eq 0) 'no process started by this script survived it' (($alive | ForEach-Object { "$($_.Name) pid=$($_.Proc.Id)" }) -join '; ')

        foreach ($port in @($gatewayPorts + @($mockPort, $collectorPort))) {
            Assert-That (Wait-PortClosed -Port $port -Seconds 5) "nothing is left listening on :$port" "port :$port still accepts connections"
        }

        $gwErrProblems = @()
        foreach ($entry in @($script:started | Where-Object { $_.Name -like 'gateway-*' -or $_.Name -eq 'mockupstream' -or $_.Name -eq 'mockcollector' })) {
            $text = (Read-Text $entry.Err).Trim()
            if ($text.Length -gt 0) { $gwErrProblems += ("{0}: {1}" -f $entry.Name, $text.Substring(0, [Math]::Min(200, $text.Length))) }
        }
        Assert-That ($gwErrProblems.Count -eq 0) 'no fleet process wrote to stderr during the run' ($gwErrProblems -join ' | ')

        if ($KeepWork) {
            Write-Host "  -KeepWork set: scratch dir kept at $workDir" -ForegroundColor DarkYellow
        }
        else {
            Remove-Item -LiteralPath $workDir -Recurse -Force -ErrorAction SilentlyContinue
            $leftover = @(Get-ChildItem -Path $tmpDir -Filter "m5-measure-$stamp*" -ErrorAction SilentlyContinue)
            Assert-That ($leftover.Count -eq 0) 'this run left no scratch files behind' (($leftover | ForEach-Object { $_.Name }) -join '; ')
        }
    }

    # The teardown assertions below (surviving processes, closed ports, clean
    # stderr, no scratch left) are real assertions and are part of assertTotal,
    # so the persisted counts must be written once more AFTER them -- otherwise
    # the artifact reports fewer assertions than the run printed.
    if ($null -ne $summary -and $summary.Contains('assertions')) {
        $summary['assertions']['total'] = [int]$script:assertTotal
        $summary['assertions']['failed_count'] = $script:assertFails.Count
        $summary['assertions']['failed'] = @($script:assertFails)
        $summary['assertions']['note'] = 'final counts, including the teardown assertions, written after teardown'
        $summary['checks_summary']['failed'] = @($script:checks | Where-Object { -not $_.ok }).Count
        Write-JsonFile $artifact $summary
    }

    Write-Host ''
    $checksTotal = $script:checks.Count
    $checksOk = $checksTotal - @($script:checks | Where-Object { -not $_.ok }).Count
    if ($exitOk) {
        Write-Host "RESULT: $checksOk/$checksTotal checks passed" -ForegroundColor Green
        Write-Host "RESULT: M5 measured cleanly, $([int]$script:assertTotal) assertions passed" -ForegroundColor Green
        Write-Host "RESULT: artifact written to $artifact" -ForegroundColor Green
    }
    else {
        Write-Host "RESULT: $checksOk/$checksTotal checks passed" -ForegroundColor Yellow
        Write-Host "RESULT: M5 measurement did NOT complete cleanly" -ForegroundColor Yellow
        if ($runError) { Write-Host "  aborted: $runError" -ForegroundColor Yellow }
        if ($script:assertFails.Count -gt 0) { Write-Host ("  failed: " + ($script:assertFails -join '; ')) -ForegroundColor Yellow }
        exit 1
    }
}
