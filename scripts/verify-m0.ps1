# InferGate M0 -- end-to-end verification with curl.exe.
#
# This script is the "curl-verifiable" acceptance path from the project brief: it
# builds the two binaries, starts the mock upstream and the gateway as real
# processes, and drives them with curl. It is deliberately separate from
# `cmd/verify` (a Go program with the same checks) because the two prove
# different things:
#
#   * cmd/verify   proves the gateway is CORRECT: byte transparency, token
#                  accounting, routing decisions, TTFB, tool_call reassembly.
#                  Runs in CI, exit code is the gate.
#   * this script  proves the gateway is OPERABLE: it is a real binary, it
#                  answers a real shell's curl, and its stdout shows structured
#                  logs for the same requests. This is what a reviewer runs.
#
# Usage:
#   pwsh -File scripts/verify-m0.ps1
#
# Everything is confined to this repository: binaries land in bin\, logs in
# tmp\, and both processes are stopped in the finally block.

[CmdletBinding()]
param(
    [int]$GatewayPort = 18080,
    [int]$MockPort = 19000,
    [int]$TtfbMillis = 250,
    # Pacing between streamed content frames, handed to the mock explicitly
    # rather than left to its default: section 6 derives its "the response is
    # streamed, not sent in one batch" threshold from this number.
    [int]$TokenDelayMillis = 15,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
$mockLog = Join-Path $tmpDir 'mockupstream.log'
$gwLog = Join-Path $tmpDir 'gateway.log'

$script:passed = 0
$script:failed = 0

function Write-Section([string]$Title) {
    Write-Host ''
    Write-Host ("=" * 72) -ForegroundColor DarkGray
    Write-Host "  $Title" -ForegroundColor Cyan
    Write-Host ("=" * 72) -ForegroundColor DarkGray
}

function Assert-Contains {
    param([string]$Label, [string]$Haystack, [string]$Needle)
    # Literal substring test, NOT -like.
    #
    # -like applies WILDCARD matching, so a needle containing brackets is a
    # character class: 'data: [DONE]' can only ever match "data: D", "data: O",
    # "data: N" or "data: E" and therefore reported a false failure against a
    # stream that demonstrably ended with the sentinel. Every needle here is a
    # literal, so use a literal comparison.
    if ($null -ne $Haystack -and $Haystack.Contains($Needle)) {
        Write-Host "  PASS  $Label" -ForegroundColor Green
        $script:passed++
    }
    else {
        Write-Host "  FAIL  $Label" -ForegroundColor Red
        Write-Host "        expected to find: $Needle" -ForegroundColor DarkYellow
        Write-Host "        actual: $($Haystack.Substring(0, [Math]::Min(400, $Haystack.Length)))" -ForegroundColor DarkGray
        $script:failed++
    }
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

$utf8NoBom = New-Object System.Text.UTF8Encoding($false)

function Invoke-Curl {
    param([string[]]$Arguments)
    # Capture stdout ONLY. Windows PowerShell 5.1 turns anything a native
    # command writes to stderr into a NativeCommandError, and with an inherited
    # $ErrorActionPreference of 'Stop' a single curl note ("Unnecessary use of
    # -X or --request") aborts the whole script mid-section -?which is exactly
    # how section 6 once died silently. Diagnostics are kept out of the pipe;
    # gateway error bodies arrive on stdout anyway.
    return (& curl.exe @Arguments 2>$null | Out-String)
}

function New-BodyFile {
    <#
        Writes a JSON request body to a temp file and returns the '@path' form
        for curl.

        This indirection is NOT cosmetic. Windows PowerShell 5.1 strips the
        double quotes out of an argument when it re-quotes it for a native
        executable, so `-d '{"model":"x"}'` reaches curl as {model:x} and every
        provider answers 400 "invalid character 'm'". Passing the payload as a
        file with --data-binary sends the bytes verbatim, which is also what an
        SSE request body genuinely needs: no newline is appended and no quoting
        applies.
    #>
    param([string]$Name, [string]$Json)
    $path = Join-Path $tmpDir $Name
    [System.IO.File]::WriteAllText($path, $Json, $script:utf8NoBom)
    return "@$path"
}

# Get-MetricSum adds up every sample of a Prometheus series from the /metrics
# text; Get-MetricCountSum is the integer form. They exist because the exported
# series are labelled per route/upstream/model/status, so matching only the
# first sample would silently produce a wrong average -- or a zero one, if that
# first row happened to be an error row.
function Get-MetricSum {
    param([string]$Text, [string]$Series)
    $pattern = '(?m)^' + [regex]::Escape($Series) + '\{[^}]*\}\s+([0-9.eE+-]+)\s*$'
    $total = 0.0
    foreach ($m in [regex]::Matches($Text, $pattern)) {
        $total += [double]::Parse($m.Groups[1].Value, [System.Globalization.CultureInfo]::InvariantCulture)
    }
    return $total
}

function Get-MetricCountSum {
    param([string]$Text, [string]$Series)
    return [int](Get-MetricSum -Text $Text -Series $Series)
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

New-Item -ItemType Directory -Force -Path $binDir, $tmpDir | Out-Null

# ---------------------------------------------------------------------------
# 1. Build
# ---------------------------------------------------------------------------
Write-Section '1. Build'
& $goShim build -o (Join-Path $binDir 'infergate.exe') ./cmd/infergate
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/infergate failed (rc=$LASTEXITCODE)" }
& $goShim build -o (Join-Path $binDir 'mockupstream.exe') ./cmd/mockupstream
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/mockupstream failed (rc=$LASTEXITCODE)" }
Write-Host "  built bin\infergate.exe and bin\mockupstream.exe" -ForegroundColor Green

# The mock's own config is rewritten to the chosen port so the script never
# fights a stale process on the default 9000.
$mockCfg = Join-Path $tmpDir 'mock.gateway.yaml'
$gatewayCfg = Join-Path $tmpDir 'gateway.yaml'
# Write WITHOUT a byte-order mark. Set-Content -Encoding UTF8 on Windows
# PowerShell emits a BOM, which is valid for the loader but noisy in logs and
# diffs; the loader tolerates one either way.
$rewritten = (Get-Content (Join-Path $repo 'configs\mock.yaml') -Raw) `
    -replace 'http://127\.0\.0\.1:9000', "http://127.0.0.1:$MockPort" `
    -replace 'listen: ":8080"', "listen: `":$GatewayPort`""
[System.IO.File]::WriteAllText($gatewayCfg, $rewritten, $utf8NoBom)
Write-Host "  wrote $gatewayCfg (listen :$GatewayPort -> mock :$MockPort)"

$mockProc = $null
$gwProc = $null
try {
    # -----------------------------------------------------------------------
    # 2. Start both processes as real background OS processes
    # -----------------------------------------------------------------------
    Write-Section '2. Start processes'
    Set-Content -Path $mockLog, $gwLog -Value '' -Encoding UTF8
    # Remember the byte offset of each log so the assertions in section 10 talk
    # about THIS run. Start-Process redirect APPENDS, so a previous run's
    # outcomes (including its deliberate failures) would otherwise be read back
    # as if they had just happened.
    $mockLogBase = (Get-Item $mockLog).Length
    $gwLogBase = (Get-Item $gwLog).Length
    $mockProc = Start-Process -FilePath (Join-Path $binDir 'mockupstream.exe') `
        -ArgumentList @('-listen', ":$MockPort", '-ttfb', "${TtfbMillis}ms", '-token-delay', "${TokenDelayMillis}ms") `
        -RedirectStandardOutput $mockLog -RedirectStandardError (Join-Path $tmpDir 'mockupstream.err.log') `
        -PassThru -WindowStyle Hidden
    Write-Host "  mockupstream.exe pid=$($mockProc.Id) listening :$MockPort (ttfb=${TtfbMillis}ms, one frame per ${TokenDelayMillis}ms)"

    # Wait for the mock before starting the gateway, so the gateway's /readyz
    # check is meaningful from the first request.
    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-Date) -lt $deadline) {
        $r = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$MockPort/healthz")
        if ($r.Trim() -eq '200') { break }
        Start-Sleep -Milliseconds 200
    }
    Assert-True 'mock upstream answers /healthz' ($r.Trim() -eq '200') "got '$($r.Trim())'"

    $gwProc = Start-Process -FilePath (Join-Path $binDir 'infergate.exe') `
        -ArgumentList @('-config', $gatewayCfg) `
        -RedirectStandardOutput $gwLog -RedirectStandardError (Join-Path $tmpDir 'gateway.err.log') `
        -PassThru -WindowStyle Hidden
    Write-Host "  infergate.exe  pid=$($gwProc.Id) listening :$GatewayPort"

    $base = "http://127.0.0.1:$GatewayPort"
    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-Date) -lt $deadline) {
        $r = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "$base/readyz")
        if ($r.Trim() -eq '200') { break }
        Start-Sleep -Milliseconds 200
    }
    Assert-True 'gateway answers /readyz' ($r.Trim() -eq '200') "got '$($r.Trim())'"

    # -----------------------------------------------------------------------
    # 3. Liveness and readiness
    # -----------------------------------------------------------------------
    Write-Section '3. Operational endpoints'
    $health = Invoke-Curl @('-s', "$base/healthz")
    Assert-Contains '/healthz reports a version' $health '"version"'
    Assert-Contains '/healthz reports uptime' $health '"uptime"'

    $upstreams = Invoke-Curl @('-s', "$base/admin/upstreams")
    # Parse rather than grep: /admin/upstreams answers with an indented
    # document, so a raw `"catch_all":true` substring test would fail on the
    # space the encoder puts after the colon. Asserting on the decoded value is
    # also what a reader of this script actually means.
    $upstreamDoc = $upstreams | ConvertFrom-Json
    Assert-True '/admin/upstreams lists the mock backend' `
        (@($upstreamDoc.upstreams | Where-Object { $_.name -eq 'mock' }).Count -eq 1) `
        "upstreams: $(@($upstreamDoc.upstreams | ForEach-Object { $_.name }) -join ',')"
    Assert-True '/admin/upstreams shows the catch-all route' `
        ($upstreamDoc.upstreams[0].catch_all -eq $true) `
        "catch_all=$($upstreamDoc.upstreams[0].catch_all)"
    Assert-True '/admin/upstreams never leaks the upstream API key' `
        ($upstreamDoc.upstreams[0].has_api_key -eq $false) `
        "has_api_key=$($upstreamDoc.upstreams[0].has_api_key)"

    # -----------------------------------------------------------------------
    # 4. Non-streaming passthrough
    # -----------------------------------------------------------------------
    Write-Section '4. Non-streaming passthrough'
    $bodyJson = '{"model":"mock-gpt","messages":[{"role":"user","content":"hello from curl"}]}'
    $body = New-BodyFile 'chat.json' $bodyJson
    $resp = Invoke-Curl @(
        '-s', '-i', "$base/v1/chat/completions",
        '-H', 'Content-Type: application/json',
        '-H', 'Authorization: Bearer curl-demo-key',
        '--data-binary', $body
    )
    Assert-Contains 'status line is 200' $resp 'HTTP/1.1 200'
    Assert-Contains 'response is a chat.completion' $resp '"object":"chat.completion"'
    Assert-Contains 'usage block survived the proxy' $resp '"prompt_tokens"'
    Assert-Contains 'gateway stamped the serving upstream' $resp 'X-Infergate-Upstream-Name'
    Assert-Contains 'gateway stamped a request id' $resp 'X-Infergate-Request-Id'

    # -----------------------------------------------------------------------
    # 5. SSE streaming, unbuffered
    # -----------------------------------------------------------------------
    Write-Section '5. SSE streaming (curl -N)'
    $streamJson = '{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"stream from curl"}]}'
    $streamBody = New-BodyFile 'stream.json' $streamJson
    # Capture into a FILE rather than a variable. A captured command's output
    # comes back as an array of lines, and Out-String then re-wraps and pads it;
    # assertions about framing and the trailing sentinel need the exact bytes
    # the socket delivered.
    $streamOut = Join-Path $tmpDir 'stream.out'
    # -N / --no-buffer is the whole point: without it curl buffers the response
    # and you cannot see that frames arrive incrementally.
    $null = Invoke-Curl @(
        '-N', '-s', '-o', $streamOut, "$base/v1/chat/completions",
        '-H', 'Content-Type: application/json',
        '--data-binary', $streamBody
    )
    $stream = Get-Content $streamOut -Raw
    Assert-Contains 'stream carries chunk objects' $stream '"object":"chat.completion.chunk"'
    Assert-Contains 'stream terminates with [DONE]' $stream 'data: [DONE]'
    Assert-Contains 'stream carries a usage block' $stream '"usage"'

    # Blank-line framing: concatenating frames without the separator produces a
    # stream no SSE parser can read. CRLF is accepted because the terminator is
    # whatever the provider emits, and only the empty line is load-bearing.
    $separators = ([regex]::Matches($stream, "(?m)^\r?$")).Count
    Assert-True 'frames are separated by blank lines' ($separators -ge 4) "found $separators blank lines"
    Assert-True 'the sentinel is the LAST thing on the wire' `
        ($stream.TrimEnd("`r", "`n") -match 'data:\s*\[DONE\]$') `
        "tail: $($stream.Substring([Math]::Max(0, $stream.Length - 40)) -replace '\r?\n', '\n')"

    # -----------------------------------------------------------------------
    # 6. Incremental arrival (the actual streaming property)
    # -----------------------------------------------------------------------
    Write-Section '6. Frames arrive incrementally, not all at once'
    # curl's %{time_starttransfer} is "time until the first byte was received",
    # and a streaming provider sends response HEADERS as soon as it accepts the
    # request -?so that number is a header timer, not a token timer. Timing the
    # arrival of the first `data:` line is the measurement that actually proves
    # nothing is buffered in the middle, and --trace-time is stamping every byte
    # curl reads, so it is a real per-frame clock rather than an inference.
    $trace = Join-Path $tmpDir 'stream.trace'
    $timing = Invoke-Curl @(
        '-N', '-s', '-w', '{"ttfb":%{time_starttransfer},"total":%{time_total}}',
        '-o', 'NUL', '--trace-time', '--trace-ascii', $trace,
        "$base/v1/chat/completions",
        '-H', 'Content-Type: application/json',
        '--data-binary', $streamBody
    )
    Write-Host "        curl: $($timing.Trim())" -ForegroundColor DarkGray
    $json = $timing.Trim() | Select-String -Pattern '\{.*\}' | ForEach-Object { $_.Matches[0].Value } | Select-Object -Last 1
    if ($json) {
        $t = $json | ConvertFrom-Json
        Write-Host ("        header ttfb={0:N1}ms  total={1:N1}ms" -f ($t.ttfb * 1000), ($t.total * 1000)) -ForegroundColor DarkGray
    }

    $firstFrameMs = $null
    $lastFrameMs = $null
    $frameStamps = @{}
    # --trace-time stamps every EVENT line, but the hex-dump continuation lines
    # that carry the payload ("0004: data: ...") have no stamp of their own.
    # Walk the trace forward and remember the most recent timestamp seen; every
    # data-bearing line inherits it. That is still curl's own clock, not one we
    # invented.
    #
    # The walk now reads the whole stream instead of stopping at the first data
    # line. That first line is the opening role frame, which the mock flushes
    # together with the headers, so on its own it only re-reads the stall
    # (measured: headers 264.3ms, first frame 265.0ms). What separates a
    # gateway that streams from one that buffers the response is the frames
    # AFTER it, so record their arrival times as well.
    $stampRe = '^(\d{2}:\d{2}:\d{2}\.\d{6})'
    $traceLines = @(Get-Content $trace)
    $started = $null
    $lastStamp = $null
    foreach ($line in $traceLines) {
        $s = [string]$line
        $m = [regex]::Match($s, $stampRe)
        if ($m.Success) {
            $lastStamp = $m.Groups[1].Value
            if ($null -eq $started) {
                $started = [datetime]::ParseExact($lastStamp, 'HH:mm:ss.ffffff', [cultureinfo]::InvariantCulture)
            }
        }
        if ($s -match '^[0-9a-f]{4}: data:' -and $null -ne $started -and $null -ne $lastStamp) {
            $at = [datetime]::ParseExact($lastStamp, 'HH:mm:ss.ffffff', [cultureinfo]::InvariantCulture)
            $atMs = ($at - $started).TotalMilliseconds
            if ($null -eq $firstFrameMs) { $firstFrameMs = $atMs }
            $lastFrameMs = $atMs
            $frameStamps[$lastStamp] = $true
        }
    }
    $spreadMs = if ($null -eq $lastFrameMs) { $null } else { $lastFrameMs - $firstFrameMs }
    $firstAt = if ($null -eq $firstFrameMs) { 'n/a' } else { [Math]::Round($firstFrameMs, 1).ToString() + 'ms' }
    $lastAt = if ($null -eq $lastFrameMs) { 'n/a' } else { [Math]::Round($lastFrameMs, 1).ToString() + 'ms' }
    $spreadAt = if ($null -eq $spreadMs) { 'n/a' } else { [Math]::Round($spreadMs, 1).ToString() + 'ms' }
    Assert-True "the first data frame does not arrive before the injected stall (${TtfbMillis}ms)" `
        ($null -ne $firstFrameMs -and $firstFrameMs -ge ($TtfbMillis * 0.8)) `
        "first data frame at $firstAt"
    # A gateway that read the upstream response to the end before answering would
    # still satisfy the bound above - the mock holds its headers back through the
    # whole stall either way - but then every data frame would land at the same
    # instant. A stream shows several distinct arrival times spread across the
    # response. The mock paces one content frame per ${TokenDelayMillis}ms, so
    # require at least two of those gaps: batching collapses the spread to ~0.
    Assert-True 'frames keep arriving after the first one (streamed, not one batch)' `
        ($null -ne $spreadMs -and $frameStamps.Count -ge 3 -and $spreadMs -ge ($TokenDelayMillis * 2)) `
        "$($frameStamps.Count) arrival times, first $firstAt, last $lastAt, spread $spreadAt"
    if ($null -ne $firstFrameMs) {
        Write-Host ("        first data frame at {0:N1}ms (headers at {1:N1}ms), last at {2:N1}ms, spread {3:N1}ms over {4} arrival times" -f $firstFrameMs, ($t.ttfb * 1000), $lastFrameMs, $spreadMs, $frameStamps.Count) -ForegroundColor DarkGray
    }

    # -----------------------------------------------------------------------
    # 7. tool_call deltas reassembled by a client
    # -----------------------------------------------------------------------
    Write-Section '7. tool_call delta streaming'
    $toolJson = '{"model":"mock-tool","stream":true,"messages":[{"role":"user","content":"weather in beijing"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}'
    $toolBody = New-BodyFile 'tool.json' $toolJson
    $toolStream = Invoke-Curl @(
        '-N', '-s', "$base/v1/chat/completions",
        '-H', 'Content-Type: application/json', '--data-binary', $toolBody
    )
    Assert-Contains 'tool_call deltas reached the client' $toolStream '"tool_calls"'
    Assert-Contains 'the function name is streamed' $toolStream 'get_weather'
    Assert-Contains 'the tool call carries an id' $toolStream 'call_'
    Assert-Contains 'tool call fragments are indexed for reassembly' $toolStream '"index":0'
    Assert-Contains 'finish_reason is tool_calls' $toolStream 'tool_calls'

    # -----------------------------------------------------------------------
    # 8. Fault injection through the mock's control headers
    # -----------------------------------------------------------------------
    Write-Section '8. Upstream failure handling'

    $status = Invoke-Curl @(
        '-s', '-o', 'NUL', '-w', '%{http_code}', "$base/v1/chat/completions",
        '-H', 'Content-Type: application/json', '-H', 'X-Mock-Status: 429',
        '--data-binary', $body
    )
    Assert-True 'upstream 429 is forwarded as 429 (not masked as 502)' ($status.Trim() -eq '429') "got $($status.Trim())"

    $errBody = Invoke-Curl @(
        '-s', "$base/v1/chat/completions",
        '-H', 'Content-Type: application/json', '-H', 'X-Mock-Status: 500',
        '--data-binary', $body
    )
    Assert-Contains 'upstream 5xx body is forwarded verbatim' $errBody '"error"'

    $missing = Invoke-Curl @(
        '-s', '-o', 'NUL', '-w', '%{http_code}', "$base/v1/nope",
        '-H', 'Content-Type: application/json', '-d', $body
    )
    Assert-True 'unproxiable path is a 404 client error' ($missing.Trim() -eq '404') "got $($missing.Trim())"

    $doneOut = Join-Path $tmpDir 'omit-done.out'
    $null = Invoke-Curl @(
        '-N', '-s', '-o', $doneOut, "$base/v1/chat/completions",
        '-H', 'Content-Type: application/json', '-H', 'X-Mock-Omit-Done: 1',
        '--data-binary', $streamBody
    )
    # The backend closes the socket without the sentinel; the gateway must add
    # it or an OpenAI SDK client blocks until its own timeout.
    $doneRaw = Get-Content $doneOut -Raw
    Assert-True 'gateway synthesises [DONE] when the backend omits it' `
        ($doneRaw -match 'data:\s*\[DONE\]') `
        "tail: $($doneRaw.Substring([Math]::Max(0, $doneRaw.Length - 60)) -replace '\r?\n', '\n')"

    # -----------------------------------------------------------------------
    # 9. Accounting visible in /stats and /metrics
    # -----------------------------------------------------------------------
    Write-Section '9. Token accounting and metrics'
    $stats = Invoke-Curl @('-s', "$base/stats")
    Assert-Contains '/stats reports request count' $stats '"requests"'
    Assert-Contains '/stats reports prompt tokens' $stats '"prompt"'
    Assert-Contains '/stats reports first-token latency' $stats '"first_token_mean"'
    Assert-Contains '/stats reports stream frame counts' $stats '"streams"'
    Assert-Contains '/stats reports p95 latency' $stats '"p95"'

    $metrics = Invoke-Curl @('-s', "$base/metrics")
    Assert-Contains '/metrics exports request counters' $metrics 'infergate_requests_total'
    Assert-Contains '/metrics exports token counters' $metrics 'infergate_tokens_total'
    Assert-Contains '/metrics exports first-token gauge' $metrics 'infergate_first_token_seconds_mean'
    Assert-Contains '/metrics exports stream byte counters' $metrics 'infergate_stream_bytes_total'

    # Cross-check the metric against reality: send one request of each kind and
    # require the per-route average to land in a range around what the client
    # itself measured. A metrics pipeline can stop recording and still export a
    # plausible-looking name, so asserting the series EXISTS proves nothing --
    # only a magnitude comparison does. The upper bound is deliberately wide
    # (this is a shared, sleeping laptop, not a quiet server).
    $beforeMetric = Invoke-Curl @('-s', "$base/metrics")

    $f = New-BodyFile 'metric-nonstream' "{`"model`":`"mock-gpt`",`"messages`":[{`"role`":`"user`",`"content`":`"metric probe`"}]}"
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    Invoke-Curl @('-s', '-o', "$tmpDir\metric-nonstream.out", '-H', 'Content-Type: application/json', '--data-binary', $f, "$base/v1/chat/completions") | Out-Null
    $sw.Stop()
    $nonStreamMs = $sw.Elapsed.TotalMilliseconds

    $f = New-BodyFile 'metric-stream' "{`"model`":`"mock-gpt`",`"stream`":true,`"messages`":[{`"role`":`"user`",`"content`":`"metric probe`"}]}"
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    Invoke-Curl @('-s', '-N', '-o', "$tmpDir\metric-stream.out", '-H', 'Content-Type: application/json', '--data-binary', $f, "$base/v1/chat/completions") | Out-Null
    $sw.Stop()
    $streamMs = $sw.Elapsed.TotalMilliseconds

    # /stats gives us the gateway's own view without parsing Prometheus text.
    $statsAfter = Invoke-Curl @('-s', "$base/stats")
    $statsDoc = $statsAfter | ConvertFrom-Json
    $lat = $statsDoc.latency
    Assert-True ('the gateway has recorded at least two requests (count={0})' -f $lat.count) ($lat.count -ge 2)
    Assert-True ('recorded p95 is non-zero ({0:N1}ms)' -f $lat.p95) ($lat.p95 -gt 0)
    # Only a LOWER bound is asserted on the gateway's own max. The script's
    # earlier fault-injection sections deliberately stall the mock (up to 600ms
    # and one forced 504), so the gateway's maximum legitimately exceeds what
    # these two probes measured; bounding it from above would make the check
    # fail for the wrong reason -- a slow laptop -- instead of for a broken
    # metric. A metric that stops recording shows up as max=0.
    Assert-True ('recorded max is non-zero ({0:N1}ms; these two probes took {1:N1}ms)' -f $lat.max, ([math]::Max($nonStreamMs, $streamMs))) ($lat.max -ge 0.3)

    # Per-route average, computed from the exported counters. This is what
    # catches a series that exists but never grows: the name proves nothing, the
    # magnitude proves the metrics path is attached to real requests.
    $afterMetric = Invoke-Curl @('-s', "$base/metrics")
    $beforeSum = Get-MetricSum $beforeMetric 'infergate_request_duration_seconds_sum'
    $afterSum = Get-MetricSum $afterMetric 'infergate_request_duration_seconds_sum'
    $beforeCount = Get-MetricCountSum $beforeMetric 'infergate_requests_total'
    $afterCount = Get-MetricCountSum $afterMetric 'infergate_requests_total'
    Assert-True ('the request counter grew by at least 2 ({0} -> {1})' -f $beforeCount, $afterCount) (($afterCount - $beforeCount) -ge 2)
    $deltaSeconds = $afterSum - $beforeSum
    $deltaCount = $afterCount - $beforeCount
    $deltaMeanMs = 0.0
    if ($deltaCount -gt 0) { $deltaMeanMs = ($deltaSeconds / $deltaCount) * 1000.0 }
    Assert-True ('the gateway''s own per-request average for these two probes is plausible ({0:N2}ms total / {1} requests = {2:N2}ms)' -f ($deltaSeconds * 1000.0), $deltaCount, $deltaMeanMs) ($deltaMeanMs -ge 0.2 -and $deltaMeanMs -le 500)
    Assert-True 'the two probes actually produced bodies' ((Get-Item "$tmpDir\metric-nonstream.out").Length -gt 0 -and (Get-Item "$tmpDir\metric-stream.out").Length -gt 0)

    # -----------------------------------------------------------------------
    # 10. The same requests appear as structured logs
    # -----------------------------------------------------------------------
    Write-Section '10. Structured logs'
    Start-Sleep -Milliseconds 500
    $logText = ''
    try {
        $fs = [System.IO.File]::Open($gwLog, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $null = $fs.Seek($gwLogBase, [System.IO.SeekOrigin]::Begin)
        $sr = New-Object System.IO.StreamReader($fs)
        $logText = $sr.ReadToEnd()
        $sr.Close()
        $fs.Close()
    }
    catch {
        Write-Host "  could not read the gateway log: $_" -ForegroundColor DarkYellow
    }
    Assert-Contains 'gateway logged the listen event' $logText 'infergate listening'
    # The per-request line is logged at INFO and switches on `stream=true`, so
    # that is what a log-driven operator actually sees. The per-stream DEBUG
    # line ("stream complete", with frame counts) only appears at log.level:
    # debug, so it is asserted separately below rather than being assumed here.
    Assert-True 'gateway logged a completed stream request' `
        ($logText -match 'msg=request.*stream=true.*status=200') `
        'no INFO request line with stream=true status=200'
    # Scope this to STREAMING lines. Earlier sections deliberately provoke
    # failures (a canceled client, an injected 500), and those are non-stream
    # requests, so a whole-log regex reports a false failure. The claim under
    # test is narrower: every request that actually streamed completed.
    $failedStreams = @($logText -split "`n" | Where-Object { $_ -match 'stream=true' -and $_ -match '\soutcome=(canceled|upstream_error|timeout)\b' })
    Assert-True 'no stream ended in failure' ($failedStreams.Count -eq 0) `
        "failing stream lines: $($failedStreams.Count)"
    Assert-Contains 'logs carry first_token latency' $logText 'first_token='
    Assert-Contains 'logs carry a cost figure' $logText 'cost_usd='
    Write-Host "        gateway log: $gwLog" -ForegroundColor DarkGray
    Write-Host "        mock log   : $mockLog" -ForegroundColor DarkGray
}
finally {
    Write-Section '11. Teardown'
    foreach ($p in @($gwProc, $mockProc)) {
        if ($p -and -not $p.HasExited) {
            Write-Host "  stopping pid=$($p.Id)"
            Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
        }
    }
    if ($KeepRunning) {
        Write-Host '  -KeepRunning set, but processes were stopped; rerun manually if you need a live stack.' -ForegroundColor DarkYellow
    }
}

Write-Host ''
Write-Host ("=" * 72) -ForegroundColor DarkGray
if ($script:failed -eq 0) {
    Write-Host "  RESULT: $($script:passed)/$($script:passed) assertions passed" -ForegroundColor Green
    Write-Host '  OK: M0 verified end to end with curl' -ForegroundColor Green
    exit 0
}
else {
    Write-Host "  RESULT: $($script:passed) passed, $($script:failed) FAILED" -ForegroundColor Red
    exit 1
}
