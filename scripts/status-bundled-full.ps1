# =============================================================================
# status-bundled-full.ps1 — show status of bundled-full stack
# =============================================================================
# Usage:
#   .\scripts\status-bundled-full.ps1
# =============================================================================

$ErrorActionPreference = "Stop"

$ProjectName = "ol-bundled-full"

Write-Host "==================================================" -ForegroundColor Cyan
Write-Host "$ProjectName status" -ForegroundColor Cyan
Write-Host "==================================================" -ForegroundColor Cyan

# 1. Containers
Write-Host ""
Write-Host "Containers:" -ForegroundColor Yellow
$containers = docker ps -a --filter "name=$ProjectName" --format "table {{.Names}}`t{{.Status}}`t{{.Image}}" 2>&1
if ($containers) {
    $containers | Out-String | Write-Host
} else {
    Write-Host "  (no containers)" -ForegroundColor DarkGray
}

# 2. Health summary
Write-Host ""
Write-Host "Health:" -ForegroundColor Yellow
foreach ($c in @(
    "ol-bundled-full-balancer",
    "ol-bundled-full-cppworker-gpu",
    "ol-bundled-full-cppworker-gpu-agent",
    "ol-bundled-full-webui"
)) {
    $status = docker inspect --format "{{.State.Health.Status}}" $c 2>$null
    $running = docker inspect --format "{{.State.Running}}" $c 2>$null
    $marker = if ($status -eq "healthy" -and $running -eq "true") { "✓" } else { "✗" }
    $color = if ($status -eq "healthy" -and $running -eq "true") { "Green" } else { "Red" }
    Write-Host "  $marker $c : $status (running=$running)" -ForegroundColor $color
}

# 3. Endpoints
Write-Host ""
Write-Host "Endpoints:" -ForegroundColor Yellow
Write-Host "  WebUI:     http://localhost:18083"
Write-Host "  Ollama API: http://localhost:18080"
Write-Host "  Admin:     http://localhost:18081"
Write-Host "  cppworker: http://localhost:18092"

# 4. Resource usage
Write-Host ""
Write-Host "Resource usage:" -ForegroundColor Yellow
docker stats --no-stream --format "table {{.Name}}`t{{.CPUPerc}}`t{{.MemUsage}}`t{{.NetIO}}`t{{.BlockIO}}" `
    --filter "name=$ProjectName" 2>&1 | Out-String | Write-Host
