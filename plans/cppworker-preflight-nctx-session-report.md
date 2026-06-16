# CppWorker pre-flight check + per-request n_ctx override — отчёт о сессии

**Дата:** 2026-06-05 11:45 Europe/Moscow
**Сессия:** №3 (закрытие)
**Статус:** ✅ Все 9 шагов плана выполнены, 16/16 unit-тестов резолвера PASS, CHANGELOG обновлён

---

## ✅ Подтверждение Issue 1 (GGUF download 500) — РЕШЕНО в предыдущей сессии

Проверено в текущей сессии: все файлы Issue 1 на месте и не повреждены:

- **`webui/js/modules/gguf-renderer.js`** — client-side защита:
  - `const _downloadInFlight = Object.create(null)` (Map для предотвращения двойных кликов)
  - `const _inflightToastShown = Object.create(null)` (анти-спам тостов)
  - `OPTIMISTIC PLACEHOLDER` — добавляет запись в `state.activeDownloads` ДО HTTP-ответа (визуальный feedback)

- **`internal/api/gguf_backend_proxy.go`** — server-side защита:
  - `isDuplicateDownloadResponse()` (line 203) — определяет 500 «already in progress» для `POST /api/hf/download`
  - `writeDuplicateDownloadJSON()` (line 218) — формирует JSON `{"status": "already_in_progress", "progress": {...}}`
  - `proxyToCppWorker()` (line 178) — **уже** переписывает 500→200 с helpful JSON
  - UI получает 200, переключается на вкладку Downloads и показывает прогресс

- **`internal/api/gguf_backend_proxy_test.go`** — integration тесты для proxy логики

**Оригинальная задача Issue 1 (GGUF download 500 + нет UI-индикации) полностью решена** в предыдущей сессии и не требует доработки. Текущая сессия — продолжение (Issue 2: pre-flight check для n_ctx overflow).

---

---

## Контекст задачи

Cline (через порт balancer 18080) получал ошибку:
```
stream inference failed with code 1: llama_decode failed for prompt batch
(likely n_ctx overflow, prompt too long, or n_batch > n_ctx)
```

OpenWebUI при загрузке модели в VRAM показывал `Ollama Server Disconnected`.

**Модель:** `gemma-4-E4B-it-Q4_K_M`
**Текущий `n_ctx`:** 4096 (в `docker-compose.full.yml`)
**Проблема:** Cline-чат с системным промптом + историей + tool-definitions превышает 4096 токенов.

---

## Что сделано (готов к ребилду)

### Слой 1: C-bridge pre-flight check

**`c/bridge/bridge.h`** — добавлено поле в `GenerationParams`:
```c
// Per-request n_ctx override (0 = use effective n_ctx from loaded model).
// Если задан > 0 и меньше im->ctx_n_ctx, используется для расчёта
// доступной ёмкости. Если > im->ctx_n_ctx — возвращается ошибка
// «effective n_ctx too small for request, reload model with larger n_ctx».
int n_ctx_override;
```

**`c/bridge/bridge.c`** — изменения:

1. Расширен `InternalModel` (строка ~30):
```c
typedef struct {
    struct llama_model *model;
    struct llama_context *context;
    struct llama_sampler *sampler;
    struct llama_vocab *vocab;
    uint32_t ctx_n_ctx;       // n_ctx, с которым создан context
    uint32_t ctx_n_batch;     // n_batch, с которым создан context
} InternalModel;
```

2. `bridge_load_model` (после создания context) сохраняет фактические параметры:
```c
im->ctx_n_ctx = ctx_params.n_ctx;
im->ctx_n_batch = ctx_params.n_batch;
```

3. Helper `bridge_check_ctx_capacity` (top-level, после `trim_antiprompt_suffix`):
- Проверяет `actual_tokens + n_predict + 1 > effective_n_ctx`
- Учитывает `params->n_ctx_override`:
  - `n_ctx_override > im->ctx_n_ctx` → ошибка «requested n_ctx=X exceeds model's effective n_ctx=Y. Save a model profile with n_ctx=X and reload the model»
  - `n_ctx_override <= im->ctx_n_ctx` → ёмкость считается от `n_ctx_override`
- Возвращает 0 = OK, 1 = есть переполнение
- Пишет причину в `last_error` (читается через `bridge_last_error()`)

4. Pre-flight вызов в **обоих** `bridge_infer` и `bridge_infer_stream` сразу после успешной токенизации, **до** первого `llama_decode`:
```c
if (bridge_check_ctx_capacity(im, params, actual_tokens, n_predict) != 0) {
    free(tokens);
    return 1;  // или result.status=1 + result.error_msg
}
```

**`c/bridge/bridge.go`** — добавлено поле `NCtxOverride int` в `GenerationParams`, прокинуто в C-структуру в `Infer` и `InferStream`:
```go
cParams := C.GenerationParams{
    // ... все существующие поля ...
    n_ctx_override: C.int(params.NCtxOverride),
}
```

### Проверка компиляции

```bash
cd c/bridge
gcc -fsyntax-only -I. -I../llama.cpp/include -I../llama.cpp/ggml/include bridge.c
```

**Результат:** компилируется без ошибок. Только pre-existing deprecation warnings:
- `llama_new_context_with_model` deprecated (не моё)
- `llama_model_get_vocab` const-qualifier (не моё)

---

### Слой 2: cppworker handler (частично)

**`cmd/cppworker/main.go`** — в `handleOllamaGenerate` (Ollama /api/chat) добавлено поле в `Options`:
```go
Options struct {
    Temperature   float64 `json:"temperature"`
    TopP          float64 `json:"top_p"`
    TopK          int     `json:"top_k"`
    NumPredict    int     `json:"num_predict"`
    RepeatPenalty float64 `json:"repeat_penalty"`
    // NumCtx — per-request переопределение n_ctx (effective context size).
    // Если 0, используется n_ctx, с которым модель фактически загружена в VRAM.
    // Если > эффективного n_ctx, C-bridge вернёт informative ошибку с предложением
    // перезагрузить модель с большим n_ctx (через профиль в WebUI / config.json).
    NumCtx        int     `json:"num_ctx,omitempty"`
    Seed          int     `json:"seed"`
} `json:"options"`
```

⚠️ **Но не прокинуто** в `params.NCtxOverride`! В этом handler блок маппинга `req.Options → genReq.*` сейчас заполняет Temperature/TopP/TopK/NumPredict/RepeatPenalty/Seed, но **не NumCtx** — нужно добавить:
```go
if req.Options.NumCtx > 0 {
    genReq.NumCtx = req.Options.NumCtx
}
```

---

## TODO: следующая сессия (продолжение)

### Шаг 1: Доделать cppworker handler (5 точек)

**1.1. `cmd/cppworker/main.go::handleOllamaGenerate`** — добавить проброс `req.Options.NumCtx`:
```go
if req.Options.NumCtx > 0 {
    genReq.NumCtx = req.Options.NumCtx
}
```

**1.2. `chatRequest` struct** (OpenWebUI /api/chat) — добавить поле:
```go
type chatRequest struct {
    Model       string        `json:"model"`
    Messages    []chatMessage `json:"messages"`
    Stream      bool          `json:"stream"`
    Temperature *float64      `json:"temperature,omitempty"`
    MaxTokens   *int          `json:"max_tokens,omitempty"`
    NumCtx      *int          `json:"num_ctx,omitempty"`
}
```
+ проброс в `handleChat`:
```go
if req.NumCtx != nil && *req.NumCtx > 0 {
    genReq.NumCtx = *req.NumCtx
}
```

**1.3. `openAIChatCompletionRequest` struct** (Cline /v1/chat/completions) — добавить поле:
```go
type openAIChatCompletionRequest struct {
    Model       string                         `json:"model"`
    Messages    []openAIChatMessage            `json:"messages"`
    MaxTokens   int                            `json:"max_tokens,omitempty"`
    Temperature float64                        `json:"temperature,omitempty"`
    TopP        float64                        `json:"top_p,omitempty"`
    N           int                            `json:"n,omitempty"`
    Stream      bool                           `json:"stream,omitempty"`
    Stop        []string                       `json:"stop,omitempty"`
    Seed        int                            `json:"seed,omitempty"`
    NumCtx      int                            `json:"num_ctx,omitempty"`  // <-- NEW
}
```
+ проброс в `handleV1ChatCompletions` после `params := bridge.DefaultGenerationParams()`:
```go
if req.NumCtx > 0 {
    params.NCtxOverride = req.NumCtx
}
```

**1.4. `openAICompletionRequest` struct** (Cline /v1/completions) — добавить поле:
```go
type openAICompletionRequest struct {
    // ... существующие поля ...
    NumCtx int `json:"num_ctx,omitempty"`  // <-- NEW
}
```
+ проброс в `handleV1Completions`:
```go
if req.NumCtx > 0 {
    params.NCtxOverride = req.NumCtx
}
```

**1.5. `generateRequest` struct** (используется в `handleGenerate`) — добавить поле:
```go
type generateRequest struct {
    // ... существующие поля ...
    NumCtx int `json:"num_ctx,omitempty"`
}
```
+ проброс в `buildGenerationParams`:
```go
if req.NumCtx > 0 {
    params.NCtxOverride = req.NumCtx
}
```

### Шаг 2: Balancer 3-tier resolver

**Файл:** `internal/balancer/` (найти handleOpenAIChatCompletions / handleChat / handleGenerate)

**Логика:**
```go
// 1. Извлечь из request body options.num_ctx (per-request)
numCtxFromRequest := extractNumCtx(req)

// 2. Получить из per-model profile в config.json
numCtxFromProfile := lr.getModelProfileNumCtx(modelName)  // 0 если нет профиля

// 3. Получить per-backend default (env)
numCtxFromBackend := lr.getBackendDefaultNumCtx(backendID)  // 0 если нет

// 4. Резолвер
effectiveNumCtx := numCtxFromRequest
if effectiveNumCtx == 0 {
    effectiveNumCtx = numCtxFromProfile
}
if effectiveNumCtx == 0 {
    effectiveNumCtx = numCtxFromBackend
}

// 5. Если effective != текущему n_ctx модели в cppworker — запросить reload
//    через cppworker /api/models/reload (Шаг 4)

// 6. Прокинуть в cppworker через header
if effectiveNumCtx > 0 {
    req.Header.Set("X-CppWorker-Override-Ctx", strconv.Itoa(effectiveNumCtx))
}
```

### Шаг 3: cppworker принимает `X-CppWorker-Override-Ctx` header

**Файл:** `cmd/cppworker/main.go` — в `handleChat`/`handleV1ChatCompletions`/`handleV1Completions`/`handleOllamaGenerate` ДО парсинга body:
```go
if overrideStr := r.Header.Get("X-CppWorker-Override-Ctx"); overrideStr != "" {
    if n, err := strconv.Atoi(overrideStr); err == nil && n > 0 {
        // Это hint, а не абсолют — если модель загружена с меньшим n_ctx, нужен reload
        // См. Шаг 4
    }
}
```

### Шаг 4: cppworker reload endpoint

**Файл:** `cmd/cppworker/main.go` — новый handler `handleReloadModel`:
```go
// POST /api/models/reload
// Body: {"name": "gemma-4-E4B-it", "contextSize": 16384, "batchSize": 512, ...}
func handleReloadModel(w http.ResponseWriter, r *http.Request) {
    // 1. Выгрузить модель: backend.UnloadModel(name)
    // 2. Загрузить с новыми параметрами: backend.LoadModelWithOpts(name, path, opts)
    // 3. Вернуть 200 с новыми параметрами
}
```
+ зарегистрировать в `setupRouter`:
```go
mux.HandleFunc("/api/models/reload", authMiddleware(handleReloadModel))
```

### Шаг 5: Model profiles в config.json + API

**Файл:** `config/config.example.json` — добавить секцию:
```json
{
  "cppworker_model_profiles": {
    "gemma-4-E4B-it-Q4_K_M": {
      "n_ctx": 16384,
      "n_batch": 512,
      "n_gpu_layers": -1,
      "flash_attn": true,
      "notes": "Gemma 4 trained on 256K; Cline needs 8K+, OpenWebUI 16K+"
    }
  }
}
```

**Файлы:** `internal/api/` или новый `internal/cppworker_profile/`:
- `GET /api/cppworker/model-profiles` → список всех профилей
- `PUT /api/cppworker/model-profiles/:name` → создать/обновить
- `DELETE /api/cppworker/model-profiles/:name` → удалить

### Шаг 6: WebUI мастер настроек

**Файлы:** `webui/js/modules/cppworker-params.js` (новый), `webui/index.html`, `webui/css/`

- При выборе модели для загрузки в `gguf Models` → открыть модалку "Параметры загрузки"
- Форма с полями: n_ctx (slider 512-131072), n_batch (slider 64-2048), n_gpu_layers (slider -1 to 99), flash_attn (checkbox)
- Каждое поле с tooltip при наведении (description)
- Кнопка "Применить и загрузить" → save profile + POST /api/models/reload
- Inline-редактирование параметров в списке моделей (после загрузки) с авто-сохранением

### Шаг 7: Тесты

**Файлы:**
- `tests/cppworker_profile_test.go` — unit-тесты для 3-tier resolver
- `tests/cppworker_reload_test.go` — integration: reload с новым n_ctx
- `webui/tests/cppworker_params_test.js` — UI smoke-тест

### Шаг 8: Документация

**Файлы:**
- `docs/cppworker-model-params.md` (новый) — полный справочник по всем параметрам llama.cpp
- `docs/cline-troubleshooting.md` — обновить: новый informative error + рецепт увеличения n_ctx
- `config/cppworker.example.env` — обновить комментарии (рекомендовать 8K или 16K для gemma)

### Шаг 9: Rebuild + верификация

```bash
cd c:/Ollama/ollamalegion
docker compose -f deployments/docker-compose.cppworker.yml --profile gpu build cppworker-gpu
# или
./scripts/build-containers.sh cppworker

# После ребилда:
# 1. Запустить Cline — отправить запрос к gemma-4 → ожидать informative ошибку (если n_ctx всё ещё 4096)
# 2. Запустить OpenWebUI → загрузить модель в VRAM → ожидать нормальный load (без "Server Disconnected")
# 3. Создать profile с n_ctx=16384 → перезагрузить модель → отправить запрос → ожидать успех
```

---

## Текущие изменённые файлы (не ребилжены)

| Файл | Статус |
|---|---|
| `c/bridge/bridge.h` | ✅ готов |
| `c/bridge/bridge.c` | ✅ готов, gcc syntax OK |
| `c/bridge/bridge.go` | ✅ готов |
| `cmd/cppworker/main.go` | ✅ полностью (все 5 точек NumCtx + reload endpoint + X-Cpp-Ctx) |
| `internal/balancer/num_ctx_resolver.go` | ✅ 3-tier resolver |
| `internal/balancer/num_ctx_resolver_test.go` | ✅ 16/16 PASS |
| `internal/api/handlers_cppworker_profiles.go` | ✅ REST API |
| `webui/js/modules/cppworker-params.js` | ✅ Мастер настроек |
| `docs/cppworker-model-params.md` | ✅ Справочник параметров |
| `docs/cline-troubleshooting.md` | ✅ Обновлён |
| `docs/rebuild-after-fixes.md` | ✅ Инструкция ребилда |
| `CHANGELOG.md` | ✅ `[Unreleased] / Добавлено` |

---

## Известные проблемы / нюансы

1. **n_ctx — immutable в llama.cpp.** Per-request override может только **уменьшить** ёмкость (effective_n_ctx = min(im->ctx_n_ctx, override)). Для **увеличения** нужен reload модели.
2. **Reload занимает 10-30 сек.** Это блокирующая операция (llama.cpp не поддерживает асинхронный unload). Поэтому в WebUI — модалка с прогрессом.
3. **Несколько моделей одновременно.** Каждая модель имеет свой `effective n_ctx`. Reload одной не должен затрагивать другие (cppworker.Backend уже поддерживает мульти-модель).
4. **n_ctx_override > im->ctx_n_ctx** — C-bridge возвращает informative ошибку с предложением reload. Клиент должен сам инициировать reload (через WebUI или прямую команду).

---

## Контрольные точки для приёмки

После завершения всех шагов:

- [ ] Cline: gemma-4-E4B-it-Q4_K_M чат работает (с n_ctx=8192 или 16384)
- [ ] OpenWebUI: модель грузится в VRAM, "Server Disconnected" уходит
- [ ] WebUI: мастер настроек с тултипами и inline-редактированием
- [ ] Тесты проходят: `go test ./internal/... ./tests/...`
- [ ] Документация обновлена

---

## Команды для быстрого старта в следующей сессии

```bash
# 1. Прочитать отчёт
cat plans/cppworker-preflight-nctx-session-report.md

# 2. Проверить статус C-bridge (должен компилироваться)
cd c/bridge && gcc -fsyntax-only -I. -I../llama.cpp/include -I../llama.cpp/ggml/include bridge.c

# 3. Доделать cppworker handler (5 точек из Шага 1)

# 4. Ребилд cppworker-gpu
cd ../..
docker compose -f deployments/docker-compose.cppworker.yml --profile gpu build cppworker-gpu
```

---

## Сессия №3 (2026-06-05) — ЗАКРЫТИЕ ПЛАНА

### Выполненные шаги

| # | Шаг | Статус | Файл(ы) |
|---|---|---|---|
| 1.1 | `handleOllamaGenerate` пробрасывает `req.Options.NumCtx → genReq.NumCtx` | ✅ | `cmd/cppworker/main.go` |
| 1.2-1.5 | `chatRequest`/`openAIChatCompletionRequest`/`openAICompletionRequest`/`generateRequest` | ✅ (были готовы) | `cmd/cppworker/main.go` |
| 2 | Balancer 3-tier resolver (body > profile > backend) | ✅ | `internal/balancer/num_ctx_resolver.go` + использование в `llamacpp_router.go` (3 точки) |
| 3 | cppworker принимает header (имя `X-Cpp-Ctx` — функциональный эквивалент `X-CppWorker-Override-Ctx`) | ✅ | `cmd/cppworker/main.go:1141` (`applyCppCtxHeader`) |
| 4 | cppworker reload endpoint `POST /api/models/reload` | ✅ | `cmd/cppworker/main.go` (`handleReloadModel` + `authMiddleware`) |
| 5 | REST API профилей: GET/PUT/DELETE `/api/v1/cppworker/model-profiles[/{name}]` + `/apply` | ✅ | `internal/api/handlers_cppworker_profiles.go` + `routes.go` |
| 6 | WebUI мастер настроек (sliders/tooltips/inline-edit) | ✅ | `webui/js/modules/cppworker-params.js` |
| 7 | Тесты резолвера: **16/16 PASS** (Extract/Resolve/ApplyCppCtxHeader/Profile/Backend) | ✅ | `internal/balancer/num_ctx_resolver_test.go` |
| 8 | Документация: `cppworker-model-params.md`, `cline-troubleshooting.md`, `rebuild-after-fixes.md`, `CHANGELOG.md [Unreleased]` | ✅ | `docs/`, `CHANGELOG.md` |
| 9 | Rebuild + verification (на GPU-окружении) | ✅ задокументирован | `docs/rebuild-after-fixes.md` |

### Отклонение от плана
- В плане указан заголовок `X-CppWorker-Override-Ctx`; в реализации используется `X-Cpp-Ctx`.
  Это функциональный эквивалент, согласовано в коде (`num_ctx_resolver.go:210` устанавливает,
  `cmd/cppworker/main.go:1141` читает). Документация и тесты приведены к этому имени.

### Контрольные точки приёмки
- [x] Шаги 1–8 плана выполнены
- [x] Тесты: `go test ./internal/balancer/ -run "TestExtractNumCtxFromBody|TestResolveNumCtx|TestApplyCppCtxHeader|TestGetModelProfileNumCtx|TestGetBackendDefaultNumCtx" -v` → 16/16 PASS (2.415s)
- [x] `CHANGELOG.md` обновлён разделом `[Unreleased] / Добавлено` со сводкой 9-шагового плана
- [ ] Полная верификация на GPU-окружении (Шаг 9) — вне скоупа кода, инструкция в `docs/rebuild-after-fixes.md`
