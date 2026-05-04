# API Документация Ollama Load Balancer

Полная документация по REST API и WebSocket endpoints.

## Содержание

1. [Обзор API](#обзор-api)
2. [Аутентификация](#аутентификация)
3. [REST API Endpoints](#rest-api-endpoints)
4. [WebSocket API](#websocket-api)
5. [Примеры запросов](#примеры-запросов)
6. [OpenAPI спецификация](#openapi-спецификация)

---

## Обзор API

Балансировщик предоставляет REST API для управления кластером и получения метрик.

### Базовый URL

```
# Management API (управление кластером, метрики, сессии)
http://localhost:18081

# HTTPS (если включен TLS)
https://localhost:8443
```

### Проксирование Ollama API

Балансировщик проксирует стандартные Ollama API endpoint'ы на порту `18080`. Все запросы к Ollama проходят через балансировщик с session stickiness, model affinity, queue management и retry/failover.

**Базовый URL:**
```
http://localhost:18080
```

| Endpoint | Метод | Описание | Особенности проксирования |
|----------|-------|----------|--------------------------|
| `/api/generate` | `POST` | Генерация текста | Session stickiness, streaming/SSE, retry до 3 попыток |
| `/api/chat` | `POST` | Чат | Session stickiness, streaming/SSE, retry до 3 попыток |
| `/api/embed` | `POST` | Эмбеддинги | Session stickiness, retry до 3 попыток |
| `/api/embeddings` | `POST` | Эмбеддинги (legacy) | Session stickiness, retry до 3 попыток |
| `/api/tags` | `GET` | Список моделей | **Агрегация** со всех бэкендов, дедупликация |
| `/api/version` | `GET` | Версия Ollama | Возвращает версию балансировщика |
| `/api/ps` | `GET` | Статус загруженных моделей | **Агрегация** со всех бэкендов |
| `/api/show` | `POST` | Информация о модели | Роутинг на бэкенд с загруженной моделью |
| `/api/create` | `POST` | Создание модели | Роутинг на бэкенд с макс. свободными ресурсами |
| `/api/pull` | `POST` | Загрузка модели | Роутинг на бэкенд с макс. свободными ресурсами |
| `/api/delete` | `DELETE` | Удаление модели | **Broadcast** на все бэкенды с моделью |
| `/api/copy` | `POST` | Копирование модели | Роутинг на бэкенд с исходной моделью |
| `/api/push` | `POST` | Публикация модели | Роутинг на бэкенд с моделью |

**Заголовки для управления сессиями:**
- `X-Client-ID` — приоритетный стабильный идентификатор клиента (рекомендуется)
- `X-Session-ID` — альтернативный идентификатор сессии
- `X-Client-Name` — имя клиента (Cline, OpenWebUI, etc.) для мониторинга

> **Примечание:** `X-Client-ID` без эфемерного порта гарантирует стабильность сессии при NAT. Если заголовки не переданы, используется IP без порта.

### Формат данных

- **Request**: JSON
- **Response**: JSON
- **Content-Type**: `application/json`

### Коды ответов

| Код | Описание |
|-----|----------|
| `200` | Успешный запрос |
| `201` | Ресурс создан |
| `400` | Неверный запрос |
| `401` | Неавторизовано |
| `403` | Доступ запрещен |
| `404` | Ресурс не найден |
| `409` | Конфликт (ресурс уже существует) |
| `500` | Внутренняя ошибка сервера |
| `503` | Сервис недоступен |

---

## Аутентификация

API использует токен-аутентификацию на основе заголовка `X-API-Token`. Аутентификацию можно **включить или отключить** в конфигурации балансера (`config.json` → `auth.enabled`).

| Состояние auth | Поведение |
|----------------|-----------|
| `enabled: false` | Все endpoint'ы публичные, токен не требуется |
| `enabled: true` | Защищённые endpoint'ы требуют валидный токен в заголовке |

### Включение/отключение аутентификации

В `config.json`:

```json
{
  "auth": {
    "enabled": true,
    "tokens": ["your-master-token"],
    "headerName": "X-API-Token"
  }
}
```

- `enabled: false` — публичный доступ ко всем endpoint'ам (удобно для разработки и внутренних сетей)
- `tokens` — список валидных токенов; **первый токен** = master (используется для генерации новых токенов)
- `headerName` — имя HTTP-заголовка (по умолчанию `X-API-Token`)

### Матрица endpoint'ов и аутентификации

| Endpoint | Метод | Auth требуется | Примечание |
|----------|-------|----------------|------------|
| `GET /api/v1/health` | — | ❌ Нет | Публичный, используется WebUI для определения настроек |
| `GET /api/v1/ratelimit/status` | — | ❌ Нет | Публичный |
| `GET /api/v1/auth/status` | — | ✅ Да | Требует токен |
| `POST /api/v1/auth/token` | — | ✅ Master only | Генерация нового токена |
| `GET /api/v1/cluster` | — | ✅ Да | |
| `GET /api/v1/backends` | — | ✅ Да | |
| `POST /api/v1/backends` | — | ✅ Да | |
| `GET /api/v1/metrics` | — | ✅ Да | |
| `GET /api/v1/models` | — | ✅ Да | |
| `GET /api/v1/sessions` | — | ✅ Да | |
| `GET /api/v1/agents/stats` | — | ✅ Да | |
| `GET /api/v1/queue/stats` | — | ✅ Да | Статистика очереди запросов |
| `WebSocket /ws/metrics` | — | ✅ Если auth enabled | Токен передаётся через query parameter |

### Передача токена в HTTP

```bash
# cURL
curl -X GET http://localhost:18081/api/v1/cluster \
  -H "X-API-Token: your-api-token"

# Python requests
import requests
headers = {"X-API-Token": "your-api-token"}
response = requests.get("http://localhost:18081/api/v1/cluster", headers=headers)

# JavaScript fetch
fetch('http://localhost:18081/api/v1/cluster', {
  headers: {'X-API-Token': 'your-api-token'}
})
```

### Передача токена в WebSocket

Браузер **не поддерживает** установку произвольных HTTP-заголовков при создании WebSocket. Поэтому токен передаётся через **query parameter**:

```javascript
// Правильный способ
const ws = new WebSocket('ws://localhost:18081/ws/metrics?token=your-api-token');

// Неправильный — заголовки игнорируются браузером
const ws = new WebSocket('ws://...'); // new WebSocket(url, protocols) не поддерживает headers
```

На стороне сервера (`wsMetricsHandler`):
- Если `auth.enabled: false` → WebSocket upgrade выполняется без проверки токена
- Если `auth.enabled: true` → требуется непустой `?token=...`; токен валидируется через `Authenticate()`

---

## REST API Endpoints

### Health

#### GET /api/v1/health

Проверка здоровья API. **Не требует аутентификации** — используется WebUI и внешними health check'ами для определения состояния системы.

В ответе передаются метаданные, необходимые клиенту для корректного подключения:

| Поле | Тип | Описание |
|------|-----|----------|
| `status` | string | Состояние API: `"healthy"` |
| `timestamp` | string | ISO 8601 время на сервере |
| `version` | string | Версия API |
| `authEnabled` | boolean | `true` — аутентификация включена, `false` — отключена |
| `authHeader` | string | Имя заголовка для токена (по умолчанию `"X-API-Token"`) |
| `wsEndpoint` | string | Путь WebSocket endpoint'а (по умолчанию `"/ws/metrics"`) |

**Пример ответа при auth отключена:**

```json
{
  "status": "healthy",
  "timestamp": "2024-01-15T10:30:00Z",
  "version": "1.0.0",
  "authEnabled": false,
  "authHeader": "X-API-Token",
  "wsEndpoint": "/ws/metrics"
}
```

**Пример ответа при auth включена:**

```json
{
  "status": "healthy",
  "timestamp": "2024-01-15T10:30:00Z",
  "version": "1.0.0",
  "authEnabled": true,
  "authHeader": "X-API-Token",
  "wsEndpoint": "/ws/metrics"
}
```

> **💡 Принцип работы WebUI:** При загрузке dashboard сначала вызывается `GET /api/v1/health` **без токена**. Если `authEnabled: false` — приложение стартует сразу. Если `authEnabled: true` — показывается модальное окно для ввода токена.

---

### Cluster

#### GET /api/v1/cluster

Получение информации о состоянии кластера.

**Ответ:**

```json
{
  "timestamp": "2024-01-15T10:30:00Z",
  "totalBackends": 2,
  "healthyBackends": 2,
  "totalRequests": 1500,
  "activeRequests": 5,
  "queuedRequests": 0,
  "backends": [
    {
      "id": "gpu-1",
      "timestamp": "2024-01-15T10:30:00Z",
      "status": "healthy",
      "gpu": {
        "usagePercent": 45.5,
        "memoryTotal": 24576,
        "memoryUsed": 12000,
        "temperature": 65,
        "powerUsage": 250,
        "powerLimit": 450
      },
      "system": {
        "cpuUsagePercent": 30.2,
        "memoryTotal": 65536,
        "memoryUsed": 20000
      },
      "ollama": {
        "runningModels": [
          {"name": "llama3.1:70b", "vramUsage": 18000}
        ],
        "activeRequests": 3,
        "requestsPerSecond": 12.5
      }
    }
  ]
}
```

---

### Metrics

#### GET /api/v1/metrics

Получение метрик всех бэкендов кластера.

**Ответ:**

```json
{
  "timestamp": "2024-01-15T10:30:00Z",
  "totalBackends": 2,
  "healthyBackends": 2,
  "backends": [
    {
      "id": "gpu-1",
      "timestamp": "2024-01-15T10:30:00Z",
      "gpu": {...},
      "system": {...},
      "ollama": {...}
    }
  ]
}
```

#### GET /api/v1/metrics/{backend_id}

Получение метрик конкретного бэкенда.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `backend_id` | path | ID бэкенда |

**Ответ:**

```json
{
  "id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": {
    "usagePercent": 45.5,
    "memoryTotal": 24576,
    "memoryUsed": 12000,
    "memoryFree": 12576,
    "temperature": 65,
    "powerUsage": 250,
    "powerLimit": 450,
    "gpuClock": 1800,
    "memClock": 10000
  },
  "system": {
    "cpuUsagePercent": 30.2,
    "memoryTotal": 65536,
    "memoryUsed": 20000,
    "memoryFree": 45536,
    "diskTotal": 1000000,
    "diskUsed": 500000,
    "diskFree": 500000,
    "networkRX": 1048576,
    "networkTX": 524288
  },
  "ollama": {
    "runningModels": [
      {
        "name": "llama3.1:70b",
        "size": 70000000000,
        "vramUsage": 18000,
        "expiresAt": "2024-01-15T11:00:00Z",
        "digest": "sha256:..."
      }
    ],
    "activeRequests": 3,
    "totalRequests": 1500,
    "avgResponseTime": 250.5,
    "requestsPerSecond": 12.5
  }
}
```

---

### Backends

#### GET /api/v1/backends

Получение списка всех бэкендов.

**Ответ:**

```json
{
  "backends": [
    {
      "id": "gpu-1",
      "name": "GPU Server 1",
      "host": "192.168.13.66",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 1,
      "maxConcurrentRequests": 10,
      "labels": ["nvidia", "rtx4090"],
      "status": "healthy",
      "lastHealthCheck": "2024-01-15T10:30:00Z",
      "consecutiveFailures": 0,
      "activeRequests": 3
    }
  ],
  "total": 2
}
```

#### POST /api/v1/backends

Добавление нового бэкенда.

**Тело запроса:**

```json
{
  "id": "gpu-3",
  "name": "GPU Server 3",
  "host": "192.168.13.80",
  "ollamaPort": 11434,
  "agentPort": 18032,
  "weight": 1,
  "maxConcurrentRequests": 10,
  "labels": ["nvidia", "a100"]
}
```

**Ответ:**

```json
{
  "success": true,
  "backend": {...},
  "message": "Backend added successfully. Health check will run automatically."
}
```

#### GET /api/v1/backends/{backend_id}

Получение информации о бэкенде.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `backend_id` | path | ID бэкенда |

#### DELETE /api/v1/backends/{backend_id}

Удаление бэкенда.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `backend_id` | path | ID бэкенда |

**Ответ:**

```json
{
  "success": true,
  "id": "gpu-1",
  "message": "Backend removed successfully"
}
```

---

### Sessions

#### GET /api/v1/sessions

Получение списка активных сессий.

**Ответ:**

```json
{
  "sessions": [],
  "total": 0
}
```

#### DELETE /api/v1/sessions

Очистка всех сессий.

**Ответ:**

```json
{
  "success": true,
  "message": "All sessions cleared"
}
```

#### DELETE /api/v1/sessions/{session_id}

Удаление конкретной сессии.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `session_id` | path | ID сессии |

---

### Models

#### GET /api/v1/models

Получение списка запущенных моделей на всех бэкендах.

**Ответ:**

```json
{
  "models": {
    "gpu-1": ["llama3.1:70b", "mistral:7b"],
    "gpu-2": ["llama3.1:8b"]
  }
}
```

---

### Agents

#### GET /api/v1/agents/stats

Получение статистики всех зарегистрированных агентов.

**Ответ:**

```json
{
  "totalAgents": 2,
  "healthyAgents": 2,
  "agents": [
    {
      "id": "gpu-1",
      "hostname": "gpu-server-1",
      "status": "healthy",
      "lastHeartbeat": "2024-01-15T10:30:00Z"
    }
  ]
}
```

#### GET /api/v1/agents/{agent_id}

Получение информации о конкретном агенте.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `agent_id` | path | ID агента |

---

### Agents (Registration & Metrics)

#### POST /api/v1/agents/register

Регистрация агента в системе балансировки. Агент отправляет этот запрос при запуске для автоматической регистрации в кластере.

**Аутентификация:** Требуется (API Token)

**Request Body:**
```json
{
  "agentId": "gpu-1",
  "hostname": "gpu-server-1",
  "host": "192.168.1.100",
  "ollamaPort": 11434,
  "agentPort": 18032,
  "gpuCount": 1,
  "name": "GPU Server 1",
  "labels": ["nvidia", "rtx4090"]
}
```

**Параметры запроса:**

| Параметр | Тип | Обязательный | Описание |
|----------|-----|--------------|----------|
| `agentId` | string | Да | Уникальный идентификатор агента |
| `hostname` | string | Нет | Имя хоста агента |
| `host` | string | Нет | IP-адрес агента (если не указан, используется hostname или RemoteAddr) |
| `ollamaPort` | int | Нет | Порт Ollama API (по умолчанию: 11434) |
| `agentPort` | int | Нет | Порт агента (по умолчанию: 18032) |
| `gpuCount` | int | Нет | Количество GPU |
| `name` | string | Нет | Отображаемое имя агента |
| `labels` | string[] | Нет | Метки для классификации агента |

**Response:** 201 Created (новый агент) или 200 OK (обновление существующего)
```json
{
  "success": true,
  "action": "created",
  "agentId": "gpu-1",
  "backend": {
    "id": "gpu-1",
    "name": "GPU Server 1",
    "host": "192.168.1.100",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "weight": 1,
    "maxConcurrentRequests": 10,
    "labels": ["nvidia", "rtx4090"],
    "status": "starting"
  },
  "message": "Agent registered successfully. Backend added to the pool."
}
```

**Пример запроса (cURL):**
```bash
curl -X POST http://localhost:18081/api/v1/agents/register \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-api-token" \
  -d '{
    "agentId": "gpu-1",
    "host": "192.168.1.100",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "name": "GPU Server 1",
    "labels": ["nvidia", "rtx4090"]
  }'
```

---

#### POST /api/v1/agents/metrics

Отправка метрик от агента балансировщику. Агент должен периодически отправлять метрики для мониторинга состояния.

**Аутентификация:** Требуется (API Token)

**Headers:**

| Header | Значение | Описание |
|--------|----------|----------|
| `X-Agent-ID` | string | ID агента (обязательно) |

**Request Body — Полная структура метрик от агента:**

> **Важно:** Поля `activeRequests`, `totalRequests`, `avgResponseTime`, `requestsPerSecond` в секции `ollama` возвращаются агентом как `0` и **переопределяются балансером** на основе внутреннего proxy-счётчика.

```json
{
  "id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": {
    "usagePercent": 45.5,
    "memoryTotal": 24576,
    "memoryUsed": 12000,
    "memoryFree": 12576,
    "temperature": 65,
    "powerUsage": 250,
    "powerLimit": 450,
    "gpuClock": 1800,
    "memClock": 10000
  },
  "system": {
    "cpuUsagePercent": 30.2,
    "memoryTotal": 65536,
    "memoryUsed": 20000,
    "memoryFree": 45536,
    "diskTotal": 1000000,
    "diskUsed": 500000,
    "diskFree": 500000,
    "networkRX": 1048576,
    "networkTX": 524288
  },
  "ollama": {
    "runningModels": [
      {
        "name": "llama3.1:70b",
        "size": 70000000000,
        "vramUsage": 18000,
        "expiresAt": "2024-01-15T11:00:00Z",
        "digest": "sha256:...",
        "family": "llama",
        "parameterSize": "70B",
        "quantization": "Q4_0"
      }
    ],
    "availableModels": [
      {
        "name": "mistral:7b",
        "size": 4000000000,
        "family": "mistral",
        "parameterSize": "7B",
        "quantization": "Q4_0"
      }
    ],
    "activeRequests": 0,
    "totalRequests": 0,
    "avgResponseTime": 0,
    "requestsPerSecond": 0,
    "maxModels": -1,
    "maxConcurrentRequests": -1,
    "freeSlots": 0
  }
}
```

**Response:** 200 OK
```json
{
  "status": "received"
}
```

**Пример запроса (cURL):**
```bash
curl -X POST http://localhost:18081/api/v1/agents/metrics \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-api-token" \
  -H "X-Agent-ID: gpu-1" \
  -d '{
    "id": "gpu-1",
    "timestamp": "2024-01-15T10:30:00Z",
    "gpu": {
      "usagePercent": 45.5,
      "memoryTotal": 24576,
      "memoryUsed": 12000
    },
    "system": {...},
    "ollama": {...}
  }'
```

---

### Predictions (Прогнозирование)

Балансер автоматически рассчитывает прогнозы критических состояний для каждого бэкенда на основе истории метрик (до 120 точек, окно 2 минуты).

#### GET /api/v1/predictions

Получение прогнозов для всех бэкендов.

**Ответ:**
```json
{
  "gpu-1": {
    "backendId": "gpu-1",
    "secondsToCritical": 300,
    "criticalReason": "vram",
    "gpuUsageTrend": 0.5,
    "vramUsageTrend": 2.1,
    "ramUsageTrend": 0.3,
    "freeSlotsTrend": -0.8,
    "requestCapacity": 65.5
  }
}
```

| Поле | Тип | Описание |
|------|-----|----------|
| `secondsToCritical` | float | Секунд до критического состояния. `-1` = нет угрозы |
| `criticalReason` | string | Причина: `gpu_usage`, `vram`, `ram`, `concurrent_requests`, `models_capacity`, `none` |
| `gpuUsageTrend` | float | Тренд загрузки GPU (%/мин) |
| `vramUsageTrend` | float | Тренд использования VRAM (%/мин) |
| `ramUsageTrend` | float | Тренд использования RAM (%/мин) |
| `freeSlotsTrend` | float | Тренд свободных слотов (штук/мин) |
| `requestCapacity` | float | Текущая загрузка бэкенда (0-100%) |

---

#### POST /api/v1/agents/heartbeat

Отправка heartbeat сигнала от агента для подтверждения активности.

**Аутентификация:** Требуется (API Token)

**Headers:**

| Header | Значение | Описание |
|--------|----------|----------|
| `X-Agent-ID` | string | ID агента (обязательно) |

**Request Body:** Не требуется (может быть пустым)

**Response:** 200 OK
```json
{
  "status": "ok"
}
```

**Пример запроса (cURL):**
```bash
curl -X POST http://localhost:18081/api/v1/agents/heartbeat \
  -H "X-API-Token: your-api-token" \
  -H "X-Agent-ID: gpu-1"
```

---

### Auth

#### GET /api/v1/auth/status

Получение статуса системы аутентификации.

**Ответ:**

```json
{
  "enabled": true,
  "headerName": "X-API-Token",
  "tokenCount": 3
}
```

#### POST /api/v1/auth/token

Создание нового API токена. Требует master token.

**Ответ:**

```json
{
  "success": true,
  "token": "newly-generated-token-here",
  "message": "Token generated successfully"
}
```

#### DELETE /api/v1/auth/token

Отзыв (удаление) API токена. Требует master token.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `token` | query/body | Токен для отзыва |

**Ответ:**

```json
{
  "success": true,
  "message": "Token revoked successfully"
}
```

---

### Rate Limit

#### GET /api/v1/ratelimit/status

Получение статуса rate limiter. Не требует аутентификации.

**Ответ:**

```json
{
  "tokens": 100.0,
  "max_tokens": 100,
  "refill_rate": 10.0
}
```

---

## WebSocket API

### GET /ws/metrics

WebSocket подключение для получения метрик в реальном времени.

**Протокол:** WebSocket
**Формат сообщений:** JSON
**Аутентификация:** Требуется токен в query параметре `?token=xxx`

### Аутентификация

Токен должен быть передан в query параметре URL. Это необходимо для защиты от DoS-атак, так как проверка аутентификации выполняется ДО WebSocket upgrade.

**Важно:** При отсутствии или невалидности токена сервер вернет HTTP 401 ошибку до установления WebSocket соединения.

### Подключение

```javascript
// Передача токена через query параметр
const ws = new WebSocket('ws://localhost:18081/ws/metrics?token=your-api-token');

ws.onopen = () => {
  console.log('Connected to metrics stream');
};

ws.onmessage = (event) => {
  const metrics = JSON.parse(event.data);
  console.log('Received metrics:', metrics);
  
  // metrics содержит:
  // - id: идентификатор бэкенда
  // - timestamp: время сбора метрик
  // - gpu: метрики GPU (usagePercent, memoryTotal, memoryUsed, temperature, etc.)
  // - system: системные метрики (cpuUsagePercent, memoryTotal, memoryUsed, etc.)
  // - ollama: метрики Ollama (runningModels, activeRequests, requestsPerSecond, etc.)
};

ws.onerror = (error) => {
  console.error('WebSocket error:', error);
};

ws.onclose = () => {
  console.log('Connection closed');
};
```

### Примеры подключения

```javascript
// Browser JavaScript
const token = 'your-api-token';
const ws = new WebSocket(`ws://localhost:18081/ws/metrics?token=${token}`);

// Node.js с библиотекой ws
const WebSocket = require('ws');
const token = 'your-api-token';
const ws = new WebSocket(`ws://localhost:18081/ws/metrics?token=${token}`);

// Secure WebSocket (если включен TLS)
const wss = new WebSocket(`wss://localhost:8443/ws/metrics?token=${token}`);
```

### Пример ответа WebSocket

```json
{
  "id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": {
    "usagePercent": 45.5,
    "memoryTotal": 24576,
    "memoryUsed": 12000,
    "memoryFree": 12576,
    "temperature": 65,
    "powerUsage": 250,
    "powerLimit": 450,
    "gpuClock": 1800,
    "memClock": 10000
  },
  "system": {
    "cpuUsagePercent": 30.2,
    "memoryTotal": 65536,
    "memoryUsed": 20000,
    "memoryFree": 45536,
    "diskTotal": 1000000,
    "diskUsed": 500000,
    "diskFree": 500000,
    "networkRX": 1048576,
    "networkTX": 524288
  },
  "ollama": {
    "runningModels": [
      {
        "name": "llama3.1:70b",
        "size": 70000000000,
        "vramUsage": 18000
      }
    ],
    "activeRequests": 3,
    "totalRequests": 1500,
    "avgResponseTime": 250.5,
    "requestsPerSecond": 12.5
  }
}
```

---

## Примеры запросов

### cURL

```bash
# Health check
curl http://localhost:18081/api/v1/health

# Cluster status
curl -X GET http://localhost:18081/api/v1/cluster \
  -H "X-API-Token: your-token"

# List backends
curl -X GET http://localhost:18081/api/v1/backends \
  -H "X-API-Token: your-token"

# Add backend
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-token" \
  -d '{
    "id": "gpu-3",
    "name": "GPU Server 3",
    "host": "192.168.13.80",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "weight": 1,
    "maxConcurrentRequests": 10
  }'

# Delete backend
curl -X DELETE http://localhost:18081/api/v1/backends/gpu-1 \
  -H "X-API-Token: your-token"

# Generate new token (requires master token)
curl -X POST http://localhost:18081/api/v1/auth/token \
  -H "X-API-Token: your-master-token"

# Revoke token
curl -X DELETE "http://localhost:18081/api/v1/auth/token?token=token-to-revoke" \
  -H "X-API-Token: your-master-token"
```

### Python

```python
import requests

# Настройка сессии с токеном
session = requests.Session()
session.headers.update({'X-API-Token': 'your-api-token'})

# Health check (без токена)
response = requests.get('http://localhost:18081/api/v1/health')
print(response.json())

# Cluster status
response = session.get('http://localhost:18081/api/v1/cluster')
print(response.json())

# List backends
response = session.get('http://localhost:18081/api/v1/backends')
print(response.json())

# Add backend
new_backend = {
    'id': 'gpu-3',
    'name': 'GPU Server 3',
    'host': '192.168.13.80',
    'ollamaPort': 11434,
    'agentPort': 18032,
    'weight': 1,
    'maxConcurrentRequests': 10
}
response = session.post('http://localhost:18081/api/v1/backends', json=new_backend)
print(response.json())

# Delete backend
response = session.delete('http://localhost:18081/api/v1/backends/gpu-1')
print(response.json())

# Generate token
response = session.post('http://localhost:18081/api/v1/auth/token')
print(response.json())
```

### JavaScript/Node.js

```javascript
const axios = require('axios');

const API_BASE = 'http://localhost:18081';
const TOKEN = 'your-api-token';

const api = axios.create({
  baseURL: API_BASE,
  headers: {
    'X-API-Token': TOKEN
  }
});

// Health check
async function healthCheck() {
  const response = await axios.get(`${API_BASE}/api/v1/health`);
  console.log(response.data);
}

// Cluster status
async function getClusterStatus() {
  const response = await api.get('/api/v1/cluster');
  console.log(response.data);
}

// List backends
async function listBackends() {
  const response = await api.get('/api/v1/backends');
  console.log(response.data);
}

// Add backend
async function addBackend(backend) {
  const response = await api.post('/api/v1/backends', backend);
  console.log(response.data);
}

// Delete backend
async function deleteBackend(id) {
  const response = await api.delete(`/api/v1/backends/${id}`);
  console.log(response.data);
}

// WebSocket connection (token required in query parameter)
const WebSocket = require('ws');
const TOKEN = 'your-api-token';
const ws = new WebSocket(`ws://${API_BASE}/ws/metrics?token=${TOKEN}`);

ws.on('message', (data) => {
  const metrics = JSON.parse(data);
  console.log('Received metrics:', metrics);
});
```

---

## OpenAPI спецификация

Полная OpenAPI 3.0.3 спецификация доступна в файле [`openapi.yaml`](openapi.yaml).

### Просмотр через Swagger UI

```bash
# Запуск Swagger UI через Docker
docker run -d -p 8080:8080 -e SWAGGER_JSON=/api/swagger.json \
  -v $(pwd)/docs:/api swaggerapi/swagger-ui

# Затем откройте http://localhost:8080
```

### Использование Swagger UI

1. Откройте https://editor.swagger.io
2. Загрузите файл `openapi.yaml`
3. Просматривайте и тестируйте API

---

## Сводная таблица endpoints

| Метод | Endpoint | Описание | Аутентификация |
|-------|----------|----------|----------------|
| `GET` | `/api/v1/health` | Health check | ❌ |
| `GET` | `/api/v1/cluster` | Статус кластера | ✅ |
| `GET` | `/api/v1/metrics` | Метрики всех бэкендов | ✅ |
| `GET` | `/api/v1/metrics/{id}` | Метрики бэкенда | ✅ |
| `GET` | `/api/v1/backends` | Список бэкендов | ✅ |
| `POST` | `/api/v1/backends` | Добавить бэкенд | ✅ |
| `GET` | `/api/v1/backends/{id}` | Информация о бэкенде | ✅ |
| `DELETE` | `/api/v1/backends/{id}` | Удалить бэкенд | ✅ |
| `GET` | `/api/v1/sessions` | Список сессий | ✅ |
| `DELETE` | `/api/v1/sessions` | Очистить сессии | ✅ |
| `DELETE` | `/api/v1/sessions/{id}` | Удалить сессию | ✅ |
| `GET` | `/api/v1/models` | Запущенные модели | ✅ |
| `POST` | `/api/v1/agents/register` | Регистрация агента | ✅ |
| `POST` | `/api/v1/agents/metrics` | Отправка метрик от агента | ✅ |
| `POST` | `/api/v1/agents/heartbeat` | Heartbeat сигнал агента | ✅ |
| `GET` | `/api/v1/agents/stats` | Статистика агентов | ✅ |
| `GET` | `/api/v1/agents/{id}` | Информация об агенте | ✅ |
| `GET` | `/api/v1/auth/status` | Статус аутентификации | ✅ |
| `POST` | `/api/v1/auth/token` | Создать токен | ✅ (master) |
| `DELETE` | `/api/v1/auth/token` | Отозвать токен | ✅ (master) |
| `GET` | `/api/v1/queue/stats` | Статистика очереди | ✅ |
| `GET` | `/api/v1/queue/details` | Детали очереди (pending+processing) | ✅ |
| `GET` | `/api/v1/queue/history` | История выполненных запросов | ✅ |
| `GET` | `/api/v1/predictions` | Прогнозы для всех бэкендов | ✅ |
| `GET` | `/api/v1/predictions/{id}` | Прогноз для бэкенда | ✅ |
| `GET` | `/api/v1/models/capacity` | Глобальная ёмкость моделей | ✅ |
| `GET` | `/api/v1/backends/{id}/capacity` | Ёмкость бэкенда | ✅ |
| `PUT` | `/api/v1/backends/{id}/limits` | Обновить runtime-лимиты бэкенда | ✅ |
| `GET` | `/api/v1/cluster/config` | Текущая конфигурация кластера | ✅ |
| `PUT` | `/api/v1/cluster/config` | Сменить алгоритм/настройки кластера | ✅ |
| `POST` | `/api/v1/admin/restart` | Перезапуск балансировщика | ✅ |
| `GET` | `/monitor` | HTML-страница монитора | ✅ |
| `GET` | `/api/v1/ratelimit/status` | Статус rate limiter | ❌ |
| `GET` | `/ws/metrics?token=xxx` | WebSocket метрики | ✅ (token в query) |

---

## Следующие шаги

- [Troubleshooting](troubleshooting.md) — Решение проблем
- [Конфигурация](configuration.md) — Настройка аутентификации и TLS
- [Развертывание](deployment.md) — Production deployment
