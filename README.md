# Ollama Load Balancer

Интеллектуальный балансировщик нагрузки для кластера Ollama с мониторингом ресурсов GPU/CPU/RAM/Disk и умным распределением запросов.

## 🚀 Быстрый старт

### Запуск через Docker Compose

```bash
# Клонирование репозитория
git clone https://github.com/your-org/ollama-loadbalancer.git
cd ollama-loadbalancer/deployments

# Запуск балансировщика и Web UI
docker-compose up -d

# Проверка статуса
docker-compose ps

# Просмотр логов
docker-compose logs -f loadbalancer
```

### Развертывание агента на GPU сервере

```bash
# На каждом GPU сервере с Ollama
cd scripts
./deploy-agent-docker.sh \
  --balancer-url http://<balancer-ip>:18081 \
  --agent-id gpu-1
```

### Проверка

```bash
# Health check
curl http://localhost:18081/api/v1/health

# Web UI
# Откройте http://localhost:18030 в браузере
```

---

## 📚 Документация

| Документ | Описание |
|----------|----------|
| [docs/README.md](docs/README.md) | 📖 Полная документация |
| [docs/installation.md](docs/installation.md) | 🔧 Установка и сборка |
| [docs/configuration.md](docs/configuration.md) | ⚙️ Конфигурация системы |
| [docs/deployment.md](docs/deployment.md) | 🚀 Развертывание |
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
│   └── agent/             # Агент
├── internal/
│   ├── balancer/          # Логика балансировки
│   ├── agent/             # Сбор метрик
│   ├── api/               # REST API
│   └── config/            # Конфигурация
├── docker/
│   ├── balancer/          # Dockerfile балансировщика
│   ├── agent/             # Dockerfile агента
│   └── webui/             # Dockerfile UI
├── deployments/
│   └── docker-compose.yml # Docker Compose
├── config/
│   └── config.example.json
└── docs/                  # Документация
```

---

## 🔧 Требования

### Балансировщик
- Go 1.21+ (для сборки)
- Docker (опционально)
- 512 MB RAM, 2 CPU cores

### Агент (на каждом GPU сервере)
- Docker 20.10+ с NVIDIA Container Toolkit
- nvidia-smi (для GPU метрик)
- Ollama на порту 11434
- 128 MB RAM

---

## 📖 Дополнительная документация

- 📖 [Полная документация](docs/README.md)
- 🔧 [Установка и сборка](docs/installation.md)
- ⚙️ [Конфигурация](docs/configuration.md)
- 🚀 [Развертывание](docs/deployment.md)
- 📡 [API документация](docs/api.md)
- 🔧 [Troubleshooting](docs/troubleshooting.md)
- 📋 [OpenAPI спецификация](docs/openapi.yaml)

---

## Лицензия

MIT License
