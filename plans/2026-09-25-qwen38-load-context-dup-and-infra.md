# Qwen3.8 на A10: загрузка, контекст, дублирование ответа + инфраструктура образов

- **Дата:** 2026-09-25
- **Контекст:** ручная проверка на железе **NVIDIA A10 (24 GB)** с моделью
  `Qwen3.8-27B-UD-Q4_K_M` (`/app/models/Qwen3.8-27B-UD-Q4_K_M.gguf`, 15.3 GB, 64 слоя,
  `gguf_max_context=262144`). Клиенты — **OpenWebUI** (Ollama-провайдер) и **Cline**.
- **Стек:** `deployments/docker-compose.cppworker-bundled-with-agent.yml`
  (`cppworker-gpu-bundled-agent`, порт 18080/18092).
- **Итог проверки:** при **n_ctx=32768 модель отвечает без проблем и ошибок**;
  при **65536 и 128000** — не работает ни загрузка, ни инференс.

## 0. Сводка

| # | Блок | Приоритет | Суть |
|---|------|-----------|------|
| 1 | [Монитор мёртв](#1-монитор-мёртв-p0) | **P0** | `ReferenceError: updateText is not defined` — обрывает `updateUI`, пустой `/monitor` |
| 2 | [Потолок контекста](#2-потолок-контекста-p0) | **P0** | 32768 = рабочий дефолт; >32768 уходит в RAM-fallback/auto-tune, потолки в `.env` разрешают недостижимое |
| 3 | [Дублирование ответа](#3-дублирование-ответа-p0) | **P0** | авто-продолжение эмитит перегенерацию (fail-open), `LB_AUTO_CONTINUE_ON_TRUNCATION=1` в проде |
| 4 | [Резолв имени модели](#4-резолв-имени-модели-p0) | **P0** | `qwen3.8:latest` не резолвится нигде → 500 вместо 404, 404 от adaptive |
| 5 | [Дашборд «0 загружено»](#5-дашборд-0-загружено-p1) | P1 | 4-й аргумент `dashboard()` не передан → считается только Ollama |
| 6 | [Уведомления о причине провала](#6-уведомления-о-причине-провала-p1) | P1 | связка **cppworker → agent → webui** без нагрузки на балансер |
| 7 | [Инфраструктура образов](#7-инфраструктура-образов-p2) | P2 | продовый compose, путь **1+2+3** (переменные тегов + алиасы + релизный скрипт) |
| 8 | [Мелочи](#8-мелочи-p2) | P2 | i18n, единый `?v=`, путаница RAM/VRAM в карточке |

**Порядок работ:** 1 → 2 → 3 → 4 → 5 → 6 → 7 → 8.
Монитор первым — иначе результат остальных правок нечем проверить визуально.

---

## 1. Монитор мёртв (P0)

### Симптом
`/monitor` пустой: «Нет данных для отображения топологии», нет таблицы бэкендов и
кластерных ресурсов. В консоли — `[monitor] fetchAll failed` каждые ~2 с.

### Доказательство
`log.txt` (445 строк) — это **только консоль браузера**; ошибок загрузки модели там нет.
Одна ошибка, ~150 повторов, первое вхождение — строки 6–13:

```
ReferenceError: updateText is not defined
    at renderAdmissionStats (ui-renderer.js?v=R77:695:5)
    at updateUI (ui-renderer.js?v=R77:367:5)
    at api.js?v=R77:265:57
```

- `webui/js/monitor/ui-renderer.js:3-7` — свой IIFE, в области видимости только `MA` и `T`.
- Вызовы без определения: `:695-702` (`renderAdmissionStats`, добавлен в R70) и
  `:720-732` (`renderPlacementPolicy`, добавлен в R77).
- `updateText` определён **приватно** в `webui/js/modules/monitor-metrics.js:83`
  и экспортируется только как `MonitorMetrics.updateText` (`:133`).
- **Проверено через git:** ни в R70 (`721dead`), ни в R77 (`fe10c73`) определения
  `updateText` в `ui-renderer.js` нет. То есть хелпер не терялся — его там не было никогда.
- Исключение на `:367` обрывает `updateUI` → теряется всё после строки 367:
  `renderPlacementPolicy`, `renderCandidateBackends`, `renderVirtualModels`, `diagnose`,
  `renderClusterResources`, `renderModelsInMemory`, `renderBackends`, `renderDiskNetwork`,
  `renderQueue`, `renderSessions`, `window.updateTopology` (`:368-389`).
- Смешанные версии в одной странице: `webui/monitor.html:553` — `monitor-metrics.js?v=R66d`,
  `:557` — `ui-renderer.js?v=R77`.

### Правки
1. **M1.** Локальный хелпер в `ui-renderer.js` (устраняет зависимость от порядка скриптов):
   ```js
   var updateText = (window.MonitorMetrics && MonitorMetrics.updateText) || function(id, t) {
     var el = document.getElementById(id); if (el) el.textContent = t;
   };
   ```
2. **M2.** (важнее по сути) Обернуть каждый вызов рендера в `updateUI` в `try/catch`
   (per-panel guard), чтобы одна панель не убивала всю страницу и `fetchAll failed`
   не повторялся каждые 2 с.
3. **M3.** Смоук-тест `/monitor` (Playwright): после `fetchAll` нет `console.error` и
   `backendsTableBody` непустой. Сейчас покрытия нет — ошибку поймал бы любой смоук.

### Проверка
`/monitor` без ошибок в консоли, топология и таблица бэкендов заполнены,
`[monitor] fetchAll failed` исчез из логов.

---

## 2. Потолок контекста (P0)

### Симптом
32768 — работает. 65536 и 128000 — нет (при этом по скринам видно, что 65536
**загрузился**: VRAM 17.0/22.5 GB, `C-65.5K`, кнопка «Выгрузить»).

### Доказательство: 32768 — не «примерно влезает», а точка перехода на другой путь

| | ≤ 32768 (работает) | > 32768 (ломается) |
|---|---|---|
| Дефолт | `CPPWORKER_CTX_SIZE=32768`, `config/cppworker-defaults.json:8` | запрошенное значение |
| Загрузка | обычная, на GPU | RAM-fallback + auto-tune |
| RAM-fallback | не нужен | `CPPWORKER_RAM_FALLBACK_N_CTX=true`, `GPU_LAYERS=-2` |
| Потолок n_ctx | — | `CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000` |
| Авто-подбор KV | не выполняется | ветка `requestedNCtx > 32768`, `cmd/cppworker/adaptive_loader.go:411` |
| n_ctx-reload | не нужен | `LB_NCTX_RELOAD_MAX_N_CTX=131072` |

Всё из `deployments/.env.bundled-with-agent` (боевой конфиг). Дефолт KV —
`defaultKvCacheType: "q4_0"` (`config/cppworker-defaults.json:27`).

**Три конкретные проблемы перехода:**

1. **Потолки разрешают недостижимое.** `CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000` +
   `LB_NCTX_RELOAD_MAX_N_CTX=131072` — то есть стек *принимает* ровно те значения,
   которые не влезают на A10. Жёсткий отказ есть только выше 128000
   (`cmd/cppworker/inference.go:629-630`), а `auto_tune_nctx.go:102-107,293-294`
   клампит к тому же 128000 — защиты на 65536/128000 нет.
2. **Ветка подбора KV для >32768 считает по чужой модели:**
   `adaptive_loader.go:414` берёт `conservativeModelMB = 5120` и `:445`
   `defaultLayers = 42`, тогда как у нас 15.3 GB и 64 слоя. Решение о типе KV
   принимается по заниженной втрое модели.
3. **Правильный механизм существует, но его обходят.**
   `cmd/cppworker/handlers_model.go:1163-1165` отдаёт `feasible_max_context = min(VRAM, RAM, GGUF)`;
   балансер умеет по нему отказывать с числами (`internal/balancer/proxy_helpers.go:401-406`);
   `internal/balancer/autotune.go:297-308` ловит over-allocation и рекомендует
   `RecommendedNumCtx = FeasibleMaxContext`. Но загрузка «Загрузить» с явным
   `contextSize` идёт прямо в cppworker **мимо** этого preflight.

### Что подтвердить перед правками
```bash
cppworker -feasible Qwen3.8-27B-UD-Q4_K_M          # cmd/cppworker/main.go:62
curl .../api/models | jq '.feasible_max_context, .gguf_max_context'
```
В логе попытки на 65536 искать: `failed to allocate`, `CUDA out of memory`, `fallback`,
`code 3`, `exceeds model`, `auto_tune`. Это отделит «не влезло» от «сломался стрим».
**Ожидание:** для 27B Q4_K_M на 24 GB `feasible` ≈ 32768 — совпадает с наблюдением.

### Правки
1. **C1.** Привести потолки к `feasible`: `CPPWORKER_RAM_FALLBACK_MAX_N_CTX`
   128000 → значение из `-feasible`; `LB_NCTX_RELOAD_MAX_N_CTX` 131072 → туда же.
2. **C2.** `adaptive_loader.go:402-433`: брать `meta.SizeBytes`/`meta.NLayers`
   (они уже в аргументах) вместо хардкода 5120 MB / 42 слоя.
3. **C3.** Честный отказ вместо RAM-fallback: `n_ctx > feasible_max_context` → **422**
   с `{requested, feasible, need_vram_mb, free_vram_mb}` (в духе R76 — «честный отказ
   вместо тихой подмены»).
4. **C4.** Preflight и на прямом пути загрузки (не только в балансере): «Загрузить»
   с `contextSize` выше feasible должна предупреждать/отказывать.
5. **C5.** Показать `feasible_max` в UI рядом с `ctx` и `gguf_max=262144` —
   сейчас 262144 читается оператором как «можно 128k/256k».
6. **C6.** Закрепить 32768 как рабочий профиль этой модели на A10
   (`config/cppworker-defaults.json` / профиль модели) и записать в `docs/`.

### Проверка
`-feasible`, `feasible_max_context` и потолки в `.env` дают одно и то же число;
65536/128000 → 422 с числами, а не RAM-fallback и не 500.

---

## 3. Дублирование ответа (P0)

### Симптом
OpenWebUI показывает ответ **дважды, слово-в-слово**.

### Доказательство
В репозитории **уже есть тест ровно про этот случай** —
`internal/balancer/auto_continue_duplicate_api_r66d_test.go:5-6`:

> «при запуске **qwen3.8 на NVIDIA A10** без reasoning ответ клиенту **OpenWebUI**
> пришёл **продублированным слово-в-слово**»

То есть баг воспроизводили и закрыли эвристикой подавления, а не устранением причины.
**В боевом конфиге авто-продолжение включено:** `deployments/.env.bundled-with-agent` →
`LB_AUTO_CONTINUE_ON_TRUNCATION=1`, `LB_AUTO_CONTINUE_MAX_TOKENS=2048`.

Механизм (`internal/balancer/llamacpp_transport.go:1275-1341`): ответ обрывается на
стороне модели → авто-продолжение шлёт `continue` → модель **перегенерирует** ответ →
защита не распознаёт перегенерацию → `EmitContinuationNDJSON(w, …, finalContent, continuation, …)`
отдаёт оригинал + продолжение.

**Четыре дырки в защите:**

| # | Дырка | Ссылка |
|---|-------|--------|
| 1 | similarity **отключён**, если нормализованный оригинал короче 120 рун → короткие ответы дублируются гарантированно | `auto_continue.go:481,546` |
| 2 | пороги префикса/вхождения 40 и 60 рун — перегенерация «своими словами» проходит | `auto_continue.go:462,469` |
| 3 | политику можно снести в `always` → подавление выключено | `auto_continue.go:650` |
| 4 | логика **fail-open**: эмитим, если не доказали перегенерацию (нужно наоборот) | `llamacpp_transport.go:1303` |

**Два альтернативных механизма** (развести обязательно):

- **A. Финальный done-чанк несёт весь текст.** `llamacpp_translate_resp.go:801-808`
  кладёт в `message.content` полный накопленный ответ, хотя дельты уже отдали его
  по частям. Поле уже флапало: R60.49 менял `""`→полный текст, R60.54 добавлял
  `deduplicateResponseContent`. Работает **при дефолтном конфиге**.
- **C. Ретрай без гарда «уже отдали клиенту».** `proxy.go:1103` и `:1183` переигрывают
  `proxyRequest(w, …)` в тот же `w`; `statusRecorder.WriteHeader` второй заголовок
  глушит (`:1590-1600`), а `Write` **дописывает тело** → второй ответ в тот же ответ.
  Hijack-путь (`proxy_request_hijack.go`) пишет в соединение вручную — там то же самое.

### Как развести за 10 минут
1. `LB_AUTO_CONTINUE_ON_TRUNCATION=0`, рестарт балансера, тот же запрос. Дубль исчез →
   механизм B. Готовый скрипт: `scripts/diag_auto_continue_live.py`.
2. Перехватить сырой NDJSON/SSE балансер→OpenWebUI. Текст **дважды на проводе** → B или C.
   **Один раз** → A (клиент приклеивает done-чанк). В логах искать `R60.21 detected truncation`,
   `auto-continue succeeded`, `suppressing duplicate`.

### Правки
1. **D1.** Перевернуть дефолт с fail-open на **fail-closed**: продолжение эмитим только
   при доказанном продолжении, иначе молча оставляем обрезанный оригинал.
2. **D2.** Снять порог `minRegenerationOriginalRunes=120` для коротких ответов
   (или сравнивать по всему оригиналу).
3. **D3.** Убрать `always` (или пометить как заведомо ломающую).
4. **D4.** Метрика `auto_continue_regeneration_suppressed` vs `emitted` — сейчас
   подавление видно только строкой в логе. Сюда же — уведомление (см. блок 6).
5. **D5.** Гард в `proxy.go` перед обоими ретраями: запомнить, что в `w` уже ушли байты,
   и не переигрывать (вернуть ошибку вместо второго ответа). То же для hijack-пути.
6. **D6.** Зафиксировать контракт done-чанка тестом под версию OpenWebUI из деплоя
   (либо `content` пустой, либо клиент заменяет, а не приклеивает). Сейчас не зафиксировано.

### Связь с блоком 2
```
контекст выше feasible → RAM-fallback/auto-tune → медленный prefill
  → обрыв/деградация стрима → TruncateReason срабатывает
    → авто-продолжение → перегенерация → дубль
```
Правки C1–C4 убирают большую часть триггеров, но D1–D2 нужны **отдельно**:
обрыв возможен и на 32768 (тест R66d воспроизводит дубль вообще без RAM-fallback).

---

## 4. Резолв имени модели (P0)

### Симптом
`POST /api/models/load` → **500**, в логе cppworker:
```
gguf_init_from_file: failed to open GGUF file models/qwen3.8:latest.gguf (No such file or directory)
POST /api/models/load status=500
GET  /api/v1/cppworker/adaptive/strategy status=404
```
Клиент (Cline: Ollama, `http://192.168.50.26:18080`) просит `qwen3.8:latest`;
на диске — `Qwen3.8-27B-UD-Q4_K_M.gguf`.

### Доказательство
1. `matchCppWorkerModel` (`internal/balancer/llamacpp_backend_helpers.go:255-271`)
   сравнивает только точное имя, basename и `name+".gguf"` → `qwen3.8:latest`
   не совпадает → балансер считает модель **не загруженной**.
2. `ensureModelLoadedOnBackend` (`:340`) инициирует загрузку несуществующей модели.
3. `resolveModelPath` (`cmd/cppworker/handlers_model.go:2541-2593`) не резолвит:
   шаг 3 (auto-pick) работает только если в каталоге **ровно один** `.gguf`;
   иначе шаг 4 — голый `filepath.Join(modelsDir, name+".gguf")` **без `os.Stat`**.
   На lazy-load пути early-fail **есть** (`cmd/cppworker/lazyload.go:159-179`) —
   асимметрия и есть дефект.
4. `handleAdaptiveStrategy` (`cmd/cppworker/adaptive_loader.go:968-975`) →
   `GetModelMeta` (`internal/cppbackend/model_manager.go:577-584`, точный поиск по map) → **404**.
5. Балансер этот 404 глотает в `Debugw` (`internal/balancer/nctx_reload_adaptive.go:75-79`) →
   «модель не найдена» выглядит как «стратегия не выбрана».

### Правки
1. **N1.** Единый резолвер имени в `cppbackend`: нормализация (`:latest`/`:tag`, регистр,
   `.gguf`, `~`↔`-`↔`_`), цепочка алиасов (`AliasSourcePath`/`resolveAliasChain`),
   `.name_history.json`. Использовать в `resolveModelPath`, `handleAdaptiveStrategy`,
   `FindModelByPath`, `lazyload.go`.
2. **N2.** `os.Stat` early-fail в `resolveModelPath` + **404** с `available_models`
   и ближайшим совпадением вместо 500 с сырым путём.
3. **N3.** Та же нормализация в `matchCppWorkerModel`; в `ensureModelLoadedOnBackend`
   сверять с известным списком (`/api/models/files`, алиасы есть с R66d) и **не
   триггерить загрузку** для неизвестного имени.
4. **N4.** `queryAdaptiveStrategy`: поднять 404 с `Debug` до `Warn` + пробросить причину
   в метрику/ответ.
5. **N5.** Отдавать клиенту канонический список ID (`/api/tags`, `/v1/models`) и
   показывать в WebUI копируемый идентификатор — сейчас `Qwen3.8-27B-UD-Q4_K_M`
   невозможно угадать из `qwen3.8:latest`.

### Замечание о приоритетах
Уточнение «при 32768 всё работает» означает, что **имя модели не является блокером
ручного теста** (та же связка клиент+имя работает). Этот блок — отдельный реальный
дефект, который даёт 500 и 404 в логе, но чинить его нужно **после** блоков 1–3.

---

## 5. Дашборд «0 загружено» (P1)

### Симптом
На `/` счётчик «ЗАГРУЖЕНО МОДЕЛЕЙ 0», при этом карточка `Ollama Runtime` на той же
странице показывает модель (`C-65.5K`).

### Доказательство
- `webui/js/app.js:235` вызывает `dashboard(filteredBackends, data.sessions, data.queue)` —
  **4-й аргумент не передан**.
- `webui/js/modules/renderers.js:71-73` — fallback считает только
  `b.ollama?.runningModels?.length`, что для llama.cpp-бэкенда всегда 0.
- `renderers.js:114-116` на той же странице рисует модель из `llamaCpp.loadedModels`.
- Гонка: async `Api.clusterModels.loaded()` (`app.js:229-234`) правит счётчик **после**
  синхронного `dashboard()`, и каждый refresh снова перезатирает его нулём.

### Правки
1. **B1.** Считать синхронно из `backends` (`llamaCpp.loadedModels.length +
   ollama.runningModels.length`) и передавать 4-м аргументом; async-ответ — только уточнение.
2. **B2.** Не перезатирать уточнённое значение на последующих refresh.

---

## 6. Уведомления о причине провала (P1)

**Цель:** оператор видит в WebUI, **почему** модель не загрузилась (границы конфига,
нехватка памяти, прочие нештатные ситуации), без чтения логов.
**Ограничение:** **не нагружать балансер** — он должен чётко отрабатывать свои задачи.
**Целевая связка:** `cppworker → agent → webui`.

### Что уже есть (переиспользуем, не строим заново)

| Звено | Что делает | Где |
|---|---|---|
| cppworker | `/api/gpu`, `/api/models`, `/api/info`; реестр провалов TTL 10 мин → `state:"failed"` | `llama_collector.go`, `load_failures.go:32` |
| agent | **сам** поллит cppworker каждые ≥10 с (`minCollectInterval`), 3 вызова **параллельно**, best-effort; пушит `POST /api/v1/backends/{id}/agent/metrics`, heartbeat 3 с | `internal/agent/collector.go:258-272,406-411,545-550` |
| balancer | только **принимает** push, декодит в `types.BackendMetrics`, `UpdateMetrics` — **не поллит** | `internal/api/handlers_agents.go:872-923` |
| webui | читает `llamaCpp.*` из cluster state; рисует уведомления `severity/source/model/message` | `renderers.js:114-116`, `notifications.js:207-227` |

Уже готовые кирпичи: `types.Event` + severity (`pkg/types/events.go:27-44`),
SSE `/api/v1/events` с ring buffer 100 (`internal/api/handlers_events.go`),
образец публикации `publishTransportEOF` (`internal/balancer/transport_events.go:33-63`),
`LoadAttempt` со стадиями (`cmd/cppworker/handlers_diagnostics.go:39-60`),
`InsufficientResourcesError` → **413 + `code=6` + `max_viable_n_ctx` + `suggestion`**
(`cmd/cppworker/utils.go:449-513`), словарь причин (`cmd/cppworker/debug_last_prompt.go:95-99`).

### Три разрыва
1. Причина провала — **свободный текст**: `loadFailureEntry{At, Err string}`
   (`cmd/cppworker/load_failures.go:39`), машиночитаемого кода нет.
2. Нет пути «провал загрузки → EventBus»: балансер публикует только `transport_eof`
   (падение *запроса*), провал *загрузки* не публикуется вообще.
3. UI только поллит `/api/models/load/progress` и только на странице GGUF.

### Таксономия причин (cppworker)

| Код | Триггер | Что показать |
|---|---|---|
| `model_not_found` | `lazyload.go:159`, `resolveModelPath` | путь + **список доступных `.gguf`** |
| `config_out_of_bounds` | `inference.go:629`, `> feasible_max_context` | requested vs feasible |
| `insufficient_resources` | `InsufficientResourcesError` (413) | `max_viable_n_ctx`, need/free VRAM/RAM |
| `gguf_incompatible` | ошибка llama.cpp | arch/quant + сырой текст |
| `backend_not_ready` | `model_manager_nil`, backend init | текст + `/api/diagnostics` |
| `load_timeout` | не догрузилась за maxWait | сколько ждали, размер модели |
| `alias_broken` | alias source missing | имя алиаса + пропавший путь |
| `unknown` | всё остальное | сырой текст (обязательно) |

### Правки
1. **A1.** `loadFailureEntry` → `{At, Reason string, Err string, Diagnostics map[string]any}`;
   `Err` сохранить для совместимости (`handlers_load_progress_failed_test.go`).
   Поля `Diagnostics` **уже формируются** на lazy-load пути (`lazyload.go:170-175`).
2. **A2.** Отдать `load_failure` в **`/api/models`** (top-level, рядом с
   `feasible_max_context`) — агент его **уже** парсит каждые 10 с → **ноль новых
   HTTP-запросов во всей связке**.
3. **A3.** agent: поле `LoadFailure` в `LlamaMetrics` (`internal/agent/llama_collector.go:23-31`),
   заполняется из уже разобранного ответа, уезжает в существующем push-е.
   Ни новых горутин, ни тикеров, ни endpoint'ов.
4. **A4.** balancer (только релей): поле в `types.BackendMetrics`
   (`pkg/types/metrics.go:35`), в `agentBackendMetricsHandler` после `UpdateMetrics`
   (`handlers_agents.go:912`) — **проверка перехода**: публикуем `EventNotification`
   только если `(backendID, model, reason)` изменился. Ни поллинга, ни таймеров,
   ни агрегации; `Publish` неблокирующий.
5. **A5.** webui: **level** — `llamaCpp.loadFailure` в карточке модели/бэкенда;
   **edge** — уведомление. v1 клиент **не трогаем** (`notifications.js` уже всё умеет),
   далее клик → переход на GGUF-страницу + раскрытие чисел, затем кнопка
   «Загрузить с feasible=N» (`autotune.go:297-308` уже считает `RecommendedNumCtx`).

### Обязательные условия
- **Edge, не level — иначе спам.** Провал живёт 10 мин, push идёт каждые 10 с → без
  guard'а перехода ~60 одинаковых уведомлений, а ring buffer SSE всего 100
  (`handlers_events.go:29`) и вытеснит полезное. Нужны переходы `нет→есть` и
  `есть→нет` (одно `info` «восстановлено»).
- **Экранирование (это ещё и уязвимость).** `webui/js/modules/notifications.js:216-225`
  вставляет `message`/`source`/`model` в **`innerHTML` без экранирования**. Сейчас туда
  идёт текст балансера, с этой фичей попадёт `raw_error` от llama.cpp, пути и
  **имя модели из запроса клиента** → XSS в браузере оператора. Экранирование — в том же коммите.
- `Message` — готовая человеческая фраза с числами; `raw_error` — **только в `Data`**.

### Тесты
cppworker — табличный тест кодов причин (`insufficient_resources_response_test.go`);
agent — что `Collect` заполняет `LoadFailure` (`llama_collector_test.go:33-45`);
balancer — «один переход → одно событие, повторные push'и молчат» (`transport_events_test.go`);
webui — фикстурное событие + тест экранирования (сейчас такого нет).

---

## 7. Инфраструктура образов (P2)

**Решение по целям: продовый стек.**
Цель — `deployments/docker-compose.cppworker-bundled-with-agent.yml`.
Принят **путь 1+2+3**. `latest` в проде **не используем**.

### Фактура (почему именно так)

- **Правка тегов уже автоматизирована**, причём тремя разными способами:

  | Компонент | Как обновляется версия | Где |
  |---|---|---|
  | balancer | собирает `:${Tag}` и **сам перезаписывает** `image:` в compose регуляркой | `scripts/rebuild_balancer_r66a.ps1:43,50-55` |
  | cppworker | **не трогает compose**, пишет `CPPWORKER_GPU_TAG=` в `.env` и `.env.bundled-with-agent` | `scripts/rebuild_cppworker_r66a.ps1:55,59-67` |
  | webui | как balancer: сборка + пин `image:` в compose | `scripts/rebuild_webui_r66d.ps1:51-60` |
  | agent | отдельный `scripts/deploy-agent-docker.ps1` (`-Build`/`-Pull`/`-EnvFile`) | — |

  У balancer это сделано осознанно, с объяснением в коде (`:47-49`): образ прописан
  строкой, поэтому тег обновляется прямо в compose — иначе compose пересоздаст
  контейнер на СТАРОМ образе. То есть боли «правлю имена руками» уже нет;
  проблема — **рассинхрон между четырьмя подходами**.

- **CI образы не собирает.** В `.github/workflows/ci.yml` — Go-бинарники и тесты;
  `docker build`/`push` нет. Образы собираются **локально на деплой-хосте**.
  Значит `latest` — тег локального демона, а не реестра.

- **`latest` уже используется в dev/standalone**: `docker-compose.yml:23,140`,
  `docker-compose.full.yml:210,251,315`, `docker-compose.llama.cpp.yml:25,112`,
  `docker-compose.llama.cpu.yml:16,69`, `docker-compose.cppworker-with-agent.standalone.yml:61,91`.

- **`latest` неоднозначен в этом репозитории** — один репозиторий содержит взаимно
  исключающие варианты: `cppworker:cpu|stub|gpu-86-…|gpu-arch_all|gpu-llamacpp|gpu-86-abort-v2`,
  `agent:latest|latest-gpu|gpu-llamacpp|…`. `latest` — один тег на репозиторий, побеждает
  последняя сборка. Для A10 (sm_86) это не теория: cppworker собирается
  `--build-arg CUDA_ARCH=86` (`rebuild_cppworker_r66a.ps1:55`), в репозитории есть и
  `Dockerfile.gpu`, и `Dockerfile.gpu.86`. То, что уже заведены `agent:latest-gpu`
  (`docker-compose.full.yml:210`) и `cppworker:latest-cpu` (`docker-compose.llama.cpu.yml:16`),
  — обходное решение, подтверждающее ограничение.

- **Что теряется при `latest` в проде:** откат (сейчас = вернуть тег); журнал релиза
  (тег в compose несёт историю R78/R76/R75/… в комментарии); молчаливое обновление при
  рестарте; и главное — **согласованность не достигается**: `latest` двигается по каждому
  компоненту отдельно, что и даёт рассинхрон вида `?v=R66d` + `?v=R77` в одной странице (блок 1).

### Правки

1. **Путь 1 — параметризовать теги переменными** (по образцу, уже работающему у cppworker).
   В `deployments/docker-compose.cppworker-bundled-with-agent.yml`:

   ```yaml
   # :69
   image: ollama-legion/balancer:${LB_TAG:-r80-submodule-v17}
   # :163
   image: ollama-legion/cppworker:gpu-${CPPWORKER_GPU_TAG:-86-r60.4-webui-meta}
   # :265
   image: ollama-legion/agent:${AGENT_TAG:-cppworker-bundled-r41-agent-x-api-token}
   # :348
   image: ollama-legion/webui:${WEBUI_TAG:-r77-submodule-v2}
   ```

   Дефолт = зафиксированная проверенная версия; `rebuild_*.ps1` пишут переменную в `.env`
   (cppworker уже так делает). **Compose не правится никогда**, история версий — в `.env`,
   откат — правка переменной.

2. **Путь 2 — в сборке давать второй (вариантозависимый) алиас.** В каждом rebuild-скрипте
   после `docker build -t $image:$Tag`:
   ```powershell
   docker tag "${imageName}:${Tag}" "${imageName}:latest-gpu-86"   # или latest-cpu, latest-stub
   ```
   Дополнительно можно `docker tag … :latest` — дёшево и полезно для dev-стендов,
   которые уже на `latest`, но **осознавая**, что для вариантных репозиториев это
   «последняя сборка побеждает».

3. **Путь 3 — единый релизный скрипт на все четыре компонента:** собирает, пинит все
   четыре тега **одним коммитом** и пишет **манифест** (компонент, тег, digest).
   Это лечит настоящую боль — рассинхрон версий между сервисами — и даёт «актуальный
   последний образ» как согласованный **набор**, а не как набор независимых `latest`.

4. **Обязательные оговорки:** если `latest` всё же где-то в проде — `pull_policy: never`
   (или `docker compose up --no-pull`), чтобы рестарт не подменял образ; писать
   `org.opencontainers.image.revision` в label, чтобы «что работало тогда»
   восстанавливалось по digest.

### Файлы
`deployments/docker-compose.cppworker-bundled-with-agent.yml`,
`scripts/rebuild_balancer_r66a.ps1`, `scripts/rebuild_cppworker_r66a.ps1`,
`scripts/rebuild_webui_r66d.ps1`, `scripts/deploy-agent-docker.ps1`, новый релизный скрипт.

---

## 8. Мелочи (P2)

1. **i18n:** `common.search` отсутствует в `webui/js/i18n/ru.js`, используется в
   `webui/js/modules/renderers.js:1579` → предупреждение в логе (строка 405). Добавить
   в `ru.js`/`en.js`.
2. **Кэш-версии:** `webui/monitor.html` хардкодит per-file `?v=` (R66d и R77 вперемешку,
   строки 553/557) — цельный релиз даёт смешанные поколения JS. Ввести единый токен сборки.
   Это **не** причина `ReferenceError`, а гигиена.
3. **Проверить (не подтверждено):** карточка модели на Models показывает
   «Использование памяти … RAM 17272/54181 MB (31.9%)» и одновременно «RAM 16.87 GB»,
   а VRAM выше — «—». Похоже на путаницу RAM/VRAM или неполные поля от cppworker
   (`size` vs `size_vram`). Смотреть `webui/js/modules/renderers.js:1045` (`computeMemorySplit`)
   и `memoryBar` (`:1289-1290`); нужен реальный JSON `/api/models`.

---

## 9. Критерии приёмки

1. `/monitor` — нет `updateText is not defined`, топология и таблица бэкендов заполнены.
2. `-feasible`, `feasible_max_context` и потолки в `.env` дают одно и то же число;
   при 32768 ответ без ошибок (регресса нет).
3. Запрос 65536/128000 → **422 с числами**, а не RAM-fallback, не обрыв стрима и не 500.
4. Тот же промпт, что давал дубль, при `LB_AUTO_CONTINUE_ON_TRUNCATION=1` отвечает
   **один раз**; в логе `regeneration suppressed`.
5. `POST /api/models/load` с `qwen3.8:latest` при загруженной `Qwen3.8-27B-UD-Q4_K_M`:
   либо инференс на загруженную модель, либо чистый 404 со списком — **не** 500
   и **не** `failed to open GGUF`.
6. `/` — счётчик загруженных моделей = 1 и не мигает нулём.
7. Провал загрузки появляется в bell-уведомлениях **один раз** и остаётся видимым
   в карточке модели до истечения TTL — **без новых запросов к cppworker и без поллинга
   на стороне балансера**.
8. Продовый compose не правится при выпуске: версия меняется одной переменной в `.env`;
   релизный скрипт пинит все четыре компонента одним коммитом и пишет манифест.

## 10. Открытые вопросы

1. Что печатает `cppworker -feasible Qwen3.8-27B-UD-Q4_K_M` на A10 и что именно в логе
   при попытке на 65536 (`failed to allocate`/`CUDA OOM` или `code 3`/`fallback`)?
   Без этого причина отказа на 65536 остаётся выводом, а не фактом.
2. Каким провайдером OpenWebUI ходит в балансер — Ollama (`/api/chat`) или OpenAI
   (`/v1/chat/completions`)? Если OpenAI — механизм A дублирования исключается сразу
   (`writeStreamingSSEDone` для `/v1` шлёт только `[DONE]`).
3. Показывать `raw_error` полностью или только в «подробностях» уведомления?
4. `Publish` в балансере оставляем (одна проверка перехода) или убираем совсем и
   ограничиваемся level-отображением в карточке?
