@echo off
setlocal EnableExtensions EnableDelayedExpansion

rem ---------------------------------------------------------------------------
rem  server_restart.cmd
rem
rem  Local developer helper: stop any running server.exe, rebuild from source,
rem  and start it detached with config\settings.env. stdout/stderr go to
rem  server_run.log / server_run_err.log at the repo root (see .gitignore).
rem ---------------------------------------------------------------------------

pushd "%~dp0" >nul

set "BIN=server.exe"
set "PKG=./cmd/server"
set "CONFIG=config\settings.env"
set "STDOUT=server_run.log"
set "STDERR=server_run_err.log"

echo [1/3] Stopping running %BIN% ...

rem Kill by image name first (covers the happy path).
for /f "tokens=2 delims=," %%a in (
    'tasklist /FI "IMAGENAME eq %BIN%" /FO CSV /NH 2^>nul ^| findstr /i "%BIN%"'
) do (
    set "PID=%%~a"
    echo   killing PID !PID! ^(image %BIN%^)
    taskkill /F /PID !PID! >nul 2>&1
)

rem Defensive: also kill whoever is listening on the BOS port, in case the
rem previous run crashed or was renamed.
for /f "tokens=5" %%p in (
    'netstat -ano -p tcp 2^>nul ^| findstr /r /c:":%BOS_PORT% .*LISTENING"'
) do (
    echo   killing PID %%p ^(listener on :%BOS_PORT%^)
    taskkill /F /PID %%p >nul 2>&1
)

rem Give Windows a moment to actually release the port / file handle on BIN.
ping -n 2 127.0.0.1 >nul

echo [2/3] Building %BIN% ...
go build -o "%BIN%" "%PKG%"
if errorlevel 1 (
    echo Build failed.
    popd >nul
    endlocal
    exit /b 1
)

echo [3/3] Starting %BIN% with %CONFIG% ...
rem Truncate previous logs so each restart starts fresh.
break > "%STDOUT%"
break > "%STDERR%"
start "open-oscar-server" /B cmd /c "%BIN% -config %CONFIG% 1>> %STDOUT% 2>> %STDERR%"

rem Wait a moment so the new process shows up in tasklist and any immediate
rem startup failure has time to land in %STDERR%.
ping -n 2 127.0.0.1 >nul

rem Report the new PID.
for /f "tokens=2 delims=," %%a in (
    'tasklist /FI "IMAGENAME eq %BIN%" /FO CSV /NH 2^>nul ^| findstr /i "%BIN%"'
) do (
    echo   started PID %%~a
)

echo Done. Tail %STDOUT% / %STDERR% for output.
popd >nul
endlocal
exit /b 0
