@echo off
rem InferGate local Go toolchain shim.
rem
rem The host cannot write to the default GOCACHE (%LOCALAPPDATA%\go-build) or to
rem the pre-populated GOMODCACHE (D:\goproject\pkg\mod\cache), so this script
rem points every Go invocation at workspace-local caches. It also selects the
rem complete toolchain extracted under .gotoolchain:
rem
rem   * D:\goproject\pkg\mod\golang.org\toolchain@...\ is TRUNCATED (missing
rem     src\unsafe and src\runtime) and cannot compile anything.
rem   * The full distribution was extracted from the module cache zip
rem     golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64.zip.
rem
rem Usage:  tools\go.cmd <go arguments>       e.g. tools\go.cmd test ./...
setlocal
set "GOROOT=%~dp0..\.gotoolchain\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64"
set "GOTOOLCHAIN=local"
set "GOCACHE=%~dp0..\.gocache"
set "GOTMPDIR=%~dp0..\.gotmp"
set "GOMODCACHE=%~dp0..\.gomodcache"
if not exist "%GOTMPDIR%" mkdir "%GOTMPDIR%"
"%GOROOT%\bin\go.exe" %*
exit /b %ERRORLEVEL%
