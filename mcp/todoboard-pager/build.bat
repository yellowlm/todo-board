@echo off
cd /d "%~dp0"
if not exist "..\..\..\Dev\build" mkdir "..\..\..\Dev\build"
echo Building todoboard-pager.exe ...
go build -trimpath -buildvcs=false -ldflags "-s -w" -o "..\..\..\Dev\build\todoboard-pager.exe" .
if errorlevel 1 (
  echo BUILD FAILED
  exit /b 1
)
echo OK: Dev\build\todoboard-pager.exe