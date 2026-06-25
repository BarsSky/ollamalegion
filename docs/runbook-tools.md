# Runbook: дебаг сброса запросов с tools/tool_calls

**Версия:** 2026-06-22
**Назначение:** пошаговое руководство по поиску причин, по которым OpenWebUI / Cline / Roo Code
получают «сброс» или пустой ответ при запросах с `tools[]` к OllamaLegion (cppworker/llama.cpp).

---

## 1. Симптомы

Клиент сообщает одно из следующего (часто все сразу):

- `использован один источник, но самого ответа нет`
- HTTP 5xx / 413 на каждой второй итерации диалога с tools
- `data: [DONE]` без `tool_calls` (модель «думала», но не вызвала инструмент)
- бесконечный «reload loop»: модель выгружается и загружается заново на каждом запросе
- HTTP 503 `model is loading` сразу после первого запроса

---

## 2. Где искать (top-down)

### 2.1 Уровень клиента

Что шлёт клиент → что получает:

```bash
# PowerShell
curl -X POST http://localhost:18080/v1/chat/completions `
  -H "Content-Type: application/json" `
  -d (Get-Content test_toolcall.json -Raw) `
  -o response.json

# Просмотр raw response
Get-Content response.json
```

Включи `--verbose` (`-v` в curl) чтобы увидеть заголовки: наличие `X-Cpp-Ctx` от балансера
и `Content-Type: text/event-stream` (SSE) vs `application/x-ndjson` (Ollama-format).

**Ключевые поля в запросе:**

| Поле | Где | Что проверить |
|---|---|---|
| `tools[]` | body | Должен присутствовать (OpenWebUI шлёт всегда при включённых tools) |
| `model` | body | Совпадает с тем, что есть на бэкенде (qwen2.5:7b-instruct-q4_K_M и т.п.) |
| `stream` | body | `true` для OpenWebUI — иначе балансер проксирует по другому пути |
| `options.num_ctx` | body | Если клиент задаёт явно (например 16384), балансер клампит через `X-Cpp-Ctx` header |
| `options.num_predict` / `max_tokens` | body | 0 = невалидно; 31744 + 6887 prompt > 32768 n_ctx → `code 3` |

### 2.2 Уровень балансировщика

Проверить, что запрос вообще дошёл до балансировщика:

```bash
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/health
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends
```

Логи балансировщика (zap JSON в stdout):

```powershell
docker logs -f deployments-loadbalancer-1 2>&1 | Select-String "tools|chat_id|stream|/v1/chat"
```

**Что искать:**

- `parsed request: model=... stream=...` — балансер распарсил тело.
- `[BALANCER → BACKEND] POST /api/chat` или `/v1/chat/completions` — запрос ушёл на бэкенд.
- `proxyRequestOpenAIStreaming: heartbeat write failed` — клиент закрыл соединение по таймауту (часто при reload-loop).

### 2.3 Уровень cppworker (главный источник проблемы)

Включить verbose-режим:

```yaml
# deployments/.env.bundled
CPPWORKER_VERBOSE=true
```

или:

```powershell
$env:CPPWORKER_VERBOSE = "true"
```

Перезапустить cppworker и повторить запрос.

**Ключевые логи для поиска причин сброса:**

| Лог-сообщение | Что значит | Действие |
|---|---|---|
| `clamping n_predict to fit n_ctx` | `n_predict` уменьшен через `clampNPredictToFitContext`. Поля: `actual_prompt_tokens`, `requested_n_predict`, `clamped_n_predict`, `n_ctx` | Если `actual_prompt_tokens` большой (>4096 при n_ctx=8192) — увеличить n_ctx профиля модели |
| `applyCppCtxHeader: clamping body num_ctx to balancer header limit` | Балансер уменьшил n_ctx | Проверить `X-Cpp-Ctx` header в запросе от балансера |
| `applyCppCtxHeader: replacing default n_predict with n_ctx-reserve` | n_predict заменён на `n_ctx - reserve`. Поля: `has_tools`, `prompt_reserve`, `reference_default` | Если `has_tools=false` при реальных tools — handler не передал опцию |
| `RAM fallback: reload disabled for tools-request` | **Главный источник «сброса»** — reload для tools отключён (`inference.go:491`) | Клиент получит HTTP 413. Решение: уменьшить history/tools или увеличить n_ctx профиля |
| `RAM fallback: cycle limit reached, refusing reload` | Превышен `ramFallbackMaxAttempts=3` за 60 сек. Поля: `attempts`, `elapsed`, `max_attempts` | Это и есть «бесконечный reload loop». Решение: перезагрузить cppworker или подождать окно |
| `RAM fallback: reloading model with larger n_ctx` | Начата перезагрузка модели. Поля: `old_n_ctx`, `new_n_ctx`, `old_gpu_layers`, `new_gpu_layers`, `use_mmap` | На время reload (10-30 сек) все запросы к модели ждут или получают 503 `model is loading` |
| `RAM fallback: model reloaded successfully` | Успешный reload | После этого запросы должны идти нормально |
| `bridge code 3` или HTTP 413 + `prompt too long` | C-bridge вернул `ErrPromptTooLong`. Сумма `prompt + n_predict + 1 > n_ctx` | Уменьшить prompt или n_predict |

---

## 3. Воспроизведение на конкретной машине

### 3.1 Быстрое воспроизведение (bundled stack)

```powershell
# Запустить bundled стек (cppworker-gpu + balancer + webui)
.\scripts\start-bundled.ps1 -Rebuild

# Воспроизвести проблему (test_toolcall.json содержит запрос с одним tool `search`)
curl -X POST http://localhost:18080/v1/chat/completions `
  -H "Content-Type: application/json" `
  -d (Get-Content test_toolcall.json -Raw)
```

Если получаем пустой ответ или 413 — проблема воспроизведена.

### 3.2 Сравнение с «рабочей» ollama на этой же машине

```powershell
# 1. Запустить ollama напрямую на порт 11435
$env:OLLAMA_HOST = "127.0.0.1:11435"
ollama serve

# 2. Зарегистрировать этот ollama как backend в балансировщике
curl -X POST http://localhost:18081/api/v1/backends `
  -H "X-API-Token: <token>" -H "Content-Type: application/json" `
  -d '{"id":"ollama-native","host":"127.0.0.1","ollama_port":11435,"type":"ollama","weight":1,"max_concurrent_reqs":10}'

# 3. Прогнать тот же тест, направляя на ollama-native
curl -X POST http://localhost:18080/api/chat `
  -H "Content-Type: application/json" `
  -d '{"model":"qwen2.5:7b-instruct-q4_K_M","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"search","description":"Search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],"stream":false}'
```

**Если ollama-native работает, а cppworker — нет:** проблема в логике cppworker (clamp/fallback/Hermes detection).

**Если оба не работают:** проблема в балансировщике или в формате запроса (например, Ollama native не понимает `tools[]` без `options.tools` — нужна конвертация).

### 3.3 Stub-режим для локального дебага (без GPU)

```powershell
# Собрать stub-бинарник
go build -tags llama_stub -o cppworker-stub.exe .\cmd\cppworker

# Запустить stub на порту 18092
.\cppworker-stub.exe --port 18092 --models-dir ./models

# В отдельном терминале — наш bundled балансер должен быть запущен на 18081
# Зарегистрировать stub:
curl -X POST http://localhost:18081/api/v1/backends `
  -H "X-API-Token: <token>" -H "Content-Type: application/json" `
  -d '{"id":"cppworker-stub","host":"127.0.0.1","ollama_port":18092,"cppworker_port":18092,"type":"llama_cpp","engine":"llama_cpp","weight":1,"max_concurrent_reqs":10}'
```

Stub-режим полезен для проверки transport-логики (SSE-трансляция, NDJSON-парсинг, tools fallback),
но **НЕ** для проверки реального n_ctx overflow (потому что stub не имеет реального tokenizer'а).

### 3.4 Запуск существующих диагностических тестов

```powershell
# Baseline для проверки transport-проксирования tools
go test ./tests/ -run "TestDebugOpenWebUI_ToolCalls" -tags llama_stub -count=1 -v

# Новые тесты для проверки inference-логики (clamp/fallback/Hermes)
go test ./tests/ -run "TestCppWorker_ToolsRequest" -tags llama_stub -count=1 -v
```

---

## 4. Конкретные сценарии сброса и их причины

### 4.1 Сценарий A: HTTP 413 на первом же запросе с tools

**Симптом:** клиент сразу получает 413, модель не успевает ничего сгенерировать.

**Корневая причина:** сумма `prompt_tokens + NPredict + 1 > n_ctx` → C-bridge возвращает `code 3` →
`clampNPredictToFitContext` обрезает NPredict до `minNPredictClamp=512`, но всё равно не хватает →
`tryRamFallbackReload` отказывается делать reload для tools (`hasTools=true`) →
возвращается `*ReloadDisabledForToolsError` → handler транслирует в HTTP 413.

**Диагностика:**

1. Лог `clamping n_predict to fit n_ctx` с `clamped_n_predict < requested_n_predict`.
2. Лог `RAM fallback: reload disabled for tools-request`.
3. Лог `bridge code 3` (если до fallback).

**Решение:**

- Уменьшить количество tools / длину system prompt в OpenWebUI.
- Увеличить `n_ctx` в профиле модели (через WebUI → Manage Models).
- Установить `CPPWORKER_RAM_FALLBACK_N_CTX=true` и `CPPWORKER_RAM_FALLBACK_MAX_N_CTX=65536` — это позволит reload при tools (но потенциально с медленным mmap-fallback'ом).

### 4.2 Сценарий B: первый запрос OK, второй — «пустой ответ»

**Симптом:** первая итерация диалога с tools проходит нормально; вторая (с историей) — пустой NDJSON-чанк.

**Корневая причина:** на второй итерации prompt включает всю историю + tool definitions + tool results.
Суммарно `history_tokens + tool_defs + NPredict + 1 > n_ctx`. `clampNPredictToFitContext` обрезает NPredict
до 512, и даже при `minNPredictClamp` модель не может сгенерировать ничего осмысленного — финальный чанк
с `done:true` приходит с пустым `content` и без `tool_calls`. Клиент видит «модель использовала один
источник, но ответа нет».

**Диагностика:**

1. Лог `clamping n_predict to fit n_ctx` с `actual_prompt_tokens` >> 2048.
2. Лог `applyCppCtxHeader: replacing default n_predict with n_ctx-reserve` с `has_tools=true` и
   `clamped_n_predict < 1024`.
3. Финальный NDJSON-чанк: `message.content=""`, `message.tool_calls=null`.

**Решение:**

- Включить в OpenWebUI «Truncate history» (Sliding Window).
- Уменьшить `MAX_HISTORY_TOKENS` или `MAX_TOOLS` в настройках клиента.
- Увеличить `n_ctx` до 16384-32768.

### 4.3 Сценарий C: бесконечный reload loop

**Симптом:** cppworker в логах постоянно `RAM fallback: reloading model with larger n_ctx` и
`RAM fallback: model unloaded` → клиент получает 503 `model is loading` или таймауты.

**Корневая причина:** `ramFallbackMaxAttempts=3` за 60 сек превышен, но каждая итерация диалога
всё равно вызывает overflow → cycle limit → `ReloadLoopLimitError` → HTTP 413. Это выглядит как
«модель сбрасывается» (потому что в логах действительно видны reload-операции).

**Диагностика:**

1. Лог `RAM fallback: cycle limit reached, refusing reload` с `attempts=3`, `elapsed~10s`.
2. Лог `RAM fallback: reloading model with larger n_ctx` повторяется 3+ раза за 60 сек.
3. Клиент видит HTTP 413 с сообщением `reload limit reached... reduce tools/prompt`.

**Решение:**

- Срочно: перезапустить cppworker (`docker restart deployments-cppworker-gpu-1`), чтобы сбросить
  счётчик `ramFallbackAttempts`.
- Долгосрочно: устранить первопричину overflow (см. 4.1 / 4.2).

### 4.4 Сценарий D: tool_call в content не извлекается (Hermes/Qwen)

**Симптом:** cppworker стримит SSE с `delta.content="<tool_call>{...}</tool_call>"` (Hermes-style),
но клиент (OpenWebUI через балансер) получает финальный NDJSON без `message.tool_calls`.

**Корневая причина:** cppworker отдаёт tool_call в content (потому что chat template модели —
Hermes/Qwen). Балансер при трансляции SSE→NDJSON должен вызвать `detectAndExtractToolCallsFromContent`,
но либо не вызывает, либо парсер не находит pattern.

**Диагностика:**

1. Лог cppworker: финальный chunk содержит `choices[0].delta.content="<tool_call>..."`, без `tool_calls`.
2. Лог балансировщика: `tool_calls найден: false` или подобный.
3. В raw response клиента: `message.tool_calls=null`, но `message.content="<tool_call>..."`.

**Решение:**

- Это известная проблема (см. `tests/openwebui_tool_calls_debug_test.go:684-911`, сценарии E/E2).
- Проверить, что в `cmd/cppworker/tool_calls.go` парсер `parseToolCallsFromOutput` /
  `extractHermesToolCalls` покрывает формат используемой модели.
- Если модель эмитит `[TOOL_CALLS][...]` — проверить `extractMistralToolCalls`.
- Если `<|python_tag|>{...}` — `extractLlamaPythonTagCalls`.

### 4.6 Сценарий F: prompt > n_ctx даже с reload (Cline 55K токенов)

**Симптом:** клиент (Cline/OpenWebUI) присылает prompt значительно больше текущего
`n_ctx` модели — например, Cline шлёт 55111 токенов + `n_predict=512` = 55624,
при этом модель загружена с `n_ctx=32768`, а VRAM позволяет максимум
`max_vram_n_ctx=32719` (или любое значение ≤ required). Пользователь видит:

```
[OLLAMA] Ollama stream processing error: stream inference failed with code 3:
prompt too long for n_ctx: prompt_tokens=55111 + n_predict=512 + 1 = 55624
> n_ctx=32768 (model loaded with n_ctx=32768, request asked for n_ctx=32768).
Reduce prompt, set smaller max_tokens, or save a model profile with bigger
n_ctx and reload the model (current_n_ctx=32768, required_n_ctx=55624,
max_vram_n_ctx=32719): prompt + n_predict exceeds n_ctx
```

**Корневая причина:** даже с включённым `AutoReloadNCtx=true` и `AutoTuneNCtx`,
balancer НЕ МОЖЕТ перезагрузить модель с `n_ctx > MaxVRAMNCtx * safety_factor`
(по умолчанию 0.85) — VRAM физически не позволяет разместить такой объём
KV-cache. Preflight в этом случае возвращает HTTP 413 с actionable советом.

**Решение (по возрастанию инвазивности):**

1. **Сохранить профиль модели с большим `n_ctx`** и применить его, ЕСЛИ
   `MaxVRAMNCtx` в принципе поддерживает нужный размер:
   ```bash
   # Узнать текущий лимит VRAM
   curl http://cppworker:18092/api/v1/cppworker/config | jq .runtime
   
   # PUT профиль с n_ctx=65536 (если VRAM=80GB и model_max поддерживает)
   curl -X PUT http://balancer:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
     -H 'X-API-Token: <token>' \
     -H 'Content-Type: application/json' \
     -d '{"contextLength": 65536, "batchSize": 512, "numGpuLayers": -1, "flashAttn": true}'
   
   # Apply (save + reload на всех бэкендах)
   curl -X POST http://balancer:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply \
     -H 'X-API-Token: <token>'
   ```

2. **Уменьшить prompt или `n_predict`** в настройках клиента. Cline:
   `Settings → API Configuration → Max Tokens`. OpenWebUI:
   `Workspace → Models → Parameters → Max Tokens`.

3. **Перейти на GPU с большим VRAM** (A100 80GB, H100 и т.п.) — единственный
   способ обойти физ. лимит для очень больших prompt.

**Preflight (новое поведение, default с 2026-06-23):**

- Balancer ДО отправки запроса оценивает размер prompt (`chars/4 + n_predict + 1`).
- Если `estimated > current_n_ctx` И `≤ MaxVRAMNCtx * 0.85` И `≤ model_max_context`
  → balancer **сам** перезагружает модель с целевым `n_ctx` через `POST /api/models/reload`,
  ждёт завершения и отправляет запрос. Один round-trip, без code 3.
- Если `> MaxVRAMNCtx * 0.85` → HTTP 413 с JSON `{error, reason, required_n_ctx,
  current_n_ctx, max_vram_n_ctx, model_max_context, suggestion, profile_endpoint}`.
- Лог-маркеры: `preflight: triggering reload`, `preflight: reload succeeded`,
  `preflight: reject (VRAM/model_max exhausted)`.

**Флаги конфигурации:**

- `LB_NCTX_RELOAD_ENABLED=true` (default true) — включает auto-reload.
- `LB_NCTX_RELOAD_ALLOW_TOOLS=true` (default true) — разрешает reload при tools-запросах.
- `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR=0.85` — доля от MaxVRAMNCtx, доступная для KV-cache.
- `CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS=true` (default true) — на стороне cppworker.
- Чтобы вернуть старое поведение (без auto-reload при tools):
  `LB_NCTX_RELOAD_ENABLED=false` ИЛИ `LB_NCTX_RELOAD_ALLOW_TOOLS=false`.

### 4.7 Сценарий G: EOF при preflight n_ctx reload (cppworker рвёт соединение) — ИСПРАВЛЕНО 2026-06-24

**Симптом:** OpenWebUI / curl получают **пустой ответ / EOF / connection reset** на первом же
запросе с `options.num_ctx` отличным от default (например `num_ctx=16384`, а модель загружена
с `n_ctx=8196`). В логах cppworker — `UnloadModel → LoadModelWithOpts` reload. В логах
балансера — `preflight: triggering reload` + `queryCppWorkerModels: EOF`.

**Корневая причина (ИСПРАВЛЕНО):**

cppworker в `handleReloadModel` вызывал `backend.UnloadModel()` → `backend.LoadModelWithOpts()`
**без ожидания завершения активных inference-запросов**. В момент `UnloadModel` cppworker
рвал HTTP-соединения всех текущих запросов → клиенты получали EOF. Балансер,
который в этот момент polling'ом проверял `/api/models` через `queryCppWorkerModels`,
ловил connection reset / EOF и зависал в `concurrent load already in progress, waiting`
polling loop (модель state=loading, но polling тоже падал).

**Что сделано (Track 1: cppworker graceful reload):**

1. `internal/cppbackend/inflight.go` — новый per-model `InFlightCounter` (atomic).
2. `cmd/cppworker/handlers_generate.go:handleGenerate` / `handleOllamaGenerate`,
   `handlers_chat.go:handleChat`, `handlers_openai.go:handleV1ChatCompletions` /
   `handleV1Completions` — каждый делает `Inc(modelName)` на входе и `Dec(modelName)` в defer.
3. `cmd/cppworker/handlers_model.go:handleReloadModel` — **перед** `UnloadModel` зовёт
   `backend.InFlight().WaitZero(modelName, 0)` (без лимита; общий watchdog — на уровне reload).
4. `internal/cppbackend/backend.go:SetReloadPending/GetReloadPending` — добавляет
   `reload_pending: {model, startedAt, elapsedMs}` в JSON `/api/info` на время reload.
5. `internal/balancer/preflight_nctx.go:preflightNCtxReloadIfNeededSync` (Track 2) —
   синхронная версия preflight, polling heartbeat `/api/info` 500ms, default timeout 60s
   (`Balancing.PreflightSyncTimeoutMs`, max 180s). При таймауте — fallback на
   `503 + Retry-After: 15`.

**Что сделано (Track 3: balancer EOF retry):**

`internal/balancer/llamacpp_backend_helpers.go:queryCppWorkerModels` теперь:

- До 3 попыток с exponential backoff (100ms, 200ms, 400ms) на EOF/`connection reset`/
  `broken pipe`/bad status/decode error.
- При неудаче всех попыток — fallback на `lastKnownModels` кэш (TTL 30s, per backend).
- `ensureModelLoadedOnBackend` теперь использует этот retry, polling в
  `concurrent load already in progress, waiting` loop больше НЕ зависает.

**Что сделано (Track 4: balancer reload dedup):**

`internal/balancer/nctx_reload_dedup.go` — `reloadDedupRegistry` (per backendID+modelName+targetNCtx).
`executeAsyncReload` (preflight) и `ensureModelLoadedOnBackend` координируются:

- `IsReloadPending(backendID, modelName)` — если true, polling на state=loading заменяется
  на `WaitReloadDone(timeout=5min)`.
- `StartReloadIfNotPending` — если reload с тем же target уже идёт, возвращает
  существующий entry, не запускает второй HTTP запрос.

**Acceptance criteria для верификации:**

1. cppworker `handleReloadModel` **не вызывает** `UnloadModel`, пока `InFlight().Get(modelName) > 0`.
2. cppworker `/api/info` показывает `reload_pending` пока reload в процессе.
3. Балансер `preflightNCtxReloadIfNeededSync` возвращает 200 OK с проксированным
   ответом, если reload завершился за `PreflightSyncTimeoutMs` (default 60s).
4. `queryCppWorkerModels` retry 3 раза на EOF и возвращает последний snapshot из кэша.
5. Параллельный `executeAsyncReload` (preflight) + `ensureModelLoadedOnBackend` —
   только один HTTP reload на cppworker, второй ждёт через `WaitReloadDone`.

**Конфигурация:**

- `Balancing.PreflightSyncEnabled` (default `true`) — sync режим (round-trip один).
- `Balancing.PreflightSyncTimeoutMs` (default `60000`, max `180000`) — таймаут ожидания.
- Kill-switch для legacy скриптов: `Balancing.PreflightSyncEnabled=false` →
  fallback на async 503 + `Retry-After: 5` (старое поведение).

### 4.5 Сценарий E: «сброс» без видимых причин в логах

**Симптом:** клиент получает HTTP 5xx или пустой ответ, в логах cppworker нет ни fallback,
ни clamp, ни cycle limit — но балансер отвечает «reset by peer».

**Корневая причина:** таймаут на стороне балансировщика. `WriteTimeout` cppworker
(по умолчанию 30 минут, но может быть переопределён через `CPPWORKER_WRITE_TIMEOUT` или `--write-timeout`),
или `RequestTimeout` балансера (`Balancing.RequestTimeout=30` по умолчанию).

**Диагностика:**

1. Время от старта запроса до ошибки в логах балансировщика.
2. Лог cppworker: `backend.GenerateStream` завершился за X секунд; если X > `RequestTimeout` —
   балансер разорвал соединение.
3. Лог cppworker: `write: broken pipe` или `connection reset by peer` — клиент закрыл соединение раньше.

**Решение:**

- Увеличить `Balancing.RequestTimeout` (в `config/config.json`) до 300-600 секунд.
- Увеличить `WriteTimeout` cppworker.
- Уменьшить `max_tokens` / `num_predict` в запросе — длинная генерация может превысить таймаут.

---

## 5. Чек-лист диагноста

1. ☐ Включён `--verbose` (`CPPWORKER_VERBOSE=true`) на cppworker.
2. ☐ Проверен `data/state.json` (балансер) — список бэкендов и их статусы.
3. ☐ Проверены логи балансировщика: `parsed request`, `[BALANCER → BACKEND]`, `heartbeat write failed`.
4. ☐ Проверены логи cppworker: `clamping n_predict`, `RAM fallback`, `bridge code N`.
5. ☐ Запущен `TestDebugOpenWebUI_ToolCalls_ScenarioA..E2` — проходят ли baseline-тесты.
6. ☐ Запущен `TestCppWorker_ToolsRequest_*` (новые тесты из `cppworker_inference_test.go`).
7. ☐ Сравнение с `ollama serve` напрямую — если ollama работает, проблема в cppworker.
8. ☐ Проверены `n_ctx` и `n_predict` в запросе (через `curl -v` + `X-Cpp-Ctx` header от балансера).
9. ☐ Проверена длина `tools[]` и system prompt в OpenWebUI (через WebUI → Workspace → Tools).
10. ☐ Проверен `WriteTimeout` cppworker и `RequestTimeout` балансера.
11. ☐ При HTTP 413 с `bridge_info` — проверены `current_n_ctx`, `max_vram_n_ctx`, `model_max_context`.
     Решение в сценарии F: сохранить профиль модели с большим `n_ctx` через
     `POST /api/v1/cppworker/model-profiles/{name}/apply` или увеличить GPU VRAM.
     При `preflight: triggering reload` в логах — preflight сработал, ждём завершения reload.
12. ☐ При `RAM fallback: reload disabled for tools-request (flag off)` — проверить, что
     `CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS=true` (или `LB_NCTX_RELOAD_ALLOW_TOOLS=true`).

---

## 6. Что НЕ нужно трогать при дебаге

- `c/bridge/bridge.c` — низкоуровневый C-код, изменения требуют перекомпиляции всего.
- `c/llama.cpp/` — upstream subtree.
- Файлы с тегом `// DO NOT EDIT` или генерируемые.

---

## 7. Ссылки

- `docs/troubleshooting.md` — общий troubleshooting.
- `docs/nctx-troubleshooting.md` — специфика n_ctx.
- `docs/cppworker-routing-fixes-2026-06-07.md` — Phase D.3-fix (X-Cpp-Ctx header).
- `docs/test-report-2026-06-09-cppworker-clamping.md` — Phase D.6/D.8 (clamp n_predict).
- `tests/openwebui_tool_calls_debug_test.go` — диагностические тесты транспорта.
- `tests/cppworker_inference_test.go` (новый) — диагностические тесты inference-логики.
- `cmd/cppworker/inference.go` — `generateWithRamFallback`, `clampNPredictToFitContext`.
- `cmd/cppworker/nctx_clamp.go` — `ApplyCppCtxHeader`.
- `cmd/cppworker/tool_calls.go` — парсеры Hermes/Llama-3/Mistral.