<#
    measure-m1.ps1 -- the M1 numbers, MEASURED rather than asserted.

    verify-m1.ps1 answers "does routing and failover behave correctly".  This
    script answers "what does it cost and what does it save", which is the other
    half of the M1 deliverable: a milestone is not finished until it has numbers.

    Three measurements, all against real processes on the loopback:

      1. ROUTING OVERHEAD.  The same load generator, the same mock binary, the
         same session, run back to back against
           (a) an M0 single-upstream gateway  (no router, no breaker bookkeeping)
           (b) an M1 gateway with a three-replica fleet (priority routing + the
               per-upstream breaker window)
         so the delta is the routing layer and nothing else.  Interleaved rounds
         rather than three runs in a row, because this host drifts: the M0
         baseline showed 40%+ round-to-round spread on the direct baseline, and
         running (a) three times and then (b) three times would attribute that
         drift to the router.

      2. FAULT ABSORPTION.  The same load, with the priority-1 replica killed
         mid-run.  Every request must still succeed (the client never sees the
         lost backend) and the surviving throughput is compared with (b).

      3. THE BREAKER'S SAVING.  A stalled replica (1.5s before its first stream
         frame) behind a 400ms per-attempt budget used to be unmeasurable in
         tests/probes because verify-m1.ps1 kills its primary: a refused
         connection fails in microseconds, so there is nothing for a breaker to
         save.  With a SLOW backend each request pays a full 400ms timeout until
         the breaker opens -- and pays nothing once it does.  The per-request
         wall time is printed as a series, which is the honest form of the claim
         "the breaker cut the client's latency from X to Y".
#>
[CmdletBinding()]
param(
    [int]$SingleGatewayPort = 18380,
    [int]$RouterGatewayPort = 18381,
    [int]$SlowGatewayPort = 18390,
    [int]$M0MockPort = 19050,
    [int]$PrimaryPort = 19150,
    [int]$SecondaryPort = 19151,
    [int]$ToolsPort = 19152,
    [int]$SlowPort = 19200,
    [int]$FastPort = 19201,
    [int]$Rounds = 3,
    [int]$Requests = 1500,
    [int]$Warmup = 300,
    [string]$Concurrency = '8,32',
    [int]$SlowStallMs = 1500,
    [int]$UpstreamTimeoutMs = 400,
    [int]$ProbeRequests = 10,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
# Every `go run ./cmd/...` below is relative to the module root, and the script
# may be launched from anywhere.
Set-Location $repo
$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
$baselineDir = Join-Path $repo 'docs\baseline'
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$goShim = Join-Path $repo 'tools\go.cmd'

function Write-Section([string]$Title) {
    Write-Host ''
    Write-Host ("=" * 72) -ForegroundColor DarkGray
    Write-Host "  $Title" -ForegroundColor Cyan
    Write-Host ("=" * 72) -ForegroundColor DarkGray
}

function Invoke-Curl {
    param([string[]]$Arguments)
    # stdout only: Windows PowerShell 5.1 turns native stderr into a terminating
    # NativeCommandError under the default ErrorActionPreference.
    return (& curl.exe @Arguments 2>$null | Out-String)
}

function Read-Text {
    param([string]$Path)
    if (-not (Test-Path $Path)) { return '' }
    $reader = New-Object System.IO.StreamReader($Path, $script:utf8NoBom, $true)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
}

function Get-Header {
    param([string]$Headers, [string]$Name)
    $m = [regex]::Match($Headers, "(?im)^$([regex]::Escape($Name)):\s*(.+?)\s*$")
    if ($m.Success) { return $m.Groups[1].Value }
    return ''
}

function New-BodyFile {
    param([string]$Name, [string]$Json)
    $path = Join-Path $tmpDir $Name
    [System.IO.File]::WriteAllText($path, $Json, $script:utf8NoBom)
    return "@$path"
}

function Write-Config {
    <# Instantiates a shipped config onto the ports this run owns. #>
    param([string]$Source, [string]$Target, [hashtable]$Replace)
    $text = Read-Text (Join-Path $repo $Source)
    foreach ($k in $Replace.Keys) { $text = $text.Replace($k, $Replace[$k]) }
    [System.IO.File]::WriteAllText($Target, $text, $utf8NoBom)
    Write-Host "  wrote $Target"
}

function Start-Mock {
    param([int]$Port, [string]$Name, [string]$Tag, [int]$TtfbMs = 0, [int]$TokenDelayMs = 0)
    $log = Join-Path $tmpDir "m1-measure-$Tag.log"
    [System.IO.File]::WriteAllText($log, '', $utf8NoBom)
    $args = @('-listen', ":$Port", '-name', $Name, '-token-delay', "${TokenDelayMs}ms")
    if ($TtfbMs -gt 0) { $args += @('-ttfb', "${TtfbMs}ms") }
    $p = Start-Process -FilePath (Join-Path $binDir 'mockupstream.exe') -ArgumentList $args `
        -RedirectStandardOutput $log -RedirectStandardError (Join-Path $tmpDir "m1-measure-$Tag.err") `
        -PassThru -WindowStyle Hidden
    return $p
}

function Start-Gateway {
    param([int]$Port, [string]$ConfigPath, [string]$Tag)
    $log = Join-Path $tmpDir "m1-measure-$Tag.log"
    [System.IO.File]::WriteAllText($log, '', $utf8NoBom)
    $p = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') -ArgumentList @('-config', $ConfigPath) `
        -RedirectStandardOutput $log -RedirectStandardError (Join-Path $tmpDir "m1-measure-$Tag.err") `
        -PassThru -WindowStyle Hidden
    return $p
}

function Wait-Healthy {
    param([int]$Port, [string]$Path = '/healthz', [int]$Seconds = 20)
    $deadline = (Get-Date).AddSeconds($Seconds)
    $code = ''
    while ((Get-Date) -lt $deadline) {
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$Port$Path")).Trim()
        if ($code -eq '200') { return $code }
        Start-Sleep -Milliseconds 150
    }
    return $code
}

function Get-Median {
    param([double[]]$Values)
    $sorted = @($Values | Sort-Object)
    if ($sorted.Count -eq 0) { return 0.0 }
    return $sorted[[int][Math]::Floor($sorted.Count / 2)]
}

# ---------------------------------------------------------------------------
# 0. Preconditions / build
# ---------------------------------------------------------------------------
Write-Section '0. Preconditions and build'
if (-not (Get-Command curl.exe -ErrorAction SilentlyContinue)) { throw 'curl.exe not found on PATH' }
if (-not (Test-Path $goShim)) { throw "missing toolchain shim: $goShim" }
New-Item -ItemType Directory -Force -Path $tmpDir, $baselineDir | Out-Null

& $goShim build -o (Join-Path $binDir 'infergate.exe') ./cmd/infergate
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/infergate failed (rc=$LASTEXITCODE)" }
& $goShim build -o (Join-Path $binDir 'mockupstream.exe') ./cmd/mockupstream
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/mockupstream failed (rc=$LASTEXITCODE)" }
Write-Host '  built bin\infergate.exe and bin\mockupstream.exe' -ForegroundColor Green

# The M0-shaped config: one upstream, no routing block, no health block. This is
# the M0 code path -- upstreams.Resolve, no candidate scoring, no breaker.
$singleCfg = Join-Path $tmpDir 'm1-measure-single.yaml'
Write-Config -Source 'configs\mock.yaml' -Target $singleCfg -Replace @{
    'listen: ":8080"'            = "listen: `":$SingleGatewayPort`""
    'http://127.0.0.1:9000'      = "http://127.0.0.1:$M0MockPort"
    'upstream_timeout: "10m"'    = 'upstream_timeout: "30s"'
}

# The M1 fleet: three replicas, priority routing, a breaker per upstream.
$routerCfg = Join-Path $tmpDir 'm1-measure-router.yaml'
Write-Config -Source 'configs\routing-local.yaml' -Target $routerCfg -Replace @{
    'listen: ":8080"'          = "listen: `":$RouterGatewayPort`""
    'http://127.0.0.1:9100'    = "http://127.0.0.1:$PrimaryPort"
    'http://127.0.0.1:9101'    = "http://127.0.0.1:$SecondaryPort"
    'http://127.0.0.1:9102'    = "http://127.0.0.1:$ToolsPort"
}

# The breaker probe: a stalled priority-1 backend, a healthy fallback, and a
# per-attempt budget short enough that the stall is a timeout rather than a
# wait. min_requests=3 is deliberately tiny so the trip happens inside a
# 10-request probe; the ratio semantics are the production ones.
$slowCfg = Join-Path $tmpDir 'm1-measure-slow.yaml'
$slowYaml = @"
# Generated by scripts/measure-m1.ps1 -- do not edit.
server:
  listen: ":$SlowGatewayPort"
  read_header_timeout: "10s"
  idle_timeout: "90s"
  upstream_timeout: "${UpstreamTimeoutMs}ms"
  shutdown_timeout: "5s"
  max_body_bytes: 8388608
  max_idle_conns_per_host: 256

log:
  level: "info"
  format: "text"

routing:
  strategy: "priority"
  default_capabilities:
    - "chat"

health:
  window: "60s"
  buckets: 6
  min_requests: 3
  failure_ratio: 0.5
  open_duration: "30s"
  half_open_probes: 1
  max_failures_per_request: 2
  retry_backoff: "0s"

upstreams:
  - name: "stalled"
    kind: "openai"
    base_url: "http://127.0.0.1:$SlowPort"
    api_key: ""
    models:
      - "/"
    capabilities:
      - "chat"
    priority: 1
    weight: 1

  - name: "fast"
    kind: "openai"
    base_url: "http://127.0.0.1:$FastPort"
    api_key: ""
    models:
      - "/"
    capabilities:
      - "chat"
    priority: 2
    weight: 1
"@
[System.IO.File]::WriteAllText($slowCfg, $slowYaml, $utf8NoBom)
Write-Host "  wrote $slowCfg (upstream_timeout ${UpstreamTimeoutMs}ms, stalled primary ${SlowStallMs}ms TTFB)"

# ---------------------------------------------------------------------------
# 1. Bring the whole host up: two gateways for the load comparison, a third for
#    the breaker probe, and six mock replicas.
# ---------------------------------------------------------------------------
$procs = @()
try {
    Write-Section '1. Start the fleet'
    $primary = Start-Mock -Port $PrimaryPort -Name 'primary' -Tag 'primary'
    $procs += @(
        (Start-Mock -Port $M0MockPort -Name 'm0mock' -Tag 'm0mock'),
        $primary,
        (Start-Mock -Port $SecondaryPort -Name 'secondary' -Tag 'secondary'),
        (Start-Mock -Port $ToolsPort -Name 'tools' -Tag 'tools'),
        # The probe mocks keep a small token delay so a stream visibly streams.
        (Start-Mock -Port $SlowPort -Name 'stalled' -Tag 'stalled' -TtfbMs $SlowStallMs -TokenDelayMs 2),
        (Start-Mock -Port $FastPort -Name 'fast' -Tag 'fast' -TokenDelayMs 2)
    )

    foreach ($m in @(
            @{ Port = $M0MockPort; Name = 'm0mock' }, @{ Port = $PrimaryPort; Name = 'primary' },
            @{ Port = $SecondaryPort; Name = 'secondary' }, @{ Port = $ToolsPort; Name = 'tools' },
            @{ Port = $SlowPort; Name = 'stalled' }, @{ Port = $FastPort; Name = 'fast' })) {
        $code = Wait-Healthy -Port $m.Port
        if ($code -ne '200') { throw "mock $($m.Name) on :$($m.Port) never became healthy (last HTTP $code)" }
        Write-Host "  $($m.Name) :$($m.Port) ok"
    }

    $singleGw = Start-Gateway -Port $SingleGatewayPort -ConfigPath $singleCfg -Tag 'gw-single'
    $routerGw = Start-Gateway -Port $RouterGatewayPort -ConfigPath $routerCfg -Tag 'gw-router'
    $slowGw = Start-Gateway -Port $SlowGatewayPort -ConfigPath $slowCfg -Tag 'gw-slow'
    $procs += @($singleGw, $routerGw, $slowGw)
    foreach ($g in @(
            @{ Port = $SingleGatewayPort; Name = 'gw-single' }, @{ Port = $RouterGatewayPort; Name = 'gw-router' },
            @{ Port = $SlowGatewayPort; Name = 'gw-slow' })) {
        $code = Wait-Healthy -Port $g.Port -Path '/readyz'
        if ($code -ne '200') { throw "gateway $($g.Name) on :$($g.Port) never became ready (last HTTP $code)" }
        Write-Host "  $($g.Name) :$($g.Port) ready"
    }

    # -----------------------------------------------------------------------
    # 2. Interleaved rounds: single, router, and router-with-a-dead-replica
    # -----------------------------------------------------------------------
    Write-Section '2. Load rounds (single vs router vs faulted)'
    $singleRuns = @()
    $routerRuns = @()
    $faultedRuns = @()
    for ($r = 1; $r -le $Rounds; $r++) {
        Write-Host "  round $r/$Rounds" -ForegroundColor Cyan

        # A fresh window for every round: otherwise round 2 inherits round 1's
        # trip and the "healthy fleet" measurement is really a failover one.
        $null = Invoke-Curl @('-s', '-X', 'POST', "http://127.0.0.1:$RouterGatewayPort/admin/breakers/reset")

        $out = Join-Path $baselineDir "m1-load-single-r$r.json"
        & $goShim run ./cmd/loadtest -url "http://127.0.0.1:$SingleGatewayPort" -c $Concurrency -n $Requests -warmup $Warmup -out $out
        if ($LASTEXITCODE -ne 0) { throw "loadtest (single) round $r failed (rc=$LASTEXITCODE)" }
        $singleRuns += $out

        $out = Join-Path $baselineDir "m1-load-router-r$r.json"
        & $goShim run ./cmd/loadtest -url "http://127.0.0.1:$RouterGatewayPort" -c $Concurrency -n $Requests -warmup $Warmup -out $out
        if ($LASTEXITCODE -ne 0) { throw "loadtest (router) round $r failed (rc=$LASTEXITCODE)" }
        $routerRuns += $out

        # Kill the priority-1 replica. The client must not notice: this is the
        # claim M1 exists to make.
        Stop-Process -Id $primary.Id -Force -ErrorAction SilentlyContinue
        Start-Sleep -Milliseconds 300

        $out = Join-Path $baselineDir "m1-load-faulted-r$r.json"
        & $goShim run ./cmd/loadtest -url "http://127.0.0.1:$RouterGatewayPort" -c $Concurrency -n $Requests -warmup $Warmup -out $out
        if ($LASTEXITCODE -ne 0) { throw "loadtest (faulted) round $r failed (rc=$LASTEXITCODE)" }
        $faultedRuns += $out

        $primary = Start-Mock -Port $PrimaryPort -Name 'primary' -Tag 'primary'
        $procs += $primary
        if ((Wait-Healthy -Port $PrimaryPort) -ne '200') { throw 'the replacement primary replica never became healthy' }
        $null = Invoke-Curl @('-s', '-X', 'POST', "http://127.0.0.1:$RouterGatewayPort/admin/breakers/reset")
    }

    # -----------------------------------------------------------------------
    # 3. The breaker's latency saving, request by request
    # -----------------------------------------------------------------------
    Write-Section "3. Breaker saving: ${SlowStallMs}ms stall behind a ${UpstreamTimeoutMs}ms budget"
    $probeBody = New-BodyFile 'm1-measure-probe.json' '{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"probe"}]}'
    $probe = @()
    for ($i = 1; $i -le $ProbeRequests; $i++) {
        $bodyPath = Join-Path $tmpDir "m1-measure-probe-$i.body"
        $hdrPath = Join-Path $tmpDir "m1-measure-probe-$i.hdr"
        $out = (Invoke-Curl @(
                '-s', '-o', $bodyPath, '-D', $hdrPath, '-w', '%{http_code} %{time_total}',
                "http://127.0.0.1:$SlowGatewayPort/v1/chat/completions",
                '-H', 'Content-Type: application/json', '--data-binary', $probeBody)).Trim()
        $parts = $out -split '\s+'
        $status = 0
        if ($parts[0] -match '^\d+$') { $status = [int]$parts[0] }
        $ms = 0.0
        if ($parts.Count -gt 1) {
            $ms = [double]::Parse($parts[1], [System.Globalization.CultureInfo]::InvariantCulture) * 1000.0
        }
        $hdr = Read-Text $hdrPath
        $entry = [pscustomobject]@{
            n        = $i
            status   = $status
            ms       = [Math]::Round($ms, 2)
            upstream = Get-Header $hdr 'X-InferGate-Upstream-Name'
            attempts = Get-Header $hdr 'X-InferGate-Attempt'
            tried    = Get-Header $hdr 'X-InferGate-Tried'
        }
        $probe += $entry
        Write-Host ("    #{0,-2} {1} {2,7:N2}ms  upstream={3,-8} attempts={4} tried='{5}'" -f `
                $entry.n, $entry.status, $entry.ms, $entry.upstream, $entry.attempts, $entry.tried)
    }

    $breakerDoc = (Invoke-Curl @('-s', "http://127.0.0.1:$SlowGatewayPort/admin/breakers")) | ConvertFrom-Json
    $stalledBreaker = @($breakerDoc.upstreams | Where-Object { $_.name -eq 'stalled' })[0]

    $slowPhase = @($probe | Where-Object { $_.attempts -ne '1' })
    $fastPhase = @($probe | Where-Object { $_.attempts -eq '1' })
    $probeSummary = [pscustomobject]@{
        scenario                       = 'stalled-primary behind a per-attempt timeout'
        stall_ms                       = $SlowStallMs
        upstream_timeout_ms            = $UpstreamTimeoutMs
        requests                       = $ProbeRequests
        all_succeeded                   = (@($probe | Where-Object { $_.status -ne 200 }).Count -eq 0)
        requests_that_paid_a_timeout   = $slowPhase.Count
        median_ms_while_breaker_closed = [Math]::Round((Get-Median @($slowPhase | ForEach-Object { $_.ms })), 2)
        median_ms_once_breaker_open    = [Math]::Round((Get-Median @($fastPhase | ForEach-Object { $_.ms })), 2)
        breaker_state                  = $stalledBreaker.state
        breaker_attempts               = $stalledBreaker.attempts
        breaker_failures               = $stalledBreaker.failures
        breaker_timeouts               = $stalledBreaker.timeouts
        breaker_failure_ratio          = $stalledBreaker.failure_ratio
    }
    $probeDoc = [pscustomobject]@{ summary = $probeSummary; series = $probe }
    [System.IO.File]::WriteAllText((Join-Path $baselineDir 'm1-failover.json'),
        ((ConvertTo-Json $probeDoc -Depth 6) + "`n"), $utf8NoBom)

    Write-Host ''
    Write-Host "  all requests answered 200          : $($probeSummary.all_succeeded)" -ForegroundColor Green
    Write-Host "  requests that paid a timeout       : $($probeSummary.requests_that_paid_a_timeout) / $ProbeRequests"
    Write-Host "  median while the breaker was closed: $($probeSummary.median_ms_while_breaker_closed)ms"
    Write-Host "  median once the breaker opened     : $($probeSummary.median_ms_once_breaker_open)ms"
    Write-Host "  stalled breaker state              : $($probeSummary.breaker_state) (attempts=$($probeSummary.breaker_attempts) failures=$($probeSummary.breaker_failures) timeouts=$($probeSummary.breaker_timeouts))"

    # -----------------------------------------------------------------------
    # 4. Reduction: medians per scenario, written as data AND as a table
    # -----------------------------------------------------------------------
    Write-Section '4. Results'
    $scenarios = [ordered]@{
        'M0 single upstream'   = $singleRuns
        'M1 router (healthy)'  = $routerRuns
        'M1 router (1 replica down)' = $faultedRuns
    }
    $rows = @()
    $keys = @()
    foreach ($f in $singleRuns) {
        foreach ($r in (Get-Content $f -Raw | ConvertFrom-Json)) {
            $k = "$($r.stream)|$($r.concurrency)"
            if ($keys -notcontains $k) { $keys += $k }
        }
    }
    foreach ($name in $scenarios.Keys) {
        foreach ($k in $keys) {
            $stream, $conc = $k -split '\|'
            $qps = @()
            $p95 = @()
            $p99 = @()
            $errs = 0
            foreach ($f in $scenarios[$name]) {
                foreach ($r in (Get-Content $f -Raw | ConvertFrom-Json)) {
                    if ("$($r.stream)" -eq $stream -and "$($r.concurrency)" -eq $conc) {
                        $qps += [double]$r.qps
                        $p95 += [double]$r.p95
                        $p99 += [double]$r.p99
                        $errs += [int]$r.errors
                    }
                }
            }
            $rows += [pscustomobject]@{
                Scenario    = $name
                Workload    = $(if ($stream -eq 'True') { 'stream' } else { 'non-stream' })
                Concurrency = [int]$conc
                QPS         = [Math]::Round((Get-Median $qps), 0)
                QPSMin      = [Math]::Round(($qps | Measure-Object -Minimum).Minimum, 0)
                QPSMax      = [Math]::Round(($qps | Measure-Object -Maximum).Maximum, 0)
                P95_ms      = [Math]::Round((Get-Median $p95) / 1e6, 3)
                P99_ms      = [Math]::Round((Get-Median $p99) / 1e6, 3)
                Errors      = $errs
            }
        }
    }
    $rows | Format-Table -AutoSize | Out-String -Width 200 | Write-Host
    $summaryDoc = [pscustomobject]@{
        generated_at = (Get-Date).ToString('s')
        host         = "$env:COMPUTERNAME / $env:PROCESSOR_IDENTIFIER"
        rounds       = $Rounds
        requests     = $Requests
        warmup       = $Warmup
        rows         = $rows
        failover     = $probeSummary
    }
    [System.IO.File]::WriteAllText((Join-Path $baselineDir 'm1-summary.json'),
        ((ConvertTo-Json $summaryDoc -Depth 6) + "`n"), $utf8NoBom)
    Write-Host "  wrote docs\baseline\m1-summary.json and m1-load-*.json"
}
finally {
    Write-Section '5. Teardown'
    if (-not $KeepRunning) {
        foreach ($p in $procs) {
            if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
        }
        Write-Host '  stopped every mock and gateway started by this run'
    }
    else {
        Write-Host '  -KeepRunning set: leaving the fleet up' -ForegroundColor DarkYellow
    }
}
