# Дорожная карта рефакторинга OllamaLegion

> Документ-ориентир для продолжения модульного рефакторинга проекта.
> Последнее обновление: 2026-05-06

---

## Текущее состояние

### Уже выделенные модули (15 файлов)

| Файл | Строки | Содержимое | Статус |
|------|--------|-----------|--------|
| `session_manager.go` | 147 | SessionManager (TTL + cleanup) | ✅ Готово |
| `metrics_manager.go` | 20 | Хранение метрик бэкендов | ✅ Готово |
| `queue_manager.go` | 347 | Очередь + workers + история | ✅ Готово |
| `candidate.go` | 176 | Candidate groups (P1-P4) | ✅ Готово |
| `scoring.go` | 295 | Мультифакторный scoring | ✅ Готово |
| `streaming.go` | 122 | SSE streaming + heartbeat | ✅ Готово |
| `eventbus.go` | 68 | Pub/sub событий кластера | ✅ Готово |
| `client.go` | 194 | Fingerprint, session ID, real IP | ✅ Готово |
| `state.go` | 133 | Save/Load state.json | ✅ Готово |
| `routes.go` (api) | 80 | Регистрация HTTP маршрутов | ✅ Готово |
| `backend_state.go` | 25 | BackendState struct + contextKey | ✅ Готово |
| `router.go` | 50 | HTTP routing (health, Ollama API) | ✅ Готово |
| `session_handler.go` | 100 | Session stickiness + rebalance | ✅ Готово |
| `slot_handler.go` | 80 | Slot acquisition + retry + fallback | ✅ Готово |

### Размер proxy.go

- **Было:** ~1994 строк (до Фазы 3)
- **Фаза 3:** ~650 строк (после выноса backend_selector, slot_manager, proxy_request)
- **Фаза 4:** ~280 строк (после выноса cluster_state, agent_manager)
- **Фаза 5:** ~195 строк (после выноса router, session_handler, slot_handler, backend_state)
- **Фактически (2026-05-13):** ~720 строк (после добавления RPC модулей, initRpcModules, background controllers)

> Примечание: proxy.go содержит struct, NewProxy, ServeHTTP-оркестратор, extractModel, UpdateMetrics, queueRequest, initRpcModules, RPC Model Distribution модули.

### Размер collector.go (agent)

- **Было:** ~1264 строк (до 2026-05-13)
- **Фактически (2026-05-13):** ~280 строк (ядро агента)
- Вынесено в: `collector_network.go`, `collector_register.go`, `collector_gpu.go`, `collector_health.go`, `collector_ollama.go`, `collector_system.go`

### Размер ollama_router.go (balancer)

- **Было:** ~608 строк (до 2026-05-13)
- **Фактически (2026-05-13):** ~224 строк (ядро маршрутизатора)
- Вынесено в: `ollama_router_tags.go` (агрегация tags/ps/version), `ollama_router_admin.go` (CRUD: show/create/pull/delete/copy/push)

### Размер тестовых файлов

- **proxy_ollama_test.go:** 1375 → ~680 строк (мок вынесен в `proxy_mock_test.go`)
- **proxy_reliability_test.go:** 1186 строк — уже использует общий мок
- **load_scenarios_test.go:** 1299 строк — использует ExpandedMockServer

---

## Что осталось в proxy.go (58 методов)

### 1. HTTP Handler — ServeHTTP (~250 строк) 🔴 ВЫСОКИЙ ПРИОРИТЕТ

**Что вынести:**
- `ServeHTTP` — главный HTTP handler, содержит:
  - Routing логику (`/health`, ollamaRouter)
  - Session stickiness + rebalance логику
  - Slot acquisition + retry
  - Error handling + fallback

**Куда вынести:**
- `router.go` — HTTP routing (/health, ollamaRouter.Route)
- `session_handler.go` — Session stickiness + rebalance
- `slot_handler.go` — tryAcquireSlot + retry loop

**Сложность:** Средняя. ServeHTTP тесно связан с sessionMgr, queueMgr, backends.

---

### 2. HTTP Proxy — proxyRequest (~120 строк) 🔴 ВЫСОКИЙ ПРИОРИТЕТ

**Что вынести:**
- `proxyRequest` — проксирование запроса к бэкенду
- `isStreamingRequest` — определение streaming запроса
- `isStreamingResponse` — определение streaming ответа
- `recordRequest` — запись таймстемпа для RPS

**Куда вынести:**
- `proxy_request.go` — HTTP проксирование + streaming detection

**Сложность:** Низкая. Эти методы логически связаны и слабо зависят от остального кода.

---

### 3. Backend Selection (~300 строк) 🟡 СРЕДНИЙ ПРИОРИТЕТ

**Что вынести:**
- `selectBackend` — 5-этапный алгоритм выбора
- `selectBackendExcluding` — выбор с исключением
- `selectByResources` / `selectByResourcesExcluding` — выбор по ресурсам
- `findBackendWithModel` / `findBackendWithModelExcluding` — поиск по модели
- `findLessLoadedBackendWithModel` / `findLessLoadedBackendAny` — rebalance
- `findWarmingBackendForModelUnsafe` — поиск warming бэкенда
- `findFreeBackendForModelUnsafe` — поиск свободного бэкенда
- `selectFreeBackendAny` — выбор любого свободного
- `checkModelReadyUnsafe` — проверка готовности модели
- `modelIsRunningOnBackendUnsafe` — проверка running модели

**Куда вынести:**
- `backend_selector.go` — Весь backend selection

**Сложность:** Средняя. Много методов, но логически связаны.

---

### 4. Slot Management (~80 строк) 🟡 СРЕДНИЙ ПРИОРИТЕТ

**Что вынести:**
- `tryAcquireSlot` — атомарный захват слота
- `releaseSlot` — освобождение слота
- `checkResourceLimits` — проверка ресурсных лимитов

**Куда вынести:**
- `slot_manager.go` — Slot management + resource limits

**Сложность:** Низкая. Самостоятельная логика.

---

### 5. Backend CRUD (~200 строк) 🟡 СРЕДНИЙ ПРИОРИТЕТ

**Что вынести:**
- `AddBackend` — добавление бэкенда
- `RemoveBackend` — удаление с graceful drain
- `UpdateBackend` — обновление бэкенда
- `GetBackend` / `GetAllBackends` — получение
- `BackendExists` — проверка существования
- `UpdateBackendStatus` — обновление статуса
- `UpdateBackendAgentStatus` — обновление флага агента
- `UpdateBackendLimits` — обновление runtime лимитов
- `scheduleRecoveryCheck` — планирование проверки

**Куда вынести:**
- `backend_registry.go` — Backend CRUD операции

**Сложность:** Низкая. Самостоятельная логика.

---

### 6. Cluster State (~100 строк) 🟡 СРЕДНИЙ ПРИОРИТЕТ

**Что вынести:**
- `GetClusterState` — формирование состояния кластера
- `GetQueueStats` — статистика очереди
- `GetQueueHistory` — история запросов
- `GetQueuePendingRequests` / `GetQueueProcessingRequests` / `GetDirectProcessingRequests` — запросы очереди

**Куда вынести:**
- `cluster_state.go` — Формирование состояния кластера
- Или оставить в `queue_manager.go` / `metrics_manager.go` как обёртки

**Сложность:** Средняя. GetClusterState — большой метод с доступом к metricsMgr.

---

### 7. Session Access (~30 строк) 🟢 НИЗКИЙ ПРИОРИТЕТ

**Что вынести:**
- `GetSessions` — делегат к sessionMgr
- `DeleteSession` — делегат к sessionMgr
- `ClearSessions` — делегат к sessionMgr

**Куда вынести:**
- Уже делегаты, минимальная выгода от выноса.

**Сложность:** Низкая. Можно оставить в proxy.go как тонкие обёртки.

---

### 8. Agent Management (~30 строк) 🟢 НИЗКИЙ ПРИОРИТЕТ

**Что вынести:**
- `StartAgentTimeoutChecker` — фоновая проверка таймаута
- `StopAgentTimeoutChecker` — заглушка

**Куда вынести:**
- `agent_manager.go` — Agent timeout management

**Сложность:** Низкая. Маленький компонент.

---

### 9. Test Helpers (~150 строк) 🟢 НИЗКИЙ ПРИОРИТЕТ

**Что вынести:**
- `ExpandCandidates` — обёртка для тестов
- `DispatchWithModelLoad` — обёртка для тестов
- `SetBackendMetrics` — установка метрик для тестов
- `SelectBackend` — обёртка для тестов
- `GetBackendState` — доступ к BackendState для тестов
- `GetWarmingUpModels` — получение warming моделей для тестов
- `GetConfig` — получение конфига для тестов
- `SetWarmingUpModel` — установка warming модели для тестов
- `StopQueue` — остановка QueueManager
- `StopSessionManager` — остановка SessionManager
- `UpdateBackendLimits` — обновление лимитов

**Куда вынести:**
- `proxy_test_helpers.go` — Все тест-хелперы (только компилируется при тестах)

**Сложность:** Низкая. Только для тестов.

---

### 10. Prediction (~20 строк) 🟢 НИЗКИЙ ПРИОРИТЕТ

**Что вынести:**
- `GetPrediction` — получение прогноза для бэкенда

**Куда вынести:**
- Уже делегат к predictor. Минимальная выгода.

---

### 11. Queue Integration (~50 строк) 🟢 НИЗКИЙ ПРИОРИТЕТ

**Что вынести:**
- `queueRequest` — постановка в очередь
- `GetQueuePendingRequests` / `GetQueueProcessingRequests` / `GetDirectProcessingRequests`
- `GetQueueHistory` / `GetQueueStats`

**Куда вынести:**
- Уже делегаты к queueMgr. Минимальная выгода.

---

## Рекомендуемый порядок (по приоритету)

### ✅ Фаза 3 (высокий приоритет) — завершена 2026-05-07

| Файл | Строки | Содержимое | Статус |
|------|--------|-----------|--------|
| `backend_selector.go` | ~470 | selectBackend + 12 методов выбора | ✅ Готово |
| `slot_manager.go` | ~105 | tryAcquireSlot + releaseSlot + checkResourceLimits | ✅ Готово |
| `proxy_request.go` | ~185 | proxyRequest + isStreamingRequest/Response + recordRequest + warmupModel | ✅ Готово |
| `backend_registry.go` | ~185 | AddBackend + RemoveBackend + UpdateBackend + GetBackend + GetAllBackends + BackendExists + UpdateBackendStatus + UpdateBackendAgentStatus + scheduleRecoveryCheck + UpdateBackendLimits + Restart | ✅ Готово |
| `proxy_test_helpers.go` | ~90 | ExpandCandidates + DispatchWithModelLoad + SetBackendMetrics + SelectBackend + GetBackendState + GetWarmingUpModels + GetConfig + SetWarmingUpModel + StopQueue + StopSessionManager + StopAgentTimeoutChecker | ✅ Готово |

**proxy.go после Фазы 3:** ~650 строк (было 1994). Остались: ServeHTTP, extractModel, UpdateMetrics, StartAgentTimeoutChecker, GetClusterState, queueRequest + queue обёртки, GetPrediction, eventBus делегаты.

### ✅ Фаза 4 (средний приоритет) — завершена 2026-05-07

| Файл | Строки | Содержимое | Статус |
|------|--------|-----------|--------|
| `cluster_state.go` | ~350 | GetClusterState + GetQueueStats + GetQueueHistory + GetQueuePendingRequests + GetQueueProcessingRequests + GetDirectProcessingRequests + GetDispatchStats + ResetDispatchCounters + GetSessions + DeleteSession + ClearSessions + GetPrediction + SubscribeEvents + UnsubscribeEvents + PublishEvent + SetUnloadScheduler + SetWeightTuner + GetUnloadScheduler + GetWeightTuner | ✅ Готово |
| `agent_manager.go` | ~55 | StartAgentTimeoutChecker + StopAgentTimeoutChecker + agentTimeoutChecker struct | ✅ Готово |

**proxy.go после Фазы 4:** ~280 строк (было 650). Остались: ServeHTTP, extractModel, UpdateMetrics, queueRequest, StartAgentTimeoutChecker (делегат), SetQueueManagerProxy.

### ✅ Фаза 5 (завершена 2026-05-07)

| Файл | Строки | Содержимое | Статус |
|------|--------|-----------|--------|
| `backend_state.go` | ~25 | BackendState struct + contextKey | ✅ Готово |
| `router.go` | ~50 | HTTP routing (health, Ollama API) | ✅ Готово |
| `session_handler.go` | ~100 | Session stickiness + rebalance | ✅ Готово |
| `slot_handler.go` | ~80 | Slot acquisition + retry + fallback | ✅ Готово |

**proxy.go после Фазы 5:** ~195 строк (было ~280). ServeHTTP сокращён с ~170 до ~25 строк — чистый оркестратор.

---

## История изменений

| Дата | Изменение |
|------|-----------|
| 2026-04-28 | Аудит кода, создан `audit-report-2026-04-28.md` |
| 2026-04-29 | Создан `refactoring-plan-2026-04-29.md` |
| 2026-05-02 | Фаза 1-2 рефакторинга proxy.go |
| 2026-05-03 | Создан консолидированный план `consolidated-plan-2026-05-03.md` |
| 2026-05-06 | Фаза 3: выделены `backend_selector.go`, `slot_manager.go`, `proxy_request.go`, `backend_registry.go`, `proxy_test_helpers.go`. proxy.go сокращён с 1994 до ~650 строк |
| 2026-05-07 | Фаза 4: созданы `cluster_state.go` (~350 строк) и `agent_manager.go` (~55 строк). Удалены дубли из proxy.go. proxy.go сокращён с ~650 до ~280 строк. Все тесты проходят |
| 2026-05-07 | **Фаза 5**: выделены `backend_state.go`, `router.go`, `session_handler.go`, `slot_handler.go`. proxy.go сокращён с ~280 до ~195 строк. ServeHTTP — тонкий оркестратор. `go build ./...` и `go test ./...` — все PASS |
| 2026-05-13 | **Фаза 6**: разбит `collector.go` (1264→280 строк) на 6 модулей. Разбит `ollama_router.go` (608→224 строк) на 3 модуля. Разбит `proxy_ollama_test.go` (1375→680 строк) — мок вынесен в `proxy_mock_test.go`. Созданы тесты для `UnloadScheduler` (6) и `AdaptiveWeightTuner` (12). Исправлены баги: race condition в `cluster_state.go`, паника при двойном close канала в `unload_scheduler.go`/`weight_tuner.go`. Все тесты проходят |

---

## Итоговая структура internal/balancer/

| Файл | Размер | Содержимое |
|------|--------|------------|
| `proxy.go` | ~720 строк | Struct, NewProxy, ServeHTTP-оркестратор, extractModel, UpdateMetrics, queueRequest, initRpcModules, RPC модули |
| `queue_dispatch.go` | ~234 строк | Dispatch с 4 приоритетами + waitForModelReady + canAcceptRequest |
| `backend_state.go` | ~25 строк | BackendState struct, contextKey константы |
| `router.go` | ~50 строк | HTTP routing (health, Ollama API) |
| `session_handler.go` | ~100 строк | Session stickiness + rebalance logic |
| `slot_handler.go` | ~80 строк | Slot acquisition + retry + fallback execution |
| `proxy_request.go` | ~185 строк | HTTP проксирование |
| `backend_selector.go` | ~470 строк | Backend selection алгоритмы |
| `slot_manager.go` | ~105 строк | Slot management |
| `backend_registry.go` | ~185 строк | Backend CRUD |
| `cluster_state.go` | ~350 строк | Cluster state + обёртки |
| `agent_manager.go` | ~55 строк | Agent timeout checker |
| `proxy_test_helpers.go` | ~90 строк | Test helpers |
| `ollama_router_tags.go` | ~280 строк | Агрегация /api/tags, /api/ps, /api/version |
| `ollama_router_admin.go` | ~180 строк | Targeted routing: show, create, pull, delete, copy, push |

### Итоговая структура internal/agent/

| Файл | Размер | Содержимое |
|------|--------|------------|
| `collector.go` | ~280 строк | Ядро агента: NewAgent, Start, Stop, collectLoop, sendHeartbeat |
| `collector_network.go` | ~60 строк | getPublicHost, getOutboundIP, extractOllamaPort, getOllamaBaseURL |
| `collector_register.go` | ~90 строк | register, healthCheckOllama |
| `collector_gpu.go` | ~50 строк | collectGPUInfo, collectGPUMetrics |
| `collector_health.go` | ~80 строк | startHealthServer, appendLog |
| `collector_ollama.go` | ~340 строк | collectOllamaMetrics, fetchOllamaTags, getRunningModels, getOllamaVersion, getOllamaStats |
| `collector_system.go` | ~30 строк | collectSystemMetrics (делегат к platform-specific) |
| `gpu_common.go` | ~112 строк | executeNvidiaSmi, parseNvidiaSmiOutput |
| `ollama_config.go` | ~117 строк | applyOllamaConfig, restartOllamaServer |
| `ollama_context.go` | ~568 строк | collectOllamaRuntimeFlags, getModelContext, calculateContextMemory, calculateBackendCapacity |
| `system.go` | ~395 строк | Platform-specific system metrics (Linux/Darwin) |
| `system_windows.go` | ~155 строк | Platform-specific system metrics (Windows) |

---

## Выполненные задачи

- [x] **BackendState** вынесен в `backend_state.go`
- [x] **HTTP routing** вынесен в `router.go`
- [x] **Session stickiness + rebalance** вынесены в `session_handler.go`
- [x] **Slot acquisition + retry + fallback** вынесены в `slot_handler.go`
- [x] **ServeHTTP** превращён в тонкий оркестратор (~25 строк)
- [x] **Все тесты** проходят: `go test ./internal/balancer`, `go test ./tests`
- [x] **Сборка** без ошибок: `go build ./...`
- [x] **Agent collector** разбит на 6 модулей
- [x] **OllamaRouter** разбит на 3 модуля
- [x] **Тесты UnloadScheduler** — 6 тестов
- [x] **Тесты AdaptiveWeightTuner** — 12 тестов
- [x] **proxy_mock_test.go** — общий мок для тестов

---

## Чек-лист для каждого рефакторинга

Перед выносом нового компонента:

- [x] Прочитать все методы, которые планируется вынести
- [x] Определить зависимости (какие поля Proxy используются)
- [x] Создать новый файл с выделенными методами
- [x] Удалить дубли из proxy.go
- [x] Обновить импорты
- [x] `go build ./...` — должно компилироваться
- [x] `go test ./internal/balancer ./tests -count=1` — должны проходить
- [x] Обновить `docs/refactoring-roadmap.md` с новым файлом
