# OllamaLegion — Roadmap (живой документ)

> **Дата обновления:** 2026-06-28
> **Назначение:** единственный источник правды по реализованному и оставшемуся в проекте OllamaLegion.
> Все устаревшие/завершённые планы — в `plans/archive/`.
> **HEAD:** `006ae0c` на ветке `integration/q3-w3-4` (в процессе Session F).

---

## 0. Планы в работе (WIP)

| План | Файл | Сессия | Статус |
|------|------|--------|--------|
| [Session F — UI/UX quick wins (5.3, 5.4, 5.5, 5.7)](2026-q3-session-f-quick-wins.md) | `plans/2026-q3-session-f-quick-wins.md` | Session F (июнь 2026) | 🔄 F.0a/b/α/β/γ DONE, F.4 ⏳ |
| [Production-ready (3.2, 3.3, B8.7, 7.1)](2026-q3-production-ready-plan.md) | `plans/2026-q3-production-ready-plan.md` | Sessions G+ (август-сентябрь 2026) | ⏳ WIP |

Все планы B1–B8 реализованы (см. Roadmap ниже).
Все секции roadmap до раздела 2.2 (Models tab gaps) **полностью DONE** в Sessions 13-19 + A-E.
F-сессия (5.3/5.4/5.5/5.7) — F.0a/b/α/β/γ закрыты, остаётся F.4 (i18n EN/RU баланс).

---

## 1. Реализовано (подтверждено кодом и тестами)

### 1.1 Backend Type Isolation (commit `affb4e8`)

| Компонент | Файл | Тест |
|---|---|---|
| `selectBackend(model, bt)` — фильтрация по типу | `internal/balancer/backend_selector.go` | `TestSelectBackend_FiltersByType_OllamaOnly` |
| `expandCandidates(model, allowedTypes)` | `internal/balancer/candidate.go` | `TestExpandCandidates_FiltersByType` |
| `proxyRequest` — проверка `IsModeCompatibleWithBackendType` | `internal/balancer/proxy_request.go` | `TestServeHTTP_LlamaCppRequest_RoutedToLlamaCppBackend` |
| `determineRequestBackendType(r)` | `internal/balancer/proxy.go` | `DetermineRequestBackendTypeForTest` |
| `getBackendPort()` — без fallback для llama_cpp | `internal/balancer/backend_state.go` | (unit) |
| CRUD валидация при `addBackend`/`updateBackend` | `internal/api/handlers_backends.go` | (integration) |
| WebUI бейджи 🦙/🦒 в Dashboard/Backends/Monitor | `webui/js/modules/renderers.js`, `monitor/ui-renderer.js`, `monitor/backend-type-badges.js` | manual |
| BackendTypeFilter + `toggleAgentsTab()` для llama_cpp | `webui/js/modules/backend-type-filter.js` | manual |

### 1.2 CppWorker: Ollama-совместимость + защита от двойной загрузки

| Endpoint / фича | Файл | Статус |
|---|---|---|
| `/api/generate`, `/api/ollama/generate` — `runGenerateCore` | `cmd/cppworker/handlers_generate.go`, `main.go` | ✅ |
| `/api/chat` — роли `system`/`user`/`assistant`/`tool`, chat template из GGUF | `cmd/cppworker/handlers_chat.go` | ✅ |
| `/api/embeddings` — JSON parse | `c/bridge/bridge.go` (`GetEmbeddings`) | ✅ |
| `/api/tags`, `/api/show`, `/api/copy`, `/api/create`, `/api/pull` (hf:), `/api/push` (HTTP 501), `/api/delete`, `/api/version` | `internal/balancer/llamacpp_handlers_*.go` | ✅ |
| `handleLoadModel` — `status: "already_loaded"` через `sameLoadOptions` + `GetModel` | `cmd/cppworker/handlers_model.go:97-113` | ✅ |
| `applyCppCtxHeader` — X-Cpp-Ctx upper-limit semantics | `cmd/cppworker/main.go` | ✅ |
| `applyKeepAlive` — keep_alive == "0" → UnloadModel | `cmd/cppworker/handlers_generate.go` | ✅ |
| `tryRamFallbackReload` — reload в RAM если VRAM не хватает | `cmd/cppworker/inference.go:491` | ✅ |
| RAM fallback disabled для tools (`hasTools=true`) → HTTP 413 | `cmd/cppworker/inference.go:tryRamFallbackReload` + `utils.go:writeReloadDisabledForToolsResponse` | ✅ |
| `ReloadLoopLimitError` при `ramFallbackAttempts >= 3`/60 сек | `cmd/cppworker/inference.go` | ✅ |
| Tools/tool_calls detection (Hermes/Mistral/Llama-python) | `internal/balancer/llamacpp_toolcall_detector.go` | ✅ + 8 тестов |

### 1.3 n_ctx resolver + auto-reload

| Компонент | Файл | Тест |
|---|---|---|
| 3-tier resolver: body → per-model profile → default profile | `internal/balancer/num_ctx_resolver.go` | `TestApplyCppCtxHeader` |
| `NCtxReloadCoordinator` (DecideReloadBackend, RecordCycleAttempt) | `internal/balancer/nctx_reload.go` | `TestReloadLoopLimit` |
| `X-Cpp-Ctx` header как upper limit | `cmd/cppworker/main.go:applyCppCtxHeader` | `TestClampNPredict` |
| `clampNPredictToFitContext` (tools-aware) | `internal/balancer/nctx_clamp.go` | (unit) |
| Auto-reload config (`AutoReloadNCtx: true` через ENV) | `internal/balancer/nctx_reload_config_bridge.go` | (integration) |

### 1.4 Per-Model Profiles + Reload API

| Endpoint | Файл | Тест |
|---|---|---|
| `GET /api/v1/cppworker/model-profiles` | `internal/api/handlers_cppworker_profiles.go` | `TestProfilesList` |
| `GET /api/v1/cppworker/model-profiles/{name}` | то же | (unit) |
| `PUT /api/v1/cppworker/model-profiles/{name}` | то же | (unit) |
| `DELETE /api/v1/cppworker/model-profiles/{name}` | то же | (unit) |
| `POST /api/v1/cppworker/model-profiles/{name}/apply` (reload on all backends) | то же | `TestApply` |

### 1.5 WebUI (P0–P2 — 100%)

| Компонент | Файл | Статус |
|---|---|---|
| Dashboard, Backends, Models, Sessions, Queue, Logs, Settings | `webui/index.html`, `webui/js/modules/renderers.js` | ✅ |
| Agents page + agent actions (Restart/Logs/Config) | `webui/js/modules/renderers.js`, `api.js`, `app.js` | ✅ |
| Backend type badges, gguf Settings tab, Backend load options | `webui/js/modules/backend-type-filter.js`, `gguf-renderer.js`, `cppworker-params.js` | ✅ |
| i18n (en.js 918 ключей, ru.js 824 ключа) | `webui/js/i18n/` | ✅ |
| Load/Unload/Delete buttons в modelsGrid | `webui/js/modules/renderers.js`, `app.js` (`window.modelCardAction`) | ✅ |
| Manage Models modal | `webui/js/app.js:openModelManageModal` | ✅ |
| Model details (family/format/quantization/RAM/expiresAt/digest) | `webui/js/modules/renderers.js` | ✅ |
| Hidden GPU tooltips (powerLimit/gpuClock/memClock) | `webui/js/monitor/ui-renderer.js` | ✅ |
| Disk/Network panel в Monitor | `webui/monitor.html`, `monitor/ui-renderer.js` | ✅ |

### 1.6 Background controllers

| Контроллер | Файл |
|---|---|
| Prewarm | `internal/balancer/prewarm_controller.go` |
| ModelInstanceController (scale up/down) | `internal/balancer/model_instance_controller.go` |
| UnloadScheduler (LRU) | `internal/balancer/unload_scheduler.go` |
| AdaptiveWeightTuner | `internal/balancer/weight_tuner.go` |
| AdaptiveTimeout (P95) | `internal/balancer/adaptive_timeout.go` |
| LlamaCpp metrics poller | `internal/balancer/llamacpp_metrics_poller.go` |

### 1.7 Метрики (полный набор)

- Все метрики агента: GPU (usage/memory/temperature/power), system (CPU/RAM/Disk/Network), Ollama (`runningModels`, `availableModels`, `version`), model details (family/format/parameterSize/quantization), `expiresAt`, `digest`, RAM estimation.
- Proxy-calculated: `ActiveRequests`, `FreeSlots`, `CalculatedRPS`, `AvailableSlots`, `QueueStats.{dispatch_by_affinity, dispatch_by_load, dispatch_by_config}`.
- Подробнее: [`docs/metrics.md`](../docs/metrics.md).

### 1.8 API endpoints

- REST API: backends CRUD, agents (register/metrics/heartbeat/stats/info/restart/logs), metrics, models management, sessions, queue, replication groups, virtual models, n_ctx-reload, model profiles, gguf backend proxy, config export/import, proxy logs, restart.
- WebSocket: `/ws/metrics` для real-time метрик.
- Полная спецификация: [`docs/api.md`](../docs/api.md).

### 1.9 RPC Model Distribution (B1–B8, Sessions 5–12)

| Фаза | Что сделано | Файл(ы) | Тесты |
|---|---|---|---|
| **B1** (Session 5) | `cmd/rpcworker` + `internal/rpcworker` — HTTP-сервер с 7 endpoints (`/rpc/infer`, `/rpc/kv_*`, `/rpc/health`, `/rpc/load_slice`, `/rpc/unload_slice`, `/rpc/metrics`, `/rpc/info`) | `cmd/rpcworker/`, `internal/rpcworker/` | 22+ unit + 8+ e2e |
| **B2** (Session 6) | RPC Management API в балансировщике: workers list/register/get/delete, models list, infer через coordinator | `internal/api/handlers_rpc.go`, `routes.go` | 13 unit (`handlers_rpc_test.go`) |
| **B3** (Session 7) | `HeartbeatLoop` — фоновый health-checker, `Start/Stop/tickAll/Forget`, порог unhealthy | `internal/rpccoordinator/heartbeat.go` | 8 unit (`heartbeat_test.go`) |
| **B4** (Session 8) | `KVStore` с TTL/capacity, реальная реализация `/rpc/kv_sync`/`/rpc/kv_fetch`, SSE-стриминг `/rpc/infer?stream=true` | `internal/rpcworker/kv_store.go` | 13+ KV/streaming |
| **B5** (Session 9) | gRPC-style binary protocol через `net/rpc`/gob: 7 RPC-методов + `MetricsSnapshot` (gob), `WorkerRPCClient` (lazy dial + reconnect) | `pkg/protocol/rpc_protocol.go`, `internal/rpcworker/server.go`, `internal/rpccoordinator/worker_rpc_client.go` | 9 unit + 7 e2e |
| **B6** (Session 10) | `Selector` interface: `LeastLoadedSelector` (default) + `RoundRobinSelector`, `LayerSlice.WorkerCandidates`, failover на 500 | `internal/rpccoordinator/selector.go` | 19 unit + 4 e2e |
| **B7** (Session 11) | `MetricsAggregator` (Counter/Gauge/Histogram) + Prometheus exposition format + endpoint `/api/v1/rpc/metrics` + WebUI `rpc-status.html` | `internal/rpccoordinator/metrics.go`, `internal/api/handlers_rpc_metrics.go`, `webui/rpc-status.html` | 11 unit |
| **B8** (Session 12) | Tensor Parallelism: `internal/rptensor` (~900 LOC), `ShardedModel`/`PartitionStrategy` (column/row/megatron/replicated), `TensorParallelCoordinator` (parallel goroutines + barrier + AllReduce), `StubTPRuntime`, `/rpc/tp/*` endpoints, `POST /api/v1/rpc/tp/infer`, `GET /api/v1/rpc/tp/status`, `webui/tp-pipeline.html` | `internal/rptensor/`, `internal/rpcworker/handlers_tp.go`, `internal/rpccoordinator/worker_client.go`, `internal/api/handlers_rpc_tp.go`, `webui/tp-pipeline.html` | 60+ unit + 4 e2e + 11 API |

**Архитектура:** `cmd/rpcworker` поднимает HTTP+gRPC-сервер, регистрируется в `internal/rpccoordinator/ModelCoordinator` через heartbeat (B3). Coordinator распределяет слои модели по воркерам (B1, B6), гоняет инференс параллельно (B4 streaming) и синхронизирует KV-cache между воркерами по sessionID. B5 добавляет бинарный протокол для low-latency локальных вызовов. B7 — Prometheus-метрики для observability. B8 — Tensor Parallelism для ускорения инференса через шардирование весов между несколькими GPU/workers с all-reduce после каждого слоя.

**Полная документация:** [`docs/rpc-coordinator.md`](../docs/rpc-coordinator.md) (все 8 секций B1–B8).

**Известные ограничения (не в этом релизе):**
- Real ggml/NCCL интеграция в `TPRuntime` (10–15 дней, post-1.0) — сейчас фреймворк работает на `StubTPRuntime` для e2e-тестов и UI-демо.
- `rpc_coordinator` и `virtual_router` режимы балансировщика — каркас создан (B1–B8), но pipeline execution ещё не завершён для production-режимов (см. [Known Limitations](#3-архитектурные-ограничения-known-limitations)).

### 1.10 Session F — UI/UX quick wins (F.0a, F.0b, F.α, F.β, F.γ)

| Подсессия | Что сделано | Файл(ы) | Тесты |
|---|---|---|---|
| **F.0a** (EOF diagnostics) | Классификация EOF в transport с error context (`backend_id`, `attempt`, `duration_ms`, `error_type`, `stream_position`), `inferErrorCategoryFromEOF` (12 категорий: timeout/reset/closed/aborted/incomplete/etc.) | `internal/balancer/transport_eof.go`, `transport_eof_test.go` | 12 unit |
| **F.0b** (SSE transport для notifications) | `EventBroker` (`pkg/types/event.go`) + `EventBus` для balancer-внутренних событий, `RingBuffer` для snapshot на reconnect, `GET /api/v1/events` SSE-handler, `Last-Event-ID` resume | `pkg/types/event.go`, `internal/api/handlers_events.go`, `internal/balancer/event_bus.go` | 8 unit |
| **F.α** (Health-aggregator из 3 источников) | `HealthAggregator` объединяет данные из `HealthChecker` + `EventBus` (sse) + transport EOF (per-backend), `HealthScore` формула (`100 - %unhealthy - errorPenalty`), `types.HealthLevel` (enum: healthy/degraded/unhealthy/critical) | `internal/balancer/health_aggregator.go`, `pkg/types/health.go` | 14 unit (`health_aggregator_test.go`) |
| **F.β** (Backend-таблица с подсветкой ошибок) | Endpoint `GET /api/v1/health/detailed` возвращает `[]BackendHealth` (id/name/type/uptime/error_count/last_error/health_level) + `[]RecentError` (timestamp/backend_id/error_type/transport/category), интегрирован с `HealthAggregator` | `internal/api/handlers_health.go`, `handlers_health_test.go` | 12 unit |
| **F.γ** (Frontend /health страница) | Standalone `webui/health.html` (447 строк, inline i18n EN+RU в `<script>` через JSON-escape, XSS-safe `escapeHtml`), 5 summary cards + backends/recent-errors tables, auto-refresh 2/5/10/30s, pause/refresh/back-to-dashboard, connection status indicator, nav-link "Здоровье" в `index.html`, `healthUIHandler` в `internal/api/handlers_core.go` (search paths `/app/webui/`, `webui/`, `../webui/`, `../../webui/`, `../../../webui/health.html`), route `/health` в `internal/api/routes.go`, COPY `health.html` в `docker/balancer/Dockerfile` и `docker/webui/Dockerfile`, nginx `location = /health { try_files /health.html =404; }` в `webui/nginx.conf` (preserves Docker healthcheck) | `webui/health.html`, `internal/api/handlers_core.go`, `internal/api/routes.go`, `internal/api/handlers_health_test.go`, `webui/index.html`, `docker/balancer/Dockerfile`, `docker/webui/Dockerfile`, `webui/nginx.conf` | 2 unit (`TestHealthUIHandler_GetReturnsHTML` + `TestHealthUIHandler_MethodNotAllowed`) |

**Архитектура F-сессии:** 3 источника ошибок (HealthChecker background-poll, SSE events от EventBus, transport EOF классификация) → HealthAggregator → /api/v1/health/detailed → standalone /health страница. Все 3 источника теперь видны одновременно, error_penalty от каждого складывается в HealthScore, что позволяет оператору видеть «тихие» деградации (например, health-checker ещё не заметил, а transport EOF уже зафиксировал 10 ресетов за минуту).

---

## 2. Реально оставшиеся задачи (R-1 … R-7)

| # | Задача | Приоритет | Файл(ы) | Оценка | Статус |
|---|---|---|---|---|---|
| **R-1** | Windows GPU metrics через `nvidia-smi`/WMI fallback | 🟢 P2 | `internal/agent/system_windows.go` | 6–10 ч | ✅ (Session 3, 2026-06-26 — WMI Win32_VideoController fallback через AdapterRAM, 11 unit-тестов в `system_wmi_parse_test.go`) |
| **R-2** | WebUI фильтры «Все / 🦙 Ollama / 🦒 llama.cpp» в Dashboard, Backends management, Monitor | 🟢 P2 | `webui/js/modules/renderers.js`, `app.js`, `monitor/ui-renderer.js`, `index.html`, `monitor.html`, `css/custom.css` | 2–3 ч | ✅ (Dashboard+Backends: `.type-filter-btn`, Monitor: `renderBackendTypeSwitcher`) |
| **R-3** | Убрать условный `t.Skip` в `TestTransferEncoding_BackendReturns503ThenRecovers` | 🟢 P2 | `tests/proxy_streaming_test.go:638` | 1–2 ч | ✅ (skip уже убран) |
| **R-4** | `setup-wizard.js` — полноценный выбор типа бэкенда (применение типа к Backend) | 🟡 P1 | `webui/js/modules/setup-wizard.js` | 2–3 ч | ✅ (`wizardState.backendType` → payload `backendEngine`, localStorage persist) |
| **R-5** | `cocoindex.js` — поддержка llama_cpp API | 🟢 P2 | `webui/js/modules/cocoindex.js` | 2–3 ч | ⚠️ **NOT APPLICABLE** (Session 13, 2026-06-27 — файл физически отсутствует; embeddings покрыты через `/v1/embeddings` cppworker + MCP-сервис `cocoindex/cocoindex-code` для code-search) |
| **R-6** | Endpoint `POST /api/v1/cppworker/reset-reload-counter` для сброса `ramFallbackAttempts` без `docker restart` | 🟡 P1 | новый handler + cppworker `cmd/cppworker/handlers_*.go` | 2 ч | ✅ (cppworker-model-params.md §7.3, 5 тестов) |
| **R-7** | Документация nctx-reload endpoints (`/api/v1/nctx-reload/*`) | ⚪ P3 | `docs/cppworker-model-params.md` | 1 ч | ✅ (cppworker-model-params.md §7.2-7.3) |

**Итого реально:** 19 задач-часов (R-1…R-7, без R-5 backlog).

---

## 3. Архитектурные ограничения (Known Limitations)

Зафиксированы и не планируются к реализации в текущем релизе.

| Ограничение | Причина | Где задокументировано |
|---|---|---|
| Ollama `images[]` в `/api/generate` | cppworker не поддерживает multimodal | `docs/api.md` (cppworker Ollama compatibility) |
| `context` (base64 KV-cache) между запросами | KV-cache не сохраняется между инференсами | `docs/cppworker-model-params.md` |
| `keep_alive` таймер unload | unload выполняется по `applyKeepAlive`; полный TTL-таймер не реализован | `docs/api.md` |
| Agent → Ollama runtime config | Ollama не имеет публичного REST API | `docs/troubleshooting.md` |
| `ollama pull <name>` для cppworker | нет Ollama registry → HTTP 501 | `docs/api.md` |
| Точная `modelLoadFeasibility` | требует GGUF metadata parser llama.cpp | `docs/metrics.md` |
| `rpc_coordinator` и `virtual_router` режимы | каркас создан, pipeline execution не завершён | `docs/configuration.md` |

---

## 4. Тесты (smoke-check)

```bash
# Backend type isolation
go test ./tests -run TestBackendType -count=1 -v

# OperatingMode dispatch
go test ./tests -run TestOperatingMode -count=1 -v

# Tools/tool_calls
go test ./tests -run TestDebugOpenWebUI_ToolCalls -tags llama_stub -count=1 -v

# n_ctx / RAM fallback / clamping
go test ./cmd/cppworker -run "TestApplyCppCtxHeader|TestClampNPredict|TestReloadDisabledForTools|TestParseToolCalls" -tags llama_stub -count=1 -v

# LlamaCpp proxy / transport
go test ./tests -run TestLlamaCpp -count=1 -v

# Model profiles
go test ./tests -run TestModelProfile -count=1 -v
```

---

## 5. Хронология ключевых вех

| Дата | Веха |
|---|---|
| 2026-04-28 | P0 аудит безопасности (Master Token, TLS, утечка goroutine) ✅ |
| 2026-05-03 | Консолидированный план |
| 2026-05-05 | Implementation plan (P0–P2) |
| 2026-05-07 | WebUI Monitor рефакторинг + background controllers |
| 2026-05-08 | Backend type isolation — первая фаза |
| 2026-05-13 | Refactoring фазы 1–6 |
| 2026-06-05 | gguf Settings tab + Backend load options |
| 2026-06-08 | Streaming fix (P-4 TransferEncodingError) ✅ |
| 2026-06-14 | Backend type isolation 100% (commit `affb4e8`) |
| 2026-06-17 | n_ctx fix (`config.json` `defaultModelProfile.contextLength=0`) |
| 2026-06-22 | Documentation consolidation + Tools runbook |
| 2026-06-26 | Session 1 — Tech debt A-1/A-2/A-3 (alias `applyCppCtxHeader` удалён, `RequestID` в `BackendMetrics`, TODO `model_management.go` убран) |
| 2026-06-26 | Session 2 — PF-1 (TestBackendsHandler_Get + TestServeHTTP_MixedCluster_RoutingByURLPath FIXED) |
| 2026-06-26 | Session 3 — R-1 DONE (Windows GPU metrics: WMI Win32_VideoController fallback через `AdapterRAM`, 11 unit-тестов) |
| 2026-06-26 | Session 4 — P-1 DONE (`POST /api/models/load-with-params`: расширенные llama.cpp параметры — `nThreads`/`parallel`/`kvCacheType`/`splitMode`/`overrideTensor`. 11 unit-тестов в `handlers_model_loadwithparams_test.go`, проксирование через `/api/v1/gguf/backends/{id}/proxy/...`. Обнаружены pre-existing failures PF-5/PF-6/PF-7 — задокументированы в `plans/pre-existing-test-failures.md`) |
| 2026-06-26 | Session 5 — **PF-3 FIXED** (`TestDefaultConfig` ожидал 4096, реально 32768) + **B1 DONE** (RPC Worker HTTP Server: `cmd/rpcworker` + `internal/rpcworker`, 7 endpoints, 22+ unit + 8+ e2e тестов, Dockerfile + compose) |
| 2026-06-26 | Session 6 — **B2 DONE** (RPC Management API: 6 endpoints в `internal/api/handlers_rpc.go` — workers list/register/get/delete, models list, infer через coordinator. Роуты в `internal/api/routes.go`. 13 unit-кейсов в `handlers_rpc_test.go`. Все три пакета rpcworker/rpccoordinator/api — PASS) |
| 2026-06-26 | Session 7 — **B3 DONE** (Heartbeat & Auto-Discovery: `HeartbeatLoop` в `internal/rpccoordinator/heartbeat.go` — фоновый health-checker с `Start/Stop`/`tickAll`/`Forget`/порогом unhealthy. 8 unit-кейсов в `heartbeat_test.go` (defaults, idempotency, healthy worker, unhealthy worker, recovery, Forget, IsRunning). Все три пакета — PASS) |
| 2026-06-26 | Session 8 — **B4 DONE** (Streaming Pipeline + KV-cache sync: `KVStore` в `internal/rpcworker/kv_store.go` — in-memory KV с TTL/capacity. Реальная реализация `/rpc/kv_sync` (POST) и `/rpc/kv_fetch` (GET). SSE-streаming `/rpc/infer?stream=true`. Обновлены B1-stub тесты под новую реализацию. 13+ KV/streaming кейсов. Все три пакета — PASS) |
| 2026-06-26 | Session 9 — **B5 DONE** (gRPC-style binary protocol через `net/rpc`/gob: `WorkerRPCService` в `pkg/protocol/rpc_protocol.go` с 7 RPC-методами (HealthCheck/LoadSlice/UnloadSlice/Infer/Metrics/KvSync/KvFetch) и `MetricsSnapshot` (gob-сериализуемый аналог `map[string]interface{}`). Helper-методы в `internal/rpcworker/server.go` (`WorkerID`/`Version`/`HandleInferRPC`/`MetricsRPC`/`KvStoreSaveRPC`/`KvStoreLoadRPC`). Флаги `--rpc-host/--rpc-port` + ENV `RPC_WORKER_RPC_*` в `cmd/rpcworker/main.go` (RPC-порт по умолчанию = HTTP+1000, т.е. 18080→19080). `WorkerRPCClient` в `internal/rpccoordinator/worker_rpc_client.go` (lazy dial + reconnect на любой RPC-ошибке). 9 unit-тестов в `pkg/protocol/rpc_protocol_test.go` + 7 e2e в `internal/rpccoordinator/e2e_rpc_test.go` (HealthCheck, FullPipeline с LoadSlice→Infer→Metrics→KvSync→KvFetch→UnloadSlice, TwoWorkersIsolated, ConcurrentClients, ReconnectAfterServerRestart, LoadSliceInvalidModel, MetricsContainsAllFields). Документация в `docs/rpc-coordinator.md` (секция B5). Все три пакета `pkg/protocol`/`internal/rpcworker`/`internal/rpccoordinator` — PASS) |
| 2026-06-26 | Session 10 — **B6 DONE** (Load Balancing между срезами: `Selector` interface в `internal/rpccoordinator/selector.go` с двумя реализациями — `LeastLoadedSelector` (default, выбор по min(active_requests/capacity), healthy workers перед unhealthy) и `RoundRobinSelector` (для тестов). `WorkerLoadInfo.LoadRatio()` возвращает долю 0..∞. `LayerSlice.WorkerCandidates []string` (backward-compatible: пусто → fallback [WorkerID]) + метод `Candidates()`. `WorkerClient.LastMetrics atomic.Value` (кэш метрик) + `WorkerMetricsSnapshot` структура + метод `RefreshMetrics(ctx)`. `ModelCoordinator.selector` поле + `SetSelector()/GetSelector()` + `SelectWorkersForSlice()` + `GetWorkerLoadInfo()`. `executePipeline` обновлён: для каждого среза получает ordered candidates через `selector.Select()`, пробует по очереди, при ошибке переходит к следующему (failover). Если все кандидаты fail — возвращается `"slice X-Y failed on all candidates"`. 19 unit-тестов в `selector_test.go` (LoadRatio, LeastLoadedSelector — пустые/all-unknown/healthy-first/all-unhealthy/Name, RoundRobinSelector — пустые/rotates/Name, ModelCoordinator integration — SetGetSelector/SelectWorkersForSlice/HealthyPreferred/GetWorkerLoadInfo_NoWorkers, LayerSlice.Candidates — only-WorkerID/empty/in-list/not-in-list, concurrent safety для обоих selector'ов) + 4 e2e в `e2e_selector_test.go` (FailoverToSecondary — primary 500→secondary, PrimaryHealthy — оба healthy, AllCandidatesFail — оба 500, NoCandidatesConfigured — fallback на WorkerID). Документация в `docs/rpc-coordinator.md` (секция B6). `go test -tags llama_stub ./internal/rpccoordinator/` — PASS (6.5s, без регрессий по B1-B5 тестам)) |
| 2026-06-26 | Session 11 — **B7 DONE** (Prometheus Metrics + WebUI: `MetricsAggregator` в `internal/rpccoordinator/metrics.go` — `Counter` (atomic.Int64), `Gauge` (atomic.Int64), `Histogram` (fixed-bucket: `DefaultLatencyBuckets` = `[5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000]` ms). Экспорт в Prometheus exposition format (text/plain; version=0.0.4) через `WritePrometheus(w io.Writer)`. Метрики: counters — `rpc_inference_total`, `rpc_inference_errors_total`, `rpc_slice_infer_total`, `rpc_slice_infer_errors_total`, `rpc_load_slice_total`, `rpc_unload_slice_total`; gauges — `rpc_active_jobs`, `rpc_registered_workers`, `rpc_registered_models`, `rpc_kv_cache_size_bytes`, `rpc_uptime_seconds`, per-worker `rpc_worker_health{worker_id="..."}`; histograms — `rpc_inference_duration_ms`, `rpc_slice_latency_ms`. `ModelCoordinator.metrics` поле + `Metrics()` getter + `Stats()` (новый тип `CoordinatorStats`: Workers/Models/ActiveJobs/WorkersHealth/Selector). Endpoint `GET /api/v1/rpc/metrics` в `internal/api/handlers_rpc_metrics.go` (с AuthMiddleware + RateLimit) — refresh gauges перед экспортом. Роут зарегистрирован в `routes.go`. WebUI `webui/rpc-status.html` — dark-mode self-contained панель (parseProm/Overview cards/Workers table/Histogram table/raw viewer/auto-refresh 10s). 11 unit-тестов в `metrics_test.go` (Counter concurrent, Gauge, Histogram Observe + BucketsSorted, MetricsAggregator New + Setters + AllWorkerHealth, WritePrometheus Format + Empty + HistogramBucketCumulative). Все тесты `./internal/api/` и `./internal/rpccoordinator/` PASS без регрессий. Документация в `docs/rpc-coordinator.md` (секция B7). |
| 2026-06-27 | **Session 12 — B8 DONE** (Tensor Parallelism: `internal/rptensor` (~900 LOC) с `ShardedModel`/`PartitionStrategy` (column/row/megatron/replicated), `TensorParallelCoordinator` с параллельными goroutines + barrier + AllReduce (Concat/SumBytes/XorBytes/Mean), `StubTPRuntime` (deterministic). `internal/rpcworker` — `/rpc/tp/{infer,kv_sync,kv_fetch}` + rank-keyed `KVStore.SaveShard/LoadShard`. `internal/rpccoordinator` — `WorkerClient.TPInferSlice/TPKvSync/TPKvFetch` + E2E test (4 workers × 4 layers через httptest, parallel speedup ~3.8x). `internal/api` — `POST /api/v1/rpc/tp/infer` + `GET /api/v1/rpc/tp/status` + `tpCoordinatorRegistry`. `webui/tp-pipeline.html` — dark-mode панель layers × ranks. 60+ unit + 4 e2e + 11 API tests. Build OK: `go test -tags llama_stub ./internal/{rptensor,rpcworker,rpccoordinator,api}/` — все PASS. Реальная ggml/NCCL интеграция — post-1.0. См. [plans/b8-tensor-parallelism-plan.md](b8-tensor-parallelism-plan.md) и [docs/rpc-coordinator.md § B8](../docs/rpc-coordinator.md). |
| 2026-06-27 | **Session 13 — R-5 NOT-APPLICABLE** (`webui/js/modules/cocoindex.js` физически отсутствует; embeddings покрыты через `/v1/embeddings` на cppworker и MCP-сервис `cocoindex/cocoindex-code`. Задача закрыта как not-applicable) + **PF-5/6/7 FIXED** (ResponseHeaderTimeout для streaming + passthrough финального чанка в NDJSON + Content-Type для non-streaming OpenAI chat) + **Models tab Filter/Search** (`324ddbe`) |
| 2026-06-27 | **Session 14 — Pull progress UI** (`089a931`): progress-bar + ETA + Cancel-кнопка в Manage Models modal, auto-refresh 2 сек, heuristic-расчёт прогресса по типу операции (pull/load/unload) |
| 2026-06-27 | **Session 15 — Model profiles UI** (`77e3442`): advanced секция (flash_attn/numa/use_mmap 3-state + 4 per-model таймаута), `Api.cppworkerModelProfiles` namespaced client, i18n парные ключи (40 EN = 40 RU) |
| 2026-06-27 | **Session 16 — Per-Model Profiles: parallel + kv_cache_type** (`54cfb8d`, `bd02938`): полная цепочка backend → C-bridge → cppworker → WebUI. `pkg/types/balancing.go` + `internal/cppbackend/backend.go` + `c/bridge/bridge.{h,c,go,stub.go}` + `cmd/cppworker/handlers_model.go` + WebUI wizard. Q3 roadmap section 2.2 закрыта полностью. |
| 2026-06-27 | **Session 17 — CSS hardcoded color audit** (`db64ac4`): 178→123 хардкоженных цветов (-31%), 40+ новых theme-aware translucent tokens в `webui/css/themes.css` (dark/light), рефакторинг `components.css`/`monitor-app.css`/`pages.css`/`data.css` |
| 2026-06-27 | **Session 18 — PF-4 flaky teardown FIXED** (`86bcd7f`): `tests/first_byte_timeout_test.go` — `defer upstream.Close()` заменён на `Listener.Close() + CloseClientConnections()` (паттерн из `TestOpenAIChat_HeaderTimeout_StillWorks`), таймаут 70s→60s, defensive `r.Context().Done()` в keepalive-цикл. **Все 7 pre-existing failures (PF-1#1, PF-1#2, PF-3, PF-4, PF-5, PF-6, PF-7) теперь ✅ FIXED.** Q3 метрика "Все pre-existing failures закрыты" выполнена. |
| 2026-06-27 | **Session 19 — Model details panel + Dashboard "Loaded models" counter** (`58206d6`): backend `GET /api/v1/cluster/models/{name}/info` (проксирование `/api/show` на каждый llama_cpp/ollama бэкенд, per-backend error semantics), WebUI info-кнопка в `.model-card-actions`, модалка с секциями General/Capabilities/Runtime/Other backends, Dashboard counter через `Api.clusterModels.loaded()`. 9 тестов PASS. Закрывает последний gap в roadmap section 2.2. |
| 2026-06-28 | **Session A — Bulk operations on Models tab** (`d2aacff`): cluster-level endpoint `POST /api/v1/cluster/models/bulk` (operation: load/unload/reload, до 100 моделей, параллельное выполнение `sync.WaitGroup` с concurrency=4, Unload All через `collectAllLoadedModels()`). WebUI: multi-select чекбоксы + toolbar + quick-select (All/Loaded/Inverse/Clear) + 4 bulk-кнопки + confirm dialog для Delete. 13 unit-тестов PASS. |
| 2026-06-28 | **Session B — Filter/Search/Sort improvements** (`1a17596`): 2 новых sort-опции (vram-desc/asc), `modelsCount` ("X из Y моделей"), localStorage persistence для search query, автоприменение фильтра при auto-refresh. |
| 2026-06-28 | **Session C — Theme toggle improvements** (`e2b9857`): anti-FOIT inline `<script>` в `<head>`, keyboard shortcut `Ctrl+Shift+T`, system preference tracking через `matchMedia`, broadcast событие `theme:changed` для Chart.js/monitor. |
| 2026-06-28 | **Session D — Per-Model Profiles parallel+kv_cache_type VERIFICATION** (`9f8e029`): проверка показала, что Session 16 полностью завершила задачу; никаких дополнительных изменений не требуется. |
| 2026-06-28 | **Session E — Export logs to CSV/JSON/TXT** (`dc49912`): `webui/js/app.js:exportLogs()` расширен до 3 форматов (CSV с RFC 4180 escape, JSON-массив с metadata, TXT legacy), level filter (all/debug/info/warn/error), format dropdown, имена файлов с timestamp+level. 11 i18n ключей × 2 языка. Закрывает roadmap section 5.2. |
| 2026-06-28 | **Session F.0a — EOF diagnostics** (`internal/balancer/transport_eof.go`): `inferErrorCategoryFromEOF` классифицирует 12 категорий EOF-ошибок (timeout/reset/closed/aborted/incomplete/...). `BackendErrorContext` с полями `backend_id`, `attempt`, `duration_ms`, `error_type`, `stream_position`, `category`. 12 unit-тестов в `transport_eof_test.go` (EmptyStream, MidStreamEOF, TimeoutEOF, ConnectionResetEOF, BrokenPipeEOF, AbortedEOF, IncompleteResponseEOF, AlreadyClosedEOF, NetworkUnreachableEOF, IOTimeoutEOF, EarlyEOF, EndOfStreamIsNormal). |
| 2026-06-28 | **Session F.0b — SSE transport для notifications** (`pkg/types/event.go` + `internal/api/handlers_events.go` + `internal/balancer/event_bus.go`): `EventBroker` для типизированных событий (backend_status/error/load/unload/...), `EventBus` для balancer-внутренней pub/sub, `RingBuffer` для snapshot на reconnect (последние 100 событий), `GET /api/v1/events` SSE-handler с `Last-Event-ID` resume, `text/event-stream` content-type, heartbeat `:ping` каждые 30s. 8 unit-тестов. |
| 2026-06-28 | **Session F.α — Health-aggregator из 3 источников** (`internal/balancer/health_aggregator.go` + `pkg/types/health.go`): `HealthAggregator` объединяет данные из `HealthChecker` (background-poll) + `EventBus` (SSE) + transport EOF (per-backend counters). `HealthScore` формула: `100 - %unhealthy * 0.7 - errorPenalty * 0.3` (clamp 0..100). `types.HealthLevel` enum: healthy (≥90), degraded (≥70), unhealthy (≥40), critical (<40). 14 unit-тестов. |
| 2026-06-28 | **Session F.β — Backend-таблица с подсветкой ошибок** (`internal/api/handlers_health.go`): `GET /api/v1/health/detailed` возвращает `[]BackendHealth{id, name, type, uptime, error_count, last_error, health_level, source_breakdown: {healthchecker, sse_events, transport_eof}}` + `[]RecentError{timestamp, backend_id, error_type, transport, category, message}`. 12 unit-тестов в `handlers_health_test.go`. |
| 2026-06-28 | **Session F.γ — Frontend /health страница** (`webui/health.html` + handlers/routing/docker): standalone self-contained HTML (447 строк, inline i18n EN+RU через JSON-escape `\uXXXX` в `<script>`, XSS-safe `escapeHtml`), 5 summary cards (Health Score / Total / Healthy / Unhealthy / Recent Errors), 2 tables (Backends / Recent Errors), auto-refresh 2/5/10/30s, pause/refresh/back-to-dashboard, connection status indicator, nav-link "Здоровье" в `index.html`, `healthUIHandler` в `internal/api/handlers_core.go` (5 search paths), route `/health` в `routes.go`, `COPY health.html` в `docker/balancer/Dockerfile` + `docker/webui/Dockerfile`, `nginx.conf`: `location = /health { try_files /health.html =404; }` (preserves Docker healthcheck). 2 unit-теста (`TestHealthUIHandler_GetReturnsHTML` PASS, `TestHealthUIHandler_MethodNotAllowed` PASS). |
| **2026-Q3** | **Roadmap to 1.0 — Q3 W3-4 (Models tab gaps) ЗАКРЫТ полностью** (Sessions 13-19 + A-E) + **Session F.0a/b/α/β/γ ЗАКРЫТ** (UI/UX quick wins: EOF diagnostics, SSE notifications, Health aggregator, /health page). F.4 (i18n EN/RU баланс) — последняя подзадача Session F. Production-ready фичи (rpc_coordinator, virtual_router, real ggml, CI/CD) запланированы в [production-ready plan](2026-q3-production-ready-plan.md) на август-сентябрь 2026. |

---

## 6. Связанные документы

- [`docs/audit-2026-06.md`](../docs/audit-2026-06.md) — финальный аудит-отчёт (соответствует roadmap).
- [`docs/README.md`](../docs/README.md) — главный индекс документации.
- [`docs/backend-type-isolation.md`](../docs/backend-type-isolation.md) — архитектура изоляции типов.
- [`docs/cppworker-model-params.md`](../docs/cppworker-model-params.md) — n_ctx + Per-Model Profiles + endpoints.
- [`docs/runbook-tools-debug.md`](../docs/runbook-tools-debug.md) — пошаговая диагностика tools/tool_calls.
- [`docs/troubleshooting.md`](../docs/troubleshooting.md) — общий FAQ + n_ctx диагностика.
- [`.clinerules`](../.clinerules) — рабочие правила для Cline.
- [`plans/archive/`](../plans/archive/) — устаревшие/завершённые планы.