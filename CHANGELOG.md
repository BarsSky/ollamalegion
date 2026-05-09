# Changelog

Все заметные изменения в проекте Ollama Load Balancer будут задокументированы в этом файле.

Формат ведётся в соответствии с [Keep a Changelog](https://keepachangelog.com/ru/1.0.0/),
и этот проект придерживается [Semantic Versioning](https://semver.org/lang/ru/).

## [Unreleased]

### Добавлено (2026-05-04 — Консолидация планов и документации)

#### Консолидированный мастер-план
- Создан `plans/consolidated-plan-2026-05-03.md` — объединяет 6 разрозненных планов в единую дорожную карту (78 задач, 5 блоков).
- Выполненные планы перемещены в `plans/archive/`: `refactoring-plan-2026-04-29.md`, `optimal-distribution-mechanism.md`, `оптимизация-механизма-распределения.md`, `примечания.md`, `plan_balancer.md`, `fix-docs-plan-2026-05-03.md`.

#### Конфигурируемый порог загрузки (thresholdLoad)
- **Файл:** `internal/balancer/proxy.go:642-644` — `Model Affinity` теперь использует `Prewarm.TriggerLoadThreshold` из конфигурации вместо hardcoded `0.80`.
- **Параметр:** `balancing.prewarm.triggerLoadThreshold` (default: `0.80`).

#### Метрики диска
- **Файл:** `pkg/types/types.go` — в `SystemMetrics` добавлены поля `DiskTotal`, `DiskUsed`, `DiskFree` (Total/Used/Free в MB).

#### Документация
- **`docs/README.md`** — актуализирована структура проекта (добавлены `pkg/logger/`, `cmd/monitor/`, `tests/`, `logo/`, `plans/`, `balancing-guide.md`; исправлено расположение `queue.go`).
- **`docs/configuration.md`** — добавлен порт `TLSPort+1` (8444) для HTTPS Management API.
- **`docs/deployment.md`** — добавлен порт `TLSPort+1` (8444) в секцию TLS.

### Исправлено (Балансировщик — критические проблемы от 2026-04-28)

#### 1. Cline монополизирует балансировщик — Session Stickiness без ребалансировки
**Проблема:** Cline через OpenWebUI отправлял все запросы на один бэкенд, полностью занимая его. Другие клиенты не получали доступ к бэкенду.

**Исправления:**
- **Ребалансировка при превышении загрузки 70%:** Если бэкенд сессии загружен более 70% от `MaxConcurrentReqs`, балансировщик ищет альтернативный бэкенд с той же моделью и меньшей загрузкой.
- **Файл:** `internal/balancer/proxy.go` — добавлена функция `findLessLoadedBackendWithModel()` и логика ребалансировки в `ServeHTTP()`.

#### 2. Непонятно какие агенты с кем работают — сессии без идентификации клиента
**Проблема:** В API `/api/v1/sessions` не было информации об имени клиента (Cline/OpenWebUI) и IP.

**Исправления:**
- **Расширен Session struct:** добавлены поля `ClientName`, `ClientIP`.
- **Стабильный session ID:** `getSessionID()` больше не использует эфемерный порт. Приоритет: `X-Client-ID` > `X-Session-ID` > cookie > IP (без порта).
- **Извлечение имени клиента:** `getClientName()` парсит `X-Client-Name` или User-Agent ("Cline", "OpenWebUI").
- **Файлы:** `pkg/types/session.go`, `internal/balancer/proxy.go`, `internal/api/handlers.go`.

#### 3. Агент показывает `unhealthy` в Docker, но работает
**Проблема:** Healthcheck агента timeout был 10s — агент не успевал инициализироваться.

**Исправления:**
- **Timeout увеличен до 60s:** Docker healthcheck теперь ждёт 60 секунд до ответа.
- **Замена wget на curl:** curl с `-f` корректно обрабатывает HTTP-ошибки и fallback на корневой endpoint.
- **Healthcheck interval/retries:** `start_period: 60s` даёт время на инициализацию до первой проверки.
- **Файл:** `deployments/docker-compose.agent.yml`.

#### 4. Неясно какой алгоритм балансировки работает после смены
**Проблема:** После смены алгоритма и перезапуска оставался старый вариант из config.json.

**Исправления:**
- **Environment override `LB_ALGORITHM`:** Приоритет `env` > config.json. Добавлены переменные `LB_ALGORITHM`, `LB_MODEL_AFFINITY`, `LB_SESSION_STICKINESS`.
- **Runtime смена через API:** `PUT /api/v1/cluster/config` позволяет менять алгоритм без перезапуска.
- **Логирование алгоритма:** При старте балансировщика печатается текущий алгоритм в баннере.
- **Файл:** `cmd/balancer/main.go`, `internal/api/handlers.go`.

#### 5. Пустая вкладка "Очередь" — нет информации о pending-запросах
**Проблема:** Не было API для просмотра деталей очереди и pending-запросов.

**Исправления:**
- **Новый endpoint:** `GET /api/v1/queue/details` — возвращает детали очереди (длина, maxSize, processed, workers, timeout).
- **Расширен `queueStatsHandler`:** возвращает подробную информацию о состоянии очереди.
- **Файл:** `internal/api/handlers.go`.

### Добавлено

#### Интеграционные тесты (~170 тестов)
- Полное покрытие тестами основных компонентов системы
- **Ядро (internal/):** 100 тестов
  - Тесты агента (`internal/agent/collector_test.go`): сбор GPU/Ollama/системных метрик, регистрация, heartbeat, статусы degraded/healthy — **28 тестов**
  - Тесты аутентификации (`internal/api/auth_test.go`): TokenAuthenticator, master token, генерация/отзыв токенов, middleware — **24 теста**
  - Тесты MetricsBroker (`internal/api/metrics_broker_test.go`): pub/sub, множественные клиенты, потокобезопасность — **17 тестов**
  - Тесты Rate Limiting (`internal/api/ratelimit_test.go`): token bucket, пополнение, middleware — **11 тестов**
  - Тесты Proxy/Queue Manager (`internal/balancer/proxy_test.go`): очередь, выбор бэкенда (resource-aware), session/metrics manager — **20 тестов**
- **Внешние тесты (tests/):** 70 тестов
  - `balancer_scenarios_test.go` — 9 сценариев (failover, stickiness, health, concurrent)
  - `agent_metrics_test.go` — 11 тестов структуры метрик, диапазонов, консистентности
  - `balancer_optimization_test.go` — 11 тестов (prewarm, warmup, model instance controller)
  - `balancer_new_test.go` — 7 тестов (headroom, backpressure, scoring)
  - `integration_test.go` — 6 E2E тестов (full workflow, cluster state, error handling)
  - `monitor_test.go` + `monitor_display_test.go` — 13 тестов (RPS, JSON, endpoints, HTML)
  - `proxy_ollama_test.go` — 12 тестов (generate, chat, embeddings, stickiness, retry)
  - `hasagent_test.go` — 1 тест флага hasAgents
- **Файлы:** [`internal/agent/collector_test.go`](internal/agent/collector_test.go), [`internal/api/auth_test.go`](internal/api/auth_test.go), [`internal/api/metrics_broker_test.go`](internal/api/metrics_broker_test.go), [`internal/api/ratelimit_test.go`](internal/api/ratelimit_test.go), [`internal/balancer/proxy_test.go`](internal/balancer/proxy_test.go), и 9 файлов в [`tests/`](../tests/)

#### OpenAPI/Swagger спецификация
- Полная спецификация REST API в формате OpenAPI 3.0.3
- Документирование всех endpoints:
  - Health check (`/api/v1/health`)
  - Cluster status (`/api/v1/cluster`)
  - Metrics endpoints (`/api/v1/metrics`, `/api/v1/metrics/{id}`)
  - Backend management (`/api/v1/backends`)
  - Session management (`/api/v1/sessions`)
  - Models list (`/api/v1/models`)
  - Agent endpoints (`/api/v1/agents/*`)
  - Authentication (`/api/v1/auth/*`)
  - Rate limit status (`/api/v1/ratelimit/status`)
  - WebSocket endpoint (`/ws/metrics`)
- Схемы компонентов для всех типов данных:
  - BackendMetrics, GPUMetrics, SystemMetrics, OllamaMetrics
  - BackendInfo, SessionInfo, RunningModel
  - AuthStatus, TokenResponse, RateLimitStatus
  - Error responses
- Security схемы (ApiKeyAuth)
- Поддержка Swagger UI для интерактивной документации
- **Файлы:** [`docs/openapi.yaml`](docs/openapi.yaml), [`docs/swagger.json`](docs/swagger.json)

#### Web UI Dashboard
- Контейнеризированный Web UI для мониторинга кластера
- Nginx конфигурация с:
  - Gzip сжатием для оптимизации трафика
  - Security headers (X-Frame-Options, X-Content-Type-Options, X-XSS-Protection)
  - Проксирование API запросов к балансировщику
  - WebSocket поддержка для real-time метрик
  - Кэширование статических ассетов
  - Health check endpoint
- Dockerfile для сборки образа webui
- Интеграция с docker-compose
- Поддержка:
  - Dashboard с визуализацией метрик GPU/CPU/RAM
  - Real-time обновления через WebSocket
  - Управление бэкендами
  - Мониторинг активных сессий и моделей
- **Файлы:** [`webui/nginx.conf`](webui/nginx.conf), [`docker/webui/Dockerfile`](docker/webui/Dockerfile), [`docker/webui/nginx.conf`](docker/webui/nginx.conf)

#### WebSocket Server для real-time мониторинга
- WebSocket endpoint `/ws/metrics` для потоковой передачи метрик
- MetricsBroker с pub/sub паттерном для распределения метрик между клиентами
- Поддержка множественных подключений клиентов
- Автоматическая отправка начального состояния кластера при подключении
- Heartbeat механизм для поддержания соединения
  - **Файлы:** [`internal/api/metrics_broker.go`](internal/api/metrics_broker.go), [`internal/api/handlers.go`](internal/api/handlers.go:1379-1514)

#### Queue Manager для обработки перегрузок
- Очередь запросов при отсутствии доступных бэкендов
- Настраиваемый максимальный размер очереди (`queueMaxSize`)
- Таймаут ожидания в очереди (`queueTimeout`)
- Обработка запросов в порядке FIFO
- Статистика очереди (текущий размер, обработано, среднее время ожидания)
- Worker для обработки очереди с автоматическим повтором
- **Файлы:** [`internal/balancer/proxy.go`](internal/balancer/proxy.go:52-70), [`internal/balancer/proxy.go`](internal/balancer/proxy.go:549-701)

#### Ollama API интеграция
- Сбор статистики запущенных моделей с каждого бэкенда
- Мониторинг активных запросов в реальном времени
- Расчёт RPS (requests per second) для каждого бэкенда
- Среднее время ответа API
- Model Affinity - направление запросов к серверам с загруженной моделью
- Интеграция с Ollama API endpoints (`/api/tags`, `/api/ps`)
- **Файлы:** [`internal/agent/collector.go`](internal/agent/collector.go:403-592), [`internal/balancer/proxy.go`](internal/balancer/proxy.go:247-266)

#### NVML поддержка для GPU метрик
- Интеграция с NVIDIA Management Library (go-nvml)
- Точные метрики GPU:
  - Загрузка GPU (%)
  - Использование VRAM (Total/Used/Free)
  - Температура GPU
  - Потребление мощности (Power Usage/Limit)
  - Частоты GPU и памяти (gpuClock, memClock)
- Поддержка Linux/Unix и Windows
- Автоматическая инициализация NVML при старте
- Graceful shutdown с освобождением ресурсов NVML
- **Файлы:** [`internal/agent/nvml_unix.go`](internal/agent/nvml_unix.go), [`internal/agent/nvml_windows.go`](internal/agent/nvml_windows.go), [`internal/agent/collector.go`](internal/agent/collector.go:310-401)

#### TLS/SSL поддержка
- Генерация self-signed сертификатов
- Поддержка TLS 1.2 и TLS 1.3
- HTTPS редирект с HTTP
- Middleware для редиректа HTTP → HTTPS
- AutoCert для автоматической генерации сертификатов
- **Файлы:** [`internal/api/tls.go`](internal/api/tls.go)

#### API Token аутентификация
- TokenAuthenticator для проверки API токенов
- Поддержка кастомных заголовков (X-API-Token по умолчанию)
- Master token для управления другими токенами
- Генерация случайных токенов
- Middleware для защиты endpoints
- Endpoints для управления токенами (`/api/v1/auth/status`, `/api/v1/auth/token`)
- **Файлы:** [`internal/api/auth.go`](internal/api/auth.go)

#### Rate Limiting
- Token bucket алгоритм для ограничения частоты запросов
- Настраиваемые параметры rate limit и burst
- Отдельный rate limiter для WebSocket
- Заголовки X-RateLimit-Limit, X-RateLimit-Remaining, Retry-After
- Endpoint статуса `/api/v1/ratelimit/status`
- **Файлы:** [`internal/api/ratelimit.go`](internal/api/ratelimit.go), [`internal/api/handlers.go`](internal/api/handlers.go:98-124)

#### REST API endpoints
- `POST /api/v1/agents/register` - регистрация агента
- `POST /api/v1/agents/metrics` - получение метрик от агента
- `POST /api/v1/agents/heartbeat` - heartbeat от агента
- `GET /api/v1/models` - список запущенных моделей
- `GET /api/v1/sessions` - активные сессии
- `GET /api/v1/metrics/:id` - метрики конкретного бэкенда
- `PUT /api/v1/backends/:id` - обновление бэкенда
- `DELETE /api/v1/backends/:id` - удаление бэкенда
- `GET /api/v1/cluster` - состояние кластера
- `GET /api/v1/health` - health check API
- **Файлы:** [`internal/api/handlers.go`](internal/api/handlers.go:89-655)

### Изменено

#### Архитектура
- Обновлена диаграмма архитектуры с указанием новых компонентов
- Добавлен WebSocket Server как отдельный компонент
- Добавлен Queue Manager в состав Reverse Proxy

#### API
- Расширен ответ `/api/v1/cluster` с информацией о queued requests
- Добавлены новые поля в метрики GPU (powerUsage, powerLimit, gpuClock, memClock)
- Добавлены новые поля в метрики Ollama (activeRequests, totalRequests, avgResponseTime, requestsPerSecond)

#### Документация
- Обновлён README.md с информацией о новых возможностях
- Обновлён DEPLOYMENT.md с новыми параметрами конфигурации
- Добавлены примеры использования WebSocket endpoint
- Добавлена таблица компонентов системы со статусами

### Исправлено

- **Nil pointer dereference в proxy.go:397 при POST /api/generate** — балансировщик падал с panic при проксировании generate-запросов. Исправлено в актуальном коде; Docker-образ пересобран с фиксом. Проверено: 20/20 запросов успешно, 156.7 RPS.
- Обработка ошибок при недоступности NVML библиотеки
- Утечка памяти при отключении WebSocket клиентов
- Race condition в SessionManager при очистке сессий

### Технические детали

#### Новые типы данных
- `QueueManager` - управление очередью запросов
- `MetricsBroker` - pub/sub система для метрик
- `QueuedRequest` - запрос в очереди ожидания
- `MetricsClient` - подключенный WebSocket клиент
- `TokenAuthenticator` - аутентификатор на основе токенов
- `RateLimiter` - ограничитель частоты запросов

#### Зависимости
- `github.com/NVIDIA/go-nvml` - NVML bindings для Go
- `github.com/gorilla/websocket` - WebSocket поддержка

---

## Новые конфигурационные параметры

### Переменные окружения

#### LoadBalancer настройки
| Переменная | Описание | Значение по умолчанию |
|------------|----------|----------------------|
| `LB_HOST` | Хост для прослушивания | `0.0.0.0` |
| `LB_PORT` | Порт прокси | `18080` |
| `LB_API_PORT` | Порт API | `18081` |
| `LB_TLS_HOST` | Хост для TLS | `` |
| `LB_TLS_PORT` | Порт для TLS | `8443` |

#### TLS настройки
| Переменная | Описание | Значение по умолчанию |
|------------|----------|----------------------|
| `TLS_ENABLED` | Включение TLS | `false` |
| `TLS_CERT_FILE` | Путь к сертификату | `certs/server.crt` |
| `TLS_KEY_FILE` | Путь к ключу | `certs/server.key` |
| `TLS_MIN_VERSION` | Минимальная версия TLS | `TLS12` |
| `TLS_AUTO_CERT` | Автогенерация сертификатов | `false` |

#### Auth настройки
| Переменная | Описание | Значение по умолчанию |
|------------|----------|----------------------|
| `AUTH_ENABLED` | Включение аутентификации | `false` |
| `AUTH_TOKENS` | Список токенов (через запятую) | `` |
| `AUTH_HEADER_NAME` | Имя заголовка для токена | `X-API-Token` |

#### API Rate Limiting
| Переменная | Описание | Значение по умолчанию |
|------------|----------|----------------------|
| `API_RATE_LIMIT` | Лимит запросов в секунду | `100` |
| `API_RATE_BURST` | Burst capacity | `200` |

#### Balancing настройки
| Переменная | Описание | Значение по умолчанию |
|------------|----------|----------------------|
| `LB_ALGORITHM` | Алгоритм балансировки | `resource-aware` |
| `LB_MODEL_AFFINITY` | Включение model affinity | `true` |
| `LB_SESSION_STICKINESS` | Включение session stickiness | `true` |
| `LB_HEALTH_CHECK_INTERVAL` | Интервал health check (сек) | `10` |
| `LB_METRICS_INTERVAL` | Интервал сбора метрик (сек) | `5` |
| `LB_REQUEST_TIMEOUT` | Таймаут запроса (сек) | `120` |
| `LB_QUEUE_TIMEOUT` | Таймаут очереди (сек) | `300` |
| `LB_QUEUE_MAX_SIZE` | Максимальный размер очереди | `100` |
| `LB_QUEUE_WORKERS` | Количество workers очереди | `4` |

#### Resource лимиты
| Переменная | Описание | Значение по умолчанию |
|------------|----------|----------------------|
| `LB_GPU_MAX_USAGE` | Макс. загрузка GPU (%) | `90.0` |
| `LB_GPU_MAX_VRAM` | Макс. использование VRAM (%) | `85.0` |
| `LB_GPU_MAX_TEMP` | Макс. температура GPU (°C) | `85` |
| `LB_CPU_MAX_USAGE` | Макс. загрузка CPU (%) | `80.0` |
| `LB_MEMORY_MAX_USAGE` | Макс. использование RAM (%) | `85.0` |
| `LB_DISK_MIN_FREE_MB` | Мин. свободно на диске (MB) | `10240` |

#### Logging настройки
| Переменная | Описание | Значение по умолчанию |
|------------|----------|----------------------|
| `LB_LOG_LEVEL` | Уровень логирования | `info` |
| `LB_LOG_FORMAT` | Формат логов | `json` |

#### Backend переменные окружения
| Переменная | Описание |
|------------|----------|
| `BACKEND_{N}_ID` | ID бэкенда |
| `BACKEND_{N}_HOST` | Хост бэкенда |
| `BACKEND_{N}_PORT` | Порт Ollama |
| `BACKEND_{N}_AGENT_PORT` | Порт агента |
| `BACKEND_{N}_WEIGHT` | Вес бэкенда |
| `BACKEND_{N}_MAX_REQS` | Макс. одновременных запросов |
| `BACKEND_{N}_NAME` | Имя бэкенда |

### JSON параметры конфигурации

```json
{
  "loadBalancer": {
    "host": "0.0.0.0",
    "port": 18080,
    "apiPort": 18081,
    "tlsHost": "",
    "tlsPort": 8443
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
    "tokens": ["your-master-token-here"],
    "headerName": "X-API-Token"
  },
  "api": {
    "rateLimit": 100,
    "rateBurst": 200
  },
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
  }
}
```

**Файлы конфигурации:** [`config/config.example.json`](config/config.example.json), [`internal/config/config.go`](internal/config/config.go)

---

## Прогресс проекта

### Статистика кода

| Компонент | Файлы | Тесты | Строк кода |
|-----------|-------|-------|------------|
| Agent | 4 | 28 | ~600 |
| API/Balancer | 8 | 55 | ~800 |
| Config | 1 | - | ~200 |
| Web UI | 3 | - | ~100 |
| Документация | 2 | - | ~1100 |
| **Итого** | **18** | **170** | **~2800** |

### Покрытие тестами

| Модуль | Тестов | Покрытие функциональности |
|--------|--------|--------------------------|
| `internal/agent` | 28 | Сбор метрик, регистрация, heartbeat |
| `internal/api` (auth) | 24 | Аутентификация, токены, middleware |
| `internal/api` (metrics_broker) | 17 | WebSocket pub/sub система |
| `internal/api` (ratelimit) | 11 | Token bucket rate limiting |
| `internal/balancer` | 20 | Proxy, queue manager, session manager |
| **Всего** | **170** | **~95%** |

### Реализованные компоненты

- [x] Load Balancer с resource-aware алгоритмом
- [x] Agent для сбора метрик GPU/CPU/RAM/Disk
- [x] NVML интеграция для GPU метрик
- [x] Ollama API интеграция (сбор статистики моделей)
- [x] WebSocket Server для real-time мониторинга
- [x] Queue Manager для обработки перегрузок
- [x] TLS/SSL поддержка
- [x] API Token аутентификация
- [x] Rate Limiting
- [x] Session Stickiness
- [x] Model Affinity
- [x] Health Check API
- [x] REST API (15+ endpoints)
- [x] OpenAPI/Swagger спецификация
- [x] Web UI Dashboard
- [x] Интеграционные тесты (170 тестов)
- [x] Docker контейнеры (agent, balancer, webui)
- [x] Docker Compose конфигурация

### Технические достижения

- **Алгоритм балансировки**: Resource-aware с учётом GPU/CPU/RAM/Disk лимитов
- **Метрики**: 15+ метрик на бэкенд (GPU загрузка, VRAM, температура, power, clock)
- **Производительность**: Token bucket rate limiting до 100+ RPS
- **Масштабируемость**: Поддержка множественных бэкендов с весами
- **Надёжность**: Health check, degraded status, queue manager
- **Безопасность**: TLS 1.2/1.3, token authentication, security headers

---

## [0.1.0] - 2024-01-15

### Добавлено

- Начальная версия балансировщика нагрузки
- Базовая поддержка алгоритмов балансировки:
  - Round Robin
  - Least Connections
  - Resource-Aware
- Health Check для мониторинга бэкендов
- Session Stickiness для сохранения сессий
- Model Affinity для направления запросов к серверам с загруженной моделью
- Агент для сбора метрик GPU/CPU/RAM/Disk
- REST API для управления бэкендами
- Web UI Dashboard (базовая версия)
- Docker контейнеры для балансировщика и агента
- Docker Compose конфигурация
- Ск��ипты сборки и развертывания
