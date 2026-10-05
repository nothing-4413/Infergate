# InferGate M4 -- measure TIERED local/cloud routing against a REAL local vLLM.
#
# M4's claim is economic: a request the local box can serve never pays cloud
# prices, and a request it cannot serve is not silently mis-routed to it. What
# this script produces is the evidence for that claim -- and it deliberately
# produces it against the real thing, not a mock:
#
#   quantization   a real vLLM in WSL serves Qwen2.5-1.5B-Instruct at fp16, AWQ
#                  and GPTQ-Int4, one at a time, and measures TTFT / total latency
#                  / throughput / VRAM load and unload per variant.
#   tiering        the SAME real vLLM becomes the gateway's `local` tier while
#                  cmd\mockupstream stands in for the cloud tier. A known mix of
#                  simple and hard requests goes through the gateway and the
#                  per-upstream split in /stats is compared against what was sent.
#   cost           derived, because there is no cost metric: pricing is by MODEL
#                  NAME, so a request served locally is priced at that model's
#                  list price -- and that price is exactly the cloud spend the
#                  local tier displaced. The convention is stated wherever the
#                  number appears.
#
# What is NOT claimed, and is written into the artifact's limitations:
#   * "agreement" between quantizations is DRIFT from the fp16 reference text,
#     not accuracy -- nothing here checks whether an answer is correct.
#   * the cloud tier is cmd\mockupstream, an instant in-repo stand-in, so the
#     cross-tier LATENCY difference is not a comparison with any real cloud.
#   * nvidia-smi reports TOTAL VRAM in use, and the Windows desktop holds ~1.8
#     GiB of this laptop's 8 GiB before any model loads; the model's footprint is
#     the DELTA, whose noise floor is ~50 MiB.
#
# Usage (pwsh does not exist on this host):
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m4.ps1
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m4.ps1 -SkipBench
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m4.ps1 -Variant awq -Requests 8
#
# Everything is started and stopped inside ONE powershell invocation: the WSL
# vLLM is killed strictly by the pid this script started (its process tree is
# walked) and only after /v1/models has stopped answering.

[CmdletBinding()]
param(
    [int]$GatewayPort = 18410,
    [int]$CloudPort = 19620,
    [int]$VllmPort = 8000,
    [ValidateSet('awq', 'gptq', 'fp16', 'auto')]
    [string]$Variant = 'auto',
    [int]$Requests = 8,
    [int]$AwqWaitSeconds = 0,
    [switch]$SkipBench,
    [switch]$SkipTiering,
    [switch]$KeepRunning,
    # Debug aid: leave tmp\m4-measure-* files on disk instead of deleting them,
    # so a failed start script can be read after the fact.
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
$baseConfig = Join-Path $repo 'configs\tiered-local.yaml'
$strictConfig = Join-Path $repo 'configs\tiered.yaml'
$artifact = Join-Path $baselineDir 'm4-summary.json'

foreach ($d in @($binDir, $tmpDir, $baselineDir)) {
    if (-not (Test-Path -LiteralPath $d)) { New-Item -ItemType Directory -Path $d -Force | Out-Null }
}

$stamp = "$(Get-Date -Format 'yyyyMMdd-HHmmss')-$PID"
$gwExe = Join-Path $binDir 'infergate.exe'
$mockExe = Join-Path $binDir 'mockupstream.exe'
$configPath = Join-Path $tmpDir "m4-measure-$stamp.yaml"
$gwLog = Join-Path $tmpDir "m4-measure-gateway-$stamp.log"
$gwErr = Join-Path $tmpDir "m4-measure-gateway-$stamp.err"
$mockLog = Join-Path $tmpDir "m4-measure-mock-$stamp.log"
$mockErr = Join-Path $tmpDir "m4-measure-mock-$stamp.err"
$buildLog = Join-Path $tmpDir "m4-measure-build-$stamp.log"
$checkOut = Join-Path $tmpDir "m4-measure-check-$stamp.out"
$checkErr = Join-Path $tmpDir "m4-measure-check-$stamp.err"
$vllmLog = Join-Path $tmpDir "m4-measure-vllm.log"
$startSh = Join-Path $tmpDir "m4-measure-vllm-start.sh"
$waitSh = Join-Path $tmpDir "m4-measure-awq-wait.sh"
$vllmPidFile = Join-Path $tmpDir "m4-measure-vllm.pid"
$benchSh = Join-Path $tmpDir "m4-measure-bench.sh"

$gwUrl = "http://127.0.0.1:$GatewayPort"
$wslDistro = 'Ubuntu24'
$wslRoot = '/mnt/c/Users/20106/Desktop/infergate'
$localVariant = ''
$localVariantReason = ''
$runnerVariant = ''
$vllmPid = $null

$script:gwProc = $null
$script:mockProc = $null
$script:startedPids = @()
$script:upstreamStarted = 0
$script:utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$script:assertFails = New-Object System.Collections.ArrayList
$script:failures = New-Object System.Collections.ArrayList
$script:assertTotal = 0
$script:log = New-Object System.Collections.ArrayList
$script:comparisons = New-Object System.Collections.ArrayList

$startedAt = Get-Date

# ---------------------------------------------------------------------------
# Console + assertion plumbing
# ---------------------------------------------------------------------------

function Write-Section {
    param([string]$Title)
    Write-Host ''
    Write-Host ('=' * 74) -ForegroundColor DarkGray
    Write-Host "  $Title" -ForegroundColor Cyan
    Write-Host ('=' * 74) -ForegroundColor DarkGray
}

function Log-Line {
    # Everything printed here also goes into the run log array, which is written
    # into the artifact: a number with no derivation is not evidence.
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

function Assert-Contains {
    param([string]$Haystack, [string]$Needle, [string]$Name)
    # Literal substring, never -like: a needle containing brackets becomes a
    # character class under wildcard matching and fails against text that
    # demonstrably contains it.
    $detail = "expected to find: $Needle"
    if ($Haystack) { $detail += "`n        actual: $($Haystack.Substring(0, [Math]::Min(300, $Haystack.Length)))" }
    Assert-That ($null -ne $Haystack -and $Haystack.Contains($Needle)) $Name $detail
}

function Add-Failure {
    param([string]$Text)
    [void]$script:failures.Add($Text)
    Write-Host "  UNMEASURED  $Text" -ForegroundColor Yellow
    [void]$script:log.Add("UNMEASURED: $Text")
}

# ---------------------------------------------------------------------------
# File helpers -- every log read goes through FileShare.ReadWrite
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
    # Last N lines of a possibly huge, possibly still-growing log. Read through
    # Read-Text so the FileShare.ReadWrite contract is kept.
    param([string]$Path, [int]$Lines = 20)
    $all = Read-Text $Path
    if ([string]::IsNullOrWhiteSpace($all)) { return '' }
    $split = @($all -split "`r?`n" | Where-Object { $_ -match '\S' })
    if ($split.Count -le $Lines) { return ($split -join "`n") }
    return (($split[($split.Count - $Lines)..($split.Count - 1)]) -join "`n")
}

function Write-NoBom {
    # A script that WSL is about to execute must not carry a BOM and must not
    # carry CRLF: bash reports "bad interpreter" for the first and the trailing
    # CR for the second. Files come from here, so the conversion happens HERE,
    # on the Windows side -- never through `wsl bash -lc "sed ..."`, where the
    # backslash is eaten and the substitution destroys every trailing 'r'.
    param([string]$Path, [string]$Text)
    [System.IO.File]::WriteAllText($Path, $Text, $script:utf8NoBom)
}

function Write-JsonFile {
    param([string]$Path, $Value)
    $json = ($Value | ConvertTo-Json -Depth 15)
    [System.IO.File]::WriteAllText($Path, $json + "`n", $script:utf8NoBom)
}

function Get-Json {
    param([string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text)) { return $null }
    try { return ($Text | ConvertFrom-Json) } catch { return $null }
}

function Get-LineByPrefix {
    # Probe scripts print one `key=value ...` line per item, so a caller can pull
    # exactly its own line out of a merged multi-line answer without a regex over
    # the whole blob.
    param([string]$Text, [string]$Prefix)
    if ([string]::IsNullOrEmpty($Text)) { return '' }
    foreach ($line in ($Text -split "`r?`n")) {
        if ($line.StartsWith($Prefix)) { return $line.Trim() }
    }
    return ''
}

function Get-KeyValueText {
    # `torch=2.8.0+cu128` -> '2.8.0+cu128'. The value is taken from the first
    # '=' onward because versions legitimately contain '+' and '-'.
    param([string]$Text, [string]$Key)
    if ([string]::IsNullOrEmpty($Text)) { return '' }
    $prefix = "$Key="
    foreach ($line in ($Text -split "`r?`n")) {
        if ($line.StartsWith($prefix)) { return $line.Substring($prefix.Length).Trim() }
    }
    return ''
}

# ---------------------------------------------------------------------------
# HTTP helpers -- bodies through files, headers through -D, never -i
# ---------------------------------------------------------------------------

function Invoke-Curl {
    param([string[]]$Arguments)
    # stderr is dropped: a native command that writes to stderr raises
    # NativeCommandError, which $ErrorActionPreference='Stop' escalates into a
    # terminating error that would abort the whole measurement.
    return (& curl.exe @Arguments 2>$null | Out-String)
}

function New-TmpPath {
    param([string]$Tag)
    return (Join-Path $tmpDir "m4-measure-$Tag-$stamp.txt")
}

function New-BodyFile {
    param([string]$Name, [string]$Json)
    $path = New-TmpPath "body-$Name"
    Write-NoBom $path $Json
    return $path
}

function Get-Header {
    param([string]$Headers, [string]$Name)
    if ([string]::IsNullOrEmpty($Headers)) { return '' }
    $m = [regex]::Match($Headers, "(?im)^$([regex]::Escape($Name)):\s*(.+?)\s*$")
    if ($m.Success) { return $m.Groups[1].Value.Trim() }
    return ''
}

function Invoke-Http {
    param(
        [string]$Url,
        [string]$Method = 'GET',
        [string[]]$Headers = @(),
        [string]$BodyFile = ''
    )
    $bodyPath = New-TmpPath 'resp-body'
    $hdrPath = New-TmpPath 'resp-hdr'
    $args = @('-s', '-o', $bodyPath, '-D', $hdrPath, '-w', '%{http_code}', '-X', $Method, $Url)
    foreach ($h in $Headers) { $args += @('-H', $h) }
    if ($BodyFile) { $args += @('--data-binary', "@$BodyFile") }
    $raw = Invoke-Curl $args
    return [pscustomobject]@{
        Status  = ($raw -replace '\s+$', '')
        Headers = (Read-Text $hdrPath)
        Body    = (Read-Text $bodyPath)
    }
}

# $Headers MUST be declared: PowerShell 5.1 silently DROPS an unknown named
# argument on a plain function, so a caller passing -Headers @(...) to a function
# that does not declare it sends the request with no headers at all.
function Send-Chat {
    param(
        [string]$Base,
        [string]$BodyFile,
        [string[]]$Headers = @()
    )
    $all = @('-H', 'Content-Type: application/json') + $Headers
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $r = Invoke-Http -Url "$Base/v1/chat/completions" -Method 'POST' -Headers $all -BodyFile $BodyFile
    $sw.Stop()
    $r | Add-Member -NotePropertyName 'ClientMs' -NotePropertyValue ([Math]::Round($sw.Elapsed.TotalMilliseconds, 3))
    return $r
}

# ---------------------------------------------------------------------------
# Statistics
# ---------------------------------------------------------------------------

function Get-Percentile {
    # Nearest-rank, the same definition internal/server uses, so a percentile in
    # this artifact and a percentile from /stats mean the same thing.
    param([double[]]$Values, [double]$P)
    if ($null -eq $Values -or $Values.Count -eq 0) { return 0.0 }
    $sorted = @($Values | Sort-Object)
    if ($P -le 0) { return [double]$sorted[0] }
    if ($P -ge 1) { return [double]$sorted[$sorted.Count - 1] }
    $idx = [int][Math]::Floor($sorted.Count * $P)
    if ($idx -ge $sorted.Count) { $idx = $sorted.Count - 1 }
    return [double]$sorted[$idx]
}

function Get-Mean {
    param([double[]]$Values)
    if ($null -eq $Values -or $Values.Count -eq 0) { return 0.0 }
    $sum = 0.0
    foreach ($v in $Values) { $sum += $v }
    return [Math]::Round(($sum / $Values.Count), 4)
}

# The /stats latency block prints Go duration strings; the micro sign (U+00B5)
# is rewritten to 'u' so the whole file stays ASCII.
function Convert-GoDurationToMs {
    param([string]$Text)
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

# ---------------------------------------------------------------------------
# Ports and processes
# ---------------------------------------------------------------------------

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
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$Port$Path")).Trim()
        if ($code -eq '200') { return $code }
        Start-Sleep -Milliseconds 200
    }
    return $code
}

function Stop-Tracked {
    param($Proc, [string]$Label)
    if ($null -eq $Proc) {
        Write-Host "  $Label was never started" -ForegroundColor DarkGray
        return
    }
    if (-not $Proc.HasExited) {
        Stop-Process -Id $Proc.Id -Force -ErrorAction SilentlyContinue
        Write-Host "  stopped $Label (pid=$($Proc.Id))" -ForegroundColor DarkGray
    }
    else {
        Write-Host "  $Label (pid=$($Proc.Id)) had already exited with $($Proc.ExitCode)" -ForegroundColor DarkGray
    }
}

# ---------------------------------------------------------------------------
# WSL bridge
#
# Every wsl.exe invocation writes a harmless Chinese notice about a localhost
# proxy to stderr and therefore reports [exit code: 1] even when the command
# succeeded. Nothing here ever reads the exit code; the scripts print their
# result and this side parses the OUTPUT.
# ---------------------------------------------------------------------------

function Invoke-Native {
    # Every wsl.exe / cmd.exe / curl.exe call goes through here.
    #
    # $ErrorActionPreference is 'Stop' for the whole script, and under PS 5.1 a
    # native command that writes ANYTHING to stderr becomes a terminating
    # NativeCommandError -- even with 2>$null AND even with 2>file (both were
    # measured on this host). wsl.exe writes one harmless Chinese line about a
    # localhost proxy on every single call, so the very first WSL call used to
    # abort the whole run after four assertions with no visible error. Setting
    # the preference to Continue inside this scriptblock is what stops it; the
    # assignment is local to the block, so the caller's Stop is untouched.
    param([scriptblock]$Command)
    $result = & {
        $ErrorActionPreference = 'Continue'
        & $Command 2>$null
    }
    return $result
}

function Invoke-Wsl {
    # Single-line bash only, and NO double quotes in the payload: PS 5.1 strips
    # them when marshalling native arguments, so `python -c "..."` arrives at
    # bash as two bare words. Anything like that belongs in a script file via
    # Invoke-WslScript.
    param([string]$ShellCommand)
    $out = Invoke-Native { & wsl.exe -d $wslDistro -u root -- bash -c $ShellCommand }
    return (($out | Out-String).Trim())
}

function Convert-ToUnixPath {
    # Windows path -> the path bash sees. Every path handed to a WSL script is
    # converted here and is therefore /mnt/c/... with no spaces and no colon,
    # i.e. safe to pass as a bare (unquoted) argument.
    #
    # Why bare: PS 5.1's native-argument marshalling strips the single quotes an
    # argument carries, so `bash script.sh 'C:/x/y.log'` reaches bash as
    # C:/x/y.log -- bash then honours the colon and the redirection fails with
    # "No such file or directory". Quoting the WHOLE argument list instead wraps
    # every argument into one, which is just as broken. Converted paths need no
    # quoting at all.
    param([string]$Path)
    return $Path.Replace('\', '/').Replace('C:', '/mnt/c')
}

function Invoke-WslScript {
    # The script runs from a real file, so no backslash survives a quoted
    # -lc payload and no CRLF survives into bash.
    #
    # The parameter must NOT be called $Args: that name is an automatic variable
    # in PowerShell, and a scriptblock called with a positional argument bound to
    # a parameter of that name receives NOTHING (measured: the generated command
    # was `bash 'script.sh' ` with an empty argument list, the script then ran
    # with no MODEL_DIR and produced no output at all). $ArgumentString is used
    # for that reason.
    param([string]$ScriptPath, [string]$ArgumentString = '')
    $unix = Convert-ToUnixPath $ScriptPath
    $cmd = "bash '$unix' $ArgumentString"
    $out = Invoke-Native { & wsl.exe -d $wslDistro -u root -- bash -c $cmd }
    return (($out | Out-String).Trim())
}

# ---------------------------------------------------------------------------
# vLLM lifecycle -- only ever the pid THIS script started
# ---------------------------------------------------------------------------

$startScriptBody = @'
#!/usr/bin/env bash
# Start one real vLLM server for the local tier and wait until its HTTP surface
# answers. Written by Windows and executed by WSL, so: LF endings, ASCII only.
set -u
MODEL_DIR="$1"; PORT="$2"; LOG="$3"; PIDFILE="$4"; MAXLEN="$5"; GPUUTIL="$6"
VLLM_BIN="${VLLM_BIN:-/opt/vllm/bin/vllm}"

if [ ! -x "$VLLM_BIN" ]; then echo "RESULT start=no-vllm-bin path=$VLLM_BIN"; exit 2; fi
if [ -z "$(ls -A "$MODEL_DIR" 2>/dev/null)" ]; then echo "RESULT start=model-dir-empty dir=$MODEL_DIR"; exit 3; fi
if [ -f "$PIDFILE" ]; then
  oldpid="$(cat "$PIDFILE" 2>/dev/null || true)"
  if [ -n "$oldpid" ] && kill -0 "$oldpid" 2>/dev/null; then
    echo "RESULT start=already-running pid=$oldpid"; exit 4
  fi
fi
: > "$LOG" 2>/dev/null || true
nohup "$VLLM_BIN" serve "$MODEL_DIR" \
    --served-model-name local-chat \
    --host 127.0.0.1 \
    --port "$PORT" \
    --max-model-len "$MAXLEN" \
    --gpu-memory-utilization "$GPUUTIL" \
    >> "$LOG" 2>&1 &
PID=$!
echo "$PID" > "$PIDFILE"
START="$(date +%s)"
for _ in $(seq 1 240); do
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "RESULT start=exited pid=$PID"
    tail -n 20 "$LOG" 2>/dev/null
    exit 5
  fi
  code="$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/v1/models" 2>/dev/null || true)"
  if [ "$code" = "200" ]; then
    echo "RESULT start=ok pid=$PID seconds=$(( $(date +%s) - START ))"
    exit 0
  fi
  sleep 1
done
echo "RESULT start=timeout pid=$PID waited=$(( $(date +%s) - START ))s"
tail -n 20 "$LOG" 2>/dev/null
exit 6
'@

$stopScriptBody = @'
#!/usr/bin/env bash
# Stop ONLY the pid recorded in the pidfile, walking its process tree: the
# `vllm serve` front end forks an EngineCore child, and killing just the parent
# leaves the GPU memory held and the port bound. Nothing here ever kills by
# name -- a foreign vLLM belongs to somebody else.
set -u
PORT="$1"; PIDFILE="$2"
pid=""
if [ -f "$PIDFILE" ]; then pid="$(cat "$PIDFILE" 2>/dev/null || true)"; fi
if [ -z "$pid" ]; then echo "RESULT stop=no-pidfile"; exit 0; fi
if ! kill -0 "$pid" 2>/dev/null; then
  rm -f "$PIDFILE"
  echo "RESULT stop=already-gone pid=$pid"
  exit 0
fi
collect() {
  echo "$1"
  for kid in $(pgrep -P "$1" 2>/dev/null || true); do collect "$kid"; done
}
TREE="$(collect "$pid" | tr '\n' ' ')"
echo "pidtree=$TREE"
kill $TREE 2>/dev/null || true
for _ in $(seq 1 30); do
  alive=0
  for p in $TREE; do if kill -0 "$p" 2>/dev/null; then alive=1; fi; done
  if [ "$alive" -eq 0 ]; then break; fi
  sleep 1
done
for p in $TREE; do if kill -0 "$p" 2>/dev/null; then kill -9 "$p" 2>/dev/null || true; fi; done
sleep 2
still=""
for p in $TREE; do if kill -0 "$p" 2>/dev/null; then still="$still $p"; fi; done
listening="no"
if curl -s -m 3 -o /dev/null "http://127.0.0.1:$PORT/v1/models" 2>/dev/null; then listening="yes"; fi
rm -f "$PIDFILE"
echo "RESULT stop=done still_alive=[${still}] v1_models_answers=$listening"
'@

function Start-VllmInWsl {
    param([string]$ModelDir, [int]$Port, [string]$LogPath, [int]$MaxLen, [double]$GpuUtil)
    Write-NoBom (Join-Path $tmpDir 'm4-measure-vllm-start.sh') $startScriptBody
    $unixLog = $LogPath.Replace('\', '/').Replace('C:', '/mnt/c')
    $unixPid = $vllmPidFile.Replace('\', '/').Replace('C:', '/mnt/c')
    $out = Invoke-WslScript (Join-Path $tmpDir 'm4-measure-vllm-start.sh') "$ModelDir $Port '$unixLog' '$unixPid' $MaxLen $GpuUtil"
    Write-Host "  $out" -ForegroundColor DarkGray
    [void]$script:log.Add("vllm start: $out")
    if ($out -match 'start=ok pid=(\d+) seconds=(\d+)') {
        return [pscustomobject]@{ Ok = $true; Pid = [int]$Matches[1]; Seconds = [int]$Matches[2]; Raw = $out }
    }
    return [pscustomobject]@{ Ok = $false; Pid = 0; Seconds = 0; Raw = $out }
}

function Stop-VllmInWsl {
    param([int]$Port)
    Write-NoBom (Join-Path $tmpDir 'm4-measure-vllm-stop.sh') $stopScriptBody
    $unixPid = $vllmPidFile.Replace('\', '/').Replace('C:', '/mnt/c')
    $out = Invoke-WslScript (Join-Path $tmpDir 'm4-measure-vllm-stop.sh') "$Port '$unixPid'"
    Write-Host "  $out" -ForegroundColor DarkGray
    [void]$script:log.Add("vllm stop: $out")
    return $out
}

# ---------------------------------------------------------------------------
# Build + config check
# ---------------------------------------------------------------------------

function Invoke-GoBuild {
    param([string]$Out, [string]$Pkg)
    for ($i = 1; $i -le 3; $i++) {
        Write-NoBom $buildLog ''
        & $goShim build -o $Out $Pkg 2>$buildLog | Out-Null
        if ($LASTEXITCODE -eq 0) { return $true }
        Write-Host "  build attempt $i failed:" -ForegroundColor DarkYellow
        Write-Host (Read-Text $buildLog) -ForegroundColor DarkYellow
        Start-Sleep -Milliseconds 500
    }
    return $false
}

# Native stderr must NOT be captured with 2>&1 inside a pipeline: PowerShell
# wraps it in a NativeCommandError that $ErrorActionPreference='Stop' promotes to
# a terminating error before the exit code can be read -- and one of the configs
# below is EXPECTED to be rejected. Start-Process hands back the exit code as data.
function Invoke-Check {
    param([string]$Config)
    Write-NoBom $checkOut ''
    Write-NoBom $checkErr ''
    $proc = Start-Process -FilePath $gwExe -ArgumentList @('-config', $Config, '-check') `
        -RedirectStandardOutput $checkOut -RedirectStandardError $checkErr -PassThru -Wait
    return [pscustomobject]@{ Exit = $proc.ExitCode; Text = ((Read-Text $checkOut) + (Read-Text $checkErr)) }
}

# ---------------------------------------------------------------------------
# Quantization artifacts
# ---------------------------------------------------------------------------

function Get-QuantRecord {
    param([string]$V)
    $jsonPath = Join-Path $tmpDir "m4-quant-$V.json"
    $runPath = Join-Path $tmpDir "m4-quant-$V.run.json"
    $errPath = Join-Path $tmpDir "m4-quant-$V.error.json"
    if (-not (Test-Path -LiteralPath $jsonPath)) {
        $why = 'no measurement artifact'
        if (Test-Path -LiteralPath $errPath) {
            $err = Get-Json (Read-Text $errPath)
            if ($err -and $err.detail) { $why = "bench failed: $($err.status) - $($err.detail)" }
            elseif ($err -and $err.status) { $why = "bench failed: $($err.status)" }
        }
        return [pscustomobject]@{ Ok = $false; Variant = $V; Json = $null; Run = $null; JsonPath = $jsonPath; RunPath = $runPath; Why = $why }
    }
    $j = Get-Json (Read-Text $jsonPath)
    $r = $null
    if (Test-Path -LiteralPath $runPath) { $r = Get-Json (Read-Text $runPath) }
    return [pscustomobject]@{ Ok = $true; Variant = $V; Json = $j; Run = $r; JsonPath = $jsonPath; RunPath = $runPath; Why = '' }
}

function New-VariantSummary {
    param($Rec)
    $j = $Rec.Json
    $r = $Rec.Run
    # errors is a plain integer in tmp\m4-quant-<v>.json ($j.errors -eq 0 for all
    # three variants), so @($j.errors).Count was wrong: @(0).Count is 1, which
    # wrote "errors: 1" into the artifact for a run with zero request errors.
    # Both counts below are now derived from the artifact's own fields.
    $reqErrorCount = @($j.requests | Where-Object { $null -ne $_.error }).Count
    $warmupErrorCount = @($j.warmup_errors | Where-Object { $null -ne $_ }).Count
    $out = [ordered]@{
        variant             = $Rec.Variant
        measured            = $true
        model               = [string]$j.model
        n                   = [int]$j.n
        requests_attempted  = [int]$j.requests_attempted
        errors              = [int]$j.errors
        errors_source       = 'tmp\m4-quant-<variant>.json field `errors` (an integer); requests[] carries error: null on all n entries and warmup_errors[] is empty'
        request_error_count = $reqErrorCount
        warmup_error_count  = $warmupErrorCount
        ttft_ms             = $j.ttft_ms
        total_ms            = $j.total_ms
        completion_chars    = $j.completion_chars
        throughput          = $j.throughput
        protocol            = $j.protocol
        agreement           = $j.agreement
        agreement_note      = $j.agreement_note
        usage_source        = [string]$j.usage_source
        created_utc         = [string]$j.created_utc
        artifact            = "tmp\m4-quant-$($Rec.Variant).json"
        server_log          = "tmp\vllm-$($Rec.Variant).log"
        load_seconds        = $null
        vram_baseline_mb    = $null
        vram_used_after_load_mb = $null
        vram_delta_mb       = $null
        vram_released_mb    = $null
        bench_status        = 'ok'
    }
    if ($null -ne $r) {
        $out['load_seconds'] = $r.load_seconds
        $out['vram_baseline_mb'] = $r.vram_baseline_before_load_mb
        $out['vram_used_after_load_mb'] = $r.vram_used_after_load_mb
        $out['vram_delta_mb'] = $r.vram_delta_after_load_mb
        $out['vram_released_mb'] = $r.vram_released_mb
        $out['bench_status'] = [string]$r.status
        if ($r.server_log) { $out['server_log'] = [string]$r.server_log }
    }
    return $out
}

# ---------------------------------------------------------------------------
# Pricing + cost arithmetic
#
# internal/gateway/pricing.go prices a request by the MODEL the response
# carries. Both tiers here answer as `local-chat`, so a request served by the
# local model is priced at local-chat's list price -- and that list price is
# what the same request would have cost in the cloud. The displaced spend is
# therefore (local tokens) x (list price), and the avoided fraction is that
# spend over what the whole mix would have cost. There is no cost metric to
# read, so this is derived from configured prices and measured token counts.
# ---------------------------------------------------------------------------

function Get-PricingFromConfig {
    param([string]$Text)
    $lines = $Text -split "`r?`n"
    $inPricing = $false
    $inModels = $false
    $default = $null
    $models = [ordered]@{}
    $pending = ''
    foreach ($line in $lines) {
        $clean = ($line -replace '#.*$', '')
        if ($clean.Trim().Length -eq 0) { continue }
        $indent = $clean.Length - $clean.TrimStart().Length
        $t = $clean.Trim()
        if ($t -match '^pricing:\s*$') { $inPricing = $true; $inModels = $false; continue }
        if ($inPricing) {
            if ($indent -eq 0) { $inPricing = $false; $inModels = $false; continue }
            if ($t -match '^default:\s*\{\s*in:\s*([0-9.]+)\s*,\s*out:\s*([0-9.]+)\s*\}\s*$') {
                $default = [ordered]@{ in = [double]$Matches[1]; out = [double]$Matches[2] }
                continue
            }
            if ($t -match '^models:\s*$') { $inModels = $true; $pending = ''; continue }
            # `default:` comes in two shapes: the inline flow form
            # `default: {in: 1.0, out: 3.0}` and the nested block form
            # configs\tiered-local.yaml actually uses, i.e. `default:` followed
            # by indented `in:`/`out:` lines. Both are handled; the nested form
            # is tracked through $pending exactly like the models entries.
            if ($t -match '^default:\s*$') { $pending = 'default'; $default = [ordered]@{ in = 0.0; out = 0.0 }; continue }
            if ($pending -eq 'default' -and $t -match '^in:\s*([0-9.]+)\s*$') { $default['in'] = [double]$Matches[1]; continue }
            if ($pending -eq 'default' -and $t -match '^out:\s*([0-9.]+)\s*$') { $default['out'] = [double]$Matches[1]; continue }
            if ($inModels) {
                if ($t -match '^([A-Za-z0-9._/-]+):\s*\{\s*in:\s*([0-9.]+)\s*,\s*out:\s*([0-9.]+)\s*\}\s*$') {
                    $models[$Matches[1]] = [ordered]@{ in = [double]$Matches[2]; out = [double]$Matches[3] }
                    $pending = ''
                    continue
                }
                if ($t -match '^([A-Za-z0-9._/-]+):\s*$') { $pending = $Matches[1]; continue }
                if ($pending -ne '' -and $t -match '^in:\s*([0-9.]+)\s*$') {
                    $models[$pending] = [ordered]@{ in = [double]$Matches[1]; out = 0.0 }
                    continue
                }
                if ($pending -ne '' -and $t -match '^out:\s*([0-9.]+)\s*$') {
                    if ($models.Contains($pending)) { $models[$pending]['out'] = [double]$Matches[1] }
                    $pending = ''
                    continue
                }
            }
        }
    }
    return [pscustomobject]@{ Default = $default; Models = $models }
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

$exitOk = $false
$quantRecords = New-Object System.Collections.ArrayList
$tieringBlock = $null
$tieringRan = $false
$runLog = @()

try {
    # -----------------------------------------------------------------------
    # 0. Preconditions
    # -----------------------------------------------------------------------
    Write-Section '0. Preconditions (host, GPU, WSL, vLLM)'

    $psVersion = $PSVersionTable.PSVersion.ToString()
    Write-Host "  repo      : $repo"
    Write-Host "  stamp     : $stamp"
    Write-Host "  powershell: $psVersion"
    Assert-That ($PSVersionTable.PSVersion.Major -eq 5) 'the script runs under Windows PowerShell 5.1 (no PS7 syntax is used)' "found $psVersion"
    Assert-That (Test-Path -LiteralPath $goShim) 'tools\go.cmd exists (go is not on PATH on this host)'
    Assert-That (Test-Path -LiteralPath $baseConfig) 'configs\tiered-local.yaml exists'
    Assert-That (Test-Path -LiteralPath $strictConfig) 'configs\tiered.yaml exists (the strict negative case)'

    # --- WSL reachable -----------------------------------------------------
    $wslKernel = Invoke-Wsl 'uname -r'
    $wslDistroName = Invoke-Wsl 'echo $WSL_DISTRO_NAME'
    Assert-That ($wslKernel.Length -gt 0) "wsl.exe -d $wslDistro -u root answers" "uname -r returned '$wslKernel'"
    Write-Host "  wsl       : $wslDistroName kernel $wslKernel"

    # --- vLLM present ------------------------------------------------------
    $vllmBin = Invoke-Wsl 'ls -l /opt/vllm/bin/vllm 2>/dev/null || echo MISSING'
    Assert-That ($vllmBin -notmatch 'MISSING') '/opt/vllm/bin/vllm exists inside WSL' $vllmBin
    $vllmVersion = Invoke-Wsl '/opt/vllm/bin/vllm --version 2>/dev/null | tail -1'
    # Double quotes inside a bash -c payload are eaten by PS 5.1's native
    # argument marshalling (measured: -c "import torch" arrives as the two
    # words import and torch, and bash reports a syntax error near the second
    # one), so anything with a quoted sub-command goes through a real script
    # file instead.
    $envSh = New-TmpPath 'probe-env'
    Write-NoBom $envSh @'
#!/usr/bin/env bash
printf 'torch=%s\n' "$(/opt/vllm/bin/python -c 'import torch;print(torch.__version__)' 2>/dev/null)"
printf 'cuda=%s\n' "$(/opt/vllm/bin/python -c 'import torch;print(torch.version.cuda)' 2>/dev/null)"
printf 'python=%s\n' "$(/opt/vllm/bin/python -c 'import sys;print(sys.version.split()[0])' 2>/dev/null)"
printf 'cuda_toolkit=%s\n' "$(/opt/vllm/bin/python -c 'import torch;print(torch.version.cuda)' 2>/dev/null)"
'@
    $envText = Invoke-WslScript $envSh ''
    $torchVersion = (Get-KeyValueText $envText 'torch')
    $pyVersion = (Get-KeyValueText $envText 'python')
    $cudaVersion = (Get-KeyValueText $envText 'cuda')
    Write-Host "  vllm      : $vllmVersion (torch $torchVersion, cuda $cudaVersion, python $pyVersion)"

    # --- model directories -------------------------------------------------
    $modelState = [ordered]@{}
    $stateSh = New-TmpPath 'probe-model-state'
    Write-NoBom $stateSh @'
#!/usr/bin/env bash
# One line per variant: state=present files=<n> safetensors=<bytes> config_json=<yes|no>
for v in fp16 awq gptq; do
  d="/opt/models/$v"
  if [ ! -d "$d" ]; then
    echo "variant=$v state=missing"
    continue
  fi
  n=$(ls -A "$d" 2>/dev/null | wc -l)
  sz=$(stat -c '%s' "$d/model.safetensors" 2>/dev/null || echo 0)
  if [ -f "$d/config.json" ]; then cfg=yes; else cfg=no; fi
  echo "variant=$v state=present files=$n safetensors=$sz config_json=$cfg"
done
'@
    $stateText = Invoke-WslScript $stateSh ''
    foreach ($v in @('fp16', 'awq', 'gptq')) {
        $dir = "/opt/models/$v"
        $st = (Get-LineByPrefix $stateText "variant=$v ")
        $files = 0; $sz = 0; $cfg = 'no'; $present = $false
        if ($st -match 'state=present files=(\d+) safetensors=(\d+) config_json=(\w+)') {
            $present = $true
            $files = [int]$Matches[1]
            $sz = [int64]$Matches[2]
            $cfg = $Matches[3]
        }
        $modelState[$v] = [ordered]@{
            dir = $dir; present = $present; files = $files
            safetensors_bytes = $sz; config_json = $cfg
            runnable = ($present -and $cfg -eq 'yes' -and $sz -gt 0)
            raw = $st
        }
        Write-Host ("  model {0,-5}: present={1} files={2} safetensors={3} config_json={4} runnable={5}" -f $v, $present, $files, $sz, $cfg, $modelState[$v]['runnable'])
    }
    Assert-That ($modelState['fp16']['present'] -and $modelState['fp16']['config_json'] -eq 'yes') 'the fp16 model directory has a config.json'

    # --- ports -------------------------------------------------------------
    $ports = @($GatewayPort, $CloudPort, $VllmPort)
    foreach ($port in $ports) {
        $busy = Test-PortOpen -Port $port
        Assert-That (-not $busy) "port :$port is free before the run" 'something is already listening -- stop it or pick another port'
    }

    # --- GPU facts, both sides --------------------------------------------
    $winSmiLines = @()
    try {
        $tmpSmi = New-TmpPath 'win-smi'
        # cmd.exe also writes to stderr (its own warnings, and the shell's echo
        # of a failed command), so it goes through the same native wrapper.
        Invoke-Native { & cmd.exe /c "nvidia-smi > `"$tmpSmi`" 2>&1" } | Out-Null
        $winSmiLines = @((Read-Text $tmpSmi) -split "`r?`n")
    }
    catch { $winSmiLines = @() }
    $wslSmi = Invoke-Wsl '/usr/lib/wsl/lib/nvidia-smi 2>/dev/null || nvidia-smi 2>/dev/null || echo MISSING'
    Assert-That ($wslSmi -notmatch 'MISSING') 'nvidia-smi answers inside WSL (/usr/lib/wsl/lib/nvidia-smi)' $wslSmi

    $winSmiText = ($winSmiLines -join "`n")
    $gpuName = ''
    $driverVersion = ''
    $m = [regex]::Match($wslSmi, '(?m)^\|\s+([0-9]+)\s+([A-Za-z0-9._-]+)\s+([0-9]+)\s+')
    $gpuWslRaw = $wslSmi
    $m2 = [regex]::Match($wslSmi, 'Driver Version:\s*([0-9.]+)')
    if ($m2.Success) { $driverVersion = $m2.Groups[1].Value }
    $m3 = [regex]::Match($winSmiText, 'Driver Version:\s*([0-9.]+)')
    if ($driverVersion -eq '' -and $m3.Success) { $driverVersion = $m3.Groups[1].Value }
    $m4 = [regex]::Match($winSmiText, '(?m)^\|\s+0\s+(NVIDIA[^|]+?)\s+(?:WDDM|TCC)?\s*\|')
    if ($m4.Success) { $gpuName = $m4.Groups[1].Value.Trim() }
    if ($gpuName -eq '') {
        $m5 = [regex]::Match($winSmiText, '(?m)^\|\s+0\s+(NVIDIA[^|]+?)\|')
        if ($m5.Success) { $gpuName = $m5.Groups[1].Value.Trim() }
    }
    $winGpuQuery = ''
    try {
        $tmpQ = New-TmpPath 'win-gpuquery'
        Invoke-Native { & cmd.exe /c "nvidia-smi --query-gpu=name,driver_version,memory.total,memory.used,utilization.gpu --format=csv,noheader > `"$tmpQ`" 2>&1" } | Out-Null
        $winGpuQuery = (Read-Text $tmpQ).Trim()
    }
    catch { $winGpuQuery = '' }
    $wslGpuQuery = Invoke-Wsl '/usr/lib/wsl/lib/nvidia-smi --query-gpu=name,driver_version,memory.total,memory.used,utilization.gpu --format=csv,noheader 2>/dev/null'
    if ($gpuName -eq '' -and $winGpuQuery -match '^([^,]+),') { $gpuName = $Matches[1].Trim() }
    Write-Host "  gpu       : $gpuName (driver $driverVersion)"
    Write-Host "  gpu win   : $winGpuQuery"
    Write-Host "  gpu wsl   : $wslGpuQuery"

    # --- provenance: where each checkpoint came from, and how big it is ----
    # The three checkpoints did NOT all arrive the same way (AWQ stalled on the
    # hf-mirror endpoint and finished at 6-12x the throughput from ModelScope),
    # so the source of every file is recorded instead of assumed.
    $downloaderProc = (Invoke-Wsl "ps -eo pid,etime,cmd | grep -i -E 'hf download|huggingface-cli|modelscope' | grep -v grep || true").Trim()
    $provenanceBlock = [ordered]@{}
    foreach ($v in @('fp16', 'awq', 'gptq')) {
        $p = "/opt/models/$v/model.safetensors"
        $sizes = (Invoke-Wsl "stat -c '%s %Y' $p 2>/dev/null || echo 0 0").Trim()
        $bytes = 0; $mtime = 0
        $parts = @($sizes -split '\s+')
        if ($parts.Count -ge 2) { $bytes = [int64]$parts[0]; $mtime = [int64]$parts[1] }
        $mtimeLocal = ''
        if ($mtime -gt 0) {
            try { $mtimeLocal = [DateTimeOffset]::FromUnixTimeSeconds($mtime).ToLocalTime().ToString('yyyy-MM-ddTHH:mm:sszzz') }
            catch { $mtimeLocal = '' }
        }
        $provenanceBlock[$v] = [ordered]@{
            model_dir   = "/opt/models/$v"
            safetensors = 'model.safetensors'
            file_bytes  = $bytes
            mtime_local = $mtimeLocal
        }
    }
    foreach ($v in @('fp16', 'awq', 'gptq')) {
        $hf = (Invoke-Wsl "ls /root/.cache/huggingface/hub 2>/dev/null | grep -i '$v' || true").Trim()
        if ($v -eq 'fp16') { $hf = (Invoke-Wsl "ls /root/.cache/huggingface/hub 2>/dev/null | grep -i -E 'fp16|Qwen2.5-1.5B-Instruct$' || true").Trim() }
        $provenanceBlock[$v]['hf_cache_entry'] = $hf
    }
    $provenanceBlock['fp16']['source_hint'] = 'huggingface / hf-mirror (root/.cache/huggingface/hub)'
    $provenanceBlock['gptq']['source_hint'] = 'huggingface / hf-mirror (root/.cache/huggingface/hub)'
    $provenanceBlock['awq']['source_hint'] = 'ModelScope (https://www.modelscope.cn/api/v1/models/Qwen/Qwen2.5-1.5B-Instruct-AWQ/repo?Revision=master&FilePath=model.safetensors) after the hf-mirror transfer stalled at 732 MB'
    $provenanceBlock['note'] = 'file_bytes and mtime_local are read with stat(1) at measure time; source_hint records how the checkpoint was fetched (hf-mirror vs ModelScope) and is not re-verified against the network here.'
    $provenanceBlock['download_process_seen_at_start'] = $downloaderProc
    Write-Host '  provenance:' -ForegroundColor DarkGray
    foreach ($v in @('fp16', 'awq', 'gptq')) {
        Write-Host ("    {0,-5} {1} bytes  mtime {2}" -f $v, $provenanceBlock[$v]['file_bytes'], $provenanceBlock[$v]['mtime_local']) -ForegroundColor DarkGray
    }
    if ($downloaderProc.Length -gt 0) { Write-Host "    download process still running: $($downloaderProc -replace "`n", ' | ')" -ForegroundColor DarkYellow }

    # --- kernels -----------------------------------------------------------
    $wslKernelFull = Invoke-Wsl 'uname -a'

    # -----------------------------------------------------------------------
    # 1. Build + config surface
    # -----------------------------------------------------------------------
    Write-Section '1. Build the binaries and check the configs'

    $gwBuilt = Invoke-GoBuild -Out $gwExe -Pkg '.\cmd\infergate'
    $mockBuilt = Invoke-GoBuild -Out $mockExe -Pkg '.\cmd\mockupstream'
    Assert-That ($gwBuilt -and (Test-Path -LiteralPath $gwExe)) 'go build produced bin\infergate.exe'
    Assert-That ($mockBuilt -and (Test-Path -LiteralPath $mockExe)) 'go build produced bin\mockupstream.exe'

    # configs\ is never modified: the instance under test is a tmp copy whose
    # literal ports and upstream URLs are substituted.
    $configText = Read-Text $baseConfig
    Assert-That ($configText.Length -gt 1000) 'the config under test was read' "read $($configText.Length) characters"
    $instantiated = $configText -replace '(?m)^(\s*listen:\s*)":8080"', ('${1}":' + $GatewayPort + '"')
    $instantiated = $instantiated -replace 'http://127\.0\.0\.1:8000', "http://127.0.0.1:$VllmPort"
    $instantiated = $instantiated -replace 'http://127\.0\.0\.1:9100', "http://127.0.0.1:$CloudPort"
    Write-NoBom $configPath $instantiated
    Assert-That ($instantiated.Contains("127.0.0.1:$VllmPort")) 'the tmp config points the local tier at the real vLLM port'
    Assert-That ($instantiated.Contains("127.0.0.1:$CloudPort")) 'the tmp config points the cloud tier at the mock port'

    $goodCheck = Invoke-Check -Config $configPath
    Assert-That ($goodCheck.Exit -eq 0) 'bin\infergate.exe -config <tmp tiered-local> -check exits 0' "exit=$($goodCheck.Exit) text=$((($goodCheck.Text -replace '\s+', ' ')).Trim())"
    Assert-Contains $goodCheck.Text 'configuration OK' '-check reports the configuration is OK'

    # The strict config deliberately references ${VLLM_BASE_URL}; nothing in this
    # script defines it, so the loader MUST refuse it. This is the expected
    # failure that proves the check is a real check.
    $strictCheck = Invoke-Check -Config $strictConfig
    Assert-That ($strictCheck.Exit -ne 0) 'configs\tiered.yaml -check FAILS (expected: it references undefined ${VAR}s)' "exit=$($strictCheck.Exit)"
    Assert-Contains $strictCheck.Text 'undefined environment variable' '-check names the undefined environment variable(s)'
    Write-Host "  strict -check (expected failure, exit $($strictCheck.Exit)):" -ForegroundColor DarkGray
    Write-Host "    $((($strictCheck.Text -replace '\s+', ' ')).Trim())" -ForegroundColor DarkGray

    # -----------------------------------------------------------------------
    # 2. Variant selection for the local tier
    # -----------------------------------------------------------------------
    Write-Section '2. Choose the local variant'

    $preferred = @()
    if ($Variant -eq 'auto') { $preferred = @('awq', 'gptq', 'fp16') } else { $preferred = @($Variant) }

    # If the bench is about to run, wait (bounded) for an in-flight AWQ download;
    # a half-written safetensors file would be "runnable" by directory listing and
    # would fail minutes later inside vLLM for a reason that has nothing to do
    # with the gateway.
    $awqWaitResult = 'not attempted'
    if ((-not $SkipBench) -and $AwqWaitSeconds -gt 0) {
        Write-Host "  waiting up to $AwqWaitSeconds s for a settled /opt/models/awq/model.safetensors ..." -ForegroundColor DarkGray
        $waitBody = @'
#!/usr/bin/env bash
# Wait, bounded, for an AWQ checkpoint that is still being downloaded, and stop
# waiting early once its size has held still for SETTLE consecutive polls -- a
# growing file is a half-written file, and vLLM loading one fails in a way that
# looks like a vLLM bug instead of an unfinished download.
set -u
if [ "$#" -lt 3 ]; then echo "RESULT awq=bad-args argc=$#"; exit 2; fi
TARGET="$1"; MAX="$2"; SETTLE="$3"
start="$(date +%s)"
last=""; still=0
while :; do
  if [ -f "$TARGET" ]; then
    cur="$(stat -c '%s' "$TARGET" 2>/dev/null || echo 0)"
    if [ "$cur" != "$last" ]; then
      echo "progress bytes=$cur t=$(( $(date +%s) - start ))s"
      last="$cur"; still=0
    else
      still=$(( still + 1 ))
    fi
    if [ "$still" -ge "$SETTLE" ] && [ "$cur" -gt 0 ]; then
      echo "RESULT awq=settled bytes=$cur waited=$(( $(date +%s) - start ))s"
      exit 0
    fi
  else
    still=0
    last=""
  fi
  if [ $(( $(date +%s) - start )) -ge "$MAX" ]; then
    echo "RESULT awq=not-settled wanted=$TARGET waited=$(( $(date +%s) - start ))s"
    exit 1
  fi
  sleep 30
done
'@
        Write-NoBom $waitSh $waitBody
        $unixTarget = '/opt/models/awq/model.safetensors'
        $w = Invoke-WslScript $waitSh "$unixTarget $AwqWaitSeconds 2"
        $awqWaitResult = $w
        Write-Host "  $($w -replace "`n", "`n  ")" -ForegroundColor DarkGray
        [void]$script:log.Add("awq wait: $($w -replace "`n", ' | ')")
    }

    foreach ($v in $preferred) {
        if ($modelState[$v] -and $modelState[$v]['runnable']) { $localVariant = $v; break }
    }
    if ($localVariant -eq '') {
        # Fall back to any runnable model rather than dying: the artifact must say
        # which variant actually served the local tier.
        foreach ($v in @('fp16', 'gptq', 'awq')) {
            if ($modelState[$v] -and $modelState[$v]['runnable']) {
                $localVariant = $v
                $localVariantReason = "requested '$Variant' is not runnable; fell back to '$v'"
                break
            }
        }
    }
    else {
        $localVariantReason = "first runnable of the requested order ($($preferred -join ' > '))"
    }
    if ($localVariant -eq '') {
        throw 'no model directory under /opt/models is runnable (need config.json + a non-empty model.safetensors)'
    }
    Write-Host "  local variant for the tiering section: $localVariant" -ForegroundColor White
    $runnerVariant = $localVariant

    # -----------------------------------------------------------------------
    # 3. Quantization comparison
    # -----------------------------------------------------------------------
    Write-Section '3. Quantization comparison (real vLLM, one variant at a time)'

    $benchVariants = @()
    foreach ($v in @('fp16', 'awq', 'gptq')) {
        if ($SkipBench) {
            $rec = Get-QuantRecord -V $v
            if ($rec.Ok) { $benchVariants += $v }
        }
        elseif ($modelState[$v]['runnable']) {
            $benchVariants += $v
        }
        else {
            Add-Failure "quantization for '$v' was skipped: /opt/models/$v is not runnable (config_json=$($modelState[$v]['config_json']) safetensors_bytes=$($modelState[$v]['safetensors_bytes']))"
        }
    }
    if ($SkipBench) {
        Write-Host '  -SkipBench: reusing the measurement artifacts already in tmp\' -ForegroundColor DarkYellow
    }

    if ($benchVariants.Count -gt 0) {
        if (-not $SkipBench) {
            $benchBody = @'
#!/usr/bin/env bash
# Run scripts/bench-vllm-quant.sh for the workable variants, sequentially and in
# ONE shell, because the bench refuses to start while anything else holds the
# port and each variant needs the previous one to have released it.
set -u
cd /mnt/c/Users/20106/Desktop/infergate || exit 9
export OUT_DIR="${OUT_DIR:-/mnt/c/Users/20106/Desktop/infergate/tmp}"
echo "BENCH variants: $*  OUT_DIR=$OUT_DIR"
bash scripts/bench-vllm-quant.sh "$@"
rc=$?
echo "BENCH_EXIT=$rc"
exit $rc
'@
            Write-NoBom $benchSh $benchBody
            $benchStart = Get-Date
            Write-Host "  running the bench for: $($benchVariants -join ' ')" -ForegroundColor DarkGray
            # Native stderr from wsl.exe is NOT captured: it carries the harmless
            # proxy notice on every call.
            $benchOut = Invoke-Native { & wsl.exe -d $wslDistro -u root -- bash /mnt/c/Users/20106/Desktop/infergate/tmp/m4-measure-bench.sh @benchVariants }
            $benchText = ($benchOut | Out-String)
            $benchSeconds = [Math]::Round(((Get-Date) - $benchStart).TotalSeconds, 1)
            $benchExit = -1
            if ($benchText -match 'BENCH_EXIT=(-?\d+)') { $benchExit = [int]$Matches[1] }
            foreach ($line in ($benchText -split "`r?`n")) {
                if ($line.Trim().Length -gt 0) { Write-Host "  $line" -ForegroundColor DarkGray }
            }
            [void]$script:log.Add("bench variants=$($benchVariants -join ',') exit=$benchExit duration_s=$benchSeconds")
            Assert-That ($benchExit -eq 0) 'scripts/bench-vllm-quant.sh exited 0 for every variant it ran' "exit=$benchExit after ${benchSeconds}s"
        }
    }
    else {
        Add-Failure 'no quantization variant could be measured on this run'
    }

    foreach ($v in @('fp16', 'awq', 'gptq')) {
        $rec = Get-QuantRecord -V $v
        if ($rec.Ok) {
            [void]$quantRecords.Add((New-VariantSummary -Rec $rec))
        }
        else {
            [void]$quantRecords.Add([ordered]@{
                    variant      = $v
                    measured     = $false
                    reason       = $rec.Why
                    runnable     = $modelState[$v]['runnable']
                    dir_present  = $modelState[$v]['present']
                    files        = $modelState[$v]['files']
                    safetensors_bytes = $modelState[$v]['safetensors_bytes']
                    config_json  = $modelState[$v]['config_json']
                    awq_wait     = $awqWaitResult
                })
        }
    }

    $measuredCount = @($quantRecords | Where-Object { $_.measured }).Count
    Assert-That ($measuredCount -ge 1) 'at least one quantization variant has a real measurement' "measured=$measuredCount"
    if ($measuredCount -lt 3) {
        Add-Failure "only $measuredCount of 3 quantization variants have measurements; the missing ones are recorded with their reason in the artifact"
    }

    # per-variant comparison, or one variant with a range
    foreach ($v in @('awq', 'gptq')) {
        $base = @($quantRecords | Where-Object { $_.variant -eq 'fp16' -and $_.measured })
        $this = @($quantRecords | Where-Object { $_.variant -eq $v -and $_.measured })
        if ($base.Count -eq 1 -and $this.Count -eq 1) {
            $b = $base[0]; $t = $this[0]
            $bTps = [double]$b.throughput.output_tokens_per_s_request_wall
            $tTps = [double]$t.throughput.output_tokens_per_s_request_wall
            $ratio = 0.0
            if ($bTps -gt 0) { $ratio = [Math]::Round(($tTps / $bTps), 3) }
            $snr = 'single run each; no repeats, so this is a difference, not a distribution'
            Write-Host ("  {0} vs fp16: ttft p50 {1} -> {2} ms | {3} -> {4} tok/s ({5}x) | vram delta {6} -> {7} MiB | load {8} -> {9} s" -f `
                    $v, $b.ttft_ms.p50, $t.ttft_ms.p50, $bTps, $tTps, $ratio, $b.vram_delta_mb, $t.vram_delta_mb, $b.load_seconds, $t.load_seconds) -ForegroundColor DarkGray
            $comparison = [ordered]@{
                variant              = $v
                reference            = 'fp16'
                ttft_p50_ms          = [ordered]@{ fp16 = $b.ttft_ms.p50; variant = $t.ttft_ms.p50; delta_ms = [Math]::Round(([double]$t.ttft_ms.p50 - [double]$b.ttft_ms.p50), 3) }
                total_p50_ms         = [ordered]@{ fp16 = $b.total_ms.p50; variant = $t.total_ms.p50; delta_ms = [Math]::Round(([double]$t.total_ms.p50 - [double]$b.total_ms.p50), 3) }
                output_tokens_per_s  = [ordered]@{ fp16 = $bTps; variant = $tTps; ratio = $ratio }
                vram_delta_mb        = [ordered]@{ fp16 = $b.vram_delta_mb; variant = $t.vram_delta_mb; saved_mb = $null }
                load_seconds         = [ordered]@{ fp16 = $b.load_seconds; variant = $t.load_seconds }
                agreement_vs_fp16    = $t.agreement
                agreement_meaning    = 'DRIFT: how much the quantized answer text differs from the fp16 reference text. It is NOT an accuracy or quality score.'
                signal_to_noise      = $snr
            }
            if ($null -ne $b.vram_delta_mb -and $null -ne $t.vram_delta_mb) {
                $comparison['vram_delta_mb']['saved_mb'] = [double]$b.vram_delta_mb - [double]$t.vram_delta_mb
            }
            $script:comparisons += $comparison
        }
        else {
            $comparison = [ordered]@{
                variant = $v
                reference = 'fp16'
                comparable = $false
                reason = "fp16 measured=$($base.Count) $v measured=$($this.Count); a comparison needs both"
            }
            $script:comparisons += $comparison
        }
    }

    # -----------------------------------------------------------------------
    # 4. Tiered routing against the real local model + the mock cloud
    # -----------------------------------------------------------------------
    Write-Section "4. Tiered routing (real vLLM '$runnerVariant' as local, cmd\mockupstream as cloud)"

    if ($SkipTiering) {
        Add-Failure '-SkipTiering was set: no request was sent through the gateway on this run'
        $tieringBlock = [ordered]@{ measured = $false; reason = '-SkipTiering'; artifact_note = 'the tiering measurement was switched off with -SkipTiering, so this object carries no mix, latency or cost' }
    }
    else {
        $tieringRan = $true
        # ---- fleet up ------------------------------------------------------
        $vllmStart = Start-VllmInWsl -ModelDir "/opt/models/$runnerVariant" -Port $VllmPort -LogPath $vllmLog -MaxLen 4096 -GpuUtil 0.85
        Assert-That $vllmStart.Ok "vLLM served /opt/models/$runnerVariant on :$VllmPort and answered /v1/models" "start script said: $($vllmStart.Raw)"
        if (-not $vllmStart.Ok) {
            Write-Host "  vLLM log tail:" -ForegroundColor DarkYellow
            Write-Host (Get-TailText $vllmLog 25) -ForegroundColor DarkYellow
            throw "the local tier never came up: $($vllmStart.Raw)"
        }
        $vllmPid = $vllmStart.Pid
        Write-Host "  vLLM pid=$vllmPid ready after $($vllmStart.Seconds)s" -ForegroundColor DarkGray

        # vLLM's own view of the model it loaded -- the proof that the local tier
        # is a real model server and not the mock.
        $vllmModels = Get-Json (Invoke-Curl @('-s', '--max-time', '20', "http://127.0.0.1:$VllmPort/v1/models"))
        $vllmModelId = ''
        if ($null -ne $vllmModels -and @($vllmModels.data).Count -gt 0) { $vllmModelId = [string]$vllmModels.data[0].id }
        Assert-That ($vllmModelId -eq 'local-chat') "the local model server reports served-model-name 'local-chat'" "got '$vllmModelId'"

        $mock = Start-Process -FilePath $mockExe -ArgumentList @('-listen', ":$CloudPort", '-name', 'cloud-mock', '-token-delay', '1ms') `
            -RedirectStandardOutput $mockLog -RedirectStandardError $mockErr -PassThru -WindowStyle Hidden
        $script:mockProc = $mock
        $script:startedPids += $mock.Id
        $script:upstreamStarted += 1
        $cloudHealthy = Wait-Healthy -Port $CloudPort -Seconds 15
        Assert-That ($cloudHealthy -eq '200') "the cloud-tier mock answers /healthz on :$CloudPort"

        $script:gwProc = Start-Process -FilePath $gwExe -ArgumentList @('-config', $configPath) `
            -RedirectStandardOutput $gwLog -RedirectStandardError $gwErr -PassThru -WindowStyle Hidden
        $script:startedPids += $script:gwProc.Id
        $script:upstreamStarted += 1
        $gwHealthy = Wait-Healthy -Port $GatewayPort -Seconds 25
        Assert-That ($gwHealthy -eq '200') "the gateway answers /healthz on :$GatewayPort"
        if ($gwHealthy -ne '200') {
            Write-Host (Read-Text $gwErr) -ForegroundColor DarkYellow
            Write-Host (Read-Text $gwLog) -ForegroundColor DarkYellow
            throw 'the gateway never became healthy'
        }
        $adminText = (Invoke-Curl @('-s', '--max-time', '20', "$gwUrl/admin/upstreams"))
        $adminJson = Get-Json $adminText
        # /admin/upstreams answers PRETTY-PRINTED JSON ("strategy": "tiered"),
        # so a compact-substring probe never matches: parse it and read the
        # properties instead.
        $adminStrategy = ''
        if ($null -ne $adminJson -and $null -ne $adminJson.routing) { $adminStrategy = [string]$adminJson.routing.strategy }
        Assert-That ($adminStrategy -eq 'tiered') '/admin/upstreams reports the tiered strategy' "strategy=$adminStrategy"
        $tierLocalCount = 0
        $tierCloudCount = 0
        foreach ($u in @($adminJson.upstreams)) {
            if ([string]$u.tier -eq 'local') { $tierLocalCount += 1 }
            if ([string]$u.tier -eq 'cloud') { $tierCloudCount += 1 }
        }
        Assert-That ($tierLocalCount -ge 1) 'the gateway loaded at least one local-tier upstream' "local=$tierLocalCount cloud=$tierCloudCount"
        Assert-That ($tierCloudCount -ge 1) 'the gateway loaded at least one cloud-tier upstream' "local=$tierLocalCount cloud=$tierCloudCount"

        # ---- the mix -------------------------------------------------------
        $simpleText = 'Summarise the water cycle in one sentence.'
        $hardText = (('The following is a long technical note about inference routing. ' * 38) + 'End of note.')
        $simpleJson = '{"model":"local-chat","messages":[{"role":"user","content":' + (ConvertTo-Json $simpleText -Compress) + '}],"max_tokens":96}'
        $hardJson = '{"model":"local-chat","messages":[{"role":"user","content":' + (ConvertTo-Json $hardText -Compress) + '}],"max_tokens":96}'
        $simpleFile = New-BodyFile -Name 'simple' -Json $simpleJson
        $hardFile = New-BodyFile -Name 'hard' -Json $hardJson
        $hardChars = $hardText.Length
        Write-Host "  simple body: $($simpleJson.Length) bytes; hard body: $($hardJson.Length) bytes ($hardChars chars of user text)" -ForegroundColor DarkGray
        Assert-That ($hardChars -gt 2400) 'the hard prompt carries more than 2400 characters of user text' "chars=$hardChars"

        $statsBefore = Get-Json (Invoke-Curl @('-s', '--max-time', '20', "$gwUrl/stats"))
        Assert-That ($null -ne $statsBefore) 'GET /stats answers JSON before the mix'
        $localBefore = 0; $cloudBefore = 0; $promptBefore = 0; $completionBefore = 0
        if ($null -ne $statsBefore) {
            foreach ($row in @($statsBefore.series)) {
                if ($row.upstream -eq 'local-vllm') { $localBefore += [int]$row.count }
                if ($row.upstream -eq 'cloud-mock') { $cloudBefore += [int]$row.count }
            }
            $promptBefore = [int64]$statsBefore.tokens.prompt
            $completionBefore = [int64]$statsBefore.tokens.completion
        }

        $simpleResults = New-Object System.Collections.ArrayList
        $hardResults = New-Object System.Collections.ArrayList
        $mixStart = Get-Date
        for ($i = 1; $i -le $Requests; $i++) {
            $r = Send-Chat -Base $gwUrl -BodyFile $simpleFile -Headers @('X-InferGate-Capabilities: chat')
            $tier = Get-Header $r.Headers 'X-InferGate-Upstream-Name'
            $body = Get-Json $r.Body
            $content = ''
            $usagePresent = $false
            $promptTokens = 0; $completionTokens = 0
            if ($null -ne $body) {
                if ($null -ne $body.choices -and @($body.choices).Count -gt 0) { $content = [string]$body.choices[0].message.content }
                if ($null -ne $body.usage) {
                    $usagePresent = $true
                    if ($null -ne $body.usage.prompt_tokens) { $promptTokens = [int]$body.usage.prompt_tokens }
                    if ($null -ne $body.usage.completion_tokens) { $completionTokens = [int]$body.usage.completion_tokens }
                }
            }
            [void]$simpleResults.Add([ordered]@{
                    index = $i; tier = $tier; status = $r.Status
                    client_ms = $r.ClientMs
                    local_spend_usd = 0.0
                    prompt_tokens = $promptTokens; completion_tokens = $completionTokens
                    usage_present = $usagePresent; content_chars = $content.Length; content = $content
                })
        }
        for ($i = 1; $i -le $Requests; $i++) {
            $r = Send-Chat -Base $gwUrl -BodyFile $hardFile -Headers @('X-InferGate-Capabilities: chat')
            $tier = Get-Header $r.Headers 'X-InferGate-Upstream-Name'
            $body = Get-Json $r.Body
            $content = ''
            $usagePresent = $false
            $promptTokens = 0; $completionTokens = 0
            if ($null -ne $body) {
                if ($null -ne $body.choices -and @($body.choices).Count -gt 0) { $content = [string]$body.choices[0].message.content }
                if ($null -ne $body.usage) {
                    $usagePresent = $true
                    if ($null -ne $body.usage.prompt_tokens) { $promptTokens = [int]$body.usage.prompt_tokens }
                    if ($null -ne $body.usage.completion_tokens) { $completionTokens = [int]$body.usage.completion_tokens }
                }
            }
            [void]$hardResults.Add([ordered]@{
                    index = $i; tier = $tier; status = $r.Status
                    client_ms = $r.ClientMs
                    local_spend_usd = 0.0
                    prompt_tokens = $promptTokens; completion_tokens = $completionTokens
                    usage_present = $usagePresent; content_chars = $content.Length; content = $content
                })
        }
        $mixSeconds = [Math]::Round(((Get-Date) - $mixStart).TotalSeconds, 1)

        $statsAfter = Get-Json (Invoke-Curl @('-s', '--max-time', '20', "$gwUrl/stats"))
        Assert-That ($null -ne $statsAfter) 'GET /stats answers JSON after the mix'
        $localAfter = 0; $cloudAfter = 0
        $localMeanSeconds = 0.0; $cloudMeanSeconds = 0.0
        $localTotalSeconds = 0.0; $cloudTotalSeconds = 0.0
        if ($null -ne $statsAfter) {
            foreach ($row in @($statsAfter.series)) {
                if ($row.upstream -eq 'local-vllm') {
                    $localAfter += [int]$row.count
                    $localMeanSeconds = [double]$row.mean_seconds
                    $localTotalSeconds = [double]$row.total_seconds
                }
                if ($row.upstream -eq 'cloud-mock') {
                    $cloudAfter += [int]$row.count
                    $cloudMeanSeconds = [double]$row.mean_seconds
                    $cloudTotalSeconds = [double]$row.total_seconds
                }
            }
        }
        $promptDelta = [int64]$statsAfter.tokens.prompt - $promptBefore
        $completionDelta = [int64]$statsAfter.tokens.completion - $completionBefore
        $localDelta = $localAfter - $localBefore
        $cloudDelta = $cloudAfter - $cloudBefore

        $simpleLocal = @($simpleResults | Where-Object { $_.tier -eq 'local-vllm' }).Count
        $simpleCloud = @($simpleResults | Where-Object { $_.tier -eq 'cloud-mock' }).Count
        $hardLocal = @($hardResults | Where-Object { $_.tier -eq 'local-vllm' }).Count
        $hardCloud = @($hardResults | Where-Object { $_.tier -eq 'cloud-mock' }).Count

        Write-Host ("  mix: {0} simple -> local={1} cloud={2} | {0} hard -> local={3} cloud={4}" -f $Requests, $simpleLocal, $simpleCloud, $hardLocal, $hardCloud) -ForegroundColor White
        Write-Host ("  /stats deltas: local-vllm +{0} cloud-mock +{1} (requested {2})" -f $localDelta, $cloudDelta, (2 * $Requests)) -ForegroundColor DarkGray

        Assert-That ($simpleLocal -eq $Requests) 'EVERY simple request was answered by the local tier' "local=$simpleLocal of $Requests"
        Assert-That ($simpleCloud -eq 0) 'NO simple request was answered by the cloud tier' "cloud=$simpleCloud"
        Assert-That ($hardLocal -eq 0) 'NO hard request was answered by the local tier' "local=$hardLocal"
        Assert-That ($hardCloud -eq $Requests) 'EVERY hard request was answered by the cloud tier' "cloud=$hardCloud"
        Assert-That ($localDelta -eq $Requests) 'the local tier series grew by exactly the simple requests' "delta=$localDelta want=$Requests"
        Assert-That ($cloudDelta -eq $Requests) 'the cloud tier series grew by exactly the hard requests' "delta=$cloudDelta want=$Requests"

        $allResults = @($simpleResults) + @($hardResults)
        $badStatus = @($allResults | Where-Object { $_.status -ne '200' })
        Assert-That ($badStatus.Count -eq 0) 'every request in the mix answered HTTP 200' "non-200: $($badStatus.Count)"

        $localAnswered = @($simpleResults | Where-Object { $_.tier -eq 'local-vllm' })
        $nonEmpty = @($localAnswered | Where-Object { $_.content_chars -gt 0 })
        $withUsage = @($localAnswered | Where-Object { $_.usage_present })
        Assert-That ($nonEmpty.Count -eq $localAnswered.Count) 'every locally served answer carried non-empty content from the real model' "non-empty=$($nonEmpty.Count) of $($localAnswered.Count)"
        Assert-That ($withUsage.Count -eq $localAnswered.Count) 'every locally served answer carried a usage block' "usage=$($withUsage.Count) of $($localAnswered.Count)"
        if ($localAnswered.Count -gt 0) {
            Write-Host "  first local answer: $($localAnswered[0].content)" -ForegroundColor DarkGray
        }

        # ---- cost arithmetic ----------------------------------------------
        $pricing = Get-PricingFromConfig $instantiated
        Assert-That ($null -ne $pricing.Default) 'the pricing block parsed out of the loaded config (pricing.default)'
        Assert-That ($pricing.Models.Contains('local-chat')) "the loaded config prices the model every request asks for (pricing.models.'local-chat')"

        $priceIn = 0.0; $priceOut = 0.0
        if ($pricing.Models.Contains('local-chat')) {
            $priceIn = [double]$pricing.Models['local-chat']['in']
            $priceOut = [double]$pricing.Models['local-chat']['out']
        }
        $cloudIn = 0.0; $cloudOut = 0.0
        if ($null -ne $pricing.Default) {
            $cloudIn = [double]$pricing.Default['in']
            $cloudOut = [double]$pricing.Default['out']
        }

        # Per-request money, from the usage each response carried.
        $localPrompt = 0; $localCompletion = 0
        $cloudPromptTok = 0; $cloudCompletionTok = 0
        for ($i = 0; $i -lt $simpleResults.Count; $i++) {
            $row = $simpleResults[$i]
            $usd = (($row.prompt_tokens * $priceIn) + ($row.completion_tokens * $priceOut)) / 1000000.0
            $row['local_spend_usd'] = [Math]::Round($usd, 9)
            if ($row.tier -eq 'local-vllm') {
                $localPrompt += [int]$row.prompt_tokens
                $localCompletion += [int]$row.completion_tokens
            }
            else {
                $cloudPromptTok += [int]$row.prompt_tokens
                $cloudCompletionTok += [int]$row.completion_tokens
            }
        }
        for ($i = 0; $i -lt $hardResults.Count; $i++) {
            $row = $hardResults[$i]
            $usd = (($row.prompt_tokens * $priceIn) + ($row.completion_tokens * $priceOut)) / 1000000.0
            $row['local_spend_usd'] = [Math]::Round($usd, 9)
            if ($row.tier -eq 'local-vllm') {
                $localPrompt += [int]$row.prompt_tokens
                $localCompletion += [int]$row.completion_tokens
            }
            else {
                $cloudPromptTok += [int]$row.prompt_tokens
                $cloudCompletionTok += [int]$row.completion_tokens
            }
        }

        # Three DISTINCT quantities, each recomputed from its own token counts:
        #   local_list     = the locally served requests priced at the booked
        #                    (cloud list) rate -- this IS the displaced spend;
        #   cloud_served   = the cloud tier's own remaining spend;
        #   equiv_all_cloud= the whole mix at the same rate, i.e. what the bill
        #                    would have been with no local tier at all.
        # avoided is the LOCAL arm, NOT "all-cloud minus local" (that subtraction
        # yields the cloud arm and turns the saving into its complement).
        $listLocal = (($localPrompt * $priceIn) + ($localCompletion * $priceOut)) / 1000000.0
        $listCloudServed = (($cloudPromptTok * $priceIn) + ($cloudCompletionTok * $priceOut)) / 1000000.0
        $allPrompt = $localPrompt + $cloudPromptTok
        $allCompletion = $localCompletion + $cloudCompletionTok
        $equivalentAllCloud = (($allPrompt * $priceIn) + ($allCompletion * $priceOut)) / 1000000.0
        $avoided = $listLocal
        $avoidedPct = 0.0
        if ($equivalentAllCloud -gt 0) { $avoidedPct = [Math]::Round((100.0 * $avoided / $equivalentAllCloud), 2) }
        $cloudListDefault = (($allPrompt * $cloudIn) + ($allCompletion * $cloudOut)) / 1000000.0
        $cloudTierSpend = (($cloudPromptTok * $cloudIn) + ($cloudCompletionTok * $cloudOut)) / 1000000.0

        # Independent re-derivation from the raw token counts, so a sign or
        # operand mistake in the block above cannot pass unnoticed.
        $localPromptTokensRaw = 0; $localCompletionTokensRaw = 0
        $cloudPromptTokensRaw = 0; $cloudCompletionTokensRaw = 0
        foreach ($row in @($simpleResults) + @($hardResults)) {
            if ($row.tier -eq 'local-vllm') {
                $localPromptTokensRaw += [int]$row.prompt_tokens
                $localCompletionTokensRaw += [int]$row.completion_tokens
            }
            else {
                $cloudPromptTokensRaw += [int]$row.prompt_tokens
                $cloudCompletionTokensRaw += [int]$row.completion_tokens
            }
        }
        $localRecomputed = (($localPromptTokensRaw * $priceIn) + ($localCompletionTokensRaw * $priceOut)) / 1000000.0
        $allCloudRecomputed = ((($localPromptTokensRaw + $cloudPromptTokensRaw) * $priceIn) + (($localCompletionTokensRaw + $cloudCompletionTokensRaw) * $priceOut)) / 1000000.0
        $equiv_all_check = $listLocal + $listCloudServed
        $avoidedPctRecomputed = 0.0
        if ($allCloudRecomputed -gt 0) { $avoidedPctRecomputed = [Math]::Round((100.0 * $localRecomputed / $allCloudRecomputed), 2) }

        $costNote = 'pricing is by MODEL NAME (internal/gateway/pricing.go) and both tiers answered as ''local-chat'', so EVERY request is priced at pricing.models.local-chat (in ' + $priceIn + ' / out ' + $priceOut + ' per 1e6 tokens). A locally served request is free in cash but is BOOKED at that cloud list rate, so avoided_usd is a list-price DISPLACEMENT, not a bill: it is the same tokens priced at the cloud rate the gateway would have charged. avoided_usd is the local arm alone (local tokens x price); cloud_served_list_price_usd is the cloud tier''s own spend; equiv_all_cloud_usd = avoided + cloud_served is what all requests would have cost with no local tier, and avoided_percent is avoided/equiv_all_cloud.'
        Write-Host ("  cost: prices in={0} out={1} USD/1e6 (model local-chat)" -f $priceIn, $priceOut) -ForegroundColor DarkGray
        Write-Host ("  cost: local tokens prompt={0} completion={1} -> displaced cloud list spend `${2:F6}" -f $localPrompt, $localCompletion, $listLocal) -ForegroundColor DarkGray
        Write-Host ("  cost: cloud tokens prompt={0} completion={1} -> cloud tier's own spend `${2:F6}" -f $cloudPromptTok, $cloudCompletionTok, $listCloudServed) -ForegroundColor DarkGray
        Write-Host ("  cost: whole mix at local-chat list price `${0:F6}; avoided `${1:F6} ({2}% of it)" -f $equivalentAllCloud, $avoided, $avoidedPct) -ForegroundColor DarkGray
        Write-Host ("  cost: check avoided+cloud == all-cloud: {0:F9} + {1:F9} = {2:F9}" -f $avoided, $listCloudServed, ($avoided + $listCloudServed)) -ForegroundColor DarkGray
        Write-Host ("  cost: pricing.default (in={0} out={1}) -> the cloud-tier requests alone would be `${2:F6}; the whole mix `${3:F6}" -f $cloudIn, $cloudOut, $cloudTierSpend, $cloudListDefault) -ForegroundColor DarkGray

        Assert-That ($listLocal -gt 0) 'the cost of the locally served requests is a positive number' "listLocal=$listLocal (it would be 0 if the local tier had answered nothing)"
        Assert-That ($equivalentAllCloud -gt $listLocal) 'the whole mix would have cost more in the cloud than the local tier actually cost' "allCloud=$equivalentAllCloud local=$listLocal"
        Assert-That ([Math]::Abs($avoided - $localRecomputed) -lt 1e-12) 'avoided_usd equals the locally served tokens priced from the raw per-request counts' "avoided=$avoided recomputed=$localRecomputed"
        Assert-That ([Math]::Abs($equiv_all_check - $equivalentAllCloud) -lt 1e-12) 'equiv_all_cloud_usd equals the whole mix priced from the raw per-request counts' "stated=$equivalentAllCloud recomputed=$equiv_all_check"
        # avoided is the LOCAL arm: it must be the smaller part of the all-cloud
        # bill, and it must not be the cloud arm (the sign error this pins).
        Assert-That ($avoided -lt $equivalentAllCloud) 'avoided_usd is smaller than the all-cloud bill (it is a fraction, not the complement)' "avoided=$avoided allCloud=$equivalentAllCloud"
        Assert-That ([Math]::Abs($avoidedPct - $avoidedPctRecomputed) -lt 0.011) 'avoided_percent equals avoided/equiv_all_cloud to two decimals' "stated=$avoidedPct recomputed=$avoidedPctRecomputed"
        Assert-That ($avoidedPct -gt 0 -and $avoidedPct -lt 100) 'the avoided fraction is a real percentage of the mix' "avoided=$avoidedPct%"
        Assert-That ([Math]::Abs(($avoided + $listCloudServed) - $equivalentAllCloud) -lt 1e-12) 'avoided_usd + cloud_served_list_price_usd accounts for the whole all-cloud bill' "avoided=$avoided cloud=$listCloudServed allCloud=$equivalentAllCloud"
        Write-Host ("  cost check: avoided {0:F9} + cloud-served {1:F9} = {2:F9} (all-cloud)" -f $avoided, $listCloudServed, ($avoided + $listCloudServed)) -ForegroundColor DarkGray
        Assert-That ($avoidedPct -gt 0 -and $avoidedPct -lt 100) 'the avoided fraction is a real percentage of the mix' "avoided=$avoidedPct%"

        # Cross-check the gateway's own token counters against the usage the
        # responses carried: if they disagree, the cost above is built on numbers
        # the gateway does not recognise.
        $usagePrompt = $allPrompt
        $usageCompletion = $allCompletion
        Write-Host ("  /stats token delta prompt={0} completion={1} vs response usage prompt={2} completion={3}" -f $promptDelta, $completionDelta, $usagePrompt, $usageCompletion) -ForegroundColor DarkGray
        Assert-That ([Math]::Abs($promptDelta - $usagePrompt) -le 2) 'the gateway token counter agrees with the usage blocks (prompt)' "stats=$promptDelta usage=$usagePrompt"
        Assert-That ([Math]::Abs($completionDelta - $usageCompletion) -le 2) 'the gateway token counter agrees with the usage blocks (completion)' "stats=$completionDelta usage=$usageCompletion"

        # ---- per-tier latency ---------------------------------------------
        $simpleMs = @($simpleResults | ForEach-Object { [double]$_.client_ms })
        $hardMs = @($hardResults | ForEach-Object { [double]$_.client_ms })
        $allMs = @($allResults | ForEach-Object { [double]$_.client_ms })
        $simpleMin = 0.0; $simpleMax = 0.0; $hardMin = 0.0; $hardMax = 0.0
        if ($simpleMs.Count -gt 0) {
            $simpleMin = [double]($simpleMs | Measure-Object -Minimum).Minimum
            $simpleMax = [double]($simpleMs | Measure-Object -Maximum).Maximum
        }
        if ($hardMs.Count -gt 0) {
            $hardMin = [double]($hardMs | Measure-Object -Minimum).Minimum
            $hardMax = [double]($hardMs | Measure-Object -Maximum).Maximum
        }
        $tierLatency = [ordered]@{
            source              = 'client-side wall clock around curl (Invoke-Http), one tier per request'
            note                = '/stats exposes per-upstream request COUNT, mean and total seconds, but NOT per-upstream latency percentiles -- its latency p50/p90/p95/p99 block is computed over every request the gateway served. Per-tier percentiles therefore come from the client-side wall clock for each request, attributed by the X-InferGate-Upstream-Name response header.'
            local               = [ordered]@{
                upstream        = 'local-vllm'
                count           = $simpleLocal
                client_ms_p50   = Get-Percentile -Values $simpleMs -P 0.50
                client_ms_p95   = Get-Percentile -Values $simpleMs -P 0.95
                client_ms_mean  = Get-Mean -Values $simpleMs
                client_ms_min   = $simpleMin
                client_ms_max   = $simpleMax
                gateway_mean_seconds = $localMeanSeconds
                gateway_total_seconds = $localTotalSeconds
            }
            cloud               = [ordered]@{
                upstream        = 'cloud-mock'
                count           = $hardCloud
                client_ms_p50   = Get-Percentile -Values $hardMs -P 0.50
                client_ms_p95   = Get-Percentile -Values $hardMs -P 0.95
                client_ms_mean  = Get-Mean -Values $hardMs
                client_ms_min   = $hardMin
                client_ms_max   = $hardMax
                gateway_mean_seconds = $cloudMeanSeconds
                gateway_total_seconds = $cloudTotalSeconds
            }
            mix                 = [ordered]@{
                count = $allResults.Count
                client_ms_p50 = Get-Percentile -Values $allMs -P 0.50
                client_ms_p95 = Get-Percentile -Values $allMs -P 0.95
                client_ms_mean = Get-Mean -Values $allMs
            }
        }
        $statsLatency = [ordered]@{}
        if ($null -ne $statsAfter -and $null -ne $statsAfter.latency) {
            foreach ($k in @('count', 'p50', 'p90', 'p95', 'p99', 'max')) {
                $statsLatency[$k] = [string]$statsAfter.latency.$k
            }
            $statsLatency['p50_ms'] = Convert-GoDurationToMs ([string]$statsAfter.latency.p50)
            $statsLatency['p95_ms'] = Convert-GoDurationToMs ([string]$statsAfter.latency.p95)
            $statsLatency['note'] = 'gateway-side, over ALL requests the gateway served on this run (there is no per-tier latency block in /stats)'
        }
        Write-Host ("  latency: local p50 {0} ms p95 {1} ms | cloud p50 {2} ms p95 {3} ms | gateway overall p50 {4} p95 {5}" -f `
                $tierLatency.local.client_ms_p50, $tierLatency.local.client_ms_p95, $tierLatency.cloud.client_ms_p50, $tierLatency.cloud.client_ms_p95, $statsLatency['p50'], $statsLatency['p95']) -ForegroundColor White

        Assert-That ($localMeanSeconds -gt 0) 'the gateway recorded a real local-tier mean latency' "mean_seconds=$localMeanSeconds"
        Assert-That ($tierLatency.local.client_ms_p50 -gt 0) 'the local tier latency p50 is a real measurement' "p50=$($tierLatency.local.client_ms_p50) ms"
        Assert-That ($tierLatency.cloud.client_ms_p50 -gt 0) 'the cloud tier latency p50 is a real measurement' "p50=$($tierLatency.cloud.client_ms_p50) ms"

        # ---- the routing decision in the log ------------------------------
        $logText = Read-Text $gwLog
        Assert-Contains $logText 'strategy=tiered tier=local' 'the gateway log names the local tier on a decision line'
        Assert-Contains $logText 'strategy=tiered tier=cloud' 'the gateway log names the cloud tier on a decision line'
        Assert-Contains $logText 'reason=simple request prefers this tier' 'the gateway log records the simple-prefers-local reason verbatim'
        Assert-Contains $logText 'reason=hard request prefers this tier' 'the gateway log records the hard-prefers-cloud reason verbatim'

        $tieringBlock = [ordered]@{
            measured          = $true
            local_variant     = $runnerVariant
            gateway_port      = $GatewayPort
            local_tier        = [ordered]@{
                upstream = 'local-vllm'
                base_url = "http://127.0.0.1:$VllmPort"
                kind     = 'real vLLM in WSL'
                variant  = $runnerVariant
                model_dir = "/opt/models/$runnerVariant"
                served_model_name = 'local-chat'
                max_model_len = 4096
                gpu_memory_utilization = 0.85
                ready_seconds = $vllmStart.Seconds
                pid       = $vllmPid
                reported_model_id = $vllmModelId
                log       = 'tmp\m4-measure-vllm.log'
            }
            cloud_tier        = [ordered]@{
                upstream = 'cloud-mock'
                base_url = "http://127.0.0.1:$CloudPort"
                kind     = 'cmd\mockupstream (in-repo stand-in, NOT a real cloud provider)'
                note     = 'the stand-in answers instantly, so the cross-tier latency difference is a property of this stand-in plus the real local model, never a comparison with a real cloud API'
            }
            config            = [ordered]@{
                source = 'configs\tiered-local.yaml'
                instance = "tmp\m4-measure-$stamp.yaml"
                strategy = 'tiered'
                local_max_prompt_tokens = 400
                local_max_completion_tokens = 256
                cloud_capabilities = @('tools', 'vision')
                note = 'configs\ is never modified: only the literal listen port and the two upstream URLs are substituted'
            }
            mix               = [ordered]@{
                requests_total       = 2 * $Requests
                simple_per_kind      = $Requests
                hard_per_kind        = $Requests
                simple_definition    = "short prompt (~$($simpleText.Length) chars), max_tokens 96"
                hard_definition      = "$($hardText.Length) chars of user text (2400 required by the brief), max_tokens 96"
                served_locally       = $simpleLocal + $hardLocal
                served_by_cloud      = $simpleCloud + $hardCloud
                local_share_percent  = [Math]::Round((100.0 * ($simpleLocal + $hardLocal) / (2 * $Requests)), 2)
                wall_seconds         = $mixSeconds
                simple_results       = @($simpleResults | ForEach-Object { $_ })
                hard_results         = @($hardResults | ForEach-Object { $_ })
            }
            stats_series_delta = [ordered]@{
                local_vllm = $localDelta
                cloud_mock = $cloudDelta
                tokens_prompt = $promptDelta
                tokens_completion = $completionDelta
            }
            latency           = $tierLatency
            gateway_stats_latency = $statsLatency
            cost              = [ordered]@{
                prices_usd_per_1e6_tokens = [ordered]@{
                    'local-chat' = [ordered]@{ in = $priceIn; out = $priceOut }
                    default      = [ordered]@{ in = $cloudIn; out = $cloudOut }
                }
                tokens = [ordered]@{
                    local_prompt = $localPrompt
                    local_completion = $localCompletion
                    cloud_prompt = $cloudPromptTok
                    cloud_completion = $cloudCompletionTok
                    mix_prompt = $allPrompt
                    mix_completion = $allCompletion
                }
                local_list_price_usd    = [Math]::Round($listLocal, 9)
                cloud_served_list_price_usd = [Math]::Round($listCloudServed, 9)
                equiv_all_cloud_usd     = [Math]::Round($equivalentAllCloud, 9)
                avoided_usd             = [Math]::Round($avoided, 9)
                avoided_percent         = $avoidedPct
                cloud_tier_requests_at_default_price_usd = [Math]::Round($cloudTierSpend, 9)
                all_requests_at_default_price_usd = [Math]::Round($cloudListDefault, 9)
                formula = 'usd = (prompt_tokens * price_in + completion_tokens * price_out) / 1000000'
                note = $costNote
            }
        }
    }

    # -----------------------------------------------------------------------
    # 5. Artifact
    # -----------------------------------------------------------------------
    Write-Section '5. Artifact'

    $duration = [Math]::Round(((Get-Date) - $startedAt).TotalSeconds, 1)
    $comparisonRecords = @($script:comparisons)

    $summary = [ordered]@{
        generated_at = (Get-Date).ToString('s')
        host         = "$env:COMPUTERNAME / $env:PROCESSOR_IDENTIFIER / $psVersion"
        os           = [System.Environment]::OSVersion.VersionString
        duration_s   = $duration
        milestone    = 'M4 - tiered local/cloud routing with a real local model'
        host_detail  = [ordered]@{
            machine   = "$env:COMPUTERNAME"
            os        = [System.Environment]::OSVersion.VersionString
            processor = "$env:PROCESSOR_IDENTIFIER"
            powershell = $psVersion
            repo      = $repo
            script    = 'scripts\measure-m4.ps1'
            command   = 'powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m4.ps1'
            duration_s = $duration
            stamp     = $stamp
        }
        gpu          = [ordered]@{
            name              = $gpuName
            driver_version    = $driverVersion
            windows_nvidia_smi = $winGpuQuery
            windows_nvidia_smi_table = $winSmiText
            wsl_nvidia_smi    = $wslGpuQuery
            wsl_nvidia_smi_table = $gpuWslRaw
            vram_total_mb_note = 'nvidia-smi memory.used is TOTAL VRAM in use; the Windows desktop holds roughly 1.8 GiB of this 8188 MiB laptop GPU before any model loads, so a model footprint is always the DELTA and its noise floor is about 50 MiB'
        }
        runtime      = [ordered]@{
            wsl_distro = $wslDistroName
            kernel     = $wslKernel
            kernel_full = $wslKernelFull
            vllm       = $vllmVersion
            vllm_bin   = '/opt/vllm/bin/vllm'
            torch      = $torchVersion
            cuda       = $cudaVersion
            python     = $pyVersion
        }
        method       = [ordered]@{
            quantization = 'scripts/bench-vllm-quant.sh (real vLLM), one variant at a time, load -> warmup -> 12 measured streaming requests at concurrency 1 -> unload; the client is scripts/bench_vllm_quant.py'
            tiering      = 'the SAME real vLLM serves the gateway''s local tier while cmd\mockupstream stands in for the cloud tier; a known mix of simple and hard requests goes through the gateway with curl.exe and the split is read back from GET /stats'
            routing_split_source = 'GET /stats -> series[] per upstream request counts, and tokens{prompt,completion}'
            per_tier_latency_source = 'client-side wall clock per request, attributed by the X-InferGate-Upstream-Name response header (see tiering.latency.note)'
            cost_source = 'derived from the pricing block of the config that was actually loaded plus the usage block of every response; there is no cost metric on any surface'
            host_shared = 'one laptop GPU, shared with the Windows desktop, concurrency 1, a single run: these are measurements, not a benchmark suite'
        }
        environment  = [ordered]@{
            models = $modelState
        }
        quantization = [ordered]@{
            variants   = @($quantRecords)
            measured   = $measuredCount
            compared   = $comparisonRecords
            model_provenance = $provenanceBlock
            awq_wait   = $awqWaitResult
            bench_variants_run = @($benchVariants)
            skip_bench = [bool]$SkipBench
            note       = 'agreement (where present) is DRIFT against the fp16 reference text -- exact_match_rate and mean_token_overlap measure how much the quantized answer differs from fp16, NOT whether either answer is correct'
        }
        tiering      = $tieringBlock
        assertions   = [ordered]@{
            total = [int]$script:assertTotal
            failed_count = $script:assertFails.Count
            failed = @($script:assertFails)
            not_measured = @($script:failures)
            note = 'written before the final artifact re-parse assertion, then rewritten with the final counts'
        }
        limitations  = @(
            'The cloud tier is cmd\mockupstream, an in-repo stand-in that answers instantly: the cross-tier latency difference is NOT a comparison with a real cloud provider, and no money was spent on this run.',
            '"agreement" between AWQ/GPTQ and fp16 is textual DRIFT from the fp16 reference (exact match rate, mean token overlap). It is not an accuracy, quality or correctness score; no answer was graded.',
            'quantization is a single run per variant at concurrency 1 with 12 measured requests after warmup. That gives a difference, not a distribution, and there is no repeat to estimate run-to-run noise.',
            'nvidia-smi reports TOTAL VRAM in use on a laptop GPU that the Windows desktop is also using (~1.8 GiB of 8188 MiB before any model loads). Every model footprint here is the DELTA, and the delta carries a noise floor of roughly 50 MiB.',
            'Each variant''s "before load" VRAM baseline is a single reading taken right after the previous variant was unloaded, so those baselines differ between variants (the desktop allocates and frees VRAM on its own schedule). The deltas are therefore comparable only within their own noise floor, and the raw baseline and after-load readings are reported next to each delta so the arithmetic can be checked.',
            'vLLM was started with --gpu-memory-utilization 0.85, which reserves KV-cache VRAM beyond the weights; the VRAM delta is that reservation plus the weights, not the checkpoint size on disk.',
            'The tiering mix is 2N requests of two fixed shapes at concurrency 1. It proves the local/cloud classification and the per-tier split for THAT mix; it says nothing about behaviour under load, with streaming, or at the token-estimate boundary.',
            'Cost is DERIVED, not billed: there is no cost metric on any surface, so it is computed from the pricing block of the loaded config and the usage block of the responses. The convention (pricing by model name, and therefore list price for a locally served request) is stated in tiering.cost.note.',
            'The gateway estimates prompt tokens as len(serialised messages)/4 -- a character proxy, not a tokenizer -- so the simple/hard boundary is the gateway''s estimate, not a real token count.',
            'Per-tier latency p50/p95 come from the client-side wall clock because /stats has no per-upstream latency block; they include gateway overhead plus loopback, and for the local tier they include the real model''s generation time.',
            'Everything runs on loopback on one host that may also be running other gates; the two tiers were exercised sequentially, not simultaneously.'
        )
    }
    if ($awqWaitResult -ne 'not attempted') {
        $summary['environment']['awq_wait'] = $awqWaitResult
    }
    $runLog = @($script:log)
    $summary['run_log'] = $runLog

    Write-JsonFile $artifact $summary
    $parsed = Get-Json (Read-Text $artifact)
    Assert-That ($null -ne $parsed) 'the artifact parses back as JSON'
    Assert-That ($parsed.milestone -match '^M4') 'the artifact names the milestone'
    Assert-That ($parsed.quantization.variants.Count -eq 3) 'the artifact carries a record for all three quantization variants'
    # The counts can only be final now: the assertion above is itself one of them.
    $summary['assertions']['total'] = [int]$script:assertTotal
    $summary['assertions']['failed_count'] = $script:assertFails.Count
    $summary['assertions']['failed'] = @($script:assertFails)
    $summary['assertions']['not_measured'] = @($script:failures)
    $summary['run_log'] = @($script:log)
    Write-JsonFile $artifact $summary
    $reparsed = Get-Json (Read-Text $artifact)
    Assert-That ($null -ne $reparsed) 'the artifact still parses after the final rewrite'

    # -----------------------------------------------------------------------
    # 6. Headline
    # -----------------------------------------------------------------------
    Write-Section 'M4 HEADLINE'

    foreach ($rec in $quantRecords) {
        if ($rec.measured) {
            Write-Host ("  {0,-5} ttft p50 {1,8} ms / p95 {2,8} ms | total p50 {3,9} ms | {4,7} tok/s | load {5,5} s | vram delta {6,6} MiB | n={7} errors={8}" -f `
                    $rec.variant, $rec.ttft_ms.p50, $rec.ttft_ms.p95, $rec.total_ms.p50, $rec.throughput.output_tokens_per_s_request_wall, $rec.load_seconds, $rec.vram_delta_mb, $rec.n, $rec.errors)
        }
        else {
            Write-Host ("  {0,-5} NOT MEASURED: {1}" -f $rec.variant, $rec.reason) -ForegroundColor Yellow
        }
    }
    foreach ($cmp in $comparisonRecords) {
        if ($cmp.comparable -ne $false) {
            Write-Host ("  {0,-5} vs fp16: ttft {1} vs {2} ms | {3} vs {4} tok/s ({5}x) | vram {6} vs {7} MiB | agreement {8}" -f `
                    $cmp.variant, $cmp.ttft_p50_ms.variant, $cmp.ttft_p50_ms.fp16, $cmp.output_tokens_per_s.variant, $cmp.output_tokens_per_s.fp16, $cmp.output_tokens_per_s.ratio, $cmp.vram_delta_mb.variant, $cmp.vram_delta_mb.fp16, $cmp.agreement_vs_fp16)
        }
    }
    if ($null -ne $tieringBlock -and $tieringBlock.measured) {
        Write-Host ("  tiering: {0} simple -> local, {1} hard -> cloud (all {2} requests accounted for); {3}% of the mix was served locally" -f `
                $tieringBlock.mix.simple_per_kind, $tieringBlock.mix.hard_per_kind, $tieringBlock.mix.requests_total, $tieringBlock.mix.local_share_percent)
        Write-Host ("  latency: local p50 {0} ms p95 {1} ms | cloud-stand-in p50 {2} ms p95 {3} ms" -f `
                $tieringBlock.latency.local.client_ms_p50, $tieringBlock.latency.local.client_ms_p95, $tieringBlock.latency.cloud.client_ms_p50, $tieringBlock.latency.cloud.client_ms_p95)
        Write-Host ("  cost: locally served {0} prompt + {1} completion tokens = `${2:F6} of displaced cloud list spend; the whole mix in the cloud would be `${3:F6}; avoided {4}%" -f `
                $tieringBlock.cost.tokens.local_prompt, $tieringBlock.cost.tokens.local_completion, $tieringBlock.cost.local_list_price_usd, $tieringBlock.cost.equiv_all_cloud_usd, $tieringBlock.cost.avoided_percent)
    }
    else {
        Write-Host '  tiering: not measured on this run' -ForegroundColor Yellow
    }
    Write-Host ("  assertions: {0} run, {1} failed, {2} unmeasured" -f $script:assertTotal, $script:assertFails.Count, $script:failures.Count)
    Write-Host "  artifact: $artifact"
    $exitOk = (($script:assertFails.Count -eq 0) -and ($script:failures.Count -eq 0))
}
finally {
    # -----------------------------------------------------------------------
    # 7. Teardown -- only what this script started
    # -----------------------------------------------------------------------
    Write-Section '7. Teardown'

    if ($KeepRunning) {
        Write-Host '  -KeepRunning set: the fleet (and the WSL vLLM) is left running for inspection.' -ForegroundColor DarkYellow
        if ($vllmPid) { Write-Host "  vllm pid=$vllmPid in WSL, gateway pid=$($script:gwProc.Id), mock pid=$($script:mockProc.Id)" -ForegroundColor DarkYellow }
    }
    else {
        # The vLLM front end forks an EngineCore child; the stop script walks the
        # process tree of the pid THIS script started, and nothing else.
        if ($vllmPid) {
            $stopOut = Stop-VllmInWsl -Port $VllmPort
            $gone = Wait-PortClosed -Port $VllmPort -Seconds 20
            Assert-That $gone "the vLLM pid this script started is gone and :$VllmPort no longer answers" $stopOut
            $stillAnswers = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', '--max-time', '5', "http://127.0.0.1:$VllmPort/v1/models")).Trim()
            Assert-That ($stillAnswers -ne '200') 'GET /v1/models on the local tier no longer answers 200' "http=$stillAnswers"
        }
        else {
            Write-Host '  no vLLM was started by this run' -ForegroundColor DarkGray
        }

        Stop-Tracked -Proc $script:gwProc -Label 'gateway'
        Stop-Tracked -Proc $script:mockProc -Label 'cloud mock'

        $deadline = (Get-Date).AddSeconds(5)
        $tracked = @($script:startedPids | ForEach-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
        while ((Get-Date) -lt $deadline -and $tracked.Count -gt 0) {
            Start-Sleep -Milliseconds 150
            $tracked = @($script:startedPids | ForEach-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
        }
        # Get-CimInstance Win32_Process is denied by the sandbox, so this asks
        # Get-Process about the pids recorded when they were started -- which is
        # why the first assertion matters: it checks that every process this run
        # actually started (the mock and/or the gateway) was recorded. With
        # -SkipTiering nothing is started and the check would compare 0 with 0,
        # so it is skipped there instead of being reported as a failure.
        if ($script:upstreamStarted -gt 0) {
            Assert-That ($script:startedPids.Count -ge $script:upstreamStarted) 'the script recorded every process it started' "recorded=$($script:startedPids.Count) started=$($script:upstreamStarted): $($script:startedPids -join ', ')"
        }
        else {
            Write-Host '  no gateway or mock process was started on this run: nothing to track' -ForegroundColor DarkGray
        }
        $alive = @($script:startedPids | ForEach-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
        Assert-That ($alive.Count -eq 0) 'no process started by this script survived it' (($alive | ForEach-Object { "$($_.ProcessName) pid=$($_.Id)" }) -join '; ')
        if ($tieringRan) {
            foreach ($port in @($GatewayPort, $CloudPort)) {
                Assert-That (Wait-PortClosed -Port $port -Seconds 5) "nothing is left listening on :$port" "port :$port still accepts connections"
            }
        }
        else {
            Write-Host '  -SkipTiering: the gateway and mock were never started, so their ports are not asserted.' -ForegroundColor DarkGray
        }
        $gwErrText = (Read-Text $gwErr).Trim()
        Assert-That ($gwErrText.Length -eq 0) 'the gateway wrote no stderr during the run' "stderr: $gwErrText"
    }

    # The tmp config is derived from configs\ and can be regenerated, so it is
    # removed either way; leaving copies behind is how "which config was that?"
    # becomes unanswerable a week later.
    foreach ($p in @($configPath, $startSh, $waitSh, $benchSh)) {
        Remove-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue
    }
    $leftoverConfig = @(Get-ChildItem -Path $tmpDir -Filter "m4-measure-$stamp*" -ErrorAction SilentlyContinue)
    Assert-That ($leftoverConfig.Count -eq 0) 'this run left no tmp config or script behind' (($leftoverConfig | ForEach-Object { $_.Name }) -join '; ')

    Write-Host ''
    if ($exitOk) {
        Write-Host "RESULT: M4 measured cleanly, $([int]$script:assertTotal) assertions passed" -ForegroundColor Green
        Write-Host "RESULT: artifact written to $artifact" -ForegroundColor Green
    }
    else {
        Write-Host "RESULT: M4 measurement did NOT complete cleanly ($($script:assertFails.Count) failed assertion(s), $($script:failures.Count) unmeasured step(s))" -ForegroundColor Yellow
        if ($script:assertFails.Count -gt 0) { Write-Host ("  failed: " + ($script:assertFails -join '; ')) -ForegroundColor Yellow }
        if ($script:failures.Count -gt 0) { Write-Host ("  not measured: " + ($script:failures -join '; ')) -ForegroundColor Yellow }
        exit 1
    }
}
