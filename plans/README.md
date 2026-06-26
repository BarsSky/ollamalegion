# OllamaLegion — Roadmap (живой документ)

> **Дата обновления:** 2026-06-27  
> **Назначение:** единственный источник правды по реализованному и оставшемуся в проекте OllamaLegion.  
> Все устаревшие/завершённые планы — в `plans/archive/`.

---

## 0. Планы в работе (WIP)

| План | Файл | Сессия | Статус |
|------|------|--------|--------|
| **B8 — Tensor Parallelism** | [`b8-tensor-parallelism-plan.md`](b8-tensor-parallelism-plan.md) | Session 12 | ⬜ WIP (план готов, 9 этапов) |

Когда начнёте Session 12 — передайте Cline start prompt из секции 8 плана B8.

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

---

## 2. Реально оставшиеся задачи (R-1 … R-7)

| # | Задача | Приоритет | Файл(ы) | Оценка | Статус |
|---|---|---|---|---|---|
| **R-1** | Windows GPU metrics через `nvidia-smi`/WMI fallback | 🟢 P2 | `internal/agent/system_windows.go` | 6–10 ч | ✅ (Session 3, 2026-06-26 — WMI Win32_VideoController fallback через AdapterRAM, 11 unit-тестов в `system_wmi_parse_test.go`) |
| **R-2** | WebUI фильтры «Все / 🦙 Ollama / 🦒 llama.cpp» в Dashboard, Backends management, Monitor | 🟢 P2 | `webui/js/modules/renderers.js`, `app.js`, `monitor/ui-renderer.js`, `index.html`, `monitor.html`, `css/custom.css` | 2–3 ч | ✅ (Dashboard+Backends: `.type-filter-btn`, Monitor: `renderBackendTypeSwitcher`) |
| **R-3** | Убрать условный `t.Skip` в `TestTransferEncoding_BackendReturns503ThenRecovers` | 🟢 P2 | `tests/proxy_streaming_test.go:638` | 1–2 ч | ✅ (skip уже убран) |
| **R-4** | `setup-wizard.js` — полноценный выбор типа бэкенда (применение типа к Backend) | 🟡 P1 | `webui/js/modules/setup-wizard.js` | 2–3 ч | ✅ (`wizardState.backendType` → payload `backendEngine`, localStorage persist) |
| **R-5** | `cocoindex.js` — поддержка llama_cpp API | 🟢 P2 | `webui/js/modules/cocoindex.js` | 2–3 ч | ⬜ **Backlog (после 1.0)** |
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
| **2026-Q3** | **Roadmap to 1.0 — реализация B5-B8 + R-2…R-7** |

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