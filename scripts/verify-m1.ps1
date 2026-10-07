# InferGate M1 -- end-to-end verification with curl.exe.
#
# M1 is multi-provider routing and failover. The interesting claims are all about
# what DID NOT happen -- a request that never reached the dead backend, a client
# that never saw the 503, a breaker that stopped spending attempts on a corpse --
# and none of those are visible in a single response body. So this script drives a
# real three-replica fleet with real binaries and asserts on the wire-level
# evidence the gateway adds:
#
#   X-InferGate-Upstream-Name   which backend actually answered
#   X-InferGate-Tried           every backend tried, in order
#   X-InferGate-Attempt         1-based attempt counter
#   /admin/breakers             what the breakers decided, and why
#   /metrics                    cumulative counters, compared as DELTAS
#
# It complements two other gates rather than replacing them:
#
#   cmd/verify-m1   in-process, 64 assertions, ~10s, exit code is the CI gate.
#   this script     real binaries, a real process being killed mid-flight, and a
#                   shell you can poke at afterwards. This is what a reviewer
#                   runs, and it is the only one that proves the thing works when
#                   a backend DIES rather than merely answering badly.
#
# Usage (pwsh does not exist on this host):
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m1.ps1
#
# Everything is confined to this repository: binaries in bin\, logs and bodies in
# tmp\, and all four processes are stopped in the finally block.

[CmdletBinding()]
param(
    [int]$GatewayPort = 18180,
    [int]$PrimaryPort = 19100,
    [int]$SecondaryPort = 19101,
    [int]$ToolsPort = 19102,
    [int]$TokenDelayMillis = 2,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
$gwLog = Join-Path $tmpDir 'm1-gateway.log'

$script:passed = 0
$script:failed = 0

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

function Assert-Equal {
    param([string]$Label, $Expected, $Actual)
    Assert-True $Label ($Expected -eq $Actual) "expected '$Expected', got '$Actual'"
}

function Assert-Contains {
    param([string]$Label, [string]$Haystack, [string]$Needle)
    # Literal substring test, NOT -like: -like applies wildcard matching, so a
    # needle containing brackets becomes a character class and reports false
    # failures against text that demonstrably contains it.
    Assert-True $Label ($null -ne $Haystack -and $Haystack.Contains($Needle)) `
        "expected to find: $Needle`n        actual: $(if ($Haystack) { $Haystack.Substring(0, [Math]::Min(400, $Haystack.Length)) })"
}

$utf8NoBom = New-Object System.Text.UTF8Encoding($false)

# Get-ConfigScalar reads a value out of the fleet config this run renders, so the
# policy asserted below is the one the gateway was actually handed.
. (Join-Path $PSScriptRoot 'lib\config-scalar.ps1')

function Invoke-Curl {
    param([string[]]$Arguments)
    # Capture stdout ONLY. Windows PowerShell 5.1 turns anything a native command
    # writes to stderr into a NativeCommandError, and with an inherited
    # $ErrorActionPreference of 'Stop' a single curl note aborts the whole script
    # mid-section. curl diagnostics are not assertions; error bodies arrive on
    # stdout anyway.
    return (& curl.exe @Arguments 2>$null | Out-String)
}

function Read-Text {
    param([string]$Path)
    if (-not (Test-Path $Path)) { return '' }
    # StreamReader, never Get-Content: a Get-Content/Set-Content round trip has
    # produced mojibake in this repo, and a response body carrying CJK content
    # would be silently corrupted by the console codepage.
    $reader = New-Object System.IO.StreamReader($Path, $script:utf8NoBom, $true)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
}

function New-BodyFile {
    <#
        Writes a JSON request body to a temp file and returns the '@path' form for
        curl. This indirection is not cosmetic: Windows PowerShell 5.1 strips the
        double quotes out of an argument when it re-quotes it for a native
        executable, so -d '{"model":"x"}' reaches curl as {model:x}. Passing the
        payload as a file with --data-binary sends the bytes verbatim.
    #>
    param([string]$Name, [string]$Json)
    $path = Join-Path $tmpDir $Name
    [System.IO.File]::WriteAllText($path, $Json, $script:utf8NoBom)
    return "@$path"
}

function Get-Header {
    param([string]$Headers, [string]$Name)
    $m = [regex]::Match($Headers, "(?im)^$([regex]::Escape($Name)):\s*(.+?)\s*$")
    if ($m.Success) { return $m.Groups[1].Value }
    return ''
}

function Send-Chat {
    <#
        One proxied request. The body and the headers go to FILES rather than
        being captured from stdout: curl's -i output interleaves headers and body
        and a streamed body cannot be reliably split back out, whereas -D/-o give
        each one a file apiece. -w prints the status code last, so the return
        value stays a single number even when the body is empty (a 400 from the
        gateway's own error path, or a client disconnect).
    #>
    param(
        [string]$Url,
        [string]$BodyFile,
        [string[]]$ExtraHeaders = @(),
        [string]$Tag = 'resp'
    )
    $bodyPath = Join-Path $tmpDir "$Tag.body"
    $hdrPath = Join-Path $tmpDir "$Tag.hdr"
    $a = @('-s', '-o', $bodyPath, '-D', $hdrPath, '-w', '%{http_code}',
        $Url, '-H', 'Content-Type: application/json', '--data-binary', $BodyFile)
    foreach ($h in $ExtraHeaders) { $a += @('-H', $h) }
    $code = (Invoke-Curl $a).Trim()
    $status = 0
    if ($code -match '^\d+$') { $status = [int]$code }
    return [pscustomobject]@{
        Status  = $status
        Headers = (Read-Text $hdrPath)
        Body    = (Read-Text $bodyPath)
        Name    = (Get-Header (Read-Text $hdrPath) 'X-InferGate-Upstream-Name')
        Tried   = (Get-Header (Read-Text $hdrPath) 'X-InferGate-Tried')
        Attempt = (Get-Header (Read-Text $hdrPath) 'X-InferGate-Attempt')
    }
}

function Get-SeriesValue {
    <#
        Sums every sample of a Prometheus series whose label set CONTAINS the
        given labels, whatever order they appear in.

        The containment test is a lookahead rather than a prefix match, and that
        is not pedantry: the exported families put their labels in different
        orders (infergate_requests_total starts with route=, infergate_failovers_total
        starts with upstream=), so a prefix match silently returns zero for the
        ones that happen to start with something else -- a wrong number that
        looks exactly like "the gateway did nothing".

        Summing all matches is also the semantics wanted here: a counter family
        split across labels, added up. Labels is an ordered regex fragment, so a
        two-label check is written 'upstream="primary",\s*state="open"'.
    #>
    param([string]$Text, [string]$Series, [string]$Labels = '')
    $lookahead = ''
    if ($Labels -ne '') { $lookahead = '(?=[^}]*' + $Labels + ')' }
    $pattern = '(?m)^' + [regex]::Escape($Series) + '\{' + $lookahead + '[^}]*\}\s+([0-9.eE+-]+)\s*$'
    $total = 0.0
    foreach ($m in [regex]::Matches($Text, $pattern)) {
        $total += [double]::Parse($m.Groups[1].Value, [System.Globalization.CultureInfo]::InvariantCulture)
    }
    return $total
}

function Read-OpenLog {
    <#
        Reads a log file that another process still has open for writing.

        A plain StreamReader constructor fails with "the process cannot access
        the file because it is being used by another process": Start-Process
        redirection does not share the handle. Opening it explicitly with
        FileShare.ReadWrite is what makes the file readable while the gateway is
        still logging to it.
    #>
    param([string]$Path)
    if (-not (Test-Path $Path)) { return '' }
    try {
        $fs = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        try {
            $sr = New-Object System.IO.StreamReader($fs)
            try { return $sr.ReadToEnd() } finally { $sr.Dispose() }
        }
        finally { $fs.Dispose() }
    }
    catch {
        Write-Host "  could not read $Path : $_" -ForegroundColor DarkYellow
        return ''
    }
}

# ---------------------------------------------------------------------------
# 0. Preconditions
# ---------------------------------------------------------------------------
Write-Section '0. Preconditions'

if (-not (Get-Command curl.exe -ErrorAction SilentlyContinue)) {
    throw 'curl.exe not found on PATH. Windows 10 1803+ ships it; otherwise install curl.'
}
Write-Host "  curl    : $((& curl.exe --version | Select-Object -First 1))"

$goShim = Join-Path $repo 'tools\go.cmd'
if (-not (Test-Path $goShim)) { throw "missing toolchain shim: $goShim" }
Write-Host "  go shim : $goShim"

New-Item -ItemType Directory -Force -Path $binDir, $tmpDir | Out-Null

# ---------------------------------------------------------------------------
# 1. Build
# ---------------------------------------------------------------------------
Write-Section '1. Build'
& $goShim build -o (Join-Path $binDir 'infergate.exe') ./cmd/infergate
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/infergate failed (rc=$LASTEXITCODE)" }
& $goShim build -o (Join-Path $binDir 'mockupstream.exe') ./cmd/mockupstream
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/mockupstream failed (rc=$LASTEXITCODE)" }
Write-Host '  built bin\infergate.exe and bin\mockupstream.exe' -ForegroundColor Green

# Rewrite the fleet config onto the ports this run owns, so the script never
# fights a stale process on 9100-9102.
$gatewayCfg = Join-Path $tmpDir 'm1-gateway.yaml'
$rewritten = (Read-Text (Join-Path $repo 'configs\routing-local.yaml')) `
    -replace 'http://127\.0\.0\.1:9100', "http://127.0.0.1:$PrimaryPort" `
    -replace 'http://127\.0\.0\.1:9101', "http://127.0.0.1:$SecondaryPort" `
    -replace 'http://127\.0\.0\.1:9102', "http://127.0.0.1:$ToolsPort" `
    -replace 'listen: ":8080"', "listen: `":$GatewayPort`""
[System.IO.File]::WriteAllText($gatewayCfg, $rewritten, $utf8NoBom)
Write-Host "  wrote $gatewayCfg (gateway :$GatewayPort -> primary :$PrimaryPort / secondary :$SecondaryPort / tools :$ToolsPort)"

# The strategy and the failover cap are NOT this script's constants: they belong
# to the fleet config it just rendered. Reading them back out of that text is
# what keeps a config edit from turning a gateway that did exactly what it was
# told red - the same rule the M0/M2/M4/M6 gates follow.
$policyStrategy = Get-ConfigScalar -Text $rewritten -Block 'routing' -Key 'strategy'
$policyMaxFailures = Get-ConfigScalar -Text $rewritten -Block 'health' -Key 'max_failures_per_request'
Assert-True 'the rendered fleet config declares its routing strategy' `
    ($null -ne $policyStrategy) "read $gatewayCfg"
Assert-True 'the rendered fleet config declares its failover cap' `
    ($null -ne $policyMaxFailures) "read $gatewayCfg"

# The three replicas are named by the config as well, and the gate never sees
# those names except through what the gateway reports back (the
# X-InferGate-Upstream-Name header, the breaker document, metrics labels, the
# request log). The entry is picked by the port this run substituted into it,
# which is the one handle on a replica the gate owns; a rename in the sample is
# then a config edit and nothing else.
$policyPrimary = Get-ConfigListScalar -Text $rewritten -Block 'upstreams' -Key 'name' `
    -WhereKey 'base_url' -WhereValue "http://127.0.0.1:$PrimaryPort"
$policySecondary = Get-ConfigListScalar -Text $rewritten -Block 'upstreams' -Key 'name' `
    -WhereKey 'base_url' -WhereValue "http://127.0.0.1:$SecondaryPort"
$policyTools = Get-ConfigListScalar -Text $rewritten -Block 'upstreams' -Key 'name' `
    -WhereKey 'base_url' -WhereValue "http://127.0.0.1:$ToolsPort"
Assert-True 'the rendered fleet config declares the backend on the primary port' `
    ($null -ne $policyPrimary) "read $gatewayCfg"
Assert-True 'the rendered fleet config declares the backend on the secondary port' `
    ($null -ne $policySecondary) "read $gatewayCfg"
Assert-True 'the rendered fleet config declares the backend on the tools port' `
    ($null -ne $policyTools) "read $gatewayCfg"

function Start-Replica {
    param([int]$Port, [string]$Name, [string]$LogName)
    $log = Join-Path $tmpDir $LogName
    [System.IO.File]::WriteAllText($log, '', $utf8NoBom)
    $p = Start-Process -FilePath (Join-Path $binDir 'mockupstream.exe') `
        -ArgumentList @('-listen', ":$Port", '-name', $Name, '-token-delay', "${TokenDelayMillis}ms") `
        -RedirectStandardOutput $log -RedirectStandardError (Join-Path $tmpDir "$LogName.err") `
        -PassThru -WindowStyle Hidden
    Write-Host "  mockupstream.exe -name $Name pid=$($p.Id) listening :$Port"
    return $p
}

function Wait-Healthy {
    param([int]$Port, [int]$Seconds = 15)
    $deadline = (Get-Date).AddSeconds($Seconds)
    $code = ''
    while ((Get-Date) -lt $deadline) {
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$Port/healthz")).Trim()
        if ($code -eq '200') { break }
        Start-Sleep -Milliseconds 150
    }
    return $code
}

$primaryProc = $null
$secondaryProc = $null
$toolsProc = $null
$gwProc = $null
try {
    # -----------------------------------------------------------------------
    # 2. Start a three-replica fleet
    # -----------------------------------------------------------------------
    Write-Section '2. Start a three-replica fleet'
    $primaryProc = Start-Replica -Port $PrimaryPort -Name 'primary' -LogName 'm1-primary.log'
    $secondaryProc = Start-Replica -Port $SecondaryPort -Name 'secondary' -LogName 'm1-secondary.log'
    $toolsProc = Start-Replica -Port $ToolsPort -Name 'tools' -LogName 'm1-tools.log'

    Assert-Equal 'the primary replica answers /healthz' '200' (Wait-Healthy -Port $PrimaryPort)
    Assert-Equal 'the secondary replica answers /healthz' '200' (Wait-Healthy -Port $SecondaryPort)
    Assert-Equal 'the tools replica answers /healthz' '200' (Wait-Healthy -Port $ToolsPort)

    [System.IO.File]::WriteAllText($gwLog, '', $utf8NoBom)
    $gwProc = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $gatewayCfg) `
        -RedirectStandardOutput $gwLog -RedirectStandardError (Join-Path $tmpDir 'm1-gateway.err') `
        -PassThru -WindowStyle Hidden
    Write-Host "  infergate.exe pid=$($gwProc.Id) listening :$GatewayPort"

    $base = "http://127.0.0.1:$GatewayPort"
    $deadline = (Get-Date).AddSeconds(15)
    $code = ''
    while ((Get-Date) -lt $deadline) {
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base/readyz")).Trim()
        if ($code -eq '200') { break }
        Start-Sleep -Milliseconds 150
    }
    Assert-Equal 'the gateway answers /readyz' '200' $code

    $upstreamDoc = (Invoke-Curl @('-s', "$base/admin/upstreams")) | ConvertFrom-Json
    Assert-Equal 'the fleet has exactly three backends' 3 @($upstreamDoc.upstreams).Count
    Assert-Equal "routing strategy comes from the config file ('$policyStrategy')" $policyStrategy $upstreamDoc.routing.strategy
    Assert-Equal "the failover chain is capped by max_failures_per_request ($policyMaxFailures)" ([int]$policyMaxFailures) $upstreamDoc.routing.max_attempts

    $chatBody = New-BodyFile 'm1-chat.json' '{"model":"mock-gpt","messages":[{"role":"user","content":"hello from curl"}]}'
    $streamBody = New-BodyFile 'm1-stream.json' '{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"stream please"}]}'

    # -----------------------------------------------------------------------
    # 3. Priority routing picks the best backend
    # -----------------------------------------------------------------------
    Write-Section '3. Priority routing picks the best backend'
    $r = Send-Chat "$base/v1/chat/completions" $chatBody
    Assert-Equal 'a healthy request answers 200' 200 $r.Status
    Assert-Equal "priority 1 wins ('$policyPrimary')" $policyPrimary $r.Name
    Assert-Equal 'no failover happened' '' $r.Tried
    Assert-Contains 'the response is the provider answer, not a re-encoding' $r.Body '"mock answer to'

    # -----------------------------------------------------------------------
    # 4. A DEAD backend fails over -- the whole point of M1
    # -----------------------------------------------------------------------
    Write-Section '4. A dead backend fails over'
    Write-Host "  killing the primary replica (pid=$($primaryProc.Id))" -ForegroundColor DarkYellow
    Stop-Process -Id $primaryProc.Id -Force -ErrorAction SilentlyContinue
    $primaryProc = $null
    Start-Sleep -Milliseconds 400

    # Non-streaming first: a transport failure (connection refused) happens
    # before any byte is written downstream, which is the case failover can
    # always absorb.
    $r = Send-Chat "$base/v1/chat/completions" $chatBody
    Assert-Equal 'the request still succeeds' 200 $r.Status
    Assert-Equal "the answer came from the next backend ('$policySecondary')" $policySecondary $r.Name
    Assert-Equal "every backend tried is named in order ('$policyPrimary, $policySecondary')" "$policyPrimary, $policySecondary" $r.Tried
    Assert-Equal 'the attempt counter reports the second attempt' '2' $r.Attempt
    # The body cannot say which replica answered it: cmd/mockupstream answers
    # `mock answer to "<prompt>"` on every replica, and the per-replica identity
    # is in the X-InferGate-Upstream-Name header asserted above. Assert the
    # OpenAI envelope instead, so a byte-copied body is still proven to have
    # traversed the gateway rather than been replaced by an error page.
    Assert-Contains 'the client never sees the dead backend error' $r.Body '"object":"chat.completion"'

    # Then the SAME path for a stream. This is the claim that matters most: SSE
    # is not a separate code path with its own error handling, so a stream
    # fails over exactly like a JSON request, and the client still gets a
    # terminated SSE body with a [DONE] sentinel rather than a truncated one.
    $s = Send-Chat "$base/v1/chat/completions" $streamBody @() 'm1-stream'
    Assert-Equal 'a stream fails over too' 200 $s.Status
    Assert-Equal "the stream came from the surviving backend ('$policySecondary')" $policySecondary $s.Name
    Assert-Equal "the stream names both attempts ('$policyPrimary, $policySecondary')" "$policyPrimary, $policySecondary" $s.Tried
    Assert-Contains 'the stream is a real SSE body' $s.Headers 'text/event-stream'
    Assert-True 'the streamed failover ends with the [DONE] sentinel' `
        ($s.Body.TrimEnd("`r", "`n").EndsWith('data: [DONE]')) `
        "tail: $($s.Body.Substring([Math]::Max(0, $s.Body.Length - 80)))"
    Assert-Contains 'the streamed content came from the surviving backend' $s.Body '"object":"chat.completion.chunk"'

    # -----------------------------------------------------------------------
    # 5. Repeated failures open the breaker, and an open breaker spends NO attempt
    # -----------------------------------------------------------------------
    Write-Section '5. The breaker takes the dead backend out of rotation'
    # min_requests=4 and failure_ratio=0.5 in the fleet config. The ratio is
    # computed over the whole window, so the healthy request from section 3 and
    # the two failed attempts from section 4 already count: primary should trip
    # on the first request here. Loop rather than hard-code the iteration -- the
    # window is time-bucketed, and asserting on the OBSERVABLE transition is both
    # stronger and less brittle than asserting on the count that produced it.
    #
    # The transition has TWO halves and both matter: while the breaker is closed
    # the dead backend is still tried (Tried names it and then the next one,
    # because the router ranks an open backend last but does not remove it), and
    # once it opens that backend is not reached at all. That second state is
    # invisible in X-InferGate-Tried -- the header is only written when more than
    # one attempt happened, and exactly one does -- so the signature is the next
    # backend's name in X-InferGate-Upstream-Name WITH NO Tried header.
    $skipped = $false
    $sawFailover = $false
    $attempts = 0
    for ($i = 1; $i -le 8; $i++) {
        $attempts = $i
        $r = Send-Chat "$base/v1/chat/completions" $chatBody @() 'm1-trip'
        if ($r.Tried -eq "$policyPrimary, $policySecondary") { $sawFailover = $true }
        if ($r.Status -eq 200 -and $r.Name -eq $policySecondary -and $r.Tried -eq '') {
            $skipped = $true
            break
        }
    }
    Assert-True 'the dead backend was still tried while its breaker was closed' $sawFailover `
        "after $attempts requests, tried='$($r.Tried)' name='$($r.Name)' status=$($r.Status)"
    Assert-True 'the dead backend stopped being tried entirely once the breaker opened' $skipped `
        "after $attempts requests, tried='$($r.Tried)' name='$($r.Name)' status=$($r.Status)"

    $breakerDoc = (Invoke-Curl @('-s', "$base/admin/breakers")) | ConvertFrom-Json
    $primaryBreaker = @($breakerDoc.upstreams | Where-Object { $_.name -eq $policyPrimary })[0]
    Assert-Equal 'the breaker reports the dead backend as open' 'open' $primaryBreaker.state
    Assert-True 'the breaker tripped at least once' ($primaryBreaker.trips -ge 1) "trips=$($primaryBreaker.trips)"
    # Asserted against the values the gateway itself publishes in health{}, not
    # against literals: an open breaker must rest on at least min_requests
    # samples, or it would be tripping on a single unlucky request.
    Assert-True 'the breaker is acting on real evidence from the window' `
        ($primaryBreaker.attempts -ge $breakerDoc.health.min_requests) `
        "attempts=$($primaryBreaker.attempts) min_requests=$($breakerDoc.health.min_requests)"
    Assert-True 'the failure ratio crossed the configured threshold' `
        ($primaryBreaker.failure_ratio -ge $breakerDoc.health.failure_ratio) `
        "ratio=$($primaryBreaker.failure_ratio) threshold=$($breakerDoc.health.failure_ratio)"

    # The strongest form of the claim: an OPEN breaker means the dead backend
    # receives no attempt AT ALL. Asserting on the response alone cannot
    # distinguish "not tried" from "tried and failed instantly", so compare the
    # cumulative failover counter across one request as a DELTA.
    $metricsBefore = Invoke-Curl @('-s', "$base/metrics")
    $failoversBefore = Get-SeriesValue -Text $metricsBefore -Series 'infergate_failovers_total' -Labels "upstream=`"$policyPrimary`""
    $r = Send-Chat "$base/v1/chat/completions" $chatBody @() 'm1-open'
    $metricsAfter = Invoke-Curl @('-s', "$base/metrics")
    $failoversAfter = Get-SeriesValue -Text $metricsAfter -Series 'infergate_failovers_total' -Labels "upstream=`"$policyPrimary`""
    Assert-Equal 'a request served while the breaker is open adds no attempt on the dead backend' `
        $failoversBefore $failoversAfter
    Assert-Equal 'the request still succeeded' 200 $r.Status

    # -----------------------------------------------------------------------
    # 6. Capability routing
    # -----------------------------------------------------------------------
    Write-Section '6. Capability routing excludes what cannot serve the request'
    # Only the tools replica declares "tools", so the requirement must route
    # past two higher-priority backends.
    $r = Send-Chat "$base/v1/chat/completions" $chatBody @('-H', 'X-InferGate-Capabilities: tools') 'm1-caps'
    Assert-Equal 'a required capability reaches the backend that has it' 200 $r.Status
    Assert-Equal "the capability backend served it ('$policyTools')" $policyTools $r.Name
    Assert-Equal 'no capability-incapable backend was tried' '' $r.Tried

    # A capability nobody has is a 400, NOT a silent downgrade. Downgrading a
    # tool-calling request to a model that cannot call tools produces a
    # plausible-looking wrong answer, which is the worst possible failure mode
    # for an agent.
    $r = Send-Chat "$base/v1/chat/completions" $chatBody @('-H', 'X-InferGate-Capabilities: embeddings') 'm1-nocap'
    Assert-Equal 'an unservable capability is a 400' 400 $r.Status
    Assert-Contains 'the error names the gateway, not a backend' $r.Body '"infergate_no_upstream"'

    # -----------------------------------------------------------------------
    # 7. Explicit pinning
    # -----------------------------------------------------------------------
    Write-Section '7. Explicit pinning bypasses routing'
    $r = Send-Chat "$base/v1/chat/completions" $chatBody @('-H', "X-InferGate-Upstream: $policyTools") 'm1-pin'
    Assert-Equal 'a pinned backend serves the request' 200 $r.Status
    Assert-Equal "the pin overrides priority and breaker state ('$policyTools')" $policyTools $r.Name
    Assert-Equal 'nothing else was tried' '' $r.Tried

    $r = Send-Chat "$base/v1/chat/completions" $chatBody @('-H', 'X-InferGate-Upstream: does-not-exist') 'm1-badpin'
    Assert-Equal 'an unknown pin is a 400, not a silent fallback' 400 $r.Status

    # -----------------------------------------------------------------------
    # 8. Observability surfaces
    # -----------------------------------------------------------------------
    Write-Section '8. Observability surfaces'
    $metrics = Invoke-Curl @('-s', "$base/metrics")
    Assert-Contains '/metrics exports a failover counter' $metrics 'infergate_failovers_total'
    Assert-Contains '/metrics exports breaker state as a gauge' $metrics 'infergate_breaker_state'
    Assert-Contains '/metrics exports breaker trips' $metrics 'infergate_breaker_trips_total'
    Assert-True 'the failover counter recorded the dead backend' `
        ((Get-SeriesValue -Text $metrics -Series 'infergate_failovers_total' -Labels "upstream=`"$policyPrimary`"") -ge 1) `
        'no failover was recorded against primary'
    Assert-Equal 'the breaker state gauge is one-hot for the dead backend' 1 `
        (Get-SeriesValue -Text $metrics -Series 'infergate_breaker_state' -Labels "upstream=`"$policyPrimary`"")
    Assert-Equal 'the gauge says the dead backend is open' 1 `
        (Get-SeriesValue -Text $metrics -Series 'infergate_breaker_state' -Labels "upstream=`"$policyPrimary`",state=`"open`"")
    Assert-True 'requests are attributed to the backend that actually answered' `
        ((Get-SeriesValue -Text $metrics -Series 'infergate_requests_total' -Labels "upstream=`"$policySecondary`"") -ge 1) `
        'no request was attributed to secondary'

    $stats = (Invoke-Curl @('-s', "$base/stats")) | ConvertFrom-Json
    $secondarySeries = @($stats.series | Where-Object { $_.upstream -eq $policySecondary })
    Assert-True '/stats carries a per-upstream series for the surviving backend' `
        ($secondarySeries.Count -ge 1) "series: $(@($stats.series | ForEach-Object { "$($_.upstream)/$($_.outcome)" }) -join ',')"
    Assert-True '/stats carries a p95 latency' `
        ($null -ne $stats.latency.p95 -and $stats.latency.p95 -ne '') "latency.p95=$($stats.latency.p95)"

    # -----------------------------------------------------------------------
    # 9. Recovery: a replaced backend is admitted again
    # -----------------------------------------------------------------------
    Write-Section '9. Recovery'
    $primaryProc = Start-Replica -Port $PrimaryPort -Name 'primary' -LogName 'm1-primary.log'
    Assert-Equal 'the replacement replica answers /healthz' '200' (Wait-Healthy -Port $PrimaryPort)

    # Restarting the process is not enough: the breaker is still open, and a
    # half-open probe is admitted only after open_duration (30s here, chosen so
    # section 5 cannot race it). Resetting the breaker is what an operator does
    # after fixing a backend, and it is a POST because it mutates state.
    $reset = (Invoke-Curl @('-s', '-X', 'POST', "$base/admin/breakers/reset")) | ConvertFrom-Json
    Assert-Equal 'the breaker reset applies to the whole fleet' 'all' $reset.reset

    $r = Send-Chat "$base/v1/chat/completions" $chatBody @() 'm1-recover'
    Assert-Equal "traffic returns to the recovered backend ('$policyPrimary')" $policyPrimary $r.Name
    Assert-Equal 'recovery did not cost a failover' '' $r.Tried

    $breakerDoc = (Invoke-Curl @('-s', "$base/admin/breakers")) | ConvertFrom-Json
    $primaryBreaker = @($breakerDoc.upstreams | Where-Object { $_.name -eq $policyPrimary })[0]
    Assert-Equal 'the recovered backend is trusted again' 'closed' $primaryBreaker.state

    # -----------------------------------------------------------------------
    # 10. Logs tell the same story
    # -----------------------------------------------------------------------
    Write-Section '10. Gateway logs'
    $logText = Read-OpenLog $gwLog
    Assert-Contains 'logs record the listen event with the fleet' $logText 'infergate listening'

    # A failover is folded into the ONE request line rather than logged as a
    # second line: the record carries `attempts` and `tried`, so grepping for a
    # request id shows both the backend that failed and the one that answered.
    # This asserts on the LINE and not on a substring, because the answering
    # upstream's name on its own is also exactly what an ordinary healthy request logs -- the
    # claim is that this particular line admits two attempts.
    #
    # (slog quotes a value containing a space, so the attribute in the file is
    # tried="<first> -> <second>"; the regex tolerates the quotes.)
    # The two attribute regexes are built from the names the config gave these
    # replicas: the gateway logs the name it was configured with.
    $answeringAttr = "\supstream=" + [regex]::Escape($policySecondary) + "\b"
    $triedAttr = 'tried="?' + [regex]::Escape($policyPrimary) + ' -> ' + [regex]::Escape($policySecondary) + '"?'
    $failoverLines = @($logText -split "`n" | Where-Object {
        $_ -match 'msg=request' -and
        $_ -match $answeringAttr -and
        $_ -match '\sstatus=200\b' -and
        $_ -match '\sattempts=2\b' -and
        $_ -match $triedAttr
    })
    Assert-True 'the failed-over request is logged as one line naming both attempts' `
        ($failoverLines.Count -ge 1) `
        "no such line; lines naming secondary: $(@($logText -split "`n" | Where-Object { $_ -match $answeringAttr }).Count)"
    Assert-Contains "logs carry the upstream that answered ('$policySecondary')" $logText "upstream=$policySecondary"
    Write-Host "        gateway log: $gwLog" -ForegroundColor DarkGray
}
finally {
    Write-Section '11. Teardown'
    foreach ($p in @($gwProc, $primaryProc, $secondaryProc, $toolsProc)) {
        if ($p -and -not $p.HasExited) {
            Write-Host "  stopping pid=$($p.Id)"
            Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
        }
    }
    if ($KeepRunning) {
        Write-Host '  -KeepRunning set, but processes were stopped; rerun manually if you need a live fleet.' -ForegroundColor DarkYellow
    }
}

Write-Host ''
Write-Host ("=" * 72) -ForegroundColor DarkGray
if ($script:failed -eq 0) {
    Write-Host "  RESULT: $($script:passed)/$($script:passed) assertions passed" -ForegroundColor Green
    Write-Host '  OK: M1 routing and failover verified end to end with curl' -ForegroundColor Green
    exit 0
}
else {
    Write-Host "  RESULT: $($script:passed) passed, $($script:failed) FAILED" -ForegroundColor Red
    exit 1
}
