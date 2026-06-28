# Session F — WebUI: UI/UX quick wins (Q3 W3-4 post-final)

> **Branch:** `feature/q3-session-f`
> **Date:** 2026-06-28
> **Status (обновлено 2026-06-28 15:23 MSK):** 🟢 F.0a DONE, 🟢 F.0b DONE, 🟢 F.1 (= F.α) DONE, 🟢 F.2 (live tail) DONE, 🟢 F.3 (health UI = F.β + F.γ) DONE, 🟢 F.4 (i18n) DONE.
> **HEAD:** TBD на ветке `feature/q3-session-f`.
> **Приоритет:** 🟡 P1 (последние UI/UX gap'ы из roadmap section 5)

## Прогресс по коммитам

| Sub-task | Коммиты | Статус |
|---|---|---|
| **F.0a** EOF diagnostics (root cause из bundled-теста) | `0a76724` | ✅ DONE |
| **F.0b** recover middleware (panic recovery в cppworker) | `5047239` | ✅ DONE |
| **F.1** Notifications — backend (SSE `/api/v1/events` + EventBus) | `873c10d` | ✅ DONE |
| **F.1** Notifications — frontend (bell icon + dropdown + i18n) | `1bc1bf1` | ✅ DONE |
| **F.2** Live tail Logs — backend (`/ws/logs`) | `d4375f1` | ✅ DONE |
| **F.2** Live tail Logs — frontend (`logs-stream.js`) | `20bb65d` | ✅ DONE |
| **F.3** Health-aggregator (HealthChecker + SSE + transport EOF) | в коммитах F.1+F.2 | ✅ DONE |
| **F.3** Health endpoint `/api/v1/health/detailed` (backend) | `006ae0c` | ✅ DONE |
| **F.3** Health UI `/health` страница (frontend) | `0543704` | ✅ DONE |
| **F.4** i18n финализация (EN/RU баланс) | TBD (HEAD) | ✅ DONE |

---

## Контекст

В Q3 W3-4 (Sessions 13-19 + A-E, 2026-06-26 — 2026-06-28) **полностью закрыты** все gap'ы
Models tab (Filter/Search, Sort, Pull progress UI, Model profiles UI, Model details panel,
Dashboard "Loaded models" counter, Bulk operations, Export logs to CSV). Запись в
`plans/README.md` подтверждает: «**Roadmap to 1.0 — Q3 W3-4 (Models tab gaps) ЗАКРЫТ полностью**».

Однако в roadmap остаются 4 нереализованных UI/UX фичи из **section 5** (UI/UX
improvements, low-hanging fruit). Они не блокируют 1.0 release, но заметно улучшают
production-ready UX для Cline/OpenWebUI/Roo Code операторов.

| Sub-task | Источник | Оценка | Приоритет |
|---|---|---:|---|
| **F.1** Notification system (toast) | roadmap 5.4 | 3 ч | 🟡 P1 |
| **F.2** Live tail Logs через WebSocket | roadmap 5.3 | 4-5 ч | 🟡 P1 |
| **F.3** Health-check UI (отдельная страница) | roadmap 5.7 | 6-8 ч | 🟢 P2 |
| **F.4** i18n финализация (EN/RU баланс) | roadmap 5.5 | 1 ч | 🟢 P2 |

**Итого:** 14-17 ч. Можно разбить на 2-3 под-сессии (F.1+F.4 за один заход).

---

## Цель

Реализовать 4 финальных UI/UX gap'а из roadmap section 5, чтобы **WebUI был готов к 1.0
release** без известных TODO-фич для production-операторов.

---

## F.1 — Notification system (toast) для критических событий (3 ч)

### Контекст

Сейчас в WebUI есть базовый `showToast(level, message)` для inline-ошибок (например,
после failed bulk operation в Session A). Но нет **централизованной системы** для
критических backend-driven событий:
- Backend down → user не видит уведомления, пока не откроет Dashboard.
- Model load failed → user кликает на модель, ждёт 30 сек, ничего не происходит.
- OOM/n_ctx auto-reload failed → пользователь Cline получает HTTP 413 молча.

### Решение

**Backend** (новый endpoint `GET /api/v1/events`):
- `internal/api/handlers_events.go` (новый, ~150 LOC) — Server-Sent Events (SSE)
  с ring-buffer последних 100 событий + live-stream для новых.
- Хранилище: `internal/api/events_buffer.go` (новый, ~50 LOC) — thread-safe ring buffer.
- Источники событий:
  - Backend health changes (через `internal/balancer/state_watcher.go` poll, period 5s).
  - Model operation results (load/unload/pull/copy/delete — через существующий `OperationRegistry`).
  - n_ctx auto-reload failed (через `internal/balancer/nctx_reload.go:OnFailure` callback).
- Тип события: `{ts, severity, source, message, backendId?, modelName?, httpStatus?}`.
- Auth: требуется `X-API-Token`.

**Frontend** (централизованная NotificationCenter):
- `webui/js/modules/notifications.js` (новый, ~200 LOC) — singleton `window.notifications`:
  - `notifications.subscribe(eventType, callback)`.
  - `notifications.show({severity, message, dismissible})` — обёртка над `showToast`
    с persistence в localStorage (последние 50 событий).
  - Bell icon в header (рядом с theme toggle) с badge counter (непрочитанные события).
  - Dropdown panel со списком событий (severity icon + ts + source + message),
    filter по severity, "Mark all as read" / "Clear".
  - Broadcast событие `notifications:new` для подписки из других модулей.
- `webui/js/modules/api.js` — новый namespace `Api.events.subscribe(callback)` —
  EventSource wrapper с reconnect on close.
- `webui/index.html` — добавить bell icon, dropdown panel, i18n-ключи.
- `webui/js/app.js` — init в `DOMContentLoaded`, при смене языка — refresh dropdown.

### Acceptance criteria

1. `GET /api/v1/events` (SSE) возвращает `Content-Type: text/event-stream` и прислает
   последние события при reconnect.
2. Backend health change (healthy → unhealthy) генерирует событие
   `{severity: "warning", source: "backend", message: "cppworker-gpu is unhealthy"}`.
3. Model load failure → событие `{severity: "error", source: "model", ...}`.
4. WebUI bell icon показывает badge с количеством непрочитанных событий.
5. Dropdown отображает список с severity icons, timestamps, source badges.
6. При клике на событие → toast с подробностями.
7. Reconnect после потери SSE-соединения в течение 5 сек (exponential backoff).
8. 4 unit-теста для `events_buffer.go` (enqueue, overflow, drain).
9. 2 integration-теста для SSE handler (auth, headers, формат).

---

## F.2 — Live tail в Logs через WebSocket (4-5 ч)

### Контекст

Сейчас Logs tab (`webui/index.html` Logs panel) использует polling:
`refreshLogs()` опрашивает `data/state.json` каждые 2-5 сек. Это:
- Создаёт нагрузку на filesystem при больших логах (>100 MB).
- Имеет задержку 2-5 сек до появления новой записи.
- Не масштабируется для multi-instance deployments.

### Решение

**Backend** (новый endpoint `GET /ws/logs`):
- `internal/api/handlers_logs_ws.go` (новый, ~150 LOC) — WebSocket upgrader на
  gorilla/websocket (уже есть в зависимостях для `/ws/metrics`).
- При подключении:
  1. Отправить последние 100 записей из `data/state.json` (initial backlog).
  2. Подписаться на `LogBroker` (новый internal/logger broker).
- `internal/logger/broker.go` (новый, ~80 LOC) — pub/sub для log-записей:
  - `Subscribe() <-chan LogEntry`.
  - `Publish(entry LogEntry)` — вызывается из существующих логгеров.
- Frontmatter формат: `{ts, level, source, message}` (JSON), close msg `{type:"ping"}` каждые 30s.

**Frontend** (WebSocket client):
- `webui/js/modules/logs-stream.js` (новый, ~180 LOC):
  - `window.logsStream` singleton с методами `start()`, `stop()`, `subscribe()`.
  - `connect()` с reconnect+backoff (1s → 30s).
  - Ring buffer в памяти (max 500 записей) + persistence в localStorage (для reload).
- `webui/js/app.js` — в Logs tab использовать `logsStream` вместо polling.
  - При init: загрузить последние 500 из localStorage (если есть), затем connect().
  - При получении новой записи → prepend в DOM + scroll to top.
  - При смене level filter → просто скрыть/показать строки (без refetch).

### Acceptance criteria

1. `GET /ws/logs` (with `Upgrade: websocket`) устанавливает соединение и шлёт
   initial backlog (последние 100 записей).
2. При появлении новой log-записи через `Publish()` → broadcast всем подписчикам.
3. WebUI Logs tab показывает записи в real-time (latency < 500ms).
4. При потере WebSocket → reconnect с exponential backoff.
5. При переключении на другую вкладку WebSocket остаётся открытым (но не
   потребляет CPU — просто пустой цикл).
6. При F5 — reconnect, загрузить из localStorage (если есть) + затем live.
7. 6 unit-теста для `broker.go` (subscribe/publish/unsubscribe/buffer-overflow).
8. 2 integration-теста для WebSocket handler (handshake, формат, close).

---

## F.3 — Health-check UI (отдельная страница) (6-8 ч)

### Контекст

Сейчас статус бэкендов виден частично:
- Dashboard: бейджи Healthy/Unhealthy рядом с именем бэкенда (только общий статус).
- Monitor: per-backend панель с метриками (но только real-time, без history).
- Backend details page: конфиг + metrics.

Нет **единой страницы**, где оператор может быстро увидеть:
- Все бэкенды с их текущим статусом + uptime + last seen.
- Последние 10 ошибок (HTTP 5xx, timeouts, OOM) per backend.
- Версии (balancer, cppworker, ollama, agent).
- Last config reload timestamp.

### Решение

**Backend** (новый endpoint `GET /api/v1/health/detailed`):
- `internal/api/handlers_health.go` (новый, ~250 LOC):
  - Агрегирует:
    - `balancer.Version`, `balancer.Uptime`, `balancer.ConfigReloadedAt`.
    - Per-backend: `{id, type, status, lastSeen, uptime, version, lastErrors[]}`.
    - Системные: `balancer_host`, `go_version`, `os`, `arch`, `goroutines_count`.
  - Возвращает JSON с timestamp + sections.
  - Требует `X-API-Token`.
- `pkg/types/health.go` (новый) — типы: `HealthReport`, `BackendHealth`, `RecentError`.

**Frontend** (отдельная страница `webui/health.html`):
- Self-contained dark-mode панель (по аналогии с `rpc-status.html`, `tp-pipeline.html`).
- Header: balancer version + uptime + last config reload timestamp.
- Sections:
  - **System**: go_version, OS, arch, goroutines, memory usage (через `runtime.MemStats`).
  - **Backends grid**: карточки с id, type (🦙/🦒), status (healthy/unhealthy/offline/draining),
    uptime, last seen, last error (с tooltip), link "View details".
  - **Recent errors**: последние 20 ошибок (HTTP 5xx, panics, OOM, n_ctx auto-reload failed)
    с timestamp, backend, error_code, message preview.
  - **Auto-refresh**: 10s toggle.
- Навигация: добавить link "Health" в header (рядом с "Monitor", "RPC", "TP").
- Кнопка "Refresh now" + кнопка "Export to JSON" (download health-snapshot.json).

### Acceptance criteria

1. `GET /api/v1/health/detailed` возвращает 200 + JSON со всеми секциями.
2. `webui/health.html` открывается без ошибок, показывает все данные.
3. Auto-refresh каждые 10 сек, индикатор последнего обновления.
4. При unhealthy backend — красная карточка с последней ошибкой.
5. "Export to JSON" скачивает файл с текущим snapshot.
6. Header link "Health" активен на новой странице (CSS highlight).
7. 5 unit-тестов для `handlers_health.go` (auth, aggregation, error formatting).
8. 2 integration-теста для endpoint (с mock backends).

---

## F.4 — i18n финализация (EN/RU баланс) (1 ч)

### Контекст

Из `plans/README.md` секция 1.5: «i18n (en.js 918 ключей, ru.js 824 ключа)».
Разрыв **~94 ключа** — ru.js отстаёт. После Sessions 14-19 + A-E разрыв увеличился
(новые ключи для bulk operations, model details, export logs добавлялись преимущественно
в en.js первыми, ru.js догонялся).

### Решение

- Запустить diff-скрипт:
  ```bash
  node scripts/i18n_diff.js webui/js/i18n/en.js webui/js/i18n/ru.js
  ```
  (или grep-based скрипт — список ключей из `en.js` без перевода в `ru.js`).
- Добавить недостающие русские переводы (для ключей, которые реально рендерятся в UI).
- Skip для:
  - Ключи, которые deprecated (например, `models.empty_filtered_old`).
  - Debug-only ключи (`debug.*`).
- Цель: **минимальный разрыв <10 ключей** (только deprecated/debug).
- Не требует новых фич — только синхронизация переводов.

### Acceptance criteria

1. `webui/js/i18n/en.js` имеет N ключей, `ru.js` имеет ≥ N-10 ключей.
2. Diff-скрипт (новый, `scripts/i18n_diff.js`) выводит список missing keys.
3. Все visible UI строки переведены на оба языка.
4. Build OK: `node -c webui/js/i18n/{en,ru}.js` (syntax check).
5. При смене языка в WebUI нет видимых английских строк (manual smoke test).

---

## Файлы для модификации (суммарно)

### Backend (Go)
- `internal/api/handlers_events.go` (новый, F.1)
- `internal/api/events_buffer.go` (новый, F.1)
- `internal/api/handlers_logs_ws.go` (новый, F.2)
- `internal/api/handlers_health.go` (новый, F.3)
- `internal/api/routes.go` (регистрация новых роутов)
- `internal/logger/broker.go` (новый, F.2)
- `pkg/types/health.go` (новый, F.3)

### Frontend (JS/HTML/CSS)
- `webui/js/modules/notifications.js` (новый, F.1)
- `webui/js/modules/api.js` (Api.events namespace, F.1)
- `webui/js/modules/logs-stream.js` (новый, F.2)
- `webui/health.html` (новый, F.3)
- `webui/index.html` (header link "Health", bell icon, dropdown panel)
- `webui/js/app.js` (init notifications, switch polling→WebSocket в Logs)
- `webui/css/components.css` + `data.css` (~150 LOC стили)
- `webui/js/i18n/en.js` + `ru.js` (~30 ключей × 2)

### Scripts
- `scripts/i18n_diff.js` (новый, F.4)

### Tests
- `internal/api/handlers_events_test.go` (F.1, ~50 LOC)
- `internal/api/events_buffer_test.go` (F.1, ~80 LOC)
- `internal/api/handlers_logs_ws_test.go` (F.2, ~100 LOC)
- `internal/logger/broker_test.go` (F.2, ~120 LOC)
- `internal/api/handlers_health_test.go` (F.3, ~150 LOC)
- `webui/js/modules/notifications.test.js` (F.1, optional, ~80 LOC)

---

## Оценка трудозатрат

| Sub-task | Backend | Frontend | Tests | Итого |
|---|---:|---:|---:|---:|
| F.1 Notification system | 1 ч | 1.5 ч | 0.5 ч | **3 ч** |
| F.2 Live tail Logs | 1.5 ч | 2 ч | 1 ч | **4.5 ч** |
| F.3 Health-check UI | 1.5 ч | 4 ч | 1.5 ч | **7 ч** |
| F.4 i18n финализация | — | 1 ч | — | **1 ч** |
| **Итого** | **4 ч** | **8.5 ч** | **3 ч** | **~15.5 ч** |

---

## План работы (чеклист)

### Под-сессия F.α (1 день, ~4 ч): F.1 + F.4
- [ ] Создать ветку `feature/q3-w3-4-post` от `integration/q3-w3-4`.
- [ ] Реализовать `internal/api/events_buffer.go` + tests.
- [ ] Реализовать `internal/api/handlers_events.go` (SSE endpoint).
- [ ] Регистрация роута `/api/v1/events` в `internal/api/routes.go`.
- [ ] Реализовать `webui/js/modules/notifications.js`.
- [ ] Bell icon + dropdown panel в `webui/index.html`.
- [ ] `webui/js/i18n/{en,ru}.js` — ключи `notifications.*` (~10 × 2).
- [ ] Запустить `scripts/i18n_diff.js`, добавить недостающие переводы.
- [ ] Verification: `go build -tags llama_stub ./cmd/balancer/`, `node -c webui/js/...`.
- [ ] Commit + push + PR.

### Под-сессия F.β (1 день, ~5 ч): F.2
- [ ] Создать ветку `feature/live-tail-logs` от `integration/q3-w3-4`.
- [ ] Реализовать `internal/logger/broker.go` + tests.
- [ ] Реализовать `internal/api/handlers_logs_ws.go`.
- [ ] Подписать существующие логгеры на broker (`pkg/logger/logger.go`).
- [ ] Реализовать `webui/js/modules/logs-stream.js`.
- [ ] Заменить polling → WebSocket в `webui/js/app.js:refreshLogs()`.
- [ ] Verification + commit + PR.

### Под-сессия F.γ (1 день, ~7 ч): F.3
- [ ] Создать ветку `feature/health-check-ui` от `integration/q3-w3-4`.
- [ ] Реализовать `internal/api/handlers_health.go` + tests.
- [ ] Регистрация `/api/v1/health/detailed` в `routes.go`.
- [ ] Создать `webui/health.html` (self-contained dark-mode панель).
- [ ] Header link "Health" + i18n ключи `health.*` (~12 × 2).
- [ ] Verification + commit + PR.

---

## Зависимости

- **F.1** — независим, требует `gorilla/websocket` (уже в go.mod).
- **F.2** — независим, требует тот же `gorilla/websocket`.
- **F.3** — независим, чистый HTML+JS без дополнительных deps.
- **F.4** — независим, чисто редакторская работа.

Все 4 sub-task'a можно выполнять **параллельно** в 4 разных ветках и мёрджить независимо.

---

## Связанные документы

- [plans/2026-q3-roadmap.md](2026-q3-roadmap.md) — главный roadmap (section 5).
- [plans/README.md](README.md) — общий статус (Models tab gaps ЗАКРЫТ, остались только UI/UX).
- [plans/2026-q3-production-ready-plan.md](2026-q3-production-ready-plan.md) — следующий этап (rpc_coordinator, virtual_router, real ggml, CI/CD).

---

## Acceptance criteria (Session F целиком)

1. F.1: bell icon с badge + dropdown panel + SSE reconnect работают.
2. F.2: Live tail Logs через WebSocket, latency < 500ms.
3. F.3: `webui/health.html` показывает все секции, auto-refresh 10s.
4. F.4: i18n баланс EN ≈ RU (разрыв <10 ключей).
5. Build OK: `go build -tags llama_stub ./cmd/balancer/` + `node -c webui/js/...`.
6. Тесты: ~17 новых тестов PASS, 0 regressions.
7. CHANGELOG.md — подсекция `### Added (Session F — UI/UX quick wins)`.
8. Все 4 PR смержены в `integration/q3-w3-4`.

**После Session F:** WebUI полностью готов к 1.0 release. Следующий этап — [production-ready план](2026-q3-production-ready-plan.md).

---

**Подготовлено:** 2026-06-28 (после Session E, commit `dc49912`)
**Ветка:** `integration/q3-w3-4` → новая ветка `feature/q3-w3-4-post`