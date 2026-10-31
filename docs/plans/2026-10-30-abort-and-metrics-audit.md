# Аудит: доставка отмены до воркера + «записано ↔ показано» в метриках (2026-10-30)

**Статус:** только чтение кода. Правок в код НЕ вносилось (`git log -1` = `07419dc0`, R340).
**Область разбора — часть 1:** `internal/balancer/proxy.go` (ServeHTTP), `cancel_client_r229.go`,
`cancel_upstream_r184.go`, `llamacpp_transport.go`, `proxy_request_hijack.go`, `admission_queue.go`,
`slot_manager.go`, `model_presence_r229.go`, `model_name_match.go`;
воркер: `cmd/cppworker/router.go`, `handlers_cancel.go`, `cancel_tracking.go`, `abort_watcher.go`,
`inference.go`, `handlers_chat.go`, `handlers_openai.go`, `lazyload.go`, `generate_cancel_r248.go`,
`internal/cppbackend/active_generations.go`, `internal/cppbackend/metrics.go`, `c/bridge/bridge.go`, `c/bridge/bridge.c`, `c/bridge/bridge.h`.
**Область разбора — часть 2:** `internal/balancer/llamacpp_metrics_poller.go`, `pkg/types/llama_cpp_metrics.go`,
`pkg/types/metrics.go`, `internal/balancer/cluster_state.go`, `internal/balancer/metrics_manager.go`,
`internal/api/handlers_backends.go`, `internal/api/gguf_backends_handler.go` ↔ `webui/js/modules/renderers.js`,
`webui/js/monitor/ui-renderer.js`, `webui/js/modules/monitor-*.js`, `webui/js/modules/gguf-renderer-*.js`.

**Каждое утверждение снабжено ссылкой `файл:строка`.**

---

## 0. Резюме: что нашлось (по убыванию важности)

| # | Находка | Где |
|---|---|---|
| **A1** | **Верхний `status` ответа `/api/cancel` врёт.** `sendCancelToBackend` считает успехом любой 2xx, а воркер на пустом трекере отвечает **200** с `cancelled:0, status:"no_active_generation"`. Балансер собирает из этого `status:"cancelled"` и непустой `backends[]`. Клиент, читающий только верхний `status`, получает ложное «отменено»; правда лежит в `results[].cancelled`. | `cancel_client_r229.go:91-94`, `:209`; `handlers_cancel.go:75-93` |
| **A2** | **Имя модели сверяется по-разному на двух концах.** Балансер выбирает адресатов «мягко» (`containsFold`, срезание `.gguf`/тега), воркер отменяет по **точному** равенству `entry.model == model`. Итог: адресат выбран, приказ доставлен, `cancelled:0` — и из-за A1 это невидимо. | `model_name_match.go:32-48`, `cancel_client_r229.go:145`; `active_generations.go:126` |
| **A3** | **Список адресатов зависит от 30-секундного кэша поллера и статуса `healthy`.** Модель в кэше на узле A, генерация идёт на узле B (лаг кэша, две копии) → отмена уходит только на A. Узел, помеченный нездоровым, молча выпадает из списка. | `cancel_client_r229.go:133-162`, `llamacpp_metrics_poller.go:81`, `:167-173` |
| **A4** | **Отмена во время загрузки модели не работает по замыслу.** Загрузка отвязана от клиентского ctx; ctx-отмена лишь «запоминается» и взводит abort-флаг на первом checkpoint уже **после** загрузки. Оператор ждёт минуты и не понимает, почему Stop не действует. | `lazyload.go:700-703`; `inference.go:590-601` |
| **A5** | **Отмена «по модели» гасит всех пользователей этой модели на воркере.** R184 (обрыв/таймаут/тишина) шлёт только `{"model":…}` без `request_id`, а `CancelByModel` при пустом `userID` матчит все записи модели. Это отменяет ровно тот сценарий, ради которого делали per-inference флаги (R63): один отвалившийся клиент останавливает чужие стримы той же модели. | `cancel_upstream_r184.go:48-52`; `handlers_cancel.go:114-115`; `active_generations.go:119-126` |
| **A6** | **В stub-сборке (`-tags llama_stub`) отмены нет вообще**, но `/api/cancel` рапортует успех: `NewInferAbortFlag()` → nil, `NewAbortWatcher` не создаётся, `RequestAbort` — no-op. | `bridge_stub.go:844-847`, `:472-479`; `inference.go:590-594` |
| **A7** | **На OpenAI-поверхности (`/v1/*`) явной отмены нет.** `isClientCancelPath` знает только `/api/cancel`; клиент, который «остановился», но не закрыл сокет, оставляет генерацию живой (сработает только обрыв/таймаут/сторожа). | `cancel_client_r229.go:46-51` |
| **A8** | **Ни одного счётчика отмены.** На воркере отменённая генерация пишется как **успех** (`isBackendFailure` исключает `ErrAborted`), в `/metrics` и `/api/infer/metrics` нет `cancelled`; на балансере тоже нет. Ответ на вопрос «сколько отмен было за сутки» — только по логам. | `backend.go:3657-3662`, `:3117-3123`; `handlers_metrics.go:202-229`, `:66-78` |
| **M1** | **Поле `loadingError` пишется, а WebUI читает `error`.** Причина провала загрузки никогда не показывается: UI печатает заглушку «error». | `pkg/types/llama_cpp_metrics.go:253`; `renderers.js:1216`, `monitor-backends.js:500`, `gguf-renderer-detail-render.js:1404` |
| **M2** | **Целый блок полей VRAM-состава пишется и не показывается нигде:** `availableVramMb`, `totalVramMb`, `vramLocalFreeMb`, `vramRemoteFreeMb`, `vramRemoteNodes`, `maxVramNCtx`, `modelMaxContext`, `maxFeasibleContext` (top-level), `rpcServersActive`. Часть из них читается только внутренней логикой балансера. | `llamacpp_metrics_poller.go:524-555`; в WebUI — 0 вхождений (проверено обходом 80 файлов) |
| **M3** | **WebUI читает поля, которых балансер не заполняет никогда:** `llamaCpp.activeRequests`, `requestsPerSecond`, `avgResponseTime`, `freeSlots`, `availableSlots`, `maxConcurrentReqs`, `gpuInfo`, `availableModels`, а также `lm.nHeads`. Для llama.cpp-бэкенда панели показывают `0`, `null` или `10` вместо реальных чисел. | `ui-renderer.js:629`, `:664-666`, `:447-451`; `renderers.js:1443`, `backend-merge.js:604-605` |
| **M4** | **Четыре поля `/api/models` декодируются и выбрасываются:** `n_heads`, `n_vocab`, `active_queries`, `total_queries`. `active_queries` — единственный дешёвый признак «на воркере реально идёт генерация», и он не доезжает ни до API, ни до UI. | `llamacpp_metrics_poller.go:309`, `:314`, `:333-334` |
| **M5** | **Путь `b.llamaCppMetrics` в WebUI мёртв** — такого ключа не отдаёт ни один обработчик (все отдают `llamaCpp`); альтернативная ветка отображения загрузки не работает никогда. | `renderers.js:1197-1200`, `monitor-backends.js:480` |
| **M6** | **Нули не просто отсутствуют — они ПЕРЕКРЫВАЮТ правильные значения.** У `activeRequests`, `requestsPerSecond`, `avgResponseTime`, `freeSlots` и др. нет `omitempty`, поэтому они всегда уходят в JSON как `0`, а проверки вида `lc.activeRequests !== undefined` в WebUI всегда истинны. Недостижим fallback на `ollama.*`, куда балансер **кладёт** настоящие счётчики. Панель монитора показывает `0` (активные запросы, RPS, свободные слоты), хотя значения в ответе есть — в соседнем поле. | `pkg/types/llama_cpp_metrics.go:132-139`; `ui-renderer.js:628-629`, `:664-666`; `pkg/types/metrics.go:6-116`; `cluster_state.go:248`; `proxy.go:1718-1726` |

---

# Часть 1: отмена

## 1.1 Два независимых механизма — не путать

| Механизм | Что инициирует | Где в коде | На какой поверхности |
|---|---|---|---|
| **A. Явная отмена** (R229) | клиент шлёт `POST /api/cancel` (кнопка Stop в OpenWebUI, скрипт, Cline) | `proxy.go:942-945` → `cancel_client_r229.go:54` | только нативная Ollama-поверхность `/api/cancel` (`cancel_client_r229.go:49-51`) |
| **B. Неявная отмена** (R184) | балансер сам заметил разрыв клиента, истечение stream-таймаута или тишину upstream | `llamacpp_transport.go:856-883`, `:899-917`; hijack — `proxy_request_hijack.go:462-496`; отправка — `cancel_upstream_r184.go:36-83` | все пути (`/api/chat`, `/api/generate`, `/v1/chat/completions`) |

Оба механизма в итоге делают один и тот же HTTP-вызов: `POST http://<backend>/api/cancel` с телом
`{"model": …}` (и/или `request_id`, `user_id`) и заголовком `X-API-Token`
(`cancel_client_r229.go:186-193`, `cancel_upstream_r184.go:59-67`).

## 1.2 Путь A: явная отмена по шагам

### (1) Балансер принимает запрос ДО всякой маршрутизации

`proxy.go:942-945`: если путь — `/api/cancel` или `/api/cancel/`, вызывается `handleClientCancel` и
запрос **не** попадает ни в выбор бэкенда, ни в autoload, ни в admission-очередь. Это и было
исправлением R229 (комментарий `proxy.go:938-941`: раньше отмена маршрутизировалась как генерация,
балансер не отвечал 15 с, а стрим доходил до конца — 380 чанков, 62 с).

Свойства обработчика (`cancel_client_r229.go`):

| Что | Где |
|---|---|
| только `POST`, иначе 405 | `:59-62` |
| тело читается терпимо (лишние поля не дают 400), лимит 1 МБ | `:64-69` |
| `model` тримится, но **регистр не меняется** (намеренно) | `:70`, `:218-223` |
| всегда 200 (даже «отменять нечего») | `:100-108` |

### (2) Выбор адресатов — `cancelTargets`

`cancel_client_r229.go:117-164`:

- перебираются **все** бэкенды (`p.GetAllBackends()`, `:130`);
- остаются только `type == llama_cpp` (`:133`);
- отбрасываются все, у кого `Status != healthy` и `Status != ""` (`:136-138`);
- если задана модель — проверяется, что она **есть в кэше метрик** этого бэкенда
  (`llamaMetricsForPresence` → `metricsMgr.SnapshotLlamaCppMetrics`, `model_presence_r229.go:73-82`),
  сравнение имён — «мягкое» (`modelNameMatches`, `:145`);
- если модель не нашлась нигде — приказ рассылается **всем** здоровым llama.cpp-бэкендам
  (`:154-162`, комментарий: «приказ дешёвый»).

### (3) Отправка одному воркеру — `sendCancelToBackend`

`cancel_client_r229.go:167-210`:

- адрес: `backendHTTPAddrByID` → `http://<host>:<cppworkerPort>` (`:168`, `backend_state.go:86-97`);
- тело: только заданные поля `model` / `request_id` / `user_id` (`:172-181`);
- `X-API-Token` ставится, только если токен известен балансеру (`:191-193`);
- таймаут **2 с** (`:36`, `:194`) — вызов **синхронный**, в цикле по бэкендам (`:75-89`);
- успех = код 2xx (`:209`), тело разбирается в `detail` и попадает в `results[]` (`:204-208`).

### (4) Воркер: маршрут и обработчик

- `cmd/cppworker/router.go:67` — `mux.HandleFunc("/api/cancel", handleCancel)`. Обратите внимание:
  **без `authMiddleware`** (ср. соседние `:45`, `:83`), то есть токен не требуется, а комментарий
  в `handlers_cancel.go:19` («Auth: X-API-Token required») коду не соответствует. Практическое
  следствие: 401 отменить не может — этот путь отказа исключён.
- `handlers_cancel.go:26-94`: декодирование терпимое (`:39`), трекер активных генераций (`:51-55`),
  разбор области отмены (`:57` → `applyCancelScope`, `:103-128`):
  - `request_id` → `CancelByID` (`:109-113`), при промахе — **404** `request_id not found` (`:58-61`);
  - `model` → `CancelByModel(user_id, model)` (`:114-115`);
  - `user_id` → `CancelByUser` (`:116-117`);
  - пусто → отменить всё (админский сброс, `:118-126`).
- `internal/cppbackend/active_generations.go`:
  - `Add(requestID, userID, model, backend, cancel)` (`:39-55`), ключ — `requestID`;
  - `CancelByID` (`:74-88`) → `entry.cancel()` (`:84`);
  - `CancelByModel` (`:119-142`) → **точное** `entry.model == model` и `userID == "" || entry.userID == userID`
    (`:126`), затем `entry.cancel()` (`:134`);
  - журнал: `ActiveGenerations: cancelled by ID` (`:85`), `… by user` (`:111`), `… by model` (`:138`).

### (5) Кто вообще попадает в трекер (иначе отменять некого)

Регистрация делается в хендлерах через `setupCancelTracking`
(`cancel_tracking.go:25-54`):

- `requestID` — из `X-Request-Id` или сгенерированный `chat-<nanos>`/`gen-<nanos>` (`:26-29`);
- создаётся **child context** от `r.Context()` (`:31`), `cancel` кладётся в трекер вместе с моделью
  (`:42-44`), а сам `r` пере-биндится на этот ctx (`:54`) — иначе стрим-писатель смотрел бы на
  родительский контекст и не увидел отмену (комментарий `:20-24`);
- `cleanup` (`:45-53`) вызывает `cancel()` и снимает запись с трекера.

Места регистрации:

| Обработчик | Строка |
|---|---|
| `POST /api/chat` | `handlers_chat.go:289-290` |
| `POST /v1/chat/completions` | `handlers_openai.go:396-399` |
| `POST /v1/completions` | `handlers_openai.go:1694-1697` |
| `POST /api/generate` | `handlers_generate.go:357-360` |

Важная деталь: регистрируется **`req.Model`, как его прислал клиент**, а не `actualModel` после
алиасов/дедупа (`handlers_chat.go:289` против `:298`). Это и есть причина A2: сравнение на отмене
идёт с «сырым» именем.

Второе следствие: `request_id` записи — это `X-Request-Id` **клиента**, если тот его прислал
(`cancel_tracking.go:26`). Балансер перекладывает заголовки клиента в запрос к воркеру без изменений
(`llamacpp_transport.go:294-302`), но собственный `X-Request-ID` (`requestid.go:21`) в этот запрос
**не добавляет** — он только возвращается клиенту в ответе (`proxy.go:911-913`). Значит, отмена по
`request_id` доступна лишь тогда, когда идентификатор задал клиент; для OpenWebUI (его не присылает)
остаётся отмена по модели — со всеми последствиями A5.

### (6) ctx → флаг отмены → C-мост

1. Отмена ctx не трогает C напрямую. На ctx сидит **watcher**:
   `abort_watcher.go:58-68` (nil-safe: без ctx или без флага watcher не создаётся), `:71-79` —
   блокируется на `<-ctx.Done()` (`:73`), затем `atomic.StoreInt32((*int32)(abortFlag), 1)` (`:76`)
   и пишет журнал `AbortWatcher: per-infer abort set via C atomic flag` (`:78`).
2. Флаг **создаётся до** генерации: `inference.go:577` (`abortFlagFactory = bridge.NewInferAbortFlag`),
   `:590-601` (`beginInferCancellation`: сброс флага `:598`, вооружение watcher `:599`),
   `:603-609` (`streamWithAbort`). Это исправление R83 — раньше watcher создавался после возврата из
   блокирующего C-вызова (комментарий `:555-568`).
3. Go-биндинг: `bridge.go:958-963` (`SetInferAbort` → `C.bridge_set_infer_abort`),
   C: `bridge.c:3346-3348` (`atomic_store`), объявление `bridge.h:363`.
4. C выбирает именно этот флаг в `bridge_infer_stream`: `bridge.c:2762-2765`
   (`abort = (abort_flag != NULL) ? (atomic_int*)abort_flag : &local_abort`).
5. Checkpoints, где флаг читается:
   - prompt-фаза: `bridge.c:2836-2843` (`atomic_load(abort)` → `BRIDGE_ERR_ABORTED`, error_msg
     «generation aborted by user (bridge_infer_stream, prompt phase)»);
   - генерация: `bridge.c:2965-2974` (перед каждым `llama_decode` шага);
   - финальный сброс хвоста: `bridge.c:2992-2996`.
6. Код ошибки: `bridge.h:504` (`#define BRIDGE_ERR_ABORTED (-100)`); Go превращает его в sentinel
   `bridge.ErrAborted` (`bridge.go:323`, `:1478-1485`).
7. Хендлер превращает sentinel в честный терминатор для клиента:
   `handlers_chat.go:1310-1323` — чанк `{"done":true,"done_reason":"cancelled","cancelled":true}`;
   OpenAI-путь: `handlers_openai.go:1407`.

Итого задержка отмены = один batch (`bridge.c:2968-2969`), т.е. 10–200 мс на обычной модели,
и до нескольких секунд, если batch уходит на удалённый узел раскладки (см. L9 в разделе 1.4).

## 1.3 Путь B: обрыв клиента, таймаут, тишина upstream

В потоковом пути балансера (`llamacpp_transport.go`) висит watcher-горутина (`:856-883`):

| Ветка | Условие | Действие | Строки |
|---|---|---|---|
| клиент ушёл | `<-r.Context().Done()` | журнал `proxyRequestLlamaCpp: client cancelled, aborting stream` + `cancelUpstreamGeneration(..., "client_cancel")` | `:858-866` |
| stream-таймаут | `<-reqCtx.Done()` | `cancelUpstreamGeneration(..., "stream_timeout")` + warn `streamTimeout fired` | `:867-876` |
| закрытие по выходу из функции | `<-watcherStop` | выход | `:877-879` |

Дополнительно — «сторож тишины» R298 (`llamacpp_transport.go:899-917`): по `onStall` сначала
закрывает `resp.Body` (разблокировать `scanner.Scan()`), затем пишет warn
`proxyRequestLlamaCpp: upstream stalled — body closed to unblock scan (R298)` и отправляет
`cancelUpstreamGeneration(..., "upstream_stall")` (`:906-914`). На hijack-пути `/v1` то же самое:
`proxy_request_hijack.go:478-496` (`:482` журнал, `:495` отмена).

Отправка (`cancel_upstream_r184.go:36-83`): **асинхронная** горутина (`:58`), таймаут 3 с (`:30`),
тело — только `model` (`:48-52`), успех логируется как `cancel upstream: генерация отменена на воркере`
(`:79-81`), неудача — `cancel upstream: воркер не принял отмену` (`:72-74`).

Ещё один источник — ожидание слота: `admission_queue.go:665`
(`admission: client canceled while waiting for slot`) и `:831`
(`admission: request canceled before slot was granted`). Здесь генерации ещё нет, поэтому воркеру
ничего не отправляется — и это правильно.

## 1.4 Где отмена теряется (полный список условий)

| # | Условие потери | Механизм | Чем видно оператору |
|---|---|---|---|
| L1 | **Имя модели не совпало точно** | балансер выбрал адресата «мягко» (`model_name_match.go:36-47`), воркер сравнивает точно (`active_generations.go:126`) → `cancelled:0` | в `results[]` есть `"cancelled":0`, но верхний `status` = `cancelled` (A1) → снаружи не видно |
| L2 | **Статус бэкенда не `healthy`** | `cancel_client_r229.go:136-138` — узел выпадает из `targets` | в журнале балансера `targets` без нужного узла; `status:no_active_generation` |
| L3 | **Кэш метрик отстал / модель на другом узле** | адресат выбирается по кэшу поллера (`:143-150`), обновление раз в 30 с (`llamacpp_metrics_poller.go:81`, `:167-173`) | отмена ушла на узел, где генерации нет; на реальном узле `active_queries` не падает |
| L4 | **Генерация ещё в фазе загрузки модели** | загрузка отвязана от клиентского ctx (`lazyload.go:682-703`), явная отмена её не прерывает (`:700-703`) | `/api/cancel` отвечает 200, а `docker logs` воркера продолжает показывать загрузку; abort-флаг взведётся только на первом checkpoint после загрузки (`inference.go:590-601`) |
| L5 | **Не llama.cpp бэкенд** | фильтр по типу (`cancel_client_r229.go:133`); image- и Ollama-узлы приказ не получают | — |
| L6 | **OpenAI-клиент «остановился», не закрыв сокет** | на `/v1/*` нет пути явной отмены (`:46-51`) | стрим идёт в никуда, слот занят до конца `n_predict` |
| L7 | **Воркер не отвечает по сети / 2 с истекли** | синхронный вызов с таймаутом 2 с (`:36`, `:194-201`) | warn `client cancel: воркер не принял отмену` с `error` |
| L8 | **stub-сборка воркера** | `NewInferAbortFlag()` → nil (`bridge_stub.go:844-847`), watcher не создаётся (`inference.go:591-594`) | `/api/cancel` отвечает `cancelled:1`, генерация идёт до конца |
| L9 | **Раскладка RPC (порт 50052): batch в полёте** | флаг читается только между batch'ами (`bridge.c:2838`, `:2970`), сетевой обмен слоями не прерывается | задержка отмены = время одного обмена; подсказка в `handlers_cancel.go:87-90` прямо адресует этот случай |
| L10 | **Балансер завершается** | R184-отмена живёт в непривязанной горутине (`cancel_upstream_r184.go:58`), её никто не ждёт; при остановке процесса приказ может не уйти | в журнале нет `cancel upstream: …` |
| L11 | **`request_id` не совпал** | `CancelByID` при промахе даёт **404** (`handlers_cancel.go:58-61`), и балансер считает это «не 2xx» → `ok:false` | `client cancel: воркер не принял отмену` не пишется (ошибки сети нет), в `results[]` `"ok":false` без причины |

## 1.5 Что именно видно в журналах (точные строки `msg`)

Язык сообщений задаётся `LB_LOG_LANG` / `<COMPONENT>_LOG_LANG` / локалью (по умолчанию `ru`), затем
синхронизируется с балансером (`internal/i18n/bootstrap.go:14-21`, `:25-41`). Ниже — оба варианта
для i18n-сообщений.

### Балансер (`docker logs ol-stack-balancer`)

| Уровень | `msg` | Поля | Где |
|---|---|---|---|
| info | `client cancel: отмена от клиента разослана воркерам` / `client cancel: the client cancellation was broadcast to the workers` | `model`, `request_id`, `user_id`, `targets`, `accepted`, `status` | `cancel_client_r229.go:96-98`; текст `messages_balancer3_r234.go:235-238` |
| warn | `client cancel: воркер не принял отмену` / `client cancel: the worker did not accept the cancellation` | `backend`, `model`, `error` | `cancel_client_r229.go:197-201`; текст `messages_balancer3_r234.go:239-242` |
| info | `cancel upstream: генерация отменена на воркере` / `cancel upstream: the generation was cancelled on the worker` | `backend`, `model`, `reason`, `status` | `cancel_upstream_r184.go:78-81`; текст `messages_balancer3_r234.go:249-252` |
| warn | `cancel upstream: воркер не принял отмену` / `cancel upstream: the worker did not accept the cancellation` | `backend`, `model`, `reason`, `error` | `cancel_upstream_r184.go:70-76`; текст `messages_balancer3_r234.go:245-248` |
| info | `proxyRequestLlamaCpp: client cancelled, aborting stream` | `backend`, `model`, `context_error`, `bytes_forwarded` | `llamacpp_transport.go:859-862` |
| warn | `proxyRequestLlamaCpp: streamTimeout fired, aborting stream` | `backend`, `model`, `context_error`, `stream_timeout_sec`, `bytes_forwarded` | `llamacpp_transport.go:872-876` |
| warn | `proxyRequestLlamaCpp: upstream stalled — body closed to unblock scan (R298)` | `backend`, `model`, `prefill`, `stalled_sec`, `original_path`, `bytes_forwarded` | `llamacpp_transport.go:906-911` |
| info | `proxyRequestLlamaCpp: stream ended by client (client_cancel)` | `reason`, `backend`, `model`, `original_path`, `bytes_forwarded`, `elapsed_sec`, `scanner_err` | `llamacpp_transport.go:2256-2259` |
| info | `hijack streaming: client disconnected, closing upstream` | `backend` | `proxy_request_hijack.go:464-465` |
| warn | `hijack streaming: upstream stalled — closing stream with terminator (R298)` | `backend`, `model`, `prefill`, `stalled_sec` | `proxy_request_hijack.go:482-484` |
| info | `admission: client canceled while waiting for slot` | — | `admission_queue.go:665` |
| info | `admission: request canceled before slot was granted` | — | `admission_queue.go:831` |
| error | `client cancelled streaming request` | — | `streaming.go:211` |

`reason` в сообщениях R184: `client_cancel`, `stream_timeout`, `upstream_stall` (`:866`, `:871`, `:914`).

### Воркер (`docker logs ol-stack-cppworker-gpu`)

| Уровень | `msg` | Поля | Где |
|---|---|---|---|
| info | `handleCancel: completed` | `by` (id/user/model/all), `cancelled`, `request_id`, `user_id`, `model` | `handlers_cancel.go:63-65` |
| info | `ActiveGenerations: cancelled by ID` | `request_id`, `user`, `model` | `active_generations.go:85-86` |
| info | `ActiveGenerations: cancelled by user` | `user`, `count` | `active_generations.go:111-112` |
| info | `ActiveGenerations: cancelled by model` | `user`, `model`, `count` | `active_generations.go:138-139` |
| info | `AbortWatcher: per-infer abort set via C atomic flag` | `ctx_err` | `abort_watcher.go:77-78` |
| debug | `handleChat: cancel tracking enabled` / `handleV1ChatCompletions: …` / `handleV1Completions: …` / `handleGenerate: …` | `request_id`, `model`, `user_id` | `handlers_chat.go:291-292`, `handlers_openai.go:401`, `:1699`, `handlers_generate.go:359` |
| warn | `unload force=true: cancelled active generations` | — | `handlers_model.go:1104` |
| warn | `reload force=true: cancelled active generations` | — | `handlers_model.go:2553` |

Текст ошибки от C-моста при отмене в prompt-фазе: `generation aborted by user
(bridge_infer_stream, prompt phase)` (`bridge.c:2840`) — он попадает в `error`-чанк клиенту, если
отмена пришла из фазы префилла.

**Чего в журналах НЕТ:** строки о том, что «отмена не сработала». Отсутствие
`ActiveGenerations: cancelled by model` при наличии `handleCancel: completed` — единственный признак.

## 1.6 Счётчики и метрики: что меняется, а что нет

| Место | Что происходит при отмене | Где |
|---|---|---|
| воркер, `/api/infer/active` | `count` уменьшается (запись снимается на выходе из хендлера) | `handlers_cancel.go:136-153`, снятие — `cancel_tracking.go:45-53` |
| воркер, `/api/models` → `models[].active_queries` | уменьшается (`InFlight().Dec` на `defer`) | `handlers_chat.go:264-267` (Inc/Dec), поле — `handlers_model.go:1501-1506` |
| воркер, `/api/infer/active` → `generations[]` | запись исчезает; при `cancelled:1` — сразу после `cancel()` | `active_generations.go:146-162` |
| воркер, `/metrics` (`cppworker_requests_total`) | **+1**: отменённая генерация пишется как обычный запрос | `backend.go:3117-3123` → `metrics.go:84-88` |
| воркер, `/metrics` (`cppworker_errors_total`), `error_rate`, p50/p95/p99 | **не меняется**: `isBackendFailure` исключает `ErrAborted` | `backend.go:3657-3662`; намерение описано в комментарии `:3649-3656` |
| воркер, счётчика отмен нет вовсе | в `/metrics` нет ни одного `cancelled`/`aborted` | `handlers_metrics.go:202-229` |
| воркер, `/api/infer/metrics` | счётчика отмен нет; `totals.requests` растёт | `handlers_metrics.go:66-78` |
| балансер, `ActiveReqs` бэкенда | декремент при освобождении слота (в т.ч. на отмене) | `slot_manager.go:79-96` |
| балансер, `TotalRequests` | **+1** уже на старте запроса (отмена тоже считается) | `proxy_request.go:513` |
| балансер, `CalculatedRPS` | считается по истории стартов (`recordRequest`), отмена не вычитается | `proxy_request.go:1156-1180`; публикуется как `ollama.requestsPerSecond` — `cluster_state.go:246-251` |
| балансер, очередь | счётчик `processed` относится к завершённым ожиданиям; отмена в очереди логируется | `admission_queue.go:665`, `:831` |

Вывод: **по метрикам отмену не увидеть.** Ни `error_rate`, ни RPS не отличают «дошло» от «не дошло».
Рабочие инструменты — `GET /api/infer/active` на воркере (сразу видно, сняли запись или нет) и журналы.

## 1.7 Найденные дефекты части 1 (что стоит починить)

1. **A1: не врать в верхнем `status`.** Считать успехом не 2xx, а `detail["cancelled"] > 0`
   (`cancel_client_r229.go:209` + `:91-94`), либо отдавать `status` по агрегату `results[]`.
   Иначе клиент/оператор не отличит «отменено» от «нечего было отменять» — а это ровно тот случай,
   когда генерация продолжается.
2. **A2: одно правило сравнения имён.** Либо воркер нормализует имя так же, как балансер
   (`model_name_match.go:65-73`), либо балансер шлёт `request_id` и воркер отменяет по нему.
   Сейчас «мягкий выбор адресата + точное сравнение на воркере» — гарантированный класс промахов.
3. **A5: R184 не должен гасить чужие стримы.** `cancel_upstream_r184.go:48-52` шлёт только `model`;
   надо передавать `request_id` **запроса** — но он известен балансеру только если клиент прислал
   `X-Request-Id` (см. 1.2(5)) — либо `user_id`; иначе один отвалившийся клиент останавливает всех
   пользователей модели на узле (`active_generations.go:126` при `userID == ""`). Более надёжный
   вариант: пусть балансер сам проставляет `X-Request-Id` в запрос к воркеру
   (`llamacpp_transport.go:294-302`) — тогда `request_id` есть всегда.
4. **A4: сказать оператору правду про загрузку.** Либо в `handleCancel` возвращать признак
   «генерация в фазе загрузки, отмена применится после неё» (данные есть: `Status()`, состояние
   загрузки), либо реализовать отмену загрузки. Сейчас молчаливое «cancelled:1» + минуты ожидания.
5. **A8: счётчик отмен.** Простейший `cppworker_inference_cancelled_total{model}` в
   `/metrics` + поле `cancelled` в `/api/infer/metrics` — иначе «сколько Stop'ов было» не отвечает
   ничто (см. `handlers_metrics.go:202-229`).

---

# Часть 2: таблица метрик

## 2.1 Как данные доходят до WebUI (три независимых пути)

| Путь | Источник | Что отдаёт | Кто в WebUI читает |
|---|---|---|---|
| **P1. `/api/v1/cluster`** | `GetClusterState` мержит `metricsMgr.llamaMetrics[id]` в `metrics.LlamaCpp` | вложенный объект `backend.llamaCpp` (JSON-тег `llamaCpp`, `pkg/types/metrics.go:53`) — **все** поля `types.LlamaCppMetrics` как есть | `renderers.js` (dashboard/models), `webui/js/monitor/ui-renderer.js`, `monitor-backends.js` |
| **P2. `/api/v1/backends` (список) и `/api/v1/backends/{id}`** | `LlamaCppMetricsWithProvenance` | список: **плоские** `loadedModels`, `loadingModels`, `loadingModelCount`, `loadedModelCount`, `autoTune` (`handlers_backends.go:900-949`); по id — полный `BackendMetrics` с `llamaCpp.*` | `renderers.js:1198`, страница GGUF (`gguf-renderer-refresh.js:456`, `:494`) |
| **P3. `/api/v1/gguf/backends`** | `buildGgufBgInfo` | **snake_case-факты** инстанса: `contextSize`, `kv_cache_type`, `gpu_layers`, `batch_size`, `parallel`, `max_slots`, `context_per_seq`, `reasoning_enabled`, `device_layers_exact`, `quantization`, `size_bytes`, `n_layers`, `architecture`, `gguf_max_context`, `parameters` | страница GGUF (`gguf-renderer-*`) |

Плюс `app.js` склеивает P2 в объекты P1 (`app.js:875-903`, `:935-953`), поэтому JS-объект бэкенда
несёт одновременно `b.llamaCpp.loadedModels` (P1) и `b.loadedModels` (P2). Это причина, по которой
одни и те же данные читаются из двух разных мест — и расходятся.

## 2.2 Верхний уровень `LlamaCppMetrics` (путь P1)

| Поле (JSON) | Пишется из | Показывается в WebUI | Вердикт |
|---|---|---|---|
| `maxVramNCtx` | `/api/models`.max_vram_n_ctx → `llamacpp_metrics_poller.go:264`, `:524` | — (0 вхождений в `webui/`) | пишется, наружу не выходит; используется preflight'ом |
| `vramKnown` | `/api/models`.vram_known → `:268`, `:525` | — | пишется, не показывается (единственный `vramKnown` в UI — про участника группы, `monitor-groups.js:156`) |
| `maxRamNCtx` | **не пишется никем** (объявлено `llama_cpp_metrics.go:92`) | — | мёртвое поле |
| `availableVramMb` | `:274`, `:543` | — | пишется, читается только логикой балансера (`backend_selector_vram_r248.go:43`, `placement_over_reload_r293.go:228`, `image_resources.go:1364`) |
| `totalVramMb` | `:275`, `:544` | — | пишется, не показывается |
| `rpcEnabled` | `/api/models`.rpc_enabled → `:279`, `:548` | — (в UI `b.rpcEnabledOverride` — другое поле, `monitor-backends.js:191`) | пишется, не показывается |
| `rpcServersActive` | `:280`, `:549` | — живьём; только демо-данные `demo-mode.js:268`, `:289` | пишется, не показывается |
| `vramLocalFreeMb` | `:281`, `:550` | — | пишется, не показывается |
| `vramRemoteFreeMb` | `:282`, `:551` | — | пишется, не показывается |
| `vramRemoteNodes` | `:283`, `:552` | — | пишется, не показывается |
| `vramRemoteSource` | `:287`, `:555` | `monitor-backends.js:344` (подпись в колонке Status/роль) | **показывается** |
| `rpcNode` | `/api/models`.rpc_server → `:292-301`, `:564-573` | `monitor-backends.js:304-306` (бейдж узла раскладки) | **показывается** |
| `modelMaxContext` | `:269`, `:526` | — | пишется, не показывается |
| `maxFeasibleContext` | агрегат по моделям, `:530-542` | — (per-model `feasibleMaxContext` показывается, top-level — нет) | пишется, не показывается |
| `ggufMaxContext` | `:535-541` | — как top-level | пишется, не показывается |
| `loadedModels` | `:523` | `renderers.js:202-207`, `:254-329`, `:1312`; `ui-renderer.js:446`, `:638-642`; GGUF — `gguf-renderer-refresh.js:494-495`, `:520-521` | **показывается** (главный источник) |
| `loadingModels` | `:791` (из `/api/models/load/progress`) | через P2: `renderers.js:1198-1199`, `monitor-backends.js:480`; ветка `b.llamaCppMetrics.loadingModels` (`renderers.js:1200`) **мертва** — см. M5 | **показывается** (через P2) |
| `availableModels` | только инициализируется пустым (`metrics_manager.go:229`, `:334`, `:352`); поллер не пишет | `backend-merge.js:604-605` читает `llamaCpp.availableModels` | пустое обещание |
| `activeRequests` | **не пишется** в `LlamaCpp` (только в Ollama-проекцию: `proxy.go:1718-1720`) | `ui-renderer.js:629` | ожидается, не заполняется |
| `totalRequests` | не пишется в `LlamaCpp` (`cluster_state.go:252`, `:258` — про Ollama) | — (читается только `o.totalRequests`) | ожидается частично |
| `avgResponseTime` | не пишется в `LlamaCpp` | `ui-renderer.js:665` | ожидается, не заполняется |
| `requestsPerSecond` | не пишется в `LlamaCpp` (`cluster_state.go:248` — Ollama) | `ui-renderer.js:664` | ожидается, не заполняется |
| `freeSlots` | не пишется в `LlamaCpp` (`proxy.go:1722-1726` — Ollama) | `ui-renderer.js:447`, `:666` | ожидается, не заполняется |
| `availableSlots` | не пишется | `ui-renderer.js:448` | ожидается, не заполняется |
| `maxModels` | не пишется в `LlamaCpp` | — | мёртвое |
| `maxConcurrentReqs` | не пишется в `LlamaCpp` (вместимость живёт в `backend.MaxConcurrentReqs`) | `ui-renderer.js:450`, `:656` | ожидается, не заполняется |
| `gpuInfo` | **не пишется никем**; читается в `handlers_backends.go:860-884` | `gguf-renderer-refresh.js:482` берёт `data.gpu` (метрики агента), не это поле | мёртвое поле |

## 2.3 Per-model `loadedModels[]` (поллер `llamacpp_metrics_poller.go:455-509`)

| Поле (JSON) | Пишется из | Показывается в WebUI (`файл:строка`) | Вердикт |
|---|---|---|---|
| `name` | `:456` ← `name` | `renderers.js:603`, `:670`, `:1332`; `ui-renderer.js:468`, `:639`; `gguf-renderer-detail-render.js:1067` | показывается |
| `path` | `:457` | `renderers.js:1344` (в `ggufPath`), `gguf-renderer-detail-render.js:971`, `:1068` | показывается |
| `size` | `:458` (`size_bytes` ← `loading_size_bytes`) | `renderers.js:1334`, `:1437` (`computeMemorySplit`), `gguf_backends_handler.go:203` → карточка GGUF | показывается |
| `vramUsage` | `:459` (`size_vram` ÷ 1 МиБ) | `renderers.js:324` (tooltip карточки рантайма) | показывается (в одном месте) |
| `ramUsage` | `:460` | `renderers.js:325` | показывается (в одном месте) |
| `contextLength` | `:461` ← `context_size` | `renderers.js:613`, `:657`, `:1350`; `ui-renderer.js:469`; `gguf:996` (через `context_size`/`contextSize`) | показывается |
| `parallel` | `:462` | `gguf-renderer-detail-render.js:1042`, `gguf_backends_handler.go:197` | показывается |
| `maxSlots` | `:463` | `gguf-renderer-detail-render.js:1042`, `:1046` | показывается |
| `context_per_seq` | `:466` (JSON-тег snake_case, `llama_cpp_metrics.go:176`) | `gguf-renderer-detail-render.js:999` (читается и `context_per_seq`, и `contextPerSeq`) | показывается |
| `batchSize` | `:467` | `renderers.js:288-290`, `:323`, `:1352`; GGUF — из `batch_size` (`gguf_backends_handler.go:196`), факт в редакторе `gguf-renderer-detail-render.js:2470` | показывается частично |
| `numGpuLayers` | `:468` — **производное** от `device_layers_exact` (`:443-454`), т.к. `gpu_layers` воркер не отдаёт | `renderers.js:283`, `:322`, `:614`, `:1351`; `gguf:1115`, `:1126` | показывается |
| `quantization` | `:469` (с fallback-парсингом имени, `:410-417`) | `renderers.js:292`, `:321`, `:598`, `:660`, `:1343`; `gguf:1206` | показывается |
| `state` | `:470` (`resolveModelStateR257`, R257) | `renderers.js:326`, `:612`, `:1353`; в GGUF — нормализованный `status` (`gguf_backends_handler.go:190`) | показывается |
| `device_layers_exact` | `:471`, `:427-432` (JSON-тег snake_case, `llama_cpp_metrics.go:185`) | `gguf-renderer-detail-render.js:1115`, `:1316`; `gguf-renderer-refresh.js:675-676` | показывается |
| `architecture` | `:474` | `renderers.js:1355`; `gguf_backends_handler.go:205` | показывается |
| `nLayers` | `:475` | `renderers.js:1356`, `:1438`; `gguf:1127` (`n_layers`/`nLayers`) | показывается |
| `nKvHeads` | `:476` | `renderers.js:1357`, `:1440` (расчёт KV) | показывается |
| `nEmbd` | `:477` | `renderers.js:1358`, `:1443` | показывается |
| `headDimK` | `:478` | `renderers.js:1359`, `:1442` | показывается |
| `headDimV` | `:479` | `renderers.js:1360`, `:1444` | показывается |
| `maxContext` | `:480` ← `gguf_context_length` | `renderers.js:622` (fallback для `ggufMaxContext`), `:1361` | показывается |
| `loadedAt` | `:481` | — для llama.cpp (единственное чтение `loadedAt` — `image-page.js:180`, image-модели) | пишется, не показывается |
| `capabilities` | `:502` | `gguf-renderer-detail-render.js:1183` (`capabilities.reasoning`) | показывается (узко) |
| `kvCacheType` | `:489` | `gguf-renderer-detail-render.js:1063` | показывается |
| `flashAttnType` | `:490` | `gguf-renderer-detail-render.js:1159` | показывается |
| `useMmap` | `:491` | `gguf-renderer-detail-render.js:1170` | показывается |
| `feasibleMaxContext` | `:497` | `renderers.js:621`, `:1880` | показывается |
| `ggufMaxContext` | `:498` | `renderers.js:622`, `:1881` | показывается |
| `vramKnown` (per-model) | `:499` | — | пишется, не показывается |
| `reasoningEnabled` | `:505` | `gguf-renderer-detail-render.js:1182` | показывается |
| `deviceLayers` (camelCase-дубль) | `:508` | — (UI читает только `device_layers_exact`) | пишется, не показывается |
| `parameters` | приписывается не поллером, а балансером: `cluster_state.go:280-286`, `gguf_backends_handler.go:161-165` | `gguf-renderer-detail-render.js:892-918` (источники параметров, R119) | показывается |

## 2.4 `loadingModels[]` (прогресс загрузки)

| Поле | Пишется | Показывается | Вердикт |
|---|---|---|---|
| `name` | `:776` | `renderers.js:1217`, `:1221`, `monitor-backends.js` (тот же блок) | показывается |
| `state` | `:777` | `renderers.js:1215` | показывается |
| `size` | `:778` (из `loadingSizeBytes`) | — (UI читает `loadingSizeBytes`, см. ниже) | пишется, не показывается |
| `loadingStartedAt` | `:779` | `renderers.js:1209`, `monitor-backends.js:493`, `gguf-renderer-detail-render.js:1379`, `gguf-renderer-list.js:91` | показывается |
| `loadingSizeBytes` | `:780` | через балансер — нигде; показывается только когда UI идёт **напрямую** в воркер (`gguf-load-progress.js:105`) | пишется, не показывается |
| `loadingError` | `:781` | — | **пишется под неверным именем** (см. M1) |

## 2.5 Пишется, но не показывается нигде — кандидаты на удаление или на отображение

Полный список (0 вхождений в `webui/` — проверено обходом всех `.js`/`.html` вне тестов):

1. `maxVramNCtx` (`llama_cpp_metrics_poller.go:524`) — нужен **внутри** (preflight), но оператор не видит,
   «сколько влезает в VRAM» — это ровно то, что он ищет в панели.
2. `maxRamNCtx` (`llama_cpp_metrics.go:92`) — **не пишется никем**, удалить.
3. `availableVramMb` / `totalVramMb` (`:543-544`) — используются решениями балансера, не показываются.
4. `vramLocalFreeMb` / `vramRemoteFreeMb` / `vramRemoteNodes` (`:550-552`) — «состав ёмкости раскладки»
   заявлен в комментарии `llama_cpp_metrics.go:95-104` как нужный панели, но панель показывает
   только качественный `vramRemoteSource`. Кандидат №1 на добавление в UI.
5. `rpcServersActive` (`:549`) — список адресов узлов, которые воркер реально использует; в UI только
   демо-данные.
6. `rpcEnabled` (`:548`) — в UI есть только `role`/`rpcEnabledOverride` (`monitor-backends.js:171-191`).
7. `modelMaxContext` (`:526`), `maxFeasibleContext` (top-level, `:530-542`) — в UI есть только
   per-model значения.
8. `availableModels` (`llama_cpp_metrics.go:131`) — инициализируется пустым и никогда не наполняется,
   при этом `backend-merge.js:604-605` его читает. Либо наполнять, либо удалять.
9. `gpuInfo` (`llama_cpp_metrics.go:140`) — не пишется никем, но читается в
   `handlers_backends.go:860-884` (мёртвая ветка «подставить GPU из LlamaCpp»).
10. `deviceLayers` (`llama_cpp_metrics.go:236`) — дубль `device_layers_exact`
    (`llama_cpp_metrics.go:185`); UI читает только второй.
11. `loadedAt` (per-model, `:196`) — для llama.cpp в UI не читается.
12. `vramKnown` (per-model, `:220`) — в UI не читается.
13. `loadingSizeBytes` (per-model, `:252`) — из балансера в UI не доходит.
14. `totalRequests`, `maxModels` (top-level) — в UI не читаются (для llama.cpp).
15. Поля `size` и `loadingError` в `loadingModels[]` (см. 2.4).

## 2.6 WebUI ожидает, балансер не заполняет — пустые обещания

| Кто ожидает | Что ожидает | Что реально | Следствие |
|---|---|---|---|
| `ui-renderer.js:629` | `llamaCpp.activeRequests` | не пишется нигде; `b.activeRequests` в объекте бэкенда тоже нет — `BackendMetrics` такого поля не имеет (`pkg/types/metrics.go:6-116`) | в колонке «Запросы» у llama.cpp-бэкенда **всегда 0**, даже во время генерации (см. M6) |
| `ui-renderer.js:664` | `llamaCpp.requestsPerSecond` | не пишется | RPS llama.cpp бэкенда = 0 |
| `ui-renderer.js:665` | `llamaCpp.avgResponseTime` | не пишется | «среднее время» = 0 |
| `ui-renderer.js:447-449` | `llamaCpp.freeSlots`, `availableSlots` | не пишутся | карточка доступности: `freeSlots = 0` |
| `ui-renderer.js:450`/`:656` | `llamaCpp.maxConcurrentReqs` | не пишется | «loadable» считается от `b.maxConcurrentRequests` или дефолта 10 |
| `renderers.js:1514-1516` | `oll.activeRequests`, `maxConcurrentRequests`, `freeSlots` **для llama.cpp** | `b.ollama` у llama.cpp пуст (`cluster_state.go:230` обнуляет `RunningModels`, Ollama-метрик нет) | блок «Загрузка бэкендов» показывает `active: 0/10, free: 0` при живой генерации |
| `renderers.js:1443` | `lm.nHeads` | поле не доезжает (декодируется и выбрасывается, `llamacpp_metrics_poller.go:309`) | fallback `headDimK = nEmbd/nHeads` не работает никогда; остаётся `headDimK` из воркера или 0 |
| `renderers.js:1200`, `monitor-backends.js:480` | `b.llamaCppMetrics.loadingModels` | такого ключа не отдаёт ни один обработчик (все отдают `llamaCpp`, `loadedModels`, `loadingModels`) | ветка-«альтернатива» мертва (см. M5) |
| `backend-merge.js:604-605` | `llamaCpp.availableModels` | пустой массив | «доступные модели» для llama.cpp пусты (UI спасается `loadedModels`) |
| `gguf-renderer-detail-render.js:1196` | `batched_parallel` / `batchedParallel` | ни воркер, ни балансер не отдают | чип «batched» не появляется никогда |

## 2.7 Декодируется из `/api/models` и выбрасывается (поллер)

Поля объявлены в структуре-декодере (`llamacpp_metrics_poller.go:302-379`), но в `types.LlamaCppModel`
не переносятся и дальше в API не попадают:

| Поле вора /api/models | Строка декодера | Куда должно было попасть | Комментарий |
|---|---|---|---|
| `n_heads` | `:309` | в UI — как fallback для `headDim` (`renderers.js:1443`) | бессмысленный fallback без доезда |
| `n_vocab` | `:314` | никуда | подтверждение «декодируем и забываем» |
| `active_queries` | `:333` | никуда | единственный признак «на воркере идёт генерация»; нужен и для наблюдаемости отмены |
| `total_queries` | `:334` | никуда | счётчик запросов модели |
| `count` (top-level) | `:263` | никуда | — |

## 2.8 Несоответствия, из-за которых данные не видны (а не отсутствуют)

1. **`loadingError` ↔ `error`.** Балансер сериализует причину как `loadingError`
   (`pkg/types/llama_cpp_metrics.go:253`), WebUI читает `lm.error` (`renderers.js:1216`,
   `monitor-backends.js:500`, `gguf-renderer-detail-render.js:1404`). Воркер отдаёт именно `error`
   (`llamacpp_metrics_poller.go:746`) — поле теряется при переупаковке, и оператор видит заглушку
   «error» вместо причины. Это ровно тот класс дефекта, который R119 уже исправлял для `state`.
2. **Смешанные регистры в одном JSON.** В `LlamaCppModel` большинство тегов camelCase, но два —
   snake_case: `context_per_seq` (`:176`) и `device_layers_exact` (`:185`). UI написан «терпимо»
   (`gguf-renderer-detail-render.js:999` принимает оба варианта, `:1115` — тоже), но любой новый
   потребитель, читающий `contextPerSeq`/`deviceLayersExact` из P1, получит `undefined`.
   Разъезд уже виден в демо-данных: `demo-mode.js:221-222` кладёт оба ключа.
3. **Единицы `vramUsage`/`ramUsage`.** Объявлены в МБ (`llama_cpp_metrics.go:148-149`), и в tooltip
   читаются как МБ (`renderers.js:324-325`), но в другом месте тот же `ramUsage` делится на
   `1024^3` как байты (`renderers.js:1127`, `:2622`). Для llama.cpp там всегда 0 (секция «Loaded
   Models» строится из `ollama.runningModels`, `renderers.js:996`), то есть баг «спит» — но
   при добавлении llama.cpp в эту секцию число будет врать в 1024² раз.
4. **`gguf-renderer-refresh.js:482`** берёт `state.gpuInfo = data.gpu` (метрики агента), а не
   `llamaCpp.gpuInfo`; при воркере без агента карточка GPU пуста, хотя `totalVramMb`/`availableVramMb`
   у балансера есть.
5. **Два источника одного списка.** `renderers.js` читает `b.llamaCpp.loadedModels` (P1),
   а страница «Бэкенды» того же файла — `b.loadingModels` (P2, `:1198`) и `b.llamaCpp?.loadedModels`
   (`:1528`) одновременно; при рассинхроне `app.js:935-953` они могут показать разные наборы
   (P1 обновляется поллером, P2 — тем же кэшем, но иначе упакованным).

## 2.9 Нули, которые перекрывают правду (M6) — самый неприятный класс

У полей `activeRequests`, `totalRequests`, `avgResponseTime`, `requestsPerSecond`, `freeSlots`,
`availableSlots`, `maxModels`, `maxConcurrentReqs` в `types.LlamaCppMetrics` **нет `omitempty`**
(`pkg/types/llama_cpp_metrics.go:132-139`). Поллер их не пишет, но в JSON они всё равно уходят —
нулями. WebUI проверяет именно `!== undefined`. Начнём с самого заметного — `activeRequests`
(`ui-renderer.js:628-629`):

```js
var activeReq = b.activeRequests;
if (activeReq === undefined) activeReq = lc.activeRequests !== undefined ? lc.activeRequests : (o.activeRequests || 0);
```

`b.activeRequests` в объекте бэкенда отсутствует (`BackendMetrics`, `pkg/types/metrics.go:6-116`,
такого поля не имеет), а `lc.activeRequests` — всегда `0` и **определён**, потому что `omitempty`
у него нет (`pkg/types/llama_cpp_metrics.go:132`). Значит, ветка `o.activeRequests` недостижима, хотя
настоящий счётчик балансер держит именно там (`proxy.go:1718-1720`). Следствие на живом стенде:
**во время генерации Monitor показывает 0 активных запросов**.

То же самое — у `requestsPerSecond`, `avgResponseTime`, `freeSlots` (`ui-renderer.js:664-666`):

```js
rps:             b.rps !== undefined ? b.rps : (lc.requestsPerSecond !== undefined ? lc.requestsPerSecond : (o.requestsPerSecond || 0)),
avgResponseTime: b.avgResponseTime !== undefined ? b.avgResponseTime : (lc.avgResponseTime !== undefined ? lc.avgResponseTime : (o.avgResponseTime || 0)),
freeSlots:       b.freeSlots !== undefined ? b.freeSlots : (lc.freeSlots !== undefined ? lc.freeSlots : null),
```

Настоящий RPS балансер кладёт в Ollama-проекцию (`cluster_state.go:246-251`:
`metrics.Ollama.RequestsPerSecond = backendState.CalculatedRPS`), `FreeSlots` — тоже
(`proxy.go:1722-1726`), но монитор читает затёртые `lc.*`. Верхнеуровневых `b.rps`/`b.freeSlots`
в объекте бэкенда не существует вовсе (`pkg/types/metrics.go:6-116`), так что первая ветка
`b.X !== undefined` не срабатывает никогда.

Итог: **данные в ответе есть, но панель показывает 0.** Это не «поле не заполняется» — это
«поле заполняется нулём и побеждает в приоритете». Лечится либо `omitempty` на этих полях
`LlamaCppMetrics`, либо переносом реальных значений в `LlamaCpp.*`.

---

# Живая проверка (чек-лист)

Стенд: `docker-compose.stack.yml`, профиль `full`. Контейнеры `ol-stack-balancer`,
`ol-stack-cppworker-gpu`; порты по умолчанию — клиентская поверхность `18080`, админ-API `18081`,
воркер `18092` (`deployments/docker-compose.stack.yml:116-122`, `:796-800`). Токен —
`CPPWORKER_API_TOKEN` из `deployments/.env` (`:151`, `:648-649`).

> Ниже — команды в форме POSIX (`bash` на хосте стенда). В PowerShell замените `$VAR` на `$env:VAR`,
> а `grep -E` на `Select-String`.
> ВАЖНО: `/api/cancel` и `/api/infer/active` на воркере **не требуют токена** (`router.go:67-68`),
> а админ-API балансера — требует (`routes.go:118` и `AuthMiddleware` вокруг).

## Шаг 0. Переменные и «нулевая точка»

```bash
LB=http://127.0.0.1:18080      # LB_PORT
ADM=http://127.0.0.1:18081     # LB_ADMIN_PORT
CW=http://127.0.0.1:18092      # CPPWORKER_PORT_HOST
TOK=$(grep -E '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2-)
MODEL='<имя модели, как его зовёт клиент>'
```

Снять «нулевую точку» (до отмены) — **все три снимка нужны для сравнения**:

```bash
# 1) трекер отмен на воркере: тут должна появиться/исчезнуть запись генерации
curl -s $CW/api/infer/active | jq '{count, gens: [.generations[]|{request_id,user_id,model}]}'

# 2) счётчик in-flight по модели (active_queries) + состояние
curl -s $CW/api/models | jq '[.models[]|{name,state,active_queries,context_size}]'

# 3) точка зрения балансера: слоты/модели/метрики llama.cpp
curl -s $ADM/api/v1/cluster -H "X-API-Token: $TOK" \
  | jq '[.backends[]|{id,status,activeRequests,maxConcurrentRequests,
        models, loaded:[.llamaCpp.loadedModels[].name],
        rpc:.llamaCpp.rpcEnabled, rpcNodes:.llamaCpp.vramRemoteNodes}]'
```

Замечание по последней команде: `activeRequests` в объекте бэкенда придёт как **`null`** — поля с таким
именем у бэкенда нет вовсе (`pkg/types/metrics.go:6-116`), счётчик живёт в `ollama.activeRequests`.
Это не ошибка команды, а подтверждение пункта 2.9/M6: именно отсутствие поля верхнего уровня вместе
с всегда-нулевым `llamaCpp.activeRequests` даёт в панели «0 активных запросов».

Ожидание: `count` = 0, `active_queries` = 0; поле `activeRequests` в третьем снимке — `null`
(см. замечание выше).

## Шаг 1. Запустить длинную стримящую генерацию

```bash
curl -sN -o /tmp/live-cancel.ndjson $LB/api/chat -H 'Content-Type: application/json' \
  -H 'X-Request-Id: livecheck-cancel-1' -H 'X-User-Id: operator' \
  -d "{\"model\":\"$MODEL\",\"stream\":true,
       \"messages\":[{\"role\":\"user\",\"content\":\"напиши подробный текст на 2000 слов\"}],
       \"options\":{\"num_predict\":2000}}" &
STREAM_PID=$!
sleep 5
```

Через 5 с — три контрольных снимка:

```bash
curl -s $CW/api/infer/active | jq '.count'          # ожидание: 1
curl -s $CW/api/models | jq '[.models[]|{name,active_queries}]'   # ожидание: >0 у $MODEL
curl -s $ADM/api/v1/cluster -H "X-API-Token: $TOK" \
  | jq '[.backends[]|{id,status,models,ollamaActiveRequests:.ollama.activeRequests,
        loaded:[.llamaCpp.loadedModels[].name], llamaCppActiveRequests:.llamaCpp.activeRequests}]'
```

Третий снимок — наглядная демонстрация M6: `ollamaActiveRequests` = 1, а `llamaCppActiveRequests` = 0.
Панель монитора читает второе (затёртое) значение и показывает «0 запросов».

Если `count` уже 0 — генерация идёт не на этом воркере или запись не зарегистрирована
(`cancel_tracking.go:42-44`); дальше проверять отмену бессмысленно.

## Шаг 2. Явная отмена (`POST /api/cancel`) — главный сценарий

```bash
curl -s -X POST $LB/api/cancel -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\"}" | jq
```

Смотреть в ответе:

- `status` — верхний уровень (он **оптимистичен**, см. A1);
- `results[].cancelled` — **правда**: сколько генераций реально отменено;
- `results[].status` — `cancelled` или `no_active_generation`;
- `results[].active_generations`, `rpc_enabled`, `hint` — диагностика воркера
  (`handlers_cancel.go:80-93`).

Проверка доставки (в течение 1–2 с):

```bash
curl -s $CW/api/infer/active | jq '.count'                 # ожидание: 0
curl -s $CW/api/models | jq '[.models[]|{name,active_queries}]'  # ожидание: 0
tail -n 3 /tmp/live-cancel.ndjson                           # ожидание: done:true, done_reason=cancelled, cancelled:true
```

Журналы:

```bash
docker logs ol-stack-balancer   --since 2m 2>&1 | grep -E 'client cancel:|cancel upstream:'
docker logs ol-stack-cppworker-gpu --since 2m 2>&1 \
  | grep -E 'handleCancel: completed|ActiveGenerations: cancelled|AbortWatcher: per-infer abort'
```

**Что должно быть (успешная отмена):**

1. балансер: `client cancel: отмена от клиента разослана воркерам` с `status: cancelled`,
   непустым `accepted` и `targets`, содержащим нужный бэкенд;
2. воркер: `handleCancel: completed` с `by: model` и `cancelled: 1`;
3. воркер: `ActiveGenerations: cancelled by model`;
4. воркер: `AbortWatcher: per-infer abort set via C atomic flag`;
5. клиент: терминатор `done_reason: "cancelled"`.

Отсутствие шага 4 при наличии шагов 2–3 = ctx отменили, но watcher не был вооружён (флаг `nil`:
stub-сборка либо путь без `beginInferCancellation`) — см. A6.

## Шаг 3. Отмена «как от кнопки Stop в OpenWebUI» (проверка A5 — чужие стримы)

Запустить **два** стрима одной модели (разные `X-Request-Id`), затем отменить только один:

```bash
curl -s -X POST $LB/api/cancel -H 'Content-Type: application/json' \
  -d "{\"request_id\":\"livecheck-cancel-1\"}" | jq '.results'
```

Ожидание по коду: `by: id`, `cancelled: 1`, второй стрим продолжается.
Проверка «по модели» (то, что делает R184 при обрыве):

```bash
# запустить два стрима заново, затем:
curl -s -X POST $LB/api/cancel -H 'Content-Type: application/json' -d "{\"model\":\"$MODEL\"}" | jq '.results'
```

Если в `cancelled` окажется **2** — подтверждён A5: отмена по модели гасит всех пользователей
(`active_generations.go:126` при пустом `userID`). Это же произойдёт при простом обрыве клиента,
т.к. R184 шлёт только `{"model":…}` (`cancel_upstream_r184.go:48-52`).

## Шаг 4. Отмена разрывом соединения (механизм R184)

```bash
kill -INT $STREAM_PID    # имитируем уход клиента
sleep 3
curl -s $CW/api/infer/active | jq '.count'      # ожидание: 0 (иначе отмена не дошла)
docker logs ol-stack-balancer --since 1m 2>&1 \
  | grep -E 'client cancelled, aborting stream|cancel upstream:'
docker logs ol-stack-cppworker-gpu --since 1m 2>&1 | grep -E 'ActiveGenerations: cancelled|AbortWatcher'
```

Ожидание: балансер — `proxyRequestLlamaCpp: client cancelled, aborting stream` и
`cancel upstream: генерация отменена на воркере` (`reason: client_cancel`).
Воркер — `ActiveGenerations: cancelled by model` (R184 шлёт именно `model`).

## Шаг 5. Проверка «отмена не дошла» — дерево решений

| Что видно | Причина | Куда смотреть |
|---|---|---|
| `status: no_active_generation`, в `results[]` `cancelled: 0` | приказ доставлен, но имя не совпало (L1) | сравнить `model` в ответе `/api/infer/active` (что зарегистрировал воркер) с тем, что вы послали; см. `active_generations.go:126` |
| `targets: []` (пусто) | нет здоровых llama.cpp-узлов (L2/L5) | `curl $ADM/api/v1/cluster \| jq '[.backends[]\|{id,status,backendType}]'` |
| `accepted: []`, `results[].ok: false` | воркер не ответил за 2 с (L7) | `docker logs ol-stack-cppworker-gpu`; проверить `curl -s $CW/api/version` |
| воркер: `handleCancel: completed` со `cancelled: 1`, но `count` не падает | флаг не действует: stub-сборка (L8) или batch в полёте при раскладке (L9) | `docker logs … \| grep -i 'AbortWatcher'`; `curl -s $CW/api/v1/cppworker/rpc-peers \| jq` |
| `count` падает, но стрим клиента продолжается | балансер не эмитит терминатор | `docker logs ol-stack-balancer \| grep 'stream ended'`; см. `llamacpp_transport.go:2229-2281` |
| отмена «не действует» во время загрузки модели | L4 — документированное поведение | `curl -s $CW/api/models/load/progress \| jq`; `lazyload.go:700-703` |
| `404 request_id not found` | не тот `request_id` (L11) | `curl -s $CW/api/infer/active \| jq '.generations[].request_id'` |

## Шаг 6. Проверка, что счётчики не врут (или врут)

```bash
# до отмены
curl -s $CW/metrics | grep -E 'cppworker_(requests|errors)_total' | grep "$MODEL"
# отменить генерацию (шаг 2), затем
curl -s $CW/metrics | grep -E 'cppworker_(requests|errors)_total' | grep "$MODEL"
```

Ожидание по коду: `requests_total` **вырос** на 1, `errors_total` **не изменился**
(`backend.go:3657-3662`). Если вы ждали «отмена = ошибка» — в этой системе это не так, и это
намеренно (комментарий `:3649-3656`).

## Шаг 7. Проверка второго блока (метрики «записано ↔ показано»)

```bash
# что балансер реально отдаёт по метрикам llama.cpp (P1)
curl -s $ADM/api/v1/cluster -H "X-API-Token: $TOK" \
  | jq '.backends[]|select(.backendType=="llama_cpp")|.llamaCpp|{
      maxVramNCtx,vramKnown,availableVramMb,totalVramMb,
      vramLocalFreeMb,vramRemoteFreeMb,vramRemoteNodes,vramRemoteSource,
      rpcServersActive,rpcNode,modelMaxContext,
      loaded:[.loadedModels[]|{name,state,contextLength,context_per_seq,
              device_layers_exact,numGpuLayers,parallel,maxSlots,batchSize,
              kvCacheType,flashAttnType,useMmap,quantization,loadedAt,
              vramUsage,ramUsage}],
      loading:[.loadingModels[]|{name,state,loadingStartedAt,loadingSizeBytes,loadingError}]}'
```

Ключи, которые будут **непустыми** в ответе, но **не видны** ни в одной панели: `maxVramNCtx`,
`availableVramMb`, `totalVramMb`, `vramLocalFreeMb`, `vramRemoteFreeMb`, `vramRemoteNodes`,
`rpcServersActive`, `modelMaxContext`, `loadedAt`, `vramUsage`, `ramUsage`, `loadingSizeBytes` —
это и есть подтверждение раздела 2.5.

Ключи, которых в ответе **нет** (или они `0`), хотя WebUI их читает: `activeRequests`,
`requestsPerSecond`, `avgResponseTime`, `freeSlots`, `availableSlots`, `maxConcurrentReqs`,
`gpuInfo`, `availableModels` — подтверждение раздела 2.6.

Показать «сломанное имя причины загрузки» (M1) на живом стенде:

```bash
# намеренно сломать загрузку (несуществующий файл / недостаточная память), затем:
curl -s $ADM/api/v1/backends -H "X-API-Token: $TOK" \
  | jq '[.backends[]|{id, loading:[.loadingModels[]|{name,state,loadingError,error}]}]'
```

`loadingError` будет заполнен, `error` — пуст; WebUI читает `error` (`renderers.js:1216`) и покажет
заглушку.

---

# Не проверено

1. **Живой прогон не выполнялся.** Все утверждения — чтение кода; ни одна команда из чек-листа в
   этой сессии не запускалась (нет доступа к стенду из этой среды).
2. **Фактическая задержка отмены** на конкретной сборке llama.cpp (обещание «один batch»,
   `bridge.c:2968-2969`) не измерялась — зависит от модели, размера batch и наличия раскладки.
3. **Поведение `docker pause`** воркера посреди `llama_decode`: флаг ставится, но C-код не
   возвращается → отмена «зависает» до `resume`. Проверяется только на живом стенде.
4. **Корректность отмены загрузки модели** (`RequestLoadAbort`, `bridge.c:3384-3419`): код
   существует, но `lazyload.go:700-703` прямо говорит, что явный `/api/cancel` загрузку не
   прерывает. Сценарий «отмена во время загрузки для пути auto-load балансера» не прослежен
   до конца: авто-загрузку инициирует балансер (`model_management.go`), и там свой путь отмены —
   он вне области этого аудита.
5. **`v1/*` (OpenAI) поверхность**: отмена приходит только через hijack-путь
   (`proxy_request_hijack.go:462-466`); плагины/клиенты, которые закрывают SSE-соединение не сразу,
   не проверялись.
6. **Image-воркер** (`image_cpp`): его блок метрик (`backend.image`) в эту сверку не входил, хотя
   в WebUI он читается (`renderers.js:1368-1401`).
7. **Мульти-узловая раскладка (RPC)**: доставка отмены внутрь `rpc-server`-процесса не проверялась;
   известно только, что отмена проверяется между batch'ами (`bridge.c:2838`, `:2970`) и что
   `handlers_cancel.go:87-90` содержит подсказку про keepalive/реконнект узла 50052.
8. **`/api/v1/gguf/backends` ↔ `gguf-renderer-*`**: сверка сделана по коду; какие именно ключи
   реально приходят в браузер при живой модели — не снималось (нет доступа к UI-сессии).
9. **`webui/js/modules/renderers.js`** читается частично: файл 2800 строк, сверены только блоки,
   работающие с метриками llama.cpp (`:184-350`, `:515-677`, `:1085-1160`, `:1193-1230`,
   `:1296-1470`, `:1496-1577`, `:2600-2640`). Остальные блоки (сессии, очередь, GPU-кластер)
   не проверялись на предмет чтения метрик llama.cpp.
10. **Профили моделей и `parameters`** (R119): путь «происхождение параметров» прослежен только до
    отображения в GGUF-карточке (`gguf-renderer-detail-render.js:892-918`); какие ещё панели его
    читают, не проверялось.

---

# Приложение: журналы одной строкой (для runbook'а)

```bash
# Балансер: всё, что относится к отмене
docker logs ol-stack-balancer --since 30m 2>&1 \
  | grep -E 'client cancel:|cancel upstream:|client cancelled, aborting stream|streamTimeout fired|stream ended by client|upstream stalled|admission: client canceled'

# Воркер: приём приказа, снятие с трекера, взведение флага
docker logs ol-stack-cppworker-gpu --since 30m 2>&1 \
  | grep -E 'handleCancel: completed|ActiveGenerations: cancelled|AbortWatcher: per-infer abort|cancelled active generations'

# Проверка «что было активно в момент отмены» (без логов)
curl -s http://127.0.0.1:18092/api/infer/active | jq
```

## Исправлено после аудита

- **M1 (loadingError ↔ error) — исправлено в R345.** Причина провала загрузки
  модели пишется в JSON как `error` (`pkg/types/llama_cpp_metrics.go` и DTO
  `internal/api/handlers_cluster_models.go`), то есть под тем именем, которое
  читает WebUI. Прежнее `loadingError` убрано, чтобы не было двух источников
  правды. Тесты: `pkg/types/llama_cpp_metrics_loading_error_r345_test.go`.