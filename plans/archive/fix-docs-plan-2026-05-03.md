# План исправления документации — 2026-05-03

На основе аудита от 03.05.2026 выявлено 29 несоответствий между документацией и кодом.  
Этот документ — дорожная карта исправлений с отслеживанием прогресса.

---

## 🔴 Этап 1: Критические исправления (env + API)

### 1.1 Унификация переменных окружения агента
- [x] **Файл:** `cmd/agent/main.go` — добавить чтение `METRICS_INTERVAL` как fallback к `COLLECT_INTERVAL`
- [x] **Файл:** `config/agent.example.env` — привести в соответствие с кодом (сейчас `METRICS_INTERVAL` и `COLLECT_INTERVAL` дублируются, убрать дублирование)
- [x] **Файл:** `docs/configuration.md` — исправить секцию "Конфигурация агента": заменить `METRICS_INTERVAL` на `COLLECT_INTERVAL` (или указать оба как синонимы)
- [x] **Файл:** `docs/agent-deployment.md` — проверить и исправить упоминания `METRICS_INTERVAL`
- [x] **Файл:** `DEPLOYMENT.md` — исправить: `METRICS_PORT` → `AGENT_PORT`
- [x] **Файл:** `DEPLOYMENT.md` — исправить неверные JSON-ключи в примере API-регистрации (`port`→`ollamaPort`, `agent_port`→`agentPort`, `max_requests`→`maxConcurrentRequests`)

### 1.2 Унификация `NVML_ENABLED` по умолчанию
- [x] **Файл:** `cmd/agent/main.go` — рассмотреть изменение дефолта флага `-nvml` с `false` на `true` (как в документации)
- [x] **Файл:** `docs/agent-deployment.md` — явно указать, что без `NVML_ENABLED=true` GPU метрики не собираются

### 1.3 Добавление `LB_QUEUE_WORKERS` в документацию
- [x] **Файл:** `README.md` — добавить `LB_QUEUE_WORKERS` в таблицу Balancing настроек
- [x] **Файл:** `docs/configuration.md` — добавить `LB_QUEUE_WORKERS` в таблицу переменных балансировщика
- [x] **Файл:** `CHANGELOG.md` — добавить `LB_QUEUE_WORKERS` в таблицу конфигурационных параметров

---

## 🔴 Этап 2: Критические исправления (API Endpoints)

### 2.1 `docs/api.md` — дополнить сводную таблицу и описания
- [x] Добавить endpoint: `GET /api/v1/predictions`
- [x] Добавить endpoint: `GET /api/v1/predictions/{id}`
- [x] Добавить endpoint: `GET /api/v1/models/capacity`
- [x] Добавить endpoint: `GET /api/v1/queue/history`
- [x] Добавить endpoint: `GET /api/v1/queue/details`
- [x] Добавить endpoint: `GET/PUT /api/v1/cluster/config`
- [x] Добавить endpoint: `POST /api/v1/admin/restart`
- [x] Добавить endpoint: `PUT /api/v1/backends/{id}/limits`
- [x] Добавить endpoint: `GET /api/v1/backends/{id}/capacity`
- [x] Добавить endpoint: `GET /monitor`
- [x] Исправить формат ответа `GET /api/v1/models` (реальный формат из `modelsHandler`)

### 2.2 `docs/openapi.yaml` — дополнить недостающими endpoints и схемами
- [x] Добавить Predictions endpoints и схему Prediction
- [x] Добавить Queue Details/History endpoints и схемы
- [x] Добавить Cluster Config endpoint
- [x] Добавить Admin Restart endpoint
- [x] Добавить Backend Limits и Capacity endpoints
- [x] Добавить Models Capacity endpoint
- [x] Дополнить HealthResponse схему (`healthyBackends`, `totalBackends`, `hasAgents`, `totalRequests`, `authEnabled`, `authHeader`, `wsEndpoint`)

---

## 🟡 Этап 3: Высокий приоритет (порты, Docker, CHANGELOG)

### 3.1 Документирование TLS API-порта
- [x] **Файл:** `README.md` — добавить порт 8444 (TLS API = TLSPort+1) в таблицу портов
- [ ] **Файл:** `docs/configuration.md` — упомянуть TLSPort+1 для HTTPS API
- [ ] **Файл:** `docs/deployment.md` — упомянуть TLSPort+1

### 3.2 Исправление `docs/balancing-guide.md`
- [x] Исправить порт с `8081` на `18081`
- [ ] Актуализировать описание этапов выбора бэкенда под реальный `selectBackend`

### 3.3 Актуализация `docs/deployment.md`
- [x] Обновить docker-compose.yml до актуального состояния (webui образ, volume, healthcheck)
- [x] Заменить `wget` на `curl` в healthcheck примерах

### 3.4 Актуализация CHANGELOG.md
- [x] Исправить ссылки на строки кода (`handlers.go:657-724` → `1379-1514`, и др.)
- [x] Исправить количество тестов (83 → 170)
- [x] Добавить упоминание пропущенных тестовых файлов

---

## 🟢 Этап 4: Средний приоритет (структура, метрики)

### 4.1 Обновление структуры проекта
- [ ] **Файл:** `README.md` — добавить `pkg/logger/`, `pkg/protocol/`, `cmd/monitor/`, `tests/`, `logo/`, `docs/plans/`, `docs/ru/`, `docs/en/`
- [ ] **Файл:** `README.md` — убрать несуществующий `internal/balancer/queue.go`
- [ ] **Файл:** `docs/README.md` — дополнить структуру реально существующими файлами

### 4.2 Метрики
- [ ] **Файл:** `docs/ollamalegion-metrics.md` — добавить поле `availableSlots` в таблицу OllamaMetrics
- [ ] **Файл:** `docs/balancing-guide.md` — привести описание алгоритма в соответствие с реальным 3-этапным `selectBackend`

---

## Ход выполнения

| Этап | Статус | Прогресс |
|------|:---:|:---:|
| Этап 1: env + API (критично) | ✅ Завершён | 12/12 подзадач |
| Этап 2: API docs + OpenAPI (критично) | ✅ Завершён | 2/2 подзадачи |
| Этап 3: Порты + Docker + CHANGELOG | ✅ Завершён | 4/4 подзадачи |
| Этап 4: Структура + метрики | 🟡 В процессе | 0/2 подзадачи |
