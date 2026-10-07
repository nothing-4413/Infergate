# Runs one gate script and makes its transcript reach the build log, not just the
# artifact.
#
# WHY THIS FILE EXISTS. The curl gates were the one thing in this repository with
# no evidence anyone could read. On the first runs, a red gate produced a single
# annotation -- "Process completed with exit code 1." -- and nothing else: the
# step's log is the transcript, but the job log endpoint needs admin rights on the
# repository, so the only readable copy was in an artifact, which needs
# authentication too. Every diagnosis cost a round trip through a human.
#
# Piping the transcript through this file puts the FAIL lines in the step log,
# which the Actions UI shows to anyone who can see the repository. It also writes
# the same text to tmp\<name>.log as a plain-text copy for the artifact.
#
# WHY NOT `powershell ... | Tee-Object`. A pipeline has no exit code, so the step
# would go green on a red gate. Capturing into a variable keeps $LASTEXITCODE
# intact, and the wrapper still exits with the gate's own code.
#
# WHY IT ALSO WRITES A MARKER. Every gate step in ci.yml sets
# continue-on-error: true so that one red gate does not hide the others --
# and GitHub documents what that flag costs: "When a continue-on-error step
# fails, the outcome is failure, but the final conclusion is success." So this
# step's exit code cannot fail the job, and every gate could be red
# while the run, the badge and the anonymous jobs API said success.
# scripts/lib/summarize-gates.ps1 reads these markers and is the step that is
# allowed to fail. A file is used because environment variables do not travel
# between steps and a child process cannot write its parent's $GITHUB_OUTPUT.
#
# WHY IT READS THE TABLE. Each gate ends by printing how many assertions it ran,
# and this wrapper has always captured that line without ever looking at the
# number in it -- so a gate could grow and the list of its size could keep saying
# the old number, which is what M0 did: its curl gate grew by one assertion in
# af4209f and both gate tables still said 47 until this check read them. The Go
# wrapper (run-go-verify.ps1) now compares each printed total
# with its row in README's gate table; this is the same check for the curl side,
# against the gate table of docs/ACCEPTANCE.md, which is keyed by milestone (and
# by the gate's own script name for the access gate) and whose third cell is the
# curl number. internal/repofmt keeps that table equal to README's, so either one
# can be read. A gate with no row is a failure: renaming one must not quietly
# switch the check off.
#
# Usage:  scripts/lib/run-gate.ps1 -Path scripts\verify-m5.ps1 -Name m5
param(
    [Parameter(Mandatory = $true)][string]$Path,
    [Parameter(Mandatory = $true)][string]$Name
)

$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$logPath = Join-Path $repo "tmp\$Name.log"
$markerPath = Join-Path $repo "tmp\gate-$Name.exit"

# The gate's own row, read before the run so the note can be printed next to the
# gate's verdict. The curl column is the third cell, so the pattern spells out
# three cells and captures the third: a row whose third cell is a sentence rather
# than a number -- the evidence tables below the gate table name the hardening
# script too -- is not this gate's row.
$leaf = Split-Path -Leaf $Path
$label = $leaf
$acceptancePath = Join-Path $repo 'docs\ACCEPTANCE.md'
$acceptance = ''
if (Test-Path $acceptancePath) {
    $acceptance = [System.IO.File]::ReadAllText($acceptancePath, [System.Text.Encoding]::UTF8)
}
$documented = -1
if ($acceptance -ne '') {
    if ($Name -match '^m([0-6])$') {
        $label = 'M' + $Matches[1]
        $pattern = '(?m)^\|\s*M' + $Matches[1] + '[^\r\n|]*\|[^\r\n|]*\|\s*(\d+)\s*\|'
        $row = [regex]::Match($acceptance, $pattern)
        if ($row.Success) { $documented = [int]$row.Groups[1].Value }
    } else {
        # The access gate's label is prose, not a milestone, so its row is the one
        # that names this script -- in the curl column of the gate table, or in a
        # sentence of an evidence table below it, which is why a candidate row only
        # counts when its third cell is a bare number.
        $needle = [regex]::Escape($leaf)
        foreach ($line in ($acceptance -split "`n")) {
            $t = $line.Trim()
            if (-not $t.StartsWith('|')) { continue }
            if ($t -notmatch $needle) { continue }
            $cells = $t -split '\|'
            if ($cells.Count -le 3) { continue }
            $number = [regex]::Match($cells[3], '^\s*(\d+)\s*$')
            if ($number.Success) {
                $documented = [int]$number.Groups[1].Value
                break
            }
        }
    }
}

$out = & powershell -NoProfile -ExecutionPolicy Bypass -File $Path 2>&1 | Out-String
$code = $LASTEXITCODE
if ($null -eq $code) { $code = 0 }

Write-Host $out

# The number the gate printed and the number its row gives. Both have to exist and
# agree; the denominator is the total the gate ran, which is what the table states.
$printed = -1
foreach ($line in ($out -split "`n")) {
    if ($line -match 'RESULT:\s*\d+/(\d+) assertions passed') { $printed = [int]$Matches[1] }
}

if ($acceptance -eq '') {
    $tableNote = "docs/ACCEPTANCE.md could not be read, so the gate table cannot be checked"
} elseif ($documented -ge 0) {
    $tableNote = "the gate table says $documented assertions for $label; the gate printed $printed"
} else {
    $tableNote = "the gate table has no row for $label"
}
Write-Host "  $tableNote"

$problem = ''
if ($acceptance -eq '') {
    $problem = 'docs/ACCEPTANCE.md could not be read, so its gate table cannot be checked'
} elseif ($documented -lt 0) {
    $problem = "docs/ACCEPTANCE.md's gate table has no row for $label"
} elseif ($printed -lt 0) {
    # Only a green gate has to print a total: the red verdict line names the
    # failures instead, and a red gate is already red without help.
    if ($code -eq 0) {
        $problem = 'the gate exited 0 without printing a RESULT line, so how much it checked is unknown'
    }
} elseif ($printed -ne $documented) {
    $problem = "the gate ran $printed assertions, but docs/ACCEPTANCE.md's row for $label says $documented"
}
if ($problem -ne '') {
    Write-Host "::error title=curl gate $Name::$problem"
    if ($code -eq 0) { $code = 1 }
}

New-Item -ItemType Directory -Force -Path (Split-Path -Parent $logPath) | Out-Null
# WriteAllText rather than Out-File / Set-Content: those prefix a BOM, which turns
# the first line of the copy into noise. The table note rides along so a mismatch
# can be diagnosed from the artifact alone.
[System.IO.File]::WriteAllText($logPath, ($out.TrimEnd() + "`r`n  $tableNote`r`n"), [System.Text.UTF8Encoding]::new($false))

# The marker is written before the exit code is reported, and a failure to write
# it is not swallowed: an absent marker counts as red downstream, which is the
# safe direction for a gate.
[System.IO.File]::WriteAllText($markerPath, "$code", [System.Text.UTF8Encoding]::new($false))

if ($code -ne 0) {
    Write-Host ""
    Write-Host "---- gate '$Name' exited $code ----"
}
exit $code
