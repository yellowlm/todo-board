@echo off
cd /d "%~dp0"
if not exist "..\Dev\build" mkdir "..\Dev\build"

if not exist "rsrc_windows_amd64.syso" (
  echo Generating resources ^(icon + manifest^) ...
  go run github.com/akavel/rsrc@v0.10.2 -manifest app.manifest -ico frontend\icon.ico -arch amd64 -o rsrc_windows_amd64.syso
  if errorlevel 1 (
    echo RESOURCE GENERATION FAILED
    exit /b 1
  )
)

echo Building TodoBoard.exe ...
go build -trimpath -buildvcs=false -ldflags "-s -w -H=windowsgui" -o "..\Dev\build\TodoBoard.exe" .
if errorlevel 1 (
  echo BUILD FAILED
  exit /b 1
)
echo OK: ..\Dev\build\TodoBoard.exe
