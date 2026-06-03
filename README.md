# Ollama Load Balancer

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE.md)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg)](https://www.docker.com/)

Интеллектуальный балансировщик нагрузки для кластера Ollama с мониторингом ресурсов GPU/CPU/RAM/Disk и умным распределением запросов.

> 📚 **Документация:** [Русский](docs/README.md) | [English](docs/en/README.md)  
> 🌐 **WebUI:** тёмная/светлая тема • русский/English

## 🚀 Быстрый старт

### Запуск через Docker Compose

```bash
# Клонирование репозитория
git clone https://github.com/BarsSky/ollamalegion.git
cd ollamalegion/deployments

# Статические файлы WebUI должны быть в webui/dist/
# Убедитесь, что index.html и связанные файлы находятся там

# Запуск балансировщика и Web UI
docker-compose up -d

# Проверка статуса
docker-compose ps

# Просмотр логов
docker-compose logs -f loadbalancer
```

### Сборка Docker образов

> 💡 **На Windows обязателен Legacy builder:** `$env:DOCKER_BUILDKIT=0`  
> BuildKit может обрывать контекст сборки (`context canceled`).  
> 📘 **Подробная инструкция:** [Сборка Docker-образов](docs/ru/docker-compose-guide.md#1-сборка-docker-образов) (на русском)  
> 🔧 **Анализ проблем:** [Анализ сборок Docker](docs/docker-build-analysis.md)

```powershell
# Отключаем BuildKit (обязательно на Windows 11 + Docker Desktop)
$env:DOCKER_BUILDKIT=0

# CppWorker CPU (реальный llama.cpp, без CUDA — для продакшена и разработки)
docker build -t ollama-legion/cppworker:cpu --target runtime -f docker/cppworker/Dockerfile.cpu .

# CppWorker GPU (CUDA 12.2, реальный llama.cpp — 3.2 GB, для production инференса)
docker build -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .

# CppWorker STUB (заглушка, без llama.cpp — только для CI/тестов)
docker build -t ollama-legion/cppworker:stub --target runtime -f docker/cppworker/Dockerfile.stub .

# Balancer (основной сервис — 54 MB)
docker build -t ollama-legion/balancer:latest --target production -f docker/balancer/Dockerfile .

# Agent CPU (15.9 MB)
docker build -t ollama-legion/agent:cpu --target agent-cpu -f docker/agent/Dockerfile .

# Agent GPU (395 MB, с NVML)
docker build -t ollama-legion/agent:gpu --target agent-gpu -f docker/agent/Dockerfile .

# WebUI (Nginx + статика — 101 MB)
docker build -t ollama-legion/webui:latest -f docker/webui/Dockerfile .
```

| Образ | CPU/GPU | Размер | Время сборки | Когда использовать |
|-------|---------|--------|-------------|--------------------|
| `cppworker:cpu` | CPU (реальный llama.cpp) | ~150 MB | ~10 мин | Production и разработка CPU-инференса |
| `cppworker:gpu` | GPU (CUDA) | 3.2 GB | ~17 мин | Production инференс GGUF с CUDA |
| `cppworker:stub` | CPU (заглушка) | ~100 MB | ~4 мин | CI/тесты, без реального llama.cpp |
| `balancer:latest` | — | 54 MB | ~4 мин | Основной сервис балансировки |

### Запуск CppWorker (llama.cpp инференс)

CppWorker — сервис инференса GGUF-моделей через llama.cpp. Может работать в двух режимах: CPU (без GPU) и GPU (с NVIDIA CUDA).

#### CPU-режим (Alpine, легковесный)

```powershell
# Базовый запуск с томом для моделей
docker run -d --name cppworker-cpu `
  -p 18091:18091 `
  -v ${PWD}\models:/app/models `
  ollama-legion/cppworker:cpu `
  --port 18091 --models-dir ./models
```

```bash
# Linux/macOS
docker run -d --name cppworker-cpu \
  -p 18091:18091 \  -v $(pwd)/models:/app/models \
  ollama-legion/cppworker:cpu \
  --port 18091 --models-dir ./models
```


#### GPU-режим (CUDA 12.2, реальный llama.cpp)

**Требования:** NVIDIA Driver 470+, [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)

```powershell
# Запуск с GPU-ускорением
docker run -d --name cppworker-gpu `
  --gpus all `
  -p 18091:18091 `
  -v ${PWD}\models:/app/models `
  ollama-legion/cppworker:gpu `
  --port 18091 --models-dir ./models --gpu-layers -1 --flash-attn
```

```bash
# Linux/macOS
docker run -d --name cppworker-gpu \
  --gpus all \
  -p 18091:18091 \
  -v $(pwd)/models:/app/models \
  ollama-legion/cppworker:gpu \
  --port 18091 --models-dir ./models --gpu-layers -1 --flash-attn
```

#### Флаги командной строки

| Флаг | По умолчанию | Описание |
|------|-------------|----------|
| `--port` | `18091` | HTTP API порт |
| `--models-dir` | `./models` | Директория с GGUF моделями |
| `--gpu-layers` | `-1` | Количество слоёв на GPU (`-1` = все, только GPU-режим) |
| `--flash-attn` | `false` | Flash Attention (только GPU-режим) |
| `--context-length` | `2048` | Размер контекста |
| `--threads` | `12` | Количество потоков CPU |
| `--batch-size` | `512` | Размер батча |

#### Проверка работоспособности

```bash
# Health check
curl http://localhost:18091/health
# → {"status":"ok"}

# Список доступных моделей
curl http://localhost:18091/api/tags

# Тестовый инференс
curl -X POST http://localhost:18091/api/chat \
  -H "Content-Type: application/json" \
  -d '{"model":"test-model","messages":[{"role":"user","content":"Hello!"}],"stream":false}'
```

#### Устранение неполадок GPU

```bash
# Проверка видимости GPU из контейнера
docker run --rm --gpus all nvidia/cuda:12.2.0-runtime-ubuntu22.04 nvidia-smi

# Проверка логов cppworker
docker logs cppworker-gpu

# Если GPU не виден — установить NVIDIA Container Toolkit
# https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html
```

### Развертывание агента

Агент запускается **на каждом сервере с Ollama** и собирает метрики для балансировщика. Поддерживаются режимы **GPU** (с NVIDIA GPU и NVML) и **CPU** (без GPU).

#### GPU-развертывание (с NVIDIA GPU)

Требуется: NVIDIA Driver 470+, NVIDIA Container Toolkit, Docker 20.10+.

```bash
cd deployments

# Создайте .env
cat > .env << 'EOF'
AGENT_ID=gpu-1
BALANCER_URL=http://<balancer-ip>:18081
AGENT_PUBLIC_HOST=<agent-public-ip>
GPU_MODE=gpu
NVML_ENABLED=true
AGENT_PORT=18032
OLLAMA_URL=http://host.docker.internal:11434
METRICS_INTERVAL=5
HEARTBEAT_INTERVAL=3
EOF

# Раскомментируйте секцию deploy.resources.reservations.devices в docker-compose.agent.yml
# Запуск
docker-compose -f docker-compose.agent.yml --env-file .env up -d
```

#### CPU-развертывание (без GPU)

Требуется: Docker 20.10+, Ollama на порту 11434. GPU не нужен.

```bash
cd deployments

# Создайте .env
cat > .env << 'EOF'
AGENT_ID=cpu-1
BALANCER_URL=http://<balancer-ip>:18081
AGENT_PUBLIC_HOST=<agent-public-ip>
GPU_MODE=cpu
NVML_ENABLED=false
AGENT_PORT=18032
OLLAMA_URL=http://host.docker.internal:11434
METRICS_INTERVAL=5
HEARTBEAT_INTERVAL=3
EOF

# Запуск (секция GPU в docker-compose.agent.yml должна быть закомментирована)
docker-compose -f docker-compose.agent.yml --env-file .env up -d
```

> 📖 **Подробное руководство** по развертыванию агента в обоих режимах, требованиям к системе и troubleshooting — в файле [`docs/agent-deployment.md`](docs/agent-deployment.md).

### Проверка

```bash
# Health check
curl http://localhost:18081/api/v1/health

# Web UI Dashboard
# Откройте http://localhost:18030 в браузере

# Монитор (real-time визуализация кластера)
# Откройте http://localhost:18030/monitor.html в браузере
```

---

## 📚 Документация

| Документ | Описание |
|----------|----------|
| [docs/README.md](docs/README.md) | 📖 Полная документация |
| [docs/installation.md](docs/installation.md) | 🔧 Установка и сборка |
| [docs/configuration.md](docs/configuration.md) | ⚙️ Конфигурация системы |
| [docs/deployment.md](docs/deployment.md) | 🚀 Развертывание всех компонентов |
| [docs/agent-deployment.md](docs/agent-deployment.md) | 🤖 Развертывание агента (CPU/GPU) |
| [docs/api.md](docs/api.md) | 📡 API документация |
| [docs/rpc-coordinator.md](docs/rpc-coordinator.md) | 🌐 RPC Model Distribution |
| [docs/troubleshooting.md](docs/troubleshooting.md) | 🔧 Решение проблем |
| [docs/openapi.yaml](docs/openapi.yaml) | 📋 OpenAPI спецификация |

---

## ⚙️ Базовая конфигурация

### Переменные окружения балансировщика

```bash
# Основные настройки
LB_PORT=18080          # Порт Ollama API proxy
LB_API_PORT=18081      # Порт Management API
LB_ALGORITHM=resource-aware

# Очередь запросов
LB_QUEUE_TIMEOUT=300    # Таймаут очереди (сек)
LB_QUEUE_MAX_SIZE=100   # Максимальный размер очереди
LB_QUEUE_WORKERS=4      # Количество workers очереди

# Бэкенды
BACKEND_0_ID=gpu-1
BACKEND_0_HOST=192.168.13.66
BACKEND_0_PORT=11434
BACKEND_0_AGENT_PORT=18032
```

### Переменные окружения агента

```bash
AGENT_ID=gpu-1
BALANCER_URL=http://<balancer-ip>:18081
NVML_ENABLED=true
```

---

## 📊 Таблица портов

| Порт | Компонент | Описание |
|------|-----------|----------|
| **18080** | Load Balancer | Ollama API Proxy (внешний) |
| **18081** | Load Balancer | Management API + WebSocket |
| **8443** | Load Balancer | HTTPS Proxy (TLS) |
| **8444** | Load Balancer | HTTPS Management API (TLSPort+1) |
| **18030** | Web UI | Dashboard |
| **18032** | Agent | Локальные метрики агента |
| **11434** | Ollama | Ollama API (на бэкендах) |

---

## ✨ Особенности

- 🚀 **Умная балансировка** — распределение на основе доступных ресурсов GPU, CPU, RAM
- 🎯 **Model Affinity** — направление запросов к серверам с уже загруженной моделью
- 📊 **Real-time мониторинг** — метрики в реальном времени через WebSocket
- 🔍 **Health Check** — автоматическое обнаружение нерабочих бэкендов
- 💾 **Session Stickiness** — сохранение сессии на одном сервере
- 🔄 **Queue Manager** — обработка перегрузок с очередью запросов
- 🐳 **Docker Ready** — готовые Dockerfile и docker-compose конфигурации
- 🎮 **NVML Support** — точные метрики GPU через NVIDIA Management Library

---

## 🏗️ Архитектура

```
┌─────────────┐     ┌──────────────────────────────────────┐     ┌─────────────┐
│   Clients   │────▶│         Load Balancer                │────▶│   Agent 1   │───▶ Ollama + GPU 1
│  (API/Web)  │     │  ┌────────────────────────────────┐  │     │             │
└─────────────┘     │  │  Reverse Proxy + Queue Manager │  │     └──────┬──────┘
                    │  └────────────────────────────────┘  │            │
                    │  ┌────────────────────────────────┐  │            │
                    │  │  WebSocket Server (Metrics)    │◀─┼────────────┘
                    │  └────────────────────────────────┘  │
                    │  ┌────────────────────────────────┐  │
                    │  │  Ollama API Integration        │  │
                    └──────────────────────────────────────┘
                               │
                               ▼
                    ┌──────────────────┐
                    │     Web UI       │
                    │   (Dashboard)    │
                    └──────────────────┘
```

---

## 📋 Примеры использования

### cURL

```bash
# Запрос к Ollama через балансировщик
curl -X POST http://localhost:18080/api/generate \
  -H "Content-Type: application/json" \
  -d '{"model": "llama3.1:8b", "prompt": "Hello!", "stream": false}'
```

### Python

```python
import requests

# Настройка прокси на балансировщик
session = requests.Session()
session.proxies = {
    'http': 'http://localhost:18080',
    'https': 'http://localhost:18080'
}

# Запрос к Ollama
response = session.post(
    'http://localhost:18080/api/generate',
    json={
        'model': 'llama3.1:8b',
        'prompt': 'Hello!',
        'stream': False
    }
)

print(response.json())
```

---

## 🔒 Аутентификация и TLS

### Включение аутентификации

```bash
AUTH_ENABLED=true
AUTH_TOKENS=your-master-token,client-token
AUTH_HEADER_NAME=X-API-Token
```

### Включение TLS

```bash
TLS_ENABLED=true
TLS_CERT_FILE=certs/server.crt
TLS_KEY_FILE=certs/server.key
TLS_AUTO_CERT=true
```

См. [Конфигурация](docs/configuration.md#аутентификация-и-rate-limiting) для подробной настройки.

---

## 📦 Структура проекта

```
ollama-loadbalancer/
├── cmd/
│   ├── balancer/          # Балансировщик (main.go)
│   ├── agent/             # Агент (main.go)
│   └── monitor/           # Простой монитор (отдельный бинарник)
├── internal/
│   ├── balancer/          # Логика балансировки
│   │   ├── proxy.go           # HTTP оркестратор (ServeHTTP)
│   │   ├── backend_selector.go # 4-этапный выбор бэкенда (+ pre-step Model Replication)
│   │   ├── backend_registry.go # CRUD бэкендов
│   │   ├── backend_state.go    # Состояние бэкенда
│   │   ├── candidate.go        # Candidate groups (P1-P4)
│   │   ├── cluster_state.go    # Состояние кластера + метрики
│   │   ├── eventbus.go         # Pub/sub событий
│   │   ├── health.go           # Health checker
│   │   ├── metrics.go          # Balancer metrics
│   │   ├── model_instance_controller.go # Контроллер экземпляров
│   │   ├── model_management.go # Управление моделями (pull/load/unload)
│   │   ├── prewarm_controller.go # Превентивная загрузка
│   │   ├── predictor.go        # Прогнозирование загрузки
│   │   ├── proxy_request.go    # HTTP проксирование
│   │   ├── queue_dispatch.go   # Dispatch очереди с 4 приоритетами
│   │   ├── queue_manager.go    # Очередь + workers
│   │   ├── router.go           # HTTP routing
│   │   ├── rpc_modules.go      # RPC/Virtual/Replication модули
│   │   ├── scoring.go          # Мультифакторный scoring
│   │   ├── session_handler.go  # Session stickiness + rebalance
│   │   ├── session_manager.go  # Session manager (TTL + cleanup)
│   │   ├── slot_handler.go     # Slot acquisition + retry
│   │   ├── slot_manager.go     # Slot management
│   │   ├── streaming.go        # SSE streaming + heartbeat
│   │   ├── unload_scheduler.go # LRU выгрузка моделей
│   │   └── weight_tuner.go     # Адаптивный тюнер весов
│   ├── agent/             # Сбор метрик GPU/CPU/RAM/Disk/Network
│   ├── api/               # REST API + WebSocket
│   ├── config/            # Конфигурация
│   ├── modelreplication/  # Репликация моделей (ModelGroupManager)
│   └── virtualmodel/      # Виртуальные модели (pipeline)
├── pkg/
│   ├── logger/            # Библиотека логирования (zap)
│   ├── protocol/          # Протокол коммуникации агент↔балансер
│   └── types/             # Типы данных (BackendMetrics, Config и т.д.)
├── docker/
│   ├── balancer/          # Dockerfile балансировщика
│   ├── agent/             # Dockerfile агента
│   ├── cocoindex/         # CocoIndex embeddings
│   └── webui/             # Dockerfile Web UI
├── webui/                 # Web UI (dashboard, monitor, nginx, js, css)
├── deployments/           # Docker Compose конфигурации
├── tests/                 # Интеграционные и сценарные тесты
├── config/                # Примеры конфигураций
├── logo/                  # Логотип (png, svg)
├── plans/                 # Архитектурные планы и дорожные карты
├── scripts/               # Скрипты сборки и развёртывания
└── docs/                  # Документация
    ├── en/                # Английская документация
    └── ru/                # Русская документация
```

---

## 🔧 Требования

### Балансировщик
- Go 1.21+ (для сборки)
- Docker (опционально)
- 512 MB RAM, 2 CPU cores

### Агент

| Режим | Требования |
|-------|------------|
| **GPU** | Docker 20.10+, NVIDIA GPU, NVIDIA Driver 470+, NVIDIA Container Toolkit, Ollama на порту 11434, 128 MB RAM |
| **CPU** | Docker 20.10+, Ollama на порту 11434, 128 MB RAM (NVIDIA не требуется) |

> 📖 Полная таблица требований и рекомендуемые версии — в [`docs/agent-deployment.md`](docs/agent-deployment.md#требования-к-системе).

---

## 📖 Дополнительная документация

- 📖 [Полная документация](docs/README.md)
- 🔧 [Установка и сборка](docs/installation.md)
- ⚙️ [Конфигурация](docs/configuration.md)
- 🚀 [Развертывание всех компонентов](docs/deployment.md)
- 🤖 [Развертывание агента (CPU/GPU)](docs/agent-deployment.md)
- 📡 [API документация](docs/api.md)
- 🔧 [Troubleshooting](docs/troubleshooting.md)
- 📋 [OpenAPI спецификация](docs/openapi.yaml)

---

## Лицензия

MIT License
