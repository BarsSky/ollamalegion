# Аудит AUTO-PULL и ретраев в `Proxy.ServeHTTP` (2026-10-30)

**Статус:** только чтение кода. Правок в код НЕ вносилось.
**Область разбора:** `internal/balancer/proxy.go` (`ServeHTTP`, 896–1664) и связанные с ним
`auto_pull.go`, `proxy_request.go`, `llamacpp_backend_helpers.go`, `autoload_wait*.go`,
`loading_retry.go`, `model_management.go`, `rpc_layout_failure_r118.go`, `stale_model_snapshot_r160.go`,
`unified_queue_r73.go`, `admission_queue.go`, `load_backoff.go`, `slot_manager.go`, `router.go`.
**Каждое утверждение снабжено ссылкой `файл:строка`.**

---

## 0. Резюме: что нашлось (по убыванию важности)

| # | Находка | Где |
|---|---|---|
| F1 | При **выключенном** auto-pull (это дефолт: `"enabled": false`) ответ 404 «model not found» от Ollama попадает в **общую** ветку обработки ошибки: бэкенд помечается `unhealthy`, `scheduleRecoveryCheck` — пустая функция, клиент получает **503 «all backends failed» вместо 404**. Одна несуществующая модель выносит живой узел из пула. | `proxy.go:1563-1564`, `proxy.go:1613-1633`, `proxy.go:1639-1663`, `backend_registry.go:544-577`, `backend_registry.go:630-632`; дефолт конфига — `config/config.bundled.json:40-45`, `config/config.json:44-49` |
| F2 | `AutoPullManager.EnsureModel(model string)` **не принимает контекст** и не проверяет отмену клиента: отключение клиента не разблокирует ни `ServeHTTP`, ни горутину. `apm.httpClient` создан **без `Timeout`** (`&http.Client{}`), поэтому закачка может висеть неограниченно. Единственный кап — `PullTimeout`, который по умолчанию пуст (`""` ⇒ без капа). | `auto_pull.go:126-206`, `auto_pull.go:188-205`, `auto_pull.go:47-53`, `auto_pull.go:435-463` |
| F3 | `RetryCount` («сколько раз повторить запрос после загрузки») **нигде не читается**: только валидируется/экспортируется. Повторных попыток pull-а в коде нет вообще — ровно одна попытка на модель. После успешного pull выполняется ровно один повтор исходного запроса. | `auto_pull.go:63-65`, `auto_pull.go:84-86`, `pkg/types/balancing.go:265`; повтор — `proxy.go:1590-1598` |
| F4 | `CleanupStalePulls` **не вызывается в продакшене** (только из теста). Запись в `activePulls`, оставшаяся после таймаута ожидания, живёт вечно и навсегда переводит модель в режим «pull уже идёт» для всех последующих запросов. | `auto_pull.go:200-205` (оставляем в фоне), `auto_pull.go:465-478`; вызовов в коде нет (grep: только `auto_pull_test.go:232`) |
| F5 | Auto-pull ходит по **`backend.OllamaPort` напрямую**, а не через `getBackendPort` (который для llama.cpp отдаёт `CppWorkerPort`/18092). `findBestBackendForPull` **не фильтрует по типу бэкенда**, поэтому может выбрать llama.cpp-узел и отправить `POST http://host:0/api/pull` (или на чужой порт). | `auto_pull.go:221-223`, `auto_pull.go:340-410`; ср. `backend_state.go:53-73` |
| F6 | Auto-pull существует **только на generic Ollama-пути**. На llama.cpp-пути 404 отдаётся раньше, чем сработал бы детектор «model not found», и до `AutoPull` дело не доходит — две поверхности одного стенда ведут себя по-разному. | `proxy_request.go:245-309` (ранний 404), `proxy_request.go:574-609` (детектор), `proxy.go:1563-1611` (ветка pull) |
| F7 | Несколько веток ожидания загрузки **без капа по умолчанию**: цикл «кто-то уже грузит» (200 мс, бесконечно), финальный цикл после успешного load (200 мс, бесконечно), `waitForModelLoad` с продлением дедлайна по прогрессу. Кап — только явный opt-in `LB_ALLOW_LOAD_WAIT_CAP_SEC`. Всё это время запрос держит слот/соединение. | `llamacpp_backend_helpers.go:864-900`, `:1216-1256`, `:724-744`; `autoload_wait.go:293-377` |
| F8 | **Read-only запрос слот не берёт — но освобождает чужой.** Для read/mgmt путей `acquiredBackend` выставляется без `tryAcquireSlot`, а `defer releaseAcquired()` вызывает `releaseSlot`, который декрементирует `ActiveReqs`, если он > 0. На занятом бэкенде read-only запрос «съедает» слот живого inference-запроса (пере-допуск). | `proxy.go:1362-1376`, `proxy.go:1342-1355`, `proxy.go:1514-1517`, `slot_manager.go:79-96` |
| F9 | Read-only/инференс-ветки, которые ходят в сеть на каждый запрос: live-probe копий R303 (до 4 с, кэш 3 с), `backendReachable` (2 × 4 с), `queryCppWorkerModels` (до 3 попыток). Для read/mgmt R303 отключён явно, но остальные проверки живут внутри auto-load-гейта. | `live_copy_probe_r303.go:57-62`, `proxy.go:1317`; `llamacpp_backend_helpers.go:337-380`, `:533-548` |

---

## 1. Ветки, где балансер САМ инициирует загрузку/скачивание и повторяет запросы

Формат таблиц: **ветка → условие → куда ходит → сколько раз / таймаут → что видит клиент → риск**.

### A. `ServeHTTP` (Ollama-поверхность и общий план)

| Ветка | Условие | Куда ходит | Сколько раз / таймаут | Что видит клиент | Риск |
|---|---|---|---|---|---|
| **A1. Прогрев/выбор бэкенда для read/mgmt** (`skipWarmup`) | `targetBackend == ""` и `isReadOnlyOrMgmtEndpoint(path)` (`proxy.go:1282-1292`) | Только кэш метрик: `findModelOnAnyBackendNoVRAMCheck`, иначе `selectAnyHealthy` | 0 сетевых запросов | Ответ проксируется от выбранного бэкенда | Загрузки/скачивания НЕ инициирует — ветка именно для этого и сделана (`router.go:281-324`) |
| **A2. `selectBackendPreferWindow`** | `targetBackend == ""`, путь не read-only (`proxy.go:1293-1297`) | Кэш метрик + `checkResourceLimits` | 0 сетевых | — | — |
| **A3. Live-probe копий R303** | `targetBackend != ""`, `model != ""`, путь **не** read/mgmt, и «модель отсутствует по кэшу» (`proxy.go:1317-1318`) | `GET /api/models` **на каждый узел** (параллельно, `live_copy_probe_r303.go:95-180`) | Предел 4 с на весь опрос, кэш результата 3 с (`live_copy_probe_r303.go:57-62`) | Как повлияет на выбор; клиент видит только итог | Добавляет до 4 с задержки к запросу; при недоступном узле — «модель есть/нет» решается по неполному опросу (флаг `LB_LIVE_COPY_PROBE`) |
| **A4. Повтор выбора слота (10 альтернатив)** | Не удалось `tryAcquireSlot(targetBackend)` (`proxy.go:1379-1399`) | Память (`tryAcquireSlot` — только счётчики, `slot_manager.go:18-76`) | До 10 попыток, без сети и пауз | — | Не зацикливается (жёсткий `maxRetries = 10`, `proxy.go:1381`) |
| **A5. Guard «окно не вырастет» R301** | Слот не взят, есть тело, `growthVerdictR301(...).Defer` (`proxy.go:1417-1439`) | Кэш метрик (`backendWithLoadedModelR301`) | 0 сетевых | `503` + `X-NCtx-Reload-Decision: context-growth`, стандартное «отложено» (`writePreflightDeferredR299`, `proxy.go:1434-1435`) | Быстрый отказ вместо 900 с в очереди — корректно |
| **A6. Read/mgmt без слота** | `readOnlyOrMgmt && acquiredBackend == ""` → `503 no healthy backends for read endpoint` (`proxy.go:1444-1448`) | — | — | `503` текст/plain | Форма ошибки не JSON (ср. R332 `router.go:133-158`) |
| **A7. Единая admission-очередь** | `acquiredBackend == ""` и путь не read-only (`proxy.go:1450-1511`) | Память (очередь), `waitForInferenceBackend` (`unified_queue_r73.go:112-189`) | Бюджет = `max(LB_ADMISSION_WAIT_SEC=300s, LB_AUTO_LOAD_WAIT_SEC=180s)` (`admission_queue.go:87-100`, `:127-136`); внутри — до 10 повторных выборов бэкенда (`unified_queue_r73.go:77-88`) | `X-Queue-Wait-Ms`, `X-Queue-Position`; при исчерпании — `503` (`writeAdmissionUnavailable`, `proxy.go:1499`); при переполнении — `503`+`Retry-After: 5` (`proxy.go:1494-1495`) | Ждёт до 300+ с; «пустой» 503 в остальных ветках — см. A11 |
| **A8. 503 от бэкенда на **streaming** запрос → `BackendBusyError`** | `resp.StatusCode == 503` и `isStreamingResponse(resp)` (`proxy_request.go:612-626`) | Повтор `proxyRequest` на 1–2 других бэкендах (`proxy.go:1520-1560`) | Итого до 3 обращений к воркерам (1 + `attempt=1,2`, `proxy.go:1535`); пауз нет | `503 Service unavailable - all backends busy` (текст/plain, `proxy.go:1558`) | Второй ответ не дописывается: гард `clientAlreadyCommitted` (`proxy.go:1529-1533`, `proxy.go:2082-2087`) |
| **A9. 503 от бэкенда на **non-stream** запрос** | `resp.StatusCode == 503` и не streaming (`proxy_request.go:627-641`) | Никуда — passthrough | 0 повторов | Тело воркера + `Retry-After: 3` | Разное поведение streaming/non-streaming на одну и ту же причину |
| **A10. AUTO-PULL (скачивание)** | `errors.As(err, &ModelNotFoundError)` **и** `p.AutoPull != nil` **и** `model != ""` (`proxy.go:1563-1571`) | `AutoPullManager.EnsureModel` → `POST http://<host>:<OllamaPort>/api/pull` c `{"name":..,"stream":false}` (`auto_pull.go:209-252`) | **1 попытка** (`RetryCount` не реализован); ожидание до `PullTimeout`, по умолчанию — **без капа** (`auto_pull.go:435-463`); клиент `http.Client{}` без `Timeout` (`auto_pull.go:53`) | Успех: повтор исходного запроса на выбранном бэкенде → 200 или `500 Model '%s' pulled but request failed` (`proxy.go:1592-1598`). Нет слота → `503 Model '%s' pulled but no free slot` (`proxy.go:1599-1603`). Провал → **`404`** текстом `Model '%s' not found on any backend and auto-pull failed: ...` (`proxy.go:1604-1609`) | Зависание на всё время закачки (F2); мёртвая запись `activePulls` (F4); выбор чужого узла/порта (F5); форма ответа — plain text, не JSON (ср. R332) |
| **A11. Общая ошибка бэкенда → «unhealthy» + повторы** | Любая ошибка, не 503-busy и не model-not-found; **а также model-not-found при `AutoPull == nil`** (`proxy.go:1613-1637`) | `UpdateBackendStatus(Unhealthy)` (память) + до 2 повторов на других бэкендах (`proxy.go:1639-1661`) | Итого до 3 обращений; `scheduleRecoveryCheck` — no-op (`backend_registry.go:630-632`) | `503 Service unavailable - all backends failed` (`proxy.go:1663`) | **F1**: 404 «model not found» при выключенном auto-pull = unhealthy + 503 вместо 404 |
| **A12. Ошибка соединения внутри `proxyRequest` (Ollama, streaming)** | `client.Do` вернул ошибку, тип не `context_canceled/deadline` (`proxy_request.go:454-493`) | Повтор того же запроса на тот же бэкенд | 1 повтор, пауза `getStreamingRetryDelay()` = 500 мс (`proxy_timeout.go:135-141`) | Либо ответ, либо ошибка дальше по A11 | Клиент не знает о повторе; при небуферизованном теле — риск дубля (тело восстанавливается из `bodyBuf`, `proxy_request.go:464-466`) |
| **A13. Клиент отвалился** | `isClientDisconnectErr(err)` (`proxy.go:1619-1624`) | — | — | Ответ не пишется (писать некому) | Бэкенд НЕ помечается unhealthy (R194) — корректно |

### B. llama.cpp-путь: auto-load и его ретраи (вызывается из `routeRequest` → handler'ы, и из `proxyRequest`→R325-гейт)

| Ветка | Условие | Куда ходит | Сколько раз / таймаут | Что видит клиент | Риск |
|---|---|---|---|---|---|
| **B1. R325-гейт в generic-прокси** | Модель не пуста и `p.llamaCppRouter != nil` (`proxy_request.go:245-309`) | `ensureModelLoadedOnBackend` (+ `ResolveNumCtx`) | См. B2–B6 | 404 при «модели нет» (`proxy_request.go:275-284`), 503 при недоступном воркере (`:286-294`), иначе ожидание `waitModelReadyForAutoLoad` | Гейт стоит ДО обращения к воркеру — но после admission-очереди |
| **B2. Отказ по disabled-профилю** | `GetModelProfile(model).Disabled` (`llamacpp_backend_helpers.go:796-803`) | Никуда | 0 | Через handler: `writeServiceUnavailable(...)` (`llamacpp_handlers_inference.go:209-211`) | Breaker не трогается — правильно |
| **B3. «Воркер недоступен»** | `!backendReachable(backendID)` и загрузка не идёт (`llamacpp_backend_helpers.go:815-835`) | `GET /api/version`, fallback `GET /api/models` на этом узле (`:384-402`) | 2 × 4 с (`:337-380`) | `503 ... backend unreachable ... (cppworker did not answer /api/version within 4s)` | 8 с на «мёртвый» узел до failover |
| **B4. Модель уже загружена** | `isModelReadyOnBackend` (`:837-843`) | `GET /api/models` (`:518-611`) | 1 запрос (до 3 попыток при ошибке, 100/200 мс, `:546-548`; клиент 5 с, `:533-534`) | Нормальный inference | Горячий путь: минимум 1 HTTP-опрос на каждый запрос (комментарий `proxy_request.go:241-244`) |
| **B5. Ожидание чужой загрузки / reload** | `IsReloadPending` (`:847-862`) или `state=loading` в `/api/models` (`:864-872`) | `WaitReloadDone` (память) + поллинг `/api/models` каждые 200 мс (`:876-899`) | **Без капа** по умолчанию; проверка `loadWaitDeadline()` только при явном `LB_ALLOW_LOAD_WAIT_CAP_SEC` (`:724-744`); выход — `state=error`/`loaded` | 503 «load started, waiting…» (`:1139`) либо ожидание | **Потенциальное зависание**: единственный выход — терминальное состояние у воркера. Слот/соединение заняты всё это время |
| **B6. Circuit breaker загрузки** | 3 подряд провала за 5 мин (`load_backoff.go:35-41`, `:101-139`) | Никуда | Решение мгновенное; breaker открыт 60 с | `503 ... circuit breaker open ... (until ...)` + `Retry-After: 60` (`proxy_backoff` через `writeAutoLoadRetryAfter`, `proxy_helpers.go:314-317`) | Защита от crash-loop (заявлено `load_backoff.go:1-13`) |
| **B7. Async-загрузка + ожидание** | `lbAutoLoadAsync` (дефолт `true`, `proxy.go:508`) (`llamacpp_backend_helpers.go:982-1140`) | `ModelManager.ExecuteOperation` → `POST /api/models/load` (`model_management.go:1525-1528`) в горутине; затем `waitForModelLoad` | Бюджет `autoLoadWaitTimeoutForModel`: `max(180s, size/20MB/s)`, потолок 24 ч (`autoload_wait.go:38`, `:121-158`); опрос `/api/models/load/progress` каждые 2 с, дедлайн **продлевается** при росте `elapsedMs` (`:316-326`) | Успех → модель обслуживается; иначе `503` + `Retry-After` 15/60/90 (`proxy_helpers.go:293-320`) | Ожидание может быть очень долгим по дизайну; при отмене клиента — выход по `ctx` (`autoload_wait.go:294-296`, `:354-357`) |
| **B8. Retry при 503 «model is loading» от воркера (загрузка)** | `executeLlamaCppLoadOnce`: 503 + `loading` (`model_management.go:1757-1780`) | Повтор `POST /api/models/load` на тот же узел | **10 попыток × 3 с** (`model_management.go:820-822`, `:1759-1767`), затем 202-«ещё грузится» (`:1776-1779`) | 202/ожидание, не 5xx | До 30 с дополнительного ожидания |
| **B9. Транспортный retry 503 «model is loading» (llama.cpp)** | `loadingSignalFromBody` = true (`loading_retry.go:60-95`) в `llamacpp_transport.go:462-475` / `llamacpp_transport_nonstream.go:336-404` | Поллинг `/api/models/load/progress?model=` (llama.cpp) или `/api/ps` (ollama), затем повтор исходного запроса | 60 попыток × 500 мс = **30 с** (`loading_retry.go:39-50`), клиент опроса 2 с (`:49`) | При исчерпании — `503` + `Retry-After: 5` + `X-Model-Loading-Retry: exhausted` (`llamacpp_transport_nonstream.go:387-404`) | **404 от `/progress` трактуется как «загружено»** (`loading_retry.go:226-231`) — может дать преждевременный повтор |
| **B10. Retry при connection-level ошибке (llama.cpp)** | `isConnectionLevelError` (`llamacpp_transport.go:377-406`, `llamacpp_transport_nonstream.go:256-296`) | Повтор того же запроса на тот же узел | 3 попытки, паузы 1/2/4 с | Ошибка формата `llama.cpp request failed [backend=.., attempt=.., error_type=..]` | +7 с к «мёртвому» узлу; `markBackendConnectionFailed` при `connection_refused` (`proxy_request.go:134-141`, `llamacpp_transport_nonstream.go:313-315`) |
| **B11. Failover на другую копию модели** | `backendUnavailable || IsConnectionLevelError || errBackendUnreachableR82` (`llamacpp_backend_helpers.go:264-269`), либо есть готовая копия (`:271-277`) | Загрузка на альтернативном узле | До 3 альтернатив (`:227-229`, `:287-305`) | Прозрачно: `X-Backend-ID` меняется (`llamacpp_handlers_inference.go:213-223`) | До 3 × (проверка 8 с + загрузка) — «тихое» многоминутное ожидание |
| **B12. Добор после успешного load** | `ExecuteOperation` вернул `Success`, но `state!=loaded` (`llamacpp_backend_helpers.go:1216-1256`) | `/api/models` каждые 200 мс | **Бесконечно** до `loaded`/`error`; кап — только opt-in | Успех/ошибка загрузки | Зависание при «залипшем» воркере |
| **B13. Фоновая догрузка при «протухшем» снапшоте** | Воркер ответил 502/404 «unknown model» (`stale_model_snapshot_r160.go:74-137`) | `ensureModelLoadedOnBackend` в горутине (`:111-122`) | Без привязки к запросу | `503` + `Retry-After: 15` («load started, retry the request») | Фоновая загрузка может конкурировать с клиентскими (R271/R292-guard, `destructive_guard_r292.go`) |
| **B14. Retry n_ctx-reload** | Внутри preflight/nctx-координатора (`nctx_reload.go:1335-1355`) | Повтор `POST .../reload`/load | 5 попыток × 5 с + поллинг до 10 мин (`nctx_reload.go:1316-1324`) | Зависит от вызывающего пути | +25 с и до 10 мин ожидания в глубине запроса |
| **B15. RPC-layout повтор загрузки** | `shouldRetryLoadWithoutLayout` или узел перестал отвечать (`rpc_layout_failure_r118.go:195-241`) | Повтор `POST /api/models/load` **без** раскладки | +1 полная загрузка (после `WaitBackendReachable`, `:217-221`) | Успех со сноской в `Message` | Удвоение времени загрузки |

### C. `AutoPullManager` (скачивание модели) — детально

| Ветка | Условие | Куда ходит | Сколько раз / таймаут | Что видит клиент | Риск |
|---|---|---|---|---|---|
| **C1. Модель уже есть (running/available)** | `findBackendWithModelReady` нашёл совпадение (`auto_pull.go:132-136`, `:303-336`) | Кэш метрик агентов (`metricsMgr.SnapshotBackendMetrics`) | 0 сетевых | Повтор запроса на найденный бэкенд | Сравнение `strings.Contains(m.Name, model)` (`:318`, `:327`) — ложные совпадения на коротких именах |
| **C2. Dedup (pull уже идёт)** | В `activePulls` есть запись (`:139-155`) | — | Ждёт `Done` до `pullWaitTimeout()`; **без капа** при пустом `PullTimeout` | `404 ... timed out waiting for in-progress pull` при капе | Мёртвая запись (F4) ⇒ вечное «pull уже идёт» |
| **C3. Лимит параллельных закачек** | `len(activePulls) >= MaxConcurrent` (дефолт 3, `:57-59`) | — | Мгновенно | `404 ... max concurrent pulls reached` (`proxy.go:1604-1609`) | Лимит считается по числу записей, включая мёртвые (F4) |
| **C4. Собственно pull** | Есть best-бэкенд (`:164-179`) | `POST http://<host>:<OllamaPort>/api/pull` с `stream:false` (`:209-252`) | **1 попытка**; `http.Client{}` без `Timeout` (`:53`); чтение тела `io.ReadAll` (`:261`) | Ожидание; затем повтор исходного запроса (`proxy.go:1590-1598`) | F2/F3/F5; также `POST` без `ctx` ⇒ нет отмены |
| **C5. Ошибка pull** | Не-200 или ошибка соединения (`:245-284`) | — | — | `404 Model '%s' not found on any backend and auto-pull failed: %v` (`proxy.go:1604-1609`) | Единственный случай, где клиент видит 404 — но и он возможен только при включённом auto-pull |
| **C6. Таймаут ожидания** | `PullTimeout > 0` и истёк (`:200-205`) | Закачка продолжается в фоне | — | `404 ... timed out (pull continues in background)` | Запись остаётся в `activePulls`; очистки нет (F4) |

### D. Сводка бюджетов/таймаутов (для быстрой сверки)

| Параметр | Значение по умолчанию | Файл:строка |
|---|---|---|
| Ожидание авто-загрузки | 180 с, тир `max(180s, size/20MB/s)`, потолок 24 ч | `autoload_wait.go:38`, `:121-158` |
| `LB_AUTO_LOAD_WAIT_SEC` | не задан ⇒ тир; `0` ⇒ не ждать; `<0` ⇒ 24 ч | `autoload_wait.go:92-109` |
| Ожидание слота (admission) | 300 с; бюджет `max(admission, auto-load)` | `admission_queue.go:87-100`, `:127-136` |
| Кап ожидания загрузки | выключен (opt-in `LB_ALLOW_LOAD_WAIT_CAP_SEC`) | `llamacpp_backend_helpers.go:724-744` |
| Кап HTTP-операций с моделью | выключен (opt-in `LB_ALLOW_MODEL_OP_TIMEOUT_SEC`) | `model_management.go:65-74` |
| Кап конкретного load-запроса | выключен (opt-in `LB_ALLOW_MODEL_LOAD_TIMEOUT_SEC`) | `model_management.go:76-82` |
| Ожидание pull | без капа (пусто); `PullTimeout` из конфига | `auto_pull.go:435-463` |
| Повторы `POST /api/models/load` | 10 × 3 с | `model_management.go:820-822` |
| Поллинг `/api/models` при ожидании | 200 мс | `llamacpp_backend_helpers.go:898`, `:1204`, `:1255` |
| Поллинг `/api/models/load/progress` | 2 с | `autoload_wait.go:54` |
| Транспортный loading-retry | 60 × 500 мс = 30 с | `loading_retry.go:39-50` |
| Connection-retry llama.cpp | 3 × (1/2/4 с) | `llamacpp_transport.go:377-406`, `llamacpp_transport_nonstream.go:256-296` |
| Retry streaming на тот же бэкенд (Ollama) | 1 × 500 мс | `proxy_request.go:454-493`, `proxy_timeout.go:135-141` |
| Проверка живости воркера | 2 × 4 с | `llamacpp_backend_helpers.go:337-380` |
| Live-probe копий R303 | 4 с на опрос, кэш 3 с | `live_copy_probe_r303.go:57-62` |
| Circuit breaker загрузки | 3 провала / окно 5 мин; открыт 60 с | `load_backoff.go:35-41` |

---

## 2. Может ли ветка зациклиться, зависнуть или сходить в сеть на read-only запрос

**Зациклиться (бесконечно повторять):**
- Явных бесконечных циклов нет: все циклы повторного выбора бэкенда ограничены (`proxy.go:1381`, `proxy.go:1535`, `proxy.go:1639`, `unified_queue_r73.go:77`), повтор загрузки — 10 попыток (`model_management.go:820-822`).
- Фактически «зацикливание» достигается на уровне клиента: ветки возвращают `503 + Retry-After`, который клиент обязан повторить (A7/A11/B7). При дефолтном конфиге ничто не мешает клиенту повторять вечно.

**Зависнуть (не вернуться без внешнего таймаута):**
1. **A10/C4** — pull без капа и без контекста (`auto_pull.go:53`, `:188-205`). Клиентский `ServeHTTP` не имеет собственного таймаута: единственный «предохранитель» на этом пути — таймаут клиента, который сервер не контролирует.
2. **B5/B12** — циклы ожидания загрузки без капа (`llamacpp_backend_helpers.go:876-899`, `:1224-1256`), плюс `waitForModelLoad` с продлением дедлайна (`autoload_wait.go:316-326`).
3. **B3** — 8 с на каждый «мёртвый» узел на горячем пути (`llamacpp_backend_helpers.go:337-380`).
4. **B10** — +7 с пауз на connection-level ошибку, до 3 раз (`llamacpp_transport.go:377-406`).
5. **A7** — до 300+ с в очереди даже когда ответ «невозможно» уже известен (закрыто только для одного класса — R301, `proxy.go:1417-1439`).

**Сеть на read-only запрос:**
- Прямая загрузка/скачивание на read-only путях **исключена явно**: `proxyReadOnlyRequest` («ни preflight n_ctx, ни захват слота, ни авто-загрузка»), `proxy_helpers.go:510-533`, вызывается из `proxy_request.go:223-227`; плюс `skipWarmup` в `ServeHTTP` (`proxy.go:1275-1297`) и отказ от слота (`proxy.go:1362-1376`); R303-probe для read/mgmt выключен (`proxy.go:1317`).
- **Остаточный риск:** read-only/mgmt путь, который не перехвачен ни `OllamaRouter.Route`, ни `LlamaCppRouter.Route` (например `/api/blobs/sha256:...`, `router.go:314-317`), на **Ollama**-бэкенде идёт через generic `proxyRequest`, где живёт детектор `isModelNotFoundError` (`proxy_request.go:581-600`) и, следовательно, ветка auto-pull (`proxy.go:1563-1611`). Для срабатывания тело 404 должно содержать одновременно «model» и «not found» — для blob-ответов Ollama это маловероятно, но контрактом не запрещено. Для llama.cpp-бэкенда этот путь безопасен (`proxy_request.go:223-227`).
- На read-only путях остаются **обычные** сетевые походы, свойственные проксированию (`proxyRequestToBackend`, `proxy_helpers.go:487-508`) — это ожидаемо.

**Особый случай, найденный при разборе (F8):** read-only запрос не берёт слот, но `defer releaseAcquired()` его **освобождает** — `acquiredBackend` выставлен вручную (`proxy.go:1374-1376`), `releaseAcquired` вызывает `releaseSlot` (`proxy.go:1342-1353`), а `releaseSlot` декрементирует `ActiveReqs`, если он > 0 (`slot_manager.go:87-90`). На бэкенде, где идёт живой inference, read-only запрос уменьшает счётчик занятости, то есть фактически освобождает чужой слот.

---

## 3. Ветки, которые в тестах срабатывают непредсказуемо (объяснение флаков)

1. **`freeTestPorts` — гонка «свободный порт».** Порты получаются через `net.Listen` и сразу закрываются (`proxy_test.go:26-47`); между закрытием и реальным `Listen` порт может занять другой процесс, а параллельные тесты могут получить один и тот же номер. Любой тест, ожидающий «connection refused», при попадании на живой сервис уходит в реальные ретраи (вплоть до 3 × 4 с в `backendReachable`, `llamacpp_backend_helpers.go:337-380`) — ровно этот класс описан в `proxy_test.go:13-25`.
2. **Откат на настоящий порт 11434.** `mustParsePort` при неразобранном URL возвращает `11434` (`auto_pull_test.go:574-584`) — тогда `TestAutoPullExecutePull*`/`TestAutoPullFullScenario` (`auto_pull_test.go:282-327`, `:621-726`) ходят в **реальный Ollama** на машине разработчика. Это прямой источник «зелёно/красно без изменений в коде».
3. **`//go:build llama_stub` — 88 тестовых файлов, включая почти все тесты авто-загрузки:** `autoload_wait_r67a_test.go`, `load_order_test.go`, `failover_r82_test.go`, `llamacpp_async_load_breaker_r66d_test.go`, `llamacpp_async_dedup_wait_r83fix_test.go`, `autoload_wait_tier_r83_test.go`. В дефолтном прогоне `go test ./internal/balancer` их **нет**; при `-tags llama_stub` появляются, и тогда в один прогон попадают ветки с реальными 200 мс/2 с/8 с ожиданиями. Один и тот же коммит ведёт себя по-разному в зависимости от тега.
4. **`TestMain` глушит все ожидания** (`waits_testmain_test.go:30-50`: `LB_AUTO_LOAD_WAIT_SEC=0`, `LB_ADMISSION_WAIT_SEC=0`, `LB_NCTX_PREFLIGHT_WAIT_SEC=0`) и ставит `http.DefaultClient.Timeout = 90s`. Ветки «ждать вместо 503» проверяются только там, где тест сам выставляет env — то есть дефолтное поведение в тестах **не** проверяется, а поведение под нагрузкой (медленный CI) может уехать в другой таймаут.
5. **Env читается на каждом вызове** (`autoload_wait.go:92-109`, `admission_queue.go:87-100`, `live_copy_probe_r303.go:46-51`), при этом в пакете **217** `t.Setenv`. Любой тест, чья фоновая горутина (поллер метрик, таймер) пережил `t.Setenv`-тест, увидит чужое значение. Формально `t.Setenv` и `t.Parallel` несовместимы, но фоновые горутины, созданные `NewProxy` (`proxy.go:599-666`), продолжают читать env после возврата теста.
6. **Тесты, где ожидаемый исход зависит от расписания:** `TestAutoPullEnsureModelTimeout` (таймаут 100 мс, `auto_pull_test.go:549-571`), `TestWaitForModelLoad_LoadingThenLoaded_R67a` (проверяет `elapsed >= 2s`, `autoload_wait_r67a_test.go:140-149` при бюджете 10 с), `TestAutoPullEnsureModelDedup*` (закрытие канала через 50 мс, `auto_pull_test.go:505-515`, `:538-545`). На загруженном CI «успех» и «таймаут» могут поменяться местами.
7. **Фоновые контроллеры `NewProxy`** (`llamaCppMetricsPoller`, `weightTuner`, `imageRes`, `linkMeasure`, `StartAgentTimeoutChecker` — `proxy.go:599-666`) ходят в те же тестовые порты и меняют статусы/метрики параллельно с телом теста. Именно на этом были гонки, уже зафиксированные в комментариях (`auto_pull_test.go:602-614`, `:672-679`).

---

## 4. Тесты (5, детерминированные)

Общие правила: только loopback (`httptest`) либо инъекция `RoundTripper`; никаких внешних адресов; ожидания проверяются по счётчикам вызовов и каналам, а не по «прошло N секунд». Где тест всё же использует таймер (T4), он указан явно.

### T1. `TestAuditReadOnly_NoAutoLoadNoPullAndNoPhantomSlotRelease`

- **Цель:** зафиксировать контракт read-only/mgmt ветки и поймать F8.
- **Подать на вход:** `httptest.NewRequest("POST", "/api/blobs/sha256:deadbeef", body)` — путь есть в `isReadOnlyOrMgmtEndpoint` (`router.go:307-324`), но его не перехватывает ни один роутер (`llamacpp_router.go:100-183`, `ollama_router.go:36-73`), поэтому запрос гарантированно уходит в main flow `ServeHTTP` (`router.go:151-158`).
- **Что замокать:** `httptest`-воркер (llama.cpp-тип), который считает обращения и отвечает `200 {"ok":true}` на `/api/blobs/...`; на `/api/models/load`, `/api/models/load/progress`, `/api/pull` — счётчик + `t.Errorf`.
- **Подготовка состояния:** `ActiveReqs = 1` через существующий helper `proxy.AcquireSlotForTest("backend-1")` (`proxy_test_helpers.go:118-129`) — имитируем живой inference на этом бэкенде.
- **Проверить:** (а) воркер получил ровно 1 запрос и это `/api/blobs/...`; (б) 0 вызовов load/progress/pull; (в) ответ клиенту `200` с телом воркера; (г) **`ActiveReqs` остался `1`** — сейчас тест упадёт, документируя F8 (`proxy.go:1374-1376` + `slot_manager.go:87-90`).
- **Детерминизм:** только loopback, ноль таймеров, состояние читается под `state.mu` (можно через `GetBackendState`, `proxy_test_helpers.go:54-59`).
- **Привязка к коду:** `proxy.go:1282-1292`, `1362-1376`, `1342-1355`, `1514`; `proxy_request.go:223-227`.

### T2. `TestAuditServeHTTP_ModelNotFoundWithoutAutoPull_Is503AndUnhealthy`

- **Цель:** зафиксировать F1 (дефолтная конфигурация: auto-pull выключен).
- **Подать на вход:** `POST /api/generate`, тело `{"model":"missing-xyz","prompt":"hi"}`, контекст с `modelContextKey` — как уже сделано в `auto_pull_test.go:700-712`.
- **Что замокать:** **один** `httptest`-воркер (Ollama) на `backend-1`, отвечающий `404 {"error":"model \"missing-xyz\" not found"}` на `/api/generate`; второй бэкенд убрать (`config.Backends = config.Backends[:1]`, как `auto_pull_test.go:669-670`), иначе `selectBackend` может выбрать другой узел и проверка статуса `backend-1` станет недетерминированной. Обязательно `config.BackendEngine = types.EngineOllamaAPI` (как `auto_pull_test.go:679`), чтобы запрос шёл generic-путём, минуя `LlamaCppRouter.Route` (`llamacpp_router.go:128-133`); `AutoPull` оставить `nil` (дефолт `config.bundled.json:40-45`).
- **Проверить:** `w.Code == 503` (сейчас ожидается именно 503, а не 404); тело содержит `all backends failed`; `GetBackendState("backend-1").Backend.Status == types.StatusUnhealthy`; `/api/pull` не вызывался ни разу (auto-pull выключен).
- **Детерминизм:** два loopback/закрытых порта, никаких пауз; решение принимается до любых таймаутов.
- **Привязка к коду:** `proxy.go:1563-1564`, `1613-1633`, `1639-1663`; `backend_registry.go:544-577`, `630-632`.

### T3. `TestAuditAutoPull_PullRequestShapeAndNoHiddenRetries`

- **Цель:** зафиксировать «куда ходит», «сколько раз» и отсутствие контекста клиента (F2/F3/F5).
- **Подать на вход:** `apm := proxy.AutoPull` при `AutoPullConfig{Enabled:true, MaxConcurrent:3, PullTimeout:"", RetryCount:3}`; вызов `apm.EnsureModel("m1")` (прямо, без ServeHTTP — так тест не зависит от `selectBackend`).
- **Что замокать:** подменить `apm.httpClient` на `&http.Client{Transport: fakeRT}` — поле доступно внутри пакета (`auto_pull.go:38`); `fakeRT` записывает `*http.Request` (метод, URL, тело) и возвращает `200 {"status":"success"}`. Метрики для выбора бэкенда задать через `proxy.SetBackendMetrics` (`proxy_test_helpers.go:35-42`) с `DiskFree` — иначе `findBestBackendForPull` вернёт `""`.
- **Проверить:** ровно **1** вызов транспорта; метод `POST`, путь `/api/pull`, хост/порт = `backend-1`+`OllamaPort`; тело `{"name":"m1","stream":false}`; `RetryCount:3` **не** даёт повторов; у запроса `Context().Deadline()` отсутствует (`ok == false`) — доказательство, что отмена клиента не влияет; `activePulls` очищен после успеха (`auto_pull.go:295-297`).
- **Детерминизм:** подменённый транспорт, ни одного реального соединения, ни одного таймера.
- **Привязка к коду:** `auto_pull.go:53`, `209-252`, `295-297`, `221-223`, `63-65`; `proxy.go:1571`.

### T4. `TestAuditAutoPull_TimeoutLeavesDeadEntryThatBlocksNextPull`

- **Цель:** зафиксировать F4 (мёртвая запись + отсутствие фоновой очистки).
- **Подать на вход:** `AutoPullConfig{Enabled:true, MaxConcurrent:3, PullTimeout:"10ms"}`; `fakeRT`, который **блокируется** на канале `release` (закрывается в `t.Cleanup`). Вызов `EnsureModel("m1")` → ожидаем ошибку «timed out (pull continues in background)». Затем повторный `EnsureModel("m1")` → ожидаем «timed out waiting for in-progress pull».
- **Проверить:** после первого вызова `IsPullInProgress("m1") == true` (запись осталась); второй вызов **не** инициировал новый pull (счётчик транспорта остался `1`); ошибка второго вызова содержит `in-progress pull`.
- **Детерминизм:** единственная зависимость от времени — бюджет 10 мс; ожидаемый исход («таймаут») сохраняется при любом планировщике, а количество обращений к транспорту от времени не зависит. Это компромисс: без правки кода «часы» не инжектируются.
- **Привязка к коду:** `auto_pull.go:200-205`, `139-155`, `465-478`; `proxy.go:1604-1609`.

### T5. `TestAuditServeHTTP_BackendBusyRetryStopsAfterThreeWorkerCalls`

- **Цель:** зафиксировать верхнюю границу повторов на 503 и отсутствие «фантомного» второго ответа.
- **Подать на вход:** `POST /api/chat` с телом `{"model":"m","stream":true}`. Конфиг — только Ollama-бэкенды и `config.BackendEngine = types.EngineOllamaAPI` (как `auto_pull_test.go:679`), иначе `/api/chat` перехватит `LlamaCppRouter.Route` → `handleChat` (`llamacpp_router.go:128-130`). При чисто Ollama-кластере `OllamaRouter.Route` `/api/chat` не обрабатывает (`ollama_router.go:36-73`), а при `bt == ""` `routeRequest` и сам возвращает `false` (`router.go:113-117`) — запрос гарантированно попадает в main flow.
- **Что замокать:** три `httptest`-воркера (все healthy, у всех `MaxConcurrentReqs > 0`, метрики с `DiskFree` выше `Resources.Disk.MinFreeMB`, модель нигде не помечена загруженной ⇒ выбор идёт по fallback-пулу), каждый отвечает `503` с `Content-Type: application/x-ndjson` и телом `{"error":"model is loading: m","loading":true}`; каждый считает свои обращения. Конфиг: `AutoPull` выключен, слоты доступны.
- **Проверить:** суммарно **3** обращения к воркерам (1 первичный + 2 из цикла `attempt<3`, `proxy.go:1535`); ответ клиенту `503` с телом `all backends busy` (`proxy.go:1558`) и ровно **одним** `WriteHeader`; после запроса `ActiveReqs` у всех бэкендов = 0 (слоты возвращены, `proxy.go:1525`, `:1556`).
- **Детерминизм:** `503` приходит сразу, таймауты не задействованы; счётчики — атомарные, порядок бэкендов не важен (проверяется сумма, а не конкретные ID).
- **Привязка к коду:** `proxy_request.go:611-626`, `proxy.go:1520-1560`, `proxy.go:2082-2087`.

**Кандидаты, не вошедшие в набор (по одной строке):** `TestAuditLlamaCpp_ConcurrentLoadWaitOnlyCappedByEnv` (`LB_ALLOW_LOAD_WAIT_CAP_SEC=1`, `llamacpp_backend_helpers.go:864-900`), `TestAuditLlamaCpp_ProgressEndpointFallback` (2 с попытка → `loadWaitExpired` → ошибка `timed out waiting for backend model load`, `llamacpp_backend_helpers.go:864-900`), `TestAuditAutoPull_DedupReturnsCachedFailure` (`drainLoadFailure`/`EnsureModel` returning cached error, `auto_pull.go:146-154`), `TestAuditAutoPull_SelectorMayPickLlamaCppNode` (F5: `findBestBackendForPull` без фильтра типа, `auto_pull.go:340-410`).

---

## 5. Не проверено

1. **Реальное поведение Ollama на 404 `/api/generate` в streaming-режиме.** Ветка auto-pull требует `!isStreamingResponse(resp)` (`proxy_request.go:578-581`); `isStreamingResponse` смотрит заголовки ответа/`Transfer-Encoding`/`ContentLength == -1` (`proxy_request.go:1120-1153`). Если реальный Ollama отдаёт ошибку чанками, ветка auto-pull **не сработает** для streaming-клиентов. Живой замер не проводился.
2. **Частота срабатывания `/api/blobs/*` → generic `proxyRequest` → детектор 404** (раздел 2): контракт тела 404 у Ollama не проверялся, поэтому вероятность ложного auto-pull на read-only запрос оценена теоретически.
3. **Влияние F8 на прод:** не измерялось, сколько раз read-only запросы реально приходят на загруженный бэкенд и уменьшают `ActiveReqs`. Нужен лог/метрика `releaseSlot`, которых сейчас нет.
4. **`pullWaitTimeout()` и nil-канал:** анализ сделан по коду (`auto_pull.go:435-443`); отдельный тест на «nil никогда не срабатывает» в наборе не приводится, т.к. он требует таймаута-свидетеля.
5. **`descriptive_guard_r292`/`destructive_guard` взаимодействие с auto-pull:** auto-pull идёт через `POST /api/pull` (`auto_pull.go:223`), минуя `ModelManager.ExecuteOperation`, значит guard `guardModelOpR292` (`model_management.go:334-349`) к нему не применяется — не проверялось, ожидаемо ли это (закачка не выгружает модель).
6. **Взаимные блокировки:** `findBestBackendForPull` вызывается под `apm.mu` и берёт `proxy.mu.RLock` + `state.mu` (`auto_pull.go:339-410`, `:139-179`); обратного порядка (`proxy.mu` → `apm.mu`) в прочитанном коде не найдено, но полноценного анализа локов не делалось.
7. **Сетевые/поведенческие утверждения о registry (HF/Ollama) и о `stream:false`:** тело pull формируется как `{"name":"<m>","stream":false}` без экранирования имени (`auto_pull.go:226`) — для имени с кавычкой JSON станет невалидным; не проверялось на живом Ollama.
8. **Сколько именно тестов падает сейчас:** прогон `go test ./internal/balancer` (в т.ч. с `-tags llama_stub`) в рамках этого аудита не выполнялся — только чтение кода.

---

## Приложение: какие «швы» уже есть для детерминированных тестов (код не менялся)

| Шов | Что даёт | Где |
|---|---|---|
| Поле `AutoPullManager.httpClient` | подмена транспорта для pull без сети | `auto_pull.go:38` |
| `proxy.metricsMgr.metrics` + `SetBackendMetrics` | предсказуемый `findBestBackendForPull`/`findBackendWithModelReady` | `proxy_test_helpers.go:35-42`, `auto_pull_test.go:248-264` |
| `httptest.NewServer` + `config.Backends[i].OllamaPort/CppWorkerPort` | «воркер» с любым поведением | `auto_pull_test.go:285-309`, `loading_retry_test.go:280-309` |
| `config.BackendEngine = types.EngineOllamaAPI` | не уходить в llama.cpp-ветку без обнуления роутера | `auto_pull_test.go:672-679` |
| `AcquireSlotForTest` / `GetBackendState` | подготовка и чтение состояния слотов | `proxy_test_helpers.go:118-129`, `:54-59` |
| `loadBackoff.Reset/ResetAll` | изоляция circuit breaker между тестами | `load_backoff.go:157-194` |
| `resetLiveCopyProbeCacheR303()` | изоляция глобального кэша live-probe | `live_copy_probe_r303.go:75-80` |
| `t.Setenv("LB_ALLOW_LOAD_WAIT_CAP_SEC", ...)` | превратить «вечное» ожидание в проверяемый таймаут | `llamacpp_backend_helpers.go:724-744` |
