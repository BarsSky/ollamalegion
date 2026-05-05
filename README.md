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
│   ├── balancer/          # Балансировщик
│   ├── agent/             # Агент
│   └── monitor/           # Простой монитор (отдельный бинарник)
├── internal/
│   ├── balancer/          # Логика балансировки (proxy, health, sessions, predictor)
│   ├── agent/             # Сбор метрик GPU/CPU/RAM
│   ├── api/               # REST API + WebSocket
│   └── config/            # Конфигурация
├── pkg/
│   ├── logger/            # Библиотека логирования
│   ├── protocol/          # Протокол коммуникации агент↔балансер
│   └── types/             # Типы данных
├── docker/
│   ├── balancer/          # Dockerfile балансировщика
│   ├── agent/             # Dockerfile агента
│   ├── cocoindex/         # CocoIndex embeddings
│   └── webui/             # Dockerfile Web UI
├── webui/                 # Web UI (dashboard, monitor, nginx)
├── deployments/           # Docker Compose конфигурации
├── tests/                 # Интеграционные и сценарные тесты
├── config/                # Примеры конфигураций
├── logo/                  # Логотип (png, svg)
├── scripts/               # Скрипты сборки и развёртывания
└── docs/                  # Документация (RU, EN, планы)
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
