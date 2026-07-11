# CppWorker: n_ctx, Per-Model Profiles, Ollama ↔ OpenAI Proxy

> **Версия:** 2.0 (2026-06-22)  
> **Связанные документы:** [`api.md`](api.md), [`runbook-tools.md`](runbook-tools.md), [`audit-2026-06.md`](audit-2026-06.md), [`../.clinerules`](../.clinerules) §3–4  
> **Тесты:** `cmd/cppworker/*_test.go`, `tests/nctx_reload_integration_test.go`, `tests/llamacpp_initial_setup_test.go`

## Содержание

1. [Per-Model Profiles и n_ctx](#1-per-model-profiles-и-n_ctx)
2. [3-tier resolver n_ctx](#2-3-tier-resolver-n_ctx)
3. [Per-model profile (структура)](#3-per-model-profile-структура)
4. [Header `X-Cpp-Ctx`](#4-header-x-cpp-ctx)
5. [RAM fallback и reload](#5-ram-fallback-и-reload)
6. [Tools/tool_calls и RAM fallback](#6-toolstool_calls-и-ram-fallback)
7. [API endpoints (Per-Model Profiles + n_ctx-reload)](#7-api-endpoints-per-model-profiles--n_ctx-reload)
8. [WebUI мастер настройки](#8-webui-мастер-настройки)
9. [Трансляция Ollama ↔ OpenAI](#9-трансляция-ollama--openai)
10. [Типичные сценарии](#10-типичные-сценарии)

---

## 1. Per-Model Profiles и n_ctx

### 1.1 Проблема

`n_ctx` (context size) в llama.cpp — **immutable после загрузки модели**. Если модель загружена с `n_ctx=4096`, увеличить до 16384 без перезагрузки **нельзя** — придёт `n_ctx overflow` от Cline/OpenWebUI при попытке использовать длинный system prompt.

Раньше администратору приходилось:
1. Менять `LLAMA_CTX_SIZE` в `.env` cppworker.
2. Перезапускать cppworker-контейнер (даунтайм ~10-30 секунд).
3. Надеяться, что 8K хватит всем моделям.

Это плохо, потому что:
- **gemma-4-E4B-it-Q4_K_M** просит 32K-128K (большие system prompts, long context).
- **llama-3.1-8b** комфортно работает на 8K.
- **qwen2.5-coder-7b** нужно 16K для редактирования файлов.
- **embedding-модели** не нуждаются в длинном контексте.

### 1.2 Решение

**Per-model profile** + **3-tier resolver** + **reload endpoint** дают:
- Задать `n_ctx` для каждой модели независимо.
- Применить новый профиль **на лету** (reload без перезапуска контейнера).
- Клиент может override-нуть через `options.num_ctx` в body (Ollama) или `num_ctx` (OpenAI).

---

## 2. 3-tier resolver n_ctx

Файл: `internal/balancer/num_ctx_resolver.go`. При обработке каждого запроса balancer вычисляет эффективный `n_ctx`:

```
┌────────────────────────────────────────────────────────────────────┐
│ Tier 1: request body (наивысший приоритет)                         │
│   Ollama:   {"options": {"num_ctx": 4096}}                         │
│   OpenAI:   {"num_ctx": 4096}                                      │
│   → клиент явно попросил → это всегда побеждает                    │
├────────────────────────────────────────────────────────────────────┤
│ Tier 2: per-model profile (config.LlamaCppModelProfiles[name])    │
│   → если админ настроил профиль для gemma-4 → 32768                │
├────────────────────────────────────────────────────────────────────┤
│ Tier 3: per-backend default (state.Backend.CppWorkerConfig.ContextLength) │
│   → fallback из дефолта cppworker (LLAMA_CTX_SIZE из .env)         │
└────────────────────────────────────────────────────────────────────┘
```

**Важно:**
- Tier 1 всегда побеждает.
- Tier 2 побеждает Tier 3 — профиль модели приоритетнее общего дефолта бэкенда.
- Если ни один tier не дал значение (0), cppworker использует свой `defaultCtxSize`.
- В `config/config.json` **`defaultModelProfile.contextLength` должен быть `0` или отсутствовать** (иначе Tier 2 фиксирует значение и Tier 3 не достигается).

После резолва balancer выставляет header `X-Cpp-Ctx` в запросе к cppworker (если `Value > 0`).

---

## 3. Per-model profile (структура)

Профиль живёт в `config.json` под ключом `llamaCppModelProfiles`:

```json
{
  "llamaCppModelProfiles": {
    "gemma-4-E4B-it-Q4_K_M": {
      "contextLength": 32768,
      "batchSize": 1024,
      "numGpuLayers": -1,
      "flashAttn": true,
      "numa": false,
      "useMmap": true,
      "notes": "Cline long context + system prompt"
    },
    "llama-3.1-8b-instruct-q4_K_M": {
      "contextLength": 8192,
      "batchSize": 512,
      "numGpuLayers": -1
    }
  }
}
```

### 3.1 Поля профиля

| Поле | Тип | Обязательно | Default | Описание |
|---|---|---|---|---|
| `contextLength` | int | **да** | — | Размер контекста. Диапазон `[256, 262144]` (256K). |
| `batchSize` | int | нет | 512 | Размер батча. `0` = не задано. |
| `numGpuLayers` | int | нет | -1 | Слои на GPU. `-1` = все, `0` = CPU-only, `N` = конкретное число. |
| `flashAttn` | bool (ptr) | нет | true | Flash Attention. `null` = не переопределять. |
| `numa` | bool (ptr) | нет | false | NUMA-оптимизация. |
| `useMmap` | bool (ptr) | нет | true | Memory mapping. |
| `notes` | string | нет | "" | Свободный комментарий для админа. |

> **Почему bool-поля — указатели?** PATCH-семантика: если в body `flashAttn: false`, это перезапишет `true` в профиле. Если поле отсутствует — старое значение сохранится.

### 3.2 Валидация

- `contextLength` ∈ `[256, 262144]`. 256K — потолок для gemma-4 (native 262144).
- `contextLength = 0` в профиле **недопустимо**.
- `batchSize < 1` — ошибка (если задан).
- `numGpuLayers < -1` — ошибка.

---

## 4. Header `X-Cpp-Ctx`

Когда balancer резолвит `n_ctx > 0` из профиля или backend default, он добавляет в запрос к cppworker заголовок:

```
X-Cpp-Ctx: 32768
```

cppworker в `cmd/cppworker/main.go:applyCppCtxHeader` читает этот header **и** `body.options.num_ctx` (Ollama) / `body.num_ctx` (OpenAI), применяет к `params.NCtxOverride`.

**Приоритет в cppworker (внутри одного запроса):**

```
1. body.options.num_ctx (Ollama) / body.num_ctx (OpenAI) — самый высокий
2. X-Cpp-Ctx header от balancer — fallback
3. defaultCtxSize (из LLAMA_CTX_SIZE) — последний fallback
```

> **Body > Header.** Это правило общее для обоих уровней (balancer→cppworker, и внутри cppworker). Body — это явный per-request override, header — это политика.

**Семантика header: upper limit.**
- Если `body.num_ctx <= headerLimit` → header не понижает.
- Если `body.num_ctx > headerLimit` → clamp к headerLimit.

Дополнительно:
- Если в body задан `num_predict` и его default превышает header/2 — выполняется `clampNPredictToFitContext` (см. `internal/balancer/nctx_clamp.go`).

---

## 5. RAM fallback и reload

### 5.1 RAM fallback

`cmd/cppworker/inference.go:tryRamFallbackReload` (строка ~491):

Когда моель запрашивает `n_ctx`, превышающий VRAM, cppworker пытается:
1. Проверить флаг `ramFallbackNCtx` (env `CPPWORKER_RAM_FALLBACK_N_CTX=true` или `--ram-fallback-n-ctx`).
2. Сверить с `ramFallbackMaxNCtx` (env `CPPWORKER_RAM_FALLBACK_MAX_N_CTX`, default без лимита).
3. `UnloadModel()` текущей модели.
4. `LoadModelWithOpts(n_ctx=requestedNCtx, use_mmap=true, gpu_layers=ramFallbackGpuLayers)`.
5. Если успех — retry inference.

**Счётчик `ramFallbackAttempts`:** ограничен `ramFallbackMaxAttempts=3` за окно 60 сек. Превышение → `ReloadLoopLimitError` → HTTP 413.

### 5.2 Auto-reload на балансировщике

`internal/balancer/nctx_reload.go:NCtxReloadCoordinator`:

Когда cppworker возвращает `bridge code 2` (`ErrNCtxNeedsReload`), балансировщик:
1. Парсит `NCtxError` через `internal/balancer/llamacpp_error.go:ParseCppWorkerError`.
2. Вызывает `DecideReloadBackend(model, requestedNCtx, maxVramCtx)`.
3. Решение на основе `NCtxReloadConfig` (`AutoReloadNCtx`, `VramSafetyFactor`, `MaxNCtx`).
4. Если решение = `DecisionAccept` → `POST /api/models/reload` на cppworker.

### 5.3 Endpoint `POST /api/models/reload` (cppworker)

Применяет новые параметры к загруженной модели:

```json
POST /api/models/reload
Content-Type: application/json

{
  "name": "gemma-4-E4B-it-Q4_K_M",
  "contextSize": 32768,
  "batchSize": 1024,
  "numGpuLayers": -1,
  "flashAttn": true
}
```

Что делает:
1. `unload` текущей модели (если загружена).
2. `load` с новыми параметрами (через C-bridge `bridge_load_model`).
3. Pre-flight check (`bridge_check_ctx_capacity`) перед загрузкой.
4. Возвращает `200 OK` после успешной загрузки.

**Время reload:** 5-30 секунд в зависимости от размера модели и диска.

### 5.4 Защита от двойной загрузки

`cmd/cppworker/handlers_model.go:handleLoadModel`:
- Перед `LoadModelWithOpts` проверяет `backend.GetModel(modelName)`.
- Если модель **уже загружена** с теми же путём/параметрами → возвращает `status: "already_loaded"`.
- Если параметры или путь отличаются → `UnloadModel` + `LoadModelWithOpts` (reload-in-place).
- Защита от race-condition через mutex + `WaitForLoad`.

---

## 6. Tools/tool_calls и RAM fallback

`cmd/cppworker/inference.go:tryRamFallbackReload` **отключает reload, если в запросе есть `tools[]`** (`hasTools=true`):

```go
if hasTools {
    return ReloadDisabledForToolsError
}
```

`cmd/cppworker/utils.go:writeReloadDisabledForToolsResponse` транслирует это в **HTTP 413** с подсказкой:
- Уменьшить `tools[]`/history.
- Или увеличить `n_ctx`.

**Почему:** `tools[]` сериализуется в system prompt → после reload контекст был бы занят tools, и inference не поместился бы.

---

## 7. API endpoints

### 7.1 Per-Model Profiles (`internal/api/handlers_cppworker_profiles.go`)

Все endpoints защищены `AuthMiddleware` (если `API_TOKEN` задан) и `RateLimitMiddleware`.

#### `GET /api/v1/cppworker/model-profiles`

Список всех профилей.

**Ответ 200:**
```json
{
  "models": {
    "gemma-4-E4B-it-Q4_K_M": {
      "contextLength": 32768,
      "batchSize": 1024,
      "numGpuLayers": -1,
      "flashAttn": true,
      "numa": false,
      "useMmap": true,
      "notes": "Cline long context"
    }
  },
  "total": 1
}
```

#### `GET /api/v1/cppworker/model-profiles/{name}`

Получить профиль для одной модели. `404` если профиля нет.

#### `PUT /api/v1/cppworker/model-profiles/{name}`

Создать или обновить профиль. **Persist в `config.json`.**

**Body:**
```json
{
  "contextLength": 32768,
  "batchSize": 1024,
  "numGpuLayers": -1,
  "flashAttn": true,
  "notes": "Cline long context"
}
```

**Ответ 200:** `{ "status": "ok", "model": "...", "profile": {...} }`

**Ошибки:** `400 invalid JSON`, `400 invalid profile`, `400 model name required`.

#### `DELETE /api/v1/cppworker/model-profiles/{name}`

Удалить профиль. **Persist в `config.json`.**

**Ответ 200:** `{ "status": "ok", "model": "..." }`

#### `POST /api/v1/cppworker/model-profiles/{name}/apply`

**Главный endpoint для админа:** применить профиль с reload на всех бэкендах.

**Body (опционально):** patch — поля из body мерджатся с текущим профилем (zero-value поля не перезаписывают).

**Алгоритм:**
1. Прочитать body, смержить с текущим профилем.
2. Валидировать merged профиль.
3. Сохранить в config.
4. Для каждого llama_cpp бэкенда:
   - Если модель **не загружена** → `skipped: "model not currently loaded on this backend"`.
   - Если загружена → `POST /api/models/reload` на cppworker.
5. Вернуть агрегированный результат.

**Ответ 200:**
```json
{
  "model": "gemma-4-E4B-it-Q4_K_M",
  "profile": { "contextLength": 32768, "batchSize": 1024, "numGpuLayers": -1 },
  "backends": [
    { "backendId": "llama-gpu-1", "status": "reloaded" },
    { "backendId": "llama-cpu-1", "status": "skipped", "message": "model not currently loaded on this backend" }
  ]
}
```

### 7.2 Reload-counter reset endpoint

#### `POST /api/v1/cppworker/reset-reload-counter`

Сбрасывает счётчик `ramFallbackAttempts` на cppworker. Поддерживает сброс для одного бэкенда
(через `{"backendId": "..."}`) или для всех сразу (через `{"all": true}` или пустое тело).

**Использование:** после ручного исправления причины reload-loop (например, увеличение VRAM или смена модели).

```bash
# Сброс для конкретного бэкенда
curl -X POST http://localhost:18081/api/v1/cppworker/reset-reload-counter \
  -H "Content-Type: application/json" \
  -d '{"backendId": "cppworker-gpu-1"}'

# Сброс для всех бэкендов
curl -X POST http://localhost:18081/api/v1/cppworker/reset-reload-counter \
  -H "Content-Type: application/json" \
  -d '{"all": true}'
```

**Примечание:** endpoint `/api/v1/nctx-reload/*` deprecated, остался только этот unified endpoint.

**Ответ 200:**
```json
{
  "backends": {
    "cppworker-gpu-1": {
      "attempts": 2,
      "decisions": { "accept": 1, "reject": 1, "noop": 0 },
      "lastError": "n_ctx overflow",
      "lastDecisionAt": "2026-06-22T12:00:00Z"
    }
  }
}
```

### 7.3 `POST /api/v1/cppworker/reset-reload-counter` (R-6 ✅)

> **Статус:** ✅ реализовано в `cmd/cppworker/handlers_reset_reload.go`.

Когда счётчик `ramFallbackAttempts` на стороне cppworker достигает лимита, администратор может сбросить его без `docker restart`.

**Без body** — сбрасывает счётчик для всех моделей.

```bash
curl -X POST http://localhost:18091/api/v1/cppworker/reset-reload-counter
```

**С `{"model": "..."}`** — сбрасывает только для указанной модели.

```bash
curl -X POST http://localhost:18091/api/v1/cppworker/reset-reload-counter \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M"}'
```

**Ответ 200:**
```json
{
  "status": "ok",
  "model": "",
  "resetCount": 3,
  "message": "ramFallbackAttempts cleared. Cppworker can now attempt reload again."
}
```

**Защита:** `authMiddleware` (если задан `API_TOKEN`).

**Типичные ошибки:**
- `405 Method Not Allowed` — используется не-POST.
- `400 Bad Request` — невалидный JSON в body.

**Тесты:** `cmd/cppworker/handlers_reset_reload_test.go` (5 тестов).

---

## 8. WebUI мастер настройки

В **Settings → CppWorker → Model Profiles** доступен визуальный мастер:

1. **Список профилей** — все настроенные модели.
2. **Add Profile** — wizard:
   - Поле "Model name" (обязательно).
   - Слайдер n_ctx от **256** до **262144** (256K) с пресетами: 4K, 8K, 16K, 32K, 64K, 128K, 256K, Custom.
   - Поля `batchSize`, `numGpuLayers`, `notes`.
3. **Apply** — вызывает `POST /.../apply`, показывает **progress bar**:
   - Step 1: Save profile (HTTP PUT).
   - Step 2: Reload on backends (HTTP POST /apply).
   - Step 3: Done.
4. **Edit / Delete** — иконки рядом с профилем.

Также в Settings → CppWorker отображается **Backend load options** для выбранного cppworker'а:
- `defaultCtxSize`, `defaultBatchSize`, `defaultGpuLayers`, `defaultFlashAttnType`, `defaultNuma`, `defaultUseMmap`, `defaultNThreads`.
- Read-only: `nodeName`, `balancerUrl`, `uptime`.

> **i18n:** все строки переведены в `webui/js/i18n/ru.js` и `en.js` (ключ `settings.profiles.*`).

---

## 9. Трансляция Ollama ↔ OpenAI

Файл: `internal/balancer/llamacpp_translate_*.go`.

Балансер транслирует Ollama-формат запросов в OpenAI-формат для cppworker и обратно.

### 9.1 Архитектура

```
Клиент (OpenWebUI) → Балансер (ollamalegion) → CppWorker (llama.cpp)
     Ollama API           Трансляция форматов        OpenAI API
  /api/chat           →  /v1/chat/completions
  /api/generate       →  /v1/completions
  /api/embeddings     →  /v1/embeddings
  /api/tags           →  /v1/models (или метрики)
```

### 9.2 Маппинг полей

| Ollama | OpenAI |
|---|---|
| `options.temperature` | `temperature` |
| `options.top_p` | `top_p` |
| `options.top_k` | `top_k` |
| `options.num_predict` | `max_tokens` |
| `options.num_ctx` | `num_ctx` |
| `options.stop` (string или []string) | `stop` |
| `options.repeat_penalty` | `repeat_penalty` |
| `options.seed` (0 валидно) | `seed` |
| `prompt` | `prompt` (для `/v1/completions`) |
| `messages` | `messages` |
| `tools[]` | `tools[]` |
| `stream` | `stream` |

### 9.3 Трансляция streaming

`internal/balancer/llamacpp_translate_resp.go:translateOpenAISSEDataToOllama`:
- Конвертирует OpenAI SSE чанки (`data: {…}\n\n`) в Ollama NDJSON (`{…}\n`).
- Сохраняет `tool_calls` delta-структуру.
- Добавляет финальный chunk с `done: true, done_reason: "stop"` (или `"tool_calls"` если были tool calls).

### 9.4 Tool calls detection

`internal/balancer/llamacpp_toolcall_detector.go`:
- **`detectAndExtractToolCallsFromContent`** — основная функция, пробует несколько форматов.
- **`detectHermesToolCallsInContent`** — `<tool_call>{…}</tool_call>` (Qwen, Hermes).
- **`detectMistralToolCallsInContent`** — `[TOOL_CALLS][…]` (Mistral).
- **`detectLlamaPythonTagInContent`** — `<|python_tag|>{…}` (Llama-3).
- **`detectStandardArrayToolCalls`** — стандартный массив.

Если модель эмитит tool_call в `content`, а `tool_calls` пустой — извлекаем из content и заполняем структуру OpenAI.

Подробнее в [`runbook-tools.md`](runbook-tools.md).

---

## 10. Типичные сценарии

### 10.1 Настройка длинного контекста для gemma-4

```bash
# 1. Создать профиль
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -d '{"contextLength": 32768, "batchSize": 1024, "numGpuLayers": -1, "flashAttn": true}'

# 2. Применить (reload на всех бэкендах)
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply

# 3. Запрос от Cline с длинным system prompt
curl -X POST http://localhost:18081/api/chat \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma-4","messages":[{"role":"system","content":"... длинный prompt ..."},{"role":"user","content":"hi"}]}'
```

### 10.2 Исправление HTTP 413 на tools-запросе

См. [`runbook-tools.md` сценарий A](runbook-tools.md).

### 10.3 Сброс reload loop (HTTP 413 `ReloadLoopLimitError`)

```bash
# После исправления причины (например, освободили VRAM):
curl -X POST http://localhost:18081/api/v1/nctx-reload/cppworker-gpu-1/reset

# Если счётчик на стороне cppworker — нужен R-6 endpoint:
curl -X POST http://localhost:18081/api/v1/cppworker/reset-reload-counter
# (после реализации R-6)
```

### 10.4 Полная конфигурация .env.bundled для длинного контекста

```env
# RAM fallback для n_ctx
CPPWORKER_RAM_FALLBACK_N_CTX=true
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000
CPPWORKER_RAM_FALLBACK_GPU_LAYERS=0  # CPU-only fallback при OOM

# n_ctx auto-reload на балансировщике
LB_NCTX_RELOAD_ENABLED=true
LB_NCTX_RELOAD_MAX_N_CTX=131072
LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR=0.85
LB_NCTX_RELOAD_TIMEOUT_SEC=120

# Write timeout для длинных streaming-ответов
CPPWORKER_WRITE_TIMEOUT=1800  # 30 минут
```

---

## 11. Связанные документы

- [`api.md`](api.md) — полная спецификация REST API.
- [`runbook-tools.md`](runbook-tools.md) — диагностика tools/tool_calls.
- [`audit-2026-06.md`](audit-2026-06.md) — статус реализации.
- [`../.clinerules`](../.clinerules) §3–4 — рабочие правила для n_ctx/RAM fallback.
- [`../plans/README.md`](../plans/README.md) — roadmap (R-1…R-7).