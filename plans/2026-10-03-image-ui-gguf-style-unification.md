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

## 5. Замороженный контракт между модулями

```
webui/js/modules/image-models-page.js   (шелл, владелец: shell)
  window.ImageModelsPage = {
    mount(),                     // идемпотентно: навигация, табы, обработчики
    render(backends, ctx),       // перерисовать шелл и активный таб
    syncVisibility(backends),    // показ/скрытие пункта меню и страницы
    showTab(tabId)
  };

webui/js/modules/image-models-hf.js     (владелец: агент A)
  window.ImageModelsHf = { mount(root), render(ctx), tabIds: ['hf','downloads'] };

webui/js/modules/image-models-list.js   (владелец: агент B)
  window.ImageModelsList = { mount(root), render(ctx), tabIds: ['models','loaded','settings'] };

ctx = { apiBase, headers, backendId, backend, models, capabilities, hfToken }
```

Правила: модули не трогают файлы друг друга; i18n добавляет ТОЛЬКО shell-владелец;
табы получают данные сами через пути балансера (`/api/v1/image/backends/{id}/...`),
шелл передаёт только контекст.

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
  `imageModels.use_suggested_roles`, `imageModels.confirm_delete_bundle`, `imageModels.deleted_freed`.

## 7. Шаги

1. **Шелл** (nav, контейнер, табы, выбор бэкенда, видимость, i18n-ключи). Проверка: `check_webui_assets`, node-тест шелла.
   ✅ Сделано: `webui/js/modules/image-models-page.js` (+ тест на 9 проверок), табы в разметке
   `index.html` (`.gguf-tabs`/`.gguf-tab-content` — тот же стиль, что у GGUF), ключи i18n
   (паритет 1622/1622), видимость страницы по-прежнему за `image-backends-page.js`.
2. **Обзор**: перенос CRUD/политики/счётчиков из `image-backends-page.js` + «Проверка бэкенда» (64×64, 1 шаг, без картинки). Паритет с прежней страницей — до удаления старой.
   ✅ Сделано: карточка бэкендов переехала в таб «Обзор» (id сохранены, модуль и его 36 проверок
   зелёные), «Проверка бэкенда» реализована в шелле через `POST /generate` (`sync: true`) —
   тест проверяет, что base64 из ответа НЕ попадает в DOM.
   ✅ Удалены из разметки генерация, «Результат» и «Галерея»; пункт меню «Image-бэкенды» убран,
   страница «Изображения» переименована в «Image-модели».
   ⏳ `image-page.js`: вычистка кода генерации/галереи (в работе) + обновление его DOM-теста.
3. **Модели на диске + Загруженные**: таблица bundle'ов с ролями/профилем, load/unload/reload, SSE-прогресс, удаление с диска (ручка готова).
4. **HuggingFace + Загрузки**: поиск → файлы с ролями → bundle download; активные/история/орфаны.
5. **Настройки**: монтирование `image-profiles.js` в таб + параметры бэкенда.
6. **Удаление** генерации/галереи, чистка i18n и ссылок («К генерации» → «К моделям»).
   ✅ Ссылка уже переименована в коде (`openImages` ведёт на таб «Модели на диске»);
   чистка неиспользуемых i18n-ключей — после вычистки `image-page.js`.
7. **Проверки**: node-тесты модулей, `ui-renderer-smoke`, `check_iife_exports`, `check_webui_assets`,
   `i18n_diff --strict`, E2E-проверка структуры UI (табы есть, формы генерации нет) + живой прогон
   в Docker-стенде (загрузка bundle с HF → load → генерация сторонним клиентом).
8. **Документация**: `docs/image-generation.md` (+en) — новый UI; CHANGELOG.

## 8. Риски и правила

- **Не терять функционал**: старая страница живёт до паритета; каждый шаг — с тестами.
- **Роли файлов**: только серверная эвристика (`suggestedRole`), в JS не дублировать.
- **Удаление bundle**: только внутри `ModelsDir`, загруженный — нельзя (409).
- **Порты и Docker**: локальный стенд — только на сдвинутых портах (19079/19082/19084/19093)
  и погашен перед docker-операциями; после пересоздания контейнеров при слёте публикации
  портов — `docker restart ol-stack-balancer` (зафиксировано в CHANGELOG 0.7.0).
