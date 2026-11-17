# Диагностика дефектов обновления WebUI (Д1 / Д2 / Д3)

Дата: 2026-11-17. Только диагностика: код, Go, разметка и тесты не правились.
Отчёт построен на воспроизведении в headless-браузере, каждое утверждение ниже имеет
число, строку кода или сетевой запрос.

**Важно про снимок.** Пока шла диагностика, в дерево лёг коммит
`6680c112` «Одна кнопка «Обновить» и авто-обновление, которое обновляет страницу»
(17.11, 13:40 MSK) — он меняет ровно эти три места. Поэтому отчёт разделён:

* разделы 2-5 — измерения на **снимке до коммита** (HEAD `2e0faec5`, образ собран
  17.11 13:15:20 MSK). Именно этот снимок воспроизводил жалобы оператора;
* раздел 6 — что из измеренного коммит `6680c112` изменил, а что **не** изменил
  (проверено на сборке из HEAD, отдельный контейнер, те же скрипты).

---

## 1. Стенд, снимок и метод

### 1.1. Снимок и образ

| Проверка | Значение |
|---|---|
| HEAD на момент сборки образа | `2e0faec5` (рабочее дерево без правок webui) |
| Образ | `ollama-legion/webui:check-r493`, собран `2026-11-17 13:15:20 MSK`, 103 MB, **локальный** (в registry не публиковался) |
| `js/modules/data-refresh.js` | в образе 15 208 B, SHA-256 `5d03b5fb…b5fd7` = файл дерева байт в байт |
| `js/app.js` | в образе 210 883 B, SHA-256 `a3e61edc…33a0` = файл дерева байт в байт |
| `index.html` | в образе 238 458 B, в дереве на снимке `2e0faec5` — 237 890 B. Разница 568 B = 71 cache-busting-токен × 8 B: Dockerfile (`docker/webui/Dockerfile:131`) делает `sed "s/?v=[A-Za-z0-9._-]*/?v=${HASH}/g" *.html`; в `index.html` 71 токен (70 × `?v=R425` + 1 × `?v=R476`), каждый удлиняется с 4 до 12 символов. После обратной замены файл совпадает с файлом дерева (237 890 B). |

**Вывод:** дефекты воспроизводились на коде, который лежит в дереве, а не на
«старом образе». Образ `ollama-legion/webui:r261112-1716`, обслуживающий `:18083`,
содержит те же `data-refresh.js` / `app.js` (SHA-256 совпадают) — то есть на рабочем
контейнере оператора дефекты те же.

### 1.2. Временный контейнер (стенд `ol-stack-*` не трогался)

```
docker run -d --name ol-webui-check --network ol-net -p 18084:80 \
  -e API_TOKEN=<из deployments/.env> -e LB_API_URL=http://loadbalancer:18081 \
  ollama-legion/webui:check-r493
```

* `GET http://127.0.0.1:18084/health` → **200**;
* `GET /api/v1/cluster` без заголовка → **401** `{"error":"Unauthorized: valid API token required"}`,
  с заголовком `X-API-Token` → **200** (nginx проксирует `/api/` на `loadbalancer:18081`,
  `webui/nginx.conf:187-203`).

### 1.3. Браузер и авторизация

* Авторизация как в UI: ключ `sessionStorage.ol_break_glass_token`
  (`webui/js/modules/auth.js:47`, чтение `:49-58`, подстановка заголовка `X-API-Token`
  `:82-83`). UI-оверлей входа при этом не появляется, токен сразу уходит в запросы.
* **Ограничение метода:** chromium из `node_modules/playwright` (131.0.6778.33)
  запускается, но `browser.newPage()` не отвечает (>15 с, таймаут) — поэтому все
  измерения сделаны headless **Edge** (`chromium.launch({channel:'msedge'})`,
  Edge 157.0.4341.60). Тот же Chromium-движок, но версия другая — см. §7.

---

## 2. Сводная таблица: страницы → кнопки → обработчик → что реально делает

Кнопки обновления на снимке до коммита — **7 штук** в разметке/рендере:

| № | Где | Разметка | Обработчик |
|---|---|---|---|
| 1 | шапка (рядом с «Добавить бэкенд») | `webui/index.html:190` `#refreshBtn` | `webui/js/app.js:464-506` |
| 2 | Image-модели, у селекта модели | `webui/index.html:805` `#imgModelsRefreshBtn` | `webui/js/modules/image-page.js:973-974` → `loadModels()` |
| 3 | Image-модели → таб «Загрузки» | `webui/index.html:948` `[data-imh-action="refresh-downloads"]` | `image-models-hf.js:1404` → `refreshDownloads()` |
| 4 | Диагностика | `webui/index.html:1226` `#diagRefresh` | `diagnostics.js:367-370` → `load()` |
| 5 | Настройки → AutoTune | `webui/index.html:2370` `#autotune-refresh-btn` | `autotune_settings.js:152-154` → `render()` |
| 6 | GGUF, левая панель | `gguf-renderer-list.js:31` `#ggufRefreshBackends` | `gguf-renderer-detail.js:442-447` → `M.refreshBackends()` |
| 7 | GGUF, шапка панели деталей | `gguf-renderer-detail-render.js:160` `#ggufRefreshDetail` | `gguf-renderer-detail.js:450-455` → `M.refreshDetail()` |

На странице GGUF одновременно **видны три** кнопки обновления (шапка + №6 + №7):
инвентаризация DOM (`visibleNow=true` у всех трёх) — это и есть «их несколько» из жалобы.
На Image-странице ещё две (№2, №3), но они живут на неактивных табах (`visibleNow=false`),
на Диагностике — №4, в Настройках — №5.

Просьба оператора «оставить одну в заголовке рядом с кнопкой подключить бэкенд» указывает
ровно на `#refreshBtn` (`index.html:190`) и `#addBackendBtn` «Добавить бэкенд»
(`index.html:191`) — других кнопок в шапке нет, оба элемента в одном `.header-actions`.

### Что делает кнопка шапки (`app.js:464-506`)

1. `fetchClusterState()` — `GET /api/v1/cluster` + `GET /api/v1/backends` (обогащение
   конфигурацией, `app.js:1051`), затем `markFresh()`;
2. `DataRefresh.refresh(currentPage)` — провайдеры активной страницы
   (`data-refresh.js:230-253`);
3. `refreshPage(currentPage)` — перерисовка страницы из `data.*` (`app.js:272-404`).

Ключевое: **шаг 2 для части страниц пустой**. Реестр провайдеров на снимке:

| Ключ | Кто регистрирует |
|---|---|
| `models, agents, logs, sessions, queue, monitor, settings` | `app.js:1953-1979` (`registerPageProviders`) |
| `gguf` | `gguf-page-provider.js:25` |
| `image` (3 шт.) | `image-page.js:971`, `image-backends-page.js:1130`, `image-profiles.js:1771` |
| **нет** | `dashboard`, `backends`, `diagnostics` |

Измерено в браузере (одинаково на всех страницах, кроме image):
`DataRefresh._providers` = `{gguf:1, models:1, agents:1, logs:1, sessions:1, queue:1, monitor:1, settings:1}`.
`registerGlobal` (провайдер «на любой странице») определён (`data-refresh.js:92`), но
**не вызывается нигде** (grep по `webui/js`: только объявление и экспорт) → `providers['*']`
всегда пуст → для страницы без ключа `refresh(page)` немедленно резолвится, ничего не сделав
(`data-refresh.js:231-237`).

### Постранично (клик по `#refreshBtn`, окно 5 с, снимок до коммита)

| Страница | Провайдер страницы | Что ушло в сеть при клике | Изменился ли текст страницы |
|---|---|---|---|
| dashboard | нет | 44 запроса; из них к кнопке: `/api/v1/cluster`, `/api/v1/backends`, `/api/v1/cluster/models/loaded` (`refreshPage('dashboard')`, `app.js:278`) | да, `dashboard-page` 1313 → 1365 B |
| backends | нет | 49 запросов; новые относительно фона: повторные `/api/v1/cluster`, `/api/v1/backends` | да, `backends-page` 679 → 1040 B |
| models | `models` | 53 запроса; `/api/v1/cluster`, `/api/v1/cluster/models/loaded` | да, `models-page` 315 → 539 B |
| gguf | `gguf` | 45 запросов, в т.ч. **2 × `GET /api/v1/gguf/backends`**; ни одного `/api/v1/backends/{id}` | **нет** (текст `gguf-page` не изменился) |
| image (через сайдбар) | `image` ×3 | `/api/v1/image/backends`, `/api/v1/image/model-profiles`, `/api/v1/image/model-catalog`, `/api/v1/image/backends/imageworker/proxy/api/image/models` | да |
| diagnostics | нет | 47 запросов, в т.ч. **`GET /api/v1/diag/summary`** | текст 7033 → 7033 B (меняется только время генерации) |
| settings | `settings` | 48 запросов, в т.ч. `/api/v1/cluster/config`, `/api/v1/settings`, `/api/v1/admin/autotune/config` | да, `settings-page` 14 759 → 14 904 B |
| queue | `queue` | 47 запросов | нет (очередь пуста, те же числа) |
| sessions | `sessions` | 43 запроса | нет (0 сессий) |

Оговорка по числам: в окно 5 с попадает и фоновый опрос (5-секундный тик + 2-секундный
цикл iframe монитора, §5.1), поэтому «44-53 запроса» — это фон + клик. Атрибуция сделана
по появлению эндпоинтов, которых в фоновом наборе нет вообще (`diag/summary`, `image/*`,
`cluster/models/loaded`, второй `gguf/backends`).

Отдельная деталь по Диагностике: провайдера у неё нет, но `refreshPage('diagnostics')`
вызывает `Diagnostics.mount()` (`app.js:329-331`), а `mount()` заканчивается
безусловным `load()` (`diagnostics.js:400`) — то есть кнопка шапки тянет
`/api/v1/diag/summary` **побочным эффектом перерисовки**, а не через реестр. Провайдер у
страницы появился только в `6680c112`.

### Клики по страничным кнопкам (снимок до коммита)

| Кнопка | Запросы | Что перерисовалось |
|---|---|---|
| `#ggufRefreshBackends` | 1 × `GET /api/v1/gguf/backends` | ничего (текст не изменился; функция обновляет только левый список, `gguf-renderer-refresh.js:330`) |
| `#diagRefresh` | `GET /api/v1/diag/summary` (+ `cluster/models/loaded`) | блок «Данные для разбора» |
| `#autotune-refresh-btn` | `GET /api/v1/admin/autotune/config`, `GET /api/v1/admin/autotune` | только виджет AutoTune |
| `#imgModelsRefreshBtn`, `refresh-downloads` | не кликал: обе на неактивных табах Image-страницы; по коду — `loadModels()` / `refreshDownloads()`, то есть **только** список моделей выбранного image-бэкенда / только список загрузок | — |

---

## 3. Д1 — «кнопок несколько и они отрабатывают по-разному»

### Корневая причина

Единая точка (`data-refresh.js`) есть, но она **не покрывает все страницы**, и вторая
половина работы кнопки (перерисовка) живёт не в реестре:

1. **Реестр неполон.** `registerPageProviders` (`app.js:1953-1979`) регистрирует 7 ключей;
   `dashboard`, `backends`, `diagnostics` — без провайдеров. `registerGlobal` не
   используется никогда. Следствие: `DataRefresh.refresh('dashboard')` и
   `refresh('backends')` — no-op (`data-refresh.js:231-237`, возврат `Promise.resolve(true)`).
   Это подтверждается трассировкой: `перехват DataRefresh.refresh` в браузере показал
   `{ev:'refresh', page:'backends', providers:[gguf,models,agents,logs,sessions,queue,monitor,settings]}` —
   ключа `backends` в списке нет, и `refresh-done` пришёл в ту же миллисекунду, т.е. работы не было.
2. **Эти страницы спасает только `refreshPage`** — перерисовка из уже загруженного
   `data.*`, которую делает обработчик кнопки (`app.js:478`). Именно поэтому кнопка на
   dashboard/backends «как бы работает», а на GGUF/Диагностике ведёт себя иначе.
3. **Разные кнопки делают разные подмножества работы.** Ни одна страничная кнопка не
   делает того же, что шапка: `#ggufRefreshBackends` не обновляет панель деталей,
   `#diagRefresh` не тянет кластер, `#autotune-refresh-btn` перерисовывает только AutoTune,
   `#imgModelsRefreshBtn` — только список моделей (это и записано комментарием в
   `image-page.js:966-969`). Оператор видит «одна кнопка обновила, другая нет».
4. **«Кнопка ничего не делает» на свежеоткрытой GGUF-странице** имеет отдельную причину:
   провайдер `gguf` зовёт `refreshDetail()` (`gguf-page-provider.js:30-32`), а тот
   стартует с `if (!state.selectedBackendId) return Promise.resolve();`
   (`gguf-renderer-refresh.js:449`). Измерено: после загрузки `?page=gguf`
   `state.selectedBackendId === null`, бэкенд не выбирается автоматически → шапка
   дёргает `/api/v1/gguf/backends` дважды и не трогает панель деталей.

### Доказательство

* Инвентаризация DOM (все страницы, все `button/a/[role=button]` с текстом/id/title/иконкой
  sync): 7 элементов, перечисленных в §2; на `gguf-page` одновременно видимы 3.
* Таблица §2: клик по шапке на `gguf` → 45 запросов, текст страницы не изменился;
  на `dashboard` → текст изменился; `drLog` на `dashboard`/`backends`/`diagnostics` показывает
  `refresh:<page>` без соответствующего ключа в реестре.
* `#ggufRefreshBackends` → ровно 1 запрос `/api/v1/gguf/backends`, текст не изменился.
* 401-ошибка в консоли на всех страницах — это `GET /api/v1/i18n` (перехват `response`
  с `status>=400`: единственная запись `{s:401,u:"/api/v1/i18n",m:"GET"}`), к обновлению
  отношения не имеет.

---

## 4. Д2 — «на странице GGUF загруженные модели видны только после F5»

### Откуда берётся блок «Загруженные»

* `state.loadedModels` заполняется **только** в `refreshDetail()`
  (`gguf-renderer-refresh.js:540-546`) из `GET /api/v1/backends/{id}` →
  `llamaCpp.loadedModels`; рисуется `renderLoadedPane()` (`gguf-renderer-detail-render.js:1384`).
* `refreshDetail()` вызывается только из: `GgufRenderer.render()` при уже выбранном
  бэкенде (`gguf-renderer.js:135-139`), `selectBackend()` (`gguf-renderer-actions.js:76`),
  действий load/unload/delete/params (`:253, :266, :298, :445, :469, :481, :503, :650,
  :713, :804, :825, :834, :862, :875`), провайдера `gguf` (`gguf-page-provider.js:30-32`)
  и «наблюдения» после ответа 202 (`:425-454`).
* **Таймера, который перечитывает состояние бэкенда, нет.** Единственный периодический
  цикл страницы — `startActiveQueriesPolling` (`gguf-renderer-refresh.js:766`) — дёргает
  только `/api/v1/models/active-queries`, и сам он **никогда не запускается**: оба обращения
  к `startActiveQueriesPolling` лежат внутри `refreshActiveQueriesPolling`
  (`gguf-renderer-refresh.js:841-849`, `gguf-renderer.js:183-193`), а тот вызывается
  только из cross-tab sync (`app-listeners.js:138-139`) и `actions.js:733`.
  Второй поллер — загрузки (`gguf-renderer-refresh.js:852-865`) — живёт только пока
  `state.activeDownloads` непуст.

### Что измерено (снимок до коммита)

Загрузка модели **мимо вкладки** (`POST /api/v1/backends/cppworker-gpu-bundled-agent/models`
с `{"operation":"load","modelName":"Qwen3-4B-Instruct-2507-Q4_K_M"}`, как curl):

| Время | Сервер (`/api/v1/backends/{id}` → `llamaCpp.loadedModels`) | Страница (вкладка «Загружено», бэкенд выбран, вкладка видима, авто=вкл) |
|---|---|---|
| t+0…t+20 с | `0` | `state.loadedModels.length = 0`, панель «Нет загруженных моделей» |
| t+25 с | `["Qwen3-4B-Instruct-2507-Q4_K_M"]` | `0`, «Нет загруженных моделей» |
| t+101 с (20 замеров по 5 с) | `1` | `0`, «Нет загруженных моделей» |
| после клика `#refreshBtn` | `1` | `1`; в панели карточка: `ctx=32768 VRAM=4064 MB gpu_layers=36/36 kv=q8_0` |
| после F5 + выбор бэкенда + таб | `1` | `1` |

В 30-секундном окне на GGUF-странице **не ушло ни одного** запроса к `/api/v1/gguf/backends`
или `/api/v1/backends/{id}` — фоновый набор целиком состоит из общих эндпоинтов (§5.1).
То есть страница физически не спрашивает сервер о загруженных моделях, пока её не ткнут
кнопкой или пока не нажмут F5.

### Проверка трёх гипотез, зафиксированных в задании

| Гипотеза | Факт | Вердикт |
|---|---|---|
| (а) `refreshDetailPane` не вызывается по завершении загрузки | Вызывается, но только в цепочке UI-действия: `loadOnSelectedBackend` → `manageModel` → `.then` → `refreshDetail()` (`gguf-renderer-actions.js:266`). UI-путь проверен отдельно: клик по `.gguf-load-btn` на той же странице → строка появилась **сама** на t+45 с (`loadedModelsLen=1`, кнопка «Выгрузить»). Изменения, сделанные вне вкладки, не подхватывает никто. | гипотеза верна только для внешних изменений |
| (б) alias-функции потерялись из-за порядка скриптов | `index.html:2790` (refresh) грузится до `:2796` (renderer). В браузере: `typeof GgufModule.refreshBackends === 'function'`, `refreshDetail` — `function`, `refreshDetailPane` — `function`; в `Object.keys(window.GgufModule)` **110 ключей**, все `refresh*` на месте. | **отвергнута** |
| (в) провайдер `gguf` не регистрируется (`register()` вернул false) | Провайдер зарегистрирован: `_providers.gguf = 1`, событие `register page=gguf` пришло на 47 мс **раньше** регистраций из `registerPageProviders`. `defer`-скрипты выполняются до `DOMContentLoaded`, поэтому `window.GgufModule` к моменту `DOMContentLoaded` уже есть. | **отвергнута** |

### Корневая причина Д2

Нет ни одного источника, который перечитывает состояние загруженных моделей: `loadedModels`
заполняется только в `refreshDetail()`, а её зовут только явные действия оператора.
F5 «помогает» потому, что `render()` (`gguf-renderer.js:125-140`) заново строит страницу и
повторно запрашивает бэкенды, а после выбора бэкенда срабатывает `selectBackend → refreshDetail`.
То же самое, что Д3: у страницы нет собственного цикла обновления, а общий тик её не касается.

---

## 5. Д3 — «авто-обновление не работает, данные сами не приходят»

### 5.1. Что реально тикает (снимок до коммита)

* Авто включено по умолчанию: `localStorage['ollamalegion_auto_refresh'] = null` →
  `state.auto = true` (`data-refresh.js:47`, `:215-220`); в браузере `isAuto()=true`,
  `shouldPoll()=true`, подпись индикатора «Обновлено только что · авто».
* Тик — `startPeriodicRefresh` (`app.js:1848-1887`), интервал `WEBUI_CONFIG.REFRESH_INTERVAL = 5000`
  (проверено: сгенерированный `/js/modules/config.js` отдаёт `REFRESH_INTERVAL: 5000`).
  Тик вызывает только: `fetchClusterState`, `fetchAgents`, `fetchQueue`, `fetchQueueDetails`,
  `fetchQueueHistory`, `fetchSessions` (+`loadBackendLimits` при `currentPage==='settings'`).
  **Провайдеры страницы и `refreshPage` из тика не вызываются.**
* Второй цикл живёт в iframe монитора: `index.html:1140` грузит `monitor.html`
  на любой странице; внутри `webui/js/monitor/state.js:57` (`refreshInterval = 2000`) и
  `monitor/api.js:314,408` (`fetchAllSafe` + `setInterval`). Атрибуция по кадрам за 15 с на
  странице «Бэкенды»: **103 запроса, из них 78 (76 %) — из `monitor.html`**, 25 — из основного
  документа (3 тика × ~8 эндпоинтов).

Таймлайн за 30 с (одинаковый на всех проверенных страницах, счётчики запросов):

```
GET /api/v1/cluster            21      GET /api/v1/autopull          15
GET /api/v1/queue/details      21      GET /api/v1/autopull/status   15
GET /api/v1/queue/stats        21      GET /api/v1/virtualmodels     15
GET /api/v1/sessions           21      GET /api/v1/candidates        15
GET /api/v1/backends            9      GET /api/v1/models/operations 15
GET /api/v1/agents/stats        6      GET /api/v1/placement         15
GET /api/v1/queue/history       6      GET /api/v1/window/align      15
GET /api/v1/cluster/models/loaded 6
```

21 = 6 (тик 5 с) + 15 (iframe 2 с). При этом **ни одного** запроса к собственным источникам
страниц: `/api/v1/gguf/backends`, `/api/v1/backends/{id}`, `/api/v1/image/*`,
`/api/v1/diag/summary`, `/api/v1/proxy/logs` — 0 за 30 с.

### 5.2. Что происходит с DOM

Мутации DOM (MutationObserver на каждом `.page`, окно 30 с):

| Активная страница | Мутации активной страницы | `dashboard-page` | `settings-page` | остальные |
|---|---|---|---|---|
| backends | **0** | 132 | 18 | 0 |
| models | **0** | 132 | 18 | 0 |
| gguf | **0** | 132 | 18 | 0 |
| image (deep-link увёл на backends) | **0** | 132 | 18 | 0 |
| dashboard | 132 | 132 | 18 | 0 |

Индикатор всё это время показывал «Обновлено только что · авто»: подпись перерисовывается
своим `setInterval(…, 1000)` (`data-refresh.js:281-285`) без сети, поэтому «свежесть»
выглядит живой, даже когда страница не менялась.

Контроль (тот же наблюдатель, клик по `#refreshBtn`): 13-35 мутаций активной страницы за 4 с.
То есть механизм перерисовки работоспособен — его просто не зовут из таймера.

### 5.3. Вторая, отдельная причина: `App.context.currentPage` — снимок значения

`app.js:47-59` создаёт `App.context = { data: data, currentPage: currentPage, … }` —
`currentPage` это примитив, он **копируется** при инициализации. `switchPage()` меняет только
локальную переменную (`app.js:190`), а `App.context.currentPage` не обновляет.

Измерено: на всех 5 проверенных страницах `window.App.context.currentPage === "dashboard"`
(даже когда активна `backends-page`, `gguf-page` и т. д.).

Следствие в `app-listeners.js:171,186`:
```js
if (ctx.currentPage === 'dashboard') scheduleDashboardRender();
```
условие истинно **всегда** → на каждом тике перерисовывается скрытый дашборд
(132 мутации `dashboard-page` за 30 с с любой страницы), а страницы `backends`, `models`,
`gguf`, `diagnostics` не перерисовываются вообще (для них в коде нет ни одного
`currentPage ===` триггера: есть только queue (`app.js:1103,1121,1125,1133,1137`),
sessions (`:1149`), settings (`:1883`), logs (`:2318,2363,2558,2567,2683`),
agents (`:2580`), image (`:1063`)).

### 5.4. Авто-выключение не останавливает iframe монитора

С `localStorage['ollamalegion_auto_refresh'] = '0'`:
`isAuto()=false`, `shouldPoll()=false`, подпись «Обновлено 6 с назад · пауза», основной
документ почти молчит (2 запроса за 12 с), но из `monitor.html` уходит **67 запросов за 12 с**
(6 циклов × 11 эндпоинтов). Причина: `postMonitorPause()` (`app.js:1896-1902`) вызывается
только из обработчика переключателя (`app.js:95-107`) и `visibilitychange`
(`app.js:1928-1943`); при загрузке страницы с уже сохранённой паузой сообщение в iframe
не отправляется.

### Корневая причина Д3

Тик авто-обновления делает **не то же, что кнопка**: он обновляет общие данные и не вызывает
провайдеров активной страницы и не перерисовывает её. Собственные источники страниц
(GGUF-список и панель деталей, image-модели/загрузки, диагностика) не опрашиваются вообще;
перерисовка активной страницы происходит только там, где у fetch-функции есть свой
`currentPage ===` гард, а дашборд перерисовывается всегда из-за застывшего
`App.context.currentPage`. Индикатор при этом честно врёт «Обновлено только что».

---

## 6. Дельта: что изменил коммит `6680c112` (проверено на HEAD)

Коммит (13:40 MSK, во время диагностики) правит `webui/index.html`, `webui/js/app.js`,
`data-refresh.js`, `gguf-page-provider.js`, `diagnostics.js`, `image-page.js`,
`image-models-hf.js`, `autotune_settings.js` и тесты.

Проверка: собрал отдельный образ из HEAD **без сети** — `FROM ollama-legion/webui:check-r493`
+ `COPY webui/`, тег `ollama-legion/webui:check-head` (локальный), контейнер
`ol-webui-check-head` на `:18086` (порт 18085 занят чужим `ol-webui-verify`, не трогал).

| Измеренный факт (до коммита) | На HEAD |
|---|---|
| 7 кнопок обновления | в `index.html` 0 вхождений `imgModelsRefreshBtn`/`diagRefresh`/`autotune-refresh-btn`/`refresh-downloads`; в DOM на «Бэкендах» — только `#refreshBtn` и `#dataAutoToggle`. В GGUF остались только две иконки внутри страницы |
| провайдеров у `diagnostics` нет | есть (`diagnostics.js:373`) |
| тик не зовёт провайдеров и не перерисовывает | зовёт: `DR.refresh(currentPage)` + `refreshPage(currentPage)` (`app.js`, вставка R493 после `:1870`); на «Бэкендах» за 20 с — **28 мутаций `backends-page`** (было 0) |
| GGUF: внешняя загрузка видна только после F5 | строка появилась на **первом тике (t+5 с)** после старта загрузки; на странице появились запросы `/api/v1/gguf/backends`, `/api/v1/backends/{id}`, `.../proxy/api/models`, `.../models/files`, `.../config/runtime` (по 17-21 за ~2,5 мин) |
| `App.context.currentPage` застыл | **не изменилось**: на HEAD при `activeId=backends-page` `App.context.currentPage === "dashboard"`, и за 20 с `dashboard-page` получил **88 мутаций** (скрытая страница по-прежнему перерисовывается каждым тиком) |
| iframe монитора игнорирует сохранённую паузу | **не изменилось** (код `postMonitorPause` не тронут) |

Обнаруженная побочная цена правки: провайдер `gguf` вызывает `refreshBackends()`, и сразу за
ним `refreshPage('gguf') → GgufRenderer.render() → refreshBackends()` — в логе HEAD видны
**пары** `GET /api/v1/gguf/backends` на каждый тик (21 запрос за ~2,5 мин против 17 у
`/api/v1/backends/{id}`).

Ничего из перечисленного не отменяет диагноз §3-§5: на снимке до коммита дефекты
воспроизводились, после — Д1 и Д2 закрыты, Д3 закрыт частично (активная страница
перерисовывается, скрытый дашборд — всё ещё да, пауза не доходит до iframe монитора).

---

## 7. Что не проверено и почему

1. **Chromium из `node_modules/playwright` (131.0.6778.33)** запускается, но
   `browser.newPage()` не отвечает (>15 с) — все измерения сделаны в headless Edge
   157.0.4341.60. Поведение в Firefox/Safari не проверялось.
2. **Скрытая вкладка**: headless-страница всегда `visibilityState === 'visible'`;
   пауза опроса на `visibilitychange` (`app.js:1928-1943`) не проверялась.
3. **Поведение при `auto=off`, выставленном руками через UI** (а не через localStorage
   до загрузки) — проверено только состояние «пауза на старте» (§5.4).
4. **Страница «Монитор»** (её собственный iframe-цикл) отдельно не снималась; измерен
   только факт её запросов из фонового iframe.
5. **WebSocket**: вклад в перерисовку (`clusterState`/`statusChange`) не измерялся;
   проверялся только REST-цикл.
6. **GGUF с несколькими бэкендами и RPC-донором**: на стенде один llama_cpp-бэкенд
   (`cppworker-gpu-bundled-agent`), поэтому сценарий «выбрал другой бэкенд — данные не те»
   не воспроизводился.
7. **Кнопки `#imgModelsRefreshBtn` и `refresh-downloads`** не кликались (живут на неактивных
   табах Image-страницы); их поведение описано по коду (`image-page.js:973-974`,
   `image-models-hf.js:1404`), а не измерено.
8. **Переход по `?page=image`** на холодном старте уводит на «Бэкенды»:
   `ImageBackendsPage.syncVisibility(data.backends)` получает ещё пустой `data.backends`,
   скрывает пункт и вызывает `switchPage('backends')` (`image-backends-page.js:775-807`).
   Измерено: `?page=image` → `active=backends-page`, `currentPage='backends'`. Это
   побочный дефект, в Д1-Д3 не входит, отдельно не разбирался.
9. **`GET /api/v1/i18n` → 401** один раз при загрузке любой страницы (единственная
   консольная ошибка). Причина не исследовалась.
10. **Секрет**: значение `CPPWORKER_API_TOKEN` из `deployments/.env` в отчёт не переносилось.

---

## 8. Предлагаемые правки (минимальные, без реализации)

Пометки: **[сделано]** — уже закрыто коммитом `6680c112`; **[остаётся]** — на HEAD
не воспроизводится как исправленное. Номера строк — из снимка до коммита (§1.1); после
`6680c112` часть из них сместилась на 10-35 строк вниз.

1. **[сделано]** Убрать страничные кнопки обновления из разметки (до коммита:
   `index.html:805, :948, :1226, :2370` и две иконки в GGUF — `gguf-renderer-list.js:31`,
   `gguf-renderer-detail-render.js:160`), их действия отдать провайдерам:
   `diagnostics → load()`, image-загрузки `→ refreshDownloads()`, AutoTune `→ render()`
   в провайдера `settings`. Иконки GGUF оставить только если нужен «перечитать именно
   этот список» — иначе они дублируют `#refreshBtn`.
2. **[сделано]** В тик `startPeriodicRefresh` добавить `DataRefresh.refresh(currentPage)`
   + `refreshPage(currentPage)`; общие `fetch*` не дублировать, если у страницы есть
   свой провайдер.
3. **[остаётся]** `App.context.currentPage` (`app.js:47-59`) — либо геттер
   (`Object.defineProperty(ctx,'currentPage',{get})`), либо присваивание в `switchPage()`.
   Иначе скрытый дашборд перерисовывается каждым тиком с любой страницы
   (измерено: 88-132 мутации за 20-30 с) и маскирует отсутствие перерисовки других страниц.
4. **[остаётся]** `postMonitorPause` (`app.js:1896-1902`) вызывать один раз после
   `DataRefresh.mount()` с текущим `isAuto()` — иначе сохранённая «пауза» не останавливает
   2-секундный цикл `monitor.html` (67 запросов за 12 с при `shouldPoll()=false`).
5. **[остаётся, дешёвая гигиена]** Убрать двойной `GET /api/v1/gguf/backends` на тик:
   либо провайдер `gguf` не зовёт `refreshBackends()` (его всё равно зовёт `render()`),
   либо `refreshPage('gguf')` не пересобирает страницу целиком, когда изменились только данные.
6. **[остаётся]** `image` при холодном старте: `syncVisibility` не должен уводить со
   страницы, пока `data.backends` пуст (первый `fetchClusterState` ещё не ответил) —
   `image-backends-page.js:775-807`.
7. **[остаётся]** Один запрос `/api/v1/i18n` → 401 в консоли каждой страницы: либо не
   запрашивать без прав, либо не писать это в `console.error`.

Все правки укладываются в перечисленные файлы; переписывать `data-refresh.js` не требуется —
реестр провайдеров рабочий, дыра в том, кто и когда его вызывает.

---

## 9. Как воспроизвести измерения

Скрипты диагностики (вне репозитория): `C:\ollamalegion-diag\`
(`pages-probe.js` — кнопки/провайдеры/перехват сети по страницам, `d3-auto.js` — 30-секундный
таймлайн и мутации DOM, `d2-gguf.js` / `d2ui.js` — внешняя и UI-загрузка модели,
`inventory.js` — инвентаризация кнопок, `frames.js` / `frames-off.js` — атрибуция фоновых
запросов по кадрам и поведение при паузе, `verify-head.js` — то же на сборке из HEAD).
Токен передаётся через переменную окружения `OL_TOKEN`, в скрипты и в отчёт не вписан.
Сырые результаты: `pages-probe.json`, `d3.json`, `d2.json`, `d2ui.json`, `inventory.json`,
`nav.json`, `verify.json` в том же каталоге.
