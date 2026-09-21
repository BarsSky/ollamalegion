#!/usr/bin/env pwsh
# rebuild_cppworker_r66a.ps1 — R66a: фикс Mistral [TOOL_CALLS] = [...] parsing
#
# Изменение в cmd/cppworker/tool_calls.go:
#   extractMistralToolCalls теперь strip'ит "= " префикс после [TOOL_CALLS].
#   Qwen3-Instruct и некоторые Mistral-Nemo варианты выдают
#   "[TOOL_CALLS] = [...]" вместо "[TOOL_CALLS][...]" — без strip'а
#   json.Unmarshal падает, и tool_call остаётся текстом в content.
#
# Использование:
#   pwsh -File scripts/rebuild_cppworker_r66a.ps1

$ErrorActionPreference = "Stop"
Set-Location C:\Ollama\ollamalegion

$tag = "gpu-r66-submodule-v4"
$imageName = "ollama-legion/cppworker"

Write-Host "[R66a cppworker] Building $imageName`:$tag ..." -ForegroundColor Cyan
docker build -t "${imageName}:${tag}" -f docker/cppworker/Dockerfile.gpu .

Write-Host "[R66a cppworker] Stopping current container ..." -ForegroundColor Cyan
docker stop ol-bundled-cppworker-gpu 2>$null
docker rm ol-bundled-cppworker-gpu 2>$null

Write-Host "[R66a cppworker] Starting new container ..." -ForegroundColor Cyan
docker run -d --gpus all --name ol-bundled-cppworker-gpu --restart unless-stopped `
  --network ollama-legion-bundled-net `
  -p 18092:18092 `
  -v ollama-legion-models:/app/models:ro `
  -v ollama-legion-config-shared:/app/config-shared:ro `
  -v ollama-legion-cppworker-data:/app/data `
  -v ollama-legion-logs:/app/logs `
  -e CPPWORKER_HOST=0.0.0.0 -e CPPWORKER_PORT=18092 `
  -e CPPWORKER_MODELS_DIR=/app/models `
  -e CPPWORKER_CTX_SIZE=32768 -e CPPWORKER_BATCH_SIZE=512 `
  -e CPPWORKER_GPU_LAYERS=12 -e CPPWORKER_FLASH_ATTN_TYPE=-1 `
  -e CPPWORKER_RAM_FALLBACK_N_CTX=true -e CPPWORKER_RAM_FALLBACK_GPU_LAYERS=-2 `
  -e CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000 `
  -e CPPWORKER_AUTO_OFFLOAD=true -e CPPWORKER_AUTO_TUNE_NCTX=true `
  -e CPPWORKER_USE_MMAP=true -e CPPWORKER_API_TOKEN=changeme-bundled-with-agent-token `
  -e CPPWORKER_BALANCER_URL= `
  -e CPPWORKER_REGISTER_DISABLE=true `
  -e CUDA_ARCH=86 `
  "${imageName}:${tag}"

Start-Sleep -Seconds 5
docker ps --filter name=ol-bundled-cppworker-gpu --format '{{.Names}}\t{{.Status}}\t{{.Image}}'
Write-Host "[R66a cppworker] DONE." -ForegroundColor Green
