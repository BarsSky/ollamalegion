#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Перезапускает локальный стек OllamaLegion: останавливает balancer.exe и agent-контейнер,
    пересобирает балансировщик, запускает его и agent, проверяет health и состояние кластера.

.DESCRIPTION
    По умолчанию работает в каталоге, где лежит скрипт (корень репозитория).
    Путь к репозиторию можно переопределить через -RepoPath.

    Параметры по умолчанию рассчитаны на локальную dev-сессию под Windows +
    Docker Desktop. Под linux/macOS часть шагов (Stop-Process balancer, Start-Process .exe)
    нужно адаптировать.

.EXAMPLE
    pwsh scripts/restart_stack.ps1
    pwsh scripts/restart_stack.ps1 -RepoPath C:\Work\ollamalegion
    pwsh scripts/restart_stack.ps1 -SkipAgent -SkipBuild
#>

[CmdletBinding()]
param(
    [string]$RepoPath,
    [string]$BalancerExe = "balancer.exe",
    [string]$ConfigPath,
    [string]$AgentImage = "ollama-legion/agent:latest",
    [string]$AgentContainer = "ollama-legion-agent-gpu",
    [int]$AgentPort = 18032,
    [string]$BalancerUrl = "http://localhost:18081",
    [string]$BalancerHostAlias = "host.docker.internal",
    [string]$AgentId = "llama-gpu-node-1",
    [switch]$SkipBuild,
    [switch]$SkipAgent,
    [switch]$KeepState
)

# Определяем корень репозитория
if ([string]::IsNullOrWhiteSpace($RepoPath)) {
    $RepoPath = Split-Path -Parent $PSScriptRoot
}

if ([string]::IsNullOrWhiteSpace($ConfigPath)) {
    $ConfigPath = Join-Path $RepoPath "config\config.json"
}

Write-Host "=== RepoPath: $RepoPath ==="

# 1) Останавливаем локальный процесс балансировщика
Write-Host "=== Stopping balancer.exe ==="
Get-Process -Name ([System.IO.Path]::GetFileNameWithoutExtension($BalancerExe)) -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 2

# 2) Останавливаем старый agent-контейнер
if (-not $SkipAgent) {
    Write-Host "=== Stopping agent container $AgentContainer ==="
    docker rm -f $AgentContainer 2>$null
    Start-Sleep -Seconds 1
}

# 3) Удаляем state.json (если не попросили сохранить)
if (-not $KeepState) {
    Write-Host "=== Removing state.json ==="
    $stateFile = Join-Path $RepoPath "data\state.json"
    Remove-Item $stateFile -ErrorAction SilentlyContinue
}

# 4) Сборка балансировщика
if (-not $SkipBuild) {
    Write-Host "=== Building $BalancerExe ==="
    Push-Location $RepoPath
    try {
        go build -o $BalancerExe ./cmd/balancer
        if ($LASTEXITCODE -ne 0) { Write-Host "BUILD FAILED"; exit 1 }
        Write-Host "Build OK"
    }
    finally {
        Pop-Location
    }
}

# 5) Запуск балансировщика
Write-Host "=== Starting $BalancerExe ==="
$balancerPath = Join-Path $RepoPath $BalancerExe
if (-not (Test-Path $balancerPath)) {
    Write-Host "ERROR: $BalancerExe не найден: $balancerPath" -ForegroundColor Red
    exit 1
}
Start-Process -FilePath $balancerPath -ArgumentList @('-config', $ConfigPath) -WindowStyle Hidden
Start-Sleep -Seconds 4

# 6) Health check
Write-Host "=== Checking health ==="
try {
    $h = Invoke-RestMethod "$BalancerUrl/api/v1/health" -TimeoutSec 5
    Write-Host "Health: $($h.status)"
}
catch {
    Write-Host "Health check failed: $_" -ForegroundColor Red
    exit 1
}

# 7) Запуск agent (если не отключён)
if (-not $SkipAgent) {
    Write-Host "=== Starting agent ==="
    docker run -d --name $AgentContainer --restart unless-stopped -p ${AgentPort}:${AgentPort} `
        -e BACKEND_TYPE=llama_cpp `
        -e AGENT_ID=$AgentId `
        -e BALANCER_URL="http://$BalancerHostAlias`:18081" `
        -e CPPWORKER_URL="http://$BalancerHostAlias`:18091" `
        -e OLLAMA_URL="http://$BalancerHostAlias`:11434" `
        -e COLLECT_INTERVAL=10 `
        -e HEARTBEAT_INTERVAL=15 `
        -e NODE_LABELS=zone=lan `
        $AgentImage
    Start-Sleep -Seconds 10
}

# 8) Итоговое состояние
Write-Host "=== CLUSTER STATE ==="
try {
    $c = Invoke-RestMethod "$BalancerUrl/api/v1/cluster" -TimeoutSec 5
    Write-Host "backendEngine:      $($c.backendEngine)"
    Write-Host "operatingMode:      $($c.operatingMode)"
    Write-Host "totalBackends:      $($c.totalBackends)"
    Write-Host "healthyBackends:    $($c.healthyBackends)"
    Write-Host "backendTypeCounts:  $($c.backendTypeCounts | ConvertTo-Json -Compress)"
    foreach ($b in $c.backends) {
        Write-Host "Backend: id=$($b.id) status=$($b.status) type=$($b.backendType)"
    }
}
catch {
    Write-Host "Не удалось получить состояние кластера: $_" -ForegroundColor Red
    exit 1
}