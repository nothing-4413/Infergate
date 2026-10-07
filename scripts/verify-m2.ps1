# InferGate M2 -- end-to-end verification of the semantic cache with curl.exe.
#
# M2 is a cache, and the interesting claims about a cache are all about what did
# NOT happen: a request that never reached a provider, tokens that were never
# bought, a second caller who was NOT served the first caller's answer. None of
# that is visible in the response body, so this script drives real binaries and
# asserts on the wire-level evidence the gateway adds:
#
#   X-InferGate-Cache          miss | hit-exact | hit-semantic | skip | bypass | refresh | error
#   X-InferGate-Upstream-Name  "cache" when the answer came from the cache
#   X-InferGate-Cache-Age      how old the served answer is, in milliseconds
#   /admin/cache               policy, counters and the scope table
#   /admin/cache/lookup        what the cache WOULD match, and how closely
#   /metrics                   cumulative counters, compared as DELTAS
#
# The strongest single assertion in this file does not look at a counter at all:
# section 6 KILLS the mock upstream, then asks the same question again and still
# gets a 200 out of the cache. A counter can only tell you the gateway believes
# it skipped the backend; a dead backend cannot lie about having answered.
#
# Usage (pwsh does not exist on this host -- Windows PowerShell 5.1 only):
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m2.ps1
#
# Everything is confined to this repository: binaries in bin\, logs and bodies in
# tmp\, and every process it starts is stopped in the finally block. Pass
# -KeepRunning to leave the fleet up for inspection instead (useful when an
# assertion fails and you want to curl the running gateway yourself).

[CmdletBinding()]
param(
    [int]$GatewayPort = 18280,
    [int]$UpstreamPort = 19500,
    [int]$RedisGatewayPort = 18281,
    [int]$RedisUpstreamPort = 19501,
    [int]$MiniredisPort = 19399,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
$gwLog = Join-Path $tmpDir 'm2-gateway.log'
$redisGwLog = Join-Path $tmpDir 'm2-redis-gateway.log'

$script:passed = 0
$script:failed = 0
$script:promptSeq = 0

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

function Assert-TrueOr {
    <#
        An assertion for a behaviour that is deliberately one of two acceptable
        outcomes -- "a dead cache is a miss OR an error, never a 500". The point
        is that some values are forbidden, not that one particular value is
        required, and the detail string records which the gateway actually chose
        so the report is evidence rather than a guess.
    #>
    param([string]$Label, [bool]$Condition, [string]$Detail = '')
    Assert-True $Label $Condition $Detail
}

function Assert-Contains {
    param([string]$Label, [string]$Haystack, [string]$Needle)
    # Literal substring test, NOT -like: -like applies wildcard matching, so a
    # needle containing brackets (scope strings, label selectors) becomes a
    # character class and reports false failures against text that demonstrably
    # contains it.
    Assert-True $Label ($null -ne $Haystack -and $Haystack.Contains($Needle)) `
        "expected to find: $Needle`n        actual: $(if ($Haystack) { $Haystack.Substring(0, [Math]::Min(400, $Haystack.Length)) })"
}

function Assert-Number {
    <#
        Asserts a value is a number, and optionally that it respects a bound.
        "X-InferGate-Cache-Age is a number of milliseconds" cannot be asserted as
        a literal: the value is a clock reading, so the claim is about its TYPE,
        not its value.
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
    # produced mojibake in this repo, and a response body carrying CJK content
    # would be silently corrupted by the console codepage.
    $reader = New-Object System.IO.StreamReader($Path, $script:utf8NoBom, $true)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
}

function Get-CachePolicy {
    <#
        Reads the cache policy out of the config TEXT this run hands the gateway,
        so section 3 can assert "the running process reports what this file says"
        instead of comparing the live admin surface against numbers copied into
        this script.  The file is the input under test: retuning
        configs/cache-local.yaml -- a threshold is an operator decision, and the
        comment next to it in that file says why 0.86 -- then moves the
        expectation with it rather than turning the gate red on a gateway that did
        exactly what it was told.

        Returns $null when the text carries no cache block at all, which is what
        the first assertion of section 3 is about.
    #>
    param([string]$Text)
    $block = [regex]::Match($Text, '(?ms)^cache:\s*\r?\n(.*?)(?=^\S|\z)')
    if (-not $block.Success) { return $null }
    $body = $block.Groups[1].Value
    function Read-Key([string]$Key) {
        $m = [regex]::Match($body, "(?m)^\s*$([regex]::Escape($Key)):\s*([^\s#]+)")
        if ($m.Success) { return $m.Groups[1].Value.Trim('"') }
        return $null
    }
    $enabled = Read-Key 'enabled'
    $threshold = Read-Key 'threshold'
    $floor = Read-Key 'min_prompt_chars'
    $nondeterministic = Read-Key 'allow_nondeterministic'
    $tools = Read-Key 'allow_tools'
    return [pscustomobject]@{
        Enabled               = $(if ($null -ne $enabled) { [bool]::Parse($enabled) } else { $null })
        Store                 = (Read-Key 'store')
        Threshold             = $(if ($null -ne $threshold) { [double]$threshold } else { $null })
        MinPromptChars        = $(if ($null -ne $floor) { [int]$floor } else { $null })
        AllowNondeterministic = $(if ($null -ne $nondeterministic) { [bool]::Parse($nondeterministic) } else { $null })
        AllowTools            = $(if ($null -ne $tools) { [bool]::Parse($tools) } else { $null })
    }
}

function New-BodyFile {
    <#
        Writes a JSON request body to a temp file and returns the '@path' form for
        curl. This indirection is not cosmetic: Windows PowerShell 5.1 strips the
        double quotes out of an argument when it re-quotes it for a native
        executable, so -d '{"model":"x"}' reaches curl as {model:x}. Passing the
        payload as a file with --data-binary sends the bytes verbatim -- which
        matters doubly for M2, because the cache's exact key is a hash of those
        bytes: a mangled body would not be a miss, it would be a different request.
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

function Get-HeaderValue {
    param($Response, [string]$Name)
    return (Get-Header $Response.Headers $Name)
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
    param([string]$Prompt)
    return '{"model":"mock-gpt","messages":[{"role":"user","content":"' + $Prompt + '"}]}'
}

function Get-UniquePrompt {
    <#
        A prompt that has never been sent before.

        The cache is keyed by the prompt, so a section that introduced its own
        wording would silently be answered from an earlier section's entry and
        assert "miss" against a hit. Every section that wants a cold start asks
        for a fresh prompt; the sequence number also makes a failing run readable,
        because the prompt text names the section that produced it.
    #>
    param([string]$Stem)
    $script:promptSeq++
    $n = $script:promptSeq.ToString('00')
    # Padded so that even the shortest stem clears cache.min_prompt_chars (12).
    return "$Stem number $n unique request"
}

function Invoke-Http {
    <#
        One HTTP request, body and headers to FILES.

        -D/-o instead of -i because curl's -i interleaves headers and body and a
        streamed body cannot be reliably split back out. -w prints the status code
        LAST so the captured stdout is a single number even when the body is
        empty.
    #>
    param(
        [string]$Url,
        [string]$Method = 'GET',
        [string]$BodyFile = $null,
        [string[]]$Headers = @(),
        [string]$Tag = 'm2'
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
        Cache   = (Get-Header $hdrText 'X-InferGate-Cache')
        Name    = (Get-Header $hdrText 'X-InferGate-Upstream-Name')
        Age     = (Get-Header $hdrText 'X-InferGate-Cache-Age')
        Tried   = (Get-Header $hdrText 'X-InferGate-Tried')
        Attempt = (Get-Header $hdrText 'X-InferGate-Attempt')
    }
}

function Send-Chat {
    <#
        A POST /v1/chat/completions through the gateway.

        The parameter is called Headers, not ExtraHeaders, because Windows
        PowerShell 5.1 SILENTLY DROPS an unknown named parameter on a plain
        function: a call site that says -Headers against a -ExtraHeaders
        parameter does not fail, it quietly sends the request with no extra
        headers at all -- which, for a cache, looks exactly like the directive
        being ignored by the gateway.
    #>
    param([string]$Base, [string]$BodyFile, [string[]]$Headers = @(), [string]$Tag = 'm2-chat')
    return (Invoke-Http -Url "$Base/v1/chat/completions" -Method 'POST' -BodyFile $BodyFile -Headers $Headers -Tag $Tag)
}

function Get-CacheAdmin {
    <# GET /admin/cache as an object, or $null when it is not answering. #>
    param([string]$Base)
    $r = Invoke-Http -Url "$Base/admin/cache" -Tag 'm2-admin'
    if ($r.Status -ne 200) { return $null }
    try { return ($r.Body | ConvertFrom-Json) } catch { return $null }
}

function Get-SeriesValue {
    <#
        Sums every sample of a Prometheus series whose label set CONTAINS the
        given labels, whatever order they appear in.

        The containment test is a lookahead rather than a prefix match, and that
        is not pedantry: the exported families put their labels in different
        orders, so a prefix match silently returns zero for the ones that happen
        to start with something else -- a wrong number that looks exactly like
        "the cache did nothing".

        Summing all matches is also the semantics wanted here: a counter family
        split across labels, added up. Labels is an ordered regex fragment, so a
        two-label check is written 'upstream="local",\s*state="open"'.
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

        Used only for the one proof that a counter cannot fake: the mock upstream
        is really gone before the cached replay is attempted. A TcpClient is used
        rather than Test-NetConnection because the Windows PowerShell 5.1 cmdlet
        is dramatically slower.
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

function Start-MockUpstream {
    param([int]$Port, [string]$Name, [string]$LogName)
    $log = Join-Path $tmpDir $LogName
    [System.IO.File]::WriteAllText($log, '', $utf8NoBom)
    $p = Start-Process -FilePath (Join-Path $binDir 'mockupstream.exe') `
        -ArgumentList @('-listen', ":$Port", '-name', $Name, '-token-delay', '1ms') `
        -RedirectStandardOutput $log -RedirectStandardError (Join-Path $tmpDir "$LogName.err") `
        -PassThru -WindowStyle Hidden
    Write-Host "  mockupstream.exe -name $Name pid=$($p.Id) listening :$Port"
    return $p
}

function Start-Gateway {
    param([string]$Config, [string]$Log, [string]$ErrName)
    [System.IO.File]::WriteAllText($Log, '', $utf8NoBom)
    $p = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $Config) `
        -RedirectStandardOutput $Log -RedirectStandardError (Join-Path $tmpDir $ErrName) `
        -PassThru -WindowStyle Hidden
    Write-Host "  infergate.exe pid=$($p.Id) config=$Config"
    return $p
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
$env:GOTOOLCHAIN = 'local'

New-Item -ItemType Directory -Force -Path $binDir, $tmpDir | Out-Null
Write-Host "  ports   : gateway :$GatewayPort -> mock upstream :$UpstreamPort"

$mockProc = $null
$mockRedisProc = $null
$gwProc = $null
$miniProc = $null
$redisGwProc = $null

try {
    # -----------------------------------------------------------------------
    # 1. Build
    # -----------------------------------------------------------------------
    Write-Section '1. Build'
    # The three binaries are built rather than assumed: a green script that ran
    # against a stale bin\ proves nothing about the tree under test.
    & $goShim build -o (Join-Path $binDir 'infergate.exe') ./cmd/infergate
    if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/infergate failed (rc=$LASTEXITCODE)" }
    Assert-True 'go build ./cmd/infergate succeeded' ($LASTEXITCODE -eq 0)

    & $goShim build -o (Join-Path $binDir 'mockupstream.exe') ./cmd/mockupstream
    if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/mockupstream failed (rc=$LASTEXITCODE)" }
    Assert-True 'go build ./cmd/mockupstream succeeded' ($LASTEXITCODE -eq 0)

    # miniredis is the in-repo RESP2 server: the Redis path is verified against a
    # real wire protocol implementation, not a stub.
    & $goShim build -o (Join-Path $binDir 'miniredis.exe') ./cmd/miniredis
    if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/miniredis failed (rc=$LASTEXITCODE)" }
    Assert-True 'go build ./cmd/miniredis succeeded' ($LASTEXITCODE -eq 0)

    foreach ($exe in @('infergate.exe', 'mockupstream.exe', 'miniredis.exe')) {
        Assert-True "bin\$exe exists" (Test-Path (Join-Path $binDir $exe))
    }

    # Rewrite both sample configs onto the ports this run owns, so the script
    # never fights a stale process on 8082/8083/9200/9201 or against another
    # copy of itself running at the same time. The substitutions name the YAML
    # keys, so the sample command lines in the files' own header comments are
    # left as documentation rather than silently rewritten.
    $gatewayCfg = Join-Path $tmpDir 'm2-cache-local.yaml'
    $cfg = (Read-Text (Join-Path $repo 'configs\cache-local.yaml')) `
        -replace '(?m)^(\s*listen:\s*)":8082"', "`$1`":$GatewayPort`"" `
        -replace 'http://127\.0\.0\.1:9200', "http://127.0.0.1:$UpstreamPort"
    [System.IO.File]::WriteAllText($gatewayCfg, $cfg, $utf8NoBom)
    Assert-Contains 'the rewritten memory config listens on this run''s gateway port' $cfg "listen: `":$GatewayPort`""
    Assert-Contains 'the rewritten memory config points at this run''s upstream' $cfg "http://127.0.0.1:$UpstreamPort"

    $redisCfg = Join-Path $tmpDir 'm2-cache-redis.yaml'
    $cfgR = (Read-Text (Join-Path $repo 'configs\cache-redis.yaml')) `
        -replace '(?m)^(\s*listen:\s*)":8083"', "`$1`":$RedisGatewayPort`"" `
        -replace 'http://127\.0\.0\.1:9201', "http://127.0.0.1:$RedisUpstreamPort" `
        -replace 'addr:\s*"127\.0\.0\.1:6399"', "addr: `"127.0.0.1:$MiniredisPort`""
    [System.IO.File]::WriteAllText($redisCfg, $cfgR, $utf8NoBom)
    Assert-Contains 'the rewritten redis config listens on this run''s redis gateway port' $cfgR "listen: `":$RedisGatewayPort`""
    Assert-Contains 'the rewritten redis config points at this run''s miniredis' $cfgR "addr: `"127.0.0.1:$MiniredisPort`""
    Assert-Contains 'the redis config really selects the redis store' $cfgR 'store: "redis"'

    # -----------------------------------------------------------------------
    # 2. Start the stack
    # -----------------------------------------------------------------------
    Write-Section '2. Start the mock upstream and the cache-enabled gateway'
    $mockProc = Start-MockUpstream -Port $UpstreamPort -Name 'local' -LogName 'm2-mock.log'
    Assert-Equal 'the mock upstream answers /healthz' '200' (Wait-Healthy -Port $UpstreamPort)

    $gwProc = Start-Gateway -Config $gatewayCfg -Log $gwLog -ErrName 'm2-gateway.err'
    $base = "http://127.0.0.1:$GatewayPort"
    Assert-Equal 'the gateway answers /readyz' '200' (Wait-Healthy -Port $GatewayPort -Path '/readyz')

    # -----------------------------------------------------------------------
    # 3. The admin surface describes the configured policy
    # -----------------------------------------------------------------------
    Write-Section '3. GET /admin/cache reports the configured policy'
    # The point of this section is that the cache the tests are about to exercise
    # is the one the operator configured: a semantic threshold of 0.86 is what
    # makes the section 5 paraphrase a hit, so the running process has to report
    # the policy the config file declares. Both sides are read: the expectation
    # comes out of the config text this run wrote for the gateway ($cfg above),
    # the actual out of /admin/cache. Neither is a literal in this script.
    $policy = Get-CachePolicy -Text $cfg
    Assert-True 'the config under test carries a cache block' ($null -ne $policy) 'read configs\cache-local.yaml'

    $adminResp = Invoke-Http -Url "$base/admin/cache" -Tag 'm2-admin'
    Assert-Equal 'GET /admin/cache answers 200' 200 $adminResp.Status

    $admin = $null
    try { $admin = $adminResp.Body | ConvertFrom-Json } catch { }
    Assert-True '/admin/cache returns parseable JSON' ($null -ne $admin) "body: $($adminResp.Body.Substring(0, [Math]::Min(200, $adminResp.Body.Length)))"

    if ($null -ne $admin -and $null -ne $policy) {
        Assert-Equal 'the cache is enabled as the config says' $policy.Enabled $admin.enabled
        Assert-Equal 'the configured store is the in-process memory store' $policy.Store $admin.store
        Assert-True '/admin/cache carries a config block' ($null -ne $admin.config)
        Assert-True '/admin/cache carries a stats block' ($null -ne $admin.stats)
        if ($null -ne $admin.config) {
            Assert-Equal "the running semantic threshold is the config's $($policy.Threshold)" $policy.Threshold ([double]$admin.config.threshold)
            Assert-Equal "the running prompt floor is the config's $($policy.MinPromptChars)" $policy.MinPromptChars ([int]$admin.config.min_prompt_chars)
            Assert-Equal 'sampling requests are exact-match only, as the config says' $policy.AllowNondeterministic $admin.config.allow_nondeterministic
            Assert-Equal 'tool requests are exact-match only, as the config says' $policy.AllowTools $admin.config.allow_tools
        }
        if ($null -ne $admin.stats) {
            # A brand-new gateway reports every counter at zero; asserting that
            # here is what makes the later deltas meaningful rather than merely
            # non-decreasing.
            Assert-Equal 'a fresh gateway has counted no lookups yet' 0 ([double]$admin.stats.lookups)
            Assert-Equal 'a fresh gateway has counted no hits yet' 0 ([double]$admin.stats.hits)
        }
    }

    # -----------------------------------------------------------------------
    # 4. MISS then EXACT HIT: the same bytes twice
    # -----------------------------------------------------------------------
    Write-Section '4. The same request twice: miss, then hit-exact'
    $p1 = Get-UniquePrompt 'cache baseline'
    $body1 = New-BodyFile 'm2-exact.json' (New-ChatBody $p1)

    $miss = Send-Chat -Base $base -BodyFile $body1 -Tag 'm2-exact-1'
    Assert-Equal 'the first request answers 200' 200 $miss.Status
    Assert-Equal 'the first request is a cache MISS' 'miss' $miss.Cache
    # The MISS must be resolved by a real backend: a cache that reports "miss"
    # while quietly serving something else would pass a header-only test.
    Assert-Equal 'the miss was served by the upstream, not the cache' 'local' $miss.Name

    $hit = Send-Chat -Base $base -BodyFile $body1 -Tag 'm2-exact-2'
    Assert-Equal 'the repeat answers 200' 200 $hit.Status
    # "hit-exact" and not merely "hit": the two requests were byte-identical, so
    # the cache must not have needed (or used) the embedding path at all.
    Assert-Equal 'an identical repeat is an EXACT hit' 'hit-exact' $hit.Cache
    Assert-Equal 'the hit is attributed to the cache, not to a backend' 'cache' $hit.Name
    # The age is a clock reading, so the claim is about its type. It also proves
    # the header is the entry's age and not a copy of some other value.
    Assert-Number 'X-InferGate-Cache-Age is a number of milliseconds' $hit.Age -Min 0
    # Byte equality, not JSON equality: M2's whole promise is that a replay is
    # indistinguishable from the original answer, and whitespace or key order
    # differences would break a client that hashed the body.
    Assert-True 'the replayed body is byte-identical to the live answer' ($miss.Body -ceq $hit.Body) `
        "live: $($miss.Body.Substring(0, [Math]::Min(160, $miss.Body.Length)))`n        replay: $($hit.Body.Substring(0, [Math]::Min(160, $hit.Body.Length)))"
    Assert-Contains 'the replayed body is a real completion, not an error envelope' $hit.Body '"object":"chat.completion"'

    # -----------------------------------------------------------------------
    # 5. SEMANTIC HIT: different words, same question
    # -----------------------------------------------------------------------
    Write-Section '5. A paraphrase of the same question: hit-semantic'
    # The pair is not invented here. It is the corpus entry cmd/measure-m2 and
    # internal/gateway/cache_test.go use, with a measured cosine of 0.8819
    # against the shipped threshold of 0.86 -- a paraphrase at 0.88 is a
    # deliberate margin, because a pair sitting at 0.861 would make this section
    # a coin flip on a different host.
    $s1 = 'summarise the design document'
    $s2 = 'please summarise the design document'
    $semBody1 = New-BodyFile 'm2-sem-1.json' (New-ChatBody $s1)

    $sMiss = Send-Chat -Base $base -BodyFile $semBody1 -Tag 'm2-sem-1'
    Assert-Equal 'the canonical wording answers 200' 200 $sMiss.Status
    Assert-Equal 'the canonical wording is a MISS' 'miss' $sMiss.Cache

    $semBody2 = New-BodyFile 'm2-sem-2.json' (New-ChatBody $s2)
    $sHit = Send-Chat -Base $base -BodyFile $semBody2 -Tag 'm2-sem-2'
    Assert-Equal 'the paraphrase answers 200' 200 $sHit.Status
    Assert-Equal 'a paraphrase above the threshold is a SEMANTIC hit' 'hit-semantic' $sHit.Cache
    Assert-Equal 'the semantic hit is attributed to the cache' 'cache' $sHit.Name
    # The answer to the paraphrase must be the answer to the ORIGINAL question:
    # that is what makes it a cache hit and not a re-run.
    Assert-True 'the semantic hit replays the stored answer' ($sHit.Body -ceq $sMiss.Body) `
        "stored: $($sMiss.Body.Substring(0, [Math]::Min(160, $sMiss.Body.Length)))`n        served: $($sHit.Body.Substring(0, [Math]::Min(160, $sHit.Body.Length)))"

    $admin = Get-CacheAdmin -Base $base
    Assert-True 'the stats block counts the semantic hit separately from the exact one' `
        ($null -ne $admin -and [double]$admin.stats.semantic_hits -ge 1) `
        "semantic_hits=$(if ($admin) { $admin.stats.semantic_hits })"

    # -----------------------------------------------------------------------
    # 6. A hit does not call the upstream -- proven with a dead upstream
    # -----------------------------------------------------------------------
    Write-Section '6. A hit is served without the upstream, and without its tokens'

    # From here to the restart the mock upstream is gone on purpose, so both the
    # gateway's own retry logic and the OS have a moment to let the socket go.
    # The prove-it retries below do NOT paper over a product bug: if the gateway
    # could not answer from cache it would return 502 every time, and the
    # assertion would still fail after the window.
    Write-Host "  killing the mock upstream (pid=$($mockProc.Id))" -ForegroundColor DarkYellow
    Stop-Process -Id $mockProc.Id -Force -ErrorAction SilentlyContinue
    $mockProc = $null
    Assert-True "the mock upstream on :$UpstreamPort is really gone" (Wait-PortClosed -Port $UpstreamPort) `
        'the port still accepts connections after killing the process'

    # This is the argument the counters cannot make. The body was already stored
    # by section 4, and the only process that could have produced this answer is
    # dead, so a 200 here is a 200 from the cache.
    $deadReply = Send-Chat -Base $base -BodyFile $body1 -Tag 'm2-dead'
    Assert-Equal 'with the upstream DEAD an identical request still answers 200' 200 $deadReply.Status
    Assert-Equal 'the answer came from the cache, not the corpse' 'hit-exact' $deadReply.Cache
    Assert-Equal 'the dead upstream is not credited for the answer' 'cache' $deadReply.Name
    Assert-True 'the cached answer is byte-identical even with no backend alive' ($deadReply.Body -ceq $miss.Body) `
        "served: $($deadReply.Body.Substring(0, [Math]::Min(160, $deadReply.Body.Length)))"

    Write-Host '  restarting the mock upstream' -ForegroundColor DarkYellow
    $mockProc = Start-MockUpstream -Port $UpstreamPort -Name 'local' -LogName 'm2-mock.log'
    Assert-Equal 'the restarted mock upstream answers /healthz' '200' (Wait-Healthy -Port $UpstreamPort)

    # Token accounting, as deltas across ONE request.
    #
    # infergate_tokens_total is the provider-attributed family: a replay must not
    # touch it, or the gateway would report spending money it did not spend. The
    # cache's own saved-tokens counter must move by the same amount instead --
    # which is the only way to tell a working cache from a cache that quietly
    # re-sends requests.
    $mPrompt = Get-UniquePrompt 'token accounting'
    $mBody = New-BodyFile 'm2-tokens.json' (New-ChatBody $mPrompt)

    $prime = Send-Chat -Base $base -BodyFile $mBody -Tag 'm2-tok-1'
    Assert-Equal 'the priming request is a MISS' 'miss' $prime.Cache

    $before = Invoke-Curl @('-s', "$base/metrics")
    $tokensBefore = Get-SeriesValue -Text $before -Series 'infergate_tokens_total'
    $hitsBefore = Get-SeriesValue -Text $before -Series 'infergate_cache_hits_total' -Labels 'kind="exact"'
    $savedBefore = Get-SeriesValue -Text $before -Series 'infergate_cache_saved_tokens_total' -Labels 'kind="prompt"'

    $replay = Send-Chat -Base $base -BodyFile $mBody -Tag 'm2-tok-2'
    Assert-Equal 'the replay is an exact hit' 'hit-exact' $replay.Cache

    $after = Invoke-Curl @('-s', "$base/metrics")
    $tokensAfter = Get-SeriesValue -Text $after -Series 'infergate_tokens_total'
    $hitsAfter = Get-SeriesValue -Text $after -Series 'infergate_cache_hits_total' -Labels 'kind="exact"'
    $savedAfter = Get-SeriesValue -Text $after -Series 'infergate_cache_saved_tokens_total' -Labels 'kind="prompt"'

    Assert-Equal 'a cache hit buys NO provider tokens' $tokensBefore $tokensAfter
    Assert-Equal 'the cache records exactly one more exact hit' ($hitsBefore + 1) $hitsAfter
    Assert-True 'the cache records the prompt tokens it saved' ($savedAfter -gt $savedBefore) `
        "saved prompt tokens before=$savedBefore after=$savedAfter"

    # -----------------------------------------------------------------------
    # 7. Tenant isolation
    # -----------------------------------------------------------------------
    Write-Section '7. Two callers never share an answer'
    # The failure this guards against is the one that makes a cache dangerous:
    # tenant beta receiving tenant alpha's answer. A different caller must be a
    # different scope even when the bytes are identical.
    $tp = Get-UniquePrompt 'tenant isolation'
    $tBody = New-BodyFile 'm2-tenant.json' (New-ChatBody $tp)
    $alpha = @('-H', 'X-InferGate-Tenant: alpha')
    $beta = @('-H', 'X-InferGate-Tenant: beta')

    $rA = Send-Chat -Base $base -BodyFile $tBody -Headers $alpha -Tag 'm2-tenant-a'
    $rB = Send-Chat -Base $base -BodyFile $tBody -Headers $beta -Tag 'm2-tenant-b'
    Assert-Equal 'tenant alpha''s first request is a MISS' 'miss' $rA.Cache
    Assert-Equal 'tenant beta''s identical bytes are a MISS too' 'miss' $rB.Cache
    Assert-Equal 'beta''s miss went to a real backend' 'local' $rB.Name

    $rA2 = Send-Chat -Base $base -BodyFile $tBody -Headers $alpha -Tag 'm2-tenant-a2'
    Assert-Equal 'tenant alpha still gets its own exact hit' 'hit-exact' $rA2.Cache
    Assert-True 'alpha''s replay is alpha''s answer' ($rA2.Body -ceq $rA.Body)

    # The two tenants must live in two scopes, not one: asserting on the scope
    # table is what distinguishes "isolated" from "happened to miss".
    $admin = Get-CacheAdmin -Base $base
    Assert-True 'the two tenants occupy two cache scopes' `
        ($null -ne $admin -and @($admin.scopes.PSObject.Properties).Count -ge 2) `
        "scopes=$(if ($admin) { (@($admin.scopes.PSObject.Properties | ForEach-Object { "$($_.Name)=$($_.Value)" })) -join ',' })"

    # -----------------------------------------------------------------------
    # 8. Directives: bypass and refresh
    # -----------------------------------------------------------------------
    Write-Section '8. X-InferGate-Cache directives'
    # A cache an operator cannot override is a liability during an incident, so
    # both directives are asserted to (a) be reported on the response and (b)
    # actually reach the backend despite a stored answer.
    $bp = Get-UniquePrompt 'bypass directive'
    $bBody = New-BodyFile 'm2-bypass.json' (New-ChatBody $bp)

    $b0 = Send-Chat -Base $base -BodyFile $bBody -Tag 'm2-bypass-0'
    Assert-Equal 'the priming request is a MISS' 'miss' $b0.Cache

    $b1 = Send-Chat -Base $base -BodyFile $bBody -Headers @('-H', 'X-InferGate-Cache: bypass') -Tag 'm2-bypass-1'
    Assert-Equal 'a bypassed request answers 200' 200 $b1.Status
    Assert-Equal 'the directive is echoed on the response' 'bypass' $b1.Cache
    # "bypass" must mean the upstream was called even though an entry existed:
    # the header alone would be satisfied by a gateway that reported bypass and
    # served the cached answer anyway.
    Assert-Equal 'bypass reached the upstream instead of the cache' 'local' $b1.Name

    # A bypassed answer is still a good answer, so it is stored: the next plain
    # request must hit. Otherwise bypass would be indistinguishable from
    # "disable the cache", and an operator debugging an answer could not warm it.
    $b2 = Send-Chat -Base $base -BodyFile $bBody -Tag 'm2-bypass-2'
    Assert-Equal 'the bypassed answer was stored for next time' 'hit-exact' $b2.Cache

    $rp = Get-UniquePrompt 'refresh directive'
    $rBody = New-BodyFile 'm2-refresh.json' (New-ChatBody $rp)
    $r0 = Send-Chat -Base $base -BodyFile $rBody -Tag 'm2-refresh-0'
    Assert-Equal 'the priming request is a MISS' 'miss' $r0.Cache

    $r1 = Send-Chat -Base $base -BodyFile $rBody -Headers @('-H', 'X-InferGate-Cache: refresh') -Tag 'm2-refresh-1'
    Assert-Equal 'a refreshed request answers 200' 200 $r1.Status
    Assert-Equal 'the refresh directive is echoed on the response' 'refresh' $r1.Cache
    Assert-Equal 'refresh re-read the upstream rather than the stored answer' 'local' $r1.Name

    $r2 = Send-Chat -Base $base -BodyFile $rBody -Tag 'm2-refresh-2'
    Assert-Equal 'the refreshed answer replaced the stored one' 'hit-exact' $r2.Cache

    # -----------------------------------------------------------------------
    # 9. Policy limits: what must never be answered from a neighbour
    # -----------------------------------------------------------------------
    Write-Section '9. Policy limits'

    # (a) temperature > 0. Sampling requests are matched exactly only. The
    # reason is the one the config file gives: an identical retry is the same
    # sampling call and should not be re-sampled, but answering a paraphrase
    # with someone else's sample is a wrong answer.
    $sp = Get-UniquePrompt 'sampling request'
    $sBody = New-BodyFile 'm2-temp.json' ('{"model":"mock-gpt","temperature":0.9,"messages":[{"role":"user","content":"' + $sp + '"}]}')
    $t1 = Send-Chat -Base $base -BodyFile $sBody -Tag 'm2-temp-1'
    Assert-Equal 'a sampling request is a MISS' 'miss' $t1.Cache
    $t2 = Send-Chat -Base $base -BodyFile $sBody -Tag 'm2-temp-2'
    Assert-Equal 'an identical sampling request is an EXACT hit' 'hit-exact' $t2.Cache

    $sp2 = Get-UniquePrompt 'sampling paraphrase'
    $sBody2 = New-BodyFile 'm2-temp-para.json' ('{"model":"mock-gpt","temperature":0.9,"messages":[{"role":"user","content":"' + $sp2 + '"}]}')
    $t3 = Send-Chat -Base $base -BodyFile $sBody2 -Tag 'm2-temp-3'
    Assert-Equal 'a fresh sampling request does not inherit the first one''s answer' 'miss' $t3.Cache

    # The paraphrase test above uses distinct wording by construction; this one
    # uses an actual paraphrase of the PRIMED text, which is the case the policy
    # exists for: the same words at temperature 0 must hit, and at temperature
    # 0.9 they must not.
    $tempPair1 = 'summarise the quarterly revenue report'
    $tempPair2 = 'please summarise the quarterly revenue report'
    $ta = New-BodyFile 'm2-temp-a.json' ('{"model":"mock-gpt","temperature":0.9,"messages":[{"role":"user","content":"' + $tempPair1 + '"}]}')
    $tb = New-BodyFile 'm2-temp-b.json' ('{"model":"mock-gpt","temperature":0.9,"messages":[{"role":"user","content":"' + $tempPair2 + '"}]}')
    $ta1 = Send-Chat -Base $base -BodyFile $ta -Tag 'm2-temp-a1'
    $tb1 = Send-Chat -Base $base -BodyFile $tb -Tag 'm2-temp-b1'
    Assert-Equal 'the first phrasing of the sampling pair is a MISS' 'miss' $ta1.Cache
    Assert-Equal 'its paraphrase is a MISS, never a semantic hit' 'miss' $tb1.Cache

    # (b) tools. Two paraphrases of a question can legitimately choose different
    # tools, so a tool request is exact-only: same bytes hit, paraphrase misses.
    $toolReq = '{"type":"function","function":{"name":"get_weather","description":"look up the weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}'
    $gp = Get-UniquePrompt 'tool request'
    $gBody = New-BodyFile 'm2-tools.json' ('{"model":"mock-gpt","messages":[{"role":"user","content":"' + $gp + '"}],"tools":[' + $toolReq + ']}')
    $g1 = Send-Chat -Base $base -BodyFile $gBody -Tag 'm2-tools-1'
    Assert-Equal 'a tool request is a MISS' 'miss' $g1.Cache
    $g2 = Send-Chat -Base $base -BodyFile $gBody -Tag 'm2-tools-2'
    Assert-Equal 'an identical tool request is an EXACT hit' 'hit-exact' $g2.Cache

    $gp2 = Get-UniquePrompt 'tool paraphrase'
    $gBody2 = New-BodyFile 'm2-tools-para.json' ('{"model":"mock-gpt","messages":[{"role":"user","content":"' + $gp2 + '"}],"tools":[' + $toolReq + ']}')
    $g3 = Send-Chat -Base $base -BodyFile $gBody2 -Tag 'm2-tools-3'
    Assert-Equal 'a different tool request is a MISS' 'miss' $g3.Cache

    # (c) Trivial prompts. Under min_prompt_chars (12) the embedding cost is not
    # worth paying, and "hi" matching another caller's "hi" is not a cache being
    # useful -- it is a cache being lucky. The status is `skip`, distinct from
    # `miss`, so an operator can see WHY nothing was cached.
    $hiBody = New-BodyFile 'm2-hi.json' (New-ChatBody 'hi')
    $h1 = Send-Chat -Base $base -BodyFile $hiBody -Tag 'm2-hi-1'
    Assert-Equal 'a prompt under min_prompt_chars answers 200' 200 $h1.Status
    Assert-Equal 'a prompt under the floor reports skip' 'skip' $h1.Cache
    Assert-Equal 'the skipped request still reached the upstream' 'local' $h1.Name

    $h2 = Send-Chat -Base $base -BodyFile $hiBody -Tag 'm2-hi-2'
    Assert-Equal 'a trivial prompt is never stored, so the repeat is a skip again' 'skip' $h2.Cache
    Assert-Equal 'the repeat was answered by the upstream, not the cache' 'local' $h2.Name

    # (d) max_tokens is part of the answer's shape. Two requests that differ only
    # in how long an answer they asked for are not the same request, so the exact
    # key must separate them even though the prompt is identical.
    $mp = Get-UniquePrompt 'max tokens'
    $maxBody50 = New-BodyFile 'm2-max-50.json' ('{"model":"mock-gpt","max_tokens":50,"messages":[{"role":"user","content":"' + $mp + '"}]}')
    $maxBody900 = New-BodyFile 'm2-max-900.json' ('{"model":"mock-gpt","max_tokens":900,"messages":[{"role":"user","content":"' + $mp + '"}]}')
    $x1 = Send-Chat -Base $base -BodyFile $maxBody50 -Tag 'm2-max-1'
    Assert-Equal 'max_tokens=50 is a MISS' 'miss' $x1.Cache
    $x2 = Send-Chat -Base $base -BodyFile $maxBody900 -Tag 'm2-max-2'
    Assert-Equal 'the same prompt with max_tokens=900 is a DIFFERENT request' 'miss' $x2.Cache
    $x3 = Send-Chat -Base $base -BodyFile $maxBody50 -Tag 'm2-max-3'
    Assert-Equal 'the original max_tokens=50 body still hits its own entry' 'hit-exact' $x3.Cache

    # -----------------------------------------------------------------------
    # 10. Streaming
    # -----------------------------------------------------------------------
    Write-Section '10. Streamed answers are stored and replayed frame for frame'
    # A cached stream must stay a stream: an SSE client reads incrementally, and
    # a replay that arrives as one JSON blob with an SSE content type would hang
    # the reader waiting for a [DONE] that never comes.
    $strPrompt1 = 'summarise the incident review document'
    $strPrompt2 = 'please summarise the incident review document'
    # Prime the stream through a NON-streaming request: M2 stores the stream
    # variant under the same prompt, so the streaming replay is a semantic hit on
    # the primed wording -- which also proves the SSE frames come out of the
    # stored entry rather than a fresh backend call.
    $primeBody = New-BodyFile 'm2-stream-prime.json' (New-ChatBody $strPrompt1)
    $p0 = Send-Chat -Base $base -BodyFile $primeBody -Tag 'm2-stream-prime'
    Assert-Equal 'the streaming prompt is primed with a MISS' 'miss' $p0.Cache

    $strBody = New-BodyFile 'm2-stream.json' ('{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"' + $strPrompt2 + '"}]}')
    $live = Send-Chat -Base $base -BodyFile $strBody -Tag 'm2-stream-live'
    Assert-Equal 'the live stream answers 200' 200 $live.Status
    Assert-Contains 'the live stream is text/event-stream' $live.Headers 'text/event-stream'
    Assert-Contains 'the live stream carries completion chunks' $live.Body 'chat.completion.chunk'
    Assert-True 'the live stream ends with the [DONE] sentinel' `
        ($live.Body.TrimEnd("`r", "`n").EndsWith('data: [DONE]')) `
        "tail: $($live.Body.Substring([Math]::Max(0, $live.Body.Length - 80)))"

    $replayStream = Send-Chat -Base $base -BodyFile $strBody -Tag 'm2-stream-replay'
    Assert-Equal 'the replayed stream answers 200' 200 $replayStream.Status
    Assert-Equal 'the repeated stream is an EXACT hit' 'hit-exact' $replayStream.Cache
    Assert-Equal 'the replay is attributed to the cache' 'cache' $replayStream.Name
    Assert-Contains 'the replay is typed as an event stream' $replayStream.Headers 'text/event-stream'
    Assert-Contains 'the replay carries completion chunks' $replayStream.Body 'chat.completion.chunk'
    Assert-True 'the replayed stream ends with the [DONE] sentinel' `
        ($replayStream.Body.TrimEnd("`r", "`n").EndsWith('data: [DONE]')) `
        "tail: $($replayStream.Body.Substring([Math]::Max(0, $replayStream.Body.Length - 80)))"

    # Counting data: lines is the strongest available check on the replay: it
    # proves every frame survived the store-and-replay round trip and that the
    # terminal [DONE] was not consumed as a payload frame. The prompts differ by
    # one word, so the completion text -- and therefore the frame count -- is the
    # deterministic part of both answers.
    $liveFrames = @($live.Body -split "`n" | Where-Object { $_.TrimStart().StartsWith('data:') }).Count
    $replayFrames = @($replayStream.Body -split "`n" | Where-Object { $_.TrimStart().StartsWith('data:') }).Count
    Assert-True 'both streams carry more than the [DONE] sentinel' ($liveFrames -ge 2) "live data lines: $liveFrames"
    Assert-Equal 'the replayed stream has the same number of data: lines as the live one' $liveFrames $replayFrames

    # -----------------------------------------------------------------------
    # 11. Admin surfaces: lookup and flush
    # -----------------------------------------------------------------------
    Write-Section '11. The cache can be inspected and flushed'
    # Discover the scope the cache chose rather than guessing it: the scope is a
    # hash of tenant + model + capabilities, and a hard-coded guess would make
    # this section assert on an empty table for reasons that have nothing to do
    # with the feature.
    $admin = Get-CacheAdmin -Base $base
    Assert-True '/admin/cache reports the scope table' ($null -ne $admin -and $null -ne $admin.scopes)
    $scopes = @()
    if ($null -ne $admin -and $null -ne $admin.scopes) {
        $scopes = @($admin.scopes.PSObject.Properties | ForEach-Object { $_.Name })
    }
    Write-Host "  cache scopes: $($scopes -join ', ')" -ForegroundColor DarkGray
    Assert-True 'the cache has at least one scope to look into' ($scopes.Count -ge 1) `
        'no scopes reported: nothing was stored at all'

    $lookupScope = ''
    # A scope is a hash of tenant + model + capabilities, so it cannot be guessed
    # and it cannot be read off as "the first scope in the table": that table is
    # map-ordered and already holds several scopes by now (plain, streaming,
    # tools). Picking the wrong one is a silent, confusing failure -- a lookup
    # against another caller's scope reports "0 matches" and a flush against it
    # removes the wrong entries, which looks exactly like a broken cache.
    #
    # The reliable way to name the scope is to make one appear: prime with a
    # brand-new tenant identity and watch the table grow by exactly one key.
    $scopesBefore = @()
    $probeTenant = ''
    if ($null -ne $admin -and $null -ne $admin.scopes) {
        $scopesBefore = @($admin.scopes.PSObject.Properties | ForEach-Object { $_.Name })
    }
    $probeTenant = 'probe-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
    $probeHeaders = @('-H', "X-InferGate-Tenant: $probeTenant")

    # A real question, primed under that fresh identity, so the lookup below has
    # exactly one entry in exactly one scope to find.
    $lp = Get-UniquePrompt 'lookup probe'
    $lpBody = New-BodyFile 'm2-lookup.json' (New-ChatBody $lp)
    $l0 = Send-Chat -Base $base -BodyFile $lpBody -Headers $probeHeaders -Tag 'm2-lookup-0'
    Assert-Equal 'the lookup probe is primed with a MISS' 'miss' $l0.Cache

    $adminProbe = Get-CacheAdmin -Base $base
    $scopesAfter = @()
    if ($null -ne $adminProbe -and $null -ne $adminProbe.scopes) {
        $scopesAfter = @($adminProbe.scopes.PSObject.Properties | ForEach-Object { $_.Name })
    }
    $newScopes = @($scopesAfter | Where-Object { $scopesBefore -notcontains $_ })
    Assert-True 'storing for a brand-new caller created exactly one new scope' `
        ($newScopes.Count -eq 1) `
        "new=$(($newScopes -join ', ')); before=$($scopesBefore.Count) after=$($scopesAfter.Count)"
    if ($newScopes.Count -ge 1) { $lookupScope = $newScopes[0] }

    $enc = [uri]::EscapeDataString($lp)
    $encScope = [uri]::EscapeDataString($lookupScope)
    $lookup = Invoke-Http -Url "$base/admin/cache/lookup?prompt=$enc&scope=$encScope" -Tag 'm2-lookup-1'
    Assert-Equal 'GET /admin/cache/lookup answers 200' 200 $lookup.Status
    $lookupDoc = $null
    try { $lookupDoc = $lookup.Body | ConvertFrom-Json } catch { }
    Assert-True '/admin/cache/lookup returns parseable JSON' ($null -ne $lookupDoc)
    if ($null -ne $lookupDoc) {
        # `features` is the embedded representation the match was made from: it
        # is what makes a surprising hit explainable rather than mysterious.
        Assert-True 'the lookup explains itself with a features array' `
            ($null -ne $lookupDoc.features -and @($lookupDoc.features).Count -ge 1) `
            "features=$(if ($lookupDoc.features) { @($lookupDoc.features).Count } else { 'none' })"
        $matches = @($lookupDoc.matches)
        Assert-True 'the lookup finds the stored entry' ($matches.Count -ge 1) `
            "matches: $($matches.Count); body: $($lookup.Body.Substring(0, [Math]::Min(200, $lookup.Body.Length)))"
        if ($matches.Count -ge 1) {
            # The threshold is read back from the running gateway, so this asserts
            # "the reported similarity is above the configured bar" rather than
            # against a number copied into the script. The config text this run
            # handed the gateway is the fallback for the case the admin surface
            # omits the block -- section 3 has already failed by then.
            $threshold = 0.0
            if ($null -ne $policy) { $threshold = [double]$policy.Threshold }
            if ($null -ne $admin.config) { $threshold = [double]$admin.config.threshold }
            $best = [double]$matches[0].similarity
            Assert-True 'the match reaches the configured similarity threshold' ($best -ge $threshold) `
                "similarity=$best threshold=$threshold"
            Assert-Contains 'the match names the stored prompt' ([string]$matches[0].prompt) $lp
        }
    }

    # A lookup with no scope has nothing to search, and the route must say so
    # rather than searching every scope -- a lookup that ignores its argument is
    # how one tenant's prompts leak into another tenant's answer.
    $lookupNoScope = Invoke-Http -Url "$base/admin/cache/lookup?prompt=$enc" -Tag 'm2-lookup-2'
    Assert-Equal 'a scope-less lookup still answers 200' 200 $lookupNoScope.Status
    $noScopeDoc = $null
    try { $noScopeDoc = $lookupNoScope.Body | ConvertFrom-Json } catch { }
    Assert-True 'a scope-less lookup matches nothing' `
        ($null -ne $noScopeDoc -and @($noScopeDoc.matches).Count -eq 0) `
        "matches=$(if ($noScopeDoc) { @($noScopeDoc.matches).Count })"

    # Flushing must be observable, so the flush is checked from the outside: the
    # response has to say how much it removed, and the lookup that found the
    # entry a moment ago must now find nothing. Asserting on the lookup rather
    # than on the stats counters is deliberate -- /admin/cache/lookup goes
    # straight to the store, so it is evidence that the ENTRIES are gone, not
    # merely that a counter moved.
    $flush = Invoke-Http -Url "$base/admin/cache/flush?scope=$encScope" -Method 'POST' -Tag 'm2-flush'
    Assert-Equal 'POST /admin/cache/flush answers 200' 200 $flush.Status
    $flushDoc = $null
    try { $flushDoc = $flush.Body | ConvertFrom-Json } catch { }
    Assert-True 'the flush reports how many entries it removed' ($null -ne $flushDoc -and [int]$flushDoc.flushed -ge 1) `
        "body: $($flush.Body)"

    $lookupAfter = Invoke-Http -Url "$base/admin/cache/lookup?prompt=$enc&scope=$encScope" -Tag 'm2-lookup-3'
    Assert-Equal 'the post-flush lookup still answers 200' 200 $lookupAfter.Status
    $afterDoc = $null
    try { $afterDoc = $lookupAfter.Body | ConvertFrom-Json } catch { }
    Assert-True 'the flushed scope is empty' `
        ($null -ne $afterDoc -and @($afterDoc.matches).Count -eq 0) `
        "matches=$(if ($afterDoc) { @($afterDoc.matches).Count }); body: $($lookupAfter.Body.Substring(0, [Math]::Min(200, $lookupAfter.Body.Length)))"

    # The same caller identity as the prime, because that is what puts the
    # request back in the flushed scope: without the tenant header this would be
    # a different scope's first request and would miss for the wrong reason.
    $after = Send-Chat -Base $base -BodyFile $lpBody -Headers $probeHeaders -Tag 'm2-flush-after'
    Assert-Equal 'after a flush the same request is cold again' 'miss' $after.Cache
    Assert-Equal 'the cold request was answered by the upstream' 'local' $after.Name

    # The route is POST-only. The GET below must not mutate anything, and the
    # status it gets is worth recording honestly: the proxy's catch-all pattern
    # is registered on "/", and Go's ServeMux only reports 405 when NOTHING else
    # matches the path. Here the catch-all does match, so the proxy answers with
    # its own 404 "unsupported path" instead -- a 405 would be the neater answer,
    # but what matters, and what is asserted, is that a GET never flushes.
    $getFlush = Invoke-Http -Url "$base/admin/cache/flush?scope=$encScope" -Method 'GET' -Tag 'm2-flush-get'
    Assert-Equal 'GET /admin/cache/flush is rejected, not executed' 404 $getFlush.Status
    $getDoc = $null
    try { $getDoc = $getFlush.Body | ConvertFrom-Json } catch { }
    Assert-True 'the rejected GET returns an error envelope, not a flush report' `
        ($null -ne $getDoc -and $null -eq $getDoc.flushed) `
        "body: $($getFlush.Body.Substring(0, [Math]::Min(200, $getFlush.Body.Length)))"

    # -----------------------------------------------------------------------
    # 12. /stats and /metrics
    # -----------------------------------------------------------------------
    Write-Section '12. /stats and /metrics report the cache'
    $stats = Invoke-Http -Url "$base/stats" -Tag 'm2-stats'
    Assert-Equal 'GET /stats answers 200' 200 $stats.Status
    $statsDoc = $null
    try { $statsDoc = $stats.Body | ConvertFrom-Json } catch { }
    Assert-True '/stats carries a cache block' ($null -ne $statsDoc -and $null -ne $statsDoc.cache) `
        "body: $($stats.Body.Substring(0, [Math]::Min(200, $stats.Body.Length)))"
    if ($null -ne $statsDoc -and $null -ne $statsDoc.cache) {
        Assert-Equal '/stats says the cache is enabled' $true $statsDoc.cache.enabled
        Assert-True '/stats reports a hit ratio' ($null -ne $statsDoc.cache.hit_ratio) `
            "hit_ratio=$($statsDoc.cache.hit_ratio)"
        # saved_tokens is nested because "saved" only means anything split into
        # prompt and completion: prompt tokens are what a replay avoids paying,
        # and the two have different prices.
        Assert-True '/stats reports saved tokens as a prompt/completion split' `
            ($null -ne $statsDoc.cache.saved_tokens -and $null -ne $statsDoc.cache.saved_tokens.total) `
            "saved_tokens=$($statsDoc.cache.saved_tokens | ConvertTo-Json -Compress)"
        if ($null -ne $statsDoc.cache.saved_tokens) {
            Assert-True 'the saved-token total is positive after all these hits' `
                ([double]$statsDoc.cache.saved_tokens.total -gt 0) `
                "total=$($statsDoc.cache.saved_tokens.total)"
        }
    }

    $metrics = Invoke-Curl @('-s', "$base/metrics")
    foreach ($family in @('infergate_cache_lookups_total', 'infergate_cache_hits_total',
            'infergate_cache_misses_total', 'infergate_cache_stores_total',
            'infergate_cache_saved_tokens_total', 'infergate_cache_entries')) {
        Assert-Contains "/metrics exports $family" $metrics $family
    }
    Assert-True 'the hit counter is split by kind, so exact and semantic hits cannot hide in one number' `
        ((Get-SeriesValue -Text $metrics -Series 'infergate_cache_hits_total' -Labels 'kind="exact"') -ge 1) `
        'no exact hits recorded'
    Assert-True 'the semantic hit was counted as its own kind' `
        ((Get-SeriesValue -Text $metrics -Series 'infergate_cache_hits_total' -Labels 'kind="semantic"') -ge 1) `
        'no semantic hits recorded'
    Assert-True 'saved prompt tokens are counted separately from saved completion tokens' `
        ((Get-SeriesValue -Text $metrics -Series 'infergate_cache_saved_tokens_total' -Labels 'kind="completion"') -ge 1) `
        'no saved completion tokens recorded'

    # -----------------------------------------------------------------------
    # 13. The Redis store
    # -----------------------------------------------------------------------
    Write-Section '13. The same policy behind the Redis store'
    # The Redis store is not a second cache implementation: it is the same policy
    # behind a different Store. So the sequence that proves it is the same
    # sequence as section 4/5, and the response headers must be identical. What
    # changes is the admin surface, which must name the store and list scopes
    # from Redis rather than from process memory.
    #
    # The two gateway configurations name different upstreams on purpose. The
    # redis gateway gets its own mock on :$RedisUpstreamPort so that the two
    # stacks never share a backend, and so a failure here cannot be explained by
    # something that happened to the memory gateway's upstream in section 6.
    $mockRedisProc = Start-MockUpstream -Port $RedisUpstreamPort -Name 'local' -LogName 'm2-mock-redis.log'
    Assert-Equal 'the redis upstream answers /healthz' '200' (Wait-Healthy -Port $RedisUpstreamPort)

    $miniLog = Join-Path $tmpDir 'm2-miniredis.log'
    [System.IO.File]::WriteAllText($miniLog, '', $utf8NoBom)
    $miniProc = Start-Process -FilePath (Join-Path $binDir 'miniredis.exe') `
        -ArgumentList @('-listen', ":$MiniredisPort") `
        -RedirectStandardOutput $miniLog -RedirectStandardError (Join-Path $tmpDir 'm2-miniredis.err') `
        -PassThru -WindowStyle Hidden
    Write-Host "  miniredis.exe pid=$($miniProc.Id) listening :$MiniredisPort"
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $deadline -and -not (Test-PortOpen -Port $MiniredisPort)) { Start-Sleep -Milliseconds 100 }
    Assert-True "miniredis accepts connections on :$MiniredisPort" (Test-PortOpen -Port $MiniredisPort)

    $redisGwProc = Start-Gateway -Config $redisCfg -Log $redisGwLog -ErrName 'm2-redis-gateway.err'
    $redisBase = "http://127.0.0.1:$RedisGatewayPort"
    Assert-Equal 'the redis-backed gateway answers /readyz' '200' (Wait-Healthy -Port $RedisGatewayPort -Path '/readyz')

    $rAdmin = Get-CacheAdmin -Base $redisBase
    Assert-True 'GET /admin/cache answers on the redis gateway' ($null -ne $rAdmin)
    if ($null -ne $rAdmin) {
        Assert-Equal 'the running store is redis' 'redis' $rAdmin.store
    }
    # The scope table is asserted AFTER the requests below, not here. A scope only
    # exists once something has been stored, so the table is legitimately empty on
    # a freshly started gateway -- asserting it here would be asserting that the
    # cache has already been used, which is the next section's job.

    $rPrompt = Get-UniquePrompt 'redis store'
    $rPrompt2 = 'please ' + $rPrompt
    $rdBody = New-BodyFile 'm2-redis.json' (New-ChatBody $rPrompt)
    $rdBody2 = New-BodyFile 'm2-redis-para.json' (New-ChatBody $rPrompt2)

    $d1 = Send-Chat -Base $redisBase -BodyFile $rdBody -Tag 'm2-redis-1'
    Assert-Equal 'redis: the first request answers 200' 200 $d1.Status
    Assert-Equal 'redis: the first request is a MISS' 'miss' $d1.Cache
    Assert-Equal 'redis: the miss was served by the upstream' 'local' $d1.Name

    $d2 = Send-Chat -Base $redisBase -BodyFile $rdBody -Tag 'm2-redis-2'
    Assert-Equal 'redis: an identical request is an EXACT hit' 'hit-exact' $d2.Cache
    Assert-Equal 'redis: the hit is attributed to the cache' 'cache' $d2.Name
    Assert-True 'redis: the replayed body is byte-identical' ($d2.Body -ceq $d1.Body)
    Assert-Number 'redis: X-InferGate-Cache-Age is a number of milliseconds' $d2.Age -Min 0

    # The semantic half is what proves the redis store carries the embedding and
    # not just the bytes: the vector has to survive JSON into Redis and back.
    $d3 = Send-Chat -Base $redisBase -BodyFile $rdBody2 -Tag 'm2-redis-3'
    Assert-Equal 'redis: a paraphrase is a SEMANTIC hit' 'hit-semantic' $d3.Cache
    Assert-True 'redis: the semantic hit replays the stored answer' ($d3.Body -ceq $d1.Body)

    # Now that three answers have been stored, the scope table must be readable
    # back out of Redis. This is the assertion that proves the gateway is talking
    # to a real external store and not to a process-local map wearing the name
    # "redis": a memory store would answer /admin/cache identically, but only
    # Redis has to round-trip the scope bookkeeping through RESP.
    $rAdmin2 = Get-CacheAdmin -Base $redisBase
    Assert-True 'the redis store lists its scopes' `
        ($null -ne $rAdmin2 -and $null -ne $rAdmin2.scopes -and @($rAdmin2.scopes.PSObject.Properties).Count -ge 1) `
        "scopes=$(if ($rAdmin2 -and $rAdmin2.scopes) { (@($rAdmin2.scopes.PSObject.Properties | ForEach-Object { "$($_.Name)=$($_.Value)" })) -join ',' } else { 'none' })"
    $rScopeNames = ''
    if ($null -ne $rAdmin2 -and $null -ne $rAdmin2.scopes) {
        $rScopeNames = (@($rAdmin2.scopes.PSObject.Properties | ForEach-Object { $_.Name }) -join ' ')
    }
    Assert-True 'the listed scope belongs to the model under test' `
        ($rScopeNames.Contains('mock-gpt/')) "scopes=$rScopeNames"

    # A dead cache must never be a dead gateway. Kill the store under the running
    # gateway and require an answer anyway: the cache is an optimisation, so its
    # outage may cost latency and a marked header, but never a 5xx.
    Write-Host "  killing miniredis (pid=$($miniProc.Id)) while the gateway keeps running" -ForegroundColor DarkYellow
    Stop-Process -Id $miniProc.Id -Force -ErrorAction SilentlyContinue
    $miniProc = $null
    Start-Sleep -Milliseconds 400

    $dead = Send-Chat -Base $redisBase -BodyFile $rdBody -Tag 'm2-redis-dead'
    Assert-Equal 'with the cache store DEAD the gateway still answers 200' 200 $dead.Status
    # The gateway reports `error` when the store cannot be reached and `miss`
    # when it can be reached but has nothing; a dead miniredis is the former, and
    # either is acceptable here -- what is forbidden is a 5xx or a silent claim
    # that the cache worked.
    Assert-TrueOr 'a dead store is reported as error or miss, never as a hit' `
        ($dead.Cache -eq 'error' -or $dead.Cache -eq 'miss') `
        "X-InferGate-Cache='$($dead.Cache)' (observed behaviour)"
    Write-Host "  observed X-InferGate-Cache with a dead store: '$($dead.Cache)'" -ForegroundColor DarkGray
    Assert-Equal 'the request was answered by the upstream' 'local' $dead.Name

    # -----------------------------------------------------------------------
    # 14. The gateway log tells the same story
    # -----------------------------------------------------------------------
    Write-Section '14. The gateway log records the cache decision'
    # The log is the only surface an operator has after the fact, and the cache
    # verdict is folded into the single msg=request line rather than logged
    # separately: grepping a request id must show what the cache decided AND
    # which backend, if any, answered.
    $logText = Read-OpenLog $gwLog
    Assert-Contains 'logs record the listen event' $logText 'infergate listening'

    $hitLines = @($logText -split "`n" | Where-Object {
        $_ -match 'msg=request' -and $_ -match 'cache=hit-exact'
    })
    Assert-True 'a cache hit is logged as one request line carrying cache=hit-exact' `
        ($hitLines.Count -ge 1) `
        "no such line; lines mentioning cache=: $(@($logText -split "`n" | Where-Object { $_ -match 'cache=' }).Count)"

    # A hit must also be attributed to the cache in the same line: a line that
    # said cache=hit-exact and upstream=local would mean the request was counted
    # against a backend it never called.
    $cacheUpstreamLines = @($logText -split "`n" | Where-Object {
        $_ -match 'msg=request' -and $_ -match 'cache=hit-exact' -and $_ -match '\supstream=cache\b'
    })
    Assert-True 'the hit line attributes the request to the cache, not to a backend' `
        ($cacheUpstreamLines.Count -ge 1) `
        'no hit line named upstream=cache'
    Assert-Contains 'logs record the semantic verdict too' $logText 'cache=hit-semantic'
    Assert-Contains 'logs record skipped prompts with a reason' $logText 'cache=skip'
    Assert-Contains 'logs record why a prompt was skipped' $logText 'cache_reason='
    Write-Host "        gateway log: $gwLog" -ForegroundColor DarkGray
}
finally {
    # -----------------------------------------------------------------------
    # 15. Teardown
    # -----------------------------------------------------------------------
    Write-Section '15. Teardown'
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
                @('gateway        ', $GatewayPort, $gwProc),
                @('redis gateway  ', $RedisGatewayPort, $redisGwProc),
                @('mock upstream  ', $UpstreamPort, $mockProc),
                @('mock upstream  ', $RedisUpstreamPort, $mockRedisProc))) {
            if ($entry[2]) { Write-Host "  pid=$($entry[2].Id)  $($entry[0]) :$($entry[1])" -ForegroundColor DarkYellow }
        }
        if ($miniProc) { Write-Host "  pid=$($miniProc.Id)  miniredis       :$MiniredisPort" -ForegroundColor DarkYellow }
        Write-Host '  none of these are stopped by this script when -KeepRunning is set.' -ForegroundColor DarkYellow
    }
    else {
        foreach ($p in @($gwProc, $redisGwProc, $mockProc, $mockRedisProc, $miniProc)) {
            if ($p -and -not $p.HasExited) {
                Write-Host "  stopping pid=$($p.Id)"
                Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
            }
        }
    }
}

Write-Host ''
Write-Host ("=" * 72) -ForegroundColor DarkGray
if ($script:failed -eq 0) {
    Write-Host "  RESULT: $($script:passed)/$($script:passed) assertions passed" -ForegroundColor Green
    Write-Host '  OK: M2 semantic cache verified end to end with curl' -ForegroundColor Green
    exit 0
}
else {
    Write-Host "  RESULT: $($script:passed) passed, $($script:failed) FAILED" -ForegroundColor Red
    exit 1
}
