# Справка и шаблоны: страница WebUI поверх готового API

**Статус.** Страница реализована, тесты и статические проверки зелёные, три
обязательные мутации проверены (красный → зелёный), серверные ручки проверены
живьём на временном конфиге. Ничего не коммитилось и не пушилось.
`internal/**`, `deployments/**`, `cmd/**` не изменялись ни на строку.
Комментарии в коде и этот отчёт — по-русски.

**Что читать вместе с отчётом:**
* контракт хранения и API — `docs/plans/2026-11-17-reference-templates-design.md`
  (§2 схемы полей, §3 ручки и коды ошибок, §4 UI, §6 решения человека, §7 «что НЕ надо делать»);
* что фактически реализовано на сервере — `docs/plans/2026-11-17-reference-api-implementation.md`
  (§2.3 «зарегистрированные ручки» — источник правды по путям и кодам);
* живая диагностика WebUI (почему одна кнопка обновления) —
  `docs/plans/2026-11-17-webui-refresh-diagnosis.md` §2–§6.

---

## 1. Что сделано (по пунктам задания A–D)

| Пункт | Состояние | Где |
|---|---|---|
| **A. Навигация, страница, роутер** | сделано | `webui/index.html` (пункт меню + контейнер + тег `<script>`), `webui/js/app.js` (`pageFromURL`, `getPageTitle`, `refreshPage → case 'reference'`) |
| **B. Модуль страницы: две вкладки, карточки, фильтр, редактор, шаблоны, preview/apply** | сделано | `webui/js/modules/reference-page.js` (новый, 1787 строк с комментариями) |
| **C. Обёртки HTTP `Api.reference`** | сделано | `webui/js/modules/api.js` (объект `reference` + внутренний `referencePath`) |
| **D. Человеческие сообщения об ошибках** | сделано | `reference-page.js → errorInfo/errorText/errorHtml` + ключи `reference.error.*` в i18n |
| Дополнительно: прогресс применения переиспользован | сделано | `webui/js/modules/cppworker-params.js` — у `applyProfileWithProgress` появился третий (необязательный) аргумент `applier` |
| Дополнительно: раскладка/статусы страницы | сделано | `webui/css/pages.css` (секция «R493: страница Справка и шаблоны») |
| Дополнительно: i18n | сделано | `webui/js/i18n/ru.js`, `en.js` — 119 ключей (одинаковый набор в обоих пакетах) |

### 1.1 Почему страница смонтирована, а не пересобирается

`app.js → refreshPage('reference')` делает **только** `ReferencePage.mount()`,
а `mount()` идемпотентен (второй вызов возвращает `false`). Это ровно тот
приём, который R493 применил к `case 'gguf'`: `refreshPage` вызывает и клик по
странице, и тик авто-обновления, поэтому полная отрисовка внутри него означала
бы пересборку DOM каждые `REFRESH_INTERVAL` (5 с) со сбросом фильтра, выбранной
модели/бэкенда и позиции прокрутки. Данные обновляет **провайдер** `reference`,
зарегистрированный в реестре `data-refresh.js`:

```js
window.DataRefresh.register('reference', function () { return load(); });
```

`load()` перечитывает три источника (`GET /reference/recommendations`,
`GET /reference/templates`, `GET /api/v1/cluster/models/loaded`) и
перерисовывает списки; ошибка **пробрасывается** дальше, чтобы индикатор
свежести в шапке показал ту же причину, а не «Обновлено только что»
(`data-refresh.js:markError`).

### 1.2 Где живёт состояние

`reference-page.js` — владелец состояния страницы (образец —
`settings-registry.js`): `state.recommendations`, `state.templates`,
`state.filter {search, model}`, `state.loadedModels/loadedByModel`,
`state.tpl {id, model, backendId, mode, preview, error, busy}`,
`state.editor`. Значения полей при вводе пишутся в состояние, а не читаются из
DOM, поэтому повторная отрисовка (после ответа сервера или тика) не теряет
набранное. Редактор перерисовывается только по структурным действиям
(открыть/добавить строку/удалить строку), а не на каждый символ.

### 1.3 Список «где загружена модель» — только `cluster/models/loaded`

`Api.clusterModels.loaded()` → `{count, models:[{name, backendId, backendType,
contextLength, …}]}` — `handlers_cluster_models.go:20-53`. Для llama.cpp
`GET /api/v1/models` отдаёт `models:null` (design §3.5), поэтому второго
источника нет. Селектор бэкенда показывает **только те бэкенды, где модель
загружена**; сравнение имён нечувствительно к регистру и к суффиксу `.gguf`
(та же нормализация, что на границе профилей).

### 1.4 «Применить»: гейт, режим и переиспользование модалки

`applyGate({model, backendId, mode, targets})` возвращает `{disabled, reason}`:

| Ситуация | reason | Что видит оператор |
|---|---|---|
| модель не выбрана | `model_required` | «Сначала выберите модель.» |
| нет ни одной загруженной цели | `no_loaded_target` | «Модель нигде не загружена — применять некуда…» |
| `mode="profile+reload"`, бэкенд не выбран | `backend_required` | «Выберите бэкенд, где загружена модель…» |
| бэкенд выбран, но модель там не загружена | `backend_not_loaded` | «На выбранном бэкенде эта модель не загружена…» |
| `mode="profile-only"` (reload не запускается), цели есть | — | кнопка активна, бэкенд не нужен |

Проверка «есть ли загруженная цель» стоит **до** разбора режима: требование
оператора — «не быть активной, если в targets нет ни одной загруженной цели»,
в любом режиме. Дальше режим решает только одно: нужен ли явный бэкенд.

Причина совпадает с серверным контрактом: `mode="profile+reload"` с пустым
`backendId` → **400 `backend_required`** (решение человека №3), то есть UI
запрещает ровно тот запрос, который сервер всё равно отклонит.

**Второго механизма reload нет.** Применение идёт одной из двух дорог:

* `mode="profile-only"` → прямой `Api.reference.apply({model, mode})`. Reload не
  запускается, прогресса нет — показывать «Reload модели на бэкендах…» было бы
  враньём.
* `mode="profile+reload"` → **существующая** модалка
  `CppWorkerParams.applyProfileWithProgress(model, backendId, applier)`.
  Модалка осталась единственной: в неё передаётся функция применения
  (`Api.reference.apply`), а SSE-прогресс (`cppworkerApplyProgress.streamProgress`)
  и polling — те же, что и для профилей. Для этого в `cppworker-params.js`
  добавлен третий необязательный аргумент `applier` и `progressModel`
  (имя модели для progress/status берётся из ответа сервера — он его
  нормализует). Без аргумента поведение прежнее, остальные вызовы не затронуты.

Перед применением, если в предпросмотре память уже сказала «не помещается»,
оператор получает явный `window.confirm` и только после согласия уходит
`confirmNonFeasible: true` — иначе 422 пришёл бы уже после нажатия.

### 1.5 Ошибки сервера — по-человечески (пункт D)

`errorInfo(err)` разбирает `err.body` (его кладёт `Api.request`) и отдаёт
`{code, title, hint, details}`:

| Код | Что видит оператор |
|---|---|
| 503 `store_unavailable` | «Справочник недоступен: хранилище не инициализировано» + «проверьте `LB_MODEL_REFERENCE_PATH` и каталог `state.json` (`LB_STATE_PATH`)» |
| 400 `backend_required` | «Выберите бэкенд, где загружена модель» + «применение на все узлы сразу задевает чужие и недоступные» |
| 409 `revision_conflict` | «Запись изменена другим оператором — перечитайте её» + `current=`/`sent=` (какая ревизия у вас и какая на сервере) |
| 422 `profile_does_not_fit_memory` | «Набор не помещается в память» + текст вердикта + поля блока `memory` (`stage=…`, `freeMiB=…`) |
| 502 `reload_failed` | «Модель не перезагрузилась ни на одном бэкенде» + сообщение по каждому бэкенду |
| 404 `*_not_found` | «Запись не найдена: возможно, её удалили» |
| прочие коды (`invalid_value`, `unknown_param`, `invalid_mode`, …) | заголовок «Сервер отклонил запрос» + дословный `message` сервера + `field=` |
| timeout / сеть | «Балансер не ответил вовремя» / «Не удалось выполнить запрос» + исходный текст |

Ни одного «что-то пошло не так»: у каждой ветки есть причина и подсказка.

### 1.6 Что переиспользовано (правило «не дублируй знания»)

| Знание | Источник | Как используется |
|---|---|---|
| метаданные полей профиля | `window.GgufModule.MODEL_PARAM_SPECS` (`gguf-renderer-detail-render.js:2490-2524`, экспорт `:3949`) | селект поля, тип контрола (number/select/checkbox), `min`/`max`/`step`, `options`, подписи |
| «где загружена модель» | `Api.clusterModels.loaded()` | список моделей и бэкендов шаблона |
| прогресс применения | `CppWorkerParams.applyProfileWithProgress` | режим `profile+reload` (см. §1.4) |
| HTTP-заголовки и таймауты | `Api.request`/`Api.reference` | модуль не вызывает `fetch` и не собирает `X-API-Token`/`X-CSRF-Token` |
| admin-видимость | `data-admin-only` + `auth.js` (включая `MutationObserver` для динамических кнопок) | все мутирующие кнопки, включая нарисованные из JS |
| i18n | `window.I18N.t(key, fallback)` | все подписи; ключи в обоих пакетах |

---

## 2. Файлы

### 2.1 Новые

| Файл | Содержимое |
|---|---|
| `webui/js/modules/reference-page.js` | страница целиком: состояние, чистые помощники (`applyGate`, `loadedTargetsFor`, `filterRecommendations`, `globMatch`, `formatDiffRow`, `renderDiff`, `renderPreview`, `renderMemory`, `renderTargets`, `renderParamsTable`, `renderRecommendationRow`, `renderTemplateCard`, `renderTemplateDetail`, `errorInfo`, `errorText`), действия (редактор, удаление, `to-template`, preview, apply), идемпотентный `mount()`, `load()`, регистрация провайдера, экспорт `window.ReferencePage` |
| `webui/js/modules/reference-page.test.js` | 30 проверок логики без DOM-библиотек (стабы `global.window`, `global.document`, `window.Api`, `window.I18N`, `window.GgufModule`) |
| `webui/tests/reference-page-static.test.js` | 19 статических инвариантов (меню/контейнер/порядок скриптов/роутер/нет своей кнопки обновления/провайдер/`data-admin-only`/i18n в обоих пакетах/переиспользование знаний/обёртки в `api.js`) |

### 2.2 Изменённые

| Файл | Что изменено | Масштаб |
|---|---|---|
| `webui/index.html` | пункт меню `data-page="reference"` + `data-i18n="nav.reference"`; контейнер `<div class="page" id="reference-page">` с статическим каркасом (вкладки, поиск, фильтр, панели, контейнер редактора); тег `<script defer src="js/modules/reference-page.js?v=R493">` перед `js/app.js` | +53 |
| `webui/js/app.js` | `pageFromURL`: `reference` в списке известных страниц; `getPageTitle`: `reference`; `refreshPage`: `case 'reference'` — только `ReferencePage.mount()` | +9/−1 |
| `webui/js/modules/api.js` | объект `Api.reference` (`list/get/upsert/remove/toTemplate/preview/apply`) + внутренний `referencePath(kind, id, action)` | +143 |
| `webui/js/modules/cppworker-params.js` | `applyProfileWithProgress(modelName, backendID, applier)` — необязательный источник применения; `progressModel` из ответа для SSE/status | +23/−6 |
| `webui/js/i18n/ru.js`, `webui/js/i18n/en.js` | по 119 ключей (`nav.reference`, `header.reference`, `reference.*`) | +117/−1 каждый |
| `webui/css/pages.css` | секция `.reference-*` (карточки, вкладки, статусы, предпросмотр, редактор) на переменных темы | +345 |

`internal/**`, `deployments/**`, `cmd/**`, `CHANGELOG.md` — не изменялись.
Секретов и PII в коде/отчёте нет.

---

## 3. Тесты и проверки

### 3.1 Полный прогон WebUI

```
node scripts/run-webui-tests.js --quiet
Наборов: 107, зелёных: 107, красных: 0
ИТОГ: все наборы WebUI зелёные
```

На старте работы было **104** набора (все зелёные). Добавлены два моих
(`reference-page.test.js` — 30 проверок, `reference-page-static.test.js` —
19 проверок); ещё один набор в дереве появился от параллельной работы R493
(`webui/tests/providers-callable.test.js`) — он не мой и тоже зелёный.

Отдельно:

```
node webui/js/modules/reference-page.test.js
ИТОГ: все проверки пройдены (30)

node webui/tests/reference-page-static.test.js
ИТОГ: все проверки пройдены (19)
```

### 3.2 Обязательные проверки перед сдачей

| Команда | Результат |
|---|---|
| `node --check webui/js/modules/reference-page.js` | OK |
| `node --check webui/js/modules/api.js` | OK |
| `node --check webui/js/app.js` | OK |
| `node --check webui/js/modules/cppworker-params.js` | OK |
| `node --check webui/js/i18n/ru.js`, `en.js` | OK |
| `python scripts/check_iife_exports.py` | `[PASS] all 171 files export IIFE public API to window`, exit 0 |
| `python scripts/check_webui_i18n_keys.py` | `проверено атрибутов — 558, пакетов — 2`, `все ключи страниц присутствуют в ru.js и en.js`, exit 0 |
| `node scripts/i18n_diff.js webui/js/i18n/en.js webui/js/i18n/ru.js --strict` | `EN: 2560 ключей`, `RU: 2560 ключей`, `Только в EN: 0`, `Только в RU: 0`, `Пустых значений в EN/RU: 0`, exit 0 |

---

## 4. Мутационный прогон (обязательный пункт G)

Каждая мутация — минимальная правка, которая **компилируется**
(`node --check` = OK) и ломает ровно одну проверку. После каждой — возврат и
зелёный прогон.

### 4.1 Убрать регистрацию провайдера `reference`

Файл `webui/js/modules/reference-page.js`, из `registerProvider()` удалён весь
блок `window.DataRefresh.register('reference', function () { return load(); });`.

```
node --check webui/js/modules/reference-page.js   → OK
node webui/tests/reference-page-static.test.js
  FAILED: 1
  - модуль регистрирует провайдера DataRefresh.register('reference', ...):
    нет регистрации провайдера reference — кнопка и авто-тик в шапке страницу
    не обновят
```

После возврата:

```
node scripts/run-webui-tests.js --filter=reference
  ✓ webui/js/modules/reference-page.test.js
  ✓ webui/tests/reference-page-static.test.js
Наборов: 2, зелёных: 2, красных: 0
```

Заодно эта мутация вскрыла слабость самой проверки: первая версия статического
теста искала строку `DataRefresh.register('reference'` по всему файлу и
оставалась зелёной, потому что та же строка есть в комментарии-шапке модуля.
Проверка исправлена на поиск **строки кода** (`^[ \t]*window\.DataRefresh\.register\('reference'`).

### 4.2 Разрешить «Применить» без выбранного бэкенда

Файл `webui/js/modules/reference-page.js`, `applyGate()`:
`if (!backendId) return { disabled: true, reason: 'backend_required' };`
заменено на `return { disabled: false, reason: '' };`. Мутация прогнана дважды —
до и после перестановки проверки «нет загруженных целей» выше разбора режима;
результат одинаковый.

```
node --check webui/js/modules/reference-page.js   → OK
node webui/js/modules/reference-page.test.js
  ✗ гейт: без выбранного бэкенда «Применить» запрещена (сервер ответит 400
    backend_required) — без явного бэкенда применять нельзя: «на все узлы»
    задевает чужие/недоступные
  FAILED: 1
```

После возврата — 30/30 зелёных.

### 4.3 Убрать ветку обработки `backend_required`

Файл `webui/js/modules/reference-page.js`, из `errorInfo()` удалён блок
`if (code === 'backend_required') { … }` (тогда 400 попадает в общую ветку
«Сервер отклонил запрос»).

```
node --check webui/js/modules/reference-page.js   → OK
node webui/js/modules/reference-page.test.js
  ✗ ошибка 400 backend_required: подсказка выбрать бэкенд — нет слова «бэкенд»:
    Сервер отклонил запрос — backend_required
  FAILED: 1
```

После возврата — 30/30 зелёных, полный прогон 107/107.

---

## 5. Живая проверка серверных ручек (пункт H)

Сервер в запущенном контейнере собран ДО появления ручек, поэтому живые вызовы
из WebUI были бы недоступны. Собран локальный бинарь и поднят временный стенд —
**вне репозитория**, на временном конфиге и временном хранилище (каталог
`%TEMP%\ol-refcheck`, удалён после проверки; настройки/файлы стенда оператора не
трогались).

```
go version go1.25.4 windows/amd64
go build -tags llama_stub -o dist\balancer-refcheck.exe ./cmd/balancer   → exit 0
```

Тег `llama_stub` — тот же, что в CI-линте (`.golangci.yml`): без него cgo-сборка
`c/bridge` падает на отсутствии `llama.h` (состояние дерева, не связанное с этой
задачей). Временный конфиг — копия `config/config.test-stub.json` с портами
28080/28081/28079 и `statePath` во временном каталоге.

Лог старта (фрагмент):

```
{"level":"info","caller":"reference/store.go:172","msg":"reference: однократный сид из docs/operations/model-tuning-reference.ru.md выполнен",
 "path":"C:\\Users\\knaga\\AppData\\Local\\Temp\\ol-refcheck\\model-reference.json","recommendations":7,"templates":0}
[API] Listening on 0.0.0.0:28081
```

Запросы (`curl.exe -s -w "\nHTTP %{http_code}\n"`):

| Запрос | Ответ |
|---|---|
| `GET /api/v1/reference/templates` | **200** `{"items":[],"total":0,"schemaVersion":1}` |
| `GET /api/v1/reference/recommendations` | **200**, `total: 7`, `schemaVersion: 1`, UTF-8 русские заголовки (например `«DeepSeek-R1-Distill: окно 32K, размышления ВКЛ и потолок вывода 8192»`) |
| `GET /api/v1/reference/templates/nope` | **404** `{"error":"template_not_found","id":"nope","message":"шаблон \"nope\" не найден"}` |
| `PUT /api/v1/reference/templates/live-check` `{"name":"Проверка вживую","version":1,"params":{"contextLength":32768,"kvCacheType":"q8_0"},"comment":"…"}` | **200** `{"status":"ok","template":{…,"revision":1}}` |
| `POST /api/v1/reference/templates/live-check/apply` `{"model":"Qwen3-4B-Instruct-2507-Q4_K_M","mode":"profile+reload"}` | **400** `{"error":"backend_required","message":"выберите бэкенд, где загружена модель: применение на все сразу задевает чужие/недоступные узлы","model":"Qwen3-4B-Instruct-2507-Q4_K_M"}` |
| `POST /api/v1/reference/templates/live-check/preview` `{"model":"Qwen3-4B-Instruct-2507-Q4_K_M"}` | **200** `{"ok":true,"model":"…","templateId":"live-check","templateVersion":1,"profile":{…},"valid":{"ok":true,"violations":[]},"diff":[{"field":"contextLength","current":0,"next":32768},{"field":"kvCacheType","current":null,"next":"q8_0"}],"targets":[{"backendId":"cppworker-stub","backendType":"llama_cpp","status":"starting","loaded":false}],"memory":{"checked":false,"feasible":true,"degraded":…}}` |
| `DELETE /api/v1/reference/templates/live-check` | **200** `{"id":"live-check","status":"ok"}` |
| `GET /api/v1/cluster/models/loaded` | **200** `{"count":0,"models":[]}` |

Что это доказывает по существу:

1. Формы ответов **совпадают** с теми, под которые написан UI: `valid.violations`,
   `diff[].current/next`, `targets[].loaded`, `memory.checked/feasible/stage`.
2. `backend_required` — реальный 400 с тем самым текстом; ветка `errorInfo`
   показывает его, а не «HTTP 400».
3. `memory.checked=false` — штатное состояние, когда cppworker не ответил: UI
   обязан называть это «проверить не удалось», а не «помещается» (и называет —
   проверка «вердикт памяти: «проверить не удалось» — это не «помещается»»).
4. `targets[].loaded=false` (модель нигде не загружена) — ровно тот случай, когда
   «Применить» должна быть неактивна; гейт это делает.

---

## 6. Что НЕ проверено живьём (честный список)

| Не проверено | Почему | Чем рискованно |
|---|---|---|
| **Страница в реальном браузере** (клик по меню, вкладки, редактор, `confirm`/`prompt`) | в этой сессии нет браузерного прогона; проверена логика (30 проверок) и статические инварианты (19) | возможны визуальные дефекты раскладки (CSS новый) и поведение на узких экранах — в тестах не воспроизводится |
| **Реальный apply с запущенным cppworker** (`force:true`, `reload`, токен воркера) | на временном стенде нет cppworker (бэкенд `starting`), а мутировать стенд оператора нельзя | применяется существующий путь профилей, но фактическую перезагрузку модели и окно после reload никто не наблюдал |
| **202 + SSE/поллинг прогресса** | требует занятого бэкенда с активными запросами | модалка и её SSE — существующий код; связка с `Api.reference.apply` проверена только по типам ответа |
| **Вердикт памяти `feasible=false` и `confirmNonFeasible`** | cppworker нет (`memory.checked=false`) | ветка «спросить и повторить с подтверждением» исполнялась только в тесте на `errorInfo`, не вживую |
| **Модель загружена на ДВУХ бэкендах** | на стенде `count:0` | селектор бэкенда и гейт покрыты тестами, но реальный выбор между двумя узлами не наблюдался |
| **Права viewer** (кнопки скрыты, мутации 403) | нет пользователей (`auth: пользователей нет`) — проверка прав делалась по коду (`auth.js` + `data-admin-only`), статическим тестом | сервер — арбитр (`AuthMiddleware`), UI-скрытие это только UX |
| **`LB_MODEL_REFERENCE_PATH` в compose** | переменная не добавлялась (`deployments/**` вне задачи) | по умолчанию путь = каталог `state.json`, то есть `/app/data/model-reference.json` на существующем томе |
| **Кэш-бастинг `?v=R493`** | в рабочем дереве версия ставится скриптом (`scripts/bump_webui_cache_bust.py` при сборке образа) | если версия не поднимется при выкатке, браузер возьмёт старый `index.html` без страницы |

---

## 7. Границы (что НЕ трогал)

* `internal/**` — ни строки (серверные ручки уже готовы и протестированы).
* `deployments/**`, `cmd/**`, `config/**` — не изменялись. Временный стенд жил в
  `%TEMP%`, конфиг и `model-reference.json` создавались там же и удалены.
* `CHANGELOG.md` — не правил (по заданию); формулировка для ведущего агента:
  * `feat(webui)`: страница «Справка и шаблоны» (`reference`): вкладки
    «Рекомендации»/«Шаблоны», фильтр по модели, редактор записей (admin),
    предпросмотр (diff + нарушения + вердикт памяти) и применение шаблона на
    явно выбранный бэкенд через существующую модалку прогресса; `Api.reference`;
    провайдер `reference` в `data-refresh` (своей кнопки обновления нет);
    119 ключей i18n (ru/en); 49 проверок в двух новых наборах.
* `docs/plans/README.md` — добавлена одна строка в таблицу «Планы в работе (WIP)».
* Коммитов и пушей нет.

---

## 8. Как воспроизвести проверку

```powershell
node scripts/run-webui-tests.js                       # 107 наборов
node webui/js/modules/reference-page.test.js          # 30 проверок
node webui/tests/reference-page-static.test.js        # 19 проверок
python scripts/check_iife_exports.py
python scripts/check_webui_i18n_keys.py
node scripts/i18n_diff.js webui/js/i18n/en.js webui/js/i18n/ru.js --strict
```

Живая проверка ручек (временный стенд, ничего не пишет в репозиторий):

```powershell
go build -tags llama_stub -o dist\balancer-refcheck.exe ./cmd/balancer
# конфиг: копия config/config.test-stub.json с портами 28080/28081/28079
#         и statePath = <временный каталог>\state.json
.\dist\balancer-refcheck.exe -config <временный каталог>\config.json -log-level warn
curl.exe -s http://127.0.0.1:28081/api/v1/reference/templates
```
