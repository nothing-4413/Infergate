# One command that leaves a working InferGate stack running behind this terminal.
#
# WHY THIS EXISTS: README section 3.2 used to be three terminals, a hand-written
# request body and a note about which port the gateway actually bound. Every step
# in that recipe is checkable -- the port is free, the binaries build, the derived
# config loads, /healthz turns 200, the body is UTF-8 without a BOM -- and every
# one of those checks is a thing a reader gets wrong once. This script does them,
# prints where the logs went, and starts the stack.
#
# It is deliberately not a supervisor: it starts processes, waits for /healthz,
# records the pids under tmp\dev-up\state.json, and returns. The child processes
# survive this script (they are started with Start-Process, not as jobs), so the
# terminal is free again. `-Down` stops exactly what the last run started.
#
# ASCII only, on purpose: Windows PowerShell 5.1 reads a BOM-less .ps1 as ANSI,
# so non-ASCII text in this file would arrive mojibake.
#
# USAGE (from the repository root):
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev-up.ps1
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev-up.ps1 -Profile ollama
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev-up.ps1 -Profile docker
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev-up.ps1 -Down
#
# PROFILES:
#   mock   (default) mockupstream on :9000 + gateway on :8080 (configs/mock.yaml).
#                    No Docker, no model, no network. Good first command.
#   ollama           gateway on :8080 pointed at Ollama on :11434 (configs/ollama.yaml).
#                    Creates the container and pulls the model when missing.
#   docker           docker compose up -d --build: gateway :18080, mock :19000,
#                    RESP2 :16399 (Prometheus/Grafana stay behind --profile obs).
#
# -Port and -MockPort rewrite only server.listen into tmp\dev-up\, so a machine
# that already holds 8080 can still run this without editing a tracked file.

[CmdletBinding()]
param(
    [ValidateSet('mock', 'ollama', 'docker')]
    [string]$Profile = 'mock',
    [int]$Port = 8080,
    [int]$MockPort = 9000,
    [int]$OllamaPort = 11434,
    [string]$Model = 'qwen2.5:0.5b',
    [string]$Container = 'ollama-infergate',
    [int]$WaitSeconds = 40,
    [switch]$Rebuild,
    [switch]$DryRun,
    [switch]$Down
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$tmp = Join-Path $root 'tmp\dev-up'
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$statePath = Join-Path $tmp 'state.json'
$goShim = Join-Path $root 'tools\go.cmd'

$script:started = @()

# BOM-less UTF-8: Set-Content -Encoding utf8 on PowerShell 5.1 writes a BOM, and
# a leading EF BB BF makes the gateway reject an otherwise perfect JSON body.
function Write-Utf8([string]$Path, [string]$Text) {
    [System.IO.File]::WriteAllText($Path, $Text, (New-Object System.Text.UTF8Encoding($false)))
}

function Get-HttpCode([string]$Url) {
    $code = (& curl.exe -s -o NUL -w '%{http_code}' --max-time 3 $Url 2>$null | Out-String).Trim()
    return $code
}

function Test-PortOpen([int]$PortNumber) {
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $client.BeginConnect('127.0.0.1', $PortNumber, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne(300)) { return $false }
        $client.EndConnect($iar)
        return $true
    }
    catch { return $false }
    finally { $client.Close() }
}

function Wait-PortOpen([int]$PortNumber, [int]$Seconds = 15) {
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (Test-PortOpen -PortNumber $PortNumber) { return $true }
        Start-Sleep -Milliseconds 150
    }
    return $false
}

function Wait-Healthy([int]$PortNumber, [string]$Path = '/healthz', [int]$Seconds = 40) {
    $deadline = (Get-Date).AddSeconds($Seconds)
    $code = ''
    while ((Get-Date) -lt $deadline) {
        $code = Get-HttpCode "http://127.0.0.1:$PortNumber$Path"
        if ($code -eq '200') { return $true }
        Start-Sleep -Milliseconds 200
    }
    return $false
}

function Get-ListenPort([string]$ConfigPath) {
    $text = [System.IO.File]::ReadAllText((Join-Path $root $ConfigPath))
    $m = [regex]::Match($text, '(?m)^\s*listen:\s*"?[^"\r\n]*?:(\d+)"?')
    if ($m.Success) { return [int]$m.Groups[1].Value }
    throw "cannot read server.listen out of $ConfigPath"
}

# Copy a tracked config into tmp\dev-up\ with only the listen port changed, so a
# busy 8080 never requires editing a file the repository pins.
function New-ConfigOnPort([string]$ConfigPath, [int]$PortNumber, [int]$UpstreamPort = 0) {
    # -UpstreamPort rewrites the 127.0.0.1 backend address as well. Without it a
    # moved mock port would leave the gateway dialing the old one, and the first
    # request would come back 502 from an upstream that is running fine -- caught
    # by the -MockPort run that produced chat=502 before this branch existed.
    $rewritten = $null
    if ((Get-ListenPort -ConfigPath $ConfigPath) -ne $PortNumber) {
        $text = [System.IO.File]::ReadAllText((Join-Path $root $ConfigPath))
        $rewritten = [regex]::Replace($text, '(?m)^(\s*listen:\s*)"[^"]*"', ('$1":' + $PortNumber + '"'), 1)
    }
    $baseUrlPattern = '(?m)^(\s*base_url:\s*")http://127\.0\.0\.1:(\d+)'
    if ($UpstreamPort -gt 0) {
        $current = [regex]::Match($rewritten, $baseUrlPattern)
        if ($current.Success -and [int]$current.Groups[2].Value -ne $UpstreamPort) {
            if ($null -eq $rewritten) { $rewritten = [System.IO.File]::ReadAllText((Join-Path $root $ConfigPath)) }
            $rewritten = [regex]::Replace($rewritten, $baseUrlPattern, ('$1http://127.0.0.1:' + $UpstreamPort), 1)
        }
    }
    if ($null -eq $rewritten) { return $ConfigPath }
    $leaf = Split-Path -Leaf $ConfigPath
    $out = Join-Path $tmp $leaf
    Write-Utf8 $out $rewritten
    Write-Host ("  config    derived {0} -> {1} (listen :{2}{3})" -f $ConfigPath, "tmp\dev-up\$leaf", $PortNumber,
        $(if ($UpstreamPort -gt 0) { ", upstream :$UpstreamPort" } else { '' }))
    return "tmp\dev-up\$leaf"
}

function Ensure-Binary([string]$Name, [string]$Package) {
    $exe = Join-Path $root "bin\$Name.exe"
    if ((Test-Path $exe) -and -not $Rebuild) { return $exe }
    Write-Host "  building  $Name -> bin\$Name.exe"
    & $goShim build -o "bin\$Name.exe" $Package
    if ($LASTEXITCODE -ne 0) { throw "go build $Package failed (exit $LASTEXITCODE)" }
    return $exe
}

function Start-Tracked([string]$Name, [string]$Exe, [string[]]$Arguments) {
    if ($DryRun) {
        Write-Host ("  [dry-run] {0} {1}" -f $Exe, ($Arguments -join ' '))
        return $null
    }
    $log = Join-Path $tmp "$Name.log"
    $err = Join-Path $tmp "$Name.err.log"
    $proc = Start-Process -FilePath $Exe -ArgumentList $Arguments -RedirectStandardOutput $log `
        -RedirectStandardError $err -PassThru -WindowStyle Hidden
    $script:started += [pscustomobject]@{ Name = $Name; Id = $proc.Id }
    Write-Host "  started   $Name (pid=$($proc.Id), log tmp\dev-up\$Name.log)"
    return $proc
}

function Save-State([bool]$Compose, [int[]]$Ports) {
    if ($DryRun) { return }
    $state = @{
        profile   = $Profile
        compose   = $Compose
        ports     = @($Ports)
        processes = @($script:started)
        startedAt = (Get-Date).ToString('o')
    }
    Write-Utf8 $statePath ($state | ConvertTo-Json -Depth 5)
}

function Stop-OwnersOnPort([int]$PortNumber) {
    $conns = Get-NetTCPConnection -LocalPort $PortNumber -State Listen -ErrorAction SilentlyContinue
    foreach ($c in @($conns)) {
        $proc = Get-Process -Id $c.OwningProcess -ErrorAction SilentlyContinue
        if ($null -eq $proc) { continue }
        $path = ''
        try { $path = $proc.Path } catch { $path = '' }
        if ($path -like '*\bin\infergate.exe' -or $path -like '*\bin\mockupstream.exe') {
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
            Write-Host "  stopped   leftover $($proc.ProcessName) (pid=$($proc.Id), was holding :$PortNumber)" -ForegroundColor DarkGray
        }
    }
}

function Stop-Stack {
    if (-not (Test-Path $statePath)) {
        Write-Host 'dev-up: nothing recorded under tmp\dev-up\state.json -- nothing to stop.' -ForegroundColor Yellow
        return
    }
    $state = Get-Content -Raw $statePath | ConvertFrom-Json
    foreach ($entry in @($state.processes)) {
        $proc = Get-Process -Id $entry.Id -ErrorAction SilentlyContinue
        if ($null -ne $proc) {
            Stop-Process -Id $entry.Id -Force -ErrorAction SilentlyContinue
            Write-Host "  stopped   $($entry.Name) (pid=$($entry.Id))"
        }
    }
    foreach ($portNumber in @($state.ports)) {
        Stop-OwnersOnPort -PortNumber ([int]$portNumber)
    }
    if ($state.compose) {
        Write-Host '  compose   docker compose down'
        & docker compose down
    }
    if ($state.profile -eq 'ollama') {
        Write-Host "  note      the Ollama container was left running on purpose: docker stop $Container" -ForegroundColor DarkGray
    }
    Remove-Item -Force $statePath
    Write-Host 'dev-up: stopped.' -ForegroundColor Green
}

# ---------------------------------------------------------------------------
# -Down short-circuits everything else: it is the undo of the last run.
# ---------------------------------------------------------------------------
if ($Down) {
    Stop-Stack
    exit 0
}

if (Test-Path $statePath) {
    Write-Host 'dev-up: a stack from an earlier run is still recorded under tmp\dev-up\state.json.' -ForegroundColor Yellow
    Write-Host '        stop it first with -Down, or the ports below will be reported as busy.' -ForegroundColor Yellow
}

Write-Host ''
Write-Host "dev-up: profile '$Profile'" -ForegroundColor Cyan

# Every listener this script is about to use has to be free: the gateway binding
# [::]:8080 while another process owns 127.0.0.1:8080 is the failure README 3.2
# warns about, and it looks like a broken gateway rather than a busy port.
$ports = @()
if ($Profile -eq 'docker') {
    $composePort = if ($env:INFERGATE_GATEWAY_PORT) { [int]$env:INFERGATE_GATEWAY_PORT } else { 18080 }
    $ports = @($composePort)
}
else {
    $ports = if ($Profile -eq 'mock') { @($Port, $MockPort) } else { @($Port) }
}
foreach ($portNumber in $ports) {
    if (Test-PortOpen -PortNumber $portNumber) {
        Write-Host "  port      :$portNumber is already in use -- pass -Port/-MockPort to move, or stop the owner" -ForegroundColor Red
        exit 1
    }
}

$upstream = ''
$chatModel = ''
$gatewayPort = $Port

if ($Profile -eq 'docker') {
    $composePort = if ($env:INFERGATE_GATEWAY_PORT) { [int]$env:INFERGATE_GATEWAY_PORT } else { 18080 }
    if ($DryRun) {
        Write-Host '  [dry-run] docker compose up -d --build'
        exit 0
    }
    Write-Host '  compose   docker compose up -d --build (first build compiles the images)'
    & docker compose up -d --build
    if ($LASTEXITCODE -ne 0) { throw "docker compose up failed (exit $LASTEXITCODE)" }
    $gatewayPort = $composePort
    $upstream = 'container mockupstream :9000'
    $chatModel = 'mock-gpt'
}
else {
    $gatewayExe = Ensure-Binary -Name 'infergate' -Package '.\cmd\infergate'

    if ($Profile -eq 'mock') {
        $mockExe = Ensure-Binary -Name 'mockupstream' -Package '.\cmd\mockupstream'
        $config = New-ConfigOnPort -ConfigPath 'configs/mock.yaml' -PortNumber $Port -UpstreamPort $MockPort
        Start-Tracked -Name 'mockupstream' -Exe $mockExe -Arguments @('-listen', ":$MockPort", '-name', 'mock', '-ttfb', '250ms') | Out-Null
        if (-not $DryRun -and -not (Wait-PortOpen -PortNumber $MockPort -Seconds 15)) {
            Write-Host "  mock      did not open :$MockPort in 15s -- see tmp\dev-up\mockupstream.err.log" -ForegroundColor Red
            exit 1
        }
        $upstream = "mockupstream http://127.0.0.1:$MockPort"
        $chatModel = 'mock-gpt'
    }
    else {
        # Ollama profile: make sure something answers on :11434 before the gateway
        # spends its startup probe discovering that nothing does.
        if ((Get-HttpCode "http://127.0.0.1:$OllamaPort/v1/models") -ne '200') {
            $dockerCli = Get-Command docker -ErrorAction SilentlyContinue
            if ($null -eq $dockerCli) {
                Write-Host "  ollama    nothing answers on :$OllamaPort and docker is not on PATH." -ForegroundColor Red
                Write-Host "            docker run -d --name $Container --restart unless-stopped -p ${OllamaPort}:11434 -v D:\ollama\models:/root/.ollama ollama/ollama"
                exit 1
            }
            if ($DryRun) {
                Write-Host "  [dry-run] docker start/run $Container, then ollama pull $Model"
            }
            else {
                $existing = (& docker ps -a --filter "name=^$Container$" --format '{{.Names}}' 2>$null | Out-String).Trim()
                if ($existing -eq $Container) {
                    Write-Host "  ollama    starting existing container $Container"
                    & docker start $Container | Out-Null
                }
                else {
                    Write-Host "  ollama    creating container $Container (models land in D:\ollama\models)"
                    & docker run -d --name $Container --restart unless-stopped -p "${OllamaPort}:11434" `
                        -v 'D:\ollama\models:/root/.ollama' ollama/ollama | Out-Null
                    if ($LASTEXITCODE -ne 0) { throw "docker run $Container failed (exit $LASTEXITCODE)" }
                }
                Write-Host "  ollama    waiting for /v1/models on :$OllamaPort"
                $ok = Wait-Healthy -PortNumber $OllamaPort -Path '/v1/models' -Seconds 60
                if (-not $ok) {
                    Write-Host "  ollama    still not answering on :$OllamaPort -- check 'docker logs $Container'" -ForegroundColor Red
                    exit 1
                }
                $listing = (& docker exec $Container ollama list 2>$null | Out-String)
                if ($listing -notmatch [regex]::Escape($Model)) {
                    Write-Host "  model     pulling $Model (about 400MB on first run; the only slow step)"
                    & docker exec $Container ollama pull $Model
                    if ($LASTEXITCODE -ne 0) { throw "ollama pull $Model failed (exit $LASTEXITCODE)" }
                }
                else {
                    Write-Host "  model     $Model is already pulled"
                }
            }
        }
        else {
            Write-Host "  ollama    already answering on :$OllamaPort"
        }
        $config = New-ConfigOnPort -ConfigPath 'configs/ollama.yaml' -PortNumber $Port -UpstreamPort $OllamaPort
        $upstream = "ollama http://127.0.0.1:$OllamaPort"
        $chatModel = $Model
    }

    & (Join-Path $root 'bin\infergate.exe') -config $config -check | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "config check failed for $config" }

    Start-Tracked -Name 'infergate' -Exe $gatewayExe -Arguments @('-config', $config) | Out-Null
}

if (-not $DryRun) {
    Write-Host "  waiting   http://127.0.0.1:$gatewayPort/healthz"
    if (-not (Wait-Healthy -PortNumber $gatewayPort -Seconds $WaitSeconds)) {
        Write-Host '  healthz   never turned 200 -- last lines of tmp\dev-up\infergate.err.log:' -ForegroundColor Red
        $errLog = Join-Path $tmp 'infergate.err.log'
        if (Test-Path $errLog) { Get-Content $errLog -Tail 10 | ForEach-Object { Write-Host "            $_" } }
        exit 1
    }
}

$body = '{"model":"' + $chatModel + '","messages":[{"role":"user","content":"hello"}]}'
Write-Utf8 (Join-Path $tmp 'chat.json') $body

Save-State -Compose ($Profile -eq 'docker') -Ports $ports

if ($DryRun) {
    Write-Host ''
    Write-Host 'dev-up: dry run -- nothing was started.'
    exit 0
}

Write-Host ''
Write-Host 'dev-up: stack is up.' -ForegroundColor Green
Write-Host "  gateway   http://127.0.0.1:$gatewayPort    (health: /healthz, stats: /stats, metrics: /metrics)"
Write-Host "  upstream  $upstream"
Write-Host '  logs      tmp\dev-up\infergate.log  and  tmp\dev-up\infergate.err.log'
Write-Host ('  request   curl.exe -s http://127.0.0.1:{0}/v1/chat/completions -H "Content-Type: application/json" --data-binary "@tmp/dev-up/chat.json"' -f $gatewayPort)
Write-Host '  stop      powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev-up.ps1 -Down'
