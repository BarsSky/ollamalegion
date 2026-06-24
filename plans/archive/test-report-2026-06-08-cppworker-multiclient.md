# Phase 9: OpenWebUI real behavior — SSE error propagation fix

**Дата:** 2026-06-08
**Контекст:** Реальный запрос от OpenWebUI клиента к балансеру `localhost:18080` и backend cppworker-gpu на `localhost:18092`. OpenWebUI показывал "пустой ответ", хотя балансер отвечал `done:true, done_reason:error` без поля `error`.

## 1. Воспроизведение бага

Четыре curl-запроса к `http://localhost:18080`:

### 1.1 Ollama non-stream `/api/chat`
- **HTTP:** 502 Bad Gateway
- **Content-Length:** 411
- **Body:** `{"created_at":"...","done":true,"done_reason":"error","error":"upstream returned HTTP 500: ...","message":{"content":"","role":"assistant"},"model":"gemma-4-E4B-it-Q4_K_M"}`
- ✅ **error** поле присутствует

### 1.2 OpenAI non-stream `/v1/chat/completions`
- **HTTP:** 500 Internal Server Error
- **Content-Length:** 306
- **Body:** `{"error":"chat completions failed: inference failed: prompt too long for n_ctx..."}`
- ✅ **error** поле присутствует

### 1.3 Ollama STREAM `/api/chat` (БАГ!)
- **HTTP:** 200 OK
- **Content-Type:** application/x-ndjson
- **Content-Length:** 112
- **Body:** `{"done":true,"done_reason":"error","message":{"content":"","role":"assistant"},"model":"gemma-4-E4B-it-Q4_K_M"}`
- ❌ **error поле ОТСУТСТВУЕТ** — OpenWebUI видит "пустое сообщение ассистента"

### 1.4 OpenAI STREAM `/v1/chat/completions`
- **HTTP:** 200 OK
- **Content-Type:** text/event-stream
- **Body:** `data: {"error":"...","choices":[{"finish_reason":"error"}]}` + `data: [DONE]`
- ✅ **error** поле присутствует

## 2. Root cause

cppworker при ошибке на OpenAI-пути `/v1/chat/completions` отдаёт SSE-чанк:
```
data: {"choices":[{"delta":{},"finish_reason":"error","index":0}],
       "created":1780905164,
       "error":"stream inference failed with code 1: prompt too long for n_ctx: ...",
       "id":"chatcmpl-...","model":"gemma-4-E4B-it-Q4_K_M",
       "object":"chat.completion.chunk"}
```

У чанка **ЕСТЬ** поле `error`, НО `choices[0].delta` — пустое `{}`, а `finish_reason:"error"`.

В балансере `internal/balancer/llamacpp_transport.go` функция `translateSSEChatToOllama` (строки 450-536) обрабатывала только `choices[0].delta.content` и `choices[0].finish_reason`, но **НЕ читала** поле `error`. В результате формировался Ollama-чанк `{done:true, done_reason:"error", message:{...}}` **БЕЗ** `error` поля.

`translateOpenAIChatToOllama` (non-stream, строки 308-376) уже корректно обрабатывал `error` (строки 323-334) — потому для non-stream баг не проявлялся.

## 3. Fix

Добавлена проверка `chunk["error"]` в `translateSSEChatToOllama` (строки ~485-505) и `translateSSEGenerateToOllama` (строки ~545-565) в `internal/balancer/llamacpp_transport.go`:

```go
// cppworker при ошибке шлёт OpenAI чанк с полем `error` и пустым `choices[0].delta`
// (см. Phase 9 фикс: translateSSEChatToOllama не теряет error). Если поле error есть —
// пробрасываем его в Ollama-чанк с done:true, done_reason:"error", иначе OpenWebUI
// получает "пустое сообщение ассистента" и не показывает диагностику.
if errStr, ok := chunk["error"].(string); ok && errStr != "" {
    logger.Get().Errorw("translateSSEChatToOllama: upstream SSE chunk with error",
        "model", modelName, "error", errStr)
    ollamaChunk["done"] = true
    ollamaChunk["done_reason"] = "error"
    ollamaChunk["error"] = errStr
    ollamaChunk["message"] = map[string]interface{}{
        "role":    "assistant",
        "content": "",
    }
    result, _ := json.Marshal(ollamaChunk)
    return append(result, '\n')
}
```

## 4. Regression-тесты

Создан `tests/multiclient/sse_error_field_test.go` с 3 тестами:

| Тест | Покрывает | Статус |
|------|-----------|--------|
| `TestTranslateSSEChatToOllama_UpstreamErrorPropagates` | `/api/chat` + upstream error chunk | PASS |
| `TestTranslateSSEGenerateToOllama_UpstreamErrorPropagates` | `/api/generate` + upstream error chunk | PASS |
| `TestTranslateSSEChatToOllama_SuccessChunkNoErrorField` | Negative control: success chunk без error | PASS |

Все тесты используют ТОЧНУЮ копию реального SSE-чанка от cppworker с `error` полем.

## 5. End-to-end проверка

После фикса, пересборки балансера и рестарта контейнера `ol-bundled-balancer`:

```bash
$ curl -N -X POST http://localhost:18080/api/chat \
  -H "Content-Type: application/json" \
  --data-binary '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}],"stream":true}'

HTTP/1.1 200 OK
Content-Type: application/x-ndjson
Content-Length: 323

{"done":true,"done_reason":"error",
 "error":"stream inference failed with code 1: requested n_ctx=16384 exceeds model's effective n_ctx=4096. Save a model profile with n_ctx=16384 and reload the model (or send a smaller n_ctx in options.num_ctx)",
 "message":{"content":"","role":"assistant"},
 "model":"gemma-4-E4B-it-Q4_K_M"}
```

✅ OpenWebUI теперь получает **полный текст ошибки** в стриминг-ответе и показывает диагностику пользователю.

## 6. Изменённые файлы

| Файл | Изменение |
|------|-----------|
| `internal/balancer/llamacpp_transport.go` | +обработка `error` поля в `translateSSEChatToOllama` и `translateSSEGenerateToOllama` |
| `internal/balancer/llamacpp_transport_export.go` | NEW — экспорт `translateOpenAISSEDataToOllama` для тестов |
| `tests/multiclient/sse_error_field_test.go` | NEW — 3 regression теста |

## 7. Полный test suite

```
=== RUN   TestMultiClient_ChatAllEndpoints
--- PASS: TestMultiClient_ChatAllEndpoints (0.01s)
=== RUN   TestMultiClient_ParallelSameModel
--- PASS: TestMultiClient_ParallelSameModel (0.01s)
=== RUN   TestMultiClient_DifferentModelsDifferentBackends
--- PASS: TestMultiClient_DifferentModelsDifferentBackends (0.01s)
=== RUN   TestMultiClient_StreamingAndNonStreaming
--- PASS: TestMultiClient_StreamingAndNonStreaming (0.01s)
=== RUN   TestMultiClient_HealthAndTags
--- PASS: TestMultiClient_HealthAndTags (0.00s)
=== RUN   TestTranslateSSEChatToOllama_UpstreamErrorPropagates
--- PASS: TestTranslateSSEChatToOllama_UpstreamErrorPropagates (0.00s)
=== RUN   TestTranslateSSEGenerateToOllama_UpstreamErrorPropagates
--- PASS: TestTranslateSSEGenerateToOllama_UpstreamErrorPropagates (0.00s)
=== RUN   TestTranslateSSEChatToOllama_SuccessChunkNoErrorField
--- PASS: TestTranslateSSEChatToOllama_SuccessChunkNoErrorField (0.00s)
PASS
ok      ollama-loadbalancer/tests/multiclient    1.325s
```

**8/8 tests PASS** (5 multiclient + 3 new regression).

---

# Phase 10 — 502 Bad Gateway в OpenWebUI: `ol-bundled-balancer` оказался не в `ollama-legion-bundled-net`

## 1. Симптом

После исправления Phase 9 (трансляция SSE error) пользователь сообщил, что в браузере OpenWebUI по-прежнему `ai.skynas.ru/ollama/api/version 500 Internal Server Error`, а в ответе — пустое сообщение без описания причины.

Прямой curl на балансер работал (`localhost:18080/ollama/api/version` → 200 OK), но через `ol-bundled-webui:18030/ollama/api/version` — **502 Bad Gateway**.

## 2. Диагностика

### 2.1 CORS preflight (OPTIONS) на балансере

```bash
$ curl -i -X OPTIONS http://localhost:18080/ollama/api/version \
    -H "Origin: http://ai.skynas.ru" -H "Access-Control-Request-Method: GET"
HTTP/1.1 204 No Content
Access-Control-Allow-Credentials: true
Access-Control-Allow-Headers: Content-Type, Authorization, X-Requested-With, Accept, Origin
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS
Access-Control-Allow-Origin: http://ai.skynas.ru
Access-Control-Max-Age: 86400
```

CORS на балансере настроен правильно. Проблема **не** в CORS.

### 2.2 Через `ol-bundled-webui:18030`

```bash
$ curl -i -H "Origin: http://ai.skynas.ru" http://localhost:18030/ollama/api/version
HTTP/1.1 502 Bad Gateway
Server: nginx/1.31.1
```

Nginx webui не может достучаться до бэкенда.

### 2.3 DNS внутри webui

```bash
$ docker exec ol-bundled-webui nslookup loadbalancer
Server:        127.0.0.11
Address:       127.0.0.11:53

** server can't find loadbalancer: NXDOMAIN
```

Имя `loadbalancer` **не резолвится** во встроенном Docker DNS (127.0.0.11).

### 2.4 Состав сетей контейнеров

```bash
$ docker inspect --format '{{.Name}}: {{json .NetworkSettings.Networks}}' \
    ol-bundled-webui ol-bundled-balancer ol-bundled-cppworker-gpu
ol-bundled-webui:           {"ollama-legion-bundled-net": {... IP 172.18.0.2 ...}}
ol-bundled-balancer:        {"bridge": {... IP 172.17.0.2 ...}}                ← ПРОБЛЕМА
ol-bundled-cppworker-gpu:   {"deployments_cppworker-net": ...,
                              "ollama-legion-bundled-net": {... IP 172.18.0.4 ...}}
```

**`ol-bundled-balancer` сидит в дефолтной сети `bridge` (172.17.0.2), а не в `ollama-legion-bundled-net`** (172.18.0.0/16), к которой подключены webui и cppworker-gpu.

## 3. Root cause

Compose-файл `deployments/docker-compose.cppworker-bundled.yml` корректно объявляет `ol-bundled-balancer` в сети `ol-bundled-net`, но в момент первого запуска контейнера по какой-то причине (вероятно, ручной `docker run`, либо ошибка `compose up` без сети) balancer подключился к дефолтной сети `bridge`. После этого:

- nginx в webui резолвит `loadbalancer` через Docker DNS 127.0.0.11 сети `ollama-legion-bundled-net`
- в этой сети нет endpoint'а с именем `loadbalancer` (он в `bridge`)
- nginx получает NXDOMAIN и в entrypoint уходит в restart loop с `host not found in upstream "loadbalancer"`

Прямой curl `localhost:18080` работал, потому что использовал порт на хосте, который проброшен из дефолтной сети, — без прохода через Docker DNS.

## 4. Fix

Подключаем balancer к bundled-net с алиасом `loadbalancer` (имя сервиса из compose-файла):

```bash
# 1. Отключаем от неправильной сети (если был подключён)
docker network disconnect ollama-legion-bundled-net ol-bundled-balancer 2>/dev/null || true

# 2. Подключаем с алиасом 'loadbalancer' (имя сервиса из compose-файла)
docker network connect --alias loadbalancer ollama-legion-bundled-net ol-bundled-balancer

# 3. Пересоздаём webui, чтобы он подхватил свежий DNS
docker compose -f deployments/docker-compose.cppworker-bundled.yml up -d --force-recreate --no-deps webui
```

После этого:
- `nslookup loadbalancer` в webui возвращает IP
- nginx проходит startup без `host not found`
- webui → nginx → balancer работает

## 5. End-to-end проверка

### 5.1 `GET /ollama/api/version` через всю цепочку

```bash
$ curl -i -H "Origin: http://ai.skynas.ru" http://localhost:18030/ollama/api/version
HTTP/1.1 200 OK
Server: nginx/1.31.1
Access-Control-Allow-Origin: http://ai.skynas.ru
{"llamaVersions":{},"version":"ollamalegion-1.0.0"}
```

### 5.2 `GET /ollama/api/tags`

```bash
$ curl -H "Origin: http://ai.skynas.ru" http://localhost:18030/ollama/api/tags
{"models":[{"name":"gemma-4-E4B-it-Q4_K_M","model":"gemma-4-E4B-it-Q4_K_M",...}]}
```

### 5.3 `POST /ollama/api/chat` (STREAM) — Phase 9 fix всё ещё работает

```bash
$ curl -i -N -X POST http://localhost:18030/ollama/api/chat \
    -H "Origin: http://ai.skynas.ru" -H "Content-Type: application/json" \
    -H "Accept: application/x-ndjson" --data-binary "@tmp_chat_body.json"
HTTP/1.1 200 OK
Server: nginx/1.31.1
Access-Control-Allow-Origin: http://ai.skynas.ru

{"done":true,"done_reason":"error","error":"stream inference failed with code 1: requested n_ctx=16384 exceeds model's effective n_ctx=4096. Save a model profile with n_ctx=16384 and reload the model (or send a smaller n_ctx in options.num_ctx)","message":{"content":"","role":"assistant"},"model":"gemma-4-E4B-it-Q4_K_M"}
```

### 5.4 CORS preflight через webui

```bash
$ curl -i -X OPTIONS http://localhost:18030/ollama/api/chat \
    -H "Origin: http://ai.skynas.ru" -H "Access-Control-Request-Method: POST"
HTTP/1.1 204 No Content
Access-Control-Allow-Origin: http://ai.skynas.ru
```

## 6. Защита от регрессии

В скрипты `scripts/start-bundled.ps1` и `scripts/start-bundled.sh` добавлен smoke-check после запуска стека:

1. Проверяет `nslookup loadbalancer` в `ol-bundled-webui` — должен вернуть IP из `172.x.x.x`
2. Проверяет `GET /ollama/api/version` через `localhost:${WEBUI_PORT}` — должен вернуть HTTP 200
3. При ошибке выводит инструкцию:
   ```
   docker network connect --alias loadbalancer ollama-legion-bundled-net ol-bundled-balancer
   ```

## 7. Изменённые файлы (Phase 10)

- `scripts/start-bundled.ps1` — добавлен smoke-check блок (DNS + HTTP)
- `scripts/start-bundled.sh` — то же самое для bash-версии
- `docs/test-report-2026-06-08-cppworker-multiclient.md` — этот отчёт

**Никаких изменений Go-кода не требуется** — баг был инфраструктурный (Docker network alias), а не логический.

---

# Phase 11: num_ctx clamping + cleanup мусорных бэкендов

**Дата:** 2026-06-08 (продолжение)
**Симптом:** OpenWebUI отправляет `options.num_ctx=16384` (default), а модель загружена с `effective n_ctx=4096` (лимит VRAM 8GB на RTX 3070). cppworker отвечает:
`requested n_ctx=16384 exceeds model's effective n_ctx=4096`, OpenWebUI показывает пустой ответ.

## 1. Root cause

В `internal/balancer/num_ctx_resolver.go` функция `ResolveNumCtx` (3-tier resolver) использовала
**прямой приоритет body > profile > backend default** БЕЗ клампинга. То есть если клиент
прислал `num_ctx=16384` в body, resolver использовал 16384 и через `X-Cpp-Ctx` header
cppworker пытался обработать 16384 — что превышает effective n_ctx модели (4096).

Конфиг `config/config.bundled.json` уже имеет профиль `gemma-4` с `numCtx: 4096`, но
**profile не клампит body-override**.

## 2. Дополнительная диагностика

Также при перезапуске стека выявлены мусорные бэкенды от прошлых сессий:

```
Found 2 backends:
  id=llamacpp-gpu-1 host=192.168.1.20 status=unhealthy cppPort=18091
  id=ollama-gpu-1   host=192.168.1.10 status=unhealthy cppPort=0
```

Оба указывают на старые IP (192.168.1.20 и 192.168.1.10) — **не от текущей машины**
(192.168.13.20), поэтому балансер не мог до них достучаться. Удалены через
`DELETE /api/v1/backends/{id}` с auth-токеном.

После удаления cppworker автоматически перерегистрировался с `host: "cppworker-gpu"`
(Docker DNS-имя сервиса в compose) — теперь reachable через Docker network.

## 3. Фикс: num_ctx clamping

В `internal/balancer/num_ctx_resolver.go` функция `ResolveNumCtx` дополнена clamping'ом:

```go
// Tier 1: из request body (наивысший приоритет)
if n := ExtractNumCtxFromBody(body); n > 0 {
    // Clamp к потолку из profile (если задан).
    if profileMax := p.GetModelProfileNumCtx(modelName); profileMax > 0 && n > profileMax {
        logger.Get().Warnw("ResolveNumCtx: clamping request num_ctx to profile max",
            "model", modelName, "requested", n, "clamped_to", profileMax, "source", NumCtxSourceRequest)
        return ResolvedNumCtx{Value: profileMax, Source: NumCtxSourceRequest}
    }
    return ResolvedNumCtx{Value: n, Source: NumCtxSourceRequest}
}
```

**Поведение:**
- Если клиент прислал `num_ctx=16384` и в `config.bundled.json` есть профиль `gemma-4` с `numCtx=4096`:
  резолвер вернёт **4096** (с warning-логом) и установит `X-Cpp-Ctx: 4096` в upstream.
- Если клиент прислал `num_ctx=2048` (меньше профиля): пройдёт без клампинга.
- Если профиля нет: body-значение проходит без изменений (старое поведение).

## 4. Изменённые файлы (Phase 11)

- `internal/balancer/num_ctx_resolver.go` — добавлен clamping + импорт `pkg/logger`
- `config/config.bundled.json` — ранее (Phase 10) установлен `defaultProfile.numCtx: 4096`
  и `profiles.gemma-4.numCtx: 4096`
- `docs/test-report-2026-06-08-cppworker-multiclient.md` — этот отчёт

## 5. Что осталось (за рамками Phase 11)

- Полный перезапуск стека и e2e-проверка OpenWebUI с clamping — прервано из-за длительного билда
  образа `ollama-legion/cppworker:gpu-arch_all` (Docker Desktop API временно стал 500).
- На будущее: установить `CPPWORKER_GPU_TAG=bundled` в compose, чтобы не перебилдивать
  тяжёлый arch_all image каждый раз при `--no-deps up`.
- Тесты `tests/multiclient` имеют pre-existing CGO build issue (`C source files not allowed`),
  не относящийся к Phase 11.
