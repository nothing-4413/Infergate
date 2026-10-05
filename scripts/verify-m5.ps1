#Requires -Version 5.1
<#
  InferGate M5 -- end-to-end observability acceptance gate (real processes, curl).

  Where cmd/verify-m5 asserts the M5 contract in-process (histograms, the bounded
  latency ring, the trace store and the exporters against httptest backends), THIS
  script proves the same surfaces through the shipped binary:

    * one real gateway process, three real upstream processes and the real
      cmd/mockcollector process standing in for an OTLP receiver,
    * a trace per proxied request, discoverable by trace id, by request id and
      through the JSONL listing,
    * traceparent continuation: the caller's trace id survives, the caller's span
      becomes the parent of the gateway span, and (the part only a real process can
      show) the gateway hands the CHILD context to the upstream -- mockcollector
      records the headers it received, so the propagated traceparent is read back
      off the wire,
    * the bounded trace store dropping the oldest traces,
    * OTLP/HTTP JSON export reaching a real socket,
    * the Prometheus exposition contract (HELP/TYPE, the M0 regex, +Inf == _count)
      and the /stats window/drop counters,
    * a failing upstream: attempt spans with error status, the failover event, and
      a 502 whose trace explains itself.

  Everything runs on its own ports, in its own tmp directory, and is torn down in a
  finally block. Use -KeepRunning to leave the fleet up for inspection.

  Run it as:
      powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m5.ps1
#>
[CmdletBinding()]
param(
    [int]$GatewayPort = 18510,
    [int]$PrimaryPort = 19710,
    [int]$BackupPort = 19711,
    [int]$CollectorPort = 19999,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# Paths and run state
# ---------------------------------------------------------------------------

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
$goShim = Join-Path $repo 'tools\go.cmd'
$stamp = "$(Get-Date -Format 'yyyyMMdd-HHmmss')-$PID"
$script:utf8NoBom = New-Object System.Text.UTF8Encoding($false)

if (-not (Test-Path $tmpDir)) { New-Item -ItemType Directory -Path $tmpDir | Out-Null }

$script:passed = 0
$script:failed = 0
$script:notes = @()
$script:procs = [ordered]@{}
$script:startedPids = @()
$script:proxied = 0
$script:traced = 0

$configPath = Join-Path $tmpDir "m5-verify-$stamp.yaml"
$traceFile = Join-Path $tmpDir "m5-traces-$stamp.jsonl"
$traceFileFwd = $traceFile.Replace('\', '/')
$gwLog = Join-Path $tmpDir "m5-gateway-$stamp.log"
$gwErr = Join-Path $tmpDir "m5-gateway-$stamp.err"
$primaryLog = Join-Path $tmpDir "m5-primary-$stamp.log"
$primaryErr = Join-Path $tmpDir "m5-primary-$stamp.err"
$backupLog = Join-Path $tmpDir "m5-backup-$stamp.log"
$backupErr = Join-Path $tmpDir "m5-backup-$stamp.err"
$collectorLog = Join-Path $tmpDir "m5-collector-$stamp.log"
$collectorErr = Join-Path $tmpDir "m5-collector-$stamp.err"
$checkOutFile = Join-Path $tmpDir "m5-check-$stamp.out"
$checkErrFile = Join-Path $tmpDir "m5-check-$stamp.err"
$buildLog = Join-Path $tmpDir "m5-build-$stamp.log"

$gwBase = "http://127.0.0.1:$GatewayPort"
$colBase = "http://127.0.0.1:$CollectorPort"

# The traceparent the caller sends: version 00, a fixed trace id, a fixed parent
# span id and the sampled flag set.
$callerTraceId = '4bf92f3577b34da6a3ce929d0e0e4736'
$callerSpanId = '00f067aa0ba902b7'
$callerTraceparent = "00-$callerTraceId-$callerSpanId-01"

# ---------------------------------------------------------------------------
# Assertion helpers
# ---------------------------------------------------------------------------

function Write-Section {
    param([string]$Title)
    Write-Host ''
    Write-Host ("-- $Title " + ("-" * [Math]::Max(0, 68 - $Title.Length))) -ForegroundColor Cyan
}

function Assert-True {
    param([string]$Label, [bool]$Condition, [string]$Detail = '')
    if ($Condition) {
        $script:passed++
        Write-Host "  PASS  $Label" -ForegroundColor Green
    }
    else {
        $script:failed++
        $script:notes += $Label
        if ($Detail) { Write-Host "  FAIL  $Label -- $Detail" -ForegroundColor Red }
        else { Write-Host "  FAIL  $Label" -ForegroundColor Red }
    }
}

Set-Alias -Name Assert-That -Value Assert-True

function Assert-Equal {
    param([string]$Label, $Expected, $Actual, [string]$Extra = '')
    $ok = ("$Expected" -ceq "$Actual")
    $detail = "want '$Expected', got '$Actual'"
    if ($Extra) { $detail = "$detail ($Extra)" }
    Assert-True -Label $Label -Condition $ok -Detail $detail
}

function Assert-Contains {
    param([string]$Label, [string]$Haystack, [string]$Needle, [string]$Extra = '')
    if ($null -eq $Haystack) { $Haystack = '' }
    $ok = $Haystack.Contains($Needle)
    $detail = "missing '$Needle'"
    if ($Extra) { $detail = "$detail ($Extra)" }
    Assert-True -Label $Label -Condition $ok -Detail $detail
}

function Assert-Number {
    param([string]$Label, [string]$Text, [double]$Min, [double]$Max)
    $value = 0.0
    $ok = [double]::TryParse($Text, [ref]$value)
    if (-not $ok) {
        Assert-True -Label $Label -Condition $false -Detail "not a number: '$Text'"
        return
    }
    Assert-True -Label $Label -Condition ($value -ge $Min -and $value -le $Max) -Detail "got $value, want [$Min, $Max]"
}

function Add-Note {
    param([string]$Text)
    Write-Host "  note  $Text" -ForegroundColor DarkGray
}

# ---------------------------------------------------------------------------
# Process / IO helpers
# ---------------------------------------------------------------------------

function Invoke-Curl {
    param([string[]]$Arguments)
    return (& curl.exe @Arguments 2>$null | Out-String)
}

function Read-Text {
    param([string]$Path)
    if (-not (Test-Path $Path)) { return '' }
    $sr = New-Object System.IO.StreamReader($Path, [System.Text.Encoding]::UTF8)
    try { return $sr.ReadToEnd() } finally { $sr.Dispose() }
}

# The gateway holds its own log open for writing, so a plain read would collide
# with it: open with FileShare.ReadWrite instead.
function Read-OpenLog {
    param([string]$Path)
    if (-not (Test-Path $Path)) { return '' }
    $fs = New-Object System.IO.FileStream($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    try {
        $sr = New-Object System.IO.StreamReader($fs)
        return $sr.ReadToEnd()
    }
    finally { $fs.Dispose() }
}

function New-TmpPath {
    param([string]$Tag)
    return (Join-Path $tmpDir "m5-$Tag-$stamp.txt")
}

function New-BodyFile {
    param([string]$Name, [string]$Json)
    $path = Join-Path $tmpDir "m5-body-$Name-$stamp.json"
    [System.IO.File]::WriteAllText($path, $Json, $script:utf8NoBom)
    return "@$path"
}

function Get-Header {
    param([string]$Headers, [string]$Name)
    if (-not $Headers) { return '' }
    $m = [regex]::Match($Headers, "(?im)^$([regex]::Escape($Name)):\s*(.+?)\s*$")
    if ($m.Success) { return $m.Groups[1].Value }
    return ''
}

function Invoke-Http {
    param(
        [string]$Url,
        [string]$Method = 'GET',
        [string[]]$Headers = @(),
        [string]$BodiesFile = ''
    )
    $bodyPath = New-TmpPath ("out-" + [guid]::NewGuid().ToString('N').Substring(0, 8))
    $headerPath = New-TmpPath ("hdr-" + [guid]::NewGuid().ToString('N').Substring(0, 8))
    $arguments = @('-s', '-o', $bodyPath, '-D', $headerPath, '-w', '%{http_code}', '-X', $Method, $Url)
    foreach ($h in $Headers) { $arguments += @('-H', $h) }
    if ($BodiesFile) { $arguments += @('--data-binary', $BodiesFile) }
    $status = (Invoke-Curl -Arguments $arguments).Trim()
    return [pscustomobject]@{
        Status      = $status
        StatusCode  = $status
        Headers     = (Read-Text $headerPath)
        Body        = (Read-Text $bodyPath)
        BodyPath    = $bodyPath
        HeaderPath  = $headerPath
    }
}

function Get-Json {
    param([string]$Text)
    if (-not $Text) { return $null }
    try { return ($Text | ConvertFrom-Json) } catch { return $null }
}

function Send-Chat {
    param([string]$BodyFile, [string[]]$Headers = @(), [string]$Tag = 'chat')
    $hs = @('Content-Type: application/json') + $Headers
    return Invoke-Http -Url "$gwBase/v1/chat/completions" -Method 'POST' -Headers $hs -BodiesFile $BodyFile
}

function New-ChatBody {
    param([string]$Tag, [switch]$Stream)
    $tail = ''
    if ($Stream) { $tail = ',"stream":true' }
    return '{"model":"mock-gpt","messages":[{"role":"user","content":"' + $Tag + '"}],"max_tokens":16' + $tail + '}'
}

# Every proxied request is counted by the metrics, but only a request the tracer
# decided to record leaves a trace -- so the two counters are kept apart and the
# store assertions use $script:traced.
function Send-Proxied {
    param([string]$Tag, [string[]]$Headers = @(), [switch]$Stream, [switch]$NoTrace)
    $script:proxied++
    if (-not $NoTrace) { $script:traced++ }
    $bodyFile = New-BodyFile $Tag (New-ChatBody -Tag $Tag -Stream:$Stream)
    return Send-Chat -BodyFile $bodyFile -Headers $Headers -Tag $Tag
}

function Test-PortOpen {
    param([int]$Port, [int]$TimeoutMs = 400)
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $task = $client.ConnectAsync('127.0.0.1', $Port)
        if (-not $task.Wait($TimeoutMs)) { return $false }
        return $client.Connected
    }
    catch { return $false }
    finally { $client.Close() }
}

function Wait-PortOpen {
    param([int]$Port, [int]$Seconds = 15)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (Test-PortOpen -Port $Port) { return $true }
        Start-Sleep -Milliseconds 150
    }
    return $false
}

function Wait-PortClosed {
    param([int]$Port, [int]$Seconds = 10)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (-not (Test-PortOpen -Port $Port)) { return $true }
        Start-Sleep -Milliseconds 150
    }
    return $false
}

function Wait-Healthy {
    param([int]$Port, [string]$Path = '/healthz', [int]$Seconds = 15)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        $code = (Invoke-Curl -Arguments @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$Port$Path")).Trim()
        if ($code -match '200') { return $true }
        Start-Sleep -Milliseconds 150
    }
    return $false
}

function Start-Binary {
    param([string]$Exe, [string[]]$Arguments, [string]$Tag, [string]$Log, [string]$Err)
    $p = Start-Process -FilePath (Join-Path $binDir $Exe) -ArgumentList $Arguments `
        -WorkingDirectory $repo -RedirectStandardOutput $Log -RedirectStandardError $Err `
        -PassThru -WindowStyle Hidden
    $script:procs[$Tag] = $p
    $script:startedPids += $p.Id
    return $p
}

function Stop-Named {
    param([string]$Tag, [string]$Label)
    $p = $script:procs[$Tag]
    if ($p -and -not $p.HasExited) {
        Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
        Write-Host "  stopped $Label (pid=$($p.Id))" -ForegroundColor DarkGray
    }
}

function Stop-Fleet {
    if ($KeepRunning) {
        Write-Host '  -KeepRunning set: leaving the fleet up' -ForegroundColor DarkYellow
        return
    }
    foreach ($tag in @($script:procs.Keys)) {
        $p = $script:procs[$tag]
        if ($p -and -not $p.HasExited) {
            Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
            Write-Host "  stopped $tag (pid=$($p.Id))" -ForegroundColor DarkGray
        }
    }
}

function Invoke-GoBuild {
    param([string]$Out, [string]$Pkg)
    for ($i = 1; $i -le 3; $i++) {
        [System.IO.File]::WriteAllText($buildLog, '', $script:utf8NoBom)
        & $goShim build -o $Out $Pkg 2>$buildLog | Out-Null
        if ($LASTEXITCODE -eq 0) { return $true }
        Write-Host "  build attempt $i failed:" -ForegroundColor DarkYellow
        Write-Host (Read-Text $buildLog) -ForegroundColor DarkYellow
        Start-Sleep -Milliseconds 500
    }
    return $false
}

# A config the gate EXPECTS to be rejected must not abort the run, so the check
# goes through Start-Process (exit code as data) instead of 2>&1.
function Invoke-Check {
    param([string]$Config)
    [System.IO.File]::WriteAllText($checkOutFile, '', $script:utf8NoBom)
    [System.IO.File]::WriteAllText($checkErrFile, '', $script:utf8NoBom)
    $proc = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $Config, '-check') `
        -RedirectStandardOutput $checkOutFile -RedirectStandardError $checkErrFile -PassThru -Wait
    return [pscustomobject]@{ Exit = $proc.ExitCode; Text = ((Read-Text $checkOutFile) + (Read-Text $checkErrFile)) }
}

# ---------------------------------------------------------------------------
# Evidence helpers
# ---------------------------------------------------------------------------

function Get-MetricValue {
    param([string]$Text, [string]$Name, [string]$LabelFragment)
    $pattern = '(?m)^' + [regex]::Escape($Name) + '\{([^}]*)\}\s+([0-9.eE+-]+)\s*$'
    foreach ($m in [regex]::Matches($Text, $pattern)) {
        if ($m.Groups[1].Value.Contains($LabelFragment)) {
            return [double]$m.Groups[2].Value
        }
    }
    return -1.0
}

function Get-MetricValueNoLabels {
    param([string]$Text, [string]$Name)
    $pattern = '(?m)^' + [regex]::Escape($Name) + '\s+([0-9.eE+-]+)\s*$'
    $m = [regex]::Match($Text, $pattern)
    if ($m.Success) { return [double]$m.Groups[1].Value }
    return -1.0
}

function Get-MetricSum {
    param([string]$Text, [string]$Name)
    $pattern = '(?m)^' + [regex]::Escape($Name) + '(\{[^}]*\})?\s+([0-9.eE+-]+)\s*$'
    $sum = 0.0
    foreach ($m in [regex]::Matches($Text, $pattern)) { $sum += [double]$m.Groups[2].Value }
    return $sum
}

function Get-Lines {
    param([string]$Text)
    return @($Text -split "`n" | Where-Object { $_.Trim().Length -gt 0 })
}

function Get-Property {
    param($Object, [string]$Name)
    if ($null -eq $Object) { return $null }
    foreach ($p in $Object.PSObject.Properties) {
        if ($p.Name -ieq $Name) { return $p.Value }
    }
    return $null
}

function Get-RecordHeader {
    param($Record, [string]$Name)
    $header = Get-Property $Record 'header'
    if ($null -eq $header) { return '' }
    foreach ($p in $header.PSObject.Properties) {
        if ($p.Name -ieq $Name) {
            $values = @($p.Value)
            if ($values.Count -gt 0) { return "$($values[0])" }
        }
    }
    return ''
}

function Get-TraceByRequestId {
    param([string]$RequestId)
    $r = Invoke-Http -Url "$gwBase/admin/traces/$RequestId"
    return [pscustomobject]@{ Status = $r.Status; Body = $r.Body; Json = (Get-Json $r.Body) }
}

function Get-Spans {
    param($Trace)
    if ($null -eq $Trace) { return @() }
    return @($Trace.spans)
}

function Get-SpanNamed {
    param($Trace, [string]$Name)
    foreach ($s in (Get-Spans $Trace)) {
        if ("$($s.name)" -eq $Name) { return $s }
    }
    return $null
}

function Test-Duration {
    param([string]$Text)
    return [regex]::IsMatch("$Text", '^[0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h)')
}

function Get-Duration {
    param([string]$Text)
    $m = [regex]::Match("$Text", '^([0-9]+(?:\.[0-9]+)?)(ns|us|µs|ms|s|m|h)$')
    if (-not $m.Success) { return -1.0 }
    $v = [double]$m.Groups[1].Value
    switch ($m.Groups[2].Value) {
        'ns' { return $v / 1000000.0 }
        'us' { return $v / 1000.0 }
        'µs' { return $v / 1000.0 }
        'ms' { return $v }
        's' { return $v * 1000.0 }
        'm' { return $v * 60000.0 }
        'h' { return $v * 3600000.0 }
    }
    return -1.0
}

# ---------------------------------------------------------------------------
# Preamble
# ---------------------------------------------------------------------------

Write-Host ''
Write-Host ("=" * 72) -ForegroundColor DarkGray
Write-Host '  InferGate M5 -- tracing, observability and the metrics contract (curl gate)' -ForegroundColor White
Write-Host ("=" * 72) -ForegroundColor DarkGray
Write-Host "  repo      : $repo"
Write-Host "  gateway   : $GatewayPort   primary: $PrimaryPort   backup: $BackupPort   collector: $CollectorPort"
Write-Host "  traces    : $traceFile"
Write-Host "  stamp     : $stamp"

$exitCode = 1
try {

    # =======================================================================
    # 0. Preconditions
    # =======================================================================
    Write-Section '0. Preconditions'

    Assert-True '0.1  tools\go.cmd exists' (Test-Path $goShim)
    Assert-True '0.2  cmd\mockcollector exists' (Test-Path (Join-Path $repo 'cmd\mockcollector\main.go'))
    Assert-True '0.3  cmd\mockupstream exists' (Test-Path (Join-Path $repo 'cmd\mockupstream\main.go'))
    Assert-True '0.4  configs\observability.yaml exists' (Test-Path (Join-Path $repo 'configs\observability.yaml'))
    Assert-True '0.5  curl.exe is available' ($null -ne (Get-Command curl.exe -ErrorAction SilentlyContinue))

    $gwFree = -not (Test-PortOpen -Port $GatewayPort)
    Assert-True "0.6  gateway port $GatewayPort is free" $gwFree 'another gateway is already listening'
    $upFree = (-not (Test-PortOpen -Port $PrimaryPort)) -and (-not (Test-PortOpen -Port $BackupPort))
    Assert-True "0.7  upstream ports $PrimaryPort/$BackupPort are free" $upFree
    $colFree = -not (Test-PortOpen -Port $CollectorPort)
    Assert-True "0.8  collector port $CollectorPort is free" $colFree 'an OTLP sink is already listening'

    # The runnable sample config must still validate as shipped.
    $sample = Read-Text (Join-Path $repo 'configs\observability.yaml')
    $sampleTmp = Join-Path $tmpDir "m5-sample-$stamp.yaml"
    [System.IO.File]::WriteAllText($sampleTmp, $sample, $script:utf8NoBom)

    # =======================================================================
    # 1. Build
    # =======================================================================
    Write-Section '1. Build the binaries'

    Assert-True '1.1  built bin\infergate.exe' (Invoke-GoBuild -Out (Join-Path $binDir 'infergate.exe') -Pkg '.\cmd\infergate')
    Assert-True '1.2  built bin\mockupstream.exe' (Invoke-GoBuild -Out (Join-Path $binDir 'mockupstream.exe') -Pkg '.\cmd\mockupstream')
    Assert-True '1.3  built bin\mockcollector.exe' (Invoke-GoBuild -Out (Join-Path $binDir 'mockcollector.exe') -Pkg '.\cmd\mockcollector')

    $sampleCheck = Invoke-Check -Config $sampleTmp
    Assert-Equal '1.4  configs\observability.yaml passes -check' 0 $sampleCheck.Exit $sampleCheck.Text

    # =======================================================================
    # 2. The generated fleet config
    # =======================================================================
    Write-Section '2. Fleet config and processes'

    $cfgText = @"
server:
  listen: "127.0.0.1:$GatewayPort"
  upstream_timeout: "5s"
  shutdown_timeout: "5s"
  max_body_bytes: 8388608
  max_idle_conns_per_host: 64

log:
  level: "info"
  format: "text"

upstreams:
  - name: "primary"
    kind: "openai"
    base_url: "http://127.0.0.1:$PrimaryPort"
    api_key: "test-key"
    models: ["/"]
    capabilities: ["chat"]
    priority: 1
  - name: "backup"
    kind: "openai"
    base_url: "http://127.0.0.1:$BackupPort"
    api_key: "test-key"
    models: ["/"]
    capabilities: ["chat"]
    priority: 2
  - name: "probe"
    kind: "openai"
    base_url: "http://127.0.0.1:$CollectorPort"
    api_key: "test-key"
    models: ["/"]
    capabilities: ["chat"]
    priority: 3

routing:
  strategy: "priority"
  default_capabilities: ["chat"]

health:
  window: "30s"
  buckets: 6
  min_requests: 1000
  failure_ratio: 0.5
  open_duration: "30s"
  half_open_probes: 1
  max_failures_per_request: 2
  retry_backoff: "20ms"

tracing:
  enabled: true
  capacity: 4
  sample_ratio: 1.0
  jsonl_path: "$traceFileFwd"
  otlp:
    endpoint: "http://127.0.0.1:$CollectorPort"
    timeout: "5s"
    service_name: "infergate-m5-curl"
"@
    [System.IO.File]::WriteAllText($configPath, $cfgText, $script:utf8NoBom)

    $cfgCheck = Invoke-Check -Config $configPath
    Assert-Equal '2.1  the generated M5 config validates' 0 $cfgCheck.Exit $cfgCheck.Text

    Start-Binary -Exe 'mockcollector.exe' -Tag 'collector' -Log $collectorLog -Err $collectorErr `
        -Arguments @('-listen', "127.0.0.1:$CollectorPort", '-name', 'mockcollector', '-quiet') | Out-Null
    Start-Binary -Exe 'mockupstream.exe' -Tag 'primary' -Log $primaryLog -Err $primaryErr `
        -Arguments @('-listen', ":$PrimaryPort", '-name', 'primary', '-token-delay', '1ms') | Out-Null
    Start-Binary -Exe 'mockupstream.exe' -Tag 'backup' -Log $backupLog -Err $backupErr `
        -Arguments @('-listen', ":$BackupPort", '-name', 'backup', '-token-delay', '1ms') | Out-Null
    Start-Binary -Exe 'infergate.exe' -Tag 'gateway' -Log $gwLog -Err $gwErr `
        -Arguments @('-config', $configPath) | Out-Null

    Assert-True "2.2  collector is listening on $CollectorPort" (Wait-PortOpen -Port $CollectorPort)
    Assert-True "2.3  primary mock is listening on $PrimaryPort" (Wait-PortOpen -Port $PrimaryPort)
    Assert-True "2.4  backup mock is listening on $BackupPort" (Wait-PortOpen -Port $BackupPort)
    Assert-True "2.5  gateway is listening on $GatewayPort" (Wait-PortOpen -Port $GatewayPort)
    Assert-True '2.6  gateway /healthz answers 200' (Wait-Healthy -Port $GatewayPort)

    $upstreams = Get-Json (Invoke-Http -Url "$gwBase/admin/upstreams").Body
    $names = @()
    foreach ($u in @($upstreams.upstreams)) { $names += "$($u.name)" }
    Assert-True '2.7  /admin/upstreams lists primary, backup and probe' (($names -contains 'primary') -and ($names -contains 'backup') -and ($names -contains 'probe')) ("got " + ($names -join ', '))

    $tracing = Get-Json (Invoke-Http -Url "$gwBase/admin/tracing").Body
    Assert-True '2.8  /admin/tracing reports tracing enabled' ($tracing.enabled -eq $true)
    Assert-Equal '2.9  configured trace capacity' 4 $tracing.capacity
    Assert-Equal '2.10 configured jsonl_path' $traceFileFwd "$($tracing.jsonl_path)"
    $exporters = @($tracing.exporters)
    Assert-True '2.11 both exporters are registered' (($exporters -contains 'jsonl') -and ($exporters -contains 'otlp')) ("got " + ($exporters -join ', '))

    # =======================================================================
    # 3. A traced request
    # =======================================================================
    Write-Section '3. Every proxied request leaves a trace'

    $r1 = Send-Proxied -Tag 'm5-curl-0001' -Headers @('X-InferGate-Request-Id: m5-curl-0001')
    Assert-Equal '3.1  the chat request succeeded' 200 $r1.Status
    Assert-Equal '3.2  the caller request id is echoed back' 'm5-curl-0001' (Get-Header $r1.Headers 'X-InferGate-Request-Id')
    Assert-Equal '3.3  the priority router chose primary' 'primary' (Get-Header $r1.Headers 'X-InferGate-Upstream-Name')
    Assert-Contains '3.4  the answer came from the mock upstream' $r1.Body 'mock answer to'
    Assert-Equal '3.5  a single healthy candidate means no -Tried header' '' (Get-Header $r1.Headers 'X-InferGate-Tried')

    $list = Get-Json (Invoke-Http -Url "$gwBase/admin/traces").Body
    Assert-True '3.6  /admin/traces reports tracing enabled' ($list.enabled -eq $true)
    Assert-True '3.7  the store holds at least one trace' (@($list.traces).Count -ge 1)

    $t1 = Get-TraceByRequestId 'm5-curl-0001'
    Assert-Equal '3.8  a trace is retrievable by request id' 200 $t1.Status
    $trace1 = $t1.Json
    Assert-True '3.9  the trace carries a 32-hex trace id' ([regex]::IsMatch("$($trace1.trace_id)", '^[0-9a-f]{32}$')) "got '$($trace1.trace_id)'"
    Assert-Equal '3.10 the trace knows the request id' 'm5-curl-0001' "$($trace1.request_id)"
    Assert-Equal '3.11 the trace reports its route' '/v1/chat/completions' "$($trace1.attributes.route)"
    Assert-Equal '3.12 the root span reports the upstream that answered' 'primary' "$($trace1.spans[0].attributes.upstream)"
    Assert-Equal '3.13 the root span reports the model' 'mock-gpt' "$($trace1.spans[0].attributes.model)"
    Assert-Equal '3.14 the trace status is ok' 'ok' "$($trace1.status)"
    Assert-Equal '3.15 one request and one upstream attempt make two spans' 2 (@($trace1.spans).Count)
    Assert-Equal '3.16 the root span is the gateway span' 'gateway.request' "$($trace1.spans[0].name)"
    Assert-Equal '3.17 the root span is a server span' 'server' "$($trace1.spans[0].kind)"
    Assert-Equal '3.18 the root span has no parent of its own' '' "$($trace1.spans[0].parent_span_id)"
    Assert-Equal '3.19 the root span counted one attempt' 1 "$($trace1.spans[0].attributes.attempts)"
    Assert-Equal '3.20 the root span recorded the response status' 200 "$($trace1.spans[0].attributes.status)"
    Assert-Equal '3.21 the root span recorded the outcome' 'success' "$($trace1.spans[0].attributes.outcome)"

    $attempt1 = $trace1.spans[1]
    Assert-Equal '3.22 the attempt span names the upstream' 'upstream.primary' "$($attempt1.name)"
    Assert-Equal '3.23 the attempt span is a client span' 'client' "$($attempt1.kind)"
    Assert-Equal '3.24 the attempt span is parented to the root span' "$($trace1.spans[0].span_id)" "$($attempt1.parent_span_id)"
    Assert-Equal '3.25 the attempt span is numbered' 1 "$($attempt1.attributes.attempt)"
    Assert-Equal '3.26 the attempt span carries the same trace id' "$($trace1.trace_id)" "$($attempt1.trace_id)"
    Assert-True '3.27 the attempt span has a real duration' ([double]"$($attempt1.duration_ms)" -ge 0.0)
    Assert-True '3.28 the attempt span timestamps are nanoseconds' ([double]"$($attempt1.end_unix_nano)" -gt 1600000000000000000)

    # Lookup by trace id finds the same trace.
    $byTraceId = Get-TraceByRequestId "$($trace1.trace_id)"
    Assert-Equal '3.29 the trace is also retrievable by trace id' 200 $byTraceId.Status
    Assert-Equal '3.30 both lookups return the same request' 'm5-curl-0001' "$($byTraceId.Json.request_id)"

    # Lookup by the caller's own request id header.
    $r1b = Send-Proxied -Tag 'm5-curl-0001b' -Headers @('X-Request-Id: caller-side-id-0001b')
    $minted = Get-Header $r1b.Headers 'X-InferGate-Request-Id'
    Assert-True '3.31 an unrelated request id header does not become the gateway id' ([regex]::IsMatch($minted, '^[0-9a-f]{32}$')) "got '$minted'"
    $t1b = Get-TraceByRequestId 'caller-side-id-0001b'
    Assert-Equal '3.32 the gateway minted its own request id' 404 $t1b.Status

    # =======================================================================
    # 4. Traceparent continuation and propagation
    # =======================================================================
    Write-Section '4. traceparent is continued and propagated to the upstream'

    $r2 = Send-Proxied -Tag 'm5-curl-0002' -Headers @(
        'X-InferGate-Request-Id: m5-curl-0002',
        "traceparent: $callerTraceparent",
        'X-InferGate-Upstream: probe'
    )
    Assert-Equal '4.1  the pinned probe upstream answered' 200 $r2.Status
    Assert-Equal '4.2  the explicit pin was honoured' 'probe' (Get-Header $r2.Headers 'X-InferGate-Upstream-Name')

    $t2 = Get-TraceByRequestId 'm5-curl-0002'
    Assert-Equal '4.3  the pinned request produced a trace' 200 $t2.Status
    Assert-Equal '4.4  the caller trace id is continued' $callerTraceId "$($t2.Json.trace_id)"
    Assert-Equal '4.5  the caller span becomes the gateway span parent' $callerSpanId "$($t2.Json.spans[0].parent_span_id)"
    Assert-True '4.6  the gateway mints a fresh span id for the request' ("$($t2.Json.spans[0].span_id)" -ne $callerSpanId)

    # mockcollector recorded the headers the gateway actually put on the wire.
    $col = Get-Json (Invoke-Http -Url "$colBase/requests").Body
    $probeRec = $null
    foreach ($rec in @($col.requests)) {
        if ("$($rec.path)" -eq '/v1/chat/completions') { $probeRec = $rec }
    }
    Assert-True '4.7  the probe upstream received the proxied request' ($null -ne $probeRec)
    if ($probeRec) {
        $wireTraceparent = Get-RecordHeader -Record $probeRec -Name 'traceparent'
        Assert-True '4.8  the upstream received a traceparent' ($wireTraceparent.Length -gt 0)
        Assert-Contains '4.9  the propagated traceparent keeps the caller trace id' $wireTraceparent $callerTraceId
        Assert-True '4.10 the propagated traceparent carries a new child span id' ($wireTraceparent -ne $callerTraceparent)
        Assert-Contains '4.11 the propagated traceparent keeps the sampled flag' $wireTraceparent '-01'
        $wireRequestId = Get-RecordHeader -Record $probeRec -Name 'X-Request-Id'
        Assert-Equal '4.12 the upstream received the request id on X-Request-Id' 'm5-curl-0002' $wireRequestId
        Assert-True '4.13 the upstream received the body' ([int]"$($probeRec.bytes)" -gt 0)
    }
    else {
        Assert-True '4.8  the upstream received a traceparent' $false 'no recorded probe request'
    }

    # A caller that did not sample must not be recorded, but its context still travels.
    $r2b = Send-Proxied -Tag 'm5-curl-0002b' -NoTrace -Headers @(
        'X-InferGate-Request-Id: m5-curl-0002b',
        'traceparent: 00-11111111111111111111111111111111-2222222222222222-00',
        'X-InferGate-Upstream: probe'
    )
    Assert-Equal '4.14 an unsampled caller is still proxied' 200 $r2b.Status
    $t2b = Get-TraceByRequestId 'm5-curl-0002b'
    Assert-Equal '4.15 an unsampled caller leaves no trace' 404 $t2b.Status
    $col2 = Get-Json (Invoke-Http -Url "$colBase/requests").Body
    $unsampled = $null
    foreach ($rec in @($col2.requests)) {
        $rid = Get-RecordHeader -Record $rec -Name 'X-Request-Id'
        if ($rid -eq 'm5-curl-0002b') { $unsampled = $rec }
    }
    Assert-True '4.16 the unsampled context is still propagated' ($null -ne $unsampled)
    if ($unsampled) {
        $wire2 = Get-RecordHeader -Record $unsampled -Name 'traceparent'
        Assert-True '4.17 the unsampled traceparent keeps the caller trace id and the clear flag' `
            ($wire2.Contains('11111111111111111111111111111111') -and $wire2.EndsWith('-00')) "got '$wire2'"
    }
    else {
        Assert-True '4.17 the unsampled traceparent keeps the caller trace id and the clear flag' $false 'no recorded request'
    }

    # A malformed traceparent is replaced by a fresh root context.
    $r2c = Send-Proxied -Tag 'm5-curl-0002c' -Headers @(
        'X-InferGate-Request-Id: m5-curl-0002c',
        'traceparent: not-a-traceparent'
    )
    Assert-Equal '4.18 a malformed traceparent does not break the request' 200 $r2c.Status
    $t2c = Get-TraceByRequestId 'm5-curl-0002c'
    Assert-Equal '4.19 the request is still traced' 200 $t2c.Status
    Assert-True '4.20 a fresh root context was minted' ([regex]::IsMatch("$($t2c.Json.trace_id)", '^[0-9a-f]{32}$'))
    Assert-Equal '4.21 the fresh root span has no parent' '' "$($t2c.Json.spans[0].parent_span_id)"

    # =======================================================================
    # 5. Streaming
    # =======================================================================
    Write-Section '5. A streamed request records frames and time to first token'

    $r3 = Send-Proxied -Tag 'm5-curl-0003' -Stream -Headers @('X-InferGate-Request-Id: m5-curl-0003')
    Assert-Equal '5.1  the streamed request succeeded' 200 $r3.Status
    Assert-Contains '5.2  the body is an SSE stream' $r3.Body 'data: '
    Assert-Contains '5.3  the stream terminates with [DONE]' $r3.Body 'data: [DONE]'
    $frameCount = ([regex]::Matches($r3.Body, 'data: ')).Count
    Assert-True '5.4  the stream carried more than one frame' ($frameCount -gt 1) "frames=$frameCount"

    $t3 = Get-TraceByRequestId 'm5-curl-0003'
    Assert-Equal '5.5  the streamed request produced a trace' 200 $t3.Status
    Assert-Equal '5.6  the root span records that it was a stream' $true "$($t3.Json.spans[0].attributes.stream)"
    Assert-Equal '5.7  the root span records the frame count' $frameCount "$($t3.Json.spans[0].attributes.frames)"
    $ttft = $t3.Json.spans[0].attributes.first_token_ms
    Assert-True '5.8  the root span records a time to first token' ($null -ne $ttft -and [double]"$ttft" -ge 0.0) "got '$ttft'"
    $streamEvent = $null
    foreach ($ev in @($t3.Json.spans[0].events)) {
        if ("$($ev.name)" -eq 'stream.complete') { $streamEvent = $ev }
    }
    Assert-True '5.9  the root span carries a stream.complete event' ($null -ne $streamEvent)
    if ($streamEvent) {
        Assert-Equal '5.10 the event repeats the frame count' $frameCount "$($streamEvent.attributes.frames)"
        Assert-True '5.11 the event records a first_token_ms' ($null -ne $streamEvent.attributes.first_token_ms -and [double]"$($streamEvent.attributes.first_token_ms)" -ge 0.0) "got '$($streamEvent.attributes.first_token_ms)'"
    }
    else {
        Assert-True '5.10 the event repeats the frame count' $false 'no stream.complete event'
    }

    # =======================================================================
    # 6. The bounded store
    # =======================================================================
    Write-Section '6. The trace store is bounded and its listings are validated'

    while ($script:proxied -lt 9) {
        $tag = "m5-curl-fill-{0:d4}" -f ($script:proxied + 1)
        Send-Proxied -Tag $tag -Headers @("X-InferGate-Request-Id: $tag") | Out-Null
    }
    Assert-Equal '6.1  the gate sent more requests than the store can hold' 9 $script:proxied
    Assert-Equal '6.1b one request was deliberately not traced' ($script:proxied - 1) $script:traced

    $list2 = Get-Json (Invoke-Http -Url "$gwBase/admin/traces").Body
    Assert-Equal '6.2  the store holds exactly its capacity' 4 "$($list2.stored)"
    Assert-Equal '6.3  the listing returns the same count' 4 (@($list2.traces).Count)
    Assert-Equal '6.4  every trace beyond capacity was dropped' ($script:traced - 4) "$($list2.dropped)"
    Assert-Equal '6.5  stored + dropped accounts for every recorded request' $script:traced ([int]"$($list2.stored)" + [int]"$($list2.dropped)")
    Assert-Equal '6.6  the newest trace is listed first' ("m5-curl-fill-{0:d4}" -f 9) "$($list2.traces[0].request_id)"
    Assert-Equal '6.7  the oldest surviving trace is the streamed request' 'm5-curl-0003' "$($list2.traces[3].request_id)"

    $evicted = Get-TraceByRequestId 'm5-curl-0001'
    Assert-Equal '6.8  an evicted trace is gone' 404 $evicted.Status
    Assert-Contains '6.9  the 404 explains itself' $evicted.Body 'trace not found'

    $one = Get-Json (Invoke-Http -Url "$gwBase/admin/traces?limit=1").Body
    Assert-Equal '6.10 ?limit=1 returns one trace' 1 (@($one.traces).Count)
    $zero = Invoke-Http -Url "$gwBase/admin/traces?limit=0"
    Assert-Equal '6.11 ?limit=0 is rejected' 400 $zero.Status
    $bad = Invoke-Http -Url "$gwBase/admin/traces?limit=abc"
    Assert-Equal '6.12 ?limit=abc is rejected' 400 $bad.Status
    $badId = Invoke-Http -Url "$gwBase/admin/traces/not%20a%20valid%20id"
    Assert-Equal '6.13 a malformed trace id is rejected' 400 $badId.Status
    $missing = Invoke-Http -Url "$gwBase/admin/traces/0123456789abcdef0123456789abcdef"
    Assert-Equal '6.14 an unknown trace id is a 404' 404 $missing.Status

    $ndjson = Invoke-Http -Url "$gwBase/admin/traces?format=jsonl"
    Assert-Equal '6.15 the jsonl listing succeeded' 200 $ndjson.Status
    Assert-Contains '6.16 the jsonl listing sets an ndjson content type' (Get-Header $ndjson.Headers 'Content-Type') 'application/x-ndjson'
    $ndLines = Get-Lines $ndjson.Body
    Assert-Equal '6.17 the jsonl listing has one line per stored trace' 4 $ndLines.Count
    $allParse = $true
    foreach ($line in $ndLines) {
        $obj = Get-Json $line
        if ($null -eq $obj) { $allParse = $false }
        elseif ("$($obj.trace_id)".Length -ne 32) { $allParse = $false }
    }
    Assert-True '6.18 every jsonl line is a complete trace' $allParse

    # =======================================================================
    # 7. JSONL export on disk
    # =======================================================================
    Write-Section '7. Traces are exported to the JSONL file'

    Start-Sleep -Milliseconds 300
    $tracing2 = Get-Json (Invoke-Http -Url "$gwBase/admin/tracing").Body
    $jsonlStats = Get-Property $tracing2.export_stats 'jsonl'
    Assert-True '7.1  the jsonl reporter reports stats' ($null -ne $jsonlStats)
    if ($jsonlStats) {
        Assert-Equal '7.2  the reporter echoes its path' $traceFileFwd "$($jsonlStats.path)"
        Assert-Equal '7.3  the reporter wrote no errors' '' "$($jsonlStats.error)"
        $written = [int]"$($jsonlStats.written)"
        Assert-Equal '7.4  every recorded request was exported' $script:traced $written
        Assert-True '7.5  the jsonl file exists' (Test-Path $traceFile)
        $fileLines = Get-Lines (Read-OpenLog $traceFile)
        Assert-Equal '7.6  the file holds one line per exported trace' $written $fileLines.Count
        $last = Get-Json ($fileLines[$fileLines.Count - 1])
        Assert-True '7.7  the exported line is a complete trace' ($null -ne $last -and "$($last.trace_id)".Length -eq 32)
    }
    else {
        Assert-True '7.2  the reporter echoes its path' $false 'no jsonl stats'
    }

    # =======================================================================
    # 8. OTLP export
    # =======================================================================
    Write-Section '8. Traces are exported over OTLP/HTTP JSON'

    $colStats = Get-Json (Invoke-Http -Url "$colBase/requests").Body
    $tracePosts = 0
    $lastPost = $null
    foreach ($rec in @($colStats.requests)) {
        if ("$($rec.path)" -eq '/v1/traces') { $tracePosts++; $lastPost = $rec }
    }
    Assert-True '8.1  the collector received OTLP posts' ($tracePosts -gt 0) "posts=$tracePosts"
    if ($lastPost) {
        Assert-Contains '8.2  the export is JSON' "$($lastPost.content_type)" 'application/json'
        Assert-True '8.3  the export carries a real payload' ([int]"$($lastPost.bytes)" -gt 200) "bytes=$($lastPost.bytes)"
    }
    else {
        Assert-True '8.2  the export is JSON' $false 'no /v1/traces post recorded'
    }

    # The recorded bodies are only rendered by /dump: a recorded struct keeps its
    # body unexported, so /requests deliberately omits it.
    $dump = (Invoke-Http -Url "$colBase/dump").Body
    Assert-Contains '8.4  the exported resource names the configured service' $dump 'infergate-m5-curl'
    Assert-Contains '8.5  the payload declares resourceSpans' $dump 'resourceSpans'
    Assert-Contains '8.6  the payload declares scopeSpans' $dump 'scopeSpans'
    Assert-Contains '8.7  the payload names the gateway scope' $dump 'infergate.gateway'
    Assert-Contains '8.8  the payload carries the gateway span' $dump 'gateway.request'
    Assert-Contains '8.9  the payload carries the upstream span' $dump 'upstream.primary'
    $otlpStats = Get-Property $tracing2.export_stats 'otlp'
    Assert-True '8.10 the otlp reporter reports stats' ($null -ne $otlpStats)
    if ($otlpStats) {
        Assert-Equal '8.11 the otlp reporter echoes its endpoint' "http://127.0.0.1:$CollectorPort" "$($otlpStats.endpoint)"
        Assert-Equal '8.12 no otlp export failed' 0 ([int]"$($otlpStats.failed)")
        Assert-True '8.13 the otlp reporter exported traces' ([int]"$($otlpStats.exported)" -gt 0)
    }
    else {
        Assert-True '8.11 the otlp reporter echoes its endpoint' $false 'no otlp stats'
    }

    # =======================================================================
    # 9. The Prometheus contract
    # =======================================================================
    Write-Section '9. The metrics exposition contract'

    $metrics = (Invoke-Http -Url "$gwBase/metrics").Body
    Assert-Contains '9.1  build info is exposed' $metrics 'infergate_build_info'

    foreach ($family in @('infergate_requests_total', 'infergate_tokens_total', 'infergate_stream_frames_total',
            'infergate_stream_bytes_total', 'infergate_failovers_total', 'infergate_first_token_seconds_mean')) {
        Assert-Contains "9.2  # TYPE for $family" $metrics "# TYPE $family "
    }
    foreach ($family in @('infergate_request_duration_seconds', 'infergate_upstream_attempt_duration_seconds',
            'infergate_first_token_seconds', 'infergate_completion_tokens_per_request')) {
        Assert-Contains "9.3  # TYPE histogram for $family" $metrics "# TYPE $family histogram"
        Assert-Contains "9.4  # HELP for $family" $metrics "# HELP $family "
    }

    # The M0 acceptance regex must keep matching.
    $m0 = [regex]::IsMatch($metrics, '(?m)^infergate_request_duration_seconds_sum\{[^}]*\}\s+([0-9.eE+-]+)\s*$')
    Assert-True '9.5  the M0 request-duration regex still matches' $m0

    Assert-Contains '9.6  the stream bytes family is declared before its samples' $metrics "# TYPE infergate_stream_bytes_total counter"

    $reqTotal = Get-MetricSum -Text $metrics -Name 'infergate_requests_total'
    Assert-Equal '9.7  every proxied request was counted' $script:proxied ([int]$reqTotal)

    $infBucket = Get-MetricValue -Text $metrics -Name 'infergate_request_duration_seconds_bucket' -LabelFragment 'le="+Inf"'
    $countMetric = Get-MetricValue -Text $metrics -Name 'infergate_request_duration_seconds_count' -LabelFragment 'upstream='
    Assert-True '9.8  the +Inf bucket is present' ($infBucket -ge 0)
    Assert-Equal '9.9  +Inf equals _count' $countMetric $infBucket

    $attemptCount = Get-MetricValue -Text $metrics -Name 'infergate_upstream_attempt_duration_seconds_count' -LabelFragment 'upstream="primary"'
    Assert-True '9.10 upstream attempts are counted' ($attemptCount -ge 1) "count=$attemptCount"

    $framesMetric = Get-MetricValue -Text $metrics -Name 'infergate_stream_frames_total' -LabelFragment 'upstream="primary"'
    Assert-Equal '9.11 the relayed stream frames match the client body' $frameCount $framesMetric
    $bytesMetric = Get-MetricValue -Text $metrics -Name 'infergate_stream_bytes_total' -LabelFragment 'upstream="primary"'
    Assert-True '9.12 relayed stream bytes are positive' ($bytesMetric -gt 0) "bytes=$bytesMetric"
    $ttftCount = Get-MetricValue -Text $metrics -Name 'infergate_first_token_seconds_count' -LabelFragment 'upstream="primary"'
    Assert-Equal '9.13 exactly one streamed request recorded a first token' 1 $ttftCount
    $promptTokens = Get-MetricValue -Text $metrics -Name 'infergate_tokens_total' -LabelFragment 'kind="prompt"'
    Assert-True '9.14 prompt tokens were recorded' ($promptTokens -ge 7.0) "prompt=$promptTokens"

    Assert-Contains '9.15 the breaker state gauge is exposed' $metrics 'infergate_breaker_state{upstream="primary",state="closed"} 1'
    Assert-Contains '9.16 the build version is exposed' $metrics 'infergate_runtime_go_version{version='
    foreach ($family in @('infergate_runtime_goroutines', 'infergate_runtime_num_cpu', 'infergate_runtime_memstats_alloc_bytes',
            'infergate_runtime_memstats_gc_cycles_total', 'infergate_runtime_gc_pause_seconds_total')) {
        Assert-Contains "9.17 runtime family $family" $metrics $family
    }

    # =======================================================================
    # 10. /stats
    # =======================================================================
    Write-Section '10. /stats reports the latency window'

    $stats = Get-Json (Invoke-Http -Url "$gwBase/stats").Body
    Assert-Equal '10.1 /stats counts every proxied request' $script:proxied ([int]"$($stats.requests)")
    Assert-Equal '10.2 the latency window is the default ring size' 65536 ([int]"$($stats.latency.window)")
    Assert-Equal '10.3 an unfilled window dropped nothing' 0 ([int]"$($stats.latency.dropped)")
    $p50 = Get-Duration "$($stats.latency.p50)"
    $p90 = Get-Duration "$($stats.latency.p90)"
    $p95 = Get-Duration "$($stats.latency.p95)"
    $p99 = Get-Duration "$($stats.latency.p99)"
    $pmax = Get-Duration "$($stats.latency.max)"
    Assert-True '10.4 p50 parses as a duration' ($p50 -ge 0)
    Assert-True '10.5 p50 <= p90' ($p50 -le $p90) "p50=$p50 p90=$p90"
    Assert-True '10.6 p90 <= p95' ($p90 -le $p95) "p90=$p90 p95=$p95"
    Assert-True '10.7 p95 <= p99' ($p95 -le $p99) "p95=$p95 p99=$p99"
    Assert-True '10.8 p99 <= max' ($p99 -le $pmax) "p99=$p99 max=$pmax"
    $series = @($stats.series)
    Assert-True '10.9 /stats breaks the traffic down by route/upstream/status' ($series.Count -ge 1) "series=$($series.Count)"

    # =======================================================================
    # 11. Failover and a fully failed request
    # =======================================================================
    Write-Section '11. Failures are traced'

    Stop-Named -Tag 'primary' -Label 'primary mock'
    Assert-True "11.1 the primary mock port $PrimaryPort is closed" (Wait-PortClosed -Port $PrimaryPort)

    $r4 = Send-Proxied -Tag 'm5-curl-failover' -Headers @('X-InferGate-Request-Id: m5-curl-failover')
    Assert-Equal '11.2 the request still succeeded on the backup' 200 $r4.Status
    Assert-Equal '11.3 the backup answered' 'backup' (Get-Header $r4.Headers 'X-InferGate-Upstream-Name')
    $tried = Get-Header $r4.Headers 'X-InferGate-Tried'
    Assert-Contains '11.4 the failed primary is listed as tried' $tried 'primary'
    Assert-Contains '11.5 the backup is listed as tried' $tried 'backup'
    Assert-Equal '11.6 two attempts were made' '2' (Get-Header $r4.Headers 'X-InferGate-Attempt')

    $t4 = Get-TraceByRequestId 'm5-curl-failover'
    Assert-Equal '11.7 the failed-over request produced a trace' 200 $t4.Status
    Assert-Equal '11.8 the trace has one span per attempt plus the root' 3 (@($t4.Json.spans).Count)
    Assert-Equal '11.9 the root span counted two attempts' 2 "$($t4.Json.spans[0].attributes.attempts)"
    $failedSpan = Get-SpanNamed -Trace $t4.Json -Name 'upstream.primary'
    $okSpan = Get-SpanNamed -Trace $t4.Json -Name 'upstream.backup'
    Assert-True '11.10 the failed attempt has its own span' ($null -ne $failedSpan)
    Assert-Equal '11.11 the failed attempt span is an error' 'error' "$($failedSpan.status)"
    Assert-True '11.12 the failed attempt records why' ("$($failedSpan.attributes.reason)".Length -gt 0)
    Assert-Equal '11.13 the succeeding attempt span is ok' 'ok' "$($okSpan.status)"
    $failoverEvent = $null
    foreach ($ev in @($t4.Json.spans[0].events)) {
        if ("$($ev.name)" -eq 'failover') { $failoverEvent = $ev }
    }
    Assert-True '11.14 the root span carries a failover event' ($null -ne $failoverEvent)
    if ($failoverEvent) {
        Assert-Equal '11.15 the failover event counted one failover' 1 "$($failoverEvent.attributes.failovers)"
        Assert-Contains '11.16 the failover event names the failed upstream' "$($failoverEvent.attributes.tried)" 'primary'
    }
    else {
        Assert-True '11.15 the failover event counted one failover' $false 'no failover event'
    }

    Stop-Named -Tag 'backup' -Label 'backup mock'
    Assert-True "11.17 the backup mock port $BackupPort is closed" (Wait-PortClosed -Port $BackupPort)

    $r5 = Send-Proxied -Tag 'm5-curl-down' -Headers @('X-InferGate-Request-Id: m5-curl-down')
    Assert-Equal '11.18 with every provider down the client sees 502' 502 $r5.Status
    Assert-Contains '11.19 the 502 envelope explains the failure' $r5.Body 'no upstream could serve this request'
    $t5 = Get-TraceByRequestId 'm5-curl-down'
    Assert-Equal '11.20 the failed request is still traced' 200 $t5.Status
    Assert-Equal '11.21 the failed trace is marked error' 'error' "$($t5.Json.status)"
    Assert-Equal '11.22 the root span records the outcome' 'upstream_error' "$($t5.Json.spans[0].attributes.outcome)"
    Assert-Equal '11.23 both attempts are recorded' 2 "$($t5.Json.spans[0].attributes.attempts)"
    Assert-True '11.24 the root span explains itself' ("$($t5.Json.spans[0].attributes.reason)".Length -gt 0)
    $allFailed = $true
    foreach ($s in @($t5.Json.spans)) {
        if ("$($s.name)" -ne 'gateway.request' -and "$($s.status)" -ne 'error') { $allFailed = $false }
    }
    Assert-True '11.25 every attempt span is an error' $allFailed

    $metrics2 = (Invoke-Http -Url "$gwBase/metrics").Body
    $failovers = Get-MetricValue -Text $metrics2 -Name 'infergate_failovers_total' -LabelFragment 'outcome='
    Assert-True '11.26 failovers were counted' ($failovers -ge 1.0) "failovers=$failovers"
    $errSeries = $false
    $stats2 = Get-Json (Invoke-Http -Url "$gwBase/stats").Body
    foreach ($row in @($stats2.series)) {
        if ("$($row.outcome)" -eq 'upstream_error') { $errSeries = $true }
    }
    Assert-True '11.27 /stats exposes the upstream_error outcome' $errSeries

    # =======================================================================
    # 12. Log evidence
    # =======================================================================
    Write-Section '12. The request log carries the trace ids'

    $log = Read-OpenLog $gwLog
    Assert-True '12.1 the gateway wrote a log' ($log.Length -gt 0)
    Assert-Contains '12.2 the first request is logged with its id' $log 'request_id=m5-curl-0001'
    Assert-Contains '12.3 the log names the upstream that answered' $log 'upstream=primary'
    Assert-Contains '12.4 the failed request is logged at warn level' $log 'level=WARN'
    Assert-Contains '12.5 the failure log records the 502' $log 'status=502'
    Assert-Contains '12.6 the failure log records the outcome' $log 'outcome=upstream_error'
    Assert-Contains '12.7 the streamed request is logged with its frame count' $log 'frames='

    # =======================================================================
    # 13. Teardown
    # =======================================================================
    Write-Section '13. Teardown'
    $aliveTags = @()
    foreach ($tag in @($script:procs.Keys)) {
        $p = $script:procs[$tag]
        if ($p -and -not $p.HasExited) { $aliveTags += $tag }
    }
    Assert-True '13.1 the gateway and the collector survived the failure section' (($aliveTags -contains 'gateway') -and ($aliveTags -contains 'collector')) ("alive: " + ($aliveTags -join ', '))
    Assert-True '13.2 the two upstreams were stopped on purpose' (($aliveTags -notcontains 'primary') -and ($aliveTags -notcontains 'backup')) ("alive: " + ($aliveTags -join ', '))

    $exitCode = 0
}
catch {
    Write-Host ''
    Write-Host "  FATAL: $_" -ForegroundColor Red
    Write-Host "  at   : $($_.InvocationInfo.PositionMessage)" -ForegroundColor DarkRed
    $script:notes += "fatal: $_"
    $exitCode = 1
}
finally {
    Stop-Fleet
}

Write-Host ''
Write-Host ('=' * 72) -ForegroundColor DarkGray
$total = $script:passed + $script:failed
Write-Host "RESULT: $($script:passed)/$total assertions passed" -ForegroundColor White
if ($script:failed -gt 0) {
    Write-Host 'FAILED assertions:' -ForegroundColor Red
    foreach ($n in $script:notes) { Write-Host "  - $n" -ForegroundColor Red }
}
elseif ($exitCode -eq 0) {
    Write-Host 'OK: M5 tracing, observability and metrics verified end to end with curl' -ForegroundColor Green
}
Write-Host ('=' * 72) -ForegroundColor DarkGray

if ($script:failed -gt 0 -or $exitCode -ne 0) { exit 1 }
exit 0
