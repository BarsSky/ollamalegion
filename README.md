# OllamaLegion — Adaptive LLM Inference Cluster

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE.md)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg)](https://www.docker.com/)

Интеллектуальный балансировщик нагрузки для llama.cpp inference с адаптивной загрузкой моделей, авто-подбором параметров под доступные ресурсы и мониторингом GPU/CPU/RAM.

## Что нового

**v0.2.0-adaptive** (в разработке, см. `CHANGELOG.md` → `[Unreleased]`):

- **Адаптивная загрузка моделей** — авто-подбор GPU-слоёв и типа KV-cache (f16→q8_0→q4_0) под доступную VRAM/RAM
- **KV-cache fallback** — работает даже без GGUF-метаданных (gemma4, новые архитектуры)
- **Авто-reload n_ctx** — при превышении контекста балансер перезагружает модель с бóльшим n_ctx и уменьшенными GPU-слоями
- **Динамический max viable n_ctx** — рассчитывается из VRAM+RAM, без жёстких лимитов
- **Bundled deployment** — готовый стек: balancer + cppworker + agent + webui в одном compose
- **Phase 8 P.1 — RPC Coordinator (production mode)** — distributed inference через RPC workers
- **Phase 8 P.2 — Virtual Models (alias-on-pool)** — управление виртуальными моделями через `/api/v1/virtual-models`
- **Phase 8 P.3 — Tensor Parallelism (research)** — ресёрч-фаза, post-1.0

Последний релиз: **v0.1.0** (2026-05-14). Все «Unreleased» секции в `CHANGELOG.md`
описывают в разработке, но ещё не зарелижены.

## Быстрый старт (Bundled)

Требуется: Docker 24+ с поддержкой Compose v2, Git, NVIDIA драйвер + NVIDIA Container Toolkit (для GPU-режима).

```bash
# 1. Клонировать репозиторий (имя каталога — произвольное)
git clone https://github.com/BarsSky/ollamalegion.git
cd ollamalegion

# 2. Подготовить .env (один раз)
cp deployments/.env.bundled-with-agent.example deployments/.env.bundled-with-agent
# отредактируйте deployments/.env.bundled-with-agent под свою машину
# (минимум — замените CPPWORKER_API_TOKEN на свой)

# 3. Запустить стек
```

**Windows (PowerShell):**

```powershell
$env:DOCKER_BUILDKIT=1
$env:CUDA_ARCH=86        # sm_86 для RTX 30xx, sm_89 для RTX 40xx, sm_90 для RTX 50xx
docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml `
               --env-file  deployments/.env.bundled-with-agent `
               up -d --build
```

**Linux / macOS / WSL2:**

```bash
export DOCKER_BUILDKIT=1
export CUDA_ARCH=86
docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml \
               --env-file  deployments/.env.bundled-with-agent \
               up -d --build
```

После запуска:
- Балансер: http://localhost:18080 (Ollama API) / :18081 (Management API)
- CppWorker: http://localhost:18092 (llama.cpp inference)
- WebUI: http://localhost:18083

> Если нужна более простая сборка **без sidecar-агента метрик**, используйте
> `deployments/docker-compose.cppworker-bundled.yml` + готовые скрипты:
> `.\scripts\start-bundled.ps1` (Windows) или `./scripts/start-bundled.sh` (Linux/macOS).

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

Все команды выполняются из **корня репозитория** (каталог, в который вы склонировали проект).

**Windows (PowerShell):**

```powershell
$env:DOCKER_BUILDKIT=1
$env:CUDA_ARCH=86     # подставьте своё значение: 86/89/90/120

# cppworker GPU (cuda 12.x, sm_86)
docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml `
               --env-file  deployments/.env.bundled-with-agent `
               build cppworker-gpu

# Только balancer
docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml `
               --env-file  deployments/.env.bundled-with-agent `
               build loadbalancer

# Всё вместе
docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml `
               --env-file  deployments/.env.bundled-with-agent `
               build
```

**Linux / macOS / WSL2:**

```bash
export DOCKER_BUILDKIT=1
export CUDA_ARCH=86

docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml \
               --env-file  deployments/.env.bundled-with-agent \
               build cppworker-gpu

docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml \
               --env-file  deployments/.env.bundled-with-agent \
               build loadbalancer

docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml \
               --env-file  deployments/.env.bundled-with-agent \
               build
```

BuildKit=1 обязателен — с ним Go-only изменения собираются за ~30 сек (cached CUDA layers).

## Конфигурация

### .env (ключевые переменные)

Дефолты — из `deployments/.env.bundled-with-agent.example` и `config/cppworker-defaults.json` (defaultCtxSize=32768).

```bash
CPPWORKER_CTX_SIZE=32768       # контекст по умолчанию
CPPWORKER_GPU_LAYERS=20        # слоёв на GPU (-1=все, -2=авто)
CPPWORKER_AUTO_OFFLOAD=true    # авто-расчёт gpuLayers
CPPWORKER_AUTO_TUNE_NCTX=true  # авто-подбор n_ctx
CPPWORKER_AUTO_KV_CACHE=true   # авто-выбор kvCacheType (q4_0/q8_0/f16)
CPPWORKER_RAM_FALLBACK_N_CTX=true  # RAM fallback при нехватке VRAM
```

Дополнительные (в .env-файле, но редко меняются):
`CPPWORKER_BATCH_SIZE`, `CPPWORKER_FLASH_ATTN_TYPE`, `CPPWORKER_N_THREADS`,
`CPPWORKER_NUMA`, `CPPWORKER_USE_MMAP`, `CPPWORKER_SPLIT_MODE`,
`CPPWORKER_RAM_FALLBACK_GPU_LAYERS`, `CPPWORKER_RAM_FALLBACK_MAX_N_CTX`,
`CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS`, `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD`,
`CPPWORKER_DEFAULT_N_PREDICT_REASONING` (reasoning-модели, default 8192),
`OLLAMALEGION_HEARTBEAT_MS`.

### Балансер (config.json)

Полный пример — `config/config.example.json`. Минимальный релевантный фрагмент
(с реальными дефолтами из `config/config.example.json`):

```json
{
  "balancing": {
    "firstByteTimeout": 30,
    "nctxReload": {
      "auto_reload_n_ctx": true,
      "auto_reload_max_n_ctx": 262144,
      "auto_reload_vram_safety_factor": 0.85,
      "auto_reload_timeout_sec": 90
    }
  }
}
```

> ENV-overrides для `nctxReload` (приоритет над config.json):
> `LB_NCTX_RELOAD_ENABLED`, `LB_NCTX_RELOAD_MAX_N_CTX`,
> `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR`, `LB_NCTX_RELOAD_TIMEOUT_SEC`.
> Читаются в `internal/balancer/nctx_reload_config_bridge.go`.

## Структура проекта

```
cmd/
  balancer/          — HTTP proxy + Management API (port 18080/18081)
  cppworker/         — llama.cpp inference server (port 18092)
    adaptive_loader.go     — SelectStrategy: авто-подбор параметров
    adaptive_integration.go — adaptive API routes, NaN-healer
    handlers_model.go      — load/reload с адаптивной стратегией
    lazyload.go            — ленивая загрузка моделей
    reasoning_content.go   — reasoning extraction (qwen3.5, deepseek-r1, gemma-4)
    balancer_register.go   — auto-register в балансер
    nctx_clamp.go          — preflight clamp по VRAM
  agent/             — GPU/CPU метрики (NVML, port 18032)
  monitor/           — встроенный монитор-страница (HTML UI)
  rpcworker/         — RPC worker для distributed inference (Phase 8 P.1)

internal/
  api/               — HTTP handlers, routes, auth, rate-limit
    routes.go              — все REST endpoints балансера
    gguf_backend_proxy.go  — proxy /api/v1/gguf/backends/{id}/proxy/*
  balancer/
    nctx_reload.go          — auto-reload n_ctx
    nctx_reload_adaptive.go — запрос стратегии у cppworker
    proxy.go, ollama_router.go, llamacpp_router.go — routing
    scoring.go              — resource-aware scoring
    session_manager.go      — session affinity, stickiness
    rpc_coordinator_dispatcher.go — Phase 8 P.1 RPC coord
  cppbackend/
    backend.go        — Go-обёртка над C-bridge, params, KVCacheType
  agent/             — общие типы для agent
  config/            — config loading, validation
  modelreplication/  — per-model replication groups
  rpccoordinator/    — RPC coordinator state + workers
  rpcworker/         — RPC worker runtime
  rptensor/          — tensor parallelism (P.3, research)
  virtualmodel/      — Phase 8 P.2 virtual models (alias-on-pool)

c/                   — C-bridge к llama.cpp (build-msvc-cuda / build-cpu-mingw)
  llama.cpp/         — submodule: исходники llama.cpp
  bridge/            — Go ↔ C bridge
  ggml/              — submodule: GGML tensor library

deployments/
  docker-compose.cppworker-bundled-with-agent.yml  — основной стек
  docker-compose.cppworker-bundled.yml             — без sidecar-agent
  docker-compose.cppworker.yml                     — одиночный cppworker
  docker-compose.agent.yml                         — только agent
  .env.bundled-with-agent.example                  — переменные для основного стека
  .env.bundled.example                             — для упрощённого стека

config/
  config.example.json         — шаблон основного config балансера
  config.bundled.json         — bundled-конфиг для docker-compose
  cppworker-defaults.json     — дефолты для CppWorker (defaultCtxSize=32768)
  cppworker.example.env       — env-файл для локального cppworker
  agent.example.env           — env-файл для локального agent

scripts/             — утилиты: start-bundled.{ps1,sh}, build-containers, e2e-тесты
docs/                — полная документация (RU + EN)
plans/               — roadmap, ADR, фазовые отчёты
```

## Документация

- [docs/README.md](docs/README.md) — индекс документации
- [docs/installation.md](docs/installation.md) — установка и сборка
- [docs/deployment.md](docs/deployment.md) — развёртывание
- [docs/api.md](docs/api.md) — REST API
- [plans/README.md](plans/README.md) — roadmap

## Лицензия

MIT
