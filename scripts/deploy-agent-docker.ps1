#Requires -Version 5.1
<#
.SYNOPSIS
    Ollama Load Balancer - Agent Docker Deployment Script (PowerShell)
.DESCRIPTION
    Автоматически определяет GPU-сервер и выбирает правильный compose-файл.
    Поддерживает ручное переключение CPU/GPU через параметры.
.PARAMETER Gpu
    Принудительно использовать GPU-конфигурацию
.PARAMETER Cpu
    Принудительно использовать CPU-конфигурацию
.PARAMETER EnvFile
    Путь к файлу .env (по умолчанию: deployments/.env)
.PARAMETER Build
    Пересобрать образ (по умолчанию включено)
.PARAMETER NoBuild
    Использовать существующий образ без пересборки
.PARAMETER Pull
    Pull base images перед сборкой
.EXAMPLE
    .\deploy-agent-docker.ps1 -EnvFile "deployments/.env"
    Автоопределение CPU/GPU и запуск с указанным .env файлом
.EXAMPLE
    .\deploy-agent-docker.ps1 -Gpu -EnvFile "deployments/.env"
    Принудительно GPU-режим
.EXAMPLE
    .\deploy-agent-docker.ps1 -Cpu -EnvFile "deployments/.env"
    Принудительно CPU-режим
#>
[CmdletBinding()]
param(
    [switch]$Gpu,
    [switch]$Cpu,
    [string]$EnvFile = "deployments/.env",
    [switch]$Build = $true,
    [switch]$NoBuild,
    [switch]$Pull
)

$ErrorActionPreference = "Stop"

# --- Цвета ---
function Write-Info    { param($msg) Write-Host "[INFO]  $msg" -ForegroundColor Green }
function Write-Warn    { param($msg) Write-Host "[WARN]  $msg" -ForegroundColor Yellow }
function Write-Error   { param($msg) Write-Host "[ERROR] $msg" -ForegroundColor Red }
function Write-Debug   { param($msg) Write-Host "[DEBUG] $msg" -ForegroundColor Cyan }

# --- Определение путей ---
$RepoRoot = Split-Path -Parent $PSScriptRoot
$ComposeDir = Join-Path $RepoRoot "deployments"
$EnvFilePath = if ([System.IO.Path]::IsPathRooted($EnvFile)) { $EnvFile } else { Join-Path $RepoRoot $EnvFile }

# --- Проверка Docker ---
function Test-Docker {
    try {
        $null = docker version 2>$null
        if ($LASTEXITCODE -ne 0) { throw "Docker не отвечает" }
    }
    catch {
        Write-Error "Docker не найден или не запущен. Установите Docker Desktop: https://docs.docker.com/get-docker/"
        exit 1
    }
}

# --- Проверка NVIDIA Container Toolkit ---
function Test-NvidiaToolkit {
    # Способ 1: Проверяем Docker runtime на наличие nvidia
    $runtimes = docker info --format '{{json .Runtimes}}' 2>$null
    if ($LASTEXITCODE -eq 0 -and $runtimes -match "nvidia") {
        return $true
    }

    # Способ 2: Проверяем nvidia-ctk
    try {
        $null = nvidia-ctk --version 2>$null
        if ($LASTEXITCODE -eq 0) { return $true }
    } catch {}

    # Способ 3: Pull образа и запуск тестового контейнера (мягкая обработка)
    # Сначала тихо пытаемся pull образ
    $pullResult = docker pull nvidia/cuda:12.2.0-base-ubuntu22.04 2>&1
    if ($LASTEXITCODE -ne 0) {
        # Не можем pull — возможно нет интернета или образа нет в registry
        return $false
    }

    # Пробуем запустить тестовый контейнер
    $savedErrorAction = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        $result = docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi 2>&1
        if ($LASTEXITCODE -eq 0 -and ($result | Out-String) -match "NVIDIA-SMI") {
            return $true
        }
    } catch {
        # Игнорируем ошибки docker run (например, "Unable to find image")
        return $false
    } finally {
        $ErrorActionPreference = $savedErrorAction
    }

    return $false
}

# --- Проверка наличия GPU ---
function Test-Gpu {
    if ($Gpu) { return $true }
    if ($Cpu) { return $false }

    # Проверяем nvidia-smi
    try {
        $smi = nvidia-smi -L 2>$null
        if ($LASTEXITCODE -eq 0 -and $smi -match "GPU") {
            return $true
        }
    } catch {}

    # Проверяем через wmic (Windows)
    try {
        $wmi = Get-WmiObject Win32_VideoController -ErrorAction SilentlyContinue |
              Where-Object { $_.Name -match "NVIDIA|AMD|Intel" }
        if ($wmi) { return $true }
    } catch {}

    return $false
}

# --- Определение режима ---
function Get-Mode {
    if ($Gpu) { return "gpu" }
    if ($Cpu) { return "cpu" }
    if (Test-Gpu) { return "gpu" }
    return "cpu"
}

# --- Проверка .env файла ---
function Test-EnvFile {
    if (-not (Test-Path $EnvFilePath)) {
        Write-Warn "Файл .env не найден: $EnvFilePath"
        $exampleEnv = Join-Path $RepoRoot "config" "agent.example.env"
        if (Test-Path $exampleEnv) {
            Copy-Item $exampleEnv $EnvFilePath
            Write-Info "Шаблон скопирован в $EnvFilePath"
            Write-Warn "ВАЖНО: Отредактируйте $EnvFilePath и замените placeholder-значения!"
        } else {
            Write-Error "config/agent.example.env не найден. Создайте .env вручную."
            exit 1
        }
    }
}

# --- Валидация переменных ---
function Test-EnvVariables {
    $content = Get-Content $EnvFilePath -Raw

    # Извлекаем значения
    $agentId = if ($content -match "AGENT_ID\s*=\s*([^\r\n]+)") { $matches[1].Trim() } else { "" }
    $balancerUrl = if ($content -match "BALANCER_URL\s*=\s*([^\r\n]+)") { $matches[1].Trim() } else { "" }
    $agentHost = if ($content -match "AGENT_PUBLIC_HOST\s*=\s*([^\r\n]+)") { $matches[1].Trim() } else { "" }

    $missing = 0

    if ([string]::IsNullOrWhiteSpace($agentId) -or $agentId -eq "agent") {
        Write-Warn "AGENT_ID не установлен или имеет значение по умолчанию (agent)"
        $missing++
    }

    if ([string]::IsNullOrWhiteSpace($balancerUrl) -or $balancerUrl -match "REPLACE_WITH") {
        Write-Warn "BALANCER_URL не установлен или содержит placeholder"
        $missing++
    }

    if ([string]::IsNullOrWhiteSpace($agentHost) -or $agentHost -match "REPLACE_WITH") {
        Write-Warn "AGENT_PUBLIC_HOST не установлен или содержит placeholder"
        $missing++
    }

    if ($missing -gt 0) {
        Write-Error "Найдены незаполненные обязательные переменные в $EnvFilePath"
        Write-Info "Отредактируйте файл и запустите скрипт снова."
        exit 1
    }

    Write-Info "Переменные окружения валидны."
}

# --- Развертывание ---
function Deploy-Agent {
    param([string]$Mode)

    $composeFiles = @("-f", (Join-Path $ComposeDir "docker-compose.agent.yml"))

    if ($Mode -eq "gpu") {
        $composeFiles += "-f"
        $composeFiles += (Join-Path $ComposeDir "docker-compose.agent.gpu.yml")
        Write-Info "Режим: GPU (с NVIDIA Container Toolkit)"

        if (-not (Test-NvidiaToolkit)) {
            Write-Warn "NVIDIA Container Toolkit не обнаружен!"
            Write-Info "Для GPU-режима на Windows:"
            Write-Info "  1. Установите Docker Desktop с WSL2 backend"
            Write-Info "  2. Включите 'Use the WSL 2 based engine'"
            Write-Info "  3. Включите 'Enable NVIDIA Container Toolkit' в Settings > Resources > WSL Integration"
            Write-Warn "Если toolkit установлен, используйте -Gpu для принудительного режима."
            $confirm = Read-Host "Продолжить anyway? [y/N]"
            if ($confirm -notmatch "^[Yy]$") {
                Write-Info "Отменено."
                exit 1
            }
        }
    } else {
        Write-Info "Режим: CPU"
    }

    # Формируем команду
    $cmd = @("compose") + $composeFiles + @("--env-file", $EnvFilePath)

    if ($Pull) {
        Write-Info "Pull base images..."
        docker @cmd pull
    }

    $upArgs = @("up", "-d")
    if (-not $NoBuild) {
        $upArgs += "--build"
    }

    Write-Info "Запуск контейнера..."
    docker @cmd @upArgs

    Write-Info "Контейнер запущен. Просмотр логов:"
    $logCmd = $cmd + @("logs", "-f")
    Write-Host "  docker $($logCmd -join ' ')"
}

# --- Main ---
Write-Info "Ollama Load Balancer - Agent Deployment (PowerShell)"
Write-Debug "ENV_FILE: $EnvFilePath"
Write-Debug "COMPOSE_DIR: $ComposeDir"

Test-Docker
Test-EnvFile
Test-EnvVariables

$mode = Get-Mode
Deploy-Agent -Mode $mode

Write-Info "Готово! Агент развернут в режиме: $mode"