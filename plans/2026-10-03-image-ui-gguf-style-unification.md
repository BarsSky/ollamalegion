# Унификация image-части WebUI в стиле «GGUF модели» (Phase 9)

Статус: план согласован (2026-10-03), серверные предпосылки сделаны, UI — в работе.
Связанные документы: `docs/image-generation.md`, `plans/2026-09-27-image-generation-backend-plan.md`,
страница-эталон — «GGUF модели» (`#gguf-page`, модули `webui/js/modules/gguf-*.js`).

## 1. Задача

1. Привести отображение image-бэкендов и их моделей к **стилю страницы «GGUF модели»**:
   поиск по HuggingFace, видимость выкачанных моделей, настройки, состояние загрузки,
   понятный интерфейс.
2. Убрать из WebUI **отображение сгенерированных картинок**: WebUI — для настройки
   балансера/бэкендов и понимания состояния системы, показ картинок — задача клиентов.
3. Ничего не потерять из существующего отображения (Backends, Дашборд, Модели, Monitor).

## 2. Что есть сейчас (инвентаризация)

| Место | Что показывает | Судьба |
|---|---|---|
| «GGUF модели» (`#gguf-page` + `gguf-*.js`, 167 ключей) | Выбор бэкенда; табы **HuggingFace / Модели на диске / Загруженные / Загрузки / Настройки**; поиск репо, файлы, размер/квант, «уже скачано», прогресс/история/остатки, удаление с диска; настройки модели по табам (general/inference/perf/multi_gpu/kv_cache/rope_yarn/about), backend options | **эталон вёрстки и UX** |
| «Изображения» (`#image-page`, `image-page.js` + `image-profiles.js`, 41 ключ) | генерация, результат, галерея (localStorage), таблица image-моделей, ручная HF-форма bundle, редактор профилей | генерация/результат/галерея — **удаляются**; модели, HF и профили — переезжают в новую страницу |
| «Image-бэкенды» (`image-backends-page.js`) | CRUD бэкендов, порт, GPU-индекс, состояние, модель, VRAM, счётчики запросов, политика, load/unload | переезжает в таб **Обзор**; отдельный пункт меню убирается |
| Backends / Дашборд / Модели | image-строка, секция воркера, модели в памяти, бейдж 🎨, фильтр `image_cpp` | **остаётся как есть** |
| Monitor | строка бэкенда (Active/RPS/AvgRT из `image.requests`, Image req/OK/err) + панель «Запросы к image-бэкендам» | **остаётся как есть** |
| Метрики Phase 8 | per-backend + агрегат пула в `/api/v1/metrics` и `/api/v1/cluster` | без изменений, используется в «Обзоре» |

## 3. Серверная часть — СДЕЛАНО (коммит `7d697bd`)

| Что | Где |
|---|---|
| `suggestedRole` в ответе `GET /api/hf/files` (эвристика ролей экспортирована как `sdbackend.SuggestRole`) | `internal/sdbackend/models.go`, `internal/cppbackend/hf_downloader.go`, `cmd/sdworker/handlers_hf.go` |
| `POST /api/image/models/delete` — удаление bundle с диска (409 на загруженный, 400 на traversal, 404/400 прочее, перечитывание реестра, `freed_bytes`) | `cmd/sdworker/handlers_model.go`, `router.go` |
| Алиас балансера `POST /api/v1/image/backends/{id}/models/delete` | `internal/api/handlers_image_models.go` |
| HF-пути (`/api/hf/search|files|download|bundle|progress|downloads|cancel|cleanup`) | уже проксируются как есть через `/api/v1/image/backends/{id}/hf/...`, **query string сохраняется** (`internal/api/gguf_backend_proxy.go`) |

## 4. Целевой вид

Одна страница **«Image-модели»** вместо «Изображения» + «Image-бэкенды»:

| Таб | Содержимое |
|---|---|
| **Обзор** | Карточка выбранного image-бэкенда: порт воркера, host, GPU-индекс, состояние, текущая модель, VRAM free/total, движок + `pinnedRevision`, политика сосуществования, счётчики запросов; CRUD бэкендов (добавить/изменить/удалить); кнопка **«Проверка бэкенда»** (1 шаг 64×64, показывается только OK/время, **без картинки**) |
| **HuggingFace** | Поиск репозиториев (query, опционально `task=text-to-image`), карточка репо, файлы с размером и **предложенной ролью** (`suggestedRole`), выбор файлов, имя/семейство bundle, «Скачать bundle» |
| **Модели на диске** | Таблица bundle'ов: имя, семейство, размер, состав файлов по ролям, состояние, оценка VRAM, применённый профиль; действия: Загрузить / Выгрузить / Изменить профиль / **Удалить с диска** |
| **Загруженные** | Текущая модель воркера, SSE-прогресс загрузки (`/api/image/models/load/progress/stream`), busy-бейдж, выгрузка, preload |
| **Загрузки** | Активные загрузки, история, «остатки» (orphans) + очистка (`/api/hf/downloads`, `/api/hf/cleanup`) |
| **Настройки** | Редактор профиля bundle (`image-profiles.js`, табы-группы: Defaults / Runtime / VRAM) + параметры бэкенда (imagePort, GPU-индекс, вес, maxConcurrent, idleUnload) |

Убирается: форма генерации, «Результат», «Скачать все», галерея и её localStorage,
ключи `image.prompt|negative|width|height|steps|cfg|sampler|scheduler|seed|batch|generate|images_returned|download_one|gallery_*`,
действие «К генерации» на странице бэкендов.

## 5. Контракт между модулями (ФАКТ, после шага 4)

```
webui/js/modules/image-models-page.js   (шелл, владелец: shell)
  window.ImageModelsPage = {
    mount(),                     // идемпотентно: навигация, табы, обработчики
    render(backends),            // перерисовать шелл и активный таб
    syncVisibility(backends),    // показ/скрытие пункта меню и страницы
    showTab(tabId),
    runSelfTest()
  };

webui/js/modules/image-models-hf.js     (владелец табов hf + downloads)
  window.ImageModelsHf = { mount(), render(ctx), tabIds: ['hf','downloads'], _actions, _state, pure };

webui/js/modules/image-page.js          (владелец табов models + loaded + settings)
  window.ImagePage = { init, refresh, pure, _state, _actions };
  // Табы «Модели на диске» (состав ролей + удаление с диска), «Загруженные»
  // (состояние + polling прогресса), «Настройки» (вход в редактор параметров
  // бэкенда + профили через ImageProfiles.mount).

ctx = { apiBase, headers, backendId, backend, backends, tab }
```

**Отступление от первоначального плана (обосновано).** Планировался второй
таб-модуль `image-models-list.js` для табов models/loaded/settings. По факту эти
табы уже полностью реализованы рабочим кодом `image-page.js` (таблица моделей,
load/unload, polling прогресса, монтирование профилей) и покрыты тестами; вынос
их в новый модуль означал бы переписать работающее ради красоты контракта — ровно
тот риск «потерять функционал», от которого страхует этот план. Поэтому:

- HF-часть (поиск, файлы, bundle, прогресс bundle) **вынесена** в
  `image-models-hf.js` — её в `image-page.js` не было (была ручная форма «строка =
  repo+filename+роль»), и она переписана в стиле GGUF;
- models/loaded/settings **остались** в `image-page.js`, но в них добавлено то,
  чего не хватало для паритета с GGUF: состав bundle по ролям, «Удалить с диска»
  (ручка `POST /api/image/models/delete`), вход в параметры бэкенда из «Настроек»
  (форма одна — в `image-backends-page.js`, чтобы не держать две копии валидации).

Правила: модули не трогают файлы друг друга; i18n добавляет ТОЛЬКО shell-владелец
(ключи `imageModels.*`) плюс переиспользуются `gguf.*`/`image.*`; табы получают
данные сами через пути балансера (`/api/v1/image/backends/{id}/...`), шелл
передаёт только контекст.

## 6. Ключи i18n

- Общие действия/статусы — **переиспользуются существующие `gguf.*`**
  (`gguf.download`, `gguf.cancel_download`, `gguf.delete_from_disk`, `gguf.load_model`,
  `gguf.unload_model`, `gguf.models_on_disk`, `gguf.download_history`, `gguf.total_size`,
  `gguf.model_size`, `gguf.search_btn`, `gguf.search_placeholder`, `gguf.no_results`, …).
- Редактор профилей — существующие `image.profiles.*`.
- Новые ключи (только специфика image): `nav.image_models`, `imageModels.tab_*`,
  `imageModels.backend_select|worker_port|gpu_index|policy|requests_summary`,
  `imageModels.selftest_*`, `imageModels.role` + `imageModels.role_<role>`,
  `imageModels.family|vram_estimate|bundle_*|profile_applied`, `imageModels.search_task_filter`,
  `imageModels.use_suggested_roles`, `imageModels.confirm_delete_bundle`, `imageModels.deleted_freed`,
  `imageModels.hf_*` (поиск/файлы/отметки), `imageModels.dl_files_active|orphans_title|no_downloads*`,
  `imageModels.roles_summary|roles_unknown|backend_params_*|open_backend_editor|status_interrupted`.
  Факт: паритет en/ru = 1647/1647 (`scripts/i18n_diff.js --strict`).

## 7. Шаги

1. **Шелл** (nav, контейнер, табы, выбор бэкенда, видимость, i18n-ключи). Проверка: `check_webui_assets`, node-тест шелла.
   ✅ Сделано: `webui/js/modules/image-models-page.js` (+ тест на 9 проверок), табы в разметке
   `index.html` (`.gguf-tabs`/`.gguf-tab-content` — тот же стиль, что у GGUF), ключи i18n
   (паритет 1647/1647), видимость страницы по-прежнему за `image-backends-page.js`.
2. **Обзор**: перенос CRUD/политики/счётчиков из `image-backends-page.js` + «Проверка бэкенда» (64×64, 1 шаг, без картинки). Паритет с прежней страницей — до удаления старой.
   ✅ Сделано: карточка бэкендов переехала в таб «Обзор» (id сохранены, модуль и его 36 проверок
   зелёные). «Проверка бэкенда» идёт КЛИЕНТСКИМ путём `POST /v1/images/generations`
   (`size=64x64, n=1, steps=1`): он проходит VRAM-гейт и считается метриками, поэтому проверка
   видна в Monitor; если модель не загружена, проверка сначала грузит её (гейт не пускает
   генерацию без модели) и повторяет запрос на транзиентный отказ гейта, пока кэш балансера не
   увидит загрузку. В UI попадают только OK, время и имя модели; base64 из ответа в DOM НЕ
   попадает — это проверяет тест.
   ✅ Удалены из разметки генерация, «Результат» и «Галерея»; пункт меню «Image-бэкенды» убран,
   страница «Изображения» переименована в «Image-модели».
   ✅ `image-page.js`: код генерации/галереи/формы bundle вычищен (802 → 900 строк с новыми
   возможностями), оба его теста обновлены (15 + 23 проверки).
3. **Модели на диске + Загруженные**: таблица bundle'ов с ролями/профилем, load/unload/reload, SSE-прогресс, удаление с диска (ручка готова).
   ✅ Сделано: колонка «Состав (роли)» (роли с сервера, `rolesSummary`), кнопка «Удалить с диска»
   (`POST /api/image/models/delete`, confirm с именем bundle, тост с `freed_bytes`, отказ 409
   показывается текстом сервера), прогресс загрузки модели — **SSE**
   (`/api/image/models/load/progress/stream` через прокси балансера, `?token=` в URL, потому что
   EventSource не умеет заголовки) с откатом на polling, если поток закрылся или EventSource
   недоступен; тест проверяет и транспорт, и откат.
4. **HuggingFace + Загрузки**: поиск → файлы с ролями → bundle download; активные/история/орфаны.
   ✅ Сделано: `webui/js/modules/image-models-hf.js` (+ тест на 26 проверок): поиск репозиториев
   (query + фильтр `text-to-image`), файлы с `suggestedRole` и выбором роли, автоотметки
   (diffusion/vae/clip_l/clip_g — да; t5xxl/llm — только вручную), сборка `POST /hf/bundle` с
   `sizeBytes`, прогресс по агрегату `GET /hf/progress?bundleId=`, таб «Загрузки» (активные bundle
   и файлы, история, остатки + `DELETE /hf/cleanup`, отмена через `POST /hf/cancel`).
5. **Настройки**: монтирование `image-profiles.js` в таб + параметры бэкенда.
   ✅ Сделано: профили монтируются как раньше; параметры бэкенда — карточка-сводка (имя, хост,
   порт воркера) + кнопка «Открыть параметры», которая открывает редактор
   `image-backends-page.js` (`_actions.openEditor`): одна форма на проект, без второй валидации.
6. **Удаление** генерации/галереи, чистка i18n и ссылок («К генерации» → «К моделям»).
   ✅ Ссылка переименована в коде (`openImages` ведёт на таб «Модели на диске»).
   ✅ Чистка i18n: удалены 48 мёртвых ключей (`image.prompt|width|height|steps|cfg|sampler|
   scheduler|seed|batch|generate|images_returned|gallery_*|bundle_<старая форма>` и др.) в обоих
   языках одновременно; паритет 1607/1607.
7. **Проверки**: node-тесты модулей, `ui-renderer-smoke`, `check_iife_exports`, `check_webui_assets`,
   `i18n_diff --strict`, E2E-проверка структуры UI (табы есть, формы генерации нет) + живой прогон
   в Docker-стенде (load → генерация клиентом).
   ✅ Node-тесты: 15 файлов + `webui/tests/*` + `tests/webui/*` — все зелёные; `check_iife_exports`
   67 файлов PASS; `check_webui_assets` 95 ассетов OK; i18n 1607/1607; полный Go-набор зелёный.
   ✅ `scripts/docker-stack-smoke.ps1`: S8 проверяет структуру (6 табов, оба модуля табов,
   отсутствие формы генерации) — 9 PASS / 0 FAIL / 2 SKIP в живом Docker.
   ✅ Живой прогон (Chromium/CDP против контейнера): 6 табов, `suggestedRole`, SSE-прогресс
   загрузки (`loading/spawn` → `loaded/ready (1s)`), «Проверка бэкенда» `OK — 11.3 с · sd15-q4`
   с ростом метрик (`image.requests total=1 ok=1`) и без изображения в DOM; регрессий на
   Backends/Дашборде/Моделях/Monitor нет (0 ошибок консоли).
8. **Документация**: `docs/image-generation.md` (+en) — новый UI; CHANGELOG.
   ✅ §12.3 переписан под шесть табов; CHANGELOG 0.7.1 (+ раздел о дефектах, найденных живым E2E).

## 8. Риски и правила

- **Не терять функционал**: старая страница живёт до паритета; каждый шаг — с тестами.
- **Роли файлов**: только серверная эвристика (`suggestedRole`), в JS не дублировать.
- **Удаление bundle**: только внутри `ModelsDir`, загруженный — нельзя (409).
- **Порты и Docker**: локальный стенд — только на сдвинутых портах (19079/19082/19084/19093)
  и погашен перед docker-операциями; после пересоздания контейнеров при слёте публикации
  портов — `docker restart ol-stack-balancer` (зафиксировано в CHANGELOG 0.7.0).
