# InferGate M4 -- end-to-end verification of TIERED local/cloud routing with curl.exe.
#
# M4 is the claim that a request the local box can serve never pays cloud prices,
# and that the request the local box cannot serve is not silently mis-routed to
# it. Neither half is visible in a status code: a 200 from the wrong tier is
# indistinguishable from a 200 from the right one unless you read the gateway's
# own routing evidence. So this script drives a real two-replica fleet with the
# real binaries and asserts on:
#
#   X-InferGate-Upstream-Name   which tier actually answered
#   X-InferGate-Tried           present only when failover happened, so absence
#                               after a simple request is itself the assertion
#   /admin/upstreams            how the config resolved (strategy, tiers)
#   /stats and /metrics         the per-tier request split and the token split,
#                               compared as DELTAS against a known mix
#   the gateway log             the literal tierReason string, read out of
#                               internal/router/router.go rather than guessed
#
# The config under test is configs/tiered-local.yaml with ONLY its literal ports
# substituted: the local tier is the in-repo mock upstream (no GPU, no network)
# so the whole tiered story is reproducible on this host. configs/ is never
# edited -- the copy lands in tmp\ and is removed on the way out.
#
# Usage (pwsh does not exist on this host):
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m4.ps1
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m4.ps1 -KeepRunning
#
# Every tmp path carries the run's timestamp and pid: two gates running at once
# each get their own config, bodies and logs, instead of overwriting each
# other's evidence.

[CmdletBinding()]
param(
    [int]$GatewayPort = 18310,
    [int]$LocalPort = 19610,
    [int]$CloudPort = 19611,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'

$repo = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
$goShim = Join-Path $repo 'tools\go.cmd'
$baseConfig = Join-Path $repo 'configs\tiered-local.yaml'

$stamp = "$(Get-Date -Format 'yyyyMMdd-HHmmss')-$PID"
$configPath = Join-Path $tmpDir "m4-verify-gateway-$stamp.yaml"
$gwLog = Join-Path $tmpDir "m4-gateway-$stamp.log"
$gwErr = Join-Path $tmpDir "m4-gateway-$stamp.err"
$localLog = Join-Path $tmpDir "m4-local-vllm-$stamp.log"
$localErr = Join-Path $tmpDir "m4-local-vllm-$stamp.err"
$cloudLog = Join-Path $tmpDir "m4-cloud-mock-$stamp.log"
$cloudErr = Join-Path $tmpDir "m4-cloud-mock-$stamp.err"
$buildLog = Join-Path $tmpDir "m4-build-$stamp.log"
$checkErrFile = Join-Path $tmpDir "m4-check-$stamp.err"
$checkOutFile = Join-Path $tmpDir "m4-check-$stamp.out"
$variantNames = @('bad-tier', 'no-local', 'negative-prompt')

$gwUrl = "http://127.0.0.1:$GatewayPort"
$ports = @($GatewayPort, $LocalPort, $CloudPort)

$script:passed = 0
$script:failed = 0
$script:notes = @()
$script:startedPids = @()
$script:gwProc = $null
$script:localProc = $null
$script:cloudProc = $null
$script:utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$script:variantPaths = @{}

# The price reader is shared with scripts\measure-m4.ps1: section 12 derives money
# from the prices the config under test carries, and both scripts have to read
# them the same way.
. (Join-Path $PSScriptRoot 'lib\pricing.ps1')

# The tier limits are read out of the config under test with the same reader the
# M1 and M6 gates use, so section 11 compares the admin echo against the policy
# the gateway actually loaded instead of against a second copy of the numbers.
. (Join-Path $PSScriptRoot 'lib\config-scalar.ps1')

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

function Write-Section([string]$Title) {
    Write-Host ''
    Write-Host ("=" * 72) -ForegroundColor DarkGray
    Write-Host "  $Title" -ForegroundColor Cyan
    Write-Host ("=" * 72) -ForegroundColor DarkGray
}

function Assert-True {
    param([string]$Label, [bool]$Condition, [string]$Detail = '')
    if ($Condition) {
        Write-Host "  PASS  $Label" -ForegroundColor Green
        $script:passed++
    }
    else {
        Write-Host "  FAIL  $Label" -ForegroundColor Red
        if ($Detail) { Write-Host "        $Detail" -ForegroundColor DarkYellow }
        $script:failed++
    }
}

function Assert-That {
    param([string]$Label, [bool]$Condition, [string]$Detail = '')
    Assert-True $Label $Condition $Detail
}

function Assert-Equal {
    param([string]$Label, $Expected, $Actual, [string]$Extra = '')
    Assert-True $Label ($Expected -eq $Actual) "expected '$Expected', got '$Actual'$Extra"
}

function Assert-Contains {
    param([string]$Label, [string]$Haystack, [string]$Needle)
    # Literal substring test, NOT -like: -like applies wildcard matching, so a
    # needle containing brackets becomes a character class and reports false
    # failures against text that demonstrably contains it.
    $detail = "expected to find: $Needle"
    if ($Haystack) { $detail += "`n        actual: $($Haystack.Substring(0, [Math]::Min(400, $Haystack.Length)))" }
    Assert-True $Label ($null -ne $Haystack -and $Haystack.Contains($Needle)) $detail
}

function Assert-Number {
    param([string]$Label, [string]$Text, [double]$Min = -1, [double]$Max = -1)
    $value = 0.0
    $ok = [double]::TryParse($Text, [ref]$value)
    $inRange = $ok
    if ($ok -and $Min -ge 0) { $inRange = $inRange -and ($value -ge $Min) }
    if ($ok -and $Max -ge 0) { $inRange = $inRange -and ($value -le $Max) }
    Assert-True $Label $inRange "not a number in range: '$Text' (min=$Min max=$Max)"
}

# A finding worth reporting that is deliberately NOT a failure: it is recorded,
# counted once so the assertion total stays deterministic, and printed loudly so
# a green run cannot hide it.
function Add-Note {
    param([string]$Text)
    $script:notes += $Text
    Write-Host "  NOTE  $Text" -ForegroundColor Magenta
}

function Note-Ok {
    param([string]$Label, [string]$Text)
    Add-Note $Text
    Assert-True $Label $true
}

# ---------------------------------------------------------------------------
# HTTP helpers -- bodies through files, headers through -D, never -i
# ---------------------------------------------------------------------------

function Invoke-Curl {
    param([string[]]$Arguments)
    # stderr is dropped: a native command that writes to stderr raises
    # NativeCommandError, which $ErrorActionPreference='Stop' turns into a
    # terminating error that would abort the whole run.
    return (& curl.exe @Arguments 2>$null | Out-String)
}

function Read-Text {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $reader = New-Object System.IO.StreamReader($Path, $script:utf8NoBom, $true)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
}

function Read-OpenLog {
    param([string]$Path)
    # The gateway holds its log open for the life of the process, and
    # Get-Content reports that as an error. Opening with FileShare.ReadWrite
    # reads what is there without asking the writer to stop.
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    $reader = New-Object System.IO.StreamReader($stream, $script:utf8NoBom, $true)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose(); $stream.Dispose() }
}

function Get-FileLength {
    param([string]$Path)
    # Byte length of a file another process holds open. Used as a log
    # watermark: everything before the offset predates the phase under test.
    try {
        $fi = Get-Item -LiteralPath $Path -ErrorAction Stop
        return [int64]$fi.Length
    } catch {
        return [int64]0
    }
}

function Get-OpenLogTail {
    param([string]$Path, [int64]$Offset = 0)
    # Read only what was appended to an open log after $Offset bytes.
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    try {
        if ($Offset -gt 0) {
            if ($Offset -ge $stream.Length) { return '' }
            [void]$stream.Seek($Offset, [System.IO.SeekOrigin]::Begin)
        }
        $reader = New-Object System.IO.StreamReader($stream, $script:utf8NoBom, $true)
        try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
    } finally { $stream.Dispose() }
}

function New-TmpPath {
    param([string]$Tag)
    return (Join-Path $tmpDir "m4-$Tag-$stamp.txt")
}

function New-BodyFile {
    param([string]$Name, [string]$Json)
    $path = New-TmpPath "body-$Name.json"
    [System.IO.File]::WriteAllText($path, $Json, $script:utf8NoBom)
    return "@$path"
}

function Get-Header {
    param([string]$Headers, [string]$Name)
    if (-not $Headers) { return '' }
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
    [System.IO.File]::WriteAllText($bodyPath, '', $script:utf8NoBom)
    [System.IO.File]::WriteAllText($hdrPath, '', $script:utf8NoBom)

    $args = @('-s', '-o', $bodyPath, '-D', $hdrPath, '-w', '%{http_code}', '-X', $Method, $Url)
    foreach ($h in $Headers) { $args += @('-H', $h) }
    if ($BodyFile) { $args += @('--data-binary', $BodyFile) }

    $raw = Invoke-Curl $args
    $text = ($raw -replace '\s+$', '')
    return [pscustomobject]@{
        Status     = $text
        StatusCode = 0
        Headers    = (Read-Text $hdrPath)
        Body       = (Read-Text $bodyPath)
        BodyPath   = $bodyPath
        HeaderPath = $hdrPath
    }
}

# The header parameter MUST be spelled $Headers: PowerShell 5.1 silently drops an
# unknown named argument on a plain function, so a caller passing -Headers @{...}
# to a function that does not declare it sends the request with no headers at all
# and every header assertion then fails for a reason that has nothing to do with
# the gateway.
function Send-Chat {
    param(
        [string]$Base,
        [string]$BodyFile,
        [string[]]$Headers = @(),
        [string]$Tag = 'chat'
    )
    $all = @('-H', 'Content-Type: application/json') + $Headers
    return Invoke-Http -Url "$Base/v1/chat/completions" -Method 'POST' -Headers $all -BodyFile $BodyFile
}

function Get-Json {
    param([string]$Text)
    if (-not $Text) { return $null }
    try { return ($Text | ConvertFrom-Json) } catch { return $null }
}

# ---------------------------------------------------------------------------
# Process helpers
# ---------------------------------------------------------------------------

function Test-PortOpen {
    param([int]$Port, [int]$TimeoutMs = 400)
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $task = $client.ConnectAsync('127.0.0.1', $Port)
        if ($task.Wait($TimeoutMs)) { return $client.Connected }
        return $false
    }
    catch { return $false }
    finally { $client.Close() }
}

function Wait-PortOpen {
    param([int]$Port, [int]$Seconds = 10)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (Test-PortOpen -Port $Port) { return $true }
        Start-Sleep -Milliseconds 100
    }
    return $false
}

function Wait-PortClosed {
    param([int]$Port, [int]$Seconds = 3)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (-not (Test-PortOpen -Port $Port)) { return $true }
        Start-Sleep -Milliseconds 100
    }
    return $false
}

function Wait-Healthy {
    param([int]$Port, [string]$Path = '/healthz', [int]$Seconds = 15)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        $code = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$Port$Path")
        if ($code -match '200') { return $true }
        Start-Sleep -Milliseconds 150
    }
    return $false
}

function Get-TrackedProcess {
    return @($script:startedPids | ForEach-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
}

function Start-MockUpstream {
    param([int]$Port, [string]$Name, [string]$Tag)
    $log = New-TmpPath "replica-$Tag.log"
    $err = New-TmpPath "replica-$Tag.err"
    $proc = Start-Process -FilePath (Join-Path $binDir 'mockupstream.exe') `
        -ArgumentList @('-listen', ":$Port", '-name', $Name, '-token-delay', '1ms') `
        -RedirectStandardOutput $log -RedirectStandardError $err -PassThru -WindowStyle Hidden
    $script:startedPids += $proc.Id
    return [pscustomobject]@{ Proc = $proc; Log = $log; Err = $err; LogPath = $log }
}

function Start-Gateway {
    $proc = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $configPath) `
        -RedirectStandardOutput $gwLog -RedirectStandardError $gwErr -PassThru -WindowStyle Hidden
    $script:startedPids += $proc.Id
    return $proc
}

function Stop-Tracked {
    param($Proc, [string]$Label)
    if ($Proc -and -not $Proc.HasExited) {
        Stop-Process -Id $Proc.Id -Force -ErrorAction SilentlyContinue
        Write-Host "  stopped $Label (pid=$($Proc.Id))" -ForegroundColor DarkGray
    }
    elseif ($Proc) {
        Write-Host "  $Label (pid=$($Proc.Id)) had already exited" -ForegroundColor DarkGray
    }
}

# ---------------------------------------------------------------------------
# Config helpers -- configs/ is read-only, so every instance is a tmp copy
# ---------------------------------------------------------------------------

function New-ConfigVariant {
    param([string]$Name, [string]$Text)
    $path = Join-Path $tmpDir "m4-verify-$Name-$stamp.yaml"
    [System.IO.File]::WriteAllText($path, $Text, $script:utf8NoBom)
    $script:variantPaths[$Name] = $path
    return $path
}

# Native stderr must NOT be captured with 2>&1 or 2>$file inside a PowerShell
# pipeline: PowerShell wraps the output of a native command that wrote to stderr
# in a NativeCommandError ErrorRecord, and $ErrorActionPreference='Stop' promotes
# that record to a terminating error before the exit code can be read -- so a
# config the gate EXPECTS to be rejected would abort the whole run instead of
# being asserted on. Start-Process redirects both streams to files and hands back
# the exit code as data.
function Invoke-Check {
    param([string]$Config)
    [System.IO.File]::WriteAllText($checkOutFile, '', $script:utf8NoBom)
    [System.IO.File]::WriteAllText($checkErrFile, '', $script:utf8NoBom)
    $proc = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $Config, '-check') `
        -RedirectStandardOutput $checkOutFile -RedirectStandardError $checkErrFile -PassThru -Wait
    $text = (Read-Text $checkOutFile) + (Read-Text $checkErrFile)
    return [pscustomobject]@{ Exit = $proc.ExitCode; Text = $text }
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

function Get-StatCount {
    param([string]$Text, [string]$Upstream)
    $obj = Get-Json $Text
    if (-not $obj) { return -1 }
    $sum = 0
    foreach ($row in @($obj.series)) {
        if ($row.upstream -eq $Upstream) { $sum += [int]$row.count }
    }
    return $sum
}

# ---------------------------------------------------------------------------
# Preamble
# ---------------------------------------------------------------------------

Write-Host ''
Write-Host ("=" * 72) -ForegroundColor DarkGray
Write-Host '  InferGate M4 -- tiered local/cloud routing (curl-level acceptance gate)' -ForegroundColor White
Write-Host ("=" * 72) -ForegroundColor DarkGray
Write-Host "  repo      : $repo"
Write-Host "  stamp     : $stamp"
Write-Host "  ports     : gateway=:$GatewayPort local=:$LocalPort cloud=:$CloudPort"
Write-Host "  gateway   : $gwUrl"
Write-Host "  config    : $configPath"

$tierLocalReason = 'strategy=tiered tier=local reason=simple request prefers this tier'
$tierCloudReason = 'strategy=tiered tier=cloud reason=hard request prefers this tier'
$tierCloudFallback = 'strategy=tiered tier=cloud reason=simple request falls back to this tier'

$simpleBody = '{"model":"local-chat","messages":[{"role":"user","content":"hello there"}]}'
$maxBody = '{"model":"local-chat","messages":[{"role":"user","content":"hi"}],"max_tokens":512}'
$edgeBody = '{"model":"local-chat","messages":[{"role":"user","content":"hi"}],"max_tokens":256}'
$overBody = '{"model":"local-chat","messages":[{"role":"user","content":"hi"}],"max_tokens":257}'
$toolsBody = '{"model":"local-chat","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}'

# The router sizes a prompt as len(serialised message array) / 4 with integer
# division, so the local/cloud boundary sits at 1604 bytes: 1600..1603 estimates
# to exactly 400 (local, because classification is strictly '>') and 1604
# estimates to 401 (cloud). Both boundary bodies are DERIVED here by padding and
# then measured, rather than hand-tuned to a number that only worked once.
$boundaryChecker = {
    param([string]$Json)
    $idx = $Json.IndexOf('"messages"')
    if ($idx -lt 0) { return -1 }
    $open = $Json.IndexOf('[', $idx)
    if ($open -lt 0) { return -1 }
    $depth = 0
    for ($i = $open; $i -lt $Json.Length; $i++) {
        $ch = $Json[$i]
        if ($ch -eq '[') { $depth++ }
        elseif ($ch -eq ']') {
            $depth--
            if ($depth -eq 0) { return ($i - $open + 1) }
        }
    }
    return -1
}

$fillerBase = 'z' * 1500
$boundaryAt400 = -1
$boundaryAbove = -1

try {
    # -----------------------------------------------------------------------
    # 0. Preconditions
    # -----------------------------------------------------------------------
    Write-Section '0. Preconditions'

    $psVersion = $PSVersionTable.PSVersion.ToString()
    Write-Host "  host      : $([System.Environment]::MachineName) / $([System.Environment]::OSVersion.VersionString)"
    Write-Host "  powershell: $psVersion"
    Write-Host "  go shim   : $goShim"

    Assert-True 'the gate runs under Windows PowerShell 5.1 (no PS7 syntax is used)' `
        ($PSVersionTable.PSVersion.Major -eq 5) "found $psVersion"
    Assert-True 'tools\go.cmd exists (go is not on PATH on this host)' (Test-Path -LiteralPath $goShim)
    Assert-True 'configs\tiered-local.yaml exists and is the config under test' (Test-Path -LiteralPath $baseConfig)
    Assert-True 'cmd\mockupstream exists as the stand-in for both tiers' `
        (Test-Path -LiteralPath (Join-Path $repo 'cmd\mockupstream\main.go'))

    foreach ($port in $ports) {
        $busy = Test-PortOpen -Port $port
        Assert-True "port :$port is free before the run" (-not $busy) `
            "something is already listening on :$port -- stop it or pick another port"
    }

    if (-not (Test-Path -LiteralPath $tmpDir)) { New-Item -ItemType Directory -Path $tmpDir | Out-Null }

    # -----------------------------------------------------------------------
    # 1. Build
    # -----------------------------------------------------------------------
    Write-Section '1. Build the two binaries'

    $gwExe = Join-Path $binDir 'infergate.exe'
    $mockExe = Join-Path $binDir 'mockupstream.exe'

    if (-not (Test-Path -LiteralPath $binDir)) { New-Item -ItemType Directory -Path $binDir | Out-Null }
    $gwBuilt = Invoke-GoBuild -Out $gwExe -Pkg '.\cmd\infergate'
    $mockBuilt = Invoke-GoBuild -Out $mockExe -Pkg '.\cmd\mockupstream'

    Assert-True 'go build produced bin\infergate.exe' ($gwBuilt -and (Test-Path -LiteralPath $gwExe))
    Assert-True 'go build produced bin\mockupstream.exe' ($mockBuilt -and (Test-Path -LiteralPath $mockExe))

    # -----------------------------------------------------------------------
    # 2. Config surface
    # -----------------------------------------------------------------------
    Write-Section '2. Config surface (the tiered schema validates, typos do not)'

    # Instantiate the config under test by substituting ONLY the literal ports and
    # upstream URLs; configs\ is never modified.
    $configText = Read-Text $baseConfig
    Assert-True 'the config under test was read (UTF-8, em-dashes intact)' ($configText.Length -gt 1000) `
        "read $($configText.Length) characters from $baseConfig"

    $instantiated = $configText -replace '(?m)^(\s*listen:\s*)":8080"', ('${1}":' + $GatewayPort + '"')
    $instantiated = $instantiated -replace 'http://127\.0\.0\.1:8000', "http://127.0.0.1:$LocalPort"
    $instantiated = $instantiated -replace 'http://127\.0\.0\.1:9100', "http://127.0.0.1:$CloudPort"
    [System.IO.File]::WriteAllText($configPath, $instantiated, $script:utf8NoBom)

    Assert-True 'the tmp config carries the substituted gateway port' `
        ($instantiated.Contains(":$GatewayPort"))
    Assert-True 'the tmp config points the local tier at the local replica port' `
        ($instantiated.Contains("127.0.0.1:$LocalPort"))
    Assert-True 'the tmp config points the cloud tier at the cloud replica port' `
        ($instantiated.Contains("127.0.0.1:$CloudPort"))
    Assert-True 'no reference to the shipped :8080 listen address survives' `
        (-not ($instantiated -match '(?m)^\s*listen:\s*":8080"'))

    # Negative variants, each mutating exactly the one thing under test.
    $badTier = $instantiated -replace 'tier: "local"', 'tier: "edge"'
    $noLocal = $instantiated.Replace('tier: "local"', 'tier: "cloud"')
    $negative = $instantiated -replace 'local_max_prompt_tokens: 400', 'local_max_prompt_tokens: -1'
    New-ConfigVariant -Name 'bad-tier' -Text $badTier | Out-Null
    New-ConfigVariant -Name 'no-local' -Text $noLocal | Out-Null
    New-ConfigVariant -Name 'negative-prompt' -Text $negative | Out-Null

    $goodCheck = Invoke-Check -Config $configPath
    Assert-Equal '-check accepts the instantiated tiered config (exit 0)' 0 $goodCheck.Exit `
        " output: $((($goodCheck.Text -replace '\s+', ' ')).Trim())"
    Assert-Contains '-check reports the upstream count it accepted' $goodCheck.Text 'configuration OK: 2 upstream(s)'

    $badCheck = Invoke-Check -Config $script:variantPaths['bad-tier']
    Assert-True '-check rejects tier: "edge" with a non-zero exit' ($badCheck.Exit -ne 0) "exit=$($badCheck.Exit)"
    Assert-Contains '-check names the unsupported tier' $badCheck.Text 'unsupported tier'
    Assert-Contains '-check quotes the offending tier value' $badCheck.Text '"edge" (want local or cloud)'

    $noLocalCheck = Invoke-Check -Config $script:variantPaths['no-local']
    Assert-True '-check rejects strategy tiered with no local upstream' ($noLocalCheck.Exit -ne 0) "exit=$($noLocalCheck.Exit)"
    Assert-Contains '-check states the local-tier requirement' $noLocalCheck.Text 'requires at least one upstream with tier: local'

    $negCheck = Invoke-Check -Config $script:variantPaths['negative-prompt']
    Assert-True '-check rejects a negative local_max_prompt_tokens' ($negCheck.Exit -ne 0) "exit=$($negCheck.Exit)"
    Assert-Contains '-check states that the tier limit must not be negative' $negCheck.Text 'must not be negative'


    # -----------------------------------------------------------------------
    # 3. Fleet up
    # -----------------------------------------------------------------------
    Write-Section '3. Fleet up (two tier replicas + the gateway)'

    $local = Start-MockUpstream -Port $LocalPort -Name 'local-vllm' -Tag "local-$LocalPort"
    $script:localProc = $local.Proc
    $cloud = Start-MockUpstream -Port $CloudPort -Name 'cloud-mock' -Tag "cloud-$CloudPort"
    $script:cloudProc = $cloud.Proc

    $localHealthy = Wait-Healthy -Port $LocalPort -Seconds 15
    $cloudHealthy = Wait-Healthy -Port $CloudPort -Seconds 15
    Assert-True "the local tier replica answers /healthz on :$LocalPort" $localHealthy
    Assert-True "the cloud tier replica answers /healthz on :$CloudPort" $cloudHealthy
    # The replica logs are held open by the replica processes, so they are read
    # through the shared-read helper rather than a plain open.
    Assert-Contains 'the local replica reports the name the config uses' (Read-OpenLog $local.Log) 'name=local-vllm'
    Assert-Contains 'the cloud replica reports the name the config uses' (Read-OpenLog $cloud.Log) 'name=cloud-mock'

    $script:gwProc = Start-Gateway
    $gwHealthy = Wait-Healthy -Port $GatewayPort -Seconds 20
    Assert-True "the gateway answers /healthz on :$GatewayPort" $gwHealthy
    if (-not $gwHealthy) {
        Write-Host (Read-Text $gwErr) -ForegroundColor DarkYellow
        Write-Host (Read-Text $gwLog) -ForegroundColor DarkYellow
    }
    Assert-Contains 'the gateway log names both tiers it loaded' (Read-OpenLog $gwLog) 'upstreams=local-vllm,cloud-mock'

    # Watermarks: the replica logs are shared and keep growing for the whole
    # run, so section 8 must count only the lines appended after this point.
    $localMark = Get-FileLength $local.Log
    $cloudMark = Get-FileLength $cloud.Log

    # -----------------------------------------------------------------------
    # 4. A simple request prefers the local tier
    # -----------------------------------------------------------------------
    Write-Section '4. Simple request -> local tier'

    $simpleBodyFile = New-BodyFile -Name 'simple' -Json $simpleBody
    $simple = Send-Chat -Base $gwUrl -BodyFile $simpleBodyFile -Headers @('X-InferGate-Capabilities: chat') -Tag 'simple'
    Assert-Equal 'a simple chat request answers 200' '200' $simple.Status
    Assert-Equal 'the local tier served the simple request' 'local-vllm' (Get-Header $simple.Headers 'X-InferGate-Upstream-Name')
    Assert-Equal 'the simple request was a single attempt' '1' (Get-Header $simple.Headers 'X-InferGate-Attempt')
    Assert-Equal 'X-InferGate-Tried is ABSENT for a one-attempt request' '' (Get-Header $simple.Headers 'X-InferGate-Tried')
    Assert-Equal 'the simple response body is a completion' 'chat.completion' ((Get-Json $simple.Body).object)

    # -----------------------------------------------------------------------
    # 5. Hard by prompt size -> cloud tier
    # -----------------------------------------------------------------------
    Write-Section '5. Hard by prompt size -> cloud tier'

    # ~2464 bytes of body: far enough past the 1604-byte estimate boundary that
    # the classification cannot be a rounding accident.
    $hardBody = '{"model":"local-chat","messages":[{"role":"user","content":"' + ('x' * 2400) + '"}]}'
    $hardBodyFile = New-BodyFile -Name 'hard-prompt' -Json $hardBody
    $hard = Send-Chat -Base $gwUrl -BodyFile $hardBodyFile -Headers @('X-InferGate-Capabilities: chat') -Tag 'hard'
    Assert-Equal 'an oversized prompt answers 200' '200' $hard.Status
    Assert-Equal 'the cloud tier served the oversized prompt' 'cloud-mock' (Get-Header $hard.Headers 'X-InferGate-Upstream-Name')
    Assert-Equal 'the oversized prompt needed one attempt' '1' (Get-Header $hard.Headers 'X-InferGate-Attempt')
    Assert-True 'the oversized body really is past the boundary' ($hardBody.Length -gt 1604) "body=$($hardBody.Length) bytes"

    # The exact boundary the code draws: len(serialised messages)/4 must come out
    # at exactly 400 for local (strictly '>') and 401 for cloud. Both bodies are
    # derived by measurement, so the pair proves the comparison and not just the
    # direction.
    $baseBoundaryBody = '{"model":"local-chat","messages":[{"role":"user","content":"' + $fillerBase + '"}]}'
    $baseLen = & $boundaryChecker $baseBoundaryBody
    $pad400 = 1600 - $baseLen
    $pad401 = 1604 - $baseLen
    Assert-True 'the boundary bodies can be derived from the serialised message array' ($baseLen -gt 0 -and $pad401 -gt 0) `
        "base message array = $baseLen bytes"

    $bodyAt400 = '{"model":"local-chat","messages":[{"role":"user","content":"' + ($fillerBase + ('z' * $pad400)) + '"}]}'
    $bodyAbove400 = '{"model":"local-chat","messages":[{"role":"user","content":"' + ($fillerBase + ('z' * $pad401)) + '"}]}'
    $boundaryAt400 = & $boundaryChecker $bodyAt400
    $boundaryAbove = & $boundaryChecker $bodyAbove400
    Assert-Equal 'the at-boundary body estimates to exactly 400 prompt tokens' 400 ([int]($boundaryAt400 / 4))
    Assert-Equal 'the one-byte-over body estimates to 401 prompt tokens' 401 ([int]($boundaryAbove / 4))

    $at400File = New-BodyFile -Name 'prompt-at-400' -Json $bodyAt400
    $above400File = New-BodyFile -Name 'prompt-at-401' -Json $bodyAbove400
    $at400 = Send-Chat -Base $gwUrl -BodyFile $at400File -Headers @('X-InferGate-Capabilities: chat') -Tag 'at400'
    $above400 = Send-Chat -Base $gwUrl -BodyFile $above400File -Headers @('X-InferGate-Capabilities: chat') -Tag 'above400'
    Assert-Equal 'an estimated 400-token prompt stays local (classification is strictly >)' `
        'local-vllm' (Get-Header $at400.Headers 'X-InferGate-Upstream-Name')
    Assert-Equal 'an estimated 401-token prompt goes to the cloud tier' `
        'cloud-mock' (Get-Header $above400.Headers 'X-InferGate-Upstream-Name')

    # -----------------------------------------------------------------------
    # 6. Hard by completion size -> cloud tier
    # -----------------------------------------------------------------------
    Write-Section '6. Hard by completion size -> cloud tier'

    $maxFile = New-BodyFile -Name 'max-512' -Json $maxBody
    $edgeFile = New-BodyFile -Name 'max-256' -Json $edgeBody
    $overFile = New-BodyFile -Name 'max-257' -Json $overBody

    $max = Send-Chat -Base $gwUrl -BodyFile $maxFile -Tag 'max512'
    $edge = Send-Chat -Base $gwUrl -BodyFile $edgeFile -Tag 'max256'
    $over = Send-Chat -Base $gwUrl -BodyFile $overFile -Tag 'max257'

    Assert-Equal 'max_tokens: 512 answers 200' '200' $max.Status
    Assert-Equal 'max_tokens: 512 goes to the cloud tier' 'cloud-mock' (Get-Header $max.Headers 'X-InferGate-Upstream-Name')
    Assert-Equal 'max_tokens: 256 exactly stays local (strictly >)' 'local-vllm' (Get-Header $edge.Headers 'X-InferGate-Upstream-Name')
    Assert-Equal 'max_tokens: 257 goes to the cloud tier' 'cloud-mock' (Get-Header $over.Headers 'X-InferGate-Upstream-Name')

    # -----------------------------------------------------------------------
    # 7. Capability requests -> cloud tier
    # -----------------------------------------------------------------------
    Write-Section '7. Capability requests -> cloud tier'

    $toolsFile = New-BodyFile -Name 'tools' -Json $toolsBody
    $tools = Send-Chat -Base $gwUrl -BodyFile $toolsFile -Headers @('X-InferGate-Capabilities: tools') -Tag 'tools'
    Assert-Equal 'a tools request answers 200' '200' $tools.Status
    Assert-Equal 'a tools request goes to the cloud tier' 'cloud-mock' (Get-Header $tools.Headers 'X-InferGate-Upstream-Name')

    $toolsBare = Send-Chat -Base $gwUrl -BodyFile $toolsFile -Tag 'tools-bare'
    Assert-Equal 'a tools BODY without the capability header stays local' `
        'local-vllm' (Get-Header $toolsBare.Headers 'X-InferGate-Upstream-Name')

    # The capability header is the only difference between those two requests,
    # so the routing decision is what proves it was honoured. The header is a
    # request-only constraint -- internal/gateway/proxy.go reads it in
    # (*Proxy).plan, and internal/gateway/cachereq.go reads it in
    # capabilitiesFor so the tags are part of the cache scope -- and no response
    # path writes it back, so the consequence a caller can see is the upstream
    # name. That is recorded as an observation rather than asserted either way,
    # and it is cited by symbol rather than by line: line numbers drift.
    Assert-True 'the capability header is what moved the request to the cloud tier' `
        ((Get-Header $tools.Headers 'X-InferGate-Upstream-Name') -ne (Get-Header $toolsBare.Headers 'X-InferGate-Upstream-Name')) `
        "with header='$((Get-Header $tools.Headers 'X-InferGate-Upstream-Name'))' without='$((Get-Header $toolsBare.Headers 'X-InferGate-Upstream-Name'))'"
    if ((Get-Header $tools.Headers 'X-InferGate-Capabilities').Length -eq 0) {
        Add-Note "OBSERVATION: the response carries no X-InferGate-Capabilities echo. The header is a request-only constraint: internal/gateway/proxy.go reads it in (*Proxy).plan to resolve the routing chain, and internal/gateway/cachereq.go reads the same header in capabilitiesFor so two requests with different tags cannot share a cache entry. No response path writes it back, so what a caller can see is the consequence (X-InferGate-Upstream-Name, asserted above); the tags that produced the decision are the caller's own request header plus /admin/upstreams."
    }

    # -----------------------------------------------------------------------
    # 8. Both tiers really answered
    # -----------------------------------------------------------------------
    Write-Section '8. Both tiers really answered (the mock logs identify the replica)'

    $localLogText = Read-OpenLog $local.Log
    $cloudLogText = Read-OpenLog $cloud.Log
    # Only the traffic sections 4-7 generated: the replica logs are shared for
    # the whole run, so everything before the watermark is ignored.
    $localFresh = Get-OpenLogTail -Path $local.Log -Offset $localMark
    $cloudFresh = Get-OpenLogTail -Path $cloud.Log -Offset $cloudMark
    # Counts below are the numbers the gateway actually routed in sections 4-7,
    # taken from the `msg=request upstream=...` lines in the gateway log rather
    # than from the intended sequence: nine tier-governed requests, four landing
    # on the local tier and five on the cloud tier. The exact split is the thing
    # under test -- a tier policy that sent everything to one tier would fail
    # here even though every single-request assertion above had passed.
    $localChats = [regex]::Matches($localFresh, 'msg="chat request"').Count
    $cloudChats = [regex]::Matches($cloudFresh, 'msg="chat request"').Count
    Assert-Contains 'the local replica served the simple request' $localFresh 'model=local-chat stream=false messages=1 tools=false'
    Assert-Equal 'the local replica logged exactly four chat requests (its four preferred-tier bodies)' `
        4 $localChats "fresh local lines: $($localFresh -replace '\s+', ' ')"
    Assert-Equal 'the local replica logged the bare tools body (tools=true)' `
        1 ([regex]::Matches($localFresh, 'tools=true').Count) `
        "fresh local lines: $($localFresh -replace '\s+', ' ')"
    Assert-Equal 'the cloud replica logged exactly five chat requests (its five preferred-tier bodies)' `
        5 $cloudChats "fresh cloud lines: $($cloudFresh -replace '\s+', ' ')"
    Assert-Equal 'the cloud replica logged the tools request (tools=true)' `
        1 ([regex]::Matches($cloudFresh, 'tools=true').Count) `
        "fresh cloud lines: $($cloudFresh -replace '\s+', ' ')"
    Assert-Equal 'the two replicas between them served every tier-governed request sent' `
        9 ($localChats + $cloudChats) "local=$localChats cloud=$cloudChats"
    Assert-Contains 'the local replica log carries its own name' $localLogText 'name=local-vllm'
    Assert-Contains 'the cloud replica log carries its own name' $cloudLogText 'name=cloud-mock'
    Assert-True 'the two replicas are separate processes' `
        ($local.Proc.Id -ne $cloud.Proc.Id) "local=$($local.Proc.Id) cloud=$($cloud.Proc.Id)"


    # -----------------------------------------------------------------------
    # 9. Failover A -- the cloud tier dies, a hard request still lands local
    # -----------------------------------------------------------------------
    Write-Section '9. Failover A -- cloud replica dead, hard request falls back to local'

    $cloudPid = $cloud.Proc.Id
    Stop-Process -Id $cloudPid -Force -ErrorAction SilentlyContinue
    Assert-True "the cloud replica on :$CloudPort is really down" (Wait-PortClosed -Port $CloudPort -Seconds 5) `
        "pid $cloudPid is gone but :$CloudPort is still accepting connections"

    $hardFile2 = New-BodyFile -Name 'hard-prompt-2' -Json $hardBody
    $foHard = Send-Chat -Base $gwUrl -BodyFile $hardFile2 -Headers @('X-InferGate-Capabilities: chat') -Tag 'failover-cloud'
    Assert-Equal 'a hard request with the cloud tier dead still answers 200' '200' $foHard.Status
    Assert-Equal 'the local tier served the request the cloud tier could not' `
        'local-vllm' (Get-Header $foHard.Headers 'X-InferGate-Upstream-Name')
    $triedHard = Get-Header $foHard.Headers 'X-InferGate-Tried'
    Assert-True 'X-InferGate-Tried is PRESENT once a real failover happened' ($triedHard.Length -gt 0) `
        "Tried='$triedHard'"
    Assert-Equal 'the failover tried both tiers' 'cloud-mock, local-vllm' $triedHard
    Assert-Equal 'the request took two attempts' '2' (Get-Header $foHard.Headers 'X-InferGate-Attempt')
    Assert-Equal 'the failover response is a real completion' 'chat.completion' ((Get-Json $foHard.Body).object)

    # Restore the tier the way the run started.
    $cloud = Start-MockUpstream -Port $CloudPort -Name 'cloud-mock' -Tag "cloudB-$CloudPort"
    $script:cloudProc = $cloud.Proc
    Assert-True "the cloud tier replica is back on :$CloudPort" (Wait-Healthy -Port $CloudPort -Seconds 15)

    # -----------------------------------------------------------------------
    # 10. Failover B -- the local tier dies, a simple request still answers
    # -----------------------------------------------------------------------
    Write-Section '10. Failover B -- local replica dead, simple request falls back to cloud'

    $localPid = $local.Proc.Id
    Stop-Process -Id $localPid -Force -ErrorAction SilentlyContinue
    Assert-True "the local replica on :$LocalPort is really down" (Wait-PortClosed -Port $LocalPort -Seconds 5) `
        "pid $localPid is gone but :$LocalPort is still accepting connections"

    $simpleFile2 = New-BodyFile -Name 'simple-2' -Json $simpleBody
    $foSimple = Send-Chat -Base $gwUrl -BodyFile $simpleFile2 -Headers @('X-InferGate-Capabilities: chat') -Tag 'failover-local'
    Assert-Equal 'a simple request with the local tier dead still answers 200' '200' $foSimple.Status
    Assert-Equal 'the cloud tier served the request the local tier could not' `
        'cloud-mock' (Get-Header $foSimple.Headers 'X-InferGate-Upstream-Name')
    Assert-Equal 'the fallback tried both tiers in preference order' `
        'local-vllm, cloud-mock' (Get-Header $foSimple.Headers 'X-InferGate-Tried')
    Assert-Equal 'the fallback response is a real completion' 'chat.completion' ((Get-Json $foSimple.Body).object)
    $breakerText = Invoke-Curl @('-s', "$gwUrl/metrics")
    Assert-Equal 'the still-healthy cloud tier reports a closed breaker' `
        1.0 (Get-MetricValue -Text $breakerText -Name 'infergate_breaker_state' -LabelFragment 'upstream="cloud-mock",state="closed"')

    # Restore the local tier for the remaining sections.
    $local = Start-MockUpstream -Port $LocalPort -Name 'local-vllm' -Tag "localB-$LocalPort"
    $script:localProc = $local.Proc
    Assert-True "the local tier replica is back on :$LocalPort" (Wait-Healthy -Port $LocalPort -Seconds 15)

    # -----------------------------------------------------------------------
    # 11. /admin/upstreams -- what the gateway says it loaded
    # -----------------------------------------------------------------------
    Write-Section '11. /admin/upstreams -- the loaded topology'

    $admin = Invoke-Http -Url "$gwUrl/admin/upstreams"
    Assert-Equal '/admin/upstreams answers 200' '200' $admin.Status
    Assert-Contains '/admin/upstreams names the local tier replica' $admin.Body 'local-vllm'
    Assert-Contains '/admin/upstreams names the cloud tier replica' $admin.Body 'cloud-mock'

    $adminJson = Get-Json $admin.Body
    Assert-True '/admin/upstreams is parseable JSON with an upstreams array' `
        ($null -ne $adminJson -and $null -ne $adminJson.upstreams) 'body did not parse or had no upstreams key'
    $adminNames = @($adminJson.upstreams | ForEach-Object { $_.name })
    Assert-Equal '/admin/upstreams lists exactly the two configured upstreams' 2 $adminNames.Count `
        " names: $($adminNames -join ', ')"

    # The M4 view of an upstream is only complete if it carries its tier, so the
    # loaded tier of each backend is asserted on the row itself. A build that
    # stops publishing the field fails here rather than being papered over.
    foreach ($pair in @(@('local-vllm', 'local'), @('cloud-mock', 'cloud'))) {
        $row = @($adminJson.upstreams | Where-Object { $_.name -eq $pair[0] })
        Assert-Equal "/admin/upstreams reports tier=$($pair[1]) for $($pair[0])" $pair[1] $row[0].tier
    }

    Assert-Equal '/admin/upstreams reports the tiered strategy' 'tiered' $adminJson.routing.strategy

    # The limits that decide local vs cloud have to be visible on the admin
    # surface, and they have to be the values behaviour was measured against.
    # They are read back out of the config the gateway loaded and compared with
    # the echo, so a retuned policy is not a second copy of the numbers here.
    # The two presence checks come first: a renamed config key then reports "no
    # ceiling in the config" instead of decaying into a 0-vs-0 comparison.
    $policyLocalPrompt = Get-ConfigScalar -Text $instantiated -Block 'routing' -Key 'local_max_prompt_tokens'
    $policyLocalCompletion = Get-ConfigScalar -Text $instantiated -Block 'routing' -Key 'local_max_completion_tokens'
    Assert-True 'the loaded config sets a local prompt ceiling' ($null -ne $policyLocalPrompt) "read $configPath"
    Assert-True 'the loaded config sets a local completion ceiling' ($null -ne $policyLocalCompletion) "read $configPath"

    $tp = $adminJson.routing.tier_policy
    Assert-Equal "the echoed tier_policy carries the configured local prompt ceiling ($policyLocalPrompt)" ([int]$policyLocalPrompt) ([int]$tp.local_max_prompt_tokens)
    Assert-Equal "the echoed tier_policy carries the configured local completion ceiling ($policyLocalCompletion)" ([int]$policyLocalCompletion) ([int]$tp.local_max_completion_tokens)
    Assert-Contains 'the echoed tier_policy routes tools to the cloud' (@($tp.cloud_capabilities) -join ',') 'tools'
    Assert-Contains 'the echoed tier_policy routes vision to the cloud' (@($tp.cloud_capabilities) -join ',') 'vision'


    # -----------------------------------------------------------------------
    # 12. /stats and /metrics -- the per-tier split, and the cost arithmetic
    # -----------------------------------------------------------------------
    Write-Section '12. /stats and /metrics -- the split matches what was sent'

    $statsBefore = Invoke-Http -Url "$gwUrl/stats"
    Assert-Equal '/stats answers 200' '200' $statsBefore.Status
    $metricsBefore = Invoke-Curl @('-s', "$gwUrl/metrics")
    $localBefore = Get-StatCount -Text $statsBefore.Body -Upstream 'local-vllm'
    $cloudBefore = Get-StatCount -Text $statsBefore.Body -Upstream 'cloud-mock'
    Assert-True '/stats reports a per-upstream series for both tiers' `
        (($localBefore -ge 0) -and ($cloudBefore -ge 0)) "local=$localBefore cloud=$cloudBefore"
    Write-Host "        series before: local-vllm=$localBefore cloud-mock=$cloudBefore" -ForegroundColor DarkGray

    # A known mix: two simple (local) and two hard-by-completion (cloud).
    $mixLocal = 2
    $mixCloud = 2
    for ($i = 1; $i -le $mixLocal; $i++) {
        $bf = New-BodyFile -Name "mix-local-$i" -Json $simpleBody
        $r = Send-Chat -Base $gwUrl -BodyFile $bf -Tag "mix-local-$i"
        Assert-Equal "mix request $i (simple) was served by the local tier" 'local-vllm' (Get-Header $r.Headers 'X-InferGate-Upstream-Name')
    }
    for ($i = 1; $i -le $mixCloud; $i++) {
        $bf = New-BodyFile -Name "mix-cloud-$i" -Json $maxBody
        $r = Send-Chat -Base $gwUrl -BodyFile $bf -Tag "mix-cloud-$i"
        Assert-Equal "mix request $i (max_tokens 512) was served by the cloud tier" 'cloud-mock' (Get-Header $r.Headers 'X-InferGate-Upstream-Name')
    }

    $statsAfter = Invoke-Http -Url "$gwUrl/stats"
    $metricsAfter = Invoke-Curl @('-s', "$gwUrl/metrics")
    $localAfter = Get-StatCount -Text $statsAfter.Body -Upstream 'local-vllm'
    $cloudAfter = Get-StatCount -Text $statsAfter.Body -Upstream 'cloud-mock'
    Assert-Equal 'the local-tier series grew by exactly the simple requests sent' $mixLocal ($localAfter - $localBefore) `
        " before=$localBefore after=$localAfter"
    Assert-Equal 'the cloud-tier series grew by exactly the hard requests sent' $mixCloud ($cloudAfter - $cloudBefore) `
        " before=$cloudBefore after=$cloudAfter"
    Assert-Equal 'the two tiers together account for every request in the mix' ($mixLocal + $mixCloud) `
        (($localAfter + $cloudAfter) - ($localBefore + $cloudBefore))

    # The token counters are the only source of cost in the metrics surface --
    # there is no cost metric -- so cost is derived here from the configured
    # prices and asserted against the per-tier token deltas.
    $localPrompt = (Get-MetricValue -Text $metricsAfter -Name 'infergate_tokens_total' -LabelFragment 'upstream="local-vllm",model="local-chat",kind="prompt"') - `
        (Get-MetricValue -Text $metricsBefore -Name 'infergate_tokens_total' -LabelFragment 'upstream="local-vllm",model="local-chat",kind="prompt"')
    $localCompletion = (Get-MetricValue -Text $metricsAfter -Name 'infergate_tokens_total' -LabelFragment 'upstream="local-vllm",model="local-chat",kind="completion"') - `
        (Get-MetricValue -Text $metricsBefore -Name 'infergate_tokens_total' -LabelFragment 'upstream="local-vllm",model="local-chat",kind="completion"')
    $cloudPrompt = (Get-MetricValue -Text $metricsAfter -Name 'infergate_tokens_total' -LabelFragment 'upstream="cloud-mock",model="local-chat",kind="prompt"') - `
        (Get-MetricValue -Text $metricsBefore -Name 'infergate_tokens_total' -LabelFragment 'upstream="cloud-mock",model="local-chat",kind="prompt"')
    $cloudCompletion = (Get-MetricValue -Text $metricsAfter -Name 'infergate_tokens_total' -LabelFragment 'upstream="cloud-mock",model="local-chat",kind="completion"') - `
        (Get-MetricValue -Text $metricsBefore -Name 'infergate_tokens_total' -LabelFragment 'upstream="cloud-mock",model="local-chat",kind="completion"')

    Write-Host "        tokens in mix: local prompt=$localPrompt completion=$localCompletion | cloud prompt=$cloudPrompt completion=$cloudCompletion" -ForegroundColor DarkGray
    Assert-True 'the local tier reported prompt tokens for its two requests' ($localPrompt -gt 0) "local prompt delta=$localPrompt"
    Assert-True 'the local tier reported completion tokens for its two requests' ($localCompletion -gt 0) "local completion delta=$localCompletion"
    Assert-True 'the cloud tier reported prompt tokens for its two requests' ($cloudPrompt -gt 0) "cloud prompt delta=$cloudPrompt"
    Assert-True 'the cloud tier reported completion tokens for its two requests' ($cloudCompletion -gt 0) "cloud completion delta=$cloudCompletion"

    # The prices come out of the config the gateway loaded -- $instantiated is the
    # exact text written to $configPath -- instead of sitting next to the
    # arithmetic as a second copy: a price edited in configs\tiered-local.yaml
    # would otherwise leave this gate green while its own arithmetic priced a
    # config nobody runs.
    #
    # configs\tiered-local.yaml prices the alias every request uses
    # (pricing.models.local-chat) and, for any model it does not name,
    # pricing.default -- which is also what the router's cost weights compare a
    # tier against. Both mock replicas echo the requested alias, so the gateway
    # itself charges every response the local-chat price; the cloud figure below
    # is therefore the counterfactual list price of the same tokens, not a bill.
    # There is no infergate cost metric, so the money is:
    #   local spend = (localPrompt * local-chat.in + localCompletion * local-chat.out) / 1e6
    #   cloud spend = (cloudPrompt * default.in  + cloudCompletion * default.out)  / 1e6
    # and the split is exactly what the tier policy is supposed to buy.
    $pricing = Get-PricingFromConfig -Text $instantiated
    Assert-True 'the config under test prices the alias every request uses (pricing.models.local-chat)' `
        ($pricing.Models.Contains('local-chat')) "read $configPath"
    Assert-True 'the config under test carries the fallback list price (pricing.default)' `
        ($null -ne $pricing.Default) "read $configPath"

    $localIn = 0.0; $localOut = 0.0
    if ($pricing.Models.Contains('local-chat')) {
        $localIn = [double]$pricing.Models['local-chat']['in']
        $localOut = [double]$pricing.Models['local-chat']['out']
    }
    $cloudIn = 0.0; $cloudOut = 0.0
    if ($null -ne $pricing.Default) {
        $cloudIn = [double]$pricing.Default['in']
        $cloudOut = [double]$pricing.Default['out']
    }

    $localCostUSD = (($localPrompt * $localIn) + ($localCompletion * $localOut)) / 1000000.0
    $cloudCostUSD = (($cloudPrompt * $cloudIn) + ($cloudCompletion * $cloudOut)) / 1000000.0
    Write-Host ("        derived spend for the mix: local=`${0:F6} cloud=`${1:F6}" -f $localCostUSD, $cloudCostUSD) -ForegroundColor DarkGray
    Assert-True "the derived local-tier spend for the mix is positive (pricing.models.local-chat in $localIn / out $localOut per 1e6)" ($localCostUSD -gt 0) `
        "localCostUSD=$localCostUSD"
    Assert-True "the derived cloud-tier spend for the mix is positive (pricing.default in $cloudIn / out $cloudOut per 1e6)" ($cloudCostUSD -gt 0) `
        "cloudCostUSD=$cloudCostUSD"
    Assert-True 'a locally served request is cheaper than the same request in the cloud' `
        ($localCostUSD -lt $cloudCostUSD) `
        ("local=`${0:F6} for {1}+{2} tokens vs cloud=`${3:F6} for {4}+{5} tokens" -f $localCostUSD, $localPrompt, $localCompletion, $cloudCostUSD, $cloudPrompt, $cloudCompletion)

    # -----------------------------------------------------------------------
    # 13. The decision reason in the gateway log
    # -----------------------------------------------------------------------
    Write-Section '13. The routing decision reason in the gateway log'

    # These strings are read out of internal/router/router.go (tierReason), not
    # guessed: a green gate that matched wording the code does not emit would
    # prove nothing at all.
    $logText = Read-OpenLog $gwLog
    Assert-Contains 'the log carries the simple-prefers-local reason verbatim' $logText $tierLocalReason
    Assert-Contains 'the log carries the hard-prefers-cloud reason verbatim' $logText $tierCloudReason
    Assert-Contains 'the log records the hard request falling back to the local tier' $logText 'strategy=tiered tier=local reason=hard request falls back to this tier'
    Assert-Contains 'the log records the simple request falling back to the cloud tier' $logText $tierCloudFallback
    Assert-Contains 'the log records the failed attempt before the fallback' $logText 'tried="cloud-mock -> local-vllm"'
    Assert-Contains 'the log records the second failover order too' $logText 'tried="local-vllm -> cloud-mock"'
    Assert-Contains 'the log names the tier strategy on the decision line' $logText 'strategy=tiered tier=cloud'
    Assert-Contains 'the log names the local tier on the decision line' $logText 'strategy=tiered tier=local'

    # Far end guard: the fleet that produced all of the above must still be the
    # fleet this script started.
    Assert-True 'the gateway process survived the whole run' `
        ($null -ne $script:gwProc -and -not $script:gwProc.HasExited)
    Assert-True 'the gateway still answers /healthz at the end of the run' (Wait-Healthy -Port $GatewayPort -Seconds 5)

    # -----------------------------------------------------------------------
    # 14. Ungoverned surfaces still answer
    # -----------------------------------------------------------------------
    Write-Section '14. /v1/models sanity check'

    $models = Invoke-Http -Url "$gwUrl/v1/models"
    Assert-Equal '/v1/models answers 200' '200' $models.Status
    $modelsJson = Get-Json $models.Body
    Assert-True '/v1/models returns a parseable model list' `
        ($null -ne $modelsJson -and @($modelsJson.data).Count -ge 1) 'body did not parse as an OpenAI model list'
    Assert-True 'the model list is not empty' (@($modelsJson.data).Count -ge 1)
}
finally {
    # -----------------------------------------------------------------------
    # 15. Teardown
    # -----------------------------------------------------------------------
    Write-Section '15. Teardown'

    # Every process is stopped in the finally block, including after a thrown
    # error: a half-started fleet left listening on the test ports is what makes
    # the NEXT run fail for reasons that have nothing to do with the code.
    #
    # -KeepRunning is the deliberate exception, for the case where a failure
    # needs to be inspected against a live fleet: leaving the processes up is
    # the whole point of the switch, so it must actually suppress the kill.
    if ($KeepRunning) {
        Write-Host '  -KeepRunning set: the fleet is left running for inspection.' -ForegroundColor DarkYellow
        foreach ($entry in @(
                @('gateway       ', $GatewayPort, $script:gwProc),
                @('local replica ', $LocalPort, $script:localProc),
                @('cloud replica ', $CloudPort, $script:cloudProc))) {
            if ($entry[2]) { Write-Host "  pid=$($entry[2].Id)  $($entry[0]) :$($entry[1])" -ForegroundColor DarkYellow }
        }
        Write-Host '  none of these are stopped by this script when -KeepRunning is set.' -ForegroundColor DarkYellow
    }
    else {
        Stop-Tracked -Proc $script:gwProc -Label 'gateway'
        Stop-Tracked -Proc $script:localProc -Label 'local replica'
        Stop-Tracked -Proc $script:cloudProc -Label 'cloud replica'

        # Process death and socket release are asynchronous; give them a moment
        # rather than reporting a leak that is really a race.
        $deadline = (Get-Date).AddSeconds(5)
        while ((Get-Date) -lt $deadline -and @(Get-TrackedProcess).Count -gt 0) { Start-Sleep -Milliseconds 100 }

        # This check cannot enumerate the process table (the sandbox denies
        # Get-CimInstance Win32_Process with HRESULT 0x80041003, and an empty
        # table makes "nothing survived" pass for the wrong reason). It asks
        # Get-Process about the pids recorded when they were started -- which is
        # why the first assertion below matters: if nothing had been recorded,
        # the second one would prove nothing at all.
        Assert-True 'the script recorded every process it started' ($script:startedPids.Count -ge 5) `
            "tracked=$($script:startedPids.Count) pids: $($script:startedPids -join ', ')"
        $alive = @(Get-TrackedProcess)
        Assert-Equal 'no process started by this script survived it' 0 $alive.Count `
            (($alive | ForEach-Object { "$($_.ProcessName) pid=$($_.Id)" }) -join '; ')
        foreach ($port in $ports) {
            Assert-True "nothing is left listening on :$port" (Wait-PortClosed -Port $port -Seconds 3) `
                "port :$port still accepts connections after teardown"
        }
        Assert-True 'the gateway wrote no stderr during the run' ((Read-Text $gwErr).Trim().Length -eq 0) `
            "stderr: $((Read-Text $gwErr).Trim())"
    }

    # The tmp config is removed either way: it is derived from configs\ and can
    # be regenerated, and leaving copies behind is how "which config was that
    # run using?" becomes unanswerable a week later.
    $tmpPaths = @($configPath, $gwLog, $gwErr, $localLog, $localErr, $cloudLog, $cloudErr, $checkOutFile, $checkErrFile)
    $tmpPaths += @(Get-ChildItem -Path $tmpDir -Filter "m4-verify-*-$stamp.yaml" -ErrorAction SilentlyContinue | ForEach-Object { $_.FullName })
    $tmpPaths += @(Get-ChildItem -Path $tmpDir -Filter "m4-*-$stamp.*" -ErrorAction SilentlyContinue | ForEach-Object { $_.FullName })
    foreach ($p in ($tmpPaths | Select-Object -Unique)) {
        Remove-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue
    }
    $leftovers = @(Get-ChildItem -Path $tmpDir -Filter "m4-*-$stamp.*" -ErrorAction SilentlyContinue)
    $leftoverConfigs = @(Get-ChildItem -Path $tmpDir -Filter "m4-verify-*-$stamp.yaml" -ErrorAction SilentlyContinue)
    Assert-Equal 'this run left no tmp config behind' 0 ($leftovers.Count + $leftoverConfigs.Count) `
        (($leftovers.FullName + $leftoverConfigs.FullName) -join '; ')
    # The config check above would still pass if the request bodies survived, so
    # the body/response families are counted explicitly: a gate that litters tmp\
    # is indistinguishable from one that was run twice at once.
    $leftoverBodies = @(Get-ChildItem -Path $tmpDir -Filter "m4-body-*-$stamp*" -ErrorAction SilentlyContinue)
    $leftoverResponses = @(Get-ChildItem -Path $tmpDir -Filter "m4-resp-*-$stamp*" -ErrorAction SilentlyContinue)
    Assert-Equal 'this run left no request bodies or response files behind' 0 `
        ($leftoverBodies.Count + $leftoverResponses.Count) `
        (($leftoverBodies.FullName + $leftoverResponses.FullName) -join '; ')
}

$foreign = @(Get-Process -Name infergate, mockupstream -ErrorAction SilentlyContinue |
    Where-Object { $script:startedPids -notcontains $_.Id })
Write-Host ''
Write-Host "  fleet binaries NOT started by this script still running: $($foreign.Count)" -ForegroundColor DarkYellow
foreach ($p in $foreign) { Write-Host "    $($p.ProcessName) pid=$($p.Id) (a concurrent run is not this run's leak)" -ForegroundColor DarkYellow }

if ($script:notes.Count -gt 0) {
    Write-Host ''
    Write-Host '  DEFECTS / OBSERVATIONS (not asserted as failures):' -ForegroundColor Magenta
    foreach ($n in $script:notes) { Write-Host "    - $n" -ForegroundColor Magenta }
}

Write-Host ''
Write-Host ("=" * 72) -ForegroundColor DarkGray
if ($script:failed -eq 0) {
    Write-Host "  RESULT: $($script:passed)/$($script:passed) assertions passed" -ForegroundColor Green
    Write-Host '  OK: M4 tiered local/cloud routing verified end to end with curl' -ForegroundColor Green
    exit 0
}
else {
    Write-Host "  RESULT: $($script:passed) passed, $($script:failed) FAILED" -ForegroundColor Red
    exit 1
}
