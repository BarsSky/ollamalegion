# OllamaLegion — Adaptive LLM Inference Cluster

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE.md)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg)](https://www.docker.com/)

Интеллектуальный балансировщик нагрузки для llama.cpp inference с адаптивной загрузкой моделей, авто-подбором параметров под доступные ресурсы и мониторингом GPU/CPU/RAM.

## Что нового в v0.2.0-adaptive

- **Адаптивная загрузка моделей** — авто-подбор GPU-слоёв и типа KV-cache (f16→q8_0→q4_0) под доступную VRAM/RAM
- **KV-cache fallback** — работает даже без GGUF-метаданных (gemma4, новые архитектуры)
- **Авто-reload n_ctx** — при превышении контекста балансер перезагружает модель с бóльшим n_ctx и уменьшенными GPU-слоями
- **Динамический max viable n_ctx** — рассчитывается из VRAM+RAM, без жёстких лимитов
- **Bundled deployment** — готовый стек: balancer + cppworker + agent + webui в одном compose

## Быстрый старт (Bundled)

```powershell
cd C:\Ollama\ollamalegion\deployments
$env:DOCKER_BUILDKIT=1
$env:CUDA_ARCH=86
docker compose -f docker-compose.cppworker-bundled-with-agent.yml --env-file .env.bundled-with-agent up -d --build
```

После запуска:
- Балансер: http://localhost:18080 (Ollama API) / :18081 (Management API)
- CppWorker: http://localhost:18092 (llama.cpp inference)
- WebUI: http://localhost:18083

## Архитектура

```
Клиент (Cline/OpenWebUI)
  → Balancer (:18080) — routing, session affinity, n_ctx auto-reload
    → CppWorker (:18092) — llama.cpp inference
      ├── Adaptive Loader — SelectStrategy: f16→q8_0→q4_0, MoE, partial offload
      ├── KV-cache fallback — работает без GGUF metadata
      └── Auto-reload API — /api/models/reload с адаптивными параметрами
    → Agent (:18032) — NVML GPU/CPU/RAM метрики, health check
  → WebUI (:18083) — дашборд, мониторинг, sparkline
```

## Порты

| Порт | Компонент | Назначение |
|------|-----------|------------|
| 18080 | Balancer | Ollama API proxy |
| 18081 | Balancer | Management API + WebSocket |
| 18092 | CppWorker | llama.cpp inference |
| 18032 | Agent | Метрики GPU/CPU/RAM |
| 18083 | WebUI | Дашборд |

## Сборка

```powershell
# cppworker GPU (cuda 12.x, sm_86)
$env:DOCKER_BUILDKIT=1
$env:CUDA_ARCH=86
cd deployments
docker compose -f docker-compose.cppworker-bundled-with-agent.yml --env-file .env.bundled-with-agent build cppworker-gpu

# Только balancer
docker compose -f docker-compose.cppworker-bundled-with-agent.yml --env-file .env.bundled-with-agent build loadbalancer

# Всё вместе
docker compose -f docker-compose.cppworker-bundled-with-agent.yml --env-file .env.bundled-with-agent build
```

BuildKit=1 обязателен — с ним Go-only изменения собираются за ~30 сек (cached CUDA layers).

## Конфигурация

### .env (ключевые переменные)

```bash
CPPWORKER_CTX_SIZE=65536       # контекст по умолчанию
CPPWORKER_GPU_LAYERS=20        # слоёв на GPU (-1=все, -2=авто)
CPPWORKER_AUTO_OFFLOAD=true    # авто-расчёт gpuLayers
CPPWORKER_AUTO_TUNE_NCTX=true  # авто-подбор n_ctx
CPPWORKER_AUTO_KV_CACHE=true   # авто-выбор kvCacheType (q4_0/q8_0/f16)
CPPWORKER_RAM_FALLBACK_N_CTX=true  # RAM fallback при нехватке VRAM
```

### Балансер (config.json)

```json
{
  "balancing": {
    "firstByteTimeout": 600,
    "nctxReload": {
      "auto_reload_n_ctx": true,
      "auto_reload_max_n_ctx": 131072,
      "auto_reload_vram_safety_factor": 0.85
    }
  }
}
```

## Структура проекта

```
cmd/
  balancer/          — HTTP proxy + Management API
  cppworker/         — llama.cpp inference server
    adaptive_loader.go  — SelectStrategy: авто-подбор параметров
    handlers_model.go   — load/reload с адаптивной стратегией
    lazyload.go         — ленивая загрузка моделей
  agent/             — GPU/CPU метрики (NVML)
internal/
  balancer/
    nctx_reload.go          — auto-reload n_ctx
    nctx_reload_adaptive.go — запрос стратегии у cppworker
  cppbackend/
    backend.go              — C-bridge, VRAM estimation, KV-cache
deployments/
  docker-compose.cppworker-bundled-with-agent.yml  — основной стек
  .env.bundled-with-agent                          — переменные окружения
```

## Документация

- [docs/README.md](docs/README.md) — индекс документации
- [docs/installation.md](docs/installation.md) — установка и сборка
- [docs/deployment.md](docs/deployment.md) — развёртывание
- [docs/api.md](docs/api.md) — REST API
- [plans/README.md](plans/README.md) — roadmap

## Лицензия

MIT
