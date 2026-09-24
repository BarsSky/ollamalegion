# Admission-очередь и per-user сессии в балансере (R67b)

Статус: **РЕАЛИЗОВАНО** (R67b, коммит `R67b: admission-очередь вместо 503 +
различение пользователей OpenWebUI`, образ `ollama-legion/balancer:r67-submodule-v1`).

## Что сделано (кратко)

* `internal/balancer/admission_queue.go` — очередь ожидания слотов:
  `LB_ADMISSION_WAIT_SEC` (default 300, 0 = прежнее «сразу 503»),
  `X-Queue-Position` / `X-Queue-Wait-Ms` / `X-Queue-Wait-Max-Sec`, 503 только
  как последний рубеж (+ `Retry-After`), `releaseSlot` будит очередь
  broadcast'ом, per-session inflight + окно справедливости 30 c.
* Интеграция: `acquireInferenceAdmission` в трёх inference-хендлерах llama.cpp
  (`handleChat`, `handleGenerate`, `handleOpenAIChatCompletions`) — слот берётся
  на всё время запроса и освобождается в defer.
* Идентичность: `requestUserIdentity` (X-User-Id / X-OpenWebUI-User-Id /
  X-User-Email), `requestBodyIdentity` (user/user_id, chat_id/session_id, в т.ч.
  вложенный `metadata` — так помечает запросы OpenWebUI), `getSessionIDWithModel`
  строит `usr:<user>::<model>[::chat:<chat>]`, `RequestSessionKey` расширен.
* Наблюдаемость: `/api/v1/queue/stats` → блок `admission` (`enabled`, `waiting`,
  `sessions`, `waiting_by_backend`, `served_total`, `timeout_total`,
  `avg_wait_ms`, `wait_max_sec`, `active_sessions`).
* Тесты: `internal/balancer/admission_queue_r67b_test.go` (7 тестов, включая
  HTTP-уровень) + `TestMain` в `./tests` выключает ожидания для интеграционных
  тестов.

## Жалобы (R67, дословно)

* «проблема на стороне балансера, он резко отбивает повторный запрос от клиента
  выдавая 503, а не поставляя в очередь, скорей всего причина в том что не
  различает разных пользователей клиента OpenWebUI, что может предоставлять
  модель сразу нескольким пользователям одновременно»;
* «даже если от одного пользователя идет новый запрос, балансер его не ставит в
  очередь на отработку и не делает как новую сессию».

## Диагноз (подтверждён чтением кода, R67a)

1. **Слоты без очереди.** Inference-хендлеры llama.cpp выбирали бэкенд и сразу
   проксировали запрос, НЕ беря слот: `tryAcquireSlot` вызывался только из
   общего flow `Proxy.ServeHTTP` и частично из `QueueManager`. Поэтому
   `maxConcurrentReqs` (в живом стеке 4) на llama.cpp-путь не влиял вообще:
   `activeRequests` оставался 0 при шести параллельных генерациях, а всё лишнее
   ждало ВНУТРИ cppworker без очереди и видимости у балансера.
2. **Пользователь не различался.** `getClientFingerprint` шёл по цепочке
   X-Client-ID → Authorization → X-Session-ID → IP+UA. OpenWebUI проксирует всех
   пользователей с одного IP, с одним User-Agent и ОДНИМ API-токеном, поэтому
   fingerprint совпадал. Замер на живом стенде: три запроса от трёх разных
   `X-User-Id` дали ОДНУ сессию (`fp:…::curl/8.21.0::<model>::chat`,
   `requestCount=3`).
3. **n_parallel не согласован** с `maxConcurrentReqs` бэкенда (см. «Осталось»).

## Проверено на живом стенде (образ r67-submodule-v1)

* 3 параллельных `/api/chat` с разными `X-User-Id` → **3 отдельные сессии**
  (`usr:<hash>::gemma-4-E4B-it-Q4_K_M`, `count=1` каждая) вместо одной с count=3.
* 10 параллельных запросов при `maxConcurrentRequests=4` →
  `admission.waiting=2`, `sessions=[X-User-Id:r67b-q-user-6,
  X-User-Id:r67b-q-user-8]`, оба получили **200** с `X-Queue-Position` 1/2 и
  `X-Queue-Wait-Ms` 2526/5502; `served=2`, `avg_wait_ms=4014`;
  `activeRequests` доходил до 4 (до R67b — всегда 0).
* `LB_ADMISSION_WAIT_SEC=2` → один из 10 запросов получил
  `503 + Retry-After: 15 + X-Queue-Position: 3 + X-Queue-Wait-Max-Sec: 2 +
  X-Queue-Wait-Ms: 2000`, `timeouts=1`.

## R69 (2026-09-23): что закрыто по хвостам и что осталось

Закрыто (коммиты `8be78bc`, `82a480f`, образ балансера `r69-submodule-v2`,
cppworker `gpu-r69-submodule-v1`):

* **Гонка reload'ов** — единый gate `StartModelReloadIfNotPending` (ключ только
  backend+model) для ВСЕХ триггеров: preflight, R60.47-обработчик 413,
  R60.35/44 `executeAsyncReload`. До этого два последних лаунчера работали без
  дедупликации → cppworker отменял загрузку («model load aborted by user»),
  rollback падал и модель оставалась выгруженной.
* **Обработчик 413 больше не планирует reload «вниз»**, если координатор уже
  знает больший n_ctx (иначе качели 65536 → 16384 → 65536).
* **AutoTune не уменьшает n_ctx ниже клиентского запроса** (окно 30 минут) —
  именно это давало ping-pong reload и 503 на каждом запросе Cline.
* **KV-cache type**: профиль `gemma-4-E4B-it-Q4_K_M` получил `kvCacheType: q4_0`;
  cppworker при reload'е уважает выбор пользователя/профиля
  (`reload: SelectStrategy kvCacheType differs from user choice, honoring user`),
  поэтому 65K влезает в 8 GB VRAM (KV 720 MiB) и грузится на GPU. Стратегия
  по-прежнему *рекомендует* f16 — это её эвристика без учёта профиля; менять
  контракт стратегии не стали (см. «Осталось»).

Осталось (хвосты R67b/R69, не влияют на сценарий Cline):

1. **Keepalive для streaming-клиентов во время ожидания** (`LB_ADMISSION_KEEPALIVE_SEC`).
   Сейчас ожидающий запрос молчит до получения слота (максимум
   `LB_ADMISSION_WAIT_SEC`). Для очередей длиннее клиентского таймаута
   (OpenWebUI 300 с) нужен hijack + SSE-комментарии/пустые строки NDJSON —
   реализация отложена: сначала нужно понять, что важнее для Cline/Roo (они
   переиспользуют соединение и не любят преамбулу).
2. **Связь `maxConcurrentReqs` ↔ `n_parallel` cppworker.** Сейчас значение
   приходит из конфига бэкенда (default 1 для llama_cpp, в живом стеке 4 от
   агента). План: если cppworker сообщает `parallel`/`slots` в метриках —
   использовать его как default `maxConcurrentReqs`, чтобы очередь балансера
   совпадала с реальной вместимостью модели.
3. **Карточка очереди в WebUI** (monitor): блок `admission` уже отдаётся API,
   осталось отрисовать ожидающих и их сессии в `/monitor` (`renderDispatchStats`
   рядом) — вместе с i18n-ключами и `scripts/check_webui_assets.py`.
4. **Единая очередь для Ollama-пути**: `Proxy.ServeHTTP` использует старый
   `QueueManager` (workers + pending/processing). Он работает, но это вторая
   независимая очередь; имеет смысл свести обе к admission-очереди.
5. **KV-хинт в запросе к адаптивной стратегии cppworker**: `queryAdaptiveStrategy`
   не передаёт известный KV-тип, поэтому стратегия считает по f16 и может
   выбрать `gpu_layers=0` (CPU-only) там, где с q4_0 модель влезает на GPU.
   Рабочий обход — `kvCacheType` в профиле (сделано для gemma); системное
   решение — добавить параметр `kv_cache_type` в strategy-endpoint cppworker.


