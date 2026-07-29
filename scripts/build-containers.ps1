#!/usr/bin/env pwsh
# =============================================================================
# Сборка Docker-образов OllamaLegion
# =============================================================================
# Использование:
#   .\scripts\build-containers.ps1                    # все образы
#   .\scripts\build-containers.ps1 -Balancer           # только balancer
#   .\scripts\build-containers.ps1 -WebUI              # только webui
#   .\scripts\build-containers.ps1 -Agent              # только agent cppworker
#   .\scripts\build-containers.ps1 -CppWorker          # только cppworker
#   .\scripts\build-containers.ps1 -CppWorker -CudaArch "86"   # только cppworker:gpu с тегом :gpu-86
#   .\scripts\build-containers.ps1 -CudaArch "all"    # все поддерживаемые архитектуры (тэг :gpu-arch_all)
# =============================================================================
param(
    [switch]$Balancer,
    [switch]$WebUI,
    [switch]$Agent,
    [switch]$CppWorker,
    [string]$CudaArch = $env:CUDA_ARCH
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
        # Определяем архитектуру CUDA. Пустая/не задана = "all" (по умолчанию в Dockerfile).
        if ([string]::IsNullOrWhiteSpace($CudaArch)) {
            $CudaArch = "all"
        }

        # Нормализуем значение для тега:
        #   "all"        -> arch_all
        #   "75;86;89"   -> arch_75-86-89
        #   "86"         -> 86
        #   "ALL"        -> arch_all
        $tagArchSegment = ""
        $lowerArch = $CudaArch.Trim().ToLower()
        if ($lowerArch -in @("all", "*", "any", "native", "arch_all")) {
            $tagArchSegment = "arch_all"
        } else {
            # заменяем разделители ; , пробел на дефис
            $normalized = ($CudaArch -replace '[;,\s]+', '-').Trim('-')
            if ([string]::IsNullOrEmpty($normalized)) {
                $tagArchSegment = "arch_all"
            } else {
                $tagArchSegment = $normalized
            }
        }

        $gpuImage = "ollama-legion/cppworker:gpu-$tagArchSegment"
        $cpuImage = "ollama-legion/cppworker:cpu"

        Write-Host "=== Building $gpuImage (CUDA_ARCH=$CudaArch) ===" -ForegroundColor Cyan
        $gpuBuildArgs = @()
        if ($lowerArch -ne "arch_all") {
            # передаём --build-arg только если архитектура не "all" (иначе используем дефолт в Dockerfile)
            $gpuBuildArgs += "--build-arg"
            $gpuBuildArgs += "CUDA_ARCH=$CudaArch"
        }
        # Round 14a follow-up (2026-07-29): для одиночной архитектуры (напр. "86")
        # используем Dockerfile.gpu.<arch> — у него более полный COPY в go-builder
        # stage и короче компиляция (только один CUDA arch). Для "all" или
        # нескольких arch — Dockerfile.gpu (универсальный).
        $dockerfile = "docker/cppworker/Dockerfile.gpu"
        if ($lowerArch -notmatch "all" -and $lowerArch -notmatch "-" -and $lowerArch -notmatch ";") {
            $archSpecific = "docker/cppworker/Dockerfile.gpu.$CudaArch"
            if (Test-Path $archSpecific) {
                $dockerfile = $archSpecific
                Write-Host "Using arch-specific Dockerfile: $dockerfile" -ForegroundColor Yellow
            }
        }
        & docker build @gpuBuildArgs -t $gpuImage -f $dockerfile --target runtime .
        if ($LASTEXITCODE -ne 0) { throw "CppWorker GPU build failed" }

        Write-Host "=== Building $cpuImage ===" -ForegroundColor Cyan
        docker build -t $cpuImage -f docker/cppworker/Dockerfile.cpu --target runtime .
        if ($LASTEXITCODE -ne 0) { throw "CppWorker CPU build failed" }
    }

    Write-Host "`n=== All builds completed ===" -ForegroundColor Green
}
finally {
    Pop-Location
}
