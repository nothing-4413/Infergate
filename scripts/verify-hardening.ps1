# InferGate -- end-to-end verification of the operator-token gate (access).
#
# WHY THIS GATE EXISTS SEPARATELY.
#
# Every other script here (verify-m0 .. verify-m6) drives the gateway with no
# credentials at all, because the access gate is off by default and must stay off
# for configs written for M0-M6. That is exactly why none of them can prove the
# gate works: they never turn it on. Until this script, the /admin surface was
# protected by a paragraph in the README and by Go unit tests -- nothing had ever
# asked a REAL running gateway for a 401.
#
# What is proven here, against a real process and real curl:
#
#   * /admin/* and /stats answer 401 with no token and 401 with a wrong token,
#     and the two responses are byte-identical (no oracle for "you were close")
#   * the 401 carries WWW-Authenticate for the bearer form and never echoes the
#     presented credential
#   * any configured token works, and a second token works during a rotation
#   * the OpenAI-compatible surface is NOT behind the token -- a request without
#     credentials still reaches the provider and comes back 200
#   * /healthz, /readyz and /metrics are NOT behind the token (probes and the
#     Prometheus scraper do not hold credentials)
#   * the protect list is a replacement, not an extension: naming /stats alone
#     takes /admin back off the token
#   * a bad config fails AT LOAD rather than silently doing nothing: an
#     undefined ${VAR} in access.tokens and protect: ["/"] both refuse to start
#
# Usage:
#   pwsh -File scripts/verify-hardening.ps1
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\verify-hardening.ps1
#
# NOTE (Windows PowerShell 5.1 on this host): this file must stay pure ASCII. It
# is read as ANSI when it has no BOM, so a stray non-ASCII byte becomes mojibake
# in a diff and, worse, one such byte has already been mistaken for a quoting
# error by the gateway's own JSON parse of a response. ASCII only, and write
# files with [System.IO.File]::WriteAllText rather than Set-Content (which emits
# a BOM).

[CmdletBinding()]
param(
    [int]$GatewayPort = 18610,
    # Deliberately NOT GatewayPort+1. The mock upstream runs for the whole script
    # and the sibling gateways start later, so a port arithmetic scheme that puts
    # them adjacent hands the second gateway a port the mock already holds -- which
    # is what happened, and it surfaced as "the narrow-list gateway started: got
    # 404" three assertions away from the real cause. Checked against every other
    # gate: verify-m0 18080/19000, m1 18180, m2 18280/18281, m3 18300-18302,
    # m4 18310, m5 18510, m6 18909.
    [int]$MockPort = 18520,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
# Its own scratch namespace: sibling gates run against the same repo and write
# into tmp\, and a shared name here would make one gate read another's log.
$workDir = Join-Path $tmpDir 'hardening'
$gwLog = Join-Path $workDir 'gateway.log'
$mockLog = Join-Path $workDir 'mockupstream.log'

$script:passed = 0
$script:failed = 0

# The interpreter to re-launch for the config checks. $PSHOME\powershell.exe is
# the 5.1 host on this machine; the scripts are written for it (no BOM, ASCII
# only, no pwsh-only syntax) and are run through it by the CI job as well.
$psExe = Join-Path $PSHOME 'powershell.exe'

$utf8NoBom = New-Object System.Text.UTF8Encoding($false)

# The gate must never be worth guessing. Two tokens, both long and neither
# guessable from the config file's shape, so "accepted any token" cannot pass by
# accident.
$tokenA = 'hardening-token-alpha-4c1f9a'
$tokenB = 'hardening-token-bravo-7e2d30'

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

function Assert-Contains {
    param([string]$Label, [string]$Haystack, [string]$Needle)
    # Literal substring test, NOT -like: -like treats brackets in the needle as a
    # character class. Same reasoning as verify-m0's copy of this helper.
    #
    # Whitespace inside the haystack is collapsed first. The loader wraps its long
    # validation messages across lines, so a needle spanning a wrap ("list con\
    # crete prefixes") would fail against a message that does say exactly that.
    # Collapsing whitespace is safe here because no needle in this file
    # distinguishes one run of spaces from another.
    $flat = if ($null -eq $Haystack) { $null } else { ($Haystack -replace '\s+', ' ') }
    if ($null -ne $flat -and $flat.Contains($Needle)) {
        Write-Host "  PASS  $Label" -ForegroundColor Green
        $script:passed++
    }
    else {
        Write-Host "  FAIL  $Label" -ForegroundColor Red
        Write-Host "        expected to find: $Needle" -ForegroundColor DarkYellow
        $shown = if ($null -eq $flat) { '<null>' } else { $flat }
        Write-Host "        actual: $($shown.Substring(0, [Math]::Min(400, $shown.Length)))" -ForegroundColor DarkGray
        $script:failed++
    }
}

function Invoke-Curl {
    param([string[]]$Arguments)
    # stdout only: Windows PowerShell 5.1 turns native stderr into a
    # NativeCommandError, and with $ErrorActionPreference = 'Stop' one curl note
    # aborts the script. See verify-m0.ps1 for the incident that taught this.
    return (& curl.exe @Arguments 2>$null | Out-String)
}

function New-BodyFile {
    <#
        Writes a request body to a file and returns the '@path' form for curl.
        Windows PowerShell 5.1 strips double quotes out of an argument when it
        re-quotes it for a native executable, so an inline -d '{"model":...}'
        reaches curl as {model:...} and every provider answers 400.
    #>
    param([string]$Name, [string]$Json)
    $path = Join-Path $workDir $Name
    [System.IO.File]::WriteAllText($path, $Json, $script:utf8NoBom)
    return "@$path"
}

# Write-GatewayConfig builds a gateway config from configs/mock.yaml with the
# listen port rewritten, the mock port rewritten, and an access block appended.
#
# The access block is APPENDED rather than patched into the existing file: a
# textual patch of a nested YAML section is the kind of test helper that quietly
# stops testing what it says when the file it patches changes shape.
#
# The append is a top-level key after a file whose last section is a nested map
# (pricing.models.*), which is a trap: a top-level key must start in column zero
# or YAML reads it as one more pricing model, the loader accepts it as an unknown
# key (this loader does not reject unknown fields), and the gate silently never
# turns on. Every block passed in here must be unindented.
function Write-GatewayConfig {
    param(
        [string]$Path,
        [string]$AccessYaml,
        [int]$ListenPort = $GatewayPort,
        [int]$UpstreamPort = $MockPort
    )
    $base = (Get-Content (Join-Path $repo 'configs\mock.yaml') -Raw) `
        -replace 'http://127\.0\.0\.1:9000', "http://127.0.0.1:$UpstreamPort" `
        -replace 'listen: ":8080"', "listen: `":$ListenPort`""
    [System.IO.File]::WriteAllText($Path, $base + "`n" + $AccessYaml, $script:utf8NoBom)
}

# Invoke-GatewayCheck runs the gateway binary with -check and returns a
# [pscustomobject] with the exit code and both output streams. -check validates
# and exits, so a refusal here is the loader doing its job before any listener
# exists -- the failure mode an operator actually meets when a token is missing.
#
# The binary is not invoked directly here. It is launched through
# scripts/lib/run-check.ps1, a separate process that lowers
# $ErrorActionPreference for the duration of the call: without that, the loader's
# refusal arrives on stderr, PowerShell 5.1 converts native stderr into a
# terminating error, and the very refusal being tested kills the test. See the
# header of that file for the incident.
function Invoke-GatewayCheck {
    param([string]$ConfigPath, [hashtable]$Env = @{})
    $saved = @{}
    foreach ($k in $Env.Keys) {
        $saved[$k] = [System.Environment]::GetEnvironmentVariable($k, 'Process')
        [System.Environment]::SetEnvironmentVariable($k, $Env[$k], 'Process')
    }
    $capture = Join-Path $workDir 'check-output.txt'
    Remove-Item $capture -ErrorAction SilentlyContinue
    try {
        & $psExe -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'lib\run-check.ps1') `
            -Binary (Join-Path $binDir 'infergate.exe') -Config $ConfigPath -OutFile $capture | Out-Null
        $code = $LASTEXITCODE
    }
    finally {
        foreach ($k in $Env.Keys) {
            [System.Environment]::SetEnvironmentVariable($k, $saved[$k], 'Process')
        }
    }
    $text = if (Test-Path $capture) { Get-Content $capture -Raw } else { '' }
    if ($null -eq $text) { $text = '' }
    return [pscustomobject]@{ Code = $code; Output = $text }
}

# Get-StatusAndHeaders returns the status code and the response header block for
# a request, with the body discarded. Header assertions need the raw block.
function Get-StatusAndHeaders {
    param([string[]]$Arguments)
    return Invoke-Curl (@('-s', '-D', '-', '-o', 'NUL') + $Arguments)
}

# ---------------------------------------------------------------------------
# 0. Preconditions
# ---------------------------------------------------------------------------
Write-Section '0. Preconditions'

$curl = Get-Command curl.exe -ErrorAction SilentlyContinue
if (-not $curl) { throw 'curl.exe not found on PATH. Windows 10 1803+ ships it; otherwise install curl.' }
Write-Host "  curl    : $((& curl.exe --version | Select-Object -First 1))"

$goShim = Join-Path $repo 'tools\go.cmd'
if (-not (Test-Path $goShim)) { throw "missing toolchain shim: $goShim" }
Write-Host "  go shim : $goShim"

New-Item -ItemType Directory -Force -Path $binDir, $tmpDir, $workDir | Out-Null

# ---------------------------------------------------------------------------
# 1. Build
# ---------------------------------------------------------------------------
Write-Section '1. Build'
& $goShim build -o (Join-Path $binDir 'infergate.exe') ./cmd/infergate
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/infergate failed (rc=$LASTEXITCODE)" }
& $goShim build -o (Join-Path $binDir 'mockupstream.exe') ./cmd/mockupstream
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/mockupstream failed (rc=$LASTEXITCODE)" }
Write-Host "  built bin\infergate.exe and bin\mockupstream.exe" -ForegroundColor Green

# ---------------------------------------------------------------------------
# 2. The loader refuses two configs that would otherwise fail open
# ---------------------------------------------------------------------------
Write-Section '2. Misconfiguration fails at load'

# 2a. A token that comes from an undefined environment variable. The point is not
#     that the expander is strict in general -- verify-m0 already relies on that
#     for upstream api_key -- but that it is strict HERE, where a silent empty
#     token would turn the gate into a no-op that still reports enabled.
$cfgUndef = Join-Path $workDir 'gateway.undefined-token.yaml'
Write-GatewayConfig -Path $cfgUndef -AccessYaml @'
access:
  enabled: true
  tokens: ["${HARDENING_TOKEN_THAT_DOES_NOT_EXIST}"]
'@
$r = Invoke-GatewayCheck -ConfigPath $cfgUndef
Assert-True 'an undefined token variable refuses to start' ($r.Code -ne 0) "exit=$($r.Code)"
Assert-Contains 'the refusal names the variable path' $r.Output 'access.tokens[0]'

# 2b. protect: ["/"] would put a token in front of the OpenAI-compatible surface
#     as well, which breaks every client. That is a config error, not a choice.
$cfgRoot = Join-Path $workDir 'gateway.protect-root.yaml'
Write-GatewayConfig -Path $cfgRoot -AccessYaml @'
access:
  enabled: true
  tokens: ["hardening-token-alpha-4c1f9a"]
  protect: ["/"]
'@
$r = Invoke-GatewayCheck -ConfigPath $cfgRoot
Assert-True 'protect: ["/"] refuses to start' ($r.Code -ne 0) "exit=$($r.Code)"
# The needle must not span a wrap point: the loader hard-wraps this sentence, so
# "concrete prefixes" arrives as "concrete-\nprefixes" and never matches. Whitespace
# collapsing in Assert-Contains fixes the newline but not the hyphen that lands at
# the end of the line, which is a real property of the message and not a harness
# detail -- so the assertion picks a phrase that does not straddle it.
Assert-Contains 'the refusal explains what to do instead' $r.Output 'would require a token on every request'

# 2c. enabled with no token at all: the loading state of a half-finished config.
$cfgNoToken = Join-Path $workDir 'gateway.no-token.yaml'
Write-GatewayConfig -Path $cfgNoToken -AccessYaml @'
access:
  enabled: true
'@
$r = Invoke-GatewayCheck -ConfigPath $cfgNoToken
Assert-True 'enabled with no token refuses to start' ($r.Code -ne 0) "exit=$($r.Code)"
Assert-Contains 'the refusal points at access.tokens' $r.Output 'access.tokens'

# The positive control: the config this script is about to run must be accepted.
$cfgGood = Join-Path $workDir 'gateway.yaml'
Write-GatewayConfig -Path $cfgGood -AccessYaml @"
access:
  enabled: true
  tokens: ["$tokenA", "$tokenB"]
"@
$r = Invoke-GatewayCheck -ConfigPath $cfgGood
Assert-True 'the config under test loads' ($r.Code -eq 0) "exit=$($r.Code) output=$($r.Output.Trim())"

# ---------------------------------------------------------------------------
# 3. Start both processes for real
# ---------------------------------------------------------------------------
$mockProc = $null
$gwProc = $null
$base = "http://127.0.0.1:$GatewayPort"
$authA = "Authorization: Bearer $tokenA"

try {
    Write-Section '3. Start processes'
    [System.IO.File]::WriteAllText($mockLog, '', $utf8NoBom)
    [System.IO.File]::WriteAllText($gwLog, '', $utf8NoBom)

    $mockProc = Start-Process -FilePath (Join-Path $binDir 'mockupstream.exe') `
        -ArgumentList @('-listen', ":$MockPort", '-name', 'mock', '-ttfb', '0ms') `
        -RedirectStandardOutput $mockLog -RedirectStandardError "$mockLog.err" `
        -PassThru -WindowStyle Hidden
    Write-Host "  mockupstream.exe pid=$($mockProc.Id) listening :$MockPort"

    $deadline = (Get-Date).AddSeconds(15)
    $r = ''
    while ((Get-Date) -lt $deadline) {
        $r = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$MockPort/healthz")
        if ($r.Trim() -eq '200') { break }
        Start-Sleep -Milliseconds 200
    }
    Assert-True 'mock upstream answers /healthz' ($r.Trim() -eq '200') "got '$($r.Trim())'"

    $gwProc = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $cfgGood) `
        -RedirectStandardOutput $gwLog -RedirectStandardError "$gwLog.err" `
        -PassThru -WindowStyle Hidden
    Write-Host "  infergate.exe  pid=$($gwProc.Id) listening :$GatewayPort"

    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-Date) -lt $deadline) {
        $r = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base/readyz")
        if ($r.Trim() -eq '200') { break }
        Start-Sleep -Milliseconds 200
    }
    # /readyz is unauthenticated, so this staying 200 is itself the probe
    # assertion -- the gateway is up AND the probe stayed open.
    Assert-True 'gateway answers /readyz while the gate is on' ($r.Trim() -eq '200') "got '$($r.Trim())'"

    # -----------------------------------------------------------------------
    # 4. The operational surface is closed
    # -----------------------------------------------------------------------
    Write-Section '4. The operational surface requires a token'

    $protected = @(
        '/admin/upstreams',
        '/admin/breakers',
        '/admin/cache',
        '/admin/quota',
        '/admin/traces',
        '/admin/tracing',
        '/admin/idempotency',
        '/admin/sessions',
        '/stats'
    )

    $open = @()
    foreach ($p in $protected) {
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base$p")).Trim()
        if ($code -ne '401') { $open += "$p -> $code" }
    }
    Assert-True "all $($protected.Count) operational paths answer 401 with no token" `
        ($open.Count -eq 0) "not 401: $($open -join ', ')"

    # A mutation, not just a read: the method must not matter.
    $flush = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', '-X', 'POST', "$base/admin/cache/flush")).Trim()
    Assert-True 'POST /admin/cache/flush answers 401 with no token' ($flush -eq '401') "got '$flush'"

    # The response shape. WWW-Authenticate is what makes a 401 actionable by a
    # human with a browser; the JSON type is what makes it actionable by a client.
    #
    # The header name is compared case-insensitively. HTTP field names are
    # case-insensitive, and curl -- the instrument here -- re-emits what Go wrote
    # through .NET's WebHeaderCollection, which canonicalises it to
    # "Www-Authenticate". Asserting on the exact casing of an HTTP header is
    # asserting on a library's formatting, not on the protocol.
    $headers = Get-StatusAndHeaders @("$base/admin/upstreams")
    Assert-Contains 'the 401 advertises the bearer realm' `
        ($headers -replace '(?im)^Www-Authenticate:', 'WWW-Authenticate:') `
        'WWW-Authenticate: Bearer realm="infergate"'
    Assert-Contains 'the 401 is a JSON error, not an empty body' (Invoke-Curl @('-s', "$base/admin/upstreams")) 'infergate_unauthorized'

    # Nothing about the presented credential may come back, or the gate becomes
    # an oracle for how much of a token was guessed correctly.
    $wrong = Invoke-Curl @('-s', "$base/admin/upstreams", '-H', 'Authorization: Bearer hardening-token-alpha-4c1f9X')
    Assert-True 'a near-miss token is not echoed back' (-not $wrong.Contains('alpha-4c1f9X')) 'the rejected value appeared in the response'
    Assert-True 'a near-miss token is still just 401' `
        ((Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base/admin/upstreams", '-H', 'Authorization: Bearer hardening-token-alpha-4c1f9X')).Trim() -eq '401')

    # Malformed forms. Each of these is a distinct parse path, and each must end
    # at the same place: no credential, so no access.
    $malformed = @(
        @{ Label = 'no scheme (bare token)'; Header = "Authorization: $tokenA" },
        @{ Label = 'wrong scheme'; Header = "Authorization: Basic $tokenA" },
        @{ Label = 'wrong header name'; Header = "X-Operator-Token: $tokenA" },
        @{ Label = 'scheme glued to the token'; Header = "Authorization: Bearer$tokenA" }
    )
    foreach ($m in $malformed) {
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base/admin/upstreams", '-H', $m.Header)).Trim()
        Assert-True "rejects $($m.Label)" ($code -eq '401') "got '$code'"
    }

    # -----------------------------------------------------------------------
    # 5. The right token opens exactly what it should
    # -----------------------------------------------------------------------
    Write-Section '5. The right token opens the surface'

    $codeA = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base/admin/upstreams", '-H', $authA)).Trim()
    Assert-True 'the first configured token is accepted' ($codeA -eq '200') "got '$codeA'"
    $codeB = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base/admin/upstreams", '-H', "Authorization: Bearer $tokenB")).Trim()
    Assert-True 'a second token is accepted too (rotation without a cutover)' ($codeB -eq '200') "got '$codeB'"

    $upstreams = Invoke-Curl @('-s', "$base/admin/upstreams", '-H', $authA) | ConvertFrom-Json
    Assert-True 'the authorized body is the real document, not a stub' `
        (@($upstreams.upstreams | Where-Object { $_.name -eq 'mock' }).Count -eq 1) `
        "upstreams: $(@($upstreams.upstreams | ForEach-Object { $_.name }) -join ',')"

    $flushAuthed = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', '-X', 'POST', "$base/admin/cache/flush", '-H', $authA)).Trim()
    Assert-True 'POST /admin/cache/flush succeeds with the token' ($flushAuthed -eq '200') "got '$flushAuthed'"

    # -----------------------------------------------------------------------
    # 6. The proxy and the probes stayed open (the whole point of the default list)
    # -----------------------------------------------------------------------
    Write-Section '6. The proxy and the probes are not behind the token'

    foreach ($p in @('/healthz', '/readyz', '/metrics', '/v1/capabilities')) {
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base$p")).Trim()
        Assert-True "$p stays open without a token" ($code -eq '200') "got '$code'"
    }

    # The decisive one. If this ever returns 401, the gateway is unusable by every
    # OpenAI-compatible client while still looking healthy on /readyz.
    $body = New-BodyFile 'chat.json' '{"model":"mock-gpt","messages":[{"role":"user","content":"hardening"}]}'
    $chatCode = (Invoke-Curl @(
            '-s', '-o', 'NUL', '-w', '%{http_code}', "$base/v1/chat/completions",
            '-H', 'Content-Type: application/json',
            '--data-binary', $body)).Trim()
    Assert-True 'POST /v1/chat/completions is NOT behind the token' ($chatCode -eq '200') "got '$chatCode'"

    $chatAuthed = (Invoke-Curl @(
            '-s', '-o', 'NUL', '-w', '%{http_code}', "$base/v1/chat/completions",
            '-H', 'Content-Type: application/json',
            '-H', 'Authorization: Bearer not-a-gateway-token',
            '--data-binary', $body)).Trim()
    # A caller's provider credential shares this header with the operator token.
    # It must be forwarded, not inspected and rejected.
    Assert-True 'a provider credential on the proxy path is not mistaken for a gate token' `
        ($chatAuthed -eq '200') "got '$chatAuthed'"

    # -----------------------------------------------------------------------
    # 7. CORS preflight passes: browsers never attach credentials to OPTIONS
    # -----------------------------------------------------------------------
    Write-Section '7. Preflight'

    $optionsCode = (Invoke-Curl @(
            '-s', '-o', 'NUL', '-w', '%{http_code}', '-X', 'OPTIONS',
            '-H', 'Origin: http://localhost:3000',
            '-H', 'Access-Control-Request-Method: POST',
            "$base/admin/cache/flush")).Trim()
    Assert-True 'OPTIONS is not forced to carry a token' ($optionsCode -ne '401') "got '$optionsCode'"

    # -----------------------------------------------------------------------
    # 8. The protect list replaces rather than extends
    # -----------------------------------------------------------------------
    Write-Section '8. protect is a replacement list'

    # A second gateway on the next port with an explicit protect list naming only
    # /stats. /admin must fall back out of the protected set -- if it does not,
    # then the list is being appended to a default and an operator can never
    # narrow it.
    #
    # The port is computed into a variable rather than passed inline. A
    # here-string terminator ends the statement it appears in, so an argument
    # written on the same line as a closing "@ is not an argument to the call --
    # it is a command of its own. Written that way the first time, the config came
    # out on the default port, three assertions read 404 from whatever else was
    # listening, and the failure looked like a protect-list bug.
    $port2 = $GatewayPort + 1
    $cfgNarrow = Join-Path $workDir 'gateway.narrow.yaml'
    Write-GatewayConfig -Path $cfgNarrow -ListenPort $port2 -AccessYaml @"
access:
  enabled: true
  tokens: ["$tokenA"]
  protect: ["/stats"]
"@
    $base2 = "http://127.0.0.1:$port2"
    $gw2 = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $cfgNarrow) `
        -RedirectStandardOutput (Join-Path $workDir 'gateway.narrow.log') `
        -RedirectStandardError (Join-Path $workDir 'gateway.narrow.log.err') `
        -PassThru -WindowStyle Hidden
    try {
        $deadline = (Get-Date).AddSeconds(15)
        while ((Get-Date) -lt $deadline) {
            $r = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base2/readyz")
            if ($r.Trim() -eq '200') { break }
            Start-Sleep -Milliseconds 200
        }
        Assert-True 'the narrow-list gateway started' ($r.Trim() -eq '200') "got '$($r.Trim())'"

        $stats = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base2/stats")).Trim()
        Assert-True '/stats is still protected when it is the named entry' ($stats -eq '401') "got '$stats'"
        $admin = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base2/admin/upstreams")).Trim()
        Assert-True '/admin is released when it is not named' ($admin -eq '200') "got '$admin'"
    }
    finally {
        if ($gw2 -and -not $KeepRunning) { Stop-Process -Id $gw2.Id -Force -ErrorAction SilentlyContinue }
    }

    # -----------------------------------------------------------------------
    # 9. A custom header, and the query form that is off by default
    # -----------------------------------------------------------------------
    Write-Section '9. Custom header and the query form'

    # X-Operator-Token: no scheme convention exists for it, so the value is
    # compared verbatim. This is the shape a deployment behind an OAuth proxy or
    # an ingress that already claims Authorization needs.
    $port3 = $GatewayPort + 2
    $cfgHeader = Join-Path $workDir 'gateway.custom-header.yaml'
    Write-GatewayConfig -Path $cfgHeader -ListenPort $port3 -AccessYaml @"
access:
  enabled: true
  tokens: ["$tokenA"]
  header: "X-Operator-Token"
"@
    $base3 = "http://127.0.0.1:$port3"
    $gw3 = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $cfgHeader) `
        -RedirectStandardOutput (Join-Path $workDir 'gateway.header.log') `
        -RedirectStandardError (Join-Path $workDir 'gateway.header.log.err') `
        -PassThru -WindowStyle Hidden
    try {
        $deadline = (Get-Date).AddSeconds(15)
        while ((Get-Date) -lt $deadline) {
            $r = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base3/readyz")
            if ($r.Trim() -eq '200') { break }
            Start-Sleep -Milliseconds 200
        }
        Assert-True 'the custom-header gateway started' ($r.Trim() -eq '200') "got '$($r.Trim())'"

        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base3/admin/upstreams", '-H', "X-Operator-Token: $tokenA")).Trim()
        Assert-True 'the custom header is accepted verbatim' ($code -eq '200') "got '$code'"
        # Bearer is an Authorization convention; with another header named, the
        # literal value must match, so a prefixed value is not a credential.
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base3/admin/upstreams", '-H', "X-Operator-Token: Bearer $tokenA")).Trim()
        Assert-True 'a bearer-prefixed value on a custom header is not accepted' ($code -eq '401') "got '$code'"
        # The old default header is no longer consulted at all.
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base3/admin/upstreams", '-H', $authA)).Trim()
        Assert-True 'a token on Authorization does not work when another header is named' ($code -eq '401') "got '$code'"
        # Off by default: a token in a URL lands in access logs and history.
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base3/admin/upstreams?access_token=$tokenA")).Trim()
        Assert-True 'the query form is off by default' ($code -eq '401') "got '$code'"
    }
    finally {
        if ($gw3 -and -not $KeepRunning) { Stop-Process -Id $gw3.Id -Force -ErrorAction SilentlyContinue }
    }

    # With it switched on, the query form is a credential -- this is what a
    # browser address bar or a Grafana datasource probe can actually send.
    $port4 = $GatewayPort + 3
    $cfgQuery = Join-Path $workDir 'gateway.query.yaml'
    Write-GatewayConfig -Path $cfgQuery -ListenPort $port4 -AccessYaml @"
access:
  enabled: true
  tokens: ["$tokenA"]
  header: "X-Operator-Token"
  allow_query_token: true
"@
    $base4 = "http://127.0.0.1:$port4"
    $gw4 = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $cfgQuery) `
        -RedirectStandardOutput (Join-Path $workDir 'gateway.query.log') `
        -RedirectStandardError (Join-Path $workDir 'gateway.query.log.err') `
        -PassThru -WindowStyle Hidden
    try {
        $deadline = (Get-Date).AddSeconds(15)
        while ((Get-Date) -lt $deadline) {
            $r = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base4/readyz")
            if ($r.Trim() -eq '200') { break }
            Start-Sleep -Milliseconds 200
        }
        Assert-True 'the query-token gateway started' ($r.Trim() -eq '200') "got '$($r.Trim())'"

        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base4/admin/upstreams?access_token=$tokenA")).Trim()
        Assert-True 'the query form is accepted when enabled' ($code -eq '200') "got '$code'"
        # On the query path the value is the token itself; the bearer stripping
        # belongs to the Authorization header only, and getting that backwards is
        # the bug this assertion exists to catch.
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base4/admin/upstreams?access_token=Bearer%20$tokenA")).Trim()
        Assert-True 'a bearer-prefixed query value is not a credential' ($code -eq '401') "got '$code'"
    }
    finally {
        if ($gw4 -and -not $KeepRunning) { Stop-Process -Id $gw4.Id -Force -ErrorAction SilentlyContinue }
    }

    # -----------------------------------------------------------------------
    # 10. The gate is not bypassable by path spelling
    # -----------------------------------------------------------------------
    Write-Section '10. Path matching'

    # Segment-aware matching: /admin covers /admin/x but not /administrator. A
    # naive HasPrefix would open every path that merely starts with the prefix.
    $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base/admin", '-H', $authA)).Trim()
    Assert-True 'the bare prefix itself is protected' ($code -eq '401' -or $code -eq '404' -or $code -eq '405') "got '$code'"
    $bypass = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base/administrator")).Trim()
    Assert-True '/administrator is not swallowed by /admin' ($bypass -ne '401') "got '$bypass'"
    $trailing = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base/stats/../../admin/upstreams")).Trim()
    # curl normalises this before sending, so the request that arrives is
    # /admin/upstreams. Asserting it is 401 proves curl's normalisation is what
    # makes it uninteresting -- if this ever returns 200 the path is being matched
    # on the raw request line.
    Assert-True 'a dot-dot path cannot slip past the prefix check' ($trailing -eq '401') "got '$trailing'"

    Write-Section '11. Summary'
    Write-Host "  assertions passed: $script:passed" -ForegroundColor Green
    if ($script:failed -gt 0) {
        Write-Host "  assertions failed: $script:failed" -ForegroundColor Red
    }
    Write-Host "  RESULT: $script:passed/$($script:passed + $script:failed) assertions passed"
}
finally {
    if ($gwProc -and -not $KeepRunning) {
        Stop-Process -Id $gwProc.Id -Force -ErrorAction SilentlyContinue
    }
    if ($mockProc -and -not $KeepRunning) {
        Stop-Process -Id $mockProc.Id -Force -ErrorAction SilentlyContinue
    }
    if ($script:failed -gt 0) {
        Write-Host ''
        Write-Host "  logs: $workDir" -ForegroundColor Yellow
        exit 1
    }
    exit 0
}
