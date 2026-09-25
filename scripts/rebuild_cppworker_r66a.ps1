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
    [switch]$SkipBuild,
    # R83: не вешать вариантный «последний» тег-алиас (latest-gpu-86).
    [switch]$NoAlias
)

# R66d (2026-09-22): НЕ 'Stop'. В Windows PowerShell 5.1 (pwsh на машине нет)
# docker пишет прогресс сборки в stderr, каждая строка становится
# NativeCommandError, и с ErrorActionPreference='Stop' скрипт умирал на первой
# же строке прогресса — ещё до проверки $LASTEXITCODE, то есть сборка вообще не
# выполнялась. Та же правка сделана в rebuild_balancer_r66a.ps1 и
# rebuild_webui_r66d.ps1. Ошибки ловим явными проверками $LASTEXITCODE ниже.
$ErrorActionPreference = "Continue"
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

# R83 (2026-09-25): запись тега вынесена в общий helper (lib-image-tags.ps1),
# чтобы все скрипты выпуска делали это ОДИНАКОВО. Логика прежняя: compose
# интерполирует `gpu-${CPPWORKER_GPU_TAG}` из deployments/.env, а не из env_file
# .env.bundled-with-agent, поэтому держим оба файла синхронными.
. (Join-Path $PSScriptRoot 'lib-image-tags.ps1')
Set-ImageTagVar -VariableName 'CPPWORKER_GPU_TAG' -Value $Tag -EnvDir $envDir
if (-not $NoAlias) {
    # ВАЖНО: алиас ВАРИАНТНЫЙ, а не plain `latest`.
    # В репозитории ollama-legion/cppworker лежат взаимоисключающие варианты
    # (cpu / stub / gpu-86 / gpu-arch_all / gpu-llamacpp), а `latest` — один тег на
    # репозиторий: его перезапишет последняя сборка, и `latest` может начать
    # указывать на CPU- или arch_all-сборку. Этот скрипт собирает CUDA_ARCH=86.
    Add-ImageAlias -ImageRef "${imageName}:${imageTag}" -AliasRef "${imageName}:latest-gpu-86"
}

Push-Location $envDir
try {
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
