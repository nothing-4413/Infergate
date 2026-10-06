# Run the race detector the way CI runs it: one package at a time, so a single
# run reports every rejected package instead of stopping at the first.
#
# Why this script exists at all: `go test -race` needs cgo, and cgo needs a C
# compiler plus a writable temp directory. On this machine gcc is not on PATH
# (it lives under C:\msys64), and the default TEMP is not writable from the
# sandbox, so plain `tools\go.cmd test -race ./...` fails at link time with:
#
#   cgo: open ...\AppData\Local\Temp\cgo-gcc-input-...: Access is denied.
#
# Those two facts used to be recorded as "this machine cannot run -race". It
# can; it just needs to be told where the compiler and the temp dir are. That
# mistake cost the project weeks of a CI step that was red with no readable
# reason, and the race it hid was in production code (the Redis store's
# counters). Hence a script rather than a paragraph.
#
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\run-race.ps1
#   powershell ... -File scripts\run-race.ps1 -Package ./internal/cache
#   powershell ... -File scripts\run-race.ps1 -Timeout 10m
#
# Exit code is 0 only when every package passed.

[CmdletBinding()]
param(
  [string[]]$Package,
  [string]$Timeout = '10m',
  [string]$CC = 'C:\msys64\ucrt64\bin\gcc.exe',
  [switch]$Quiet
)

$ErrorActionPreference = 'Continue'
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

if (-not (Test-Path $CC)) {
  Write-Host "no C compiler at $CC"
  Write-Host 'race needs cgo; install one or pass -CC <path to gcc>'
  exit 2
}

# cgo shells out to the compiler, so the directory holding it (and its runtime
# DLLs under usr\bin) has to be reachable, and GOROOT stays whatever
# tools\go.cmd points at.
$compilerDir = Split-Path -Parent $CC
$usrBin = Join-Path (Split-Path -Parent $compilerDir) 'usr\bin'
$env:PATH = "$compilerDir;$usrBin;$env:PATH"
$env:CC = $CC
$env:CGO_ENABLED = '1'

# The real reason plain `-race` fails here: cgo writes its input file into
# TEMP, and TEMP points at a path the sandbox refuses. Redirecting both
# variables into the repo fixes it; .gotmp is gitignored.
$tmp = Join-Path $repo '.gotmp'
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$env:TMP = $tmp
$env:TEMP = $tmp

& (Join-Path $repo 'tools\go.cmd') version | Out-Null
Write-Host "race: CC=$CC cgo=on temp=$tmp"

if (-not $Package) {
  $Package = @(& (Join-Path $repo 'tools\go.cmd') list ./...)
}
if (-not $Package) {
  Write-Host 'go list returned no packages'
  exit 2
}

$started = Get-Date
$failed = @()
$races = 0
$n = 0

foreach ($pkg in $Package) {
  $n++
  $out = & (Join-Path $repo 'tools\go.cmd') test -race $pkg -count=1 -timeout $Timeout 2>&1
  $code = $LASTEXITCODE
  $hits = @($out | Select-String -Pattern 'DATA RACE').Count
  $races += $hits

  if ($code -ne 0) {
    # Named on the way out as well as in the log: a caller scrolling terminal
    # history should not have to know which line to look for.
    Write-Host "FAILED(running): $pkg"
    $failed += $pkg
    $out | ForEach-Object { "    | $_" }
  } elseif (-not $Quiet) {
    Write-Host ("ok    {0} ({1} DATA RACE line(s))" -f $pkg, $hits)
  }
}

$elapsed = [int]((Get-Date) - $started).TotalSeconds
Write-Host ''
if ($failed.Count -eq 0) {
  Write-Host ("race clean: {0} package(s), 0 failures, {1} DATA RACE line(s), {2}s" -f $n, $races, $elapsed)
  exit 0
}
Write-Host ("race failed: {0}/{1} package(s), {2} DATA RACE line(s), {3}s" -f $failed.Count, $n, $races, $elapsed)
foreach ($pkg in $failed) { Write-Host "  $pkg" }
exit 1
