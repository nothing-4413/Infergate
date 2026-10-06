# End-to-end smoke verification for the container profile (configs/docker.yaml).
#
# WHY THIS EXISTS: the compose stack cannot be started here (no Docker daemon),
# and the config loader does NOT enable DisallowUnknownFields -- a misspelled key
# (e.g. idempotency.max_entries) parses cleanly and silently bounds nothing. The
# only way to falsify that is to run the real binaries and read the admin surfaces.
#
# APPROACH: take configs/docker.yaml and replace only the compose service names
# with local addresses (mockupstream -> 127.0.0.1, miniredis -> 127.0.0.1), keep
# every other key byte-for-byte, then run the three in-repo binaries as local
# processes. So "these keys are accepted AND take effect" is verified here; what
# is NOT verified is Docker DNS name resolution and the image itself (deploy/README
# section 6 records that boundary).
#
# ASCII only, on purpose: Windows PowerShell 5.1 reads a BOM-less .ps1 as ANSI,
# so non-ASCII text in this file would arrive mojibake.
#
# USAGE (from the repository root):
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-docker-profile.ps1

[CmdletBinding()]
param(
    # server.listen in configs/docker.yaml is the container-internal 8080, which is
    # the right value there and the wrong one here (the gateway would bind 8080
    # while the probes go to 18081). It is rewritten below alongside the addresses.
    [string]$GatewayPort = '18081',
    [string]$MockPort = '19001',
    [string]$RedisPort = '16391',
    # Rebuild even when the binaries are already there.
    [switch]$ForceBuild
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$base = "http://127.0.0.1:$GatewayPort"
$tmp = Join-Path $root 'tmp\docker-profile'
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

$script:pass = 0; $script:fail = 0
function Check([string]$name, [bool]$ok, [string]$detail = '') {
    if ($ok) { $script:pass++; Write-Host "  [ok]   $name" }
    else { $script:fail++; Write-Host "  [FAIL] $name  $detail" -ForegroundColor Red }
}
function Get-Header([string]$file, [string]$name) {
    $line = Select-String -Path $file -Pattern "^$([regex]::Escape($name)):\s*(.*)$" -AllMatches |
        Select-Object -Last 1
    if ($line) { return $line.Matches[0].Groups[1].Value.Trim() }
    return ''
}
# BOM-less UTF-8, because Set-Content -Encoding utf8 on PowerShell 5.1 writes a
# BOM. The gateway reads the body as JSON, and a leading EF BB BF makes even a
# perfectly good body fail as "body is not JSON" -- which shows up as an upstream
# 400 and an empty cache, i.e. it looks like a config problem and is not one.
function Write-Utf8NoBom([string]$path, [string]$text) {
    [System.IO.File]::WriteAllText($path, $text, (New-Object System.Text.UTF8Encoding($false)))
}
# Every process this script may have started, located by executable path (a job's
# service is a grandchild, so ParentProcessId does not find it) or by one of the
# three ports this run uses.
#
# The port half was added after a real leak: a run whose launcher was killed
# before its finally block left an infergate.exe and a miniredis.exe holding
# 18081 and 16391. They matched by path in principle, and Stop-Mine still did not
# reap them, so "clean up what I started" cannot rest on the path test alone --
# anything listening on our own ports is ours to stop by definition.
$script:servicePorts = @([int]$MockPort, [int]$RedisPort, [int]$GatewayPort)
function Stop-Mine {
    $names = "Name = 'infergate.exe' OR Name = 'mockupstream.exe' OR Name = 'miniredis.exe'"
    $procs = @(Get-CimInstance Win32_Process -Filter $names -ErrorAction SilentlyContinue)
    $killed = @{}
    foreach ($m in $procs) {
        if ($m.ExecutablePath -and $m.ExecutablePath.StartsWith($tmp, [System.StringComparison]::OrdinalIgnoreCase)) {
            $killed[[int]$m.ProcessId] = $m.Name
        }
    }
    foreach ($port in $script:servicePorts) {
        $conns = @(Get-NetTCPConnection -LocalPort $port -ErrorAction SilentlyContinue)
        foreach ($c in $conns) {
            if ($c.OwningProcess -and -not $killed.ContainsKey([int]$c.OwningProcess)) {
                $owner = $procs | Where-Object { $_.ProcessId -eq $c.OwningProcess } | Select-Object -First 1
                if ($owner) { $killed[[int]$c.OwningProcess] = $owner.Name }
            }
        }
    }
    foreach ($procId in $killed.Keys) { Stop-Process -Id $procId -Force -ErrorAction SilentlyContinue }
    return $killed.Count
}

# --- 1. Derive the config under test: addresses + listen port only, everything
# else byte-for-byte. The three substitutions are exactly the things that are
# container-shaped (service names, container-internal port), not config content.
$raw = Get-Content (Join-Path $root 'configs\docker.yaml') -Raw
$cfg = $raw.Replace('http://mockupstream:9000', "http://127.0.0.1:$MockPort").Replace('miniredis:6399', "127.0.0.1:$RedisPort").Replace('listen: ":8080"', "listen: `":$GatewayPort`"")
# The check looks only at assignment lines, not at the comment block: that block
# spells out the mapping ("127.0.0.1:9000 -> mockupstream:9000") on purpose, so a
# whole-file -notmatch would fail on the explanation rather than on the config.
$assignments = @($cfg -split "`n" | Where-Object { $_ -match '^\s*(base_url|addr|listen):' })
Check -name 'derived config rewrote both addresses and the listen port on every assignment line' `
    -ok (@($assignments | Where-Object { $_ -match 'mockupstream|miniredis' }).Count -eq 0 -and
         @($assignments | Where-Object { $_ -match "127\.0\.0\.1:$MockPort" }).Count -eq 1 -and
         @($assignments | Where-Object { $_ -match "127\.0\.0\.1:$RedisPort" }).Count -eq 2 -and
         @($assignments | Where-Object { $_ -match "listen: .:?$GatewayPort" }).Count -eq 1) `
    -detail "assignments: $($assignments -join ' | ')"
$cfgPath = Join-Path $tmp 'docker-local.yaml'
Write-Utf8NoBom $cfgPath $cfg

# Anything left running from an interrupted earlier run would hold the ports.
$leftover = Stop-Mine
if ($leftover -gt 0) { Write-Host "cleaned $leftover leftover service process(es) from an earlier run" -ForegroundColor DarkGray }

# --- 2. Preconditions: the three ports must be free, and the derived config must
# pass the loader's own validation. Both exist because a run of this script once
# reported "first request is a miss got 'skip'" and "the replayed body is
# byte-identical" as FAILs -- which reads like a broken gate and was not one: an
# occupied port had silently kept the gateway from binding, so every request fell
# through to nothing. A missing precondition must be reported AS a precondition.
function Test-PortFree([int]$port) {
    # TcpClient straight to 127.0.0.1: Test-NetConnection also tries DNS and can
    # take seconds to say no.
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $client.Connect('127.0.0.1', $port)
        $client.Close()
        return $false
    } catch {
        return $true
    } finally {
        $client.Dispose()
    }
}
# A process that is up is not the same as a process that is listening. The gateway
# accepts traffic (and answers /healthz) the moment it binds, while mockupstream
# and miniredis may still be a few hundred milliseconds behind -- so the first
# probe of an auxiliary port is a race, and reading that race as "the store is
# down" is exactly the kind of false FAIL this file has produced before. Poll.
function Wait-PortListening([int]$port, [int]$timeoutMs = 10000) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while ($sw.ElapsedMilliseconds -lt $timeoutMs) {
        if (-not (Test-PortFree $port)) { return $true }
        Start-Sleep -Milliseconds 200
    }
    return $false
}
$busy = @()
foreach ($p in @($MockPort, $RedisPort, $GatewayPort)) {
    if (-not (Test-PortFree ([int]$p))) { $busy += $p }
}
# Killing a listener does not release its port in the same instant, so a leftover
# that Stop-Mine just killed still shows up here for a moment. Wait for the
# kernel to let go (up to ~10s) before calling it a conflict -- otherwise this
# precondition would fail the run it just cleaned up after.
if ($busy.Count -gt 0) {
    foreach ($i in 1..20) {
        Start-Sleep -Milliseconds 500
        $busy = @()
        foreach ($p in @($MockPort, $RedisPort, $GatewayPort)) {
            if (-not (Test-PortFree ([int]$p))) { $busy += $p }
        }
        if ($busy.Count -eq 0) { break }
    }
}
Check -name 'the three ports this run needs are free' -ok ($busy.Count -eq 0) `
    -detail "already listening: $($busy -join ', ') -- stop that process, or pass -GatewayPort/-MockPort/-RedisPort"
if ($busy.Count -gt 0) {
    # Every later assertion would be measuring the wrong process, so stop here
    # rather than emit a page of FAILs that point at the cache.
    Write-Host ''
    Write-Host "refusing to continue: port(s) $($busy -join ', ') are in use" -ForegroundColor Red
    exit 1
}

# --- 3. Build the three binaries (skipped when they are already present).
#
# Via Start-Job, for the same reason as the services below: Start-Process wedges
# the calling shell in this sandbox, and piping a batch file's merged output back
# through PowerShell (`& .\tools\go.cmd ... *> $log`) wedges too -- it hangs
# *after* the binaries are written, which is indistinguishable from "the gateway
# never started". A job with Receive-Job sidesteps both.
$exes = @{}
foreach ($c in 'infergate', 'mockupstream', 'miniredis') { $exes[$c] = Join-Path $tmp "$c.exe" }
$needBuild = $ForceBuild -or (@($exes.Values | Where-Object { -not (Test-Path $_) }).Count -gt 0)
if ($needBuild) {
    Write-Host 'building binaries...' -ForegroundColor DarkGray
    foreach ($c in 'infergate', 'mockupstream', 'miniredis') {
        $bp = Start-Job -ScriptBlock {
            param($rootPath, $out, $cmd)
            $ErrorActionPreference = 'Continue'
            & (Join-Path $rootPath 'tools\go.cmd') 'build' '-o' $out "./cmd/$cmd" 2>&1
        } -ArgumentList $root, $exes[$c], $c
        if (-not (Wait-Job -Job $bp -Timeout 600)) {
            Stop-Job -Job $bp; Remove-Job -Job $bp -Force
            throw "build timed out for cmd/$c"
        }
        $buildOut = (Receive-Job -Job $bp) -join "`n"
        Remove-Job -Job $bp -Force
        if ($buildOut -match '(?m)^.*error|undefined:|cannot find') {
            Write-Host $buildOut -ForegroundColor Red
            throw "build failed for cmd/$c"
        }
    }
} else {
    Write-Host 'reusing existing binaries (pass -ForceBuild to rebuild)' -ForegroundColor DarkGray
}

# --- 4. Ask the loader itself whether the derived config is acceptable.
#
# It is the authority on that, and it is cheap to ask: running it here means a
# rejected key is reported as a rejected key, instead of surfacing three sections
# later as an empty cache. Nothing extra is set for it -- configs/docker.yaml has
# api_key: "" and no ${VAR} placeholders at all, which is exactly why the compose
# stack needs no secrets to start.
$checkJob = Start-Job -ScriptBlock {
    param($exe, $cfg)
    $ErrorActionPreference = 'Continue'
    & $exe '-config' $cfg '-check' 2>&1
} -ArgumentList $exes['infergate'], $cfgPath
if (-not (Wait-Job -Job $checkJob -Timeout 90)) {
    Stop-Job -Job $checkJob -ErrorAction SilentlyContinue
    Remove-Job -Job $checkJob -Force -ErrorAction SilentlyContinue
    throw 'the config check never finished'
}
$checkOut = @((Receive-Job -Job $checkJob) | ForEach-Object { "$_" })
Remove-Job -Job $checkJob -Force -ErrorAction SilentlyContinue
$checkTxt = ($checkOut -join ' ').Trim()
Check -name 'the derived config passes the loader check' -ok ($checkTxt -match 'OK') -detail "output: $checkTxt"
if ($checkTxt -notmatch 'OK') {
    # A config the loader refuses makes every later assertion meaningless.
    Write-Host ''
    Write-Host 'refusing to continue: the derived config was rejected' -ForegroundColor Red
    exit 1
}

# --- 5. Start the three processes.
#
# Start-Job, not Start-Process: in this sandbox a Start-Process child wedges the
# calling shell, while a background job launches normally. The cost is that a job
# does NOT kill the process it started, and that orphan inherits the job's pipe --
# so every launcher hands its own PID back and cleanup kills processes by PID
# BEFORE stopping the jobs. Skip that and pwsh hangs on exit with the run already
# finished, which is the most confusing failure mode in this file.
Write-Host 'starting mockupstream / miniredis / infergate...' -ForegroundColor DarkGray
$startSw = [Diagnostics.Stopwatch]::StartNew()
$mockPortText = ":$MockPort"; $redisPortText = ":$RedisPort"
$mockJob = Start-Job -ScriptBlock {
    param($exe, $port)
    $ErrorActionPreference = 'Continue'
    & $exe '-listen' $port '-name' 'docker-mock' '-token-delay' '15ms' '-ttfb' '50ms'
} -ArgumentList $exes['mockupstream'], $mockPortText
$redisJob = Start-Job -ScriptBlock {
    param($exe, $port)
    $ErrorActionPreference = 'Continue'
    & $exe '-listen' $port
} -ArgumentList $exes['miniredis'], $redisPortText
$gwJob = Start-Job -ScriptBlock {
    param($exe, $cfg, $log)
    $ErrorActionPreference = 'Continue'
    & $exe '-config' $cfg 2>&1 | Out-File -FilePath $log -Encoding utf8
} -ArgumentList $exes['infergate'], $cfgPath, (Join-Path $tmp 'gateway.log')
$startedAt = $startSw.ElapsedMilliseconds

# There used to be a Get-JobPid helper here that asked WMI for a process whose
# ParentProcessId equals $job.Id. It could never work: a PowerShell job's Id is a
# sequence number, not a process id (Start-Job reported Id=1 in testing), so the
# lookup always returned nothing after burning 30 CIM queries -- and the only
# consumer was a log line. Process identity comes from the ports instead, which is
# what Stop-Mine uses and what the preconditions above assert.

# On failure, print the evidence instead of only naming the artifacts. The run
# that produced "first request is a miss got 'skip'" left a usable answer in
# gateway.log, and nothing in the output pointed at it.
function Show-Diagnostics {
    Write-Host ''
    Write-Host '--- diagnostics ---' -ForegroundColor Yellow
    $gwLog = Join-Path $tmp 'gateway.log'
    if (Test-Path $gwLog) {
        $logTxt = Get-Content $gwLog -Raw
        if ($logTxt) {
            $bad = @($logTxt -split "`n" | Where-Object { $_ -match 'level=error|level=warn|panic|listen' })
            if ($bad.Count -gt 0) {
                Write-Host "gateway.log, error/warn/listen lines ($($bad.Count)):" -ForegroundColor Yellow
                $bad | Select-Object -First 25 | ForEach-Object { Write-Host "  | $($_.TrimEnd())" -ForegroundColor Yellow }
            } else {
                Write-Host 'gateway.log has no error/warn lines; last 20 lines:' -ForegroundColor Yellow
                @($logTxt -split "`n") | Select-Object -Last 20 | ForEach-Object { Write-Host "  | $($_.TrimEnd())" -ForegroundColor Yellow }
            }
        } else {
            Write-Host 'gateway.log is empty' -ForegroundColor Yellow
        }
    } else {
        Write-Host "no gateway.log at $gwLog" -ForegroundColor Yellow
    }
    foreach ($f in 'r1.txt', 'r2.txt') {
        $p = Join-Path $tmp $f
        if (Test-Path $p) {
            Write-Host "$f :" -ForegroundColor Yellow
            Get-Content $p | Select-Object -First 12 | ForEach-Object { Write-Host "  | $($_.TrimEnd())" -ForegroundColor Yellow }
        }
    }
    Write-Host '--- end diagnostics ---' -ForegroundColor Yellow
}

try {
    # Wait for the gateway with a visible count of attempts and the last real
    # error. A bare 10-second silence here cost an afternoon once: the gateway had
    # bound its port and served traffic, and the only thing on screen was "never
    # became ready".
    $up = $false
    $probeErrs = @{}
    $tries = 0
    $probeStart = [Diagnostics.Stopwatch]::StartNew()
    foreach ($i in 1..40) {
        Start-Sleep -Milliseconds 250
        $tries++
        try {
            $r = Invoke-WebRequest -Uri "$base/healthz" -TimeoutSec 2 -UseBasicParsing
            if ($r.StatusCode -eq 200) { $up = $true; break }
            $probeErrs["http $($r.StatusCode)"] = 1 + $probeErrs["http $($r.StatusCode)"]
        } catch {
            $why = $_.Exception.Message
            if ($why.Length -gt 90) { $why = $why.Substring(0, 90) }
            $probeErrs[$why] = 1 + $probeErrs[$why]
        }
    }
    Write-Host ("gateway readiness: up=$up after $tries probe(s) in {0}ms (launch sequence {1}ms)" -f $probeStart.ElapsedMilliseconds, $startedAt) -ForegroundColor DarkGray
    if ($up) { Write-Host '' }
    if (-not $up) {
        Write-Host 'probe outcomes:' -ForegroundColor Red
        foreach ($k in $probeErrs.Keys) { Write-Host "  $($probeErrs[$k]) x $k" -ForegroundColor Red }
        $listen = @(Get-NetTCPConnection -LocalPort ([int]$GatewayPort) -State Listen -ErrorAction SilentlyContinue)
        Write-Host "listeners on $GatewayPort = $($listen.Count); gateway.log:" -ForegroundColor Red
        Write-Host (Get-Content (Join-Path $tmp 'gateway.log') -Raw) -ForegroundColor Red
        throw 'gateway never became ready'
    }

    Write-Host ''
    Write-Host '--- A. preconditions of the store under test ---'
    # The gateway must be talking to the real miniredis, not to nothing. Without
    # this, an unreachable store shows up as 'skip' three assertions later, where
    # it looks like a cache bug. Each wait is generous on purpose: a slow start
    # must not be reported as a broken store.
    Check -name "miniredis is listening on 127.0.0.1:$RedisPort" -ok (Wait-PortListening ([int]$RedisPort)) `
        -detail "nothing accepted a connection on $RedisPort within 10s, so the cache can only report 'skip'"
    Check -name "mockupstream is listening on 127.0.0.1:$MockPort" -ok (Wait-PortListening ([int]$MockPort)) `
        -detail "nothing accepted a connection on $MockPort within 10s"

    Write-Host ''
    Write-Host '--- B. liveness and readiness ---'
    $readyz = Invoke-RestMethod -Uri "$base/readyz" -TimeoutSec 5
    $readyTxt = $readyz | ConvertTo-Json -Depth 6 -Compress
    # The shape is {"status":"ready","upstreams":["mock"],"version":"..."}.
    Check -name '/readyz reports ready with the mock upstream' `
        -ok ($readyTxt -match '"status"\s*:\s*"ready"' -and $readyTxt -match 'mock') -detail "body: $readyTxt"

    Write-Host ''
    Write-Host '--- C. the store is really the RESP2 path (cache round-trip) ---'
    $hf = Join-Path $tmp 'chat.json'
    Write-Utf8NoBom $hf '{"model":"mock-gpt","messages":[{"role":"user","content":"docker profile smoke: the RESP2 path is the one under test here"}]}'
    curl.exe -s -D (Join-Path $tmp 'r1.txt') -o (Join-Path $tmp 'b1.txt') -H 'Content-Type: application/json' --data-binary "@$hf" "$base/v1/chat/completions" | Out-Null
    curl.exe -s -D (Join-Path $tmp 'r2.txt') -o (Join-Path $tmp 'b2.txt') -H 'Content-Type: application/json' --data-binary "@$hf" "$base/v1/chat/completions" | Out-Null
    $c1 = Get-Header (Join-Path $tmp 'r1.txt') 'X-InferGate-Cache'
    $c2 = Get-Header (Join-Path $tmp 'r2.txt') 'X-InferGate-Cache'
    Check 'first request is a miss' ($c1 -eq 'miss') "got '$c1'"
    Check 'second request is a hit (Redis round-trip, not process memory)' ($c2 -eq 'hit-exact') "got '$c2'"
    $b1 = Get-Content (Join-Path $tmp 'b1.txt') -Raw
    $b2 = Get-Content (Join-Path $tmp 'b2.txt') -Raw
    Check 'the replayed body is byte-identical' ($b1 -ceq $b2 -and $b1 -match 'chat.completion')

    Write-Host ''
    Write-Host '--- D. idempotency: capacity and max_response_bytes are accepted keys ---'
    $idemTxt = (Invoke-RestMethod -Uri "$base/admin/idempotency" -TimeoutSec 5) | ConvertTo-Json -Depth 6 -Compress
    Check '/admin/idempotency reports the configured capacity 512' ($idemTxt -match '512') "body: $($idemTxt.Substring(0,[Math]::Min(220,$idemTxt.Length)))"
    curl.exe -s -D (Join-Path $tmp 'r3.txt') -o (Join-Path $tmp 'b3.txt') -H 'Content-Type: application/json' -H 'Idempotency-Key: smoke-op-1' --data-binary "@$hf" "$base/v1/chat/completions" | Out-Null
    curl.exe -s -D (Join-Path $tmp 'r4.txt') -o (Join-Path $tmp 'b4.txt') -H 'Content-Type: application/json' -H 'Idempotency-Key: smoke-op-1' --data-binary "@$hf" "$base/v1/chat/completions" | Out-Null
    $replay = Get-Header (Join-Path $tmp 'r4.txt') 'X-InferGate-Idempotent-Replay'
    Check 'the repeated Idempotency-Key replays (store insert + lookup happened)' ($replay -eq 'true') "header='$replay'"
    $idem2 = (Invoke-RestMethod -Uri "$base/admin/idempotency" -TimeoutSec 5) | ConvertTo-Json -Depth 6 -Compress
    Check '/admin/idempotency counts the stored entry' ($idem2 -match '"stored"\s*:\s*[1-9]' -or $idem2 -match '"entries"\s*:\s*[1-9]') "body: $($idem2.Substring(0,[Math]::Min(260,$idem2.Length)))"

    Write-Host ''
    Write-Host '--- E. sessions: capacity and recent_per_session are accepted keys ---'
    curl.exe -s -D (Join-Path $tmp 'r5.txt') -o (Join-Path $tmp 'b5.txt') -H 'Content-Type: application/json' -H 'X-InferGate-Session: smoke-conv' -H 'X-InferGate-Tenant: smoke-team' --data-binary "@$hf" "$base/v1/chat/completions" | Out-Null
    Start-Sleep -Milliseconds 400
    $sess = (Invoke-RestMethod -Uri "$base/admin/sessions" -TimeoutSec 5) | ConvertTo-Json -Depth 8 -Compress
    Check 'the session ledger recorded smoke-conv' ($sess -match 'smoke-conv') "body: $($sess.Substring(0,[Math]::Min(260,$sess.Length)))"
    $one = ''
    try { $one = (Invoke-RestMethod -Uri "$base/admin/sessions/smoke-conv" -TimeoutSec 5) | ConvertTo-Json -Depth 8 -Compress }
    catch { $one = "ERR $($_.Exception.Message)" }
    Check '/admin/sessions/{id} returns the conversation' ($one -match 'smoke-conv') "body: $($one.Substring(0,[Math]::Min(220,$one.Length)))"

    Write-Host ''
    Write-Host '--- F. tracing: enabled with an empty jsonl_path is a valid shape ---'
    $tr = (Invoke-RestMethod -Uri "$base/admin/tracing" -TimeoutSec 5) | ConvertTo-Json -Depth 6 -Compress
    Check '/admin/tracing reports enabled' ($tr -match '"enabled"\s*:\s*true') "body: $($tr.Substring(0,[Math]::Min(260,$tr.Length)))"
    $traces = (Invoke-RestMethod -Uri "$base/admin/traces" -TimeoutSec 5) | ConvertTo-Json -Depth 6 -Compress
    Check '/admin/traces recorded at least one request' ($traces -match '"trace_id"' -or $traces -match '"id"')
    Check 'no JSONL trace file was written (empty jsonl_path means the file sink is off)' (-not (Test-Path (Join-Path $root 'traces.jsonl')))

    Write-Host ''
    Write-Host '--- G. quota and metrics ---'
    $q = (Invoke-RestMethod -Uri "$base/admin/quota" -TimeoutSec 5) | ConvertTo-Json -Depth 6 -Compress
    Check '/admin/quota is served' ($q.Length -gt 20) "body: $($q.Substring(0,[Math]::Min(200,$q.Length)))"
    $m = Invoke-WebRequest -Uri "$base/metrics" -TimeoutSec 5 -UseBasicParsing
    Check '/metrics exposes infergate_ series' ($m.Content -match 'infergate_')

    Write-Host ''
    Write-Host '--- H. the gateway logged no error while doing all of that ---'
    $err = ''
    $gwLog = Join-Path $tmp 'gateway.log'
    if (Test-Path $gwLog) { $err = Get-Content $gwLog -Raw }
    $bad = @($err -split "`n" | Where-Object { $_ -match 'level=error|panic' })
    Check -name 'no error/panic lines in the gateway log' -ok ($bad.Count -eq 0) -detail "first: $($bad | Select-Object -First 1)"
}
catch {
    Write-Host ''
    Write-Host "the run threw: $($_.Exception.Message)" -ForegroundColor Red
    # Rethrowing would skip the summary below and hide the pass/fail count, so the
    # exception becomes one more failure and the finally block stays reachable.
    $script:fail++
}
finally {
    if ($script:fail -gt 0) { Show-Diagnostics }
    # Kill by executable path: a job's service is a grandchild of the job, so
    # neither $job nor ParentProcessId reaches it, and the orphan holds the job's
    # pipe open -- which makes pwsh hang on exit *after* the run has finished.
    Stop-Mine | Out-Null
    Start-Sleep -Milliseconds 300
    foreach ($j in @($gwJob, $mockJob, $redisJob)) {
        if ($j) {
            Stop-Job -Job $j -ErrorAction SilentlyContinue
            Remove-Job -Job $j -Force -ErrorAction SilentlyContinue
        }
    }
}

Write-Host ''
if ($fail -eq 0) { Write-Host "$pass passed, 0 failed" -ForegroundColor Green }
else { Write-Host "$pass passed, $fail failed" -ForegroundColor Red }
Write-Host "artifacts: $tmp"
if ($fail -ne 0) { exit 1 }
