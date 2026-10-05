# Hardening gate support: run `infergate -check` on one config and merge both
# output streams into a file.
#
# WHY THIS IS A SEPARATE FILE AND A SEPARATE PROCESS.
#
# Windows PowerShell 5.1 turns anything a native command writes to stderr into a
# NativeCommandError. Under $ErrorActionPreference = 'Stop' -- which the calling
# gate sets, like every other script here -- that becomes a terminating error, so
# the refusal this gate is trying to observe would abort the gate instead. `2>&1`
# does not help: it merges into PowerShell's own error stream, which is still
# terminating. Setting the preference to 'Continue' around the call is the fix,
# and it has to happen in a process of its own, because the preference is read
# when the throw is raised and a nested scope does not reliably cover it.
#
# The contract is deliberately dull: exit code is the binary's, and the merged
# output is in the file named by -OutFile. No objects, nothing for a caller to
# misparse.
#
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File run-check.ps1 `
#       -Binary bin\infergate.exe -Config tmp\hardening\gateway.yaml -OutFile tmp\hardening\out.txt
#   exit code == the binary's exit code

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Binary,
    [Parameter(Mandatory = $true)][string]$Config,
    [Parameter(Mandatory = $true)][string]$OutFile
)

$ErrorActionPreference = 'Continue'

& $Binary -config $Config -check > $OutFile 2>&1
exit $LASTEXITCODE
