# InferGate M3 -- end-to-end verification of token and cost governance with curl.exe.
#
# M3 is a budget, and the interesting claims about a budget are about what did NOT
# happen: a request that never reached a provider because the tenant was out of
# tokens, a reservation that was given back when the request was refused, a
# counter that lives in Redis rather than in the process that is charging for it.
# None of that is visible in a 200 response, so this script drives real binaries
# and asserts on the wire-level evidence the gateway adds:
#
#   X-InferGate-Quota               allow | degrade | reject
#   X-InferGate-Quota-Reason        within-budget | tokens_per_day | cost_per_day_usd |
#                                   requests_per_minute | tokens_per_session | store-error
#   X-InferGate-Quota-Limit/-Used   the window's limit and the total before this request
#   X-InferGate-Quota-Model         the model a degraded request was rewritten to
#   X-InferGate-Quota-Max-Tokens    the completion ceiling a degraded request was capped to
#   /admin/quota                    the policy, the counters and a per-tenant report
#   /stats                          the same counters, per upstream
#   /metrics                        the quota families, compared against /admin/quota
#
# The claims that carry the most weight here are the ones a counter cannot fake:
#
#   * section 4 refuses a tenant and then proves the provider was NOT called for
#     the refusal. A gateway that checks the budget after proxying the request
#     would still return 429 -- and would still have spent the money.
#   * section 8 reads the request body the UPSTREAM actually received, so the
#     rewritten model and the lowered max_tokens are observed where they matter,
#     not inferred from a header the gateway writes about itself.
#   * section 10 kills the Redis the counters live in and shows the gateway
#     refuses (fail closed) rather than letting requests through unmetered, then
#     recovers the moment the store is back -- without being restarted.
#
# Usage (pwsh does not exist on this host -- Windows PowerShell 5.1 only):
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m3.ps1
#
# Everything is confined to this repository: binaries in bin\, generated configs,
# captured bodies and logs in tmp\, and every process it starts is stopped in the
# finally block. Pass -KeepRunning to leave the fleet up for inspection instead
# (useful when an assertion fails and you want to curl the running gateway).
#
# Nothing outside tmp\ is written, and no repository config is edited: the tmp
# configs are literal substitutions of configs\quota-local.yaml and
# configs\quota-redis.yaml, whose own comments stay as documentation.

[CmdletBinding()]
param(
    [int]$GatewayPort = 18300,
    [int]$RedisGatewayPort = 18301,
    [int]$UpstreamPort = 19600,
    [int]$RedisUpstreamPort = 19601,
    [int]$MiniredisPort = 6398,
    [int]$CaptureGatewayPort = 18302,
    [int]$CaptureUpstreamPort = 19602,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
$goShim = Join-Path $repo 'tools\go.cmd'
$psExe = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
$stamp = Get-Date -Format 'HHmmss'

$script:passed = 0
$script:failed = 0
$script:promptSeq = 0
# Every pid this script starts, recorded at the moment it starts it. This is the
# only trustworthy way to answer "did anything I started survive me?" on this
# host: the sandbox denies WMI process enumeration (see Get-TrackedProcess), and
# asking by image name would blame a concurrently running gate for a leak.
$script:startedPids = @()

function New-LogPath {
    <#
        Log file names are per-run: m3-<tag>-<HHmmss>-<pid>.log. A fixed name
        would be truncated by the first process of the next run, so a failing
        run's evidence would be destroyed by the run that follows it.
    #>
    param([string]$Tag)
    return (Join-Path $tmpDir "m3-$Tag-$stamp-$PID.log")
}

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
    <#
        $Extra is context for the failure line only, and it exists because six
        call sites below hand one in. PowerShell does NOT reject an extra
        positional argument to a simple function: it binds what it can and drops
        the rest, so without this parameter "no process started by this script
        survived it" printed "expected '0', got '1'" with no pid list -- the one
        thing an operator needs to tell a real leak from a slow teardown.
    #>
    param([string]$Label, $Expected, $Actual, [string]$Extra = '')
    $detail = "expected '$Expected', got '$Actual'"
    if ($Extra) { $detail += "`n        $Extra" }
    Assert-True $Label ($Expected -eq $Actual) $detail
}

function Assert-Contains {
    param([string]$Label, [string]$Haystack, [string]$Needle)
    # Literal substring test, NOT -like: -like applies wildcard matching, so a
    # needle containing brackets (label selectors, JSON fragments) becomes a
    # character class and reports a false failure against text that demonstrably
    # contains it.
    Assert-True $Label ($null -ne $Haystack -and $Haystack.Contains($Needle)) `
        "expected to find: $Needle`n        actual: $(if ($Haystack) { $Haystack.Substring(0, [Math]::Min(400, $Haystack.Length)) })"
}

function Assert-Number {
    <#
        Asserts a value is a number, and optionally that it respects a bound.
        "Retry-After is at least 1 second" cannot be asserted as a literal: the
        value is a clock reading, so the claim is about its TYPE and its bound.
    #>
    param([string]$Label, [string]$Text, [double]$Min = -1, [double]$Max = -1)
    $value = 0.0
    $ok = [double]::TryParse($Text, [ref]$value)
    if ($ok -and $Min -ge 0) { $ok = $value -ge $Min }
    if ($ok -and $Max -ge 0) { $ok = $value -le $Max }
    Assert-True $Label $ok "expected a number$(if ($Min -ge 0) { " >= $Min" })$(if ($Max -ge 0) { " <= $Max" }), got '$Text'"
}

$utf8NoBom = New-Object System.Text.UTF8Encoding($false)

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
    # produced mojibake in this repo, and a captured request body would be
    # silently corrupted by the console codepage.
    $reader = New-Object System.IO.StreamReader($Path, $script:utf8NoBom, $true)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
}

function New-BodyFile {
    <#
        Writes a JSON request body to a temp file and returns the '@path' form for
        curl. This indirection is not cosmetic: Windows PowerShell 5.1 strips the
        double quotes out of an argument when it re-quotes it for a native
        executable, so -d '{"model":"x"}' reaches curl as {model:x}. For M3 that
        would also change the length of the body, and the gateway estimates the
        reservation from the body's size.
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

function Get-Field {
    <# Reads one numeric field out of parsed JSON as a double; $null-safe. #>
    param($Json, [string]$Name)
    if ($null -eq $Json) { return $null }
    $v = $Json.$Name
    if ($null -eq $v) { return $null }
    return [double]$v
}

function New-ChatBody {
    param([string]$Prompt, [int]$MaxTokens = 0)
    $cap = ''
    if ($MaxTokens -gt 0) { $cap = ',"max_tokens":' + $MaxTokens }
    return '{"model":"mock-gpt"' + $cap + ',"messages":[{"role":"user","content":"' + $Prompt + '"}]}'
}

function Invoke-Http {
    <#
        One HTTP request, body and headers to FILES.

        -D/-o instead of -i because curl's -i interleaves headers and body and a
        body cannot be reliably split back out. -w prints the status code LAST so
        the captured stdout is a single number even when the body is empty.
    #>
    param(
        [string]$Url,
        [string]$Method = 'GET',
        [string]$BodyFile = $null,
        [string[]]$Headers = @(),
        [string]$Tag = 'm3'
    )
    $bodyPath = Join-Path $tmpDir "$Tag.body"
    $hdrPath = Join-Path $tmpDir "$Tag.hdr"
    [System.IO.File]::WriteAllText($bodyPath, '', $utf8NoBom)
    [System.IO.File]::WriteAllText($hdrPath, '', $utf8NoBom)
    $a = @('-s', '-o', $bodyPath, '-D', $hdrPath, '-w', '%{http_code}', '-X', $Method, $Url)
    foreach ($h in $Headers) { $a += @('-H', $h) }
    if ($BodyFile) { $a += @('-H', 'Content-Type: application/json', '--data-binary', $BodyFile) }
    $code = (Invoke-Curl $a).Trim()
    $status = 0
    if ($code -match '^\d+$') { $status = [int]$code }
    $hdrText = Read-Text $hdrPath
    return [pscustomobject]@{
        Status  = $status
        Headers = $hdrText
        Body    = (Read-Text $bodyPath)
        Quota   = (Get-Header $hdrText 'X-InferGate-Quota')
        Reason  = (Get-Header $hdrText 'X-InferGate-Quota-Reason')
        Limit   = (Get-Header $hdrText 'X-InferGate-Quota-Limit')
        Used    = (Get-Header $hdrText 'X-InferGate-Quota-Used')
        Model   = (Get-Header $hdrText 'X-InferGate-Quota-Model')
        MaxTok  = (Get-Header $hdrText 'X-InferGate-Quota-Max-Tokens')
        Retry   = (Get-Header $hdrText 'Retry-After')
        UpName  = (Get-Header $hdrText 'X-InferGate-Upstream-Name')
    }
}

function Send-Chat {
    <#
        A POST /v1/chat/completions through the gateway.

        The parameter is called Headers, not ExtraHeaders, because Windows
        PowerShell 5.1 SILENTLY DROPS an unknown named parameter on a plain
        function: a call site that says -Headers against a -ExtraHeaders
        parameter does not fail, it quietly sends the request with no tenant at
        all -- which, for a quota, looks exactly like every tenant sharing the
        default policy.
    #>
    param([string]$Base, [string]$BodyFile, [string[]]$Headers = @(), [string]$Tag = 'm3-chat')
    return (Invoke-Http -Url "$Base/v1/chat/completions" -Method 'POST' -BodyFile $BodyFile -Headers $Headers -Tag $Tag)
}

function Send-ChatAs {
    <# One chat request as a tenant (and optionally a session), fresh prompt. #>
    param(
        [string]$Base,
        [string]$Tenant,
        [string]$Tag,
        [string]$Prompt = '',
        [string]$Session = '',
        [int]$MaxTokens = 0
    )
    if ($Prompt -eq '') {
        $script:promptSeq++
        $Prompt = "$Tenant probe $($script:promptSeq.ToString('000'))"
    }
    $body = New-BodyFile "$Tag.json" (New-ChatBody $Prompt $MaxTokens)
    $h = @("X-InferGate-Tenant: $Tenant")
    if ($Session -ne '') { $h += "X-InferGate-Session: $Session" }
    return (Send-Chat -Base $Base -BodyFile $body -Headers $h -Tag $Tag)
}

function Get-Json {
    param([string]$Text)
    try { return ($Text | ConvertFrom-Json) } catch { return $null }
}

function Get-QuotaAdmin {
    <# GET /admin/quota as an object, or $null when it is not answering. #>
    param([string]$Base)
    $r = Invoke-Http -Url "$Base/admin/quota" -Tag 'm3-admin'
    if ($r.Status -ne 200) { return $null }
    return (Get-Json $r.Body)
}

function Get-QuotaReport {
    <# GET /admin/quota?tenant=..&session=.. : the per-tenant window report. #>
    param([string]$Base, [string]$Tenant, [string]$Session = '')
    $url = "$Base/admin/quota?tenant=$Tenant"
    if ($Session -ne '') { $url += "&session=$Session" }
    $r = Invoke-Http -Url $url -Tag 'm3-report'
    if ($r.Status -ne 200) { return $null }
    return (Get-Json $r.Body)
}

function Get-Stats {
    param([string]$Base)
    $r = Invoke-Http -Url "$Base/stats" -Tag 'm3-stats'
    if ($r.Status -ne 200) { return $null }
    return (Get-Json $r.Body)
}

function Get-UpstreamRequests {
    <#
        How many requests the gateway sent to one upstream, summed over every
        series /stats splits that upstream into (route, model, status, outcome).

        This is the counter that makes "the refusal never called the provider"
        checkable: a refused request is not attributed to any upstream at all, so
        the upstream's own count is the honest witness.
    #>
    param($Stats, [string]$Upstream)
    if ($null -eq $Stats) { return -1 }
    $total = 0.0
    foreach ($s in @($Stats.series)) {
        if ($s.upstream -eq $Upstream) { $total += [double]$s.count }
    }
    return $total
}

function Get-LocalRequestCount {
    <#
        How many requests one gateway sent to its upstream named "local".

        This is the counter that makes "the refusal never called the provider"
        checkable: a refused request is not attributed to any upstream at all, so
        the upstream's own count is the honest witness -- a header the gateway
        writes cannot be.
    #>
    param([string]$Base)
    return (Get-UpstreamRequests -Stats (Get-Stats -Base $Base) -Upstream 'local')
}

function Get-Metric {
    <#
        Sums every sample of a Prometheus series whose label set CONTAINS the
        given labels, whatever order they appear in -- and, unlike the M2 helper,
        also reads a family that is exported with NO labels at all
        ("infergate_quota_store_errors_total 5"), because M3 exports six families
        and three of them are unlabelled.

        The containment test is a lookahead rather than a prefix match, and that
        is not pedantry: a prefix match against a family whose labels are in a
        different order silently reads 0 -- a wrong number that looks exactly
        like "the quota never fired".
    #>
    param([string]$Text, [string]$Series, [string]$Labels = '')
    $esc = [regex]::Escape($Series)
    if ($Labels -ne '') {
        $pattern = '(?m)^' + $esc + '\{(?=[^}]*' + $Labels + ')[^}]*\}\s+([0-9.eE+-]+)\s*$'
    }
    else {
        $pattern = '(?m)^' + $esc + '(\{[^}]*\})?\s+([0-9.eE+-]+)\s*$'
    }
    $total = 0.0
    $found = $false
    foreach ($m in [regex]::Matches($Text, $pattern)) {
        # The value is always the LAST capture group, whatever came before it.
        $raw = $m.Groups[$m.Groups.Count - 1].Value
        $total += [double]::Parse($raw, [System.Globalization.CultureInfo]::InvariantCulture)
        $found = $true
    }
    if (-not $found) { return $null }
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

function Wait-Healthy {
    <# Polls /healthz (mock upstream) or /readyz (gateway); returns the last code. #>
    param([int]$Port, [string]$Path = '/healthz', [int]$Seconds = 15)
    $deadline = (Get-Date).AddSeconds($Seconds)
    $code = '000'
    while ((Get-Date) -lt $deadline) {
        $code = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$Port$Path")).Trim()
        if ($code -eq '200') { break }
        Start-Sleep -Milliseconds 150
    }
    return $code
}

function Test-PortOpen {
    <#
        True when something accepts a TCP connection on the port.

        A TcpClient is used rather than Test-NetConnection because the Windows
        PowerShell 5.1 cmdlet is dramatically slower, and this is called in tight
        polling loops.
    #>
    param([int]$Port, [int]$TimeoutMs = 400)
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

function Wait-PortOpen {
    param([int]$Port, [int]$Seconds = 10)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (Test-PortOpen -Port $Port) { return $true }
        Start-Sleep -Milliseconds 100
    }
    return (Test-PortOpen -Port $Port)
}

function Wait-PortClosed {
    # Killing a process releases its listening socket asynchronously; probe until
    # it is really gone (or give up, and let the assertion report the truth).
    param([int]$Port, [int]$Seconds = 5)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (-not (Test-PortOpen -Port $Port)) { return $true }
        Start-Sleep -Milliseconds 100
    }
    return (-not (Test-PortOpen -Port $Port))
}

function Assert-PortsFree {
    param([int[]]$Ports)
    foreach ($p in $Ports) {
        Assert-True "port $p is free before the run" (-not (Test-PortOpen -Port $p)) `
            "something is already listening on :$p -- stop it, or pass different -GatewayPort/-UpstreamPort/-MiniredisPort values"
    }
}

function Get-TrackedProcess {
    <#
        The processes THIS script started, looked up by the pid recorded when it
        started them.

        Why not the process table: under this host's sandbox Get-CimInstance
        Win32_Process is denied outright (HRESULT 0x80041003) and returns an empty
        list. A leak check built on that passes for the wrong reason -- it reports
        "nothing survived" because it cannot see anything at all. Get-Process is
        permitted, and the pids this script holds identify its own children
        exactly, without claiming anything about the other gates that may be
        running in the same workspace.
    #>
    return @($script:startedPids | ForEach-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
}

function Get-ForeignFleetProcess {
    <#
        Fleet binaries that are running and that this script did NOT start.

        Diagnostics only, never an assertion. Another gate may legitimately be
        running beside this one -- a sibling measure-m3 run in this same workspace
        once killed this script's mock upstream mid-run -- so those processes are
        evidence to print, not a pass or a fail.
    #>
    $mine = @($script:startedPids)
    $found = @()
    foreach ($name in @('infergate', 'mockupstream', 'miniredis')) {
        foreach ($proc in @(Get-Process -Name $name -ErrorAction SilentlyContinue)) {
            if ($mine -notcontains $proc.Id) { $found += $proc }
        }
    }
    return $found
}

function Get-DeadFleetMember {
    <#
        Which fleet members stopped answering their health endpoint.

        Why: a process that dies mid-run (killed by name from outside this script)
        otherwise surfaces as a bewildering cascade -- one run produced 46 failing
        assertions, all downstream of a single 502 -- because every later request
        is proxied to a port nobody is listening on. One explicit "is the fleet
        still there?" line turns that into a fact the operator can act on.
    #>
    param([string[]]$Urls)
    $discard = Join-Path $tmpDir 'm3-health.discard'
    $dead = @()
    foreach ($u in $Urls) {
        $a = @('-s', '-o', $discard, '-w', '%{http_code}', $u)
        $code = (Invoke-Curl $a).Trim()
        if ($code -ne '200') { $dead += "$u -> $code" }
    }
    return $dead
}

function Start-MockUpstream {
    param([int]$Port, [string]$Name, [string]$Tag)
    $log = New-LogPath $Tag
    [System.IO.File]::WriteAllText($log, '', $utf8NoBom)
    $p = Start-Process -FilePath (Join-Path $binDir 'mockupstream.exe') `
        -ArgumentList @('-listen', ":$Port", '-name', $Name, '-token-delay', '1ms') `
        -RedirectStandardOutput $log -RedirectStandardError "$log.err" `
        -PassThru -WindowStyle Hidden
    $script:startedPids += $p.Id
    Write-Host "  mockupstream.exe -name $Name pid=$($p.Id) listening :$Port"
    return $p
}

function Start-Miniredis {
    param([int]$Port)
    $log = New-LogPath 'miniredis'
    [System.IO.File]::WriteAllText($log, '', $utf8NoBom)
    $p = Start-Process -FilePath (Join-Path $binDir 'miniredis.exe') `
        -ArgumentList @('-listen', ":$Port") `
        -RedirectStandardOutput $log -RedirectStandardError "$log.err" `
        -PassThru -WindowStyle Hidden
    $script:startedPids += $p.Id
    Write-Host "  miniredis.exe pid=$($p.Id) listening :$Port"
    return $p
}

function Start-Gateway {
    param([string]$Config, [string]$Tag)
    $log = New-LogPath $Tag
    [System.IO.File]::WriteAllText($log, '', $utf8NoBom)
    $p = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $Config) `
        -RedirectStandardOutput $log -RedirectStandardError "$log.err" `
        -PassThru -WindowStyle Hidden
    $script:startedPids += $p.Id
    Write-Host "  infergate.exe pid=$($p.Id) config=$Config"
    return $p
}

function Start-CaptureUpstream {
    <#
        A one-request-at-a-time HTTP listener that LOGS the request body it was
        sent and answers with a canned completion.

        Why this exists: the mock upstream logs the model it decoded but not the
        max_tokens, and the gateway never publishes the body it forwards. The only
        way to prove the degrade rewrite lowered the completion ceiling -- rather
        than merely claiming it in a header the gateway writes about itself -- is
        to stand at the far end of the wire and read the bytes that arrived.
    #>
    param([int]$Port, [string]$ScriptPath, [string]$LogPath)
    [System.IO.File]::WriteAllText($LogPath, '', $utf8NoBom)
    $p = Start-Process -FilePath $psExe `
        -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $ScriptPath, '-Port', $Port, '-LogPath', $LogPath) `
        -RedirectStandardOutput "$LogPath.out" -RedirectStandardError "$LogPath.err" `
        -PassThru -WindowStyle Hidden
    $script:startedPids += $p.Id
    Write-Host "  capture upstream pid=$($p.Id) listening :$Port"
    return $p
}

function Invoke-Redis {
    <#
        One raw RESP2 command over a socket, returning the reply as a string:
        the bulk string for a GET, the integer for TTL, '+PONG' for PING, '' for
        a nil reply.

        Written by hand because there is no redis-cli on this host and because
        "the counters are real Redis keys" is a claim about the WIRE, not about
        the admin endpoint echoing its own in-process state back. The keys and
        the values are all ASCII, so counting bytes and characters is the same
        thing here.
    #>
    param([int]$Port, [string[]]$Command, [int]$TimeoutMs = 3000)
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $client.BeginConnect('127.0.0.1', $Port, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne($TimeoutMs)) { throw "redis: connect to 127.0.0.1:$Port timed out" }
        $client.EndConnect($iar)
        $stream = $client.GetStream()
        $stream.ReadTimeout = $TimeoutMs
        $sb = New-Object System.Text.StringBuilder
        [void]$sb.Append('*' + $Command.Count + "`r`n")
        foreach ($arg in $Command) {
            $len = [System.Text.Encoding]::UTF8.GetByteCount($arg)
            [void]$sb.Append('$' + $len + "`r`n" + $arg + "`r`n")
        }
        $payload = [System.Text.Encoding]::UTF8.GetBytes($sb.ToString())
        $stream.Write($payload, 0, $payload.Length)
        $stream.Flush()
        $reader = New-Object System.IO.StreamReader($stream, [System.Text.Encoding]::UTF8)
        $line = $reader.ReadLine()
        if ($null -eq $line) { return '' }
        if ($line.StartsWith('+')) { return $line.Substring(1) }
        if ($line.StartsWith(':')) { return $line.Substring(1) }
        if ($line.StartsWith('-')) { return $line }
        if ($line.StartsWith('$')) {
            $len = [int]$line.Substring(1)
            if ($len -lt 0) { return '' }
            $buf = New-Object char[] $len
            $read = 0
            while ($read -lt $len) {
                $n = $reader.Read($buf, $read, $len - $read)
                if ($n -le 0) { break }
                $read += $n
            }
            return [string]::new($buf, 0, $read)
        }
        return $line
    }
    finally { $client.Close() }
}

function Get-DayBucket { return [datetime]::UtcNow.ToString('yyyyMMdd', [System.Globalization.CultureInfo]::InvariantCulture) }
function Get-MinuteBucket { return [datetime]::UtcNow.ToString('yyyyMMddHHmm', [System.Globalization.CultureInfo]::InvariantCulture) }

function Get-QuotaKey {
    <#
        The key layout the manager writes: <prefix>:<tenant>:<window>:<bucket>:<counter>.
        Built by hand here rather than scraped from the store, so a silently
        renamed key shows up as "the value is not there" instead of passing.
    #>
    param([string]$Tenant, [string]$Window, [string]$Bucket, [string]$Counter)
    return "ig:quota:${Tenant}:${Window}:${Bucket}:${Counter}"
}

# The Go toolchain is vendored under .gotoolchain; never let the shim try to
# download another one mid-run.
$env:GOTOOLCHAIN = 'local'

$mockProc = $null
$mockRedisProc = $null
$captureProc = $null
$gwProc = $null
$redisGwProc = $null
$captureGwProc = $null
$miniProc = $null

# The capture upstream is a PowerShell listener: it is written into tmp\ by this
# script, so the deliverable stays a single file.
$captureScript = Join-Path $tmpDir 'm3-capture-upstream.ps1'
$captureLog = New-LogPath 'capture'

# ---------------------------------------------------------------------------
# 0. Preconditions
# ---------------------------------------------------------------------------
Write-Section '0. Preconditions'

if (-not (Get-Command curl.exe -ErrorAction SilentlyContinue)) {
    throw 'curl.exe not found on PATH. Windows 10 1803+ ships it; otherwise install curl.'
}
Write-Host "  curl    : $((& curl.exe --version | Select-Object -First 1))"

Assert-True "the go shim exists at tools\go.cmd" (Test-Path $goShim) $goShim
Write-Host "  go shim : $goShim"
Assert-True 'Windows PowerShell 5.1 is where the capture upstream will run' (Test-Path $psExe) $psExe

New-Item -ItemType Directory -Force -Path $binDir, $tmpDir | Out-Null

$ports = @($GatewayPort, $RedisGatewayPort, $CaptureGatewayPort, $UpstreamPort, $RedisUpstreamPort, $CaptureUpstreamPort, $MiniredisPort)
Assert-PortsFree -Ports $ports

# Deliberately NOT asserted: whether a previous run left a process behind. That
# question needs a process enumeration this sandbox denies (see Get-TrackedProcess),
# and answering it by image name would blame a concurrently running gate. The port
# check above already covers the case that matters -- a leftover holding one of this
# run's ports -- and anything else is printed as a note instead of scored.
$foreign0 = @(Get-ForeignFleetProcess)
if ($foreign0.Count -gt 0) {
    Write-Host ("  note: $($foreign0.Count) infergate/mockupstream/miniredis process(es) are already running, not started by this script: " `
        + (($foreign0 | ForEach-Object { "$($_.ProcessName) pid=$($_.Id)" }) -join '; ')) -ForegroundColor DarkYellow
}

try {
    # -----------------------------------------------------------------------
    # 1. Build
    # -----------------------------------------------------------------------
    Write-Section '1. Build and configuration'

    function Invoke-GoBuild {
        <#
            go build with up to three attempts.

            Another gate may be running at the same time, and on Windows the
            linker cannot replace an .exe that a previous run is still holding
            open. A retry is not papering over a compile error: the compiler's own
            stderr is printed on every failed attempt, and three failures still
            fail the section.
        #>
        param([string]$Out, [string]$Pkg)
        $errFile = Join-Path $tmpDir 'm3-go-build.err'
        for ($attempt = 1; $attempt -le 3; $attempt++) {
            [System.IO.File]::WriteAllText($errFile, '', $utf8NoBom)
            # 2> to a FILE, never inherited: PS 5.1 raises a terminating
            # NativeCommandError when a native command writes to an unredirected
            # stderr, which would abort the script before the retry could happen.
            & $goShim build -o $Out $Pkg 2>$errFile
            $rc = $LASTEXITCODE
            if ($rc -eq 0) { return @{ Ok = $true; Code = 0; Attempts = $attempt } }
            $text = (Read-Text $errFile).Trim()
            Write-Host "  attempt $attempt of go build $Pkg failed (rc=$rc): $text" -ForegroundColor DarkYellow
            Start-Sleep -Milliseconds 400
        }
        return @{ Ok = $false; Code = $rc; Attempts = 3 }
    }

    # The binaries are built rather than assumed: a green script that ran against
    # a stale bin\ proves nothing about the tree under test.
    $bGw = Invoke-GoBuild -Out (Join-Path $binDir 'infergate.exe') -Pkg './cmd/infergate'
    Assert-True "go build ./cmd/infergate succeeded (attempts=$($bGw.Attempts))" $bGw.Ok "rc=$($bGw.Code)"
    Assert-True 'bin\infergate.exe exists' (Test-Path (Join-Path $binDir 'infergate.exe'))

    $bMock = Invoke-GoBuild -Out (Join-Path $binDir 'mockupstream.exe') -Pkg './cmd/mockupstream'
    Assert-True "go build ./cmd/mockupstream succeeded (attempts=$($bMock.Attempts))" $bMock.Ok "rc=$($bMock.Code)"
    Assert-True 'bin\mockupstream.exe exists' (Test-Path (Join-Path $binDir 'mockupstream.exe'))

    # miniredis is the in-repo RESP2 server: the Redis path is verified against a
    # real wire-protocol implementation, not a stub.
    $bMini = Invoke-GoBuild -Out (Join-Path $binDir 'miniredis.exe') -Pkg './cmd/miniredis'
    Assert-True "go build ./cmd/miniredis succeeded (attempts=$($bMini.Attempts))" $bMini.Ok "rc=$($bMini.Code)"
    Assert-True 'bin\miniredis.exe exists' (Test-Path (Join-Path $binDir 'miniredis.exe'))

    # -----------------------------------------------------------------------
    # 1b. Materialise the configs
    # -----------------------------------------------------------------------
    # The sample configs are copied by literal substitution rather than used in
    # place: the ports must not collide with a stale process, and the sample
    # budgets are sized for a demo (acme is 2000 tokens/day) rather than for a
    # script that has to cross them in a dozen requests.
    #
    # Every budget that is rewritten here is asserted back out of the RUNNING
    # gateway in section 2, so a substitution that silently failed to apply would
    # fail the run instead of quietly testing the sample values.
    $localCfgPath = Join-Path $tmpDir 'm3-quota-local.yaml'
    $captureCfgPath = Join-Path $tmpDir 'm3-quota-capture.yaml'
    $redisCfgPath = Join-Path $tmpDir 'm3-quota-redis.yaml'

    $costerBlock = @(
        '    - tenant: "coster"',
        '      tokens_per_day: 5000000',
        '      cost_per_day_usd: 0.00006',
        '      requests_per_minute: 600',
        '      on_exceed: reject'
    ) -join "`n"

    $localBody = (Read-Text (Join-Path $repo 'configs\quota-local.yaml')) `
        -replace '(?m)^(\s*listen:\s*)":8084"', "`$1`":$GatewayPort`"" `
        -replace 'http://127\.0\.0\.1:9300', "http://127.0.0.1:$UpstreamPort" `
        -replace 'tokens_per_day: 20000', 'tokens_per_day: 50' `
        -replace 'tokens_per_day: 2000\b', 'tokens_per_day: 500' `
        -replace 'requests_per_minute: 5\b', 'requests_per_minute: 2' `
        -replace '(?m)^(\s*- tenant: "growth")', ($costerBlock + "`n" + '$1')
    [System.IO.File]::WriteAllText($localCfgPath, $localBody, $utf8NoBom)

    $captureBody = $localBody `
        -replace "(?m)^(\s*listen:\s*)`":$GatewayPort`"", "`$1`":$CaptureGatewayPort`"" `
        -replace "http://127\.0\.0\.1:$UpstreamPort", "http://127.0.0.1:$CaptureUpstreamPort"
    [System.IO.File]::WriteAllText($captureCfgPath, $captureBody, $utf8NoBom)

    $redisBody = (Read-Text (Join-Path $repo 'configs\quota-redis.yaml')) `
        -replace '(?m)^(\s*listen:\s*)":8085"', "`$1`":$RedisGatewayPort`"" `
        -replace 'http://127\.0\.0\.1:9301', "http://127.0.0.1:$RedisUpstreamPort" `
        -replace 'addr:\s*"127\.0\.0\.1:6398"', "addr: `"127.0.0.1:$MiniredisPort`"" `
        -replace '(?m)^(\s*)tokens_per_day: 300(?=\r?\n\s*requests_per_minute:)', "`${1}tokens_per_day: 50000" `
        -replace '(?m)^(\s*)requests_per_minute: 60', "`$1cost_per_day_usd: 0.05`n`$1tokens_per_session: 4000`n`$1requests_per_minute: 60"
    [System.IO.File]::WriteAllText($redisCfgPath, $redisBody, $utf8NoBom)

    Assert-Contains 'the generated memory config listens on this run''s gateway port' $localBody "listen: `":$GatewayPort`""
    Assert-Contains 'the generated memory config points at this run''s upstream' $localBody "http://127.0.0.1:$UpstreamPort"
    Assert-Contains 'the generated memory config selects the memory store' $localBody 'store: "memory"'
    Assert-Contains 'acme''s daily token budget is tightened for this run' $localBody 'tokens_per_day: 500'
    Assert-Contains 'bursty''s per-minute limit is tightened to 2' $localBody 'requests_per_minute: 2'
    Assert-Contains 'growth''s daily token budget is tightened to 50' $localBody 'tokens_per_day: 50'
    Assert-Contains 'a tenant whose daily SPEND budget is tiny was added' $localBody 'cost_per_day_usd: 0.00006'
    Assert-Contains 'growth still degrades rather than rejects' $localBody 'on_exceed: "degrade"'

    Assert-Contains 'the generated redis config listens on the redis gateway port' $redisBody "listen: `":$RedisGatewayPort`""
    Assert-Contains 'the generated redis config points at this run''s redis upstream' $redisBody "http://127.0.0.1:$RedisUpstreamPort"
    Assert-Contains 'the generated redis config points at this run''s miniredis' $redisBody "addr: `"127.0.0.1:$MiniredisPort`""
    Assert-Contains 'the generated redis config really selects the redis store' $redisBody 'store: "redis"'
    # The shipped redis tenants spend on tokens and requests only, and a counter
    # key is only created for a dimension the tenant is actually budgeted on
    # (internal/quota builds its check list from the non-zero limits). So the
    # redis acme tenant is given a spend budget and a session budget here, which
    # is what lets section 9 read those key shapes back out of a real Redis.
    Assert-Contains 'the redis acme budget is raised so a probe can run repeatedly' $redisBody 'tokens_per_day: 50000'
    Assert-Contains 'the redis acme tenant gets a spend budget' $redisBody 'cost_per_day_usd: 0.05'
    Assert-Contains 'the redis acme tenant gets a per-session budget' $redisBody 'tokens_per_session: 4000'

    Assert-Contains 'the capture config listens on its own port' $captureBody "listen: `":$CaptureGatewayPort`""
    Assert-Contains 'the capture config points at the capture listener' $captureBody "http://127.0.0.1:$CaptureUpstreamPort"

    # internal/config REFUSES an undefined ${VAR}, so an environment variable that
    # crept into a substitution would make the gateway unstartable. The sample
    # files mention ${REDIS_PASSWORD} in a comment; only live lines matter.
    foreach ($pair in @(@('memory', $localBody), @('redis', $redisBody), @('capture', $captureBody))) {
        $live = @($pair[1] -split "`n" | Where-Object { $_ -match '\$\{' -and $_ -notmatch '^\s*#' })
        Assert-Equal "the generated $($pair[0]) config introduces no environment variables" 0 $live.Count ($live -join ' | ')
    }

    function Test-ConfigCheck {
        <# `infergate -config <file> -check` validates and exits without serving. #>
        param([string]$Config)
        $out = (& (Join-Path $binDir 'infergate.exe') -config $Config -check 2>$null | Out-String)
        return @{ Code = $LASTEXITCODE; Out = $out }
    }

    $chkLocal = Test-ConfigCheck -Config $localCfgPath
    Assert-Equal 'infergate -check accepts the generated memory config' 0 $chkLocal.Code
    Assert-Contains 'the memory config check names the file it validated' $chkLocal.Out 'configuration OK'
    $chkRedis = Test-ConfigCheck -Config $redisCfgPath
    Assert-Equal 'infergate -check accepts the generated redis config' 0 $chkRedis.Code
    $chkCapture = Test-ConfigCheck -Config $captureCfgPath
    Assert-Equal 'infergate -check accepts the generated capture config' 0 $chkCapture.Code

    # -----------------------------------------------------------------------
    # 2. Fleet up
    # -----------------------------------------------------------------------
    Write-Section '2. Fleet up: two gateways, three upstreams, one Redis'

    $mockProc = Start-MockUpstream -Port $UpstreamPort -Name 'local' -Tag 'mock-local'
    Assert-Equal 'the local mock upstream answers /healthz' '200' (Wait-Healthy -Port $UpstreamPort)
    $mockRedisProc = Start-MockUpstream -Port $RedisUpstreamPort -Name 'local' -Tag 'mock-redis'
    Assert-Equal 'the redis mock upstream answers /healthz' '200' (Wait-Healthy -Port $RedisUpstreamPort)

    # The Redis protocol server must be up BEFORE the redis gateway: a store that
    # cannot be dialled at startup is a fatal configuration error, which is the
    # point of failing closed rather than serving unmetered.
    $miniProc = Start-Miniredis -Port $MiniredisPort
    Assert-True 'miniredis accepts connections' (Wait-PortOpen -Port $MiniredisPort)
    Assert-Equal 'miniredis answers PING on the wire' 'PONG' (Invoke-Redis -Port $MiniredisPort -Command @('PING'))

    $captureSource = @'
# Written by scripts\verify-m3.ps1. A minimal HTTP listener that records the body
# of every request it is sent and answers with a canned completion, so the request
# the gateway actually forwarded can be read back byte-for-byte.
param(
    [int]$Port,
    [string]$LogPath
)
$ErrorActionPreference = 'Continue'
$listener = New-Object System.Net.Sockets.TcpListener -ArgumentList @([System.Net.IPAddress]::Loopback, $Port)
$listener.Start()
while ($true) {
    $client = $listener.AcceptTcpClient()
    try {
        $stream = $client.GetStream()
        $buffer = New-Object byte[] 4096
        $text = ''
        $headEnd = -1
        while ($headEnd -lt 0) {
            $n = $stream.Read($buffer, 0, $buffer.Length)
            if ($n -le 0) { break }
            $text += [System.Text.Encoding]::ASCII.GetString($buffer, 0, $n)
            $headEnd = $text.IndexOf("`r`n`r`n")
        }
        if ($headEnd -lt 0) { continue }
        $head = $text.Substring(0, $headEnd)
        $body = $text.Substring($headEnd + 4)
        $length = 0
        foreach ($line in ($head -split "`r`n")) {
            if ($line -match '^(?i)content-length:\s*(\d+)') { $length = [int]$Matches[1] }
        }
        while ($body.Length -lt $length) {
            $n = $stream.Read($buffer, 0, $buffer.Length)
            if ($n -le 0) { break }
            $body += [System.Text.Encoding]::ASCII.GetString($buffer, 0, $n)
        }
        if ($body.Length -gt $length) { $body = $body.Substring(0, $length) }
        Add-Content -Path $LogPath -Value ('CAPTURED ' + $body) -Encoding ASCII
        $json = '{"id":"chatcmpl-capture","object":"chat.completion","created":1,"model":"mock-gpt-mini","choices":[{"index":0,"message":{"role":"assistant","content":"captured answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}'
        $payload = [System.Text.Encoding]::ASCII.GetBytes($json)
        $header = "HTTP/1.1 200 OK`r`nContent-Type: application/json`r`nContent-Length: $($payload.Length)`r`nConnection: close`r`n`r`n"
        $hb = [System.Text.Encoding]::ASCII.GetBytes($header)
        $stream.Write($hb, 0, $hb.Length)
        $stream.Write($payload, 0, $payload.Length)
        $stream.Flush()
    }
    catch {
        Add-Content -Path $LogPath -Value ('ERROR ' + $_.Exception.Message) -Encoding ASCII
    }
    finally {
        $client.Close()
    }
}
'@
    [System.IO.File]::WriteAllText($captureScript, $captureSource, $utf8NoBom)
    $captureProc = Start-CaptureUpstream -Port $CaptureUpstreamPort -ScriptPath $captureScript -LogPath $captureLog
    Assert-True 'the capture upstream listens on its port' (Wait-PortOpen -Port $CaptureUpstreamPort)

    $gwProc = Start-Gateway -Config $localCfgPath -Tag 'gateway'
    $gwLog = New-LogPath 'gateway'
    $base = "http://127.0.0.1:$GatewayPort"
    Assert-Equal 'the memory gateway answers /healthz' '200' (Wait-Healthy -Port $GatewayPort -Path '/healthz')
    Assert-Equal 'the memory gateway answers /readyz' '200' (Wait-Healthy -Port $GatewayPort -Path '/readyz')

    $redisGwProc = Start-Gateway -Config $redisCfgPath -Tag 'redis-gateway'
    $redisGwLog = New-LogPath 'redis-gateway'
    $redisBase = "http://127.0.0.1:$RedisGatewayPort"
    Assert-Equal 'the redis gateway answers /healthz' '200' (Wait-Healthy -Port $RedisGatewayPort -Path '/healthz')
    Assert-Equal 'the redis gateway answers /readyz' '200' (Wait-Healthy -Port $RedisGatewayPort -Path '/readyz')

    $captureGwProc = Start-Gateway -Config $captureCfgPath -Tag 'capture-gateway'
    $captureBase = "http://127.0.0.1:$CaptureGatewayPort"
    Assert-Equal 'the capture gateway answers /readyz' '200' (Wait-Healthy -Port $CaptureGatewayPort -Path '/readyz')

    # -----------------------------------------------------------------------
    # 2b. The admin surface describes the policy that is actually running
    # -----------------------------------------------------------------------
    $admin = Get-QuotaAdmin -Base $base
    Assert-True 'GET /admin/quota returns parseable JSON' ($null -ne $admin)
    Assert-Equal 'quota enforcement is enabled on the memory gateway' $true $admin.enabled
    Assert-Equal 'the memory gateway uses the memory store' 'memory' $admin.store
    Assert-Equal 'the memory gateway fails closed' $false $admin.fail_open
    Assert-Equal 'the estimator is the configured chars-per-token' 4 ([int]$admin.config.estimate_chars_per_token)
    Assert-Equal 'the default completion estimate is the configured 256' 256 ([int]$admin.config.estimate_completion_tokens)

    $memTenants = @($admin.config.tenants | ForEach-Object { $_.tenant })
    foreach ($name in @('acme', 'agent', 'bursty', 'coster', 'growth')) {
        Assert-True "the memory config lists tenant '$name'" ($memTenants -contains $name) "tenants: $($memTenants -join ', ')"
    }
    $acmePolicy = $admin.config.tenants | Where-Object { $_.tenant -eq 'acme' }
    Assert-Equal 'the RUNNING acme policy carries this run''s tightened token budget' 500 ([int]$acmePolicy.tokens_per_day)
    $burstyPolicy = $admin.config.tenants | Where-Object { $_.tenant -eq 'bursty' }
    Assert-Equal 'the RUNNING bursty policy carries this run''s per-minute limit' 2 ([int]$burstyPolicy.requests_per_minute)
    $growthPolicy = $admin.config.tenants | Where-Object { $_.tenant -eq 'growth' }
    Assert-Equal 'the RUNNING growth policy carries this run''s tightened token budget' 50 ([int]$growthPolicy.tokens_per_day)
    Assert-Equal 'growth degrades instead of rejecting' 'degrade' $growthPolicy.on_exceed
    Assert-Equal 'growth''s downgrade model is the cheap one' 'mock-gpt-mini' $growthPolicy.downgrade_model
    Assert-Equal 'growth''s completion ceiling is the configured cap' 128 ([int]$growthPolicy.max_tokens_cap)

    # Named $redisAdmin, not $acme: 'acme' is also a TENANT in these configs and
    # a variable that shadows it makes every later assertion ambiguous.
    $redisAdmin = Get-QuotaAdmin -Base $redisBase
    Assert-True 'GET /admin/quota returns parseable JSON on the redis gateway' ($null -ne $redisAdmin)
    Assert-Equal 'quota enforcement is enabled on the redis gateway' $true $redisAdmin.enabled
    Assert-Equal 'the redis gateway uses the redis store' 'redis' $redisAdmin.store
    $redisTenants = @($redisAdmin.config.tenants | ForEach-Object { $_.tenant })
    Assert-True 'the redis config lists tenant ''acme''' ($redisTenants -contains 'acme') "tenants: $($redisTenants -join ', ')"
    Assert-True 'the redis config lists tenant ''growth''' ($redisTenants -contains 'growth') "tenants: $($redisTenants -join ', ')"
    Assert-Equal 'the redis gateway advertises no quota decisions yet' 0 ([double]$redisAdmin.stats.allowed)

    foreach ($field in @('allowed', 'degraded', 'rejected', 'store_errors', 'alerts', 'reserved_tokens', 'settled_tokens', 'released_tokens', 'overshoot_tokens', 'overshoot_cost_micros', 'released_cost_micros')) {
        Assert-Equal "a fresh memory gateway reports stats.$field = 0" 0 ([double]$admin.stats.$field)
    }

    # -----------------------------------------------------------------------
    # 3. ALLOW: a funded tenant is admitted, and the provider really was called
    # -----------------------------------------------------------------------
    Write-Section '3. ALLOW: a funded tenant is admitted and metered'

    # Guard before the first traffic section. A fleet member can be killed from
    # outside this script (a sibling run in this workspace did exactly that), and
    # without this line the only symptom is a 502 on every request for the next
    # five sections -- 46 failing assertions from one external event.
    $fleetUrls = @(
        "http://127.0.0.1:$UpstreamPort/healthz",
        "http://127.0.0.1:$RedisUpstreamPort/healthz",
        "$base/healthz",
        "$redisBase/healthz",
        "$captureBase/readyz")
    $deadFleet = @(Get-DeadFleetMember -Urls $fleetUrls)
    $captureUp = Test-PortOpen -Port $CaptureUpstreamPort
    Assert-True 'every fleet member is still up before the traffic sections' `
        (($deadFleet.Count -eq 0) -and $captureUp) `
        (($deadFleet -join '; ') + " capture-listener=$captureUp")

    $before3 = Get-LocalRequestCount $base
    # The allow path is exercised as tenant 'acme' rather than 'agent' because
    # /admin/quota only reports a spend counter for a tenant that HAS a spend
    # budget (internal/quota reserves a dimension only when its limit is non-zero),
    # and acme is the well-funded tenant that carries both budgets here.
    $r1 = Send-ChatAs -Base $base -Tenant 'acme' -Prompt 'allow path probe one' -Tag 'm3-allow-1'
    Assert-Equal 'a funded tenant is admitted' 200 $r1.Status
    Assert-Equal 'the admission is reported as allow' 'allow' $r1.Quota
    Assert-Equal 'the allow carries the within-budget reason' 'within-budget' $r1.Reason
    # No limit is exceeded on an allow, so the limit/used headers must be absent:
    # a gateway that always sends a limit would make "which budget stopped me?"
    # unanswerable on the paths that never stopped anyone.
    Assert-Equal 'an allow carries no quota limit header' '' $r1.Limit
    Assert-Equal 'an allow carries no quota used header' '' $r1.Used
    Assert-Equal 'the allow was served by the configured upstream' 'local' $r1.UpName
    Assert-Contains 'the provider answered with a real completion' $r1.Body '"object":"chat.completion"'
    $r1Json = Get-Json $r1.Body
    Assert-Number 'the provider reported prompt tokens' ([string]$r1Json.usage.prompt_tokens) -Min 1

    $after3 = Get-LocalRequestCount $base
    Assert-Equal 'exactly one request reached the provider' ($before3 + 1) $after3

    $rep1 = (Get-QuotaReport -Base $base -Tenant 'acme').report
    Assert-True 'the tenant report answers for a tenant with no session' ($null -ne $rep1)
    Assert-True 'the report counts the tokens the request really cost' ([double]$rep1.tokens_today -gt 0) "tokens_today=$($rep1.tokens_today)"
    Assert-True 'the report prices the request' ([double]$rep1.cost_today_micros -gt 0) "cost_today_micros=$($rep1.cost_today_micros)"
    Assert-Equal 'no session was named, so no session is reported' 0 ([double]$rep1.session_tokens)

    # The gateway stamps a report with the UTC bucket it read the clock at, and
    # this compares that stamp against the clock after a report round trip. Read
    # the clock on both sides of the window and accept either bucket below: what
    # is asserted is that the report names a bucket this probe actually crossed,
    # not that the calendar stood still for the whole round trip.
    $dayBefore = Get-DayBucket
    $minuteBefore = Get-MinuteBucket
    $r2 = Send-ChatAs -Base $base -Tenant 'acme' -Prompt 'allow path probe two' -Tag 'm3-allow-2'
    Assert-Equal 'a second funded request is admitted' 200 $r2.Status
    $rep2 = (Get-QuotaReport -Base $base -Tenant 'acme').report
    $dayAfter = Get-DayBucket
    $minuteAfter = Get-MinuteBucket
    Assert-True 'tokens_today rises between requests' ([double]$rep2.tokens_today -gt [double]$rep1.tokens_today) `
        "before=$($rep1.tokens_today) after=$($rep2.tokens_today)"
    Assert-True 'cost_today_micros rises between requests' ([double]$rep2.cost_today_micros -gt [double]$rep1.cost_today_micros) `
        "before=$($rep1.cost_today_micros) after=$($rep2.cost_today_micros)"
    # Not "== 2": both requests normally land in one UTC minute, but a run that
    # straddles a minute boundary would see 1 and the assertion would be about
    # the clock rather than about the gateway.
    Assert-True 'the minute window counted this run''s traffic' ([double]$rep2.requests_this_minute -ge 1) `
        "requests_this_minute=$($rep2.requests_this_minute)"
    Assert-True 'the reported day is a UTC day this probe crossed' `
        (@($dayBefore, $dayAfter) -contains [string]$rep2.day) `
        "reported=$($rep2.day) before=$dayBefore after=$dayAfter"
    Assert-True 'the reported minute is a UTC minute this probe crossed' `
        (@($minuteBefore, $minuteAfter) -contains [string]$rep2.minute) `
        "reported=$($rep2.minute) before=$minuteBefore after=$minuteAfter"

    # The mock's own log is the witness that a provider process was really
    # called: the gateway cannot write into it.
    $mockLog = Read-OpenLog (New-LogPath 'mock-local')
    Assert-Contains 'the mock upstream logged a chat request' $mockLog 'msg="chat request"'
    Assert-Contains 'the mock upstream logged the model the gateway routed' $mockLog 'model=mock-gpt'

    # -----------------------------------------------------------------------
    # 4. DAILY TOKEN BUDGET
    # -----------------------------------------------------------------------
    Write-Section '4. DAILY TOKEN BUDGET: admitted up to the budget, then 429'

    # acme's daily budget is 500 tokens in the generated config; the mock charges
    # ~15 real tokens per request, and a reservation is SETTLED down to the real
    # usage, so the crossing takes a dozen requests. The loop is bounded and
    # asserts on where it stopped rather than on a fixed count, because the
    # provider's tokenizer -- not this script -- decides the exact crossing point.
    $admitted = 0
    $refusal = $null
    for ($i = 1; $i -le 40; $i++) {
        $r = Send-ChatAs -Base $base -Tenant 'acme' -Prompt "acme budget probe number $i" -Tag "m3-acme-$i"
        if ($r.Status -eq 200) { $admitted++; continue }
        $refusal = $r
        break
    }
    Assert-True 'the budget admitted several requests before crossing' ($admitted -ge 5) "admitted=$admitted"
    Assert-True 'the tenant was eventually refused' ($null -ne $refusal) 'no refusal inside 40 requests'
    if ($null -ne $refusal) {
        Assert-Equal 'crossing the daily token budget answers 429' 429 $refusal.Status
        Assert-Equal 'the refusal is reported as reject' 'reject' $refusal.Quota
        Assert-Equal 'the refusal names the daily token budget' 'tokens_per_day' $refusal.Reason
        Assert-Equal 'the refusal reports the configured limit' '500' $refusal.Limit
        Assert-Number 'the refusal reports the total before this request' $refusal.Used -Min 0
        Assert-Number 'the refusal carries a Retry-After of at least one second' $refusal.Retry -Min 1
        $errBody = Get-Json $refusal.Body
        Assert-True 'the refusal body is JSON' ($null -ne $errBody)
        Assert-Equal 'the refusal body type is infergate_quota_exceeded' 'infergate_quota_exceeded' $errBody.error.type
        Assert-Contains 'the refusal body names the daily token budget' $refusal.Body 'daily token budget of 500'
        # The needle keeps the backslashes: the message is JSON-encoded on the wire,
        # so the tenant name really is surrounded by \" in the body bytes.
        Assert-Contains 'the refusal body names the tenant' $refusal.Body 'tenant \"acme\"'
    }

    # The bug this section exists for: a refusal that still costs a provider call.
    $before4 = Get-LocalRequestCount $base
    $refusal2 = Send-ChatAs -Base $base -Tenant 'acme' -Prompt 'acme budget probe again' -Tag 'm3-acme-again'
    Assert-Equal 'a still-exhausted tenant is refused again' 429 $refusal2.Status
    $after4 = Get-LocalRequestCount $base
    Assert-Equal 'a refused request never reaches the provider' $before4 $after4 `
        "upstream=local before=$before4 after=$after4"

    # A refusal must also not leave a reservation behind: the day counter is the
    # same before and after, which is only true if every reserved dimension was
    # rolled back.
    $repBefore = (Get-QuotaReport -Base $base -Tenant 'acme').report
    $null = Send-ChatAs -Base $base -Tenant 'acme' -Prompt 'acme budget probe third' -Tag 'm3-acme-third'
    $repAfter = (Get-QuotaReport -Base $base -Tenant 'acme').report
    Assert-Equal 'a refusal leaves the tenant''s day counter untouched' ([double]$repBefore.tokens_today) ([double]$repAfter.tokens_today)
    Assert-True 'the tenant''s day counter is at the budget, not past it' ([double]$repAfter.tokens_today -le 500) `
        "tokens_today=$($repAfter.tokens_today)"

    # -----------------------------------------------------------------------
    # 5. PER-MINUTE LIMIT
    # -----------------------------------------------------------------------
    Write-Section '5. PER-MINUTE LIMIT: exactly two, then 429'

    # bursty has a per-minute limit of 2 in the generated config and no token
    # budget at all, so the only dimension that can stop it is the request rate.
    $b1 = Send-ChatAs -Base $base -Tenant 'bursty' -Prompt 'bursty probe one' -Tag 'm3-bursty-1'
    Assert-Equal 'the first request of the minute is admitted' 200 $b1.Status
    Assert-Equal 'the first request is an allow' 'allow' $b1.Quota
    $b2 = Send-ChatAs -Base $base -Tenant 'bursty' -Prompt 'bursty probe two' -Tag 'm3-bursty-2'
    Assert-Equal 'the second request of the minute is admitted' 200 $b2.Status
    Assert-Equal 'the second request is an allow' 'allow' $b2.Quota

    $before5 = Get-LocalRequestCount $base
    $b3 = Send-ChatAs -Base $base -Tenant 'bursty' -Prompt 'bursty probe three' -Tag 'm3-bursty-3'
    Assert-Equal 'the third request in the minute is refused' 429 $b3.Status
    Assert-Equal 'the rate refusal is reported as reject' 'reject' $b3.Quota
    Assert-Equal 'the rate refusal names requests_per_minute' 'requests_per_minute' $b3.Reason
    Assert-Equal 'the rate refusal reports the configured limit of 2' '2' $b3.Limit
    Assert-Equal 'the rate refusal reports the two requests already used' '2' $b3.Used
    Assert-Number 'the rate refusal carries a Retry-After inside the minute' $b3.Retry -Min 1 -Max 60
    Assert-Equal 'the rate refusal body type is infergate_quota_exceeded' 'infergate_quota_exceeded' (Get-Json $b3.Body).error.type
    Assert-Contains 'the rate refusal body names the per-minute request budget' $b3.Body 'per-minute request budget of 2'

    $b4 = Send-ChatAs -Base $base -Tenant 'bursty' -Prompt 'bursty probe four' -Tag 'm3-bursty-4'
    Assert-Equal 'a fourth request in the same minute is refused too' 429 $b4.Status
    Assert-Equal 'the fourth refusal names the same reason' 'requests_per_minute' $b4.Reason
    $after5 = Get-LocalRequestCount $base
    Assert-Equal 'neither refused rate request reached the provider' $before5 $after5 `
        "upstream=local before=$before5 after=$after5"

    # -----------------------------------------------------------------------
    # 6. SESSION BUDGET
    # -----------------------------------------------------------------------
    Write-Section '6. SESSION BUDGET: two sessions of one tenant are independent'

    # The prompt is long on purpose: each request must cost enough real tokens
    # that agent's 4000-token session budget is reached inside a bounded loop.
    $filler = (1..120 | ForEach-Object { "filler$_" }) -join ' '
    $sessionA = 'm3-session-alpha'
    $sessionB = 'm3-session-beta'
    $okA = 0
    $refusalA = $null
    for ($i = 1; $i -le 30; $i++) {
        $r = Send-ChatAs -Base $base -Tenant 'agent' -Prompt $filler -Session $sessionA -Tag "m3-session-a-$i"
        if ($r.Status -eq 200) { $okA++; continue }
        $refusalA = $r
        break
    }
    Assert-True 'session alpha was admitted several times' ($okA -ge 5) "admitted=$okA"
    Assert-True 'session alpha eventually hit its per-session budget' ($null -ne $refusalA) 'no refusal inside 30 requests'
    if ($null -ne $refusalA) {
        Assert-Equal 'exhausting a session answers 429' 429 $refusalA.Status
        Assert-Equal 'the session refusal is reported as reject' 'reject' $refusalA.Quota
        Assert-Equal 'the session refusal names tokens_per_session' 'tokens_per_session' $refusalA.Reason
        Assert-Equal 'the session refusal reports the configured limit' '4000' $refusalA.Limit
        Assert-Contains 'the session refusal body names the per-session budget' $refusalA.Body 'per-session token budget of 4000'
    }

    # The other session must be unaffected: a session id that leaked into the
    # tenant-wide budget would make one user's long conversation throttle another.
    $okB = Send-ChatAs -Base $base -Tenant 'agent' -Prompt $filler -Session $sessionB -Tag 'm3-session-b-1'
    Assert-Equal 'a different session still gets through' 200 $okB.Status
    Assert-Equal 'the different session is an allow' 'allow' $okB.Quota

    $repA = (Get-QuotaReport -Base $base -Tenant 'agent' -Session $sessionA).report
    $repB = (Get-QuotaReport -Base $base -Tenant 'agent' -Session $sessionB).report
    Assert-Equal 'the report echoes the session it was asked about' $sessionA ([string]$repA.session)
    Assert-True 'session alpha is charged for its own tokens' ([double]$repA.session_tokens -gt 0) "session_tokens=$($repA.session_tokens)"
    Assert-True 'session alpha stayed under its own limit' ([double]$repA.session_tokens -le 4000) "session_tokens=$($repA.session_tokens)"
    Assert-True 'session beta was charged separately' ([double]$repB.session_tokens -lt [double]$repA.session_tokens) `
        "alpha=$($repA.session_tokens) beta=$($repB.session_tokens)"

    # -----------------------------------------------------------------------
    # 7. COST BUDGET
    # -----------------------------------------------------------------------
    Write-Section '7. COST BUDGET: a spend ceiling refuses on the spend reason'

    # coster's daily budget is 60 micro-dollars. A request with max_tokens 1
    # reserves a few micros and is admitted; the next one asks for 1000
    # completion tokens, which reserves ~3000 micros and cannot fit -- so the
    # refusal is about SPEND, not about tokens, even though the tokens differ.
    $c1 = Send-ChatAs -Base $base -Tenant 'coster' -Prompt 'cost probe one' -Tag 'm3-cost-1' -MaxTokens 1
    Assert-Equal 'a request that fits the spend budget is admitted' 200 $c1.Status
    Assert-Equal 'the cheap request is an allow' 'allow' $c1.Quota

    $before7 = Get-LocalRequestCount $base
    $c2 = Send-ChatAs -Base $base -Tenant 'coster' -Prompt 'cost probe two' -Tag 'm3-cost-2' -MaxTokens 1000
    Assert-Equal 'a request that cannot fit the spend budget is refused' 429 $c2.Status
    Assert-Equal 'the spend refusal is reported as reject' 'reject' $c2.Quota
    Assert-Equal 'the spend refusal names cost_per_day_usd' 'cost_per_day_usd' $c2.Reason
    Assert-Equal 'the spend refusal reports the limit in micro-dollars' '60' $c2.Limit
    Assert-Contains 'the spend refusal body names the daily spend budget' $c2.Body 'daily spend budget of 60'
    Assert-Equal 'the spend refusal uses the quota error type' 'infergate_quota_exceeded' (Get-Json $c2.Body).error.type
    $after7 = Get-LocalRequestCount $base
    Assert-Equal 'the spend refusal never reached the provider' $before7 $after7 `
        "upstream=local before=$before7 after=$after7"

    $repC = (Get-QuotaReport -Base $base -Tenant 'coster').report
    Assert-True 'the tenant is charged for the request it did make' ([double]$repC.cost_today_micros -gt 0) "cost_today_micros=$($repC.cost_today_micros)"
    Assert-True 'the reported spend is still inside the budget' ([double]$repC.cost_today_micros -le 60) "cost_today_micros=$($repC.cost_today_micros)"

    # -----------------------------------------------------------------------
    # 8. DEGRADE
    # -----------------------------------------------------------------------
    Write-Section '8. DEGRADE: over budget keeps serving, rewritten and capped'

    # growth's daily budget is 50 tokens, so its very first request already
    # breaches -- and because its policy is degrade with a downgrade model and a
    # cap, the request must still be SERVED. The interesting evidence is the body
    # the upstream received, which is why this section also runs against the
    # capture listener: the mock logs the model it decoded but not max_tokens, and
    # the gateway's own header is the gateway's own claim about itself.
    $g1 = Send-ChatAs -Base $base -Tenant 'growth' -Prompt 'degrade probe one' -Tag 'm3-degrade-1' -MaxTokens 1000
    Assert-Equal 'a degraded tenant is still served' 200 $g1.Status
    Assert-Equal 'the verdict is degrade, not reject' 'degrade' $g1.Quota
    Assert-Equal 'the degrade names the budget that was breached' 'tokens_per_day' $g1.Reason
    Assert-Equal 'the degrade reports the breached limit' '50' $g1.Limit
    Assert-Number 'the degrade reports the total before this request' $g1.Used -Min 0
    Assert-Equal 'the degrade names the downgrade model' 'mock-gpt-mini' $g1.Model
    Assert-Equal 'the degrade names the completion ceiling' '128' $g1.MaxTok
    # The upstream echoes the model it decoded, so the response proves the
    # rewrite happened on the wire and not only in the header.
    Assert-Contains 'the provider answer is attributed to the downgraded model' $g1.Body '"model":"mock-gpt-mini"'

    $g2 = Send-ChatAs -Base $base -Tenant 'growth' -Prompt 'degrade probe two' -Tag 'm3-degrade-2' -MaxTokens 1000
    Assert-Equal 'the budget stays gone and the tenant is still served' 200 $g2.Status
    Assert-Equal 'the second over-budget request still degrades' 'degrade' $g2.Quota
    $g3 = Send-ChatAs -Base $base -Tenant 'growth' -Prompt 'degrade probe three' -Tag 'm3-degrade-3' -MaxTokens 64
    Assert-Equal 'a request that asks for less than the cap is still served' 200 $g3.Status
    Assert-Equal 'and is still degraded' 'degrade' $g3.Quota
    Assert-Equal 'the cap header is the configured ceiling' '128' $g3.MaxTok

    $mockLog = Read-OpenLog (New-LogPath 'mock-local')
    Assert-Contains 'the mock upstream was really called with the downgraded model' $mockLog 'model=mock-gpt-mini'

    # Now the far end of the wire, where the body the gateway forwarded is visible.
    $cg1 = Send-ChatAs -Base $captureBase -Tenant 'growth' -Prompt 'capture probe one' -Tag 'm3-capture-1' -MaxTokens 1000
    Assert-Equal 'the capture gateway serves the degraded request' 200 $cg1.Status
    Assert-Equal 'the capture gateway degrades' 'degrade' $cg1.Quota
    $cg2 = Send-ChatAs -Base $captureBase -Tenant 'growth' -Prompt 'capture probe two' -Tag 'm3-capture-2' -MaxTokens 64
    Assert-Equal 'the capture gateway serves the capped request' 200 $cg2.Status
    Assert-Equal 'the capture gateway degrades the capped request too' 'degrade' $cg2.Quota

    $captured = @(Read-OpenLog $captureLog) -split "`n" | Where-Object { $_.StartsWith('CAPTURED ') } | ForEach-Object { $_.Substring(9) }
    $captured = @($captured)
    Assert-Equal 'the capture upstream recorded one body per forwarded request' 2 $captured.Count
    if ($captured.Count -ge 2) {
        $body1 = $captured[0]
        $body2 = $captured[1]
        Assert-True 'the forwarded body was rewritten to the downgrade model' `
            ($body1 -match '"model"\s*:\s*"mock-gpt-mini"') "body: $body1"
        Assert-True 'the forwarded body had its completion ceiling lowered to the cap' `
            ($body1 -match '"max_tokens"\s*:\s*128\b') "body: $body1"
        Assert-True 'the client''s 1000-token ceiling did not survive the rewrite' `
            (-not ($body1 -match '"max_tokens"\s*:\s*1000\b')) "body: $body1"
        Assert-True 'a request already under the cap keeps its own ceiling' `
            ($body2 -match '"max_tokens"\s*:\s*64\b') "body: $body2"
        Assert-True 'the under-cap request is still sent to the downgrade model' `
            ($body2 -match '"model"\s*:\s*"mock-gpt-mini"') "body: $body2"
    }

    # -----------------------------------------------------------------------
    # 9. REDIS STORE
    # -----------------------------------------------------------------------
    Write-Section '9. REDIS STORE: the counters are real Redis keys'

    # 'acme' again, but THIS acme lives in the redis config's tenant block (which
    # this script gives a spend and a session budget, so all four counter key
    # shapes exist to be read back).
    $redisTenant = 'acme'
    $probe1 = Send-ChatAs -Base $redisBase -Tenant $redisTenant -Prompt 'redis probe one' -Tag 'm3-redis-1'
    Assert-Equal 'the redis gateway admits a request' 200 $probe1.Status
    Assert-Equal 'the redis-backed admission is an allow' 'allow' $probe1.Quota
    $probe2 = Send-ChatAs -Base $redisBase -Tenant $redisTenant -Prompt 'redis probe two' -Tag 'm3-redis-2'
    Assert-Equal 'a second request through the redis store is admitted' 200 $probe2.Status

    Assert-Equal 'miniredis still answers PING' 'PONG' (Invoke-Redis -Port $MiniredisPort -Command @('PING'))
    Assert-True 'the redis keyspace is not empty' ([int](Invoke-Redis -Port $MiniredisPort -Command @('DBSIZE')) -gt 0)

    # The bucket names come from /admin/quota's own report, NOT from the clock, so
    # the keys read below are the keys the report just described. The two
    # assertions after it only ask that the report's bucket is one this probe
    # crossed: the clock is read on both sides of the report fetch, so a UTC
    # minute (or day) that rolls mid-probe is a non-event instead of a red gate.
    $redisDayBefore = Get-DayBucket
    $redisMinuteBefore = Get-MinuteBucket
    $report9 = (Get-QuotaReport -Base $redisBase -Tenant $redisTenant).report
    $redisDayAfter = Get-DayBucket
    $redisMinuteAfter = Get-MinuteBucket
    $day = [string]$report9.day
    $minute = [string]$report9.minute
    Assert-True 'the report dates the day window' `
        (@($redisDayBefore, $redisDayAfter) -contains $day) `
        "reported=$day before=$redisDayBefore after=$redisDayAfter"
    Assert-True 'the report dates the minute window' `
        (@($redisMinuteBefore, $redisMinuteAfter) -contains $minute) `
        "reported=$minute before=$redisMinuteBefore after=$redisMinuteAfter"
    $keyTokens = Get-QuotaKey -Tenant $redisTenant -Window 'day' -Bucket $day -Counter 'tokens'
    $keyCost = Get-QuotaKey -Tenant $redisTenant -Window 'day' -Bucket $day -Counter 'cost_micros'
    $keyMinute = Get-QuotaKey -Tenant $redisTenant -Window 'minute' -Bucket $minute -Counter 'requests'
    $keyAbsent = Get-QuotaKey -Tenant 'neverused' -Window 'day' -Bucket $day -Counter 'tokens'

    $rawTokens = Invoke-Redis -Port $MiniredisPort -Command @('GET', $keyTokens)
    $rawCost = Invoke-Redis -Port $MiniredisPort -Command @('GET', $keyCost)
    $rawMinute = Invoke-Redis -Port $MiniredisPort -Command @('GET', $keyMinute)
    $rawAbsent = Invoke-Redis -Port $MiniredisPort -Command @('GET', $keyAbsent)
    $ttlTokens1 = Invoke-Redis -Port $MiniredisPort -Command @('TTL', $keyTokens)
    $ttlMinute = Invoke-Redis -Port $MiniredisPort -Command @('TTL', $keyMinute)

    Assert-True "the day token key $keyTokens holds a number" ($rawTokens -match '^\d+$') "GET returned '$rawTokens'"
    Assert-Equal 'the day token key matches what /admin/quota reports' ([string]([long]$report9.tokens_today)) $rawTokens
    Assert-True "the day cost key $keyCost holds a number" ($rawCost -match '^\d+$') "GET returned '$rawCost'"
    Assert-Equal 'the day cost key matches what /admin/quota reports' ([string]([long]$report9.cost_today_micros)) $rawCost
    Assert-True "the minute request key $keyMinute holds a number" ($rawMinute -match '^\d+$') "GET returned '$rawMinute'"
    Assert-Equal 'the minute request key matches what /admin/quota reports' ([string]([long]$report9.requests_this_minute)) $rawMinute
    Assert-Equal 'a tenant that never sent traffic has no key at all' '' $rawAbsent

    Assert-True 'the day key has a positive TTL' ($ttlTokens1 -match '^\d+$' -and [long]$ttlTokens1 -gt 0) "TTL=$ttlTokens1"
    # untilDayEnd is "next UTC midnight, plus an hour of slack": never more than
    # 86400 + 3600 seconds, and never zero. A TTL of -1 (no expiry) would mean a
    # counter that outlives its window forever.
    Assert-True 'the day key expires inside the day window (plus the configured slack)' `
        ($ttlTokens1 -match '^\d+$' -and [long]$ttlTokens1 -le 90000) "TTL=$ttlTokens1"
    Assert-True 'the minute key expires inside two minutes' `
        ($ttlMinute -match '^\d+$' -and [long]$ttlMinute -gt 0 -and [long]$ttlMinute -le 120) "TTL=$ttlMinute"

    # The session counter lives in a third window, under its own key shape.
    $sessionId = 'm3-redis-session'
    $probe3 = Send-ChatAs -Base $redisBase -Tenant $redisTenant -Prompt 'redis probe session' -Session $sessionId -Tag 'm3-redis-3'
    Assert-Equal 'a session-scoped request through redis is admitted' 200 $probe3.Status
    $keySession = Get-QuotaKey -Tenant $redisTenant -Window 'session' -Bucket $sessionId -Counter 'tokens'
    $rawSession = Invoke-Redis -Port $MiniredisPort -Command @('GET', $keySession)
    $ttlSession = Invoke-Redis -Port $MiniredisPort -Command @('TTL', $keySession)
    $reportS = (Get-QuotaReport -Base $redisBase -Tenant $redisTenant -Session $sessionId).report
    Assert-True "the session token key $keySession holds a number" ($rawSession -match '^\d+$') "GET returned '$rawSession'"
    Assert-Equal 'the session token key matches what /admin/quota reports' ([string]([long]$reportS.session_tokens)) $rawSession
    Assert-True 'the session key expires within the default session TTL' `
        ($ttlSession -match '^\d+$' -and [long]$ttlSession -gt 0 -and [long]$ttlSession -le 86400) "TTL=$ttlSession"

    # More traffic must not push the expiry out: EXPIRE is issued only when the
    # key is created, so a busy tenant cannot hold a counter alive past its
    # window (which would silently charge today's traffic to tomorrow).
    $null = Send-ChatAs -Base $redisBase -Tenant $redisTenant -Prompt 'redis probe four' -Tag 'm3-redis-4'
    # Explicit guard: the two TTL reads must be of the same bucket, so a run that
    # straddles UTC midnight fails here with an unmistakable reason instead of
    # reporting a phantom TTL defect.
    $report9b = (Get-QuotaReport -Base $redisBase -Tenant $redisTenant).report
    Assert-Equal 'the UTC day did not roll during the redis section' $day ([string]$report9b.day)
    $rawTokens2 = Invoke-Redis -Port $MiniredisPort -Command @('GET', $keyTokens)
    $ttlTokens2 = Invoke-Redis -Port $MiniredisPort -Command @('TTL', $keyTokens)
    Assert-True 'more traffic keeps incrementing the same key' ([long]$rawTokens2 -gt [long]$rawTokens) `
        "before=$rawTokens after=$rawTokens2"
    Assert-True 'the day key''s second count did not grow' ([long]$ttlTokens2 -le [long]$ttlTokens1) `
        "first=$ttlTokens1 second=$ttlTokens2"

    # -----------------------------------------------------------------------
    # 10. FAIL CLOSED
    # -----------------------------------------------------------------------
    Write-Section '10. FAIL CLOSED: a dead store refuses instead of serving unmetered'

    $before10 = Get-LocalRequestCount $redisBase
    Write-Host "  stopping miniredis (pid=$($miniProc.Id))" -ForegroundColor DarkYellow
    Stop-Process -Id $miniProc.Id -Force -ErrorAction SilentlyContinue
    $miniProc = $null
    Assert-True "the redis store on :$MiniredisPort is really gone" (Wait-PortClosed -Port $MiniredisPort)

    $dead = Send-ChatAs -Base $redisBase -Tenant $redisTenant -Prompt 'fail closed probe' -Tag 'm3-failclosed'
    Assert-Equal 'with the store dead the redis gateway answers 503' 503 $dead.Status
    $deadBody = Get-Json $dead.Body
    Assert-True 'the 503 body is JSON' ($null -ne $deadBody)
    Assert-Equal 'the error type is infergate_quota_unavailable' 'infergate_quota_unavailable' $deadBody.error.type
    Assert-Equal 'the fail-closed refusal is marked reject' 'reject' $dead.Quota
    Assert-Equal 'the fail-closed refusal names the store error' 'store-error' $dead.Reason
    Assert-Contains 'the refusal says the request was not sent upstream' $dead.Body 'was not sent upstream'
    $after10 = Get-LocalRequestCount $redisBase
    Assert-Equal 'a fail-closed refusal never reaches the provider' $before10 $after10 `
        "upstream=local before=$before10 after=$after10"
    $admin10 = Get-QuotaAdmin -Base $redisBase
    Assert-True 'the store error is counted in the stats' ([double]$admin10.stats.store_errors -ge 1) `
        "store_errors=$($admin10.stats.store_errors)"

    Write-Host '  restarting miniredis' -ForegroundColor DarkYellow
    $miniProc = Start-Miniredis -Port $MiniredisPort
    Assert-True 'the restarted miniredis accepts connections' (Wait-PortOpen -Port $MiniredisPort)

    # Recovery must not need an operator: the counters live outside the process,
    # so the gateway picks the store back up by itself. The first attempt may
    # still land on a connection the pool took before the store died, which is
    # why this is a bounded retry rather than a single request.
    $recovered = $null
    $attempts = 0
    for ($i = 1; $i -le 10; $i++) {
        $attempts = $i
        $recovered = Send-ChatAs -Base $redisBase -Tenant $redisTenant -Prompt "recovery probe $i" -Tag "m3-recover-$i"
        if ($recovered.Status -eq 200) { break }
        Start-Sleep -Milliseconds 200
    }
    Assert-Equal "the gateway recovers without a restart (after $attempts attempt(s))" 200 $recovered.Status
    Assert-Equal 'the recovered request is admitted' 'allow' $recovered.Quota
    Assert-True 'the gateway process itself was never restarted' (-not $redisGwProc.HasExited) `
        "pid=$($redisGwProc.Id) exited"
    $rawRecovered = Invoke-Redis -Port $MiniredisPort -Command @('GET', $keyTokens)
    Assert-True 'the recovered gateway writes through to the restarted Redis' `
        ($rawRecovered -match '^\d+$' -and [long]$rawRecovered -gt 0) "GET $keyTokens -> '$rawRecovered'"

    # -----------------------------------------------------------------------
    # 11. SURFACES
    # -----------------------------------------------------------------------
    Write-Section '11. SURFACES: /metrics, /stats and the tenant report agree'

    $metrics = Invoke-Curl @('-s', "$base/metrics")
    Assert-True '/metrics returns a body' ($metrics.Length -gt 0)

    $statsAdmin = Get-QuotaAdmin -Base $base
    $qs = $statsAdmin.stats

    # Every quota family the task of an operator depends on: the decision mix,
    # the reservation ledger, the overshoot (are the estimates good?) and the two
    # counters that say the store or the tenant is misbehaving.
    foreach ($family in @('infergate_quota_decisions_total', 'infergate_quota_tokens_total', 'infergate_quota_overshoot_tokens_total', 'infergate_quota_released_cost_micros_total', 'infergate_quota_store_errors_total', 'infergate_quota_alerts_total')) {
        Assert-Contains "/metrics exports $family" $metrics $family
    }

    $mAllow = Get-Metric -Text $metrics -Series 'infergate_quota_decisions_total' -Labels 'action="allow"'
    $mDegrade = Get-Metric -Text $metrics -Series 'infergate_quota_decisions_total' -Labels 'action="degrade"'
    $mReject = Get-Metric -Text $metrics -Series 'infergate_quota_decisions_total' -Labels 'action="reject"'
    $mReserved = Get-Metric -Text $metrics -Series 'infergate_quota_tokens_total' -Labels 'kind="reserved"'
    $mSettled = Get-Metric -Text $metrics -Series 'infergate_quota_tokens_total' -Labels 'kind="settled"'
    $mReleased = Get-Metric -Text $metrics -Series 'infergate_quota_tokens_total' -Labels 'kind="released"'
    $mOvershoot = Get-Metric -Text $metrics -Series 'infergate_quota_overshoot_tokens_total'
    $mCostOvershoot = Get-Metric -Text $metrics -Series 'infergate_quota_overshoot_cost_micros_total'
    $mStoreErrors = Get-Metric -Text $metrics -Series 'infergate_quota_store_errors_total'
    $mAlerts = Get-Metric -Text $metrics -Series 'infergate_quota_alerts_total'
    $mReleasedCost = Get-Metric -Text $metrics -Series 'infergate_quota_released_cost_micros_total'

    Assert-True 'the allow decision series parses as a number' ($null -ne $mAllow)
    Assert-Equal 'the allow series matches /admin/quota' ([double]$qs.allowed) $mAllow
    Assert-True 'the degrade decision series parses as a number' ($null -ne $mDegrade)
    Assert-Equal 'the degrade series matches /admin/quota' ([double]$qs.degraded) $mDegrade
    Assert-True 'at least one request was degraded by now' ([double]$qs.degraded -ge 1) "degraded=$($qs.degraded)"
    Assert-True 'the reject decision series parses as a number' ($null -ne $mReject)
    Assert-Equal 'the reject series matches /admin/quota' ([double]$qs.rejected) $mReject
    Assert-True 'at least one request was rejected by now' ([double]$qs.rejected -ge 1) "rejected=$($qs.rejected)"
    Assert-True 'the reserved-token series parses as a number' ($null -ne $mReserved)
    Assert-Equal 'the reserved-token series matches /admin/quota' ([double]$qs.reserved_tokens) $mReserved
    Assert-True 'the settled-token series parses as a number' ($null -ne $mSettled)
    Assert-Equal 'the settled-token series matches /admin/quota' ([double]$qs.settled_tokens) $mSettled
    Assert-True 'the released-token series parses as a number' ($null -ne $mReleased)
    Assert-Equal 'the released-token series matches /admin/quota' ([double]$qs.released_tokens) $mReleased
    Assert-True 'a refusing gateway has released something (refusals roll back)' ([double]$qs.released_tokens -gt 0) `
        "released=$($qs.released_tokens)"
    Assert-True 'the overshoot-token series parses as a number' ($null -ne $mOvershoot)
    Assert-Equal 'the overshoot-token series matches /admin/quota' ([double]$qs.overshoot_tokens) $mOvershoot
    Assert-True 'the overshoot-cost series parses as a number' ($null -ne $mCostOvershoot)
    Assert-True 'the store-error series parses as a number' ($null -ne $mStoreErrors)
    Assert-Equal 'the memory gateway recorded no store errors' 0 $mStoreErrors
    Assert-True 'the alert series parses as a number' ($null -ne $mAlerts)
    Assert-Equal 'the alert series matches /admin/quota' ([double]$qs.alerts) $mAlerts
    Assert-True 'the released-cost series parses as a number' ($null -ne $mReleasedCost)
    Assert-Equal 'the released-cost series matches /admin/quota' ([double]$qs.released_cost_micros) $mReleasedCost

    # The reservation ledger has to add up: every token that was reserved either
    # came back, was overshot, or was really charged. A reservation that leaks --
    # reserved but never released and never settled -- only shows up here.
    #
    # The books are kept per reserved ENTRY, not per request: reserved, released
    # and settled each count one token budget's worth every time a request
    # consumes that budget, so a tenant carrying both a daily and a per-session
    # token budget contributes two entries per session request, and a refused
    # request appears on BOTH sides (its entries are reserved and then released in
    # the same admission). Kept that way the identity below holds exactly, for any
    # mix of requests -- which is the property an operator needs from a spending
    # report, and the reason the per-request version of this accounting (Admit
    # crediting est.Tokens() once, Settle crediting each entry) was a defect: it
    # made the books balance only on a gateway whose tenants had one token budget.
    $cgqs = (Get-QuotaAdmin -Base $captureBase).stats
    Assert-True 'the reservation ledger balances on a session-free gateway' `
        (([double]$cgqs.reserved_tokens - [double]$cgqs.released_tokens + [double]$cgqs.overshoot_tokens) -eq [double]$cgqs.settled_tokens) `
        "reserved=$($cgqs.reserved_tokens) released=$($cgqs.released_tokens) overshoot=$($cgqs.overshoot_tokens) settled=$($cgqs.settled_tokens)"
    # The same identity on the memory gateway, whose traffic mixes session-carrying
    # requests, refusals and cache hits: this is the assertion that fails if the
    # ledger ever goes back to counting reservations per request instead of per
    # entry.
    Assert-True 'the reservation ledger balances with sessions, refusals and hits in the mix' `
        (([double]$qs.reserved_tokens - [double]$qs.released_tokens + [double]$qs.overshoot_tokens) -eq [double]$qs.settled_tokens) `
        "reserved=$($qs.reserved_tokens) released=$($qs.released_tokens) overshoot=$($qs.overshoot_tokens) settled=$($qs.settled_tokens)"

    $stats = Get-Stats -Base $base
    Assert-True '/stats returns parseable JSON' ($null -ne $stats)
    Assert-True '/stats carries a quota block' ($null -ne $stats.quota)
    if ($null -ne $stats.quota) {
        Assert-Equal '/stats reports quota enabled' $true $stats.quota.enabled
        Assert-Equal '/stats names the memory store' 'memory' $stats.quota.store
        Assert-Equal '/stats agrees with /admin/quota on admissions' ([double]$qs.allowed) ([double]$stats.quota.allowed)
        Assert-Equal '/stats agrees with /admin/quota on rejections' ([double]$qs.rejected) ([double]$stats.quota.rejected)
        Assert-Equal '/stats agrees with /admin/quota on degraded requests' ([double]$qs.degraded) ([double]$stats.quota.degraded)
        Assert-Equal '/stats reports the reserved tokens too' ([double]$qs.reserved_tokens) ([double]$stats.quota.reserved_tokens)
        Assert-Equal '/stats reports the released spend too' ([double]$qs.released_cost_micros) ([double]$stats.quota.released_cost_micros)
    }

    $full = Get-QuotaReport -Base $base -Tenant 'acme' -Session 'm3-report-session'
    Assert-True 'the tenant report answers' ($null -ne $full)
    $report = $full.report
    Assert-True 'the tenant report carries a report block' ($null -ne $report)
    if ($null -ne $report) {
        foreach ($key in @('tenant', 'day', 'tokens_today', 'cost_today_micros', 'cost_today_usd', 'minute', 'requests_this_minute', 'session', 'session_tokens', 'baseline_tokens', 'ratio', 'alerting', 'policy')) {
            Assert-True "the tenant report carries '$key'" ($null -ne $report.PSObject.Properties[$key]) `
                "keys: $(@($report.PSObject.Properties.Name) -join ',')"
        }
        Assert-Equal 'the report echoes the tenant it was asked about' 'acme' ([string]$report.tenant)
        Assert-Equal 'the report echoes the session it was asked about' 'm3-report-session' ([string]$report.session)
        Assert-Equal 'a tenant with no history has no anomaly baseline' 0 ([double]$report.baseline_tokens)
        Assert-Equal 'a tenant with no history is not alerting' $false $report.alerting
        Assert-Equal 'the reported policy is the tenant policy, not the default' 500 ([int]$report.policy.tokens_per_day)
        Assert-Equal 'the reported policy names the tenant''s own exceed behaviour' 'reject' ([string]$report.policy.on_exceed)
    }

    # -----------------------------------------------------------------------
    # 12. NOT GOVERNED
    # -----------------------------------------------------------------------
    Write-Section '12. NOT GOVERNED: probes and model listings cost nothing'

    # Only the completion paths are metered: a health probe that consumed quota
    # would let a load balancer exhaust a tenant's budget, and a model listing is
    # not a provider call the tenant should be charged for.
    $qsBefore = (Get-QuotaAdmin -Base $base).stats
    $health = Invoke-Http -Url "$base/healthz" -Tag 'm3-healthz'
    Assert-Equal 'GET /healthz answers 200' 200 $health.Status
    Assert-Equal '/healthz carries no quota verdict' '' (Get-Header $health.Headers 'X-InferGate-Quota')
    $ready = Invoke-Http -Url "$base/readyz" -Tag 'm3-readyz'
    Assert-Equal 'GET /readyz answers 200' 200 $ready.Status
    Assert-Equal '/readyz carries no quota verdict' '' (Get-Header $ready.Headers 'X-InferGate-Quota')
    $models = Invoke-Http -Url "$base/v1/models" -Tag 'm3-models'
    Assert-Equal 'GET /v1/models answers 200' 200 $models.Status
    Assert-Contains 'the model listing came from the provider' $models.Body 'mock-gpt'
    Assert-Equal '/v1/models carries no quota verdict' '' (Get-Header $models.Headers 'X-InferGate-Quota')
    $qsAfter = (Get-QuotaAdmin -Base $base).stats
    foreach ($field in @('allowed', 'degraded', 'rejected', 'store_errors', 'alerts', 'reserved_tokens', 'settled_tokens', 'released_tokens', 'overshoot_tokens', 'overshoot_cost_micros', 'released_cost_micros')) {
        Assert-Equal "an unmetered request does not change stats.$field" ([double]$qsBefore.$field) ([double]$qsAfter.$field)
    }

    # -----------------------------------------------------------------------
    # 13. LOGS
    # -----------------------------------------------------------------------
    Write-Section '13. LOGS: every verdict is attributable to a tenant'

    $gwLogText = Read-OpenLog $gwLog
    Assert-True 'the gateway log is not empty' ($gwLogText.Length -gt 0) "log: $gwLog"
    Assert-Contains 'the log records the listen event' $gwLogText 'infergate listening'
    Assert-Contains 'the log records which store is enforcing' $gwLogText 'quota enforcement enabled'

    $allowedLines = @($gwLogText -split "`n" | Where-Object { $_ -match 'msg=request' -and $_ -match 'quota=allow' -and $_ -match 'tenant=acme' })
    Assert-True 'an allowed request is logged with its verdict and its tenant' ($allowedLines.Count -ge 1) `
        "allow lines for acme: $($allowedLines.Count)"
    $rejectedLines = @($gwLogText -split "`n" | Where-Object { $_ -match 'msg=request' -and $_ -match 'quota=reject' -and $_ -match 'tenant=acme' })
    Assert-True 'a rejected request is logged with its verdict and its tenant' ($rejectedLines.Count -ge 1) `
        "reject lines for acme: $($rejectedLines.Count)"
    $reasonLines = @($gwLogText -split "`n" | Where-Object { $_ -match 'quota=reject' -and $_ -match 'quota_reason=tokens_per_day' })
    Assert-True 'the rejection line names the budget the tenant crossed' ($reasonLines.Count -ge 1) `
        "lines naming tokens_per_day: $($reasonLines.Count)"
    $degradeLines = @($gwLogText -split "`n" | Where-Object { $_ -match 'quota=degrade' -and $_ -match 'degraded_to=mock-gpt-mini' })
    Assert-True 'the degrade line names the model the request was sent to' ($degradeLines.Count -ge 1) `
        "degrade lines naming the downgrade model: $($degradeLines.Count)"
    $limitLines = @($gwLogText -split "`n" | Where-Object { $_ -match 'quota=reject' -and $_ -match 'quota_limit=' })
    Assert-True 'the rejection line carries the limit and the total used' ($limitLines.Count -ge 1) `
        "reject lines carrying quota_limit: $($limitLines.Count)"
    Write-Host "        gateway log: $gwLog" -ForegroundColor DarkGray

    $redisGwLogText = Read-OpenLog $redisGwLog
    Assert-Contains 'the redis gateway logs the fail-closed refusal' $redisGwLogText 'quota: store unavailable, refusing request'
    Assert-Contains 'the redis gateway logs that it is enforcing with redis' $redisGwLogText 'quota enforcement enabled'

    # The same guard at the far end of the run: a green result only means the
    # governed paths were exercised if the fleet that served them is still the
    # fleet this script started.
    $deadFleetEnd = @(Get-DeadFleetMember -Urls $fleetUrls)
    $captureUpEnd = Test-PortOpen -Port $CaptureUpstreamPort
    Assert-True 'every fleet member survived the whole run' `
        (($deadFleetEnd.Count -eq 0) -and $captureUpEnd) `
        (($deadFleetEnd -join '; ') + " capture-listener=$captureUpEnd")
}
finally {
    # -----------------------------------------------------------------------
    # 14. Teardown
    # -----------------------------------------------------------------------
    Write-Section '14. Teardown'

    # Every process is stopped in the finally block, including on a thrown error:
    # a half-started fleet left listening on the test ports is what makes the
    # NEXT run fail for reasons that have nothing to do with the code.
    #
    # -KeepRunning is the deliberate exception, for the case where a failure needs
    # to be inspected against a live fleet: leaving the processes up is the whole
    # point of the switch, so it must actually suppress the kill rather than
    # mention it and kill anyway.
    if ($KeepRunning) {
        Write-Host '  -KeepRunning set: the fleet is left running for inspection.' -ForegroundColor DarkYellow
        foreach ($entry in @(
                @('memory gateway  ', $GatewayPort, $gwProc),
                @('redis gateway   ', $RedisGatewayPort, $redisGwProc),
                @('capture gateway ', $CaptureGatewayPort, $captureGwProc),
                @('mock upstream   ', $UpstreamPort, $mockProc),
                @('mock upstream   ', $RedisUpstreamPort, $mockRedisProc),
                @('capture upstream', $CaptureUpstreamPort, $captureProc),
                @('miniredis       ', $MiniredisPort, $miniProc))) {
            if ($entry[2]) { Write-Host "  pid=$($entry[2].Id)  $($entry[0]) :$($entry[1])" -ForegroundColor DarkYellow }
        }
        Write-Host '  none of these are stopped by this script when -KeepRunning is set.' -ForegroundColor DarkYellow
    }
    else {
        foreach ($p in @($gwProc, $redisGwProc, $captureGwProc, $mockProc, $mockRedisProc, $captureProc, $miniProc)) {
            if ($p -and -not $p.HasExited) {
                Write-Host "  stopping pid=$($p.Id)"
                Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
            }
        }

        # Process death and socket release are asynchronous; give them a moment
        # rather than reporting a leak that is really a race.
        $deadline = (Get-Date).AddSeconds(5)
        while ((Get-Date) -lt $deadline -and @(Get-TrackedProcess).Count -gt 0) { Start-Sleep -Milliseconds 100 }

        # This check cannot enumerate the process table (the sandbox denies
        # Get-CimInstance Win32_Process with HRESULT 0x80041003, and an empty table
        # makes "nothing survived" pass for the wrong reason). It asks Get-Process
        # about the pids recorded when they were started -- which is why the first
        # assertion below matters: if nothing had been recorded, the second one
        # would prove nothing at all.
        Assert-True 'the script recorded every process it started' ($script:startedPids.Count -ge 7) `
            "tracked=$($script:startedPids.Count) pids: $($script:startedPids -join ', ')"
        $alive = @(Get-TrackedProcess)
        Assert-Equal 'no process started by this script survived it' 0 $alive.Count `
            (($alive | ForEach-Object { "$($_.ProcessName) pid=$($_.Id)" }) -join '; ')
        # Fleet binaries that are still running and that this script did not start
        # are reported, never counted: a concurrent gate is not this one's leak.
        $foreignDone = @(Get-ForeignFleetProcess)
        $foreignNote = "  note: $($foreignDone.Count) infergate/mockupstream/miniredis process(es) not started by this script are still running"
        if ($foreignDone.Count -gt 0) {
            $foreignNote += ': ' + (($foreignDone | ForEach-Object { "$($_.ProcessName) pid=$($_.Id)" }) -join '; ')
        }
        Write-Host $foreignNote -ForegroundColor DarkYellow
        foreach ($p in $ports) {
            Assert-True "nothing is left listening on :$p" (Wait-PortClosed -Port $p -Seconds 3) `
                "port :$p still accepts connections after teardown"
        }
    }
}

Write-Host ''
Write-Host ("=" * 72) -ForegroundColor DarkGray
if ($script:failed -eq 0) {
    Write-Host "  RESULT: $($script:passed)/$($script:passed) assertions passed" -ForegroundColor Green
    Write-Host '  OK: M3 token and cost governance verified end to end with curl' -ForegroundColor Green
    exit 0
}
else {
    Write-Host "  RESULT: $($script:passed) passed, $($script:failed) FAILED" -ForegroundColor Red
    exit 1
}
