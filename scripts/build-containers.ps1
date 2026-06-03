#!/usr/bin/env pwsh
# =============================================================================
# Сборка Docker-образов OllamaLegion
# =============================================================================
# Использование:
#   .\scripts\build-containers.ps1                    # все образы
#   .\scripts\build-containers.ps1 -Balancer           # только balancer
#   .\scripts\build-containers.ps1 -WebUI              # только webui
#   .\scripts\build-containers.ps1 -Agent              # только agent cppworker
# =============================================================================
param(
    [switch]$Balancer,
    [switch]$WebUI,
    [switch]$Agent,
    [switch]$CppWorker
)

$ErrorActionPreference = "Stop"
Push-Location $PSScriptRoot\..
try {
    if (-not $Balancer -and -not $WebUI -and -not $Agent -and -not $CppWorker) {
        $Balancer = $true
        $WebUI = $true
        $Agent = $true
        $CppWorker = $true
    }

    if ($Balancer) {
        Write-Host "=== Building ollama-legion/balancer ===" -ForegroundColor Cyan
        docker build -t ollama-legion/balancer:latest -f docker/balancer/Dockerfile .
        if ($LASTEXITCODE -ne 0) { throw "Balancer build failed" }
    }

    if ($WebUI) {
        Write-Host "=== Building ollama-legion/webui ===" -ForegroundColor Cyan
        docker compose -f deployments/docker-compose.yml build webui
        if ($LASTEXITCODE -ne 0) { throw "WebUI build failed" }
    }

    if ($Agent) {
        Write-Host "=== Building ollama-legion/agent ===" -ForegroundColor Cyan
        docker build -t ollama-legion/agent:latest -f docker/agent/Dockerfile .
        if ($LASTEXITCODE -ne 0) { throw "Agent build failed" }
    }

    if ($CppWorker) {
        Write-Host "=== Building ollama-legion/cppworker:gpu (CUDA) ===" -ForegroundColor Cyan
        docker build -t ollama-legion/cppworker:gpu -f docker/cppworker/Dockerfile.gpu --target runtime .
        if ($LASTEXITCODE -ne 0) { throw "CppWorker GPU build failed" }

        Write-Host "=== Building ollama-legion/cppworker:cpu ===" -ForegroundColor Cyan
        docker build -t ollama-legion/cppworker:cpu -f docker/cppworker/Dockerfile.cpu --target runtime .
        if ($LASTEXITCODE -ne 0) { throw "CppWorker CPU build failed" }
    }

    Write-Host "`n=== All builds completed ===" -ForegroundColor Green
}
finally {
    Pop-Location
}