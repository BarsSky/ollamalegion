@echo off
REM
REM Скрипт сборки балансировщика Ollama Load Balancer для Windows
REM 
REM Использование:
REM   build-balancer.bat [output_dir]
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

set "BALANCER_BINARY=%OUTPUT_DIR%\balancer.exe"

echo ╔═══════════════════════════════════════════════════════════╗
echo ║      Ollama Load Balancer - Balancer Builder              ║
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

REM Сборка балансировщика
echo [3/4] Building balancer...

set "CGO_ENABLED=0"
set "GOOS=%TARGET_OS%"
set "GOARCH=%TARGET_ARCH%"

go build -a -installsuffix cgo -ldflags="-s -w" -o "%BALANCER_BINARY%" ./cmd/balancer

REM Проверка результата
if exist "%BALANCER_BINARY%" (
    echo [4/4] Build successful!
    echo.
    echo        Binary: %BALANCER_BINARY%
    for %%A in ("%BALANCER_BINARY%") do echo        Size:   %%~zA bytes
    echo.
) else (
    echo ERROR: Build failed - binary not found
    exit /b 1
)

echo Usage example:
echo   %BALANCER_BINARY% -config config/config.json
echo.

endlocal
