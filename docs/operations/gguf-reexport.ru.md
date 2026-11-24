# Ре-экспорт GGUF: опциональный модуль (R709, D10 закрыты; Фаза 4 — image-bundle)

**Состав:** `services/gguf-reexport/` (Go-хост) + `py/reexport_job.py` (драйвер), образ
`ollama-legion/gguf-reexport`, профиль compose `reexport`. **Статус:** метаданные/переупаковка
проверены на живом 16.6 ГиБ файле (§6); Фаза 4 («набор файлов → image-bundle») — на живом
наборе FLUX (§7); полный ре-экспорт (HF-чекпойнт) — таргет образа `full` (§5), не собран.

## 1. Задача и почему отдельный сервис
`Inkling-2-Mini` объявляет `architecture=inkling2` и `block_count=48` при 24 физических блоках —
loop-экспорт; загрузчик ждёт ОТДЕЛЬНУЮ архитектуру `inkling2_loop`. `normalize` переупаковывает
метаданные и раскладывает лежащие матрицы запросов, `convert` прогоняет HF-чекпойнт через
`convert_hf_to_gguf.py`, цель `image_bundle` собирает каталог image-модели (роли по R696).
Отдельный сервис, а не ручка в воркере: torch (+2.5 ГБ), 16.6 ГиБ ввода-вывода на одном диске,
а однослотовый воркер занял бы слот инференса. Go держит HTTP/состояние/отмену, python — работу.

## 2. Контракт API
```
GET    /health                      → 200 {"status":"ok"} (HEALTHCHECK)
GET    /api/version                 → запечённая идентичность образа (R486)
GET    /api/reexport/capabilities   → что умеет ПРЯМО СЕЙЧАС (targets[]: available/reason)
GET    /api/reexport/models         → GGUF тома моделей + вердикт схемы
POST   /api/reexport/jobs           → {model_dir, target, options} → 202 задача
GET    /api/reexport/jobs           → список задач (свежие первыми)
GET    /api/reexport/jobs/{id}      → статус/прогресс/ошибка/хвост лога
DELETE /api/reexport/jobs/{id}      → отмена: процесс убит, недописанный файл удалён
```
`capabilities` — источник истины для WebUI: `inkling2_loop` (python + скрипт патча),
`hf_to_gguf` (torch + конвертер; в light `available=false`), `image_bundle` (всегда доступна:
роли читает Go-парсер заголовков). Ошибки — `{"error":{"code","message","detail"}}`, §8.
Задачи — файлы в томе `reexport_jobs` (tmp+rename): состояние переживает пересоздание
контейнера, задача из `running` после рестарта становится `interrupted`. Параллелизм 1.

## 3. Куда пишется результат, сколько нужно места, отмена
**Выход всегда внутри `${MODELS_DIR}`.** Файловые цели: `out_path` — имя/подкаталог ВНУТРИ
каталога модели (`filepath.Rel`, не «префикс строки»), пусто — рядом с моделью. `image_bundle`
пишет в каталог image-моделей (`GGUF_REEXPORT_IMAGE_MODELS_DIR`, по умолчанию `<MODELS_DIR>/image`;
у воркера — `SDWORKER_IMAGE_MODELS_DIR`): bundle вне его воркер не увидит, поэтому путь вне
каталога — `path_not_allowed`. Пустое имя = имя каталога набора, совпадение с ним — «манифест
на месте» (без копирования). `REEXPORT_OUT_DIR` — legacy-fallback и за том не выводит (иначе
предупреждение на старте и `out_dir_ignored=true`). **Место — ДО старта** (`Statfs`/
`GetDiskFreeSpaceEx`) с числами: `normalize` — вход + 1 ГиБ, `convert` — вход + выход,
`image_bundle` — вход + выход (копия набора; «манифест на месте» не требует ничего); не хватает
— `failed`/`insufficient_space`. **Прогресс** — `stage` = `reading → writing → validating` (на
`image_bundle` — скопированные байты). **Отмена** удаляет недописанный файл; неполный КАТАЛОГ
bundle остаётся (гигабайты уже скопированы — решает оператор), `profile.json` пишется последним.

## 4. Как WebUI узнаёт о модуле и что вызывает
Страница моделей создаёт пустой `#ggufReexportHost`, а `js/modules/gguf-reexport.js` делает
`GET /api/reexport/capabilities` через nginx панели: 200 → блок рисуется, 424/502/таймаут →
**вкладки нет вообще** (флаг в `.env` был бы вторым источником правды). nginx: upstream —
**переменная** (`set $reexport_upstream ...`): статический `proxy_pass` разрешается на старте
и без контейнера роняет ВСЮ панель (R229); проверка — `webui/tests/reexport-nginx-proxy-r709.test.js`.

| Шаг UI (D11 + Ф4) | Кнопка | Что вызывается на самом деле |
|---|---|---|
| 1. Модель и вердикт схемы | (клик по строке) | `GET /api/reexport/models` — шапки читает Go-парсер |
| 2. Проверить | «Проверить» | `POST /api/reexport/jobs` с `options.dry_run=true` (цель `inkling2_loop`) |
| 3. Нормализовать | «Нормализовать (loop-схема)» | тот же `POST` без `dry_run`; «Отменить» — `DELETE /jobs/{id}` |
| 4. Пересканировать модели | «Пересканировать модели» | `POST /api/models/refresh` на воркерах + `GET …/models?refresh=1` |
| 5. Проверить загрузку | «Проверить загрузку» | `POST /api/v1/backends/{id}/models` `{operation:"load"}`, статус — `/models/load/progress` |
| 6. Собрать bundle (Ф4) | «Проверить набор» / «Собрать bundle» | `POST /api/reexport/jobs`, цель `image_bundle` (`dry_run` у проверки) |

**«Проверить» — это `dry_run`, а не поле `mode`** (`DisallowUnknownFields`; лишнее поле дало бы
400). **Два файла:** блок — `js/modules/gguf-reexport.js`, транспорт задач
(`window.GgufReexportJobs`) — `js/modules/gguf-reexport-jobs.js` (порядок тегов в
`webui/index.html` обязателен; без слоя шаги говорят `reexport.jobs_module_missing`); причина
разделения — страж R628 (боевой файл от 1500 непустых строк запрещён).

## 5. Воркеры, профиль, версии, таргеты образа
После успеха WebUI показывает «Пересканировать модели» → `POST /api/models/refresh` (R93) плюс
сброс кэша фактов (воркер держит инвентарь с TTL). У image-воркера это
`POST /api/image/models/scan` (Фаза 4: GET перечитывает реестр только при смене набора
каталогов, поэтому запись `profile.json` в существующий каталог оставалась невидимой).
Compose: `profiles: ["reexport"]`, `${MODELS_DIR}:/app/models:rw`, том `reexport_jobs`,
healthcheck своим бинарём, порт только на `127.0.0.1`; без профиля ничего не создаётся.
Переменные (`.env.example`): `GGUF_REEXPORT_TAG`, `GGUF_REEXPORT_BUILD_TARGET` (light|full),
`GGUF_REEXPORT_WITH_TORCH`, `GGUF_REEXPORT_PORT_HOST`, `GGUF_REEXPORT_OUT_DIR` (legacy, §3),
`GGUF_REEXPORT_SCAN_DEPTH`, `GGUF_REEXPORT_LOG_LEVEL`, `GGUF_REEXPORT_IMAGEWORKER_URL` (пусто —
проверка локальная, задача пишет `worker_checked=false`), `GGUF_REEXPORT_IMAGE_MODELS_DIR`.
Версии: `scripts/release-all.ps1` знает `gguf-reexport` и пополняет `release-manifest.json`;
`scripts/check-image-tags.ps1` сверяет тег и `llamaCommit`. **light** (по умолчанию) — python3
+ stdlib + скрипт патча; **full** (`WITH_TORCH=1`) — конвертер, `conversion/`, `gguf-py` (+2.5 ГБ).

## 6. Живой прогон (2026-11-23, CPU): `Inkling-2-Mini-UD-Q4_K_M.gguf`
Файл 16 610 543 328 Б (15.47 ГиБ), 58 KV, 734 тензора, данные с байта 11 978 464.

| Проверка | Результат |
|---|---|
| `GET /api/reexport/models` (10 файлов) и dry-run | 3.5 с; `scheme=inkling2_loop_export`, `block_count=48`, `physical_blocks=24`, `patchable=true`; план — `succeeded` за **576 мс** (7 изменений метаданных, 144 переименования) |
| **ре-экспорт** (fuse) | `succeeded` за **22 мин 57 с**; 16 610 544 736 Б (+1 408); `block_count` 48→24, `loop.*`, 12 `loop_msg_query`; 62 KV / 744 тензора |
| негатив (Qwen3-4B) и очередь | `failed` / `model_not_patchable` («ожидался architecture=inkling2, получено 'qwen3'»); три задачи строго по одной, остальные `queued` |
| загрузка результата (локальный том) | `state=loaded` за **50 341 мс** (ctx 8192, 6/49 слоёв на GPU), 0.2 ток/с; ctx 32768 — 32 119 мс |

## 7. Фаза 4 на живом наборе (2026-11-24): набор FLUX → bundle
Набор `image/flux-schnell-q3-k`: 4 файла, 8 790 448 988 Б (копия жёсткими ссылками, оригинал не менялся).

| Проверка | Результат |
|---|---|
| роли по заголовкам (БЕЗ `profile.json`), dry-run | `diffusion`, `vae`, `clip_l`, `t5xxl`; семейство `flux`; вердикт **ready** (`--diffusion-model`) |
| `profile.json` набора и подсказка `roles` | роли не перебиваются содержимым: VAE остаётся VAE (arch «Flux.1-AE» — подсказка, не приговор) |
| сборка `sd15-set` (1 файл, 1 566 768 416 Б) | `succeeded` за **970 мс**; `profile.json` + `sd-server.config.json`; вердикт ready; воркер видит bundle (`compat.verdict=ready`) |
| «манифест на месте» (`out_path` = каталог набора) | `succeeded` за **72 мс**, копии нет; вердикт ready; воркер видит bundle |
| набор из одного text encoder (живой кейс R696); воркер недоступен | `failed` / `bundle_not_ready` (diffusion-роль занята энкодером) и `worker_unavailable`/`bundle_not_visible` — «собрали и не проверили» не успех |

## 8. Что делать при отказе
| Код | Что случилось | Что делать |
|---|---|---|
| `insufficient_space` | меньше, чем «вход (+ выход) + 1 ГиБ» | освободить том или взять вход меньше; числа — в сообщении |
| `path_not_allowed` | путь вне каталога модели/тома (у bundle — вне каталога image-моделей) | убрать `..` и абсолютные пути; `out_path` bundle — имя каталога в `image/` |
| `model_not_found`, `out_of_models_dir`, `input_missing` | источника нет в томе или в нём нет весов | выбрать модель/каталог из `GET /api/reexport/models` |
| `model_not_patchable` | чужой arch или нет loop-контроллеров | полный ре-экспорт (цель `hf_to_gguf`, образ `full`) |
| `bundle_not_ready` | набор собран, но движок не поднимет: нет ролей / роль занята энкодером | добавить файлы ролей (`roles`) или взять другой набор; причина в отказе |
| `bundle_not_visible`, `worker_unavailable` | воркер не видит каталог или не ответил | тот же том (`GGUF_REEXPORT_IMAGE_MODELS_DIR` = `SDWORKER_IMAGE_MODELS_DIR`), воркер поднят |
| `dst_exists`, `target_unavailable` | результат есть (у bundle — непустой каталог) / цель закрыта в этой сборке | `force` или другое имя; `targets[].reason` в `capabilities` (для `convert` нужен образ `full`) |
| `converter_failed`, `verify_failed`, `canceled`, `interrupted`, `queue_full` | упал конвертер / не сошлась схема / отмена или рестарт / очередь > 64 | причина в `log_tail`; перезапустить или дождаться (параллелизм 1); недописанный ФАЙЛ удаляется, неполный каталог bundle остаётся |

## 9. Что осталось
1. **Скорость чтения решает:** на bind-mount Windows 11–30 МиБ/с — держите модели на локальном томе.
2. **Образ `full` не собран:** `hf_to_gguf: available=false` с причиной (синтаксис таргета проверен `buildx --call=outline`).
3. **Воркер старой сборки не умеет `scan`:** сервис откатывается на GET и помечает `worker_stale_possible`; пробная загрузка bundle — шаг 5 UI (в задаче — вердикт `ready`).
