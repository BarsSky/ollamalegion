# Развёртывание OllamaLegion

> **Версия:** 3.0 (2026-07-31, обновлено под v0.5.2)
> **Связанные документы:** [`installation.md`](installation.md), [`agent-deployment.md`](agent-deployment.md), [`backend-type-isolation.md`](backend-type-isolation.md), [`../../CHANGELOG.md`](../CHANGELOG.md)

## Что нового в v0.5.x (по сравнению с v0.2.0)

Список фич, появившихся с момента предыдущей версии этого документа (2026-06-22):

- **Round 13 (n_parallel > 1)** — несколько одновременных sequences на одной модели без race conditions
- **Round 14 (native enable_thinking)** — для Qwen3-thinking моделей без `<think>` block
- **Round 15.1 (Batched Parallel Inference)** — true parallel llama_decode через BatchedScheduler
- **Round 15.2 (multi-token prefill + temperature sampling + vocab-aware EOG)** — production-ready sampling
- **Round 16 (Round 16 code review)** — CRITICAL `temperature=0` fix, 4 P1 observability/resilience fixes
- **WebUI Sprint 30** — version display wired to git tag
- **Auth improvements** — `X-API-Token` для Cline/Roo compatibility, Cline-style streaming
- **Bundle with sidecar agent** — `docker-compose.cppworker-bundled-with-agent.yml` для production

Подробности: см. [CHANGELOG.md](../../CHANGELOG.md).

## Содержание

1. [Сценарии развёртывания](#1-сценарии-развёртывания)
2. [Bundled-стек (cppworker-gpu + balancer + webui)](#2-bundled-стек-cppworker-gpu--balancer--webui)
3. [Production-конфигурация](#3-production-конфигурация)
4. [Масштабирование](#4-масштабирование)
5. [Healthcheck и observability](#5-healthcheck-и-observability)
6. [Таблица портов](#6-таблица-портов)

---

## 1. Сценарии развёртывания

### 1.1 Минимальный (только балансер + WebUI)

```bash
cd deployments
docker compose up -d --build
```

Поднимает:
- `loadbalancer` (18080, 18081)
- `webui` (18083)

Бэкенды (Ollama) добавляются вручную через WebUI или API.

### 1.2 Bundled (cppworker-gpu + balancer + webui)

Самый частый production-сценарий. Подробнее см. §2.

```bash
./scripts/start-bundled.sh  # Linux/macOS/WSL
# или
.\scripts\start-bundled.ps1  # Windows PowerShell
```

### 1.3 CppWorker отдельно

```bash
# GPU
cd deployments
docker compose -f docker-compose.cppworker.yml --profile gpu up -d --build

# CPU
docker compose -f docker-compose.cppworker.yml --profile cpu up -d --build

# Stub (для тестов без llama.cpp)
docker compose -f docker-compose.cppworker.yml --profile stub up -d --build
```

### 1.4 Агент на отдельном Ollama-сервере

```bash
# Создать .env
cp config/agent.example.env deployments/.env
# отредактировать .env (BALANCER_URL, AGENT_PUBLIC_HOST, GPU_MODE)

cd deployments
docker compose -f docker-compose.agent.yml --env-file .env up -d --build
```

Подробнее: [`agent-deployment.md`](agent-deployment.md).

### 1.5 Смешанный кластер

```bash
# Балансер + WebUI + CppWorker (GPU) + Агент
docker compose \
  -f deployments/docker-compose.yml \
  -f deployments/docker-compose.cppworker.yml --profile gpu \
  -f deployments/docker-compose.agent.yml --env-file .env \
  up -d --build
```

Подробнее: [`backend-type-isolation.md` §9](backend-type-isolation.md#9-варианты-развёртывания).

---

## 2. Bundled-стек (cppworker-gpu + balancer + webui)

### 2.1 Архитектура

```
┌─────────────────────────────────────────────────────┐
│                  Docker Compose                     │
│                                                      │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────┐ │
│  │ loadbalancer │  │   webui      │  │cppworker- │ │
│  │ :18080       │◀─┤  :18083      │  │gpu        │ │
│  │ :18081       │  └──────────────┘  │ :18092    │ │
│  └──────▲───────┘                   └─────▲─────┘ │
│         │                                 │       │
│         │       ┌──────────────┐          │       │
│         └───────┤  agent (sidecar) ├──────┘       │
│                 │  :18032       │                  │
│                 └──────────────┘                  │
└─────────────────────────────────────────────────────┘
```

Сеть: `ollama-legion-net` (external), `cppworker-net` (internal).

### 2.2 Подготовка `.env.bundled`

```bash
cd deployments
cp .env.bundled.example .env.bundled
# Отредактируйте .env.bundled:
```

| ENV | Назначение | Default |
|---|---|---|
| `CPPWORKER_API_TOKEN` | токен для auto-registration | обязательно |
| `CUDA_ARCH` | архитектура GPU (86 для RTX 3070/3080/3090, 89 для 4090) | 86 |
| `CPPWORKER_GPU_LAYERS` | -1 (все), 0 (CPU), N | -1 |
| `CPPWORKER_RAM_FALLBACK_N_CTX` | reload в RAM при OOM | true |
| `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` | верхняя граница | 128000 |
| `LB_NCTX_RELOAD_ENABLED` | auto-reload на балансировщике | true |
| `BALANCER_URL` | для cppworker auto-registration | http://loadbalancer:18081 |
| `CPPWORKER_ADVERTISE_HOST` | DNS-имя для регистрации | cppworker-gpu |
| `CPPWORKER_PORT` (внутри) | порт cppworker'а | 18091 |
| `CPPWORKER_ADVERTISED_PORT` | порт для балансировщика | 18091 |

### 2.3 Запуск

```bash
# Linux/macOS/WSL
./scripts/start-bundled.sh rebuild    # пересобрать + запустить

# Windows PowerShell (Legacy builder обязателен!)
$env:DOCKER_BUILDKIT=0
.\scripts\start-bundled.ps1 -Rebuild

# Остановка
./scripts/start-bundled.sh down

# Логи
./scripts/start-bundled.sh logs
```

### 2.4 Проверка после запуска

```bash
# 1. Health балансировщика (liveness)
curl http://localhost:18081/api/v1/ping
# Ожидается: 200 OK

# 2. Health cppworker (напрямую)
curl http://localhost:18092/health
# Ожидается: {"status":"ok"}

# 3. Список бэкендов (через API)
curl -H 'X-API-Token: <CPPWORKER_API_TOKEN>' http://localhost:18081/api/v1/backends
# Ожидается: содержит cppworker-gpu-bundled

# 4. Тестовая генерация
curl -X POST http://localhost:18081/api/generate \
  -H "Content-Type: application/json" \
  -H "X-API-Token: <token>" \
  -d '{"model":"<model.gguf>","prompt":"hello","stream":false}'

# 5. WebUI Dashboard
open http://localhost:18083
```

### 2.5 Auto-registration cppworker

При старте cppworker автоматически регистрируется в балансировщике:

```
POST http://loadbalancer:18081/api/v1/backends
{
  "id": "cppworker-gpu",
  "host": "cppworker-gpu",  // DNS внутри compose
  "cppWorkerPort": 18091,
  "type": "llama_cpp"
}
```

**Важно:**
- `CPPWORKER_ADVERTISE_HOST` — DNS-имя, под которым балансировщик достучится до cppworker.
- В bundled-compose по умолчанию `cppworker-gpu` (имя сервиса в compose).
- Для регистрации **по IP** (вне Docker-сети): задать `BALANCER_URL=http://<host-ip>:18081` и `CPPWORKER_ADVERTISE_HOST=<host-ip>`.

### 2.6 Healthcheck в bundled

- Балансировщик healthcheck: `GET /api/v1/ping` (всегда 200).
- CppWorker healthcheck: `/app/cppworker -healthcheck` (бинарник сам читает `CPPWORKER_PORT` и делает `GET /health` на localhost).

В `deployments/docker-compose.cppworker-bundled.yml`:
```yaml
healthcheck:
  test: ["CMD", "/app/cppworker", "-healthcheck"]
```

---

## 3. Production-конфигурация

### 3.1 `config/config.json`

Базовый шаблон в `config/config.example.json`. Ключевые секции:

```json
{
  "loadBalancer": {
    "host": "0.0.0.0",
    "port": 18080,
    "apiPort": 18081,
    "tlsPort": 8443,
    "tlsApiPort": 8444
  },
  "balancing": {
    "algorithm": "resource-aware",
    "modelAffinity": true,
    "sessionStickiness": true,
    "useEnhancedScoring": true,
    "prewarm": { "enabled": true },
    "autoPull": { "enabled": true },
    "nctxReload": {
      "autoReloadNCtx": true,
      "maxNCtx": 131072,
      "vramSafetyFactor": 0.85,
      "timeoutSec": 120
    },
    "requestTimeout": 600,
    "queueTimeout": 300,
    "queueMaxSize": 100
  },
  "operatingMode": "standard",
  "defaultModelProfile": {
    "contextLength": 0,    // ⚠️ должно быть 0 или null
    "batchSize": 512,
    "numGpuLayers": -1,
    "flashAttn": true
  },
  "llamaCppModelProfiles": {
    // per-model overrides (см. cppworker-model-params.md)
  }
}
```

Подробнее: [`configuration.md`](configuration.md) — скоро будет переписан.

### 3.2 ENV-переменные

Все настройки `config.json` можно override через ENV с префиксом `LB_` или прямым именем:

```bash
LB_HOST=0.0.0.0
LB_PORT=18080
LB_API_PORT=18081
LB_ALGORITHM=resource-aware
LB_MODEL_AFFINITY=true
LB_SESSION_STICKINESS=true
LB_HEALTH_CHECK_INTERVAL=10
LB_METRICS_INTERVAL=5
LB_REQUEST_TIMEOUT=600
LB_QUEUE_TIMEOUT=300
LB_QUEUE_MAX_SIZE=100
LB_GPU_MAX_USAGE=90
LB_GPU_MAX_VRAM=85
LB_GPU_MAX_TEMP=85
LB_CPU_MAX_USAGE=80
LB_MEMORY_MAX_USAGE=85
LB_LOG_LEVEL=info
LB_LOG_FORMAT=json
```

### 3.3 Backend через ENV

```bash
BACKEND_0_ID=gpu-1
BACKEND_0_HOST=192.168.13.66
BACKEND_0_PORT=11434
BACKEND_0_AGENT_PORT=18032
BACKEND_0_TYPE=ollama   # или llama_cpp
```

### 3.4 Аутентификация

```bash
AUTH_ENABLED=true
AUTH_TOKENS=master-token,client-token-1,client-token-2
AUTH_HEADER_NAME=X-API-Token
```

### 3.5 TLS

```bash
TLS_ENABLED=true
TLS_CERT_FILE=/etc/ssl/certs/server.crt
TLS_KEY_FILE=/etc/ssl/private/server.key
TLS_AUTO_CERT=true   # self-signed для тестов
```

---

## 4. Масштабирование

### 4.1 Горизонтальное (несколько бэкендов)

Добавить бэкенды через WebUI → Backends → Add, или через API:

```bash
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -H "X-API-Token: <token>" \
  -d '{"id":"gpu-2","host":"192.168.13.70","ollamaPort":11434,"type":"ollama"}'
```

### 4.2 Replication (Variant A)

Включить в `config/config.json`:

```json
{
  "modelReplication": {
    "enabled": true,
    "groups": [
      {
        "modelName": "llama3.1-8b",
        "minInstances": 2,
        "maxInstances": 4,
        "targetBackends": ["gpu-1", "gpu-2", "gpu-3"],
        "idleUnloadSec": 300
      }
    ]
  }
}
```

Подробнее: [`backend-type-isolation.md` §3](backend-type-isolation.md).

### 4.3 Virtual Router (Variant C, каркас)

```json
{
  "virtualModels": [
    {
      "name": "llama-mega",
      "description": "Pipeline: embed + middle + output",
      "slices": [
        { "id": "embed",   "modelName": "nomic-embed", "ordinal": 0, "targetBackends": ["gpu-1"] },
        { "id": "middle",  "modelName": "llama3-8b",   "ordinal": 1, "targetBackends": ["gpu-2"] },
        { "id": "output",  "modelName": "llama3-8b",   "ordinal": 2, "targetBackends": ["gpu-3"] }
      ],
      "coordination": { "mode": "sequential", "timeoutMs": 60000 }
    }
  ]
}
```

> **Статус:** каркас реализован (`internal/virtualmodel/`), но pipeline execution не завершён. См. [`audit-2026-06.md` KL-7](audit-2026-06.md#3-известные-ограничения-known-limitations).

### 4.4 RPC Coordinator (Variant B)

Каркас в `internal/rpccoordinator/`. Требует отдельных worker-инстансов с HTTP `/rpc/*` endpoints. **Не в текущем релизе.**

---

## 5. Healthcheck и observability

### 5.1 Liveness vs Readiness

| Endpoint | Назначение | Docker healthcheck |
|---|---|---|
| `GET /api/v1/ping` | liveness (всегда 200 если процесс жив) | да |
| `GET /api/v1/health` | readiness (503 в degraded) | нет (иначе restart loop) |

> **Важно:** в `healthcheck` Docker используйте `/api/v1/ping`, **не** `/api/v1/health`. Последний может вернуть 503 до того, как бэкенды зарегистрировались, что вызовет restart loop.

### 5.2 WebSocket метрики

`/ws/metrics` — real-time трансляция:

```javascript
const ws = new WebSocket("ws://localhost:18081/ws/metrics?token=...");
ws.onmessage = (e) => {
  const metrics = JSON.parse(e.data);
  console.log(metrics);
};
```

### 5.3 Prometheus-стиль метрик

Балансировщик экспортирует JSON-снапшоты через `GET /api/v1/metrics`:

```bash
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/metrics
```

Содержит: `ActiveRequests`, `FreeSlots`, `CalculatedRPS`, `QueueStats`, гистограммы (`modelLoadTime`, `queueWaitTime`).

---

## 6. Таблица портов

| Порт | Компонент | Описание |
|---|---|---|
| **18080** | Load Balancer | Ollama/OpenAI API Proxy (внешний) |
| **18081** | Load Balancer | Management API + WebSocket |
| **18083** | Web UI | Dashboard |
| **18032** | Agent | Локальные метрики агента |
| **11434** | Ollama | Ollama API (на бэкендах) |
| **18091** | CppWorker (внутри) | Ollama-compat + OpenAI API |
| **18092** | CppWorker (хост-port bundled) | маппинг на 18091 внутри контейнера |
| **8443** | Load Balancer | HTTPS Proxy (TLS) |
| **8444** | Load Balancer | HTTPS Management API (TLSPort+1) |

---

## 7. Связанные документы

- [`installation.md`](installation.md) — установка и сборка.
- [`agent-deployment.md`](agent-deployment.md) — развёртывание агента.
- [`backend-type-isolation.md`](backend-type-isolation.md) — варианты развёртывания с разными типами бэкендов.
- [`cppworker-model-params.md`](cppworker-model-params.md) — n_ctx + Per-Model Profiles.
- [`audit-2026-06.md`](audit-2026-06.md) — статус реализации.
- [`../.clinerules`](../.clinerules) §15 — bundled-стек инструкции.