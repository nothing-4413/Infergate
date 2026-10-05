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
rem Usage:  tools\go.cmd <go arguments>       e.g. tools\go.cmd test ./...
setlocal

set "GOROOT=%~dp0..\.gotoolchain\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64"

if not exist "%GOROOT%\bin\go.exe" (
    rem No vendored toolchain: do not pin GOROOT to a directory that is not there,
    rem and do not divert GOCACHE/GOMODCACHE off the host defaults -- a runner
    rem wants its own warmed cache, and the actions/setup-go cache step is keyed
    rem on exactly those locations.
    go %*
    exit /b %ERRORLEVEL%
)

set "GOTOOLCHAIN=local"
set "GOCACHE=%~dp0..\.gocache"
set "GOTMPDIR=%~dp0..\.gotmp"
set "GOMODCACHE=%~dp0..\.gomodcache"
if not exist "%GOTMPDIR%" mkdir "%GOTMPDIR%"
"%GOROOT%\bin\go.exe" %*
exit /b %ERRORLEVEL%
