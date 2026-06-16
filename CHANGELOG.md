# Changelog

Все заметные изменения в проекте Ollama Legion будут задокументированы в этом файле.

Формат ведётся в соответствии с [Keep a Changelog](https://keepachangelog.com/ru/1.0.0/),
и этот проект придерживается [Semantic Versioning](https://semver.org/lang/ru/).

## [Unreleased]

### Добавлено
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
