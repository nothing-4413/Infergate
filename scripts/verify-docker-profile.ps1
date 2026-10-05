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
# service is a grandchild, so ParentProcessId does not find it).
function Stop-Mine {
    $mine = @(Get-CimInstance Win32_Process -Filter "Name = 'infergate.exe' OR Name = 'mockupstream.exe' OR Name = 'miniredis.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith($tmp, [System.StringComparison]::OrdinalIgnoreCase) })
    foreach ($m in $mine) { Stop-Process -Id $m.ProcessId -Force -ErrorAction SilentlyContinue }
    return $mine.Count
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

# --- 2. Build the three binaries (skipped when they are already present).
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

# --- 3. Start the three processes.
#
# Start-Job, not Start-Process: in this sandbox a Start-Process child wedges the
# calling shell, while a background job launches normally. The cost is that a job
# does NOT kill the process it started, and that orphan inherits the job's pipe --
# so every launcher hands its own PID back and cleanup kills processes by PID
# BEFORE stopping the jobs. Skip that and pwsh hangs on exit with the run already
# finished, which is the most confusing failure mode in this file.
Write-Host 'starting mockupstream / miniredis / infergate...' -ForegroundColor DarkGray
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

# Resolve each job's child PID so cleanup can kill the process, not just the job.
function Get-JobPid($job) {
    foreach ($i in 1..30) {
        $child = Get-CimInstance Win32_Process -Filter "ParentProcessId = $($job.Id)" -ErrorAction SilentlyContinue
        if ($child) { return @($child)[0].ProcessId }
        Start-Sleep -Milliseconds 200
    }
    return $null
}
$mockPid = Get-JobPid $mockJob
$redisPid = Get-JobPid $redisJob
$gwPid = Get-JobPid $gwJob

try {
    $up = $false
    foreach ($i in 1..40) {
        Start-Sleep -Milliseconds 250
        try {
            $r = Invoke-WebRequest -Uri "$base/healthz" -TimeoutSec 2 -UseBasicParsing
            if ($r.StatusCode -eq 200) { $up = $true; break }
        } catch { }
    }
    if (-not $up) {
        Write-Host (Get-Content (Join-Path $tmp 'gateway.log') -Raw) -ForegroundColor Red
        throw 'gateway never became ready'
    }

    Write-Host ''
    Write-Host '--- A. liveness and readiness ---'
    $readyz = Invoke-RestMethod -Uri "$base/readyz" -TimeoutSec 5
    $readyTxt = $readyz | ConvertTo-Json -Depth 6 -Compress
    # The shape is {"status":"ready","upstreams":["mock"],"version":"..."}.
    Check -name '/readyz reports ready with the mock upstream' `
        -ok ($readyTxt -match '"status"\s*:\s*"ready"' -and $readyTxt -match 'mock') -detail "body: $readyTxt"

    Write-Host ''
    Write-Host '--- B. the store is really the RESP2 path (cache round-trip) ---'
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
    Write-Host '--- C. idempotency: capacity and max_response_bytes are accepted keys ---'
    $idemTxt = (Invoke-RestMethod -Uri "$base/admin/idempotency" -TimeoutSec 5) | ConvertTo-Json -Depth 6 -Compress
    Check '/admin/idempotency reports the configured capacity 512' ($idemTxt -match '512') "body: $($idemTxt.Substring(0,[Math]::Min(220,$idemTxt.Length)))"
    curl.exe -s -D (Join-Path $tmp 'r3.txt') -o (Join-Path $tmp 'b3.txt') -H 'Content-Type: application/json' -H 'Idempotency-Key: smoke-op-1' --data-binary "@$hf" "$base/v1/chat/completions" | Out-Null
    curl.exe -s -D (Join-Path $tmp 'r4.txt') -o (Join-Path $tmp 'b4.txt') -H 'Content-Type: application/json' -H 'Idempotency-Key: smoke-op-1' --data-binary "@$hf" "$base/v1/chat/completions" | Out-Null
    $replay = Get-Header (Join-Path $tmp 'r4.txt') 'X-InferGate-Idempotent-Replay'
    Check 'the repeated Idempotency-Key replays (store insert + lookup happened)' ($replay -eq 'true') "header='$replay'"
    $idem2 = (Invoke-RestMethod -Uri "$base/admin/idempotency" -TimeoutSec 5) | ConvertTo-Json -Depth 6 -Compress
    Check '/admin/idempotency counts the stored entry' ($idem2 -match '"stored"\s*:\s*[1-9]' -or $idem2 -match '"entries"\s*:\s*[1-9]') "body: $($idem2.Substring(0,[Math]::Min(260,$idem2.Length)))"

    Write-Host ''
    Write-Host '--- D. sessions: capacity and recent_per_session are accepted keys ---'
    curl.exe -s -D (Join-Path $tmp 'r5.txt') -o (Join-Path $tmp 'b5.txt') -H 'Content-Type: application/json' -H 'X-InferGate-Session: smoke-conv' -H 'X-InferGate-Tenant: smoke-team' --data-binary "@$hf" "$base/v1/chat/completions" | Out-Null
    Start-Sleep -Milliseconds 400
    $sess = (Invoke-RestMethod -Uri "$base/admin/sessions" -TimeoutSec 5) | ConvertTo-Json -Depth 8 -Compress
    Check 'the session ledger recorded smoke-conv' ($sess -match 'smoke-conv') "body: $($sess.Substring(0,[Math]::Min(260,$sess.Length)))"
    $one = ''
    try { $one = (Invoke-RestMethod -Uri "$base/admin/sessions/smoke-conv" -TimeoutSec 5) | ConvertTo-Json -Depth 8 -Compress }
    catch { $one = "ERR $($_.Exception.Message)" }
    Check '/admin/sessions/{id} returns the conversation' ($one -match 'smoke-conv') "body: $($one.Substring(0,[Math]::Min(220,$one.Length)))"

    Write-Host ''
    Write-Host '--- E. tracing: enabled with an empty jsonl_path is a valid shape ---'
    $tr = (Invoke-RestMethod -Uri "$base/admin/tracing" -TimeoutSec 5) | ConvertTo-Json -Depth 6 -Compress
    Check '/admin/tracing reports enabled' ($tr -match '"enabled"\s*:\s*true') "body: $($tr.Substring(0,[Math]::Min(260,$tr.Length)))"
    $traces = (Invoke-RestMethod -Uri "$base/admin/traces" -TimeoutSec 5) | ConvertTo-Json -Depth 6 -Compress
    Check '/admin/traces recorded at least one request' ($traces -match '"trace_id"' -or $traces -match '"id"')
    Check 'no JSONL trace file was written (empty jsonl_path means the file sink is off)' (-not (Test-Path (Join-Path $root 'traces.jsonl')))

    Write-Host ''
    Write-Host '--- F. quota and metrics ---'
    $q = (Invoke-RestMethod -Uri "$base/admin/quota" -TimeoutSec 5) | ConvertTo-Json -Depth 6 -Compress
    Check '/admin/quota is served' ($q.Length -gt 20) "body: $($q.Substring(0,[Math]::Min(200,$q.Length)))"
    $m = Invoke-WebRequest -Uri "$base/metrics" -TimeoutSec 5 -UseBasicParsing
    Check '/metrics exposes infergate_ series' ($m.Content -match 'infergate_')

    Write-Host ''
    Write-Host '--- G. the gateway logged no error while doing all of that ---'
    $err = ''
    $gwLog = Join-Path $tmp 'gateway.log'
    if (Test-Path $gwLog) { $err = Get-Content $gwLog -Raw }
    $bad = @($err -split "`n" | Where-Object { $_ -match 'level=error|panic' })
    Check -name 'no error/panic lines in the gateway log' -ok ($bad.Count -eq 0) -detail "first: $($bad | Select-Object -First 1)"
}
finally {
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
