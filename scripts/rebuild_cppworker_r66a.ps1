#!/usr/bin/env pwsh
# rebuild_cppworker_r66a.ps1 — пересборка и раскатка cppworker-контейнера.
#
# ИСТОРИЯ (почему скрипт переписан 2026-09-22, R66b):
#   Раньше скрипт делал `docker rm` + `docker run ...` руками. Это создавало
#   контейнер ВНЕ compose, и терялись вещи, которые compose задаёт неявно:
#     * сетевые алиасы (docker compose даёт контейнеру DNS-имя сервиса
#       `cppworker-gpu`). Без него:
#         - webui nginx (proxy_pass http://cppworker-gpu:18092) → 502, вкладка
#           GGUF пустая, /api/worker/* не работает;
#         - balancer poller http://cppworker-gpu:18092/api/metrics → "no such host",
#           метрики llama.cpp/VRAM не доходят, /api/ps → 503;
#         - agent (AGENT_CPPWORKER_URL=http://cppworker-gpu:18092) не собирает
#           GPU/VRAM-метрики;
#         - unload/load/delete модели → 500 "no such host".
#     * env: CPPWORKER_BALANCER_URL/TOKEN, CPPWORKER_REGISTER_DISABLE=false,
#       CPPWORKER_ADVERTISE_HOST/PORT, reasoning-флаги, batched parallel;
#     * тома: ../models:/app/models:rw (скрипт монтировал :ro — ломается
#       HF-докачка/удаление моделей), cppworker_cache:/app/.cache;
#     * runtime: nvidia.
#   Теперь единственный источник истины — docker-compose (deployments/
#   docker-compose.cppworker-bundled-with-agent.yml), а скрипт только собирает
#   образ и просит compose пересоздать сервис.
#
# Использование:
#   pwsh -File scripts/rebuild_cppworker_r66a.ps1                 # tag по умолчанию (R66b)
#   pwsh -File scripts/rebuild_cppworker_r66a.ps1 -Tag r66-submodule-v5
#   pwsh -File scripts/rebuild_cppworker_r66a.ps1 -SkipBuild      # только пересоздать

param(
    # Тег БЕЗ префикса "gpu-": compose собирает имя как ollama-legion/cppworker:gpu-<Tag>.
    [string]$Tag = "r66-submodule-v5",
    [switch]$SkipBuild
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

$imageName = "ollama-legion/cppworker"
$imageTag = "gpu-$Tag"
$composeFile = "docker-compose.cppworker-bundled-with-agent.yml"
$envDir = Join-Path $repoRoot "deployments"

if (-not $SkipBuild) {
    Write-Host "[cppworker] Building ${imageName}:${imageTag} (CUDA_ARCH=86, RTX 3070/sm_86) ..." -ForegroundColor Cyan
    # CUDA_ARCH=86 — только sm_86. Default в Dockerfile.gpu = "all" (sm_50..sm_90,
    # 9 архитектур) — это ~9x дольше и нужно только для multi-arch образов.
    docker build --build-arg CUDA_ARCH=86 -t "${imageName}:${imageTag}" -f docker/cppworker/Dockerfile.gpu .
    if ($LASTEXITCODE -ne 0) { throw "docker build failed (exit $LASTEXITCODE)" }
}

# ВАЖНО: `image: ollama-legion/cppworker:gpu-${CPPWORKER_GPU_TAG}` в compose
# подставляется из deployments/.env (интерполяция compose), а НЕ из env_file
# .env.bundled-with-agent. Держим оба файла синхронными, иначе compose возьмёт
# (или начнёт собирать) не тот образ, который мы только что собрали.
Push-Location $envDir
try {
    $tagLine = "CPPWORKER_GPU_TAG=$Tag"
    (Get-Content ".env") -replace '^CPPWORKER_GPU_TAG=.*', $tagLine | Set-Content ".env"
    (Get-Content ".env.bundled-with-agent") -replace '^CPPWORKER_GPU_TAG=.*', $tagLine | Set-Content ".env.bundled-with-agent"
    Write-Host "[cppworker] CPPWORKER_GPU_TAG=$Tag записан в deployments/.env и deployments/.env.bundled-with-agent" -ForegroundColor Cyan

    Write-Host "[cppworker] Recreating service cppworker-gpu via compose (aliases + env + volumes + nvidia runtime) ..." -ForegroundColor Cyan
    docker compose -f $composeFile up -d --no-deps --force-recreate cppworker-gpu
    if ($LASTEXITCODE -ne 0) { throw "docker compose up failed (exit $LASTEXITCODE)" }
}
finally {
    Pop-Location
}

Start-Sleep -Seconds 8
docker ps --filter name=ol-bundled-cppworker-gpu --format '{{.Names}}\t{{.Status}}\t{{.Image}}'
Write-Host "[cppworker] Проверка DNS-алиаса и API:" -ForegroundColor Cyan
docker exec ol-bundled-cppworker-gpu sh -c "curl -s -m 10 http://127.0.0.1:18092/api/models/files | head -c 200" 2>$null
Write-Host ""
Write-Host "[cppworker] DONE. Ожидаемый алиас: cppworker-gpu" -ForegroundColor Green
Write-Host '  проверка: docker inspect ol-bundled-cppworker-gpu -f ''{{range $k,$v := .NetworkSettings.Networks}}{{$v.DNSNames}}{{end}}'''
Write-Host '  в списке должно быть: [ol-bundled-cppworker-gpu cppworker-gpu <id>]'
