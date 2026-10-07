# InferGate M6 -- end-to-end verification with curl.exe.
#
# M6 is what an AGENT platform needs from a gateway: a retried tool call must not
# be billed twice or answered twice, and a conversation must be visible as a
# conversation rather than as an undifferentiated stream of completions.
#
# The claims that matter here are all about what did NOT happen upstream:
#
#   a replay caused ZERO extra provider calls   -> witnessed by the mock's /calls
#   a conflict called the provider ZERO times   -> same witness
#   a stream replayed as the SAME transcript    -> compared frame by frame
#   a conversation's cost came from the book    -> /admin/sessions, computed here
#
# /calls is the reason this script exists next to cmd/verify-m6: the in-process
# gate can only observe the upstreams it built in the same process, while this
# one counts the requests that really crossed a socket.
#
# It complements two other gates rather than replacing them:
#
#   cmd/verify-m6   in-process, 381 assertions, ~15s, exit code is the CI gate.
#   this script     real binaries, a scripted tool-calling model, and a shell you
#                   can poke at afterwards. This is what a reviewer runs.
#
# Usage (pwsh does not exist on this host):
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m6.ps1
#
# Everything is confined to this repository: binaries in bin\, logs, bodies and
# the scripted mock's script in tmp\, and both processes are stopped in finally.

[CmdletBinding()]
param(
    [int]$GatewayPort = 18909,
    [int]$MockPort = 19910,
    [int]$TokenDelayMillis = 2,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $repo 'bin'
$tmpDir = Join-Path $repo 'tmp'
$gwLog = Join-Path $tmpDir 'm6-gateway.log'
$gwErrLog = Join-Path $tmpDir 'm6-gateway.err.log'
$mockLog = Join-Path $tmpDir 'm6-mock.log'
$mockErrLog = Join-Path $tmpDir 'm6-mock.err.log'
$mockURL = "http://127.0.0.1:$MockPort"
$gwURL = "http://127.0.0.1:$GatewayPort"

$script:passed = 0
$script:failed = 0
$script:procs = @()

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

function Assert-Close {
    param([string]$Label, [double]$Expected, [double]$Actual, [double]$Tolerance = 1e-9)
    Assert-True $Label ([Math]::Abs($Expected - $Actual) -le $Tolerance) "expected $Expected, got $Actual"
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

function Get-ConfigScalar {
    param([string]$Text, [string]$Block, [string]$Key)
    # A top-level block is named at column zero and owns everything indented
    # under it, up to the next column-zero line. Scoping the lookup that way is
    # what lets `ttl` come from the replay store rather than from whichever
    # `ttl` happens to appear first in the file.
    $b = [regex]::Match($Text, "(?ms)^$([regex]::Escape($Block)):[^\r\n]*\r?\n(.*?)(?=^\S|\z)")
    if (-not $b.Success) { return $null }
    $k = [regex]::Match($b.Groups[1].Value, "(?m)^\s*$([regex]::Escape($Key)):\s*([^\s#]+)")
    if (-not $k.Success) { return $null }
    return $k.Groups[1].Value.Trim('"')
}

function Get-DurationSeconds {
    param([string]$Text)
    # Go renders a ten-minute TTL as "10m0s" and the config writes "10m": the
    # same value in two spellings. Both sides are folded to seconds before they
    # are compared, so the gate is testing the value and not the spelling.
    # `ms` leads the alternation because a bare `m` would eat its first letter.
    $total = 0.0
    $seen = $false
    foreach ($m in [regex]::Matches("$Text", '([0-9]+(?:\.[0-9]+)?)(ms|h|m|s)')) {
        $v = [double]$m.Groups[1].Value
        switch ($m.Groups[2].Value) {
            'ms' { $total += $v / 1000.0 }
            's' { $total += $v }
            'm' { $total += $v * 60.0 }
            'h' { $total += $v * 3600.0 }
        }
        $seen = $true
    }
    if (-not $seen) { return $null }
    return $total
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
        [string]$Tag = 'resp',
        [switch]$Stream
    )
    $bodyPath = Join-Path $tmpDir "$Tag.body"
    $hdrPath = Join-Path $tmpDir "$Tag.hdr"
    $a = @('-s', '-o', $bodyPath, '-D', $hdrPath, '-w', '%{http_code}')
    if ($Stream) { $a += '-N' }
    $a += @($Url, '-H', 'Content-Type: application/json', '--data-binary', $BodyFile)
    foreach ($h in $ExtraHeaders) { $a += @('-H', $h) }
    $code = (Invoke-Curl $a).Trim()
    $status = 0
    if ($code -match '^\d+$') { $status = [int]$code }
    $headers = Read-Text $hdrPath
    return [pscustomobject]@{
        Status     = $status
        Headers    = $headers
        Body       = (Read-Text $bodyPath)
        Name       = (Get-Header $headers 'X-InferGate-Upstream-Name')
        Tried      = (Get-Header $headers 'X-InferGate-Tried')
        Attempt    = (Get-Header $headers 'X-InferGate-Attempt')
        RequestID  = (Get-Header $headers 'X-InferGate-Request-Id')
        Replay     = (Get-Header $headers 'X-InferGate-Idempotent-Replay')
        Origin     = (Get-Header $headers 'X-InferGate-Idempotent-Origin')
        # The caller's own header is reused for the echo, so a client that sent
        # Idempotency-Key reads the same name back rather than a gateway one.
        IdemKey    = (Get-Header $headers 'Idempotency-Key')
        IdemUp     = (Get-Header $headers 'X-InferGate-Idempotent-Upstream')
        IdemAge    = (Get-Header $headers 'X-InferGate-Idempotent-Age')
        Cache      = (Get-Header $headers 'X-InferGate-Cache')
    }
}

function Get-Text {
    param([string]$Url)
    return (Invoke-Curl @('-s', $Url))
}

function Get-Json {
    param([string]$Url, [string]$Label)
    $raw = Get-Text $Url
    try { return ($raw | ConvertFrom-Json) }
    catch {
        Assert-True "$Label is JSON" $false "$_`n        body: $(if ($raw) { $raw.Substring(0, [Math]::Min(300, $raw.Length)) })"
        return $null
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

        With no labels, the family is summed over every sample AND every
        label set, including the families that carry no labels at all (the M6
        gauges are written as `infergate_idempotency_entries 2`, with no braces),
        which a brace-requiring pattern would silently score as zero.
    #>
    param([string]$Text, [string]$Series, [string]$Labels = '')
    if ($Labels -eq '') {
        $pattern = '(?m)^' + [regex]::Escape($Series) + '(?:\{[^}]*\})?\s+([0-9.eE+-]+)\s*$'
    }
    else {
        $pattern = '(?m)^' + [regex]::Escape($Series) + '\{(?=[^}]*' + $Labels + ')[^}]*\}\s+([0-9.eE+-]+)\s*$'
    }
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

function Get-Status {
    param([string]$Url)
    $code = (Invoke-Curl @('-s', '-o', (Join-Path $tmpDir 'probe.out'), '-w', '%{http_code}', $Url)).Trim()
    if ($code -match '^\d+$') { return [int]$code }
    return 0
}

function Wait-Http {
    param([string]$Url, [string]$Label, [int]$TimeoutSeconds = 25)
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        if ((Get-Status $Url) -eq 200) { return $true }
        Start-Sleep -Milliseconds 200
    }
    Write-Host "  timed out waiting for $Label at $Url" -ForegroundColor DarkYellow
    return $false
}

function Start-Stack {
    param([string]$Exe, [string[]]$Arguments, [string]$Tag)
    $out = Join-Path $tmpDir "$Tag.log"
    $err = Join-Path $tmpDir "$Tag.err.log"
    $p = Start-Process -FilePath $Exe -ArgumentList $Arguments -RedirectStandardOutput $out `
        -RedirectStandardError $err -PassThru -NoNewWindow
    $script:procs += $p
    return $p
}

# ---------------------------------------------------------------------------
# The scripted model.
#
# One file drives a deterministic tool-calling model, and it is the whole reason
# this gate needs no provider account: a request that brings tools gets a tool
# call, a request that brings a tool RESULT gets a final answer that echoes it,
# a request containing "flaky" fails the first time and succeeds after, and a
# streamed request gets real SSE frames.
#
# Rule order matters: the first matching rule wins, and `when` fields that are
# absent are compared against false, so a rule without `tools_present` matches
# only a tool-less request.
# ---------------------------------------------------------------------------
$mockScript = @'
{
  "default": {
    "content": "mock answer",
    "usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}
  },
  "rules": [
    {
      "name": "tool_call",
      "when": {"tools_present": true},
      "respond": {
        "content": "",
        "tool_calls": [{"id": "call_abc123", "name": "get_weather", "arguments": {"city": "Beijing"}}],
        "usage": {"prompt_tokens": 11, "completion_tokens": 5, "total_tokens": 16}
      }
    },
    {
      "name": "tool_result",
      "when": {"has_tool_result": true},
      "respond": {
        "echo_tool_result": true,
        "usage": {"prompt_tokens": 21, "completion_tokens": 7, "total_tokens": 28}
      }
    },
    {
      "name": "flaky",
      "when": {"contains": "flaky"},
      "respond": {
        "fail_first": 1,
        "error_body": {"error": {"message": "provider is restarting", "type": "server_error"}},
        "content": "the provider recovered",
        "usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}
      }
    },
    {
      "name": "streamed",
      "when": {"stream": true},
      "respond": {
        "content": "streamed agent answer",
        "usage": {"prompt_tokens": 9, "completion_tokens": 4, "total_tokens": 13}
      }
    }
  ]
}
'@

function New-ChatBody {
    param([string]$User, [switch]$Tools, [switch]$Stream)
    $obj = [ordered]@{
        model    = 'mock-gpt'
        messages = @(@{ role = 'user'; content = $User })
        stream   = [bool]$Stream
    }
    if ($Tools) {
        $obj['tools'] = @(@{
                type     = 'function'
                function = @{
                    name        = 'get_weather'
                    description = 'current weather for a city'
                    parameters  = @{
                        type       = 'object'
                        properties = @{ city = @{ type = 'string' } }
                        required   = @('city')
                    }
                }
            })
    }
    return ($obj | ConvertTo-Json -Depth 10 -Compress)
}

function New-ToolResultBody {
    param([string]$Result)
    # The second turn of an agent conversation: the assistant's tool call plus the
    # result the runtime is holding. This is the shape Warden persists.
    $obj = [ordered]@{
        model    = 'mock-gpt'
        stream   = $false
        messages = @(
            @{ role = 'user'; content = 'what is the weather in Beijing' },
            @{
                role       = 'assistant'
                content    = ''
                tool_calls = @(@{
                        id       = 'call_abc123'
                        type     = 'function'
                        function = @{ name = 'get_weather'; arguments = '{"city": "Beijing"}' }
                    })
            },
            @{ role = 'tool'; tool_call_id = 'call_abc123'; content = $Result }
        )
    }
    return ($obj | ConvertTo-Json -Depth 10 -Compress)
}

try {
    # -----------------------------------------------------------------------
    Write-Section '0. Preconditions'
    # -----------------------------------------------------------------------
    Assert-True 'the repository root is where this script thinks it is' (Test-Path (Join-Path $repo 'go.mod')) $repo
    Assert-True 'the gateway config sample exists' (Test-Path (Join-Path $repo 'configs\agent-local.yaml'))
    Assert-True 'the Go toolchain wrapper exists' (Test-Path (Join-Path $repo 'tools\go.cmd'))
    Assert-True 'curl.exe is on PATH' ($null -ne (Get-Command curl.exe -ErrorAction SilentlyContinue))
    foreach ($port in @($GatewayPort, $MockPort)) {
        $busy = $false
        try {
            $client = New-Object System.Net.Sockets.TcpClient
            $client.Connect('127.0.0.1', $port)
            $busy = $true
            $client.Close()
        }
        catch { $busy = $false }
        Assert-True "port $port is free" (-not $busy) 'something is already listening there'
    }
    if (-not (Test-Path $tmpDir)) { New-Item -ItemType Directory -Path $tmpDir | Out-Null }

    # -----------------------------------------------------------------------
    Write-Section '1. Build'
    # -----------------------------------------------------------------------
    & (Join-Path $repo 'tools\go.cmd') build -o (Join-Path $binDir 'infergate.exe') .\cmd\infergate
    Assert-Equal 'the gateway builds' 0 $LASTEXITCODE
    & (Join-Path $repo 'tools\go.cmd') build -o (Join-Path $binDir 'mockupstream.exe') .\cmd\mockupstream
    Assert-Equal 'the mock upstream builds' 0 $LASTEXITCODE
    Assert-True 'bin\infergate.exe exists' (Test-Path (Join-Path $binDir 'infergate.exe'))
    Assert-True 'bin\mockupstream.exe exists' (Test-Path (Join-Path $binDir 'mockupstream.exe'))

    # -----------------------------------------------------------------------
    Write-Section '2. The agent stack boots'
    # -----------------------------------------------------------------------
    $scriptPath = Join-Path $tmpDir 'm6-script.json'
    [System.IO.File]::WriteAllText($scriptPath, $mockScript, $utf8NoBom)

    # The shipped sample names the ports this script defaults to; rewriting them
    # keeps the sample itself untouchable, which is the difference between
    # testing the artifact and testing a copy of it that drifts.
    $cfgPath = Join-Path $tmpDir 'm6-gateway.yaml'
    $cfg = Read-Text (Join-Path $repo 'configs\agent-local.yaml')
    $cfg = $cfg -replace '127\.0\.0\.1:18909', "127.0.0.1:$GatewayPort"
    $cfg = $cfg -replace '127\.0\.0\.1:19910', "127.0.0.1:$MockPort"
    [System.IO.File]::WriteAllText($cfgPath, $cfg, $utf8NoBom)
    Assert-Contains 'the rendered config listens where the script expects' $cfg "127.0.0.1:$GatewayPort"
    Assert-Contains 'the rendered config points at the mock' $cfg "127.0.0.1:$MockPort"

    # Sections 3 and 7 assert what the RUNNING gateway reports about the model
    # table and the replay store. Those numbers belong to the config this script
    # hands it, so they are read back out of that text: retuning the sample has
    # to move the expectation with it, instead of turning a gateway that did
    # exactly what it was configured to do red.
    $policyContextWindow = Get-ConfigScalar -Text $cfg -Block 'models' -Key 'context_window'
    $policyReplayCapacity = Get-ConfigScalar -Text $cfg -Block 'idempotency' -Key 'capacity'
    $policyReplayTTL = Get-ConfigScalar -Text $cfg -Block 'idempotency' -Key 'ttl'
    $policyReplayMaxBytes = Get-ConfigScalar -Text $cfg -Block 'idempotency' -Key 'max_response_bytes'
    Assert-True 'the rendered config declares the model context window' `
        ($null -ne $policyContextWindow) "read $cfgPath"
    Assert-True 'the rendered config declares the replay store settings' `
        ($null -ne $policyReplayCapacity -and $null -ne $policyReplayTTL -and $null -ne $policyReplayMaxBytes) "read $cfgPath"

    & (Join-Path $binDir 'infergate.exe') -config $cfgPath -check | Out-Null
    Assert-Equal 'the rendered config validates' 0 $LASTEXITCODE

    Start-Stack (Join-Path $binDir 'mockupstream.exe') `
        @('-listen', "127.0.0.1:$MockPort", '-name', 'agent-mock', '-token-delay', "${TokenDelayMillis}ms", '-script', $scriptPath) `
        'm6-mock' | Out-Null
    Assert-True 'the scripted mock answers /healthz' (Wait-Http "$mockURL/healthz" 'the mock')

    Start-Stack (Join-Path $binDir 'infergate.exe') @('-config', $cfgPath) 'm6-gateway' | Out-Null
    Assert-True 'the gateway answers /healthz' (Wait-Http "$gwURL/healthz" 'the gateway')
    Assert-Equal 'the gateway reports ready' 200 (Get-Status "$gwURL/readyz")

    $upstreams = Get-Json "$gwURL/admin/upstreams" 'upstreams'
    if ($upstreams) {
        Assert-Equal 'one backend is configured' 1 $upstreams.upstreams.Count
        Assert-Equal 'the backend is the scripted mock' 'scripted-mock' $upstreams.upstreams[0].name
        Assert-True 'the backend is a catch-all, so any model lands somewhere' ([bool]$upstreams.upstreams[0].catch_all)
        # A catch-all backend claims no model BY NAME, so the index is empty: the
        # fallback is a routing rule, not an entry in the model table.
        Assert-Equal 'a catch-all backend claims no model by name' 0 @($upstreams.model_index.PSObject.Properties).Count
        Assert-Equal 'the routing strategy is the configured one' 'priority' $upstreams.routing.strategy
        Assert-Equal 'the breaker starts closed' 'closed' $upstreams.breaker_states.'scripted-mock'
    }
    $calls = Get-Json "$mockURL/calls" 'calls'
    if ($calls) { Assert-Equal 'the mock has seen no traffic yet' 0 $calls.calls }

    # -----------------------------------------------------------------------
    Write-Section '3. The capability surface'
    # -----------------------------------------------------------------------
    $caps = Get-Json "$gwURL/v1/capabilities" 'capabilities'
    if ($caps) {
        Assert-Equal 'the gateway declares one model' 1 $caps.model_count
        Assert-Equal "the declared context window is the config's $policyContextWindow" ([int]$policyContextWindow) $caps.models[0].context_window
        Assert-True 'the model is served by the scripted backend' ($caps.models[0].upstreams -contains 'scripted-mock')
        Assert-True 'the model is available while the breaker is closed' ([bool]$caps.models[0].available)
        Assert-True 'the supported capabilities are reported' ($caps.models[0].capabilities -contains 'chat')
    }

    $probeBody = '{"capabilities": ["chat", "embeddings"], "model": "mock-gpt"}'
    $probeFile = New-BodyFile 'm6-probe.json' $probeBody
    $probe = (Invoke-Curl @('-s', "$gwURL/v1/capabilities/probe", '-H', 'Content-Type: application/json',
            '--data-binary', $probeFile)) | ConvertFrom-Json
    Assert-Equal 'both requested capabilities were probed' 2 $probe.count
    Assert-Equal 'both were accepted by the scripted backend' 2 $probe.accepted
    Assert-Equal 'the chat probe went to the chat route' '/v1/chat/completions' $probe.probes[0].path
    Assert-Equal 'the embeddings probe went to the embeddings route' '/v1/embeddings' $probe.probes[1].path
    Assert-Contains 'the answer says what accepted does and does not mean' $probe.note 'not a statement about answer quality'

    $badProbeFile = New-BodyFile 'm6-probe-bad.json' '{"capabilities": ["telepathy"]}'
    $badStatus = (Invoke-Curl @('-s', '-o', (Join-Path $tmpDir 'bad-probe.body'), '-w', '%{http_code}',
            "$gwURL/v1/capabilities/probe", '-H', 'Content-Type: application/json', '--data-binary', $badProbeFile)).Trim()
    Assert-Equal 'an unknown capability is refused' '400' $badStatus
    Assert-Contains 'and the refusal lists what can be probed' (Read-Text (Join-Path $tmpDir 'bad-probe.body')) 'unknown capability'

    # -----------------------------------------------------------------------
    Write-Section '4. An agent conversation over the wire'
    # -----------------------------------------------------------------------
    $sessionHeaders = @('X-InferGate-Session: agent-1', 'X-InferGate-Tenant: team-a')
    # The capability probes above really did call the backend, so the provider's
    # counter is read as a DELTA around the conversation.
    $beforeTurns = (Get-Json "$mockURL/calls" 'calls before the conversation').calls
    $turn1Body = New-ChatBody -User 'what is the weather in Beijing' -Tools
    $turn1File = New-BodyFile 'm6-turn1.json' $turn1Body
    $turn1 = Send-Chat -Url "$gwURL/v1/chat/completions" -BodyFile $turn1File -ExtraHeaders $sessionHeaders -Tag 'm6-turn1'
    Assert-Equal 'the first turn is answered' 200 $turn1.Status
    Assert-Equal 'the scripted backend is named' 'scripted-mock' $turn1.Name
    Assert-Equal 'one attempt was made' '1' $turn1.Attempt
    Assert-Equal 'no failover header is set when the first candidate answers' '' $turn1.Tried
    Assert-Contains 'the model asked for a tool' $turn1.Body 'call_abc123'
    Assert-Contains 'and marked the turn as a tool call' $turn1.Body 'tool_calls'
    Assert-Contains 'the prompt tokens are the tools-call count' $turn1.Body '"prompt_tokens":11'
    Assert-True 'the gateway stamped a request id' ($turn1.RequestID -ne '')

    $turn2Body = New-ToolResultBody -Result '18C and sunny'
    $turn2File = New-BodyFile 'm6-turn2.json' $turn2Body
    $turn2 = Send-Chat -Url "$gwURL/v1/chat/completions" -BodyFile $turn2File -ExtraHeaders $sessionHeaders -Tag 'm6-turn2'
    Assert-Equal 'the second turn is answered' 200 $turn2.Status
    Assert-Contains 'the tool result travelled to the model' $turn2.Body '18C and sunny'
    Assert-Contains 'as the answer the model wrote from it' $turn2.Body '"content":"18C and sunny"'
    Assert-Contains 'and finished the turn normally' $turn2.Body '"finish_reason":"stop"'
    Assert-Contains 'the tool-result token count is the second turn' $turn2.Body '"prompt_tokens":21'

    $calls = Get-Json "$mockURL/calls" 'calls'
    if ($calls) {
        Assert-Equal 'exactly two provider calls carried the conversation' 2 ($calls.calls - $beforeTurns)
        Assert-Equal 'one was the tool call' 1 $calls.by_rule.tool_call
        Assert-Equal 'one was the tool result' 1 $calls.by_rule.tool_result
        Assert-Equal 'one tool call was emitted' 1 $calls.tool_calls_emitted
    }

    # -----------------------------------------------------------------------
    Write-Section '5. The conversation is a row, not a puddle'
    # -----------------------------------------------------------------------
    $sessions = Get-Json "$gwURL/admin/sessions" 'sessions'
    if ($sessions) {
        Assert-True 'the ledger is enabled' ([bool]$sessions.enabled)
        Assert-Equal 'one conversation is tracked' 1 $sessions.tracked
        Assert-Equal 'the conversation keeps its runtime id' 'agent-1' $sessions.sessions[0].id
        Assert-Equal 'and its tenant' 'team-a' $sessions.sessions[0].tenant
        Assert-Equal 'both turns are in it' 2 $sessions.sessions[0].requests
        Assert-Equal 'both succeeded' 2 $sessions.sessions[0].ok
        Assert-Equal 'prompt tokens are summed over the conversation' 32 $sessions.sessions[0].prompt_tokens
        Assert-Equal 'completion tokens too' 12 $sessions.sessions[0].completion_tokens
        Assert-True 'the served model is recorded' ($null -ne $sessions.sessions[0].models.'mock-gpt')
        Assert-True 'the backend that served it is recorded' ($null -ne $sessions.sessions[0].upstreams.'scripted-mock')
        Assert-Equal 'the recent turns are kept' 2 $sessions.sessions[0].recent.Count
        # (11 prompt + 5 completion) + (21 + 7): 1.0/1e6 in, 3.0/1e6 out.
        Assert-Close 'the cost comes from the configured price book' 0.000068 $sessions.sessions[0].cost_usd
    }

    $one = Get-Json "$gwURL/admin/sessions/agent-1" 'session lookup'
    if ($one) { Assert-Equal 'a session is addressable across tenants' 'agent-1' $one.id }
    Assert-Equal 'an unknown session is a 404' 404 (Get-Status "$gwURL/admin/sessions/nobody")
    $filtered = Get-Json "$gwURL/admin/sessions?tenant=team-b" 'sessions for another tenant'
    if ($filtered) {
        # `tracked` is the ledger's whole size; `count` is the filtered page, so
        # a tenant filter is asserted on the page, not on the total.
        Assert-Equal 'another tenant sees no conversations' 0 $filtered.count
        Assert-Equal 'and none of its sessions leak in' 0 $filtered.sessions.Count
        Assert-Equal 'while the ledger still reports what it holds' 1 $filtered.tracked
    }

    $metrics = Get-Text "$gwURL/metrics"
    Assert-Equal 'the tracked-session gauge is exported' 1 (Get-SeriesValue $metrics 'infergate_sessions_tracked')
    Assert-Equal 'the request gauge is exported' 2 (Get-SeriesValue $metrics 'infergate_sessions_requests')
    Assert-Equal 'the conversation token gauge is exported' 32 (Get-SeriesValue $metrics 'infergate_sessions_tokens' 'kind="prompt"')

    # -----------------------------------------------------------------------
    Write-Section '6. A retried tool call is not paid for twice'
    # -----------------------------------------------------------------------
    $keyedBody = New-ChatBody -User 'book a table for two'
    $keyedFile = New-BodyFile 'm6-keyed.json' $keyedBody
    $keyHeaders = @('Idempotency-Key: table-1')
    $before = (Get-Json "$mockURL/calls" 'calls before the retry').calls
    $first = Send-Chat -Url "$gwURL/v1/chat/completions" -BodyFile $keyedFile -ExtraHeaders $keyHeaders -Tag 'm6-first'
    Assert-Equal 'the first attempt is answered' 200 $first.Status
    Assert-Equal 'the key is echoed so a runtime can correlate the attempt' 'table-1' $first.IdemKey
    Assert-Equal 'the first attempt says it is not a replay' 'false' $first.Replay

    $replay = Send-Chat -Url "$gwURL/v1/chat/completions" -BodyFile $keyedFile -ExtraHeaders $keyHeaders -Tag 'm6-replay'
    Assert-Equal 'the retry is answered identically' 200 $replay.Status
    Assert-Equal 'and says it is a replay' 'true' $replay.Replay
    Assert-Equal 'the replay names the request that did the work' $first.RequestID $replay.Origin
    Assert-Equal 'the replay is attributed to no backend' 'replay' $replay.Name
    Assert-Equal 'and separately names the backend that did the work' 'scripted-mock' $replay.IdemUp
    Assert-True 'the answer reports its age' ($replay.IdemAge -ne '')
    Assert-Equal 'the replayed bytes are the stored bytes' $first.Body $replay.Body

    $afterReplay = (Get-Json "$mockURL/calls" 'calls after the replay').calls
    Assert-Equal 'only ONE provider call happened for two client attempts' 1 ($afterReplay - $before)

    $conflictFile = New-BodyFile 'm6-conflict.json' (New-ChatBody -User 'a completely different request')
    $conflict = Send-Chat -Url "$gwURL/v1/chat/completions" -BodyFile $conflictFile -ExtraHeaders $keyHeaders -Tag 'm6-conflict'
    Assert-Equal 'the same key with a different body is refused' 409 $conflict.Status
    Assert-Contains 'with the conflict type' $conflict.Body '"type":"infergate_idempotency_conflict"'
    $afterConflict = (Get-Json "$mockURL/calls" 'calls after the conflict').calls
    Assert-Equal 'and the provider was not called at all' $afterReplay $afterConflict

    $otherFile = New-BodyFile 'm6-other.json' (New-ChatBody -User 'a third request')
    $other = Send-Chat -Url "$gwURL/v1/chat/completions" -BodyFile $otherFile -ExtraHeaders @('Idempotency-Key: table-2') -Tag 'm6-other'
    Assert-Equal 'a different key is a different request' 200 $other.Status
    Assert-Equal 'and reaches the provider' 1 ((Get-Json "$mockURL/calls" 'calls after a new key').calls - $afterConflict)
    Assert-Equal 'a request with no key is never a replay' 'false' $other.Replay

    # -----------------------------------------------------------------------
    Write-Section '7. The replay store reports what it holds'
    # -----------------------------------------------------------------------
    $idem = Get-Json "$gwURL/admin/idempotency" 'idempotency'
    if ($idem) {
        Assert-True 'the store is enabled' ([bool]$idem.enabled)
        Assert-Equal "the capacity is the config's $policyReplayCapacity" ([int]$policyReplayCapacity) $idem.capacity
        Assert-Close "the TTL is the config's $policyReplayTTL" (Get-DurationSeconds $policyReplayTTL) (Get-DurationSeconds $idem.ttl) 0.001
        Assert-Equal "the response size ceiling is the config's $policyReplayMaxBytes" ([int]$policyReplayMaxBytes) $idem.max_response_bytes
        Assert-Equal 'two keys are stored' 2 $idem.stored
        Assert-True 'the caller scopes entries' ($idem.scopes -ge 1)
        Assert-True 'a lookup was recorded' ($idem.stats.lookups -ge 3)
        Assert-Equal 'one lookup was a hit' 1 $idem.stats.hits
        Assert-Equal 'one was refused as a conflict' 1 $idem.stats.conflicts
        Assert-Equal 'the listing returns the stored keys' 2 $idem.entries.Count
        Assert-True 'the listing names a key' ($idem.entries[0].key -ne '')
        Assert-True 'the stored answer body is NOT exposed by the admin surface' (-not $idem.entries[0].PSObject.Properties.Name.Contains('body'))
    }
    $idemRaw = Get-Text "$gwURL/admin/idempotency"
    Assert-True 'the answer text never appears in the admin listing' (-not $idemRaw.Contains('mock answer'))

    $metrics = Get-Text "$gwURL/metrics"
    Assert-True 'lookups are counted' ((Get-SeriesValue $metrics 'infergate_idempotency_lookups_total') -ge 3)
    Assert-Equal 'one replay is counted' 1 (Get-SeriesValue $metrics 'infergate_idempotency_hits_total')
    Assert-Equal 'one conflict is counted' 1 (Get-SeriesValue $metrics 'infergate_idempotency_conflicts_total')
    Assert-Equal 'the entry gauge matches the listing' 2 (Get-SeriesValue $metrics 'infergate_idempotency_entries')

    $flush = (Invoke-Curl @('-s', '-X', 'POST', "$gwURL/admin/idempotency/flush")) | ConvertFrom-Json
    Assert-Equal 'a flush reports what it dropped' 2 $flush.flushed
    $afterFlush = Get-Json "$gwURL/admin/idempotency" 'idempotency after a flush'
    if ($afterFlush) {
        Assert-Equal 'the store is empty' 0 $afterFlush.stored
        Assert-Equal 'the listing is empty' 0 $afterFlush.entries.Count
    }

    # -----------------------------------------------------------------------
    Write-Section '8. A streamed turn replays as the same transcript'
    # -----------------------------------------------------------------------
    $streamBody = New-ChatBody -User 'describe the sky' -Stream
    $streamFile = New-BodyFile 'm6-stream.json' $streamBody
    $streamHeaders = @('Idempotency-Key: stream-1', 'X-InferGate-Session: agent-1')
    $before = (Get-Json "$mockURL/calls" 'calls before streaming').calls
    $live = Send-Chat -Url "$gwURL/v1/chat/completions" -BodyFile $streamFile -ExtraHeaders $streamHeaders -Tag 'm6-stream' -Stream
    Assert-Equal 'the streamed turn is answered' 200 $live.Status
    Assert-Contains 'as an event stream' $live.Headers 'Content-Type: text/event-stream'
    $liveFrames = ([regex]::Matches($live.Body, '(?m)^data: ')).Count
    Assert-True 'the stream carried frames' ($liveFrames -ge 4) "frames: $liveFrames"
    Assert-Contains 'and terminated the stream' $live.Body 'data: [DONE]'
    Assert-Equal 'the live stream is not a replay' 'false' $live.Replay

    $replayedStream = Send-Chat -Url "$gwURL/v1/chat/completions" -BodyFile $streamFile -ExtraHeaders $streamHeaders -Tag 'm6-stream-replay' -Stream
    Assert-Equal 'the streamed retry is answered' 200 $replayedStream.Status
    Assert-Equal 'as a replay' 'true' $replayedStream.Replay
    $replayFrames = ([regex]::Matches($replayedStream.Body, '(?m)^data: ')).Count
    Assert-Equal 'the replay delivered the same frames' $liveFrames $replayFrames
    Assert-Contains 'including the terminator' $replayedStream.Body 'data: [DONE]'
    # The mock streams a scripted answer word by word, so the sentence never
    # appears contiguously in the transcript; each fragment is what proves the
    # generated text came back.
    Assert-Contains 'including the generated text' $replayedStream.Body 'streamed '
    Assert-Contains 'word by word' $replayedStream.Body 'answer'
    $afterStreamReplay = (Get-Json "$mockURL/calls" 'calls after the streamed replay').calls
    Assert-Equal 'a streamed replay costs no provider call' 1 ($afterStreamReplay - $before)

    $metrics = Get-Text "$gwURL/metrics"
    Assert-True 'stream frames are counted' ((Get-SeriesValue $metrics 'infergate_stream_frames_total') -ge 4)
    $sessions = Get-Json "$gwURL/admin/sessions" 'sessions after streaming'
    if ($sessions) {
        $agent1 = $sessions.sessions | Where-Object { $_.id -eq 'agent-1' } | Select-Object -First 1
        Assert-True 'the replay is counted on the conversation' ($agent1.idempotent_replays -ge 1)
        Assert-True 'and its cost is zero, because no tokens were generated' ($agent1.cost_usd -le 0.0002)
    }

    # -----------------------------------------------------------------------
    Write-Section '9. The trace says where the answer came from'
    # -----------------------------------------------------------------------
    $traces = Get-Json "$gwURL/admin/traces?limit=100" 'traces'
    if ($traces) {
        Assert-True 'tracing is enabled' ([bool]$traces.enabled)
        $entry = $traces.traces | Where-Object { $_.request_id -eq $replayedStream.RequestID } | Select-Object -First 1
        Assert-True 'the replayed request has a trace' ($null -ne $entry) "request id $($replayedStream.RequestID)"
        if ($entry) {
            Assert-Equal 'the trace names the route' '/v1/chat/completions' $entry.route
            Assert-True 'and has spans' ($entry.span_count -ge 1)
            $detail = Get-Text "$gwURL/admin/traces/$($entry.trace_id)"
            Assert-Contains 'the trace records the replay decision' $detail 'idempotency.replay'
            $parsed = $detail | ConvertFrom-Json
            $server = $parsed.spans | Where-Object { $_.kind -eq 'server' } | Select-Object -First 1
            Assert-True 'there is a server span' ($null -ne $server)
            if ($server) {
                Assert-Equal 'the span carries the conversation id' 'agent-1' $server.attributes.session
                # A replayed request is attributed to no provider: the backend that
                # ORIGINALLY produced the answer is named by its own attribute.
                Assert-Equal 'and attributes the replay to no backend' 'replay' $server.attributes.upstream
                Assert-Equal 'and the idempotency decision that served it' 'replay' $server.attributes.idempotency
                Assert-True 'and is marked as a streamed turn' ([bool]$server.attributes.stream)
            }
        }
    }

    # -----------------------------------------------------------------------
    Write-Section '10. A provider failure is reported, not swallowed'
    # -----------------------------------------------------------------------
    $flakyFile = New-BodyFile 'm6-flaky.json' (New-ChatBody -User 'a flaky request')
    $flakyHeaders = @('X-InferGate-Session: flaky-conv')
    $flaky = Send-Chat -Url "$gwURL/v1/chat/completions" -BodyFile $flakyFile -ExtraHeaders $flakyHeaders -Tag 'm6-flaky'
    # One candidate, one attempt: the breaker budget is a FAILOVER budget, so the
    # provider's own status reaches the caller rather than a synthesised one.
    # (The mock's documented default for an injected failure is 502, and the
    # gateway passes that through untouched.)
    Assert-Equal 'the provider failure reaches the caller as itself' 502 $flaky.Status
    Assert-Contains 'with the provider error envelope intact' $flaky.Body 'provider is restarting'
    Assert-Equal 'the backend is still named' 'scripted-mock' $flaky.Name
    Assert-Equal 'exactly one attempt was spent' '1' $flaky.Attempt
    Assert-Equal 'and no failover happened, because there was nowhere to fail over to' '' $flaky.Tried

    $recovered = Send-Chat -Url "$gwURL/v1/chat/completions" -BodyFile $flakyFile -ExtraHeaders $flakyHeaders -Tag 'm6-recovered'
    Assert-Equal 'the same request succeeds once the provider recovers' 200 $recovered.Status
    Assert-Contains 'and is a fresh answer, not a replay of the failure' $recovered.Body 'the provider recovered'
    Assert-Equal 'the failed turn is not remembered as an idempotent answer' '' $recovered.Replay

    $calls = Get-Json "$mockURL/calls" 'calls after the failure'
    if ($calls) { Assert-Equal 'the provider counts the failure it caused' 1 $calls.failed }

    $sessions = Get-Json "$gwURL/admin/sessions?tenant=anonymous" 'the failing conversation'
    if ($sessions) {
        $flakySession = $sessions.sessions | Where-Object { $_.id -eq 'flaky-conv' } | Select-Object -First 1
        Assert-True 'the conversation is tracked' ($null -ne $flakySession)
        if ($flakySession) {
            Assert-Equal 'with one failed turn' 1 $flakySession.failed
            Assert-Equal 'and one successful one' 1 $flakySession.ok
        }
    }

    # -----------------------------------------------------------------------
    Write-Section '11. Teardown and log check'
    # -----------------------------------------------------------------------
    $gwText = Read-OpenLog $gwLog
    Assert-True 'the gateway logged the replay' ($gwText.Contains('idempotency') -or $gwText.Contains('replay'))
    Assert-True 'the gateway never logged an error' (-not ($gwText -match '(?i)level=ERROR'))
    $gwErr = Read-OpenLog $gwErrLog
    Assert-True 'and wrote nothing to stderr' ($gwErr.Trim() -eq '') $gwErr
}
finally {
    if (-not $KeepRunning) {
        foreach ($p in $script:procs) {
            try {
                if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
            }
            catch { }
        }
    }
    else {
        Write-Host ''
        Write-Host 'processes left running (-KeepRunning):' -ForegroundColor DarkYellow
        foreach ($p in $script:procs) { Write-Host "  pid $($p.Id) $($p.ProcessName)" }
    }

    Write-Host ''
    Write-Host ("=" * 72) -ForegroundColor DarkGray
    if ($script:failed -eq 0) {
        Write-Host "  RESULT: $($script:passed)/$($script:passed) assertions passed" -ForegroundColor Green
    }
    else {
        Write-Host "  RESULT: $($script:passed) passed, $($script:failed) FAILED" -ForegroundColor Red
    }
    Write-Host ("=" * 72) -ForegroundColor DarkGray
    Write-Host "  gateway log: $gwLog" -ForegroundColor DarkGray
    Write-Host "  mock log:    $mockLog" -ForegroundColor DarkGray
    if ($script:failed -gt 0) { exit 1 }
}
