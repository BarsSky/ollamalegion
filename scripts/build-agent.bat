@echo off
REM
REM Скрипт сборки агента Ollama Load Balancer для Windows
REM 
REM Использование:
REM   build-agent.bat [output_dir]
REM
REM Аргументы:
REM   output_dir - директория для бинарного файла (по умолчанию: ..\bin)
REM

setlocal enabledelayedexpansion

REM Получение директории скрипта
set "SCRIPT_DIR=%~dp0"
set "PROJECT_ROOT=%SCRIPT_DIR%.."
set "OUTPUT_DIR=%~1"

if "%OUTPUT_DIR%"=="" set "OUTPUT_DIR=%PROJECT_ROOT%\bin"

set "AGENT_BINARY=%OUTPUT_DIR%\agent.exe"

echo ╔═══════════════════════════════════════════════════════════╗
echo ║         Ollama Load Balancer - Agent Builder              ║
echo ╚═══════════════════════════════════════════════════════════╝

REM Создание директории вывода
if not exist "%OUTPUT_DIR%" mkdir "%OUTPUT_DIR%"

REM Переход в корень проекта
cd /d "%PROJECT_ROOT%"

REM Проверка установки Go
echo [1/4] Checking Go installation...
go version >nul 2>&1
if errorlevel 1 (
    echo ERROR: Go is not installed or not in PATH
    exit /b 1
)
for /f "tokens=*" %%i in ('go version') do set "GO_VERSION=%%i"
echo        Found: %GO_VERSION%

REM Определение целевой платформы
set "TARGET_OS=%TARGET_OS%"
set "TARGET_ARCH=%TARGET_ARCH%"

if "%TARGET_OS%"=="" set "TARGET_OS=windows"
if "%TARGET_ARCH%"=="" set "TARGET_ARCH=amd64"

echo [2/4] Target platform: %TARGET_OS%/%TARGET_ARCH%

REM Сборка агента
echo [3/4] Building agent...

set "CGO_ENABLED=0"
set "GOOS=%TARGET_OS%"
set "GOARCH=%TARGET_ARCH%"

go build -a -installsuffix cgo -ldflags="-s -w" -o "%AGENT_BINARY%" ./cmd/agent

REM Проверка результата
if exist "%AGENT_BINARY%" (
    echo [4/4] Build successful!
    echo.
    echo        Binary: %AGENT_BINARY%
    for %%A in ("%AGENT_BINARY%") do echo        Size:   %%~zA bytes
    echo.
) else (
    echo ERROR: Build failed - binary not found
    exit /b 1
)

echo Usage example:
echo   %AGENT_BINARY% -id gpu-1 -balancer http://localhost:8081
echo.

endlocal
