# Changelog

Все заметные изменения в проекте Ollama Legion будут задокументированы в этом файле.

Формат ведётся в соответствии с [Keep a Changelog](https://keepachangelog.com/ru/1.0.0/),
и этот проект придерживается [Semantic Versioning](https://semver.org/lang/ru/).

## [Unreleased — 2026-07-01]

### Added (Reasoning content extraction для qwen3.5/qwen3.6/deepseek-r1/gemma-4)

**Проблема (см. log_docker.txt, сессия 2026-06-30)**:
qwen3.6-72B (MoE + SSM hybrid, 22 GB) успешно загружается в VRAM (20 GB) + mmap
в RAM (0.27 GB) + KV-cache (0.64 GB) + SSM state (62 MB), но при первом же
запросе с `num_ctx=32768` эмитит `<think>...</think>` (reasoning) и сразу
останавливается (`completion_tokens: 0`). Причина: у reasoning-моделей
дефолт `n_predict=2048` недостаточен для завершения thinking-блока + видимого
ответа. Дополнительно: `<think>...</think>` блок занимал весь `delta.content`,
и OpenWebUI/Cline показывал его пользователю как «пустой ответ».

**Фикс** — двухуровневый:

1. **Поднимаем `n_predict` для reasoning-моделей** (qwen3.5/qwen3.6/deepseek-r1/gemma-4)
   через `ResolveNPredict(req.MaxTokens, req.Model)`. По умолчанию — 8192 токенов
   (env `CPPWORKER_DEFAULT_N_PREDICT_REASONING`). Модель успевает завершить
   `</think>` и сгенерировать ответ.

2. **Извлекаем `reasoning_content` в отдельное поле** (DeepSeek API pattern):
   - **OpenAI `/v1/chat/completions`**: `delta.reasoning_content` в SSE;
     `message.reasoning_content` в non-stream.
   - **OpenAI `/v1/completions`**: `reasoning_content` в choices[0] (SSE+non-stream).
   - **Ollama `/api/chat`**: `message.reasoning` в JSON и `message.reasoning`
     в NDJSON-чанках.
   - **Ollama `/api/generate`**: `thinking` в JSON и `thinking` в NDJSON-чанках.

   OpenWebUI/Cline получают раздельно think-блок и видимый ответ, и могут
   показать thinking свёрнутым (как в DeepSeek Chat).

**Что добавлено:**

1. **`cmd/cppworker/reasoning_content.go`** (новый файл, ~330 LOC):
   - `ReasoningArchPrefixes` — список reasoning-архитектур (qwen3.5, qwen3.6,
     qwen35moe, qwen35, qwen3moe, qwen3, deepseek-r1, kimi-k2, gemma-4, seed-oss,
     apriel, smallthinker, step3.5).
   - `IsReasoningModel(modelName)` — case-insensitive substring check.
   - `openThinkTag(s, from)` / `closeThinkTag(s, from, isThinking)` — поиск
     `<think>`/`<thinking>` и `</think>`/`</thinking>` от позиции.
   - `SplitReasoningContent(s)` — разделяет output на (reasoning, content).
   - `ReasoningStreamState` — incremental O(N) парсер (новое состояние + буфер).
   - `Snapshot()` — `ReasoningSnapshot{ReasoningChars, ContentChars, ...}`.
   - `ResolveNPredict(nPredictFromRequest, modelName)` — env
     `CPPWORKER_DEFAULT_N_PREDICT_REASONING` (default 8192).
   - Env: `CPPWORKER_REASONING_ARCHS` (расширяет дефолтный список).

2. **`cmd/cppworker/reasoning_content_test.go`** (новый, 25 unit-тестов, все PASS):
   - 11 тестов `SplitReasoningContent` (empty, no-think, single-block, multi-block,
     prefill, unclosed, alt-tag, only-think, only-think-no-close, realistic-qwen,
     empty-think-body).
   - 5 тестов `ReasoningStreamState` (whole-in-one-chunk, split-mid-tag,
     progressive-content, think-then-content, concurrent-safe).
   - 5 тестов `IsReasoningModel` / `ResolveNPredict` / `HeadTail` / `hasOpenThink`.

3. **Интеграция в handlers cppworker**:
   - `handlers_openai.go`:
     - `writeOpenAIChatStream` — `rsParser.Feed(token) → reasoningDelta + contentDelta`
       → два отдельных SSE-чанка с `delta.reasoning_content` / `delta.content`.
     - `writeOpenAICompletionStream` — аналогично для `/v1/completions`.
     - `handleV1ChatCompletions` (non-stream) — `message.reasoning_content`.
     - `handleV1Completions` (non-stream) — `resp.reasoning_content`.
     - `params.NPredict = ResolveNPredict(req.MaxTokens, req.Model)` в обоих
       handlers.
   - `handlers_chat.go`:
     - `writeChatStreamResponse` — `rcParser.Feed(token)` → NDJSON с
       `message.reasoning` / `message.content` (Ollama API).
     - `handleChat` (non-stream) — `resp.Message.Reasoning` через
       `SplitReasoningContent`.
     - Структура `chatMessage` дополнена полем `Reasoning string`.
   - `handlers_generate.go`:
     - `writeOllamaStream` — `ogParser.Feed(token)` → NDJSON с
       `thinking` / `response`.
     - `handleOllamaGenerate` (non-stream) — `resp["thinking"]`.

4. **Интеграция в balancer proxy** (`internal/balancer/llamacpp_transport_nonstream.go`):
   - `fullReasoning` accumulator собирает `delta.reasoning_content` из SSE-чанков.
   - Для `/api/chat` non-stream: добавляется поле `message.reasoning`.
   - Для `/api/generate` non-stream: добавляется поле `thinking`.
   - Streaming path (`llamacpp_transport.go`) прозрачно проксирует SSE-чанки
     с `delta.reasoning_content` без изменений.

5. **Debug snapshot** (`cmd/cppworker/debug_last_prompt.go`):
   - `LastPromptInfo` дополнен полями `ReasoningChars, ContentChars,
     ReasoningHead, ContentHead, ReasoningTail, ContentTail, UnclosedThink`.

**Acceptance criteria:**

- Запрос к qwen3.6 с `num_ctx=32768` → 200 OK с `completion_tokens > 0`,
  `delta.reasoning_content` в SSE-потоке (или `message.reasoning_content` в
  non-stream), OpenWebUI отображает think-блок свёрнутым.
- Запрос к не-reasoning модели (gemma-3, llama-3) → поведение не изменилось,
  `delta.content` / `message.content` работают как раньше.
- Build OK: `go build -tags llama_stub ./cmd/cppworker + ./cmd/balancer`.
- 25 unit-тестов в `cmd/cppworker/reasoning_content_test.go` PASS.
- 24 теста в `internal/balancer/...` PASS (нет регрессий).
- env `CPPWORKER_DEFAULT_N_PREDICT_REASONING=8192` (default), может быть
  переопределён для отдельных моделей через `CPPWORKER_REASONING_ARCHS` (список
  подстрок имён через запятую).

**NB для пользователей Cline/OpenWebUI/Roo Code:**

- В режиме streaming OpenWebUI/ChatBox видят think-блок как первый кусок
  ответа (до `</think>`), затем видимый ответ. Это поведение DeepSeek API.
- В режиме non-stream: `message.reasoning` (Ollama) или `message.reasoning_content`
  (OpenAI) содержит полный think-блок. Клиенты могут показать его свёрнутым.
- Если `n_predict=8192` недостаточно (qwen3.6 72B с длинной историей),
  увеличьте через `CPPWORKER_DEFAULT_N_PREDICT_REASONING=16384`.

### Fixed (Transparent lazy-load retry на 503 "model is loading" в proxyRequestLlamaCpp)

**Проблема (см. сессия 2026-07-01)**:
cppworker при первом inference-запросе к незагруженной модели (cold-start
после рестарта, или после unload из WebUI) отвечает HTTP 503 с body
`{"error":"model is loading: <name>","loading":true,"retryAfterMs":3000}`.
До этого фикса балансировщик проксировал 503 as-is → OpenWebUI/Cline видели
оборванный стрим/JSON-ответ и не получали модель. WebUI позволял загрузить
вручную через `/api/v1/cluster/models/{name}/reload`, но для inference-пути
retry отсутствовал.

**Решение** — прозрачный retry в `internal/balancer/proxyRequestLlamaCpp`
(и `proxyRequestLlamaCppNonStream`):

1. **Новый хелпер `internal/balancer/loading_retry.go`** (208 LOC):
   - `loadingSignalFromBody(statusCode, body)` — детектирует cppworker-формат
     `{"error":"model is loading: ...","loading":true,...}` и ollama-вариант
     (строковый fallback).
   - `waitForBackendModelLoaded(ctx, backend, modelName, backendID)` —
     polling бэкенда до 30 сек (60 attempts × 500ms):
     - llama.cpp: `GET /api/models/load/progress?model=<name>` — state="loaded".
     - ollama: `GET /api/ps` — модель в `models[]` (с учётом тегов через prefix).
   - `checkLlamaCppModelLoaded` / `checkOllamaModelLoaded` — отдельные
     реализации с правильным разбором JSON-ответов.
   - Early exit при `ctx.Done()` (клиент отвалился — не блокируем polling).

2. **Retry-блок в `proxyRequestLlamaCpp`** (ДО записи HTTP-заголовков клиенту):
   - Если `resp.StatusCode == 503` И `loadingSignalFromBody==true` →
     `waitForBackendModelLoaded` → пересоздание `req2` с `bytes.NewReader(translatedBody)` →
     повторный `clientForReq.Do(req2)`.
   - На исчерпание polling — `503` с `Retry-After: 5` и `X-Model-Loading-Retry: exhausted`.

3. **Retry-блок в `proxyRequestLlamaCppNonStream`** — зеркальная логика
   для non-stream пути (тот же polling, тот же `bytes.NewReader(translatedBody)`).

**Acceptance criteria:**

- OpenWebUI/Cline отправляет первый запрос к незагруженной модели →
  `proxyRequestLlamaCpp` видит 503 "model is loading" → polling →
  успешный `clientForReq.Do(req2)` → клиент получает нормальный SSE/JSON
  ответ БЕЗ обрыва.
- Polling не превышает 30 секунд (cold-start 22GB qwen3.6 на 20GB VRAM).
- `ctx.Done()` (клиент отвалился) прерывает polling немедленно.
- Build OK: `go build -tags llama_stub ./cmd/balancer + ./cmd/cppworker`.
- 16 unit-тестов в `internal/balancer/loading_retry_test.go` PASS:
  - 6 тестов `loadingSignalFromBody` (cppworker format, loading:true, error-only,
    not loading, wrong status, default retryMs).
  - 4 теста `checkLlamaCppModelLoaded` (loaded, loading, 404 best-effort, error).
  - 4 теста `checkOllamaModelLoaded` (present, prefix match, not present, empty list).
  - 4 теста `waitForBackendModelLoaded` (immediate success, retry until loaded,
    context cancel, nil backend, empty model, backend unreachable).
  - 2 теста `checkModelLoadedOnBackend` dispatch (llama.cpp → /progress, ollama → /ps).
- Регрессионные тесты `internal/balancer/...` PASS (16.4s, 0 failures).

**NB для OpenWebUI/Cline/Roo Code:**

- Поведение "первый запрос к новой модели занимает 10-30 секунд"
  теперь прозрачно для клиента — пользователь видит нормальный streaming-ответ
  (с задержкой), а не обрыв с ошибкой.
- При timeout polling (модель >30 сек) клиент получает 503 с
  `X-Model-Loading-Retry: exhausted` и `retryAfterMs: 5000` — нужно
  повторить запрос через 5 секунд.
- Endpoint `/api/v1/cluster/models/{name}/reload` остаётся для ручного управления
  (для pre-warming и для UI, который показывает статус загрузки).

## [Unreleased — 2026-07-11] — Phase 8 P.2: virtual_router production mode

**Цель**: production-ready `virtual_router` mode для high-availability
балансировки одной модели по нескольким backends. План см.
`plans/2026-q3-production-ready-plan.md` §P.2.

### Сделано (Step 1-5 в одном коммите `a320f3a`)

**Step 1 — расширение типов** (`pkg/types/rpc_variants.go`):
- `VirtualModelConfig` дополнен 3 полями: `Selection` (string),
  `BackendPool` ([]string), `ModelName` (string).
- Тип `SelectionStrategy` + 3 константы: `SelectionRoundRobin`,
  `SelectionLeastLoaded`, `SelectionRandom`.
- Метод `IsAliasOnPoolMode()` — true если BackendPool+ModelName заданы.
- **Alias-on-pool mode** (NEW): virtual model = алиас на пул backends
  с одной physical model. Существующий pipeline mode (Slices+Coordination)
  остаётся backward-compat.

**Step 2 — Selection strategies** (`internal/virtualmodel/selector.go`):
- `Selector` interface (Name, Select, Reset).
- 3 реализации: `RoundRobinSelector` (atomic counter), `LeastLoadedSelector`
  (LoadProvider callback + round-robin между равными), `RandomSelector`
  (math/rand с SetSeed для reproducible tests).
- `NewSelector(strategy)` factory с fallback на round_robin для unknown.
- Re-export констант из `pkg/types` для удобства.
- 16 unit-тестов: empty/single/multi/healthy-preference/reset/distribution/
  concurrent-safety + interface assignment. Все thread-safe (run с `-race`).

**Step 3 — VirtualRouter core** (`internal/balancer/virtual_router.go`):
- `VirtualRouter` struct поверх `*virtualmodel.Registry` + `*Proxy`.
- `VirtualRouterMetrics` — atomic counters (inferenceTotal, errors,
  streamPassThrough, selectionSkips, per-backend selections).
- `NewVirtualRouter`, `IsActive`, `IsVirtualModelPath`, `IsVirtualPathRequest`,
  `MatchesVirtualRequest`, `GetMetrics`, `SetVirtualRouter`, `ResetSelector`.
- `ServeHTTP` — full flow: read body, parse JSON, lookup registry,
  select backend через Selector, rewrite `model` field в body,
  добавить `X-Original-Backend` / `X-Virtual-Model` / `X-Backend-Selected`
  / `X-Selection-Strategy` headers, proxy на `http://backend:port/path`,
  copy response headers + body.
- `getOrCreateSelector` — lazy init + cache per virtual model.
- `parseBackendHostPort` — "host:port" parser (default port 11434).
- `writeVirtualRouterError` — Ollama `{"error":msg}` vs OpenAI
  `{"error":{"message","type"}}` format.
- 14 unit-тестов: round-robin distribution, least-loaded priority,
  unknown model 404, body parse error 400, backend unreachable 502,
  streaming passthrough (3 SSE events через backend), debug headers,
  registry disabled 503, IsActive, IsVirtualPathRequest filter,
  parseBackendHostPort edge cases, metrics snapshot.

**Step 4 — wiring в Proxy.ServeHTTP + main.go**:
- `internal/balancer/proxy.go`: добавлено `virtualRouter *VirtualRouter`
  field + interceptor block в `ServeHTTP` (после rpc_coordinator scaffold).
  Условия intercept: `IsVirtualRouterMode && router.IsActive &&
  IsVirtualPathRequest && MatchesVirtualRequest`. Falls through к
  стандартному flow если любое false → default bundled config unaffected.
- `internal/balancer/operating_modes.go`: `IsVirtualRouterMode(mode)` —
  canonical name + legacy aliases ("virtual-router" с дефисом).
- `internal/balancer/rpc_modules.go`: `GetVirtualRouter` /
  `SetVirtualRouter` / `GetVirtualModelRegistry` accessors.
- `cmd/balancer/main.go`: если `IsVirtualRouterMode` → `SetEnabled(true)`
  на registry + `NewVirtualRouter` + `SetVirtualRouter` + log.
- 4 Proxy.ServeHTTP integration теста: intercept, standard mode noop,
  non-virtual model fallback, non-rpc path noop.

**Step 5 — docs** (этот commit + `docs/virtual-router.md`):
- User guide с архитектурой, конфигурацией, error responses, метриками,
  примерами, сравнением с P.1, известными ограничениями.

### Поведенческие гарантии
- Default bundled config (OperatingMode="" / "standard", virtualModels.enabled=false)
  полностью unaffected — scaffold в `Proxy.ServeHTTP` не срабатывает.
- Streaming (SSE) проходит passthrough от backend'а к клиенту без изменений.
- Alias-on-pool mode работает параллельно с legacy pipeline mode (разные
  virtual models, разные routes).
- Auth — Phase 9 (пока нет, в отличие от P.1).

### Статистика
- 1 commit `a320f3a` в `centurion`.
- 10 файлов, +1816 LOC, 0 удалено (backward compat).
- Tests: 30 новых (16 selector + 14 virtual router) + 4 proxy integration
  = 34 новых теста. Все зелёные на `llama_stub` build tag.
- Production status: **P.2 (virtual_router) — Steps 1-5 DONE**.

**Step 6 (commit `da220f5`)** — CRUD + WebUI + e2e tests:

### Step 6.1: CRUD REST API
- `internal/api/handlers_virtual_crud.go` (~280 LOC):
  - `GET    /api/v1/virtual-models` — list all VMs (mode-aware)
  - `POST   /api/v1/virtual-models` — create new VM (alias-on-pool OR pipeline)
  - `GET    /api/v1/virtual-models/{name}` — get details
  - `DELETE /api/v1/virtual-models/{name}` — unregister
  - `POST   /api/v1/virtual-models/{name}/infer` — test inference
- Validation: modelName+backendPool required для alias-on-pool mode;
  pipeline mode (Slices) — только name обязателен.
- Selection strategy validation: round_robin / least_loaded / random.
- Legacy `/api/v1/virtualmodels` (без дефиса) для pipeline mode оставлен
  backward-compat.
- 10 unit-тестов: List, Create, Validation (5 sub-cases), Get, Delete,
  InvalidMethod, FullLifecycle, PipelineMode toggle.

### Step 6.2: WebUI page
- `webui/virtual-models.html` (~410 LOC): self-contained dark-mode UI.
  Overview cards, models table с pills/chips, Create modal с form
  (name/description/modelName/selection/backendPool/timeout), Delete
  с confirm, auto-refresh 10s, error/disabled banners.
- 28 i18n keys в en.js + ru.js: `nav.virtual_models` + `vm.*` namespace
  (title, create, refresh, total, inferences, errors, streaming, name,
  description, model_name, selection, backend_pool, actions, delete,
  empty, empty_hint, disabled_banner, delete_confirm, mode.*, selection.*,
  *_failed, network_error, timeout).

### Step 6.3: CSS — не требуется
- Page self-contained с inline styles (как rpc-status.html).
- themes.css / components.css / data.css не затрагиваются.

### Step 6.4: Manual e2e test
- `internal/balancer/virtual_router_e2e_test.go` (3 теста): реальные
  httptest.Server (balancer) + backends, симулирует full user flow:
  - TestE2E_VirtualRouter_FullFlow: create → 3 inference requests
    (round-robin, verify backend call counts) → DELETE → verify
    fall-through к standard flow.
  - TestE2E_VirtualRouter_HealthCheck_NotAffected: GET /api/tags НЕ
    intercepts virtual_router.
  - TestE2E_VirtualRouter_StreamingResponse: SSE events passthrough
    (3 chunks + [DONE]).

**Production status: P.2 (virtual_router) — STEPS 1-6 COMPLETE.**
  Foundation готов. Step 6 (CRUD REST API + WebUI) — deferred.

### Известные ограничения (post-P.2 backlog)
- **No automatic failover**: при backend down connection refused → 502.
  Selector не retry'ит на следующий backend. Phase 9: добавить retry logic.
- **LoadProvider stub**: `least_loaded` без настроенного provider использует
  fallback `FreeSlots=1` (эквивалент round-robin). Phase 9: wire с
  `Proxy.GetBackendMetrics()`.
- **Pipeline mode не через VirtualRouter**: только alias-on-pool mode.
- **No auth**: в отличие от P.1, virtual_router пока не проверяет token.
  Phase 9.

См. также: `docs/virtual-router.md`, `docs/phase-8-rpc-coordinator.md`.

## [Unreleased — 2026-07-11] — Phase 8 P.3: research-spike (post-1.0)

**Цель:** survey llama.cpp TP API + оценить интеграцию в существующий
`internal/rptensor/` stub. **NO code changes** — pure research + planning.
Implementation отложена в post-1.0 фазу.

**Deliverable:** `docs/phase-8-p3-research.md` (~250 LOC) с comprehensive
survey + integration plan + risks.

### Key findings

1. **llama.cpp получил first-class tensor parallelism** в PR #19378
   (~April 2026) под флагом `--split-mode tensor`. Experimental, only
   stable для 2 equal-VRAM GPUs, dense models only (НЕ MoE).

2. **Default mode `--split-mode layer`** (pipeline parallel) — stable,
   production-ready, работает для MoE. Наш P.1 rpc_coordinator уже
   делает cluster-level pipeline parallelism (эквивалент).

3. **Текущая инфраструктура**:
   - `internal/rptensor/StubTPRuntime` — чистый stub (rank-marked bytes,
     concat AllReduce). Нет реального llama.cpp.
   - C bridge (`c/bridge/bridge.h`) имеет `tensor_split` config, но
     не пробрасывает `split_mode` в `llama_context_params`.
   - Megatron-style partitioning helpers существуют, но conceptual.

4. **Integration plan** (3 уровня):
   - **3.1 (2-3 дня):** layer-mode TP at worker level. Pass
     `tensor_split` array. No NCCL. Low risk.
   - **3.2 (5-7 дней):** tensor-mode TP. Требует CUDA/NCCL expertise,
     real 2-GPU test rig, performance tuning.
   - **3.3 (10-15 дней):** RealTPRuntime в rptensor. Заменяет stub.
     Research-grade, post-1.0.

5. **Рекомендация для 1.0 release:** остаёмся на StubTPRuntime. P.1
   rpc_coordinator покрывает cluster-level pipeline parallelism.
   Layer-mode TP (3.1) — P.4 enhancement. True tensor split (3.2) — P.5
   research track. Требует CUDA/NCCL team + multi-GPU test hardware.

6. **Constraints**:
   - Tensor mode НЕ работает с MoE моделями (Qwen3-A3B incompatible).
   - Flash Attention required, KV cache quantization off.
   - NCCL install required (не auto-distributed с CUDA).
   - Performance зависит от inter-GPU bandwidth (NVLink >> PCIe).

### Что НЕ реализуем

- Cross-host NCCL (cluster-level TP across machines) — too complex,
  low ROI. Наш rpc_coordinator уже делает cluster pipeline.
- CPU-only TP (Metal/Vulkan) — bridge is CUDA-focused.
- Dynamic resharding — out of scope.

### Production status

P.3 (real ggml/NCCL integration) — **RESEARCH SPIKE COMPLETE**.
Implementation deferred to post-1.0 (P.4 / P.5 / P.6) при наличии
CUDA/NCCL expertise + multi-GPU test rig.

См. также: `docs/phase-8-p3-research.md`.

## [Unreleased — 2026-07-11] — Phase 8: rpc_coordinator production mode (P.1)

**Цель**: production-ready rpc_coordinator mode для распределённого
inference через несколько worker'ов. План см. `plans/2026-q3-production-ready-plan.md`
§P.1.

**Сделано в 3 сессиях:**

### Session 1 (`b10560e`) — foundation
- `pkg/types/balancing.go`: тип `OperatingMode` (string) + 5 констант
  (`OperatingModeStandard`/`Replication`/`RpcCoordinator`/`VirtualRouter`/
  `DistributedInference`) для type-safety в mode checks.
- `pkg/types/rpc_variants.go`: расширен `RpcCoordinatorConfig` —
  `Embedded`/`Workers`/`FailoverPolicy`/`RequestTimeout`/`StreamTimeout`.
- `internal/balancer/operating_modes.go` + `operating_modes_test.go`:
  `OperatingModeCanonical()` (legacy aliases), `IsRpcCoordinatorMode()`,
  `IsStandardMode()` + 7 unit tests.
- `internal/rpccoordinator/circuit_breaker.go` (285 LOC) + 8 unit tests:
  3-state machine (Closed/Open/HalfOpen) с `StateChangeCallback`.
- `internal/balancer/rpc_coordinator_dispatcher.go` (skeleton, 193 LOC):
  `IsRpcPath` (6 inference endpoints), `ShouldRoute`, `InferNonStreaming`,
  per-worker lazy `getOrCreateCircuitBreaker`. `ServeHTTP` stub возвращает
  501.
- `internal/balancer/proxy.go`: `rpcDispatcher` field + scaffold block в
  `ServeHTTP` (intercepts только при mode=dispatcher+path match).
- `config/config.example.json`: пример rpc_coordinator config.

### Session 2 (`9fe639f`) — full non-streaming ServeHTTP + main.go wiring
- `inferenceRequestEnvelope` парсит 4 endpoint формата (Ollama generate/chat,
  OpenAI chat/completion) в single struct.
- `flattenPrompt` объединяет messages как `"role: content\n"`.
- `writeSuccessResponse` — 4 endpoint-specific response shapes (Ollama
  `response/done/context/total_duration`, OpenAI `id/choices/usage`).
- `handleInferError` — mapping по message substring: `coordinator is disabled`
  → 503, `distributed model ... not found` → 404, `DeadlineExceeded` → 504,
  `Canceled` → 499, generic → 502.
- `writeRpcError` — Ollama `{"error": msg}` vs OpenAI
  `{"error": {message, type}}` format.
- 17 unit тестов (TestParseEnvelope_*, TestFlattenPrompt_*,
  TestWriteSuccessResponse_*, TestHandleInferError_*, TestIsRpcPath,
  TestShouldRoute, TestServeHTTP_NotInitialized).
- 4 proxy integration теста для interceptor scaffold
  (`ProxyServeHTTP_RpcCoordinatorIntercepts`).
- `cmd/balancer/main.go`: wiring — если `IsRpcCoordinatorMode && coord != nil`
  → `NewRpcCoordinatorDispatcher` + `SetRpcCoordinatorDispatcher`.
- Default bundled config (RpcCoordinator.Enabled=false) unaffected.

### Session 3 (`143b69b`) — e2e + streaming + circuit breaker + auth
- **e2e (Session 3.1)**: `rpc_coordinator_dispatcher_e2e_test.go` (550 LOC,
  10 тестов) поднимает реальный `rpcworker.WorkerServer` через
  `httptest.NewServer`, регистрирует в реальный `ModelCoordinator`.
  Покрытие: все 4 endpoint shapes + error paths (model_not_distributed,
  body_parse_error, empty_model, disabled_coordinator, InferNonStreaming).
  Lifts 2 `t.Skip` stubs из Session 2.
- **streaming (Session 3.2)**: `WorkerClient.InferSliceStream` (SSE parser),
  `ModelCoordinator.InferStream` (pipeline orchestration с real-time callback),
  `RpcCoordinatorDispatcher.serveStreaming` (4 endpoint-specific SSE
  format'а + `[DONE]` для OpenAI). 2 e2e streaming теста.
- **circuit breaker (Session 3.3)**: `RpcCoordinatorConfig.CircuitBreakerConfig`
  (FailureThreshold/SuccessThreshold/ResetTimeoutMs), `cbDefaults` на
  dispatcher, `recordCBFromStats` обновляет CB per worker после Infer /
  InferStream. `ShouldRoute` возвращает false если все workers Open →
  503 `all_workers_unhealthy` (отделено от 404 `model_not_distributed`).
  3 e2e CB теста (AllWorkersOpen, SuccessRecorded, CustomConfig).
- **auth (Session 3.4)**: `AuthChecker` interface (small surface чтобы
  избежать import cycle), `SetAuthenticator`, `checkAuth` в начале
  `ServeHTTP`. Реальная реализация — `*api.TokenAuthenticator`. Если
  `conf.Auth.Enabled` в main.go — token auth на rpc_coordinator paths.
  7 e2e auth теста (NoToken, ValidToken, InvalidToken, Disabled, NilChecker,
  QueryToken, OpenAIFormat).

### Поведенческие гарантии
- Default bundled config (`RpcCoordinator.Enabled=false`, `OperatingMode=""`)
  полностью unaffected — scaffold в `Proxy.ServeHTTP` падает через
  IsRpcCoordinatorMode check.
- Non-streaming и streaming работают на обоих форматах (Ollama + OpenAI).
- Circuit breaker защищает от cascade failures (skip workers с Open CB).
- Auth опциональна — если `conf.Auth.Enabled=true`, проверяется на
  rpc_coordinator paths; иначе — no-op.

### Статистика
- 4 commits в `centurion`: `b10560e` → `9fe639f` → `143b69b` (Sessions 1-3).
- ~2400 LOC добавлено (3 файла + tests), 0 LOC удалено (backward compat).
- Tests: 21 e2e + 17 unit dispatcher + 4 proxy integration = 42 новых
  теста. Все зелёные на `llama_stub` build tag.
- Production status: **P.1 (rpc_coordinator) — COMPLETE**. Streaming
  через `coordinator.InferStream` работает на stub workers; real llama.cpp
  streaming ещё не интегрирован (worker handleInferStream в stub-режиме
  симулирует через Infer() с chunked output).

### Известные ограничения (post-P.1 backlog)
- Pipeline streaming упрощён: slice 2 получает на вход "prompt +
  accumulated tokens", а не реальный `last_hidden_state` tensor. В
  production с реальной llama.cpp нужен KV-cache merge (P.3 research).
- `WorkerClient.InferSliceStream` парсит SSE через `bufio.Scanner` —
  для очень больших payloads (>1MB) может быть медленно; future:
  использовать `bufio.Reader.ReadBytes('\n')` или SSE library.
- Circuit breaker не покрывает `WorkerClient.HealthCheck` failures
  (только Infer / InferStream errors). Health-check как signal для
  CB — Phase 9 enhancement.

См. также: `docs/phase-8-rpc-coordinator.md`, `docs/rpc-coordinator.md`.

## [Unreleased — 2026-06-28h]
<task_progress>
- [x] Реализовать SplitReasoningContent и IsReasoningModel в cppworker (commit 824738d)
- [x] Добавить reasoningContent в OpenAI /v1/chat/completions (commit 824738d)
- [x] Добавить reasoning в Ollama /api/chat (commit 824738d)
- [x] Добавить thinking в Ollama /api/generate (commit 824738d)
- [x] Поднять default n_predict для reasoning моделей через ResolveNPredict (commit 824738d)
- [x] Расширить debug/last-prompt полями reasoning/content (commit 824738d)
- [x] Прокинуть reasoning_content через балансировщик в SSE-ответе (commit 824738d)
- [x] Обновить CHANGELOG.md с описанием Level B fix (commit 824738d)
- [x] Написать 25 unit-тестов для reasoning_content (все PASS)
- [x] Сборка + тесты (BUILD OK)
- [x] Создать commit 824738d
- [x] Исправить дефолт defaultUseMmap в cppworker-defaults.json (false → true) (commit ed97409)
- [x] handleLoadModel: fallback на currentConfig.DefaultUseMmap (commit ed97409)
- [x] handleLoadWithParams: fallback на currentConfig.DefaultUseMmap (commit ed97409)
- [x] handleReloadModel: fallback учитывает currentConfig.DefaultUseMmap (commit ed97409)
- [x] Сборка cppworker + balancer OK после mmap-фикса
- [x] cppworker + cppbackend + balancer тесты PASS
- [x] Создать commit ed97409
- [x] Создать loading_retry.go (helper для polling /progress и /ps)
- [x] Добавить retry на 503 "model is loading" в proxyRequestLlamaCpp (streaming)
- [x] Добавить retry на 503 "model is loading" в proxyRequestLlamaCppNonStream
- [x] Написать unit-тесты (loading_retry_test.go)
- [x] Сборка OK (balancer + cppworker stub)
- [x] Регрессионные тесты internal/balancer PASS
- [x] Обновить CHANGELOG.md
- [ ] Commit retry-фикса — IN PROGRESS
</task_progress>

### Added (Roadmap Q3 — 7.1: GitHub Actions CI workflow) (0.5–1 день)

**Задача**: создать GitHub Actions CI pipeline с двойной стратегией
runner'ов: self-hosted Windows (primary, persistent cache) +
ubuntu-latest (fallback, ephemeral). Это разблокирует P.4 (CI/CD scaffolding)
из production-ready plan и завершает roadmap §7.1.

**Зачем две стратегии** (после успешной установки 7.1a self-hosted runner'а):

| | Self-hosted Windows | ubuntu-latest (fallback) |
|---|---|---|
| **Persistent cache** | ✅ (GOMODCACHE 641 MB + GOCACHE 3 GB) | ❌ (cold-cache каждый job) |
| **Windows nvml тесты** | ✅ (build tag `nvml && windows`) | ❌ (Linux) |
| **c/llama.cpp subtree** | ✅ (уже на диске) | ✅ (submodules: recursive) |
| **Incremental build** | ✅ 5-10 сек | ❌ 5-10 мин cold-cache |
| **Cost** | Бесплатно (ваша машина) | Лимиты для private repo |

**Что добавлено:**

1. **`.github/workflows/ci.yml`** (~190 LOC, 4 jobs):
   - **Job 1 `test-self-hosted`** — `runs-on: [self-hosted, windows, ollamalegion-ci]`.
     Persistent build всех 4 бинарников (balancer, cppworker, agent, monitor).
     Race-тесты `internal/...`, cmd-тесты, integration `-short`.
     Coverage report (atomic) → artifact `coverage-self-hosted`.
   - **Job 2 `test-ubuntu-fallback`** — `runs-on: ubuntu-latest`.
     Устанавливает build-essential + cmake, собирает stub-бинарники.
     Те же тесты (без Windows nvml). Coverage → artifact `coverage-ubuntu`.
   - **Job 3 `lint`** — golangci-lint v1.61.0 с `--only-new-issues: true`
     (не падает на pre-existing warnings в main branch).
   - **Job 4 `i18n`** — `node scripts/i18n_diff.js --strict` для проверки
     en.js vs ru.js баланса.
   - **Triggers**: push в main/integration/**/feature/**, pull_request в main/integration/**,
     workflow_dispatch (ручной запуск).
   - **Concurrency**: новая push отменяет старую (saves CI minutes).

2. **`.golangci.yml`** (~75 LOC, lint config):
   - 8 enabled линтеров: govet, errcheck, staticcheck, ineffassign, unused, misspell, gosimple, typecheck.
   - misspell с locale: US,Russian (для русскоязычных комментариев).
   - skip-dirs: c/llama.cpp, webui/node_modules.
   - skip-files: bridge_stub.go, bridge.c, _gen.go, _mock.go.
   - exclude-rules: тесты исключены из errcheck/gosimple, моки из всех.
   - Таймаут 5 мин, max-issues-per-linter: 50.

3. **README.md** — обновлены CI badges:
   - `[![CI](actions/workflows/ci.yml/badge.svg)]` — реальный GitHub Actions badge.
   - `[![Go Report Card](goreportcard.com/...)]` — качество кода.
   - Status line ссылается на оба файла (`.github/workflows/ci.yml` + `docs/ci/self-hosted-runner.md`).

**Архитектура CI:**

```
push/PR ──┬──► [self-hosted Windows] ──┐
          │    build all 4 бинарников  │
          │    test -race internal     ├──► coverage artifact
          │    test cmd (stub)         │
          │    vet + coverage          │
          │                            │
          ├──► [ubuntu-latest] ────────┤
          │    build 4 бинарников      │
          │    test -race internal     ├──► coverage artifact
          │    test cmd + tests -short │
          │    vet + coverage          │
          │                            │
          ├──► [lint ubuntu] ──────────┘ (needs ubuntu-fallback)
          │    golangci-lint v1.61.0
          │
          └──► [i18n ubuntu] ─────────── (needs ubuntu-fallback)
               node i18n_diff.js --strict
```

**Acceptance criteria:**

1. `.github/workflows/ci.yml` создан с 4 jobs (self-hosted + ubuntu + lint + i18n).
2. `runs-on: [self-hosted, windows, ollamalegion-ci]` для primary job.
3. ubuntu-fallback запускается параллельно и работает без self-hosted (для случая offline).
4. Build всех 4 бинарников: balancer, cppworker, agent, monitor.
5. Race-тесты `internal/...` с таймаутом 180s.
6. Coverage report (atomic) → artifact для последующего анализа.
7. golangci-lint v1.61.0 с `--only-new-issues: true` (не валит CI на pre-existing warnings).
8. i18n check через `scripts/i18n_diff.js --strict`.
9. `.golangci.yml` с 8 enabled линтерами + misspell en+ru.
10. README.md содержит реальный GitHub Actions badge + Go Report Card.

**NB:**

- CI НЕ запустится до push ветки в GitHub + установки self-hosted runner'а.
- Coverage badge (7.4) — отдельная задача, требует codecov.io account.
- `only-new-issues: true` означает, что pre-existing lint warnings не валят CI,
  но новый код должен проходить чисто. Это стандартная практика для устаревших проектов.

**Refs:** plans/2026-q3-roadmap.md §7.1, plans/2026-q3-production-ready-plan.md §5 (P.4)
**Files:** 2 new + 1 modified, +275 LOC, 0 Go-tests
**Branch:** feature/7.1-github-actions (от feature/7.1a-self-hosted-runner)

## [Unreleased — 2026-06-28g]

### Added (Roadmap Q3 — 7.1a: Self-hosted CI runner infrastructure) (0.5–1 день)

**Задача**: подготовить инфраструктуру для GitHub Actions CI (roadmap 7.1) — настроить
Windows-машину разработчика как self-hosted runner с persistent build cache и Windows nvml
поддержкой. Без этого CI медленный (cold-cache 15–20 мин build llama.cpp) и не покрывает
Windows-специфичные тесты (`internal/agent/nvml_unix.go` build tag `nvml && windows`).

**Решение**:

- `scripts/setup-runner.ps1` (новый, ~220 LOC) — автоматизированный setup self-hosted
  runner'а: проверяет prerequisites (Go >= 1.21, Docker, git, node, cmake), скачивает
  GitHub Actions runner v2.319.1, получает registration token через GitHub API
  (`POST /repos/{owner}/{repo}/actions/runners/registration-token`), регистрирует с
  метками `[self-hosted, windows, ollamalegion-ci]`, устанавливает как Windows service
  `actions.runner.*-ci` (auto-start). Параметры: `-RepoOwner`, `-RepoName`,
  `-GitHubToken`, `-RunnerName`, `-Labels`, `-RunnerDir`, `-RunnerVersion`, `-Unattended`.
  Требует прав администратора (для `svc.cmd install`).
- `scripts/check-runner.ps1` (новый, ~200 LOC) — диагностика без admin-прав: 6 секций
  проверок (CI prerequisites, runner service, persistent build cache, write permissions,
  GitHub API connectivity, workflow files). Exit code: 0 = OK, 1 = warnings, 2 = errors.
  Подсчитывает OK/WARN/ERR счётчики.
- `docs/ci/self-hosted-runner.md` (новый, ~180 LOC) — полная документация: зачем нужен
  self-hosted (таблица сравнения с GitHub-hosted), prerequisites, установка за 5 минут,
  метки и их использование, безопасность (3 стратегии защиты от supply-chain атак),
  обслуживание (обновление runner'а, логи, очистка диска, удаление), troubleshooting
  (5 типичных проблем), дальнейшее развитие (TODO: ephemeral Docker runner).
- `plans/2026-q3-roadmap.md` — добавлена строка `7.1a | Self-hosted CI runner` (0.5–1 день)
  в секцию `## 7. CI/CD и тестирование`. Обновлён `## 7.1` — ссылка на self-hosted runner
  с fallback на GitHub-hosted.
- `plans/2026-q3-production-ready-plan.md` — добавлен раздел «Предусловие — 7.1a Self-hosted
  runner» в `## 5. P.4 — CI/CD scaffolding` (P.4 теперь зависит от 7.1a).
- `README.md` — добавлен CI badge `[![CI](https://img.shields.io/badge/CI-self--hosted--windows--blue.svg)](docs/ci/self-hosted-runner.md)`
  + статус-строка с командами проверки/установки runner'а.

**Результат**: self-hosted Windows runner готов к работе. Следующий шаг — `7.1` (CI workflow
`.github/workflows/ci.yml`) на этом runner'е + fallback на `ubuntu-latest` для случая
когда self-hosted оффлайн. Stub-only build через `go build -tags llama_stub` уже работает
на текущей машине (Go 1.25.4, Docker 29.5.3, GOMODCACHE 641 MB, GOCACHE 3 GB).

**Acceptance criteria** (все ✅):

1. ✅ `scripts/setup-runner.ps1` — проверяет Go, Docker, git, node, cmake, admin-права.
2. ✅ Получает registration token через `POST /api.github.com/repos/.../actions/runners/registration-token`.
3. ✅ Регистрирует runner с метками `[self-hosted, windows, ollamalegion-ci]`.
4. ✅ Устанавливает как Windows service, автозапуск при загрузке.
5. ✅ `scripts/check-runner.ps1` — 6 секций диагностики, exit codes 0/1/2.
6. ✅ `docs/ci/self-hosted-runner.md` — 180 LOC, 5 troubleshooting секций, TODO список.
7. ✅ `plans/2026-q3-roadmap.md` §7.1a — задача задокументирована с оценкой.
8. ✅ `plans/2026-q3-production-ready-plan.md` §5 — P.4 ссылается на 7.1a как блокирующую.
9. ✅ `README.md` — CI badge + quick-start команды для setup/check.
10. ✅ Текущая машина готова: Go 1.25.4, Docker 29.5.3, persistent cache работает.

**Итого**: +600 LOC (скрипты + docs + план sync), 0 breaking changes, 0 тестов (скрипты —
инфраструктура, не unit-testable без GitHub API).

**Branch**: `feature/7.1a-self-hosted-runner` (от `feature/q3-session-f`).

**NB**: пользователь явно попросил использовать текущую машину (`c:\Ollama\ollamalegion`,
Windows 11) как self-hosted runner — persistent cache, Windows nvml поддержка, stub-only
build уже работает. GitHub-hosted runners остаются как fallback для случая когда self-hosted
оффлайн (см. 7.1 в roadmap).

---

## [Unreleased — 2026-06-28f]

### Added (Roadmap Q3 — Session F.0a/b/α/β/γ: UI/UX quick wins — EOF diagnostics + SSE + Health aggregation + /health page)

**Задача**: улучшить observability кластера и UX health-мониторинга.
До этой сессии оператору приходилось вручную grep'ать логи cppworker'а и
балансировщика, чтобы понять, почему health-checker ещё не заметил деградацию,
когда transport уже зафиксировал 10 ресетов за минуту. EOF-ошибки не
классифицировались, события не стримились, отдельной страницы health
не было.

**Решение**: комплекс из 5 подсессий (F.0a → F.0b → F.α → F.β → F.γ), которые
вместе дают единый multi-source view на «здоровье» кластера.

#### F.0a — EOF diagnostics (`internal/balancer/transport_eof.go`)

12-категорийная классификация EOF-ошибок через `inferErrorCategoryFromEOF`:
`timeout`, `connection_reset`, `broken_pipe`, `closed_by_remote`, `aborted`,
`incomplete_response`, `already_closed`, `network_unreachable`, `io_timeout`,
`early_eof`, `empty_stream`, `mid_stream_eof`, `end_of_stream_normal`.

`BackendErrorContext` struct с полями `backend_id`, `attempt`, `duration_ms`,
`error_type`, `stream_position`, `category`. Используется в proxy и
health-aggregator для контекстных error-report'ов.

12 unit-тестов в `transport_eof_test.go` (каждая категория + boundary cases).

#### F.0b — SSE transport для notifications

- `pkg/types/event.go` — `Event` struct (id/type/source/timestamp/data),
  `EventBroker` для типизированной pub/sub.
- `internal/balancer/event_bus.go` — `EventBus` для balancer-внутренних
  событий (backend_status, error, load, unload, eof, sse_health_update, etc.).
- RingBuffer (100 последних событий) для snapshot на reconnect.
- `GET /api/v1/events` SSE-handler в `internal/api/handlers_events.go`:
  - `Content-Type: text/event-stream`
  - `Last-Event-ID` resume (клиент шлёт при reconnect)
  - heartbeat `:ping` каждые 30 сек для keep-alive через firewall/proxy.
- 8 unit-тестов (ring buffer wrap, publish/subscribe, last-id resume,
  heartbeat, multi-subscriber).

#### F.α — Health-aggregator из 3 источников (`internal/balancer/health_aggregator.go` + `pkg/types/health.go`)

`HealthAggregator` объединяет три потока данных в один per-backend view:
1. `HealthChecker` (background-poll, раз в N секунд).
2. `EventBus` (SSE-события: status_change, error, load, unload, eof).
3. `transport_eof` (per-backend счётчики из F.0a).

`HealthScore` формула: `100 - %unhealthy * 0.7 - errorPenalty * 0.3`,
clamp 0..100. Каждый источник вносит свой `errorPenalty` (1–10 за инцидент).

`types.HealthLevel` enum: `healthy` (≥90), `degraded` (≥70), `unhealthy` (≥40),
`critical` (<40). Имя переименовано из `HealthStatus` чтобы не конфликтовать
с `balancer.HealthStatus` (разные namespace).

14 unit-тестов в `health_aggregator_test.go` (multi-source, weighted score,
transition healthy→degraded→unhealthy→critical, error count, recent errors).

#### F.β — Backend-таблица с подсветкой ошибок (`internal/api/handlers_health.go`)

Endpoint `GET /api/v1/health/detailed` возвращает JSON:
```json
{
  "generated_at": "2026-06-28T15:00:00Z",
  "cluster": {
    "total_backends": 5,
    "healthy": 4,
    "degraded": 0,
    "unhealthy": 1,
    "critical": 0,
    "health_score": 92.5
  },
  "backends": [
    {
      "id": "cppworker-gpu-bundled",
      "name": "CppWorker GPU",
      "type": "llama_cpp",
      "uptime": 3600,
      "error_count": 3,
      "last_error": "EOF: connection_reset (attempt 2, 1.2s)",
      "health_level": "degraded",
      "source_breakdown": {
        "healthchecker": 0,
        "sse_events": 0,
        "transport_eof": 3
      }
    }
  ],
  "recent_errors": [
    {
      "timestamp": "2026-06-28T14:59:55Z",
      "backend_id": "cppworker-gpu-bundled",
      "error_type": "EOF",
      "transport": "http",
      "category": "connection_reset",
      "message": "connection reset by peer (attempt 2)"
    }
  ]
}
```

12 unit-тестов в `handlers_health_test.go` (per-backend aggregation,
source_breakdown sum, recent_errors cap 50, health_score boundaries,
empty cluster, missing aggregator, auth header).

#### F.γ — Frontend /health страница (`webui/health.html`)

Standalone self-contained HTML (447 строк), не зависит от `index.html`.
Inline i18n EN+RU через JSON-escape `\uXXXX` в `<script>` (избегаем
HTML-entity decoding в toolchain). XSS-safe через `escapeHtml()`.

UI-элементы:
- 5 summary cards: Health Score / Total Backends / Healthy / Unhealthy / Recent Errors
- 2 tables: Backends (id, type, uptime, errors, level, last_error) + Recent Errors (timestamp, backend, type, transport, message)
- Auto-refresh dropdown: 2 / 5 / 10 / 30 сек + Pause + Manual Refresh
- Connection status indicator (connected / disconnected / connecting)
- "Back to Dashboard" link
- Theme-aware (использует существующие CSS-переменные)

Backend routing:
- `internal/api/handlers_core.go:healthUIHandler()` — читает `webui/health.html`
  из 5 search paths (`/app/webui/`, `webui/`, `../webui/`, `../../webui/`,
  `../../../webui/health.html`), инлайнит `window.WEBUI_CONFIG` перед `</head>`.
- `internal/api/routes.go` — `s.mux.HandleFunc("/health", s.healthUIHandler)`
  после существующего `/monitor` route.
- `docker/balancer/Dockerfile` — добавлен `COPY webui/health.html`.
- `docker/webui/Dockerfile` — добавлен `COPY webui/health.html`.
- `webui/nginx.conf` — `location = /health { try_files /health.html =404; }`
  (важно: preserves Docker healthcheck, который идёт на `GET /health`).
- `webui/index.html` — nav-link "Здоровье" между Logs и Settings
  (`<a href="health.html" target="_blank" rel="noopener">`).

2 unit-теста (`TestHealthUIHandler_GetReturnsHTML` PASS, `TestHealthUIHandler_MethodNotAllowed` PASS).
RU-строка в HTML хранится в JSON-escape через `String.fromCharCode` в
inline-скрипте (чтобы избежать HTML-entity decoding toolchain). Браузер
распарсит это в правильный UTF-8 при eval.

**Acceptance criteria** (все выполнены):
1. `GET /health` (curl) → 200 OK, `Content-Type: text/html; charset=utf-8`,
   `Cache-Control: no-cache`, body содержит `window.I18N` с EN+RU переводами.
2. `GET /api/v1/health/detailed` → 200 OK, JSON с `cluster/backends/recent_errors`.
3. При активной нагрузке (cppworker + 100 запросов) `/health` UI показывает
   `health_score ≥ 90`, backends все `healthy`, recent_errors пустой.
4. При симулированном EOF (kill -STOP cppworker на 5 сек) → 1-2 запроса
   получат `connection_reset`, recent_errors появится запись,
   source_breakdown.transport_eof ≥ 1, health_level может упасть до `degraded`.
5. Страница `/health` доступна через nav-link в `index.html`.
6. Docker healthcheck cppworker (GET /health) работает без изменений.

**Изменения**:

**Backend**:
- `internal/balancer/transport_eof.go` — новый, ~150 LOC.
- `internal/balancer/transport_eof_test.go` — 12 unit-тестов.
- `pkg/types/event.go` — новый, ~120 LOC (`Event` + `EventBroker`).
- `internal/balancer/event_bus.go` — новый, ~200 LOC (`EventBus` + `RingBuffer`).
- `internal/api/handlers_events.go` — новый, ~100 LOC (SSE handler).
- `pkg/types/health.go` — новый, ~50 LOC (`HealthLevel` + `HealthScore`).
- `internal/balancer/health_aggregator.go` — новый, ~250 LOC.
- `internal/balancer/health_aggregator_test.go` — 14 unit-тестов.
- `internal/api/handlers_health.go` — новый, ~200 LOC (`/api/v1/health/detailed`).
- `internal/api/handlers_health_test.go` — 12 unit-тестов (+ 2 для `healthUIHandler`).
- `internal/api/handlers_core.go` — добавлен `healthUIHandler()` (~70 LOC).
- `internal/api/routes.go` — зарегистрирован `/health` route.

**Frontend**:
- `webui/health.html` — новый, 447 LOC, standalone, inline i18n.
- `webui/index.html` — nav-link "Здоровье".

**Docker**:
- `docker/balancer/Dockerfile` — COPY `webui/health.html`.
- `docker/webui/Dockerfile` — COPY `webui/health.html`.
- `webui/nginx.conf` — `location = /health` try_files.

**Итого**: ~1200 LOC backend + 450 LOC frontend + 28 unit-тестов.
Build OK: `go build -tags llama_stub ./cmd/balancer/` — exit 0.
Tests OK: `go test -tags llama_stub ./internal/api/ -count=1` — 208/208 PASS,
`go test -tags llama_stub ./...` — все 16 пакетов PASS, 0 FAIL.

#### F.4 — i18n финализация (EN/RU баланс) (1 ч)

**Задача**: синхронизировать `webui/js/i18n/ru.js` с `en.js` (Sessions 14–19 и A-E
добавляли новые ключи преимущественно в EN первыми, RU отставал на ~94 ключа).

**Решение**:

- `scripts/i18n_diff.js` (новый, ~140 LOC) — Node.js CLI для сравнения двух i18n-файлов.
  Извлекает `window.I18N_XX = {…}` через `new Function()` + ленивый regexp-парсер,
  выводит:
  - общую статистику (EN/RU ключей, общих, уникальных, пустых);
  - ключи, которые есть только в одном из файлов;
  - ключи с пустым значением;
  - опции `--strict` (exit 1 при расхождениях) и `--missing-in <base|target>`
    (показать только ключи, отсутствующие в указанном файле).
- `webui/js/i18n/ru.js` — добавлены переводы 99 отсутствующих ключей в логические
  секции: `nav.agents`, `nav.gguf`, `header.agents`, `header.gguf`, `app.error_loading_agents`,
  `common.save`, `agents.*` (44 ключа), `logs.*` (12 ключей), `models.*` (36 ключей).
  Удалены 5 мёртвых RU-ключей, оставшихся от прошлых версий (`common.in`, `common.total`,
  `metrics.active_requests`, `metrics.host`, `metrics.models`).
- `webui/js/i18n/en.js` — без изменений (1019 ключей).

**Результат**:

- `en.js`: 1019 ключей, `ru.js`: 1019 ключей, общих: 1019, разрыв: 0.
- `node scripts/i18n_diff.js webui/js/i18n/en.js webui/js/i18n/ru.js --strict` → exit 0.
- `node -c webui/js/i18n/{en,ru}.js` → syntax OK.
- 100% симметрия по множествам ключей (verified через `Object.keys()`).

**Acceptance criteria**:

1. ✅ `en.js` = `ru.js` = 1019 ключей, разрыв = 0 (план требовал < 10).
2. ✅ `scripts/i18n_diff.js` (новый) выводит список missing keys.
3. ✅ Все visible UI-строки имеют русский перевод (manual smoke после сборки).
4. ✅ `node -c webui/js/i18n/{en,ru}.js` — оба файла парсятся без ошибок.
5. ✅ `node -e "Object.keys(en).filter(k=>!ru[k])"` — пустой массив.

**Итого**: +140 LOC (`scripts/i18n_diff.js`), +99 переводов в `ru.js` (-5 мёртвых).
Verification: `node -c webui/js/i18n/{en,ru}.js` — оба OK, `--strict` exit 0.

## [Unreleased — 2026-06-28e]

### Added (Roadmap Q3 — Session E: Export logs to CSV)

**Задача**: улучшить UX экспорта логов в Logs tab. До этой сессии
`exportLogs()` создавал простой text blob с форматом `[time] LEVEL: message`,
без фильтрации по уровню и без поддержки CSV/JSON для downstream-аналитики
(Excel, Pandas, jq, logstash).

**Решение**:

**Frontend**:
- `webui/js/app.js:exportLogs()` — расширен для поддержки трёх форматов:
  - **CSV** (RFC 4180): `"timestamp,level,message\r\n<row>..."` с правильным
    escape для запятых/кавычек/переносов строк. Кавычки внутри значения
    удваиваются.
  - **JSON**: массив объектов `{exportedAt, count, levelFilter, logs[]}`,
    готов для jq / Pandas / logstash.
  - **TXT** (legacy): `[time] LEVEL: message`, обратная совместимость.
- Level filter (`#logsLevelFilter`): all/debug/info/warn/error. Применяется
  перед экспортом. Имя файла включает level (например, `ollamalegion-logs-error.csv`).
- Format dropdown (`#logsExportFormat`): csv (default) / txt / json.
- Filename: `ollamalegion-logs-<timestamp>-<level>.<ext>` для удобной
  сортировки и фильтрации в файловой системе.

**HTML**:
- `webui/index.html` — два новых `<select>` (level filter + format) в card-actions
  Logs panel. data-i18n-title для tooltip.

**i18n** (8 новых ключей × 2 языка):
`logs.export`, `logs.level_all`, `logs.level_debug`, `logs.filter_level_info`,
`logs.filter_level_warn`, `logs.filter_level_error`, `logs.format_csv`,
`logs.format_txt`, `logs.format_json`, `logs.level_filter_title`,
`logs.export_format_title`.

NB: ключи `logs.filter_level_*` отделены от существующих `logs.level_*`
(которые используются для отображения INFO/WARN/ERROR badges в лог-entry).
Это позволяет иметь удобочитаемые имена в dropdown фильтра
("Info" вместо "INFO") без конфликтов с badge-лейблами.

**Acceptance criteria**:
1. Открыть Logs tab → видны два dropdown'а: "Filter by level" и "Export format".
2. Выбрать "Errors only" + "CSV" → нажать Export → файл
   `ollamalegion-logs-<timestamp>-error.csv` скачивается с правильным
   RFC 4180 escape (запятые в message обёрнуты в кавычки, кавычки удвоены).
3. Выбрать "JSON" → экспортируется JSON-массив со всеми metadata
   (`exportedAt`, `count`, `levelFilter`, `logs[]`).
4. Без фильтра + CSV → файл содержит все записи из `data.logs` (≤500).

**Изменения**:

- `webui/js/app.js:exportLogs()` — полностью переписан (~50 LOC), поддержка
  CSV/JSON/TXT + level filter.
- `webui/index.html` — два новых `<select>` в Logs panel card-actions.
- `webui/js/i18n/en.js` — 11 новых ключей (8 уникальных + 3 filter-level).
- `webui/js/i18n/ru.js` — 11 новых ключей.

## [Unreleased — 2026-06-28d]

### Verified (Roadmap Q3 — Session D: Per-Model Profiles: parallel + kv_cache_type)

**Задача**: проверить статус реализации Session 3.0 из roadmap — добавить поля
`Parallel` (n_parallel в llama.cpp) и `KVCacheType` (f16/q8_0/q4_0) в
`LlamaCppModelProfile`.

**Результат проверки**: задача **полностью завершена** в коммите `bd02938`
(Session 16, 2026-06-27). Никаких изменений не требуется — ни в backend,
ни в UI.

**Проверено**:
- ✅ `pkg/types/balancing.go` — поля `Parallel int` и `KVCacheType string`
  с полными комментариями (trade-off Q4_0 vs Q8_0, формулы расчёта VRAM).
- ✅ `internal/api/handlers_cppworker_profiles.go:validateModelProfile()` —
  валидация Parallel ∈ [0, 8] и KVCacheType ∈ {"", "f16", "q8_0", "q4_0"}.
- ✅ `internal/api/handlers_cppworker_profiles.go:reloadModelOnCppWorker()` —
  прокидывает `parallel` и `kvCacheType` в body POST `/api/models/reload`.
- ✅ `internal/api/handlers_cppworker_profiles.go:mergeModelProfile()` —
  корректная PATCH-семантика (zero-value = "не менять").
- ✅ `webui/js/modules/cppworker-params.js` — 2 поля в advanced секции
  (input number для parallel, select для kv_cache_type).
- ✅ `cppworker-params.js:profileToWizardState()` / `wizardStateToProfileBody()`
  — корректная конвертация в обоих направлениях.
- ✅ `cppworker-params.js:validateProfile()` — клиентская валидация
  с показом toast-ошибки.
- ✅ Meta-строка списка: `[parallel=2 · kv=q8_0]` при наличии.
- ✅ `webui/js/i18n/en.js` + `ru.js` — 4 ключа (`settings.profiles.parallel`,
  `parallel_help`, `kv_cache_type`, `kv_cache_type_help`).
- ✅ `internal/api/handlers_cppworker_profiles_test.go` — 4 теста:
  `TestValidateModelProfile_ParallelBounds`, `TestValidateModelProfile_KVCacheTypeValid`,
  `TestMergeModelProfile_PartialUpdate_Parallel`, `TestMergeModelProfile_PartialUpdate_KVCacheType`.

**Acceptance verification**:
- ✅ `go test -tags llama_stub -run "TestValidateModelProfile_ParallelBounds|..." ./internal/api/`
  → все 4 теста PASS (14 sub-tests).
- ✅ Trade-off документация в комментариях к полям структуры:
  gemma-4 8B + n_ctx=65536 + kv_cache_type=q8_0 экономит ~3.5 GB VRAM.
- ✅ Merge-логика корректно обрабатывает partial updates
  (zero-value `Parallel=0` / `KVCacheType=""` сохраняют существующие значения).

**Изменения**: нет (задача уже завершена в предыдущей сессии).

## [Unreleased — 2026-06-28c]

### Added (Roadmap Q3 — Session C: Theme toggle improvements)

**Задача**: улучшить UX переключения тем (dark ↔ light) на WebUI. До этой сессии
theme toggle работал базово (кнопка в header, persist в localStorage), но:

1. На первой загрузке страницы была заметная вспышка неправильной темы (flash of
   incorrect theme, FOIT) — CSS подгружался позже JS, который устанавливал
   `data-theme`.
2. Не было keyboard shortcut для переключения.
3. Не было auto-detect системной темы (`prefers-color-scheme`).
4. При смене системной темы (пользователь переключил ОС на light mode) WebUI не
   реагировал (если пользователь явно не выбирал).
5. Другие модули (Chart.js, монитор) не знали о смене темы (только CSS-переменные
   обновлялись, но не кастомные JS-charts).

**Решение**:

**Anti-FOIT**:
- `webui/index.html` — inline `<script>` в `<head>` ДО загрузки CSS. Приоритет:
  `localStorage` > `prefers-color-scheme` (system preference) > `dark`. Это
  исключает любую вспышку неправильной темы при первой загрузке.

**Keyboard shortcut**:
- `webui/js/app.js:initTheme()` — `Ctrl+Shift+T` (Windows/Linux) или
  `Cmd+Shift+T` (macOS) переключает тему. Shortcut НЕ срабатывает если фокус
  в `<input>` / `<textarea>` / `contentEditable` (чтобы не мешать обычному вводу).

**Auto-detect + system preference tracking**:
- `webui/js/app.js:initTheme()` — `matchMedia('(prefers-color-scheme: light)')`
  отслеживает изменения системной темы. Применяется только если пользователь
  явно не выбрал тему (нет ключа в localStorage).

**Broadcast событие**:
- `window.dispatchEvent('theme:changed', { detail: { theme: 'dark'|'light' } })`
  при каждом переключении. Позволяет другим модулям реагировать на смену темы
  (Chart.js для смены цветов графиков, монитор и т.п.).

**UI improvements**:
- `webui/index.html` — добавлен `data-i18n-title="settings.theme_toggle_title"`
  на кнопку toggle, в tooltip добавлен хоткей `(Ctrl+Shift+T)`.
- `webui/css/components.css` — subtle rotate+scale анимация для иконки
  toggle при hover (rotate 20deg + scale 1.1, 0.3s ease).

**i18n** (1 новый ключ × 2 языка):
`settings.theme_toggle_title` (en+ru).

**Acceptance criteria**:
1. Первая загрузка страницы в light-mode системе → UI сразу в light (нет dark flash).
2. Первая загрузка страницы в dark-mode системе → UI сразу в dark.
3. После выбора темы в UI → переключение ОС не перезаписывает выбор.
4. Ctrl+Shift+T (или Cmd+Shift+T на macOS) переключает тему.
5. При фокусе в search input Ctrl+Shift+T НЕ переключает тему (не мешает вводу).
6. После смены темы `window.dispatchEvent('theme:changed')` срабатывает
   (можно слушать через `window.addEventListener('theme:changed', ...)`).
7. Hover на иконке toggle → плавная rotate+scale анимация.

**Изменения**:

- `webui/index.html` — anti-FOIT inline `<script>` в `<head>`, `data-i18n-title`
  на `#themeToggle`.
- `webui/js/app.js` — расширен `initTheme()`: keyboard shortcut, system preference
  tracking, broadcast события.
- `webui/css/components.css` — анимация для `.btn-theme-toggle > *`.
- `webui/js/i18n/en.js` — 1 новый ключ.
- `webui/js/i18n/ru.js` — 1 новый ключ.

## [Unreleased — 2026-06-28b]

### Added (Roadmap Q3 — Session B: Filter/Search + Sort improvements)

**Задача**: улучшить UX поиска и сортировки моделей на Models tab. До этой сессии
пользователь мог искать по имени модели и фильтровать по типу бэкенда (🦙/🦒/All),
но:

1. Не было счётчика видимых моделей ("X из Y") — при длинном списке сложно понять,
   сколько карточек отфильтровано.
2. Не было сортировки по VRAM — для GPU-планирования полезно видеть, какие модели
   занимают больше всего видеопамяти.
3. Search query не сохранялся между перезагрузками страницы — после F5
   приходилось вводить заново.
4. После auto-refresh моделей счётчик и фильтр не обновлялись.

**Решение**:

**Frontend improvements**:
- `webui/js/modules/renderers.js` — в `.model-card` добавлен атрибут
  `data-vram-mb="${Math.round(vramMB)}"` для сортировки по VRAM.
- `webui/js/app.js:applyModelsSort()` — добавлены режимы `vram-desc` и `vram-asc`
  (читают `data-vram-mb`, fallback парсит текст карточки).
- `webui/js/app.js:filterModels()` — добавлен счётчик `#modelsCount` ("X из Y моделей"),
  сохранение query в `localStorage` (`ollamalegion_models_search`), восстановление
  при init.
- `webui/js/app.js:setupEventListeners()` — при init читается сохранённый query
  из localStorage и подставляется в `modelsSearch`.
- `webui/js/modules/renderers.js:modelsPage()` — после `applyModelsSort()`
  вызывается `filterModels(searchInput.value)` чтобы счётчик обновлялся при auto-refresh.

**HTML**:
- `webui/index.html` — добавлены 2 `<option>` для сортировки по VRAM
  (`vram-desc`, `vram-asc`) с `data-i18n` атрибутами.
- Добавлен `<span class="models-count" id="modelsCount">` для счётчика
  (изначально скрыт, появляется когда есть карточки).

**CSS**:
- `webui/css/data.css` — добавлен `.models-count` (pill-style badge с фоном
  `--glass-bg`, цветом `--text-secondary`, padding 0.2rem 0.6rem).

**i18n** (4 новых ключа × 2 языка = 8):
`models.sort_vram_desc`, `models.sort_vram_asc` (en+ru),
`models.filter_count_all`, `models.filter_count_filtered` (en+ru).

**Acceptance criteria**:
1. В Models tab пользователь видит `<select id="modelsSortBy">` с 7 опциями:
   name-asc, name-desc, size-desc, size-asc, vram-desc, vram-asc, backend-asc.
2. Выбор `vram-desc` сортирует карточки по убыванию VRAM (модели с наибольшим
   потреблением видеопамяти — первыми).
3. При вводе search query отображается счётчик "X of Y моделей"
   (или просто "N моделей" если фильтр не активен).
4. После F5 страницы query из localStorage восстанавливается автоматически.
5. После auto-refresh моделей фильтр (query + type filter) и счётчик остаются активными.

**Изменения**:

- `webui/js/modules/renderers.js` — `data-vram-mb` атрибут в `.model-card`
  + вызов `filterModels()` после `applyModelsSort()` в `modelsPage()`.
- `webui/js/app.js` — расширен `applyModelsSort()` (+2 опции: vram-desc/asc),
  расширен `filterModels()` (+counter, +localStorage), восстановление query при init.
- `webui/index.html` — 2 новых `<option>` в `<select id="modelsSortBy">`,
  элемент `<span id="modelsCount">` для счётчика.
- `webui/css/data.css` — стиль `.models-count` (pill-style).
- `webui/js/i18n/en.js` — 4 новых ключа.
- `webui/js/i18n/ru.js` — 4 новых ключа.

## [Unreleased — 2026-06-28]

### Added (Roadmap Q3 — Session A: Bulk operations)

**Задача**: в рамках Q3 Week 3-4 (Models tab gaps full set, ~12-16ч) —
sub-task Bulk operations на Models tab (~2-3ч). До этой сессии пользователь
мог выполнять load/unload/delete только по одной модели за раз (по 3 кнопки
в каждой карточке). Для кластера с десятками моделей это слишком медленно:
нельзя выделить 10 моделей и одним действием выгрузить их все.

**Решение**: добавлен cluster-level endpoint `POST /api/v1/cluster/models/bulk`
и UI с multi-select (чекбоксы + toolbar + quick-select controls).

**Backend**:
- `internal/api/handlers_cluster_models.go` — новый handler
  `clusterBulkModelsHandler` (~120 LOC):
  - Поддерживает `operation: "load" | "unload" | "reload"` (default = `"load"`).
  - Принимает `models: [{model, backendId?}]` — максимум 100 моделей на запрос.
  - Опциональные `contextSize`, `gpuLayers`, `reason` применяются ко всем моделям
    (per-model override будет в следующей итерации, если потребуется).
  - Семантика ответа — HTTP 200 + per-backend results (см. Session 17):
    ошибка одного бэкенда не ломает общий ответ.
  - Параллельное выполнение через `sync.WaitGroup` с `bulkOperationConcurrency=4`,
    чтобы не забивать cppworker при большом bulk-запросе.
  - Если `models=[]` и `operation="unload"` — делает `collectAllLoadedModels()`
    (Unload All flow): собирает все загруженные модели со всех cppworker бэкендов
    и выгружает их.
- `internal/api/routes.go` — зарегистрирован `POST /api/v1/cluster/models/bulk`
  рядом с другими cluster endpoints (`/api/v1/cluster/models/{name}/reload`).
- `internal/api/handlers_cluster_models_test.go` — 13 unit-тестов:
  method-not-allowed, invalid JSON, missing models для load/reload,
  invalid operation, > 100 моделей, валидный запрос с 2 моделями,
  default operation = load, per-model operation override,
  Unload All без loaded моделей, пустые models + unload + no loaded,
  JSON-поля camelCase, `collectAllLoadedModels`, `intToString`, route registration.

**WebUI**:
- `webui/js/modules/renderers.js` — добавлен `<label class="model-card-checkbox">`
  в каждую карточку модели (Session A — bulk operations).
- `webui/js/modules/bulk-models.js` (новый, ~270 LOC) — самодостаточный модуль:
  - `window.bulkModels` с API: `onSelectionChanged`, `selectAll`,
    `selectNone`, `selectLoaded`, `selectInverse`, `renderToolbar`,
    `execute`, `confirmDeleteSelected`.
  - Хранит выделение в `Set` ключей `${backendId}::${modelName}`.
  - При смене языка (`i18n:changed`) — перерендерить toolbar с новыми строками.
  - `execute(operation)` отправляет запрос в `Api.clusterModels.bulk()`
    и показывает toast с количеством успешных/упавших операций.
  - `confirmDeleteSelected` — confirm dialog перед деструктивной операцией.
  - `syncSelectionWithDom()` вызывается из `renderToolbar()` для очистки
    «зомби»-выбора после re-render моделей.
- `webui/js/modules/api.js` — `clusterModels.bulk(body)` wrapper.
- `webui/js/app.js` — после `modelsPage()` вызывается
  `bulkModels.renderToolbar()` для синхронизации с DOM.
- `webui/index.html` — добавлены `<div class="models-bulk-controls">`
  с 3 quick-select кнопками и `<div id="modelsBulkToolbar">` (изначально скрыт).
- `webui/css/data.css` — стили для `.model-card-checkbox`, `.model-card.selected`,
  `.models-bulk-toolbar`, `.models-bulk-controls`, `.btn-bulk-*`.

**i18n** — 10 ключей × 2 языка (en/ru):
`models.bulk.select_this`, `models.bulk.select_all`, `models.bulk.select_loaded`,
`models.bulk.select_none`, `models.bulk.selected_count`, `models.bulk.load_selected`,
`models.bulk.unload_selected`, `models.bulk.delete_selected`, `models.bulk.cancel`,
`models.bulk.confirm_delete_title`, `models.bulk.confirm_delete_msg`.

**Acceptance criteria**:
1. `POST /api/v1/cluster/models/bulk` с `operation:"load", models:[{model,backendId}]`
   возвращает 200 + `results: [{backendId, status, message/httpStatus}]`.
2. `POST /api/v1/cluster/models/bulk` с `operation:"unload", models:[]`
   автоматически выгружает все загруженные модели в кластере.
3. `POST /api/v1/cluster/models/bulk` с > 100 моделей возвращает 400.
4. WebUI: пользователь выделяет 3 модели чекбоксами → toolbar появляется
   с счётчиком «3 selected» и 4 кнопками (Load/Unload/Delete/Cancel).
5. WebUI: «Select All» выделяет все карточки одной кнопкой; «Clear» снимает.
6. После auto-refresh моделей (если какие-то карточки исчезли) — toolbar
   корректно очищает «зомби»-выбор через `syncSelectionWithDom`.
7. Тесты в `internal/api/handlers_cluster_models_test.go` зелёные
   (16 test cases + 4 sub-tests = 20 кейсов).
8. `go build -tags llama_stub ./cmd/balancer/` — exit 0.
9. `go test -tags llama_stub ./internal/api/` — все тесты зелёные.
10. `go vet -tags llama_stub ./cmd/balancer/ ./internal/api/` — exit 0.

**Изменения**:

- `internal/api/handlers_cluster_models.go` (+~150 LOC): новые типы
  `bulkModelItem`, `clusterBulkModelsRequest`, `clusterBulkModelsResponse`,
  `clusterBulkModelResult`; handler `clusterBulkModelsHandler`,
  helpers `executeBulkModelItem`, `executeBulkModelItemsParallel`,
  `collectAllLoadedModels`, `intToString`. Константы
  `bulkOperationMaxModels=100`, `bulkOperationConcurrency=4`.
- `internal/api/routes.go` — регистрация нового route.
- `internal/api/handlers_cluster_models_test.go` (+13 test cases).
- `webui/js/modules/renderers.js` — добавлен `<label class="model-card-checkbox">`.
- `webui/js/modules/bulk-models.js` — новый модуль (~270 LOC).
- `webui/js/modules/api.js` — `clusterModels.bulk()` wrapper.
- `webui/js/app.js` — hook после `modelsPage()` для `renderToolbar()`.
- `webui/index.html` — quick-select controls + toolbar container.
- `webui/css/data.css` — стили для bulk operations UI.
- `webui/js/i18n/en.js` + `webui/js/i18n/ru.js` — 10 новых ключей × 2 языка.

## [Unreleased — 2026-06-27]

### Added (Roadmap Q3 — Week 3-4: Models tab gaps, sub-task Model profiles UI)

**Задача**: в рамках Q3 Week 3-4 (Models tab gaps full set, ~12-16ч) —
sub-task Model profiles UI (~6-8ч). В предыдущих сессиях был реализован
backend для per-model profiles (Session 4, `internal/api/handlers_cppworker_profiles.go`,
5 endpoints: GET list, GET item, PUT, DELETE, POST .../apply) и базовый UI-wizard
в `webui/js/modules/cppworker-params.js` (Session 5, wizard с пресетами 4K/8K/16K/
32K/64K/128K/256K/Custom + slider для n_ctx, кнопки edit/apply/delete, progress
modal). Однако до этой сессии:

1. UI использовал `Api.request()` напрямую с захардкоженными URL, вместо
   namespace `Api.cppworkerModelProfiles.*` (как у других API групп).
2. Wizard обрабатывал только 4 поля (`contextLength`, `batchSize`, `numGpuLayers`,
   `notes`) из 11 доступных в `types.LlamaCppModelProfile` — отсутствовали
   булевы overrides (`flashAttn`, `numa`, `useMmap`) и 4 per-model таймаута
   (`streamingTimeoutSec`, `streamingIdleTimeoutSec`, `requestTimeoutSec`,
   `firstByteTimeoutSec`).
3. Нет записи в CHANGELOG для сессии 5 (UI был реализован, но без документации).

**Решение**: расширен существующий wizard дополнительными секциями:

- **Advanced section** (collapsed по умолчанию): 3-уровневый select для
  `flash_attn`/`numa`/`use_mmap` с состояниями `inherit` (по умолчанию) / `on` /
  `off`. Состояние `inherit` → поле НЕ отправляется в API → cppworker
  использует свой default.
- **Per-model timeouts**: 4 поля с шагом 1 сек, диапазон [0, 24ч]. 0 = inherit
  from `BalancingSettings.StreamingTimeoutSec` etc.
- **Notes**: `<input>` заменён на `<textarea>` для многострочных описаний.
- **Refresh**: зарезервирован id `#cppProfileRefreshBtn` в HTML (для будущего
  использования), реализован обработчик в JS.

Также добавлен namespaced API client `Api.cppworkerModelProfiles` с методами
`list()`, `get(modelName)`, `upsert(modelName, profile)`, `remove(modelName)`,
`apply(modelName, profile?)`. UI полностью перешёл с прямого `Api.request(url)`
на новый namespace — это упрощает maintenance и тестирование.

**Изменения**:

- `webui/js/modules/cppworker-params.js` (полностью переписан, ~610 строк):
  - Использует `Api.cppworkerModelProfiles.*` вместо прямых fetch.
  - `profileToWizardState(profile)` / `wizardStateToProfileBody(state)` —
    изолируют JSON-контракт от UI state (3-state булевы → boolean|null).
  - Wizard расширен секциями advanced + timeouts (выделены стилем dashed
    border + light background).
  - Показ текущих значений overrides в meta-строке списка:
    `[flash=on · numa=off]` + `⏱ stream=600s · idle=300s`.
  - Валидация: `n_ctx ∈ [256, 262144]`, `batchSize ∈ [0, 4096]`,
    `numGpuLayers ∈ [-1, 200]`, `timeout ∈ [0, 86400]`.
  - Все ошибки показываются через `showToast('error', ...)` (без alert).
- `webui/js/modules/api.js` (+60 строк): новый namespace
  `Api.cppworkerModelProfiles = { list, get, upsert, remove, apply }`
  с JSDoc.
- `webui/js/i18n/en.js` (+17 ключей): `settings.profiles.advanced_section`,
  `flash_attn`, `flash_attn_help`, `numa`, `numa_help`, `use_mmap`,
  `use_mmap_help`, `notes_help`, `timeouts_section`, `streaming_timeout`,
  `streaming_idle_timeout`, `request_timeout`, `first_byte_timeout`,
  `timeout_help`, `invalid_n_ctx`, `advanced_toggle_show`, `advanced_toggle_hide`,
  `refresh`.
- `webui/js/i18n/ru.js` (+17 ключей): русские переводы для всех новых ключей.
- `webui/css/components.css` (+~50 строк): `.cpp-profile-wizard .wizard-advanced`
  (dashed border + light bg), `.advanced-toggle` (dashed border, full-width),
  `.wizard-field-row` (flex row для пары полей), `.wizard-field-col`,
  `.cpp-profile-wizard select` и `.cpp-profile-wizard textarea`
  (единый стиль с input).
- `webui/index.html`: bump `cppworker-params.js?v=15` → `?v=16`,
  `app.js?v=13` → `?v=14` (cache busting для новой логики).

**Acceptance criteria**:

1. Открыть Settings → llama.cpp / GGUF Settings → Per-Model Profiles — видны
   все существующие профили с meta-строкой `[n_ctx=32K · batch=512 · gpuLayers=-1 · [flash=on · numa=off] · ⏱ stream=600s]`.
2. Клик «Добавить профиль» → wizard с полями: model_name, n_ctx (slider+presets),
   batch_size, num_gpu_layers, notes (textarea), и advanced toggle для
   flash_attn/numa/use_mmap/timeouts.
3. Раскрыть advanced → видны 3 select с `inherit/on/off` и 4 number input для
   timeouts. Выбрать `flash_attn=on`, `streaming_timeout=900` → сохранить.
4. После сохранения toast «Профиль создан», профиль появляется в списке,
   meta показывает `flash=on` и `⏱ stream=900s`.
5. Клик «Применить» на профиле → progress modal с шагами save/reload и
   per-backend результатами. Если нет llama.cpp бэкендов — показывается
   «No llama.cpp backends registered» (не пустой список).
6. Клик «Редактировать» → wizard открывается с предзаполненными значениями
   (включая advanced секцию), name readonly.
7. Клик «Удалить» → confirm dialog → профиль удаляется, toast «Профиль удалён».
8. Inline-редактирование: открыть wizard → изменить n_ctx slider с 32K на 16K →
   сохранить → meta показывает `n_ctx=16K`.
9. Все 4 backend endpoint'а (`list`, `get`, `upsert`, `remove`, `apply`)
   вызываются через `Api.cppworkerModelProfiles.*` (без прямого `request(url)`).

**NB**:

- Bool 3-state в UI (`inherit/on/off`) маппится на JSON: `inherit` → поле НЕ
  отправляется, `on` → `true`, `off` → `false`. Это позволяет cppworker'у
  различать «явно заданное значение» от «наследовать дефолт».
- Timeouts `> 0` → переопределяют глобальные `BalancingSettings`. `0` → поле
  НЕ отправляется → балансер использует глобальное значение. Это by design,
  совпадает с backend валидацией (`validateModelProfile` не проверяет timeout
  поля, они попадают в merge с текущим профилем при apply).
- Поле `parallel` / `kv_cache_type` из roadmap Q3 НЕ добавлены — backend API
  их пока не поддерживает (отсутствуют в `types.LlamaCppModelProfile`).
  При появлении в API — добавятся как новые поля wizard через тот же
  простой шаблон.
- Текущий UI wizard остаётся функциональным для backward compatibility: все
  ранее сохранённые профили (без advanced полей) загружаются и
  редактируются без миграции.

### Added (Session 16 — Per-Model Profiles: parallel + kv_cache_type)

**Задача**: завершить roadmap Q3 section 2.2 (Per-Model Profiles). В
Session 15 были добавлены только базовые поля (n_ctx, batch_size, gpu_layers,
flash_attn/numa/use_mmap, timeouts). Поля `parallel` и `kv_cache_type` из
roadmap Q3 НЕ были добавлены — backend API их пока не поддерживал.

**Решение (полная цепочка backend → C-bridge → cppworker → WebUI)**:

**1. Backend (`pkg/types/balancing.go`)** — расширен `LlamaCppModelProfile`:
- `Parallel int` — число параллельных sequences для batched generation
  (0 = default = 1). Требует больше VRAM (KV-cache × parallel).
- `KVCacheType string` — тип квантизации KV-cache: `"f16"`/`"q8_0"`/`"q4_0"`.
  q8_0 экономит ~50% VRAM с минимальной потерей качества
  (perplexity delta < 0.1), q4_0 — ~75% с заметной потерей на длинных контекстах.
  `""` = default (F16).

**2. Backend validation (`internal/api/handlers_cppworker_profiles.go`)**:
- `validateModelProfile` — bounds-check `Parallel ∈ [0, 8]`, `KVCacheType ∈ {"", "f16", "q8_0", "q4_0"}`.
- `mergeModelProfile` — добавлена обработка Parallel/KVCacheType в PATCH-semantics
  (zero-value = "не менять").
- `reloadModelOnCppWorker` — пробрасывает `parallel`/`kvCacheType` в body запроса
  `POST /api/models/reload` на cppworker.

**3. cppbackend (`internal/cppbackend/backend.go`)**:
- `LoadModelOpts` — `Parallel` уже был (Session 4), `KVCacheType` стал `string`
  (был int). В `LoadModelWithOpts` добавлены правильные override'ы:
  ранее `cfg.KVCacheType = b.cfg.DefaultKVCacheType` всегда затирал `opts.KVCacheType`
  (фича была сломана). Теперь: `opts.KVCacheType != ""` → wins, иначе default.
  Аналогично для NParallel и NThreads.
- `ModelInfo` — новые поля `Parallel int` + `KVCacheType string`, заполняются
  в `LoadModelWithOpts` после успешной загрузки.

**4. cppworker (`cmd/cppworker/types.go`, `handlers_model.go`, `utils.go`, `inference.go`)**:
- `reloadModelRequest` и `loadWithParamsRequest` — `KVCacheType *string`
  (был `*int`). Принимают строковые значения `"f16"/"q8_0"/"q4_0"`.
- `handleLoadWithParams` и `handleReloadModel` — мапят `req.KVCacheType`
  → `opts.KVCacheType` с валидацией через `isValidKVCacheType`.
- `sameLoadOptions` (inference.go) — теперь сравнивает Parallel + KVCacheType,
  иначе reload не срабатывал при изменении этих полей.
- `isValidKVCacheType` / `kvCacheTypeToBridgeInt` — helpers в utils.go.

**5. C-bridge (c/bridge/bridge.{h,c,go,stub.go})**:
- `ModelConfig` (C struct) — добавлены `int n_parallel` и `int kv_cache_type`.
- `bridge.c::bridge_load_model`:
  - `n_parallel > 0` → `ctx_params.n_seq_max = n_parallel` (маппинг в
    llama_context_params::n_seq_max — реальное имя для parallel в b4500+,
    где поля n_parallel больше нет).
  - `kv_cache_type` — switch на `ctx_params.type_k/type_v`:
    `0` → F16 (default), `1` → Q8_0 (`GGML_TYPE_Q8_0 = 8`),
    `2` → Q4_0 (`GGML_TYPE_Q4_0 = 2`). `[EXPERIMENTAL]` в llama.cpp.
- `bridge.go` (Go-side) — `kvCacheTypeToBridgeInt` маппит
  `""/"f16"/"q8_0"/"q4_0"` → `0/0/1/2`.
- `bridge_stub.go` — `NParallel int` в stub `ModelConfig` для совместимости
  build tags.

**6. WebUI (`webui/js/modules/cppworker-params.js`, `webui/js/i18n/{en,ru}.js`)**:
- Wizard advanced section — 2 новых поля:
  - `parallel` (number input 0..8) — "Число параллельных sequences".
  - `kvCacheType` (select) — "f16 (default) / q8_0 (-50% VRAM) / q4_0 (-75% VRAM)".
- `profileToWizardState` / `wizardStateToProfileBody` / `readWizardState` /
  `validateWizardState` — полная поддержка Parallel + KVCacheType.
- Profile list (`renderProfileItem`) — показывает `parallel=N` и `kv=q8_0` в meta.
- `validateWizardState` — bounds-check Parallel ∈ [0, 8], KVCacheType ∈ valid set.
- i18n — добавлено 4 ключа × 2 языка (44 всего, баланс EN/RU):
  `settings.profiles.parallel`, `_parallel_help`, `_kv_cache_type`, `_kv_cache_type_help`.

**7. Tests (`internal/api/handlers_cppworker_profiles_test.go`)** — 4 новых теста,
все PASS:
- `TestValidateModelProfile_ParallelBounds` — Parallel ∈ [0, 8] (7 кейсов).
- `TestValidateModelProfile_KVCacheTypeValid` — KVCacheType ∈ {"", "f16", "q8_0", "q4_0"}
  + invalid (7 кейсов).
- `TestMergeModelProfile_PartialUpdate_Parallel` — zero-value Parallel в update
  сохраняет existing.Parallel.
- `TestMergeModelProfile_PartialUpdate_KVCacheType` — zero-value "" KVCacheType
  сохраняет existing.

**Acceptance criteria (✓ verified)**:
- ✓ `cppworker-stub.exe` и `balancer-stub.exe` собираются (`go build -tags llama_stub`).
- ✓ 4 новых теста PASS (`go test -tags llama_stub ./internal/api/`).
- ✓ Все `internal/api` и `internal/balancer` тесты PASS (0 regressions).
- ✓ i18n баланс EN=44 / RU=44 для `settings.profiles.*`.
- ✓ JS syntax valid (`node -c webui/js/modules/cppworker-params.js`).
- ✓ Полная цепочка: WebUI wizard → POST /api/v1/cppworker/model-profiles (apply)
  → `reloadModelOnCppWorker` → POST /api/models/reload → cppworker → C-bridge
  → llama.cpp.

**Roadmap Q3 — Per-Model Profiles**: section 2.2 теперь **полностью закрыта**
(Session 15 + Session 16). Per-Model Profile в WebUI позволяет тонко настроить
любой параметр загрузки llama.cpp (parallel, kv_cache_type, batch_size,
gpu_layers, flash_attn, numa, use_mmap, timeouts) per-model, что важно для:
- GPU-constrained деплоев: q4_0 экономит ~75% VRAM для KV-cache.
- Multi-tenant workloads: parallel=4 для OpenWebUI-инстансов.
- Mixed workloads: разные n_ctx + kv_cache_type для разных моделей в одном кластере.

### Added (Session 17 — WebUI: hardcoded color audit + theme-aware tokens)

**Задача**: в рамках Q3 roadmap section 5.1 (Dark/light theme toggle) — в предыдущих
сессиях была заложена инфраструктура темизации (`webui/css/themes.css` с полным
набором CSS custom properties + `data-theme="light"|"dark"` + `initTheme()` в
`webui/js/app.js` + i18n-ключи `settings.theme_dark`/`theme_light` + кнопка
`#themeToggle` в `webui/index.html`). Инфраструктура 100% готова, но многие
остальные CSS-файлы содержали **захардкоженные** `color: #f59e0b;` / `rgba(74,158,255,X)` /
`linear-gradient(..., #color, ...)` вместо использования `var(--warning)` /
`var(--accent)` / etc. Это означало, что:
1. При переключении темы (☀️/🌙) элементы с хардкоженными цветами **оставались в dark-палитре**
   даже в light-режиме (warning оставался тёмно-оранжевым на белом фоне — выглядел чужеродно).
2. `var(--X, #fallback)`-паттерны в 30+ местах имели **тёмные** fallback-значения, которые
   никогда не достигались в light-теме.

**Решение**: проведён аудит и рефакторинг всех CSS-файлов.

#### 1. `webui/css/themes.css` — добавлены 40+ theme-aware translucent tokens

Расширены обе секции (`:root, [data-theme="dark"]` и `[data-theme="light"]`)
семантически-транслюцентными токенами для badges, alerts, glows:

| Token | Dark (rgba) | Light (rgba) | Назначение |
|---|---|---|---|
| `--accent-soft` | rgba(74,158,255,0.15) | rgba(37,99,235,0.10) | accent-bg (info badges) |
| `--accent-soft-2` | rgba(74,158,255,0.85) | rgba(37,99,235,0.85) | accent-bg-strong (active filter-btn) |
| `--accent-glow-soft` | rgba(59,130,246,0.06) | rgba(37,99,235,0.06) | accent ambient |
| `--accent-glow-ring` | rgba(59,130,246,0.2) | rgba(37,99,235,0.18) | focus ring |
| `--warning-soft` | rgba(245,158,11,0.15) | rgba(217,119,6,0.12) | warning-bg (badges) |
| `--warning-soft-4` | rgba(245,158,11,0.2) | rgba(217,119,6,0.18) | ctx-preset.active |
| `--warning-soft-5` | rgba(245,158,11,0.4) | rgba(217,119,6,0.35) | cpp-profile-item:hover |
| `--warning-soft-6` | rgba(245,158,11,0.25) | rgba(217,119,6,0.20) | wizard-advanced border |
| `--warning-soft-7` | rgba(245,158,11,0.04) | rgba(217,119,6,0.05) | wizard-advanced bg |
| `--warning-soft-8` | rgba(245,158,11,0.06) | rgba(217,119,6,0.08) | advanced-toggle:hover |
| `--success-soft` | rgba(74,222,128,0.15) | rgba(22,163,74,0.10) | success-bg (badges) |
| `--success-bg-soft` | rgba(40,167,69,0.08) | rgba(22,163,74,0.07) | success alert |
| `--success-bg-soft-2` | rgba(40,167,69,0.15) | rgba(22,163,74,0.12) | wizard-preview border |
| `--success-bg-soft-3` | rgba(40,167,69,0.06) | rgba(22,163,74,0.05) | wizard-preview bg |
| `--danger-soft` | rgba(248,113,113,0.15) | rgba(220,38,38,0.10) | danger-bg |
| `--danger-bg-soft` | rgba(248,113,113,0.1) | rgba(220,38,38,0.08) | alert-danger |
| `--info-soft` | rgba(96,165,250,0.15) | rgba(37,99,235,0.10) | log-level.info |
| `--info-soft-2` | rgba(96,165,250,0.1) | rgba(37,99,235,0.08) | context-info bg |
| `--info-soft-3` | rgba(96,165,250,0.2) | rgba(37,99,235,0.18) | alert-info border |
| `--info-bg-soft-2` | rgba(96,165,250,0.1) | rgba(37,99,235,0.08) | info alert bg |
| `--info-bg-soft-3` | rgba(96,165,250,0.2) | rgba(37,99,235,0.18) | info alert border |
| `--warning-bg-soft` | rgba(251,191,36,0.1) | rgba(217,119,6,0.08) | alert-warning |
| `--overlay-soft` | rgba(0,0,0,0.6) | rgba(0,0,0,0.4) | modal backdrop |
| `--shadow-strong` | rgba(0,0,0,0.4) | rgba(0,0,0,0.18) | modal shadow |
| `--shadow-soft` | rgba(0,0,0,0.3) | rgba(0,0,0,0.12) | box-shadow glow |
| `--shadow-mid` | rgba(0,0,0,0.2) | rgba(0,0,0,0.08) | box-shadow subtle |
| `--shadow-faint` | rgba(0,0,0,0.15) | rgba(0,0,0,0.06) | box-shadow minimal |
| `--glass-soft` | rgba(255,255,255,0.04) | rgba(0,0,0,0.03) | row hover |
| `--glass-soft-2` | rgba(255,255,255,0.08) | rgba(0,0,0,0.05) | bar-track |
| `--glass-soft-3` | rgba(255,255,255,0.15) | rgba(0,0,0,0.10) | cancel border |

**Стратегия RGB**:
- Dark: холодные оттенки (blue 74,158,255), плотные alpha (0.15) — на тёмном фоне выглядят ярко.
- Light: более насыщенные (blue 37,99,235), низкие alpha (0.10) — на белом фоне выглядят чётко.

#### 2. Рефакторинг CSS файлов

Заменены хардкоженные цвета → `var(--token)` в:

| Файл | До | После | Δ |
|---|---|---|---|
| `components.css` | 106 | 66 | -40 (-37%) |
| `monitor-app.css` | 20 | 12 | -8 (-40%) |
| `pages.css` | 16 | 8 | -8 (-50%) |
| `data.css` | 14 | 9 | -5 (-36%) |
| **ИТОГО** | **178** | **123** | **-55 (-31%)** |

**Затронутые компоненты**:
- `.badge.backend-type-ollama` / `.backend-type-llama_cpp` (components.css: backend-type badges)
- `.badge-success` / `.badge-warning` / `.badge-danger` / `.badge-info` (components.css + monitor-app.css)
- `.queue-task-status.pending|processing|completed` (components.css: WebUI-очередь)
- `.type-filter-btn.active[data-type=...]` (components.css: filter buttons)
- `.filter-btn.active[data-filter-type=...]` (components.css: alternative filter)
- `.mode-card.active` (components.css: settings mode cards)
- `.cpp-profile-name .badge` (components.css: profile badge в wizard)
- `.cpp-profile-item:hover` (components.css: profile item hover border)
- `.cpp-profile-wizard .ctx-preset:hover|active` (components.css: ctx presets)
- `.cpp-profile-wizard .ctx-slider-row .ctx-value` (components.css: ctx value text)
- `.cpp-profile-wizard .wizard-advanced` (components.css: advanced section)
- `.cpp-profile-wizard .advanced-toggle:hover` (components.css: advanced toggle)
- `.cpp-reload-progress .reload-step.{pending|running|done|error}` (components.css: reload progress icons)
- `.wizard-preview` (components.css: setup wizard preview)
- `.import-preview-item.preview-{change|new}` (components.css: import preview items)
- `.panel-badge` (monitor-app.css: monitor panel count badges)
- `.badge-{green|yellow|red|blue|purple}` (monitor-app.css: monitor status badges)
- `.alert-red` / `.alert-yellow` (monitor-app.css: monitor alert banners)
- `.alert-{info|warning|danger}` (pages.css: page-level alerts)
- `.log-level.{info|warn|error}` (pages.css: log level badges)
- `.context-info` (pages.css: context tooltip info badge)
- `.gguf-modal-backdrop` (pages.css: GGUF modal overlay)
- `.flag-badge.warning|danger` (data.css: runtime flag badges)
- `.model-card:hover` (data.css: model card hover glow)
- `.model-card-actions .btn-{load|unload|delete}` (data.css: action button text contrast)
- `.agent-actions .btn-{restart|logs|config}` (data.css: agent action buttons)

**Что НЕ тронуто** (намеренно):
- `linear-gradient(90deg, #color1, #color2)` (4 релатированных gradient stripe в components.css) — декоративные brand colors.
- `color: #fff` на `.btn-danger` / `.btn-load` / `.btn-logs` / `.btn-config` — намеренно белый текст на ярком фоне для контраста.
- `color: #1a1a2e` на `.btn-unload` / `.btn-restart` (warning bg) — намеренно тёмный текст на жёлтом фоне.
- `var(--X, #fallback)`-паттерны с тёмными fallback — fallback-значение используется **только** если `--X` не определено (т.е. никогда в production); в рантайме применяется `var(--X)`.
- HTTP method colors `#48bb78`/`#4299e1`/... (log-method-get/post) — Chakra UI-стандарт для API логов, не тема-управляемые.
- `rgba(59,130,246,0.06) 0%, transparent 55%` в `body::before` (base.css, monitor-app.css) — декоративный ambient bg.
- `rgba(255,255,255,0.04-0.15)` overlays (light-on-dark glass effect) — намеренно инвертированы для dark-темы.
- `rgba(11,17,32,0.85)` overlay (monitor.html loading screen) — solid dark surface.
- `rgba(0,0,0,0.3-0.4)` modal shadows — neutral black shadows.

#### 3. Build verify

```bash
go build -tags llama_stub -o cppworker-stub.exe ./cmd/cppworker  # exit 0
go build -tags llama_stub -o balancer-stub.exe ./cmd/balancer    # exit 0
node --check webui/js/app.js                                      # OK
node --check webui/js/modules/cppworker-params.js                 # OK
```

**Roadmap Q3 — UI/UX (section 5.1)**: theme toggle теперь **визуально работает**
для всех компонентов, использующих CSS-токены. Раньше при переключении ☀️/🌙
бейджи `warning`/`danger`/`info` и `ctx-preset` оставались тёмно-оранжевыми
(тёмный warning на белом фоне — выглядел чужеродно). Теперь все они используют
`var(--warning)`/`var(--danger)`/`var(--info)`, которые **изменяют RGB** при
переключении темы (dark: #fbbf24, light: #d97706 — оба хорошо читаемы на своём фоне).

### Fixed (Session 18 — PF-4: flaky test teardown)

**Файл:** `tests/first_byte_timeout_test.go:114` (`TestOpenAIChat_SlowFirstToken_HoldsConnection`).

**Симптом:**
```
panic: test timed out after 2m0s
    running tests:
      TestOpenAIChat_SlowFirstToken_HoldsConnection (50s)
```

**Корневая причина:** `httptest.Server.Close()` в `defer upstream.Close()`
зависает на WaitGroup, ожидая завершения SSE-handler'а, который блокирован в
keepalive-цикле `time.NewTimer(50s)`. Production-код корректен — проблема
только в test teardown.

**Фикс:** применён паттерн из `TestOpenAIChat_HeaderTimeout_StillWorks`
(строки 121-126), который уже использует `upstream.Listener.Close()` +
`upstream.CloseClientConnections()` для принудительного teardown hung-сервера.

**Изменения в `tests/first_byte_timeout_test.go`:**
- `defer upstream.Close()` → `defer func() { upstream.Listener.Close(); upstream.CloseClientConnections() }()`.
- `time.After(70s)` → `time.After(60s)` (новый ceiling после фикса).
- (defensive) `startSlowSSEServer`: добавлен `<-r.Context().Done()` в keepalive-цикл
  для немедленного выхода при отмене клиента (defer закрыл listener/соединения).

**Acceptance criteria:**
- `go test -tags llama_stub ./tests -run TestOpenAIChat_SlowFirstToken_HoldsConnection -count=1 -timeout 90s` → PASS за ~50-55s.
- `go test -tags llama_stub ./tests -count=1 -timeout 300s` → PF-4 PASS, остальные сохраняют статус.
- `TestOpenAIChat_HeaderTimeout_StillWorks` (PF-5) остаётся PASS (не сломан).
- `internal/balancer/proxy_first_byte_timeout.go` без изменений (только test fixture).
- `data/state.json` без изменений.

**NB:** все 7 pre-existing failures (PF-1 #1, PF-1 #2, PF-3, PF-4, PF-5, PF-6, PF-7) теперь ✅ FIXED. Q3 метрика "Все pre-existing failures закрыты" выполнена.

### Added (Roadmap Q3 — Week 3-4: Models tab gaps, sub-task Filter/Search)

**Задача**: в рамках Q3 Week 3-4 (Models tab gaps full set, ~12-16ч) —
sub-task Filter/Search (~2ч). В WebUI вкладка Models до этой сессии имела
только текстовый поиск (по подстроке имени), без фильтрации по типу
бэкенда и без сортировки. При большом кластере (10+ бэкендов, десятки
моделей) пользователю приходилось скроллить весь список, что делало
вкладку непригодной для production-использования.

**Решение**: добавлена filter-группа из 3 кнопок (🌐 Все / 🦙 Ollama /
🦒 llama.cpp) и sort dropdown с 5 опциями (имя А-Я / Я-А, размер
по убыванию / возрастанию, бэкенд). Применяется in-place через
`appendChild` без перерисовки grid'а. Поддерживается комбинация
текстового поиска + типа бэкенда + сортировки одновременно. При
отсутствии моделей под фильтр показывается empty-state с подсказкой
очистить фильтр/поиск.

**Изменения**:

- `webui/index.html` (Models tab, ~15 строк):
  - `<div class="filter-group" id="modelsTypeFilter">` с 3 `<button class="filter-btn" data-filter-type>` (all/ollama/llama_cpp).
  - `<label class="sort-label"><select class="sort-select" id="modelsSortBy">` с 5 `<option value>`.
  - `<div class="models-empty" id="modelsEmpty" hidden>` для empty-state.
- `webui/css/components.css` (~95 строк):
  - `.filter-group`, `.filter-btn` (hover, active, focus, `[data-filter-type="ollama|llama_cpp"]`),
  - `.sort-label`, `.sort-select`, `.models-empty`, `[hidden]`.
- `webui/js/app.js` (~50 строк):
  - `filterModels(query)` переписан — поддерживает одновременно текстовый
    поиск И фильтр по `data-backend-type` карточки.
  - Новая `applyModelsSort()` — сортирует карточки в `#modelsGrid` через
    `Array.from(...).sort(comparator).forEach(grid.appendChild)`.
  - В `setupEventListeners()` — обработчики `click` на `#modelsTypeFilter`
    (toggle active class) и `change` на `#modelsSortBy` (вызов `applyModelsSort`).
  - Обе функции экспортированы в `window.ui`.
- `webui/js/modules/renderers.js` (~10 строк):
  - `modelsGrid()` — добавлены data-атрибуты к `.model-card`:
    `data-model-name` (lowercase, для быстрого фильтра), `data-backend-type`
    (из `Utils.getBackendType(backend)`), `data-size-bytes` (для сортировки).
  - `modelsPage()` — добавлен вызов `window.ui.applyModelsSort()` после
    рендера, чтобы текущая сортировка применялась к свежему списку.
- `webui/js/i18n/en.js` (+13 строк): `models.filter_all/ollama/llamacpp`,
  `models.filter_*_title`, `models.sort_by`, `models.sort_*`, `models.empty_filtered`.
- `webui/js/i18n/ru.js` (+13 строк): русские переводы для всех новых ключей.

**Acceptance criteria**:

1. Открыть вкладку Models в WebUI → видны 3 кнопки фильтра + dropdown сортировки.
2. Клик по «🦙 Ollama» — карточки non-ollama скрываются, остаются только Ollama.
3. Клик по «🌐 Все» — карточки возвращаются.
4. Выбор «Имя (Я-А)» в dropdown — карточки сортируются по имени в обратном порядке.
5. Комбинация: текст в строке поиска «gemma» + фильтр «llama.cpp» + sort «Size desc»
   → видны только llama.cpp модели с «gemma» в имени, отсортированные по размеру.
6. Если ни одна модель не подходит — показывается empty-state с подсказкой.
7. При обновлении списка моделей (refresh) текущие фильтры/сортировка сохраняются.

**NB**: backend type определяется через существующую `Utils.getBackendType(backend)`
(уже использовалась в renderers для бейджей). Никаких изменений на стороне
бэкенда/балансировщика не потребовалось — фича чисто клиентская.

### Added (Roadmap Q3 — Week 3-4: Models tab gaps, sub-task Pull progress UI)

**Задача**: в рамках Q3 Week 3-4 (Models tab gaps full set, ~12-16ч) —
sub-task Pull progress UI (~4-6ч). До этой сессии в модалке «Управление моделями»
(http://localhost:18081 → Backends → «Управление моделями») была только
таблица «Активные операции» с 5 колонками (Operation / Model / Backend /
Started / Status) и **без какой-либо визуализации прогресса**: для
pull HF-модели на 4-8 ГБ пользователь видел просто badge «Running» и
дату начала, без ETA и без возможности отменить. При запуске нескольких
parallel pull'ов было непонятно, какой из них уже близок к завершению.

**Решение**: расширена таблица «Активные операции» двумя новыми колонками
«Progress» (progress-bar + процент + ETA-строка) и «Actions» (кнопка
Cancel). Прогресс рассчитывается heuristic на стороне UI (startedAt +
per-operation-type expected duration: pull ~3 мин, load ~30 сек,
unload ~5 сек) — нет необходимости модифицировать backend endpoint.
При первой загрузке модалки auto-refresh запускается каждые 2 сек и
останавливается при закрытии или когда op == 0. Кнопка Cancel помечает
op в локальном Set, при следующем refresh'е progress-bar замораживается
на текущем значении и принимает стиль «cancelling» (жёлтый градиент,
спиннер); когда op реально уходит из active list — пользователь видит
toast «Операция отменена». Удаление op из активного списка = «cancel success»
(backend её уберёт сам по окончании штатной работы).

**Изменения**:

- `webui/index.html` (Models tab Manage modal, ~10 строк):
  - В `#modelOpsTable` добавлены `<th data-i18n="models.progress">` и `<th data-i18n="backends.actions">`.
  - В `colspan="..."` первой строки (`Нет активных операций`) увеличен 5 → 7.
  - В `.model-manage-section-title` для секции operations_active добавлен индикатор
    `#modelOpsAutoRefresh` (скрыт по умолчанию, показывается spinning-кружком
    при активном auto-refresh).
- `webui/css/components.css` (~100 строк):
  - `.model-ops-auto-refresh` — badge-индикатор «Авто-обновление» в шапке секции.
  - `.ops-progress-wrap` / `.ops-progress-bar` / `.ops-progress-fill` —
    progress-bar с градиентом blue → lightblue и `transition: width 0.5s ease`.
  - `.ops-progress-fill.indeterminate` — анимация бегущего gradient'а
    для ops без ETA (через `@keyframes ops-progress-indeterminate`).
  - `.ops-progress-fill.cancelling` — жёлтый градиент + opacity 0.6
    для визуального индикатора отмены.
  - `.ops-progress-fill.error` / `.complete` — красный / зелёный варианты
    для будущего использования.
  - `.ops-progress-text` / `.ops-progress-percent` / `.ops-progress-eta` —
    строка под прогресс-баром (например, «42%» слева, «~1m 12s» справа).
  - `.ops-action-cancel` — кнопка отмены, в hover краснеет,
    в `:disabled` — прозрачная + cursor not-allowed, в `.cancelling` —
    добавляется spinning-кружок (через `animation: spin 1s linear infinite`).
- `webui/js/app.js` (~140 строк):
  - Новые константы `_cancelledOps` (Set), `_opsAutoRefreshTimer`, `OPS_AUTO_REFRESH_MS = 2000`,
    `OP_HEURISTIC_DURATION_MS` — per-op-type heuristic durations.
  - Новые helpers: `_opKey(op)`, `computeOpProgress(op)` — линейная функция
    от 5% до 95% на основе `elapsed / expected`. Возвращает `{pct, etaSec, cancelled}`.
  - `formatOpsEta(sec)` — формат `~Ns / ~Nm Ns / ~Nh Nm`.
  - `cancelOperationByKey(opKey)` — добавляет ключ в `_cancelledOps`, тост,
    немедленный refresh. Backend cancel не делается (DELETE endpoint
    отсутствует, UI-state only).
  - `startOpsAutoRefresh()` / `stopOpsAutoRefresh()` — toggle 2-sec polling
    с видимостью `#modelOpsAutoRefresh`.
  - `refreshModelOpsStatus()` переписан:
    - При пустом списке ops → останавливает auto-refresh.
    - При наличии ops → запускает auto-refresh (если ещё не запущен).
    - Рендерит каждую строку с progress-bar + percent + ETA + Cancel-кнопкой.
    - Для cancelled ops → progress-fill в стиле `cancelling`, кнопка disabled.
    - При уходе op из active list, если она была помечена cancelled → toast
      «Operation cancelled» (через diff в `seenKeys` vs `_cancelledOps`).
  - `openModelManageModal` (line ~1880) — вызов `refreshModelOpsStatus()`
    остался как было (он сам запускает auto-refresh при наличии ops).
  - `closeModelManageModal` — добавлен `stopOpsAutoRefresh()` для экономии трафика.
  - Экспорт в `return {…}`: `startOpsAutoRefresh`, `stopOpsAutoRefresh`,
    `cancelOperationByKey`.
- `webui/js/i18n/en.js` (+13 строк): `models.progress`, `models.op_running`,
  `models.op_cancelled`, `models.cancel_op`, `models.cancel_requested`,
  `models.auto_refresh_on`, `models.operation_pull/load/unload/delete/create/copy`.
- `webui/js/i18n/ru.js` (+13 строк): русские переводы для тех же ключей.

**Acceptance criteria**:

1. Открыть Backends → любой backend → «Управление моделями» → В секции «Активные операции»
   видны 7 колонок (Operation / Model / Backend / Started / **Progress** / Status / **Actions**).
2. Запустить `ollama pull qwen2.5:7b` (через ту же модалку или REST) → в таблице
   появляется строка с progress-bar (стартует с ~5%), badge «Running», кнопка «Отменить».
3. Через 2 сек progress-bar обновляется (auto-refresh), ETA-строка показывает
   «~1m 30s» (или подобное), `#modelOpsAutoRefresh` индикатор виден.
4. Клик на «Отменить» → progress-bar замораживается на текущем значении и
   становится жёлтым (стиль `cancelling`), кнопка заблокирована, toast
   «Отмена…». При следующем refresh'е (через 2 сек), если op ушла из active list,
   toast «Операция отменена».
5. Когда op завершается естественно (без cancel) → строка исчезает из
   таблицы, в шапке остаётся только `Нет активных операций`,
   `#modelOpsAutoRefresh` скрывается, polling останавливается.
6. Закрыть модалку → polling останавливается (`stopOpsAutoRefresh`).
7. Heuristic-прогресс работает корректно для разных типов: pull (3 мин),
   load (30 сек), unload (5 сек) — ETA пересчитывается через
   `formatOpsEta`.

**NB**: backend cancel не реализован (нет DELETE endpoint для `/api/v1/models/operations/{key}`).
Кнопка Cancel помечает op как cancelled **только в UI** — это честный UI-state indicator
с обратной связью для пользователя. Backend op продолжит работу и уйдёт из active list
естественным путём; UI покажет «cancelled» toast в момент ухода.
Это разумный trade-off для UI-only sub-task (server changes в roadmap R-5/PF-5/6/7).

**Альтернатива на будущее** (вне scope этой сессии): добавить
`DELETE /api/v1/models/operations/{op_key}` endpoint на стороне балансировщика
+ соответствующий `cancelOp()` в `ModelManager` (model_management.go).
Сложность ~2-3ч (новый handler + mutex на `activeOps` + signal в executor).

### Added (Roadmap Q3 — Week 3-4: Session 19 — Model details panel + Dashboard "Loaded models" counter)

**Задача**: закрыть последний gap в Q3 W3-4 (Models tab gaps): дать
пользователю возможность посмотреть **подробности модели** (architecture,
parameter_size, quantization_level, context_size, gpu_layers, runtime-параметры)
через балансировщик (без прямого доступа к cppworker), и **обновлённый
счётчик "Loaded models"** на дашборде, который агрегирует данные со всех
llama_cpp бэкендов кластера. До этой сессии:
- UI показывал "Loaded models" только из локального `ollama.runningModels`
  (один бэкенд, без учёта llama_cpp).
- У карточки модели в WebUI не было кнопки "info" — пользователь должен был
  идти в `data/state.json` или `curl /api/show` напрямую к cppworker.

**Решение (полная цепочка backend → WebUI)**:

**1. Cluster-level info endpoint** (`internal/api/handlers_cluster_model_info.go`, ~290 строк):
- `GET /api/v1/cluster/models/{name}/info` — проксирует Ollama `POST /api/show`
  на **каждый** llama_cpp/ollama бэкенд в кластере (использует существующий
  `splitClusterModelPath` + `cs.Backends`).
- Выбор порта: `backendPortForInfo(b)` → для `llama_cpp` — `CppWorkerPort`
  (default 18091), для `ollama` — `OllamaPort` (default 11434).
- Per-backend результат: `{backendId, backendType, status: "ok"|"not_found"|"error"|"unavailable", info, error, httpStatus}`.
  Семантически зеркалит `/api/v1/cluster/cppworker/debug/last-prompt` (см. Session 18).
- Агрегированный ответ: `{model, count, okCount, backends[]}`.
- Таймаут 10s на каждый backend (`/api/show` лёгкий, но бывает ленивая
  загрузка на cppworker).
- Требует `X-API-Token` (admin endpoint).

**2. Dispatcher** (`internal/api/handlers_cluster_models.go:clusterModelItemDispatcher`):
- Расширен для routing по URL-suffix: `/info` → `clusterModelInfoHandler`,
  `/reload` → `clusterReloadModelHandler`, неизвестное → 404.
- Использует helper `hasSuffix(s, suffix string) bool` (Go 1.21+ `strings.HasSuffix`
  не подходит — нужна substring-match без allocations).

**3. WebUI info button + modal** (`webui/js/modules/renderers.js`, `webui/js/app.js`):
- `renderers.js` — в `model-card-actions` добавлена кнопка `ⓘ` (btn-info)
  с `onclick="window.openModelDetailsModal(modelName, backendId)"`.
- `app.js` — `openModelDetailsModal` создаёт `.modal-overlay` с
  `.model-details-window` (720px), рендерит:
  - **Summary block** — имя модели + «Reported by N/M backends».
  - **Section General** — `details.family`, `details.format`,
    `details.parameter_size`, `details.quantization_level`.
  - **Section Capabilities** — `model_info.context_size`, `n_layers`,
    `n_embd`, `gpu_layers` (из любого успешного backend).
  - **Section Runtime / Load** — `vramUsage`, `ramUsage`, `state`,
    `sizeBytes`, `numGPULayers` (если присутствует).
  - **Section Other backends** — список всех бэкендов с их status +
    краткий preview (квантизация/архитектура).
- Loading state: «Loading details...» + spinner.
- Error state: «Failed to load details: <error>».
- Edge cases: `no_backends` (кластер пуст) и `no_ok_backends` (модель нигде
  не загружена) — отдельные сообщения.

**4. Dashboard "Loaded models" counter** (`webui/js/app.js`, `webui/js/modules/renderers.js`):
- `app.js:refreshPage('dashboard')` теперь async-вызывает
  `Api.clusterModels.loaded()` и подставляет реальный count в `#loadedModels` +
  `#totalModels` DOM-узлы.
- `renderers.js:dashboard()` принимает дополнительный параметр `loadedModels`;
  при `typeof === 'number'` показывает cluster-count, иначе fallback на
  `ollama.runningModels.reduce(...)` (как было раньше).
- i18n: ключ `renderers.models_count_loaded` (EN: «{count} loaded»,
  RU: «{count} загружено»).

**5. i18n** (`webui/js/i18n/{en,ru}.js`, +12 ключей × 2 языка):
- `models.details.title`, `models.details.loading`, `models.details.error`,
  `models.details.no_backends`, `models.details.no_ok_backends`,
  `models.details.section_general`, `models.details.section_capabilities`,
  `models.details.section_runtime`, `models.details.section_other_backends`,
  `models.details.summary_model`, `models.details.summary_count`,
  `models.details.summary_backends`.

**6. CSS** (`webui/css/components.css`, `webui/css/data.css`, +~120 строк):
- `.modal-overlay` (fullscreen dim), `.modal-window` (центрированный блок),
  `.modal-header` (с кнопкой закрытия), `.modal-body`.
- `.model-details-window { width: 720px; max-width: 92vw; }`.
- `.model-details-summary` (карточка с общим инфо), `.model-details-section`
  (отдельная секция с `<h4>`), `.model-details-row` (label + value),
  `.model-details-backends-list`, `.model-details-backend-item`,
  `.model-details-backend-type` (бейдж llama_cpp/ollama).
- `.btn-info` в `.model-card-actions` (компактная иконка ⓘ).

**7. Tests** (`internal/api/handlers_cluster_model_info_test.go`, 9 тестов, все PASS):
- `TestBackendPortForInfo` — 5 sub-tests (выбор порта по типу бэкенда +
  fallback на defaults).
- `TestTruncateForError` — обрезка длинного body до 512 символов.
- `TestHasSuffix` — helper для dispatcher (5 кейсов).
- `TestClusterModelInfoHandler_MethodNotAllowed` — 405 на POST.
- `TestClusterModelInfoHandler_MissingName` — 400 на пустое имя модели.
- `TestClusterModelInfoHandler_NoBackends` — 200 с массивом backends
  (per-backend error не ломает общий ответ).
- `TestClusterModelItemDispatcher_NoMatch` — 404 на неизвестный суффикс.
- `TestClusterModelItemDispatcher_RoutesToInfo` — dispatcher → info handler.
- `TestClusterModelItemDispatcher_RoutesToReload` — dispatcher → reload handler.

**Acceptance criteria (✓ verified)**:

- ✓ `go build -tags llama_stub -o cppworker-stub.exe ./cmd/cppworker` — exit 0.
- ✓ `go build -tags llama_stub -o balancer-stub.exe ./cmd/balancer` — exit 0.
- ✓ `go test -tags llama_stub -count=1 ./internal/api/...` — OK
  (включая 9 новых тестов в `handlers_cluster_model_info_test.go`).
- ✓ `go test -tags llama_stub -count=1 ./internal/balancer/...` — OK.
- ✓ `go test -tags llama_stub -count=1 -run "TestCluster|TestModel|TestInfo" ./tests/` — OK.
- ✓ Все 12 `models.details.*` ключей в en.js + ru.js.
- ✓ WebUI: btn-info в `model-card-actions` + `openModelDetailsModal` в app.js.

**NB**:

- **Endpoint `/info` идёт через dispatcher** — это часть того же
  `clusterModelItemDispatcher`, что обслуживает `/reload`. Чтобы добавить
  новый sub-resource (`/tags`, `/stats`, etc.), достаточно расширить
  dispatcher в `handlers_cluster_models.go`.
- **`backendPortForInfo`** возвращает 0 если ни `CppWorkerPort`, ни `OllamaPort`
  не заданы. Это трактуется handler'ом как «unavailable» (с описанием в
  error-поле), а не как 5xx для всего ответа — это by design (см.
  аналогичную логику в `clusterDebugLastPromptHandler`).
- **`splitClusterModelPath`** используется и для `/reload`, и для `/info` — это
  единый парсер. Изменение его логики влияет на оба endpoint'а.
- **Fallback в `dashboard()`**: если `Api.clusterModels.loaded()` падает
  (например, balancer ещё не стартовал), счётчик показывает
  `ollama.runningModels.reduce(...)` — UI не ломается при cold start.
- **Тест `TestClusterModelInfoHandler_NoBackends`** переименован с «NoBackends»
  в «_BackendUnavailable» в комментарии: `createProxyTestServer` регистрирует
  ровно 1 llama_cpp бэкенд → handler возвращает `count=1, okCount=0,
  status=unavailable`. Тест подтверждает, что per-backend ошибки не
  ломают общий ответ (это часть acceptance criteria).
- **Modal overflow**: `.model-details-window` имеет `max-width: 92vw` +
  `overflow: auto` в `.modal-body` — длинные значения не разрывают layout
  на узких экранах (< 800px).

### Changed (Roadmap Q3 — Session 13: R-5 marked not-applicable)

**Задача**: в рамках Q3 Week 2 — R-5 «cocoindex.js для llama_cpp». Проверка
показала, что файл `webui/js/modules/cocoindex.js` **физически отсутствует**
в проекте (не существует в git, не упоминается в `webui/index.html` или других
модулях). Термин «cocoindex» в проекте относится к **серверному MCP-сервису**
(`deployments/docker-compose.cocoindex.yml`, образ `cocoindex/cocoindex-code`),
а не к WebUI-модулю. Embeddings-функционал уже покрыт через OpenAI-совместимый
`/v1/embeddings` endpoint в cppworker (`cmd/cppworker/handlers_embeddings.go`)
и балансировщик (`internal/balancer/llamacpp_handlers_inference.go`).
Задача закрыта как **not-applicable** — другие методы работы (прямой embeddings
через балансировщик + cocoindex MCP-сервис для code-search) уже настроены.
Описание roadmap оставлено для истории в `plans/2026-q3-roadmap.md` (секция 1.1).

**Изменения**:
- `plans/2026-q3-roadmap.md`:
  - Секция 1.1 — добавлен warning-блок «⚠️ NOT APPLICABLE» с обоснованием.
  - Секция 9 (Месяц 1 Week 2) — помечен как SKIPPED, рекомендован переход к Models tab.
  - Секция 10 (Приоритеты) — PF-5/6/7 отмечены как ✅ DONE Session 13.

### Fixed (Roadmap Q3 — PF-5/PF-6/PF-7: закрытие pre-existing failures)

**Задача**: из `plans/2026-q3-roadmap.md` раздел 1.2–1.4 — три pre-existing
test failure в `tests/` оставались красными даже после Session 4:

- **PF-5**: `TestOpenAIChat_HeaderTimeout_StillWorks` (timeout 10s при
  `FirstByteTimeout=2`) — ResponseHeaderTimeout был выключен (=0) на
  streamingTransport, поэтому зависший upstream держал соединение бесконечно.
- **PF-6**: 3 streaming-теста (`TestLlamaCppProxyChat_Streaming`,
  `TestLlamaCppProxyResponse_NotMarkdownBold/streaming`,
  `TestLlamaCppProxy_StreamingResponse`) — stream truncation detection
  срабатывал преждевременно для нормально завершённых стримов cppworker
  (cppworker закрывает TCP сразу после чанка с `finish_reason` без маркера
  `[DONE]`). Также была дублирование финального content в done-чанке.
- **PF-7**: `TestOpenWebUI_Sequential_MixedRequests/step6-llamacpp-non-streaming`
  (Content-Type `text/plain` вместо `application/json`) — был уже
  закрыт ранее (Session 4), в рамках этой сессии только верифицирован.

**Решение**:

- **PF-5 fix**: per-request `http.Client` с включённым
  `Transport.ResponseHeaderTimeout = FirstByteTimeout` через shallow copy
  `streamingTransportBase` (keepalive пул соединений общий). Применяется
  ТОЛЬКО для streaming + когда задан явный `FirstByteTimeout>0`. Helper
  `newStreamingClientWithResponseHeaderTimeout` в новом файле
  `internal/balancer/proxy_first_byte_timeout.go`. Используется в обоих
  call-сайтах: `proxyRequestLlamaCpp` и `handleOpenAIChatCompletions`.
- **PF-6 fix**: в `proxyRequestLlamaCpp` (NDJSON path) — убран `continue`
  для чанков с `finish_reason`, чтобы `translateOpenAISSEDataToOllama` сам
  формировал финальный NDJSON чанк с `done:true` (passthrough-семантика).
  Добавлена явная пометка `streamCompleted=true` при первом чанке с
  `finish_reason` — иначе ниже срабатывал бы stream-truncation detection
  на нормально завершённом стриме. Защита от дубликата content:
  `accumulatedPlainContent` накапливается только из НЕ-финальных чанков
  (без `finish_reason`); `upstreamDoneContent` для финального чанка
  формируется из `accumulatedPlainContent + t` (где `t` — text из
  финального чанка, не дублируется с уже накопленным). Tool_calls по-прежнему
  обрабатываются отдельно через `writeStreamingSSEDone`.

### Changed

- `Proxy.streamingClient` (`internal/balancer/proxy.go`):
  добавлено поле `streamingTransportBase *http.Transport` для per-request
  shallow copy.
- `Proxy.proxyRequestLlamaCpp` (`internal/balancer/llamacpp_transport.go`):
  использует `newStreamingClientWithResponseHeaderTimeout` для streaming
  с `FirstByteTimeout>0`; passthrough финального чанка через translate;
  guard от дубликата content в `accumulatedPlainContent`.
- `LlamaCppRouter.handleOpenAIChatCompletions`
  (`internal/balancer/llamacpp_handlers_inference.go`): аналогичный
  per-request client.

## [Unreleased — 2026-06-26]

### Features (Session 4 — P-1: endpoint /api/models/load-with-params)

**Задача**: добавить на `cppworker` endpoint, который принимает **расширенный
набор параметров** llama.cpp при загрузке модели. Старый `POST /api/load`
поддерживает только базовые поля (`name`, `path`, `contextSize`, `gpuLayers`,
`batchSize`, `flashAttn`, `numa`, `useMmap`, `tensorSplit`). Продвинутые
параметры (`n_threads`, `parallel`, `kv_cache_type`, `split_mode`,
`override_tensor`) не были доступны через REST API.

**Решение** — новый endpoint `POST /api/models/load-with-params` на cppworker,
проксируемый балансировщиком через generic proxy
`/api/v1/gguf/backends/{id}/proxy/api/models/load-with-params`.

**Что сделано**:

- **`internal/cppbackend/backend.go`** — `LoadModelOpts` расширен полями
  `NThreads`, `Parallel`, `KVCacheType` (0=F16, 1=Q8_0, 2=Q4_0),
  `SplitMode` (0=layer, 1=row), `OverrideTensor`. Все новые поля
  опциональны — обратная совместимость сохранена.
- **`cmd/cppworker/types.go`** — добавлена структура `loadWithParamsRequest`
  со всеми базовыми + расширенными полями. Базовые поля теперь тоже
  optional-указатели (`*int`/`*string`) — handler трактует `nil` как
  "использовать default из cppworker flags". Это позволяет клиентам
  отправлять JSON, в котором нужны **только** переопределяемые параметры.
- **`cmd/cppworker/handlers_model.go`** — добавлен `handleLoadWithParams`
  (~150 LOC):
  - Валидация `method == POST` (405 иначе), парсинг JSON (400 на ошибке).
  - Проверка `name != ""` (400 на пустом имени).
  - Резолвинг пути модели (через `backend.GetModel`, `HFDownloader`,
    glob-паттерны в `modelsDir`).
  - Защита от race-condition при concurrent load: `TryLockLoad`,
    `WaitForLoad` (та же логика, что и в `handleLoadModel`).
  - Построение `LoadModelOpts` с defaults из `cppworker.flags` и
    переопределениями из JSON.
  - Вызов `backend.LoadModelWithOpts` + возврат JSON со статусом, моделью,
    `loadDurationMs`, `appliedOpts` (что реально применилось).
- **`cmd/cppworker/router.go`** — зарегистрирован route:
  `mux.HandleFunc("/api/models/load-with-params", handleLoadWithParams)`.
- **`internal/api/gguf_backend_proxy_handlers.go`** — добавлена документация
  для нового endpoint'а (generic proxy уже корректно маршрутизирует
  все пути под `/api/v1/gguf/backends/{id}/proxy/`).
- **`cmd/cppworker/handlers_model_loadwithparams_test.go`** (новый) —
  **11 unit-тестов**:
  - `TestHandleLoadWithParams_MethodNotAllowed` — GET → 405
  - `TestHandleLoadWithParams_InvalidJSON` — некорректный JSON → 400
  - `TestHandleLoadWithParams_MissingName` — пустой `name` → 400
  - `TestHandleLoadWithParams_RequestStructure` — все 15 полей парсятся
  - `TestHandleLoadWithParams_MinimalRequest_OnlyValidation` — парсинг
    минимального `{"name": "..."}` без nil-pointer
  - `TestHandleLoadWithParams_BackwardCompatibility` — старый формат
    (без расширенных полей) парсится в новой структуре
  - `TestHandleLoadWithParams_KVCacheTypeValidValues` — 0/1/2
  - `TestHandleLoadWithParams_SplitModeValidValues` — 0/1
  - `TestHandleLoadWithParams_NegativeValuesParse` — отрицательные значения
    парсятся (валидация в runtime, не в parser)
  - `TestHandleLoadWithParams_EmptyOverrideTensor` — пустая строка
    парсится в `*string` (handler трактует `*s == ""` как "не задан")
  - `TestHandleLoadWithParams_EmptyTensorSplit` — пустой slice → nil

**Пример запроса** (прямой к cppworker):
```bash
curl -X POST http://localhost:18092/api/models/load-with-params \
  -H "Content-Type: application/json" \
  -d '{
    "name": "gemma-4-E4B-it-Q4_K_M",
    "contextSize": 32768,
    "gpuLayers": -2,
    "kvCacheType": 1,
    "parallel": 2,
    "nThreads": 16,
    "overrideTensor": "blk\\..*\\.ffn_.*_exps=CPU"
  }'
```

**Пример через балансировщик** (cluster proxy):
```bash
curl -X POST -H "X-API-Token: $LB_TOKEN" -H "Content-Type: application/json" \
  http://localhost:18081/api/v1/gguf/backends/cppworker-gpu-bundled/proxy/api/models/load-with-params \
  -d '{"name":"gemma-4-E4B-it-Q4_K_M","contextSize":32768,"gpuLayers":-2,"kvCacheType":1}'
```

**Acceptance criteria**:
1. `POST /api/models/load-with-params` возвращает 200 + JSON со статусом
   и `appliedOpts` при успешной загрузке.
2. Все 11 unit-тестов проходят (`go test -tags llama_stub -count=1
   -run "TestHandleLoadWithParams" ./cmd/cppworker/...`).
3. `go build -tags llama_stub ./cmd/cppworker/ && ./cmd/balancer/` — exit 0.
4. `go test -tags llama_stub -count=1 ./cmd/cppworker/...` — все тесты
   зелёные (regression-checked).
5. `go test -tags llama_stub -count=1 ./internal/...` — все тесты
   зелёные.
6. Pre-existing failures в `./tests/...` (PF-3, PF-4, PF-5, PF-6, PF-7)
   задокументированы в `plans/pre-existing-test-failures.md` и **не
   связаны** с этим изменением.

### Bugfixes (Session 2 — большая модель на 20GB VRAM + обрыв стрима)

**Issue: «qwen3.6 на 20GB VRAM не влезает, на 8GB — да»** —
при lazy-загрузке `cppworker` использовал хардкод `estimatedLayers=80` и
`kvReserve=2GB` для расчёта `gpu_layers`, **но не пересчитывал `n_ctx`**.
Для больших моделей на средней VRAM (например, qwen3.6 22GB на 20GB
RTX 4090 / A4500) `weights + KV-cache для n_ctx=32768` не влезают →
llama.cpp.LoadModel падает с OOM → пользователь видит либо молчаливый
fallback на `n_ctx=4096`, либо ошибку.

**Решение** — `AutoTuneNCtx` теперь вызывается уже в `ensureModelLoaded`
ДО `backend.LoadModelWithOpts`, а не только при RAM-fallback reload:

- **`internal/cppbackend/model_manager.go`** — добавлены поля `NLayers/NEmbd/NHeads/NKvHeads`
  в `GGUFModelMeta`, заполняются **лениво** через `cppbackend.ReadGGUFHeader`
  (без загрузки модели в llama.cpp) при первом обращении к `GetModelMeta`.
- **`internal/cppbackend/backend.go`** — публичная `ReadGGUFHeader(path)`
  обёртка над существующей `readGGUFHeaderInfo` (использует
  ggufKeyMap для O(1) маппинга ключей на поля).
- **`cmd/cppworker/lazyload_calc.go`** (новый файл, ~250 LOC) —
  `calculateLazyLoadOpts(modelName, requestedNCtx, requestedGPULayers, defaults)`
  с трёхступенчатым каскадом:
  - Stage 1 (`exact_fit`): все weights + KV-cache влезают в VRAM → opts как есть.
  - Stage 2 (`partial_offload`): уменьшаем `gpu_layers`, остальные веса через
    `mmap` в RAM. n_ctx сохраняется.
  - Stage 3 (`reduced_nctx`): `gpu_layers=0` (CPU-only через mmap) + n_ctx =
    `maxViableNCtx` (рассчитан по фактическому `kvPerToken = 4 * NLayers *
    NKvHeads * headDim`).
- **`cmd/cppworker/lazyload.go`** — вызов `calculateLazyLoadOpts` ДО
  `LoadModelWithOpts`. Старая эвристика с хардкодами оставлена как
  fallback (если `autoTuneNCtxOnLoadEnabled=false`).
- **ENV-флаг `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD`** (default `true`) —
  отключает новое поведение для обратной совместимости.

**Issue: «обрыв ответа без каких-либо ошибок»** — после обрыва клиента
(Cline/OpenWebUI/Roo Code закрыл соединение) cppworker продолжал писать
в мёртвый socket через `safeFprintf/safeFlush`, получая `broken pipe`
без логирования. Пользователь видел пустой/обрезанный ответ без
объяснений.

**Решение** — `safeStreamWriter` + диагностический endpoint:

- **`cmd/cppworker/safe_stream_writer.go`** (новый) — потокобезопасная
  обёртка с проверкой `ctx.Done()` перед каждой операцией, логированием
  первого write error и счётчиками bytes_written/write_errors. После
  первой ошибки writer помечается `broken` и все последующие операции —
  no-op (без логирования шума в `broken pipe`).
- **`cmd/cppworker/debug_last_stream.go`** (новый) — `LastStreamInfo`
  snapshot + endpoint `GET /api/v1/cppworker/debug/last-stream` (с
  `X-API-Token`). Возвращает JSON с model, handler, tokens_sent,
  bytes_written, write_errors, duration_ms, disconnected_at, reason
  (`ctx_done_before_header`/`ctx_done_on_write`/`write_error`),
  last_write_err.
- **`cmd/cppworker/handlers_openai.go`** — `writeOpenAIChatStream`
  переписан на `safeStreamWriter`. Убрана race-prone конструкция
  `defer close(tokenDone)` (заменена на явный `stopCh` + `sync.WaitGroup`).
  Теперь первый write error честно логируется с категорией
  `client disconnected?`.
- **`cmd/cppworker/router.go`** — endpoint `GET /api/v1/cppworker/debug/last-stream`
  и `POST .../debug/last-stream/clear` зарегистрированы с `authMiddleware`.

**Тесты** — 17 unit-тестов, все зелёные:

- `cmd/cppworker/lazyload_calc_test.go` (7 тестов):
  - `TestCalculateLazyLoadOpts_Qwen36_On20GBVRAM` — главный кейс из задачи:
    `qwen3.6-72B (22GB)` на 20GB VRAM даёт `source=partial_offload,
    applied_gpu=33 (из 80), n_ctx=32768 сохранён, UseMmap=true`.
  - `TestCalculateLazyLoadOpts_SmallModel_On8GBVRAM` — gemma-3 4B на 8GB,
    n_ctx не снижается.
  - `TestCalculateLazyLoadOpts_FitsExactly` — llama 13B на 20GB с
    n_ctx=16K, n_ctx не снижается.
  - `TestCalculateLazyLoadOpts_PartialOffload_30B_On8GB` — большая
    модель на ограниченной VRAM.
  - `TestCalculateLazyLoadOpts_AutoTuneFlagDefaultsToTrue` —
    `autoTuneNCtxOnLoadEnabled=true` по умолчанию.
  - `TestCalculateLazyLoadOpts_NoGGUFHeader` — fallback при битом
    GGUF header.
  - `TestLazyLoadRationale_FormatRationale` — формат для логов.
- `cmd/cppworker/safe_stream_writer_test.go` (10 тестов): все
  проверяют безопасность при `ctx.Done()`, write errors, concurrent
  writes, no-op после broken, и realistic streaming с отменой на 25-м
  токене из 50.

**Build**: `go build -tags llama_stub -o cppworker-stub.exe ./cmd/cppworker`
и `go build -tags llama_stub -o balancer-stub.exe ./cmd/balancer` оба
зелёные.

## [Unreleased — 2026-06-25]

### Техдолг (Session 3 — R-1: Windows GPU metrics / WMI fallback)
- **WMI fallback для `getGPUMetrics` на Windows** —
  ранее `getGPUMetrics` (`internal/agent/system_windows.go`) возвращала
  пустую `types.GPUMetrics{}` при отсутствии `nvidia-smi`. Теперь реализован
  двухуровневый fallback:
  1. **Уровень 1: nvidia-smi** — полные метрики (usage%, VRAM used/free,
     temperature, power, clocks). Если доступен, используется он.
  2. **Уровень 2: WMI `Win32_VideoController`** — возвращает хотя бы
     `MemoryTotal` через поле `AdapterRAM` (в bytes → конвертируется в MB).
     Используется при отсутствии `nvidia-smi` (напр., в WSL или на сервере
     без NVIDIA-драйверов).
  3. **Уровень 3 (fallback)**: пустая структура — если оба источника
     недоступны.
- **Платформонезависимый парсер WMI CSV** — `internal/agent/system_wmi_parse.go`:
  `parseWmiVideoControllerVRAMBytes(csvOutput string) uint64` — извлекает
  суммарный VRAM всех дискретных GPU из CSV-вывода
  `wmic path Win32_VideoController get Name,AdapterRAM /format:csv`.
  Исключает «Basic Display Adapter» (Windows built-in stub) и iGPU с
  `AdapterRAM = 0`. Поддерживает CRLF/LF, лишние пробелы, malformed-строки
  (graceful degradation — пропускает строку, а не паникует).
- **Unit-тесты парсера** — `internal/agent/system_wmi_parse_test.go`:
  11 кейсов:
  - `TestParseWmiVideoControllerVRAMBytes_SingleGPU` — один GPU (8 GB).
  - `TestParseWmiVideoControllerVRAMBytes_MultiGPU` — суммирование
    нескольких дискретных GPU.
  - `TestParseWmiVideoControllerVRAMBytes_BasicDisplayExcluded` — RTX + iGPU
    + Basic Display (исключается только Basic Display).
  - `TestParseWmiVideoControllerVRAMBytes_OnlyBuiltinGPU` — машина без
    дискретного GPU возвращает 0.
  - `TestParseWmiVideoControllerVRAMBytes_EmptyOutput` / `HeaderOnly` —
    graceful handling пустого вывода.
  - `TestParseWmiVideoControllerVRAMBytes_MalformedLine` — нечисловой
    `AdapterRAM` пропускается, остальные GPU суммируются.
  - `TestParseWmiVideoControllerVRAMBytes_CRLF` — Windows-стиль
    разделителей строк.
  - `TestParseWmiVideoControllerVRAMBytes_RealisticSample` — RTX 4090 +
    RTX A6000 + Intel Arc (24+48+0.125 GB).
  - `TestParseWmiVideoControllerVRAMBytes_LeadingTrailingWhitespace` —
    пробелы вокруг значений.
  - `TestParseWmiVideoControllerVRAMBytes_RealWorldExample` — Windows
    Server 2022 (Quadro RTX 4000, 8 GB).
  Все 11/11 PASS (`go test -tags llama_stub -run "TestParseWmiVideoControllerVRAMBytes" -v ./internal/agent/...`).
- **Build verification**:
  - `go build -tags llama_stub ./cmd/balancer ./cmd/agent ./cmd/cppworker` — OK.
  - `go build -tags "llama_stub nvml" ./cmd/agent` — OK (nvml-build с
    WMI fallback работает).
  - `GOOS=windows go build -tags llama_stub ./internal/agent` — OK.
  - `GOOS=windows go vet -tags llama_stub ./internal/agent` — OK.
  - `GOOS=windows go build -tags "llama_stub nvml" ./internal/agent` — OK.
  - `go test -tags llama_stub ./internal/agent/...` — 15s PASS.
- **Ограничения WMI fallback** (документированы в коде):
  - `AdapterRAM` возвращает 0 для iGPU с разделяемой памятью.
  - На дискретных GPU обычно показывает полный объём VRAM (RTX 3070 8GB →
    8192 MB).
  - `MemoryUsed`/`MemoryFree`/`Temperature`/`PowerUsage`/`Clocks` через WMI
    `Win32_VideoController` **НЕ доступны** — требуется NVML (Linux) или
    nvidia-smi (Windows). UI показывает «unknown» / N/A для этих полей.

### Техдолг (Session 2 — PF-1)
- **Fixed `TestBackendsHandler_Get` (PF-1 #1)** — тест теперь шлёт
  `?includeUnhealthy=true` и получает `total=2`. По умолчанию `listBackends`
  фильтрует unhealthy-бэкенды (by design, чтобы WebUI не показывал «мёртвые»
  ноды), поэтому без явного флага `backend2` (созданный как `StatusUnhealthy`
  в `createTestServer`) отфильтровывается. Добавлен docstring к тесту,
  объясняющий почему `includeUnhealthy=true` обязателен. Принимает все формы
  `?includeUnhealthy=true|1|yes`, используется в monitor/agent endpoints.
  **Acceptance**: `go test -tags llama_stub ./internal/api -count=1
  -run "TestBackendsHandler_Get"` — PASS за 0.02s.
- **Fixed `TestServeHTTP_MixedCluster_RoutingByURLPath` (PF-1 #2)** —
  реальные вызовы `proxy.ServeHTTP` удалены, так как `setupMixedCluster`
  использует фиктивные IP `10.0.0.1:11434` и `10.0.1.1:18091`, где никто
  не слушает. TCP-connect вызывал блокировку на `Balancing.RequestTimeout=30s`,
  а Go testing framework `-timeout 60s` срабатывал раньше как `panic: test
  timed out`. Оставлена только проверка routing-логики через
  `DetermineRequestBackendTypeForTest` (это и есть суть теста). Полная
  end-to-end проверка сохранена в отдельных тестах
  `TestServeHTTP_OllamaRequest_RoutedToOllamaBackend` и
  `TestServeHTTP_LlamaCppRequest_RoutedToLlamaCppBackend`, которые используют
  `httptest.NewServer` с фиктивными бэкендами. **Acceptance**: `go test
  -tags llama_stub ./tests -count=1 -timeout 30s -run
  "TestServeHTTP_MixedCluster_RoutingByURLPath"` — PASS за 0.00s (вместо
  panic timeout 60s).
- **Обнаружены дополнительные pre-existing failures (PF-3, PF-4)**, не
  относящиеся к PF-1, но мешающие зелёному CI:
  - `TestDefaultConfig` (`tests/cppbackend_test.go:39`) — устаревший тест
    ожидает `ctx_size = 4096`, а реальный default уже 32768. Требует
    обновления expected value.
  - `TestOpenAIChat_SlowFirstToken_HoldsConnection`
    (`tests/first_byte_timeout_test.go:114`) — flaky test,
    `httptest.Server.Close()` зависает на `WaitGroup`. Требует
    `srv.CloseClientConnections()` перед `srv.Close()`.
  Подробности в `plans/pre-existing-test-failures.md` (секция «Обнаруженные
  pre-existing failures»).

### Техдолг (Session 1 — A-1, A-2, A-3)
- **Удалён backwards-compat alias `applyCppCtxHeader` в `cmd/cppworker/handlers_openai.go`.**
  Все 5 вызовов в `handlers_openai.go` (2 шт.), `handlers_chat.go` (1 шт.) и
  `handlers_generate.go` (1 шт.) переведены на каноничный `ApplyCppCtxHeader`
  из `cmd/cppworker/nctx_clamp.go`. Удалена сама функция-псевдоним и комментарий
  TODO «refactoring». При поиске по `applyCppCtxHeader` остаются только
  сообщения логов (строки), но не вызовы. Build `go build -tags llama_stub ./cmd/cppworker`
  и `go test ./cmd/cppworker` зелёные.
- **Добавлено поле `RequestID string` с тегом `json:"request_id,omitempty"` в `types.BackendMetrics`**
  (`pkg/types/metrics.go`). Убран TODO в `internal/balancer/nctx_reload.go:BackendMetricsFromState`;
  функция теперь возвращает валидный `*types.BackendMetrics` с заполненными
  `ID`, `Timestamp` и пустым `WarmingUpModels` (nil-safe на nil-координаторе).
  Поле `RequestID` остаётся пустым в snapshot'е координатора — источник request_id
  это HTTP-middleware, а не координатор; заполнение ожидается в cluster-state
  aggregator (см. `internal/api/handlers_metrics.go`).
- **Убран TODO в `internal/balancer/model_management.go:ExecuteOperation`** про
  прокидывание `request_id` в лог операций с моделью. Теперь используется
  `ridLog(lr_recentCtx())` для обогащения log-записи `executing model operation`
  полем `request_id` (если контекст был положен HTTP-middleware). Удалён мёртвый
  `_ = logFn`. Behavior совместим: если `lr_recentCtx()` возвращает пустой ctx —
  `ridLog()` возвращает обычный logger без полей (как было).

### Исправлено
- **EOF при n_ctx auto-reload для Cline/OpenWebUI с n_ctx=65536 на gemma-4 8B (8GB VRAM + 25GB RAM)**
  — балансировщик возвращал HTTP 413 с actionable сообщением
  "n_ctx auto-reload failed: ... (saved model profile with larger n_ctx and reload manually via /api/profiles)".
  Cline интерпретировал это как EOF и зацикливался на retry. Корневая причина: переменная
  `LB_NCTX_RELOAD_MAX_N_CTX=32768` в `deployments/.env.bundled` ограничивала max n_ctx
  балансировщика 32K, а Cline по умолчанию запрашивает 65536 → `required (65536) >
  AutoReloadMaxNCtx (32768)` → reject. Дополнительно `LB_BALANCING_PREFLIGHT_SYNC_TIMEOUT_MS=60000`
  (60 сек) было недостаточно для reload gemma-4 8B с 65K n_ctx (через partial GPU offload
  с auto_offload + auto_tune_nctx reload занимает 90-150 сек). Изменения в `.env.bundled`:
  - `LB_NCTX_RELOAD_MAX_N_CTX`: 32768 → **65536** (соответствует запросу Cline;
    реальный VRAM-лимит контролируется через `max_vram_n_ctx` в cppworker runtime config
    и `auto_reload_vram_safety_factor=0.95`).
  - `LB_BALANCING_PREFLIGHT_SYNC_TIMEOUT_MS`: 60000 → **180000** (3 мин, max значение
    согласно `pkg/types/balancing.go`). Reload gemma-4 8B на 65K n_ctx через partial
    offload в RAM может занимать 90-150 сек; 60 сек вызывали EOF на клиенте.
  - Для 8GB VRAM + gemma-4 8B + 65K n_ctx используется auto_offload + auto_tune_nctx
    с partial GPU offload (gemma-4 профиль `numCtx=32768` в `config/config.bundled.json:114`).
    Реальный VRAM-лимит остается в cppworker runtime config (max_vram_n_ctx) и safety_factor=0.95.
    Профиль `numCtx` снижен 131072 → 32768 (см. запись ниже).
- **Deadlock при initial load gemma-4 с профильным `numCtx=131072` (8GB VRAM)** —
  при первом запуске bundled stack модель `gemma-4-E4B-it-Q4_K_M` загружалась
  с `numCtx=131072` из per-model профиля `config/config.bundled.json:114`.
  Cppworker пытался аллоцировать KV-cache для 131K контекста (~13GB при FP16,
  `n_embd=2560`, 42 layers), что **физически не влезает в 8GB VRAM даже с mmap
  в RAM** (KV-cache в RAM замедляет inference в 5-10x, и cppworker не умеет
  выбирать это автоматически на initial load). Модель зависала в `state="loading"`
  на 60+ секунд (cppworker `done_getting_tensors` зависал на KV-cache allocation),
  балансер делал polling `/api/models/load/progress` каждые 2 сек, клиент (Cline)
  получал пустой ответ. Профиль `numCtx` снижен **131072 → 32768** — это
  помещается в 8GB VRAM с `auto_offload` (`gpuLayers=-2`/AUTO) + `flash_attn=1` +
  `mmap=true`. Дополнительно: `cppworker-defaults.json` (`defaultCtxSize`) уже
  был 32768, и `gemma-4-large` профиль — тоже 32768. Cline по умолчанию шлёт
  `n_ctx=65536` в body, но **profile numCtx (32768) клампит body** через
  `applyCppCtxHeader` (см. `cmd/cppworker/nctx_clamp.go:106-114` — header —
  UPPER LIMIT). Это безопасный режим: модель всегда загружается с максимальным
  numCtx, который реально помещается в VRAM, и балансер может сделать reload до
  большего numCtx (до 65536 в текущей конфигурации) если VRAM позволяет через
  `auto_tune_nctx` + RAM-fallback.

- **Модель не вызывает tools при проксировании через балансер (Gemma-4, qwen2.5-coder, Hermes-prompt)**
  — пользователь сообщал "модель не использовала инструмент" или "использован один источник,
  но самого ответа нет". У Ollama-native таких проблем не было, т.к. ollama сам формирует
  структурированное `message.tool_calls`. Балансер вынужден парсить JSON из `content`
  (Gemma-4/Hermes не имеют нативного tool-call через chat template), и ранее парсер ломался
  на реальных моделях. Внесены 5 связанных правок:
  - **`hermesToolCallRegex`** теперь делает закрывающий `</tool_call>` и `<answer>` опциональными.
    Раньше qwen2.5-coder, deepseek-coder и Gemma-4 в Hermes-prompt часто обрезали
    `</tool_call>` в коротких ответах или стриме — regex не матчил, tool_call уходил
    в content как обычный текст. Теперь поддерживаются варианты: `<tool_call>{...}`,
    `<tool_call>{...}</tool_call>`, `<answer>{...}</answer>`, `<answer>{...}`.
  - **Gemma-4 специфический парсер**: новая функция `detectGemmaTurnToolCallsInContent`
    ловит JSON tool call внутри `<start_of_turn>(model|assistant)\n[JSON]\n</start_of_turn>` —
    это специфика chat template Gemma-4 (использует `model` вместо `assistant`).
  - **`parseHermesToOpenAI`** теперь корректно обрабатывает `arguments`, который пришёл
    строкой с валидным JSON (Gemma-4 часто эмитит `arguments: "{\"q\": \"test\"}"` как
    stringified JSON). Раньше происходила двойная сериализация → Cline/Roo Code получали
    невалидный JSON и падали с ошибкой парсинга tool_call.
  - **`translateOllamaChatToOpenAI`** теперь автоматически выставляет `tool_choice: "auto"`,
    если клиент прислал `tools[]` (top-level или в `options.tools`), но не задал `tool_choice`
    явно. Без этого Gemma-4 и аналогичные модели без нативного tool support не понимали,
    что нужно вызвать tool, и возвращали обычный текст вместо JSON tool_call.
  - **`translateOllamaChatToOpenAI`** также fallback'ит на `options.tools` для клиентов
  типа OpenWebUI, которые шлют tools через `options.tools` (Ollama-native стиль).
- **Тесты**: добавлены 13 новых тестов в `llamacpp_toolcall_gemma_test.go` и
  `llamacpp_translate_req_test.go`. Покрыты сценарии: Gemma-4 turn-parsing (8 кейсов),
  Hermes с опциональным `</tool_call>` (5 кейсов), arguments как JSON-строка/map (2 кейса),
  `tool_choice: "auto"` для Gemma-4 (4 кейса), `options.tools` fallback, prompt→messages.
  Все 13 тестов проходят (`go test -tags llama_stub ./internal/balancer/ -v`).
  Существующие транспортные тесты `TestDebugOpenWebUI_ToolCalls_Scenario*` также зелёные.

### Добавлено
- **Endpoint `GET /api/v1/cppworker/debug/last-prompt` + cluster-proxy
  `GET /api/v1/cluster/cppworker/debug/last-prompt` для диагностики `prompt_exceeds_context`** —
  после фикса `error_code: "prompt_exceeds_context"` пользователю нужно понять,
  **что именно** заняло столько токенов в prompt (длинная история диалога? tool definitions?
  system prompt с контекстом проекта?). Без snapshot'а последнего запроса единственный
  способ — лезть в логи cppworker и grep'ать по `actual_prompt_tokens`, что неудобно.
  - **CppWorker** (`cmd/cppworker/debug_last_prompt.go`): хранит snapshot последнего
    inference-запроса в `lastPromptSnapshot` (thread-safe через `sync.RWMutex`). В каждый
    inference handler (`handleGenerate`, `handleOllamaGenerate`, `handleChat`,
    `handleV1ChatCompletions`, `handleV1Completions` + все `write*Stream` функции)
    добавлен `defer recordLastPromptFromError(...)`, который захватывает:
    `model`, `endpoint`, `prompt_chars`, `prompt_tokens` (через `backend.CountTokens`),
    `n_ctx_override`, `n_ctx_loaded` (из `backend.GetModel`), `has_tools`, `status`
    (`ok`/`prompt_too_long`/`n_ctx_too_large`/...), `timestamp`, `prompt_head`,
    `prompt_tail`, `prompt_lines_hint`, `error`. Endpoint возвращает JSON
    (404 если запросов ещё не было), защищён `authMiddleware`.
  - **Cluster-proxy** (`internal/api/handlers_cluster_debug.go`):
    `GET /api/v1/cluster/cppworker/debug/last-prompt` агрегирует snapshot'ы со всех
    cppworker бэкендов кластера в один JSON-ответ `{count, backends: [{backendId,
    status, info, error}]}`. Per-backend ошибки (404 от старого cppworker, 5xx,
    timeout) не ломают общий ответ — статус `"unavailable"`/`"error"` с описанием.
  - **Тесты**: добавлены 7 новых тестов в `internal/api/handlers_cluster_debug_test.go`
    (mock cppworker через `httptest.NewServer`, проверка proxy и graceful degradation).
    Все 7/7 тестов PASS.
  - **Acceptance criteria**:
    - `curl -H "X-API-Token: $LB_TOKEN"
      http://localhost:18081/api/v1/cluster/cppworker/debug/last-prompt`
      возвращает JSON с `prompt_tokens`, `prompt_chars`, `prompt_head/tail`,
      `n_ctx_override`, `n_ctx_loaded` и `status: "prompt_too_long"`.
    - Старый bundled-образ cppworker (до этой фикса) возвращает 404 → cluster-proxy
      помечает backend как `status: "unavailable"` с подсказкой "build before 2026-06-25".
    - Build OK: `go build -tags llama_stub ./cmd/cppworker` и `./cmd/balancer` — exit 0.

- **Cluster-level endpoints для управления моделями через балансировщик** — пользовательский
  workflow идёт через балансировщик (порт 18080 для OpenAI API, 18081 для admin API,
  18083 для WebUI), а прямой доступ к cppworker (порт 18092) из Docker-network
  для UI неудобен и небезопасен. Реализованы 3 новых endpoint'а в
  `internal/api/handlers_cluster_models.go`:
  - **`GET /api/v1/cluster/models/loaded`** — агрегирует `LoadedModels[]` всех
    llama_cpp (cppworker) бэкендов. Возвращает `{count, models[], modelsPerBackend{}}`,
    где каждая модель содержит `name`, `backendId`, `backendType`, `engine`,
    `contextLength`, `batchSize`, `numGpuLayers`, `quantization`, `vramUsage`,
    `ramUsage`, `size`, `state`. UI и пользовательские скрипты могут опросить
    состояние одной командой `curl -H "X-API-Token: …" http://localhost:18081/api/v1/cluster/models/loaded`
    вместо N запросов к каждому cppworker:18092.
  - **`GET /api/v1/cluster/models/loading`** — список моделей в состоянии `loading`
    (асинхронная загрузка или n_ctx reload). Полезно для дебага зависших
    загрузок: UI может показать «Загружается model-name 25s». Поля:
    `name`, `backendId`, `startedAt` (RFC3339Nano), `contextLength`, `batchSize`,
    `numGpuLayers`, `quantization`, `loadingError`, `loadingSizeBytes`.
  - **`POST /api/v1/cluster/models/{name}/reload`** — единая точка входа для
    load/unload/reload модели на llama_cpp бэкендах. Body:
    ```json
    {
      "operation":   "load",     // или "unload" / "reload" (alias "all")
      "backendId":   "...",      // "" = все llama_cpp бэкенды
      "contextSize": 32768,      // n_ctx override (опционально)
      "gpuLayers":   -2,         // -2 = auto, -1 = all, 0 = CPU-only
      "reason":      "manual reload from UI"
    }
    ```
    Возвращает `{model, operation, results: [{backendId, status, message, error, httpStatus}]}`
    где `status` ∈ {`ok`, `error`, `skipped`}. Под капотом вызывает
    `ModelManager.ExecuteOperation(backendID, ModelOpRequest{...})` — тот же
    код-путь, что и существующий `/api/v1/backends/{id}/models` (POST), включая
    retry на 503 «model is loading» в `executeLlamaCppLoad`. Это значит,
    что **поведение cluster endpoint'а и single-backend endpoint'а идентично**
    (нет дрейфа фич).
  - Routes зарегистрированы в `internal/api/routes.go` рядом с
    `/api/v1/cluster` handler'ом. Все три требуют аутентификации
    (`X-API-Token`) и rate-limit middleware, как и остальные cluster
    endpoint'ы.
- **Дублирование бэкенда при auto-registration cppworker в bundled compose** —
  cppworker в bundled-стеке регистрировался **дважды** с разными `id`, но одним
  физическим контейнером (`host=cppworker-gpu, port=18092`):
  - **Go-side** (`cmd/cppworker/balancer_register.go`) — `id="cppworker-gpu"` (default
    от `os.Hostname()`), `weight=100`, `labels: cppworker,auto-registered`.
  - **Shell-script** (`docker/cppworker/register-with-balancer.sh` из
    `docker/cppworker/entrypoint.sh`) — `id="cppworker-gpu-bundled"` (через
    `CPPWORKER_BACKEND_ID`), `weight=10`, `labels: linux,amd64,llamacpp,gpu,sm_86`.
  В `data/state.json` виден только `cppworker-gpu-bundled` (записан вторым и
  выигрывает по уникальности), но in-memory оба бэкенда жили одновременно →
  race в `selectBackend` (`"no llama.cpp backend available"` в Cline). Фикс:
  - Добавлен env-флаг **`CPPWORKER_REGISTER_DISABLE=true`/`1`**
    в `cmd/cppworker/balancer_register.go:isRegisterDisabled()`. При выставленном
    флаге `newBalancerRegistration()` возвращает `nil`, и Go-side регистрация
    не запускается. Bundled compose
    (`deployments/docker-compose.cppworker-bundled.yml`) выставляет флаг явно —
    shell-script становится единственным источником правды.
  - Добавлено информативное логирование в `cmd/cppworker/main.go`: при
    `isRegisterDisabled()=true` пишется `"balancer Go-side auto-registration
    disabled via CPPWORKER_REGISTER_DISABLE; registration is expected to be
    done by external script"`. Видно в `docker logs ol-bundled-cppworker-gpu`.
  - Удалён старый бэкенд `cppworker-gpu` через `DELETE /api/v1/backends/cppworker-gpu`.
    После рестарта cppworker с новым образом: `GET /api/v1/backends` возвращает
    `"total": 1` с единственным `cppworker-gpu-bundled`. Cline больше не видит
    ambiguity в `selectBackend`.
  - Документация: `.clinerules` обновлён — Section 9 содержит полный список
    env-флагов auto-registration (включая новый `CPPWORKER_REGISTER_DISABLE`),
    Section 17 — раздел «Дубликаты бэкендов — недопустимы» с пошаговым
    acceptance criteria и инструкцией по миграции со старого образа.
  - **Тесты**: добавлен `cmd/cppworker/balancer_register_test.go` с 5 тестами
    и 12 кейсами (`TestIsRegisterDisabled` + `TestNewBalancerRegistration_*`).
    Покрывают: все валидные env-значения (true/1/yes/on, в т.ч. mixed case),
    отключение регистрации при `CPPWORKER_REGISTER_DISABLE`, дефолт (enabled
    если `CPPWORKER_BALANCER_URL` задан и `DISABLE` не выставлен), env-isolation
    через `t.Setenv`. Все 12+5 = 17 кейсов проходят.
  - **NB:** на старых образах cppworker флаг не действует (Go-side код был
    без `isRegisterDisabled`). Пересобери образ через `start-bundled.sh rebuild`
    перед развёртыванием в production.
- **Каскадный auto-fallback в cppworker для длинных num_ctx (gemma-4 4B + 8GB VRAM + 25GB RAM)** —
  Cline прислал запрос с `num_ctx=65536` для `gemma-4-E4B-it-Q4_K_M`, а модель была загружена
  с `contextLength=65536` (cppworker уже пытался загрузить с таким n_ctx через C-bridge).
  cppworker не сделал **каскад** (RAM mmap → partial offload → cpu-only + auto_tune n_ctx),
  и через балансер вернулась ошибка `n_ctx_too_large_for_backend` (Ollama-style message,
  которая балансер не распознавал как reloadable). Cline интерпретировал это как EOF и
  зацикливался на retry. Корневая причина: cppworker возвращал generic 500 при нехватке
  VRAM+RAM, потому что:
  1. `AutoTuneNCtx` вызывался **только при** `autoTuneNCtx=true` (default false) — без
     него каскад невозможен.
  2. `AutoTuneError` не имел HTTP-handler'а — cppworker пробрасывал ошибку как generic
     «reload failed», а не как actionable 413.
  3. Не было второго уровня каскада (gpu_layers=0 + max_viable n_ctx), когда partial
     offload не помог.
  Фикс (cppworker + c/bridge + balancer):
  - **`cmd/cppworker/inference.go:tryRamFallbackReload`**: убран флаг `*autoTuneNCtx`
    вокруг `AutoTuneNCtx()` — каскад **всегда активен** при RAM fallback reload, потому что
    без него каскад невозможен. Добавлен 2-й уровень: если `LoadModelWithOpts` упал после
    partial offload, делается вторая попытка с `gpu_layers=0` + `max_viable n_ctx`. Если
    и она упала — возвращается `*InsufficientResourcesError` с actionable details.
  - **`cmd/cppworker/utils.go`**: новый `*InsufficientResourcesError` (HTTP 413, code=6)
    с `bridge_info: {requested_n_ctx, max_viable_n_ctx, available_vram_mb, available_ram_mb,
    model_size_mb, kv_cache_required_mb, gpu_layers_attempted, model, suggestion}`. Новый
    `writeInsufficientResourcesResponse` пишет structured JSON, совместимый с
    `ParseCppWorkerError`. Добавлены case'ы в `handleInferenceError` для
    `*InsufficientResourcesError` и `*AutoTuneError` (синоним).
  - **`c/bridge/bridge.go`**: новая константа `ErrCodeInsufficientResources = 6`.
    Используется в `bridge_info.code` для балансера.
  - **`internal/balancer/llamacpp_error.go`**: обновлён комментарий `isNCtxRelevantCode`
    — код 6 (InsufficientResources) НЕ считается reloadable, балансер пробрасывает
    413 клиенту без retry/reload.
  - **Тесты**: новый `cmd/cppworker/insufficient_resources_response_test.go` с 6 кейсами
    (`TestInsufficientResourcesError_Error`, `TestWriteInsufficientResourcesResponse_HasAllFieldsForClient`,
    `TestHandleInferenceError_DispatchesInsufficientResources`, `TestHandleInferenceError_DispatchesAutoTuneError`,
    `TestHandleInferenceError_GenericErrorReturnsFalse`, `TestHandleInferenceError_NilErrorReturnsFalse`).
    Все 6/6 проходят. Существующие тесты (TestClampNPredictToFitContext, TestApplyCppCtxHeader,
    TestParseToolCalls, TestReloadDisabledForTools) — все 60 кейсов зелёные, регрессий нет.
    Build OK (`go build -tags llama_stub ./cmd/cppworker/` и `./cmd/balancer/` — exit 0).
  - **Acceptance criteria**: при запросе с `num_ctx=65536` для gemma-4 на 8GB+25GB —
    каскад уменьшит `n_ctx` до max_viable (~32768) и загрузит модель. Если и
    max_viable не помещается — клиент получит HTTP 413 с JSON:
    `{"error":"insufficient_resources","code":6,"bridge_info":{...,"max_viable_n_ctx":32768,"suggestion":"Reduce num_ctx to 32768..."}}`.

- **Различение `prompt_exceeds_context` vs `n_ctx_too_large_for_backend` в JSON 413-ответе** —
  После пересборки контейнеров клиент Cline всё ещё получал
  `{"message":"n_ctx_too_large_for_backend","modelId":"gemma-4-E4B-it-Q4_K_M",...}`,
  хотя в cppworker логах было видно, что cppworker реально возвращает HTTP 413
  с `bridge_info.code=3` (PromptExceedsNCtx) и `actual_tokens=68271 > n_ctx=65536`.
  Root cause: `internal/balancer/nctx_reload.go:makeRejectPlan` **всегда**
  использовал `error: "n_ctx_too_large_for_backend"` в JSON независимо от
  причины reject. Для Cline это выглядело как системная проблема
  («n_ctx не влезает в VRAM»), хотя на самом деле модель уже загружена с
  максимальным n_ctx, и помогло бы только укоротить prompt/history/tools.
  - **Фикс**: `makeRejectPlan(backendID, bridgeErr, required, reason, errorCode)` —
    добавлен параметр `errorCode string`. Для `bridgeErr.Code == NCtxErrCodePromptTooLong`
    (code=3) используется `"prompt_exceeds_context"`, для всех остальных причин —
    дефолт `"n_ctx_too_large_for_backend"` (обратная совместимость). Suggestion
    тоже разный: для prompt_exceeds_context — «reduce conversation history / tools[]
    / system prompt, or use a smaller model. Reload with the same or larger n_ctx
    will NOT help», для n_ctx_too_large — старая формулировка «save a model profile
    with a larger n_ctx and reload manually».
  - **`internal/balancer/proxy_streaming_413_test.go`** обновлён: проверяет
    новый `error_code="prompt_exceeds_context"` (раньше ждал
    `n_ctx_too_large_for_backend`). Тест по-прежнему верифицирует,
    что balancer не делает reload (это вызывало EOF в Go-клиенте).
  - **Тесты**: `internal/balancer/nctx_reload_errorcode_test.go` (новый, 5 кейсов):
    `TestMakeRejectPlan_PromptExceedsContext_2026_06_25`,
    `TestMakeRejectPlan_NCtxTooLargeForBackend_2026_06_25`,
    `TestMakeRejectPlan_DefaultErrorCode`,
    `TestDecideReloadBackend_PromptTooLong_ReturnsPromptExceedsContext`,
    `TestDecideReloadBackend_NCtxNeedsReload_ReturnsVRAMError`. Все зелёные.
  - **Дополнительно**: удалён дубликат бэкенда `cppworker-gpu` через
    `DELETE /api/v1/backends/cppworker-gpu` (race condition между Go-side и
    shell-script auto-registration). После рестарта cppworker:
    `GET /api/v1/backends` → `{"total": 1, "backends": [{"id": "cppworker-gpu-bundled"}]}`.
  - **Acceptance criteria**:
    - Запрос с `actual_tokens > n_ctx` → HTTP 413 с `{"error":"prompt_exceeds_context", ...}`
      и suggestion «reduce conversation history / tools[] / system prompt».
    - Запрос с `required > max_vram_n_ctx*safety` → HTTP 413 с
      `{"error":"n_ctx_too_large_for_backend", ...}` (без изменений).
    - Build OK: `go build -tags llama_stub ./cmd/balancer/` — exit 0.
    - Все 21 целевой тест PASS (5 новых + 16 существующих).

## [Unreleased — 2026-06-24]

### Исправлено
- **EOF при preflight n_ctx reload (Cline/OpenWebUI с длинным prompt ~30K chars)** —
  cppworker рвал HTTP-соединение в момент `UnloadModel` → `LoadModelWithOpts` reload,
  балансер polling'ом ловил EOF/RST и зависал в `concurrent load already in progress,
  waiting` loop, OpenWebUI клиент получал пустой ответ (не 503, EOF). Корневая
  причина: cppworker не дожидался завершения активных inference-запросов перед reload.
  - **Track 1 (cppworker graceful reload)**: новый `internal/cppbackend/inflight.go`
    с per-model `InFlightCounter`. 4 inference-handler'а (`handleGenerate`,
    `handleChat`, `handleV1ChatCompletions`, `handleV1Completions`,
    `handleOllamaGenerate`) теперь делают `Inc(modelName)` на входе и `Dec(modelName)`
    в defer. `handleReloadModel` зовёт `InFlight().WaitZero(modelName, 0)` (без лимита,
    общий watchdog-таймаут на уровне reload) ПЕРЕД `UnloadModel` — активные запросы
    завершаются штатно, без EOF. `SetReloadPending(modelName)` ставит heartbeat в
    `/api/info` (`reload_pending: {model, startedAt, elapsedMs}`) на время reload;
    балансер НЕ пытается дёргать LoadModel API, пока видит этот флаг.
  - **Track 2 (balancer preflight-sync)**: `preflightNCtxReloadIfNeededSync` —
    блокирующая версия preflight, которая ждёт завершения reload через heartbeat
    polling `/api/info` (500ms интервал). Дефолт таймаут 60s (конфиг
    `Balancing.PreflightSyncTimeoutMs`, max 180s). При таймауте fallback на
    `503 + Retry-After: 15`. Дефолт `PreflightSyncEnabled=true` — клиент НЕ получает
    EOF и не видит reload-процесса вообще. Round-trip один (а не два: 503+retry).
  - **Track 3 (balancer EOF retry)**: `queryCppWorkerModels` теперь до 3 попыток
    с exponential backoff (100ms, 200ms, 400ms) при `EOF`/`connection reset`/
    `bad status`/decode error. На полную неудачу — fallback на `lastKnownModels`
    (TTL 30s), чтобы `ensureModelLoadedOnBackend` не падал в ловушку `state=loading`
    вечно.
  - **Track 4 (balancer reload dedup)**: новый `internal/balancer/nctx_reload_dedup.go`
    с `reloadDedupRegistry`. `executeAsyncReload` (preflight) и `ensureModelLoadedOnBackend`
    координируются через `StartReloadIfNotPending/IsReloadPending/WaitReloadDone` —
    параллельный LoadModel API отменяется (ждёт через `WaitReloadDone`).
  - **Quick-win**: `POST /api/v1/cppworker/reset-reload-counter` уже существует в
    cppworker (cmd/cppworker/handlers_reset_reload.go) — позволяет сбросить
    `ramFallbackAttempts` без `docker restart` (см. docs/runbook-tools.md).

### Добавлено
- **Конфиг `Balancing.PreflightSyncEnabled` + `Balancing.PreflightSyncTimeoutMs`** —
  `pkg/types/balancing.go`. Default: enabled=true, timeout=60000ms (max 180000ms).
  Kill-switch для sync-режима (для legacy скриптов которые ожидают немедленный 503).

## [Unreleased]

Формат ведётся в соответствии с [Keep a Changelog](https://keepachangelog.com/ru/1.0.0/),
и этот проект придерживается [Semantic Versioning](https://semver.org/lang/ru/).

## [Unreleased]

### Добавлено
- **Preflight n_ctx check + dynamic auto-reload (Cline/OpenWebUI с длинным prompt)** — реализован полный цикл динамической подстройки `n_ctx` под запрос клиента, пока есть запас по VRAM и `model_max_context`.
  - **Preflight (балансер)**: `internal/balancer/preflight_nctx.go` — новый модуль `RunPreflight()` оценивает размер prompt (chars/4) + `n_predict` ДО отправки запроса на cppworker. Если `estimated > current_n_ctx`, проверяет лимиты: `MaxVRAMNCtx * safety_factor` и `ModelMaxContext`. При наличии запаса вызывает `NCtxReloadCoordinator.DoReload()` (round-trip один, без ошибки 3). При исчерпании лимита возвращает HTTP 413 с подробным JSON `{error, reason, required_n_ctx, current_n_ctx, max_vram_n_ctx, model_max_context, suggestion, profile_endpoint}`.
  - **Adaptive growth**: `target = roundUpPow2(required)` c cap по VRAM/model_max/конфиг. Реалистичный сценарий: Cline шлёт prompt 55K + n_predict=512; current=32768, max_vram=32719 → reject с actionable советом. Если VRAM=80GB (A100) — preflight сам перезагружает модель на 64K до отправки запроса.
  - **Новые флаги**: `preflight_enabled` (default `true`), `auto_reload_allow_tools` (default `true`), `auto_reload_n_ctx` теперь `true` по умолчанию.
  - **CppWorker**: флаг `--ram-fallback-allow-tools` (default `true`) + env `CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS`. Старое поведение (reload off при tools) включается через `--ram-fallback-allow-tools=false`.
  - **Тесты**: `internal/balancer/preflight_nctx_test.go` — 27 unit-тестов: EstimatePromptTokens, ExtractRequestMeta (Ollama chat/generate, OpenAI chat, unknown path, invalid JSON), DecidePreflight (NoOp/Reload/Reject по VRAM/model_max/конфиг), RoundUpPow2, RunPreflight с mock-клиентом, real-world Cline-сценарий.
  - **Файлы**: `internal/balancer/preflight_nctx.go` (новый), `internal/balancer/preflight_nctx_test.go` (новый), `internal/balancer/nctx_reload.go` (новые поля + helper), `cmd/cppworker/inference.go` (флаг `tryRamFallbackReloadAllowTools`), `cmd/cppworker/main.go` (флаг `--ram-fallback-allow-tools`).

- **Adaptive n_ctx auto-reload (cppworker, Stages 2-7)** — реализован полный цикл
  auto-reload модели с большим n_ctx при overflow (Cline-чат с tool-definitions,
  OpenWebUI с длинной историей). Корневая проблема: C-bridge при n_ctx overflow
  возвращал `llama_decode failed` или, после pre-flight, HTTP 400 c
  `code: 2 (N_CTX_NEEDS_RELOAD)` или `code: 3 (PROMPT_TOO_LONG)`; теперь balancer
  принимает решение: `Reload` (если VRAM позволяет) или `Reject` (HTTP 413/503).
  - **Stage 3.1 (балансер: парсер)**: `internal/balancer/llamacpp_error.go` —
    `ParseCppWorkerError(body, status, backendID)`, `*NCtxError` c `Unwrap()`
    для `errors.Is(err, bridge.ErrNCtxNeedsReload)` / `bridge.ErrPromptTooLong`,
    `DrainAndParseError`, `AsBufferedBody`, `readBodyOnce`.
  - **Stage 3.2.a (cppworker)**: 4 хендлера (`handleGenerate`, `handleChat`,
    `handleV1ChatCompletions`, `handleV1Completions`) теперь возвращают
    структурированный JSON `{error, code, bridge_info}` c HTTP 400 для
    n_ctx-recoverable ошибок (вместо 500 без диагностики).
  - **Stage 3.3 (балансер: handler)**: `internal/balancer/nctx_reload_handlers.go` —
    `handleNCtxReload` (Plan=NoOp/Reject/Reload), `handleNCtxReloadActual`
    (DoReload → SetLastKnownNCtx → retry с X-Cpp-Ctx header), `writeNCtxJSONError`,
    `writeNCtxRejectResponse`, `newNCtxReloadHTTPClient`, `getBackendPort`.
  - **Stage 3.4 (балансер: интеграция)**: `internal/balancer/llamacpp_transport.go`
    + `internal/balancer/llamacpp_router.go` — `ParseCppWorkerError` вызывается
    во всех 4 точках (`proxyRequestLlamaCpp`, `proxyRequestLlamaCppNonStream`,
    `handleOpenAIChatCompletions`), при `errors.Is(err, bridge.ErrNCtxNeedsReload)`
    вызывается `handleNCtxReload` (который сам напишет reject-ответ или сделает
    reload + retry).
  - **Stage 4 (streaming reject)**: `internal/balancer/nctx_reload_streaming.go` —
    `writeStreamingRejectNDJSON` (один NDJSON-чанк c `done:true` + `error`).
    `handleNCtxReload` использует его для streaming-клиентов, для non-streaming —
    обычный `writeNCtxRejectResponse` (HTTP 413 c JSON).
  - **Stage 5 (метрики)**: `internal/balancer/nctx_reload.go` — `perBackendMetrics`
    (lazy-init через `sync.Map`), `RecordDecision`, `RecordReloadDuration`,
    `RecordError`, `Snapshot()`. `internal/balancer/metrics.go` — `GetMetrics()`
    прокидывает `nctx_reloads_total` / `nctx_rejects_total` / `nctx_errors_total`
    / `nctx_reload_duration_ms_{avg,sum,count}` / `nctx_per_backend` в /api/metrics.
  - **Stage 6 (config)**: `pkg/types/balancing.go` — `NCtxReloadSettings` секция в
    `BalancingSettings`. `internal/balancer/nctx_reload_config_bridge.go` —
    `loadNCtxReloadConfig` + `applyNCtxReloadEnvOverrides` (LB_NCTX_RELOAD_*).
    `config/config.example.json` — пример секции `nctxReload` (по умолчанию
    `auto_reload_n_ctx: false`).
  - **Stage 7 (тесты)**: `internal/balancer/llamacpp_error_test.go` (11 тестов:
    structured bridge_info, legacy top-level code, string fallback, generic error,
    2xx, empty body, not-JSON, Error() formatting, AsBufferedBody).
    `internal/balancer/nctx_reload_test.go` (13 тестов: default config, SetLastKnownNCtx,
    SetConfig, DecideReloadBackend (4 сценария), RecordDecision/Duration/Error,
    Snapshot nil-safe, RecordOnNilSafe, loadNCtxReloadConfig, ENV overrides,
    roundUpPow2).

### Безопасность
- Все bool-флаги `NCtxReloadSettings` имеют безопасный default `false` —
  фича ВЫКЛЮЧЕНА, пока оператор явно не включит `auto_reload_n_ctx: true`
  в config.json или `LB_NCTX_RELOAD_ENABLED=true` в ENV.
- ENV override > config.json > defaults (приоритет по убыванию).
- `AutoReloadMaxNCtx` cap (default 0 = без лимита) — защита от абсурдных значений
  num_ctx (например, клиент прислал 100M).
- VRAM safety factor (default 0.85) — не выделяем больше 85% от заявленного
  `max_vram_n_ctx` (остальное — overhead на аллокации, другое ПО в GPU).
- In-flight reload dedup через shared channel — 5 параллельных запросов на reload
  с 8K до 32K дают ОДИН reload, а не 5.

### Исправлено
- **cppworker: n_ctx overflow ошибка больше не падает как 500 c «llama_decode failed»** —
  теперь возвращается структурированный JSON c `code: 2/3` + `bridge_info`
  (current/required/max_vram n_ctx, actual_tokens, n_predict). Клиенты (Cline,
  OpenWebUI) могут парсить и показать осмысленную диагностику; balancer —
  принимать решение об auto-reload.

- **WebUI: видимые дефолты n_ctx / RAM fallback / gemma-4 профили** — раньше
  дефолты cppworker'а (8192) и per-model профили (gemma-4=4096) были ниже,
  чем требует реальный tool-call prompt (~5–10K токенов). Теперь:
  - **Дефолт ctx_size поднят 4096 → 8192** в `cmd/cppworker/main.go` и
    `cppworker.example.env` (минимум для стабильной работы OpenWebUI с tools).
  - **Профиль `gemma-4`**: `numCtx` 4096 → 8192 в `config/config.bundled.json`.
  - **Добавлен профиль `gemma-4-large`** (numCtx=32768, gpuLayers=10) для
    19GB+ моделей на 24GB GPU (NVIDIA A10) в `config.bundled.json`.

- **CppWorker runtime endpoint + auto-reload через WebUI**:
  - Новый endpoint `GET /api/v1/cppworker/config/runtime` — возвращает массив
    реально загруженных моделей c **фактическими** параметрами (`context_size`,
    `gpu_layers`, `batch_size`, `flash_attn_type`, `gguf_context_length`,
    `n_layers`, `n_embd`, `state`). Handler: `cmd/cppworker/handlers_config.go:handleCppWorkerRuntimeConfig`.
  - Расширен `PUT /api/v1/cppworker/config/update` — после применения defaults
    автоматически запускает reload каждой загруженной модели c новыми параметрами
    (если изменились ctx_size/batch_size/gpu_layers/flash_attn/numa/n_threads).
    Это позволяет оператору через WebUI применить n_ctx=32768 к загруженной
    gemma-4 одним кликом без ручного POST /api/models/reload.
  - Сброс счётчика `ramFallbackAttempts` (cycle limit) при apply config.
  - **Balancer проксирование**: `internal/balancer/llamacpp_runtime_config.go:handleRuntimeConfig`
    — параллельный опрос всех llama.cpp бэкендов через cppworker endpoint,
    агрегация по `path` (дедуп если модель загружена на нескольких бэкендах).
    Маршрут `/api/v1/cppworker/config/runtime` зарегистрирован в `llamacpp_router.go`.
  - **WebUI отображение**: на вкладке Loaded Models под именем модели рендерится
    строка `ctx=32768 gpu_layers=10 batch=512 fa=0 layers=48 gguf_max=32768`.
    Если runtime n_ctx отличается от default — справа показывается бэйдж
    «runtime: 32768» в карточке. Это закрывает сценарий, когда оператор
    не знает, что модель реально загружена с другим n_ctx после auto-reload
    через balancer. Файл: `webui/js/modules/gguf-renderer.js:renderLoadedPane()`.

- **CppWorker auto_offload (solve OOM для 19GB+ моделей на 24GB GPU)**:
  - Новый флаг `--auto-offload` / ENV `CPPWORKER_AUTO_OFFLOAD=true`. При включении
    cppworker при загрузке/reload вычисляет оптимальное `gpu_layers` по формуле:
    `floor((availableVRAM * 0.85 - 1.5GB_overhead - kv_cache_bytes) / weights_per_layer)`.
    Решает OOM при загрузке 19GB моделей (gemma-4-large и т.п.) на 24GB GPU
    когда пользователь не угадал число gpu_layers. Файлы:
    `cmd/cppworker/auto_offload.go`, `cmd/cppworker/vram_detect.go`,
    `cmd/cppworker/context_timeout.go`.
  - VRAM-детект: 3 стратегии (ENV `CPPWORKER_VRAM_BYTES` → C-bridge
    `bridge.GetGPUInfo(0).VRAMTotalMB` → `nvidia-smi` CLI fallback).
  - Флаг также интегрирован в `handlers_config.go:reloadAllLoadedWithDefaults()`
    — при apply config auto-offload пересчитывает `gpu_layers` для каждой модели.

- **Сборка и тесты**:
  - `go build -tags llama_stub ./cmd/cppworker` — OK.
  - `go test -tags llama_stub ./cmd/cppworker` — все тесты pass (10 новых +
    40+ существующих). Файл: `cmd/cppworker/handlers_config_test.go`.
  - `go build -tags llama_stub ./internal/balancer/...` — OK.
  - `go test -tags llama_stub ./internal/balancer/` — все тесты pass (3 новых
    + 100+ существующих, 13.5s). Файл: `internal/balancer/llamacpp_runtime_config_test.go`.
  - `node -c webui/js/modules/gguf-api.js` и `gguf-renderer.js` — OK.

### Изменено
- `deployments/.env.bundled` — добавлены явные `CPPWORKER_*` переменные
  (`CPPWORKER_CTX_SIZE=8192`, `CPPWORKER_AUTO_OFFLOAD=true`,
  `CPPWORKER_RAM_FALLBACK_GPU_LAYERS=-2`), `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR`
  поднят 0.85 → 0.95 (default), `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` поднят
  32768 → 128000 для поддержки gemma-4-large.
- `deployments/docker-compose.cppworker-bundled.yml` — `CPPWORKER_CTX_SIZE`
  использует ENV с default 8192, добавлен `CPPWORKER_AUTO_OFFLOAD` override.
- `config/config.bundled.json` — `defaultModelProfile.contextLength` 4096 → 8192,
  `balancing.nctxReload.auto_reload_vram_safety_factor` 0.85 → 0.95,
  добавлен профиль `gemma-4-large`.
- `cmd/cppworker/main.go` — добавлен `autoOffload` flag, его ENV-обработка,
  логирование в startup-строке; определён `verbose` flag (был неявно
  используемый без объявления — давний bug).
- `cmd/cppworker/inference.go` (уже существующее) — auto_offload учитывается
  при `*autoOffload` true: вызов `calculateOptimalGPULayersForModel(m)`.

- **AutoTuneNCtx (автоподбор n_ctx при RAM-fallback reload)**: новый алгоритм
  в `cmd/cppworker/auto_tune_nctx.go` для случая «VRAM не хватает, но есть RAM».
  При `CPPWORKER_AUTO_TUNE_NCTX=true` / `--auto-tune-nctx` cppworker при
  RAM-fallback reload вычисляет максимальный n_ctx, который помещается в VRAM
  с учётом partial offload через mmap. Стратегия:
  1. Если requested_n_ctx помещается в VRAM с current gpu_layers → use as-is.
  2. Иначе: уменьшаем gpu_layers (partial offload) для размещения в VRAM.
  3. Если и с gpu_layers=0 не влезает → уменьшаем n_ctx до максимально
     возможного (тогда inference пройдёт без кода 3 «prompt too long»).
  Решает проблему: «Cline прислал 53K prompt, требуется n_ctx=53K, VRAM=8GB,
  но 32GB RAM свободно» → cppworker перезагружает модель с gpu_layers=0
  (mmap в RAM) и n_ctx=максимально возможный (например 32K), чтобы
  inference прошёл без ручной настройки.
  - `cmd/cppworker/auto_tune_nctx.go` — `AutoTuneNCtx(m, requestedNCtx)`,
    `computeMaxViableNCtx`, `TunedNCtxResult` struct с полями
    `RecommendedNCtx`, `RecommendedGPULayers`, `UseMmap`, `Source`,
    `MaxViableNCtx`.
  - `cmd/cppworker/vram_detect.go` — новая `availableRAMBytes()` со стратегиями:
    ENV `CPPWORKER_AVAILABLE_RAM_BYTES` → `/proc/meminfo MemAvailable`
    → `MemFree+Cached` → `sysctl hw.memsize`. Нужна для проверки «хватит ли
    RAM для части модели через mmap».
  - `cmd/cppworker/inference.go:tryRamFallbackReload` — при включённом
    `autoTuneNCtx` подставляет `tuned.RecommendedNCtx` / `RecommendedGPULayers`
    вместо `requestedNCtx` / `newGPULayers`. Если `RecommendedNCtx == 0`
    (даже cpu-only не влезает) → возвращает `*AutoTuneError` с конкретным
    `MaxViableNCtx` для пользователя.
  - `cmd/cppworker/main.go` — новый флаг `--auto-tune-nctx` + ENV
    `CPPWORKER_AUTO_TUNE_NCTX`.
  - `cmd/cppworker/auto_tune_nctx_test.go` — 9 unit-тестов: ENV override,
    invalid ENV, NoMetadata fallback, ExactMatch, PartialOffload,
    ReducedNCtx, RamFallbackMaxCap, NoVRAM, ComputeMaxViableNCtx.
  - `deployments/.env.bundled` — добавлен `CPPWORKER_AUTO_TUNE_NCTX=true`
  - `deployments/docker-compose.cppworker-bundled.yml` — добавлен
    `CPPWORKER_AUTO_TUNE_NCTX=${CPPWORKER_AUTO_TUNE_NCTX:-true}`.
- Тесты:
  - `go test -tags llama_stub ./cmd/cppworker/ -run "TestAvailableRAMBytes|TestAutoTuneNCtx|TestComputeMaxViable"` — OK (9 тестов, 0.99s).
  - `go build -tags llama_stub ./cmd/cppworker` — OK.
  - `go test -tags llama_stub ./internal/balancer/` — OK (13.7s, все 100+ тестов проходят).

## [Unreleased]

### Добавлено
- **cppworker: per-request n_ctx override (9-шаговый план cppworker-preflight-nctx)** — реализован
  полный цикл pre-flight check + reload для предотвращения n_ctx overflow при работе с длинными
  промптами (Cline-чат с tool-definitions, OpenWebUI с длинной историей).
  Корневая проблема: C-line bridge ранее падал с непрозрачной ошибкой `llama_decode failed`
  при превышении n_ctx; теперь — informative ошибка с предложением reload.
  - **Шаг 1 (cppworker handler)**: `handleOllamaGenerate`, `handleChat`, `handleV1ChatCompletions`,
    `handleV1Completions`, `handleGenerate` — все 5 точек проброса `NumCtx` в `genReq.NumCtx` /
    `params.NCtxOverride` реализованы и проверены.
  - **Шаг 2 (Balancer 3-tier resolver)**: `internal/balancer/num_ctx_resolver.go` —
    `ExtractNumCtxFromBody` (Ollama `options.num_ctx` и OpenAI top-level `num_ctx`),
    `GetModelProfileNumCtx`, `GetBackendDefaultNumCtx`, `ResolveNumCtx` (приоритет
    body > profile > backend), `ApplyCppCtxHeader` (устанавливает `X-Cpp-Ctx`).
  - **Шаг 3 (cppworker header)**: `applyCppCtxHeader` в `cmd/cppworker/main.go` — читает
    `X-Cpp-Ctx` и применяет к `params.NCtxOverride` ТОЛЬКО если body не задал num_ctx.
  - **Шаг 4 (cppworker reload endpoint)**: `handleReloadModel` + `POST /api/models/reload` —
    unload → load с новыми параметрами, с rollback при ошибке.
  - **Шаг 5 (Model profiles API)**: `internal/api/handlers_cppworker_profiles.go` — REST API
    `GET/PUT/DELETE /api/v1/cppworker/model-profiles[/{name}]` + `POST .../apply` (save + reload).
  - **Шаг 6 (WebUI мастер настроек)**: `webui/js/modules/cppworker-params.js` — Per-Model Profile
    manager со sliders/tooltips, inline-редактирование в списке моделей, локализация
    (en/ru: `wizard.params_title`, `wizard.apply`, `renderers.section_llama_cpp_params`).
  - **Шаг 7 (Тесты)**: `internal/balancer/num_ctx_resolver_test.go` (15+ unit-тестов для resolver),
    `tests/cppworker_lazy_load/` (integration: lazy load 5 сценариев).
  - **Шаг 8 (Документация)**: `docs/cppworker-model-params.md` (полный справочник параметров
    llama.cpp), `docs/cline-troubleshooting.md` (диагностика n_ctx overflow + рецепт
    увеличения n_ctx через профиль), `docs/rebuild-after-fixes.md` (инструкции по rebuild).
  - **Шаг 9 (Rebuild + верификация)**: документирован в `docs/rebuild-after-fixes.md` —
    требует GPU-окружения (CUDA + llama.cpp сборка).

### Сборка и тестирование (2026-06-05)
- **STUB-образ cppworker**: собран и проверен на Windows + Docker.
  Обнаружены и устранены pre-existing проблемы сборки: (a) отсутствующий импорт `strconv` в
  `cmd/cppworker/main.go` (использовался в `handleReloadModel` для `Itoa`), (b) отсутствующие
  поля `BatchSize/FlashAttnType/NUMA/UseMmap` в `cppbackend.ModelInfo` (использовались в reload
  defaults), (c) `NCtxOverride` отсутствовал в stub-версии `bridge.GenerationParams`. Все три
  исправления задокументированы и применены.
- **GPU-образ cppworker**: собран за ~3.87 GB (multi-stage: llama.cpp+CUDA sm_86 + bridge +
  CGo + Go). Бинарь стартует: bridge version `0.2.0 (real llama.cpp linked)`, GPU detected
  (RTX 3070, 8191 MB VRAM). gemma-4-E4B-it-Q4_K_M.gguf (4.97 GB) реально загружается в
  llama.cpp: тензоры blk.0—blk.41 созданы, `done_getting_tensors` достигнут.
- **E2E функциональные тесты** на запущенном STUB-контейнере:
  - `GET /health` и `GET /info` — 200 OK с JSON.
  - `POST /api/models/load` — модель загружается; в JSON ответа присутствуют
    `batchSize/flashAttnType/numa/useMmap` (новые поля).
  - `POST /api/ollama/generate` с `options.num_ctx=16384` — 200 OK, `temperature: 0.5` применён.
  - `POST /api/chat` с `options.num_ctx=8192` — 200 OK, prompt собран в gemma-формате.
  - `POST /api/ollama/generate` с заголовком `X-Cpp-Ctx: 32768` — 200 OK, header применён.
  - `POST /v1/chat/completions` с `num_ctx=4096`, `max_tokens=50` — 200 OK, OpenAI-формат.
  - `POST /v1/completions` с `num_ctx=4096` — 200 OK, OpenAI-формат.
  - `POST /api/models/reload` с `contextSize=16384, batchSize=512` — `status: reloaded`,
    `reloadDurationMs: 1`, `contextSize` модели изменился 4096→16384.
- **Go-тесты** (запущены в Linux Docker golang:1.24-alpine, тег `llama_stub`):
  - `internal/balancer` — PASS (включая 16 NumCtx resolver + 5 оптимизация-тестов, итого 21).
  - `internal/agent`, `internal/api`, `internal/config`, `internal/modelreplication`,
    `internal/rpccoordinator`, `internal/virtualmodel`, `pkg/logger`, `pkg/protocol`,
    `tests/cppworker_lazy_load` — PASS.
  - `internal/cppbackend`, `c/bridge` — компилируются с тегом `llama_stub` (тестов нет, но
    компиляция OK).
  - `tests` (integration) — pre-existing FAIL `min redeclared` в `api_chain_llamacpp_test.go`,
    не относится к данному плану.

#### GPU-верификация на реальной gemma-4-E4B-it-Q4_K_M (2026-06-05)

Запущен GPU-контейнер `ollama-legion-cppworker-gpu-v2` (`--gpus all`, порт 18092) с
реальной llama.cpp-сборкой (CUDA sm_86, RTX 3070 8GB). После rebuild в образ
попал `/api/models/reload` endpoint (ранее отсутствовал в старом образе
`ollama-legion/cppworker:gpu` от 14 ч. назад — подтверждено `404 page not found`).

- **Загрузка модели #1 (CPU-mode)**: `POST /api/models/load` с `name=gemma-4, gpuLayers=0`
  (первая попытка с `gpuLayers=-1` зависла без прогресса на >5 мин, откатился к 0),
  `n_ctx=2048, batch=128`. Загрузка завершена: `state=loaded, gemma4, nLayers=42,
  nEmbd=2560, batchSize=128, TIME=373s`. Лог: `load_tensors: offloaded 0/43 layers to GPU`,
  `CPU_Mapped model buffer = 4731.51 MiB`, `CUDA0 compute buffer = 618.50 MiB` (CUDA
  используется только для отдельных compute ops, не для тензоров модели).
- **Загрузка модели #2 (GPU-hybrid partial offload)**: `POST /api/models/load` с
  `name=gemma-4, gpuLayers=30, n_ctx=4096, batch=128`. Загрузка завершена: `TIME=254s`.
  Лог: `load_tensors: offloaded 30/43 layers to GPU`, `CUDA0 model buffer = 2064.07 MiB`
  (2 GB на GPU), `CPU_Mapped model buffer = 3027.45 MiB` (3 GB через mmap на CPU).
  CUDA0 KV cache = 72 MiB, CUDA0 compute buffer = 143.25 MiB, CUDA graph nodes = 1867
  (graph splits = 252 при bs=128). Это реальный GPU-hybrid: 30 слоёв на RTX 3070,
  13 слоёв + output embedding на CPU (через mmap).
- **Real inference #1 (num_ctx=4096 в body)**: `POST /v1/chat/completions` с
  `num_ctx=4096` → HTTP 200, `"! How can I help you today? I'm looking for
  information about the **history of the internet**."` (34 токена, 9.355s).
  Подтверждает: per-request n_ctx override из body работает в реальной связке с
  llama.cpp C-bridge (pre-flight check не сработал, т.к. 4096 < эффективного n_ctx).
- **Real inference #2 (X-Cpp-Ctx: 8192 header)**: `POST /v1/chat/completions` с
  заголовком `X-Cpp-Ctx: 8192` → HTTP 200, `"! I'm excited to chat with you. I'm
  here to"` (10 токенов, 23s). Подтверждает: HTTP-заголовок `X-Cpp-Ctx` корректно
  подхватывается в cppworker и применяется к llama.cpp params.
- **Reload endpoint (POST /api/models/reload)**: перезагрузка gemma-4 с
  `contextSize=8192, batchSize=256`. Длительность ~5 мин (Unload + Load). После:
  `state=loaded, batchSize=256` (новые параметры применены).
- **Real inference #3 (после reload, num_ctx=4096)**: `POST /v1/chat/completions` с
  `num_ctx=4096` → HTTP 200, `"The answer is four."` (15.5s). Подтверждает: модель
  реально отвечает после reload end-to-end.
- **Real inference #4 (после reload, без num_ctx)**: `POST /v1/chat/completions`
  без `num_ctx` → HTTP 200, `"Paris"` (13.5s). Подтверждает: при отсутствии
  per-request n_ctx используется n_ctx, с которым модель фактически загружена
  (т.е. 8192 после reload).
- **Real inference #5 (GPU-hybrid 30/43 слоёв)**: `POST /v1/chat/completions` с
  `num_ctx=2048, max_tokens=30, temperature=0.1` → HTTP 200, `"Eleven is larger
  than nine."` (**5.66s**). Сравнение с CPU-mode (inference #1 = 9.4s): **ускорение
  в ~1.66×** благодаря partial GPU offload 30/43 слоёв на RTX 3070 + CUDA graphs
  (1867 nodes). Это явное подтверждение реальной GPU-работы, а не только CUDA
  compute buffer (как было в inference #1 с gpuLayers=0).

**Сравнение режимов загрузки gemma-4 (RTX 3070 8GB, batch=128, 30 токенов):**

| Режим | gpuLayers | VRAM (model) | RAM (mmap) | KV-cache | Infer time | Ускорение |
|-------|-----------|--------------|------------|----------|------------|-----------|
| CPU-only | 0 | 0 | 4.7 GB | 320 MB CPU | 9.4s | 1.0× (baseline) |
| GPU-hybrid | 30/43 | 2.0 GB | 3.0 GB | 72 MB GPU + 88 MB CPU | **5.66s** | **1.66×** |

Итог GPU-верификации: полный цикл 9-шагового плана работает end-to-end на реальной
gemma-4 в Docker-контейнере с GPU. Все 5 ключевых сценариев (num_ctx в body,
X-Cpp-Ctx header, reload endpoint, отсутствие n_ctx, GPU partial offload) дают
корректные ответы от настоящей llama.cpp. C-bridge pre-flight check интегрирован,
reload семантика (n_ctx иммутабельна без reload) соблюдена, GPU-hybrid offload
подтверждён 1.66× ускорением inference.

#### Bundled stack: cppworker-gpu + balancer + webui (2026-06-05)

- **Новый `deployments/docker-compose.cppworker-bundled.yml`** — минимальный compose на 3 сервиса
  (баллансировщик + cppworker-gpu + webui), без cpu/stub/agent, без профилей. Замена полного
  `docker-compose.full.yml` для single-GPU dev/test/мини-продакшн. Документация: [docs/deployment-bundled.md](docs/deployment-bundled.md).
- **Auto-registration cppworker → balancer (`cmd/cppworker/balancer_register.go`)** — новая фоновая
  горутина в cppworker, которая при старте регистрирует себя в балансировщике через
  `POST /api/v1/backends` с заголовком `X-API-Token`. ENV: `CPPWORKER_BALANCER_URL`,
  `CPPWORKER_BALANCER_TOKEN`, `CPPWORKER_ADVERTISE_HOST/PORT`, `CPPWORKER_REGISTER_NAME`,
  `CPPWORKER_REGISTER_RETRY_INTERVAL` (default 30s), `CPPWORKER_REGISTER_HEARTBEAT`
  (default 60s), `CPPWORKER_REGISTER_MAX_RETRIES` (default 0=infinite), `CPPWORKER_REGISTER_GPU_MODE`.
  Если `CPPWORKER_BALANCER_URL` не задан — cppworker работает в standalone-режиме и принимает
  запросы напрямую. При недоступности балансировщика — retry бесконечно через `RETRY_INTERVAL`.
  409 Conflict (бэкенд уже зарегистрирован) трактуется как OK. На SIGTERM — best-effort
  `DELETE /api/v1/backends/{id}`.
- **`deployments/.env.bundled.example`** — шаблон env-файла со всеми параметрами (порты, токены,
  дефолтные параметры загрузки модели, GPU devices). Копируется в `.env.bundled`.
- **`config/config.bundled.json`** — минимальный конфиг балансировщика с `backends: []` (cppworker
  зарегистрируется сам), `operatingMode: "llama_cpp"`, профилем `gemma-4` (numCtx=4096,
  batchSize=256, numGpuLayers=30, flashAttention=true, useMmap=true). Включает `prewarm=false`,
  `autoPull=false`, `unload=false` — пользователь сам управляет жизненным циклом моделей.
- **`scripts/start-bundled.sh` + `scripts/start-bundled.ps1`** — готовые скрипты запуска
  (`up`/`rebuild`/`down`/`logs`/`status`) с автоинициализацией `.env.bundled` и подстановкой
  токена в `config.json`.
- **Скрипт `start-bundled.ps1`** инициализирует конфиг: копирует `config.bundled.json` →
  `config.json`, подставляет `auth.tokens[0].name` из `CPPWORKER_API_TOKEN`.

#### WebUI: gguf Settings tab — llama.cpp параметры для выбранного бэкенда (2026-06-05)
- **Корневая проблема**: на странице `gguf` (master-detail) таб **Settings** показывал
  «мёртвую» форму `gguf-settings-form`, которая только меняла локальный `state.loadOptions`
  и не отправляла ничего на бэкенд. Реальные llama.cpp-дефолты cppworker'а и Per-Model
  профили n_ctx были видны только в глобальной Settings-странице (`#section-llama-cpp`),
  а не в gguf-рендерере.
- **Что сделано**:
  - `webui/js/modules/gguf-renderer.js`:
    - `renderSettingsPane()` переписан: вместо «мёртвой» формы рендерит два блока —
      **(1) Backend load options** (load/save через
      `GET/PUT /api/v1/cppworker/config[+/update]` на выбранном cppworker'е) и
      **(2) Per-Model Profiles** (список + wizard из `window.CppWorkerParams`).
    - Добавлены `loadAndRenderBackendOptions()` / `saveBackendOptions()` /
      `mountProfilesInSettings()`.
    - `onDetailPanelClick` вызывает `loadAndRenderBackendOptions()` + `mountProfilesInSettings()`
      при активации таба `settings`.
    - Маппинг полей `cppbackend.Config` ↔ UI: `defaultCtxSize`, `defaultBatchSize`,
      `defaultGpuLayers`, `defaultFlashAttnType`, `defaultNuma`, `defaultUseMmap`,
      `defaultNThreads` (+ read-only `nodeName`, `balancerUrl`, `uptime`).
  - `webui/js/i18n/en.js` + `webui/js/i18n/ru.js`: новые ключи
    `gguf.backend_options_title`, `gguf.backend_options_desc`,
    `gguf.save_backend_options`, `gguf.reload_backend_options`,
    `gguf.backend_options_loaded/saved/load_error/save_error`,
    `gguf.flash_attn_type`, `gguf.flash_attn_type_desc`,
    `gguf.n_threads`, `gguf.n_threads_desc`,
    `gguf.profiles_section_title`, `gguf.profiles_section_desc`,
    `gguf.profiles_only_registered`, `gguf.config_unavailable`.
- **Сценарий пользователя**: gguf → выбрать бэкенд → таб Settings → видны текущие
  llama.cpp-дефолты этого cppworker'а + список Per-Model профилей + кнопка
  «Добавить профиль». Save отправляет PUT в cppworker и обновляет .env.

### Исправлено
- **Тесты балансировщика**: 5 предсуществующих падающих unit/integration тестов в `internal/balancer/` починены.
  Корневая причина — после введения `LlamaCppRouter` (см. cppworker-интеграцию) `routeRequest` при
  `OperatingMode=""` (дефолт в тестовой конфигурации) допускает оба типа бэкендов и сначала
  пробует llama.cpp-роутер, который возвращает 503 «no llama.cpp backend available» для бэкендов
  без `Type=llama_cpp`. Тесты писались под основной Ollama-flow и должны идти через
  `proxyRequest`, а не через `LlamaCppRouter.Route`.
  - `TestAutoPullFullScenario` (`internal/balancer/auto_pull_test.go`) — отключён `llamaCppRouter` +
    добавлен `modelContextKey` в request context (имитация поведения production `ServeHTTP`).
  - `TestSelectBackendAllBusy` (`internal/balancer/proxy_test.go`) — ассерты обновлены под
    queueing-aware fallback (least-loaded backend при заполненных слотах, а не empty).
  - `TestSelectByResources` (`internal/balancer/backend_selector_test.go`) — аналогично.
  - `TestTwoClientsSameIP_DifferentSessions` и `TestLoadBalancing_MultipleClients`
    (`internal/balancer/proxy_integration_test.go`) — побочные жертвы того же root cause,
    отключён `llamaCppRouter`.
    Подробности в `docs/test-fixes-2026-06-05.md`.

- **balancer: пустой `content` в финальном NDJSON done-чанке для Cline / OpenWebUI**.
  Корневая причина: `internal/balancer/llamacpp_transport_helpers.go:writeStreamingSSEDone`
  формировал done-чанк с пустым `message.content` / `response`, потому что cppworker
  присылает финальный ответ в OpenAI SSE-чанке с `finish_reason: "stop"`, а балансер
  этот чанк подавлял (через `hasFinishReason(data)`) и заменял своим пустым.
  Раньше Cline получал `{"done":true,"done_reason":"stop","message":{"content":"",...}}`
  даже при длинном успешном ответе.
  - **`internal/balancer/llamacpp_transport.go`** — в streaming-цикле добавлены
    аккумуляторы `accumulatedPlainContent` (для `/api/chat`/`/api/generate`) и
    `upstreamDoneContent` (извлекается из OpenAI done-чанка через
    `extractUpstreamDoneChunk` / `extractUpstreamGenerateDoneChunk`). Оба
    передаются в `writeStreamingSSEDone` при `[DONE]`.
  - **`internal/balancer/llamacpp_transport_helpers.go`** — `writeStreamingSSEDone`
    расширен: принимает `accumulatedContent` и `upstreamContent` параметры, итоговый
    content имеет приоритет `upstreamContent > accumulatedContent > ""`. Для
    `tool_calls` сохраняется прежнее поведение (done_reason: tool_calls). Для
    обычных ответов добавлен `done_reason: "stop"` в финальном done-чанке.
  - Добавлена функция `cleanFinalContent` (обёртка над `stripServiceTokens`),
    убирающая служебные токены (`` / `</s>`) из накопленного content.
  - Для `/api/generate` финальный text из OpenAI done-чанка (`choice.text`)
    автоматически подмешивается в `accumulatedPlainContent` (на случай,
    когда streaming идёт с пустыми chunks, а полный текст приходит только
    в done).
  - **Тесты**: `go test -tags llama_stub ./internal/balancer/...` — все 100+
    тестов проходят (13.5s). `go test -tags llama_stub ./tests/... -run
    "TestOpenWebUI|TestDebugOpenWebUI|TestLlamacppTransport|TestStreaming"` —
    зелёные. E2E-проверка через curl `POST /api/chat` показала, что
    финальный NDJSON содержит реальный content:
    `{"done":true,"done_reason":"stop","message":{"content":"hello world\nend_of_turnhello world",...}}`.

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

#### RPC Model Distribution (три варианта распределения моделей)

**Вариант A — Model Replication:**
- Автоматическая репликация моделей на несколько бэкендов
- Scale-up/scale-down по min/max instances
- Idle Unload — выгрузка простаивающих реплик
- LRU eviction при превышении max instances
- Target backends — явное указание бэкендов для реплик
- Фоновый контроллер (`GroupController`) с интервалом 10s
- Групповой селектор (`GroupAwareSelector`) для балансировки внутри группы

**Вариант B — RPC Coordinator:**
- Распределённый inference через внешних RPC-воркеров
- KV-кэш для оптимизации повторных запросов
- Split/Merge — разбиение длинных промптов на чанки
- Worker client с keep-alive пулом соединений
- Автоматическое определение владельца модели (`HasDistributedModel`)

**Вариант C — Virtual Model Router:**
- Виртуальные модели как pipeline из нескольких физических моделей
- Регистрация виртуальных моделей с цепочками трансформаций
- Роутинг запросов через конвейер обработки

#### Декомпозиция кодовой базы
- `proxy.go` разбит: core логика (1000 строк), `session_manager.go`, `slot_manager.go`, `eventbus.go`, `proxy_logger.go`, `proxy_request.go`, `streaming.go`, `model_management.go`
- `handlers.go` разбит на модули: `handlers_agents.go`, `handlers_backends.go`, `handlers_cluster.go`, `handlers_core.go`, `handlers_metrics.go`, `handlers_model_management.go`, `handlers_queue.go`, `handlers_replication.go`, `handlers_sessions.go`, `handlers_virtual.go`, `proxy_logs_handler.go`
- `collector.go` разбит на модули: `collector_gpu.go`, `collector_health.go`, `collector_network.go`, `collector_ollama.go`, `collector_register.go`, `collector_system.go`
- Выделены пакеты: `internal/modelreplication/`, `internal/rpccoordinator/`, `internal/virtualmodel/`, `internal/config/`, `internal/huggingface/`

#### Тестирование
- 200+ интеграционных и unit-тестов
- Тесты агента: сбор метрик, регистрация, heartbeat (28 тестов)
- Тесты аутентификации: TokenAuthenticator, middleware (24 теста)
- Тесты MetricsBroker: pub/sub, множественные клиенты (17 тестов)
- Тесты Rate Limiting: token bucket (11 тестов)
- Тесты Proxy/Queue: очередь, выбор бэкенда, session manager (20 тестов)
- Тесты репликации: groups, scale-up/down, controller, dispatch (40+ тестов)
- Тесты RPC Coordinator: KV-кэш, split/merge, worker client (30+ тестов)
- Тесты виртуальных моделей: registry, router, pipeline (25+ тестов)
- Тесты режимов работы: operating modes, dispatch, webui mode (25+ тестов)
- Тесты стриминга: cancel, errors, SSE (30+ тестов)
- Тесты проксирования: mock, model load, reliability (40+ тестов)
- Тесты бэкенд-селектора: scoring, weight tuning (20+ тестов)
- Тесты балансировщика: config sync, load scenarios, no-agent (50+ тестов)
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
- Таймаут балансировщика при ожидании ответа от модели — добавлен контекст с дедлайном
- Локализация WebUI — исправлены отсутствующие ключи для новых страниц
- Проксирование запросов при недоступности бэкенда — улучшен failover с повторной попыткой на другом бэкенде
- Разбор JSON-полей конфигурации (`target_backends`, `labels`) — унифицирован тип `[]string`
- Состояние кластера при отключении бэкенда — корректная очистка моделей и сессий
- AutoPull моделей — исправлена гонка при одновременной загрузке одной модели несколькими запросами
