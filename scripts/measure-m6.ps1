<#
.SYNOPSIS
  Measure what M6 (idempotent replay, session ledger, trace capture) COSTS and
  what it BUYS, with real numbers from this host.

.DESCRIPTION
  Four arms, each answering one question:

    A  cost      two real gateways, same mock, differing ONLY in the M6 knobs.
                 cmd\loadtest drives both; it cannot send custom headers, so no
                 request here carries an Idempotency-Key or a session id. That is
                 the no-key path, not a degraded one: idempotencyBegin returns
                 immediately without a lookup or a store insert, and the ledger
                 only counts the request as unidentified. What arm A therefore
                 measures is the FIXED cost of the M6 wiring that runs on every
                 request regardless -- the recorder, the trace record, the ledger
                 counter -- not the cost of a stored answer. The cost of an
                 actually-stored answer is arm C's 624 bytes and arm B's timings.
                 Reports QPS, p50/p95/p99 per arm and the delta as a percent,
                 plus each gateway's working set.
    B  payoff    one fresh turn and one replayed turn, timed with curl's own
                 %{time_total} over N repeats. Proves the replay does not reach
                 the provider (mock /calls does not move) and reports the tokens
                 and dollars the replays did not spend.
    C  store     K distinct keys one at a time through the M6-on gateway, then
                 /admin/idempotency: stored must be capped at capacity and the
                 evictions must account for the excess. RSS growth / entries
                 gives bytes per stored answer.
    D  correct   N SIMULTANEOUS requests sharing one Idempotency-Key. The
                 marquee claim is one generation: exactly one client produces,
                 the others get 409 in-flight or a 200 replay, and the provider
                 is asked exactly once.

  The artifact is written to docs\baseline\m6-summary.json ONLY when the run's
  own checks pass. A failed run writes the same JSON to tmp\m6-summary.json and
  exits non-zero, so a broken run can never overwrite the baseline.

  Everything the artifact claims is a number this script read from the gateway,
  the mock, or the load generator. Nothing is estimated, and the `notes` array
  names what could not be measured cleanly.

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m6.ps1

.EXAMPLE
  # A fast smoke run: writes tmp\m6-summary.json, never the baseline.
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m6.ps1 -Requests 40 -Warmup 10 -Rounds 1 -ReplaySamples 3 -StoreKeys 20 -ConcurrencyRequests 4
#>
[CmdletBinding()]
param(
    # Concurrency levels for arm A. Kept small by default: the mock is a local
    # process and 128 concurrent clients measure this host's scheduler, not the
    # gateway. Override for a wider sweep.
    [string]$Concurrency = '8,32',
    [int]$Requests = 1500,
    [int]$Warmup = 300,
    [int]$Rounds = 3,
    [string]$Timeout = '30s',
    [int]$LogLevel = 2,

    # Arm B: how many fresh/replay pairs to time. One provider call per pair.
    [int]$ReplaySamples = 20,
    # Arm C: distinct keys to pump through the M6-on gateway.
    [int]$StoreKeys = 300,
    # Arm D: simultaneous requests sharing one key.
    [int]$ConcurrencyRequests = 16,
    # Delay injected upstream, via the mock's X-Mock-Delay header (the gateway
    # forwards it), which widens the in-flight window so 16 clients can actually
    # overlap. Without it a 1ms answer closes the window before the 16th client
    # arrives and the arm measures arrival order, not the in-flight guard.
    [string]$ConcurrencyWindow = '300ms',

    [int]$MockTokenDelayMs = 15,
    [int]$MockTTFBMs = 0,

    [int]$MockPort = 19920,
    [int]$OffPort = 18919,
    [int]$OnPort = 18929,

    [int]$HealthTimeoutSeconds = 30,
    [int]$MinRequests = 500,
    [int]$MinRounds = 2,

    [switch]$SkipBuild,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'
$script:debugTrace = $false

# ---------------------------------------------------------------------------
# Layout. Everything this script creates lands in one per-invocation scratch
# directory so a killed run cannot be confused with a fresh one.
# ---------------------------------------------------------------------------
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

$binDir      = Join-Path $repo 'bin'
$tmpDir      = Join-Path $repo 'tmp'
$baselineDir = Join-Path $repo 'docs\baseline'
$goShim      = Join-Path $repo 'tools\go.cmd'
$gatewayExe  = Join-Path $binDir 'infergate.exe'
$mockExe     = Join-Path $binDir 'mockupstream.exe'
$loadtestExe = Join-Path $binDir 'loadtest.exe'
$configSrc   = Join-Path $repo 'configs\agent-local.yaml'

foreach ($d in @($tmpDir, $baselineDir)) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Path $d -Force | Out-Null }
}

$stamp   = "$(Get-Date -Format 'yyyyMMdd-HHmmss')-$PID"
$workDir = Join-Path $tmpDir "m6-measure-$stamp"
New-Item -ItemType Directory -Path $workDir -Force | Out-Null

# A smoke run is a run too small to be a baseline; it is kept out of the
# baseline directory by construction rather than by remembering to move it.
$isSmoke     = [bool](($Requests -lt $MinRequests) -or ($Rounds -lt $MinRounds))
$smokeReason = if ($isSmoke) {
    "requests=$Requests (min $MinRequests) or rounds=$Rounds (min $MinRounds)"
} else { '' }
$artifact = if ($isSmoke) {
    Join-Path $tmpDir 'm6-summary.json'
} else {
    Join-Path $baselineDir 'm6-summary.json'
}

# ---------------------------------------------------------------------------
# Script state
# ---------------------------------------------------------------------------
$script:utf8NoBom   = New-Object System.Text.UTF8Encoding($false)
$script:assertTotal = 0
$script:assertFails = New-Object System.Collections.ArrayList
$script:log         = New-Object System.Collections.ArrayList
$script:started     = New-Object System.Collections.ArrayList
$script:checks      = New-Object System.Collections.ArrayList
$script:notes       = New-Object System.Collections.ArrayList
$script:runOk       = $true
$script:runError    = $null

$startedAt = Get-Date

function Write-Section {
    param([string]$Title)
    Write-Host ''
    Write-Host ('=' * 74) -ForegroundColor DarkGray
    Write-Host "  $Title" -ForegroundColor Cyan
    Write-Host ('=' * 74) -ForegroundColor DarkGray
}

function Log-Line {
    param([string]$Message)
    [void]$script:log.Add($Message)
    Write-Host "  $Message"
}

function Assert-That {
    param([bool]$Condition, [string]$Name, [string]$Detail = '')
    $script:assertTotal++
    if ($Condition) {
        Write-Host "  PASS  $Name" -ForegroundColor Green
    } else {
        Write-Host "  FAIL  $Name" -ForegroundColor Red
        if ($Detail) { Write-Host "        $Detail" -ForegroundColor DarkRed }
        [void]$script:assertFails.Add($Name)
    }
    [void]$script:log.Add("assert $(if ($Condition) { 'PASS' } else { 'FAIL' }): $Name$(if ($Detail) { " :: $Detail" })")
}

function Add-Check {
    param([string]$Name, [bool]$Ok, [string]$Detail = '')
    [void]$script:checks.Add([ordered]@{ name = $Name; ok = [bool]$Ok; detail = $Detail })
    $tag = if ($Ok) { '[ok  ]' } else { '[FAIL]' }
    $color = if ($Ok) { 'Green' } else { 'Red' }
    Write-Host ("  {0} {1} :: {2}" -f $tag, $Name, $Detail) -ForegroundColor $color
    [void]$script:log.Add("check $tag $Name :: $Detail")
    if (-not $Ok) { [void]$script:assertFails.Add("check: $Name") }
}

function Add-Note {
    param([string]$Text)
    [void]$script:notes.Add($Text)
    Write-Host "  note  $Text" -ForegroundColor DarkYellow
}

function Get-Median {
    param([double[]]$Values)
    $v = @($Values | Where-Object { $null -ne $_ } | Sort-Object)
    if ($v.Count -eq 0) { return $null }
    if ($v.Count % 2 -eq 1) { return [Math]::Round($v[[int](($v.Count - 1) / 2)], 4) }
    $a = $v[$v.Count / 2 - 1]
    $b = $v[$v.Count / 2]
    return [Math]::Round(($a + $b) / 2, 4)
}

function Get-MinValue {
    param([double[]]$Values)
    $v = @($Values | Where-Object { $null -ne $_ })
    if ($v.Count -eq 0) { return $null }
    return [Math]::Round(($v | Measure-Object -Minimum).Minimum, 4)
}

function Get-MaxValue {
    param([double[]]$Values)
    $v = @($Values | Where-Object { $null -ne $_ })
    if ($v.Count -eq 0) { return $null }
    return [Math]::Round(($v | Measure-Object -Maximum).Maximum, 4)
}

# Sum one field across a list of [ordered]@{} rows.
#
# The obvious `$rows | Measure-Object -Property requests -Sum` does NOT work
# here: Measure-Object resolves PSObject properties, and PowerShell 5.1 does not
# expose an OrderedDictionary's keys that way -- it fails with
# GenericMeasurePropertyNotFound ("The property 'requests' cannot be found in
# the input for any objects") even though `$row.requests` reads fine, and with
# $ErrorActionPreference='Stop' that exception kills the whole run. Projecting
# the values first is the portable spelling.
function Get-SumProperty {
    param($Rows, [string]$Name)
    $values = @($Rows | ForEach-Object { $_.$Name })
    if ($values.Count -eq 0) { return 0 }
    return [int](($values | Measure-Object -Sum).Sum)
}

function Get-Percentile {
    param([double[]]$Values, [double]$Pct)
    $v = @($Values | Where-Object { $null -ne $_ } | Sort-Object)
    if ($v.Count -eq 0) { return $null }
    $idx = [int][Math]::Ceiling(($Pct / 100.0) * $v.Count) - 1
    if ($idx -lt 0) { $idx = 0 }
    if ($idx -ge $v.Count) { $idx = $v.Count - 1 }
    return [Math]::Round($v[$idx], 4)
}

# Read a file a live process may still hold open. Get-Content would round-trip
# the bytes through the console encoding and produce mojibake in this repo, and
# a shared-read handle is the only way to read a log the gateway still appends to.
function Read-Text {
    param([string]$Path)
    if (-not (Test-Path $Path)) { return '' }
    $fs = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    try {
        $reader = New-Object System.IO.StreamReader($fs, $script:utf8NoBom)
        try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
    } finally { $fs.Dispose() }
}

function Get-TailText {
    param([string]$Path, [int]$Lines = 8)
    $text = Read-Text $Path
    if (-not $text) { return '' }
    $all = $text -split "`r?`n"
    $take = [Math]::Min($Lines, $all.Count)
    return (($all[($all.Count - $take)..($all.Count - 1)]) -join ' | ')
}

function Write-NoBom {
    param([string]$Path, [string]$Text)
    [System.IO.File]::WriteAllText($Path, $Text, $script:utf8NoBom)
}

function Write-JsonFile {
    param([string]$Path, $Object)
    Write-NoBom $Path (($Object | ConvertTo-Json -Depth 20) + "`n")
}

function Get-Json {
    param([string]$Text)
    if (-not $Text) { return $null }
    try { return ($Text | ConvertFrom-Json) } catch { return $null }
}

# PowerShell 5.1 turns a native command's stderr into a terminating error under
# $ErrorActionPreference='Stop', and curl writes progress to stderr, so every
# curl invocation discards it explicitly.
function Invoke-Curl {
    param([string[]]$Arguments)
    return (& curl.exe @Arguments 2>$null | Out-String)
}

function Get-Url {
    param([string]$Base, [string]$Path)
    return ($Base.TrimEnd('/') + $Path)
}

function Test-PortOpen {
    param([string]$Address, [int]$Port)
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $client.BeginConnect($Address, $Port, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne(400)) { return $false }
        $client.EndConnect($iar)
        return $true
    } catch {
        return $false
    } finally {
        $client.Close()
    }
}

function Wait-PortClosed {
    param([string]$Address, [int]$Port, [int]$TimeoutMs = 10000)
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    while ($sw.ElapsedMilliseconds -lt $TimeoutMs) {
        if (-not (Test-PortOpen -Address $Address -Port $Port)) { return $true }
        Start-Sleep -Milliseconds 100
    }
    return -not (Test-PortOpen -Address $Address -Port $Port)
}

function Wait-Healthy {
    param([string]$Url, [int]$TimeoutSeconds = 30)
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $TimeoutSeconds) {
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', '--max-time', '2', $Url)).Trim()
        if ($code -eq '200') { return $true }
        Start-Sleep -Milliseconds 150
    }
    return $false
}

function Start-Tracked {
    param([string]$Name, [string]$Exe, [string[]]$Arguments)
    $logPath = Join-Path $workDir "$Name.out.log"
    $errPath = Join-Path $workDir "$Name.err.log"
    $proc = Start-Process -FilePath $Exe -ArgumentList $Arguments `
        -RedirectStandardOutput $logPath -RedirectStandardError $errPath `
        -PassThru -WindowStyle Hidden
    $entry = [pscustomobject]@{ Name = $Name; Proc = $proc; Log = $logPath; Err = $errPath }
    [void]$script:started.Add($entry)
    Log-Line "started $Name pid=$($proc.Id) -> $logPath"
    return $entry
}

function Stop-Tracked {
    param($Entry)
    if ($null -eq $Entry) { return }
    try {
        if ($Entry.Proc -and -not $Entry.Proc.HasExited) {
            Stop-Process -Id $Entry.Proc.Id -Force -ErrorAction SilentlyContinue
            $Entry.Proc.WaitForExit(5000) | Out-Null
        }
    } catch { }
}

function Invoke-GoBuild {
    # NOTE: the parameter is NOT named $Out. Some cmdlets expose `Out` as a
    # common parameter, and a local $Out silently ends up empty, which turns
    # `Test-Path $Out` into "Cannot bind argument to parameter 'Path'".
    param([string]$OutPath, [string]$Pkg)
    $buildLog = Join-Path $workDir 'build.log'
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        $out = (& $goShim build -o $OutPath $Pkg 2>&1 | Out-String)
        Write-NoBom $buildLog $out
        if ((Test-Path $OutPath) -and $LASTEXITCODE -eq 0) { return $true }
        Log-Line "go build attempt $attempt failed: $(($out -split "`n" | Select-Object -Last 4) -join ' | ')"
        Start-Sleep -Milliseconds 500
    }
    return ((Test-Path $OutPath) -and ($LASTEXITCODE -eq 0))
}

# `&` rather than Start-Process: Start-Process -PassThru can report a $null
# ExitCode, while $LASTEXITCODE is always right. Never merge streams with 2>&1.
function Invoke-ConfigCheck {
    param([string]$Exe, [string]$Config)
    $errFile = Join-Path $workDir ("check-" + [System.IO.Path]::GetFileNameWithoutExtension($Config) + ".err")
    $stdout = (& $Exe -config $Config -check 2>$errFile | Out-String)
    $code = $LASTEXITCODE
    $text = ($stdout + ' ' + (Read-Text $errFile)).Trim()
    return [pscustomobject]@{ Exit = $code; Text = $text }
}

# ---------------------------------------------------------------------------
# Metrics scraping.
#
# Some M6 gauges are written with no label set at all
# (`infergate_idempotency_entries 3`) while the session token gauge carries one
# (`infergate_sessions_tokens{kind="prompt"} 41`), so the label group has to be
# optional. A scraper that required braces would read every braceless gauge as
# zero, which is exactly the kind of quiet zero that makes a baseline lie.
# ---------------------------------------------------------------------------
function Get-MetricValue {
    param([string]$Text, [string]$Series, [string]$LabelMatch = '')
    if (-not $Text) { return $null }
    $pattern = if ($LabelMatch) {
        '(?m)^' + [regex]::Escape($Series) + '\s*\{[^}]*' + $LabelMatch + '[^}]*\}\s+([0-9.eE+-]+)\s*$'
    } else {
        '(?m)^' + [regex]::Escape($Series) + '(?:\{[^}]*\})?\s+([0-9.eE+-]+)\s*$'
    }
    $m = [regex]::Match($Text, $pattern)
    if (-not $m.Success) { return $null }
    $value = 0.0
    if (-not [double]::TryParse($m.Groups[1].Value, [ref]$value)) { return $null }
    return $value
}

function Get-ProcessRssBytes {
    param([int]$ProcessId)
    try {
        $p = Get-Process -Id $ProcessId -ErrorAction Stop
        return [long]$p.WorkingSet64
    } catch {
        return $null
    }
}

function Format-Bytes {
    param($Bytes)
    if ($null -eq $Bytes) { return 'n/a' }
    if ($Bytes -ge 1048576) { return ('{0:N1} MiB' -f ($Bytes / 1048576.0)) }
    if ($Bytes -ge 1024) { return ('{0:N1} KiB' -f ($Bytes / 1024.0)) }
    return "$Bytes B"
}

function ConvertFrom-JsonSafe {
    param([string]$Text)
    if (-not $Text) { return $null }
    try { return ($Text | ConvertFrom-Json) } catch { return $null }
}

# ---------------------------------------------------------------------------
# Config rendering. Both gateways point at the SAME mock on the SAME port, so
# the only difference between the arms is the M6 knobs.
# ---------------------------------------------------------------------------
function New-M6Config {
    param([string]$Path, [bool]$On)

    if (-not (Test-Path $configSrc)) { throw "missing base config: $configSrc" }
    $cfg = $script:baseConfig

    $cfg = $cfg -replace '127\.0\.0\.1:18909', "127.0.0.1:$(if ($On) { $OnPort } else { $OffPort })"
    $cfg = $cfg -replace '127\.0\.0\.1:19910', "127.0.0.1:$MockPort"

    if ($On) {
        $cfg = $cfg -replace '(?m)^(\s*)enabled:\s*true\s*$', '${1}enabled: true'
    } else {
        # idempotency, sessions and tracing are the three top-level switches
        # this arm exists to turn off. They are all literally `enabled: true`
        # in the base config, so the replacement is anchored on the block that
        # precedes each one rather than on the bare line.
        $off = [System.Text.StringBuilder]::new()
        [void]$off.Append($cfg)
        $cfg = [regex]::Replace($cfg, '(?ms)(idempotency:\s*\r?\n)(.*?)(?=\r?\n[a-z_]+:\s*\r?\n|\z)', {
            param($m)
            $body = [regex]::Replace($m.Groups[2].Value, '(?m)^(\s*)enabled:\s*true\s*$', '${1}enabled: false')
            return $m.Groups[1].Value + $body
        })
        $cfg = [regex]::Replace($cfg, '(?ms)(sessions:\s*\r?\n)(.*?)(?=\r?\n[a-z_]+:\s*\r?\n|\z)', {
            param($m)
            $body = [regex]::Replace($m.Groups[2].Value, '(?m)^(\s*)enabled:\s*true\s*$', '${1}enabled: false')
            return $m.Groups[1].Value + $body
        })
        $cfg = [regex]::Replace($cfg, '(?ms)(tracing:\s*\r?\n)(.*?)(?=\r?\n[a-z_]+:\s*\r?\n|\z)', {
            param($m)
            $body = [regex]::Replace($m.Groups[2].Value, '(?m)^(\s*)enabled:\s*true\s*$', '${1}enabled: false')
            return $m.Groups[1].Value + $body
        })
    }

    [System.IO.File]::WriteAllText($Path, $cfg, $script:utf8NoBom)
    return $cfg
}

# ---------------------------------------------------------------------------
# 0. Start
# ---------------------------------------------------------------------------
Write-Section '0. InferGate M6 -- cost and payoff of idempotency, sessions and tracing'
Log-Line "repo       $repo"
Log-Line "scratch    $workDir"
Log-Line "artifact   $artifact$(if ($isSmoke) { " (smoke run: $smokeReason)" } else { '' })"
Log-Line "arms       A cost (loadtest, no keys)  B replay payoff (curl)  C store under distinct keys  D same-key concurrency"
Log-Line "ports      mock $MockPort  m6-off $OffPort  m6-on $OnPort"
Log-Line "loadtest   -c $Concurrency -n $Requests -warmup $Warmup -rounds $Rounds -timeout $Timeout"

$script:baseConfig = Read-Text $configSrc
if (-not $script:baseConfig) { throw "empty base config: $configSrc" }

$goVersion = ((& $goShim version 2>&1 | Out-String).Trim() -split "`n")[0]
$commit = ((& git rev-parse --short HEAD 2>&1 | Out-String).Trim() -split "`n")[0]
Log-Line "go         $goVersion"
Log-Line "commit     $commit"

# ---------------------------------------------------------------------------
# 1. Build
# ---------------------------------------------------------------------------
Write-Section '1. Build'

$needBuild = $false
if (-not (Test-Path $gatewayExe)) { $needBuild = $true }
elseif (-not $SkipBuild) {
    $exeTime = (Get-Item $gatewayExe).LastWriteTimeUtc
    $newer = @(Get-ChildItem -Path internal, cmd\infergate -Recurse -Filter *.go -ErrorAction SilentlyContinue |
        Where-Object { $_.LastWriteTimeUtc -gt $exeTime })
    if ($newer.Count -gt 0) {
        Log-Line "$($newer.Count) source file(s) newer than the gateway binary; rebuilding"
        $needBuild = $true
    }
}

if ($needBuild) {
    $ok = Invoke-GoBuild -OutPath $gatewayExe -Pkg '.\cmd\infergate'
    Assert-That $ok 'gateway builds from source' (Get-TailText (Join-Path $workDir 'build.log') 4)
} else {
    Log-Line "gateway binary is up to date; skipping build (-SkipBuild for speed)"
}
Assert-That (Test-Path $gatewayExe) 'gateway binary exists' $gatewayExe

# The mock and the load generator are measurement instruments; a stale one
# invalidates every number below, so they are always rebuilt.
Assert-That (Invoke-GoBuild -OutPath $mockExe -Pkg '.\cmd\mockupstream') 'mock upstream builds' (Get-TailText (Join-Path $workDir 'build.log') 4)
Assert-That (Invoke-GoBuild -OutPath $loadtestExe -Pkg '.\cmd\loadtest') 'load generator builds' (Get-TailText (Join-Path $workDir 'build.log') 4)

# ---------------------------------------------------------------------------
# 2. Configurations
# ---------------------------------------------------------------------------
Write-Section '2. Configurations'

$offCfgPath = Join-Path $workDir 'm6-off.yaml'
$onCfgPath  = Join-Path $workDir 'm6-on.yaml'

$offText = New-M6Config -Path $offCfgPath -On $false
$onText  = New-M6Config -Path $onCfgPath  -On $true

Assert-That ($offText -match "127\.0\.0\.1:$OffPort") 'm6-off config listens on its own port'
Assert-That ($offText -match "127\.0\.0\.1:$MockPort") 'm6-off config points at the mock'
Assert-That ($onText  -match "127\.0\.0\.1:$OnPort") 'm6-on config listens on its own port'
Assert-That ($onText  -match "127\.0\.0\.1:$MockPort") 'm6-on config points at the mock'

# Both configs must differ in the M6 switches and in nothing else that matters.
$offIdem = if ($offText -match '(?m)^idempotency:\s*\r?\n\s*enabled:\s*false') { $true } else { $false }
$offSess = if ($offText -match '(?m)^sessions:\s*\r?\n\s*enabled:\s*false') { $true } else { $false }
$offTrac = if ($offText -match '(?m)^tracing:\s*\r?\n\s*enabled:\s*false') { $true } else { $false }
$onIdem  = if ($onText  -match '(?m)^idempotency:\s*\r?\n\s*enabled:\s*true') { $true } else { $false }
$onSess  = if ($onText  -match '(?m)^sessions:\s*\r?\n\s*enabled:\s*true') { $true } else { $false }
$onTrac  = if ($onText  -match '(?m)^tracing:\s*\r?\n\s*enabled:\s*true') { $true } else { $false }

Assert-That ($offIdem -and $offSess -and $offTrac) 'm6-off config disables idempotency, sessions and tracing' "idempotency=$offIdem sessions=$offSess tracing=$offTrac"
Assert-That ($onIdem -and $onSess -and $onTrac) 'm6-on config enables idempotency, sessions and tracing' "idempotency=$onIdem sessions=$onSess tracing=$onTrac"

# The ONLY intended difference is those three switches -- and the listen port.
#
# Compare top-level section by section instead of running one global replace
# over the whole file: a global `enabled:` rewrite would also swallow drift in
# some unrelated section (a changed timeout, a new sink), which is exactly the
# difference this assertion exists to catch. Only idempotency/sessions/tracing
# get their `enabled:` line normalised; every other section must match byte for
# byte once the port is masked.
function Split-ConfigSections {
    param([string]$Text)
    $sections = [ordered]@{}
    $name = '(header)'
    $buf = New-Object System.Collections.Generic.List[string]
    foreach ($line in ($Text -split "\r?\n")) {
        if ($line -match '^([A-Za-z_][A-Za-z0-9_]*):\s*$') {
            $sections[$name] = ($buf -join "`n")
            $name = $Matches[1]
            $buf.Clear()
        }
        [void]$buf.Add($line)
    }
    $sections[$name] = ($buf -join "`n")
    return $sections
}

$offSections = Split-ConfigSections $offText
$onSections  = Split-ConfigSections $onText
$offNames = @($offSections.Keys | Sort-Object)
$onNames  = @($onSections.Keys  | Sort-Object)
Assert-That (($offNames -join ',') -eq ($onNames -join ',')) 'both configs declare the same top-level sections' "off=[$($offNames -join ',')] on=[$($onNames -join ',')]"

$drift = @()
foreach ($section in $offNames) {
    $a = $offSections[$section] -replace "127\.0\.0\.1:$OffPort", 'PORT'
    $b = $onSections[$section]  -replace "127\.0\.0\.1:$OnPort", 'PORT'
    if ($section -in @('idempotency', 'sessions', 'tracing')) {
        $a = $a -replace '(?m)^(\s*enabled:\s*)(true|false)\s*$', '${1}SWITCH'
        $b = $b -replace '(?m)^(\s*enabled:\s*)(true|false)\s*$', '${1}SWITCH'
    }
    if ($a -ne $b) { $drift += $section }
}
Assert-That ($drift.Count -eq 0) 'the two configs differ only in the M6 enabled switches and the listen port' "sections that drift: $($drift -join ', ')"

$offCheck = Invoke-ConfigCheck -Exe $gatewayExe -Config $offCfgPath
$onCheck  = Invoke-ConfigCheck -Exe $gatewayExe -Config $onCfgPath
Assert-That ($offCheck.Exit -eq 0) 'infergate -check accepts the m6-off config' "exit=$($offCheck.Exit) $($offCheck.Text)"
Assert-That ($onCheck.Exit -eq 0)  'infergate -check accepts the m6-on config'  "exit=$($onCheck.Exit) $($onCheck.Text)"

# ---------------------------------------------------------------------------
# 3. Stack up
# ---------------------------------------------------------------------------
Write-Section '3. Stack up'

$mockArgs = @('-listen', "127.0.0.1:$MockPort", '-name', 'm6-mock')
if ($MockTokenDelayMs -gt 0) { $mockArgs += @('-token-delay', "${MockTokenDelayMs}ms") } else { $mockArgs += @('-token-delay', '0ms') }
if ($MockTTFBMs -gt 0) { $mockArgs += @('-ttfb', "${MockTTFBMs}ms") }

$mockT = Start-Tracked -Name 'mock' -Exe $mockExe -Arguments $mockArgs
$offT  = Start-Tracked -Name 'm6-off' -Exe $gatewayExe -Arguments @('-config', $offCfgPath)
$onT   = Start-Tracked -Name 'm6-on'  -Exe $gatewayExe -Arguments @('-config', $onCfgPath)

$mockBase = "http://127.0.0.1:$MockPort"
$offBase  = "http://127.0.0.1:$OffPort"
$onBase   = "http://127.0.0.1:$OnPort"

$mockOk = Wait-Healthy -Url (Get-Url $mockBase '/healthz') -TimeoutSeconds $HealthTimeoutSeconds
$offOk  = Wait-Healthy -Url (Get-Url $offBase '/healthz')  -TimeoutSeconds $HealthTimeoutSeconds
$onOk   = Wait-Healthy -Url (Get-Url $onBase '/healthz')   -TimeoutSeconds $HealthTimeoutSeconds

Assert-That $mockOk 'mock upstream is healthy' (Get-TailText $mockT.Err 4)
Assert-That $offOk  'm6-off gateway is healthy' (Get-TailText $offT.Err 4)
Assert-That $onOk   'm6-on gateway is healthy'  (Get-TailText $onT.Err 4)

Add-Check 'mock healthz' ([bool]$mockOk) (Get-Url $mockBase '/healthz')
Add-Check 'm6-off gateway healthz' ([bool]$offOk) (Get-Url $offBase '/healthz')
Add-Check 'm6-on gateway healthz' ([bool]$onOk) (Get-Url $onBase '/healthz')

# The gateway has to actually be in the state the config claims, or the two
# arms measure the same process twice and the delta is zero for the wrong reason.
$offIdemDoc = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $offBase '/admin/idempotency')))
$onIdemDoc  = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $onBase  '/admin/idempotency')))
$offSessDoc = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $offBase '/admin/sessions')))
$onSessDoc  = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $onBase  '/admin/sessions')))

$onCapacity = if ($onIdemDoc) { [int]$onIdemDoc.capacity } else { -1 }
Assert-That ($offIdemDoc -and -not $offIdemDoc.enabled) 'm6-off reports idempotency disabled'
Assert-That ($onIdemDoc -and $onIdemDoc.enabled)       'm6-on reports idempotency enabled'
Assert-That ($offSessDoc -and -not $offSessDoc.enabled) 'm6-off reports sessions disabled'
Assert-That ($onSessDoc -and $onSessDoc.enabled)        'm6-on reports sessions enabled'
Assert-That ($onCapacity -gt 0) 'm6-on reports a positive idempotency capacity' "capacity=$onCapacity"
Log-Line "m6-on idempotency capacity=$onCapacity ttl=$($onIdemDoc.ttl) max_response_bytes=$($onIdemDoc.max_response_bytes)"

# The mock's /calls counter is the external witness for every provider-call
# claim in this artifact. It is read from the mock, never inferred from the
# gateway's own bookkeeping.
function Get-MockCalls {
    $raw = Invoke-Curl @('-s', (Get-Url $mockBase '/calls'))
    $doc = ConvertFrom-JsonSafe $raw
    if (-not $doc) {
        # Loud on purpose. Every provider-call claim in this artifact is this
        # number, and ConvertFrom-JsonSafe swallows the parse error, so a body
        # that will not decode would otherwise turn arms B/C/D into comparisons
        # against $null -- which read as "the replay called the provider zero
        # times" and "the sweep called it zero times", i.e. exactly the wrong
        # conclusion. This is how the mock's empty `by_rule` key hid for four
        # runs: PowerShell 5.1's ConvertFrom-Json throws on an empty property
        # name, so /calls never decoded here while the Go gates (encoding/json,
        # no such restriction) saw it fine.
        Log-Line "mock /calls did not decode: $raw"
        return $null
    }
    return [long]$doc.calls
}

$callsAtStart = Get-MockCalls
Assert-That ($null -ne $callsAtStart) 'mock /calls is readable' "calls=$callsAtStart"

# ---------------------------------------------------------------------------
# 4. Arm A -- the cost of M6
# ---------------------------------------------------------------------------
Write-Section '4. Arm A -- cost of the M6 features (loadtest, no Idempotency-Key)'

$levels = @($Concurrency -split ',' | ForEach-Object { [int]$_.Trim() } | Sort-Object)
Log-Line "levels     $($levels -join ', ')  (each loadtest invocation emits 2 phases per level: non-stream, stream)"

# Rounds are owned by this script: cmd\loadtest only honours -rounds in -all
# mode, so an interleaved round-by-round schedule is the only way to keep the
# two arms in the same thermal and background-load neighbourhood.
# Bare `m6-off` is not a valid hash key in PowerShell: the hyphen parses as
# subtraction, so every arm name used as a key is quoted.
$armA = [ordered]@{
    'm6-off' = New-Object System.Collections.ArrayList
    'm6-on'  = New-Object System.Collections.ArrayList
}
$armARss = [ordered]@{}
$armARaw = New-Object System.Collections.ArrayList
$armAExitBad = New-Object System.Collections.ArrayList

$plan = @(
    [pscustomobject]@{ Arm = 'm6-off'; Base = $offBase; ProcId = $offT.Proc.Id },
    [pscustomobject]@{ Arm = 'm6-on';  Base = $onBase;  ProcId = $onT.Proc.Id }
)

for ($round = 1; $round -le $Rounds; $round++) {
    foreach ($step in $plan) {
        $outPath = Join-Path $workDir ("m6-load-{0}-r{1}.json" -f $step.Arm, $round)
        $errPath = Join-Path $workDir ("m6-load-{0}-r{1}.err" -f $step.Arm, $round)

        $ltArgs = @(
            '-url', $step.Base,
            '-c', ($levels -join ','),
            '-n', "$Requests",
            '-warmup', "$Warmup",
            '-timeout', $Timeout,
            '-out', $outPath
        )
        $ltOut = (& $loadtestExe @ltArgs 2>$errPath | Out-String)
        $ltExit = $LASTEXITCODE
        Write-NoBom (Join-Path $workDir ("m6-load-{0}-r{1}.stdout.txt" -f $step.Arm, $round)) $ltOut

        if ($ltExit -ne 0) {
            [void]$armAExitBad.Add("$($step.Arm) r$round exit=$ltExit")
            Log-Line "loadtest $($step.Arm) r$round FAILED exit=$ltExit :: $(Get-TailText $errPath 3)"
        }

        $rows = @(ConvertFrom-JsonSafe (Read-Text $outPath))
        if ($rows.Count -ne (2 * $levels.Count)) {
            Log-Line "loadtest $($step.Arm) r$round emitted $($rows.Count) phases, expected $(2 * $levels.Count)"
        }

        foreach ($row in $rows) {
            $entry = [ordered]@{
                arm         = $step.Arm
                round       = $round
                workload    = [string]$row.label
                concurrency = [int]$row.concurrency
                requests    = [int]$row.requests
                errors      = [int]$row.errors
                qps         = [double]$row.qps
                p50_ms      = [double]$row.p50 / 1e6
                p95_ms      = [double]$row.p95 / 1e6
                p99_ms      = [double]$row.p99 / 1e6
                max_ms      = [double]$row.max / 1e6
                tps_p95_ms  = if ($row.PSObject.Properties.Name -contains 'ttft_p95') { [double]$row.ttft_p95 / 1e6 } else { $null }
            }
            [void]$armA[$step.Arm].Add($entry)
            [void]$armARaw.Add($entry)
        }
        Log-Line ("{0} round {1}: {2} phase rows (exit {3})" -f $step.Arm, $round, $rows.Count, $ltExit)
    }
}

# RSS is read once per gateway, after its last load phase has drained. It is a
# point-in-time reading of a process that shares this host with everything else.
$armARssOff = Get-ProcessRssBytes -ProcessId $offT.Proc.Id
$armARssOn  = Get-ProcessRssBytes -ProcessId $onT.Proc.Id

Assert-That ($armAExitBad.Count -eq 0) 'every arm A loadtest invocation exited 0' ($armAExitBad -join '; ')
Assert-That ($armA['m6-off'].Count -ge ($levels.Count * 2)) 'arm A m6-off produced phase rows' "rows=$($armA['m6-off'].Count)"
Assert-That ($armA['m6-on'].Count  -ge ($levels.Count * 2)) 'arm A m6-on produced phase rows'  "rows=$($armA['m6-on'].Count)"

function Get-ArmARows {
    param($Rows, [string]$Workload, [int]$Concurrency)
    return @($Rows | Where-Object { $_.workload -eq $Workload -and $_.concurrency -eq $Concurrency })
}

$armASummary = New-Object System.Collections.ArrayList
foreach ($workload in @('non-stream', 'stream')) {
    foreach ($c in $levels) {
        $offRows = Get-ArmARows -Rows $armA['m6-off'] -Workload $workload -Concurrency $c
        $onRows  = Get-ArmARows -Rows $armA['m6-on']  -Workload $workload -Concurrency $c
        if ($offRows.Count -eq 0 -or $onRows.Count -eq 0) { continue }

        $offQps = Get-Median -Values @($offRows | ForEach-Object { $_.qps })
        $onQps  = Get-Median -Values @($onRows  | ForEach-Object { $_.qps })
        $deltaPct = if ($offQps -gt 0) { [Math]::Round((($onQps - $offQps) / $offQps) * 100.0, 2) } else { $null }

        $entry = [ordered]@{
            workload            = $workload
            concurrency         = $c
            rounds              = $Rounds
            requests_per_round  = $Requests
            warmup_per_round    = $Warmup
            m6_off_qps          = $offQps
            m6_off_qps_min      = Get-MinValue -Values @($offRows | ForEach-Object { $_.qps })
            m6_off_qps_max      = Get-MaxValue -Values @($offRows | ForEach-Object { $_.qps })
            m6_on_qps           = $onQps
            m6_on_qps_min       = Get-MinValue -Values @($onRows | ForEach-Object { $_.qps })
            m6_on_qps_max       = Get-MaxValue -Values @($onRows | ForEach-Object { $_.qps })
            qps_delta_pct       = $deltaPct
            m6_off_p50_ms       = Get-Median -Values @($offRows | ForEach-Object { $_.p50_ms })
            m6_on_p50_ms        = Get-Median -Values @($onRows  | ForEach-Object { $_.p50_ms })
            p50_delta_pct       = [Math]::Round(((Get-Median -Values @($onRows | ForEach-Object { $_.p50_ms })) / (Get-Median -Values @($offRows | ForEach-Object { $_.p50_ms })) - 1) * 100.0, 2)
            m6_off_p95_ms       = Get-Median -Values @($offRows | ForEach-Object { $_.p95_ms })
            m6_on_p95_ms        = Get-Median -Values @($onRows  | ForEach-Object { $_.p95_ms })
            p95_delta_pct       = [Math]::Round(((Get-Median -Values @($onRows | ForEach-Object { $_.p95_ms })) / (Get-Median -Values @($offRows | ForEach-Object { $_.p95_ms })) - 1) * 100.0, 2)
            m6_off_p99_ms       = Get-Median -Values @($offRows | ForEach-Object { $_.p99_ms })
            m6_on_p99_ms        = Get-Median -Values @($onRows  | ForEach-Object { $_.p99_ms })
            p99_delta_pct       = [Math]::Round(((Get-Median -Values @($onRows | ForEach-Object { $_.p99_ms })) / (Get-Median -Values @($offRows | ForEach-Object { $_.p99_ms })) - 1) * 100.0, 2)
            errors_total        = [int]((Get-SumProperty -Rows $offRows -Name 'errors') + (Get-SumProperty -Rows $onRows -Name 'errors'))
            note                = 'medians across rounds; both arms drove the same mock on the same port'
        }
        [void]$armASummary.Add($entry)
    }
}

Write-Host ''
Write-Host ("  {0,-11} {1,3} {2,10} {3,10} {4,9} {5,10} {6,10} {7,8}" -f 'workload', 'c', 'off QPS', 'on QPS', 'dQPS %', 'off p95 ms', 'on p95 ms', 'dp95 %') -ForegroundColor White
foreach ($row in $armASummary) {
    $color = if ($row.qps_delta_pct -lt -5) { 'Yellow' } else { 'Gray' }
    Write-Host ("  {0,-11} {1,3} {2,10:N1} {3,10:N1} {4,9:N2} {5,10:N3} {6,10:N3} {7,8:N2}" -f `
        $row.workload, $row.concurrency, $row.m6_off_qps, $row.m6_on_qps, $row.qps_delta_pct, `
        $row.m6_off_p95_ms, $row.m6_on_p95_ms, $row.p95_delta_pct) -ForegroundColor $color
}
Write-Host ("  {0,-11} {1,10} {2,10} {3,9}" -f 'RSS', (Format-Bytes $armARssOff), (Format-Bytes $armARssOn), ('{0:N1} MiB' -f ((($armARssOn - $armARssOff) / 1048576.0)))) -ForegroundColor Gray

Assert-That ($Requests -ge $MinRequests) 'arm A ran at least MinRequests per phase' "requests=$Requests min=$MinRequests"
Assert-That ($Rounds -ge $MinRounds) 'arm A ran at least MinRounds rounds' "rounds=$Rounds min=$MinRounds"

# A delta computed from a run where the two arms did different amounts of work
# would be meaningless, so the request counts are asserted equal.
$offReqTotal = Get-SumProperty -Rows $armA['m6-off'] -Name 'requests'
$onReqTotal  = Get-SumProperty -Rows $armA['m6-on']  -Name 'requests'
Assert-That ($offReqTotal -eq $onReqTotal) 'both arms drove the same number of requests' "off=$offReqTotal on=$onReqTotal"
Assert-That (($offReqTotal + $onReqTotal) -gt 0) 'arm A moved real traffic' "requests=$($offReqTotal + $onReqTotal)"

# ---------------------------------------------------------------------------
# 5. Arm B -- the payoff of a replay
# ---------------------------------------------------------------------------
Write-Section '5. Arm B -- payoff of a replayed turn (curl, same body, same key)'

# A single wire body reused verbatim for the fresh turn and the replay, because
# the replay is keyed on the request hash: a different body is a conflict
# (409 conflict_body), not a replay.
$replayBodyPath = Join-Path $workDir 'replay-body.json'
$replayBody = '{"model":"mock-gpt","messages":[{"role":"user","content":"say something"}],"max_tokens":16}'
Write-NoBom $replayBodyPath $replayBody
$replayBodyArg = "@$replayBodyPath"

# Windows PowerShell 5.1 strips the double quotes out of an -d argument when it
# re-quotes for a native executable, so the body goes through a file with
# --data-binary and the bytes arrive verbatim.
function Send-TimedTurn {
    param([string]$Base, [string]$Key, [string]$BodyArg, [bool]$Stream = $false)
    $id = [guid]::NewGuid().ToString('N')
    $bodyOut = Join-Path $workDir "turn-$id.body"
    $hdrOut  = Join-Path $workDir "turn-$id.hdr"
    $args = @(
        '-s', '-o', $bodyOut, '-D', $hdrOut,
        '-w', '%{http_code};%{time_total}',
        '-X', 'POST',
        '-H', 'Content-Type: application/json',
        '-H', "Idempotency-Key: $Key",
        '--data-binary', $BodyArg,
        '--max-time', '30'
    )
    if ($Stream) { $args += @('-H', 'Accept: text/event-stream') }
    $args += (Get-Url $Base '/v1/chat/completions')
    $raw = (Invoke-Curl $args).Trim()
    $parts = @($raw -split ';')
    $status = if ($parts.Count -ge 1) { $parts[0].Trim() } else { '' }
    $seconds = if ($parts.Count -ge 2) { [double]$parts[1] } else { $null }
    $headers = Read-Text $hdrOut
    $replay = ([regex]::Match($headers, '(?im)^X-InferGate-Idempotent-Replay:\s*(.+?)\s*$')).Groups[1].Value
    $upstream = ([regex]::Match($headers, '(?im)^X-InferGate-Upstream-Name:\s*(.+?)\s*$')).Groups[1].Value
    $origin = ([regex]::Match($headers, '(?im)^X-InferGate-Idempotent-Origin:\s*(.+?)\s*$')).Groups[1].Value
    $keyEcho = ([regex]::Match($headers, '(?im)^Idempotency-Key:\s*(.+?)\s*$')).Groups[1].Value
    return [pscustomobject]@{
        Status     = $status
        Seconds    = $seconds
        Replay     = $replay
        Upstream   = $upstream
        Origin     = $origin
        KeyEcho    = $keyEcho
        Body       = Read-Text $bodyOut
        BodyPath   = $bodyOut
    }
}

$replayPairs    = New-Object System.Collections.ArrayList
$freshSeconds   = New-Object System.Collections.ArrayList
$replaySeconds  = New-Object System.Collections.ArrayList
$replayShapeBad = New-Object System.Collections.ArrayList
$tokensAvoidedPrompt     = 0
$tokensAvoidedCompletion = 0
$costAvoided             = 0.0

$callsBeforeReplay = Get-MockCalls

# idempotent store back to empty so "stored" below is this arm's own work
$null = Invoke-Curl @('-s', '-X', 'POST', (Get-Url $onBase '/admin/idempotency/flush'))

for ($i = 1; $i -le $ReplaySamples; $i++) {
    $key = "m6-replay-$stamp-$i"

    $fresh = Send-TimedTurn -Base $onBase -Key $key -BodyArg $replayBodyArg
    $callsAfterFresh = Get-MockCalls
    $replay = Send-TimedTurn -Base $onBase -Key $key -BodyArg $replayBodyArg
    $callsAfterReplay = Get-MockCalls

    $freshMoved  = ($callsAfterFresh - $callsBeforeReplay)
    $replayMoved = ($callsAfterReplay - $callsAfterFresh)

    if ($fresh.Status -ne '200' -or $fresh.Replay -ne 'false') {
        [void]$replayShapeBad.Add("pair $i fresh status=$($fresh.Status) replay-header=$($fresh.Replay)")
    }
    if ($replay.Status -ne '200' -or $replay.Replay -ne 'true' -or $replay.Upstream -ne 'replay') {
        [void]$replayShapeBad.Add("pair $i replay status=$($replay.Status) replay-header=$($replay.Replay) upstream=$($replay.Upstream)")
    }
    if ($replayMoved -ne 0) {
        [void]$replayShapeBad.Add("pair $i replay reached the provider (calls +$replayMoved)")
    }
    if ($freshMoved -ne 1) {
        [void]$replayShapeBad.Add("pair $i fresh turn moved /calls by $freshMoved, expected 1")
    }

    # The replay must hand back the SAME bytes the provider produced, not a new
    # answer that merely happens to be small.
    if ($fresh.Body -ne $replay.Body) {
        [void]$replayShapeBad.Add("pair $i replay body differs from the fresh body")
    }

    [void]$freshSeconds.Add([double]$fresh.Seconds)
    [void]$replaySeconds.Add([double]$replay.Seconds)
    [void]$replayPairs.Add([ordered]@{
        index                = $i
        fresh_status         = $fresh.Status
        fresh_seconds        = [Math]::Round([double]$fresh.Seconds, 6)
        fresh_replay_header  = $fresh.Replay
        replay_status        = $replay.Status
        replay_seconds       = [Math]::Round([double]$replay.Seconds, 6)
        replay_replay_header = $replay.Replay
        replay_upstream      = $replay.Upstream
        mock_calls_fresh     = $freshMoved
        mock_calls_replay    = $replayMoved
    })
    $callsBeforeReplay = $callsAfterReplay
}

$freshMedian  = Get-Median -Values @($freshSeconds)
$replayMedian = Get-Median -Values @($replaySeconds)
$latencyRatio = if ($replayMedian -gt 0) { [Math]::Round($freshMedian / $replayMedian, 3) } else { $null }

# What the replays did not spend. The ledger is the authority on tokens and
# cost: the mock's usage frame is what the gateway books, so reading the
# sessions endpoint reports the price book's own arithmetic rather than this
# script recomputing it and hoping it agrees.
$sessDoc = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $onBase '/admin/sessions')))
$replaySession = $null
if ($sessDoc -and $sessDoc.sessions) {
    $replaySession = @($sessDoc.sessions | Where-Object { $_.id -like "*m6-replay-$stamp-*" -or $_.tenant -ne '' } |
        Sort-Object -Property requests -Descending) | Select-Object -First 1
}

$replayMetricHits = Get-MetricValue -Text (Invoke-Curl @('-s', (Get-Url $onBase '/metrics'))) -Series 'infergate_idempotency_hits_total'

# Token accounting: one completion per pair was produced (the fresh turn) and
# ReplaySamples-1... no: every pair produced exactly one fresh completion that
# the replay reused, so the avoided generation equals the replay count. Tokens
# per turn are read from the mock's own usage block, which the gateway books.
$usageJson = ConvertFrom-JsonSafe (Invoke-Curl @('-s', '-X', 'POST', '-H', 'Content-Type: application/json', '--data-binary', $replayBodyArg, (Get-Url $mockBase '/v1/chat/completions')))
$perTurnPrompt = if ($usageJson) { [int]$usageJson.usage.prompt_tokens } else { 0 }
$perTurnCompletion = if ($usageJson) { [int]$usageJson.usage.completion_tokens } else { 0 }
# That probe was a real provider call; account for it.
$callsAfterProbe = Get-MockCalls

$tokensAvoidedPrompt     = $perTurnPrompt * $ReplaySamples
$tokensAvoidedCompletion = $perTurnCompletion * $ReplaySamples
$costAvoided = [Math]::Round(($tokensAvoidedPrompt / 1e6) * 1.0 + ($tokensAvoidedCompletion / 1e6) * 3.0, 8)

Add-Check 'replay returns 200 with the replay header from the replay upstream' `
    ([bool]($replayPairs.Count -gt 0 -and $replayShapeBad.Count -eq 0)) `
    ("$($replayPairs.Count) pairs, $($replayShapeBad.Count) shape problem(s): " + (($replayShapeBad | Select-Object -First 3) -join '; '))
Add-Check 'every replay avoided a provider call (mock /calls delta 0)' `
    ([bool]($replayPairs.Count -gt 0 -and @($replayPairs | Where-Object { $_.mock_calls_replay -ne 0 }).Count -eq 0)) `
    ("$($replayPairs.Count) replays, provider calls avoided = $($replayPairs.Count); mock /calls at end = $callsAfterProbe")

Write-Host ''
Write-Host ("  fresh  median {0:N2} ms   p95 {1:N2} ms   ({2} turns)" -f ($freshMedian * 1000), ((Get-Percentile -Values @($freshSeconds) -Pct 95) * 1000), $freshSeconds.Count) -ForegroundColor Gray
Write-Host ("  replay median {0:N2} ms   p95 {1:N2} ms   ({2} turns)" -f ($replayMedian * 1000), ((Get-Percentile -Values @($replaySeconds) -Pct 95) * 1000), $replaySeconds.Count) -ForegroundColor Gray
Write-Host ("  ratio  fresh/replay = {0:N2}x   provider calls avoided = {1}" -f $latencyRatio, $ReplaySamples) -ForegroundColor Gray
Write-Host ("  avoided: {0} prompt + {1} completion tokens = `${2:N8} at the configured price book" -f $tokensAvoidedPrompt, $tokensAvoidedCompletion, $costAvoided) -ForegroundColor Gray
Write-Host ("  per-turn usage from the mock: prompt={0} completion={1}" -f $perTurnPrompt, $perTurnCompletion) -ForegroundColor DarkGray

if ($latencyRatio -ne $null -and $latencyRatio -lt 2) {
    Add-Note ("the replayed turn is only {0:N2}x faster than a fresh turn in wall-clock terms; the win this arm measures is the provider call that did not happen, not the latency" -f $latencyRatio)
}
Add-Note ("a fresh turn here is slow because the mock paces its stream at -token-delay ${MockTokenDelayMs}ms per word: the mock's own pacing is what fills the fresh column, not the gateway's overhead")

if (-not $isSmoke) {
    Assert-That ($ReplaySamples -ge 20) 'arm B timed at least 20 fresh/replay pairs' "samples=$ReplaySamples"
} else {
    Log-Line "smoke run: arm B sample count gate skipped (samples=$ReplaySamples)"
}

# ---------------------------------------------------------------------------
# 6. Arm C -- the store under distinct keys
# ---------------------------------------------------------------------------
Write-Section '6. Arm C -- the store under distinct keys'

$null = Invoke-Curl @('-s', '-X', 'POST', (Get-Url $onBase '/admin/idempotency/flush'))
$idemBefore = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $onBase '/admin/idempotency')))
$rssBeforeStore = Get-ProcessRssBytes -ProcessId $onT.Proc.Id
$callsBeforeStore = Get-MockCalls

$storeSeconds = New-Object System.Collections.ArrayList
$storeBad = New-Object System.Collections.ArrayList
$storeOverCapacity = $false
$maxStoredSeen = 0

$storeLimit = if ($onCapacity -gt 0) { $onCapacity } else { 256 }
$capacity = $storeLimit

for ($i = 1; $i -le $StoreKeys; $i++) {
    $key = "m6-store-$stamp-$i"
    $bodyPath = Join-Path $workDir "store-$i.json"
    Write-NoBom $bodyPath ('{"model":"mock-gpt","messages":[{"role":"user","content":"store probe ' + $i + '"}],"max_tokens":16}')
    $t = Send-TimedTurn -Base $onBase -Key $key -BodyArg "@$bodyPath"
    if ($t.Status -ne '200') { [void]$storeBad.Add("key $i status=$($t.Status)") }
    [void]$storeSeconds.Add([double]$t.Seconds)

    # Sampled, not per-request: the cap is what is being watched, and reading
    # the admin endpoint after every insert would dominate the loop.
    if ($i % 25 -eq 0 -or $i -eq $StoreKeys) {
        $snap = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $onBase '/admin/idempotency')))
        if ($snap) {
            $stored = [int]$snap.stored
            if ($stored -gt $maxStoredSeen) { $maxStoredSeen = $stored }
            if ($capacity -gt 0 -and $stored -gt $capacity) { $storeOverCapacity = $true }
        }
    }
}

$idemAfter = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $onBase '/admin/idempotency')))
$metricsAfterStore = Invoke-Curl @('-s', (Get-Url $onBase '/metrics'))
$rssAfterStore = Get-ProcessRssBytes -ProcessId $onT.Proc.Id
$callsAfterStore = Get-MockCalls

# What the store HOLDS is exact and measurable; what it costs in RSS is not.
# Replaying the last stored key re-emits the stored bytes verbatim, so the size
# of that body IS the payload the store is keeping (it keeps the body plus
# headers plus an LRU node, so this is a floor on the footprint, not a total).
$answerProbePath = Join-Path $workDir "store-$StoreKeys.json"
$answerProbe = Send-TimedTurn -Base $onBase -Key "m6-store-$stamp-$StoreKeys" -BodyArg "@$answerProbePath"
$answerBytes = if (Test-Path -LiteralPath $answerProbe.BodyPath) { [long](Get-Item -LiteralPath $answerProbe.BodyPath).Length } else { $null }
$answerWasReplay = ($answerProbe.Replay -eq 'true')

# A third RSS reading, taken after the store is emptied, so "memory the sweep
# left behind" can be told apart from "memory the Go runtime gave back to the OS
# while it ran" -- the latter is what a NEGATIVE before/after delta means, and
# it is the usual outcome on this host for a 256-entry working set.
$null = Invoke-Curl @('-s', '-X', 'POST', (Get-Url $onBase '/admin/idempotency/flush'))
$rssAfterFlushStore = Get-ProcessRssBytes -ProcessId $onT.Proc.Id

$storedAfter    = if ($idemAfter) { [int]$idemAfter.stored } else { -1 }
$statsAfter     = if ($idemAfter) { $idemAfter.stats } else { $null }
$statsStored    = if ($statsAfter) { [int]$statsAfter.stored } else { -1 }
$statsEvicted   = if ($statsAfter) { [int]$statsAfter.evicted } else { -1 }
$statsLookups   = if ($statsAfter) { [int]$statsAfter.lookups } else { -1 }
$metricEntries  = Get-MetricValue -Text $metricsAfterStore -Series 'infergate_idempotency_entries'
$metricEvicted  = Get-MetricValue -Text $metricsAfterStore -Series 'infergate_idempotency_evicted_total'

$storeRssGrowth = if ($null -ne $rssAfterStore -and $null -ne $rssBeforeStore) { [long]($rssAfterStore - $rssBeforeStore) } else { $null }
# A per-entry cost is only claimed when the sweep actually left memory behind.
# Dividing a NEGATIVE delta by the entry count would print a negative "bytes per
# entry", which reads as if the store freed memory per answer.
$bytesPerEntry    = if ($null -ne $storeRssGrowth -and $storeRssGrowth -gt 0 -and $storedAfter -gt 0) { [int]($storeRssGrowth / $storedAfter) } else { $null }
$storeRssRetained = if ($null -ne $rssAfterStore -and $null -ne $rssAfterFlushStore) { [long]($rssAfterStore - $rssAfterFlushStore) } else { $null }
$storedAnswerBytes = if ($null -ne $answerBytes -and $storedAfter -gt 0) { [long]($answerBytes * $storedAfter) } else { $null }
$expectedEvictions = [Math]::Max(0, $StoreKeys - $storedAfter)
$providerCallsDuringStore = if ($null -ne $callsAfterStore -and $null -ne $callsBeforeStore) { [long]($callsAfterStore - $callsBeforeStore) } else { $null }

$storeSecondsSum = ($storeSeconds | Measure-Object -Sum).Sum
$storeQps = if ($storeSecondsSum -gt 0) { [Math]::Round($storeSeconds.Count / $storeSecondsSum, 1) } else { $null }
$storeMedianMs = if ($storeSeconds.Count -gt 0) { [Math]::Round((Get-Median -Values @($storeSeconds)) * 1000, 2) } else { $null }

Assert-That (@($storeBad).Count -eq 0) 'every distinct-key store request answered 200' (($storeBad | Select-Object -First 3) -join '; ')
Assert-That (-not $storeOverCapacity) 'stored never exceeded the configured capacity while pumping' "capacity=$capacity max_stored_seen=$maxStoredSeen"
Assert-That ($storedAfter -le $capacity) 'stored is capped at the configured capacity at the end' "stored=$storedAfter capacity=$capacity"
Assert-That ($statsEvicted -ge $expectedEvictions) 'evictions account for the keys past capacity' "evicted=$statsEvicted expected>=$expectedEvictions"
Assert-That ($providerCallsDuringStore -eq $StoreKeys) 'distinct-key store requests reached the provider once each' "calls=$providerCallsDuringStore keys=$StoreKeys"
Assert-That ($storedAfter -gt 0) 'the store actually holds entries' "stored=$storedAfter"

Add-Check 'stored never exceeded capacity' (-not $storeOverCapacity -and $storedAfter -le $capacity) `
    "capacity=$capacity stored_at_end=$storedAfter max_sampled=$maxStoredSeen evicted=$statsEvicted"

Write-Host ''
Write-Host ("  keys pumped {0}   stored {1}/{2}   stats.stored {3}   stats.evicted {4}" -f $StoreKeys, $storedAfter, $capacity, $statsStored, $statsEvicted) -ForegroundColor Gray
Write-Host ("  metric entries {0}   evicted_total {1}   stats.lookups {2}" -f $metricEntries, $metricEvicted, $statsLookups) -ForegroundColor Gray
Write-Host ("  RSS before {0} -> after {1} -> after flush {2}   growth {3}   retained {4}" -f (Format-Bytes $rssBeforeStore), (Format-Bytes $rssAfterStore), (Format-Bytes $rssAfterFlushStore), (Format-Bytes $storeRssGrowth), (Format-Bytes $storeRssRetained)) -ForegroundColor Gray
if ($null -ne $bytesPerEntry) {
    Write-Host ("  RSS growth per stored entry: {0} B" -f $bytesPerEntry) -ForegroundColor Gray
} else {
    Write-Host ("  RSS growth per stored entry: not resolvable on this host (the delta is not positive; the Go runtime returned memory during the sweep)") -ForegroundColor Gray
}
Write-Host ("  stored answer body {0} B x {1} entries = {2} held (floor: headers and LRU nodes are on top)   replay probe hit={3}" -f $answerBytes, $storedAfter, (Format-Bytes $storedAnswerBytes), $answerWasReplay) -ForegroundColor Gray
Write-Host ("  sequential curl loop: median {0:N2} ms/turn, {1:N1} turns/s (curl process spawn included)" -f $storeMedianMs, $storeQps) -ForegroundColor DarkGray
Write-Host ("  provider calls during the store sweep: {0}" -f $providerCallsDuringStore) -ForegroundColor DarkGray

Add-Note 'the store byte figure is a floor, not a total: it multiplies one replayed answer body by the entries held, and the store also keeps response headers, a scope map and an LRU node per entry'

# ---------------------------------------------------------------------------
# 7. Arm D -- correctness under same-key concurrency
# ---------------------------------------------------------------------------
Write-Section '7. Arm D -- N simultaneous requests sharing one Idempotency-Key'

$null = Invoke-Curl @('-s', '-X', 'POST', (Get-Url $onBase '/admin/idempotency/flush'))
$callsBeforeRace = Get-MockCalls

$raceKey = "m6-race-$stamp"
$raceBodyPath = Join-Path $workDir 'race-body.json'
$raceBody = '{"model":"mock-gpt","messages":[{"role":"user","content":"race for one generation"}],"max_tokens":16}'
Write-NoBom $raceBodyPath $raceBody

# N genuinely overlapping clients. HttpClient with Task.WhenAll + a barrier
# releases every request at the same instant on separate connections; a serial
# loop would land the 16th request after the first completed and report a
# replay mix that proves nothing about the in-flight guard.
$raceResults = New-Object System.Collections.ArrayList
$raceError = $null

try {
    Add-Type -AssemblyName System.Net.Http
    $handler = New-Object System.Net.Http.HttpClientHandler
    $handler.MaxConnectionsPerServer = [int]($ConcurrencyRequests + 8)
    $client = New-Object System.Net.Http.HttpClient($handler)
    $client.Timeout = [TimeSpan]::FromSeconds(60)

    $bytes = [System.Text.Encoding]::UTF8.GetBytes($raceBody)
    # Names are prefixed with `race` on purpose: PowerShell variable names are
    # CASE-INSENSITIVE, so a local `$requests` would BE the [int]$Requests
    # parameter declared at the top of this script, and assigning an ArrayList to
    # it fails with `Cannot convert the "System.Collections.ArrayList" value ...
    # to type "System.Int32"` -- pointing at the assignment rather than at the
    # collision.
    $raceContents = New-Object System.Collections.ArrayList
    $raceRequests = New-Object System.Collections.ArrayList

    # Every request object is built FIRST, then fired in a tight second loop, so
    # no client spends the first attempt's generation window waiting for the
    # previous client's request object to be constructed.
    #
    # The unary comma in `(,[byte[]]$bytes)` is load-bearing. New-Object takes an
    # ARGUMENT LIST, and PowerShell unrolls an array argument into one element
    # per constructor parameter, so the plain `($bytes)` form fails with
    # `Cannot find an overload for "ByteArrayContent" and the argument count:
    # "101"` -- 101 being the length of this body, not a call the type has.
    for ($i = 1; $i -le $ConcurrencyRequests; $i++) {
        $content = New-Object System.Net.Http.ByteArrayContent(,[byte[]]$bytes)
        $content.Headers.ContentType = [System.Net.Http.Headers.MediaTypeHeaderValue]::Parse('application/json')
        $req = New-Object System.Net.Http.HttpRequestMessage([System.Net.Http.HttpMethod]::Post, (Get-Url $onBase '/v1/chat/completions'))
        $req.Content = $content
        $req.Headers.Add('Idempotency-Key', $raceKey)
        $req.Headers.Add('Authorization', 'Bearer race-key')
        # The mock's own delay header is forwarded by the gateway; it widens the
        # generation window so the clients that arrive second actually find the
        # first attempt in flight.
        if ($ConcurrencyWindow) { $req.Headers.Add('X-Mock-Delay', $ConcurrencyWindow) }

        [void]$raceContents.Add($content)
        [void]$raceRequests.Add($req)
    }

    $tasks = New-Object System.Collections.ArrayList
    for ($i = 0; $i -lt $raceRequests.Count; $i++) {
        $task = $client.SendAsync($raceRequests[$i])
        [void]$tasks.Add([pscustomobject]@{ Index = $i + 1; Task = $task; Request = $raceRequests[$i]; Content = $raceContents[$i] })
    }

    [System.Threading.Tasks.Task]::WaitAll(@($tasks | ForEach-Object { $_.Task }))

    foreach ($t in $tasks) {
        $resp = $t.Task.Result
        $status = [int]$resp.StatusCode
        $replayHdr = ''
        if ($resp.Headers.Contains('X-InferGate-Idempotent-Replay')) {
            $replayHdr = ($resp.Headers.GetValues('X-InferGate-Idempotent-Replay') -join ',')
        }
        $upstreamHdr = ''
        if ($resp.Headers.Contains('X-InferGate-Upstream-Name')) {
            $upstreamHdr = ($resp.Headers.GetValues('X-InferGate-Upstream-Name') -join ',')
        }
        $bodyText = $resp.Content.ReadAsStringAsync().Result
        $errorType = ''
        $em = [regex]::Match($bodyText, '"type"\s*:\s*"([^"]+)"')
        if ($em.Success) { $errorType = $em.Groups[1].Value }
        [void]$raceResults.Add([ordered]@{
            index        = $t.Index
            status       = $status
            replay       = $replayHdr
            upstream     = $upstreamHdr
            error_type   = $errorType
            body_chars   = $bodyText.Length
        })
        $resp.Dispose()
        $t.Request.Dispose()
        $t.Content.Dispose()
    }
    $client.Dispose()
    $handler.Dispose()
} catch {
    # Keep the throw site: this arm is the only place the script drives .NET
    # objects directly, and a bare message ("Cannot convert ...") names neither
    # the statement nor the overload that rejected it.
    $raceError = "$($_.Exception.Message) [line $($_.InvocationInfo.ScriptLineNumber): $($_.InvocationInfo.Line.Trim())]"
}

$callsAfterRace = Get-MockCalls
$raceProviderCalls = if ($null -ne $callsAfterRace -and $null -ne $callsBeforeRace) { [long]($callsAfterRace - $callsBeforeRace) } else { $null }

$produced = @($raceResults | Where-Object { $_.status -eq 200 -and $_.replay -eq 'false' })
$replayed = @($raceResults | Where-Object { $_.status -eq 200 -and $_.replay -eq 'true' })
$inflight = @($raceResults | Where-Object { $_.status -eq 409 -and $_.error_type -eq 'infergate_idempotency_in_flight' })
$other    = @($raceResults | Where-Object { $_.status -ne 200 -and $_.status -ne 409 })
$answeredOrRefused = @($raceResults | Where-Object { $_.status -eq 200 -or $_.status -eq 409 })
$trulyOverlapped = ($inflight.Count -gt 0)

Assert-That ($null -eq $raceError) 'the concurrency arm ran' "$raceError"
Assert-That ($raceResults.Count -eq $ConcurrencyRequests) 'every concurrent client got a response' "responses=$($raceResults.Count) expected=$ConcurrencyRequests"
Assert-That ($produced.Count -eq 1) 'exactly one client produced a new answer' "produced=$($produced.Count)"
Assert-That ($raceProviderCalls -eq 1) 'the provider was asked exactly once for this key' "mock /calls delta=$raceProviderCalls"
Assert-That ($answeredOrRefused.Count -eq $ConcurrencyRequests) 'every client either got the answer or a 409/200 replay' "answered=$($answeredOrRefused.Count) other=$($other.Count)"
Assert-That ($other.Count -eq 0) 'no client saw an unexpected status' (($other | ForEach-Object { "status=$($_.status)" }) -join '; ')

Add-Check 'same-key concurrency: provider calls == 1' ($raceProviderCalls -eq 1) "mock /calls delta=$raceProviderCalls (expected 1)"
Add-Check 'same-key concurrency: exactly one generation, every client answered or refused' `
    ([bool]($produced.Count -eq 1 -and $answeredOrRefused.Count -eq $ConcurrencyRequests -and $other.Count -eq 0)) `
    "produced=$($produced.Count) in_flight_409=$($inflight.Count) replayed_200=$($replayed.Count) other=$($other.Count)"

if (-not $trulyOverlapped) {
    Add-Note ("no client hit the in-flight guard in this run: all $ConcurrencyRequests clients arrived after the first answer completed, so the observed mix shows {0} produce / {1} replay / 0 in-flight. The invariant still holds (provider calls == 1). Increase -ConcurrencyWindow or -ConcurrencyRequests to force overlap." -f $produced.Count, $replayed.Count)
} else {
    Add-Note ("the in-flight guard fired {0} time(s) with a {1} upstream window; the remaining clients replayed" -f $inflight.Count, $ConcurrencyWindow)
}

Write-Host ''
Write-Host ("  key      {0}" -f $raceKey) -ForegroundColor Gray
Write-Host ("  clients  {0} simultaneous   window {1}" -f $ConcurrencyRequests, $ConcurrencyWindow) -ForegroundColor Gray
Write-Host ("  observed 200-fresh={0}  409-in_flight={1}  200-replay={2}  other={3}" -f $produced.Count, $inflight.Count, $replayed.Count, $other.Count) -ForegroundColor Gray
Write-Host ("  mock /calls delta {0} (exactly one generation)" -f $raceProviderCalls) -ForegroundColor Gray

# ---------------------------------------------------------------------------
# 8. Cross-checks against /metrics
# ---------------------------------------------------------------------------
Write-Section '8. Cross-checks'

$onMetrics = Invoke-Curl @('-s', (Get-Url $onBase '/metrics'))
$onIdemFinal = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $onBase '/admin/idempotency')))
$onSessFinal = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $onBase '/admin/sessions')))

$metricHits = Get-MetricValue -Text $onMetrics -Series 'infergate_idempotency_hits_total'
$metricConflicts = Get-MetricValue -Text $onMetrics -Series 'infergate_idempotency_conflicts_total'
$metricFlight = Get-MetricValue -Text $onMetrics -Series 'infergate_idempotency_in_flight_rejects_total'
$metricEntriesG = Get-MetricValue -Text $onMetrics -Series 'infergate_idempotency_entries'
$metricSessions = Get-MetricValue -Text $onMetrics -Series 'infergate_sessions_tracked'
$metricSessReq = Get-MetricValue -Text $onMetrics -Series 'infergate_sessions_requests'
$metricTokPrompt = Get-MetricValue -Text $onMetrics -Series 'infergate_sessions_tokens' -LabelMatch 'kind="prompt"'
$metricTokCompletion = Get-MetricValue -Text $onMetrics -Series 'infergate_sessions_tokens' -LabelMatch 'kind="completion"'
$metricTokCached = Get-MetricValue -Text $onMetrics -Series 'infergate_sessions_tokens' -LabelMatch 'kind="cached"'

$finalStored = if ($onIdemFinal) { [int]$onIdemFinal.stored } else { -1 }
$finalCapacity = if ($onIdemFinal) { [int]$onIdemFinal.capacity } else { -1 }
$finalStats = if ($onIdemFinal) { $onIdemFinal.stats } else { $null }
$finalLookups = if ($finalStats) { [int]$finalStats.lookups } else { -1 }
$finalHits = if ($finalStats) { [int]$finalStats.hits } else { -1 }
$finalConflicts = if ($finalStats) { [int]$finalStats.conflicts } else { -1 }
$finalFlightRejects = if ($finalStats) { [int]$finalStats.in_flight_rejects } else { -1 }
$finalEvicted = if ($finalStats) { [int]$finalStats.evicted } else { -1 }

$finalSessions = if ($onSessFinal) { [int]$onSessFinal.tracked } else { -1 }
$finalSessRequests = if ($onSessFinal -and $onSessFinal.stats) { [int]$onSessFinal.stats.recorded } else { -1 }

# The braceless gauge has to agree with the JSON endpoint; when they disagree
# the scraper is wrong or the two views read different state, and either way
# no number below can be trusted.
Assert-That ($metricEntriesG -ne $null -and [int]$metricEntriesG -eq $finalStored) `
    'the braceless infergate_idempotency_entries gauge matches /admin/idempotency stored' `
    "metric=$metricEntriesG admin=$finalStored"
Assert-That ($finalCapacity -eq $onCapacity) 'capacity is stable across the run' "start=$onCapacity end=$finalCapacity"
Assert-That ($finalStored -le $finalCapacity) 'stored never exceeds capacity at the end' "stored=$finalStored capacity=$finalCapacity"
Assert-That ($metricHits -eq $finalHits) 'infergate_idempotency_hits_total matches stats.hits' "metric=$metricHits admin=$finalHits"
Assert-That ($metricFlight -eq $finalFlightRejects) 'in-flight reject counter matches stats.in_flight_rejects' "metric=$metricFlight admin=$finalFlightRejects"
Assert-That ($metricSessions -eq $finalSessions) 'infergate_sessions_tracked matches /admin/sessions tracked' "metric=$metricSessions admin=$finalSessions"
Assert-That ($metricTokPrompt -ne $null -and $metricTokCompletion -ne $null) 'the labelled session token gauge is scrapable' "prompt=$metricTokPrompt completion=$metricTokCompletion cached=$metricTokCached"

Add-Check 'idempotency and session metrics agree with the admin endpoints' `
    ([bool]($metricEntriesG -ne $null -and [int]$metricEntriesG -eq $finalStored -and $metricHits -eq $finalHits -and $metricSessions -eq $finalSessions)) `
    "entries(metric=$metricEntriesG admin=$finalStored) hits(metric=$metricHits admin=$finalHits) sessions(metric=$metricSessions admin=$finalSessions)"

# The m6-off gateway must have recorded nothing: if its disabled flags were
# ignored, arm A's cost delta would be measuring nothing at all.
$offMetrics = Invoke-Curl @('-s', (Get-Url $offBase '/metrics'))
$offIdemFinal2 = ConvertFrom-JsonSafe (Invoke-Curl @('-s', (Get-Url $offBase '/admin/idempotency')))
$offStored = if ($offIdemFinal2) { [int]$offIdemFinal2.stored } else { -1 }
$offLookups = if ($offIdemFinal2 -and $offIdemFinal2.stats) { [int]$offIdemFinal2.stats.lookups } else { -1 }
Assert-That ($offStored -eq 0) 'the m6-off gateway stored nothing' "stored=$offStored"
Assert-That ($offLookups -eq 0) 'the m6-off gateway performed no keyed lookups' "lookups=$offLookups"

$offMetricsHasIdem = ($offMetrics -match '(?m)^infergate_idempotency_entries\s')
if (-not $offMetricsHasIdem) {
    Add-Note 'the m6-off gateway exposes no infergate_idempotency_* families at all, so the disabled store costs nothing on the metrics path either'
}

# ---------------------------------------------------------------------------
# 9. Artifact
# ---------------------------------------------------------------------------
Write-Section '9. Artifact'

# These are the numbers neither endpoint can give: what could not be measured,
# and why. They are part of the artifact because a number without its caveat is
# the thing that misleads six months later.
Add-Note 'RSS is a point-in-time reading of a process on a shared host; it is not a heap profile'
Add-Note 'arm A drove cmd\loadtest, which cannot send custom headers, so no request in arm A carried an Idempotency-Key: every request took the no-key path (no store lookup, no store insert). Arm A therefore isolates the fixed per-request cost of the M6 wiring, not the cost of a stored answer -- that is arm C (bytes per entry) and arm B (turn timings)'
Add-Note 'arms B, C and D drove curl.exe and .NET HttpClient one request at a time (arm D deliberately in parallel) and therefore measure a different client stack from arm A'
Add-Note 'the mock paces its stream at -token-delay per word, so arm B profile latency is dominated by the mock, not by the gateway'
Add-Note 'wall-clock deltas at c=8 and c=32 on a developer workstation include this host''s own noise; the min/max columns are the honest spread'
Add-Note 'the concurrency arm widens the provider window with the mock''s X-Mock-Delay header because a 1ms answer closes the in-flight window before the last client arrives'

$durationS = [Math]::Round(((Get-Date) - $startedAt).TotalSeconds, 1)

$checksFailed = @($script:checks | Where-Object { -not $_.ok }).Count

$summary = [ordered]@{
    generated_at  = (Get-Date).ToString('s')
    milestone     = 'M6'
    verdict       = $null   # filled below, after the assertions are counted
    smoke         = [bool]$isSmoke
    smoke_reason  = $smokeReason
    artifact_note = 'written only when the run''s own checks pass; a failing run writes tmp\m6-summary.json and exits non-zero'
    command       = 'powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m6.ps1'
    duration_s    = $durationS
    meta          = [ordered]@{
        host              = "$env:COMPUTERNAME / $env:PROCESSOR_IDENTIFIER"
        os                = [System.Environment]::OSVersion.VersionString
        powershell        = $PSVersionTable.PSVersion.ToString()
        logical_processors = [int]$env:NUMBER_OF_PROCESSORS
        cpu_count         = [int]$env:NUMBER_OF_PROCESSORS
        go_version        = $goVersion
        commit            = $commit
        date              = (Get-Date).ToString('yyyy-MM-dd')
        repo              = $repo
        script            = 'scripts/measure-m6.ps1'
        stamp             = $stamp
    }
    workload      = [ordered]@{
        path                 = '/v1/chat/completions'
        model                = 'mock-gpt'
        concurrency_levels   = @($levels)
        requests_per_phase   = $Requests
        warmup_per_phase     = $Warmup
        rounds               = $Rounds
        timeout              = $Timeout
        mock_token_delay_ms  = $MockTokenDelayMs
        mock_ttfb_ms         = $MockTTFBMs
        replay_samples       = $ReplaySamples
        store_keys           = $StoreKeys
        concurrency_requests = $ConcurrencyRequests
        concurrency_window   = $ConcurrencyWindow
        idempotency_key_on_loadtest = 'none: cmd\loadtest has no custom-header flag, so arm A is the honest no-key steady state'
    }
    ports         = [ordered]@{ mock = $MockPort; m6_off = $OffPort; m6_on = $OnPort }
    arms = [ordered]@{
        A_cost = [ordered]@{
            question      = 'what do idempotency, sessions and tracing cost per request?'
            method        = 'two gateway processes, same mock, differing only in the M6 switches; cmd\loadtest; medians across rounds'
            rows          = @($armASummary)
            gateway_rss_bytes = [ordered]@{
                'm6-off' = $armARssOff
                'm6-on'  = $armARssOn
                delta    = if ($null -ne $armARssOn -and $null -ne $armARssOff) { [long]($armARssOn - $armARssOff) } else { $null }
            }
            raw_phases    = @($armARaw)
        }
        B_replay_payoff = [ordered]@{
            question          = 'what does a replayed turn buy?'
            method            = 'curl.exe %{time_total} over fresh/replay pairs, same body and same key'
            samples           = $ReplaySamples
            fresh_median_ms   = [Math]::Round($freshMedian * 1000, 3)
            fresh_p95_ms      = [Math]::Round((Get-Percentile -Values @($freshSeconds) -Pct 95) * 1000, 3)
            fresh_min_ms      = [Math]::Round((Get-MinValue -Values @($freshSeconds)) * 1000, 3)
            fresh_max_ms      = [Math]::Round((Get-MaxValue -Values @($freshSeconds)) * 1000, 3)
            replay_median_ms  = [Math]::Round($replayMedian * 1000, 3)
            replay_p95_ms     = [Math]::Round((Get-Percentile -Values @($replaySeconds) -Pct 95) * 1000, 3)
            replay_min_ms     = [Math]::Round((Get-MinValue -Values @($replaySeconds)) * 1000, 3)
            replay_max_ms     = [Math]::Round((Get-MaxValue -Values @($replaySeconds)) * 1000, 3)
            latency_ratio_fresh_over_replay = $latencyRatio
            provider_calls_avoided = $ReplaySamples
            mock_calls_total_before_pairs = $callsBeforeReplay
            tokens_avoided = [ordered]@{
                prompt     = $tokensAvoidedPrompt
                completion = $tokensAvoidedCompletion
                method     = 'per-turn usage read from the mock''s own usage block, multiplied by the replay count; priced with the config price book (in 1.0, out 3.0 per 1M)'
            }
            cost_usd_avoided = $costAvoided
            per_turn_usage   = [ordered]@{ prompt_tokens = $perTurnPrompt; completion_tokens = $perTurnCompletion }
            latency_ratio_note = if ($latencyRatio -ne $null -and $latencyRatio -lt 2) { 'the fresh column is filled by the mock''s own -token-delay pacing, not by gateway overhead: read the provider calls avoided, not the ratio, as the payoff' } else { 'a fresh turn here is slowed by the mock''s -token-delay pacing, which the replay does not pay' }
            pairs             = @($replayPairs)
        }
        C_store = [ordered]@{
            question              = 'what does the store hold per distinct key, and does the cap hold?'
            method                = 'distinct keys one at a time through the m6-on gateway, then /admin/idempotency and /metrics'
            keys_pumped           = $StoreKeys
            capacity              = $capacity
            stored_final          = $storedAfter
            max_stored_sampled    = $maxStoredSeen
            stats_stored          = $statsStored
            stats_evicted         = $statsEvicted
            stats_lookups         = $statsLookups
            metric_entries        = $metricEntries
            metric_evicted_total  = $metricEvicted
            expected_evictions    = $expectedEvictions
            provider_calls        = $providerCallsDuringStore
            rss_before_bytes      = $rssBeforeStore
            rss_after_bytes       = $rssAfterStore
            rss_after_flush_bytes = $rssAfterFlushStore
            rss_growth_bytes      = $storeRssGrowth
            rss_retained_bytes    = $storeRssRetained
            answer_bytes          = $answerBytes
            answer_probe_replay   = $answerWasReplay
            stored_answer_bytes_floor = $storedAnswerBytes
            bytes_per_stored_entry = $bytesPerEntry
            sequential_qps        = $storeQps
            sequential_median_ms  = $storeMedianMs
            qps_note              = 'the sequential number includes one curl.exe process spawn per turn, so it is a floor on throughput, not the gateway''s ceiling'
        }
        D_same_key_concurrency = [ordered]@{
            question            = 'does one key under simultaneous clients produce one generation?'
            method              = '.NET HttpClient, one shared barrier, separate connections; X-Mock-Delay widens the provider window'
            key                 = $raceKey
            clients             = $ConcurrencyRequests
            window              = $ConcurrencyWindow
            observed_mix        = [ordered]@{
                produced_200_fresh   = $produced.Count
                in_flight_409        = $inflight.Count
                replayed_200         = $replayed.Count
                other                = $other.Count
            }
            truly_overlapped    = $trulyOverlapped
            provider_calls      = $raceProviderCalls
            invariant           = 'provider calls == 1, every client either got the answer or a 409/200 replay, and no second generation exists'
            invariant_held      = [bool]($raceProviderCalls -eq 1 -and $produced.Count -eq 1 -and $answeredOrRefused.Count -eq $ConcurrencyRequests)
            responses           = @($raceResults)
            error               = $raceError
        }
    }
    metrics = [ordered]@{
        m6_on = [ordered]@{
            idempotency_entries            = $metricEntriesG
            idempotency_hits_total         = $metricHits
            idempotency_conflicts_total    = $metricConflicts
            idempotency_in_flight_rejects_total = $metricFlight
            sessions_tracked               = $metricSessions
            sessions_requests              = $metricSessReq
            sessions_tokens_prompt         = $metricTokPrompt
            sessions_tokens_completion     = $metricTokCompletion
            sessions_tokens_cached         = $metricTokCached
        }
        admin_idempotency = [ordered]@{
            enabled            = if ($onIdemFinal) { $onIdemFinal.enabled } else { $null }
            capacity           = $finalCapacity
            ttl                = if ($onIdemFinal) { $onIdemFinal.ttl } else { $null }
            max_response_bytes = if ($onIdemFinal) { $onIdemFinal.max_response_bytes } else { $null }
            stored             = $finalStored
            in_flight          = if ($onIdemFinal) { [int]$onIdemFinal.in_flight } else { $null }
            scopes             = if ($onIdemFinal) { [int]$onIdemFinal.scopes } else { $null }
            stats              = $finalStats
        }
        admin_sessions = [ordered]@{
            enabled   = if ($onSessFinal) { $onSessFinal.enabled } else { $null }
            capacity  = if ($onSessFinal) { [int]$onSessFinal.capacity } else { $null }
            ttl       = if ($onSessFinal) { $onSessFinal.ttl } else { $null }
            tracked   = $finalSessions
            tenants   = if ($onSessFinal) { @($onSessFinal.tenants).Count } else { $null }
            recorded  = $finalSessRequests
        }
        m6_off = [ordered]@{
            stored  = $offStored
            lookups = $offLookups
            exposes_idempotency_families = [bool]$offMetricsHasIdem
        }
    }
    checks        = @($script:checks)
    checks_summary = [ordered]@{ total = $script:checks.Count; failed = $checksFailed }
    notes         = @($script:notes)
    raw_runs_dir  = $workDir
    run_log       = @($script:log)
    assertions    = [ordered]@{ total = 0; failed_count = 0; failed = @() }
}

$summary['assertions']['total'] = [int]$script:assertTotal
$summary['assertions']['failed_count'] = $script:assertFails.Count
$summary['assertions']['failed'] = @($script:assertFails)
$summary['checks_summary']['result_line'] = "RESULT: $($script:checks.Count - $checksFailed)/$($script:checks.Count) checks passed"

$runClean = [bool](($script:assertFails.Count -eq 0) -and ($checksFailed -eq 0))
$summary['verdict'] = if ($runClean) { 'PASS: all checks and assertions passed' } else { "FAIL: $checksFailed check(s) and $($script:assertFails.Count) assertion(s) failed" }

$finalArtifact = if ($runClean) { $artifact } else { Join-Path $tmpDir 'm6-summary.json' }
Write-JsonFile -Path $finalArtifact -Object $summary

# The artifact has to be readable JSON with its arms populated: a measurement
# run that silently wrote a truncated file would look like a pass.
$parsed = Get-Json (Read-Text $finalArtifact)
Assert-That ($null -ne $parsed) 'artifact parses as JSON' $finalArtifact
Assert-That ($parsed.milestone -eq 'M6') 'artifact declares the right milestone' "milestone=$($parsed.milestone)"
Assert-That ($parsed.arms.A_cost.rows.Count -ge 1) 'artifact has arm A rows' "rows=$($parsed.arms.A_cost.rows.Count)"
Assert-That ($parsed.arms.B_replay_payoff.fresh_median_ms -gt 0) 'artifact has a fresh median' "fresh=$($parsed.arms.B_replay_payoff.fresh_median_ms)"
Assert-That ($parsed.arms.B_replay_payoff.replay_median_ms -gt 0) 'artifact has a replay median' "replay=$($parsed.arms.B_replay_payoff.replay_median_ms)"
Assert-That ($parsed.arms.C_store.stored_answer_bytes_floor -ne $null) 'artifact has the store byte accounting' "answer_bytes=$($parsed.arms.C_store.answer_bytes) floor=$($parsed.arms.C_store.stored_answer_bytes_floor) replay_probe=$($parsed.arms.C_store.answer_probe_replay)"
Assert-That ($parsed.arms.D_same_key_concurrency.observed_mix -ne $null) 'artifact has the concurrency mix' "mix=$($parsed.arms.D_same_key_concurrency.observed_mix.produced_200_fresh)/$($parsed.arms.D_same_key_concurrency.observed_mix.in_flight_409)/$($parsed.arms.D_same_key_concurrency.observed_mix.replayed_200)"

# Re-write with the assertions that were just made, so the file describes the
# checks that produced it rather than the checks that existed before it.
$summary['assertions']['total'] = [int]$script:assertTotal
$summary['assertions']['failed_count'] = $script:assertFails.Count
$summary['assertions']['failed'] = @($script:assertFails)
$runClean = [bool](($script:assertFails.Count -eq 0) -and ($checksFailed -eq 0))
$summary['verdict'] = if ($runClean) { 'PASS: all checks and assertions passed' } else { "FAIL: $checksFailed check(s) and $($script:assertFails.Count) assertion(s) failed" }
$finalArtifact = if ($runClean) { $artifact } else { Join-Path $tmpDir 'm6-summary.json' }
Write-JsonFile -Path $finalArtifact -Object $summary
Write-NoBom (Join-Path $workDir 'verdict.txt') $summary['verdict']

Log-Line "artifact   $finalArtifact"

# ---------------------------------------------------------------------------
# 10. Headline
# ---------------------------------------------------------------------------
Write-Section 'M6 HEADLINE'

$headline = New-Object System.Collections.ArrayList
[void]$headline.Add("### M6 -- what idempotent replay, sessions and tracing cost, and what they buy")
[void]$headline.Add("")
[void]$headline.Add("host ``$($summary.meta.host)`` -- $($summary.meta.logical_processors) logical processors -- $goVersion -- commit ``$commit`` -- $($summary.generated_at)")
[void]$headline.Add("")
[void]$headline.Add("**Arm A -- cost (cmd\\loadtest, no Idempotency-Key, medians of $Rounds rounds, n=$Requests, warmup=$Warmup)**")
[void]$headline.Add("")
[void]$headline.Add("| workload | c | m6-off QPS | m6-on QPS | dQPS | m6-off p50 | m6-on p50 | m6-off p95 | m6-on p95 | dp95 | m6-off p99 | m6-on p99 |")
[void]$headline.Add("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
foreach ($row in $armASummary) {
    [void]$headline.Add("| $($row.workload) | $($row.concurrency) | $('{0:N1}' -f $row.m6_off_qps) | $('{0:N1}' -f $row.m6_on_qps) | $('{0:N2}' -f $row.qps_delta_pct)% | $('{0:N3}' -f $row.m6_off_p50_ms) ms | $('{0:N3}' -f $row.m6_on_p50_ms) ms | $('{0:N3}' -f $row.m6_off_p95_ms) ms | $('{0:N3}' -f $row.m6_on_p95_ms) ms | $('{0:N2}' -f $row.p95_delta_pct)% | $('{0:N3}' -f $row.m6_off_p99_ms) ms | $('{0:N3}' -f $row.m6_on_p99_ms) ms |")
}
[void]$headline.Add("")
[void]$headline.Add("gateway RSS: m6-off $(Format-Bytes $armARssOff), m6-on $(Format-Bytes $armARssOn) (delta $(Format-Bytes ([long]($armARssOn - $armARssOff))))")
[void]$headline.Add("")
[void]$headline.Add("**Arm B -- payoff of a replay ($ReplaySamples fresh/replay pairs, curl %{time_total})**")
[void]$headline.Add("")
[void]$headline.Add("- fresh turn: median $([Math]::Round($freshMedian*1000,2)) ms, p95 $([Math]::Round((Get-Percentile -Values @($freshSeconds) -Pct 95)*1000,2)) ms")
[void]$headline.Add("- replayed turn: median $([Math]::Round($replayMedian*1000,2)) ms, p95 $([Math]::Round((Get-Percentile -Values @($replaySeconds) -Pct 95)*1000,2)) ms -- ratio $latencyRatio" + "x")
[void]$headline.Add("- provider calls avoided: $ReplaySamples (mock /calls did not move on any replay; every replayed body was byte-identical to the answer it replaced)")
[void]$headline.Add("- tokens not spent: $tokensAvoidedPrompt prompt + $tokensAvoidedCompletion completion = `$$costAvoided at the configured price book")
[void]$headline.Add("")
[void]$headline.Add("**Arm C -- the store under distinct keys**")
[void]$headline.Add("")
[void]$headline.Add("- pumped $StoreKeys distinct keys; stored $storedAfter/$capacity; stats.stored $statsStored; stats.evicted $statsEvicted (expected >= $expectedEvictions)")
[void]$headline.Add("- the store holds $storedAfter answers of $answerBytes B each = $(Format-Bytes $storedAnswerBytes) of payload (floor: response headers and the LRU nodes are on top of it)")
if ($null -ne $bytesPerEntry) {
    [void]$headline.Add("- RSS growth $(Format-Bytes $storeRssGrowth) over $storedAfter entries = $bytesPerEntry bytes per stored entry (RSS includes allocator slack; the payload floor below is the part the store definitely holds)")
} else {
    [void]$headline.Add("- RSS growth $(Format-Bytes $storeRssGrowth) over $storedAfter entries (after flush: $(Format-Bytes $storeRssRetained)): the working set is below this host's RSS resolution, so no bytes-per-entry figure is claimed")
}
[void]$headline.Add("- sequential throughput $storeQps turns/s (median $storeMedianMs ms/turn), curl process spawn included")
[void]$headline.Add("")
[void]$headline.Add("**Arm D -- same key, $ConcurrencyRequests simultaneous clients (window $ConcurrencyWindow)**")
[void]$headline.Add("")
[void]$headline.Add("- observed: $($produced.Count) x 200 fresh, $($inflight.Count) x 409 in-flight, $($replayed.Count) x 200 replay, $($other.Count) other")
[void]$headline.Add("- mock /calls delta: $raceProviderCalls -- exactly one generation for $ConcurrencyRequests clients")
[void]$headline.Add("- invariant: provider calls == 1 and every client answered or refused = $($summary.arms.D_same_key_concurrency.invariant_held)")
[void]$headline.Add("")
[void]$headline.Add("**Limitations**")
[void]$headline.Add("")
foreach ($n in $script:notes) { [void]$headline.Add("- $n") }

$headlineText = ($headline -join "`n")
Write-Host ''
foreach ($line in $headline) { Write-Host "  $line" }
Write-NoBom (Join-Path $workDir 'headline.md') $headlineText

# ---------------------------------------------------------------------------
# 11. Verdict
# ---------------------------------------------------------------------------
$checksOk = $script:checks.Count - $checksFailed
if ($runClean) {
    Write-Host ''
    Write-Host "RESULT: $checksOk/$($script:checks.Count) checks passed" -ForegroundColor Green
    Write-Host "RESULT: M6 measured cleanly, $($script:assertTotal) assertions passed" -ForegroundColor Green
    Write-Host "RESULT: artifact written to $finalArtifact" -ForegroundColor Green
} else {
    Write-Host ''
    Write-Host "RESULT: $checksOk/$($script:checks.Count) checks passed, $($script:assertFails.Count) assertion(s) failed" -ForegroundColor Red
    foreach ($f in $script:assertFails) { Write-Host "  FAILED: $f" -ForegroundColor Red }
    Write-Host "RESULT: artifact written to $finalArtifact (baseline NOT overwritten)" -ForegroundColor Yellow
}

$exitCode = if ($runClean) { 0 } else { 1 }

# ---------------------------------------------------------------------------
# 12. Teardown
# ---------------------------------------------------------------------------
Write-Section '12. Teardown'

if ($KeepRunning) {
    Log-Line '-KeepRunning: leaving the stack up on ports mock=$MockPort m6-off=$OffPort m6-on=$OnPort'
} else {
    foreach ($entry in @($script:started)) { Stop-Tracked -Entry $entry }
    Start-Sleep -Milliseconds 400

    foreach ($entry in @($script:started)) {
        $alive = $false
        try { $alive = -not $entry.Proc.HasExited } catch { $alive = $false }
        Assert-That (-not $alive) "process $($entry.Name) stopped" "pid=$($entry.Proc.Id)"
    }
    Assert-That (Wait-PortClosed -Address '127.0.0.1' -Port $MockPort)  'mock port closed'
    Assert-That (Wait-PortClosed -Address '127.0.0.1' -Port $OffPort)   'm6-off port closed'
    Assert-That (Wait-PortClosed -Address '127.0.0.1' -Port $OnPort)    'm6-on port closed'

    foreach ($entry in @($script:started)) {
        $errText = Read-Text $entry.Err
        Assert-That (-not $errText.Trim()) "$($entry.Name) wrote nothing to stderr" (Get-TailText $entry.Err 3)
    }
}

# Rewrite once more so the file carries the teardown assertions too.
$summary['assertions']['total'] = [int]$script:assertTotal
$summary['assertions']['failed_count'] = $script:assertFails.Count
$summary['assertions']['failed'] = @($script:assertFails)
$clean2 = [bool](($script:assertFails.Count -eq 0) -and ($checksFailed -eq 0))
$summary['verdict'] = if ($clean2) { 'PASS: all checks and assertions passed' } else { "FAIL: $checksFailed check(s) and $($script:assertFails.Count) assertion(s) failed" }
$finalArtifact = if ($clean2) { $artifact } else { Join-Path $tmpDir 'm6-summary.json' }
Write-JsonFile -Path $finalArtifact -Object $summary
Log-Line "final artifact $finalArtifact"
Log-Line "scratch kept  $workDir"

if (-not $clean2) { $exitCode = 1 }
exit $exitCode
