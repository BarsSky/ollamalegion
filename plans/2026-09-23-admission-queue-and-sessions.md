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

## R70 (2026-09-24): закрыты хвосты 1, 3, 5 и первая половина хвоста 2

Коммиты `395e301` (keepalive), `721dead` (карточка WebUI), `2e3cf0b`
(вместимость = n_parallel cppworker), образы балансера `r70-submodule-v7`,
cppworker `gpu-r70-submodule-v3`, webui `r70-submodule-v1`:

* **Keepalive для streaming-ожидающих** (хвост 1) — `LB_ADMISSION_KEEPALIVE_SEC`
  (default 5, 0 = выключено). Ожидающий streaming-запрос сразу получает
  `200` + `X-Queue-Keepalive: 1` и пустые строки-keepalive: для SSE
  (`/v1/*`) — `: keepalive\n\n`, для NDJSON (`/api/*`) — `"\n"`; не-streaming
  путь не изменился. Живая проверка: 8 streaming-запросов при 4 слотах → 3
  ждали, у всех `X-Queue-Keepalive: 1`, 3–6 ведущих LF, все **200**.
* **Карточка очереди в `/monitor`** (хвост 3) — строка admission-статистики
  (`admWaiting`, `admActive`, `admServed`, `admTimeouts`, `admAvgWait`,
  `admWaitMax`, список `admSessions`), `?v=R70`, паритет i18n ru/en
  (`monitor.admission.*`, 1247/1247).
* **KV-хинт в адаптивную стратегию** (хвост 5) — см. CHANGELOG 0.5.28.
* **Хвост 2 (часть 1)**: `maxConcurrentReqs` бэкенда теперь берётся из реального
  `n_parallel` cppworker. `cmd/cppworker/balancer_register.go` →
  `effectiveNParallel()` (вместо константы 4), `docker/cppworker/
  register-with-balancer.sh` резолвит `CPPWORKER_MAX_CONCURRENT` из
  `/app/config/cppworker-defaults.json` (`defaultNParallel`), повторная
  саморегистрация стала идемпотентной (`isSameBackendRegistration` +
  `refreshRegisteredBackend`, 200 + `updated: true` вместо 409).

## R71 (2026-09-24): хвост 2 закрыт полностью — heartbeat агента больше не перекрывает n_parallel

Хвост 2 оказался не только про источник значения: на живом стенде вместимость
оставалась **4** при `n_parallel=1`, потому что балансер писал в неё «эхо»
собственного ответа агенту (подробный разбор и живая диагностика — в
CHANGELOG 0.5.29, образ балансера `r71-submodule-v8`):

* heartbeat (оба эндпоинта) **не пишет** `runtimeMaxConcurrentRequests`;
  источники вместимости — саморегистрация ноды и оператор (`PUT /limits`);
* агенту отдаётся **эффективная** вместимость (`EffectiveMaxConcurrentRequests`),
  поэтому его локальное значение сходится к `n_parallel` (`applied
  maxConcurrentRequests=1` вместо 4);
* единое правило вместимости вместо 4 копий «runtime > 0 ? runtime : max»;
* признак «вместимость от ноды» персистентен и восстанавливается в `LoadState`
  вместе с runtime-лимитами; старое «испорченное» состояние самоисцеляется
  (для `llama_cpp` runtime > `n_parallel` приводится к вместимости ноды);
* повторная регистрация агента больше не обнуляет runtime-поля бэкенда.

## Осталось после R71

1. **Единая очередь для Ollama-пути** (хвост 4): `Proxy.ServeHTTP` использует
   старый `QueueManager` (workers + pending/processing) — вторая независимая
   очередь; свести её с admission-очередью.
2. **Placement policy** для 2+ бэкендов —
   `plans/2026-09-23-multi-backend-placement-policy.md` (открытый вопрос:
   транспорт раскладки, llama.cpp RPC vs B8.7).
3. **Self-hosted CI-раннер** `skyworker-ci` — нужен админ:
   `Restart-Service actions.runner.skyworker-ci` (сервис запущен, связь с
   GitHub потеряна, `SocketException 995`, backoff).



