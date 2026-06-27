# OllamaLegion — Roadmap Q3 2026 (после B8)

> **Дата:** 2026-06-27
> **Контекст:** все фазы B1–B8 + R-1…R-4, R-6, R-7 DONE (Sessions 1–12).
> **Назначение:** план реализаций на следующие 2–3 месяца, до и сразу после релиза 1.0.

---

## 0. Текущее состояние (что уже сделано)

В Sessions 1–12 (2026-04-28 — 2026-06-27) реализовано:

- **Backend Type Isolation** (100%, commit `affb4e8`) — Ollama vs llama.cpp routing на уровне балансировщика, WebUI-бейджи, фильтры.
- **CppWorker Ollama-совместимость** — `/api/generate`, `/api/chat`, `/api/embeddings`, `/api/tags`, `/api/show`, `/api/copy`, `/api/create`, `/api/pull`, `/api/push` (501), tool_calls detection (Hermes/Mistral).
- **n_ctx resolver + auto-reload + Per-Model Profiles** — 3-tier resolver, X-Cpp-Ctx header, clamp n_predict, reload loop counter с reset endpoint.
- **RAM fallback** — 3-stage каскад для нехватки VRAM (Stage 1 VRAM → Stage 2 partial offload + mmap → Stage 3 CPU-only + max_viable).
- **Background controllers** — Prewarm, ModelInstance, UnloadScheduler, AdaptiveWeightTuner, AdaptiveTimeout, metrics poller.
- **WebUI P0–P2** (100%) — Dashboard, Backends, Models, Sessions, Queue, Logs, Settings, Monitor, i18n (en+ru).
- **RPC Model Distribution B1–B8** — RPC Worker HTTP+gRPC, KVStore, heartbeat, streaming, бинарный протокол, selector, Prometheus metrics, WebUI rpc-status, **Tensor Parallelism framework** (Sessions 5–12).
- **Runbook-tools-debug**, **cppworker-model-params.md**, **backend-type-isolation.md** — документация.

**Открытые долги** (R-1…R-7, PF-5/6/7, R-5, B8.7 real ggml) — см. ниже.

---

## 1. Что осталось из старых бэклогов

### 1.1 R-5 — `cocoindex.js` для llama_cpp (P2, 2–3 ч)

> **⚠️ NOT APPLICABLE — отложено** (Session 13, 2026-06-27):
> файл `webui/js/modules/cocoindex.js` **физически отсутствует** в проекте —
> не существует в git, нет упоминаний в `webui/index.html` или других модулях.
> Термин «cocoindex» в проекте относится к **серверному MCP-сервису**
> (`deployments/docker-compose.cocoindex.yml`, образ `cocoindex/cocoindex-code`),
> а не к WebUI-модулю. Embeddings-функционал уже покрыт через OpenAI-совместимый
> `/v1/embeddings` endpoint в cppworker (`cmd/cppworker/handlers_embeddings.go`)
> и балансировщик (`internal/balancer/llamacpp_handlers_inference.go`).
> Задача закрыта как not-applicable — другие методы работы (прямой embeddings
> через балансировщик + cocoindex MCP-сервис для code-search) уже настроены.
>
> Оригинальное описание roadmap (оставлено для истории):
> **Файл:** `webui/js/modules/cocoindex.js`
> **Что:** сейчас модуль работает только с Ollama API. Нужно добавить llama_cpp-режим
> (через `/api/tags` или `/api/show` cppworker'а + OpenAI-совместимый `/v1/embeddings`).
> **Acceptance:**
> - При выборе backend type = llama_cpp WebUI показывает список моделей из cppworker (`GET /api/tags` через balancer).
> - Кнопка «Embeddings» в cocoindex вызывает `/v1/embeddings` на балансировщике.
> - 3 unit-теста на mock httptest-сервере (ollama mode, llamacpp mode, fallback).

### 1.2 PF-5 — `TestOpenAIChat_HeaderTimeout_StillWorks` (P2, 1–2 ч)

**Файл:** `tests/openwebui_compatibility_test.go` (или аналогичный)
**Симптом:** запрос с 2-секундным header timeout не падает за 10 секунд, как ожидает тест.
**Корневая причина:** в transport `OpenAIChat` либо игнорируется header timeout, либо он суммируется с request timeout.
**Acceptance:** тест PASS за <3 сек.

### 1.3 PF-6 — LlamaCpp streaming proxy tests (3 теста) (P2, 2–4 ч)

**Файл:** `tests/llamacpp_proxy_test.go`, `tests/llamacpp_proxy/`
**Симптом:** SSE-стрим от llama.cpp бэкенда не содержит `done: true` / `message.content` / `llama.cpp string`.
**Корневая причина:** прокси-стример неправильно маппит NDJSON → SSE (или наоборот).
**Acceptance:** 3 теста PASS.

### 1.4 PF-7 — `TestOpenWebUI_Sequential_MixedRequests/step6-llamacpp-non-streaming` (P2, 1 ч)

**Файл:** `tests/openwebui_compatibility_test.go`
**Симптом:** Content-Type = `text/plain` вместо `application/json` для non-streaming llama.cpp ответа.
**Acceptance:** тест PASS.

---

## 2. Models tab — серьёзные gap'ы из `plans/archive/webui-gap-analysis.md`

Из анализа (май 2026, раздел 2.3):

### 2.1 Models list — частично реализовано
- ✅ Список моделей (через `/api/tags`).
- ✅ Кнопки Load/Unload/Delete.
- ✅ Manage Models modal.

### 2.2 Models — отсутствует ❌

| Фича | Файл | Оценка |
|---|---|---|
| **Model details panel** — модальное окно с полной метадатой модели (size, quantization, family, parameters, expiresAt, digest) | `webui/js/modules/renderers.js`, `webui/index.html` | 3–4 ч |
| **Bulk operations** — мульти-выбор + кнопки "Load All" / "Unload All" / "Delete Selected" | `webui/js/modules/renderers.js`, `webui/js/modules/api.js` | 2–3 ч |
| **Filter / Search** — поиск по имени модели, фильтр по бэкенду (🦙/🦒), по статусу (loaded/unloaded) | `webui/js/modules/renderers.js`, `webui/js/modules/backend-type-filter.js` | 2 ч |
| **Sort** — по имени / размеру / дате создания | `webui/js/modules/renderers.js` | 1 ч |
| **Pull progress UI** — для `hf:` моделей показывать progress bar (download/total bytes, ETA) | `webui/js/modules/renderers.js`, `webui/js/modules/api.js` | 4–6 ч |
| **Model profiles UI** — визуальный редактор per-model profiles (n_ctx, gpu_layers, parallel, kv_cache_type) | `webui/js/modules/cppworker-params.js`, новый `model-profiles.html` | 6–8 ч |

  **Статус (после Session 15, commit `da852f7` 2026-06-27):** UI wizard реализован
  с полной поддержкой 11 полей `types.LlamaCppModelProfile` (ContextLength, BatchSize,
  NumGPULayers, FlashAttn, NUMA, UseMmap, Notes, + 4 per-model таймаута).
  Все 5 backend endpoints (`list`/`get`/`upsert`/`remove`/`apply`) вызываются через
  `Api.cppworkerModelProfiles` namespace. i18n парные ключи (40 EN = 40 RU).
  Поля **`parallel`** (n_parallel, число параллельных запросов) и **`kv_cache_type`**
  (тип квантизации KV-cache: F16/Q8_0/Q4_0) из roadmap **НЕ добавлены** — backend
  API их пока не поддерживает. См. **Session 16 — Per-Model Profiles: backend
  extension** ниже.

| **Loaded models counter в Dashboard** — сколько моделей загружено всего / per backend | `webui/js/modules/renderers.js` | 1 ч |

**Итого:** 19–25 ч реальной работы. Можно разбить на 2–3 сессии (P2, низкий приоритет, но сильно улучшает UX).

---

## 3. Долгосрочные архитектурные задачи (post-1.0)

### 3.0 Session 16 — Per-Model Profiles: backend extension (`parallel` + `kv_cache_type`) (3–5 ч)

**Контекст:** Session 15 (commit `da852f7`, 2026-06-27) реализовал UI wizard для per-model profiles
с полной поддержкой 11 полей `types.LlamaCppModelProfile`. Roadmap 2.2 line 90 указывает ещё 2 поля:
- **`parallel`** (n_parallel) — число параллельных слотов для одновременной обработки нескольких запросов
  к одной модели. Полезно для multi-user throughput: один экземпляр модели обслуживает N
  параллельных запросов без необходимости реплицировать модель на N бэкендов.
- **`kv_cache_type`** — тип квантизации KV-cache (F16 / Q8_0 / Q4_0). Q8_0 экономит ~50% VRAM
  на KV-cache (выигрыш на длинных контекстах для моделей с gemma-4 256K), Q4_0 — ~75%.
  Trade-off: лёгкая деградация качества (perplexity increase ~1-2%).

**Что нужно сделать**:

#### Часть A — Backend (Session 16, server-side, ~2-3 ч)

1. **`pkg/types/balancing.go`** — расширить `LlamaCppModelProfile`:
   ```go
   Parallel     int    `json:"parallel,omitempty"`     // 0 = default cppworker, 1..8 = parallel slots
   KVCacheType  string `json:"kvCacheType,omitempty"`  // "" = inherit, "f16"/"q8_0"/"q4_0" = override
   ```
   Документировать trade-off Q4_0 (max savings, 1-2% perplexity hit) vs Q8_0 (balanced).

2. **`internal/api/handlers_cppworker_profiles.go`** — расширить `validateModelProfile`:
   - `parallel ∈ [0, 8]` (cppworker не поддерживает > 8 параллельных слотов).
   - `kvCacheType ∈ {"", "f16", "q8_0", "q4_0"}`.

3. **`internal/api/handlers_cppworker_profiles.go:reloadModelOnCppWorker`** —
   прокинуть новые поля в `POST /api/models/reload` body:
   ```go
   body["parallel"] = profile.Parallel
   if profile.KVCacheType != "" {
       body["kvCacheType"] = profile.KVCacheType
   }
   ```

4. **`internal/cppbackend/backend.go`** — `LoadModelOpts` расширить полями
   `Parallel *int` (nil = inherit), `KVCacheType *string` (nil = inherit).
   Валидация на backend стороне: `parallel >= 1`, `kv_cache_type ∈ {f16, q8_0, q4_0}`.

5. **`cmd/cppworker/handlers_model.go`** — `handleLoadModel` и
   `handleLoadWithParams` (`handlers_model_loadwithparams.go`) принимают новые поля,
   маппят на `LoadModelOpts`.

6. **`cmd/cppworker/inference.go` + `cmd/cppworker/lazyload.go`** — прокинуть
   `Parallel` в `bridge.GenerationParams.Parallel`, `KVCacheType` в `params.TypeK` / `TypeV`.

7. **`c/bridge/bridge.go`** — расширить `GenerationParams`:
   ```go
   Parallel    int32
   KVCacheType int32  // 0=f16, 1=q8_0, 2=q4_0
   ```
   + `bridge_set_parallel(model, n)` и `bridge_set_kv_cache_type(model, type)`
   в `bridge.c` через llama.cpp `llama_set_n_parallel()` / `llama_set_tensor_params()`.

8. **Тесты**: `internal/api/handlers_cppworker_profiles_test.go` — 4 новых кейса
   (`TestValidateModelProfile_ParallelBounds`, `TestValidateModelProfile_KVCacheTypeValid`,
   `TestMergeModelProfile_PartialUpdate_Parallel`, `TestMergeModelProfile_PartialUpdate_KVCacheType`).

#### Часть B — WebUI extension (Session 16, ~1-2 ч)

UI wizard (`webui/js/modules/cppworker-params.js`) уже расширяется по тому же шаблону,
что и advanced секция в Session 15:

1. **В advanced секции** добавить 2 новых поля:
   - `parallel` (number, 0-8, default 0 = inherit) — с tooltip
     «Number of parallel slots for concurrent requests. 0 = use cppworker default.
     Trade-off: each slot consumes additional KV-cache memory (n_parallel × KV-cache per slot).
     Useful for multi-user throughput without model replication.»
   - `kvCacheType` (select `inherit/f16/q8_0/q4_0`, default `inherit`) — с tooltip
     «KV-cache quantization. f16 = default (full precision).
     q8_0 = -50% VRAM (recommended, minimal perplexity hit).
     q4_0 = -75% VRAM (1-2% perplexity hit, use for very long contexts).»

2. **`profileToWizardState` / `wizardStateToProfileBody`** — расширить аналогично
   `flashAttn` (3-state для bool + новое поле для select с тем же inherit-pattern).

3. **i18n** — 4 новых ключа в en.js + ru.js:
   `settings.profiles.parallel`, `settings.profiles.parallel_help`,
   `settings.profiles.kv_cache_type`, `settings.profiles.kv_cache_type_help`.

4. **CSS** — без новых классов, переиспользуется `.wizard-field` + `<select>` из Session 15.

5. **Meta-строка списка** — добавить `[parallel=2 · kv=q8_0]` если заданы.

#### Acceptance criteria

1. Backend принимает и валидирует `parallel` ∈ [0, 8] и `kvCacheType ∈ {"", "f16", "q8_0", "q4_0"}`.
2. `POST /api/v1/cppworker/model-profiles/{name}` с `{"parallel": 2, "kvCacheType": "q8_0"}`
   возвращает 200 с профилем.
3. `POST /api/v1/cppworker/model-profiles/{name}/apply` перезагружает модель
   с заданными parallel и kv_cache_type (cppworker logs показывают применение).
4. UI wizard показывает 2 новых поля в advanced секции с правильными значениями
   при edit и валидацией на save.
5. `parallel=2, kv_cache_type=q8_0` → gemma-4 8B с n_ctx=65536 экономит ~3.5 GB VRAM
   на KV-cache (было 7 GB → 3.5 GB при Q8_0) и обслуживает 2 параллельных запроса.
6. Build OK: `go build -tags llama_stub ./cmd/cppworker/ && ./cmd/balancer/`.
7. Все тесты профилей зелёные (10+4 = 14 кейсов).
8. CHANGELOG подсекция `### Added (Session 16 — Per-Model Profiles: parallel + kv_cache_type)`.

### 3.1 B8.7 — Real ggml/NCCL integration в `TPRuntime` (10–15 дней)

**Файлы:**
- `internal/rptensor/tp_runtime.go` (interface готов)
- `internal/rptensor/cuda_runtime.go` (новый) — реальный CUDA all-reduce через NCCL или собственный kernel.
- `internal/rptensor/ggml_shard.go` (новый) — загрузка одного шарда тензоров из GGUF.
- `c/bridge/bridge.go` — расширение для `LoadShard(layerIdx, rank, worldSize)`.

**Что нужно:**
1. C-bridge функция `bridge_load_shard(path, layer_idx, rank, worldSize)` — возвращает только нужные тензоры для одного rank.
2. CUDA IPC handle для KV-cache между rank'ами (через `cudaIpcGetMemHandle` / `cudaIpcOpenMemHandle`).
3. NCCL all-reduce через `ncclAllReduce` с коммуникатором `ncclCommInitRank`.
4. Megatron-совместимое размещение: rank 0 хранит Q[0..H/4], K[0..H/4], V[0..H/4], FFN gate/up[0..I/4]; rank 1 — Q[H/4..H/2] и т.д.
5. Forward pass с gRPC-streaming между rank'ами (B5 + B8).
6. Тесты: реальный GPT-2/Llama-3 8B на 2× RTX 4090, проверка parallel speedup vs single-GPU.

**Acceptance:**
- Реальный 2-rank TP inference модели 8B работает на 2 GPU.
- KV-cache шарится через CUDA IPC, latency all-reduce < 5ms на слой.
- Покрытие e2e-тестами с реальной CUDA (требует GPU в CI, сейчас можно сделать `t.Skip` если `cudaAvailable=false`).

### 3.2 `rpc_coordinator` режим балансировщика — production (5–8 дней)

**Файлы:**
- `internal/balancer/rpc_coordinator_dispatcher.go` (новый) — для режима `rpc_coordinator` маршрутизация идёт через `internal/rpccoordinator/ModelCoordinator`.
- `internal/balancer/operating_modes.go` — расширение для активации режима.

**Что нужно:**
1. Поднять `ModelCoordinator` в `cmd/balancer/main.go` если `OperatingMode == "rpc_coordinator"`.
2. Зарегистрировать RPC workers через heartbeat (B3).
3. Маршрутизация `/api/generate` → `executePipeline` вместо прямого проксирования.
4. Fallback: если все workers unhealthy → HTTP 503.
5. Тесты: `TestOperatingMode_RpcCoordinator_*` (mock workers через httptest).

**Acceptance:** `OperatingMode = "rpc_coordinator"` end-to-end inference через RPC pipeline работает, fallback на 503 при недоступности workers.

### 3.3 `virtual_router` режим — production (5–7 дней)

**Файлы:**
- `internal/virtualmodel/` (каркас создан)
- `internal/balancer/virtual_router.go` (новый)

**Что:** виртуальная модель = алиас на пул реальных бэкендов с автоматическим выбором (по географии, нагрузке, доступности). Используется для OpenAI-совместимого API: пользователь видит одну модель, балансировщик выбирает бэкенд.

**Acceptance:**
- `POST /v1/chat/completions` с `model="virtual:my-gpt"` маршрутизируется на любой доступный бэкенд с этой моделью.
- Round-robin / least-loaded выбор внутри виртуального пула.

---

## 4. Новые идеи (логично вытекают из B8 и текущей архитектуры)

### 4.1 Pipeline Parallelism (PP) — расширение B8

**Что:** сейчас TP шардирует тензоры одного слоя по нескольким rank'ам. PP шардирует **слои** между workers (layer 0-15 на worker A, layer 16-31 на worker B), с передачей скрытых состояний между ними. Это ортогональная техника, комбинируется с TP (TP внутри слоя + PP между слоями).

**Файлы:**
- `internal/rpccoordinator/pipeline_parallel.go` (новый)
- `internal/rpcworker/kv_store.go` — добавить `PipelineStage(idx) → nextWorkerID` mapping.

**Оценка:** 10–14 дней. Требует реального inference для acceptance.

### 4.2 Expert Parallelism (EP) для MoE моделей

**Что:** Mixture-of-Experts модели (Mixtral 8x7B, Qwen-MoE) имеют несколько expert'ов на каждом слое. EP шардирует expert'ов между rank'ами, all-to-all коммуникация для маршрутизации токенов к правильному expert'у.

**Зависимость:** требует 4.1 (PP) и 3.1 (real ggml) для production.
**Оценка:** 14–20 дней (включая research MoE формата в GGUF).

### 4.3 Disaggregated inference (prefill vs decode)

**Что:** отдельные workers для prefill (тяжёлые GEMM, compute-bound) и decode (memory-bound, latency-sensitive). Позволяет независимо масштабировать.

**Оценка:** 8–12 дней. Требует существенных изменений в KVStore (асинхронная миграция) и rpcworker (отдельные endpoint'ы).

### 4.4 Real-time WebSocket для TP прогресса

**Что:** `webui/tp-pipeline.html` сейчас показывает финальный результат. Можно добавить WebSocket для live-обновлений (какой слой сейчас исполняется, прогресс all-reduce, latency per layer).

**Оценка:** 2–3 дня.

### 4.5 Continuous batching для rpcworker

**Что:** сейчас каждый запрос к rpcworker обрабатывается последовательно. Continuous batching (a-la vLLM) позволяет объединять несколько запросов в один forward pass — драматически увеличивает throughput на GPU.

**Оценка:** 14–20 дней. Требует изменения KV-cache layout и scheduler'а.

---

## 5. UI/UX улучшения (low-hanging fruit)

| # | Фича | Файл | Оценка |
|---|---|---|---|
| 5.1 | **Dark/light theme toggle** для WebUI (сейчас только dark) | `webui/css/`, `webui/js/app.js` | 2–3 ч |
| 5.2 | **Export logs to CSV** в Logs tab | `webui/js/modules/renderers.js`, `internal/api/handlers_logs.go` | 2 ч |
| 5.3 | **Live tail в Logs** через WebSocket вместо polling | `webui/js/modules/api.js`, `internal/api/handlers_logs.go` | 4–5 ч |
| 5.4 | **Notification system** — toast для критических событий (backend down, model load failed) | `webui/js/app.js`, `webui/css/custom.css` | 3 ч |
| 5.5 | **i18n для новых строк** (если добавлены UI labels) | `webui/js/i18n/` | 1 ч / 50 строк |
| 5.6 | **Mobile-responsive layout** для Monitor | `webui/css/custom.css`, `monitor/ui-renderer.js` | 4–6 ч |
| 5.7 | **Health-check UI** — отдельная страница со статусом всех бэкендов и последними ошибками | `webui/health.html` (новый), `internal/api/handlers_health.go` | 6–8 ч |

---

## 6. Observability и debugging

| # | Фича | Файл | Оценка |
|---|---|---|---|
| 6.1 | **OpenTelemetry traces** для inference requests (span per backend, per slice, per TP layer) | `pkg/otel/`, `internal/balancer/proxy_request.go` | 5–7 дней |
| 6.2 | **Distributed profiling** (pprof endpoints на rpcworker + balancer) | `cmd/rpcworker/main.go`, `cmd/balancer/main.go` | 1 день |
| 6.3 | **Alerting rules** — Prometheus alerting rules + webhook интеграция | `deployments/prometheus/` (новый) | 2–3 дня |
| 6.4 | **Grafana dashboards** (импорт через provisioning) | `deployments/grafana/dashboards/` | 3–4 дня |

---

## 7. CI/CD и тестирование

| # | Фича | Файл | Оценка |
|---|---|---|---|
| 7.1 | **GitHub Actions CI** — `go build`, `go test -race`, lint на каждый PR | `.github/workflows/ci.yml` (новый) | 1 день |
| 7.2 | **GPU integration tests** — отдельный workflow с реальным CUDA runner (self-hosted) | `.github/workflows/gpu.yml` | 1–2 дня |
| 7.3 | **Mutation testing** через `go-mutesting` для критичных пакетов (`rpccoordinator`, `balancer`) | `.github/workflows/mutation.yml` | 1 день setup + анализ |
| 7.4 | **Coverage badges** в README (`gocover.io` или codecov) | `README.md` | 30 мин |
| 7.5 | **Benchmarks** для hot-path (TP infer, all-reduce, KV sync) | `internal/rptensor/coordinator_bench_test.go` | 2 ч |

---

## 8. Безопасность

| # | Фича | Файл | Оценка |
|---|---|---|---|
| 8.1 | **mTLS между balancer ↔ rpcworker** (сейчас только X-API-Token) | `internal/rpccoordinator/worker_client.go` | 5–7 дней |
| 8.2 | **Rate limiting per API key** (сейчас global rate-limit) | `internal/api/middleware_ratelimit.go` | 2–3 дня |
| 8.3 | **Audit log** для изменений конфигурации (кто/когда/что поменял) | `internal/api/handlers_audit.go` (новый) | 2–3 дня |
| 8.4 | **Secrets rotation** через env → vault интеграция | `internal/config/secrets.go` | 3–4 дня |

---

## 9. Рекомендуемый порядок (2–3 месяца)

### Месяц 1 (июль 2026) — закрытие долгов + UX

1. **Week 1**: PF-5, PF-6, PF-7 (фикс pre-existing failures) — ✅ DONE Session 13.
2. **Week 2**: ~~R-5 (cocoindex llama_cpp)~~ — ⏭️ SKIPPED (см. секцию 1.1, not-applicable).
   Переходим к **Models tab** (раздел 2).
3. **Week 3–4**: Models tab — Model details + Bulk operations + Filter/Search — 10–15 ч.
4. **Параллельно**: CI (7.1) + coverage badges (7.4) — 1 день.

### Месяц 2 (август 2026) — production-ready

1. **Week 1**: rpc_coordinator режим production (3.2) — 5–8 дней.
2. **Week 2**: UI/UX (5.1–5.7) — 15–20 ч размазано.
3. **Week 3**: Observability (6.1 OTel) — 5–7 дней.
4. **Week 4**: virtual_router режим production (3.3) — 5–7 дней.

### Месяц 3 (сентябрь 2026) — post-1.0 advanced features

1. **Week 1–2**: Pipeline Parallelism (4.1) — 10–14 дней.
2. **Week 3**: WebSocket TP live progress (4.4) — 2–3 дня.
3. **Week 4**: Security mTLS (8.1) — 5–7 дней.

### Месяц 4+ (Q4 2026) — research-grade

- B8.7 real ggml/NCCL (3.1) — 10–15 дней.
- Expert Parallelism для MoE (4.2) — 14–20 дней.
- Disaggregated inference (4.3) — 8–12 дней.
- Continuous batching (4.5) — 14–20 дней.

---

## 10. Приоритеты (TL;DR)

| Приоритет | Что | Почему |
|---|---|---|
| 🔴 P0 | PF-5/6/7 ✅ DONE Session 13 | Стабильность + завершение roadmap R-1…R-7 |
| 🟡 P1 | Models tab gaps, rpc_coordinator production | Переход от прототипа к production |
| 🟢 P2 | UI/UX, observability, continuous batching | UX + production-ready quality |
| ⚪ P3 | B8.7 real ggml, EP, disaggregated, R-5 (cocoindex) | Research / post-1.0 features |

---

## 11. Метрики успеха Q3

- **Все pre-existing failures закрыты** (PF-5/6/7 + любые новые).
- **CI pipeline зелёный** на каждом PR (build + race-тесты + lint).
- **Coverage >75%** для пакетов `rpccoordinator`, `rptensor`, `balancer`.
- **rpc_coordinator и virtual_router режимы работают end-to-end** в docker-compose.
- **Models tab UX ≥ 80%** от webui-gap-analysis P0/P1 требований.
- **WebUI темa toggle** + **i18n для всех новых строк**.

---

## 12. Связанные документы

- `plans/README.md` — текущий roadmap (R-1…R-7, Known Limitations).
- `plans/pre-existing-test-failures.md` — детальные pre-existing failures.
- `plans/archive/webui-gap-analysis.md` — полный gap-список по WebUI.
- `plans/archive/unfinished-code-audit.md` — TODO/FIXME/stub-список в коде.
- `plans/b8-tensor-parallelism-plan.md` — B8 (выполнен, для истории).
- `docs/rpc-coordinator.md` — архитектура RPC + Tensor Parallelism.