@echo off
REM
REM Ollama Load Balancer — unified build script for Windows
REM
REM Usage:
REM   build.bat agent [output_dir]
REM   build.bat balancer [output_dir]
REM
setlocal enabledelayedexpansion

set "COMPONENT=%~1"
if "%COMPONENT%"=="" (
    echo Usage: %0 ^<agent^|balancer^> [output_dir]
    exit /b 1
)

set "SCRIPT_DIR=%~dp0"
set "PROJECT_ROOT=%SCRIPT_DIR%.."
set "OUTPUT_DIR=%~2"
if "%OUTPUT_DIR%"=="" set "OUTPUT_DIR=%PROJECT_ROOT%\bin"

if /i "%COMPONENT%"=="agent" (
    set "LABEL=Agent"
    set "CMD_PATH=./cmd/agent"
    set "EXAMPLE=agent.exe -id gpu-1 -balancer http://localhost:8081"
) else if /i "%COMPONENT%"=="balancer" (
    set "LABEL=Balancer"
    set "CMD_PATH=./cmd/balancer"
    set "EXAMPLE=balancer.exe -config config/config.json"
) else (
    echo ERROR: unknown component '%COMPONENT%'. Use 'agent' or 'balancer'.
    exit /b 1
)

set "BINARY=%OUTPUT_DIR%\%COMPONENT%.exe"

echo ╔═══════════════════════════════════════════════════════════╗
echo ║         Ollama Load Balancer - %LABEL% Builder              ║
echo ╚═══════════════════════════════════════════════════════════╝

if not exist "%OUTPUT_DIR%" mkdir "%OUTPUT_DIR%"
cd /d "%PROJECT_ROOT%"

REM [1/4] Go check
echo [1/4] Checking Go installation...
go version >nul 2>&1
if errorlevel 1 (
    echo ERROR: Go is not installed or not in PATH
    exit /b 1
)
for /f "tokens=*" %%i in ('go version') do set "GO_VERSION=%%i"
echo        Found: %GO_VERSION%

REM [2/4] Platform
if "%TARGET_OS%"=="" set "TARGET_OS=windows"
if "%TARGET_ARCH%"=="" set "TARGET_ARCH=amd64"
echo [2/4] Target platform: %TARGET_OS%/%TARGET_ARCH%

REM [3/4] Build
echo [3/4] Building %COMPONENT%...
set "CGO_ENABLED=0"
set "GOOS=%TARGET_OS%"
set "GOARCH=%TARGET_ARCH%"
go build -a -installsuffix cgo -ldflags="-s -w" -o "%BINARY%" %CMD_PATH%

REM [4/4] Result
if exist "%BINARY%" (
    echo [4/4] Build successful!
    echo.
    echo        Binary: %BINARY%
    for %%A in ("%BINARY%") do echo        Size:   %%~zA bytes
    echo.
) else (
    echo ERROR: Build failed - binary not found
    exit /b 1
)

echo Usage example:
echo   %EXAMPLE%
echo.

endlocal