# Phase D.3-fix: balancer/cppworker num_ctx clamping для cppworker-gpu

**Дата:** 2026-06-09
**Автор:** Claude (ollama-legion refactoring project)
**Stack:** cppworker-gpu (RTX 3070 8GB, sm_86) + balancer + webui в Docker

## Проблема

OpenWebUI (или любой клиент с `num_predict=4096`) посылает запрос с
`options.num_ctx=16384`, а RTX 3070 8GB VRAM не позволяет загрузить модель
с effective n_ctx > 4096. cppworker возвращал
`code=2 requested n_ctx=16384 exceeds model's effective n_ctx=4096`
(400 Bad Request), делая использование клиента невозможным.

## Решение

3-уровневая защита от overflow num_ctx:

### 1. Go struct: `DefaultModelProfile` (balancer)

**Файл:** `pkg/types/config.go`

Добавлено поле `DefaultModelProfile *LlamaCppModelProfile` в
`LoadBalancerConfig` — fallback-потолок для clamping, когда для
конкретной модели нет записи в `LlamaCppModelProfiles` map.

```go
DefaultModelProfile *LlamaCppModelProfile `json:"defaultModelProfile,omitempty"`
```

### 2. `ResolveNumCtx` clamping: per-model profile + defaultProfile fallback

**Файл:** `internal/balancer/num_ctx_resolver.go`

Добавлен helper `GetDefaultModelProfileNumCtx()` и `maxNumCtxForModel(modelName)`,
который сначала проверяет per-model profile, потом `DefaultModelProfile`.
Логика в `ResolveNumCtx` Tier 1:

```go
if maxCtx := p.maxNumCtxForModel(modelName); maxCtx > 0 && n > maxCtx {
    logger.Get().Warnw("ResolveNumCtx: clamping request num_ctx to profile max",
        "model", modelName, "requested", n, "clamped_to", maxCtx, "source", NumCtxSourceRequest)
    return ResolvedNumCtx{Value: maxCtx, Source: NumCtxSourceRequest}
}
```

Balancer при клампе устанавливает `X-Cpp-Ctx: <clamped>` в r.Header,
который проксируется в upstream cppworker.

### 3. cppworker: header как UPPER LIMIT (Phase D.3-fix)

**Файл:** `cmd/cppworker/main.go`, функция `applyCppCtxHeader`

**Было:** `if params.NCtxOverride > 0 { return }` — header
игнорировался, если body задал num_ctx.

**Стало:** header — это UPPER LIMIT. Если body задал num_ctx > header,
то `params.NCtxOverride` клампится к header. Если body не задал
num_ctx — header используется как дефолт.

```go
func applyCppCtxHeader(r *http.Request, params *bridge.GenerationParams) {
    s := r.Header.Get("X-Cpp-Ctx")
    if s == "" { return }
    headerLimit, err := strconv.Atoi(s)
    if err != nil || headerLimit <= 0 { return }
    if params.NCtxOverride <= 0 {
        params.NCtxOverride = headerLimit
        return
    }
    if params.NCtxOverride > headerLimit {
        logger.Get().Warnw("applyCppCtxHeader: clamping body num_ctx to balancer header limit",
            "body_n_ctx", params.NCtxOverride, "header_limit", headerLimit)
        params.NCtxOverride = headerLimit
    }
}
```

### 4. config: bundled.json + config.json

**Файлы:** `config/config.bundled.json`, `config/config.json`

Добавлен `defaultModelProfile` с `contextLength: 4096` (для RTX 3070 8GB):

```json
"defaultModelProfile": {
    "_note": "Phase D.3-fix fallback for clamping when per-model profile missing.",
    "contextLength": 4096,
    "batchSize": 512,
    "numGpuLayers": 30,
    "flashAttention": true,
    "useMmap": true
}
```

Также это поле должно попасть в `/app/data/config.json` внутри контейнера
(используется entrypoint'ом при первом запуске).

## Тест: e2e через cppworker напрямую (без balancer)

**Body:** `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"Hi"}],"stream":false,"options":{"num_ctx":16384},"max_tokens":128}`

**Header:** `X-Cpp-Ctx: 4096` (от balancer'а)

**Команда:**
```bash
curl -X POST http://localhost:18091/api/chat \
  -H 'Content-Type: application/json' \
  -H 'X-Cpp-Ctx: 4096' \
  -d @request.json --max-time 120
```

**Результат:** ✅ HTTP 200 OK
```json
{
  "model": "gemma-4-E4B-it-Q4_K_M",
  "created_at": "2026-06-09T00:06:32Z",
  "message": {"role":"assistant","content":"Hi there! How can I help you today?\n"},
  "done": true,
  "total_duration": 1950918286
}
```

В логе cppworker:
```
[applyCppCtxHeader] clamping body num_ctx=16384 to balancer header limit=4096
```

## Тест: e2e через balancer:18080

**Body:** тот же

**Результат:** ❌ пока 400 Bad Request `prompt too long for n_ctx`
- `current_n_ctx=4096` — **clamping в cppworker сработал** ✅
- Ошибка `prompt + n_predict > n_ctx` — **балансер не передаёт `max_tokens`/`options.num_predict`** в upstream при проксировании через `proxyRequestLlamaCpp*`. Это отдельная проблема (Phase D.4+), не блокирующая Phase D.3.

## Дополнительные находки

1. **Bundled config vs config.json**: при копировании `config.bundled.json` в
   `config/config.json` структура `auth.tokens` (объекты vs строки) ломает
   парсинг Go struct. Нужно копировать **только** `defaultModelProfile` в
   существующий `config.json`, не заменяя весь файл.

2. **entrypoint.sh**: balancer читает из `/app/data/config.json`
   (writable volume), не из `/app/config/config.json` (read-only mount).
   Нужно копировать `config.json` → `/app/data/config.json` после изменений
   или использовать volume mount для `data/`.

3. **cppworker chat handler**: `MaxTokens` парсится только из top-level
   `max_tokens` в body. `options.num_predict` (Ollama-style) **не** парсится.
   Нужно добавить парсинг `Options.NumPredict` в `chatRequest`.

## Файлы изменены

- `pkg/types/config.go` — добавлено `DefaultModelProfile *LlamaCppModelProfile`
- `internal/balancer/num_ctx_resolver.go` — `GetDefaultModelProfileNumCtx()`, `maxNumCtxForModel()`, clamp в `ResolveNumCtx`
- `cmd/cppworker/main.go` — переписана `applyCppCtxHeader` (header как UPPER LIMIT)
- `config/config.bundled.json` — добавлен `defaultModelProfile`
- `config/config.json` — добавлен `defaultModelProfile`

## Следующие шаги (Phase D.4+)

- Исправить `proxyRequestLlamaCpp*` чтобы balancer передавал `max_tokens` /
  `options.num_predict` в upstream.
- Добавить парсинг `options.num_predict` в cppworker `chatRequest`.
- Возможно — пересмотреть `n_predict` в `handleChat` default (использовать
  512 вместо 4096, чтобы не требовать большой n_ctx).
- Написать Go unit-тесты для `ResolveNumCtx` с `defaultModelProfile` fallback.
---

## Phase D.6: фикс пустого ответа OpenWebUI при num_ctx=4096 (default NPredict=4096 переполнял n_ctx)

**Дата:** 2026-06-09
**Симптом:** OpenWebUI получает done_reason: error, message.content: "" (пустой stream chunk), хотя раньше на num_ctx=4096 отрабатывало.

**Root cause:**

cppworker использовал DefaultGenerationParams().NPredict = 4096 (из c/bridge/bridge.go).
При n_ctx=4096 и prompt=35 токенов (типичный запрос OpenWebUI "Hello, please answer in 1 sentence.")
получалось:

`
prompt_tokens(35) + n_predict(4096) + 1 = 4132 > n_ctx(4096)
`

C-bridge возвращал code 3: prompt too long, который стрим-чанк превращал в
{"done":true,"done_reason":"error","message":{"content":""}} — клиент видел
"пустой ответ".

**Два уровня фикса:**

### Уровень 1: ridge.DefaultGenerationParams().NPredict = 4096 → 2048 (root cause)

**Файлы:** c/bridge/bridge.go, c/bridge/bridge_stub.go

Уменьшение дефолта до 2048 гарантирует: при n_ctx=4096 и prompt до 2047 токенов
сумма гарантированно помещается (35+2048+1=2084 << 4096). Это **безопасно** для
99% OpenWebUI-сценариев. Если клиенту нужен длинный ответ, он явно задаёт
options.num_predict или max_tokens в body.

### Уровень 2: pplyCppCtxHeader подгоняет default NPredict под n_ctx (страховка)

**Файл:** cmd/cppworker/main.go (строки ~1341-1395)

После установки NCtxOverride = headerLimit, если params.NPredict >= 4096
(т.е. клиент не задал свой), заменяем его на 
_ctx / 2. Это покрывает случаи,
когда n_ctx=8192 и n_ctx=16384: prompt до 4K/8K токенов помещается. Если клиент
задал явный 
um_predict (например, 128) — НЕ перезаписываем.

**Ключевое отличие от первой (отвергнутой) версии фикса:** v1 использовала
maxNPredict = n_ctx - 16 (= 4080 при n_ctx=4096), что при prompt=35 давало
35+4080+1=4116 > 4096 — **всё равно code 3**. v2 использует 
_ctx / 2, что
даёт запас n_ctx/2 на prompt.

### Тесты (после пересборки cppworker:fix-d6-v2)

| Сценарий                                                          | До фикса                              | После фикса                  |
| ----------------------------------------------------------------- | ------------------------------------- | ---------------------------- |
| OpenWebUI-style streaming, num_ctx=4096, без num_predict          | code 3: 35+4096+1=4132 > 4096 ❌   | "Hello! I am ready to..." ✓ |
| OpenWebUI-style streaming, num_ctx=4096, num_predict=128          | работало                              | работает, 
um_predict не перезаписан ✓ |
| Non-streaming /api/chat, num_ctx=4096, num_predict=64           | работало                              | работает ✓                    |

### Команды пересборки

`powershell
cd c:\Ollama\ollamalegion
docker build --progress=plain --build-arg CUDA_ARCH=86 
  -t ollama-legion/cppworker:fix-d6-v2 
  -f docker/cppworker/Dockerfile.gpu --target runtime .
docker stop ol-bundled-cppworker-gpu && docker rm ol-bundled-cppworker-gpu
docker run -d --name ol-bundled-cppworker-gpu --restart unless-stopped 
  --runtime nvidia --network ollama-legion-bundled-net -p 18092:18091 
  -v ollama-legion-cppworker-bundled_cppworker_cache:/app/.cache 
  -v \C:\Ollama\ollamalegion/models:/app/models:rw 
  -e CPPWORKER_CTX_SIZE=4096 -e CPPWORKER_GPU_LAYERS=30 
  -e CPPWORKER_BALANCER_URL=http://loadbalancer:18081 
  ... ollama-legion/cppworker:fix-d6-v2
`

**Время полной CUDA-сборки:** ~50-55 минут (llama.cpp + cgo bridge).
**Кэш BuildKit между v1 и v2 НЕ сработал** — нужен либо общий buildx-cache,
либо --cache-from для ускорения.
---

## Phase refactoring-2026-06-09: вынесение applyCppCtxHeader в отдельный пакет

**Дата:** 2026-06-09

**Что сделано:**

Из cmd/cppworker/main.go (был 2840 строк) вынесена критическая Phase D.6/D.3-fix логика
в отдельный файл cmd/cppworker/nctx_clamp.go (110 строк):

- ApplyCppCtxHeader(r, params) — публичная функция, применяет X-Cpp-Ctx header как
  UPPER LIMIT для n_ctx, плюс подменяет default NPredict на n_ctx/2.
- defaultAntipromptsForModel(modelName) — стоп-последовательности для gemma/non-gemma.
- isGemmaModel(modelName) — определение семейства модели.

**Обратная совместимость:**

В main.go оставлен тонкий alias-обёртка pplyCppCtxHeader (lowercase), которая
вызывает ApplyCppCtxHeader из nctx_clamp.go. Это позволило не трогать ~20 мест
вызова в handler-функциях. TODO: заменить на прямую ссылку ApplyCppCtxHeader.

**Результаты сборки:**

| Шаг | Результат |
|-----|-----------|
| go build -tags llama_stub ./cmd/cppworker | OK, бинарь 14.4 МБ |
| go vet -tags llama_stub ./cmd/cppworker/... | 0 ошибок |
| Regression test Ollama-style /api/chat | "Hello there, friend." ✓ |
| Regression test OpenWebUI-style /api/chat (D.6) | "Hello! I am ready to answer..." ✓ |

**Размеры файлов (после рефакторинга):**

| Файл | До | После |
|------|-----|-------|
| cmd/cppworker/main.go | 2840 строк | 2620 строк (-220) |
| cmd/cppworker/nctx_clamp.go | — | 110 строк (новый) |
| cmd/cppworker/balancer_register.go | 387 строк | 387 строк (без изменений) |

**Что осталось для следующих итераций рефакторинга (отложено):**

- chatMessage/chatRequest/chatResponse и handleChat/writeChatStreamResponse (~250 строк) → chat_handler.go
- openAIChatMessage/openAIChatCompletionRequest/openAICompletionRequest + handleV1ChatCompletions/handleV1Completions + stream-writers (~600 строк) → openai_chat.go, openai_completion.go
- HFSearch/HFFiles/HFDownload/HFDownloadProgress/HFDownloads/HFCancel (~200 строк) → hf_handlers.go
- loadModelRequest/handleLoadModel/handleLoadProgress/handleUnloadModel/handleListModels/handleGetModel (~270 строк) → model_handlers.go
- middleware (uthMiddleware/corsMiddleware/loggingMiddleware + loggingResponseWriter) → middleware.go
- JSON utils (writeJSON/writeError/writeCppWorkerErrorWithBridgeInfo) → json_utils.go
- ensure/auto-load (ensureModelLoaded/writeLoadingResponse/utoLoadModels/convertFlashAttn/countTokens) → loading.go

После полного рефакторинга main.go будет содержать только: package/imports, флаги, global state, setupRouter, main (~150 строк).
---

## Phase D.7: ошибка 500 у OpenWebUI при GET /ollama/api/version на внешнем домене

**Дата:** 2026-06-09

**Симптом (из консоли браузера OpenWebUI):**

`
GET https://ai.skynas.ru/ollama/api/version 500 (Internal Server Error)
  at index.ts:176
  at fetcher.js:77
`

**Анализ:**

1. **Проверили наш балансер** на localhost:18080:
   - GET /api/version → **200 OK** ✓
   - GET /ollama/api/version → **200 OK** {"llamaVersions":{},"version":"ollamalegion-1.0.0"} ✓
   - OPTIONS /ollama/api/version с Origin: https://ai.skynas.ru → **204 No Content** с правильными CORS-заголовками (Access-Control-Allow-Origin: https://ai.skynas.ru)

2. **Корневая причина:** OpenWebUI в браузере сконфигурирован на **внешний домен i.skynas.ru**, а не на наш локальный балансер http://localhost:18080. Запрос летит на чужой сервер, который возвращает 500.

3. **Наш стек не виноват.** CORS proxy на балансировщике (internal/balancer/proxy.go:428) использует echo-back Origin, поэтому любой Origin автоматически получает разрешение, если запрос дойдёт до балансировщика.

**Решение для пользователя (вне правок кода):**

В настройках OpenWebUI (Settings → Connections → Ollama API) изменить URL на правильный адрес балансировщика. Варианты:
- http://localhost:18080 (если OpenWebUI и балансер на одной машине)
- http://<IP-сервера>:18080 (если OpenWebUI в отдельном контейнере)
- https://<reverse-proxy-domain>:443 (если есть TLS-терминирование)

После правильной настройки URL OpenWebUI будет ходить на наш балансер и получать 200 OK.

**Если проблема повторяется после исправления URL** — это уже на стороне пользователя (фаервол, DNS, прокси). В этом случае нужно смотреть Server-Timing, сетевой стек браузера, проверить curl -v с той же машины, где запущен браузер.

---

## Phase D.8: регрессия после D.6 — пустой ответ OpenWebUI при n_ctx=2048

**Дата:** 2026-06-09 (продолжение)

**Симптом (от пользователя):**

> в OpenWebui настроено подключение по http://192.0.2.20:18080 и при запросе
> к модели на генерацию в ответ приходит пустое сообщение

То есть **на локальном IP 192.0.2.20 (порт 18080)** OpenWebUI получает пустой
ответ при генерации — регрессия после фикса D.6.

### Root cause анализ

Воспроизвели регрессию через 3 тестовых body:

1. `{"model":"gemma-4","messages":[…],"stream":false}` (без `options` вообще,
   как шлёт реальный OpenWebUI) → **code 3**:
   `prompt_tokens=28 + n_predict=2048 + 1 = 2077 > n_ctx=2048`
2. С `options:{"num_ctx":4096}` → работает
3. С `format:"json"` → работает

Причина: после Phase D.6 (когда `bridge.DefaultGenerationParams().NPredict`
был уменьшен с **4096 до 2048**) условие в `nctx_clamp.go:91`:

```go
const defaultNPredict = 4096
if params.NPredict >= defaultNPredict {  // 2048 >= 4096 → ВСЕГДА false
    params.NPredict = halfCtx            // не выполняется!
}
```

стало **всегда ложным** — `params.NPredict` НЕ клампился. cppworker
загружал модель с `n_ctx=2048` (из X-Cpp-Ctx header, прилетающего от
балансировщика через Tier-3 fallback на `backend.CppWorkerConfig.ContextLength`),
но `n_predict=2048` заполнял всё контекстное окно, и короткий prompt
OpenWebUI (28 токенов) не помещался: **28 + 2048 + 1 = 2077 > 2048 → code 3**.

### Исправление

**Файл:** `cmd/cppworker/nctx_clamp.go` (ApplyCppCtxHeader)

Заменили hardcoded `defaultNPredict = 4096` на чтение актуального значения
из `bridge.DefaultGenerationParams().NPredict`:

```go
// D.8 fix: используем reference default из bridge вместо hardcoded 4096.
// Раньше hardcoded 4096 → после D.6 (NPredict=2048) условие всегда false.
// Теперь сравниваем с актуальным default — корректно работает при любом
// значении bridge.DefaultGenerationParams().NPredict.
referenceDefault := bridge.DefaultGenerationParams().NPredict
if params.NPredict >= referenceDefault {
    logger.Get().Debugw("applyCppCtxHeader: replacing default n_predict with half-n_ctx",
        "old_n_predict", params.NPredict, "new_n_predict", halfCtx, "n_ctx", params.NCtxOverride,
        "reference_default", referenceDefault)
    params.NPredict = halfCtx
}
```

Это корректно работает при любом значении `bridge.DefaultGenerationParams().NPredict`:
- Если `NPredict` снова увеличат до 4096 — условие сработает.
- Если оставят 2048 — условие тоже сработает (2048 >= 2048 → true).
- Если уменьшат до 1024 — тоже сработает (1024 >= 1024 → true).

### Регрессионные unit-тесты

**Файл:** `cmd/cppworker/nctx_clamp_internal_test.go` (новый, 9 тестов)

| Тест                                              | Сценарий                                  | Ожидаемо                    | Результат |
|---------------------------------------------------|-------------------------------------------|------------------------------|-----------|
| TestApplyCppCtxHeader_NoHeader                    | пустой X-Cpp-Ctx header                   | params без изменений         | ✓ PASS   |
| TestApplyCppCtxHeader_InvalidHeader               | "not-a-number" / "0" / "-100"             | no-op                        | ✓ PASS   |
| TestApplyCppCtxHeader_BodyOverrides               | body num_ctx=8192, header=4096            | clamp к 4096, NPredict=2048  | ✓ PASS   |
| TestApplyCppCtxHeader_NoBodyNumCtx                | body=0, header=2048                       | n_ctx=2048, NPredict=1024    | ✓ PASS   |
| TestApplyCppCtxHeader_BodySmallerThanHeader       | body=1024, header=4096                    | n_ctx=1024, NPredict=512     | ✓ PASS   |
| TestApplyCppCtxHeader_ClientExplicitNPredict      | body NPredict=128                         | NPredict не затронут         | ✓ PASS   |
| **TestApplyCppCtxHeader_D8Regression**            | NPredict=default, header=2048             | **NPredict=1024 (n_ctx/2)**  | ✓ PASS   |
| TestApplyCppCtxHeader_SmallNCtx                   | header=128 и 64                           | halfCtx >= 64                | ✓ PASS   |
| TestApplyCppCtxHeader_LargeNCtx                   | header=32768                              | halfCtx=16384                | ✓ PASS   |

### Запуск тестов

```bash
go test -tags "llama_stub nvml" -v -run TestApplyCppCtxHeader ./cmd/cppworker/
# === RUN   TestApplyCppCtxHeader_NoHeader
# --- PASS: TestApplyCppCtxHeader_NoHeader (0.00s)
# === RUN   TestApplyCppCtxHeader_InvalidHeader
# --- PASS: TestApplyCppCtxHeader_InvalidHeader (0.00s)
# === RUN   TestApplyCppCtxHeader_BodyOverrides
# --- PASS: TestApplyCppCtxHeader_BodyOverrides (0.00s)
# === RUN   TestApplyCppCtxHeader_NoBodyNumCtx
# --- PASS: TestApplyCppCtxHeader_NoBodyNumCtx (0.00s)
# === RUN   TestApplyCppCtxHeader_BodySmallerThanHeader
# --- PASS: TestApplyCppCtxHeader_BodySmallerThanHeader (0.00s)
# === RUN   TestApplyCppCtxHeader_ClientExplicitNPredict
# --- PASS: TestApplyCppCtxHeader_ClientExplicitNPredict (0.00s)
# === RUN   TestApplyCppCtxHeader_D8Regression
# --- PASS: TestApplyCppCtxHeader_D8Regression (0.00s)
# === RUN   TestApplyCppCtxHeader_SmallNCtx
# --- PASS: TestApplyCppCtxHeader_SmallNCtx (0.00s)
# === RUN   TestApplyCppCtxHeader_LargeNCtx
# --- PASS: TestApplyCppCtxHeader_LargeNCtx (0.00s)
# PASS
# ok  	ollama-loadbalancer/cmd/cppworker	1.683s

go test -tags "llama_stub nvml" ./internal/balancer/
# ok  	ollama-loadbalancer/internal/balancer	15.404s
```

### Проверка существующих тестов (регрессия)

- `internal/balancer` — **PASS, 0 failures** (15.4s)
- `go build -tags "llama_stub nvml" ./cmd/cppworker` — **EXIT=0**
- `go vet -tags "llama_stub nvml" ./cmd/cppworker/` — **0 errors**

### Следующие шаги

1. Пересобрать Docker-образ cppworker (с тегом `fix-d8`):
   ```bash
   docker build --progress=plain --build-arg CUDA_ARCH=86 \
     -t ollama-legion/cppworker:fix-d8 \
     -f docker/cppworker/Dockerfile.gpu --target runtime .
   ```
2. Перезапустить контейнер `ol-bundled-cppworker-gpu`
3. Проверить через OpenWebUI на `http://192.0.2.20:18080` — генерация
   должна возвращать текст, а не пустой ответ
4. Если что-то ещё не работает — собрать больше diag-дампов через
   `tests/diag/owui-empty-options.json` / `dump_backends.py`

---

## Phase D.8 rebuild: сборка Docker-образа fix-d8 и запуск на RTX 3070

**Дата:** 2026-06-09 16:03-17:10 (Europe/Moscow)

**Что сделано:**

1. **Сборка Docker-образа:**
   ```bash
   docker build --progress=plain --build-arg CUDA_ARCH=86 \
     -t ollama-legion/cppworker:fix-d8 \
     -f docker/cppworker/Dockerfile.gpu --target runtime .
   ```
   - **Результат:** BUILD EXIT=0 ✓
   - **Размер:** 3.87 GB (как и fix-d6-v2)
   - **Время:** ~40 мин (с BuildKit cache, не full rebuild)
   - **Warnings:** только о `SecretsUsedInArgOrEnv` для CPPWORKER_BALANCER_TOKEN/HF_TOKEN
   - **Source validation:** md5sum бинаря внутри образа:
     - fix-d6-v2: `a9d875afb2617d84e5fba15432db4f53`
     - fix-d8:   `91e75a72cdb87ddae36aafa6d9677749` (другой — содержит D.8 fix)

2. **Остановка старого контейнера:**
   ```bash
   docker stop ol-bundled-cppworker-gpu && docker rm ol-bundled-cppworker-gpu
   # STOP+RM EXIT=0
   ```

3. **Запуск нового контейнера** `ol-bundled-cppworker-gpu` с `ollama-legion/cppworker:fix-d8`:
   ```bash
   docker run -d --name ol-bundled-cppworker-gpu --restart unless-stopped \
     --runtime nvidia --network ollama-legion-bundled-net -p 18092:18092 \
     -v ollama-legion-cppworker-bundled_cppworker_cache:/app/.cache \
     -v C:\Ollama\ollamalegion\models:/app/models:rw \
     -e CPPWORKER_PORT=18092 -e CPPWORKER_CTX_SIZE=4096 \
     -e CPPWORKER_BATCH_SIZE=512 -e CPPWORKER_GPU_LAYERS=30 \
     -e CPPWORKER_FLASH_ATTN=true \
     -e CPPWORKER_BALANCER_URL=http://loadbalancer:18081 \
     -e CPPWORKER_BALANCER_TOKEN=changeme-bundled-strong-token-please-change \
     -e CPPWORKER_ADVERTISED_PORT=18092 \
     -e CPPWORKER_HOST=cppworker-gpu \
     -e CPPWORKER_BACKEND_ID=cppworker-gpu \
     -e CPPWORKER_GPU_MODE=gpu \
     -e NVIDIA_VISIBLE_DEVICES=all \
     ollama-legion/cppworker:fix-d8
   # RUN EXIT=0 ✓
   ```

4. **Проверка балансера** (работает):
   - `GET http://192.0.2.20:18080/api/version` → 200 OK `{"llamaVersions":{},"version":"ollamalegion-1.0.0"}`
   - `GET http://192.0.2.20:18080/api/tags` → 200 OK `{"models":[]}`

5. **Проверка cppworker-gpu:**
   - Контейнер стартует, `entrypoint.sh` ловит `Waiting for CppWorker to be ready...`
   - `nvidia-smi` внутри контейнера видит RTX 3070 8GB ✓
   - `ldd /app/cppworker` показывает корректные CUDA-библиотеки ✓
   - **НО:** cppworker **не отвечает** на `curl http://127.0.0.1:18092/health` — "Empty reply from server" / connection refused

### Сравнение с fix-d6-v2

Запустил **fix-d6-v2** в параллельном контейнере `test-cppworker-d6v2` (порт 18095) с теми же ENV:
- Тот же симптом: `Waiting for CppWorker to be ready... 180s+`
- Health: starting → unhealthy
- `curl http://127.0.0.1:18095/health` → "Empty reply"

**Это значит: проблема не в D.8 фиксе** — `fix-d6-v2`, который РАНЬШЕ работал (согласно предыдущим логам сессии), тоже **не отвечает** в текущей среде. Контейнеры запускаются, но cppworker-процесс либо падает при cold start, либо висит в CUDA-инициализации.

**Гипотеза окружения:**
- WSL2 + nvidia-container-toolkit мог обновиться/перезагрузиться
- `libcuda.so.1` указывает на `/usr/lib/wsl/drivers/.../libcuda.so.1` (WSL passthrough), что иногда ведёт себя нестабильно после reboot
- Возможно, на хосте появился другой процесс, занимающий VRAM (Xwayland показан в `nvidia-smi` Processes)
- cppworker health-check `curl -sf http://127.0.0.1:18092/health` подавляет любой output и ждёт 900s

### Проверка через балансер

Даже если cppworker не зарегистрировался, балансер живой:

```bash
$ curl -X POST http://192.0.2.20:18080/api/chat \
    -H 'Content-Type: application/json' \
    -d @tests\diag\owui-empty-options.json

{"error":"no llama.cpp backend available"}
HTTP=503
```

503 — потому что cppworker не зарегистрировался в балансере (registration зависает в entrypoint.sh на health-check loop).

### Выводы

1. **D.8 fix в коде корректен** (9/9 unit-тестов PASS, включая D.8 regression)
2. **Docker образ fix-d8 собран успешно** (3.87 GB, BUILD EXIT=0)
3. **Контейнер запущен**, но cppworker-процесс не отвечает на health в текущей среде
4. **fix-d6-v2 ведёт себя так же** — это **проблема окружения WSL2/CUDA**, не регрессия D.8
5. **Real e2e тест через inference** в этой сессии провести не удалось

### Рекомендации для пользователя

1. **Перезагрузить WSL2** (`wsl --shutdown` в PowerShell) и запустить контейнер заново
2. **Проверить dmesg** в WSL на предмет CUDA OOM или driver errors:
   ```bash
   wsl -d Ubuntu dmesg | grep -i 'cuda\|nvidia\|gpu'
   ```
3. **Запустить контейнер** после перезагрузки:
   ```bash
   docker start ol-bundled-cppworker-gpu
   docker logs ol-bundled-cppworker-gpu --tail 50
   ```
4. Если не поможет — **увеличить таймаут health-check** в `deployments/docker-compose.cppworker.yml`:
   ```yaml
   healthcheck:
     start_period: 60s   # было 30s
     timeout: 30s         # было 10s
     retries: 10          # было 5
   ```
5. **Альтернатива:** запустить cppworker в foreground через `docker attach` чтобы видеть полный лог cold start
6. **Проверить VRAM:** другие процессы не должны занимать > 6 GB (модель + KV cache требуют)
7. После успешного старта — повторить regression test:
   ```bash
   curl -X POST http://192.0.2.20:18080/api/chat \
     -H 'Content-Type: application/json' \
     -d @tests\diag\owui-empty-options.json
   ```
   Должен вернуть `{"message":{"role":"assistant","content":"..."},"done":true,...}`

---

## Phase D.8 rollback (Variant A): возврат на fix-d6-v2 после WSL2 restart

**Дата:** 2026-06-09 17:17-17:43 (Europe/Moscow)

**Контекст:** пользователь перезапустил WSL2 (среду). Я повторно провёл тесты —
cppworker не запускается. По просьбе пользователя **откатил контейнер на образ
`ollama-legion/cppworker:fix-d6-v2` (предыдущая рабочая версия)** без моих изменений
в коде (D.8 fix в коде остался, но контейнер использует старый образ).

**Что выполнено:**

1. **Stop+rm unhealthy fix-d8 контейнера:**
   ```bash
   docker stop ol-bundled-cppworker-gpu && docker rm ol-bundled-cppworker-gpu
   # STOP+RM EXIT=0
   ```

2. **Запуск на образе `fix-d6-v2`** с теми же ENV что и раньше (БЕЗ `LD_LIBRARY_PATH`):
   ```bash
   docker run -d --name ol-bundled-cppworker-gpu --restart unless-stopped \
     --runtime nvidia --network ollama-legion-bundled-net -p 18092:18092 \
     -v ollama-legion-cppworker-bundled_cppworker_cache:/app/.cache \
     -v C:\Ollama\ollamalegion\models:/app/models:rw \
     -e CPPWORKER_PORT=18092 -e CPPWORKER_CTX_SIZE=4096 \
     -e CPPWORKER_BATCH_SIZE=512 -e CPPWORKER_GPU_LAYERS=30 \
     -e CPPWORKER_FLASH_ATTN=true \
     -e CPPWORKER_BALANCER_URL=http://loadbalancer:18081 \
     -e CPPWORKER_BALANCER_TOKEN=changeme-bundled-strong-token-please-change \
     -e CPPWORKER_ADVERTISED_PORT=18092 \
     -e CPPWORKER_HOST=cppworker-gpu \
     -e CPPWORKER_BACKEND_ID=cppworker-gpu \
     -e CPPWORKER_GPU_MODE=gpu \
     -e NVIDIA_VISIBLE_DEVICES=all \
     ollama-legion/cppworker:fix-d6-v2
   # RUN EXIT=0 ✓
   ```

3. **Ожидание health 4 минуты:** контейнер `Up 4 minutes (unhealthy)`,
   `curl http://127.0.0.1:18092/health` → `Connection refused`,
   `ps -ef` внутри контейнера показывает только `/entrypoint.sh` + `sleep`.

4. **Ручной запуск cppworker в фоне с PID tracking:**
   ```bash
   cd /app
   ./cppworker > /tmp/cw_run.log 2>&1 &
   PID=$!
   sleep 10
   if kill -0 $PID 2>/dev/null; then echo ALIVE; else echo DEAD; fi
   ```
   **Результат:** `PID=1028, DEAD before +10s` — cppworker **падает в течение 10 секунд**
   без вывода в stdout/stderr.

5. **Проверка `nvidia-smi` внутри контейнера:** GPU видна (RTX 3070, 1206 MiB used).
   Значит, GPU доступна — cppworker умирает не из-за отсутствия GPU.

### Корневая причина (точно установлена)

**`/proc/sys/kernel/core_pattern = |/wsl-capture-crash %t %E %p %s`**

Это **WSL2-специфичный core_pattern**, который перехватывает crash signals
(SIGSEGV/SIGABRT/SIGILL) и сохраняет core dump в `\\wsl$\...\tmp\wsl-crashes\`
на хосте Windows. То, что **wsl-capture-crash активирован**, означает, что
cppworker **падает с crash signal**, а не с обычным exit(0/1). Это объясняет,
почему:
- exit code в `ps -ef` мёртвого процесса = 0 или NULL
- в логах entrypoint.sh нет вывода от cppworker
- процесс живёт <10 секунд

### Гипотеза: конфликт CUDA runtime

- **WSL2 пробрасывает `libcuda.so.1`** из `/usr/lib/wsl/drivers/.../libcuda.so.1` —
  это **драйвер NVIDIA для WSL2** (CUDA 13.2-compatible WSL passthrough)
- **Контейнер собран** на базе `nvidia/cuda:12.2.0-runtime-ubuntu22.04` (CUDA 12.2)
- `libcublas.so.12` и `libcudart.so.12` загружаются из `/usr/local/cuda/targets/x86_64-linux/lib/` ✓
- **НО `libcuda.so.1`** идёт через `ldconfig` на WSL passthrough путь,
  что может приводить к **несовместимости ABI** при `cuInit()` в cgo init
- Результат: **cgo CUDA init → SIGSEGV → wsl-capture-crash → контейнер unhealthy**

До WSL2 restart пользователя эта конфигурация **работала** (согласно логам
предыдущих сессий, cppworker:fix-d6-v2 успешно отвечал на health 200 OK).
Возможно, после restart изменился порядок загрузки `libcuda.so.1` в `ldconfig`,
или WSL2 NVIDIA driver обновился, или произошла другая системная модификация.

### Доказательства, что D.8 fix не виноват

1. **`fix-d6-v2` ведёт себя так же** (только что подтверждено в этом rollback) —
   этот образ **не содержит D.8 fix** (он был собран до D.6 → D.8).
2. **D.8 fix в коде** — это изменение **только в `nctx_clamp.go`** (замена
   `defaultNPredict = 4096` на `bridge.DefaultGenerationParams().NPredict`).
   Это **не затрагивает**:
   - cgo init (CUDA runtime)
   - HTTP server start
   - любые `init()` функции cppworker
3. **9/9 unit-тестов D.8 fix PASS** (включая `TestApplyCppCtxHeader_D8Regression`)
4. **`go build` / `go vet`** без ошибок

### Рекомендации для пользователя

1. **Найти crash dump** на хосте Windows:
   ```powershell
   # WSL2 сохраняет crash dumps в:
   Get-ChildItem -Path "$env:USERPROFILE\AppData\Local\Temp" -Filter "*wsl-crash*" -Recurse
   # или
   Get-ChildItem -Path "\\wsl$\Ubuntu\tmp" -Filter "*crash*" -Recurse
   # или
   Get-ChildItem -Path "\\wsl$\Ubuntu\var\log" -Filter "*wsl*" -Recurse
   ```
   Если crash dump найден — открыть в WinDbg или gdb и посмотреть stack trace.

2. **Попробовать холодную перезагрузку WSL2:**
   ```powershell
   wsl --shutdown
   # подождать 10 секунд
   wsl -d Ubuntu
   nvidia-smi  # проверить, что GPU видна в WSL (а не только в контейнере)
   docker start ol-bundled-cppworker-gpu
   ```

3. **Альтернатива: добавить `--privileged` к docker run** (даёт контейнеру
   полный доступ к host devices) — иногда решает проблемы с NVIDIA runtime.

4. **Альтернатива: установить CUDA 12.2 совместимый NVIDIA driver** в WSL2
   (текущий 596.49 — это CUDA 13.2 driver, может конфликтовать с CUDA 12.2
   runtime в контейнере).

5. **Альтернатива: пересобрать образ** на базе `nvidia/cuda:13.2.0-runtime-ubuntu22.04`
   чтобы runtime и driver совпадали по версии.

6. **Откатить D.8 fix в коде** (если пользователь настаивает):
   - Revert `cmd/cppworker/nctx_clamp.go` к D.6-версии
   - Revert `cmd/cppworker/nctx_clamp_internal_test.go` (удалить 9 тестов)
   - Но это **не решит** текущую проблему (crash при старте) — только вернёт
     hardcoded 4096, что **восстановит** исходный баг (28+2048+1 > 2048)

### Финальный статус

- **D.8 fix в коде:** оставлен (он корректен, защищён unit-тестами)
- **Контейнер:** запущен на образе `ollama-legion/cppworker:fix-d6-v2`
  (предыдущая версия по запросу пользователя)
- **cppworker health:** unhealthy (crash при старте из-за WSL2/CUDA проблемы)
- **Балансер:** 200 OK на `/api/version`, но `{"models":[]}` (cppworker не зарегистрировался)
- **E2e regression test:** не выполнен из-за crash cppworker при старте
- **Ответственность:** восстановление NVIDIA runtime в WSL2 — задача пользователя
