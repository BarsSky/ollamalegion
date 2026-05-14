# Changelog

Все заметные изменения в проекте Ollama Legion будут задокументированы в этом файле.

Формат ведётся в соответствии с [Keep a Changelog](https://keepachangelog.com/ru/1.0.0/),
и этот проект придерживается [Semantic Versioning](https://semver.org/lang/ru/).

## [0.1.0] - 2026-05-14

### Добавлено

#### Ядро балансировщика
- Resource-aware алгоритм балансировки с учётом GPU/CPU/RAM/Disk метрик
- Round Robin и Least Connections алгоритмы как альтернатива
- Session Stickiness — привязка сессий клиентов к бэкендам
- Model Affinity — направление запросов к серверам с уже загруженной моделью
- Queue Manager с FIFO-очередью для обработки перегрузок
- Автоматический prewarm моделей
- Weight Tuner — динамическая подстройка весов бэкендов
- Backpressure и headroom management
- Поддержка стриминга (Server-Sent Events)
- Автоматический health check бэкендов
- Unload scheduler — выгрузка неиспользуемых моделей под нагрузкой

#### Агент сбора метрик
- Сбор метрик GPU (загрузка, VRAM, температура, мощность, частоты) через NVML
- Сбор системных метрик (CPU, RAM, Disk)
- Интеграция с Ollama API (статистика запущенных моделей, RPS, время ответа)
- Регистрация и heartbeat агентов
- Автоматическое определение unhealthy/degraded статусов

#### REST API
- `GET /api/v1/health` — health check
- `GET /api/v1/cluster` — состояние кластера
- `GET/PUT/DELETE /api/v1/backends` — управление бэкендами
- `GET /api/v1/agents/*` — регистрация, метрики, heartbeat агентов
- `GET /api/v1/sessions` — активные сессии с идентификацией клиентов
- `GET /api/v1/models` — список запущенных моделей
- `GET /api/v1/metrics` — метрики бэкендов
- `GET /api/v1/queue/details` — детали очереди запросов
- `PUT /api/v1/cluster/config` — смена алгоритма балансировки без перезапуска
- `POST /api/v1/auth/*` — управление API токенами
- `GET /api/v1/ratelimit/status` — статус rate limiting

#### WebSocket
- `/ws/metrics` — real-time поток метрик через WebSocket
- MetricsBroker с pub/sub паттерном
- Автоматическая отправка начального состояния кластера при подключении
- Heartbeat для поддержания соединения

#### Web UI Dashboard
- Визуализация метрик GPU/CPU/RAM в реальном времени
- Мониторинг активных сессий и моделей
- Управление бэкендами через интерфейс
- Монитор с Canvas-визуализацией (топология, conveyor)
- Поддержка русской и английской локализации
- Nginx с gzip, security headers, проксированием API и WebSocket

#### Безопасность
- TLS 1.2/1.3 поддержка с self-signed сертификатами
- API Token аутентификация с Master-токеном
- Rate Limiting (Token Bucket алгоритм)
- HTTPS редирект с HTTP

#### Конфигурация
- Поддержка JSON-конфига и переменных окружения
- Environment override: `LB_ALGORITHM`, `LB_MODEL_AFFINITY`, `LB_SESSION_STICKINESS` и др.
- Настраиваемые ресурсные лимиты (GPU, CPU, RAM, Disk)
- Настройки логирования (уровень, формат)

#### Документация
- OpenAPI 3.0.3 спецификация (`docs/openapi.yaml`, `docs/swagger.json`)
- Полная документация на русском и английском: установка, конфигурация, развёртывание, API
- Balancing guide с описанием алгоритмов
- Troubleshooting guide
- Agent deployment guide

#### Инфраструктура
- Docker-контейнеры для balancer, agent, webui
- Docker Compose конфигурация (основная, agent, agent GPU, cocoindex)
- Скрипты сборки (`build.bat`, `build.sh`)
- Скрипты деплоя (`deploy-agent.sh`, `deploy-agent-docker.sh`, `deploy-agent-docker.ps1`)
- Скрипты нагрузочного тестирования (Python)
- GitHub Actions ready

#### Тестирование
- 170+ интеграционных тестов
- Тесты агента: сбор метрик, регистрация, heartbeat (28 тестов)
- Тесты аутентификации: TokenAuthenticator, middleware (24 теста)
- Тесты MetricsBroker: pub/sub, множественные клиенты (17 тестов)
- Тесты Rate Limiting: token bucket (11 тестов)
- Тесты Proxy/Queue: очередь, выбор бэкенда, session manager (20 тестов)
- Сценарии: failover, stickiness, concurrent, prewarm, backpressure (70 тестов)
- E2E тесты: full workflow, cluster state, error handling

### Исправлено

- Nil pointer dereference в proxy при проксировании generate-запросов
- Cline монополизация балансировщика — добавлена ребалансировка при загрузке >70%
- Сессии без идентификации клиента — добавлены `ClientName`, `ClientIP`
- Агент показывает unhealthy в Docker — healthcheck timeout увеличен до 60s
- Неясно какой алгоритм работает — добавлено логирование алгоритма при старте
- Обработка ошибок при недоступности NVML библиотеки
- Утечка памяти при отключении WebSocket клиентов
- Race condition в SessionManager при очистке сессий
- Пустая очередь — добавлен endpoint `/api/v1/queue/details`
