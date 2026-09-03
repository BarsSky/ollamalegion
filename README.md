# OllamaLegion — Adaptive LLM Inference Cluster

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE.md)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg)](https://www.docker.com/)
[![CI test](https://github.com/BarsSky/ollamalegion/actions/workflows/test.yml/badge.svg?branch=centurion)](https://github.com/BarsSky/ollamalegion/actions/workflows/test.yml)

> 🇷🇺 **Русский** (текущий) | [🇬🇧 English documentation](docs/en/README.md)

Интеллектуальный балансировщик нагрузки для llama.cpp inference с адаптивной загрузкой моделей, авто-подбором параметров под доступные ресурсы и мониторингом GPU/CPU/RAM.

> **🚦 Быстрый старт** (новое железо или миграция): см. [docs/ru/hardware-presets.md](docs/ru/hardware-presets.md) / [docs/en/hardware-presets.md](docs/en/hardware-presets.md) — 4 готовых пресета для RTX 30xx/40xx/50xx и A10. Скрипт `python scripts/apply-hardware-preset.py <name>` за 30 секунд правит `.env.bundled-with-agent` под вашу GPU.

## AutoTune (R54–R55.2, 2026-08-24)

Автономный оптимизатор параметров загруженных моделей. Детектит
sub-optimal состояния, вычисляет optimal params, auto-применяет через
async reload. Ручное управление сохранено (per-model `autoTune: false`).

**Что умеет:**
- R54.1: Detect sub-optimal n_ctx, KV cache (q4_0→f16), num_gpu_layers.
- R54.2 + R54.7: Global + per-model toggle (Settings page).
- R54.4: Autonomous reload + circuit breaker (60s cool-down, 300s stable).
- R54.6: Manual apply endpoint (`POST /api/v1/admin/autotune/{id}/apply`).
- R54.8: WebSocket live updates + toast notifications.
- R54.9: Workload-aware KV cache (p95 num_ctx → f16/q4_0).
- R55.2: Event history log (ring buffer 500 entries) + WebUI timeline.

**Endpoints:**
- `GET /api/v1/admin/autotune` — global state per backend.
- `POST /api/v1/admin/autotune/{backendID}/apply` — manual apply.
- `GET /api/v1/admin/autotune/history` — recent events.
- `GET/PUT /api/v1/admin/autotune/config` — global + per-model toggles.

См. [docs/api.md](docs/api.md#autotune-api-r541--r552-2026-08-24) для деталей.

## Что нового

**v0.5.22 — 2026-08-13** (последний релиз, [полный CHANGELOG](CHANGELOG.md)):

Round 35 (коммиты `6140a59` + `df7962e` + `f22dc13`) — crash-loop "model
loaded then immediately reset" полностью закрыт. CppWorker bundled-with-agent
r35 image deployed, no SIGSEGV в логах 26+ минут uptime.

- 🐛 **Cline 65K → reload→load fallback** — раньше cppworker
  `/api/models/reload` возвращал 404 "model not currently loaded"
  когда cppworker только что стартовал (после SIGSEGV auto-restart) и
  ещё не успел загрузить модель, а балансер уже верил в stale
  `loaded: true` от llamaCppMetricsPoller. Cline получал 503+Retry-After
  loop до 120s → ECONNREFUSED. Fix: async reload path переключён с
  `/api/models/reload` (требует loaded state) на `/api/models/load`
  (идемпотентный, handles all 3 cases: not-loaded → load,
  different params → unload+load, same params → 200 dedup).
  Sync reload path получил 404 → load fallback после existing
  202→poll handling. Reuses same payload minus "force" field.

- 🐛 **C++ chat template cgo SIGSEGV** — `common_chat_templates_apply`
  (c/bridge/csrc/chat_thinking.cpp) периодически SIGSEGV'ит в cgo
  execution → exit code 2 → crash-loop. Stack trace: `runtime.cgocall`
  → `bridge_apply_chat_template` в `bridge.go:834`. SIGSEGV-в-cgo
  catchable через `defer recover()` в том же goroutine (Go runtime
  конвертирует через `runtime.sigpanic`). Fix: `func() { defer
  recover() }()` wrapper в `cmd/cppworker/handlers_chat.go:281-310`
  → при cgo SIGSEGV fallback на простой C API `llama_chat_apply_template`
  (стабильный, без C++ common::chat). Для gemma-4 нативный путь
  всё равно ничего не даёт (template не поддерживает enable_thinking),
  так что C++ путь создавал crash opportunities без пользы.

- 🐛 **IdleUnloadManager SIGSEGV** — `checkAndUnload` падал в
  `bridge_free_model` если `LastUsedAt.IsZero()` (модель только что
  загружена). `now.Sub(time.Time{})` = 631 трлн наносекунд
  (2025 лет) → idleTime > idleTimeout → unload сразу →
  use-after-free в C-bridge. Fix: `IsZero()` guard ПЕРЕД `Sub()` в
  `internal/cppbackend/model_manager.go:538-549` (skip unload + debug
  log). Также `config/cppworker-defaults.json:41` `idleUnloadMinutes:
  120 → 0` (off по дефолту; client `keep_alive` достаточно для bundled
  use case).

- 🐛 **4-phase preflight (Round 34 follow-up, commit `6140a59`)** —
  реализация была в working tree с прошлой сессии, восстановлена
  отдельным коммитом. Phase 0: compose env vars (`LB_NCTX_*`) для
  async reload mode. Phase 1: stream dialog с SSE/NDJSON keepalives
  во время async reload (Cline не таймаутит). Phase 2: profile
  mismatch detection (kv_cache_type, flash_attn, use_mmap, не только
  n_ctx). Phase 3: SetLastKnownNCtx on cppworker load/unload
  callbacks. Phase 4: optimal auto-tune params (gpuLayers=-2,
  flashAttn=-1, useMmap=true) в reload payload. Plus 5 cascading
  bug fixes: orphan `mu.Unlock()` (Balancer crash через 75 min),
  `LB_NCTX_PREFLIGHT_ENABLED` env override, preflight для OpenAI
  path (Cline bypass'ил Ollama preflight), `NumCtx` field в
  `openAIChatRequestRaw`, `RequestedNCtxOverride` check в
  `DecidePreflight`.

- 🏗️ **Cppworker image `gpu-86-abort-r35`** (3.86GB, ~18 мин CUDA
  build, deployed 2026-08-12 22:54 UTC). Cppworker bundled r35
  image. Деплой через `docker compose up -d --no-build --no-deps`
  (compose пытается rebuild из source если `build:` секция есть в
  compose, нужно `--no-build` для использования freshly-tagged
  image). Env override в BOTH `deployments/.env` AND
  `deployments/.env.bundled-with-agent` (compose auto-loads `.env`
  без суффикса).

**v0.5.19 — 2026-08-10** ([полный CHANGELOG](CHANGELOG.md)):

- 🐛 **Gemma-4 `<|channel>thought` reasoning tag leak** — при включённой настройке «применять размышления» через балансировщик в ответе приходил смешанный поток `thought\n[text]\n<channel|>` без отделённого reasoning. Gemma-4 GGUF tokenizer содержит special tokens `<|channel>`, `<channel|>`, `<|think|>`, `<think|>` (найдены через `python -c "data.find(b'<|channel|>')"`); gemma-4 chat template рендерит reasoning как `<|channel>thought\n[text]\n<channel|>` **только когда `message.get('tool_calls')`**. cppworker `thinkTagPairs` содержал только 4 стандартных пары (`<think>`/`<thinking>`/`<reasoning>`/`<analysis>`) — `SplitReasoningContent` не split'ил channel format. Fix: добавлены 5 новых tag pairs в **обоих** местах — `cmd/cppworker/reasoning_content.go:thinkTagPairs` + `internal/balancer/llamacpp_translate_resp.go:stripReasoningTags` (defensive double-coverage). balancer получил `stripWithBoundary()` helper (prefix-safe orphan strip, иначе `<think` сожрал бы `<think|>`) + `sharedClose[]` boolean array (root cause первой попытки фикса: `closeTags` для thought и analysis одинаковые `\n<channel|>`, `ReplaceAll` для iter 1 сжирал close, нужный для iter 2 → analysis пара не split'алась). 33 новых test case'а (7 cppworker + 8 standalone pkg + 12 balancer strip + 6 cppworker split standalone). Live verify: gemma-4 с plain-text prompt НЕ эмитит channel format (модель использует plain text reasoning для обычного chat) — это ожидаемо, фикс активируется через Cline с tools или Qwen3.x с native thinking mode.
- ⚡ **Cancel latency 8× faster + prefill heartbeat** — Round 31 #6 (v0.5.16) детектил cancel за 8 секунд на Windows из-за FIN-only close. Fix: **C-bridge n_batch 512 → 64** (8x чаще abort check в prefill loop, ~100-250ms вместо 1-2s; prefill total time не меняется, CUDA amortizes kernel launch overhead; gen phase не зависит от n_batch, использует batch=1). 3 точки: `c/bridge/bridge.c:1381, 1644` + `c/bridge/bridge.go:108-117` + `c/bridge/bridge_stub.go:82-83`. Plus **prefill heartbeat** (immediate client feedback пока C-bridge обрабатывает 5-30s prompt phase): SSE comment `: prefill_started_at=...` сразу после `WriteHeader(200)` в `cmd/cppworker/handlers_openai.go:770-785` (RFC §4.4 keep-alive, клиенты игнорируют но connection alive) + NDJSON `{"done":false}` в `handlers_chat.go:562-577` + `handlers_generate.go:412-419` (Ollama-формат, OpenWebUI tolerates строки без "message" поля). Live verify: heartbeat emitted BEFORE first data token (5K-token prompt, gemma-4), cancel latency **0ms** (с 8s на v0.5.16), TTFB 2-4s на gemma-4 reasoning (prefill-bound, не cancel bug).
- ✨ **Webui cancel button + cross-tab BroadcastChannel sync** — раньше из webui нельзя было отменить активную генерацию (приходилось закрывать Cline/чат-клиент чтобы balancer увидел TCP close), состояние между вкладками отличалось на 5s (polling interval). Fix: **inline `<button class="gguf-cancel-gen-btn">` в busy badge** на loaded model card + `cancelActiveGeneration()` handler (visual feedback: disable + "⏳" + toast + re-enable через 1s). API: `webui/js/modules/api.js:cppworkerCancelGeneration.post(backendId, modelName, userId?)` — POST `/api/cancel` через существующий `GgufApi.requestViaBackend` proxy (идентично `cancelDownloadViaBackend`, `deleteDownloadedFileViaBackend`). Body: `{model, user_id?}` matches cppworker `handleCancel`. **Cross-tab sync**: `BroadcastChannel('ollama-legion-sync')` с lazy init + `typeof BroadcastChannel === 'undefined'` fallback; `backendsEqual` dedup чтобы broadcast шёлся только при реальных изменениях (не каждый 5s poll); `handleCrossTabMessage` триггерит `fetchClusterState()` + `refreshActiveQueriesCrossTab()`; cancel broadcasts `generationCancelled` → другие вкладки обновляются немедленно. `state._activeQueriesTickFn` pattern: store closure ref в state чтобы external modules вызывали `tick()` напрямую для immediate refresh вместо 3s polling. 6 новых i18n ключей (`gguf.busy_badge`, `busy_badge_title`, `active_short`, `cancel_generation`, `generation_cancelled`, `cancelled_short`) на ru.js + en.js. 37 unit-тестов (5 cancel API + 6 cancel button + 9 BroadcastChannel + 12 i18n + 2 dedup + 1 broadcast count) all pass. Live verify: `cancelled=1, by=all` за 11ms через webui proxy path, end-to-end 22 tokens generated → cancel → generation aborts → `/api/infer/active` count=0. 4 контейнера healthy: balancer v32r1 (`0794a4344ab8`) + cppworker:gpu-86-abort-v2 (`b52a4568096a`) + webui:cppworker-bundled (`feda98d7118a`) + agent. **3 коммита** на github/centurion: `3352d59` (channel format), `299bb66` (cancel latency + heartbeat), `15c84bc` (webui cancel + cross-tab). Round 32 #1+#2 = полный Bug 2 fix end-to-end.

**v0.5.16 — 2026-08-09**:

- ✨ **Round 31 #6: C-bridge Abort API — полноценный cancel для streaming inference**. Раньше (v0.5.15) cancel через callback работал только между токенами в gen phase. Теперь cppworker прерывает in-flight `bridge_infer_stream` **на ближайшей check point** (между `llama_decode` batches) — включая **prompt phase** (раньше: длинный prompt 20K токенов блокировал cancel на 1-3 секунды), **multi-slot `bridge_batched_decode`** (раньше: вообще не было cancel-хука), и **gen phase**. C-bridge получил `atomic_int abort_requested` в `InternalModel` — set из Go cgo thread (bridge_request_abort), read из C thread, relaxed memory ordering. Go-side `bridge.RequestAbort(model)` можно вызывать из любой горутины. `cmd/cppworker/abort_watcher.go` (NEW, 81 строка) — goroutine, блокирующаяся на `<-ctx.Done()`, вызывает `bridge.RequestAbort`. Интегрирован в 5 точках: `handleChat`, `writeChatStreamResponseWithTools`, `handleGenerate`, `handleOllamaGenerate`, `writeOpenAIChatStream`, `writeOpenAICompletionStream`. Wire protocol improvement: cancelled response теперь содержит `cancelled: true` (custom field) + `done_reason: "cancelled"` (Ollama) или `finish_reason: "stop"` (OpenAI backward-compat) вместо generic `error`. 6 abort check points в C-bridge: `bridge_infer` prompt + gen, `bridge_infer_stream` prompt + gen, `bridge_batched_decode`, callback cancel path. Backend: `GetHandle(name)` + `GetAllHandles()` для abort_watcher и shutdown. Internal: `BRIDGE_ERR_ABORTED = -100` (negative для однозначного отличия от positive int token count), `ErrCodeAborted = -100`, `ErrAborted` sentinel для `errors.Is`. Backend refactor: существующий callback cancel path теперь возвращает `BRIDGE_ERR_ABORTED` вместо 0 (status code distinction для telemetry). 21 unit-тест (14 Go + 7 C standalone, gcc -Wall -Wextra clean, race-free). Live E2E: `ollama-legion/cppworker:gpu-86-abort-v2` (RTX 3070 sm_86) + Qwen3-Instruct-2507-q4km (Q4_K_M, 36 layers) — реальный C-bridge, реальная модель, VRAM 7201 MiB, 3 AbortWatcher fires зафиксированы. End-to-end chain: TCP close → ctx.Done → abort_watcher → bridge.RequestAbort → C atomic flag → BRIDGE_ERR_ABORTED. Cancel latency: 1-2s для first CUDA batch (warmup), ~85ms per token для subsequent gen batches. 10 sequential cancels + 5 concurrent — все корректно отменяются, model переиспользуется после abort. 4 Docker images готовы: `stub-abort` (171MB), `cpu-abort` (190MB), `gpu-86-abort` (3.86GB), `gpu-86-abort-v2` (3.86GB + wire protocol). Hard cancel (pthread_kill) НЕ реализован — явно отложен как опасен, Round 31 #1+#6 покрывают 99% use cases. Sanitizers (ASan, TSan) — недоступны в MinGW, требуют Linux. Plan: `plans/cppworker-abort-api/PLAN.md` (33KB, 6 phases, 10-17h estimated). Closed Round 31 #6.

**v0.5.15 — 2026-08-05**:

- 🐛 **Order-of-checks: gemma-4 / disabled-модели теперь возвращают чистую ошибку** — в v0.5.14 follow-up мы добавили `disabled: true` в профиль, но проверка была слишком глубоко в коде. В mixed-mode main proxy (`/api/chat` → `ServeHTTP → routeRequest → selectBackend → proxyRequest`) `ensureModelLoadedOnBackend` не вызывается вообще, поэтому gemma-4 с `profile.disabled=true` всё равно летела в cppworker → SIGABRT в `ggml-backend.cpp:1367` (`GGML_ASSERT n_inputs < GGML_SCHED_MAX_SPLIT_INPUTS`). А в single-mode `isModelReadyOnBackend` возвращал `true` для уже загруженной gemma-4 (cppworker запущен с `n_ctx=32768`) → функция возвращала success ДО disabled-check. Плюс circuit breaker открывался на disabled-моделях после 3-х refused-ответов. Fix: disabled-check переехал в самый верх `ServeHTTP` (сразу после `parseRequestBody`), теперь срабатывает ВСЕГДА — для любого backend type, любого routing path, любого состояния модели. Live verify: 5× `gemma-4` → 5× HTTP 503 с чистым `model "gemma-4-E4B-it-Q4_K_M" is marked as disabled in profile`, 0 SIGABRT, 0 breaker-ошибок. В `ensureModelLoadedOnBackend` остался второй disabled-check (перед `isModelReadyOnBackend` и breaker'ом) для direct-вызовов из warmup scheduler. 3 новых unit-test'а (all PASS).
- 🐛 **Bug #4: OpenWebUI cancel** — добавлен `r.Context().Done()` check в streaming loop (Round 6 Fix 5). Round 31 #6 (v0.5.16) полностью заменил этот workaround на C-bridge Abort API — см. v0.5.16.

**v0.5.14 — 2026-08-05**:

- 🐛 **cppworker `/api/chat` keepalive must be NDJSON, not SSE comment** — Round 6 Fix 5 слал `: keepalive\n\n` каждые 100ms для TCP idle-prevention, но `/api/chat` это **NDJSON** (Ollama native), а не SSE. Строгие NDJSON-клиенты (Cline CLI, ollama-python) бросали `invalid json: : keepalive` на КАЖДОЙ строке (сотни ошибок за inference). Fix: `{"keepalive":true}\n` (валидный NDJSON) + интервал 100ms → 15s (через `getHeartbeatInterval(15s)`). OpenAI-compat `/v1/chat/completions` (SSE) — без изменений, SSE-комменты корректны для EventSource-клиентов. Live verify: `0 SSE comment lines, 0 invalid JSON` для обоих путей.
- 🐛 **bundled-full: дублирующийся backend при рестарте** — `entrypoint.sh` запускал `register-with-balancer.sh` несмотря на `CPPWORKER_REGISTER_DISABLE=true` (disable действовал только на Go-side). Дубль `cppworker-gpu-bundled` (`weight=10`, hard-coded в shell-скрипте) перебивал реальный `cppworker-gpu-bundled-agent` (`weight=1`) при routing. Fix: добавлена проверка `CPPWORKER_REGISTER_DISABLE` в `entrypoint.sh` для shell-скрипта. Существующий дубль удаляется через `DELETE /api/v1/backends/{id}`.
- ✅ **Cline CLI v2.16.0 + Qwen3 (E2E)** — keepalive-фикс обязателен. Настройка: `ollama` provider (НЕ `openai-compatible` — тот проксирует через cline.ai и требует платный Cline balance), `baseUrl=http://your-host:18092`. Cline system prompt 16K токенов → нужен `n_ctx=32768`.
- ✅ **Qwen3.6-35B-A3B-UD-Q4_K_M (20.6GB) inference** — MOE (35B total / 3B active). На 8GB VRAM + 24GB RAM с `numGpuLayers=20` (фактически 7/40 на GPU, 33/40 на CPU): load 12.7 мин, inference ~0.91 tok/s (медленно из-за CPU offload), простые запросы работают (`3+5=8` за 12.7s). Полная GPU-загрузка требует ≥24GB VRAM.

**v0.5.13 — 2026-08-04**:

- ✨ **WebUI busy badge + async apply profile (Round 26)** — при генерации ответа моделью cppworker'овский `handleReloadModel` блокировал на `inflight.WaitZero` 30-60+ сек. WebUI не мог ни показать, ни изменить настройки. Теперь: cppworker endpoint `/api/models/active-queries` показывает busy count, **api server детектит busy ДО apply и возвращает HTTP 202 + Location + SSE progress** (events каждые 500ms), WebUI показывает `🔴 Generating (N active)` на loaded model card + per-backend progress modal с auto-fallback на polling.
- ✨ **n_ctx overflow detection + X-Model-Context-Warning header (Round 26)** — preflight check перед каждой генерацией (`ComputeContextWarning`). Уровни: `ok` / `approaching` (>80% n_ctx) / `overflow` (prompt+n_predict>n_ctx, clamp до 0) / `impossible` (prompt>n_ctx). Auto-clamp n_predict (без reload-loop). HTTP headers в `/api/chat`, `/api/generate`, `/v1/chat/completions` — клиент (OpenWebUI/Cline/Hermes) видит предупреждение ДО обрыва.
- 🐛 **Long-conversation cutoff fix** — юзер подтвердил cutoff во ВСЕХ клиентах (OpenWebUI, Cline, Curl, Roo Code, IDE plugins, Hermes) → server-side. Config defaults: `streamingIdleTimeout: 600→1800`, `streamTimeout: 0→1800` (10 мин → 30 мин). Per-model profile override (через WebUI wizard) остаётся приоритетным.

**v0.5.12 — 2026-08-04**:

- ✨ **SSE load progress** — WebUI переключился с polling каждые 1.5s на `EventSource`. Cppworker шлёт `text/event-stream` push-events каждые 500ms с `{state, elapsedMs, loadingSizeBytes}`. Heartbeat `:keepalive` каждые 15s. Auto-close на terminal state. **Live verify**: 30 events за 15s direct, 22 events за 11s через balancer API proxy (без буферизации).
- ✨ **Measured load time cache** — динамическая оценка `estimatedLoadTimeMs` на основе реальных измерений (weighted average последних 20 load'ов, stale-фильтр 7d, zero-filter). После первого load'а Qwen3 (2.5GB, 99s) estimate становится ~70s вместо хардкода 28s. Более реалистичный feedback для клиента.
- 🔧 **WebUI auto-cleanup** — `pagehide` + `visibilitychange` listeners останавливают все polling/SSE при уходе со страницы. Без этого фоновые EventSource'ы удерживали cppworker'а после закрытия вкладки.
- 🔧 **Balancer SSE proxy** — `internal/api/gguf_backend_proxy.go` детектит `Content-Type: text/event-stream` и стримит напрямую через `streamCopy` (без буферизации). `X-Accel-Buffering: no` для nginx.

**v0.5.11 — 2026-08-04**: Dynamic / async model loading + Bug fixes #1, #2.

- ✨ **Dynamic / async model loading** — gemma-4 (5GB) и другие большие модели больше НЕ ломаются по client timeout. `POST /api/models/load` теперь возвращает **HTTP 202 Accepted + Location за <100ms** с динамической оценкой `estimatedLoadTimeMs` (compute из `size / 100MB/s + ctx + 2s overhead`). Реальный load идёт в background goroutine, polling через `/api/models/load/progress`. `?wait=true` для legacy sync. **Live verify**: Qwen3-Instruct-2507-q4km (2.5GB) async load = 101ms response + 99s background, end-to-end chat = 44s (load + gen), 2.1s на already-loaded.
- 🐛 **Bug #1: WebUI settings UI hang** — `handleReloadModel` теперь async (default). Apply с новыми n_ctx больше не зависает UI на 30-60+ сек при reload'е reasoning-модели.
- 🐛 **Bug #2: WebUI "active/Unload" badge на unloaded моделях** — `isLoaded` check сломан при несовпадении имён (`m.name` с `.gguf` vs `lm.name` без). Fix: `stripGGUF` helper + normalized match + path basename. Устойчиво к load через WebUI/API/balancer warmup.
- 🔧 **Balancer integration** — `executeLlamaCppLoad` детектит 202 + `status=loading` и поллит до `state=loaded` (max = estimated × 1.5 + 10s). `ensureModelLoadedOnBackend` deadline 5s → 5min (метрики кэшируются с 1-2s задержкой; 5s давал false positive).

**v0.5.10 — 2026-08-04**: Round 23 — bug fixes (5 из 9) + persistent ccache infrastructure.

- 🐛 **Bug #6+#8: Smart-skip reload** — клиент (Cline/OpenWebUI) шлёт `num_ctx=32000` "на всякий случай", но реальный prompt "2+2?" = 1 токен. Balancer теперь проверяет `estimated_prompt + n_predict` — если помещается в loaded_n_ctx, **patch'ит body** и proxy'ит БЕЗ reload. Live verify: 1.3s/req (было 30-50s + retry-loop).
- 🐛 **Bug #4: OpenWebUI cancel** — добавлен `r.Context().Done()` check в streaming loop. Cancel теперь останавливает генерацию <1s (было: модель работает до natural completion).
- 🐛 **Bug #7: Think block leak** — убран L3 "give up" на 1024 chars. Auto-detect теперь ВСЕГДА проверяет весь outputBuf на любой из 4 think-тегов.
- 🐛 **Bug #5: WebUI -2 GPU layers** — `GPU_LAYERS_MIN -1 → -2`, display "AUTO" для auto-рассчёта.
- 🏗️ **Persistent ccache** — `DOCKER_BUILDKIT=1` + `docker buildx create --driver docker-container` + `.dockerignore build-*/`. ccache теперь действительно persistent: 47 мин cold, **<2 мин warm** для Go-изменений (было 20+ мин каждый build).

**v0.5.9 — 2026-08-04**: Documentation audit + 3 pre-existing test-bug fixes (percentiles off-by-one, racy concurrent test, sanitize expectations) + security fix (real GitHub PAT removed).

- 🐛 **CRITICAL bug fix: `temperature=0` от клиента теперь honor'ится** — раньше Go-слой игнорировал `temperature=0` (Cline/Aider/Continue все шлют greedy) и подставлял default `0.7`, что приводило к не-детерминированным tool calls. Pointer types в request structs (`*float64` / `*int`) различают "не задано" от "explicit 0". 8 unit-тестов.
- 🟡 **P1 — 4 code-review fix'а**: rename misleading function, BatchedScheduler head-of-line blocking (TokenCh buffer 8→128 + non-blocking send + drop counter), sampleFromLogits silent fallback → logging + counter, UnloadModel infinite wait → 10s timeout.
- 🧹 **Dead code cleanup** — `im->sampler` removed из C-bridge (после sampler hotfix стал no-op).
- 🧪 11 новых unit-тестов (build params, sample stats). Production verified: 5/5 unique до фикса, 1/5 после на `temperature=0` (greedy).

**v0.5.1 — 2026-07-30**: Batched Parallel Inference (Round 15.1) + Round 15.2 (multi-token prefill, temperature sampling, vocab-aware EOG) + **Multi-tool Recovery** (Qwen3-4B-Instruct quirk с trailing `}]}`).

**v0.5.0 — 2026-07-30**: Batched Parallel Inference functional baseline + 6 unit-тестов.

**v0.4.12 — 2026-07-29**: Native `enable_thinking` для Qwen3-thinking + Round 13 inference fix.

**v0.4.6 — v0.4.11** (2026-07-28): WebUI settings, api token auth, Cline/Roo совместимость через `X-API-Token`, Phase 8 RPC Coordinator (production), n_parallel > 1 (state isolation), preflight n_ctx reload.

**v0.4.5** (2026-06-25): Cluster-level model management + cascade fallback + graceful reload.

**v0.2.0-adaptive** (2026-06-22): адаптивная загрузка, KV-cache fallback, авто-reload n_ctx, bundled deployment.

**v0.1.0** (2026-05-14): initial release.

Все 12+ релизов с v0.2.0+ документированы в [CHANGELOG.md](CHANGELOG.md). Каждый коммит
помечен тегом — `git log v0.5.2..HEAD` показывает unpushed changes.

## Быстрый старт (Bundled)

Требуется: Docker 24+ с поддержкой Compose v2, Git, NVIDIA драйвер + NVIDIA Container Toolkit (для GPU-режима).

```bash
# 1. Клонировать репозиторий (имя каталога — произвольное)
git clone https://github.com/BarsSky/ollamalegion.git
cd ollamalegion

# 2. Подготовить .env (один раз)
cp deployments/.env.bundled-with-agent.example deployments/.env.bundled-with-agent
# отредактируйте deployments/.env.bundled-with-agent под свою машину
# (минимум — замените CPPWORKER_API_TOKEN на свой)

# 3. Запустить стек
```

**Windows (PowerShell):**

```powershell
$env:DOCKER_BUILDKIT=1
$env:CUDA_ARCH=86        # sm_86 для RTX 30xx, sm_89 для RTX 40xx, sm_90 для RTX 50xx
docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml `
               --env-file  deployments/.env.bundled-with-agent `
               up -d --build
```

**Linux / macOS / WSL2:**

```bash
export DOCKER_BUILDKIT=1
export CUDA_ARCH=86
docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml \
               --env-file  deployments/.env.bundled-with-agent \
               up -d --build
```

После запуска:
- Балансер: http://localhost:18080 (Ollama API) / :18081 (Management API)
- CppWorker: http://localhost:18092 (llama.cpp inference)
- WebUI: http://localhost:18083

> Если нужна более простая сборка **без sidecar-агента метрик**, используйте
> `deployments/docker-compose.cppworker-bundled.yml` + готовые скрипты:
> `.\scripts\start-bundled.ps1` (Windows) или `./scripts/start-bundled.sh` (Linux/macOS).

## Архитектура

```
Клиент (Cline/OpenWebUI)
  → Balancer (:18080) — routing, session affinity, n_ctx auto-reload
    ├── Preflight (Round 35) — 4-phase n_ctx + profile mismatch detection
    │   ├── Phase 0: env vars (LB_NCTX_*)
    │   ├── Phase 1: stream dialog with SSE/NDJSON keepalives
    │   ├── Phase 2: kv_cache_type / flash_attn / use_mmap mismatch
    │   ├── Phase 3: SetLastKnownNCtx on cppworker load/unload
    │   └── Phase 4: optimal auto-tune params in reload payload
    ├── Reload→Load fallback — if /api/models/reload returns 404
    │   (model not currently loaded), use /api/models/load (idempotent)
    └── CppWorker (:18092) — llama.cpp inference
      ├── Adaptive Loader — SelectStrategy: f16→q8_0→q4_0, MoE, partial offload
      ├── KV-cache fallback — работает без GGUF metadata
      ├── Auto-reload API — /api/models/reload + /api/models/load
      └── Chat template — native C++ path wrapped in recover() (Round 35)
    → Agent (:18032) — NVML GPU/CPU/RAM метрики, health check
  → WebUI (:18083) — дашборд, мониторинг, sparkline
```

## Порты

| Порт | Компонент | Назначение |
|------|-----------|------------|
| 18080 | Balancer | Ollama API proxy |
| 18081 | Balancer | Management API + WebSocket |
| 18092 | CppWorker | llama.cpp inference |
| 18032 | Agent | Метрики GPU/CPU/RAM |
| 18083 | WebUI | Дашборд |

## Сборка

Все команды выполняются из **корня репозитория** (каталог, в который вы склонировали проект).

**Windows (PowerShell):**

```powershell
$env:DOCKER_BUILDKIT=1
$env:CUDA_ARCH=86     # подставьте своё значение: 86/89/90/120

# cppworker GPU (cuda 12.x, sm_86)
docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml `
               --env-file  deployments/.env.bundled-with-agent `
               build cppworker-gpu

# Только balancer
docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml `
               --env-file  deployments/.env.bundled-with-agent `
               build loadbalancer

# Всё вместе
docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml `
               --env-file  deployments/.env.bundled-with-agent `
               build
```

**Linux / macOS / WSL2:**

```bash
export DOCKER_BUILDKIT=1
export CUDA_ARCH=86

docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml \
               --env-file  deployments/.env.bundled-with-agent \
               build cppworker-gpu

docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml \
               --env-file  deployments/.env.bundled-with-agent \
               build loadbalancer

docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml \
               --env-file  deployments/.env.bundled-with-agent \
               build
```

BuildKit=1 обязателен — с ним Go-only изменения собираются за ~30 сек (cached CUDA layers).

## Конфигурация

### .env (ключевые переменные)

Дефолты — из `deployments/.env.bundled-with-agent.example` и `config/cppworker-defaults.json` (defaultCtxSize=32768).

```bash
CPPWORKER_CTX_SIZE=32768       # контекст по умолчанию
CPPWORKER_GPU_LAYERS=20        # слоёв на GPU (-1=все, -2=авто)
CPPWORKER_AUTO_OFFLOAD=true    # авто-расчёт gpuLayers
CPPWORKER_AUTO_TUNE_NCTX=true  # авто-подбор n_ctx
CPPWORKER_AUTO_KV_CACHE=true   # авто-выбор kvCacheType (q4_0/q8_0/f16)
CPPWORKER_RAM_FALLBACK_N_CTX=true  # RAM fallback при нехватке VRAM
```

Дополнительные (в .env-файле, но редко меняются):
`CPPWORKER_BATCH_SIZE`, `CPPWORKER_FLASH_ATTN_TYPE`, `CPPWORKER_N_THREADS`,
`CPPWORKER_NUMA`, `CPPWORKER_USE_MMAP`, `CPPWORKER_SPLIT_MODE`,
`CPPWORKER_RAM_FALLBACK_GPU_LAYERS`, `CPPWORKER_RAM_FALLBACK_MAX_N_CTX`,
`CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS`, `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD`,
`CPPWORKER_DEFAULT_N_PREDICT_REASONING` (reasoning-модели, default 8192),
`OLLAMALEGION_HEARTBEAT_MS`.

### Балансер (config.json)

Полный пример — `config/config.example.json`. Минимальный релевантный фрагмент
(с реальными дефолтами из `config/config.example.json`):

```json
{
  "balancing": {
    "firstByteTimeout": 30,
    "nctxReload": {
      "auto_reload_n_ctx": true,
      "auto_reload_max_n_ctx": 131072,
      "auto_reload_vram_safety_factor": 0.85,
      "auto_reload_timeout_sec": 120
    }
  }
}
```

> ENV-overrides для `nctxReload` (приоритет над config.json):
> `LB_NCTX_RELOAD_ENABLED`, `LB_NCTX_RELOAD_MAX_N_CTX`,
> `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR`, `LB_NCTX_RELOAD_TIMEOUT_SEC`.
> Читаются в `internal/balancer/nctx_reload_config_bridge.go`.

## Структура проекта

```
cmd/
  balancer/          — HTTP proxy + Management API (port 18080/18081)
  cppworker/         — llama.cpp inference server (port 18092)
    adaptive_loader.go     — SelectStrategy: авто-подбор параметров
    adaptive_integration.go — adaptive API routes, NaN-healer
    handlers_model.go      — load/reload с адаптивной стратегией
    lazyload.go            — ленивая загрузка моделей
    reasoning_content.go   — reasoning extraction (qwen3.5, deepseek-r1, gemma-4)
    balancer_register.go   — auto-register в балансер
    nctx_clamp.go          — preflight clamp по VRAM
  agent/             — GPU/CPU метрики (NVML, port 18032)
  monitor/           — встроенный монитор-страница (HTML UI)
  rpcworker/         — RPC worker для distributed inference (Phase 8 P.1)

internal/
  api/               — HTTP handlers, routes, auth, rate-limit
    routes.go              — все REST endpoints балансера
    gguf_backend_proxy.go  — proxy /api/v1/gguf/backends/{id}/proxy/*
  balancer/
    nctx_reload.go          — auto-reload n_ctx
    nctx_reload_adaptive.go — запрос стратегии у cppworker
    proxy.go, ollama_router.go, llamacpp_router.go — routing
    scoring.go              — resource-aware scoring
    session_manager.go      — session affinity, stickiness
    rpc_coordinator_dispatcher.go — Phase 8 P.1 RPC coord
  cppbackend/
    backend.go        — Go-обёртка над C-bridge, params, KVCacheType
  agent/             — общие типы для agent
  config/            — config loading, validation
  modelreplication/  — per-model replication groups
  rpccoordinator/    — RPC coordinator state + workers
  rpcworker/         — RPC worker runtime
  rptensor/          — tensor parallelism (P.3, research)
  virtualmodel/      — Phase 8 P.2 virtual models (alias-on-pool)

c/                   — C-bridge к llama.cpp (build-msvc-cuda / build-cpu-mingw)
  llama.cpp/         — submodule: исходники llama.cpp
  bridge/            — Go ↔ C bridge
  ggml/              — submodule: GGML tensor library

deployments/
  docker-compose.cppworker-bundled-with-agent.yml  — основной стек
  docker-compose.cppworker-bundled.yml             — без sidecar-agent
  docker-compose.cppworker.yml                     — одиночный cppworker
  docker-compose.agent.yml                         — только agent
  .env.bundled-with-agent.example                  — переменные для основного стека
  .env.bundled.example                             — для упрощённого стека

config/
  config.example.json         — шаблон основного config балансера
  config.bundled.json         — bundled-конфиг для docker-compose
  cppworker-defaults.json     — дефолты для CppWorker (defaultCtxSize=32768)
  cppworker.example.env       — env-файл для локального cppworker
  agent.example.env           — env-файл для локального agent

scripts/             — утилиты: start-bundled.{ps1,sh}, build-containers, e2e-тесты
docs/                — полная документация (RU + EN)
plans/               — roadmap, ADR, фазовые отчёты
```

## Документация

- 🇷🇺 [docs/README.md](docs/README.md) — индекс документации (RU)
- 🇬🇧 [docs/en/README.md](docs/en/README.md) — English documentation index
- 🇷🇺 [docs/installation.md](docs/installation.md) — установка и сборка
- 🇬🇧 [docs/en/installation.md](docs/en/installation.md) — installation & build
- 🇷🇺 [docs/deployment.md](docs/deployment.md) — развёртывание
- 🇬🇧 [docs/en/deployment.md](docs/en/deployment.md) — deployment
- 🇷🇺 [docs/api.md](docs/api.md) — REST API
- 🇬🇧 [docs/en/api.md](docs/en/api.md) — REST API (English)
- [plans/README.md](plans/README.md) — roadmap

## Лицензия

MIT


**v0.5.10 — 2026-08-04**: Round 23 — bug fixes (5 из 9) + persistent ccache infrastructure.
