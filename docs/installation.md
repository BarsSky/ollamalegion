# Установка и сборка OllamaLegion

> **Версия:** 2026-06-22  
> **Связанные документы:** [`deployment.md`](deployment.md), [`configuration.md`](configuration.md), [`audit-2026-06.md`](audit-2026-06.md)

## Содержание

1. [Требования к системе](#1-требования-к-системе)
2. [Установка через Docker (рекомендуемый)](#2-установка-через-docker)
3. [Локальная установка (из исходников)](#3-локальная-установка-из-исходников)
4. [Сборка CppWorker](#4-сборка-cppworker)
5. [Режимы работы без агента](#5-режимы-работы-без-агента)
6. [Smoke-проверка](#6-smoke-проверка)

---

## 1. Требования к системе

### Балансировщик (Load Balancer)

| Компонент | Минимум | Рекомендуется |
|---|---|---|
| ОС | Linux / Windows / macOS | Linux (Ubuntu 22.04+, Debian 12+) |
| CPU | 2 cores | 4 cores |
| RAM | 512 MB | 1 GB |
| Disk | 100 MB | 500 MB |
| Go | 1.21+ | 1.21+ |
| Network | 100 Mbps | 1 Gbps |

### Агент (на каждом сервере с Ollama/CppWorker)

| Компонент | Минимум | Рекомендуется |
|---|---|---|
| ОС | Linux (Ubuntu 20.04+, Debian 11+) | Ubuntu 22.04 LTS |
| CPU | 1 core | 2 cores |
| RAM | 128 MB | 256 MB |
| Disk | 50 MB | 100 MB |
| Docker | 20.10+ | 24.0+ |

**GPU-режим** дополнительно требует:

- NVIDIA GPU (Compute Capability 5.0+, рекомендуется 7.0+).
- NVIDIA Driver 470.x+ (рекомендуется 535.x+).
- NVIDIA Container Toolkit 1.12+.
- CUDA 11.0+ (рекомендуется 12.0+).

### WebUI (Nginx + статика)

| Компонент | Минимум |
|---|---|
| CPU | 1 core |
| RAM | 256 MB |
| Disk | 100 MB |

---

## 2. Установка через Docker

Рекомендуемый способ для production.

### 2.1 Клонирование

```bash
git clone https://github.com/BarsSky/ollamalegion.git
cd ollamalegion
```

### 2.2 Подготовка `deployments/.env`

```bash
cp config/config.example.json config/config.json
cp deployments/.env.bundled.example deployments/.env.bundled  # опционально
# отредактируйте под свои параметры:
# - LB_PORT, LB_API_PORT (по умолчанию 18080, 18081)
# - CPPWORKER_API_TOKEN (для auto-registration)
# - BALANCER_URL (для cppworker auto-registration)
```

### 2.3 Запуск bundled-стека

```bash
# Linux/macOS/WSL
./scripts/start-bundled.sh

# Windows PowerShell (Legacy builder обязателен!)
$env:DOCKER_BUILDKIT=0
.\scripts\start-bundled.ps1
```

Стек поднимает:
- `cppworker-gpu` (порт 18092)
- `loadbalancer` (порты 18080, 18081)
- `webui` (порт 18083)

### 2.4 Альтернативные compose

```bash
# Только балансер + WebUI (без CppWorker)
cd deployments && docker compose up -d

# Только CppWorker (CPU)
docker compose -f docker-compose.cppworker.yml --profile cpu up -d --build

# Только CppWorker (GPU)
docker compose -f docker-compose.cppworker.yml --profile gpu up -d --build

# Только CppWorker (stub — для тестов без llama.cpp)
docker compose -f docker-compose.cppworker.yml --profile stub up -d --build

# Агент на отдельном сервере
docker compose -f docker-compose.agent.yml --env-file .env up -d --build
```

### 2.5 Сборка образов вручную

```powershell
$env:DOCKER_BUILDKIT=0  # Legacy builder обязателен на Windows 11 + Docker Desktop

docker build -t ollama-legion/balancer:latest --target production -f docker/balancer/Dockerfile .
docker build -t ollama-legion/cppworker:cpu --target runtime -f docker/cppworker/Dockerfile.cpu .
docker build -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .
docker build -t ollama-legion/cppworker:stub --target runtime -f docker/cppworker/Dockerfile.stub .
docker build -t ollama-legion/agent:cpu --target agent-cpu -f docker/agent/Dockerfile .
docker build -t ollama-legion/agent:gpu --target agent-gpu -f docker/agent/Dockerfile .
docker build -t ollama-legion/webui:latest -f docker/webui/Dockerfile .
```

| Образ | Размер | Когда использовать |
|---|---|---|
| `cppworker:cpu` | ~150 MB | Production CPU-инференс |
| `cppworker:gpu` | ~3.2 GB | Production GPU (CUDA) |
| `cppworker:stub` | ~100 MB | CI/тесты без llama.cpp |
| `balancer` | ~54 MB | Балансировщик |
| `agent:cpu` | ~16 MB | Метрики без GPU |
| `agent:gpu` | ~395 MB | Метрики с NVML |
| `webui` | ~101 MB | Web UI dashboard |

---

## 3. Локальная установка (из исходников)

### 3.1 Сборка балансировщика

```bash
go build -o balancer ./cmd/balancer
./balancer --port 18080 --api-port 18081 --config ./config/config.json
```

### 3.2 Сборка агента

```bash
go build -o agent ./cmd/agent
AGENT_ID=local-1 BALANCER_URL=http://localhost:18081 ./agent
```

### 3.3 Stub-сборка CppWorker (для тестов, без llama.cpp)

```bash
# Linux/macOS
go build -tags llama_stub -o cppworker-stub ./cmd/cppworker
./cppworker-stub --port 18091 --models-dir ./models

# Windows (бинарник отдельно, см. .clinerules §12)
go test -c ./cmd/cppworker -tags llama_stub -o cppworker_test.exe
.\cppworker_test.exe
```

---

## 4. Сборка CppWorker (с реальным llama.cpp)

Только для GPU/CPU-инференса. Stub-сборки достаточно для 90% разработки.

### 4.1 Зависимости

- CMake 3.20+
- C++17 компилятор (gcc 9+, clang 10+, MSVC 19.30+)
- CUDA 12.2+ (только для GPU)
- ~30 минут на полную сборку с llama.cpp

### 4.2 GPU-сборка (CUDA)

```bash
cd docker/cppworker
docker build -f Dockerfile.gpu --target runtime -t ollama-legion/cppworker:gpu ..
```

### 4.3 CPU-сборка (Alpine)

```bash
cd docker/cppworker
docker build -f Dockerfile.cpu --target runtime -t ollama-legion/cppworker:cpu ..
```

### 4.4 Архитектуры GPU

- Аргумент `CUDA_ARCH` управляет тэгом образа `:gpu-<arch>`.
- Поддерживаются: `80` (A100), `86` (RTX 3070/3080/3090), `89` (RTX 4090), `90` (H100), `all` (multi-arch).
- Пример: `CUDA_ARCH=86 ./scripts/build-containers.sh cppworker`.

---

## 5. Режимы работы без агента

Балансировщик **может** работать без агента, но с ограниченной функциональностью.

### 5.1 Что работает без агента

| Функция | Работает? | Комментарий |
|---|---|---|
| HTTP-проксирование (`/api/generate`, `/api/chat`, `/api/embeddings`) | ✅ | Прямой проброс на бэкенд |
| Round-robin балансировка | ✅ | Не требует метрик |
| Health check | ✅ | Через `/api/tags` на Ollama |
| Session stickiness | ✅ | Хранится в памяти балансировщика |
| Queue / backpressure | ✅ | Счётчики `ActiveReqs` |
| Retry / failover | ✅ | На любой healthy бэкенд |
| Least-connections | ✅ | По `ActiveReqs` (без агента) |
| Базовый scoring | ✅ | Упрощённая формула |

### 5.2 Что НЕ работает без агента

| Функция | Требует агента | Причина |
|---|---|---|
| Model Affinity | ✅ | Список `RunningModels` приходит от агента |
| Resource-aware scoring v2 | ✅ | GPU/VRAM/CPU/RAM метрики |
| Prewarm Controller | ✅ | Нужен `freeVRAM` от агента |
| Auto-pull (Pull-on-Demand) | ✅ | Проверка `RunningModels` |
| Headroom Reservation | ✅ | Нужны VRAM usage метрики |
| Predictor | ✅ | История метрик |
| Model Replication (Variant A) | ✅ | Расширенные метрики |
| Virtual Model Router (Variant C) | ✅ | Pipeline slicing метрики |
| Adaptive Weight Tuner | ✅ | История latency/success |
| Unload Scheduler (smart eviction) | ✅ | Метрики |
| Agent actions в WebUI (Restart/Logs/Config) | ✅ | Нет endpoint'ов без агента |

### 5.3 Минимальный конфиг без агента

```json
{
  "balancing": {
    "algorithm": "roundrobin",
    "modelAffinity": false,
    "sessionStickiness": true,
    "useEnhancedScoring": false,
    "prewarm": { "enabled": false },
    "autoPull": { "enabled": false }
  }
}
```

---

## 6. Smoke-проверка

```bash
# 1. Health балансировщика (liveness, всегда 200)
curl http://localhost:18081/api/v1/ping

# 2. Health балансировщика (readiness, 503 в degraded)
curl http://localhost:18081/api/v1/health

# 3. Health CppWorker (через балансер)
curl http://localhost:18092/health

# 4. Список бэкендов
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends

# 5. Прямая генерация через CppWorker
curl -X POST http://localhost:18092/api/generate \
  -H "Content-Type: application/json" \
  -d '{"model":"model.gguf","prompt":"Hello","stream":false}'

# 6. WebUI Dashboard
open http://localhost:18083  # или http://localhost:18030 если без Docker
```

Если все 6 шагов проходят — установка успешна.

---

## 7. Связанные документы

- [`deployment.md`](deployment.md) — Docker Compose развёртывание.
- [`agent-deployment.md`](agent-deployment.md) — развёртывание агента CPU/GPU.
- [`audit-2026-06.md`](audit-2026-06.md) — текущее состояние кода.
- [`../.clinerules`](../.clinerules) §12 — полезные команды (раздел «Локальная разработка»).