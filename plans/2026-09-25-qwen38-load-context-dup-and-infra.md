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

### Прогресс реализации

| Блок | Статус | Коммит |
|------|--------|--------|
| 1. Монитор | ✅ сделано (смоук-тест воспроизводит баг на старой ревизии) | `c424065` |
| 3. Дублирование — правило **≥85%** | ✅ сделано; **D2 скорректирован** (см. блок 3) | `7c2aa70` |
| 3. Дублирование — гард ретраев (D5) | ✅ сделано (второй механизм дубля) | `5060d35` |
| 5. Дашборд «0 загружено» | ✅ сделано | `f1296f5` |
| 8. Мелочи (i18n, единый `?v=` + автоподстановка в образе) | ✅ сделано | `f1296f5` |
| 2. Контекст — классификация режима + лог | ✅ сделано | `651c9ca` |
| 2. Контекст — отказ **422** во всех трёх load-путях | ✅ сделано | `2804156` |
| 2. Контекст — режим (`n_ctx_degraded`) в `/api/models` | ✅ сделано | `6514970` |
| 2. Контекст — `feasible n_ctx` в карточке модели | ✅ сделано | `9c16ecf` |
| 2. Контекст — предупреждение о недостижимом потолке конфига (C1) | ✅ сделано | `5060d35` |
| 2. Контекст — C2 (`fallback_no_meta`: хардкод 5120 MB / 42 слоя) | ⏳ нужен только без GGUF-меты | — |
| 2. Контекст — C6 (закрепить 32768 профилем + docs) | ⏳ | — |
| 3. Остаток: D3 (`policy=always`), D4 (метрика) | ⏳ | — |
| 4. Резолв имени — нормализация (тег/регистр/расширение) в cppbackend + `pkg/modelname` | ✅ сделано | `48077bb`, `ac74bb6` |
| 4. Резолв имени — 404 со списком + `suggestion` вместо 500 | ✅ сделано | `48077bb` |
| 4. Резолв имени — adaptive/strategy резолвит имя (источник 404) | ✅ сделано | `48077bb` |
| 4. Резолв имени — матчер балансера (N3) | ✅ сделано | `ac74bb6` |
| 4. Резолв имени — N5 (канонический ID клиенту) | ✅ частично (`available_models` + `suggestion` в 404) | — |
| 6. Уведомления — причина провала в `/api/models` (таксономия + diagnostics) | ✅ сделано | `2117552` |
| 6. Уведомления — agent забирает и пересылает (без новых запросов) | ✅ сделано | `1db99f1` |
| 6. Уведомления — балансер публикует только на смену причины | ✅ сделано | `1db99f1` |
| 6. Уведомления — WebUI рисует + **закрыт XSS** в notifications.js | ✅ сделано | `1db99f1` |
| 7. Образы — путь 1: теги в переменных (`deployments/.env`) | ✅ сделано | `88b099f` |
| 7. Образы — путь 2: вариантные алиасы (`latest`, `latest-gpu-86`) | ✅ сделано | `88b099f` |
| 7. Образы — путь 3: `scripts/release-all.ps1` + манифест | ✅ сделано | `88b099f` |
| 7. Образы — проверка согласованности в CI + контракт в `.env.example` | ✅ сделано | `88b099f`, `5081157` |
| 2. Остаток — C2 (`fallback_no_meta`), C6 (документация режима 24 GB) | ✅ сделано | `2aa2668`, `d0ef10e` |
| 3. Остаток — D3 (`policy=always`), D4 (метрики) | ✅ сделано | `d9acaae` |
| 3. D1 — безопасная форма fail-closed (`isRedundantContinuation`) | ✅ сделано | `dd7393b` |
| 4. Остаток — N5 (копируемый ID модели) | ✅ сделано | `d0ef10e` |
| **Все блоки плана закрыты.** D2 отменён обоснованно, D6 требует данных от стенда | ⚠️ см. ниже | — |

**Ограничение протокола, найденное при реализации блока 2 (важно для блоков 2 и 6).**
cppworker декодирует тела **строгим** декодером (`types.DecodeJSONRequest`,
`DisallowUnknownFields`), а в репозитории есть guard-тесты
(`internal/balancer/contract/cppworker_contract_test.go`,
`nctx_reload_payload_test.go`), которые требуют, чтобы балансер **не** слал поля
`reason` / `adaptiveStage` — иначе 400 «unknown field». Поэтому любое новое поле
(причина провала, стадия загрузки) нужно добавлять **сразу на обеих сторонах и
вместе с этими тестами**; просто «дописать в запрос» нельзя. Реализованная в
`651c9ca` оценка намеренно ничего не меняет на проводе.

**Уточнение к блоку 2 по коду (важно для реализации).** Граница 32768/65536 —
это граница между `exact_fit` и `partial_offload` в `SelectStrategyWithKV`
(`cmd/cppworker/adaptive_loader.go`):
- при 32768 `totalVRAMNeeded = weights + KV(q4_0) + overhead` укладывается в
  `safeVRAM = 0.85 × FreeVRAM` → **все слои на GPU** (`exact_fit`, `:638-650`);
- при 65536 уже не укладывается → **`partial_offload`** (`:678-692`): часть слоёв
  уходит в RAM через mmap, ответ резко медленнее и упирается в таймауты клиента;
- `MaxViableNCtx` в обеих ветках просто равен `requestedNCtx` (`:645`, `:686`),
  то есть наружу не сообщается, что режим деградировал.

Отсюда: «32768 работает, 65536 нет» — не про «не влезло», а про **смену режима**.
Поэтому C3/C4 (честный отказ) должны опираться на `feasible_max_context` =
`min(VRAM, RAM, GGUF)` (`cmd/cppworker/handlers_model.go:1163-1165`), а `stage`
и `explanation` стратегии нужно **показывать оператору** — иначе деградация
остаётся невидимой.

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
2. **D2.** ~~Снять порог `minRegenerationOriginalRunes=120` для коротких ответов
   (или сравнивать по всему оригиналу).~~ **ОТМЕНЕНО при реализации (R83).**
   Снятие порога ломает документированный компромисс R60.55, закреплённый
   регресс-тестом `TestR6055_IsContinuationARegeneration_NoFalsePositiveOnCommonCode`:
   настоящее продолжение кода законно повторяет строку оригинала
   (`const height = parseFloat(...)`, 69 рун), а подавление такого «продолжения» —
   это молчаливая потеря контента, что по комментарию к самой константе **хуже
   дубликата**. Вместо снятия порога реализовано **правило «≥85% дословного
   повтора» по всему тексту** с собственным порогом `minDuplicateOriginalRunes=160`
   (см. D-факт ниже): оно ловит жалобу (содержательный ответ в сотни рун) и не
   задевает короткие фрагменты.
3. **D3.** Убрать `always` (или пометить как заведомо ломающую).
4. **D4.** Метрика `auto_continue_regeneration_suppressed` vs `emitted` — сейчас
   подавление видно только строкой в логе. Сюда же — уведомление (см. блок 6).
5. **D5.** ✅ **Сделано (`5060d35`).** `statusRecorder` считает реально записанные
   байты и факт `hijack`; `clientAlreadyCommitted()` блокирует повтор запроса на
   другом бэкенде (`proxy.go`: alternate-backend ×2, auto-pull) после того, как
   часть ответа ушла клиенту. Без гарда повтор **дописывал второй ответ** в тот же
   стрим — это второй механизм жалобы «ответ дважды» (первый — перегенерация при
   авто-продолжении).
6. **D6.** Зафиксировать контракт done-чанка тестом под версию OpenWebUI из деплоя
   (либо `content` пустой, либо клиент заменяет, а не приклеивает). Сейчас не зафиксировано.

**D-факт (реализовано в R83):** правило «>85% похожести», обещанное в docstring
R60.55, реализовано впервые — `isDuplicateBySimilarity` + `longestCommonSubstringRunes`
(DP с rolling-строками, окно 2000 рун). Ключевое: проверка идёт **до** порога
`minRegenerationOriginalRunes` и ловит перегенерацию, у которой начало
переформулировано (ни одна из «начальных» эвристик R66d её не видит). Тесты:
`auto_continue_duplicate_similarity_r83_test.go` (6 проверок, включая сверку DP
с наивной реализацией и регресс-гард на короткий фрагмент кода).

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
1. **N1.** ✅ **Сделано (`48077bb`, `ac74bb6`).** Нормализация внешнего имени
   (`StripTag`: снятие `:latest`/`:tag`; `Variants`: тег + расширение + регистр)
   вынесена в **`pkg/modelname`** и подключена в `FindModelByPath`,
   `AliasSourcePath`, `handleAdaptiveStrategy` и в матчер балансера
   `matchCppWorkerModel`. Отдельный нейтральный пакет — потому что логика нужна
   обоим бинарникам, а тянуть `cppbackend` в балансер ради строковой операции
   нельзя. Пути (в т.ч. `C:\...`) нормализация не трогает.
   **Регресс, поймавший тест:** первая версия `AliasSourcePath` потеряла контракт
   R66d — «битый алиас возвращает путь-источник» (`TestScanModels_Aliases_R66d`).
   Исправлено: путь сохраняется, варианты пробуются только если имя вообще не алиас.
2. **N2.** ✅ **Сделано (`48077bb`).** `resolveModelPathChecked` + `writeModelNotFoundResponse`:
   **404** с `available_models`, `suggestion` («возможно, вы имели в виду …») и
   `code=model_not_found` вместо 500 от llama.cpp. 404 осознанно: балансер
   ретраит только 503 «model is loading» (`model_management.go:1223`).
3. **N3.** ✅ **Сделано (`ac74bb6`).** `matchCppWorkerModel` использует
   `modelname.Matches` — семантика совпадает с резолвом cppworker (точное
   совпадение → вхождение), с порогом `MinFuzzyLen=3` против ложных срабатываний.
4. **N4.** ✅ **Сделано (`48077bb`).** `queryAdaptiveStrategy`: 404/не-200
   логируется `Warn` вместо `Debug` — «модель не найдена» перестала выглядеть как
   «стратегия просто не выбрана».
5. **N5.** ✅ **Частично.** Клиент получает канонический список в 404
   (`available_models` + `suggestion`). Отдельный «копируемый ID» в WebUI — не делался.

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
1. **A1.** ✅ **Сделано (`2117552`).** `loadFailureEntry` расширен до
   `{At, Model, Reason, Err, Diagnostics}`; добавлен словарь причин
   (`cmd/cppworker/load_failure_reason.go`), классификатор по типу ошибки и тексту,
   `severity`, и метод `latest()` для «последней причины».
2. **A2.** ✅ **Сделано (`2117552`).** `load_failure` отдаётся в **`/api/models`**
   (top-level): `model`, `reason`, `error`, `at`, `severity`, `diagnostics`.
   Агент его **уже** опрашивает каждые ~10 с → **ноль новых HTTP-запросов во всей связке**.
   Запись централизована в единственных воронках исходов: 404 `model_not_found`,
   422 `config_out_of_bounds`, ранний отказ lazy-load по отсутствию файла.
3. **A3.** ✅ **Сделано (`1db99f1`).** agent: `LlamaMetrics.LoadFailure` заполняется
   из того же ответа `/api/models`; `toBackendLoadFailure` переносит данные в
   метрики бэкенда. Ни новых горутин, ни тикеров, ни endpoint'ов.
4. **A4.** ✅ **Сделано (`1db99f1`).** Балансер: `Proxy.PublishLoadFailureTransition`
   — одно сравнение `(backendID → key)` на полученный push, публикация **только на
   смену причины** и одно `info`-событие при восстановлении. Ни поллинга, ни
   таймеров, ни агрегации.
5. **A5.** ✅ **Сделано (`1db99f1`).** WebUI: `notifications.js` рисует уведомление
   (клиент v1 не менялся — он уже умел severity/source/model/message) плюс блок
   «подробности» с числами из `diagnostics`. **Закрыт XSS**: `message`/`source`/
   `model` подставлялись в `innerHTML` без экранирования, а туда попадает имя
   модели **из запроса клиента** и сырой текст llama.cpp; `severity` теперь
   проходит whitelist (значение уходило в имя CSS-класса).

**Найденный при реализации баг (поймал тест):** `LoadFailureInfo.Key()` для
пустого объекта возвращал `"\x00"` (непустую строку), поэтому «нет данных от
cppworker» выглядело как провал с пустой причиной и порождало уведомление на
пустом месте. Исправлено: пустой объект даёт пустой ключ.

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

### Что фактически сделано (R83, `88b099f` + `5081157`)

- **Путь 1.** Все четыре тега читаются как `${VAR:-default}` из `deployments/.env`:
  `BALANCER_TAG`, `CPPWORKER_GPU_TAG` (уже был), `AGENT_TAG`, `WEBUI_TAG`.
  **Compose при выпуске не правится.** Значения подобраны так, чтобы фактически
  подставляемый образ не изменился: `BALANCER_TAG` заменил прежний литерал
  `r82-submodule-v19`, а не устаревшее `cppworker-bundled-full-v31r5` из `.env`.
- **Путь 2.** Каждая сборка вешает «последний» алиас: balancer/agent/webui —
  `latest` (вариантов нет), **cppworker — `latest-gpu-86`**, потому что
  `cpu`/`stub`/`gpu-*` делят один репозиторий и plain `latest` перезаписывался бы
  последней сборкой (именно поэтому в репозитории исторически есть `latest-cpu`
  и `latest-gpu`).
- **Путь 3.** `scripts/release-all.ps1`: собирает все четыре компонента, пишет
  теги одним заходом в `.env` и фиксирует `deployments/release-manifest.json`
  (компонент → тег + image id + git commit). Пересборка `rebuild_*.ps1` переведена
  с правки compose регуляркой на общий `scripts/lib-image-tags.ps1` — **правка
  регуляркой после параметризации затёрла бы `${...}` и вернула литерал**
  (то есть вернула бы ровно ту боль, от которой уходим).
- **Проверка.** `scripts/check-image-tags.ps1` (docker не нужен, добавлен в CI)
  ловит два класса: литерал вместо переменной и рассинхрон манифеста с `.env`.
  Оба сценария проверены — падает с `exit 1`.
- **Найдено и исправлено (`5081157`):** `deployments/.env` **в `.gitignore`**,
  поэтому правки переменных там не доехали бы до стенда, а CI-шаг упал бы на
  отсутствии файла. Контракт перенесён в отслеживаемый
  `deployments/.env.example`, а проверка выбирает `.env` (локально) или
  `.env.example` (в CI) и печатает, какой файл использован.
- Документация: раздел «Теги образов» в `docs/ENV_VARS.md`.

**Важное следствие для стенда.** Изменения R83 затрагивают **все четыре**
компонента (balancer/cppworker/agent — Go; webui — JS), поэтому раскатанные
образы (`r82-submodule-v19`, `gpu-r70-submodule-v3`, `r77-submodule-v2`) их не
содержат. Выпуск — одной командой:
```powershell
powershell -File scripts/release-all.ps1 -Tag r83-submodule-v1
```

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

---

## 11. Живая проверка на реальном GPU (2026-09-25)

Проверка выполнена НЕ на A10, а на локальной машине с **RTX 3070 (8192 MB VRAM,
8 GB)**, где развёрнут тот же стек. Это меняет не механизм, а пороги: арифметика
ниже переносится на A10 подстановкой его 24 GB.

Стенд: образ `ollama-legion/cppworker:r83-gpu-local` (собран из текущего дерева,
llama.cpp-слой взят из кэша r70), контейнер `ol-r83-cppworker-gpu` (порт 18292),
**standalone** — регистрация в живом балансере отключена, живой стек не тронут.
Модель — тот самый файл: `Qwen3.8-27B-UD-Q4_K_M.gguf`, 16 464 440 224 байт.

### 11.1 Что подтвердилось (R83 в работе)

| Проверка | Результат |
|---|---|
| Ollama-имя с тегом → файл | `qwen3.8:latest` → `/app/models/Qwen3.8-27B-UD-Q4_K_M.gguf` ✓ |
| Несуществующая модель | HTTP **404**, `code=model_not_found`, `available_models[3]`, `path`, `suggestion` (UTF-8, без BOM — проверено по байтам) ✓ |
| `load_failure` в `/api/models` | `reason=model_not_found`, `severity=error`, `diagnostics{attempted_path, available_models, models_dir, suggestion}` ✓ |

Это ровно тот payload, который `notifications.js` уже умеет рендерить в
`_renderDetails` — связка cppworker → agent → WebUI обеспечена данными.

### 11.2 Почему 65536/131072 не работают, а 32768 работает — измерено

Лог реальной загрузки при `contextSize=131072`:

```
optimal GPU layers found with RAM fallback
  totalLayers:65 gpuLayers:0 cpuLayers:65
  cpuMemoryMB:19556 ramAvailableMB:24576 kvCacheMB:8565 useMmap:false kvCPUFraction:1
auto-adapting GPU layers for model
  model:qwen3.8:latest requestedGPULayers:20 optimalGPULayers:0 useMmap:true
[bridge] loading model: .../Qwen3.8-27B-UD-Q4_K_M.gguf (ctx=131072, batch=512, threads=6, gpu_layers=0)
[bridge] VRAM-estimated max n_ctx = 26417
  (free=5495 MB, gpu_model=15691 MB, n_layers=64 n_embd=5120 n_heads=24 n_kv_heads=4 head_dim=213 kv_per_token=218112 bytes)
done_getting_tensors: tensor 'token_embd.weight' (q4_K) (and 850 others)
  cannot be used with preferred buffer type CUDA_Host, using CPU instead
```

**Причина — не таймаут и не балансер, а арифметика KV-кэша.** KV при f16 стоит
≈68.5 КБ/токен (измерено: 8565 MB на 131072), веса модели — 15 691 MB:

| n_ctx | KV ≈ | веса+KV | A10 (≈23 GB usable) | RTX 3070 (8 GB) |
|---|---|---|---|---|
| 32768 | 2.1 GB | 17.8 GB | **влезает** → «отвечает без проблем» | нет |
| 65536 | 4.3 GB | 20.0 GB | на грани (CUDA-контекст + буферы) | нет |
| 131072 | 8.6 GB | 24.3 GB | **не влезает** → `gpu_layers=0` | нет |
| 262144 | 17.1 GB | 32.8 GB | не влезает | нет |

То есть на A10 отказ на 131072 — это схлопывание оффлоада в ноль (инференс 27B
целиком на CPU, `estimatedLoadTimeMs` 175 s против фактических минут), а не
`failed to allocate`. **Ответ на открытый вопрос №1 получен: fallback, не OOM.**

Практический вывод (P0 для пользователя): при 131072 **не нужно** поднимать
таймауты — нужно снять KV-кэш с f16. При `kvCacheType=q4_0` KV на 131072 ≈ 2.1 GB,
итого 17.8 GB — влезает в A10 с запасом, и та же 131072 становится рабочей.
Кандидат в правки: adaptive_loader должен для `n_ctx > max_vram_n_ctx`
автоматически предлагать/выбирать KV-квантизацию и сообщать об этом
(`n_ctx_degraded` уже есть, но без причины «почему деградировало»).

Точные числа — из живого `cppworker -feasible` по реальному файлу (RTX 3070,
8 GB, ничего не загружено). KV-тип переключается env-хуком
`CPPWORKER_KV_CACHE_TYPE` (`internal/cppbackend/config.go:331`):

| KV-тип | KV на токен | Max VRAM ctx | Max RAM ctx |
|---|---|---|---|
| f16 | 221 520 B | **19 383** | 96 943 |
| q8_0 | 110 760 B | **38 767** | 193 886 |
| q4_0 | 55 380 B | **77 535** | 262 144 |

Масштаб ровно **1 : 2 : 4** — квантизация KV линейно расширяет рабочий контекст.
На карте 8 GB даже q4_0 не дотягивает до 131072 (веса 15.7 GB сами больше VRAM),
но на A10 свободная под KV VRAM втрое больше, поэтому граница «32768 работает /
65536 и 131072 нет» сдвигается именно в ту сторону, которую наблюдал пользователь.

**Action item на A10 (одна команда, стенд у пользователя):**

```
docker run --rm --runtime=nvidia -v <models>:/app/models \
  --entrypoint /app/cppworker ollama-legion/cppworker:<r83-tag> -feasible Qwen3.8-27B-UD-Q4_K_M
# и то же с -e CPPWORKER_KV_CACHE_TYPE=q4_0
```

Это заменит вывод рассуждением на факт по конкретному стенду. Отдельно: env
`CPPWORKER_AUTO_KV_CACHE` (`cmd/cppworker/main.go:203`) включает авто-даунгрейд
f16 → q8_0 → q4_0 в `fallback_no_meta` — на проверяемом стенде он **не был
включён**, поэтому и остался f16 (см. также D-E).

### 11.3 Таймауты: балансер не режет генерацию

Разбор по коду (вопрос «если мешают таймауты — балансер неверно работает»):

| Таймаут | Значение по умолчанию | Роль |
|---|---|---|
| total stream (`getGlobalStreamTimeout`) | **0 = выключен** (R65c) | — |
| request (`getGlobalRequestTimeout`) | **0 = выключен** (R60.26) | — |
| first-byte (`getGlobalFirstByteTimeout`) | 900 s (Round 42), по размеру GGUF до 2400 s | HTTP-заголовки от бэкенда |
| idle (`getStreamingIdleTimeout`) | 120 s, **по размеру GGUF**: 12–24 GB → **1800 s** | пауза между чанками |

`LB_STREAMING_NEVER_TIMEOUT=1` выключает всё. Для файла 16.5 GB tier — 1800 s,
так что медленная генерация 27B по таймауту не рвётся.

**НО найден реальный дефект в этой цепочке (исправлен).** Размер модели для
tier-эвристики ищется по имени, которое прислал КЛИЕНТ. Клиент зовёт модель
`qwen3.8:latest`, cppworker репортит имя файла `Qwen3.8-27B-UD-Q4_K_M`.
`getModelSizeBytes` → `modelNameMatches` → `normalizeModelName` срезал путь и
`.gguf`, но **не срезал ollama-тег**, поэтому размер не находился и idle падал в
глобальный дефолт **120 s**. Наглядно:

```
getModelSizeBytes("qwen3.8:latest")            = 0, want 16464440224
getModelStreamingIdleTimeout("qwen3.8:latest") = 2m0s, want 30m
```

Исправление: `normalizeModelName` теперь срезает тег через
`pkg/modelname.StripTag` (тот же нормализатор, которым балансер уже ищет модель
на бэкенде — таймаут-эвристика просто отставала от матчинга). Регрессия
закреплена: `internal/balancer/model_size_resolution_r83_test.go` (падал до
правки) + 4 кейса с тегом в `model_name_match_r66d_test.go`.

### 11.4 Новые дефекты, найденные на живом стенде

**D-A (P0). `load-with-params` врёт про асинхронность.** Ответ:
`202 Accepted`, `state: loading`, `progressUrl`, `message: "Model load started in
background. Poll progressUrl..."`. Фактически запрос **блокируется до конца
загрузки**:

```
HTTP request POST /api/models/load-with-params status:202 duration:9m6.671867886s
HTTP request POST /api/models/load-with-params status:202 duration:3m23.916200647s
```

Любой клиент с разумным HTTP-таймаутом (балансер, WebUI, curl) отваливается
раньше, чем придёт 202, и не получает ни модели, ни причины — ровно симптом
«модель не загрузилась». Чинить: либо честно асинхронно (вернуть 202 сразу,
грузить в горутине, прогресс через `progressUrl`), либо синхронно (200 по
завершении и без `progressUrl`). Текущий гибрид нарушает собственный контракт.

**D-B (P0). Расхождение реестра: модель загружена, но `/api/models` её не
видит.** После перекрывающихся load/unload в логе есть
`model loaded ... ctxSize=131072, gpuLayers=0`, RSS процесса 13.97 GiB, а
`/api/models` отдаёт `count=0`, `load_failure` пуст, `progress` → 404,
`feasible_max_context=0`. Балансер на таком ответе считает модель незагруженной
и инициирует повторную загрузку. Требуется разбор состояния реестра при
конкурентных load/unload (и, до фикса, — предупреждение в WebUI, а не тишина).

**D-C (P1). `unload` не является барьером.** `POST /api/models/unload` вернул
200 за **824 µs**, хотя в этот момент в фоне шла загрузка 16 GB; фактически
загрузка продолжилась и завершилась `model loaded` уже после «успешной»
выгрузки. Следующий load-запрос встал в очередь за ней.

**D-D (P1). Практически непригодный n_ctx принимается молча.** R83 отказывает
(422) только когда `n_ctx` превышает собственную длину контекста модели из GGUF
(здесь `n_ctx_train` = 262144), поэтому 131072 прошёл без предупреждения — при
том что VRAM-потолок машины 26417. Гейт должен быть **offload-aware**: если `optimalGPULayers == 0`
(или `n_ctx > max_vram_n_ctx`), это `warning`/`error`-событие с диагностикой и
подсказкой (`kvCacheType=q4_0`, меньший ctx, больше GPU-слоёв) — данные для
этого в cppworker уже есть.

**D-E (P0). Lazy-load уходит в `fallback_no_meta` с нулевыми метаданными и грузит
модель второй раз.** После расхождения реестра (D-B) запрос `/api/chat` не нашёл
загруженную модель и инициировал повторную загрузку 16.5 GB — при этом в логе:

```
lazy-loading model from filesystem, model=qwen3.8:latest
lazy-load: load opts calculated ... source=fallback_no_meta requested_nctx=32768
  applied_nctx=32768 reduction_pct=0.0% requested_gpu=-1 applied_gpu=-1
  arch= nlayers=0 nembd=0 size_mb=0 available_vram_mb=0 max_viable_nctx=0
```

Все входные данные выбора стратегии — нули: архитектура, число слоёв, размер
модели, свободная VRAM, потолок n_ctx. То есть в этом пути **не читается даже
GGUF-заголовок**, хотя `-feasible` на том же файле отдаёт полную картину, а
`GetModelMeta` доступен. Следствия: авто-выбор KV-типа и GPU-слоёв вырождается
(`applied_gpu=-1`), клиент не получает ни модели, ни причины, а 16 GB читаются с
диска повторно (в контейнере это заняло минуты при CPU 0.13 % — упор в I/O
bind-mount с Windows-диска). Заодно это объясняет, почему
`CPPWORKER_AUTO_KV_CACHE` сам по себе не спас бы: ему не с чем работать.

### 11.5 Что это меняет в плане

1. Открытый вопрос №1 закрыт: на границе работает **fallback в CPU**, не OOM.
2. Приоритет смещается: главный рычаг для 65536/131072 — **KV-квантизация**, а
   не таймауты балансера (таймауты уже масштабируются и выключены там, где надо).
3. Добавить в блок 3 (уведомления) событие «загружено без GPU-оффлоада» — это
   тот случай, когда модель формально загрузилась, но пользоваться ей нельзя.
4. D-A/D-B/D-C — новые пункты, приоритет выше косметики: они дают именно тот
   симптом, из-за которого пользователь не понимает, почему модель не поднялась.

### 11.6 Учёт RAM: почему причины не было видно (найдено и исправлено)

Проверка вопроса «правильно ли учитывается возможность выделить модели место в
оперативной памяти» дала три дефекта, и все три дополнительно объясняют, почему в
живом прогоне 131072 прошёл молча.

| Место | Было | Стало |
|---|---|---|
| `CalculateResourceLimits` → `GetModelMeta` | точный lookup по карте имён файлов: `qwen3.8:latest` не находился → **все лимиты нулевые** → `Known=false` → гейт n_ctx **fail-open** | внешнее имя резолвится через `FindModelByVariants` (тот же путь, что и загрузка) |
| `ModelMaxContext` холодной модели | брался только из *загруженной* модели → 0 (даже при точном имени файла) | берётся из `GGUFModelMeta.ContextLength` |
| `max_ram_n_ctx` | `(MemTotal − 4096) / kvPerToken` = **96 943** для 27B (по установленной RAM и без весов) | свободная RAM (`MemAvailable`) минус веса, если модель не влезает в свободную VRAM → **22 975** (f16) / ≈90k (q4_0) |
| `CalculateOptimalGPULayers` (RAM-fallback) | сравнение с **MemTotal**, в лог уходил он же под именем `ramAvailableMB` | сравнение с свободной RAM, в логе обе величины + источник |

Живое подтверждение того, что «запаса 105 MB» больше нет: решение
`cpuMemoryMB=19556 < 24576*0.8=19660` одобряло загрузку 27B (веса 15.7 GB + KV
8.6 GB) на машине с 20.5 GB свободных — по свободной памяти отказ обязан был
случиться.

**Проверка на реальном GPU** (R83-образ с правками, отдельный контейнер): тот же
запрос, который раньше уходил в 9-минутную загрузку, теперь отвечает мгновенно и с
числами:

```
POST /api/models/load-with-params  {"name":"qwen3.8:latest","contextSize":131072}
→ 422 code=n_ctx_infeasible
   max_ram_n_ctx=98105  max_vram_n_ctx=103389  gguf_max_context=262144
   hard_max_n_ctx=98105  suggestion="…используйте n_ctx <= 98105 …"
```

В логе — `R83 n_ctx превышает физический предел … stage=infeasible` и
`R83 потолок n_ctx из конфига недостижим … configured_max_n_ctx=128000 >
hard_max_n_ctx=98105`. Счётчик `[bridge] loading model` = **0**: модель не читалась
вообще. Тесты — `internal/cppbackend/r83_ram_accounting_test.go` (падают на
неисправленном коде, проверено `git stash`).

**Что осталось (сознательно не в этой правке).**

1. `max_vram_n_ctx` тоже считается без весов: на 8 GB карте показал `103389`, хотя
   15.7 GB весов в VRAM не влезают. Правильная формула — `(usable − modelSize)/kvPerToken`,
   и при `modelSize >= usable` потолок VRAM равен 0.
2. Но ноль в `MaxVRAMNCtx` сейчас означает в `evaluateNCtxFeasibility` «VRAM
   неизвестна» (stage=unknown, без предупреждения), поэтому одну только формулу
   менять нельзя — нужно различить «неизвестно» и «веса не влезают». Это отдельная
   правка логики стадий.
3. Лимит cgroup (`memory.max`) не читается: `MemAvailable` корректен для VM/ядра, но
   per-container `mem_limit` в расчёт не входит. В этом стеке `mem_limit` не задан.
4. На Windows `CalculateOptimalGPULayers` по-прежнему использует фиксированные 8192 MB
   (`getSystemRAMGB()` там возвращает 16 GB fallback), т.е. продакшн-путь — Linux-контейнер.

