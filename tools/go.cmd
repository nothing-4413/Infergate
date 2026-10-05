@echo off
rem InferGate local Go toolchain shim.
rem
rem WHY THIS FILE EXISTS. The machine this project was developed on cannot write
rem to the default GOCACHE (%LOCALAPPDATA%\go-build) or to the pre-populated
rem GOMODCACHE (D:\goproject\pkg\mod\cache), and its `go` is not on PATH, so this
rem script points every Go invocation at workspace-local caches and selects the
rem complete toolchain extracted under .gotoolchain:
rem
rem   * D:\goproject\pkg\mod\golang.org\toolchain@...\ is TRUNCATED (missing
rem     src\unsafe and src\runtime) and cannot compile anything.
rem   * The full distribution was extracted from the module cache zip
rem     golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64.zip.
rem
rem WHY IT ALSO HANDLES A NORMAL MACHINE. .gotoolchain is gitignored (hundreds of
rem MB, reproducible), so on a fresh clone -- a CI runner, another developer --
rem there is no vendored toolchain at all and nothing to redirect caches away
rem from. Every .ps1 in scripts\ still goes through this file, so the fallback
rem below is what lets those gates run on a hosted Windows runner with only
rem actions/setup-go: whatever `go` the PATH provides, with its own cache.
rem
rem WHY GOROOT IS CLEARED BEFORE THE BRANCH. It used to be pinned to the vendored
rem tree unconditionally, and that was the first failure of the curl-gates job
rem (run 37369217148: the M0 step died after five seconds with "go: cannot find
rem GOROOT directory"). On a runner, actions/setup-go exports GOROOT, so the `go`
rem the PATH did provide was handed a GOROOT pointing at a vendored toolchain a
rem fresh clone does not have. Clearing it is what both branches want: go derives
rem GOROOT from its own executable location when the variable is unset, so the
rem vendored go finds the vendored tree and the runner's go finds the runner's
rem tree. The clear has to happen before the `if`, not inside one arm of it:
rem `setlocal` inherits the parent environment, and a fallback arm that only
rem skipped the assignment still ran under the inherited value. Verified both
rem ways locally -- vendored branch with a bogus GOROOT in the environment, and
rem the fallback branch exercising the same bug.
rem
rem Usage:  tools\go.cmd <go arguments>       e.g. tools\go.cmd test ./...
setlocal

rem Whatever GOROOT the caller had belongs to the caller's toolchain, not ours.
set "GOROOT="

set "VENDORED=%~dp0..\.gotoolchain\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64\bin\go.exe"

if not exist "%VENDORED%" (
    rem No vendored toolchain: do not divert GOCACHE/GOTMPDIR/GOMODCACHE off the
    rem host defaults -- a runner wants its own warmed cache, and the
    rem actions/setup-go cache step is keyed on exactly those locations.
    go %*
    exit /b %ERRORLEVEL%
)

rem The vendored distribution: workspace-local caches, which is the reason this
rem file exists at all.
set "GOTOOLCHAIN=local"
set "GOCACHE=%~dp0..\.gocache"
set "GOTMPDIR=%~dp0..\.gotmp"
set "GOMODCACHE=%~dp0..\.gomodcache"
if not exist "%GOTMPDIR%" mkdir "%GOTMPDIR%"
"%VENDORED%" %*
exit /b %ERRORLEVEL%
