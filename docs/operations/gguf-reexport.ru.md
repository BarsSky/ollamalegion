# Ре-экспорт GGUF: опциональный модуль (R709, долги D10 закрыты)

**Состав:** `services/gguf-reexport/` (Go-хост) + `py/reexport_job.py` (python-драйвер),
образ `ollama-legion/gguf-reexport`, профиль compose `reexport`. **Статус:** путь
метаданных/переупаковки проверен на живом 16.6 ГиБ файле (§6); полный ре-экспорт из
HF-чекпойнта — таргет образа `full` (§5), не собран.

## 1. Задача и почему отдельный сервис
`Inkling-2-Mini` объявляет `general.architecture=inkling2` и `block_count=48`, а физических
блоков 24 — это loop-экспорт; загрузчик llama.cpp ждёт ОТДЕЛЬНУЮ архитектуру `inkling2_loop`
(физические слои, `loop.*`-ключи, контроллеры `blk.N.loop_*_ctrl_*`). `normalize`
переупаковывает метаданные и раскладывает уже лежащие матрицы запросов (веса не
пересчитываются), `convert` прогоняет HF-чекпойнт через `convert_hf_to_gguf.py`. Отдельный
сервис, а не ручка в воркере: torch (+2.5 ГБ), 16.6 ГиБ чтения и записи на одном диске (§6), однослотовый воркер занял бы слот инференса. Go держит состояние, HTTP и отмену, python — работу.

## 2. Контракт API
```
GET    /health                      → 200 {"status":"ok"} (HEALTHCHECK)
GET    /api/version                 → запечённая идентичность образа (R486)
GET    /api/reexport/capabilities   → что умеет ПРЯМО СЕЙЧАС
GET    /api/reexport/models         → GGUF тома моделей + вердикт схемы
POST   /api/reexport/jobs           → {model_dir, target, options} → 202 задача
GET    /api/reexport/jobs           → список задач (свежие первыми)
GET    /api/reexport/jobs/{id}      → статус/прогресс/ошибка/хвост лога
DELETE /api/reexport/jobs/{id}      → отмена: процесс убит, недописанный файл удалён
```
`capabilities` — источник истины для WebUI (версия конвертера, `python`, `torch_state`,
`targets` с `available`/`reason`): возможность объявляется ПРОБОЙ, а не константой — без torch
`hf_to_gguf` честно `available=false` с причиной. Ошибки — `{"error":{"code","message",
"detail"}}`: 400 (`path_not_allowed`), 404, 409, 422, 507, 429 (что делать — §7). Задачи —
файлы в томе `reexport_jobs` (`/app/jobs/<id>.json`, атомарная запись tmp+rename): БД нет —
задач десятки, состояние переживает пересоздание контейнера; задача, бывшая `running`, после
рестарта становится `interrupted` с объяснением. Параллелизм 1 (диск один), остальные `queued`.

## 3. Куда пишется результат, сколько нужно места, отмена
**Выход всегда внутри `${MODELS_DIR}`.** Каталог результата выбирает каталог модели:
`out_path` — имя/подкаталог ВНУТРИ него (`filepath.Rel`, не «префикс строки»), пусто — рядом с
моделью; абсолютный путь — только внутри тома. `REEXPORT_OUT_DIR` остался legacy-fallback'ом и
никогда не даёт выйти за `${MODELS_DIR}`: за томом сервис предупреждает на старте, отдаёт
`out_dir_ignored=true` и отвечает `path_not_allowed` (400) — результат мимо тома воркер не
увидит. **Место проверяется ДО старта** (Go, `Statfs`/`GetDiskFreeSpaceEx`) с числами:
`normalize` — вход + 1 ГиБ, `convert` — вход + выход (≈ вход) + 1 ГиБ; не хватает —
`failed`/`insufficient_space` «нужно X ГиБ, свободно Y ГиБ», исполнитель не запускается (нет
`started_at`); python дублирует проверку перед записью. **Прогресс** — `progress.stage` =
`reading → writing → validating`: байты на `normalize`, размер результата на `convert`.
**Отмена** убивает процесс и удаляет недописанный результат: огрызок выглядит для воркера моделью и падает на загрузке.

## 4. Как WebUI узнаёт о модуле и что вызывает
Страница моделей создаёт пустой `#ggufReexportHost`, а `js/modules/gguf-reexport.js` делает
`GET /api/reexport/capabilities` через nginx панели: 200 → блок рисуется, 424/502/таймаут →
**вкладки нет вообще** (флаг в `.env` был бы вторым источником правды). nginx: upstream —
**переменная** (`set $reexport_upstream ...`): статический `proxy_pass` на
`gguf-reexport:18095` разрешается на старте и без контейнера модуля роняет ВСЮ панель (R229);
дефолты — `docker/webui/entrypoint.sh`, связка закреплена тестом
`webui/tests/reexport-nginx-proxy-r709.test.js`.

Сценарий оператора (D11): пять шагов; ни один не изобретает своего эндпоинта.

| Шаг UI | Кнопка | Что вызывается на самом деле |
|---|---|---|
| 1. Модель и вердикт схемы | (клик по строке) | `GET /api/reexport/models` — шапки читает Go-парсер, без конвертера |
| 2. Проверить | «Проверить» | `POST /api/reexport/jobs` с `options.dry_run=true` (цель `inkling2_loop`), затем опрос `GET /api/reexport/jobs/{id}` |
| 3. Нормализовать | «Нормализовать (loop-схема)» | тот же `POST` без `dry_run`; стадии — `progress.stage` из `GET /jobs/{id}`; «Отменить» — `DELETE /api/reexport/jobs/{id}` |
| 4. Пересканировать модели | «Пересканировать модели» | `GgufModule.rescanAllBackendModels()` → `POST /api/models/refresh` на воркерах, плюс `GET /api/reexport/models?refresh=1` |
| 5. Проверить загрузку | «Проверить загрузку» | `POST /api/v1/backends/{id}/models` `{operation:"load"}` (балансер), статус — `GET /api/v1/gguf/backends/{id}/proxy/api/models/load/progress`, факты — `/api/models` воркера (`offload_degraded`, `offload_note`) |

**«Проверить» — это `dry_run`, а не поле `mode`:** `createRequest` разбирается с `DisallowUnknownFields`, лишнее поле дало бы 400; проверка — та же цель, драйвер читает шапку и печатает план.
**«Уже нормализовано»** — сразу, без задачи: сервис отказал бы на сверке визитов.
**Размер:** лимиты узла — из его `/api/models`: больше VRAM → `offload_degraded`, больше RAM → «загрузка закончится отказом».

**Два файла.** Блок — `js/modules/gguf-reexport.js` (точка входа: проба `capabilities`,
разметка, модель и вердикт, шаги и кнопки, проверка загрузки) и
`js/modules/gguf-reexport-jobs.js` (фабрика `window.GgufReexportJobs`: транспорт задач
`POST/GET/DELETE /api/reexport/jobs*`, ожидание терминального статуса, поллинг и таблица
истории, подписи статусов/стадий, разбор отказа и итоги шагов проверки и нормализации).
Порядок тегов в `webui/index.html` обязателен: слой задач → главный модуль; слой берётся
лениво, и если его нет в сборке, шаги проверки/нормализации честно говорят
`reexport.jobs_module_missing`, а остальные работают. Причина разделения — страж R628
(боевой файл от 1500 непустых строк запрещён); контракт фабрики сверяет
`scripts/webui_ctx_contract_check.js`.


## 5. Воркеры, профиль, версии, таргеты образа
Ре-экспорт кладёт НОВЫЙ файл в том моделей; воркер держит инвентарь с TTL, поэтому после
успеха WebUI показывает «Пересканировать модели» → `POST /api/models/refresh` (R93) плюс сброс
кэша фактов (`?refresh=1`); балансер о модуле не знает. Compose: `profiles: ["reexport"]`, том
`${MODELS_DIR}:/app/models:rw`, том `reexport_jobs`, healthcheck своим бинарём, порт только на
`127.0.0.1`; без профиля ничего не создаётся. Переменные (`.env.example`):
`GGUF_REEXPORT_TAG`, `GGUF_REEXPORT_BUILD_TARGET` (light|full), `GGUF_REEXPORT_WITH_TORCH`,
`GGUF_REEXPORT_PORT_HOST`, `GGUF_REEXPORT_OUT_DIR` (legacy, §3), `GGUF_REEXPORT_SCAN_DEPTH`,
`GGUF_REEXPORT_LOG_LEVEL`. Версии: `scripts/release-all.ps1` знает `gguf-reexport`, собирает с
теми же `-ldflags`/OCI-метками (+`LLAMA_BUILD` от `scripts/build-identity.ps1`) и пополняет
`release-manifest.json`; компонент `Optional`, строку тега в локальном `.env` скрипт дописывает
сам, `scripts/check-image-tags.ps1` сверяет тег и `llamaCommit`. **light** (по умолчанию) —
python3 + stdlib + `scripts/gguf_loop_meta_patch.py`; **full** (`--target full --build-arg
WITH_TORCH=1`) — `convert_hf_to_gguf.py`, `conversion/`, `gguf-py/` и зависимости из
`c/llama.cpp/requirements/` (+2.5 ГБ). Без torch полного пути нет: конвертер импортирует его безусловно.

## 6. Живой прогон (2026-11-23, CPU, без GPU)
`Inkling-2-Mini-UD-Q4_K_M.gguf`: 16 610 543 328 Б (15.47 ГиБ), 58 KV, 734 тензора, данные с
байта 11 978 464; результат писался на второй диск через `REEXPORT_OUT_DIR` (так было ДО D10.2, теперь только внутри тома, §3).

| Проверка | Результат |
|---|---|
| `GET /api/reexport/models` (10 файлов, холодный кэш) | 3.5 с; `scheme=inkling2_loop_export`, `block_count=48`, `physical_blocks=24`, `patchable=true`, обе матрицы запросов на месте |
| dry-run (только план) | `succeeded` за **576 мс**: 7 изменений метаданных, 144 переименования контроллеров, 12 будущих `loop_msg_query` |
| **ре-экспорт** (fuse включён) | `succeeded` за **22 мин 57 с** (1 377 344 мс); 15.46 ГиБ (~11.5 МиБ/с — цена bind-mount Windows, dry-run читает только шапку за 0.6 с) |
| размер и содержимое | 16 610 544 736 Б (+1 408 Б); `inkling2`→`inkling2_loop`, `block_count` 48→24, `head_count`/`head_count_kv`/`sliding_window_pattern` усечены до 24; добавлены `loop.start=6`, `loop.end=18`, `loop.count=3`, `loop.gate_rank=64`, `loop.varying=false` (+ `num_loops`, `loop.core_start/end`); 144 контроллера переименованы; 2 матрицы запросов разложены в 12 `blk.6..blk.17.loop_msg_query`; итог 62 KV / 744 тензора, данные с байта 11 979 872 |
| негатив (Qwen3-4B как цель) | `failed`, код `model_not_patchable`: «ожидался general.architecture=inkling2, получено 'qwen3'» |
| очередь | три задачи шли строго по одной: вторая и третья висели `queued`, пока первая писала 15 ГиБ |
| загрузка результата (CPU `cppworker:gpu-r261120-r708`) | `llama_model_loader` принял 62 KV и 744 тензора, `block_count=24`, `create_tensor` пройден для всех `blk.*`, буферы выделены — тот барьер, на котором файл падал (`missing blk.24` / `blk.6.loop_msg_query.weight`), снят |
| загрузка результата из локального linux-тома | `state=loaded` за **50 341 мс** (ctx 8192, 6/49 слоёв на GPU), генерация 24 токена за 119.6 с = **0.2 ток/с**; при ctx 32768 — `loaded` за 32 119 мс, пик cgroup 19.66 ГБ (`docs/features/inkling2-loop.md`, D10.6) |

## 7. Что делать при отказе
| Код | Что случилось | Что делать |
|---|---|---|
| `insufficient_space` | на томе меньше, чем «вход (+ выход) + 1 ГиБ» | освободить том моделей или взять вход меньше; нужное и свободное — в сообщении |
| `path_not_allowed` | путь результата вне каталога модели или тома | убрать `..` и абсолютные пути: `out_path` — имя или подкаталог каталога модели |
| `model_not_found`, `out_of_models_dir` | источника нет в томе моделей | выбрать модель из `GET /api/reexport/models` |
| `model_not_patchable` | чужой arch или нет loop-контроллеров | нужен полный ре-экспорт из HF-чекпойнта (цель `convert`, образ `full`) |
| `dst_exists` | результат уже есть | включить `force` или сменить имя |
| `target_unavailable` | цель закрыта в этой сборке образа | смотреть `targets[].reason` в `capabilities`; для `convert` нужен образ `full` |
| `converter_failed`, `verify_failed` | упал конвертер или не сошлась схема | причина — в хвосте лога задачи (`GET …/{id}` → `log_tail`) |
| `canceled`, `interrupted` | отмена оператором или рестарт контейнера | перезапустить задачу; при отмене недописанный файл удаляется |
| `queue_full` | в очереди больше 64 задач | дождаться завершения текущих: параллелизм 1 |

## 8. Что осталось
1. **Скорость чтения решает:** на bind-mount Windows чтение шло 11–30 МиБ/с, и загрузка 16.6 ГиБ «не доснималась» в окне теста; на локальном linux-томе — 414 МиБ/с и `loaded` за 50 с. Держите модели на локальном томе.
2. **Образ `full` не собран и не проверен живьём:** +2.5 ГБ и нет HF-чекпойнта на стенде; Dockerfile таргета `full` разобран (`docker buildx build --call=outline`) — синтаксис и аргументы валидны, `capabilities` отдаёт `hf_to_gguf: available=false` с причиной.
3. Долги D10 закрыты; Фаза 4 (imageworker → bundle) не начата.
