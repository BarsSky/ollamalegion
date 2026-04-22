# Ollama Load Balancer

Интеллектуальный балансировщик нагрузки для кластера Ollama с мониторингом ресурсов GPU/CPU/RAM/Disk и умным распределением запросов.

## 📚 Документация

| Документ | Описание |
|----------|----------|
| [README.md](README.md) | Общая информация о проекте |
| [**DEPLOYMENT.md**](DEPLOYMENT.md) | **Руководство по развертыванию агента на GPU-серверах** |
| [scripts/build-agent.sh](scripts/build-agent.sh) | Скрипт сборки агента |
| [scripts/deploy-agent.sh](scripts/deploy-agent.sh) | Скрипт автоматического развертывания агента |

## Особенности

- 🚀 **Умная балансировка** - распределение на основе доступных ресурсов GPU, CPU, RAM
- 🎯 **Model Affinity** - направление запросов к серверам с уже загруженной моделью
- 📊 **Real-time мониторинг** - метрики в реальном времени через Web UI
- 🔍 **Health Check** - автоматическое обнаружение нерабочих бэкендов
- 💾 **Session Stickiness** - сохранение сессии на одном сервере
- 🐳 **Docker Ready** - готовые Dockerfile и docker-compose конфигурации

## Архитектура

```
┌─────────────┐     ┌──────────────────┐     ┌─────────────┐
│   Clients   │────▶│   Load Balancer  │────▶│   Agent 1   │───▶ Ollama + GPU 1
│  (API/Web)  │     │   (Go + Proxy)   │     │             │
└─────────────┘     └──────────────────┘     └─────────────┘
                           │                      │
                           │                      │
                           ▼                      ▼
                    ┌──────────────────┐     ┌─────────────┐
                    │     Web UI       │     │   Agent 2   │───▶ Ollama + GPU 2
                    │   (Dashboard)    │     │             │
                    └──────────────────┘     └─────────────┘
```

## Быстрый старт

### 1. Сборка компонентов

**Балансировщик:**
```bash
cd ollama-loadbalancer

# Использование скрипта сборки (рекомендуется)
./scripts/build-balancer.sh

# Или вручную
go build -o balancer ./cmd/balancer
```

**Агент:**
```bash
# Использование скрипта сборки (рекомендуется)
./scripts/build-agent.sh

# Или вручную
go build -o agent ./cmd/agent
```

### 2. Запуск балансировщика через Docker Compose

```bash
cd deployments

# Запуск балансировщика и Web UI
docker-compose up -d

# Просмотр логов
docker-compose logs -f loadbalancer

# Остановка
docker-compose down
```

### 3. Развертывание агента на GPU-серверах

**Вариант A: Использование скрипта автоматического развертывания (рекомендуется)**

```bash
# На GPU-сервере
./scripts/deploy-agent.sh gpu-1 http://<balancer-host>:8081
```

**Вариант B: Ручной запуск через Docker**

```bash
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://<balancer-host>:8081 \
  -v /usr/bin/nvidia-smi:/usr/bin/nvidia-smi:ro \
  -v /proc:/host/proc:ro \
  --network host \
  --gpus all \
  ollama-lb/agent:latest
```

**Вариант C: Запуск как бинарный файл**

См. подробные инструкции в [DEPLOYMENT.md](DEPLOYMENT.md)

## Порты

| Порт | Описание |
|------|----------|
| 18080 | Ollama API Proxy (внешний) |
| 18081 | Management API |
| 18030 | Web UI Dashboard |
| 8080  | Ollama API Proxy (внутренний) |
| 8081  | Management API (внутренний) |

## API Endpoints

### Health Check
```bash
curl http://localhost:18081/api/v1/health
```

### Cluster State
```bash
curl http://localhost:18081/api/v1/cluster
```

### Metrics
```bash
curl http://localhost:18081/api/v1/metrics
```

### Backends
```bash
curl http://localhost:18081/api/v1/backends
```

## Web UI

Dashboard доступен по адресу: http://localhost:18030

## Примеры использования

### Python клиент

```python
import requests

# Настройка прокси на балансировщик
session = requests.Session()
session.proxies = {
    'http': 'http://localhost:18080',
    'https': 'http://localhost:18080'
}

# Запрос к Ollama через балансировщик
response = session.post(
    'http://localhost:18080/api/generate',
    json={
        'model': 'llama3.1:70b',
        'prompt': 'Hello!',
        'stream': False
    }
)

print(response.json())
```

### cURL

```bash
curl -X POST http://localhost:18080/api/generate \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.1:70b",
    "prompt": "Hello!",
    "stream": false
  }'
```

### 3. Настройка агентов на GPU серверах

На каждом GPU сервере с Ollama:

```bash
# Запуск агента
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://<balancer-host>:8081 \
  -v /usr/bin/nvidia-smi:/usr/bin/nvidia-smi:ro \
  -v /proc:/host/proc:ro \
  --network host \
  ollama-lb/agent:latest
```

## Конфигурация

### Переменные окружения балансировщика

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `LB_HOST` | 0.0.0.0 | Хост для прослушивания |
| `LB_PORT` | 8080 | Порт прокси для Ollama API |
| `LB_API_PORT` | 8081 | Порт Management API |
| `LB_ALGORITHM` | resource-aware | Алгоритм балансировки |
| `LB_MODEL_AFFINITY` | true | Привязка к модели |
| `LB_SESSION_STICKINESS` | true | Привязка сессии |
| `LB_HEALTH_CHECK_INTERVAL` | 10 | Интервал health check (сек) |
| `LB_METRICS_INTERVAL` | 5 | Интервал метрик (сек) |
| `LB_GPU_MAX_USAGE` | 90 | Макс. загрузка GPU (%) |
| `LB_GPU_MAX_VRAM` | 85 | Макс. использование VRAM (%) |
| `LB_CPU_MAX_USAGE` | 80 | Макс. загрузка CPU (%) |
| `LB_MEMORY_MAX_USAGE` | 85 | Макс. использование RAM (%) |

### Настройка бэкендов

```bash
# Backend 1
BACKEND_0_ID=gpu-1
BACKEND_0_NAME=GPU Server 1
BACKEND_0_HOST=192.168.13.66
BACKEND_0_PORT=11434
BACKEND_0_WEIGHT=1

# Backend 2
BACKEND_1_ID=gpu-2
BACKEND_1_NAME=GPU Server 2
BACKEND_1_HOST=192.168.13.70
BACKEND_1_PORT=11434
BACKEND_1_WEIGHT=1
```

## API Reference

### Health Check

```bash
GET /api/v1/health
```

### Список бэкендов

```bash
GET /api/v1/backends
```

### Метрики кластера

```bash
GET /api/v1/metrics
GET /api/v1/metrics/:id
```

### Состояние кластера

```bash
GET /api/v1/cluster
```

Response:
```json
{
  "timestamp": "2024-01-15T10:30:00Z",
  "totalBackends": 2,
  "healthyBackends": 2,
  "activeRequests": 5,
  "backends": [
    {
      "id": "gpu-1",
      "gpu": {
        "usagePercent": 45.5,
        "memoryTotal": 24576,
        "memoryUsed": 12000,
        "temperature": 65
      },
      "system": {
        "cpuUsagePercent": 30.2,
        "memoryTotal": 65536,
        "memoryUsed": 20000
      },
      "ollama": {
        "runningModels": [
          {"name": "llama3.1:70b", "vramUsage": 18000}
        ]
      }
    }
  ]
}
```

### Запущенные модели

```bash
GET /api/v1/models
```

## Алгоритмы балансировки

### Resource-Aware (по умолчанию)

Распределение на основе доступных ресурсов:
- GPU загрузка < 90%
- VRAM использование < 85%
- CPU загрузка < 80%
- RAM использование < 85%
- Свободно на диске > 10GB

### Model Affinity

Если модель уже загружена на каком-то сервере, запрос направляется туда.

### Session Stickiness

Клиент с той же сессией (IP или X-Session-ID header) попадает на тот же сервер.

## Web UI

Dashboard доступен по адресу: http://localhost:3000

Features:
- Общая картина кластера
- Графики нагрузки GPU/CPU/RAM/VRAM
- Список запущенных моделей
- Real-time обновления (5 сек)

## Примеры использования

### Python клиент

```python
import requests

# Настройка прокси на балансировщик
session = requests.Session()
session.proxies = {
    'http': 'http://localhost:8080',
    'https': 'http://localhost:8080'
}

# Запрос к Ollama через балансировщик
response = session.post(
    'http://localhost:8080/api/generate',
    json={
        'model': 'llama3.1:70b',
        'prompt': 'Hello!',
        'stream': False
    }
)

print(response.json())
```

### cURL

```bash
# Запрос через балансировщик
curl -X POST http://localhost:8080/api/generate \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.1:70b",
    "prompt": "Hello!",
    "stream": false
  }'
```

## Структура проекта

```
ollama-loadbalancer/
├── cmd/
│   ├── balancer/          # Бинарник балансировщика
│   └── agent/             # Бинарник агента
├── internal/
│   ├── balancer/          # Логика балансировки
│   ├── agent/             # Логика агента
│   ├── api/               # REST API handlers
│   └── config/            # Конфигурация
├── pkg/
│   ├── types/             # Типы данных
│   └── protocol/          # Протокол обмена
├── webui/
│   └── dist/              # Web UI файлы
├── docker/
│   ├── balancer/          # Dockerfile балансировщика
│   ├── agent/             # Dockerfile агента
│   └── webui/             # Dockerfile UI
├── deployments/
│   └── docker-compose.yml # Docker Compose
├── config/
│   └── config.example.json # Пример конфигурации
└── README.md
```

## Требования

### Балансировщик
- Go 1.21+
- Docker (опционально)
- 512 MB RAM
- 2 CPU cores

### Агент (на каждом GPU сервере)
- Go 1.21+ (для сборки)
- Docker 20.10+ с NVIDIA Container Toolkit (рекомендуется)
- nvidia-smi (для GPU метрик)
- 128 MB RAM
- Доступ к Ollama API на порту 11434

### Web UI
- Docker (опционально)
- 256 MB RAM

## Troubleshooting

### Агент не подключается к балансировщику

Проверьте:
1. Доступность балансировщика из сети агента
2. Правильность BALANCER_URL
3. Firewall правила

```bash
# Проверка подключения
curl http://<balancer-host>:8081/api/v1/health
```

### Бэкенд помечается как unhealthy

Проверьте:
1. Доступность Ollama API на бэкенде
2. Правильность HOST и PORT
3. Логи агента

```bash
# Проверка Ollama API
curl http://<ollama-host>:11434/api/tags
```

### Высокая задержка запросов

Проверьте:
1. Сетевую задержку между компонентами
2. Загрузку GPU серверов
3. Настройки лимитов ресурсов

### Ошибки доступа к GPU

Убедитесь, что NVIDIA Container Toolkit установлен и настроен:

```bash
# Проверка
docker run --rm --gpus all nvidia/cuda:11.0-base nvidia-smi

# Установка (Ubuntu/Debian)
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit.gpg
distribution=$(. /etc/os-release;echo $ID$VERSION_ID)
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
  sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit.gpg] https://#g' | \
  sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
sudo apt-get update
sudo apt-get install -y nvidia-container-toolkit
sudo systemctl restart docker
```

## Дополнительные ресурсы

- 📖 [DEPLOYMENT.md](DEPLOYMENT.md) - Полное руководство по развертыванию агента
- 🔧 [scripts/build-agent.sh](scripts/build-agent.sh) - Скрипт сборки агента
- 🚀 [scripts/deploy-agent.sh](scripts/deploy-agent.sh) - Скрипт автоматического развертывания

## Лицензия

MIT License
