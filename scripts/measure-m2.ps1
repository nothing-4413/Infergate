<#
    measure-m2.ps1 -- the M2 numbers, MEASURED rather than asserted.

    verify-m2.ps1/cmd/verify-m2 answers "does the semantic cache behave
    correctly".  This script answers the other half of the milestone: "what does
    it actually buy".  Four measurements, all against real processes on the
    loopback:

      A. HIT RATE AND SAVED TOKENS on the LABELLED corpus in
         internal/evalset/evalset.go (26 hand-ruled pairs).  Per should-hit
         pair: A (miss), A again (exact hit), B (semantic hit) -- then the
         near-miss and unrelated pairs, whose B must never be answered from the
         cache.  The false-hit count is reported as a first-class number: a
         wrong answer is worse than a miss.  Saved prompt/completion tokens are
         read from GET /admin/cache, and the USD saving is computed with the
         price book the same config carries.

      B. LATENCY AND THROUGHPUT, cache ON vs OFF.  cmd/loadtest against both
         gateways, three INTERLEAVED rounds (this host drifts round to round, so
         a single pass is not evidence), both workloads, c=8 and c=32.  HONEST
         FRAME: the upstream is the in-repo mock, which is local and free, so
         the throughput delta is mostly gateway overhead removed, not a
         provider's latency.  What the cache actually buys is (a) the
         cached-path P95 and (b) that a hit performs no upstream call at all --
         the mock's own request log is counted to prove that, and it is a
         measured fact rather than an inference from the gateway's counters.

      C. THE CACHE AS A SHIELD.  Prime the labelled prompts, kill the upstream
         PROCESS, then replay exact + paraphrase.  "N requests served while the
         provider was hard down" is the number that justifies the feature.  A
         non-primed control prompt must fail, which is what proves the outage
         was real.

      D. THRESHOLD SWEEP.  cmd/measure-m2 gives the hit-rate / false-hit curve
         for the shipped embedder, so the configured threshold of 0.86 carries
         its evidence.

    Nothing in the repository is modified: the configs this run needs are
    instantiated into tmp/ by literal substitution, exactly like measure-m1.ps1.

    Run it with Windows PowerShell 5.1:

        powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m2.ps1
#>
[CmdletBinding()]
param(
    [int]$CacheOnPort = 18290,
    [int]$CacheOffPort = 18291,
    [int]$MockPort = 19510,
    [int]$MiniRedisPort = 19389,
    [int]$Rounds = 3,
    [int]$Requests = 800,
    [int]$Warmup = 100,
    [string]$Concurrency = '8,32',
    [double]$Threshold = 0.86,
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
$script:procs = @()
$script:mockLog = ''
$script:assertFails = @()
$script:runStamp = (Get-Date -Format 'HHmmss') + '-' + $PID

if (-not (Test-Path $tmpDir)) { New-Item -ItemType Directory -Path $tmpDir | Out-Null }
if (-not (Test-Path $binDir)) { New-Item -ItemType Directory -Path $binDir | Out-Null }
if (-not (Test-Path $baselineDir)) { New-Item -ItemType Directory -Path $baselineDir | Out-Null }

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

function Write-Section([string]$Title) {
    Write-Host ''
    Write-Host ("=" * 72) -ForegroundColor DarkGray
    Write-Host "  $Title" -ForegroundColor Cyan
    Write-Host ("=" * 72) -ForegroundColor DarkGray
}

function Invoke-Curl {
    param([string[]]$Arguments)
    # stdout only: Windows PowerShell 5.1 turns native stderr into a terminating
    # NativeCommandError under the default ErrorActionPreference.
    return (& curl.exe @Arguments 2>$null | Out-String)
}

function Read-Text {
    <# FileShare.ReadWrite is mandatory: every file this script reads may still be
       held open for writing (a redirected log of a live child process).  Both
       Get-Content and the path-based StreamReader constructor open with
       FileShare.Read and fail with "being used by another process". #>
    param([string]$Path)
    if ([string]::IsNullOrEmpty($Path)) { return '' }
    if (-not (Test-Path $Path)) { return '' }
    $fs = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    $reader = New-Object System.IO.StreamReader($fs, $script:utf8NoBom)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose(); $fs.Dispose() }
}

function Get-Header {
    param([string]$Headers, [string]$Name)
    $m = [regex]::Match($Headers, "(?im)^$([regex]::Escape($Name)):\s*(.+?)\s*$")
    if ($m.Success) { return $m.Groups[1].Value }
    return ''
}

function Write-JsonFile {
    param([string]$Path, $Value)
    $json = ($Value | ConvertTo-Json -Depth 12)
    [System.IO.File]::WriteAllText($Path, $json + "`n", $script:utf8NoBom)
}

function Get-JsonText {
    param([string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text)) { return $null }
    try { return ($Text | ConvertFrom-Json) } catch { return $null }
}

function Get-JsonUrl {
    param([string]$Url)
    return (Get-JsonText (Invoke-Curl @('-s', '-m', '30', $Url)))
}

function Write-Config {
    <# Instantiates a shipped config onto the ports this run owns. #>
    param([string]$Source, [string]$Target, [hashtable]$Replace)
    $text = Read-Text (Join-Path $repo $Source)
    foreach ($k in $Replace.Keys) { $text = $text.Replace($k, $Replace[$k]) }
    [System.IO.File]::WriteAllText($Target, $text, $utf8NoBom)
    Write-Host "  wrote $Target"
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
    $sorted = @($Values | Sort-Object)
    if ($sorted.Count -eq 0) { return 0.0 }
    return $sorted[[int][Math]::Floor($sorted.Count / 2)]
}

function Convert-GoDurationToMs {
    param([string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text)) { return $null }
    # Go prints microseconds as U+00B5 + "s".  This file is deliberately pure
    # ASCII: Windows PowerShell 5.1 reads a BOM-less script as ANSI, so a
    # literal micro sign here would arrive as two characters and never match.
    $t = $Text.Trim().Replace([string][char]0x00B5, 'u')
    $m = [regex]::Match($t, '^([0-9]+(?:\.[0-9]+)?)(ns|us|ms|s|m|h)$')
    if (-not $m.Success) { return $null }
    $v = [double]$m.Groups[1].Value
    switch ($m.Groups[2].Value) {
        'ns' { return $v / 1e6 }
        'us' { return $v / 1e3 }
        'ms' { return $v }
        's' { return $v * 1e3 }
        'm' { return $v * 6e4 }
        'h' { return $v * 3.6e6 }
    }
    return $null
}

function Get-LatencyPeek {
    param($Stats)
    $out = [ordered]@{ source = 'GET /stats'; count = $null; p50 = $null; p90 = $null; p95 = $null; p99 = $null; max = $null; p95_ms = $null; p99_ms = $null }
    if ($null -eq $Stats -or $null -eq $Stats.latency) { return $out }
    foreach ($k in @('count', 'p50', 'p90', 'p95', 'p99', 'max')) {
        if ($null -ne $Stats.latency.$k) { $out[$k] = [string]$Stats.latency.$k }
    }
    $out['p95_ms'] = Convert-GoDurationToMs $out['p95']
    $out['p99_ms'] = Convert-GoDurationToMs $out['p99']
    return $out
}

function Assert-That {
    <# A measurement claim is only worth shipping if it is asserted: a failure
       lands in $script:assertFails, the JSON records the boolean, and the
       script exits non-zero. #>
    param([bool]$Ok, [string]$Name, [string]$Detail = '')
    $suffix = ''
    if ($Detail -ne '') { $suffix = ' - ' + $Detail }
    if ($Ok) {
        Write-Host ("  ASSERT PASS  {0}{1}" -f $Name, $suffix) -ForegroundColor Green
    }
    else {
        [void]$script:assertFails.Add($Name)
        Write-Host ("  ASSERT FAIL  {0}{1}" -f $Name, $suffix) -ForegroundColor Red
    }
    return $Ok
}

function Assert-PortsFree {
    <# A measurement must not share a port with a leftover or a concurrent
       process: a stale server would answer the probes and silently contaminate
       every number below it. #>
    param([int[]]$Ports)
    $busy = @()
    foreach ($p in $Ports) {
        $c = Get-NetTCPConnection -State Listen -LocalPort $p -ErrorAction SilentlyContinue
        if ($c) {
            $ownerPid = ($c | Select-Object -First 1).OwningProcess
            $proc = Get-Process -Id $ownerPid -ErrorAction SilentlyContinue
            $busy += ("{0} (pid {1} {2})" -f $p, $ownerPid, $proc.ProcessName)
        }
    }
    if ($busy.Count -gt 0) {
        throw ("port(s) already listening: " + ($busy -join ', ') + " - stop that process or pass different -MockPort/-CacheOnPort/-CacheOffPort")
    }
}

function Start-Mock {
    param([int]$Port, [string]$Name, [string]$Tag)
    $log = Join-Path $tmpDir "m2-$Tag-$script:runStamp.log"
    try { [System.IO.File]::WriteAllText($log, '', $utf8NoBom) } catch { }
    $p = Start-Process -FilePath (Join-Path $binDir 'mockupstream.exe') `
        -ArgumentList @('-listen', ":$Port", '-name', $Name, '-token-delay', '0ms') `
        -RedirectStandardOutput $log -RedirectStandardError (Join-Path $tmpDir "m2-$Tag-$script:runStamp.err") -PassThru -WindowStyle Hidden
    $script:procs = @($script:procs + $p)
    $script:mockLog = $log
    return $p
}

function Start-Gateway {
    param([int]$Port, [string]$ConfigPath, [string]$Tag, [int]$Attempts = 3)
    for ($i = 1; $i -le $Attempts; $i++) {
        $log = Join-Path $tmpDir "m2-$Tag-$script:runStamp.log"
        try { [System.IO.File]::WriteAllText($log, '', $utf8NoBom) } catch { }
        $p = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') -ArgumentList @('-config', $ConfigPath) `
            -RedirectStandardOutput $log -RedirectStandardError (Join-Path $tmpDir "m2-$Tag-$script:runStamp.err") -PassThru -WindowStyle Hidden
        $script:procs = @($script:procs + $p)
        $code = Wait-Healthy -Port $Port -Path '/readyz' -Seconds 15
        if ($code -eq '200') { return $p }
        Write-Host "  gateway $Tag not ready (http $code, attempt $i)" -ForegroundColor Yellow
        Stop-Proc $p $Tag
        Start-Sleep -Milliseconds 800
    }
    throw "gateway $Tag on port $Port never became ready"
}

function Stop-Proc {
    param($Proc, [string]$Tag = '')
    if ($null -eq $Proc) { return }
    try { if (-not $Proc.HasExited) { Stop-Process -Id $Proc.Id -Force } } catch { }
    Start-Sleep -Milliseconds 350
}

function Get-MockRequestCount {
    <# The upstream's own count of the requests it received: one slog line per
       chat request.  A cache hit must leave this number alone, which is what
       makes "the cache answered" a measured claim rather than the gateway's
       opinion of itself. #>
    if ([string]::IsNullOrEmpty($script:mockLog)) { return -1 }
    if (-not (Test-Path $script:mockLog)) { return -1 }
    for ($i = 1; $i -le 4; $i++) {
        try { return ([regex]::Matches((Read-Text $script:mockLog), 'msg="chat request"')).Count }
        catch { Start-Sleep -Milliseconds 200 }
    }
    return -1
}

function New-ChatBody {
    param([string]$Tag, [string]$Prompt)
    $path = Join-Path $tmpDir "m2-body-$Tag.json"
    $obj = [ordered]@{
        model    = 'mock-gpt'
        messages = @([ordered]@{ role = 'user'; content = $Prompt })
    }
    [System.IO.File]::WriteAllText($path, ($obj | ConvertTo-Json -Compress -Depth 5), $script:utf8NoBom)
    return $path
}

function Send-Chat {
    param([int]$Port, [string]$BodyPath)
    $hdr = Join-Path $tmpDir 'm2-last-headers.txt'
    $out = Join-Path $tmpDir 'm2-last-body.json'
    Remove-Item $hdr, $out -ErrorAction SilentlyContinue
    $code = (Invoke-Curl @('-s', '-m', '60', '-o', $out, '-D', $hdr, '-w', '%{http_code}', '-X', 'POST',
            '-H', 'content-type: application/json', '--data-binary', "@$BodyPath",
            "http://127.0.0.1:$Port/v1/chat/completions")).Trim()
    $status = 0
    if ($code -match '^\d{3}$') { $status = [int]$code }
    $h = Read-Text $hdr
    return [pscustomobject]@{
        Status   = $status
        Cache    = (Get-Header $h 'X-InferGate-Cache')
        Upstream = (Get-Header $h 'X-InferGate-Upstream-Name')
        Body     = (Read-Text $out)
    }
}

function Invoke-Flush {
    param([int]$Port)
    return (Invoke-Curl @('-s', '-m', '30', '-X', 'POST', "http://127.0.0.1:$Port/admin/cache/flush")).Trim()
}

function Get-CacheAdmin {
    param([int]$Port)
    return (Get-JsonUrl "http://127.0.0.1:$Port/admin/cache")
}

function Invoke-Load {
    <# One cmd/loadtest -url pass: both workloads (non-stream, stream) x every concurrency in -c. #>
    param([int]$Port, [string]$OutFile, [string]$Tag)
    $stdout = Join-Path $tmpDir "m2-loadtest-$Tag.txt"
    & (Join-Path $binDir 'loadtest.exe') -url "http://127.0.0.1:$Port" -c $Concurrency -n $Requests -warmup $Warmup -out $OutFile 2>$null |
        Out-File -FilePath $stdout -Encoding utf8
    if ($LASTEXITCODE -ne 0) { throw "loadtest ($Tag) exited with code $LASTEXITCODE" }
    if (-not (Test-Path $OutFile)) { throw "loadtest ($Tag) wrote no result file at $OutFile" }
}

function Add-LoadResults {
    <# Folds one loadtest result file into the per-cell round lists. #>
    param([hashtable]$Cells, [string]$Scenario, [string]$Path)
    $parsed = Get-JsonText (Read-Text $Path)
    if ($null -eq $parsed) { throw "no load results parsed from $Path" }
    $count = 0
    foreach ($e in @($parsed)) {
        $workload = if ($e.stream) { 'stream' } else { 'non-stream' }
        $key = "$Scenario|$workload|$([int]$e.concurrency)"
        if (-not $Cells.ContainsKey($key)) { $Cells[$key] = New-Object System.Collections.ArrayList }
        [void]$Cells[$key].Add($e)
        $count++
    }
    if ($count -eq 0) { throw "empty load result file $Path" }
    return $count
}

function Test-Hit([string]$Cache) { return ($Cache -eq 'hit-exact' -or $Cache -eq 'hit-semantic') }

function Get-CacheStatsDelta {
    param($Before, $After)
    $fields = @('lookups', 'hits', 'exact_hits', 'semantic_hits', 'misses', 'stores',
        'store_errors', 'lookup_errors', 'stale_evictions', 'collapsed',
        'saved_prompt_tokens', 'saved_completion_tokens')
    $d = [ordered]@{}
    foreach ($f in $fields) {
        $b = 0; $a = 0
        if ($null -ne $Before -and $null -ne $Before.stats) { $b = [int64]$Before.stats.$f }
        if ($null -ne $After -and $null -ne $After.stats) { $a = [int64]$After.stats.$f }
        $d[$f] = $a - $b
    }
    return $d
}

# ---------------------------------------------------------------------------
# The labelled corpus, read out of the Go source
# ---------------------------------------------------------------------------

function Get-Corpus {
    $path = Join-Path $repo 'internal\evalset\evalset.go'
    $src = Read-Text $path
    $pattern = '\{ID:\s*"([^"]+)",\s*Kind:\s*Kind(\w+),\s*A:\s*"((?:[^"\\]|\\.)*)",\s*B:\s*"((?:[^"\\]|\\.)*)",'
    $ms = [regex]::Matches($src, $pattern)
    $cases = New-Object System.Collections.ArrayList
    foreach ($m in $ms) {
        $rawKind = $m.Groups[2].Value
        $kind = switch ($rawKind) {
            'Identical' { 'identical' }
            'Paraphrase' { 'paraphrase' }
            'NearMiss' { 'near-miss' }
            'Unrelated' { 'unrelated' }
            default { $rawKind.ToLower() }
        }
        [void]$cases.Add([pscustomobject]@{
                id         = $m.Groups[1].Value
                kind       = $kind
                a          = $m.Groups[3].Value.Replace('\"', '"')
                b          = $m.Groups[4].Value.Replace('\"', '"')
                should_hit = ($kind -eq 'identical' -or $kind -eq 'paraphrase')
            })
    }
    return $cases
}

function Invoke-CorpusRun {
    <# Per should-hit pair: A, A, B.  Per should-not pair: A, B. #>
    param([int]$Port, $Cases, [string]$Tag)
    $records = New-Object System.Collections.ArrayList
    foreach ($c in $Cases) {
        $bodyA = New-ChatBody "$Tag-$($c.id)-a" $c.a
        $bodyB = New-ChatBody "$Tag-$($c.id)-b" $c.b
        if ($c.should_hit) {
            $r = Send-Chat $Port $bodyA
            [void]$records.Add([pscustomobject]@{ id = $c.id; kind = $c.kind; letter = 'A1'; role = 'prime'; status = $r.Status; cache = $r.Cache })
            $r = Send-Chat $Port $bodyA
            [void]$records.Add([pscustomobject]@{ id = $c.id; kind = $c.kind; letter = 'A2'; role = 'repeat'; status = $r.Status; cache = $r.Cache })
            $r = Send-Chat $Port $bodyB
            [void]$records.Add([pscustomobject]@{ id = $c.id; kind = $c.kind; letter = 'B'; role = 'paraphrase'; status = $r.Status; cache = $r.Cache })
        }
        else {
            $r = Send-Chat $Port $bodyA
            [void]$records.Add([pscustomobject]@{ id = $c.id; kind = $c.kind; letter = 'A1'; role = 'prime'; status = $r.Status; cache = $r.Cache })
            $r = Send-Chat $Port $bodyB
            [void]$records.Add([pscustomobject]@{ id = $c.id; kind = $c.kind; letter = 'B'; role = 'must-not-hit'; status = $r.Status; cache = $r.Cache })
        }
    }
    return $records
}

function Get-CorpusRunSummary {
    param($Records, $StatsDelta, [int]$MockCalls, [string]$Label, [int]$MinPromptChars, [string]$Store, [double]$Thr, [string]$Embedding)
    $should = @($Records | Where-Object { $_.kind -eq 'identical' -or $_.kind -eq 'paraphrase' })
    $shouldNot = @($Records | Where-Object { $_.kind -eq 'near-miss' -or $_.kind -eq 'unrelated' })
    $exact = @($should | Where-Object { $_.cache -eq 'hit-exact' }).Count
    $semantic = @($should | Where-Object { $_.cache -eq 'hit-semantic' }).Count
    $misses = @($should | Where-Object { $_.cache -eq 'miss' }).Count
    $skips = @($should | Where-Object { $_.cache -eq 'skip' }).Count
    $other = $should.Count - $exact - $semantic - $misses - $skips
    $matchedIds = @($should | Where-Object { (Test-Hit $_.cache) } | ForEach-Object { $_.id } | Sort-Object -Unique)
    $falseRecords = @($shouldNot | Where-Object { $_.role -eq 'must-not-hit' -and (Test-Hit $_.cache) })
    $byKind = [ordered]@{}
    foreach ($k in @('identical', 'paraphrase')) {
        $g = @($should | Where-Object { $_.kind -eq $k })
        $byKind[$k] = [ordered]@{
            requests      = $g.Count
            exact_hits    = @($g | Where-Object { $_.cache -eq 'hit-exact' }).Count
            semantic_hits = @($g | Where-Object { $_.cache -eq 'hit-semantic' }).Count
            misses        = @($g | Where-Object { $_.cache -eq 'miss' }).Count
            skips         = @($g | Where-Object { $_.cache -eq 'skip' }).Count
        }
    }
    foreach ($k in @('near-miss', 'unrelated')) {
        $g = @($shouldNot | Where-Object { $_.kind -eq $k })
        $byKind[$k] = [ordered]@{
            requests      = $g.Count
            false_hits    = @($g | Where-Object { $_.role -eq 'must-not-hit' -and (Test-Hit $_.cache) }).Count
            prime_exact   = @($g | Where-Object { $_.role -eq 'prime' -and $_.cache -eq 'hit-exact' }).Count
            prime_misses  = @($g | Where-Object { $_.role -eq 'prime' -and $_.cache -eq 'miss' }).Count
        }
    }
    $hitRate = 0.0
    if ($should.Count -gt 0) { $hitRate = [Math]::Round(($exact + $semantic) / [double]$should.Count, 4) }
    $pairHitRate = [Math]::Round($matchedIds.Count / 13.0, 4)
    return [ordered]@{
        label                     = $Label
        store                     = $Store
        embedding                 = $Embedding
        threshold                 = $Thr
        min_prompt_chars          = $MinPromptChars
        requests                  = $should.Count
        exact_hits                = $exact
        semantic_hits             = $semantic
        hits                      = $exact + $semantic
        misses                    = $misses
        skips                     = $skips
        non_cache_outcomes        = $other
        hit_rate                  = $hitRate
        hit_rate_definition       = 'cache hits (exact + semantic) / requests sent to should-hit pairs (A, A, B per pair)'
        pairs_matched             = $matchedIds.Count
        pairs_total               = 13
        pair_hit_rate             = $pairHitRate
        matched_ids               = $matchedIds
        should_not_hit_requests   = $shouldNot.Count
        false_hits                = $falseRecords.Count
        false_hit_ids             = @($falseRecords | ForEach-Object { $_.id })
        by_kind                   = $byKind
        admin_cache_delta         = $StatsDelta
        upstream_calls_seen_by_mock = $MockCalls
        upstream_calls_note       = 'counted in the mock upstream process log (msg="chat request"); a cache hit performs no upstream call, so this must equal the miss count'
    }
}

function Get-UncachedUsage {
    <# Every should-hit prompt A and B, straight from the upstream with the cache disabled: the ground truth for "what this request would have cost". #>
    param([int]$Port, $Cases)
    $usages = [ordered]@{}
    foreach ($c in $Cases) {
        if (-not $c.should_hit) { continue }
        $bodyA = New-ChatBody "ref-$($c.id)-a" $c.a
        $bodyB = New-ChatBody "ref-$($c.id)-b" $c.b
        $ra = Send-Chat $Port $bodyA
        $rb = Send-Chat $Port $bodyB
        $ja = Get-JsonText $ra.Body
        $jb = Get-JsonText $rb.Body
        $entry = [ordered]@{ a_prompt = 0; a_completion = 0; b_prompt = 0; b_completion = 0; a_status = $ra.Status; b_status = $rb.Status }
        if ($null -ne $ja -and $null -ne $ja.usage) {
            $entry['a_prompt'] = [int]$ja.usage.prompt_tokens
            $entry['a_completion'] = [int]$ja.usage.completion_tokens
        }
        if ($null -ne $jb -and $null -ne $jb.usage) {
            $entry['b_prompt'] = [int]$jb.usage.prompt_tokens
            $entry['b_completion'] = [int]$jb.usage.completion_tokens
        }
        $usages[$c.id] = $entry
    }
    return $usages
}

# ---------------------------------------------------------------------------
# Body
# ---------------------------------------------------------------------------

$summary = $null
$exitOk = $false
$startedAt = Get-Date

try {
    Write-Section 'M2 measurement - semantic cache (measure-m2.ps1)'

    # --- build ------------------------------------------------------------
    Write-Section 'Build'
    foreach ($spec in @(@('infergate.exe', './cmd/infergate'), @('mockupstream.exe', './cmd/mockupstream'), @('loadtest.exe', './cmd/loadtest'))) {
        $out = Join-Path $binDir $spec[0]
        & $goShim build -o $out $spec[1]
        if ($LASTEXITCODE -ne 0) { throw "go build $($spec[1]) failed with exit code $LASTEXITCODE" }
        Write-Host "  built $out"
    }

    # --- corpus -----------------------------------------------------------
    Write-Section 'Corpus (internal/evalset/evalset.go)'
    $cases = @(Get-Corpus)
    $shouldHit = @($cases | Where-Object { $_.should_hit })
    $shouldNot = @($cases | Where-Object { -not $_.should_hit })
    Write-Host ("  parsed {0} pairs: {1} should-hit, {2} should-not-hit" -f $cases.Count, $shouldHit.Count, $shouldNot.Count)
    if ($cases.Count -ne 26 -or $shouldHit.Count -ne 13 -or $shouldNot.Count -ne 13) {
        throw "corpus parse produced $($cases.Count) pairs ($($shouldHit.Count) should-hit); expected 26 (13/13) from internal/evalset/evalset.go"
    }
    $corpusDoc = [ordered]@{
        source        = 'internal/evalset/evalset.go'
        parsed_at     = (Get-Date).ToString('s')
        cases         = $cases.Count
        should_hit    = $shouldHit.Count
        should_not_hit = $shouldNot.Count
        kinds         = [ordered]@{
            identical  = @($cases | Where-Object { $_.kind -eq 'identical' }).Count
            paraphrase = @($cases | Where-Object { $_.kind -eq 'paraphrase' }).Count
            'near-miss' = @($cases | Where-Object { $_.kind -eq 'near-miss' }).Count
            unrelated  = @($cases | Where-Object { $_.kind -eq 'unrelated' }).Count
        }
        workload      = 'per should-hit pair: A (expect miss), A again (expect exact hit), B (expect semantic hit); per should-not-hit pair: A, then B (must not hit)'
        pairs         = @($cases | ForEach-Object { [ordered]@{ id = $_.id; kind = $_.kind; should_hit = $_.should_hit; a = $_.a; b = $_.b } })
    }
    Write-JsonFile (Join-Path $baselineDir 'm2-corpus.json') $corpusDoc

    # --- configs ----------------------------------------------------------
    Write-Section 'Configs (literal substitution into tmp/, nothing in configs/ is touched)'
    $cfgOn = Join-Path $tmpDir 'm2-cache-on.yaml'
    $cfgOnShipped = Join-Path $tmpDir 'm2-cache-on-shipped.yaml'
    $cfgOff = Join-Path $tmpDir 'm2-cache-off.yaml'
    $base = 'configs\cache-local.yaml'
    Write-Config -Source $base -Target $cfgOn -Replace @{
        'listen: ":8082"'                        = "listen: `":$CacheOnPort`""
        'base_url: "http://127.0.0.1:9200"'      = "base_url: `"http://127.0.0.1:$MockPort`""
        'level: "info"'                          = 'level: "error"'
        'min_prompt_chars: 12'                   = "min_prompt_chars: 1"
    }
    Write-Config -Source $base -Target $cfgOnShipped -Replace @{
        'listen: ":8082"'                        = "listen: `":$CacheOnPort`""
        'base_url: "http://127.0.0.1:9200"'      = "base_url: `"http://127.0.0.1:$MockPort`""
        'level: "info"'                          = 'level: "error"'
    }
    Write-Config -Source $base -Target $cfgOff -Replace @{
        'listen: ":8082"'                        = "listen: `":$CacheOffPort`""
        'base_url: "http://127.0.0.1:9200"'      = "base_url: `"http://127.0.0.1:$MockPort`""
        'level: "info"'                          = 'level: "error"'
        'enabled: true'                          = 'enabled: false'
    }
    $onText = Read-Text $cfgOn
    if ($onText -notmatch 'min_prompt_chars: 1') { throw 'the generated cache-ON config does not carry min_prompt_chars: 1' }
    if ((Read-Text $cfgOff) -notmatch 'enabled: false') { throw 'the generated cache-OFF config does not carry enabled: false' }

    # price book: read the unit and the mock-gpt prices out of the same config
    $pricingParsed = $false
    $priceIn = 1.0
    $priceOut = 3.0
    $pm = [regex]::Match($onText, 'mock-gpt:\s*\r?\n\s*in:\s*([0-9.]+)\s*\r?\n\s*out:\s*([0-9.]+)')
    if ($pm.Success) {
        $priceIn = [double]$pm.Groups[1].Value
        $priceOut = [double]$pm.Groups[2].Value
        $pricingParsed = $true
    }
    else { Write-Host '  WARNING: could not parse pricing.models.mock-gpt from the config; assuming in=1.0 out=3.0' -ForegroundColor Yellow }

    # --- processes --------------------------------------------------------
    Write-Section 'Processes'
    Assert-PortsFree -Ports @($MockPort, $CacheOnPort, $CacheOffPort)
    $mockProc = Start-Mock -Port $MockPort -Name 'primary' -Tag 'mock'
    $mockCode = Wait-Healthy -Port $MockPort -Path '/healthz' -Seconds 20
    if ($mockCode -ne '200') { throw "mock upstream on :$MockPort never became healthy (http $mockCode)" }
    Write-Host "  mock upstream    :$MockPort (pid $($mockProc.Id)) - token-delay 0ms"

    $gwOn = Start-Gateway -Port $CacheOnPort -ConfigPath $cfgOn -Tag 'cache-on'
    Write-Host "  gateway cache-ON :$CacheOnPort (pid $($gwOn.Id)) min_prompt_chars=1"
    $gwOff = Start-Gateway -Port $CacheOffPort -ConfigPath $cfgOff -Tag 'cache-off'
    Write-Host "  gateway cache-OFF:$CacheOffPort (pid $($gwOff.Id))"

    $adminOn = Get-CacheAdmin $CacheOnPort
    $effThreshold = $Threshold
    $storeName = 'memory'
    $embedding = 'hashing/512'
    if ($null -ne $adminOn) {
        if ($null -ne $adminOn.config) { $effThreshold = [double]$adminOn.config.threshold }
        if ($null -ne $adminOn.store) { $storeName = [string]$adminOn.store }
        if ($null -ne $adminOn.config -and $null -ne $adminOn.config.embedding) {
            $embedding = "$($adminOn.config.embedding.provider)/$($adminOn.config.embedding.dims)"
        }
    }
    Write-Host ("  effective threshold {0}, store {1}, embedding {2}" -f $effThreshold, $storeName, $embedding)

    # --- A: uncached ground truth ----------------------------------------
    Write-Section 'A1. Uncached token ground truth (cache OFF)'
    $refBefore = Get-CacheAdmin $CacheOffPort
    $usage = Get-UncachedUsage -Port $CacheOffPort -Cases $cases
    Write-Host ("  measured usage for {0} prompts (A and B of every should-hit pair) through the cache-OFF gateway" -f ($usage.Keys.Count * 2))
    $ptSum = 0; $ctSum = 0; $n = 0
    foreach ($k in $usage.Keys) {
        $ptSum += $usage[$k].a_prompt + $usage[$k].b_prompt
        $ctSum += $usage[$k].a_completion + $usage[$k].b_completion
        $n += 2
    }
    $avgPt = 0.0; $avgCt = 0.0
    if ($n -gt 0) { $avgPt = [Math]::Round($ptSum / [double]$n, 3); $avgCt = [Math]::Round($ctSum / [double]$n, 3) }
    Write-Host ("  mean per request: prompt_tokens {0}, completion_tokens {1}" -f $avgPt, $avgCt)

    # --- A: the labelled workload, cache ON -------------------------------
    Write-Section 'A2. Labelled workload, cache ON (clean cache)'
    [void](Invoke-Flush $CacheOnPort)
    $statsBefore = Get-CacheAdmin $CacheOnPort
    $mockBefore = Get-MockRequestCount
    $records = @(Invoke-CorpusRun -Port $CacheOnPort -Cases $cases -Tag 'primary')
    $mockAfter = Get-MockRequestCount
    $statsAfter = Get-CacheAdmin $CacheOnPort
    $delta = Get-CacheStatsDelta $statsBefore $statsAfter
    $mockCalls = -1
    if ($mockBefore -ge 0 -and $mockAfter -ge 0) { $mockCalls = $mockAfter - $mockBefore }
    $primary = Get-CorpusRunSummary -Records $records -StatsDelta $delta -MockCalls $mockCalls -Label 'full corpus (min_prompt_chars 1)' -MinPromptChars 1 -Store $storeName -Thr $effThreshold -Embedding $embedding
    Write-Host ("  requests {0} | exact {1} | semantic {2} | misses {3} | skips {4} | hit rate {5:P1} | pairs {6}/13 | false hits {7} | upstream calls {8}" -f `
            $primary.requests, $primary.exact_hits, $primary.semantic_hits, $primary.misses, $primary.skips, $primary.hit_rate, $primary.pairs_matched, $primary.false_hits, $primary.upstream_calls_seen_by_mock)

    # --- A: the same workload against the shipped min_prompt_chars --------
    Write-Section 'A3. Same workload with the shipped min_prompt_chars: 12'
    Stop-Proc $gwOn 'cache-on'
    $gwOnShipped = Start-Gateway -Port $CacheOnPort -ConfigPath $cfgOnShipped -Tag 'cache-on-shipped'
    $shippedOk = $false
    $shipped = $null
    try {
        [void](Invoke-Flush $CacheOnPort)
        $sBefore = Get-CacheAdmin $CacheOnPort
        $smBefore = Get-MockRequestCount
        $sRecords = @(Invoke-CorpusRun -Port $CacheOnPort -Cases $cases -Tag 'shipped')
        $smAfter = Get-MockRequestCount
        $sAfter = Get-CacheAdmin $CacheOnPort
        $sDelta = Get-CacheStatsDelta $sBefore $sAfter
        $smCalls = -1
        if ($smBefore -ge 0 -and $smAfter -ge 0) { $smCalls = $smAfter - $smBefore }
        $shipped = Get-CorpusRunSummary -Records $sRecords -StatsDelta $sDelta -MockCalls $smCalls -Label 'corpus with the shipped min_prompt_chars 12' -MinPromptChars 12 -Store $storeName -Thr $effThreshold -Embedding $embedding
        $shippedOk = $true
        Write-Host ("  requests {0} | exact {1} | semantic {2} | misses {3} | skips {4} | hit rate {5:P1} | pairs {6}/13 | false hits {7} | upstream calls {8}" -f `
                $shipped.requests, $shipped.exact_hits, $shipped.semantic_hits, $shipped.misses, $shipped.skips, $shipped.hit_rate, $shipped.pairs_matched, $shipped.false_hits, $shipped.upstream_calls_seen_by_mock)
    }
    finally {
        Stop-Proc $gwOnShipped 'cache-on-shipped'
    }
    if ($shippedOk) { $primary['shipped_config_variant'] = $shipped }
    else { $primary['shipped_config_variant'] = [ordered]@{ error = 'the shipped min_prompt_chars run did not complete' } }

    # --- cost -------------------------------------------------------------
    Write-Section 'A4. Cost of the labelled workload (mock-gpt price book)'
    $unit = 1e6
    $costOf = {
        param([int]$pt, [int]$ct)
        return (($pt * $priceIn) + ($ct * $priceOut)) / $unit
    }
    $without = 0.0
    $with = 0.0
    $missDetail = New-Object System.Collections.ArrayList
    foreach ($c in $shouldHit) {
        $u = $usage[$c.id]
        $without += (& $costOf $u.a_prompt $u.a_completion) * 2
        $without += (& $costOf $u.b_prompt $u.b_completion)
    }
    foreach ($r in $records) {
        $u = $usage[$r.id]
        if ($null -eq $u) { continue }
        if ($r.letter -eq 'B') { $pt = $u.b_prompt; $ct = $u.b_completion; $ul = [ordered]@{ prompt = $u.b_prompt; completion = $u.b_completion } }
        else { $pt = $u.a_prompt; $ct = $u.a_completion; $ul = [ordered]@{ prompt = $u.a_prompt; completion = $u.a_completion } }
        if ($r.cache -eq 'miss') {
            $with += (& $costOf $pt $ct)
            [void]$missDetail.Add([pscustomobject]@{ id = $r.id; kind = $r.kind; letter = $r.letter; prompt_tokens = $ul.prompt; completion_tokens = $ul.completion; cost_usd = [Math]::Round((& $costOf $pt $ct), 10) })
        }
    }
    $savedFromStats = (($delta['saved_prompt_tokens'] * $priceIn) + ($delta['saved_completion_tokens'] * $priceOut)) / $unit
    $costDelta = [Math]::Round($without - $with - $savedFromStats, 12)
    $nReq = $primary.requests
    $per1kWith = 0.0; $per1kWithout = 0.0; $reductionUsd = 0.0; $reductionPct = 0.0
    if ($nReq -gt 0) {
        $per1kWithout = ($without / $nReq) * 1000.0
        $per1kWith = ($with / $nReq) * 1000.0
        $reductionUsd = $per1kWithout - $per1kWith
        if ($per1kWithout -gt 0) { $reductionPct = ($reductionUsd / $per1kWithout) * 100.0 }
    }
    $costDoc = [ordered]@{
        unit                      = 'USD per 1,000,000 tokens (internal/gateway/pricing.go: const pricingUnitTokens = 1_000_000; CostUSD = (prompt*in + completion*out)/1e6)'
        unit_confirmed            = 'internal/gateway/pricing.go'
        price_model               = 'mock-gpt'
        price_in_per_1m_usd        = $priceIn
        price_out_per_1m_usd       = $priceOut
        pricing_parsed_from_config = $pricingParsed
        workload_requests          = $nReq
        cost_workload_with_cache_usd    = [Math]::Round($with, 10)
        cost_workload_without_cache_usd = [Math]::Round($without, 10)
        cost_saved_usd             = [Math]::Round($savedFromStats, 10)
        cost_saved_usd_from_misses = [Math]::Round($without - $with, 10)
        cost_cross_check_delta_usd = $costDelta
        cost_per_1k_requests_without_cache = [Math]::Round($per1kWithout, 6)
        cost_per_1k_requests_with_cache    = [Math]::Round($per1kWith, 6)
        reduction_usd_per_1k       = [Math]::Round($reductionUsd, 6)
        reduction_percent          = [Math]::Round($reductionPct, 2)
        extrapolation              = 'the measured workload cost divided by its measured request count, scaled to 1000 requests; no real provider was billed'
        misses                     = @($missDetail)
    }
    Write-Host ("  saved {0} prompt + {1} completion tokens -> {2} USD saved on {3} requests" -f $delta['saved_prompt_tokens'], $delta['saved_completion_tokens'], [Math]::Round($savedFromStats, 8), $nReq)
    Write-Host ("  per 1k requests: {0} USD without cache, {1} USD with cache ({2}% less)" -f [Math]::Round($per1kWithout, 6), [Math]::Round($per1kWith, 6), [Math]::Round($reductionPct, 2))

    # --- B: latency and throughput, 3 interleaved rounds ------------------
    Write-Section 'B. Latency and throughput, cache ON vs OFF (3 interleaved rounds)'
    Stop-Proc $gwOnShipped 'cache-on-shipped'
    $gwOn = Start-Gateway -Port $CacheOnPort -ConfigPath $cfgOn -Tag 'cache-on-b'
    $gwOff = Start-Gateway -Port $CacheOffPort -ConfigPath $cfgOff -Tag 'cache-off-b'
    Write-Host "  fresh gateways: :$CacheOnPort (pid $($gwOn.Id)) and :$CacheOffPort (pid $($gwOff.Id)); /stats is therefore a pure-hit / pure-miss view for round 1"
    $cellLists = @{}
    $roundStats = New-Object System.Collections.ArrayList
    $statsOn = $null
    $statsOff = $null
    foreach ($r in 1..$Rounds) {
        [void](Invoke-Flush $CacheOnPort)
        $b = Get-CacheAdmin $CacheOnPort
        $mb = Get-MockRequestCount
        $outOn = Join-Path $baselineDir "m2-load-on-r$r.json"
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        Invoke-Load -Port $CacheOnPort -OutFile $outOn -Tag "on-r$r"
        [void](Add-LoadResults -Cells $cellLists -Scenario 'cache-on' -Path $outOn)
        $sw.Stop()
        if ($r -eq 1) { $statsOn = Get-JsonUrl "http://127.0.0.1:$CacheOnPort/stats" }
        $ma = Get-MockRequestCount
        $a = Get-CacheAdmin $CacheOnPort
        $d = Get-CacheStatsDelta $b $a
        $mockCalls = -1
        if ($mb -ge 0 -and $ma -ge 0) { $mockCalls = $ma - $mb }
        [void]$roundStats.Add([pscustomobject]@{
                round                     = $r
                scenario                  = 'cache-on'
                wall_s                    = [Math]::Round($sw.Elapsed.TotalSeconds, 3)
                lookups                   = $d['lookups']
                hits                      = $d['hits']
                exact_hits                = $d['exact_hits']
                semantic_hits             = $d['semantic_hits']
                misses                    = $d['misses']
                stores                    = $d['stores']
                saved_prompt_tokens       = $d['saved_prompt_tokens']
                saved_completion_tokens   = $d['saved_completion_tokens']
                upstream_calls_observed   = $mockCalls
            })
        Write-Host ("  round {0} cache-ON : {1}s, hits {2}, misses {3}, upstream calls {4} (of {5} requests)" -f $r, [Math]::Round($sw.Elapsed.TotalSeconds, 2), $d['hits'], $d['misses'], $mockCalls, $d['lookups'])

        $outOff = Join-Path $baselineDir "m2-load-off-r$r.json"
        $sw2 = [System.Diagnostics.Stopwatch]::StartNew()
        Invoke-Load -Port $CacheOffPort -OutFile $outOff -Tag "off-r$r"
        [void](Add-LoadResults -Cells $cellLists -Scenario 'cache-off' -Path $outOff)
        $sw2.Stop()
        if ($r -eq 1) { $statsOff = Get-JsonUrl "http://127.0.0.1:$CacheOffPort/stats" }
        [void]$roundStats.Add([pscustomobject]@{
                round                     = $r
                scenario                  = 'cache-off'
                wall_s                    = [Math]::Round($sw2.Elapsed.TotalSeconds, 3)
                lookups = 0; hits = 0; exact_hits = 0; semantic_hits = 0; misses = 0; stores = 0
                saved_prompt_tokens = 0; saved_completion_tokens = 0
                upstream_calls_observed   = -1
            })
        Write-Host ("  round {0} cache-OFF: {1}s" -f $r, [Math]::Round($sw2.Elapsed.TotalSeconds, 2))
    }

    # reduce to rows, m1 contract
    $rows = New-Object System.Collections.ArrayList
    foreach ($scenario in @('cache-on', 'cache-off')) {
        foreach ($workload in @('non-stream', 'stream')) {
            foreach ($ccRaw in @($Concurrency -split ',')) {
                $cc = [int]$ccRaw.Trim()
                $key = "$scenario|$workload|$cc"
                if (-not $cellLists.ContainsKey($key)) { continue }
                $list = @($cellLists[$key])
                $qps = @($list | ForEach-Object { [double]$_.qps })
                $p95 = @($list | ForEach-Object { [double]$_.p95 / 1e6 })
                $p99 = @($list | ForEach-Object { [double]$_.p99 / 1e6 })
                $errs = 0
                foreach ($e in $list) { if ($null -ne $e.errors) { $errs += [int]$e.errors } }
                [void]$rows.Add([pscustomobject]@{
                        Scenario   = $scenario
                        Workload   = $workload
                        Concurrency = $cc
                        QPS        = [Math]::Round((Get-Median $qps), 1)
                        QPSMin     = [Math]::Round(($qps | Measure-Object -Minimum).Minimum, 1)
                        QPSMax     = [Math]::Round(($qps | Measure-Object -Maximum).Maximum, 1)
                        P95_ms     = [Math]::Round((Get-Median $p95), 3)
                        P99_ms     = [Math]::Round((Get-Median $p99), 3)
                        Errors     = $errs
                        P95Min_ms  = [Math]::Round(($p95 | Measure-Object -Minimum).Minimum, 3)
                        P95Max_ms  = [Math]::Round(($p95 | Measure-Object -Maximum).Maximum, 3)
                        P99Min_ms  = [Math]::Round(($p99 | Measure-Object -Minimum).Minimum, 3)
                        P99Max_ms  = [Math]::Round(($p99 | Measure-Object -Maximum).Maximum, 3)
                    })
            }
        }
    }
    if ($rows.Count -gt 0) { $rows | Format-Table -AutoSize | Out-String -Width 200 | Write-Host }

    $latencyDoc = [ordered]@{
        client                        = "bin\loadtest.exe -url http://127.0.0.1:PORT -c $Concurrency -n $Requests -warmup $Warmup -out docs\baseline\m2-load-<on|off>-r<round>.json"
        rounds                        = $Rounds
        interleaving                  = 'round r: flush the cache-ON cache, run the load against cache-ON, then against cache-OFF - so drift is shared by both arms instead of being attributed to the cache'
        load_body                     = 'the load client replays ONE fixed chat body: {"model":"mock-gpt","messages":[{"role":"user","content":"hello"}]} (and the same with "stream":true)'
        rows                          = @($rows)
        cache_stats_by_round          = @($roundStats)
        gateway_stats                 = [ordered]@{
            cache_on_pure_hit  = [ordered]@{
                source = '/stats taken after round 1 on a gateway whose only traffic is that one load round; the warmup (100 requests) primes the cache, so all 800 measured requests are hits'
                stats  = (Get-LatencyPeek $statsOn)
            }
            cache_off_pure_miss = [ordered]@{
                source = '/stats taken after round 1 on a fresh cache-OFF gateway: no cache at all, every request goes upstream'
                stats  = (Get-LatencyPeek $statsOff)
            }
        }
        note                          = 'the upstream is the in-repo mock (local, free), so the cache-ON/cache-OFF throughput gap measures gateway work removed (no HTTP round trip, no JSON decode/encode of a provider response), NOT a provider latency saving. The decisive numbers are the cached-path P95 and upstream_calls_observed == misses.'
    }
    Write-Host ''
    Write-Host ("  cache-ON  pure-hit  /stats: {0}" -f (($latencyDoc.gateway_stats.cache_on_pure_hit.stats | ConvertTo-Json -Compress)))
    Write-Host ("  cache-OFF pure-miss /stats: {0}" -f (($latencyDoc.gateway_stats.cache_off_pure_miss.stats | ConvertTo-Json -Compress)))

    # --- C: the cache as a shield ----------------------------------------
    # Order is the whole point of this section: prime with the provider UP, prove
    # the entries landed in the store, and only then kill the provider.  Priming
    # a dead upstream would measure the outage, not the shield.
    Write-Section 'C. Shield: serve while the provider is hard down'
    [void](Invoke-Flush $CacheOnPort)
    $primeBefore = Get-CacheAdmin $CacheOnPort
    $storesBefore = 0
    if ($null -ne $primeBefore -and $null -ne $primeBefore.stats) { $storesBefore = [int64]$primeBefore.stats.stores }
    $primeMisses = 0
    $primeErrors = 0
    $primeHits = 0
    foreach ($c in $shouldHit) {
        # one model for both letters: the cache scope is derived from the model,
        # so priming and looking up under different models could never match
        foreach ($letter in @('a', 'b')) {
            $prompt = $c.a
            if ($letter -eq 'b') { $prompt = $c.b }
            $body = New-ChatBody "shield-$($c.id)" $prompt
            $res = Send-Chat $CacheOnPort $body
            if ($res.Status -ne 200) { $primeErrors++ }
            elseif ($res.Cache -eq 'miss') { $primeMisses++ }
            elseif (Test-Hit $res.Cache) { $primeHits++ }
        }
    }
    # Every miss stores, so the reported miss count is the expected store count:
    # poll the admin surface until that many stores have landed (bounded).
    $expectStores = $primeMisses
    $primedStores = 0
    $primedEntries = 0
    $primePollDeadline = (Get-Date).AddSeconds(15)
    while ($true) {
        $primeAfter = Get-CacheAdmin $CacheOnPort
        if ($null -ne $primeAfter) {
            if ($null -ne $primeAfter.stats) { $primedStores = [int64]$primeAfter.stats.stores - $storesBefore }
            $primedEntries = 0
            if ($null -ne $primeAfter.scopes) {
                foreach ($p in $primeAfter.scopes.PSObject.Properties) { $primedEntries += [int]$p.Value }
            }
        }
        if ($primedStores -ge $expectStores) { break }
        if ((Get-Date) -ge $primePollDeadline) { break }
        Start-Sleep -Milliseconds 250
    }
    $primeLanded = ($primeMisses -gt 0 -and $primedStores -ge $expectStores -and $primeErrors -eq 0)
    Write-Host ("  primed {0} prompts with the provider UP: {1} HTTP 200 ({2} miss + {3} hit), {4} non-200, {5}/{6} stores landed, {7} entries in scope" -f `
            ($shouldHit.Count * 2), (($shouldHit.Count * 2) - $primeErrors), $primeMisses, $primeHits, $primeErrors, $primedStores, $expectStores, $primedEntries)

    # Only now take the provider away.  Everything above ran with it alive, so
    # the store holds the answers; from here on an HTTP 200 can only have come
    # from the cache, because there is no upstream left to answer.
    Stop-Proc $mockProc 'mock'

    $mockDead = $false
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $deadline) {
        $probe = (Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', '-m', '2', "http://127.0.0.1:$MockPort/healthz")).Trim()
        if ($probe -ne '200') { $mockDead = $true; break }
        Start-Sleep -Milliseconds 200
    }
    Write-Host ("  mock upstream killed (pid {0}); /healthz now http '{1}'" -f $mockProc.Id, $probe)

    $replay200 = 0
    $replayExact = 0
    $replaySemantic = 0
    $replayMiss = 0
    $replaySkip = 0
    $fivexx = 0
    $non200 = 0
    foreach ($c in $shouldHit) {
        foreach ($letter in @('a', 'b')) {
            $prompt = $c.a
            if ($letter -eq 'b') { $prompt = $c.b }
            $body = New-ChatBody "shield-$($c.id)" $prompt
            $res = Send-Chat $CacheOnPort $body
            if ($res.Status -eq 200) { $replay200++ }
            else { $non200++ }
            if ($res.Status -ge 500) { $fivexx++ }
            if ($res.Cache -eq 'hit-exact') { $replayExact++ }
            elseif ($res.Cache -eq 'hit-semantic') { $replaySemantic++ }
            elseif ($res.Cache -eq 'miss') { $replayMiss++ }
            elseif ($res.Cache -eq 'skip') { $replaySkip++ }
        }
    }
    $controlBody = New-ChatBody 'shield-control' 'what is the weather in Reykjavik tomorrow morning'
    $control = Send-Chat $CacheOnPort $controlBody
    Write-Host ("  replayed {0} primed prompts with the provider down: {1} x HTTP 200 ({2} exact, {3} semantic), {4} non-200, {5} 5xx" -f `
            ($shouldHit.Count * 2), $replay200, $replayExact, $replaySemantic, $non200, $fivexx)
    Write-Host ("  control (unprimed prompt, same dead provider): http {0} / cache '{1}' -- proves the outage was real" -f $control.Status, $control.Cache)

    # The claim is only worth shipping if it is asserted, not merely printed:
    # every primed prompt served, nothing 5xx, and the control request failing.
    $shieldServedOk = Assert-That ($replay200 -eq ($shouldHit.Count * 2)) 'shield: every primed prompt is served with the provider process dead' ("served_200={0} of {1}" -f $replay200, ($shouldHit.Count * 2))
    $shield5xxOk = Assert-That ($fivexx -eq 0) 'shield: no 5xx while replaying primed prompts with the provider dead' ("five_xx={0}" -f $fivexx)
    $shieldControlOk = Assert-That ($control.Status -ne 200) 'shield: the unprimed control request fails while the provider is dead' ("control_status={0} cache={1}" -f $control.Status, $control.Cache)

    $shieldDoc = [ordered]@{
        primed_prompts              = $shouldHit.Count * 2
        primed_http_200             = ($shouldHit.Count * 2) - $primeErrors
        primed_non_200              = $primeErrors
        primed_hits_while_up        = $primeHits
        primed_requests_that_missed = $primeMisses
        primed_stores_expected      = $expectStores
        primed_stores_landed        = $primedStores
        primed_entries_in_scope     = $primedEntries
        primed_landed               = $primeLanded
        primed_note                 = 'priming runs with the provider UP: one request per prompt, every miss stores, so primed_stores_landed is read from the /admin/cache stores delta and polled until it reaches primed_stores_expected'
        mock_pid_killed             = $mockProc.Id
        provider_confirmed_down     = $mockDead
        probe_status_after_kill     = $probe
        replay_requests             = $shouldHit.Count * 2
        served_200                  = $replay200
        served_exact_200            = $replayExact
        served_semantic_200         = $replaySemantic
        replay_misses               = $replayMiss
        replay_skips                = $replaySkip
        non_200                     = $non200
        five_xx                     = $fivexx
        five_xx_assertion_passed    = $shield5xxOk
        served_all_primed_assertion_passed = $shieldServedOk
        shield_assertion_passed     = ($shieldServedOk -and $shield5xxOk -and $shieldControlOk)
        control_not_200_assertion_passed = $shieldControlOk
        claim                       = 'requests answered with HTTP 200 while the provider process was dead: they were served from the cache, with no upstream call possible'
        control_status              = $control.Status
        control_cache               = $control.Cache
        control_note                = 'an unprimed prompt must NOT be answered while the provider process is dead; this is what separates "the cache served it" from "the provider was never down"'
        mock_restarted              = $false
    }
    if ($fivexx -ne 0) { Write-Host ("  WARNING: {0} request(s) returned 5xx while replaying primed prompts" -f $fivexx) -ForegroundColor Yellow }
    if ($replay200 -ne ($shouldHit.Count * 2)) { Write-Host ("  WARNING: only {0}/{1} primed prompts were served with the provider down" -f $replay200, ($shouldHit.Count * 2)) -ForegroundColor Yellow }

    $mockProc2 = Start-Mock -Port $MockPort -Name 'primary' -Tag 'mock-restart'
    $mockCode2 = Wait-Healthy -Port $MockPort -Path '/healthz' -Seconds 20
    $shieldDoc['mock_restarted'] = ($mockCode2 -eq '200')
    $shieldDoc['mock_restart_pid'] = $mockProc2.Id
    Write-Host ("  mock upstream restarted on :{0} (pid {1}, http {2})" -f $MockPort, $mockProc2.Id, $mockCode2)

    # --- D: threshold sweep ----------------------------------------------
    Write-Section 'D. Threshold sweep (cmd/measure-m2, offline)'
    $sweepTmp = Join-Path $tmpDir 'm2-sweep.json'
    $sweepStdout = Join-Path $tmpDir 'm2-sweep-stdout.txt'
    Remove-Item $sweepTmp -ErrorAction SilentlyContinue
    & $goShim run ./cmd/measure-m2 -json -out $sweepTmp 2>$null | Out-File -FilePath $sweepStdout -Encoding utf8
    $sweepExit = $LASTEXITCODE
    $sweepReport = Get-JsonText (Read-Text $sweepTmp)
    if ($null -eq $sweepReport) {
        # fall back to the JSON the command may have printed instead of writing
        $stdoutText = Read-Text $sweepStdout
        $i = $stdoutText.IndexOf('{'); $j = $stdoutText.LastIndexOf('}')
        if ($i -ge 0 -and $j -gt $i) { $sweepReport = Get-JsonText $stdoutText.Substring($i, $j - $i + 1) }
    }
    if ($null -eq $sweepReport) { throw "cmd/measure-m2 produced no parsable report (exit $sweepExit)" }
    Copy-Item -Path $sweepTmp -Destination (Join-Path $baselineDir 'm2-sweep.json') -Force

    $sweepStdoutText = Read-Text $sweepStdout
    $reportedRec = [regex]::Match($sweepStdoutText, 'recommended threshold ([0-9.]+): hit rate ([0-9]+)% \(([0-9]+)/([0-9]+)\), false-hit rate ([0-9]+)% \(([0-9]+)/([0-9]+)\)')
    $reportedRecThreshold = $null
    if ($reportedRec.Success) { $reportedRecThreshold = [double]$reportedRec.Groups[1].Value }

    $thresholds = @($sweepReport.thresholds)
    $rec = $null
    foreach ($t in $thresholds) {
        if ([int]$t.false_hits -gt 0) { continue }
        if ($null -eq $rec -or [int]$t.true_hits -gt [int]$rec.true_hits -or ([int]$t.true_hits -eq [int]$rec.true_hits -and [double]$t.threshold -gt [double]$rec.threshold)) { $rec = $t }
    }
    $byBudget = [ordered]@{}
    foreach ($budget in 0..3) {
        $best = $null
        foreach ($t in $thresholds) {
            if ([int]$t.false_hits -gt $budget) { continue }
            if ($null -eq $best -or [int]$t.true_hits -gt [int]$best.true_hits -or ([int]$t.true_hits -eq [int]$best.true_hits -and [double]$t.threshold -gt [double]$best.threshold)) { $best = $t }
        }
        if ($null -eq $best) { $byBudget["max_false_hits_$budget"] = 'no swept threshold satisfies the budget' }
        else {
            $byBudget["max_false_hits_$budget"] = [ordered]@{
                threshold      = [double]$best.threshold
                true_hits      = [int]$best.true_hits
                true_total     = [int]$best.true_total
                hit_rate       = [Math]::Round([int]$best.true_hits / [double][int]$best.true_total, 4)
                false_hits     = [int]$best.false_hits
                false_total    = [int]$best.false_total
                false_hit_rate = [Math]::Round([int]$best.false_hits / [double][int]$best.false_total, 4)
            }
        }
    }
    $curve = New-Object System.Collections.ArrayList
    foreach ($t in $thresholds) {
        [void]$curve.Add([pscustomobject]@{
                threshold      = [double]$t.threshold
                true_hits      = [int]$t.true_hits
                true_total     = [int]$t.true_total
                hit_rate       = [Math]::Round([int]$t.true_hits / [double][int]$t.true_total, 4)
                false_hits     = [int]$t.false_hits
                false_total    = [int]$t.false_total
                false_hit_rate = [Math]::Round([int]$t.false_hits / [double][int]$t.false_total, 4)
                missed_ids     = @($t.missed_ids)
                false_hit_ids  = @($t.false_hit_ids)
            })
    }
    # per-pair similarity, from the -pairs listing (only the numeric column is used)
    $pairsOut = Join-Path $tmpDir 'm2-sweep-pairs.txt'
    & $goShim run ./cmd/measure-m2 -pairs 2>$null | Out-File -FilePath $pairsOut -Encoding utf8
    $sims = New-Object System.Collections.ArrayList
    foreach ($line in @(Read-Text $pairsOut -split "`r?`n")) {
        $m = [regex]::Match($line, '^\s*([0-9]+\.[0-9]+)\s+(\S+)\s+(\S+)')
        if ($m.Success) {
            [void]$sims.Add([pscustomobject]@{ similarity = [double]$m.Groups[1].Value; id = $m.Groups[2].Value; kind = $m.Groups[3].Value })
        }
    }
    $shippedRow = $null
    foreach ($t in $curve) { if ([Math]::Abs([double]$t.threshold - $effThreshold) -lt 0.000001) { $shippedRow = $t } }
    $sweepDoc = [ordered]@{
        command              = ".\tools\go.cmd run ./cmd/measure-m2 -json -out tmp\m2-sweep.json"
        exit_code            = $sweepExit
        embedder             = [string]$sweepReport.embedder
        dims                 = [int]$sweepReport.dims
        cases                = [int]$sweepReport.cases
        thresholds           = @($curve)
        shipped_threshold    = $effThreshold
        shipped_threshold_row = $shippedRow
        recommendation       = if ($null -eq $rec) { 'NO threshold in the swept grid keeps false hits within 0: this embedder cannot separate the corpus' }
        else {
            [ordered]@{
                max_false_hits = 0
                threshold      = [double]$rec.threshold
                true_hits      = [int]$rec.true_hits
                true_total     = [int]$rec.true_total
                hit_rate       = [Math]::Round([int]$rec.true_hits / [double][int]$rec.true_total, 4)
                false_hits     = [int]$rec.false_hits
                false_total    = [int]$rec.false_total
                false_hit_rate = [Math]::Round([int]$rec.false_hits / [double][int]$rec.false_total, 4)
            }
        }
        recommendation_by_budget = $byBudget
        recommendation_reported_by_tool = $reportedRecThreshold
        recommendation_matches_tool    = ($null -ne $reportedRecThreshold -and $null -ne $rec -and [Math]::Abs($reportedRecThreshold - [double]$rec.threshold) -lt 0.000001)
        per_pair_similarity  = @($sims)
        per_pair_note        = 'cosine similarity per labelled pair as printed by the hashing embedder (sorted descending by the tool)'
        live_check           = 'hit_rate (the labelled workload through the real gateway) at this threshold: recorded above in hit_rate'
    }
    if ($curve.Count -gt 0) { $curve | Format-Table threshold, true_hits, true_total, hit_rate, false_hits, false_total, false_hit_rate -AutoSize | Out-String -Width 200 | Write-Host }
    if ($null -ne $rec) { Write-Host ("  recommendation: threshold {0} -> hit rate {1:P1} ({2}/{3}), false-hit rate {4:P1} ({5}/{6})" -f $rec.threshold, ([int]$rec.true_hits / [double][int]$rec.true_total), $rec.true_hits, $rec.true_total, ([int]$rec.false_hits / [double][int]$rec.false_total), $rec.false_hits, $rec.false_total) }
    else { Write-Host '  recommendation: none - no swept threshold keeps false hits at zero' -ForegroundColor Yellow }
    Write-Host ("  shipped threshold {0} -> {1}/{2} should-hit pairs, {3} false hits" -f $effThreshold, $shippedRow.true_hits, $shippedRow.true_total, $shippedRow.false_hits)

    # --- summary ----------------------------------------------------------
    Write-Section 'Summary'
    $hitRateDoc = [ordered]@{
        labelled_workload  = 'A, A, B per should-hit pair (39 requests) + A, B per should-not-hit pair (26 requests) = 65 requests'
        requests           = $primary.requests
        exact_hits         = $primary.exact_hits
        semantic_hits      = $primary.semantic_hits
        hits               = $primary.hits
        misses             = $primary.misses
        skips              = $primary.skips
        hit_rate           = $primary.hit_rate
        hit_rate_definition = $primary.hit_rate_definition
        pairs_matched      = $primary.pairs_matched
        pairs_total        = $primary.pairs_total
        pair_hit_rate      = $primary.pair_hit_rate
        matched_ids        = $primary.matched_ids
        should_not_hit_requests = $primary.should_not_hit_requests
        false_hits         = $primary.false_hits
        false_hit_ids      = $primary.false_hit_ids
        by_kind            = $primary.by_kind
        admin_cache_delta  = $primary.admin_cache_delta
        upstream_calls_seen_by_mock = $primary.upstream_calls_seen_by_mock
        upstream_calls_note = $primary.upstream_calls_note
        store              = $primary.store
        embedding          = $primary.embedding
        threshold          = $primary.threshold
        min_prompt_chars   = $primary.min_prompt_chars
        shipped_config_variant = $primary.shipped_config_variant
        config_note        = 'the shipped configs/cache-local.yaml has min_prompt_chars: 12, which skips 6 of the 26 pairs (every CJK pair is 7-8 characters) and would make the hit-rate and false-hit numbers measure the length gate instead of the semantic matcher. The headline run therefore uses min_prompt_chars: 1 in a generated tmp config; the same workload at the shipped value is reported in shipped_config_variant. Nothing in configs/ is modified.'
    }
    $tokensDoc = [ordered]@{
        saved_prompt_tokens     = $delta['saved_prompt_tokens']
        saved_completion_tokens = $delta['saved_completion_tokens']
        saved_total_tokens      = [int64]$delta['saved_prompt_tokens'] + [int64]$delta['saved_completion_tokens']
        source                  = 'GET /admin/cache stats delta across the labelled workload (min_prompt_chars 1 run)'
        hits                    = $delta['hits']
        exact_hits              = $delta['exact_hits']
        semantic_hits           = $delta['semantic_hits']
        misses                  = $delta['misses']
        stores                  = $delta['stores']
        lookups                 = $delta['lookups']
        mean_uncached_prompt_tokens_per_request     = $avgPt
        mean_uncached_completion_tokens_per_request = $avgCt
        usage_source            = 'usage block of the real responses from the cache-OFF gateway for the A and B of every should-hit pair'
        mock_usage_note         = 'the in-repo mock defines prompt_tokens = whitespace-separated words per message + 4, and completion_tokens = whitespace-separated words of its answer'
    }
    $methodDoc = [ordered]@{
        requests          = $Requests
        warmup            = $Warmup
        rounds            = $Rounds
        concurrency       = @($Concurrency -split ',')
        client            = 'bin\loadtest.exe (cmd/loadtest) -url mode'
        corpus_client     = 'curl.exe (Windows PowerShell 5.1), one POST per labelled prompt'
        upstream          = 'in-repo mock (cmd/mockupstream) on :' + $MockPort + ' - local, free and dependency-free, so a cache-ON/cache-OFF throughput delta measures gateway work removed, not a provider latency saving'
        gateway_cache_on  = ':' + $CacheOnPort
        gateway_cache_off = ':' + $CacheOffPort
        store             = $storeName
        embedder          = $embedding
        threshold         = $effThreshold
        min_prompt_chars  = $primary.min_prompt_chars
        configs           = 'configs\cache-local.yaml instantiated into tmp\m2-cache-on.yaml, tmp\m2-cache-off.yaml and tmp\m2-cache-on-shipped.yaml by literal substitution; no repository file is modified'
        ports_unused      = 'mini-redis :' + $MiniRedisPort + ' was not started: these measurements use the in-process memory store, and the Redis store path is covered by cmd/verify-m2'
        rounds_note       = 'this host drifts round to round, so the two arms are interleaved and medians over ' + $Rounds + ' rounds are reported, never a single pass'
        load_body        = 'the load client replays ONE fixed 5-character prompt ("hello"); a semantic cache cannot be measured with a single repeated request, which is why sections A and C drive the labelled corpus instead'
    }
    $summary = [ordered]@{
        generated_at = (Get-Date).ToString('s')
        host         = "$env:COMPUTERNAME / $env:PROCESSOR_IDENTIFIER / $($PSVersionTable.PSVersion)"
        duration_s   = [Math]::Round(((Get-Date) - $startedAt).TotalSeconds, 1)
        milestone    = 'M2 - semantic cache'
        method       = $methodDoc
        corpus       = $corpusDoc
        hit_rate     = $hitRateDoc
        tokens       = $tokensDoc
        cost         = $costDoc
        latency      = $latencyDoc
        shield       = $shieldDoc
        sweep        = $sweepDoc
        false_hits   = $primary.false_hits
        limitations  = @(
            'The upstream is the in-repo mock: it is local and free, so cache-ON vs cache-OFF throughput is a gateway-overhead measurement, not a provider latency saving. The decisive cached-path facts are the P95 and that a hit performs no upstream call at all (upstream_calls_observed == misses).',
            'A hit is answered from the stored body; its P95 is therefore the cache lookup plus a replay, which is what a real deployment would save the provider round trip of.',
            'Cost figures use the config price book (USD per 1,000,000 tokens) and are extrapolated from the measured workload to 1000 requests; no provider was billed.',
            'The labelled-corpus numbers use a generated config with min_prompt_chars: 1 (the shipped file says 12, which skips 6 of the 26 pairs). Both runs are reported; nothing in configs/ was edited.',
            'Three interleaved rounds give a range, not a confidence interval: the host showed ' + [Math]::Round(($rows | Where-Object { $_.QPSMax -gt 0 } | ForEach-Object { (($_.QPSMax - $_.QPSMin) / [double]$_.QPSMax) } | Measure-Object -Maximum).Maximum * 100, 1) + '% worst-case spread between rounds.'
        )
    }

    $summaryPath = Join-Path $baselineDir 'm2-summary.json'
    Write-JsonFile $summaryPath $summary

    Write-Host ''
    Write-Host '=== M2 HEADLINE ==='
    $onRow = $rows | Where-Object { $_.Scenario -eq 'cache-on' -and $_.Workload -eq 'non-stream' -and $_.Concurrency -eq 8 }
    $offRow = $rows | Where-Object { $_.Scenario -eq 'cache-off' -and $_.Workload -eq 'non-stream' -and $_.Concurrency -eq 8 }
    $onRow32 = $rows | Where-Object { $_.Scenario -eq 'cache-on' -and $_.Workload -eq 'non-stream' -and $_.Concurrency -eq 32 }
    $offRow32 = $rows | Where-Object { $_.Scenario -eq 'cache-off' -and $_.Workload -eq 'non-stream' -and $_.Concurrency -eq 32 }
    Write-Host ("  labelled workload        {0} requests to should-hit pairs: {1} exact + {2} semantic + {3} misses + {4} skips" -f $primary.requests, $primary.exact_hits, $primary.semantic_hits, $primary.misses, $primary.skips)
    Write-Host ("  measured hit rate        {0:P1} (request level), {1}/{2} pairs matched ({3:P1})" -f $primary.hit_rate, $primary.pairs_matched, $primary.pairs_total, $primary.pair_hit_rate)
    Write-Host ("  false hits               {0} of {1} near-miss/unrelated requests" -f $primary.false_hits, $primary.should_not_hit_requests)
    Write-Host ("  saved tokens             {0} prompt + {1} completion" -f $delta['saved_prompt_tokens'], $delta['saved_completion_tokens'])
    Write-Host ("  cost saved               {0} USD on this workload; {1} USD -> {2} USD per 1k requests ({3}% less) [USD per 1M tokens: in {4}, out {5}]" -f `
            [Math]::Round($costDoc.cost_saved_usd, 8), [Math]::Round($costDoc.cost_per_1k_requests_without_cache, 6), [Math]::Round($costDoc.cost_per_1k_requests_with_cache, 6), $costDoc.reduction_percent, $priceIn, $priceOut)
    if ($onRow -and $offRow) {
        Write-Host ("  P95 non-stream c=8       cache ON  {0} ms   cache OFF {1} ms" -f $onRow.P95_ms, $offRow.P95_ms)
        Write-Host ("  QPS non-stream c=8       cache ON  {0} ({1}-{2})   cache OFF {3} ({4}-{5})" -f $onRow.QPS, $onRow.QPSMin, $onRow.QPSMax, $offRow.QPS, $offRow.QPSMin, $offRow.QPSMax)
    }
    if ($onRow32 -and $offRow32) {
        Write-Host ("  P95 non-stream c=32      cache ON  {0} ms   cache OFF {1} ms" -f $onRow32.P95_ms, $offRow32.P95_ms)
        Write-Host ("  QPS non-stream c=32      cache ON  {0} ({1}-{2})   cache OFF {3} ({4}-{5})" -f $onRow32.QPS, $onRow32.QPSMin, $onRow32.QPSMax, $offRow32.QPS, $offRow32.QPSMin, $offRow32.QPSMax)
    }
    Write-Host ("  /stats pure-hit p95      {0}   pure-miss p95 {1}" -f $latencyDoc.gateway_stats.cache_on_pure_hit.stats.p95, $latencyDoc.gateway_stats.cache_off_pure_miss.stats.p95)
    Write-Host ("  shield                   {0}/{1} requests served with the provider process killed ({2} exact, {3} semantic), {4} 5xx; control request http {5}" -f `
            $replay200, ($shouldHit.Count * 2), $replayExact, $replaySemantic, $fivexx, $control.Status)
    if ($null -ne $rec) {
        Write-Host ("  swept recommendation     threshold {0}: hit rate {1:P1} ({2}/{3}), false-hit rate {4:P1} ({5}/{6})" -f `
                $rec.threshold, ([int]$rec.true_hits / [double][int]$rec.true_total), $rec.true_hits, $rec.true_total, ([int]$rec.false_hits / [double][int]$rec.false_total), $rec.false_hits, $rec.false_total)
    }
    Write-Host ("  shipped threshold        {0}" -f $effThreshold)
    Write-Host ''
    Write-Host ("  wrote {0}" -f $summaryPath)
    if ($script:assertFails.Count -gt 0) {
        Write-Host ("  {0} assertion(s) failed: {1}" -f $script:assertFails.Count, ($script:assertFails -join '; ')) -ForegroundColor Red
    }
    else { Write-Host '  all assertions passed' -ForegroundColor Green }
    $exitOk = ($script:assertFails.Count -eq 0)
}
finally {
    if ($KeepRunning) {
        Write-Host ''
        Write-Host '  -KeepRunning: leaving ' $script:procs.Count ' process(es) up' -ForegroundColor Yellow
    }
    else {
        foreach ($p in $script:procs) { Stop-Proc $p }
    }
    if (-not $exitOk) {
        Write-Host '  measurement did not complete cleanly; partial artifacts are in tmp\' -ForegroundColor Yellow
        exit 1
    }
}
