# OllamaLegion — сводный аудит обмена запросами, полноты данных и WebUI-управления

**Дата:** 2026-09-20
**Область:** `client → balancer → cppworker → balancer → client`, проксирование Ollama API,
конвертация ответов cppworker, полнота данных для разных типов моделей и клиентов
(OpenWebUI / Cline / Roo Code / ollama-python и др.), метрики и управление в WebUI.

> ## Статус исправлений (R65d — 2026-09-20)
>
> Исправлено в этой ревизии (Фазы 0–4 плана, 34 пункта):
>
> | Раздел | Что сделано |
> |---|---|
> | **1.1** (400 на Ollama-полях) | Ollama-трафик идёт на **нативные** `/api/chat`, `/api/generate`, `/api/embeddings`, `/api/embed` cppworker. Флаг `LB_OLLAMA_NATIVE_PATH` (default on, `=0` — откат без рестарта). `chatRequest` расширен до полного Ollama-набора; `openAIChatCompletionRequest`/`openAICompletionRequest` — до полного OpenAI-набора, новые поля **прокинуты** в `GenerationParams`, а не «приняты и забыты» |
> | **1.2** (потеря `options.*`) | Закрыто тем же: `options` обрабатывает Ollama-код cppworker (`generateOptions` — единый источник истины) |
> | **1.3** (фильтр удалял текст) | `stripLlamaCppServiceTokens` вырезает **только токены**, сохраняя окружающий текст; канонический список токенов с порядком «длинные раньше коротких»; убран лишний третий `\n` в SSE-кадре |
> | **1.5** (нет лимита тела) | `readRequestBodyLimited` на 5 inference-обработчиках, `LB_MAX_REQUEST_BODY_MB` (default 64), корректные 413 в формате Ollama и OpenAI |
> | **2.1** (метрики токенов) | `recordTokenUsage` вызывается на streaming-пути (нативный done-чанк + SSE usage-чанк + native non-stream), с защитой от двойного учёта |
> | **2.2** (маршрутизация) | `findModelOnLlamaCppBackend` читает `llamaMetrics` (раньше — никогда не заполняемый `metrics[id].LlamaCpp`) |
> | **2.3/2.4** (`/api/ps`, `/api/tags`) | Заполнены `digest`, `details`, `modified_at`, `size`; `expires_at` больше не «год 1»; «богатая запись побеждает» вместо «первый победил» |
> | **2.5** (camelCase/snake_case) | `pollLoadingProgress` читает camelCase (фактический формат) с fallback на snake_case |
> | **2.6** (`X-Model-Context-Warning`) | `forwardUpstreamWarningHeaders` пробрасывает warning/`Retry-After` во всех путях ответа |
> | **3.10/3.11** (tool_calls) | Depth-aware `scanBalancedJSON` вместо ленивой `\{.*?\}` (вложенные аргументы больше не теряются); `function.arguments` приводится к JSON-**строке** (`openai-python` вызывал `json.loads` на объект) |
> | **3.14/3.15** (`/api/show`, digest) | `/api/show` отдаёт реальный `context_size` + `<arch>.context_length`; `digest` — настоящий хеш, а не размер в байтах; `parameter_size` считается корректно (было `n_layers×n_embd`, дававшее 0.0001B для 7B) |
> | **§5** (падающие тесты) | `TruncateReason` (`break` на первом символе строки), `poll*`-тесты, `currentConfig` nil-deref (HTTP 500 на `/v1/chat/completions`), R65c-контракт stream-таймаута, гонка `saveNameHistory` (потеря alias'ов + флейк TempDir) |
> | **Разблокировка** | `cmd/cppworker` **впервые компилируется и проходит тесты** (две устаревшие сигнатуры блокировали весь пакет — поэтому дефекты и жили незамеченными) |
>
> **Второй заход (продолжение R65d):**
>
> | Раздел | Что сделано |
> |---|---|
> | **1.4** (auth) | `/api/v1/admin/autotune/config` и `/history` были **без** `AuthMiddleware` (literal-паттерн выигрывал у соседних защищённых) — `PUT` без токена менял `balancing.autoTune` и сохранял конфиг. Прокси `/api/v1/gguf/backends/` был публичным и передаёт любой путь в cppworker. Оба закрыты; список бэкендов оставлен публичным; токен к cppworker берётся из конфига бэкенда, а не из заголовка клиента |
> | **2.8** (настройки-заглушки) | `PUT /api/v1/cluster/config` молча игнорировал `modelReplication`/`rpcCoordinator`/`virtualModels`/`distInference` и 7 полей `llamaCpp` (+ `VirtualModelsConfig` негде было хранить `coordMode`/`timeout`, GET отдавал хардкод). Всё принимается и применяется; вложенные `groups`/`workers`/`models` (управляются отдельными endpoint'ами) сохраняются; `verifySync` на клиенте теперь ловит молчаливый откат |
> | **2.8** (секция `agent`) | Выяснилось, что реализовать нельзя: `types.AgentConfig` — конфиг отдельного процесса `cmd/agent`, у `LoadBalancerConfig` нет поля `Agent`. Поэтому секция **убрана из payload** вместо имитации управления |
> | **2.9** (backend CRUD) | `PUT /api/v1/backends/{id}` затирал `cppWorkerApiToken` (WebUI его не шлёт → 401 на reload) и `engine`; структура собиралась из запроса целиком, поэтому частичный PUT (кнопка авто-детекта порта) обнулял `host`/`name`/`labels`/лимиты. Введена merge-семантика; `UpdateBackend` теперь пишет и в `p.config.Backends` (**иначе правки терялись при FlushState→LoadState**); форма грузит `cppWorkerPort` |
> | **2.2** (virtual router streaming) | **Продакшен-баг:** `virtual_router.proxyToBackend` делал `defer cancel()`, поэтому контекст отменялся до чтения тела — стрим виртуальных моделей обрывался после первого чанка. Плюс `io.Copy` буферизовал SSE вместо построчного flush. Контекст теперь живёт до конца чтения тела, стриминг пишется построчно с `Flush` |
> | **WebUI WS** | Сервер не слал control-ping (только текстовый JSON), поэтому `/ws/metrics` рвался каждые 90 с, `/ws/logs` — каждые 120 с. Добавлен `WriteControl(PingMessage)` + продление deadline на любом входящем кадре |
> | **WebUI i18n** | Дописаны 9 отсутствующих ключей `gguf.*` (UI показывал сырые ключи) |
> | **Batched sampling** | Batched-режим применяет **только** `temperature`/`seed`; `top_p`/`top_k`/`min_p`/`repeat_penalty`/`mirostat*`/`stop` игнорировались **без единой записи в логе**. Добавлено WARN с перечнем проигнорированных параметров |
>
> **Третий заход — Фаза 5 + Фаза 6 (остаток):**
>
> | Раздел | Что сделано |
> |---|---|
> | **Фаза 5** (WebUI wiring) | `webui/js/modules/api.js` получил `backends()`/`backend(id)`; `enrichBackendsWithConfig` в `app.js` дотягивает конфиг бэкенда (weight/labels/maxModels/autoTune) в кластерный снапшот — до этого половина полей просто отсутствовала в UI; `fillForm` заполняет `formBackendCppWorkerPort` (без этого Save перезаписывал порт дефолтом `18092`) |
> | **Фаза 5** (proxy auth) | `gguf-api.js`/`gguf-load-progress.js` добавляют `?token=` к proxy-URL — `EventSource` не умеет слать заголовки, поэтому без этого SSE-прогресс отдавал 401; `cancelGeneration()`/`activeQueries()` перестали вызывать несуществующие методы |
> | **Фаза 5** (stub-настройки) | 7 полей `llamaCpp` (`flashAttnType`, `splitMode`, `idleUnloadMinutes`, `enableMetrics`, `metricsRetentionSeconds`, `enableReasoning`, `reasoningBudget`) и 4 секции конфига теперь реально принимаются и применяются сервером; `VirtualModelsConfig` хранит `coordMode`/`timeout` (GET раньше отдавал хардкод) |
> | **Фаза 6** (broker drops) | `metricsBroker` раньше **молча** терял метрики при переполнении канала клиента. Добавлены счётчики `brokerDrops`/`clientDrops` + throttled WARN (раз в 5 с), наружу — `BrokerStats()` |
> | **Фаза 6** (`user_id`) | Фолбэк-цепочка была `RemoteAddr` → `anonymous`, из-за чего все клиенты за реверс-прокси схлопывались в один `user_id` и упирались в общий `maxParallelPerUser`. Теперь `X-User-Id` → `X-Real-IP` → первый `X-Forwarded-For` → `RemoteAddr` → `anonymous` |
> | **Фаза 6** (`webui/dist`) | Устаревшая сборка помечена в `webui/dist/README.md` (не удалена: эвристика «бандл устарел» недостаточно надёжна, чтобы сносить файлы автоматически) |
> | **Фаза 6** (`countTokens`) | `countTokensSafe` в OpenAI-обработчиках вместо `len/4` по байтам |
> | **R65e** (флейки) | `Backend.Close()` не дожидался фоновой записи `.name_history.json` → `t.TempDir()` падал с «directory is not empty», а в production последний alias мог потеряться при рестарте; добавлен `WaitForPendingWrites`. Плюс два теста `health_test.go` требовали `Latency != 0`, что на Windows недостижимо (разрешение таймера ~0.5–15 мс, loopback-ответ укладывается в 0 тиков) — заменено на `>= 0` |
> | **R65e** (секрет в ответе) | `getBackend` в fallback-ветке отдавал `types.Backend` целиком, включая `cppWorkerApiToken`. Ветка сегодня практически недостижима (`GetClusterState` перечисляет все `p.backends`, `RemoveBackend` чистит оба хранилища), поэтому это **не подтверждённая утечка, а защита от оживления ветки**; правка + тест на наблюдаемый контракт |
>
> **Четвёртый заход — R66: закрытие четырёх оставшихся «долгов».**
>
> | Пункт | Итог |
> |---|---|
> | **`countTokens` для кириллицы** | **Реальный дефект, исправлен.** Эвристика `len(runes)/4` занижала счёт на **всех восьми** измеренных словарях: латиница на 63–81 %, кириллица до 304 %, и возвращала 0 для текста короче 4 символов. Занижение опасно: результат идёт в `n_predict` и в preflight-проверку `n_ctx`, то есть пропускает запрос за границу контекста. Заменено на `pkg/tokencount` — оценщик с коэффициентами, полученными **измерением**, а не подбором |
> | **Измерения** | Реальные словари `c/llama.cpp/models/ggml-vocab-*.gguf` разобраны напрямую (без сборки llama.cpp — она на Windows/MinGW невозможна из-за конфликта `_WIN32_WINNT`: `ggml-cpu` требует Win8−, `cpp-httplib` — Win10+). Инструменты: `scripts/vocab_stats_lib.js` (читатель GGUF), `scripts/vocab_byte_decoder.js` (GPT-2 byte-level ↔ unicode), `scripts/vocab_bpe_probe.js` (жадный BPE на образцах). Измеренная плотность: латиница 2.23–2.49 руны/токен, кириллица 1.00 (gpt-2) … 2.12 (gemma-4), CJK 1.02–1.33 |
> | **Ошибка в самом зонде** | Первая версия зонда давала мусор: byte-level словари хранят кириллицу в GPT-2 unicode-кодировке (`Ġ` вместо пробела), поэтому прямой поиск токенов давал ноль совпадений, и результат вырождался в «1 токен на символ» — выглядело как измерение, им не являясь. Исправлено дважды: (1) `buildByteToChar` клал в маппинг числа вместо строк, из-за чего `CHAR_TO_BYTE` был пуст; (2) вывод о byte-level делался по первому «непонятному» символу — теперь по результату декодирования |
> | **`BrokerStats()` в admin** | **Реальный пробел, закрыт.** `GET /api/v1/admin/metrics-broker/stats` (под `AuthMiddleware` — раскрывает число WS-подписчиков): `broker_drops`, `client_drops`, `total_drops`, `subscribers`, длины очередей, timestamp. До этого оператор видел только throttled WARN раз в 5 с и не мог оценить масштаб и рост потерь. `nil`-брокер отдаёт 503, а не 200 с нулями |
> | **`webui/dist`** | **Удалён** (`git rm`, 15 файлов, 10122 строки). Проверено перед удалением: `docker/webui/Dockerfile` копирует `webui/js/`, `webui/css/`, `webui/img/` — **не** `dist`; `go:embed` для WebUI в репозитории нет; ни один HTML/JS/тест/compose на `dist` не ссылается; `dist/index.html` грузит монолитный `dist/app.js` (70 КБ, апрель 2026), тогда как актуальный `webui/js/app.js` — 157 КБ (сентябрь). Файлы были в git, восстановимы |
> | **`num_ctx` в тело** | **Пробел мнимый — менять не стал.** Балансер ставит `X-Cpp-Ctx` во всех четырёх inference-обработчиках (`llamacpp_handlers_inference.go:145,365,665,791`), cppworker читает его во всех четырёх (`handlers_chat.go:207`, `handlers_generate.go:252`, `handlers_openai.go:414,1424`) через `ApplyCppCtxHeaderWithOptions`: если тело `num_ctx` не задало — заголовок становится значением по умолчанию; если задало больше — клампится. Экспорт в тело был бы **вреден**: (а) `ResolveNumCtx` предпочитает тело, поэтому запись туда отключила бы клампинг по профилю на уровне cppworker — то есть правка «для надёжности» сломала бы защиту VRAM; (б) нарушила бы зафиксированную гарантию echo-back из `ApplyCppCtxHeader` («bodyBuf не модифицируется, чтобы при echo-back клиент получил точно свой запрос») |
>
> **Проверка R66:** `go test -tags llama_stub ./internal/... ./pkg/... ./cmd/cppworker/` — **18/18 пакетов зелёные, 0 FAIL** (добавился пакет `pkg/tokencount`). `node webui/tests/test_webui_wiring_r65d.js` — 58/58; `test_gguf_api_headers.js` — 18/18.
> Обновлены тесты, фиксировавшие **старую** эвристику как ожидание: `TestEstimatePromptTokens_HundredChars` (25 → 50), `TestEstimatePromptTokens_Cyrillic` (25 → 100), хелперы `makeLongPrompt`/`makePromptOfLen` (4 → 2 символа на токен), три `TestClampNPredictToFitContext_*`.
>
> **Что осталось незакрытым (честно):** `b.metrics.RecordRequest` в `internal/cppbackend/backend.go:2089` пишет `tokens=0` для всех запросов, потому что `approxResultLen()` возвращает `""` (в стриме ответа ещё нет). Это существующее ограничение метрики бэкенда, не связанное с оценщиком; чтобы его закрыть, нужно прокинуть реальную длину ответа в deferred-блок. Не делал: вне заявленных четырёх пунктов.
>
> **Проверка:** `go test -tags llama_stub ./internal/... ./pkg/... ./cmd/cppworker/` — **17/17 пакетов зелёные, 0 FAIL, два полных прогона подряд** (после R66 — 18/18, см. ниже). Дополнительно оба флейк-пакета (`cmd/cppworker`, `internal/balancer`) прогнаны по 6 раз каждый — чисто.
> `node webui/tests/test_webui_wiring_r65d.js` — 58/58; `test_gguf_api_headers.js` — 18/18 (было 16/18).
>
> **Не сделано** (осознанно, см. §6): проброс `num_ctx` в тело — **закрыто как мнимый пробел в R66** (см. ниже: механизм `X-Cpp-Ctx` покрывает все 4 обработчика, запись в тело сломала бы клампинг по профилю).

**Метод.** Чтение исходников (`read`/`grep`/`glob`), сборка `go build -tags llama_stub ./internal/... ./pkg/... ./cmd/balancer/...`,
запуск тестов (`go test -tags llama_stub ./internal/... ./pkg/...`), и **эмпирический зонд**
через `setupRouter()` cppworker на stub-мосте: реальные HTTP-запросы к `/api/chat`,
`/api/generate`, `/v1/chat/completions` с разными наборами полей и фиксация HTTP-кода.
Docker в среде аудита не запущен, поэтому live-E2E по HTTP-портам не выполнялся —
всё, что помечено «подтверждено зондом», воспроизводимо тестом, а не рассуждением.

---

## 0. Фактическая карта запроса (важно для чтения отчёта)

```
клиент
  │  POST /api/chat | /api/generate | /api/embeddings   (Ollama native)
  ├────────────────────────────────────────────────────────────────────────┐
  │                                                                        │
  ▼                                                                        │
balancer :18080  Proxy.ServeHTTP (internal/balancer/proxy.go:540)          │
  │                                                                        │
  │ 1. parseRequestBody / disabled-profile check / recordRecentClient      │
  │ 2. routeRequest (internal/balancer/router.go:48)                       │
  │      • /api/v1/*           → reverse-proxy на API-сервер :18081        │
  │      • isUnsupported*      → ранний 404                                │
  │      • bt=="" && chat/gen  → false = падаем в основной flow            │
  │      • иначе dispatchRouters(buildRoutersForDispatch(bt))              │
  │                                                                        │
  ├─ (A) LlamaCppRouter.handleChat  (llamacpp_handlers_inference.go:548) ─┤
  │        → proxyRequestLlamaCpp (llamacpp_transport.go:28)               │
  │                                                                        │
  ├─ (B) LlamaCppRouter.handleOpenAIChatCompletions (:21) ────────────────┤
  │        → proxyRequestOpenAIStreaming / …Hijacked                       │
  │                                                                        │
  └─ (C) основной flow: selectBackend → proxyRequest (proxy_request.go:140)
           → isLlamaCppBackend → proxyRequestLlamaCpp / …NonStream         │
                                                                            │
  Во ВСЕХ трёх случаях тело уходит в cppworker так:                         │
    translatePathForLlamaCpp (llamacpp_translate_req.go:11)                 │
       /api/chat       → /v1/chat/completions                               │
       /api/generate   → /v1/completions                                    │
       /api/embeddings → /v1/embeddings                                     │
    translateOllamaBodyToOpenAI (llamacpp_translate_req.go:26)              │
  ▼                                                                        │
cppworker :18092  handleV1ChatCompletions (handlers_openai.go:216)         │
  │   types.DecodeJSONRequest → json.Decoder.DisallowUnknownFields          │
  │   (pkg/types/contract_validation.go:71)                                │
  ▼                                                                        │
SSE (OpenAI)  →  balancer  →  NDJSON/SSE (Ollama)  →  клиент  ─────────────┘
```

**Ключевой вывод, который переопределяет часть прошлых допущений:**
Ollama-native обработчики cppworker (`cmd/cppworker/handlers_chat.go`,
`handlers_generate.go`) **на пути балансера недостижимы** — балансер всегда
переписывает путь в `/v1/*`. Значит:

* Ollama-специфичные поля запроса (`options.*`, `keep_alive`, `format`, `images`, `think`)
  должны приниматься **OpenAI-обработчиками** cppworker, а они этого не делают (§1.1).
* Ollama-native обработчики cppworker содержат более полный разбор (`generateOptions`
  в `cmd/cppworker/types.go:11-30` знает top_k / repeat_penalty / seed / mirostat / …),
  но он не используется. Это не «мёртвый код, который можно удалить», а **готовый
  правильный слой**, к которому нужно маршрутизировать Ollama-трафик.

---

## 1. КРИТИЧНО

### 1.1 `DisallowUnknownFields` у cppworker убивает штатные Ollama-запросы (HTTP 400)

**Подтверждено зондом** (реальные POST'ы через `setupRouter()`):

| Тело запроса к `/api/chat` | HTTP | Ответ |
|---|---|---|
| `{model,messages,stream}` | 500 (stub) | — |
| `… ,"options":{"num_ctx":8192}` | **400** | `json: unknown field "options"` |
| `… ,"options":{"temperature":0.2}` | **400** | `unknown field "options"` |
| `… ,"options":{"top_p":0.9}` | **400** | `unknown field "options"` |
| `… ,"options":{"top_k":40}` | **400** | `unknown field "options"` |
| `… ,"options":{"repeat_penalty":1.1}` | **400** | `unknown field "options"` |
| `… ,"options":{"seed":42}` | **400** | `unknown field "options"` |
| `… ,"options":{"stop":["</s>"]}` | **400** | `unknown field "options"` |
| `… ,"keep_alive":"5m"` | **400** | `unknown field "keep_alive"` |
| `… ,"format":"json"` | **400** | `unknown field "format"` |
| `… ,"think":true` | **400** | `unknown field "think"` |
| `{"role":"user","content":"hi","images":["AAAA"]}` | **400** | `unknown field "images"` |
| «типичный OpenWebUI» (options+keep_alive) | **400** | `unknown field "options"` |

Причина: `chatRequest` (`cmd/cppworker/handlers_chat.go:34-42`) содержит только
`model / messages / stream / temperature / max_tokens / num_ctx / tools`,
а декодер — строгий (`types.DecodeJSONRequest` → `DisallowUnknownFields`,
`pkg/types/contract_validation.go:71`).

Для `/v1/chat/completions` тот же дефект шире — зонд показал 400 для
`top_k`, `repeat_penalty`, `num_predict`, `options`, `keep_alive`,
`response_format`, `parallel_tool_calls`, `frequency_penalty`,
`presence_penalty`, `logit_bias`, `user` и для мультимодального
`content: [{...}]`.

**Последствия:** любой клиент Ollama-режима (OpenWebUI, `ollama run`, ollama-python)
и любой OpenAI-клиент, отправляющий `frequency_penalty`/`response_format`/`user`,
получает 400 — не «потеряно поле», а отказ запроса.
Cline/Roo Code (tools, `temperature`, `max_tokens`, `stream_options`) проходят.

**Исправление — два шага:**
1. Быстрый: расширить `chatRequest` / `openAIChatCompletionRequest` /
   `openAICompletionRequest` до полного Ollama-набора (как `generateOptions`) и
   маппить их в `bridge.GenerationParams` так же, как `handlers_generate.go:130-189`.
2. Правильный: маршрутизировать Ollama-трафик на **нативные** `/api/chat`,
   `/api/generate`, `/api/embed`, `/api/embeddings` cppworker (они уже реализованы),
   т.е. убрать `translatePathForLlamaCpp` из Ollama-путей. Тогда Ollama-поля
   обрабатывает Ollama-код, а OpenAI-поля — OpenAI-код, и `DisallowUnknownFields`
   снова становится полезным сигналом, а не источником 400.

**Файлы:** `internal/balancer/llamacpp_translate_req.go:11-40,141-157`,
`cmd/cppworker/handlers_chat.go:34-42,65`, `cmd/cppworker/handlers_openai.go:66-86,224`,
`cmd/cppworker/types.go:11-65`, `pkg/types/contract_validation.go:71`.

### 1.2 `options.top_k` (и весь остальной Ollama-набор) молча теряется при трансляции

`translateOllamaChatToOpenAI` (`internal/balancer/llamacpp_translate_req.go:141-157`)
переносит из `options` только `temperature`, `top_p`, `top_k`, `num_predict`, `stop`.
Теряются: `repeat_penalty`, `repeat_last_n`, `seed`, `min_p`, `typical_p`, `tfs_z`,
`mirostat`, `mirostat_tau`, `mirostat_eta`, `num_keep`, `frequency_penalty`,
`presence_penalty`. Также теряются top-level `format`, `keep_alive`, `think`,
`system`, `template`, `raw`, `context`, `images`.

Худший вариант — `top_k`: он **переносится** в OpenAI-поле `top_k`, которого нет
в `openAIChatCompletionRequest` → 400 (см. §1.1). Т.е. поле одновременно и теряется,
и ломает запрос.

**Исправление:** либо полный маппинг (см. §1.1 п.1), либо маршрутизация на нативный
`/api/chat` (см. §1.1 п.2).

### 1.3 `shouldFilterLlamaCppContent` удаляет целые куски ответа

`internal/balancer/llamacpp_content_filter.go:129-167` считает чанк «служебным»,
если `strings.Contains(content, token)` для списка `<end_of_turn>`, `<eos>`, `<pad>`,
`<unk>`, `<sep>`, `<|im_start|>`, `<|end_of_text|>` и т.п. Далее
`filterOpenAIStreamingLine` (`:214-221`) **заменяет весь delta на `{}`** — текст
не вырезается по токену, а выбрасывается целиком.

Любой ответ, где эти строки упомянуты как текст (документация по chat-шаблонам,
разбор промптов, обсуждение спец-токенов, код парсеров), теряет слова и предложения.
Работает и на основном пути (`llamacpp_transport.go:720`), и в hijack-пути
(`proxy_request_hijack.go:327`).

**Исправление:** вычитать найденный токен из строки и отдавать остаток;
никогда не выбрасывать весь чанк из-за `Contains`.

### 1.4 `/api/v1/admin/autotune/config` и `/api/v1/gguf/backends/*` — без аутентификации

`internal/api/routes.go:173-177` регистрирует
`HandleFunc("/api/v1/admin/autotune/config", …)` **без** `AuthMiddleware`
(в отличие от соседних `/api/v1/admin/autotune` и `/api/v1/admin/autotune/`).
Literal-паттерн выигрывает у prefix-паттерна в `http.ServeMux`, поэтому
`PUT /api/v1/admin/autotune/config` меняет `balancing.autoTune` и per-model
`autoTune` **без токена** и сохраняет конфиг на диск.

`internal/api/routes.go:242-249`: `/api/v1/gguf/backends/` (`handleGgufBackendProxy`)
тоже без auth, и проксирует **произвольный** метод+путь в cppworker
(`gguf_backend_proxy.go:94`), включая `POST /api/models/load`, `/api/models/delete`,
`/api/hf/download`, `/api/cancel`.

**Исправление:** обернуть оба в `AuthMiddleware(RateLimitMiddleware(...))`;
токен к cppworker подставлять на стороне сервера, а не брать из заголовка клиента.

### 1.5 Нет лимита размера тела запроса на балансере при лимите 4 MB у cppworker

`pkg/types/contract_validation.go:106-108`: `MaxStreamingBodyBytes = 4 MB`,
используется в `handlers_chat.go:65`, `handlers_generate.go:293`,
`handlers_openai.go:224,1258`. Превышение → `ErrBodyTooLarge` → HTTP 413.

Балансер лимита не имеет и читает тело неограниченно (`io.ReadAll`,
`llamacpp_handlers_inference.go:30`) и **дважды** (bodyBuf + translatedBody).
Итог: длинная история с изображениями (base64) или очень большой tools-схемой
отвергается 413 без внятной диагностики, при этом балансер уже потратил память.

**Исправление:** `http.MaxBytesReader` на 5 inference-обработчиках + поднять лимит
cppworker для chat/completions (не 4 MB).

### 1.6 Фронтенд: `window.API` не существует — массовые операции моделей мертвы

`internal`-код не при чём, но это ломает управление из WebUI:

* `webui/js/modules/api.js:582-584` экспортирует `window.Api` (строчная).
* `webui/js/modules/bulk-models.js:253,277,287-288,298,304` использует `window.API`.
* `webui/js/modules/autotune_settings.js:74` — `window.API?.getAuthHeaders?.()`.
* `webui/js/modules/renderers.js:1826-1827` — `window.API.fetchGgufBackends`.
* `webui/js/modules/autotune_history.js:44-49` — `window.Api.authHeaders()`, а
  реально существует `getAuthHeaders()` (`api.js:568`).

`grep window\.API\s*=` по репозиторию — совпадений нет.

**Последствия:** «Load/Unload/Delete Selected» молча возвращаются (только
`console.error`), сохранение настроек AutoTune падает в 401/`catch`,
история AutoTune не открывается, AutoTune-карточки не рисуются.

**Исправление:** `window.API` → `window.Api`, `authHeaders` → `getAuthHeaders`.

---

## 2. ВЫСОКИЙ ПРИОРИТЕТ

### 2.1 Метрики токенов не собираются на streaming-пути
`recordTokenUsage` вызывается только в non-stream ветках
(`llamacpp_transport_nonstream.go:716`, `proxy_request.go:686,826`,
`proxy_request_hijack.go:155`). Основной streaming-цикл
(`llamacpp_transport.go:619-1030`) не вызывает его вообще, хотя
`translateUsageChunkToOllama` (`llamacpp_translate_resp.go:701`) эти данные
**имеет и отдаёт клиенту**. `GET /api/v1/stats/tokens` (`handlers_stats.go`)
вернёт нули/пусто для OpenWebUI/Cline, т.е. для 100 % реального трафика.
Комментарий в шапке файла («обновляется при каждом успешном response») неверен.

### 2.2 Маршрутизация на «холодный» бэкенд в мульти-бэкенд кластере
`findModelOnLlamaCppBackend` (`llamacpp_backend_helpers.go:75-96`) читает
`metricsMgr.metrics[id].LlamaCpp.LoadedModels`. Но `LlamaCpp.LoadedModels`
**никогда туда не пишется**: все записи идут в `metricsMgr.llamaMetrics`
(`llamacpp_metrics_poller.go:324`, `metrics_manager.go:180,228`,
`nctx_reload_handlers.go:884`, `proxy_request.go:1219`), а `metrics[id].LlamaCpp`
только **читается** (`cluster_state.go:110,167`).
Следствие: функция всегда возвращает `""`, и запрос уходит на
`selectAnyLlamaCppHealthy()` — на произвольный узел, а не на тот, где модель
загружена (перезагрузка модели на каждой смене узла).
`handlePS` (`llamacpp_handlers_readonly.go:425`) и `handleTags` (`:50`) читают
**правильный** кэш — т.е. в одном файле два разных источника истины.

### 2.3 `/api/ps` отдаёт `expires_at` = год 1
`internal/balancer/llamacpp_handlers_readonly.go:447`: `ExpiresAt: time.Time{}`
сериализуется как `"0001-01-01T00:00:00Z"`. Клиенты, читающие `expires_at`
(OpenWebUI, `ollama ps`), видят модель «протухшей». `Digest` тоже пуст.

### 2.4 `/api/tags` теряет `digest`, `details`, `modified_at`, `size`
`internal/balancer/llamacpp_handlers_readonly.go:54-62`: для загруженных моделей
создаётся запись `{Name, Model, Size:0}` без `Digest`/`ModifiedAt`/`Details`.
Поскольку ниже используется «первый победил» (`:55`, `:84`), более богатая
запись с диска **не перезаписывает** бедную. Ollama-клиенты, которые по
`details.family` / `parameter_size` / `digest` строят UI, получают пустоту.

### 2.5 Прогресс загрузки в кэше балансера всегда нулевой (camelCase vs snake_case)
cppworker `/api/models/load/progress` отдаёт camelCase
(`handlers_model.go:838-845`: `loadingStartedAt`, `loadingSizeBytes`, `elapsedMs`),
а поллер декодирует snake_case
(`llamacpp_metrics_poller.go:383-393`: `loading_started_at`, `loading_size_bytes`,
`elapsed_ms`) и даже комментирует «are all snake_case in cppworker» — это неверно.
Все поля → 0. WebUI-страница GGUF спасается тем, что ходит напрямую через
`/api/v1/gguf/backends/{id}/proxy/...` (camelCase), поэтому баг виден только
в `ClusterState`/мониторе и в логике `lastLoadingSeen`.

### 2.6 `X-Model-Context-Warning` и `Retry-After` не доходят до клиента
cppworker ставит заголовок (`nctx_clamp.go:356-379`), но балансер копирует в ответ
только `Content-Type`/`Content-Length`
(`llamacpp_transport.go:499-507`, `llamacpp_transport_nonstream.go:686-700`).
Только hijack-путь (`proxy_request_hijack.go:212-222`) переносит заголовки.
Клиент не узнаёт, что `n_predict` был урезан — тихая деградация ответа.

### 2.7 `num_ctx` может быть молча повышен
`ResolveNumCtx` (`num_ctx_resolver.go:352-370`) поднимает `num_ctx` до загруженного
и кладёт в заголовок `X-Cpp-Ctx`; в переведённом теле `num_ctx` **отсутствует**
(`translateOllamaChatToOpenAI` его не копирует), а `nctx_clamp.go:106-114` при
пустом теле берёт значение из заголовка. Клиент, попросивший 8192 против
загруженных 65536, получает 65536. Комментарий в `nctx_clamp.go:24-32`
утверждает обратное.

### 2.8 WebUI-настройки, которые отправляются, но не сохраняются
`webui/js/app.js:1211-1293` отправляет `agent`, `apiToken`, `modelReplication`,
`rpcCoordinator`, `virtualModels`, `distInference` и поля `llamaCpp`
(`flashAttnType`, `idleUnloadMinutes`, `enableMetrics`, `metricsRetentionSeconds`,
`enableReasoning`, `reasoningBudget`, `splitMode`). Обработчик PUT
(`internal/api/handlers_cluster.go:226-244`) декодирует только
`algorithm/modelAffinity/sessionStickiness/useEnhancedScoring/predictionFiltering/
gpu*Usage/cpu*Usage/ram*Usage/minFreeDisk/operatingMode/initialized/backendEngine/llamaCpp`,
а `types.LlamaCppConfig` не содержит перечисленных полей. `json.Decoder` молча
игнорирует лишнее, затем `verifySync` (`app.js:1303`) откатывает форму к серверным
значениям — при этом показывается тост «Settings saved».
Плюс `predictionFiltering` в GET всегда `true` (`handlers_cluster.go:191`),
т.е. тумблер в UI ничего не значит.

### 2.9 Редактирование бэкенда затирает `cppWorkerApiToken`
`internal/api/handlers_backends.go:679-702` строит `types.Backend` из запроса без
fallback на `existing.CppWorkerApiToken` (хотя для `cppWorkerPort`/`apiStyle`
fallback есть, `:658-677`), а UI токен не отправляет
(`webui/js/app-modals.js:201-206`). После любого редактирования бэкенда через
WebUI `POST /api/models/reload` из профилей начинает получать 401.
Частичный PUT из авто-детекта порта (`app-modals.js:213-228`) обнуляет ещё и
`host`/`name`/`weight`/`labels`/лимиты, делая бэкенд нероутируемым.
`fillForm` (`app-modals.js:122-162`) не заполняет `cppWorkerPort`, поэтому
сохранение формы перезапишет реальный порт (18091/18093) значением по умолчанию 18092.

### 2.10 WebUI не вызывает `GET /api/v1/backends` — половина полей бэкенда отсутствует
`data.backends` заполняется из `GET /api/v1/cluster` (`app.js:665-671`), т.е.
`[]types.BackendMetrics`, где нет `weight`, `labels`, `maxModels`, `autoTune`,
`apiStyle`, `gpuMode`. Их рендерят `renderers.js:203,839,841,899`,
`utils.js:37-40`. Эти поля есть только в `GET /api/v1/backends`
(`handlers_backends.go:189-214,303`), который фронтенд не запрашивает
(`api.js` умеет только POST create / PUT / DELETE).
Колонки Weight/Labels/MaxModels всегда показывают `1`/`-`/`-`, AutoTune-карточка
не рисуется, `labels`-based детект cloud-режима не работает.

### 2.11 WebSocket-каналы рвутся по таймауту
`handlers_core.go:130-135` ставит `SetReadDeadline(+90s)` и обновляет его **только**
в `SetPongHandler`, но сервер шлёт keep-alive **текстовым** кадром
(`handlers_core.go:212-220`), а не control-ping, поэтому браузер не отвечает
control-pong'ом; клиентский пинг (`websocket.js:107-113`) — тоже текст и deadline
не сбрасывает. `/ws/metrics` умирает каждые ~90 с, `/ws/logs` — каждые 120 с
(`handlers_logs_ws.go:77-82`, клиент вообще не шлёт пингов).
Симптом маскировался патчем nginx (`webui/nginx.conf:304-309`), причина — в Go.

### 2.12 Кнопка Cancel и busy-badge мертвы (имена методов не совпадают)
`gguf-renderer-actions.js:163-170` вызывает `api.cancelGeneration`,
`gguf-renderer-refresh.js:240-250` — `api.activeQueries`. В `GgufApi` таких
методов нет; в `api.js` они называются `cppworkerCancelGeneration.post`
(`:348-373`) и `cppworkerActiveQueries.get` (`:319-336`) — и **не вызываются
нигде**. Функциональность Round 32 (cancel из WebUI) фактически недоступна.

### 2.13 Единицы измерения диска и сети перепутаны
Агент отдаёт `diskTotal/diskUsed` в **MB** (`internal/agent/system.go:230-231`,
`pkg/types/metrics.go:93`), монитор форматирует их как байты
(`webui/js/monitor/ui-renderer.js:913-921`): диск 500 GB показывается как «500.0 KB».
Обратная ошибка для сети: агент отдаёт **байты** (`pkg/types/metrics.go:97-98`),
а основной UI форматирует как MB (`renderers.js:694-695`) — 1.8 MB превращается
в «1799.0 GB».

### 2.14 Названия событий WebSocket не совпадают
Бэкенд шлёт `eventType` = `backend_add|backend_remove|status_change|limits_change|
proxy_log|notification|metrics|reconfigure_request` (`pkg/types/events.go:8-22`,
`handlers_core.go:176-185`), фронтенд переключается по
`clusterState|backendAdd|backendRemove|statusChange|limitsChange|proxy_log`
(`app-listeners.js:198-231`). Совпадают только `clusterState`, `ping`, `proxy_log` —
остальные уходят в `default:` и теряются.

---

## 3. СРЕДНИЙ ПРИОРИТЕТ (выборка)

| # | Проблема | Доказательство |
|---|---|---|
| 3.1 | `window.API` → 404 несуществующих эндпоинтов, ошибки глотаются | `GET /api/v1/config` (`backend-type-filter.js:65`) — такого роута нет; правильный `/api/v1/cluster/config`. `POST /api/v1/queue/rebalance` (`monitor/init.js:59`) — роута нет → alert «404 Not Found» |
| 3.2 | `/api/models/load/progress/stream` (SSE) не имеет токена | `api.js:387-393` — `EventSource` без `?token=`, роут под `AuthMiddleware` (`routes.go:258`) → 401, тихий fallback на polling |
| 3.3 | Применение AutoTune вручную недоступно из UI | `POST /api/v1/admin/autotune/{id}/apply` есть (`handlers_autotune_admin.go:51-148`), UI не вызывает (`autotune.js:6` — TODO) |
| 3.4 | Per-model AutoTune список всегда пуст на здоровом кластере | `autotune_settings.js:37-50` берёт модели из `plan`, а `plan` появляется только при sub-optimal состоянии (`handlers_autotune_admin.go:247-255`) |
| 3.5 | Монитор ждёт `cluster.warmingUpModels` и `jobInfos`, которых нет в API | `monitor/ui-renderer.js:291,860-868`; `types/cluster_state.go:6-21`, `virtualmodel/router.go:96-117` |
| 3.6 | `POST /api/v1/backends/{id}/reconfigure` — заглушка | `handlers_backends.go:958-1006` публикует `EventReconfigure`, потребителя нет ни в `internal/`, ни в `cmd/` |
| 3.7 | Экспорт конфига из UI выгружает токен и несовместим с серверным форматом | `config-io.js:56-78` (`apiToken`), серверные `/api/v1/config/export|import` не вызываются |
| 3.8 | «Import configuration» — no-op из-за затенения переменной | `app.js:388-405`: во второй `.then` используется module-scope `data` вместо результата первого шага; плюс `autoSaveSettings` не выставлен на `window` (`config-io.js:400` vs `app.js:1342`) |
| 3.9 | `CountTokens`-эвристика отдаётся как точное `usage.prompt_tokens` | `handlers_openai.go:2077-2088`: `len(runes)/4`; для кириллицы ошибка ~2× |
| 3.10 | Аргументы tool_call парсятся ленивой регуляркой `\{.*?\}` → обрыв на вложенном `}` | `cmd/cppworker/tool_calls.go:428`, `internal/balancer/llamacpp_toolcall_detector.go:235` (корректный depth-aware вариант в тех же файлах есть) |
| 3.11 | Балансер оставляет `function.arguments` объектом, а не строкой | `llamacpp_toolcall_detector.go:198-208`; `openai-python` вызовет `json.loads(dict)` → `TypeError`. В cppworker для этого есть `stringifyArguments`, но балансер его обходит |
| 3.12 | Один и тот же preflight n_ctx выполняется дважды за OpenAI-запрос | `llamacpp_handlers_inference.go:81-90` и `:130-139` |
| 3.13 | `model_management.go:1055` выгружает без `?force=true` → 409 при занятой модели, 409 не пробрасывается | `handlers_model.go:903-925` |
| 3.14 | `/api/show` для незагруженной модели отдаёт `context_size: 0` | `handlers_model.go:2044-2082`; при этом `meta.ContextLength` заполнен (`cppbackend/model_manager.go:45`) |
| 3.15 | `/api/tags` от cppworker: `digest = fmt.Sprintf("sha256:%x", sizeBytes)` — не хеш, а размер; `quantization_level` всегда `"unknown"` | `handlers_model.go:1813,1836,1844,1890` (при этом `parseQuantization` существует) |
| 3.16 | Batched-режим теряет сэмплинг | `internal/cppbackend/batched_scheduler.go:455,489` — выживает только `temperature` |
| 3.17 | `user_id` всех клиентов схлопывается в один бакет | `cmd/cppworker/user_id.go:32-33` fallback на `RemoteAddr` = адрес балансера |
| 3.18 | `/api/embed` не в `translatePathForLlamaCpp` | уходит «как есть» без конвертации ответа (`llamacpp_translate_req.go:11-22`) |
| 3.19 | Fan-out drop в metrics-broker без счётчика | `internal/api/metrics_broker.go:93-108,120-129` |
| 3.20 | `Capacity %` в UI означает «загруженность» (инверсная семантика) | `renderers.js:239,273,307`; `pkg/types/prediction.go:13` |

---

## 4. Что проверено и работает корректно

Явно проверено, чтобы отчёт не читался как «всё сломано»:

* **Нет тихого обрезания через `bufio.Scanner` 64 KB.** Все продовые сканеры
  на wire-path ставят `Buffer`: `llamacpp_transport.go:576` (1 MB),
  `llamacpp_transport_nonstream.go:539` (1 MB),
  `proxy_request_openai_auto_stream.go:149` (4 MB),
  `rpccoordinator/worker_client.go:430` (1 MB).
* **Нет `io.LimitReader`/`MaxBytesReader`, обрезающих промпт или стрим** на
  inference-пути (все `LimitReader` — тела ошибок и HF-метаданные).
* **Flush после каждой записи** и на стороне балансера, и на стороне cppworker;
  границы чанков на passthrough-пути 1:1; hijack-путь пишет chunked-терминатор
  (`proxy_request_hijack.go:386`).
* **Ошибки и `Retry-After`** на 4xx/5xx: формы JSON
  (`{"error":…}` и `{code,bridge_info{…}}`) совпадают с `ParseCppWorkerError`
  поле-в-поле.
* **`created_at`** конвертируется из Unix-секунд в RFC3339
  (`llamacpp_translate_resp.go:311-347`).
* **`done:true`** выставляется для любого непустого `finish_reason`
  (`:943-959`), а не только для `stop`.
* **`num_ctx`** извлекается из `options.num_ctx` / top-level / float-декодирования
  (`num_ctx_resolver.go:46-91`).
* **Structured tool_calls с `thinking`**: Ollama-схема позволяет и `message.tool_calls`
  с `arguments` как объект, и `message.thinking`; `/api/generate` действительно
  имеет **top-level** `thinking` — проверено по официальной документации
  ([generate](https://docs.ollama.com/api/generate), [chat](https://docs.ollama.com/api/chat)).
  Эта часть конвертации корректна и совместима с клиентами.
* **Имена моделей без `.gguf`**: `ModelManager` (`cppbackend/model_manager.go:412-435`)
  умеет разрешать имя без расширения, `nameHistory` покрывает переименование файлов
  (`r60_61_persistent_name_history_test.go`).
* **CORS/preflight** (`proxy.go:561-575`), **correlation ID** и разделение
  `X-Request-Id` / `X-Upstream-Request-Id` (`proxy_request.go:392-403`),
  **снятие hop-by-hop заголовков** (`:386-388`), **`statusRecorder` с Flush/Hijack**
  (`proxy.go:1422-1437`), **явный `Content-Length` на JSON** (`:527-529`),
  **SSE-комментарий `: keepalive`** (`:740-742`), **`[DONE]` + flush**
  (`llamacpp_transport_helpers.go:154-163`), **отсутствие `WriteTimeout` на proxy-сервере**
  (`cmd/balancer/main.go:368`) — корректно.
* Незарегистрированных, но вызываемых эндпоинтов **нет**: все 22 пути
  balancer → cppworker резолвятся в `cmd/cppworker/router.go`.
* `/api/chat` и `/api/embeddings` присутствуют у cppworker (зонд: 400/405 — не 404);
  `/api/blobs/sha256:…` и `/api/me` — **404** (не реализованы; `ollama pull`/
  `create` через блобы работать не будут).

---

## 5. Отдельно: тесты, которые падали в baseline

> **Статус: закрыто в R65d/R65e.** Все перечисленные ниже падения разобраны — см. таблицы
> статуса в начале документа. Финальное состояние: `go test -tags llama_stub
> ./internal/... ./pkg/... ./cmd/cppworker/` — 17/17 пакетов зелёные, 0 FAIL (два прогона
> подряд + по 6 повторных прогонов для ранее флейкавших `internal/balancer` и
> `cmd/cppworker`). Раздел сохранён как запись baseline.

`go test -tags llama_stub -count=1 ./internal/... ./pkg/...`

```
--- FAIL: TestTruncateReason_MidLineCutoff_StillTriggers/trailing_open_paren
--- FAIL: TestTruncateReason_MidLineCutoff_StillTriggers/trailing_open_brace
--- FAIL: TestProxyRequestLlamaCpp_StreamTruncation_NoTruncationWhenDoneSent
--- FAIL: TestLoadBackoff_Snapshot
--- FAIL: TestProxy_LBStreamingNeverTimeout_Defaults
--- FAIL: TestProxy_LBStreamingNeverTimeout_DisabledExplicit
FAIL  ollama-loadbalancer/internal/balancer
--- FAIL: TestR60_57_LoadModelWithOpts_PreAllocatesHandle (TempDir cleanup)
FAIL  ollama-loadbalancer/internal/cppbackend
```

Два из них — реальные дефекты, а не флейки:

1. **`TruncateReason` почти всегда возвращает `""`**
   (`internal/balancer/auto_continue.go:216-222`): цикл `for _, ch := range lastLine`
   проверяет **первый** символ строки и сразу делает `break`.
   Для `"function("` первый символ `f` не из набора → `""`, хотя тест ожидает
   `mid_line_cutoff`. Значит детектор обрыва (и авто-продолжение, и метрика
   «ответ обрезан») срабатывает только когда строка **начинается** с `= ( { [ , :`.
   `repeat bug: switch { case ...: return ...; }` → `break` выходит из `switch`,
   а не из `for`... но здесь `break` стоит **после** `switch`, т.е. выходит из `for`
   на первой же итерации.

2. **Нет финального `done`-чанка, если upstream прислал `finish_reason` без usage**
   (`TestProxyRequestLlamaCpp_StreamTruncation_NoTruncationWhenDoneSent`,
   `llamacpp_transport_truncated_test.go:448` — тело ответа пустое).
   Ветка `if contentToolCallsProcessed || len(toolAccum) > 0`
   (`llamacpp_transport.go:948-969`) уходит в `writeStreamingSSEDone` **не** выставив
   `priorDoneEmitted`/`usageChunkSeen`; при последующем usage-чанке
   `translateUsageChunkToOllama` эмитит **второй** `done:true`
   (`llamacpp_translate_resp.go:636-638`), а если usage-чанка нет — клиент может
   не получить `done` вообще. Симптом в Cline/ollama-npm:
   «Did not receive done or success response in stream».

Тесты `cmd/cppworker` **не компилируются** с тегом `llama_stub`:
`abort_watcher_test.go:43` (передаёт `*bridge.ModelHandle` вместо `unsafe.Pointer`)
и `handlers_openai_load_abort_test.go:51` (`ensureModelLoaded` теперь возвращает
2 значения). Это значит, что весь пакет `cmd/cppworker` **не покрыт CI-прогоном** —
именно поэтому дефекты §1.1 не были пойманы.

---

## 6. Рекомендуемый порядок работ

> **Все пункты ниже закрыты в R65d/R65e** (P3 — последним: `cmd/cppworker` собирается и
> проходит тесты). Таблица сохранена как исходный план и карта «что чем закрывалось».

| Приоритет | Действие | Закрывает |
|---|---|---|
| P0 | Маршрутизировать Ollama-трафик на нативные `/api/chat`, `/api/generate`, `/api/embed`, `/api/embeddings` cppworker (или расширить OpenAI-структуры + полный маппинг) | 1.1, 1.2, 2.6, 2.7, 3.9, 3.18 |
| P0 | Обернуть `AuthMiddleware` `/api/v1/admin/autotune/config` и `/api/v1/gguf/backends/` | 1.4 |
| P0 | `window.API` → `window.Api`, `authHeaders` → `getAuthHeaders`, добавить `GgufApi.cancelGeneration`/`activeQueries` | 1.6, 2.12 |
| P1 | `shouldFilterLlamaCppContent`: вычитать токен, не выбрасывать чанк | 1.3 |
| P1 | Лимит тела на балансере + поднять лимит cppworker | 1.5 |
| P1 | Починить источник `LoadedModels` в `findModelOnLlamaCppBackend` | 2.2 |
| P1 | `priorDoneEmitted`/`usageChunkSeen` в tool-call-ветке; `TruncateReason` | §5 |
| P1 | `recordTokenUsage` в streaming-цикле | 2.1 |
| P2 | `expires_at`/`digest`/`details` в `/api/ps` и `/api/tags`; snake_case в поллере | 2.3, 2.4, 2.5 |
| P2 | Проброс `X-Model-Context-Warning` / `Retry-After` из upstream | 2.6 |
| P2 | Убрать `verifySync`-ложь: либо принимать поля в PUT, либо не отправлять их из UI | 2.8, 2.9 |
| P2 | WebUI: перейти на `GET /api/v1/backends`, починить WS-deadline'ы, единицы измерения, имена событий | 2.10–2.14 |
| P3 | Починить сборку тестов `cmd/cppworker` и вернуть пакет в CI | §5 |

---

## 6a. Что ещё стоит знать по WebUI (детали уровня «низкий»)

* `webui/dist/app.js` — устаревшая копия UI, на которую никто не ссылается
  (`index.html` грузит `js/...`). Если её случайно начнут отдавать, она обойдёт
  все исправления выше. Удалить или пометить как legacy.
* `GET /api/v1/backends/{id}` без метрик отдаёт `types.Backend` целиком, включая
  `cppWorkerApiToken` (`handlers_backends.go:346-364`, `pkg/types/backend.go:80`) —
  утечка секрета в ответе API.
* `notifications.js:216-225` вставляет `model`/`message` в `innerHTML` без
  экранирования (модель приходит из тела запроса клиента). Сейчас `render()`
  не вызывается (живой рендер — `app.js:2340-2346` с экранированием), но метод
  остаётся заряженным.
* `metrics_broker.go:93-108,120-129` молча дропает метрики при заполненном канале
  подписчика — без счётчика и лога.
* Кольцевой буфер SSE заполняется только пока клиент подключён
  (`handlers_events.go:196-198`), поэтому после тихого периода реконнект получает
  пустой snapshot.
* `/api/v1/events` пропускает только `EventNotification` (`:192`) — события
  AutoTune/метрик/статусов в SSE-«колокольчик» не попадают (они идут по WS,
  который теряет 4 типа, см. §2.14).
* `POST /api/v1/backends/{id}/reconfigure` отвечает «Agent will apply new settings»,
  хотя потребителя `EventReconfigure` нет ни в `internal/`, ни в `cmd/` (§3.6).
* API-эндпоинты без UI: `reset-reload-counter`, `balancer/load-backoff(/reset)`,
  `admin/cluster/autosuggest(/apply)`, `cluster/cppworker/debug/last-prompt`,
  `DELETE /sessions[/{id}]`, `GET /virtual-models/{name}` и `/infer`,
  `GET /models`, `/launch-config`, `/evacuate`.

---

## 7. Приложение: как воспроизвести ключевые находки

```powershell
# 1. Сборка и юнит-тесты (без cgo)
go build -tags llama_stub ./internal/... ./pkg/... ./cmd/balancer/...
go test  -tags llama_stub -count=1 ./internal/balancer/ -run 'TestProxyRequestLlamaCpp_StreamTruncation|TestTruncateReason'

# 2. Эмпирическая проверка §1.1 — временный тест в cmd/cppworker
#    (использует setupRouter() из main_ollama_api_test.go и stub-мост):
#    POST /api/chat с {"options":{...}}  -> HTTP 400 unknown field "options"
#    POST /api/chat с {"keep_alive":"5m"} -> HTTP 400 unknown field "keep_alive"
#    POST /v1/chat/completions с {"top_k":40} -> HTTP 400 unknown field "top_k"
#    Для запуска нужно временно убрать два некомпилирующихся теста (см. §5).

# 3. Проверка «мёртвого» кэша §2.2
grep -rn "LlamaCpp.LoadedModels =" internal/    # пусто
grep -rn "lm.LoadedModels =" internal/          # только llamaMetrics

# 4. Проверка фронтенда §1.6
grep -rn "window\.API" webui/js/                # 10 совпадений, ни одного присваивания
grep -rn "window\.Api =" webui/js/              # только api.js:583
```
