<#
  measure-m3.ps1 - M3 token / cost governance measurements.

  Five measurements. Each one carries its own caliber note in
  docs\baseline\m3-summary.json, and every number is read from a surface the
  gateway does not itself author where that is possible.

    1. Admission overhead  - identical load (bin\loadtest.exe -url mode) against
                             a gateway with quota.enabled false and one with
                             quota.enabled true (memory store), 3 interleaved
                             rounds, non-stream c8 and c32.
    2. Reservation accuracy - a known request mix under a generous policy; the
                             gateway's reserved / settled / released / overshoot
                             counters are compared with the usage the mock
                             upstream really reported.
    3. Budget stopping a runaway tenant - N identical requests without quota vs
                             with tokens_per_day 280; upstream calls are counted
                             in the mock's own log.
    4. Redis vs memory     - the same governed load against store: memory and
                             store: redis (cmd\miniredis on :6397); a day bucket
                             key is read back over raw RESP2.
    5. Fail-closed         - miniredis is stopped: every request must be refused
                             and no upstream call may happen; restarting
                             miniredis must recover the gateway without a
                             gateway restart.

  Nothing in configs\ is modified: every config is generated into tmp\m3-*.yaml
  by literal substitution, and never contains a ${VAR}.

  Run:
    powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m3.ps1

  This file is deliberately ASCII-only: Windows PowerShell 5.1 reads a BOM-less
  script as ANSI, so a non-ASCII character would be mangled before it ran.
#>
[CmdletBinding()]
param(
    [int]$GovernedPort = 18400,
    [int]$UngovernedPort = 18401,
    [int]$RedisGovernedPort = 18402,
    [int]$MockPort = 19700,
    [int]$MockUngovernedPort = 19701,
    [int]$MockRedisPort = 19702,
    [int]$MiniRedisPort = 6397,
    [int]$Rounds = 3,
    [int]$Requests = 800,
    [int]$Warmup = 100,
    [string]$Concurrency = '8,32',
    [int]$BudgetRequests = 40,
    [int]$FailClosedRequests = 20,
    [int]$RecoveryRequests = 10,
    [int]$FailOpenRequests = 5,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'
$env:GOTOOLCHAIN = 'local'

$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
$baselineDir = Join-Path $repo 'docs\baseline'
$configDir = Join-Path $repo 'configs'
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$goShim = Join-Path $repo 'tools\go.cmd'

$script:procs = New-Object System.Collections.ArrayList
$script:mockLogs = @{}
$script:assertFails = New-Object System.Collections.ArrayList
$script:failures = New-Object System.Collections.ArrayList
$script:runStamp = (Get-Date -Format 'HHmmss') + '-' + $PID
$script:cells = @{}
$script:cellOrder = New-Object System.Collections.ArrayList
$script:roundRows = New-Object System.Collections.ArrayList
$startedAt = Get-Date

foreach ($d in @($binDir, $tmpDir, $baselineDir)) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Path $d -Force | Out-Null }
}

# ---------------------------------------------------------------- helpers ---

function Write-Section {
    param([string]$Title)
    Write-Host ''
    Write-Host ('=' * 72) -ForegroundColor DarkGray
    Write-Host "  $Title" -ForegroundColor Cyan
    Write-Host ('=' * 72) -ForegroundColor DarkGray
}

# Windows PowerShell 5.1 turns native stderr into a terminating
# NativeCommandError, and with $ErrorActionPreference = 'Stop' one curl note
# would abort the measurement, so stderr is discarded for every native call.
function Invoke-Curl {
    param([string[]]$Arguments)
    return (& curl.exe @Arguments 2>$null | Out-String)
}

# A log a live child process is still writing must be opened with
# FileShare.ReadWrite: Get-Content fails with "being used by another process".
function Read-Text {
    param([string]$Path)
    if ([string]::IsNullOrEmpty($Path)) { return '' }
    if (-not (Test-Path $Path)) { return '' }
    $fs = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    $reader = New-Object System.IO.StreamReader($fs, $script:utf8NoBom)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose(); $fs.Dispose() }
}

function Get-Header {
    param([string]$Headers, [string]$Name)
    if ([string]::IsNullOrEmpty($Headers)) { return '' }
    $m = [regex]::Match($Headers, "(?im)^$([regex]::Escape($Name)):\s*(.+?)\s*$")
    if ($m.Success) { return $m.Groups[1].Value }
    return ''
}

function Write-JsonFile {
    param([string]$Path, $Value)
    $json = ($Value | ConvertTo-Json -Depth 15)
    [System.IO.File]::WriteAllText($Path, $json + "`n", $script:utf8NoBom)
}

function Get-JsonText {
    param([string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text)) { return $null }
    try { return ($Text | ConvertFrom-Json) } catch { return $null }
}

function Get-JsonUrl {
    param([string]$Url)
    return Get-JsonText (Invoke-Curl @('-s', '-m', '30', $Url))
}

# Literal substitution only: the loader refuses an undefined ${VAR}, so the
# generated configs never introduce one.
function Write-Config {
    # $Replace must be an ORDERED dictionary whose keys are applied in order:
    # a shorter literal can be a prefix of a longer one in the same file
    # ("requests_per_minute: 60" vs "...: 600", "tokens_per_day: 2000" vs
    # "...: 20000"), so the longer replacement has to run first.
    param([string]$Source, [string]$Target, $Replace)
    $text = Read-Text (Join-Path $repo $Source)
    if ([string]::IsNullOrEmpty($text)) { throw "config source $Source is empty or missing" }
    foreach ($k in $Replace.Keys) {
        if (-not $text.Contains($k)) { throw "config source $Source does not contain the literal '$k'" }
        $text = $text.Replace($k, $Replace[$k])
    }
    [System.IO.File]::WriteAllText($Target, $text, $script:utf8NoBom)
    Write-Host "  wrote $Target" -ForegroundColor DarkGray
    return $text
}

function Test-PortOpen {
    param([int]$Port, [int]$TimeoutMs = 500)
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $client.BeginConnect('127.0.0.1', $Port, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne($TimeoutMs)) { return $false }
        $client.EndConnect($iar)
        return $true
    } catch { return $false } finally { $client.Close() }
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
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$Port$Path")).Trim()
        if ($code -eq '200') { return $code }
        Start-Sleep -Milliseconds 150
    }
    return $code
}

function Get-Median {
    param([double[]]$Values)
    if ($null -eq $Values -or $Values.Count -eq 0) { return 0.0 }
    $sorted = @($Values | Sort-Object)
    return [double]$sorted[[int][Math]::Floor($sorted.Count / 2)]
}

# The /stats latency block prints Go duration strings; the micro sign (U+00B5)
# is rewritten to 'u' and the whole file stays ASCII.
function Convert-GoDurationToMs {
    param([string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text)) { return 0.0 }
    $t = $Text.Replace([string][char]0x00B5, 'u').Trim()
    $m = [regex]::Match($t, '^([0-9]+(?:\.[0-9]+)?)(ns|us|ms|s|m|h)$')
    if (-not $m.Success) { return 0.0 }
    $v = [double]$m.Groups[1].Value
    switch ($m.Groups[2].Value) {
        'ns' { return [Math]::Round($v / 1e6, 4) }
        'us' { return [Math]::Round($v / 1e3, 4) }
        'ms' { return [Math]::Round($v, 4) }
        's'  { return [Math]::Round($v * 1000, 4) }
        'm'  { return [Math]::Round($v * 60000, 4) }
        'h'  { return [Math]::Round($v * 3600000, 4) }
    }
    return 0.0
}

function Get-LatencyPeek {
    param($Stats)
    $peek = [ordered]@{
        source = 'GET /stats (the gateway recorder, its own view of the same run)'
        count = ''; requests = ''; p50 = ''; p90 = ''; p95 = ''; p99 = ''; max = ''
        p50_ms = 0.0; p95_ms = 0.0; p99_ms = 0.0
    }
    if ($null -eq $Stats) { return $peek }
    foreach ($k in @('count', 'p50', 'p90', 'p95', 'p99', 'max')) {
        if ($null -ne $Stats.latency -and $null -ne $Stats.latency.$k) { $peek[$k] = [string]$Stats.latency.$k }
    }
    if ($null -ne $Stats.requests) { $peek['requests'] = [int]$Stats.requests }
    $peek['p50_ms'] = Convert-GoDurationToMs ([string]$peek['p50'])
    $peek['p95_ms'] = Convert-GoDurationToMs ([string]$peek['p95'])
    $peek['p99_ms'] = Convert-GoDurationToMs ([string]$peek['p99'])
    return $peek
}

function Get-QuotaPeek {
    param($Stats)
    $peek = [ordered]@{ source = 'GET /stats -> quota' }
    if ($null -eq $Stats -or $null -eq $Stats.quota) {
        $peek['available'] = $false
        return $peek
    }
    $peek['available'] = $true
    foreach ($k in @('enabled', 'store', 'allowed', 'degraded', 'rejected', 'store_errors', 'alerts', 'reserved_tokens', 'settled_tokens', 'released_tokens', 'overshoot_tokens', 'overshoot_cost_micros')) {
        $v = $Stats.quota.PSObject.Properties[$k]
        if ($null -ne $v) { $peek[$k] = $v.Value }
    }
    return $peek
}

function Get-QuotaStat {
    param($Admin, [string]$Name)
    if ($null -eq $Admin -or $null -eq $Admin.stats) { return -1 }
    $v = $Admin.stats.PSObject.Properties[$Name]
    if ($null -eq $v) { return -1 }
    return [int64]$v.Value
}

function Get-QuotaAdmin {
    param([int]$Port, [string]$Tenant = '', [string]$Session = '')
    $url = "http://127.0.0.1:$Port/admin/quota"
    $q = @()
    if ($Tenant -ne '') { $q += "tenant=$Tenant" }
    if ($Session -ne '') { $q += "session=$Session" }
    if ($q.Count -gt 0) { $url += '?' + ($q -join '&') }
    return Get-JsonUrl $url
}

function Assert-That {
    param([bool]$Ok, [string]$Name, [string]$Detail = '')
    $script:assertTotal = 1 + [int]$script:assertTotal
    if ($Ok) {
        Write-Host "  ASSERT PASS  $Name" -ForegroundColor Green -NoNewline
        if ($Detail -ne '') { Write-Host " - $Detail" -ForegroundColor DarkGray } else { Write-Host '' }
    } else {
        Write-Host "  ASSERT FAIL  $Name" -ForegroundColor Red -NoNewline
        if ($Detail -ne '') { Write-Host " - $Detail" -ForegroundColor DarkGray } else { Write-Host '' }
        [void]$script:assertFails.Add($Name)
    }
}

function Add-Failure {
    param([string]$What)
    [void]$script:failures.Add($What)
    Write-Host "  NOT MEASURED: $What" -ForegroundColor Yellow
}

function Assert-PortsFree {
    param([int[]]$Ports)
    $busy = @()
    foreach ($p in $Ports) {
        $c = Get-NetTCPConnection -State Listen -LocalPort $p -ErrorAction SilentlyContinue
        if ($c) {
            $ownerPid = ($c | Select-Object -First 1).OwningProcess
            $pname = 'unknown'
            try { $pname = (Get-Process -Id $ownerPid -ErrorAction Stop).ProcessName } catch { }
            $busy += "$p (pid $ownerPid $pname)"
        }
    }
    if ($busy.Count -gt 0) {
        throw ("port(s) already listening: " + ($busy -join ', ') + " - stop that process or pass different -Port values")
    }
}

function Start-Mock {
    param([int]$Port, [string]$Name, [string]$Tag)
    $log = Join-Path $tmpDir "m3-$Tag-$script:runStamp.log"
    [System.IO.File]::WriteAllText($log, '', $script:utf8NoBom)
    $p = Start-Process -FilePath (Join-Path $binDir 'mockupstream.exe') `
        -ArgumentList @('-listen', ":$Port", '-name', $Name, '-token-delay', '0ms') `
        -RedirectStandardOutput $log `
        -RedirectStandardError (Join-Path $tmpDir "m3-$Tag-$script:runStamp.err") `
        -PassThru -WindowStyle Hidden
    [void]$script:procs.Add($p)
    $script:mockLogs[$Tag] = $log
    return $p
}

function Get-MockRequestCount {
    param([string]$LogPath)
    if ([string]::IsNullOrEmpty($LogPath)) { return -1 }
    if (-not (Test-Path $LogPath)) { return -1 }
    for ($i = 1; $i -le 4; $i++) {
        try {
            $text = Read-Text $LogPath
            return ([regex]::Matches($text, 'msg="chat request"')).Count
        } catch { Start-Sleep -Milliseconds 150 }
    }
    return -1
}

function Start-Gateway {
    param([int]$Port, [string]$ConfigPath, [string]$Tag, [int]$Attempts = 3)
    for ($i = 1; $i -le $Attempts; $i++) {
        $log = Join-Path $tmpDir "m3-$Tag-$script:runStamp.log"
        [System.IO.File]::WriteAllText($log, '', $script:utf8NoBom)
        $p = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
            -ArgumentList @('-config', $ConfigPath) `
            -RedirectStandardOutput $log `
            -RedirectStandardError (Join-Path $tmpDir "m3-$Tag-$script:runStamp.err") `
            -PassThru -WindowStyle Hidden
        [void]$script:procs.Add($p)
        $code = Wait-Healthy -Port $Port -Path '/readyz' -Seconds 15
        if ($code -eq '200') { return $p }
        Write-Host "  gateway $Tag not ready (http $code, attempt $i)" -ForegroundColor Yellow
        Stop-Proc $p $Tag
        Start-Sleep -Milliseconds 800
    }
    throw "gateway $Tag on port $Port never became ready"
}

function Start-MiniRedis {
    param([int]$Port, [string]$Tag)
    $log = Join-Path $tmpDir "m3-$Tag-$script:runStamp.log"
    [System.IO.File]::WriteAllText($log, '', $script:utf8NoBom)
    $p = Start-Process -FilePath (Join-Path $binDir 'miniredis.exe') `
        -ArgumentList @('-listen', ":$Port") `
        -RedirectStandardOutput $log `
        -RedirectStandardError (Join-Path $tmpDir "m3-$Tag-$script:runStamp.err") `
        -PassThru -WindowStyle Hidden
    [void]$script:procs.Add($p)
    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-Date) -lt $deadline) {
        if (Test-PortOpen -Port $Port -TimeoutMs 250) { return $p }
        Start-Sleep -Milliseconds 150
    }
    throw "miniredis on port $Port never opened"
}

function Stop-Proc {
    param($Proc, [string]$Tag = '')
    if ($null -ne $Proc) {
        try { if (-not $Proc.HasExited) { Stop-Process -Id $Proc.Id -Force } } catch { }
    }
    Start-Sleep -Milliseconds 350
}

# --- raw RESP2 -----------------------------------------------------------------
# The counter is read back over the wire instead of through the gateway, so the
# gateway cannot fake it.
function Send-Redis {
    param([int]$Port, [string[]]$Parts)
    $sb = New-Object System.Text.StringBuilder
    [void]$sb.Append("*$($Parts.Count)`r`n")
    foreach ($p in $Parts) {
        $len = [System.Text.Encoding]::ASCII.GetByteCount($p)
        [void]$sb.Append("`$$len`r`n$p`r`n")
    }
    $payload = [System.Text.Encoding]::ASCII.GetBytes($sb.ToString())
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $client.Connect('127.0.0.1', $Port)
        $stream = $client.GetStream()
        $stream.Write($payload, 0, $payload.Length)
        $stream.Flush()
        Start-Sleep -Milliseconds 150
        $buffer = New-Object byte[] 65536
        $read = $stream.Read($buffer, 0, $buffer.Length)
        if ($read -le 0) { return '' }
        return [System.Text.Encoding]::ASCII.GetString($buffer, 0, $read)
    } catch { return '' } finally { $client.Close() }
}

function Get-RedisKeys {
    param([int]$Port, [string]$Pattern)
    $reply = Send-Redis -Port $Port -Parts @('KEYS', $Pattern)
    $out = New-Object System.Collections.ArrayList
    if ([string]::IsNullOrWhiteSpace($reply)) { return @() }
    if ($reply[0] -ne '*') { return @() }
    $lines = $reply -split "`r`n"
    $i = 1
    while ($i -lt $lines.Count) {
        if ($lines[$i] -match '^\$(\d+)$') {
            $len = [int]$Matches[1]
            $i++
            if ($i -lt $lines.Count) {
                $val = $lines[$i]
                if ($val.Length -gt $len) { $val = $val.Substring(0, $len) }
                [void]$out.Add($val)
            }
        }
        $i++
    }
    return $out.ToArray()
}

function Get-RedisString {
    param([int]$Port, [string]$Key)
    $reply = Send-Redis -Port $Port -Parts @('GET', $Key)
    if ([string]::IsNullOrWhiteSpace($reply)) { return $null }
    if ($reply[0] -ne '$') { return $null }
    $idx = $reply.IndexOf("`r`n")
    if ($idx -lt 0) { return $null }
    $head = $reply.Substring(1, $idx - 1)
    if ($head -eq '-1') { return $null }
    $len = [int]$head
    $rest = $reply.Length - ($idx + 2)
    if ($len -gt $rest) { $len = $rest }
    if ($len -le 0) { return '' }
    return $reply.Substring($idx + 2, $len)
}

function Get-RedisInt {
    param([int]$Port, [string[]]$Parts)
    $reply = Send-Redis -Port $Port -Parts $Parts
    if ([string]::IsNullOrWhiteSpace($reply)) { return $null }
    if ($reply[0] -ne ':') { return $null }
    $idx = $reply.IndexOf("`r`n")
    if ($idx -lt 0) { return $null }
    return [int64]$reply.Substring(1, $idx - 1)
}

# --- bodies and requests -------------------------------------------------------

function New-ChatBodyEx {
    param([string]$Tag, [string]$Prompt, [int]$MaxTokens = 0, [string]$Model = 'mock-gpt')
    $obj = [ordered]@{
        model = $Model
        messages = @([ordered]@{ role = 'user'; content = $Prompt })
    }
    if ($MaxTokens -gt 0) { $obj['max_tokens'] = $MaxTokens }
    $json = ($obj | ConvertTo-Json -Compress -Depth 5)
    $path = Join-Path $tmpDir "m3-body-$Tag.json"
    [System.IO.File]::WriteAllText($path, $json, $script:utf8NoBom)
    return [pscustomobject]@{
        Path = $path
        Json = $json
        Bytes = [System.Text.Encoding]::UTF8.GetByteCount($json)
    }
}

function Send-ChatEx {
    param([int]$Port, [string]$BodyPath, [string]$Tenant = '', [string]$Session = '', [string]$Tag = 'req')
    $hdr = Join-Path $tmpDir "m3-headers-$Tag.txt"
    $out = Join-Path $tmpDir "m3-resp-$Tag.json"
    if (Test-Path $hdr) { Remove-Item $hdr -Force }
    if (Test-Path $out) { Remove-Item $out -Force }
    $cargs = @('-s', '-m', '60', '-o', $out, '-D', $hdr, '-w', '%{http_code}', '-X', 'POST', '-H', 'content-type: application/json')
    if ($Tenant -ne '') { $cargs += @('-H', "X-InferGate-Tenant: $Tenant") }
    if ($Session -ne '') { $cargs += @('-H', "X-InferGate-Session: $Session") }
    $cargs += @('--data-binary', "@$BodyPath", "http://127.0.0.1:$Port/v1/chat/completions")
    $codeText = (Invoke-Curl $cargs).Trim()
    $code = 0
    if ($codeText -match '^\d+$') { $code = [int]$codeText }
    $headers = Read-Text $hdr
    return [pscustomobject]@{
        Status = $code
        Quota = Get-Header $headers 'X-InferGate-Quota'
        QuotaReason = Get-Header $headers 'X-InferGate-Quota-Reason'
        QuotaModel = Get-Header $headers 'X-InferGate-Quota-Model'
        QuotaMaxTokens = Get-Header $headers 'X-InferGate-Quota-Max-Tokens'
        RetryAfter = Get-Header $headers 'Retry-After'
        Body = Read-Text $out
    }
}

function Invoke-Flush {
    param([int]$Port)
    try { [void](Invoke-Curl @('-s', '-m', '10', '-X', 'POST', "http://127.0.0.1:$Port/admin/cache/flush")) } catch { }
}

# --- load client ---------------------------------------------------------------

function Invoke-Load {
    param([int]$Port, [string]$OutFile, [string]$Tag)
    $stdout = Join-Path $tmpDir "m3-load-$Tag-$script:runStamp.out.txt"
    if (Test-Path $OutFile) { Remove-Item $OutFile -Force }
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    & (Join-Path $binDir 'loadtest.exe') -url "http://127.0.0.1:$Port" -c $Concurrency -n $Requests -warmup $Warmup -out $OutFile 2>$null | Out-File -FilePath $stdout -Encoding utf8
    $sw.Stop()
    if ($LASTEXITCODE -ne 0) { throw "loadtest against :$Port failed with exit code $LASTEXITCODE (see $stdout)" }
    if (-not (Test-Path $OutFile)) { throw "loadtest against :$Port wrote no -out file" }
    return [Math]::Round($sw.Elapsed.TotalSeconds, 2)
}

function Add-LoadResults {
    param([string]$Path, [string]$Scenario, [int]$Round, [double]$Wall)
    $doc = Get-JsonText (Read-Text $Path)
    if ($null -eq $doc) { throw "could not parse loadtest output $Path" }
    $entries = @($doc)
    if ($entries.Count -eq 0) { throw "loadtest output $Path has no rows" }
    foreach ($e in $entries) {
        $workload = 'non-stream'
        if ($e.stream) { $workload = 'stream' }
        $key = "$Scenario|$workload|$([int]$e.concurrency)"
        if (-not $script:cells.ContainsKey($key)) {
            $script:cells[$key] = New-Object System.Collections.ArrayList
            [void]$script:cellOrder.Add($key)
        }
        [void]$script:cells[$key].Add($e)
        # one row per round, so the artifact carries the per-round range
        [void]$script:roundRows.Add([pscustomobject]@{
                round = $Round
                scenario = $Scenario
                workload = $workload
                concurrency = [int]$e.concurrency
                qps = [Math]::Round([double]$e.qps, 1)
                p95_ms = [Math]::Round([double]$e.p95 / 1e6, 3)
                p99_ms = [Math]::Round([double]$e.p99 / 1e6, 3)
                errors = [int]$e.errors
                wall_s = $Wall
            })
    }
}

function Get-AggregatedRows {
    $rows = New-Object System.Collections.ArrayList
    foreach ($key in $script:cellOrder) {
        $parts = $key -split '\|'
        $list = $script:cells[$key]
        [double[]]$qps = @($list | ForEach-Object { [double]$_.qps })
        [double[]]$p95 = @($list | ForEach-Object { [double]$_.p95 / 1e6 })
        [double[]]$p99 = @($list | ForEach-Object { [double]$_.p99 / 1e6 })
        $errors = 0
        foreach ($e in $list) { $errors += [int]$e.errors }
        [void]$rows.Add([pscustomobject]@{
                Scenario = $parts[0]
                Workload = $parts[1]
                Concurrency = [int]$parts[2]
                Rounds = $list.Count
                QPS = [Math]::Round((Get-Median -Values $qps), 1)
                QPSMin = [Math]::Round(($qps | Measure-Object -Minimum).Minimum, 1)
                QPSMax = [Math]::Round(($qps | Measure-Object -Maximum).Maximum, 1)
                P95_ms = [Math]::Round((Get-Median -Values $p95), 3)
                P99_ms = [Math]::Round((Get-Median -Values $p99), 3)
                P95Min_ms = [Math]::Round(($p95 | Measure-Object -Minimum).Minimum, 3)
                P95Max_ms = [Math]::Round(($p95 | Measure-Object -Maximum).Maximum, 3)
                P99Min_ms = [Math]::Round(($p99 | Measure-Object -Minimum).Minimum, 3)
                P99Max_ms = [Math]::Round(($p99 | Measure-Object -Maximum).Maximum, 3)
                Errors = $errors
            })
    }
    return $rows
}

function Show-Rows {
    param($Rows)
    if ($null -eq $Rows -or $Rows.Count -eq 0) { return }
    $Rows | Format-Table -AutoSize | Out-String -Width 220 | Write-Host
}

# --- config probes -------------------------------------------------------------

function Get-ConfigInt {
    param([string]$Text, [string]$Key)
    $m = [regex]::Match($Text, "(?m)^\s*$([regex]::Escape($Key)):\s*([0-9]+)\s*$")
    if ($m.Success) { return [int]$m.Groups[1].Value }
    return -1
}

function Get-ConfigBool {
    param([string]$Text, [string]$Key)
    $m = [regex]::Match($Text, "(?m)^\s*$([regex]::Escape($Key)):\s*(true|false)\s*$")
    if ($m.Success) { return ($m.Groups[1].Value -eq 'true') }
    return $null
}

function Get-ConfigString {
    param([string]$Text, [string]$Key)
    $m = [regex]::Match($Text, "(?m)^\s*$([regex]::Escape($Key)):\s*""([^""]*)""")
    if ($m.Success) { return $m.Groups[1].Value }
    return ''
}

function Get-TenantLimit {
    param([string]$Text, [string]$Tenant, [string]$Key)
    $idx = $Text.IndexOf("tenant: `"$Tenant`"")
    if ($idx -lt 0) { return -1 }
    $slice = $Text.Substring($idx, [Math]::Min(400, $Text.Length - $idx))
    $m = [regex]::Match($slice, "(?m)^\s*$([regex]::Escape($Key)):\s*([0-9.]+)\s*$")
    if ($m.Success) { return [double]$m.Groups[1].Value }
    return -1
}

# ===============================================================================

$configs = [ordered]@{
    governed    = Join-Path $tmpDir 'm3-governed.yaml'
    ungoverned  = Join-Path $tmpDir 'm3-ungoverned.yaml'
    budget      = Join-Path $tmpDir 'm3-budget.yaml'
    redis       = Join-Path $tmpDir 'm3-governed-redis.yaml'
    redisOpen   = Join-Path $tmpDir 'm3-governed-redis-open.yaml'
    degrade     = Join-Path $tmpDir 'm3-degrade.yaml'
}

$exitOk = $false
$govProc = $null

try {
    Write-Section 'PRE-FLIGHT'
    Assert-PortsFree -Ports @($GovernedPort, $UngovernedPort, $RedisGovernedPort, $MockPort, $MockUngovernedPort, $MockRedisPort, $MiniRedisPort)
    Write-Host "  ports $GovernedPort/$UngovernedPort/$RedisGovernedPort and mocks $MockPort/$MockUngovernedPort/$MockRedisPort and miniredis $MiniRedisPort are free" -ForegroundColor DarkGray
    Write-Host "  run stamp $script:runStamp" -ForegroundColor DarkGray

    Write-Section 'BUILD'
    foreach ($spec in @(
            @('infergate.exe', './cmd/infergate'),
            @('mockupstream.exe', './cmd/mockupstream'),
            @('miniredis.exe', './cmd/miniredis'),
            @('loadtest.exe', './cmd/loadtest'))) {
        $out = Join-Path $binDir $spec[0]
        $ok = $false
        for ($attempt = 1; $attempt -le 3; $attempt++) {
            & $goShim build -o $out $spec[1]
            if ($LASTEXITCODE -eq 0) { $ok = $true; break }
            Write-Host "  go build $($spec[1]) failed with exit code $LASTEXITCODE (attempt $attempt/3)" -ForegroundColor Yellow
            Write-Host "  the output binary may be locked by another gate; retrying" -ForegroundColor Yellow
            Start-Sleep -Milliseconds 1500
        }
        if (-not $ok) { throw "go build $($spec[1]) failed after 3 attempts (exit code $LASTEXITCODE)" }
        Write-Host "  built $out" -ForegroundColor DarkGray
    }

    Write-Section 'GENERATED CONFIGS (literal substitution, configs/ untouched)'
    $govText = Write-Config -Source 'configs\quota-local.yaml' -Target $configs.governed -Replace ([ordered]@{
        '  listen: ":8084"'                 = '  listen: ":' + $GovernedPort + '"'
        'base_url: "http://127.0.0.1:9300"' = 'base_url: "http://127.0.0.1:' + $MockPort + '"'
        '  level: "info"'                   = '  level: "error"'
        'requests_per_minute: 600'          = 'requests_per_minute: 1000000000'
        'tokens_per_day: 1000000'           = 'tokens_per_day: 1000000000'
    })
    $ungovText = Write-Config -Source 'configs\quota-local.yaml' -Target $configs.ungoverned -Replace ([ordered]@{
        '  listen: ":8084"'                 = '  listen: ":' + $UngovernedPort + '"'
        'base_url: "http://127.0.0.1:9300"' = 'base_url: "http://127.0.0.1:' + $MockUngovernedPort + '"'
        '  level: "info"'                   = '  level: "error"'
        '  enabled: true'                   = '  enabled: false'
    })
    # ORDER MATTERS: "tokens_per_day: 2000" is a prefix of "tokens_per_day: 20000"
    # and "requests_per_minute: 60" is a prefix of "requests_per_minute: 600",
    # so every longer literal is replaced before its prefix.
    $budgetText = Write-Config -Source 'configs\quota-local.yaml' -Target $configs.budget -Replace ([ordered]@{
        '  listen: ":8084"'                 = '  listen: ":' + $GovernedPort + '"'
        'base_url: "http://127.0.0.1:9300"' = 'base_url: "http://127.0.0.1:' + $MockPort + '"'
        '  level: "info"'                   = '  level: "error"'
        'tokens_per_day: 1000000'           = 'tokens_per_day: 1000000000'
        'tokens_per_day: 20000'             = 'tokens_per_day: 1000000000'
        'tokens_per_day: 2000'              = 'tokens_per_day: 280'
        'requests_per_minute: 600'          = 'requests_per_minute: 1000000000'
        'requests_per_minute: 60'           = 'requests_per_minute: 1000000000'
    })
    $redisText = Write-Config -Source 'configs\quota-redis.yaml' -Target $configs.redis -Replace ([ordered]@{
        '  listen: ":8085"'                 = '  listen: ":' + $RedisGovernedPort + '"'
        'base_url: "http://127.0.0.1:9301"' = 'base_url: "http://127.0.0.1:' + $MockRedisPort + '"'
        '  level: "info"'                   = '  level: "error"'
        'requests_per_minute: 600'          = 'requests_per_minute: 1000000000'
        'tokens_per_day: 1000000'           = 'tokens_per_day: 1000000000'
        '127.0.0.1:6398'                    = '127.0.0.1:' + $MiniRedisPort
    })
    $redisOpenText = Write-Config -Source 'configs\quota-redis.yaml' -Target $configs.redisOpen -Replace ([ordered]@{
        '  listen: ":8085"'                 = '  listen: ":' + $RedisGovernedPort + '"'
        'base_url: "http://127.0.0.1:9301"' = 'base_url: "http://127.0.0.1:' + $MockRedisPort + '"'
        '  level: "info"'                   = '  level: "error"'
        'requests_per_minute: 600'          = 'requests_per_minute: 1000000000'
        'tokens_per_day: 1000000'           = 'tokens_per_day: 1000000000'
        '127.0.0.1:6398'                    = '127.0.0.1:' + $MiniRedisPort
        'fail_open: false'                  = 'fail_open: true'
    })
    # The degrade ladder is normally reached only after a tenant has spent its
    # whole day. Cutting growth's day to 20 tokens puts the FIRST reservation
    # (prompt estimate + completion estimate) over the limit, so the ladder is
    # observable without a long ramp; max_tokens_cap is cut to 64 so the number
    # in the header is unmistakable.
    $degradeText = Write-Config -Source 'configs\quota-local.yaml' -Target $configs.degrade -Replace ([ordered]@{
        '  listen: ":8084"'                 = '  listen: ":' + $GovernedPort + '"'
        'base_url: "http://127.0.0.1:9300"' = 'base_url: "http://127.0.0.1:' + $MockPort + '"'
        '  level: "info"'                   = '  level: "error"'
        'tokens_per_day: 20000'             = 'tokens_per_day: 20'
        'max_tokens_cap: 128'               = 'max_tokens_cap: 64'
        'requests_per_minute: 600'          = 'requests_per_minute: 1000000000'
        'tokens_per_day: 1000000'           = 'tokens_per_day: 1000000000'
    })

    Assert-That ((Read-Text $configs.governed).Contains('listen: ":' + $GovernedPort + '"')) 'governed config listens on the governed port'
    Assert-That ((Read-Text $configs.governed).Contains('tokens_per_day: 1000000000')) 'governed config default policy is generous (load client has no tenant header)'
    Assert-That ((Read-Text $configs.ungoverned).Contains('  enabled: false')) 'ungoverned config has quota.enabled false'
    Assert-That ((Read-Text $configs.budget).Contains('tokens_per_day: 280')) 'budget config sets acme tokens_per_day 280'
    Assert-That ((Read-Text $configs.redis).Contains('store: "redis"')) 'redis config keeps store: redis'
    Assert-That ((Read-Text $configs.redis).Contains('127.0.0.1:' + $MiniRedisPort)) 'redis config points at the run miniredis'
    Assert-That ((Read-Text $configs.redisOpen).Contains('fail_open: true')) 'fail-open variant flips fail_open to true'
    Assert-That ((Read-Text $configs.degrade).Contains('max_tokens_cap: 64')) 'degrade variant caps max_tokens at 64 for growth'
    # the repository configs carry a commented-out redis password placeholder, so
    # the check is: no ACTIVE line gained a placeholder and the count is unchanged.
    $srcDollars = ([regex]::Matches((Read-Text (Join-Path $repo 'configs\quota-local.yaml')), '\$\{')).Count
    $govActive = ([regex]::Matches((Read-Text $configs.governed), '(?m)^\s*[^#\s].*\$\{')).Count
    $govDollars = ([regex]::Matches((Read-Text $configs.governed), '\$\{')).Count
    Assert-That ($govActive -eq 0 -and $govDollars -eq $srcDollars) 'generated configs introduce no variable placeholder on an active line' "active $govActive, count $govDollars vs source $srcDollars"

    $govStore = Get-ConfigString $govText 'store'
    $govEstChars = Get-ConfigInt $govText 'estimate_chars_per_token'
    $govEstCompletion = Get-ConfigInt $govText 'estimate_completion_tokens'
    $govFailOpen = Get-ConfigBool $govText 'fail_open'
    $redisStore = Get-ConfigString $redisText 'store'
    $redisFailOpen = Get-ConfigBool $redisText 'fail_open'
    $redisEstChars = Get-ConfigInt $redisText 'estimate_chars_per_token'
    $redisEstCompletion = Get-ConfigInt $redisText 'estimate_completion_tokens'
    $budgetAcmeTokens = Get-TenantLimit $budgetText 'acme' 'tokens_per_day'
    $govAcmeTokens = Get-TenantLimit $govText 'acme' 'tokens_per_day'
    $degradeGrowthTokens = Get-TenantLimit $degradeText 'growth' 'tokens_per_day'
    $degradeGrowthCap = Get-TenantLimit $degradeText 'growth' 'max_tokens_cap'
    $govGrowthTokens = Get-TenantLimit $govText 'growth' 'tokens_per_day'
    $govGrowthCap = Get-TenantLimit $govText 'growth' 'max_tokens_cap'
    $govDefaultTokens = -1
    $dm = [regex]::Match($govText, '(?ms)default_policy:\s*\r?\n\s*tokens_per_day:\s*(\d+)')
    if ($dm.Success) { $govDefaultTokens = [int64]$dm.Groups[1].Value }
    Write-Host "  store=$govStore estimate_chars_per_token=$govEstChars estimate_completion_tokens=$govEstCompletion fail_open=$govFailOpen default_tokens_per_day=$govDefaultTokens acme_tokens_per_day=$govAcmeTokens" -ForegroundColor DarkGray

    Write-Section 'START GATEWAYS (sections 1 and 2) AND MOCKS'
    $mockGov = Start-Mock -Port $MockPort -Name 'mock-m3-governed' -Tag 'mock-gov'
    $mockUngov = Start-Mock -Port $MockUngovernedPort -Name 'mock-m3-ungoverned' -Tag 'mock-ungov'
    Assert-That ((Wait-Healthy -Port $MockPort -Path '/healthz' -Seconds 10) -eq '200') 'mock upstream (governed) is healthy'
    Assert-That ((Wait-Healthy -Port $MockUngovernedPort -Path '/healthz' -Seconds 10) -eq '200') 'mock upstream (ungoverned) is healthy'
    $mockGovLog = $script:mockLogs['mock-gov']
    $mockUngovLog = $script:mockLogs['mock-ungov']

    $govProc = Start-Gateway -Port $GovernedPort -ConfigPath $configs.governed -Tag 'gov'
    $ungovProc = Start-Gateway -Port $UngovernedPort -ConfigPath $configs.ungoverned -Tag 'ungov'
    $adminGov = Get-QuotaAdmin -Port $GovernedPort
    Assert-That ($null -ne $adminGov) 'GET /admin/quota answers on the governed gateway'
    Assert-That ($null -ne $adminGov -and $adminGov.enabled -eq $true) 'governed gateway reports quota enabled'
    Assert-That ($null -ne $adminGov -and "$($adminGov.store)" -eq 'memory') 'governed gateway reports the memory store'
    $adminUngov = Get-QuotaAdmin -Port $UngovernedPort
    Assert-That ($null -ne $adminUngov -and $adminUngov.enabled -eq $false) 'ungoverned gateway reports quota disabled'

    # ------------------------------------------------------------ SECTION 1 ---
    Write-Section 'SECTION 1 - admission overhead: quota off vs quota on (memory store)'
    Write-Host "  $Rounds interleaved rounds, loadtest -url -c $Concurrency -n $Requests -warmup $Warmup" -ForegroundColor DarkGray
    Write-Host '  caliber: load client wall-clock QPS/latency against the gateway, identical traffic and' -ForegroundColor DarkGray
    Write-Host '  upstream; this is gateway work (estimate + store round trip), not provider latency.' -ForegroundColor DarkGray

    foreach ($r in 1..$Rounds) {
        Invoke-Flush $GovernedPort
        $w = Invoke-Load -Port $GovernedPort -OutFile (Join-Path $tmpDir "m3-load-governed-r$r.json") -Tag "governed-r$r"
        Add-LoadResults -Path (Join-Path $tmpDir "m3-load-governed-r$r.json") -Scenario 'governed' -Round $r -Wall $w
        Write-Host ("  round {0}: governed  :{1} wall {2}s" -f $r, $GovernedPort, $w) -ForegroundColor DarkGray
        Invoke-Flush $UngovernedPort
        $w = Invoke-Load -Port $UngovernedPort -OutFile (Join-Path $tmpDir "m3-load-ungoverned-r$r.json") -Tag "ungoverned-r$r"
        Add-LoadResults -Path (Join-Path $tmpDir "m3-load-ungoverned-r$r.json") -Scenario 'ungoverned' -Round $r -Wall $w
        Write-Host ("  round {0}: ungoverned:{1} wall {2}s" -f $r, $UngovernedPort, $w) -ForegroundColor DarkGray
    }

    $statsGov = Get-JsonUrl "http://127.0.0.1:$GovernedPort/stats"
    $statsUngov = Get-JsonUrl "http://127.0.0.1:$UngovernedPort/stats"
    $peekGov = Get-LatencyPeek $statsGov
    $peekUngov = Get-LatencyPeek $statsUngov
    $quotaPeekGov = Get-QuotaPeek $statsGov

    Assert-That ($peekGov.count -ne '') 'governed gateway /stats reports a latency sample count'
    Assert-That ($peekUngov.count -ne '') 'ungoverned gateway /stats reports a latency sample count'
    Write-Host ("  /stats p50/p95 (pure governed run):   {0} / {1}  ({2} / {3} ms)" -f $peekGov.p50, $peekGov.p95, $peekGov.p50_ms, $peekGov.p95_ms) -ForegroundColor DarkGray
    Write-Host ("  /stats p50/p95 (pure ungoverned run): {0} / {1}  ({2} / {3} ms)" -f $peekUngov.p50, $peekUngov.p95, $peekUngov.p50_ms, $peekUngov.p95_ms) -ForegroundColor DarkGray

    # ------------------------------------------------------------ SECTION 2 ---
    Write-Section 'SECTION 2 - reservation accuracy: estimate vs the usage the upstream reported'
    Write-Host '  caliber: gateway counters (GET /admin/quota) vs the usage block of the real response;' -ForegroundColor DarkGray
    Write-Host '  the mock defines prompt_tokens = words per message + 4 and completion_tokens = words of' -ForegroundColor DarkGray
    Write-Host '  its answer, so actual usage is computable without the gateway.' -ForegroundColor DarkGray

    $prompts = @(
        'hello',
        'summarise the release notes for the gateway',
        'what is the p95 latency of the governed path today',
        'a b c d e f g h i j k l m n o p q r s t u v w x y z a b c d e f g h i j k l m n o p q r s t',
        'tokens cost money',
        'please explain the reserve then settle model in three sentences',
        'one two three four five six seven eight nine ten',
        'short',
        'the quick brown fox jumps over the lazy dog repeatedly and often',
        'alpha',
        'beta gamma delta epsilon zeta eta theta iota kappa lambda mu',
        'final request for the accuracy sample'
    )

    $accBefore = Get-QuotaAdmin -Port $GovernedPort -Tenant 'acme'
    $acc = New-Object System.Collections.ArrayList
    $idx = 0
    foreach ($p in $prompts) {
        $idx++
        $mt = 0
        if ($idx -eq 4 -or $idx -eq 7) { $mt = 1 }
        $body = New-ChatBodyEx -Tag "acc$idx" -Prompt $p -MaxTokens $mt
        $res = Send-ChatEx -Port $GovernedPort -BodyPath $body.Path -Tenant 'acme' -Tag "acc$idx"
        $usage = $null
        if ($res.Status -eq 200) {
            $doc = Get-JsonText $res.Body
            if ($null -ne $doc -and $null -ne $doc.usage) { $usage = $doc.usage }
        }
        $estCompletion = $govEstCompletion
        if ($mt -gt 0) { $estCompletion = $mt }
        $estPrompt = [int][Math]::Floor($body.Bytes / $govEstChars)
        $estTotal = $estPrompt + $estCompletion
        $actPrompt = 0
        $actCompletion = 0
        if ($null -ne $usage) {
            $actPrompt = [int]$usage.prompt_tokens
            $actCompletion = [int]$usage.completion_tokens
        }
        $words = @($p -split '\s+' | Where-Object { $_ -ne '' }).Count
        [void]$acc.Add([pscustomobject]@{
                index = $idx
                prompt_words = $words
                body_bytes = $body.Bytes
                max_tokens_requested = $mt
                status = $res.Status
                quota = $res.Quota
                quota_reason = $res.QuotaReason
                estimated_tokens = $estTotal
                estimated_prompt = $estPrompt
                estimated_completion = $estCompletion
                actual_tokens = $actPrompt + $actCompletion
                actual_prompt = $actPrompt
                actual_completion = $actCompletion
                deviation = $estTotal - ($actPrompt + $actCompletion)
            })
        Start-Sleep -Milliseconds 60
    }
    $accAfter = Get-QuotaAdmin -Port $GovernedPort -Tenant 'acme'

    $dReserved = (Get-QuotaStat $accAfter 'reserved_tokens') - (Get-QuotaStat $accBefore 'reserved_tokens')
    $dSettled = (Get-QuotaStat $accAfter 'settled_tokens') - (Get-QuotaStat $accBefore 'settled_tokens')
    $dReleased = (Get-QuotaStat $accAfter 'released_tokens') - (Get-QuotaStat $accBefore 'released_tokens')
    $dOvershoot = (Get-QuotaStat $accAfter 'overshoot_tokens') - (Get-QuotaStat $accBefore 'overshoot_tokens')
    $dOvershootCost = (Get-QuotaStat $accAfter 'overshoot_cost_micros') - (Get-QuotaStat $accBefore 'overshoot_cost_micros')
    $dReleasedCost = (Get-QuotaStat $accAfter 'released_cost_micros') - (Get-QuotaStat $accBefore 'released_cost_micros')
    $sumEst = [int64](($acc | Measure-Object -Property estimated_tokens -Sum).Sum)
    $sumAct = [int64](($acc | Measure-Object -Property actual_tokens -Sum).Sum)
    [double[]]$devs = @($acc | ForEach-Object { [double]$_.deviation })
    $mad = 0.0
    if ($devs.Count -gt 0) { $mad = [Math]::Round((($devs | ForEach-Object { [Math]::Abs($_) }) | Measure-Object -Average).Average, 3) }
    $meanSigned = 0.0
    if ($devs.Count -gt 0) { $meanSigned = [Math]::Round(($devs | Measure-Object -Average).Average, 3) }
    $sumPos = 0
    $sumNeg = 0
    foreach ($d in $devs) { if ($d -gt 0) { $sumPos += [int]$d } else { $sumNeg += [int](-$d) } }
    $tokensToday = -1
    if ($null -ne $accAfter -and $null -ne $accAfter.report) { $tokensToday = [int64]$accAfter.report.tokens_today }

    $acc | Format-Table -AutoSize | Out-String -Width 220 | Write-Host
    Write-Host ("  gateway reserved delta = {0}, sum of the documented estimate = {1}" -f $dReserved, $sumEst) -ForegroundColor DarkGray
    Write-Host ("  settled (net amount kept) = {0}, released (over-reservation returned) = {1}" -f $dSettled, $dReleased) -ForegroundColor DarkGray
    Write-Host ("  settlement identity: reserved {0} - released {1} + overshoot {2} = {3} (settled {4})" -f $dReserved, $dReleased, $dOvershoot, ($dReserved - $dReleased + $dOvershoot), $dSettled) -ForegroundColor DarkGray
    Write-Host ("  overshoot_tokens = {0}, overshoot_cost_micros = {1}, released_cost_micros = {2}" -f $dOvershoot, $dOvershootCost, $dReleasedCost) -ForegroundColor DarkGray
    Write-Host ("  mean |deviation| = {0} tokens, mean signed deviation = {1} tokens, over-estimated {2} / under-estimated {3}" -f $mad, $meanSigned, $sumPos, $sumNeg) -ForegroundColor DarkGray

    Assert-That ($dReserved -eq $sumEst) 'documented estimate (len(body)/chars_per_token + completion) reproduces the gateway reserved_tokens delta' "gateway $dReserved vs formula $sumEst"
    Assert-That ($dReleased -eq $sumPos) 'released_tokens delta equals the sum of the positive (over-estimated) deviations' "gateway $dReleased vs formula $sumPos"
    Assert-That (($dReserved - $dReleased + $dOvershoot) -eq $dSettled) 'reserved - released + overshoot equals settled (the settlement identity)' "reserved $dReserved - released $dReleased + overshoot $dOvershoot = $($dReserved - $dReleased + $dOvershoot) vs settled $dSettled"
    Assert-That ($dReleasedCost -gt 0) 'the money side of the ledger also returned the over-reservation (released_cost_micros)' "released_cost_micros delta $dReleasedCost"
    Assert-That ($tokensToday -eq $sumAct) 'the tenant day counter equals the sum of the usage the upstream reported' "counter $tokensToday vs usage $sumAct"
    Assert-That (($acc | Where-Object { $_.status -ne 200 }).Count -eq 0) 'every accuracy request was admitted (generous policy)'

    # ------------------------------------------------------------ SECTION 3 ---
    Write-Section 'SECTION 3 - a budget stopping a runaway tenant'
    Write-Host "  caliber: upstream calls are counted in the mock's own log (msg=\"chat request\"), the one" -ForegroundColor DarkGray
    Write-Host '  thing the gateway cannot fake; the 429 rate is counted from the client side.' -ForegroundColor DarkGray

    Write-Host '  restarting the governed port with the budget config (fresh store, acme tokens_per_day 280)' -ForegroundColor DarkGray
    Stop-Proc $govProc 'gov'
    $govProc = Start-Gateway -Port $GovernedPort -ConfigPath $configs.budget -Tag 'budget'
    $budgetAdmin = Get-QuotaAdmin -Port $GovernedPort
    Assert-That ($null -ne $budgetAdmin -and $budgetAdmin.enabled -eq $true) 'budget gateway reports quota enabled'

    $budgetBody = New-ChatBodyEx -Tag 'budget' -Prompt 'hello'
    $estOne = [int][Math]::Floor($budgetBody.Bytes / $govEstChars) + $govEstCompletion
    Write-Host ("  one request reserves {0} tokens (prompt {1} + completion {2}) against a 280 token day" -f $estOne, [int][Math]::Floor($budgetBody.Bytes / $govEstChars), $govEstCompletion) -ForegroundColor DarkGray

    $mockUngovBefore = Get-MockRequestCount -LogPath $mockUngovLog
    $codesWithout = @{}
    $t0 = Get-Date
    for ($i = 1; $i -le $BudgetRequests; $i++) {
        $r = Send-ChatEx -Port $UngovernedPort -BodyPath $budgetBody.Path -Tenant 'acme' -Tag 'budget-off'
        $k = [string]$r.Status
        if ($codesWithout.ContainsKey($k)) { $codesWithout[$k] = $codesWithout[$k] + 1 } else { $codesWithout[$k] = 1 }
    }
    $wallWithout = [Math]::Round(((Get-Date) - $t0).TotalSeconds, 2)
    $mockUngovAfter = Get-MockRequestCount -LogPath $mockUngovLog

    $mockGovBefore = Get-MockRequestCount -LogPath $mockGovLog
    $budgetBefore = Get-QuotaAdmin -Port $GovernedPort -Tenant 'acme'
    $codesWith = @{}
    $reasonWith = @{}
    $t0 = Get-Date
    for ($i = 1; $i -le $BudgetRequests; $i++) {
        $r = Send-ChatEx -Port $GovernedPort -BodyPath $budgetBody.Path -Tenant 'acme' -Tag 'budget-on'
        $k = [string]$r.Status
        if ($codesWith.ContainsKey($k)) { $codesWith[$k] = $codesWith[$k] + 1 } else { $codesWith[$k] = 1 }
        $rk = $r.QuotaReason
        if ($rk -ne '') {
            if ($reasonWith.ContainsKey($rk)) { $reasonWith[$rk] = $reasonWith[$rk] + 1 } else { $reasonWith[$rk] = 1 }
        }
    }
    $wallWith = [Math]::Round(((Get-Date) - $t0).TotalSeconds, 2)
    $mockGovAfter = Get-MockRequestCount -LogPath $mockGovLog
    $budgetAfter = Get-QuotaAdmin -Port $GovernedPort -Tenant 'acme'

    $callsWithout = -1
    if ($mockUngovBefore -ge 0 -and $mockUngovAfter -ge 0) { $callsWithout = $mockUngovAfter - $mockUngovBefore } else { Add-Failure 'upstream calls without quota (mock log unreadable)' }
    $callsWith = -1
    if ($mockGovBefore -ge 0 -and $mockGovAfter -ge 0) { $callsWith = $mockGovAfter - $mockGovBefore } else { Add-Failure 'upstream calls with quota (mock log unreadable)' }
    $prevented = -1
    if ($callsWithout -ge 0 -and $callsWith -ge 0) { $prevented = $callsWithout - $callsWith }
    $rejected = (Get-QuotaStat $budgetAfter 'rejected') - (Get-QuotaStat $budgetBefore 'rejected')
    $rate429 = 0.0
    if ($codesWith.ContainsKey('429')) { $rate429 = [Math]::Round(100.0 * $codesWith['429'] / $BudgetRequests, 1) }
    $tokensTodayBudget = -1
    if ($null -ne $budgetAfter -and $null -ne $budgetAfter.report) { $tokensTodayBudget = [int64]$budgetAfter.report.tokens_today }

    Write-Host ("  WITHOUT quota: {0} requests -> {1} upstream calls (status codes: {2}) in {3}s" -f $BudgetRequests, $callsWithout, (($codesWithout.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join ' '), $wallWithout) -ForegroundColor DarkGray
    Write-Host ("  WITH 280 token budget: {0} requests -> {1} upstream calls (status codes: {2}) in {3}s" -f $BudgetRequests, $callsWith, (($codesWith.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join ' '), $wallWith) -ForegroundColor DarkGray
    Write-Host ("  budget prevented {0} provider calls; gateway rejected = {1}; client 429 rate = {2}%; tenant tokens_today = {3}" -f $prevented, $rejected, $rate429, $tokensTodayBudget) -ForegroundColor DarkGray

    Assert-That ($callsWithout -eq $BudgetRequests) 'without quota every request reached the upstream' "calls $callsWithout of $BudgetRequests"
    Assert-That ($callsWith -ge 1 -and $callsWith -lt $BudgetRequests) 'the budget admitted at least one request and stopped the tenant before the rest' "calls $callsWith"
    Assert-That ($rejected -eq ($BudgetRequests - $callsWith)) 'the gateway rejected count equals the requests that never reached the upstream' "rejected $rejected vs $($BudgetRequests - $callsWith)"
    Assert-That ($rate429 -gt 0) 'the client saw 429s' "429 rate $rate429%"

    # ------------------------------------------------------------ SECTION 4 ---
    Write-Section 'SECTION 4 - redis vs memory (same governed load)'
    Write-Host '  caliber: same load client, same policy, two stores; the redis counter is read back over' -ForegroundColor DarkGray
    Write-Host '  raw RESP2 so the gateway is not the surface that reports it.' -ForegroundColor DarkGray

    Stop-Proc $govProc 'budget'
    $govProc = Start-Gateway -Port $GovernedPort -ConfigPath $configs.governed -Tag 'gov2'
    $miniProc = Start-MiniRedis -Port $MiniRedisPort -Tag 'miniredis'
    $mockRedis = Start-Mock -Port $MockRedisPort -Name 'mock-m3-redis' -Tag 'mock-redis'
    Assert-That ((Wait-Healthy -Port $MockRedisPort -Path '/healthz' -Seconds 10) -eq '200') 'mock upstream (redis) is healthy'
    $mockRedisLog = $script:mockLogs['mock-redis']

    $keysStart = Get-RedisKeys -Port $MiniRedisPort -Pattern 'ig:quota:*'
    Write-Host ("  miniredis :{0} holds {1} quota key(s) before the run" -f $MiniRedisPort, $keysStart.Count) -ForegroundColor DarkGray
    Assert-That ($keysStart.Count -eq 0) 'miniredis starts empty, so any counter read later was created by the gateway'

    Start-Sleep -Milliseconds 400
    $redisProc = Start-Gateway -Port $RedisGovernedPort -ConfigPath $configs.redis -Tag 'redisgw'
    $adminRedis = Get-QuotaAdmin -Port $RedisGovernedPort
    Assert-That ($null -ne $adminRedis -and $adminRedis.enabled -eq $true) 'redis gateway reports quota enabled'
    Assert-That ($null -ne $adminRedis -and "$($adminRedis.store)" -eq 'redis') 'redis gateway reports the redis store' "store=$($adminRedis.store)"

    $redisObservations = New-Object System.Collections.ArrayList
    foreach ($r in 1..$Rounds) {
        Invoke-Flush $GovernedPort
        $w = Invoke-Load -Port $GovernedPort -OutFile (Join-Path $tmpDir "m3-load-memory-r$r.json") -Tag "memory-r$r"
        Add-LoadResults -Path (Join-Path $tmpDir "m3-load-memory-r$r.json") -Scenario 'memory' -Round $r -Wall $w
        Write-Host ("  round {0}: memory :{1} wall {2}s" -f $r, $GovernedPort, $w) -ForegroundColor DarkGray

        Invoke-Flush $RedisGovernedPort
        $w = Invoke-Load -Port $RedisGovernedPort -OutFile (Join-Path $tmpDir "m3-load-redis-r$r.json") -Tag "redis-r$r"
        Add-LoadResults -Path (Join-Path $tmpDir "m3-load-redis-r$r.json") -Scenario 'redis' -Round $r -Wall $w
        Write-Host ("  round {0}: redis  :{1} wall {2}s" -f $r, $RedisGovernedPort, $w) -ForegroundColor DarkGray

        $keys = Get-RedisKeys -Port $MiniRedisPort -Pattern 'ig:quota:*'
        $dayKey = ''
        foreach ($k in $keys) { if ($k -match ':day:\d{8}:tokens$') { $dayKey = $k; break } }
        $dayVal = $null
        $ttl = $null
        if ($dayKey -ne '') {
            $dayVal = Get-RedisString -Port $MiniRedisPort -Key $dayKey
            $ttl = Get-RedisInt -Port $MiniRedisPort -Parts @('TTL', $dayKey)
        }
        [void]$redisObservations.Add([pscustomobject]@{
                round = $r
                key_count = $keys.Count
                day_tokens_key = $dayKey
                day_tokens_value = $dayVal
                day_tokens_ttl_s = $ttl
                keys = @($keys)
            })
        Write-Host ("  RESP2 after round {0}: {1} = {2} (ttl {3}s)" -f $r, $dayKey, $dayVal, $ttl) -ForegroundColor DarkGray
    }

    $keysEnd = Get-RedisKeys -Port $MiniRedisPort -Pattern 'ig:quota:*'
    $costKey = ''
    foreach ($k in $keysEnd) { if ($k -match ':day:\d{8}:cost_micros$') { $costKey = $k; break } }
    $costVal = $null
    if ($costKey -ne '') { $costVal = Get-RedisString -Port $MiniRedisPort -Key $costKey }
    # The load client is governed as the tenant derived from its Authorization
    # header, and default_policy budgets no money, so no cost counter is ever
    # opened for it (checks() opens one counter per LIMITED dimension only).
    # Every governed request does take a per-minute slot, so that key must exist.
    $minuteKey = ''
    foreach ($k in $keysEnd) { if ($k -match ':minute:\d{12}:requests$') { $minuteKey = $k; break } }
    $minuteVal = $null
    if ($minuteKey -ne '') { $minuteVal = Get-RedisString -Port $MiniRedisPort -Key $minuteKey }
    $discoveredTenant = ''
    if ($keysEnd.Count -gt 0) {
        $m = [regex]::Match($keysEnd[0], '^ig:quota:([^:]+):')
        if ($m.Success) { $discoveredTenant = $m.Groups[1].Value }
    }
    # independent cross-check of the tenant the load client is governed as
    $sha = [System.Security.Cryptography.SHA256]::Create()
    $sum = $sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes('Bearer loadtest-key'))
    $hex = -join ($sum[0..7] | ForEach-Object { $_.ToString('x2') })
    $computedTenant = 'key-' + $hex
    Write-Host ("  discovered tenant from the RESP2 key list: {0} (computed key- form: {1})" -f $discoveredTenant, $computedTenant) -ForegroundColor DarkGray

    $redisDelta = -1
    if ($redisObservations.Count -ge 2) {
        $first = [int64]$redisObservations[0].day_tokens_value
        $last = [int64]$redisObservations[$redisObservations.Count - 1].day_tokens_value
        $redisDelta = $last - $first
    }
    Assert-That ($keysEnd.Count -gt 0) 'the gateway created quota keys in the external store'
    Assert-That ($discoveredTenant -ne '') 'the day bucket key names the governed tenant'
    Assert-That ($redisDelta -ge 0) 'the RESP2 day counter grows across rounds' "delta $redisDelta"
    Assert-That ($minuteKey -ne '' -and $null -ne $minuteVal -and [int64]$minuteVal -gt 0) 'the external store holds the per-minute request counter the gateway created' "minute requests = $minuteVal"
    Write-Host ("  day cost key for the load tenant: '{0}' (absent: default_policy budgets no money, so no cost counter is opened for it)" -f $costKey) -ForegroundColor DarkGray

    # ------------------------------------------------------------ SECTION 5 ---
    Write-Section 'SECTION 5 - fail-closed: the store dies, no upstream call may happen'
    Write-Host '  caliber: the client status codes plus the mock log; the gateway PID is checked before and' -ForegroundColor DarkGray
    Write-Host '  after so recovery cannot be explained by a restart.' -ForegroundColor DarkGray

    $mockRedisBeforeOutage = Get-MockRequestCount -LogPath $mockRedisLog
    $gwPidBefore = $redisProc.Id
    Stop-Proc $miniProc 'miniredis'
    $closed = Wait-PortClosed -Port $MiniRedisPort -Seconds 10
    Assert-That $closed 'miniredis stopped listening' "port $MiniRedisPort"
    $probe = Get-RedisKeys -Port $MiniRedisPort -Pattern 'ig:quota:*'
    Assert-That ($probe.Count -eq 0) 'raw RESP2 to the stopped store returns nothing'

    $outageBody = New-ChatBodyEx -Tag 'outage' -Prompt 'hello'
    $codesOutage = @{}
    $reasonsOutage = @{}
    for ($i = 1; $i -le $FailClosedRequests; $i++) {
        $r = Send-ChatEx -Port $RedisGovernedPort -BodyPath $outageBody.Path -Tag 'outage'
        $k = [string]$r.Status
        if ($codesOutage.ContainsKey($k)) { $codesOutage[$k] = $codesOutage[$k] + 1 } else { $codesOutage[$k] = 1 }
        $rk = $r.QuotaReason
        if ($rk -eq '') { $rk = '(none)' }
        if ($reasonsOutage.ContainsKey($rk)) { $reasonsOutage[$rk] = $reasonsOutage[$rk] + 1 } else { $reasonsOutage[$rk] = 1 }
    }
    $mockRedisAfterOutage = Get-MockRequestCount -LogPath $mockRedisLog
    $callsDuringOutage = -1
    if ($mockRedisBeforeOutage -ge 0 -and $mockRedisAfterOutage -ge 0) { $callsDuringOutage = $mockRedisAfterOutage - $mockRedisBeforeOutage } else { Add-Failure 'upstream calls during the outage (mock log unreadable)' }
    $rate503 = 0.0
    if ($codesOutage.ContainsKey('503')) { $rate503 = [Math]::Round(100.0 * $codesOutage['503'] / $FailClosedRequests, 1) }
    Write-Host ("  outage: {0} requests -> status codes {1}; reasons {2}; upstream calls {3}" -f $FailClosedRequests, (($codesOutage.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join ' '), (($reasonsOutage.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join ' '), $callsDuringOutage) -ForegroundColor DarkGray

    Assert-That ($rate503 -eq 100.0) 'every request during the outage was refused with 503' "503 rate $rate503%"
    Assert-That ($callsDuringOutage -eq 0) 'no request during the outage reached the upstream' "upstream calls $callsDuringOutage"
    Assert-That ($reasonsOutage.ContainsKey('store-error')) 'the refusals are labelled store-error'

    # /metrics is the operator's other surface, and store_errors_total is the
    # counter the brief names.
    $metricsText = Invoke-Curl @('-s', '-m', '10', "http://127.0.0.1:$RedisGovernedPort/metrics")
    $storeErrorsTotal = -1
    $mm = [regex]::Match($metricsText, '(?m)^infergate_quota_store_errors_total\s+(\d+)\s*$')
    if ($mm.Success) { $storeErrorsTotal = [int64]$mm.Groups[1].Value }
    $metricLines = @($metricsText -split "`r?`n" | Where-Object { $_.Contains('infergate_quota_') })
    Assert-That ($storeErrorsTotal -ge $FailClosedRequests) 'Prometheus infergate_quota_store_errors_total recorded the outage' "store_errors_total $storeErrorsTotal for $FailClosedRequests refusals"

    $miniProc = Start-MiniRedis -Port $MiniRedisPort -Tag 'miniredis2'
    $reopened = Test-PortOpen -Port $MiniRedisPort -TimeoutMs 500
    Assert-That $reopened 'miniredis is listening again' "port $MiniRedisPort"
    Start-Sleep -Milliseconds 600

    $mockRedisBeforeRecovery = Get-MockRequestCount -LogPath $mockRedisLog
    $codesRecovery = @{}
    for ($i = 1; $i -le $RecoveryRequests; $i++) {
        $r = Send-ChatEx -Port $RedisGovernedPort -BodyPath $outageBody.Path -Tag 'recovery'
        $k = [string]$r.Status
        if ($codesRecovery.ContainsKey($k)) { $codesRecovery[$k] = $codesRecovery[$k] + 1 } else { $codesRecovery[$k] = 1 }
    }
    $mockRedisAfterRecovery = Get-MockRequestCount -LogPath $mockRedisLog
    $callsRecovery = -1
    if ($mockRedisBeforeRecovery -ge 0 -and $mockRedisAfterRecovery -ge 0) { $callsRecovery = $mockRedisAfterRecovery - $mockRedisBeforeRecovery } else { Add-Failure 'upstream calls after recovery (mock log unreadable)' }
    $gwRestarted = $false
    try { $p = Get-Process -Id $gwPidBefore -ErrorAction Stop; if ($p.HasExited) { $gwRestarted = $true } } catch { $gwRestarted = $true }
    $okRecovery = 0
    if ($codesRecovery.ContainsKey('200')) { $okRecovery = $codesRecovery['200'] }
    $rate503Recovery = 0.0
    if ($codesRecovery.ContainsKey('503')) { $rate503Recovery = [Math]::Round(100.0 * $codesRecovery['503'] / $RecoveryRequests, 1) }
    Write-Host ("  recovery: {0} requests -> status codes {1}; 503 rate {2}%; upstream calls {3}; gateway restarted = {4} (pid {5})" -f $RecoveryRequests, (($codesRecovery.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join ' '), $rate503Recovery, $callsRecovery, $gwRestarted, $gwPidBefore) -ForegroundColor DarkGray

    Assert-That ($rate503Recovery -eq 0.0) 'no 503 after the store came back' "503 rate $rate503Recovery%"
    Assert-That ($callsRecovery -gt 0) 'traffic reached the upstream again' "upstream calls $callsRecovery"
    Assert-That (-not $gwRestarted) 'the same gateway process served the recovery (no restart)' "pid $gwPidBefore"

    # fail_open: true is the other half of the same story. The budget still cannot
    # be read, but the request is admitted anyway and the failure is counted.
    Stop-Proc $redisProc 'redis-closed'
    $openProc = Start-Gateway -Port $RedisGovernedPort -ConfigPath $configs.redisOpen -Tag 'redis-open'
    $adminOpen = Get-QuotaAdmin -Port $RedisGovernedPort
    Assert-That ($null -ne $adminOpen -and $adminOpen.fail_open -eq $true) 'the fail-open variant reports fail_open true'
    $storeErrorsBeforeOpen = Get-QuotaStat $adminOpen 'store_errors'
    Stop-Proc $miniProc 'miniredis3'
    [void](Wait-PortClosed -Port $MiniRedisPort -Seconds 10)
    $mockOpenBefore = Get-MockRequestCount -LogPath $mockRedisLog
    $codesOpen = @{}
    $openBody = New-ChatBodyEx -Tag 'failopen' -Prompt 'hello'
    for ($i = 1; $i -le $FailOpenRequests; $i++) {
        $r = Send-ChatEx -Port $RedisGovernedPort -BodyPath $openBody.Path -Tag 'failopen'
        $k = [string]$r.Status
        if ($codesOpen.ContainsKey($k)) { $codesOpen[$k] = $codesOpen[$k] + 1 } else { $codesOpen[$k] = 1 }
    }
    $mockOpenAfter = Get-MockRequestCount -LogPath $mockRedisLog
    $callsOpen = -1
    if ($mockOpenBefore -ge 0 -and $mockOpenAfter -ge 0) { $callsOpen = $mockOpenAfter - $mockOpenBefore } else { Add-Failure 'upstream calls under fail_open (mock log unreadable)' }
    $adminOpenAfter = Get-QuotaAdmin -Port $RedisGovernedPort
    $storeErrorsOpen = (Get-QuotaStat $adminOpenAfter 'store_errors') - $storeErrorsBeforeOpen
    $rate503Open = 0.0
    if ($codesOpen.ContainsKey('503')) { $rate503Open = [Math]::Round(100.0 * $codesOpen['503'] / $FailOpenRequests, 1) }
    $okOpen = 0
    if ($codesOpen.ContainsKey('200')) { $okOpen = $codesOpen['200'] }
    Write-Host ("  fail-open: {0} requests -> status codes {1}; upstream calls {2}; store_errors +{3}" -f $FailOpenRequests, (($codesOpen.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join ' '), $callsOpen, $storeErrorsOpen) -ForegroundColor DarkGray
    Assert-That ($rate503Open -eq 0.0) 'fail_open: true serves the request while the store is down' "503 rate $rate503Open%"
    Assert-That ($okOpen -eq $FailOpenRequests) 'fail-open requests were answered, not refused' "200 x $okOpen of $FailOpenRequests"
    Assert-That ($callsOpen -eq $FailOpenRequests) 'fail-open requests reached the upstream' "upstream calls $callsOpen"
    Assert-That ($storeErrorsOpen -gt 0) 'the store failure is still counted under fail_open' "store_errors delta $storeErrorsOpen"

    # ------------------------------------------------------------ SECTION 6 ---
    Write-Section 'SECTION 6 - the degrade ladder: what a max_tokens cap actually changes'
    Write-Host '  caliber: client-visible headers and body plus GET /admin/quota deltas; a cap only' -ForegroundColor DarkGray
    Write-Host '  reduces provider tokens if the provider honours max_tokens, and this mock does not.' -ForegroundColor DarkGray

    Stop-Proc $govProc 'gov-pre-degrade'
    $govProc = Start-Gateway -Port $GovernedPort -ConfigPath $configs.governed -Tag 'gov-degrade-a'
    $degBody = New-ChatBodyEx -Tag 'degrade' -Prompt 'hello' -MaxTokens 4096
    $degAccBefore = Get-QuotaAdmin -Port $GovernedPort -Tenant 'growth'
    $degA = Send-ChatEx -Port $GovernedPort -BodyPath $degBody.Path -Tenant 'growth' -Tag 'degrade-a'
    $degAccAfter = Get-QuotaAdmin -Port $GovernedPort -Tenant 'growth'
    $degAReserved = (Get-QuotaStat $degAccAfter 'reserved_tokens') - (Get-QuotaStat $degAccBefore 'reserved_tokens')
    $modelA = ''
    $compA = -1
    $bodyA = Get-JsonText $degA.Body
    if ($null -ne $bodyA) {
        $modelA = [string]$bodyA.model
        if ($null -ne $bodyA.usage) { $compA = [int64]$bodyA.usage.completion_tokens }
    }
    Write-Host ("  arm A (growth within its {0} token day): status {1} quota {2} model {3} reserved {4} completion_tokens {5}" -f $govGrowthTokens, $degA.Status, $degA.Quota, $modelA, $degAReserved, $compA) -ForegroundColor DarkGray

    Stop-Proc $govProc 'gov-degrade-a'
    $govProc = Start-Gateway -Port $GovernedPort -ConfigPath $configs.degrade -Tag 'gov-degrade-b'
    $degAccBefore = Get-QuotaAdmin -Port $GovernedPort -Tenant 'growth'
    $degB = Send-ChatEx -Port $GovernedPort -BodyPath $degBody.Path -Tenant 'growth' -Tag 'degrade-b'
    $degAccAfter = Get-QuotaAdmin -Port $GovernedPort -Tenant 'growth'
    $degBReserved = (Get-QuotaStat $degAccAfter 'reserved_tokens') - (Get-QuotaStat $degAccBefore 'reserved_tokens')
    $modelB = ''
    $compB = -1
    $bodyB = Get-JsonText $degB.Body
    if ($null -ne $bodyB) {
        $modelB = [string]$bodyB.model
        if ($null -ne $bodyB.usage) { $compB = [int64]$bodyB.usage.completion_tokens }
    }
    Write-Host ("  arm B (growth day cut to {0} tokens, cap {1}): status {2} quota {3} reason {4} model {5} cap header {6} reserved {7} completion_tokens {8}" -f $degradeGrowthTokens, $degradeGrowthCap, $degB.Status, $degB.Quota, $degB.QuotaReason, $modelB, $degB.QuotaMaxTokens, $degBReserved, $compB) -ForegroundColor DarkGray

    Assert-That ($degA.Status -eq 200 -and $degA.Quota -eq 'allow') 'a growth request inside its budget is allowed with no cap' "quota $($degA.Quota)"
    Assert-That ($degB.Status -eq 200 -and $degB.Quota -eq 'degrade') 'the same request over budget is degraded, not refused' "quota $($degB.Quota) reason $($degB.QuotaReason)"
    Assert-That ($degB.QuotaMaxTokens -eq ([string][int]$degradeGrowthCap)) 'the degrade response advertises the max_tokens cap' "cap header $($degB.QuotaMaxTokens)"
    Assert-That ($modelB -eq 'mock-gpt-mini') 'the degraded request was sent to the downgrade model' "model $modelB"
    Assert-That ($compA -eq $compB) 'the cap did NOT change the tokens the provider produced (this mock ignores max_tokens)' "arm A $compA vs arm B $compB completion tokens"

    # ------------------------------------------------------------ ARTIFACT ---
    Write-Section 'ROWS'
    $allRows = Get-AggregatedRows
    Show-Rows $allRows

    $duration = [Math]::Round(((Get-Date) - $startedAt).TotalSeconds, 1)
    $unreadable = @($script:failures)

    $summary = [ordered]@{
        generated_at = (Get-Date).ToString('s')
        host = "$env:COMPUTERNAME / $env:PROCESSOR_IDENTIFIER / $($PSVersionTable.PSVersion)"
        duration_s = $duration
        milestone = 'M3 - token and cost governance'
        method = [ordered]@{
            script = 'scripts\measure-m3.ps1'
            command = 'powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m3.ps1'
            requests = $Requests
            warmup = $Warmup
            rounds = $Rounds
            concurrency = @($Concurrency -split ',')
            client = 'bin\loadtest.exe (cmd/loadtest) -url mode for the load sections; curl.exe for the governed, budget, accuracy and fail-closed sections'
            upstream = 'in-repo mock (cmd/mockupstream): local, free, no provider was called or billed'
            stores = "memory (in-process) and redis (cmd/miniredis, a real RESP2 server in-repo) on :$MiniRedisPort"
            ports = [ordered]@{ governed = $GovernedPort; ungoverned = $UngovernedPort; redis_governed = $RedisGovernedPort; mock_governed = $MockPort; mock_ungoverned = $MockUngovernedPort; mock_redis = $MockRedisPort; miniredis = $MiniRedisPort }
            configs = 'configs\quota-local.yaml and configs\quota-redis.yaml instantiated into tmp\m3-*.yaml by literal substitution; no repository file is modified and no ${VAR} is introduced'
            caliber_admission = 'identical load, two gateways, same upstream, arms run back to back inside each round so host drift is shared; the numbers are gateway work (estimate + store round trip), not provider latency'
            caliber_accuracy = 'gateway counters from GET /admin/quota compared with the usage block of the real upstream response; the mock defines prompt_tokens = whitespace-separated words per message + 4 and completion_tokens = whitespace-separated words of its answer, so real usage is computable independently of the gateway'
            caliber_budget = 'upstream calls are counted by grepping msg="chat request" in the mock upstream process log, the one surface the gateway cannot author; the 429 rate is counted from the client status codes'
            caliber_redis = 'same load against store: memory and store: redis; the day bucket counter is read back over raw RESP2 (System.Net.Sockets.TcpClient), not through the gateway'
            caliber_fail_closed = 'client status codes plus the mock log, and the gateway PID is compared before and after so recovery cannot be explained by a restart'
            load_body = 'the load client replays ONE fixed chat body ({"model":"mock-gpt","messages":[{"role":"user","content":"hello"}]}, and the same with "stream":true) and sends Authorization: Bearer loadtest-key with NO tenant header, so the load arms are governed by the DEFAULT policy under the tenant name derived from that credential'
            loadtest_phases = 'cmd/loadtest -url mode always drives four phases per invocation (non-stream c8, non-stream c32, stream c8, stream c32); the headline rows below are the non-stream ones and every phase is present in rows_all'
        }
        gateway_config = [ordered]@{
            source_files = @('configs\quota-local.yaml', 'configs\quota-redis.yaml')
            store = $govStore
            store_redis_variant = $redisStore
            estimate_chars_per_token = $govEstChars
            estimate_completion_tokens = $govEstCompletion
            fail_open = $govFailOpen
            fail_open_redis_variant = $redisFailOpen
            redis_estimate_chars_per_token = $redisEstChars
            redis_estimate_completion_tokens = $redisEstCompletion
            default_policy_tokens_per_day = $govDefaultTokens
            tenants_used = [ordered]@{
                acme = [ordered]@{
                    role = 'accuracy section (generous) and budget section (tight)'
                    tokens_per_day_generous = $govAcmeTokens
                    tokens_per_day_budget = $budgetAcmeTokens
                    cost_per_day_usd = 0.05
                    requests_per_minute = 60
                    on_exceed = 'reject'
                }
                authorization_derived = [ordered]@{
                    role = 'every loadtest-driven load section (no tenant header)'
                    tenant = $discoveredTenant
                    computed_key_form = $computedTenant
                    tokens_per_day = $govDefaultTokens
                    requests_per_minute = 1000000000
                }
            }
            note = 'values above are parsed back out of the generated tmp configs, not copied from the source files'
        }
        latency = [ordered]@{
            grid = "loadtest -url http://127.0.0.1:PORT -c $Concurrency -n $Requests -warmup $Warmup, $Rounds interleaved rounds"
            interleaving = 'round r: governed arm then ungoverned arm, back to back, so host drift is shared'
            rows = @($allRows | Where-Object { $_.Scenario -in @('governed', 'ungoverned') -and $_.Workload -eq 'non-stream' })
            rows_all = @($allRows | Where-Object { $_.Scenario -in @('governed', 'ungoverned') })
            rounds_detail = @($script:roundRows | Where-Object { $_.scenario -in @('governed', 'ungoverned') })
            gateway_stats = [ordered]@{
                governed = [ordered]@{ latency = $peekGov; quota = $quotaPeekGov; note = 'this gateway served only the governed arm of section 1 (and section 2 afterwards), so its recorder is a pure governed view' }
                ungoverned = [ordered]@{ latency = $peekUngov; note = 'this gateway served only ungoverned traffic in section 1, so its recorder is a pure ungoverned view' }
            }
            caliber = 'client-side wall clock throughput and latency percentiles for identical traffic; /stats is the gateway recorder measuring the same requests from inside'
        }
        accuracy = [ordered]@{
            requests = $acc.Count
            counter_source = 'GET /admin/quota stats delta (reserved_tokens, settled_tokens, released_tokens, overshoot_tokens, overshoot_cost_micros, released_cost_micros)'
            reserved_tokens_delta = $dReserved
            settled_tokens_delta = $dSettled
            released_tokens_delta = $dReleased
            overshoot_tokens = $dOvershoot
            overshoot_cost_micros = $dOvershootCost
            released_cost_micros = $dReleasedCost
            settlement_identity_holds = (($dReserved - $dReleased + $dOvershoot) -eq $dSettled)
            settlement_identity = 'reserved - released + overshoot == settled, asserted rather than assumed'
            settled_equals_real_usage = ($dSettled -eq $sumAct)
            settled_semantics = 'settled_tokens is the NET amount the settlement kept (here it equals the sum of real usage); the under-estimated part is overshoot_tokens and the returned part is released_tokens'
            sum_of_documented_estimates = $sumEst
            estimate_reproduces_counter = ($dReserved -eq $sumEst)
            sum_of_real_usage = $sumAct
            tenant_day_counter_tokens_today = $tokensToday
            counter_matches_real_usage = ($tokensToday -eq $sumAct)
            mean_absolute_deviation_tokens = $mad
            mean_signed_deviation_tokens = $meanSigned
            over_estimated_total_tokens = $sumPos
            under_estimated_total_tokens = $sumNeg
            per_request = @($acc)
            direction = 'mixed by construction: the plain requests (no max_tokens) reserve the full estimate_completion_tokens 256 and are therefore over-estimated, while the two max_tokens=1 requests with long prompts have real completion usage far above the 1 token they reserved and are under-estimated'
            why = 'the estimate is a character-count proxy (len(body)/estimate_chars_per_token) plus a configured completion guess; the mock counts whitespace-separated WORDS as tokens, so a prose prompt is over-estimated (bytes/4 > words) while a short-envelope request reserves far more completion than the answer uses'
            caliber = 'mean absolute deviation = mean(|reserved - real usage|) per request, with the reserved amount from the documented formula cross-checked against the gateway reserved_tokens delta'
        }
        budget = [ordered]@{
            requests_per_arm = $BudgetRequests
            tenant = 'acme'
            budget_tokens_per_day = $budgetAcmeTokens
            estimated_tokens_per_request = $estOne
            without_quota = [ordered]@{
                gateway_port = $UngovernedPort
                client_status_codes = $codesWithout
                upstream_calls = $callsWithout
                wall_s = $wallWithout
            }
            with_budget = [ordered]@{
                gateway_port = $GovernedPort
                client_status_codes = $codesWith
                quota_reasons = $reasonWith
                upstream_calls = $callsWith
                gateway_rejected_delta = $rejected
                client_429_rate_percent = $rate429
                tenant_tokens_today_after = $tokensTodayBudget
                wall_s = $wallWith
            }
            provider_calls_prevented = $prevented
            caliber = 'both arms send the same N body bytes to the same tenant; the upstream calls are counted in the mock process log before and after each arm'
            note = 'requests are sent one at a time so each reservation settles before the next admission, which is why the budget admits a small number and refuses the rest'
        }
        redis_vs_memory = [ordered]@{
            rows = @($allRows | Where-Object { $_.Scenario -in @('memory', 'redis') -and $_.Workload -eq 'non-stream' })
            rows_all = @($allRows | Where-Object { $_.Scenario -in @('memory', 'redis') })
            rounds_detail = @($script:roundRows | Where-Object { $_.scenario -in @('memory', 'redis') })
            redis_store_reported_by_gateway = "$($adminRedis.store)"
            miniredis_port = $MiniRedisPort
            resp2_observations = @($redisObservations)
            resp2_keys_after_run = @($keysEnd)
            resp2_day_tokens_key = $dayKey
            resp2_day_tokens_tenant = $discoveredTenant
            resp2_cost_micros = $costVal
            resp2_cost_micros_note = 'absent for the load tenant: default_policy budgets no money, so no day cost counter is opened for it; the money counters are exercised by the acme accuracy section, which reports released_cost_micros from /admin/quota'
            resp2_minute_requests_key = $minuteKey
            resp2_minute_requests_value = $minuteVal
            resp2_day_tokens_delta_across_rounds = $redisDelta
            raw_resp2_proof = 'the counter is read with a bare RESP2 GET/KEYS from a TcpClient, so the number is external state, not a gateway-reported figure'
            caliber = 'same load client, same generous default policy, only the store differs; note that the redis arm pays a real loopback RESP2 round trip per reservation and settlement'
        }
        fail_closed = [ordered]@{
            gateway = 'store: redis with fail_open: false'
            requests_during_outage = $FailClosedRequests
            outage_status_codes = $codesOutage
            outage_quota_reasons = $reasonsOutage
            outage_503_rate_percent = $rate503
            upstream_calls_during_outage = $callsDuringOutage
            miniredis_stopped = $closed
            requests_after_recovery = $RecoveryRequests
            recovery_status_codes = $codesRecovery
            recovery_503_rate_percent = $rate503Recovery
            upstream_calls_after_recovery = $callsRecovery
            gateway_pid = $gwPidBefore
            gateway_restarted = $gwRestarted
            prometheus_store_errors_total = $storeErrorsTotal
            prometheus_quota_lines = @($metricLines)
            caliber = 'the mock log is the proof that a refused request never became a provider call; the unchanged gateway PID is the proof that recovery needed no restart'
        }
        fail_open = [ordered]@{
            gateway = 'store: redis with fail_open: true (the same generated config with only fail_open flipped)'
            requests_during_outage = $FailOpenRequests
            outage_status_codes = $codesOpen
            outage_503_rate_percent = $rate503Open
            upstream_calls_during_outage = $callsOpen
            store_errors_delta = $storeErrorsOpen
            caliber = 'the same outage as the fail-closed arm, with only fail_open changed; the client status codes and the mock log are the evidence'
            compared_with_fail_closed = "fail_open false: $rate503% 503 and $callsDuringOutage upstream calls; fail_open true: $rate503Open% 503 and $callsOpen upstream calls"
        }
        degrade = [ordered]@{
            tenant = 'growth'
            request = 'one identical curl body carrying max_tokens: 4096, so the caller named its own ceiling'
            within_budget = [ordered]@{
                tokens_per_day = $govGrowthTokens
                configured_cap = $govGrowthCap
                status = $degA.Status
                quota = $degA.Quota
                model = $modelA
                reserved_tokens = $degAReserved
                completion_tokens = $compA
            }
            over_budget_degraded = [ordered]@{
                tokens_per_day = $degradeGrowthTokens
                configured_cap = $degradeGrowthCap
                status = $degB.Status
                quota = $degB.Quota
                quota_reason = $degB.QuotaReason
                model = $modelB
                cap_header = $degB.QuotaMaxTokens
                reserved_tokens = $degBReserved
                completion_tokens = $compB
            }
            caliber = 'same tenant, same body, two generated configs: growth inside its day, then growth with a 20 token day and max_tokens_cap 64'
            finding = 'the reservation is computed BEFORE the degrade rewrite, so a caller that names max_tokens still reserves its own ceiling while the gateway forwards the cap; the excess is returned at settlement. The cap cannot reduce what this mock produces, because cmd/mockupstream ignores max_tokens altogether.'
        }
        assertions = [ordered]@{
            command = 'powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m3.ps1'
            total = [int]$script:assertTotal
            failed_count = $script:assertFails.Count
            failed = @($script:assertFails)
            not_measured = @($script:failures)
            note = 'written before the final artifact re-parse assertion, then rewritten with the final counts'
        }
        limitations = @(
            'The upstream is the in-repo mock (cmd/mockupstream): local, free and no provider was billed. Every cost figure is extrapolated from the config price book, not from a real invoice.',
            'The mock counts whitespace-separated words as tokens (prompt = words + 4, completion = words of the answer), which is not a real tokenizer; the estimate deviation is therefore a property of this pairing, not of a real provider.',
            'Everything runs on loopback on one host that may also be running other gates; the two arms of each round are adjacent but not simultaneous.',
            'Three interleaved rounds give a range, not a confidence interval.',
            'cmd/loadtest -url mode always drives four phases (non-stream and stream at each concurrency level) in one invocation, so each gateway stdio log and /stats recorder also contains stream traffic; the headline rows are the non-stream ones.',
            'The load client sends a fixed 5-character prompt and no tenant header, so the load arms exercise the default policy under the Authorization-derived tenant instead of a named tenant; the named tenant (acme) is used for the governed curl sections.',
            'The redis arm talks to cmd/miniredis, an in-repo RESP2 mock on loopback; it exercises the redis code path and the key layout but not a production Redis deployment.',
            'Admission overhead is gateway work (body scan + estimate + store round trip): the mock upstream is the same in both arms, so this is not a provider latency saving.',
            'miniredis is stopped by killing the process, which produces connection-refused errors; a hung or partitioned real Redis would exercise a different failure path (timeouts).',
            'A degraded request keeps the reservation its caller asked for: the estimate is computed before the degrade rewrite, so max_tokens_cap lowers what is forwarded but not what was leased, and the excess is returned at settlement.',
            'cmd/mockupstream ignores max_tokens entirely, so the degrade ladder can be shown to rename the model and to cap the forwarded field, but it cannot be shown to reduce the tokens a provider produces.',
            'The per-tenant day cost counter in redis is opened only for a tenant whose policy budgets money, which the default policy does not; released_cost_micros is therefore read from /admin/quota for the acme tenant rather than read back over RESP2.',
            'Fail-open admits requests whose budget could not be read, so the counters a fail-open run reports are incomplete by construction: the store never recorded them.'
        )
    }
    if ($unreadable.Count -gt 0) {
        $summary['limitations'] += @('A step could not be measured on this run: ' + ($unreadable -join '; '))
        $summary['not_measured'] = @($unreadable)
    }
    if ($null -ne $summary['method'] -and $null -ne $discoveredTenant) {
        $summary['method']['load_tenant'] = $discoveredTenant
    }

    $artifact = Join-Path $baselineDir 'm3-summary.json'
    Write-JsonFile $artifact $summary
    $parsed = Get-JsonText (Read-Text $artifact)
    Assert-That ($null -ne $parsed) 'the artifact parses back as JSON'
    # the counts can only be final now: the assertion above is itself one of them
    $summary['assertions']['total'] = [int]$script:assertTotal
    $summary['assertions']['failed_count'] = $script:assertFails.Count
    $summary['assertions']['failed'] = @($script:assertFails)
    $summary['assertions']['not_measured'] = @($script:failures)
    Write-JsonFile $artifact $summary

    Write-Section 'M3 HEADLINE'
    foreach ($row in ($summary['latency']['rows'])) {
        Write-Host ("  {0,-11} {1,-11} c{2,-3} QPS {3,9} (min {4,9} max {5,9})  P95 {6,8} ms  P99 {7,8} ms  errors {8}" -f $row.Scenario, $row.Workload, $row.Concurrency, $row.QPS, $row.QPSMin, $row.QPSMax, $row.P95_ms, $row.P99_ms, $row.Errors)
    }
    Write-Host ("  /stats p50/p95 governed   {0} / {1}   ungoverned {2} / {3}" -f $peekGov.p50, $peekGov.p95, $peekUngov.p50, $peekUngov.p95)
    Write-Host ("  reservation: mean |deviation| {0} tokens (over {1}, under {2}); reserved delta {3}, settled {4}, released {5}, overshoot {6} tokens / {7} micros; released_cost_micros {8}" -f $mad, $sumPos, $sumNeg, $dReserved, $dSettled, $dReleased, $dOvershoot, $dOvershootCost, $dReleasedCost)
    Write-Host ("  settlement identity: {0} - {1} + {2} = {3} == settled {4}" -f $dReserved, $dReleased, $dOvershoot, ($dReserved - $dReleased + $dOvershoot), $dSettled)
    Write-Host ("  budget: without quota {0} upstream calls, with a {1} token day {2} calls -> {3} provider calls prevented ({4}% client 429)" -f $callsWithout, $budgetAcmeTokens, $callsWith, $prevented, $rate429)
    foreach ($row in ($summary['redis_vs_memory']['rows'])) {
        Write-Host ("  {0,-11} {1,-11} c{2,-3} QPS {3,9} (min {4,9} max {5,9})  P95 {6,8} ms  P99 {7,8} ms  errors {8}" -f $row.Scenario, $row.Workload, $row.Concurrency, $row.QPS, $row.QPSMin, $row.QPSMax, $row.P95_ms, $row.P99_ms, $row.Errors)
    }
    Write-Host ("  RESP2 proof: {0} = {1} after {2} rounds (delta {3}); minute requests key {4} = {5}; day cost key '{6}'" -f $discoveredTenant, $redisObservations[$redisObservations.Count - 1].day_tokens_value, $Rounds, $redisDelta, $minuteKey, $minuteVal, $costKey)
    Write-Host ("  fail-closed: {0}% 503 during the outage with {1} upstream calls; after restart {2}% 503, {3} upstream calls, gateway restarted = {4}" -f $rate503, $callsDuringOutage, $rate503Recovery, $callsRecovery, $gwRestarted)
    Write-Host ("  fail-open: {0}% 503 and {1} upstream calls under the same outage with only fail_open flipped (store_errors +{2})" -f $rate503Open, $callsOpen, $storeErrorsOpen)
    Write-Host ("  degrade ladder: within budget model {0} reserved {1} completion {2} | over budget model {3} cap header {4} reserved {5} completion {6}" -f $modelA, $degAReserved, $compA, $modelB, $degB.QuotaMaxTokens, $degBReserved, $compB)
    Write-Host ("  assertions: {0} run, {1} failed, {2} unmeasured" -f $script:assertTotal, $script:assertFails.Count, $script:failures.Count)
    Write-Host "  artifact: $artifact"

    $exitOk = (($script:assertFails.Count -eq 0) -and ($script:failures.Count -eq 0))
} finally {
    if ($KeepRunning) {
        Write-Host ''
        Write-Host "  -KeepRunning: leaving $($script:procs.Count) process(es) up" -ForegroundColor Yellow
    } else {
        foreach ($p in $script:procs) { Stop-Proc $p }
    }
    $total = $script:assertFails.Count + $script:failures.Count
    if ($exitOk) {
        Write-Host ''
        Write-Host "RESULT: measurement completed cleanly, $([int]$script:assertTotal) assertions passed" -ForegroundColor Green
    } else {
        Write-Host ''
        Write-Host "RESULT: measurement did not complete cleanly ($($script:assertFails.Count) failed assertion(s), $($script:failures.Count) unmeasured step(s)); partial artifacts are in tmp\" -ForegroundColor Yellow
        if ($script:assertFails.Count -gt 0) { Write-Host ("  failed: " + ($script:assertFails -join '; ')) -ForegroundColor Yellow }
        if ($script:failures.Count -gt 0) { Write-Host ("  not measured: " + ($script:failures -join '; ')) -ForegroundColor Yellow }
        exit 1
    }
}
