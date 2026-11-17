# Фундамент этапов 1 и 5: где сейчас принимаются решения о запросе

**Дата:** 2026-11-16 · **Статус:** инвентаризация по коду, правок нет ·
**Основание:** `docs/architecture/refactoring-plan.ru.md` (этапы 1 и 5, долг 12
«два мозга на один JSON») · **Метод:** чтение кода + подсчёт мест вызовов
(числа — по `internal/balancer`, без `_test.go`, если не сказано иное)

Документ отвечает на три вопроса: что уже сведено (и переделывать не надо), где
решения дублируются сейчас, и какой минимальный `requestPlanR493` их сводит —
с порядком миграции и рисками. Отдельный раздел — этап 5 (транспорт).

---

## 1. Что уже сведено (фундамент есть, его надо расширять, а не строить заново)

| Механизм | Где | Что даёт |
|---|---|---|
| `requestSurface` (`surfaceLegacy`=18080, `surfaceOpenAI`=18079) | `request_surface.go:23-30` | один `*Proxy` обслуживает два порта с разной семантикой; поверхность едет в контексте (`withRequestSurface`, `:46`) |
| `streamFlagFromBodyR484` — один разбор поля `stream` | `stream_flag_r484.go:32` | разбор ровно один; при отсутствии поля — дефолт поверхности (`streamDefaultForSurfaceR484`, `stream_flag_r484.go:47`: legacy → `true`, OpenAI → `false`) |
| `parsedRequest` — уже разобранное тело в контексте | `proxy.go:2157-2172`, `withParsedRequest` (`request_surface.go:67`), `parsedRequestOf` (`:75`) | поля `Model`, `Stream`, `RawBody`, `TooLarge`, `NumCtx`; «одно чтение тела» закреплено `body_single_read_r493_test.go` |
| `requestModelHint` | `request_surface.go:90` | модель из уже разобранного тела, без повторного парсинга |
| Разбор `num_ctx` из тела | `num_ctx_resolver.go:50` `ExtractNumCtxFromBody` | понимает `options.num_ctx` (Ollama) и верхнеуровневый `num_ctx` (OpenAI/прочее) |
| Один сток кадра + одно решение по строке SSE | `stream_line_plan_r493.go`, `stream_openai_sink_r493.go` | этап 6 (не этап 1) |

**Вывод.** Форма «решение в контексте» уже существует (`parsedRequest`), но в ней
только 2 из 4 решений этапа 1: **стрим** и **окно** лежат там, а **лимиты вывода**
и **идентичность модели/квантизации** — нет. Этап 1 — это достройка
`parsedRequest` до полного плана, а не новый слой.

---

## 2. Инвентаризация по поверхностям

Легенда: (а) стрим? (б) модель/квантизация (в) лимиты вывода (г) окно/num_ctx.
«Путь» — как запрос попадает в обработчик: маршрутизация — `router.go:125`
(`dispatchRouters`), дальше `llamacpp_router.go:73` (`Route`).

| Поверхность (точка входа) | (а) стрим | (б) модель | (в) лимиты | (г) окно | Чем отличается от других |
|---|---|---|---|---|---|
| `/api/chat` → llama_cpp-роутер → `handleChat` (`llamacpp_router.go:128` → `llamacpp_handlers_inference.go:842`) | `isStreamingFromBody` (`:1042`), дефолт `true` на legacy | типизированное `req.Model` в теле запроса | `options.num_predict` **не читается** для решения, уходит как есть | `ResolveNumCtx` (`:961`) + `ApplyCppCtxHeader` (`:996`) | handler-путь: 3-tier профиль/backend поверх тела; на generic-пути `/api/chat` часть решений другая (см. строку ниже) |
| `/api/chat` → generic-путь (`proxy.go` ServeHTTP → `proxyRequest*`) | `parsedRequest.Stream` (`parsed.Stream`, 2 чтения) | `parsed.Model` (`proxy.go:1101`) | pass-through | `parsed.NumCtx` (`proxy.go:1301,1302,1306,1358,1414,1437`) + `decideWindowR493` | здесь решения приходят из `parsedRequest`; на handler-пути — из typed-структуры. Один и тот же путь `/api/chat` имеет ДВА набора решений в зависимости от типа бэкенда |
| `/v1/chat/completions` → `handleOpenAIChatCompletions` (`llamacpp_handlers_inference.go:23`) | **не по телу**: транспорт выбирает сток по типу ответа upstream (`stream_openai_sink_r493.go:240-244`: SSE/NDJSON → стрим, иначе `writeNonSSEStreamResponseR493`). Клиентское поле читается только для **авто-стрима R258** (`:326-327`) | typed `req.Model` | `req.MaxTokens` / `options.num_predict` — pass-through | `ResolveNumCtx` (`:185`), затем `ApplyCppCtxHeader` (`:262`) | единственная поверхность с авто-стримом; на 18079 дефолт стрима `false`, на 18080 — `true` |
| `/api/generate` → `handleGenerate` (`llamacpp_router.go:131` → `:1066`) | `isStreamingFromBody` (`:1216`) | typed `req.Model` | `options.num_predict` pass-through | `ResolveNumCtx` (`:1157`) + `ApplyCppCtxHeader` (`:1188`) | нет tools-ветки, нет `think`/reasoning-полей; формат ответа NDJSON |
| `/v1/completions` → `handleOpenAICompletion` (`:451`) | **не по телу**: тот же сток по типу ответа upstream (`proxyRequestOpenAIStreaming`, `:693`); `isStreamingFromBody` в этом обработчике НЕ вызывается вовсе | typed `req.Model` | `max_tokens` pass-through | `ResolveNumCtx` (`:547`), `ApplyCppCtxHeader` (`:602`) | копия пайплайна chat-обработчика (см. §3); авто-стрима нет |
| Anthropic `/v1/messages` → `handleAnthropicMessages` (`anthropic_r247.go:771`) | `req.Stream` (`:75`) | `model` из Anthropic-тела | `req.MaxTokens` (`:71`) → `max_tokens` (`:226`) | **нет своего решения**: тело переводится в OpenAI и уходит self-proxy в `/v1/chat/completions` (`:795`) | адаптер: решения об окне принимает уже chat-поверхность; стрим переводится в OpenAI-SSE и обратно (`anthropicStreamForwarder`, `:481`) |
| Responses `/v1/responses` → адаптер (`responses_r247.go`) | `req.Stream` (`:62`) | `model` | `req.MaxOutputTokens` (`:59`) → `max_tokens` (`:211-212`) | **нет своего решения** (self-proxy в chat) | второе имя лимита (`max_output_tokens`), которое переводится в `max_tokens` |
| Авто-стрим R258 (`llamacpp_handlers_inference.go:326`) | решение НЕ клиента, а сервера: превратить `stream:false` в upstream-стрим | — | — | — | единственное серверное решение о стриме; включается env `LB_OPENAI_AUTO_STREAM` (по умолчанию off) |

### По колонкам: где именно «одно решение» уже есть, а где нет

- **(а) стрим.** Один разбор (`streamFlagFromBodyR484`) и один дефолт по
  поверхности (`streamDefaultForSurfaceR484`) — это **сведено**. Но:
  `isStreamingFromBody` вызывается **14 раз** (каждый раз перечитывая тело),
  а `parsedRequest.Stream` читают **всего 2 места** — то есть решение уже
  посчитано в контексте, но его обходят.
- **(б) модель.** Одно извлечение в `parsedRequest.Model`, но handler-путь
  работает с типизированной структурой своего протокола. Квантизация **нигде не
  участвует в решении**: она приходит из `/api/models` бэкенда
  (`llamacpp_handlers_readonly.go:97` и далее) и используется только для
  отображения и autotune (`autotune.go:91`). Это важно: «модель/квантизация» в
  этапе 1 — про идентичность модели (имя → какую копию брать), а не про разбор
  файла.
- **(в) лимиты.** **Ни одна поверхность не решает лимит сама** — все передают
  клиентское значение как есть. Решение живёт в двух других местах:
  (1) балансер читает лимиты только чтобы посчитать **окно**
  (`preflight_nctx.go:197,205,219,223`; `agentOutputBudget`,
  `preflight_agent_window_r253.go:85`; резервы `:38`), и (2) **воркер**
  окончательно урезает `n_predict` по свободному окну (`n_predict_budget_r463.go`,
  а с R493 — и `reasoningBudget`, `tools_stream_reasoning_r404.go`).
  Отдельный случай — авто-продолжение: у него свой лимит
  (`auto_continue.go:963,967,1234-1246`).
- **(г) окно.** Две реализации одного и того же чтения: `parsedRequest.NumCtx`
  (`proxy.go:2229` — внутри зовёт `ExtractNumCtxFromBody`) и **17 прямых
  вызовов `ExtractNumCtxFromBody`** по коду. Логика одна, но тело парсится
  повторно; плюс `ResolveNumCtx` (7 вызовов) добавляет 3-tier профиль/backend.

---

## 3. Одинаковые по смыслу решения, продублированные в нескольких местах

| Решение | Копии | Число | Риск |
|---|---|---|---|
| «это стрим?» | `streamFlagFromBodyR484` (+ обёртка `isStreamingFromBody`) зовётся напрямую: `admission_queue.go:751`, `image_tool_loop.go:61`, `image_tool_loop_ollama.go:78`, `llamacpp_handlers_inference.go:326,1042,1216`, `llamacpp_transport.go:100` и др. | **14 вызовов**, при этом `parsedRequest.Stream` читают 2 | дефолт поверхности можно обойти, вызвав обёртку без поверхности; при новом пути легко забыть про авто-стрим |
| «какое окно просил клиент» | `ExtractNumCtxFromBody` — `parsedRequest` уже содержит результат | **17 вызовов** + 10 в тестах | повторный `json.Unmarshal` на каждом решении; источник правды разъезжается, если формат тела изменится в одном месте |
| пайплайн «выбрать слот → дождаться → окно → авто-загрузка → Cpp-Ctx» | `llamacpp_handlers_inference.go`: `117/140/185/204-206/262` (chat), `482/502/547/566-568/602` (completions), `736/756` (embeddings — только выбор+ожидание), `912/921/961/963/996` (Ollama chat), `1157/1188` (Ollama generate) | **5 копий** (4 инференсных + embeddings-путь) | правка в одной копии не доезжает до остальных (уже случалось: R127/окно автозагрузки правили в одном месте) |
| «какой лимит вывода у клиента» | `preflight_nctx.go:197-223` (окно), `auto_continue.go:1234-1246` (продолжение), воркер `n_predict_budget_r463.go` (финальный кламп) | **3 места** с разной семантикой | лимит читается тремя разными структурами; поле `max_output_tokens` (Responses) переводится в `max_tokens` только в адаптере |
| «это image-запрос» | `countImagePartsInBody` (`llamacpp_handlers_inference.go:327`), префиксы модели (`request_surface.go:100`), пути (`:116`) | 3 признака | авто-стрим выключается отдельной проверкой, а не общим планом |

| «идём ли в стрим по ответу upstream» | `stream_openai_sink_r493.go:215` (hijack-заголовки) и `:240-244` (решение SSE vs не-SSE) | 2 проверки одного и того же типа ответа | расхождение даст SSE-заголовки на JSON-ответе (клиент ждёт `data:`, получает `{`) |

**Метрика для ориентира:** в `internal/balancer` (без тестов) **187** вызовов
`json.Unmarshal` — то есть «своё чтение тела» это не исключение, а норма. План
не обязан убрать все, но решения о запросе должны брать данные из одного места.

---

## 4. Минимальный дизайн `requestPlanR493`

**Идея.** Один раз собранный план на запрос, положенный в контекст; ниже по коду
никто не пересчитывает. Собирается там, где тело уже прочитано и разобрано
(`parseRequestBody` / handler-путь после `decodeClientJSON`), а не в 14 точках.

```go
// requestPlanR493 — ЧТО решено о запросе. Только факты, без бэкенда:
// на момент сборки ещё неизвестно, какой узел выбран.
type requestPlanR493 struct {
    // (а) стрим
    Surface      requestSurface // 18080 / 18079 — источник дефолта
    Path         string         // /api/chat, /v1/chat/completions, ...
    ClientStream bool           // что прислал клиент (явно или по дефолту поверхности)
    AutoStream   bool           // R258: серверное решение отдать upstream-стрим
    StreamUp     bool           // итог: идём в backend стримом?
    // (б) модель
    Model        string         // идентичность модели (без .gguf)
    ModelHintSrc string         // "body" | "alias" | "default" — откуда имя
    // (в) лимиты вывода
    ClientLimit  int            // max_tokens / max_output_tokens / options.num_predict
    LimitSrc     string         // "max_tokens" | "max_output_tokens" | "num_predict" | ""
    OutputBudget int            // сколько реально просим (0 = пусть воркер урежет)
    // (г) окно
    RequestedNumCtx int         // что просил клиент (0 = не просил)
    CtxSrc          string      // "body" | "profile" | "backend"
    // служебное
    RawBody []byte              // для проксирования как есть
    TooLarge bool
}
```

**Точка сборки — одна функция на поверхность-семейство, не одна на протокол:**

```go
func buildRequestPlanR493(r *http.Request, surface requestSurface, body []byte,
    typed any) *requestPlanR493
```

- `typed` — уже декодированное тело своего протокола (handler-путь) или `nil`
  (generic-путь, где есть только `parsedRequest`).
- Внутри: `streamFlagFromBodyR484` → применить дефолт поверхности → проверить
  авто-стрим только для OpenAI-chat → прочитать лимит с приоритетом
  `max_tokens > max_output_tokens > options.num_predict` → прочитать `num_ctx`
  **один раз**.
- Кладётся в контекст: `withRequestPlanR493(r, plan)`; читатели —
  `requestPlanOf(r)`.

**Кто читает (границы):**

| Читатель | Что берёт | Чего НЕ делает |
|---|---|---|
| `proxy.go` (generic), `handleChat/handleGenerate/handleOpenAIChatCompletions/handleOpenAICompletion` | `StreamUp`, `Model`, `RequestedNumCtx` | не решает стрим заново |
| `ResolveNumCtx` | `RequestedNumCtx` (+3-tier профиль) | не парсит тело |
| `preflight_nctx` / `agentOutputBudget` | `OutputBudget`, `RequestedNumCtx` | не читает `num_predict` из тела |
| `auto_continue` | `ClientLimit` | не заводит свой разбор |
| **воркер (cppworker)** | — | остаётся последней инстанцией по `n_predict` (R463): план говорит «сколько просили», воркер — «сколько влезло» |

**Порядок миграции (по возрастанию риска):**

1. **`/v1/chat/completions`** (первым). Почему: у него больше всего
   «серверных» решений (авто-стрим R258, 3-tier окно, Cpp-Ctx заголовок) и
   большинство живых клиентов; все эти решения уже в одном файле, значит
   `buildRequestPlanR493` пишется рядом и проверяется на существующих тестах
   (`auto_stream_parity_r493_test.go`, `stream_flag_r484_test.go:47`,
   `image_router_r_image_test.go:323`).
2. **`/api/generate` + `/api/chat` (handler-путь)** — там свои копии пайплайна
   (`912/921/961/963/996` и `1157/1188`); переносить по одной, начиная с
   `handleGenerate` (проще: нет tools).
3. **`/v1/completions`** — копия chat-обработчика, миграция механическая.
4. **Адаптеры Anthropic/Responses** — им нужно только поле `LimitSrc`
   (перевод имени лимита) и `StreamUp`; решения об окне у них не было и не
   должно появиться.
5. **Generic-путь `/api/chat`** — последним: он уже читает `parsedRequest`, и
   перевод на план — это «добавить поля», а не переписать.

**Риски:**

| Риск | Проявление | Чем закрыть |
|---|---|---|
| Два источника правды на время миграции | часть путей читает план, часть — тело; расхождение видно только на конкретном клиенте | страж «нет прямых `isStreamingFromBody`/`ExtractNumCtxFromBody` в обработчиках» (§5) вводится сразу, с allowlist на неготовые файлы, который уменьшается по шагам |
| Лимит клиента ≠ бюджет | план передаст `ClientLimit`, а воркер урежет — клиент увидит `done_reason=length` без объяснения | в плане явно два поля (`ClientLimit`, `OutputBudget`); воркер обязан писать в сводку, что урезал (`budget_tokens` уже есть) |
| Окно: profile vs body | 3-tier резолвер (`ResolveNumCtx`) остаётся снаружи плана; если план начнёт применять профиль, `primary`-модели получат чужое окно | в плане только «что просил клиент»; резолв профиля — отдельный шаг после выбора бэкенда |
| `Surface` по контексту (а не аргументом) | запрос, собранный в тесте или внутренним self-proxy, получит `surfaceLegacy` и `stream=true` по умолчанию | `buildRequestPlanR493` принимает `surface` явно; internal self-proxy (Anthropic/Responses, image-tool-loop) обязан передавать её из входящего запроса |
| Авто-стрим для картинок | включение R258 сломает vision (уже было: `400 vision_stream_not_supported`) | правило «image → не авто-стрим» переносится в план как поле/условие, и покрывается существующим тестом `image_router_r_image_test.go:323` |

---

## 5. Что уже фиксирует паритет и какой страж добавить

**Существующие тесты, на которые можно опереться:**

| Тест | Что фиксирует |
|---|---|
| `stream_flag_r484_test.go:47` `TestR484_SurfaceDefaults` | дефолт стрима по поверхности |
| `image_router_r_image_test.go:323` `TestParseRequestBody_StreamDefaultDependsOnSurface` | тот же дефолт через `parsedRequest` |
| `auto_stream_parity_r493_test.go:55,78,128` | байты обычного стрима не меняются, tool_call получает `index`, error-кадр закрывает поток |
| `one_inference_r493_test.go:135` `TestR493q_ParityWithHandlerPath` | паритет фич R86 между generic- и handler-путём |
| `body_single_read_r493_test.go:155` | тело читается ≤1 раза, конверт 413 по поверхности |
| `stream_line_plan_r493_test.go`, `stream_single_write_steward_r493_test.go` | одно решение по строке SSE, одна точка записи (этап 6) |
| `route_matrix_t1_test.go:185`, `r479_type_parity_test.go:26` | тип бэкенда и алиасы по поверхностям |
| `writer_payload_matrix_r219_test.go` (cppworker) | контракт кадров писателей ответа |

**Предлагаемый страж (в духе серии R493 — проверка исходника, а не поведения):**

`TestR493_RequestDecisionsOnlyInPlan`:
1. в файлах-обработчиках (`llamacpp_handlers_inference.go`, `proxy*.go`,
   `llamacpp_transport.go`, `image_tool_loop*.go`, `anthropic_r247.go`,
   `responses_r247.go`, `admission_queue.go`) **нет** прямых вызовов
   `ExtractNumCtxFromBody(` и `json.Unmarshal(` для чтения `"stream"`/`"num_ctx"`/
   `"max_tokens"` — только `requestPlanOf(r)`;
2. `isStreamingFromBody(` разрешён только там, где план недоступен (например,
   self-proxy до сборки плана) — с явным allowlist и счётом (14 → 0);
3. `buildRequestPlanR493` вызывается в каждой точке входа (список поверхностей
   из §2) — новая поверхность без сборки плана роняет тест;
4. как только файл мигрирован, он **удаляется из allowlist** (правило «список
   только уменьшается»), иначе страж превращается в вечный компромисс.

Дополнительно — тест-таблица «одинаковые тела на всех поверхностях дают
одинаковый план»: одно тело (`stream`, `max_tokens`, `num_ctx`, `model`) через
`buildRequestPlanR493` для каждой поверхности; различия только в ожидаемых
`Surface`, `LimitSrc` и `AutoStream`.

---

## 6. Этап 5: единый `runInference()`

### 6.1 Где транспорт дублируется

| Что | Где | Дублирование |
|---|---|---|
| SSE → NDJSON (Ollama-форма) | `llamacpp_transport.go` (основной), `stream_openai_sse_r493.go` (цикл v1), `auto_stream` (`proxy_request_openai_auto_stream_r258.go`), адаптеры (`anthropic_r247.go:481+`, `responses_r247.go:448`) | 4 цикла чтения SSE; каждый по-своему склеивает строки и решает про терминатор |
| non-stream | `llamacpp_transport_nonstream.go`, `proxy_request.go` (ветка non-2xx/буферизованная), handler-путь `writeBufferedChatResponse` (`image_tool_loop.go:392`) | 3 реализации «прочитать ответ целиком и отдать в формате поверхности» |
| `stats_windows` (сводка окон в `done`-кадре) | воркер считает (`stats_windows` в NDJSON), балансер прокидывает; на handler-пути — тот же воркер | одна реализация (воркер), но **два места**, где её форма меняется: NDJSON и OpenAI-адаптеры (`translate*`) |
| таймауты стрима | `getEffectiveStreamTimeoutSec` (`llamacpp_transport.go:2641`), отключение на hijack-пути (`stream_openai_sink_r493.go`) | 2 набора правил: один по `isStreaming`, второй по «hijacked или нет» |
| запись клиенту | `stream_line_plan_r493.go` (решение по строке), сток (`stream_openai_sink_r493.go`), `safeStreamWriter` в воркере | сведено в этапе 6 (по стокам), но **сводки** и **время жизни** остались у путей |

### 6.2 Что реально протокол, а что история

| Различие | Протокол? | Почему |
|---|---|---|
| Формат кадра (SSE против NDJSON) | **протокол** | клиенту обещан разный формат; это остаётся адаптером |
| Имена полей (`max_tokens`/`max_output_tokens`/`options.num_predict`, `num_ctx` против `options.num_ctx`) | **протокол** | перевод имени — работа адаптера, а не общего кода |
| Коды ошибок и конверт (`error.message` против строки) | **протокол** | `body_limit_r479.go` уже разводит по поверхности |
| Наличие tools/reasoning в протоколе | **протокол** | Anthropic/Responses не имеют `options.num_predict` и `think` |
| Дефолт стрима (legacy `true` vs OpenAI `false`) | **история** | R484 зафиксировал как совместимость; в плане это поле `Surface`, а не два кода |
| Авто-стрим R258 (серверный обход для старых клиентов) | **история** | временный обход; должен быть полем плана и выключателем, а не отдельной веткой в обработчике |
| 3-tier `num_ctx` (профиль/backend/тело) | **история+политика** | к протоколу не относится; место — после выбора бэкенда, одинаково для всех |
| Четыре копии пайплайна «слот→загрузка→окно→Cpp-Ctx» | **история** | чистый дубль; это и есть предмет этапа 5 |

### 6.3 Что должно войти в общий `runInference()`

1. Поиск/выбор слота и ожидание бэкенда (`selectForInferenceR403`,
   `waitForLlamaCppBackendR403`) — сейчас 4 копии.
2. Резолв окна (`ResolveNumCtx`) и применение Cpp-Ctx заголовка
   (`ApplyCppCtxHeader`) — сейчас 4 копии.
3. Авто-загрузка модели с failover (`ensureModelLoadedWithFailover`) — 4 копии.
4. Один вызов транспорта с описанием «куда писать» (сток уже есть из этапа 6).
5. Один блок «сводка хода» (поля, которые handler-путь пишет в лог: модель,
   n_predict, окно, `first_content_ms`) — сейчас собирается в нескольких местах.

### 6.4 Что остаётся адаптером (не входит и не должно)

- Разбор/рендер своего протокола: Anthropic (`anthropic_r247.go`), Responses
  (`responses_r247.go`), Ollama-переводы (`llamacpp_translate_req.go`).
- Формат кадра и терминатора (**этап 6** уже дал одно решение по строке; здесь
  остаются только обёртки вокруг него).
- Image-tool-loop (`image_tool_loop*.go`) — свой цикл с инструментами картинок;
  он должен звать `runInference`, а не копировать пайплайн.

### 6.5 Порядок (этап 5 после этапа 1)

1. `runInference` собирается на **уже существующих** функциях (не переписывая
   транспорт) и подключается к `handleGenerate` (проще всего: нет tools).
2. Затем `/api/chat`, `/v1/completions`, `/v1/chat/completions`.
3. Адаптеры Anthropic/Responses — в последнюю очередь и только через self-proxy
   в chat (как сейчас), чтобы не дублировать решение окна.

---

## 7. Что установлено (для строк плана)

1. **Основа уже есть:** `requestSurface` + `streamFlagFromBodyR484` +
   `parsedRequest` (`proxy.go:2157`) + одно чтение тела. Этап 1 — достройка
   `parsedRequest` до полного плана, а не новый слой.
2. **Стрим сведён в разборе, но не в использовании:** один парсер, но
   `isStreamingFromBody` — 14 вызовов, а `parsedRequest.Stream` читают 2.
3. **Окно читается дважды:** `parsedRequest.NumCtx` (`proxy.go:2229`) и
   17 прямых вызовов `ExtractNumCtxFromBody` (плюс 10 в тестах).
4. **Лимиты не решает ни одна поверхность:** они читаются в 3 местах с разной
   семантикой (`preflight_nctx.go:197-223`, `auto_continue.go:1234-1246`,
   воркер `n_predict_budget_r463.go`); поле `max_output_tokens` переводится в
   `max_tokens` только в адаптере Responses (`responses_r247.go:211`).
5. **Пайплайн «слот→окно→загрузка→Cpp-Ctx» продублирован 5 раз** —
   `llamacpp_handlers_inference.go`: 117/140/185/204/262 (chat),
   482/502/547/566/602 (completions), 736/756 (embeddings: только выбор+ожидание),
   912/921/961/963/996 (Ollama chat), 1157/1188 (Ollama generate). Это и есть
   основная работа этапа 5.
6. **Один и тот же путь бывает двух путей:** `/api/chat` обслуживается
   handler-путём (`llamacpp_router.go:128`) и generic-путём (`proxy.go`) в
   зависимости от типа бэкенда — это корень «двух мозгов» из долга 12.
7. **Авто-стрим R258** (`llamacpp_handlers_inference.go:326`) — единственное
   серверное решение о стриме; должно стать полем плана с выключателем, а не
   веткой в обработчике.
8. **На `/v1/*` клиентское `stream` не решает транспорт:** и chat, и completions
   безусловно зовут `proxyRequestOpenAIStreaming` (`:440` и `:693`), а стрим
   определяется типом ответа upstream внутри стока
   (`stream_openai_sink_r493.go:240-244`). Значит в плане поле `StreamUp`
   означает «клиент ждёт стрим», а фактический транспорт — свойство ответа;
   смешивать их в одном булеве нельзя.
