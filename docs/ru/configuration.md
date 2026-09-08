# Конфигурация Ollama Load Balancer

Полное руководство по настройке всех компонентов системы.

## Содержание

1. [Конфигурация балансировщика](#конфигурация-балансировщика)
2. [Конфигурация агента](#конфигурация-агента)
3. [Конфигурация Web UI](#конфигурация-web-ui)
4. [TLS/SSL настройка](#tlsssl-настройка)
5. [Аутентификация и Rate Limiting](#аутентификация-и-rate-limiting)
6. [Режимы работы (Operating Modes)](#режимы-работы-operating-modes)
7. [Model Replication (Вариант A)](#model-replication-вариант-a)
8. [RPC Coordinator (Вариант B)](#rpc-coordinator-вариант-b)
9. [Virtual Model Router (Вариант C)](#virtual-model-router-вариант-c)

---

## Конфигурация балансировщика

Балансировщик может быть настроен двумя способами:
- Через JSON файл конфигурации
- Через переменные окружения

### JSON конфигурация

Пример файла конфигурации [`config.example.json`](../config/config.example.json):

```json
{
  "loadBalancer": {
    "host": "0.0.0.0",
    "port": 18080,
    "apiPort": 18081,
    "tlsHost": "",
    "tlsPort": 8443
  },
  "backends": [
    {
      "id": "gpu-1",
      "name": "GPU Server 1",
      "host": "192.0.2.66",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 1,
      "maxConcurrentRequests": 10,
      "labels": ["nvidia", "rtx4090"]
    },
    {
      "id": "gpu-2",
      "name": "GPU Server 2",
      "host": "192.0.2.70",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 1,
      "maxConcurrentRequests": 10,
      "labels": ["nvidia", "rtx4090"]
    }
  ],
  "balancing": {
    "algorithm": "resource-aware",
    "modelAffinity": true,
    "sessionStickiness": true,
    "healthCheckInterval": 10,
    "metricsInterval": 5,
    "requestTimeout": 120,
    "queueTimeout": 300,
    "queueMaxSize": 100
  },
  "resources": {
    "gpu": {
      "maxUsagePercent": 90,
      "maxVramUsagePercent": 85,
      "maxTemperature": 85
    },
    "cpu": {
      "maxUsagePercent": 80
    },
    "memory": {
      "maxUsagePercent": 85
    },
    "disk": {
      "minFreeMB": 10240
    }
  },
  "logging": {
    "level": "info",
    "format": "json"
  },
  "api": {
    "rateLimit": 100,
    "rateBurst": 200
  },
  "tls": {
    "enabled": false,
    "certFile": "certs/server.crt",
    "keyFile": "certs/server.key",
    "minVersion": "TLS12",
    "autoCert": true
  },
  "auth": {
    "enabled": false,
    "tokens": [
      "your-master-token-here-change-in-production"
    ],
    "headerName": "X-API-Token"
  }
}
```

### Параметры конфигурации балансировщика

#### LoadBalancer settings

| Параметр | Тип | По умолчанию | Описание |
|----------|-----|--------------|----------|
| `host` | string | `0.0.0.0` | Хост для прослушивания HTTP запросов |
| `port` | int | `18080` | Порт для проксирования Ollama API |
| `apiPort` | int | `18081` | Порт Management API |
| `tlsHost` | string | `""` | Хост для HTTPS (опционально) |
| `tlsPort` | int | `8443` | Порт для HTTPS прокси |
| `tlsApiPort` | int | `tlsPort+1` | Порт для HTTPS Management API (вычисляется как `tlsPort + 1`) |

#### Backend settings

| Параметр | Тип | По умолчанию | Описание |
|----------|-----|--------------|----------|
| `id` | string | - | **Обязательный**. Уникальный идентификатор бэкенда |
| `name` | string | - | Человекочитаемое имя бэкенда |
| `host` | string | - | **Обязательный**. IP или hostname GPU сервера |
| `ollamaPort` | int | `11434` | Порт Ollama API на бэкенде |
| `agentPort` | int | `18032` | Порт агента для метрик |
| `weight` | int | `1` | Вес бэкенда для балансировки |
| `maxConcurrentRequests` | int | `10` | Макс. количество одновременных запросов |
| `labels` | array | `[]` | Метки для классификации бэкендов |

#### Balancing settings

| Параметр | Тип | По умолчанию | Описание |
|----------|-----|--------------|----------|
| `algorithm` | string | `resource-aware` | Алгоритм балансировки |
| `modelAffinity` | bool | `true` | Включить привязку к модели |
| `sessionStickiness` | bool | `true` | Включить привязку сессии |
| `useEnhancedScoring` | bool | `true` | Использовать расширенное скорингование с весами |
| `modelLoadTimeout` | int | `120` | Таймаут загрузки модели на бэкенд (сек) |
| `healthCheckInterval` | int | `10` | Интервал проверки здоровья (сек) |
| `metricsInterval` | int | `5` | Интервал обновления метрик (сек) |
| `requestTimeout` | int | `120` | Таймаут запроса (сек) |
| `queueTimeout` | int | `300` | Таймаут ожидания в очереди (сек) |
| `queueMaxSize` | int | `100` | Макс. размер очереди |

#### Resource limits

| Параметр | Тип | По умолчанию | Описание |
|----------|-----|--------------|----------|
| `gpu.maxUsagePercent` | float | `90` | Макс. загрузка GPU (%) |
| `gpu.maxVramUsagePercent` | float | `85` | Макс. использование VRAM (%) |
| `gpu.maxTemperature` | int | `85` | Макс. температура GPU (°C) |
| `cpu.maxUsagePercent` | float | `80` | Макс. загрузка CPU (%) |
| `memory.maxUsagePercent` | float | `85` | Макс. использование RAM (%) |
| `disk.minFreeMB` | uint | `10240` | Мин. свободно на диске (MB) |

#### Logging settings

| Параметр | Тип | По умолчанию | Описание |
|----------|-----|--------------|----------|
| `level` | string | `info` | Уровень логирования (debug, info, warn, error) |
| `format` | string | `json` | Формат логов (json, text) |

### Переменные окружения балансировщика

Альтернатива JSON конфигурации — использование переменных окружения:

```bash
# Основные настройки
LB_HOST=0.0.0.0
LB_PORT=18080
LB_API_PORT=18081
LB_TLS_HOST=
LB_TLS_PORT=8443

# Алгоритм балансировки
LB_ALGORITHM=resource-aware
LB_MODEL_AFFINITY=true
LB_SESSION_STICKINESS=true

# Интервалы (секунды)
LB_HEALTH_CHECK_INTERVAL=10
LB_METRICS_INTERVAL=5

# Таймауты (секунды)
LB_REQUEST_TIMEOUT=120
LB_QUEUE_TIMEOUT=300
LB_QUEUE_MAX_SIZE=100
LB_QUEUE_WORKERS=4

# Лимиты ресурсов
LB_GPU_MAX_USAGE=90
LB_GPU_MAX_VRAM=85
LB_GPU_MAX_TEMP=85
LB_CPU_MAX_USAGE=80
LB_MEMORY_MAX_USAGE=85
LB_DISK_MIN_FREE_MB=10240

# Логирование
LB_LOG_LEVEL=info
LB_LOG_FORMAT=json

# API Rate Limiting
API_RATE_LIMIT=100
API_RATE_BURST=200
```

### Настройка бэкендов через переменные окружения

```bash
# Backend 1
BACKEND_0_ID=gpu-1
BACKEND_0_NAME=GPU Server 1
BACKEND_0_HOST=192.0.2.66
BACKEND_0_PORT=11434
BACKEND_0_AGENT_PORT=18032
BACKEND_0_WEIGHT=1
BACKEND_0_MAX_REQS=10

# Backend 2
BACKEND_1_ID=gpu-2
BACKEND_1_NAME=GPU Server 2
BACKEND_1_HOST=192.0.2.70
BACKEND_1_PORT=11434
BACKEND_1_AGENT_PORT=18032
BACKEND_1_WEIGHT=1
BACKEND_1_MAX_REQS=10
```

---

## Конфигурация агента

Агент настраивается исключительно через переменные окружения.

### Обязательные переменные

| Переменная | Описание | Пример |
|------------|----------|--------|
| `AGENT_ID` | Уникальный идентификатор агента | `gpu-1`, `server-a100` |
| `BALANCER_URL` | URL балансировщика | `http://192.0.2.100:18081` |

### Опциональные переменные

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `AGENT_PORT` | `18032` | Порт для локальных метрик агента |
| `OLLAMA_URL` | `http://localhost:11434` | URL локального Ollama |
| `NVML_ENABLED` | `true` | Включить NVML поддержку (требуется для GPU-метрик) |
| `COLLECT_INTERVAL` | `5` | Интервал сбора метрик (секунд). Также принимается `METRICS_INTERVAL` (устар., для совместимости) |
| `HEARTBEAT_INTERVAL` | `3` | Интервал heartbeat сигналов (секунд) |
| `LOG_LEVEL` | `info` | Уровень логирования (debug, info, warn, error) |
| `LOG_FORMAT` | `text` | Формат логов (text, json) |
| `CONFIG_PATH` | - | Путь к файлу конфигурации |
| `CONNECTION_TIMEOUT` | `30` | Таймаут подключения к балансировщику (сек) |
| `OLLAMA_TIMEOUT` | `120` | Таймаут запроса к Ollama API (сек) |

### Пример .env файла для агента

```bash
# ============================================
# Ollama Load Balancer - Agent Configuration
# ============================================

# Обязательные параметры
AGENT_ID=gpu-1
BALANCER_URL=http://192.0.2.100:18081

# Настройки портов
AGENT_PORT=18032
OLLAMA_URL=http://localhost:11434

# Настройки NVML
NVML_ENABLED=true

# Интервалы
COLLECT_INTERVAL=5
HEARTBEAT_INTERVAL=3

# Логирование
LOG_LEVEL=info
LOG_FORMAT=text
```

### Запуск агента с .env файлом

```bash
# Docker Compose
docker-compose -f docker-compose.agent.yml --env-file .env up -d

# Docker run
docker run --env-file .env ollama-legion/agent:latest

# Бинарный файл
set -a; source .env; set +a; ./bin/agent
```

---

## Конфигурация Web UI

Web UI использует nginx для раздачи статических файлов и проксирования API запросов.

### Конфигурация nginx

Файл [`docker/webui/nginx.conf`](../docker/webui/nginx.conf):

```nginx
server {
    listen 80;
    server_name localhost;
    root /usr/share/nginx/html;
    index index.html;

    # Static files
    location / {
        try_files $uri $uri/ /index.html;
    }

    # API proxy to balancer
    location /api/ {
        proxy_pass http://loadbalancer:18081;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection 'upgrade';
        proxy_set_header Host $host;
        proxy_cache_bypass $http_upgrade;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }

    # WebSocket proxy to balancer
    location /ws/ {
        proxy_pass http://loadbalancer:18081;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "Upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

### Переменные окружения Web UI

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `WEBUI_PORT` | `18030` | Порт для Web UI |

---

## TLS/SSL настройка

Балансировщик поддерживает HTTPS для безопасного соединения с клиентами.

### Включение TLS

#### Через JSON конфигурацию

```json
{
  "tls": {
    "enabled": true,
    "certFile": "certs/server.crt",
    "keyFile": "certs/server.key",
    "minVersion": "TLS12",
    "autoCert": true
  }
}
```

#### Через переменные окружения

```bash
TLS_ENABLED=true
TLS_CERT_FILE=certs/server.crt
TLS_KEY_FILE=certs/server.key
TLS_MIN_VERSION=TLS12
TLS_AUTO_CERT=true
LB_TLS_PORT=8443
```

### Параметры TLS

| Параметр | Тип | По умолчанию | Описание |
|----------|-----|--------------|----------|
| `enabled` | bool | `false` | Включение TLS/SSL |
| `certFile` | string | `certs/server.crt` | Путь к SSL сертификату |
| `keyFile` | string | `certs/server.key` | Путь к SSL ключу |
| `minVersion` | string | `TLS12` | Минимальная версия TLS (TLS12, TLS13) |
| `autoCert` | bool | `false` | Автоматическая генерация self-signed сертификата |

При включённом TLS прокси доступен на `tlsPort` (8443), а HTTPS Management API — на `tlsPort+1` (8444).

### Генерация самоподписанного сертификата

```bash
# Создание директории для сертификатов
mkdir -p certs

# Генерация приватного ключа и сертификата
openssl req -x509 -nodes -days 365 -newkey rsa:4096 \
  -keyout certs/server.key \
  -out certs/server.crt \
  -subj "/C=US/ST=State/L=City/O=Organization/CN=localhost"

# Генерация с SAN (Subject Alternative Names)
openssl req -x509 -nodes -days 365 -newkey rsa:4096 \
  -keyout certs/server.key \
  -out certs/server.crt \
  -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,DNS:ollama-legion,IP:127.0.0.1"
```

### Использование с Docker Compose

```yaml
services:
  loadbalancer:
    image: ollama-legion/balancer:latest
    ports:
      - "18080:18080"
      - "8443:8443"
    volumes:
      - ./certs:/app/certs:ro
      - ./config.json:/app/config.json:ro
    environment:
      - TLS_ENABLED=true
      - TLS_CERT_FILE=certs/server.crt
      - TLS_KEY_FILE=certs/server.key
      - TLS_AUTO_CERT=false
```

### Подключение через HTTPS

```bash
# cURL с самоподписанным сертификатом
curl -k https://localhost:8443/api/v1/health

# cURL с доверенным сертификатом
curl --cacert certs/server.crt https://localhost:8443/api/v1/health

# Python requests
import requests
response = requests.get('https://localhost:8443/api/v1/health', verify=False)
```

---

## Аутентификация и Rate Limiting

### Аутентификация

Балансировщик поддерживает **токен-аутентификацию** с возможностью полного отключения. Это удобно для разработки, внутренних сетей и Docker-окружений.

#### Принципы работы

| Режим | `auth.enabled` | Поведение | Когда использовать |
|-------|----------------|-----------|-------------------|
| **Отключена** | `false` | Все endpoint'ы публичные. WebSocket работает без токена. | Разработка, внутренняя сеть, Docker Compose |
| **Включена** | `true` | Защищённые endpoint'ы требуют токен. WebSocket — через `?token=`. | Production, публичный доступ |

> **💡 Рекомендация:** Для production всегда включайте аутентификацию. Для локальной разработки или внутренних сетей можно отключить.

#### Включение/отключение аутентификации

**JSON конфигурация (`config.json`):**

```json
{
  "auth": {
    "enabled": false,
    "tokens": [],
    "headerName": "X-API-Token"
  }
}
```

**Для production:**

```json
{
  "auth": {
    "enabled": true,
    "tokens": [
      "secure-master-token-at-least-32-chars-long",
      "client-api-token-for-dashboard"
    ],
    "headerName": "X-API-Token"
  }
}
```

**Переменные окружения:**

```bash
# Отключить auth (по умолчанию)
AUTH_ENABLED=false

# Включить auth
AUTH_ENABLED=true
AUTH_TOKENS=master-token,client-token-1,client-token-2
AUTH_HEADER_NAME=X-API-Token
```

#### Параметры аутентификации

| Параметр | Тип | По умолчанию | Описание |
|----------|-----|--------------|----------|
| `enabled` | bool | `false` | Включение/отключение аутентификации |
| `tokens` | array | `[]` | Список валидных API токенов |
| `headerName` | string | `X-API-Token` | Имя HTTP-заголовка для токена |

#### Master Token

Первый токен в списке считается **master token**. Master token обладает расширенными правами:

- ✅ Создавать новые токены через `POST /api/v1/auth/token`
- ✅ Отзывать другие токены через `DELETE /api/v1/auth/token`
- ❌ Нельзя отозвать самого себя через API

> **🔒 Безопасность:** Храните master token в защищённом хранилище. Не коммитьте его в репозиторий.

#### WebSocket и аутентификация

Браузер **не поддерживает** установку произвольных HTTP-заголовков при создании WebSocket. Поэтому токен передаётся через **query parameter**:

```javascript
// Правильно — токен в URL
const ws = new WebSocket('ws://localhost:18081/ws/metrics?token=your-api-token');

// Неправильно — заголовки игнорируются
const ws = new WebSocket('ws://...', null, { headers: { 'X-API-Token': '...' } }); // ❌
```

На стороне сервера:
- Если `auth.enabled: false` → WebSocket upgrade выполняется **без проверки токена**
- Если `auth.enabled: true` → требуется `?token=...`; пустой токен → HTTP 401

#### Как WebUI определяет настройки auth

WebUI использует **публичный** endpoint `GET /api/v1/health` для автоопределения:

```json
{
  "status": "healthy",
  "authEnabled": true,
  "authHeader": "X-API-Token",
  "wsEndpoint": "/ws/metrics"
}
```

Flow WebUI:
1. Загрузка страницы → `GET /api/v1/health` (без токена)
2. Если `authEnabled: false` → приложение стартует сразу
3. Если `authEnabled: true` → проверяется сохранённый токен в `localStorage`
4. Если токен валиден → стартует; если нет — показывается модальное окно ввода

#### Публичные endpoint'ы (не требуют токена)

| Endpoint | Описание |
|----------|----------|
| `GET /api/v1/health` | Проверка здоровья + метаданные auth |
| `GET /api/v1/ratelimit/status` | Статус rate limiter |

Все остальные endpoint'ы требуют токен при `auth.enabled: true`. Полная матрица endpoint'ов — в [API документации](api.md#матрица-endpointов-и-аутентификации).

#### Управление токенами через API

```bash
# Проверка статуса аутентификации (любой валидный токен)
curl -H "X-API-Token: your-token" \
  http://localhost:18081/api/v1/auth/status

# Генерация нового токена (только master token)
curl -X POST -H "X-API-Token: your-master-token" \
  http://localhost:18081/api/v1/auth/token

# Отзыв токена (только master token)
curl -X DELETE \
  -H "X-API-Token: your-master-token" \
  "http://localhost:18081/api/v1/auth/token?token=token-to-revoke"
```

#### Генерация токена вручную

Токен можно сгенерировать через API или с помощью утилиты:

```bash
# Через API (требует master token)
curl -X POST -H "X-API-Token: your-master-token" \
  http://localhost:18081/api/v1/auth/token

# Через openssl (локально)
openssl rand -hex 32
# Результат: a1b2c3d4e5f6... (64 hex символа)
```

> **📏 Требования к токену:** Минимум 16 символов. Рекомендуется 64 hex символа (32 байта энтропии).

### Rate Limiting

Rate limiting защищает API от злоупотреблений.

#### Конфигурация Rate Limiting

**JSON конфигурация:**

```json
{
  "api": {
    "rateLimit": 100,
    "rateBurst": 200
  }
}
```

**Переменные окружения:**

```bash
API_RATE_LIMIT=100
API_RATE_BURST=200
```

#### Параметры Rate Limiting

| Параметр | Тип | По умолчанию | Описание |
|----------|-----|--------------|----------|
| `rateLimit` | float | `100` | Запросов в секунду |
| `rateBurst` | float | `200` | Burst capacity (макс. токенов) |

#### Проверка статуса Rate Limiter

```bash
GET /api/v1/ratelimit/status

# Ответ:
{
  "tokens": 100.0,
  "max_tokens": 100,
  "refill_rate": 10.0
}
```

---

## Полная пример конфигурации для production

```json
{
  "loadBalancer": {
    "host": "0.0.0.0",
    "port": 18080,
    "apiPort": 18081,
    "tlsPort": 8443
  },
  "backends": [
    {
      "id": "prod-gpu-1",
      "name": "Production GPU Server 1",
      "host": "192.0.2.10",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 2,
      "maxConcurrentRequests": 20,
      "labels": ["nvidia", "a100", "production"]
    },
    {
      "id": "prod-gpu-2",
      "name": "Production GPU Server 2",
      "host": "192.0.2.11",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 2,
      "maxConcurrentRequests": 20,
      "labels": ["nvidia", "a100", "production"]
    }
  ],
  "balancing": {
    "algorithm": "resource-aware",
    "modelAffinity": true,
    "sessionStickiness": true,
    "healthCheckInterval": 10,
    "metricsInterval": 5,
    "requestTimeout": 180,
    "queueTimeout": 600,
    "queueMaxSize": 200
  },
  "resources": {
    "gpu": {
      "maxUsagePercent": 85,
      "maxVramUsagePercent": 80,
      "maxTemperature": 80
    },
    "cpu": {
      "maxUsagePercent": 75
    },
    "memory": {
      "maxUsagePercent": 80
    },
    "disk": {
      "minFreeMB": 20480
    }
  },
  "logging": {
    "level": "info",
    "format": "json"
  },
  "api": {
    "rateLimit": 200,
    "rateBurst": 400
  },
  "tls": {
    "enabled": true,
    "certFile": "/etc/ssl/certs/ollama-legion.crt",
    "keyFile": "/etc/ssl/private/ollama-legion.key",
    "minVersion": "TLS13",
    "autoCert": false
  },
  "auth": {
    "enabled": true,
    "tokens": [
      "secure-master-token-change-in-production",
      "client-api-token-1",
      "client-api-token-2"
    ],
    "headerName": "X-API-Token"
  }
}
```

---

## Режимы работы (Operating Modes)

Балансировщик поддерживает несколько режимов работы, переключаемых через `PUT /api/v1/config/mode` или через WebUI Setup Wizard.

### Параметры конфигурации

| Поле | Тип | По умолчанию | Описание |
|------|-----|-------------|----------|
| `balancing.operating_mode` | string | `"auto"` | Режим работы: `auto`, `single_node`, `cluster_balancing` |
| `balancing.auto_detect_mode` | bool | `true` | Автоопределение режима при старте |

### Режимы

| Режим | Описание |
|-------|----------|
| **`auto` (по умолчанию)** | Автоматически определяет режим: если ≥2 healthy бэкендов — `cluster_balancing`, иначе `single_node` |
| **`single_node`** | Прямое проксирование с отключённой балансировкой |
| **`cluster_balancing`** | Полноценная балансировка с выбором бэкенда, очередью и session stickiness |

### Environment override

```env
LB_OPERATING_MODE=cluster_balancing
LB_AUTO_DETECT_MODE=true
```

### Сброс конфигурации к заводским настройкам

`POST /api/v1/config/reset` сбрасывает конфигурацию к значениям по умолчанию.

---

## Model Replication (Вариант A)

Автоматическая репликация моделей на несколько бэкендов.

| Поле | Тип | По умолчанию | Env-переменная | Описание |
|------|-----|-------------|----------------|----------|
| `balancing.model_replication.enabled` | bool | `false` | `LB_MODEL_REPLICATION_ENABLED` | Включение репликации |
| `balancing.model_replication.default_min_instances` | int | `1` | `LB_MODEL_REPLICATION_MIN_INSTANCES` | Мин. реплик |
| `balancing.model_replication.default_max_instances` | int | `3` | `LB_MODEL_REPLICATION_MAX_INSTANCES` | Макс. реплик |
| `balancing.model_replication.idle_unload_after` | duration | `"15m"` | `LB_MODEL_REPLICATION_IDLE_UNLOAD` | Время простоя до выгрузки |

---

## RPC Coordinator (Вариант B)

Распределённый inference через внешних RPC-воркеров.

| Поле | Тип | По умолчанию | Env-переменная | Описание |
|------|-----|-------------|----------------|----------|
| `balancing.rpc_coordinator.enabled` | bool | `false` | `LB_RPC_COORDINATOR_ENABLED` | Включение RPC-координации |
| `balancing.rpc_coordinator.coordinator_url` | string | `""` | `LB_RPC_COORDINATOR_URL` | URL координатора |
| `balancing.rpc_coordinator.worker_port` | int | `18050` | `LB_RPC_COORDINATOR_PORT` | Порт RPC-воркера |
| `balancing.rpc_coordinator.protocol` | string | `"http"` | `LB_RPC_COORDINATOR_PROTOCOL` | Протокол |

---

## Virtual Model Router (Вариант C)

Виртуальные модели как pipeline из нескольких физических моделей.

| Поле | Тип | По умолчанию | Env-переменная | Описание |
|------|-----|-------------|----------------|----------|
| `balancing.virtual_models.enabled` | bool | `false` | `LB_VIRTUAL_MODELS_ENABLED` | Включение виртуальных моделей |

---

## Следующие шаги

- [Развертывание](deployment.md) — Docker Compose и production deployment
- [API документация](api.md) — Использование REST API и WebSocket
- [Troubleshooting](troubleshooting.md) — Решение проблем

---

## Round 56-59 (2026-09-03): Новые ENV и endpoints

### `LB_STREAMING_NEVER_TIMEOUT` (R58.1)

Отключает ВСЕ streaming-таймауты (idle, first-byte, request, total).
Balancer НЕ обрывает streaming-соединения по таймауту — ждёт cancel
от клиента (r.Context().Done()) или EOF от backend. Полезно для
long-running inference (Qwen3.6-35B prefill 10+ мин) или когда клиент
сам управляет cancel через ctx.

```bash
# Включить
LB_STREAMING_NEVER_TIMEOUT=1
# Поддерживает: "1", "true", "yes" (case-insensitive)
# "0" / unset = defaults (120s idle, 600s total, etc.)
```

Default значения сохраняются (для существующих деплоев — bit-identical).
Opt-in через single ENV toggle.

### `LB_NCTX_RELOAD_*` (Round 34-37, 2026-08-12)

Adaptive auto-reload `n_ctx` при `prompt_too_long` (code 3) /
`n_ctx_needed` (code 2) от cppworker.

| ENV | Default | Назначение |
|---|---|---|
| `LB_NCTX_RELOAD_ENABLED` | `true` | Включить adaptive reload |
| `LB_NCTX_RELOAD_MAX_N_CTX` | `131072` | Max n_ctx для reload (128K) |
| `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR` | `0.95` | Safety margin VRAM (0.85 = rejected Gemma-4, 0.95 = OK) |
| `LB_NCTX_RELOAD_TIMEOUT_SEC` | `300` | Таймаут операции reload |

См. `deployments/.env.bundled-with-agent.example:44-50`.

### `LB_NCTX_PREFLIGHT_*` (Round 35, 2026-08-12)

Preflight check `n_ctx` ПЕРЕД синхронной загрузкой модели. Без
`LB_NCTX_PREFLIGHT_ENABLED=true` preflight short-circuits → Cline 65K →
413 sync вместо 503+Retry-After (бесконечный retry loop).

| ENV | Default | Назначение |
|---|---|---|
| `LB_NCTX_PREFLIGHT_ENABLED` | `true` | Master switch |
| `LB_NCTX_PREFLIGHT_ASYNC_RELOAD` | `true` | Async вместо blocking reload |
| `LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC` | `30` | `Retry-After` header в 503 |
| `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` | `900` (15 min) | Max polling time для загрузки. **22GB Qwen3.6 на 3070** требует `1800` (30 min) — см. Round 35c |
| `LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER` | `2` | Множитель оценки cppworker |
| `LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC` | `60` | Дополнительный buffer |

См. `deployments/.env.bundled-with-agent.example:54-74`.

### `CPPWORKER_RAM_FALLBACK_*` (Round 38, 2026-08-17)

3-tier cascade для моделей, не влезающих в VRAM: full VRAM → partial
offload → CPU-only. При `8GB VRAM` (RTX 3070) most large models не
влезают — partial offload или CPU-only обязателен.

| ENV | Default | Назначение |
|---|---|---|
| `CPPWORKER_RAM_FALLBACK_N_CTX` | `true` | Включить cascade |
| `CPPWORKER_RAM_FALLBACK_GPU_LAYERS` | `-2` (auto offload) | Слои при RAM fallback |
| `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` | `64000` (8GB) / `128000` (16-24GB) | Max n_ctx в CPU-only mode |

См. `config/hardware-presets/*.json` для preset'ов.

### `Backend.ApiStyle` (R56, EffectiveAPIStyle)

Новое поле `Backend.ApiStyle` (`ollama-native` / `openai-compatible`).
Раньше routing использовал `Backend.Type` напрямую, что блокировало
операторам override API-стиля (например, cppworker с нативным Ollama API
всё равно шёл через OpenAI-compat path).

EffectiveAPIStyle (R56) — helper:
- Если `ApiStyle` явно задан → используется
- Иначе выводится из `Type` (llama_cpp → openai-compatible, иначе ollama-native)

Routing использует EffectiveAPIStyle() вместо isLlamaCppBackend().
Backward compat: для существующих деплоев без ApiStyle = bit-identical.

### Cluster AutoDistribute (R59, R59.1)

Новые endpoints для автоматического распределения моделей между бэкендами:

```bash
# R59: получить suggestions (read-only)
GET /api/v1/admin/cluster/autosuggest
→ 200 OK {
    "timestamp": "...",
    "backends": [...],
    "suggestions": [
      {"id": "sug-1", "type": "move", "fromBackend": "b1",
       "toBackend": "b2", "model": "gemma-4-9b", "priority": 1, ...}
    ],
    "summary": {"totalBackends": 3, "overloadedCount": 1, ...}
  }

# R59.1: apply suggestions (operator-confirmed)
POST /api/v1/admin/cluster/autosuggest/apply
Body: {"suggestion_ids": ["sug-1", "sug-3"]}
→ 200 OK {
    "applied": 1,
    "failed": 1,
    "details": [
      {"suggestionId": "sug-1", "status": "applied", ...},
      {"suggestionId": "sug-99", "status": "failed", "error": "unknown suggestion id"}
    ]
  }
```

Алгоритм:
- Идентифицирует overloaded backends (busyScore > 0.8 OR modelCount > 0.9)
- Находит underloaded backends (busyScore < 0.3 AND modelCount < 0.5)
- Предлагает move loaded models из overloaded в underloaded того же Type
- Cross-type moves запрещены (ollama ↔ llama_cpp ломает load format)
- Max 3 suggestions per source (не flood operator)

Validation (R59.1):
- Re-computes suggestions from current state (не доверяет client)
- Verifies suggestion_id exists
- Verifies source/dest healthy
- Verifies cross-type
- Verifies model loaded on source

### Hardware Presets (R58.2)

`config/hardware-presets/{rtx30-8gb,rtx40-24gb,a10-24gb,rtx50-32gb}.json` —
4 готовых preset'а для разных GPU. Применение:

```bash
# Просмотр preset'ов
python scripts/apply-hardware-preset.py --list

# Применить (выводит KEY=VALUE для copy-paste)
python scripts/apply-hardware-preset.py rtx40-24gb

# Setup Wizard UI (R58.3): Settings → Setup Wizard → step 4
# dropdown "Hardware Preset" → выбор RTX 4090 / A10 / RTX 5090
# → автозаполнение vramMaxUsage/gpuMaxUsage + hint с CPPWORKER_* env
```

Подробнее: [docs/ru/hardware-presets.md](hardware-presets.md).

### Parallel LlamaCollector (R58)

`internal/agent/llama_collector.go::Collect()` — 3 sequential HTTP calls
к cppworker (/api/gpu, /api/models, /api/info) переделаны в parallel
через `sync.WaitGroup`. Ускорение ~3x (603ms → 210ms на типичных
endpoints). Закрывает user pain: «агент ждёт main thread cppworker».

### Setup Wizard Hardware Preset Dropdown (R58.3)

WebUI: Settings → Setup Wizard → Step 4 (General Settings) →
новый dropdown "Hardware Preset". При выборе preset'а (RTX 4090 / A10 /
RTX 5090) автозаполняются 4 поля (vramMaxUsage / gpuMaxUsage / cpuMaxUsage
/ ramMaxUsage) и показывается hint с CPPWORKER_* env vars.
