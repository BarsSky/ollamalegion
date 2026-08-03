# =============================================================================
# start-bundled-full.ps1 — single command to start full OllamaLegion stack
# =============================================================================
# Запускает:
#   - loadbalancer  :18080/:18081
#   - cppworker-gpu :18092  (GPU inference)
#   - agent         :sidecar для GPU/VRAM/CPU метрик
#   - webui         :18083
#
# Usage:
#   .\scripts\start-bundled-full.ps1
#   .\scripts\start-bundled-full.ps1 -Rebuild         # force rebuild образов
#   .\scripts\start-bundled-full.ps1 -Detached        # start in background (default)
#   .\scripts\start-bundled-full.ps1 -Foreground      # stream logs
#
# После старта:
#   - WebUI:     http://localhost:18083
#   - Ollama API: http://localhost:18080  (use CPPWORKER_API_TOKEN from .env)
#   - Admin:     http://localhost:18081  (use same token)
#   - cppworker: http://localhost:18092  (direct, internal)
# =============================================================================

[CmdletBinding()]
param(
    [switch]$Rebuild,
    [switch]$Detached = $true,
    [switch]$Foreground,
    [switch]$NoBuild
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

# Resolve repo root (this script is in scripts/, repo is parent)
$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
$DeployDir = Join-Path $RepoRoot "deployments"
$ComposeFile = Join-Path $DeployDir "docker-compose.bundled-full.yml"
$EnvFile = Join-Path $DeployDir ".env.bundled-full"
$ProjectName = "ol-bundled-full"

Write-Host "==================================================" -ForegroundColor Cyan
Write-Host "OllamaLegion bundled-full stack" -ForegroundColor Cyan
Write-Host "==================================================" -ForegroundColor Cyan
Write-Host "Repo:    $RepoRoot"
Write-Host "Compose: $ComposeFile"
Write-Host "Env:     $EnvFile"
Write-Host ""

# 1. Pre-flight checks
Write-Host "[1/4] Pre-flight checks..." -ForegroundColor Yellow

# Docker daemon
try {
    $null = docker info 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "docker info failed"
    }
    Write-Host "  ✓ Docker daemon running"
}
catch {
    Write-Host "  ✗ Docker daemon NOT accessible" -ForegroundColor Red
    Write-Host "    Start Docker Desktop and retry." -ForegroundColor Red
    exit 1
}

# NVIDIA Container Toolkit (предупреждение если нет)
try {
    $nvidiaTest = docker run --rm --runtime=nvidia --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi 2>&1
    if ($LASTEXITCODE -eq 0) {
        Write-Host "  ✓ NVIDIA Container Toolkit working"
    } else {
        Write-Host "  ⚠ NVIDIA Container Toolkit may not work — agent metrics may fail" -ForegroundColor Yellow
    }
}
catch {
    Write-Host "  ⚠ Cannot test NVIDIA Container Toolkit (will try anyway)" -ForegroundColor Yellow
}

# Compose file
if (-not (Test-Path $ComposeFile)) {
    Write-Host "  ✗ Compose file not found: $ComposeFile" -ForegroundColor Red
    exit 1
}
Write-Host "  ✓ Compose file exists"

# Env file
if (-not (Test-Path $EnvFile)) {
    Write-Host "  ⚠ Env file not found, creating with defaults: $EnvFile" -ForegroundColor Yellow
    & $PSScriptRoot\help\..\deployments\.env.bundled-full.example $EnvFile 2>$null
    if (-not (Test-Path $EnvFile)) {
        Write-Host "    No example either, using inline defaults" -ForegroundColor Yellow
    }
}
Write-Host "  ✓ Env file ready"

# .env substitution check (Docker Compose auto-loads .env, не --env-file)
# Чтобы избежать конфликта, переименуем .env (если есть) чтобы compose не путал
$DefaultEnv = Join-Path $RepoRoot ".env"
$EnvBackup = $null
if (Test-Path $DefaultEnv) {
    $EnvBackup = "$DefaultEnv.bundled-full.bak"
    Write-Host "  ⚠ Found $DefaultEnv — backing up to $EnvBackup" -ForegroundColor Yellow
    Move-Item $DefaultEnv $EnvBackup -Force
}

# 2. Stop existing (если запущен)
Write-Host ""
Write-Host "[2/4] Stopping existing $ProjectName (if any)..." -ForegroundColor Yellow
docker compose -p $ProjectName -f $ComposeFile --env-file $EnvFile down --remove-orphans 2>&1 | Out-String | Write-Host

# 3. Start
Write-Host ""
Write-Host "[3/4] Starting $ProjectName..." -ForegroundColor Yellow
$composeArgs = @(
    "-p", $ProjectName
    "-f", $ComposeFile
    "--env-file", $EnvFile
    "up"
)
if ($Detached -or (-not $Foreground)) { $composeArgs += "-d" }
if ($Rebuild) { $composeArgs += "--build" }
elseif (-not $NoBuild) { $composeArgs += "--build" }

Write-Host "  docker compose $($composeArgs -join ' ')"
docker compose @composeArgs
if ($LASTEXITCODE -ne 0) {
    Write-Host "  ✗ docker compose up failed" -ForegroundColor Red
    exit 1
}

# 4. Wait for health
Write-Host ""
Write-Host "[4/4] Waiting for services to be healthy..." -ForegroundColor Yellow
$containers = @(
    "ol-bundled-full-balancer",
    "ol-bundled-full-cppworker-gpu",
    "ol-bundled-full-cppworker-gpu-agent",
    "ol-bundled-full-webui"
)
$healthyCount = 0
for ($i = 0; $lt -le 90; $lt++) {
    Start-Sleep -Seconds 5
    $allHealthy = $true
    foreach ($c in $containers) {
        $status = docker inspect --format '{{.State.Health.Status}}' $c 2>$null
        $running = docker inspect --format '{{.State.Running}}' $c 2>$null
        if ($running -ne "true") {
            $allHealthy = $false
            break
        }
        if ($status -ne "healthy") {
            $allHealthy = $false
        }
    }
    if ($allHealthy) {
        $healthyCount = $containers.Count
        break
    }
}

if ($healthyCount -eq $containers.Count) {
    Write-Host "  ✓ All 4 services healthy" -ForegroundColor Green
} else {
    Write-Host "  ⚠ Some services not yet healthy (check docker ps)" -ForegroundColor Yellow
}

# 5. Show status + smoke test
Write-Host ""
Write-Host "==================================================" -ForegroundColor Green
Write-Host "✓ Stack started" -ForegroundColor Green
Write-Host "==================================================" -ForegroundColor Green
Write-Host "  WebUI:     http://localhost:18083"
Write-Host "  Ollama API: http://localhost:18080  (use CPPWORKER_API_TOKEN)"
Write-Host "  Admin:     http://localhost:18081  (use same token)"
Write-Host "  cppworker: http://localhost:18092  (direct, internal)"
Write-Host ""

# Show running containers
Write-Host "Running containers:" -ForegroundColor Cyan
docker ps --filter "name=ol-bundled-full" --format "table {{.Names}}\t{{.Status}}\t{{.Image}}" | Out-String | Write-Host

# Smoke test
Write-Host "Smoke test:" -ForegroundColor Cyan
try {
    $modelsResp = Invoke-RestMethod -Uri "http://localhost:18080/v1/models" `
        -Headers @{Authorization = "Bearer $(Get-Content $EnvFile | Select-String '^CPPWORKER_API_TOKEN=' | ForEach-Object { ($_ -split '=', 2)[1] })"} `
        -TimeoutSec 10
    Write-Host "  ✓ /v1/models → $($modelsResp.data.Count) model(s)" -ForegroundColor Green
} catch {
    Write-Host "  ⚠ /v1/models not yet responding: $($_.Exception.Message)" -ForegroundColor Yellow
}

# Show logs hint
Write-Host ""
Write-Host "To stream logs:  docker compose -p $ProjectName -f $ComposeFile logs -f" -ForegroundColor Yellow
Write-Host "To stop:         .\scripts\stop-bundled-full.ps1" -ForegroundColor Yellow

# Restore .env
if ($EnvBackup) {
    Move-Item $EnvBackup $DefaultEnv -Force
}
