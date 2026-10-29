# Аудит настроек WebUI ↔ балансер (2026-10-29)

Статус: 🔍 аудит выполнен, код НЕ менялся. Основание: живая жалоба оператора —
(1) в env стоит флаг ограничения контекстного окна, из-за которого непонятно,
почему модель «поддерживает больше, а фактически ограничена»; (2) всё, что
задаётся в env, должно быть изменяемо из WebUI; (3) ранее заявленные баги
настроек не решены, значит проблема архитектурная — вероятно, из-за
дублирования кода в WebUI (правка в одном месте не лечит).

Ветка `centurion`, HEAD `a6bf1c2e`. Метод: трассировка кода (`grep` по
`os.Getenv`/`LookupEnv`, обработчикам, резолверам) + точечные обращения к живому
API. Тестам WebUI как доказательству НЕ доверяем (см. §E).

---

## 0. Главные выводы

1. **Настроек два независимых мира.** 209 уникальных env-переменных читаются
   кодом напрямую (`os.Getenv`/`os.LookupEnv`, см. приложение), и параллельно
   WebUI пишет в `config.json` / runtime-overrides / `state.json` / `localStorage`.
   Единого реестра настроек нет, поэтому «изменяемо из WebUI» — свойство
   КОНКРЕТНОЙ переменной, а не системы. Любая новая настройка по умолчанию
   env-only, и оператор об этом не узнаёт.
2. **Окно контекста имеет 4+ источника истины** и **два разных места применения
   env-потолка**: обработчик (`effectiveAutoReloadCeiling`, R255) и решатель
   о перезагрузке (`nctx_reload.go` — сырой `cfg.AutoReloadMaxNCtx`, строки 676,
   686, 811). Из-за второго значение из WebUI молча режется, и этого не видно
   ни в UI, ни в ответе API — ровно жалоба (1).
3. **Один смысл пишется тремя путями:** страница настроек
   (`PUT /api/v1/cluster/config`, авто-сохранение 55 полей), карточка модели
   (`PUT /api/v1/cppworker/model-profiles/{name}`) и загрузочные дефолты
   (`PUT /api/v1/cppworker/load-defaults`). Плюс `contextPerSeq`, который
   перемножается на слоты. Это и есть «правка в одном месте не лечит».
4. **UI не показывает, чем ограничено значение.** Профиль отдаёт
   `contextLength`/`contextLengthMax`/`contextLengthAuto`/`contextPerSeq`, но ни
   `effective`, ни «ограничено env X», ни «ограничено физикой VRAM/GGUF».
5. **Тесты WebUI доказательством не являются:** 73 файла, у 12+ стабы
   `fetch`/DOM (`users-admin` 12, `auth` 10, `renderers-image` 6 …), то есть они
   проверяют собственные моки и пройдут при полностью сломанной связке с API.

---

## A. Инвентаризация env (209 переменных)

Полный машинно-собранный список с `file:line` — в приложении (генерируется из
кода; там же места чтения). Ниже — по областям, с акцентом на те, что оператор
воспринимает как «настройку».

### A1. Контекстное окно / n_ctx (13 переменных)

| Переменная | Где читается | Что регулирует | Изменяемо без рестарта |
|---|---|---|---|
| `LB_NCTX_RELOAD_MAX_N_CTX` | `nctx_reload_config_bridge.go:126`, `nctx_declared_r255.go:125`, `nctx_reload.go:875`, `nctx_reload.go:676/686/811` | потолок авто-перезагрузки окна (и, к сожалению, отсечка значения из UI — см. §C) | нет |
| `LB_NCTX_RELOAD_ENABLED` | `nctx_reload_config_bridge.go:115` | разрешена ли перезагрузка под окно | нет |
| `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR` | `nctx_reload_config_bridge.go:131` | запас VRAM при выборе окна | нет |
| `LB_NCTX_RELOAD_TIMEOUT_SEC` | `nctx_reload_config_bridge.go:136` | сколько ждать перезагрузку | нет |
| `LB_NCTX_RELOAD_ASYNC` | `nctx_reload_config_bridge.go`, `auto_continue.go:758` | перезагрузка в фоне | нет |
| `LB_NCTX_PREFLIGHT_ENABLED` | `nctx_reload_config_bridge.go:102,121` | включать ли preflight-проверку окна перед запросом | нет |
| `LB_NCTX_PREFLIGHT_ASYNC_RELOAD` | `nctx_reload_config_bridge.go` | перезагрузка из preflight в фоне | нет |
| `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` | `balancing.go:205`, `nctx_reload.go:114,239`, `model_management.go:1577` | сколько клиент ждёт роста окна | нет |
| `LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER` | `balancing.go:215` | множитель ожидания | нет |
| `LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC` | `balancing.go` | добавка к ожиданию | нет |
| `LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC` | `nctx_reload.go:108,223` | `Retry-After` при фоновой перезагрузке | нет |
| `LB_NCTX_GROW_FOR_AGENTS` | `preflight_agent_window_r253.go` | растить окно по оценке промпта без `num_ctx` от клиента | нет |
| `LB_AGENT_OUTPUT_RESERVE_TOKENS` | `preflight_agent_window_r253.go` | бюджет вывода в расчёте окна | нет |

### A2. Таймауты и стриминг (ключевые)

| Переменная | Где читается | Зачем | Рестарт |
|---|---|---|---|
| `LB_STREAMING_NEVER_TIMEOUT` | `proxy_timeout.go:40-47` | снять все стрим-таймауты | да |
| `LB_STREAMING_TIMEOUT_SEC` / `LB_STREAMING_IDLE_TIMEOUT_SEC` | `proxy_timeout.go:242/275` | общий/idle таймаут стрима | да |
| `LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC` | `first_byte_default_r189.go:32` | первый байт (дефолт 180 с) | да |
| `LB_STREAM_MAX_LINE_BYTES` | `stream_limit_r259.go` | лимит строки SSE (дефолт 8 МиБ) | да |
| `LB_OPENAI_AUTO_STREAM` | `proxy_request_openai_auto_stream.go:39` | превращать non-stream запрос в стрим | да |
| `LB_OPENAI_AUTO_STREAM_MAX_SEC` / `_MAX_BYTES` | там же (R258) | пределы буферизации non-stream | да |
| `LB_TOOL_CALL_BUFFER_BYTES` | `qwen_tool_markers_r266.go` | буфер сборки текстового tool-call (64 КиБ) | да |
| `LB_AUTO_CONTINUE_ON_TRUNCATION` + `_MAX_TOKENS` | `auto_continue.go:353,771` | авто-продолжение усечённого ответа | да |

### A3. Очередь / admission / загрузка

`LB_ADMISSION_WAIT_SEC` (`admission_queue.go`), `LB_AUTO_LOAD_ASYNC`,
`LB_AUTO_LOAD_WAIT_SEC`, `LB_SHUTDOWN_DRAIN_SEC` — все env-only, рестарт.

### A4. Раскладка RPC, image, i18n

`LB_RPC_*` (10), `CPPWORKER_RPC_*` (14), `RPC_WORKER_*` (13), `SDWORKER_*`,
`LB_API_*` (8), `I18N_*`: полностью env-only. Часть дублируется настройками
раскладки в WebUI (`/api/v1/placement`, роли бэкендов, `rpc-layout`) — и там
env выступает «третьим мнением» (§C, конфликт 3).

### A5. Вывод по A

Из 209 переменных **ни одна не читается через серверный реестр** — только
`os.Getenv` в точке использования. Поэтому:
- нельзя перечислить настройки из UI (нет метаданных: тип, дефолт, описание, scope);
- нельзя проверить значение на валидность один раз;
- нельзя изменить без рестарта, даже если значение читается на каждый запрос.

---

## B. Инвентаризация WebUI (что реально пишется и куда)

| Что в UI | Где контрол | Эндпоинт | Хранилище | Читается балансером |
|---|---|---|---|---|
| Страница «Настройки»: ~55 полей (в т.ч. `#ctxSize` → `contextLength`) | `app.js:1256`, `app.js:1616`, авто-сейв `app.js:1755`→`saveSettings` | `PUT /api/v1/cluster/config` (`api.js:217`) | `config.json` | да (глобальный `llamaCpp.contextLength`) |
| Карточка модели: окно `contextLength` (max 1048576), `contextPerSeq`, `contextLengthMax`, `contextLengthAuto` | `gguf-renderer-detail-render.js:2444` (+ поля профиля) | `PUT /api/v1/cppworker/model-profiles/{name}` (`api.js:417`), apply → `/apply` | `runtime-overrides` / профили | да (резолвер окна, R255/R265) |
| Загрузочные дефолты (`load-defaults`) | `api.js:481/490` | `PUT /api/v1/cppworker/load-defaults` | override | да (дефолт для моделей без профиля) |
| Раскладка (роли, доноры, стратегия) | `backend-merge.js` | `PUT /api/v1/placement`, `POST .../role`, `.../rpc-layout` | config + `backend-roles.json` | да (гард раскладки) |
| Флаг «не использовать бэкенд» | `backend-merge.js` (`bmDisabled`) | `POST /api/v1/backends/{id}/enabled` | state + `backend-disabled.json` | да (селектор) |
| Пределы бэкенда (concurrency/models) | карточка бэкенда | `PUT /api/v1/backends/{id}` | config | да |
| Язык интерфейса | шапка | `PUT /api/v1/i18n` | config | да |
| Канал между узлами (Мбит/с) | `backend-merge.js` `#bmLinkMbps` | **localStorage** `ollamalegion_link_mbps` + уходит в тело плана | `localStorage` | **нет** (влияет только на расчёт, отправляемый с запросом) |

**Ключевой факт:** у настроек нет общего описания. Каждая страница знает свой
эндпоинт, своё поле и своё хранилище. Поэтому «добавить настройку» = вручную
сделать контрол + запрос + валидацию, а env при этом остаётся жить своей жизнью.

---

## C. Сверка env ↔ WebUI. Почему env режет окно из WebUI

### C1. Цепочка контекстного окна (state of the art после R255/R265)

```
WebUI карточка модели
  gguf-renderer-detail-render.js:2444  (field=contextLength, max=1048576)
    → PUT /api/v1/cppworker/model-profiles/{name}   (api.js:417)
      → профиль модели: contextLength (суммарное), contextPerSeq (на клиента), contextLengthMax (кап), contextLengthAuto
        → R255 resolveDeclaredWindow(total, perSeq, slots): при расхождении побеждает contextLength, perSeq не уходит воркеру
          → R265 profileCapCtx(fold): contextLengthMax как источник капа
            → отправка воркеру: contextLength (суммарное) + X-Cpp-Ctx
              → cppworker: ApplyCppCtxHeaderWithOptions (header — UPPER LIMIT, не поднимает) + computeContextWarningFromLoaded (n_predict=0 при prompt≥n_ctx)
```

Параллельно и независимо:

```
env LB_NCTX_RELOAD_MAX_N_CTX → AutoReloadMaxNCtx (nctx_reload_config_bridge.go:126)
   ├─ effectiveAutoReloadCeiling = max(env, заявленное оператором)   ← ТОЛЬКО 2 места:
   │     nctx_declared_r255.go:122,139 ; nctx_reload_handlers.go:860
   └─ СЫРОЙ cfg.AutoReloadMaxNCtx в решателе перезагрузки:
         nctx_reload.go:676  → отказ  (nCtxLimitReason: required > env)
         nctx_reload.go:686  → clamp  newNCtx = env
         nctx_reload.go:811  → clamp  newNCtx = env
```

**Вывод (это и есть ответ на вопрос оператора):** потолок применяется **в двух
разных слоях**. В обработчике (R255) он поднят до заявленного в профиле, а в
решателе о перезагрузке (`nctx_reload.go`) остался сырой env. Если рост окна
инициируется путём «prompt_too_long» (а это типовой путь для агентского запроса,
не поместившегося в текущее окно), срабатывает `nctx_reload.go:676/686` и
значение из WebUI молча урезается до env. Именно поэтому «модель поддерживает
больше, а фактически ограничена».

Плюс два независимых ограничителя, о которых UI молчит:
- `physicalCeiling` (GGUF/VRAM, `preflight_nctx.go:510`);
- слоты: `perClientToTotal` (`preflight_nctx.go:743`) — окно на клиента × число
  слотов; при `n_parallel=2` запрошенные 256000 требуют 512000 суммарно.

### C2. Матрица «env ↔ UI» (по областям)

| Область | Есть UI | Есть API | Кто побеждает | Видно пользователю |
|---|---|---|---|---|
| Окно контекста (профиль) | да | да | **профиль**, но env-потолок в решателе перезагрузки режет | ❌ (не видно ни причины, ни чисел) |
| Раскладка RPC (доноры/роли) | да | да | решение оператора (`backend-roles.json`), env — бутстрап | частично (`rpcLayout`/`rpcNode`) |
| Таймауты стрима | ❌ | ❌ | env | ❌ |
| Admission/очередь | ❌ | ❌ (только статистика) | env | частично |
| Загрузка моделей (дефолты) | да | да | UI → override | ❌ |
| «Не использовать бэкенд» | да | да | UI (`backend-disabled.json`) | да |
| Язык | да | да | UI | да |
| Канал между узлами | да | ❌ (нет сохранения на сервере) | **localStorage** | частично (подсказка) |

Итого: **UI-контрол есть примерно у 6 групп настроек, env-only — у большинства
остальных** (таймауты, очередь, n_ctx-поведение, раскладка-бутстрап, image,
i18n-тюнинг). Точное число «env без UI» — см. приложение: подавляющее
большинство из 209 не имеет ни контрола, ни API.

### C3. Какое поведение правильное (целевое)

1. Явное значение из UI **побеждает** env; env — дефолт и фолбэк.
2. Любое ограничение **видно**: «значение 256000, применено 131072, причина:
   `LB_NCTX_RELOAD_MAX_N_CTX=131072`» — и в UI, и в ответе API, и одной строкой в логе.
3. Ограничители не могут жить в двух слоях: решение о потолке принимается в
   ОДНОМ месте, остальные спрашивают его.

---

## D. Дублирование в UI-коде (и почему правка в одном месте не помогает)

| # | Дубль | Где | Последствие |
|---|---|---|---|
| D1 | Окно контекста пишется из трёх мест | `app.js:1256/1616` (`ctxSize`→`contextLength` конфига), `gguf-renderer-detail-render.js:2444` (профиль), `load-defaults` (`api.js:490`) | правка в карточке модели не отменяет глобальный `contextLength`; непонятно, какое значение применилось |
| D2 | Два пути сохранения настроек | `app.js` `saveSettings` (`:1420`, PUT config) + `autoSaveSettings` (`:1755`, авто-сейв при каждом change/input) + `config-io.js` (импорт) | гонка «авто-сейв перезаписал ручную правку»; импорт молча затирается следующим change |
| D3 | Один смысл в двух хранилищах | канал `#bmLinkMbps` → `localStorage` (`ollamalegion_link_mbps`), при этом расчёт уходит в тело плана | значение «настроено», но на сервере его нет; другой браузер видит иное |
| D4 | Повторяющиеся обработчики на один смысл | `#saveSettings` (`app.js:517`) и авто-сейв (`:603-605`) на тех же полях | двойной PUT на один change |
| D5 | Логика настроек продублирована между `app.js` и `modules/*` | загрузка/применение профилей в `gguf-*` модулях и в `app.js` (`loadDefaultProfile`, `:1735`) | разные значения по умолчанию в разных местах |
| D6 | UI не знает про ограничители | ни один контрол не показывает effective/limited | оператор не может понять, «почему ограничено» |

Масштаб: настройки размазаны примерно по 15+ файлам `webui/js` (`app.js`,
`api.js`, `renderers.js`, `config-io.js`, `gguf-*` (≈10 модулей),
`backend-merge.js`, `image-*`). Один смысл (окно) — 4 записи, один смысл
(раскладка) — 3 записи (env-бутстрап, роль, rpc-layout override).

---

## E. Честная оценка тестов WebUI

| Тест | Что проверяет | Вердикт |
|---|---|---|
| `users-admin.test.js` | 12 стабов fetch/DOM | проверяет СВОИ моки, не связку с API |
| `auth.test.js` | 10 стабов | то же |
| `renderers-image.test.js` | 6 стабов | рендер по мокам |
| `gguf-model-params-r134.test.js` | 6 стабов | проверяет, что «поле отправлено», а не что сервер применил |
| `backend-merge-dom.test.js` | 4 стаба | поведение панели на моках (полезно, но не про сервер) |
| остальные 60+ | в основном чистые функции | полезны, но не покрывают «настройка → сервер → поведение» |

**Чего нет и что нужно:** сквозных тестов «контрол → PUT → сервер применил →
поведение изменилось». Минимум три: (1) окно из профиля доходит до параметров
загрузки (включая путь `prompt_too_long`); (2) таймаут из UI (когда появится)
реально меняет поведение; (3) ограничение показывается в ответе API.

---

## F. План рефактора (без реализации)

Архитектура: **единый серверный реестр настроек** как источник истины.

```
SettingsRegistry (новый пакет, напр. internal/settings)
  запись: Name, Type, Default, Source{env|config|override}, Scope{global|model|backend},
          HotReload bool, Validate, Description(i18n), Limits(applicable env caps)
  API:  GET  /api/v1/settings            — список: effective, default, source, limitedBy, description
        PUT  /api/v1/settings/{name}     — запись (в config/override), валидация, hot-apply если можно
  Env:  становится Default/фолбэком; НЕ переопределяет явно записанное значение
  WebUI: страница рендерит контролы ИЗ реестра (тип/дефолт/описание/лимиты),
         показывает «значение X, ограничено Y (источник: env Z)»
```

Порядок этапов:

1. **Этап 1 (мин. эффект, мин. риск): реестр для группы «окно контекста» +
   единая точка потолка.** Перенести решение «потолок окна» в ОДНУ функцию
   (сейчас два слоя: `effectiveAutoReloadCeiling` и сырой `cfg.AutoReloadMaxNCtx`
   в `nctx_reload.go:676/686/811`), добавить в ответ профиля поля
   `effectiveContextLength`/`limitedBy`/`limitReason`, и показать их в карточке
   модели. Это закрывает жалобу (1) без переворота всего UI.
2. **Этап 2: экспозиция «горячих» env.** Для переменных, которые читаются на
   каждый запрос (`LB_STREAM_MAX_LINE_BYTES`, `LB_OPENAI_AUTO_STREAM_*`,
   `LB_TOOL_CALL_BUFFER_BYTES`, `LB_ADMISSION_WAIT_SEC`, `LB_STREAMING_*`) —
   записи реестра + API + hot-apply, env как дефолт. Это закрывает жалобу (2)
   без рестарта контейнера.
3. **Этап 3: страница «Настройки из реестра»** и постепенный вывод ручных
   контролов (`app.js` ~55 полей) на реестр; `saveSettings`/`autoSaveSettings`/
   `config-io` сводятся к одному клиенту реестра (лечит D2/D4).
4. **Этап 4: сквозные тесты** «контрол → API → поведение» (§E) + тест-сторож на
   «ограничение показано».

Что уже есть и переиспользуется: `runtime-overrides` (persist в томе),
`config-export/import`, `i18n` (описания), `Handlers`-паттерн. Объём: этап 1 —
~1–2 дня (один резолвер + поля API + UI-показ + тесты); этап 2 — по ~1 час на
переменную, начать с 5 «горячих»; этап 3 — самый объёмный (неделя+), делать
инкрементально по страницам.

Риски: (а) двойная запись при переходе (реестр + старые пути) — нужен период,
когда старое пишется так же; (б) `config.json` в bundled read-only → запись
только в override (механизм уже есть); (в) hot-apply для части переменных
невозможен (влияет на транспорт/размеры буферов) — реестр обязан честно
помечать «нужен рестарт» и показывать это в UI.

---

## Приложение. Полный список env-переменных

Собран машинно из `internal/`, `cmd/`, `pkg/` (без тестов), имя + места чтения.

| # | Переменная | Где читается |
|---|---|---|
| 1 | `AGENT_EMBEDDED` | internal\agent\embedded.go:169 |
| 2 | `AGENT_ID` | internal\agent\embedded.go:115 |
| 3 | `AGENT_IMAGE_PORT` | internal\agent\collector_network.go:86 |
| 4 | `AGENT_PUBLIC_HOST` | internal\agent\embedded.go:116 |
| 5 | `API_TOKEN` | internal\agent\embedded.go:127 |
| 6 | `APP_VERSION` | pkg\version\version.go:45 |
| 7 | `AUTH_TOKENS` | internal\config\config.go:640 |
| 8 | `BALANCER_API_TOKEN` | internal\agent\embedded.go:129<br>cmd\sdworker\balancer_register.go:96 |
| 9 | `BALANCER_TOKEN` | internal\agent\embedded.go:119<br>cmd\sdworker\main.go:121 |
| 10 | `BALANCER_URL` | internal\agent\embedded.go:118<br>cmd\cppworker\handlers_config.go:35 |
| 11 | `BUILD_DATE` | pkg\version\version.go:47 |
| 12 | `CPPWORKER_ADVERTISE_HOST` | cmd\cppworker\balancer_register.go:94 |
| 13 | `CPPWORKER_ADVERTISE_PORT` | cmd\cppworker\balancer_register.go:104 |
| 14 | `CPPWORKER_API_TOKEN` | internal\agent\embedded.go:128<br>internal\api\handlers_cppworker_profiles.go:833<br>internal\balancer\nctx_reload_handlers.go:1523<br>internal\balancer\nctx_reload_handlers.go:1621 |
| 15 | `CPPWORKER_AUTO_GPU_DIST` | internal\cppbackend\config.go:442 |
| 16 | `CPPWORKER_AUTO_KV_CACHE` | cmd\cppworker\adaptive_loader.go:365<br>cmd\cppworker\main.go:244 |
| 17 | `CPPWORKER_AUTO_OFFLOAD` | cmd\cppworker\main.go:229 |
| 18 | `CPPWORKER_AUTO_TUNE_NCTX` | cmd\cppworker\main.go:236 |
| 19 | `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD` | cmd\cppworker\lazyload_calc.go:75 |
| 20 | `CPPWORKER_AVAILABLE_RAM_BYTES` | internal\cppbackend\ram_available.go:53<br>internal\memfit\budget.go:124<br>cmd\cppworker\vram_detect.go:226 |
| 21 | `CPPWORKER_BALANCER_TOKEN` | cmd\cppworker\balancer_register.go:89 |
| 22 | `CPPWORKER_BALANCER_URL` | cmd\cppworker\balancer_register.go:82<br>cmd\cppworker\main.go:592<br>cmd\cppworker\main.go:607 |
| 23 | `CPPWORKER_BATCH_SIZE` | internal\cppbackend\config.go:339<br>cmd\cppworker\handlers_model.go:120<br>cmd\cppworker\handlers_model.go:535 |
| 24 | `CPPWORKER_CACHE_DIR` | internal\cppbackend\config.go:328 |
| 25 | `CPPWORKER_CONFIG_DIR` | cmd\cppworker\handlers_config.go:976<br>cmd\cppworker\runtime_overrides_r119.go:39 |
| 26 | `CPPWORKER_CPU_ONLY_MAX_NCTX` | cmd\cppworker\adaptive_loader.go:816 |
| 27 | `CPPWORKER_CTX_SIZE` | internal\cppbackend\config.go:331<br>cmd\cppworker\handlers_model.go:117<br>cmd\cppworker\handlers_model.go:532 |
| 28 | `CPPWORKER_DEBUG_TOOL_PROMPT` | cmd\cppworker\prompt_fingerprint_r248.go:49 |
| 29 | `CPPWORKER_DEFAULT_CTX_SIZE` | internal\cppbackend\config.go:336 |
| 30 | `CPPWORKER_DEFAULT_ENABLE_REASONING` | internal\cppbackend\config.go:463 |
| 31 | `CPPWORKER_DEFAULT_N_PREDICT_REASONING` | cmd\cppworker\reasoning_content.go:985 |
| 32 | `CPPWORKER_DEFAULT_REASONING_BUDGET` | internal\cppbackend\config.go:468 |
| 33 | `CPPWORKER_DOWNLOADS_DIR` | internal\cppbackend\config.go:325 |
| 34 | `CPPWORKER_EMPTY_OUTPUT_RETRY` | cmd\cppworker\empty_output_retry_r83.go:31 |
| 35 | `CPPWORKER_ENABLE_BATCHED_PARALLEL` | internal\cppbackend\config.go:485<br>internal\cppbackend\config.go:499 |
| 36 | `CPPWORKER_ENABLE_METRICS` | internal\cppbackend\config.go:450 |
| 37 | `CPPWORKER_ENABLE_REASONING` | internal\cppbackend\config.go:474 |
| 38 | `CPPWORKER_FALLBACK_ESTIMATED_LAYERS` | cmd\cppworker\lazyload.go:103 |
| 39 | `CPPWORKER_FALLBACK_KV_RESERVE_MB` | cmd\cppworker\lazyload.go:112 |
| 40 | `CPPWORKER_FALLBACK_OVERHEAD_MB` | cmd\cppworker\lazyload.go:121 |
| 41 | `CPPWORKER_FALLBACK_SAFETY_FACTOR` | cmd\cppworker\lazyload.go:130 |
| 42 | `CPPWORKER_FLASH_ATTN` | internal\cppbackend\config.go:347 |
| 43 | `CPPWORKER_FLASH_ATTN_TYPE` | internal\cppbackend\config.go:345<br>cmd\cppworker\handlers_model.go:123<br>cmd\cppworker\handlers_model.go:538 |
| 44 | `CPPWORKER_FREE_VRAM_BYTES` | cmd\cppworker\vram_detect.go:123 |
| 45 | `CPPWORKER_GPU_LAYERS` | internal\cppbackend\config.go:342<br>cmd\cppworker\handlers_model.go:114<br>cmd\cppworker\handlers_model.go:529 |
| 46 | `CPPWORKER_HOST` | internal\cppbackend\config.go:316 |
| 47 | `CPPWORKER_IDLE_UNLOAD_MINUTES` | internal\cppbackend\config.go:492 |
| 48 | `CPPWORKER_KV_CACHE_TYPE` | internal\cppbackend\config.go:413 |
| 49 | `CPPWORKER_LAZY_LOAD_TIMEOUT_SEC` | cmd\cppworker\lazyload.go:75 |
| 50 | `CPPWORKER_LLAMA_BUILD` | internal\agent\collector_register.go:279<br>cmd\cppworker\balancer_register.go:195<br>cmd\cppworker\main.go:365<br>cmd\cppworker\main.go:371 |
| 51 | `CPPWORKER_LOAD_FLIGHT_WAIT_SEC` | internal\cppbackend\load_stall_r227.go:91 |
| 52 | `CPPWORKER_LOAD_STALL_SEC` | internal\cppbackend\load_stall_r227.go:33 |
| 53 | `CPPWORKER_LOAD_WAIT_SEC` | cmd\cppworker\lazyload.go:178 |
| 54 | `CPPWORKER_MAIN_GPU` | internal\cppbackend\config.go:423<br>cmd\cppworker\main.go:357 |
| 55 | `CPPWORKER_MAX_PARALLEL_PER_USER` | internal\cppbackend\config.go:506 |
| 56 | `CPPWORKER_METRICS_PORT` | internal\cppbackend\config.go:319 |
| 57 | `CPPWORKER_MODELS_DIR` | internal\cppbackend\config.go:322 |
| 58 | `CPPWORKER_N_PARALLEL` | internal\cppbackend\config.go:375 |
| 59 | `CPPWORKER_N_THREADS` | internal\cppbackend\config.go:369 |
| 60 | `CPPWORKER_NO_KV_OFFLOAD` | internal\cppbackend\config.go:410 |
| 61 | `CPPWORKER_NO_MEMORY_MAP` | internal\cppbackend\config.go:429 |
| 62 | `CPPWORKER_NUMA` | internal\cppbackend\config.go:358<br>cmd\cppworker\handlers_model.go:126<br>cmd\cppworker\handlers_model.go:541 |
| 63 | `CPPWORKER_PORT` | internal\cppbackend\config.go:313<br>cmd\cppworker\main.go:205<br>cmd\cppworker\utils.go:861 |
| 64 | `CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS` | cmd\cppworker\main.go:219 |
| 65 | `CPPWORKER_RAM_FALLBACK_GPU_LAYERS` | cmd\cppworker\utils.go:809 |
| 66 | `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` | cmd\cppworker\utils.go:817 |
| 67 | `CPPWORKER_RAM_FALLBACK_N_CTX` | cmd\cppworker\utils.go:801 |
| 68 | `CPPWORKER_READ_TIMEOUT` | cmd\cppworker\main.go:266 |
| 69 | `CPPWORKER_REASONING_ARCHS` | cmd\cppworker\reasoning_content.go:91 |
| 70 | `CPPWORKER_REASONING_BUDGET` | internal\cppbackend\config.go:478 |
| 71 | `CPPWORKER_REGISTER_DISABLE` | cmd\cppworker\balancer_register.go:77 |
| 72 | `CPPWORKER_REGISTER_GPU_MODE` | cmd\cppworker\balancer_register.go:138 |
| 73 | `CPPWORKER_REGISTER_HEARTBEAT` | cmd\cppworker\balancer_register.go:124 |
| 74 | `CPPWORKER_REGISTER_MAX_RETRIES` | cmd\cppworker\balancer_register.go:131 |
| 75 | `CPPWORKER_REGISTER_NAME` | cmd\cppworker\balancer_register.go:111<br>cmd\cppworker\main.go:639 |
| 76 | `CPPWORKER_REGISTER_RETRY_INTERVAL` | cmd\cppworker\balancer_register.go:117 |
| 77 | `CPPWORKER_RMS_NORM_EPS` | internal\cppbackend\config.go:418 |
| 78 | `CPPWORKER_ROPE_FREQ_BASE` | internal\cppbackend\config.go:382 |
| 79 | `CPPWORKER_ROPE_FREQ_SCALE` | internal\cppbackend\config.go:385 |
| 80 | `CPPWORKER_ROPE_SCALING_FACTOR` | internal\cppbackend\config.go:391 |
| 81 | `CPPWORKER_ROPE_SCALING_TYPE` | internal\cppbackend\config.go:388 |
| 82 | `CPPWORKER_RPC_BACKEND` | internal\cppbackend\config.go:426 |
| 83 | `CPPWORKER_RPC_ENABLE` | cmd\cppworker\main.go:342 |
| 84 | `CPPWORKER_RPC_EXPECT_BUILD` | internal\cppbackend\rpc_version_gate_r115.go:161 |
| 85 | `CPPWORKER_RPC_EXTERNAL_ADDR` | internal\cppbackend\config.go:529 |
| 86 | `CPPWORKER_RPC_READY_WAIT_SEC` | internal\cppbackend\rpc_ready_budget_r215.go:47 |
| 87 | `CPPWORKER_RPC_REQUIRE_VERSION` | internal\cppbackend\rpc_version_gate_r115.go:113 |
| 88 | `CPPWORKER_RPC_SERVER_BIN` | internal\cppbackend\config.go:532 |
| 89 | `CPPWORKER_RPC_SERVER_BIND` | internal\cppbackend\config.go:520 |
| 90 | `CPPWORKER_RPC_SERVER_DEBUG` | internal\cppbackend\config.go:542 |
| 91 | `CPPWORKER_RPC_SERVER_ENABLED` | internal\cppbackend\config.go:517 |
| 92 | `CPPWORKER_RPC_SERVER_PORT` | internal\cppbackend\config.go:523 |
| 93 | `CPPWORKER_RPC_SERVER_RESTART` | internal\cppbackend\config.go:545 |
| 94 | `CPPWORKER_RPC_SERVERS` | cmd\cppworker\main.go:343 |
| 95 | `CPPWORKER_RPC_SESSION_AUTO_UNLOAD` | internal\cppbackend\rpc_session_lost_r224.go:72 |
| 96 | `CPPWORKER_SPLIT_MODE` | cmd\cppworker\handlers_model.go:152<br>cmd\cppworker\handlers_model.go:563<br>cmd\cppworker\handlers_model.go:3374<br>cmd\cppworker\main.go:320 |
| 97 | `CPPWORKER_STRICT_ARGS_RETRY` | cmd\cppworker\tool_schema_validate_r247.go:329 |
| 98 | `CPPWORKER_TENSOR_SPLIT` | cmd\cppworker\main.go:285 |
| 99 | `CPPWORKER_TENSOR_SPLIT_STRATEGY` | internal\cppbackend\config.go:445 |
| 100 | `CPPWORKER_TOOLS_STREAM_CONTENT` | cmd\cppworker\tools_stream_r83.go:67 |
| 101 | `CPPWORKER_USE_MLOCK` | internal\cppbackend\config.go:364 |
| 102 | `CPPWORKER_USE_MMAP` | internal\cppbackend\config.go:361<br>cmd\cppworker\handlers_model.go:129<br>cmd\cppworker\handlers_model.go:544 |
| 103 | `CPPWORKER_VISION_MMPROJ` | internal\cppbackend\backend.go:857 |
| 104 | `CPPWORKER_VRAM_BYTES` | cmd\cppworker\vram_detect.go:39<br>cmd\cppworker\vram_detect.go:133 |
| 105 | `CPPWORKER_VRAM_OVERHEAD_MB` | internal\cppbackend\kv_cache_type.go:68 |
| 106 | `CPPWORKER_WRITE_TIMEOUT` | cmd\cppworker\main.go:258 |
| 107 | `CPPWORKER_YARN_ATTN_FACTOR` | internal\cppbackend\config.go:399 |
| 108 | `CPPWORKER_YARN_BETA_FAST` | internal\cppbackend\config.go:402 |
| 109 | `CPPWORKER_YARN_BETA_SLOW` | internal\cppbackend\config.go:405 |
| 110 | `CPPWORKER_YARN_EXT_FACTOR` | internal\cppbackend\config.go:396 |
| 111 | `GIT_COMMIT` | pkg\version\version.go:46 |
| 112 | `GPU_MODE` | internal\agent\embedded.go:122 |
| 113 | `HF_MIRROR` | internal\cppbackend\config.go:437 |
| 114 | `HF_TOKEN` | internal\cppbackend\config.go:434 |
| 115 | `LB_ADMIN_PASSWORD` | internal\api\auth_session.go:160<br>internal\api\auth_session.go:189 |
| 116 | `LB_ADMIN_USER` | internal\api\auth_session.go:159<br>internal\api\auth_session.go:188 |
| 117 | `LB_ADMISSION_KEEPALIVE_SEC` | internal\balancer\admission_queue.go:690 |
| 118 | `LB_ADMISSION_WAIT_SEC` | internal\balancer\admission_queue.go:83 |
| 119 | `LB_AGENT_OUTPUT_RESERVE_TOKENS` | internal\balancer\preflight_agent_window_r253.go:59 |
| 120 | `LB_ALGORITHM` | cmd\balancer\main.go:138 |
| 121 | `LB_ALLOW_LOAD_WAIT_CAP_SEC` | internal\balancer\llamacpp_backend_helpers.go:690 |
| 122 | `LB_ALLOW_NCTX_RELOAD_TIMEOUT` | internal\balancer\nctx_reload.go:184 |
| 123 | `LB_ALLOW_PROFILE_TIMEOUTS` | internal\balancer\proxy_timeout.go:34 |
| 124 | `LB_ALLOW_REQUEST_TIMEOUTS` | internal\balancer\adaptive_timeout.go:178 |
| 125 | `LB_API_TOKEN` | internal\api\handlers_cppworker_profiles.go:836<br>internal\balancer\nctx_reload_handlers.go:455<br>internal\balancer\nctx_reload_handlers.go:1525<br>internal\balancer\nctx_reload_handlers.go:1623<br>cmd\balancer\env_auth.go:44<br>cmd\balancer\env_auth.go:102<br>cmd\balancer\env_auth.go:128<br>cmd\cppworker\balancer_register.go:91 |
| 126 | `LB_ASYNC_WORK_SHUTDOWN_WAIT_SEC` | cmd\cppworker\async_work_tracker.go:62 |
| 127 | `LB_AUTH_TOKENS` | cmd\balancer\env_auth.go:46<br>cmd\balancer\env_auth.go:105<br>cmd\balancer\env_auth.go:130 |
| 128 | `LB_AUTO_CONTINUE_CHAT_POLICY` | internal\balancer\auto_continue.go:798 |
| 129 | `LB_AUTO_CONTINUE_MAX_TOKENS` | internal\balancer\auto_continue.go:773 |
| 130 | `LB_AUTO_CONTINUE_ON_TRUNCATION` | internal\balancer\auto_continue.go:354 |
| 131 | `LB_AUTO_CONTINUE_TIMEOUT_SEC` | internal\balancer\auto_continue.go:827 |
| 132 | `LB_AUTO_LOAD_ASYNC` | internal\balancer\auto_continue.go:738 |
| 133 | `LB_AUTO_LOAD_WAIT_SEC` | internal\balancer\autoload_wait.go:93 |
| 134 | `LB_AUTO_TUNE` | internal\config\config.go:322 |
| 135 | `LB_DATA_DIR` | cmd\balancer\main.go:270 |
| 136 | `LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC` | internal\balancer\first_byte_default_r189.go:32 |
| 137 | `LB_GHOST_BACKEND_TTL_SEC` | internal\balancer\ghost_backend_reaper.go:64 |
| 138 | `LB_MAX_REQUEST_BODY_MB` | internal\balancer\llamacpp_native_path.go:167 |
| 139 | `LB_MODEL_AFFINITY` | cmd\balancer\main.go:152<br>cmd\balancer\main.go:153 |
| 140 | `LB_NCTX_GROW_FOR_AGENTS` | internal\balancer\preflight_agent_window_r253.go:48 |
| 141 | `LB_NCTX_PREFLIGHT_ASYNC_RELOAD` | internal\balancer\nctx_reload_config_bridge.go:142 |
| 142 | `LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC` | internal\balancer\nctx_reload_config_bridge.go:147 |
| 143 | `LB_NCTX_PREFLIGHT_ENABLED` | internal\balancer\nctx_reload_config_bridge.go:121 |
| 144 | `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` | internal\balancer\nctx_reload_config_bridge.go:153 |
| 145 | `LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC` | internal\balancer\nctx_reload_config_bridge.go:163 |
| 146 | `LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER` | internal\balancer\nctx_reload_config_bridge.go:158 |
| 147 | `LB_NCTX_PREFLIGHT_WAIT_SEC` | internal\balancer\preflight_nctx.go:965 |
| 148 | `LB_NCTX_RELOAD_ASYNC` | internal\balancer\auto_continue.go:761 |
| 149 | `LB_NCTX_RELOAD_ENABLED` | internal\balancer\nctx_reload_config_bridge.go:115 |
| 150 | `LB_NCTX_RELOAD_MAX_N_CTX` | internal\balancer\nctx_reload_config_bridge.go:126 |
| 151 | `LB_NCTX_RELOAD_TIMEOUT_SEC` | internal\balancer\nctx_reload_config_bridge.go:136 |
| 152 | `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR` | internal\balancer\nctx_reload_config_bridge.go:131 |
| 153 | `LB_OLLAMA_DONE_CONTENT` | internal\balancer\llamacpp_transport.go:2295 |
| 154 | `LB_OLLAMA_NATIVE_PATH` | internal\balancer\llamacpp_native_path.go:55 |
| 155 | `LB_OPENAI_AUTO_STREAM` | internal\balancer\proxy_request_openai_auto_stream.go:40 |
| 156 | `LB_OPENAI_AUTO_STREAM_MAX_BYTES` | internal\balancer\proxy_request_openai_auto_stream_r258.go:60 |
| 157 | `LB_OPENAI_AUTO_STREAM_MAX_SEC` | internal\balancer\proxy_request_openai_auto_stream_r258.go:54 |
| 158 | `LB_PORT` | internal\balancer\anthropic_r247.go:702 |
| 159 | `LB_PREFLIGHT_STREAM_DIALOG` | internal\balancer\preflight_stream_dialog.go:68 |
| 160 | `LB_PREFLIGHT_STREAM_DIALOG_KEEPALIVE_SEC` | internal\balancer\preflight_stream_dialog.go:76 |
| 161 | `LB_REQUEST_TIMEOUT_SEC` | internal\balancer\proxy_timeout.go:75 |
| 162 | `LB_RESOURCE_LIMITS_HARD_BLOCK` | internal\balancer\slot_manager.go:227 |
| 163 | `LB_RPC_LINK_MBPS` | internal\api\handlers_backends.go:83<br>internal\balancer\link_bandwidth_r166.go:91<br>internal\balancer\llamacpp_runtime_config.go:473 |
| 164 | `LB_RPC_LINK_MEASURE_SEC` | internal\balancer\rpc_link_measure_r118.go:62 |
| 165 | `LB_RPC_LINK_MEASURED_AT` | internal\api\handlers_backends.go:77<br>internal\balancer\llamacpp_runtime_config.go:478 |
| 166 | `LB_RPC_LINK_SOURCE` | internal\api\handlers_backends.go:78<br>internal\balancer\llamacpp_runtime_config.go:469 |
| 167 | `LB_RPC_NODE_PORT` | internal\api\handlers_backends.go:448<br>internal\balancer\rpc_layout_guard.go:110 |
| 168 | `LB_SELF_BASE_URL` | internal\balancer\anthropic_r247.go:699 |
| 169 | `LB_SESSION_STICKINESS` | cmd\balancer\main.go:155<br>cmd\balancer\main.go:156 |
| 170 | `LB_SESSIONS_FILE` | internal\api\auth_session.go:493 |
| 171 | `LB_SHUTDOWN_DRAIN_SEC` | cmd\balancer\main.go:665 |
| 172 | `LB_STREAM_MAX_LINE_BYTES` | internal\balancer\stream_limit_r259.go:41 |
| 173 | `LB_STREAMING_IDLE_TIMEOUT_SEC` | internal\balancer\proxy_timeout.go:103 |
| 174 | `LB_STREAMING_NEVER_TIMEOUT` | internal\balancer\proxy_timeout.go:41 |
| 175 | `LB_TOOL_CALL_BUFFER_BYTES` | internal\balancer\qwen_tool_markers_r266.go:125 |
| 176 | `LB_USERS_FILE` | internal\api\handlers.go:232 |
| 177 | `LOG_LEVEL` | cmd\balancer\main.go:93 |
| 178 | `NODE_LABELS` | cmd\cppworker\handlers_config.go:34 |
| 179 | `NODE_NAME` | cmd\cppworker\handlers_config.go:33 |
| 180 | `OLLAMA_CONTEXT_LENGTH` | internal\agent\ollama_context.go:344 |
| 181 | `OLLAMA_KV_CACHE_TYPE` | internal\agent\ollama_context.go:171 |
| 182 | `OLLAMALEGION_HEARTBEAT_MS` | cmd\cppworker\heartbeat.go:35 |
| 183 | `OLLAMALEGION_LOG_LANG_SYNC_SEC` | internal\i18n\bootstrap.go:55 |
| 184 | `OS` | internal\agent\collector_register.go:76 |
| 185 | `RPC_WORKER_COORDINATOR_URL` | internal\rpcworker\config.go:82 |
| 186 | `RPC_WORKER_HEALTHCHECK_INTERVAL` | internal\rpcworker\config.go:91 |
| 187 | `RPC_WORKER_HEALTHCHECK_TIMEOUT` | cmd\rpcworker\main.go:264 |
| 188 | `RPC_WORKER_ID` | internal\rpcworker\config.go:80 |
| 189 | `RPC_WORKER_LAYERS` | internal\rpcworker\config.go:81 |
| 190 | `RPC_WORKER_PORT` | cmd\rpcworker\main.go:259 |
| 191 | `RPC_WORKER_READ_TIMEOUT` | internal\rpcworker\config.go:101 |
| 192 | `RPC_WORKER_RPC_HOST` | cmd\rpcworker\main.go:135 |
| 193 | `RPC_WORKER_RPC_PORT` | cmd\rpcworker\main.go:142 |
| 194 | `RPC_WORKER_STUB_MODE` | internal\rpcworker\config.go:88<br>internal\rpcworker\config.go:89 |
| 195 | `RPC_WORKER_TOKEN` | internal\rpcworker\config.go:86 |
| 196 | `RPC_WORKER_WRITE_TIMEOUT` | internal\rpcworker\config.go:96 |
| 197 | `SDWORKER_ADVERTISE_HOST` | cmd\sdworker\balancer_register.go:98 |
| 198 | `SDWORKER_ADVERTISE_PORT` | cmd\sdworker\balancer_register.go:107 |
| 199 | `SDWORKER_BACKEND_ID` | cmd\sdworker\balancer_register.go:112 |
| 200 | `SDWORKER_BALANCER_TOKEN` | cmd\sdworker\balancer_register.go:94 |
| 201 | `SDWORKER_BALANCER_URL` | cmd\sdworker\balancer_register.go:90<br>cmd\sdworker\main.go:120 |
| 202 | `SDWORKER_CONFIG` | internal\sdbackend\config.go:228<br>cmd\sdworker\main.go:200 |
| 203 | `SDWORKER_PORT` | cmd\sdworker\main.go:244<br>cmd\sdworker\main.go:272 |
| 204 | `SDWORKER_REGISTER_DISABLE` | cmd\sdworker\balancer_register.go:84 |
| 205 | `SDWORKER_REGISTER_GPU_MODE` | cmd\sdworker\balancer_register.go:134 |
| 206 | `SDWORKER_REGISTER_HEARTBEAT` | cmd\sdworker\balancer_register.go:123 |
| 207 | `SDWORKER_REGISTER_MAX_RETRIES` | cmd\sdworker\balancer_register.go:129 |
| 208 | `SDWORKER_REGISTER_RETRY_INTERVAL` | cmd\sdworker\balancer_register.go:117 |
| 209 | `VERSION` | pkg\version\version.go:45 |

Итого уникальных переменных: 209. Список собран машинно (os.Getenv/os.LookupEnv в internal/, cmd/, pkg/, без тестов). Имена, которые хелперы получают строкой-аргументом, попадают сюда, если хелпер сам вызывает Getenv.
