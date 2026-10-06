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
# Usage:  scripts/lib/run-gate.ps1 -Path scripts\verify-m5.ps1 -Name m5
param(
    [Parameter(Mandatory = $true)][string]$Path,
    [Parameter(Mandatory = $true)][string]$Name
)

$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$logPath = Join-Path $repo "tmp\$Name.log"
$markerPath = Join-Path $repo "tmp\gate-$Name.exit"

$out = & powershell -NoProfile -ExecutionPolicy Bypass -File $Path 2>&1 | Out-String
$code = $LASTEXITCODE
if ($null -eq $code) { $code = 0 }

Write-Host $out

New-Item -ItemType Directory -Force -Path (Split-Path -Parent $logPath) | Out-Null
# WriteAllText rather than Out-File / Set-Content: those prefix a BOM, which turns
# the first line of the copy into noise.
[System.IO.File]::WriteAllText($logPath, $out, [System.Text.UTF8Encoding]::new($false))

# The marker is written before the exit code is reported, and a failure to write
# it is not swallowed: an absent marker counts as red downstream, which is the
# safe direction for a gate.
[System.IO.File]::WriteAllText($markerPath, "$code", [System.Text.UTF8Encoding]::new($false))

if ($code -ne 0) {
    Write-Host ""
    Write-Host "---- gate '$Name' exited $code ----"
}
exit $code
