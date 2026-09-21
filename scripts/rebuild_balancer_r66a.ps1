#!/usr/bin/env pwsh
# rebuild_balancer_r66a.ps1 — R66a: фикс translator-bug для native Ollama path
#
# Что делает:
#  1. docker build -t ollama-legion/balancer:r66-submodule-v4 -f docker/balancer/Dockerfile .
#  2. останавливает старый ol-bundled-balancer, запускает новый с тем же набором флагов
#
# Использование:
#   pwsh -File scripts/rebuild_balancer_r66a.ps1

$ErrorActionPreference = "Stop"
Set-Location C:\Ollama\ollamalegion

$tag = "r66-submodule-v4"
$imageName = "ollama-legion/balancer"

Write-Host "[R66a] Building balancer image $imageName`:$tag ..." -ForegroundColor Cyan
docker build -t "${imageName}:${tag}" -f docker/balancer/Dockerfile .

Write-Host "[R66a] Stopping current ol-bundled-balancer ..." -ForegroundColor Cyan
docker stop ol-bundled-balancer 2>$null
docker rm ol-bundled-balancer 2>$null

Write-Host "[R66a] Starting new balancer container ..." -ForegroundColor Cyan
docker run -d --gpus all --name ol-bundled-balancer --restart unless-stopped `
  --network ol-bundled-network `
  -p 18080:18080 -p 18081:18081 `
  -e OLLAMA_BALANCER_CONFIG=/app/data/config.json `
  -e GIN_MODE=release `
  -v ollama-legion-balancer-data:/app/data `
  -v ollama-legion-config-shared:/app/config-shared:ro `
  -v ollama-legion-models:/app/models:ro `
  -v ollama-legion-logs:/app/logs `
  -v /var/run/docker.sock:/var/run/docker.sock:ro `
  "${imageName}:${tag}"

Start-Sleep -Seconds 3
docker ps --filter name=ol-bundled-balancer --format '{{.Names}}\t{{.Status}}\t{{.Image}}'
Write-Host "[R66a] DONE." -ForegroundColor Green
