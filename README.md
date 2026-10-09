# OllamaLegion — Adaptive LLM Inference Cluster

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE.md)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg)](https://www.docker.com/)
[![CI test](https://github.com/BarsSky/ollamalegion/actions/workflows/test.yml/badge.svg?branch=centurion)](https://github.com/BarsSky/ollamalegion/actions/workflows/test.yml)

> 🇷🇺 **Русский** (текущий) | [🇬🇧 English documentation](docs/en/README.md)

Интеллектуальный балансировщик нагрузки для llama.cpp inference с адаптивной загрузкой моделей, авто-подбором параметров под доступные ресурсы и мониторингом GPU/CPU/RAM. С R-Image (0.6.x–0.7.x) тот же балансер обслуживает и **генерацию изображений** — воркер `image_cpp` на stable-diffusion.cpp (`sd-server`) регистрируется рядом с текстовыми бэкендами, а клиенты работают через OpenAI- и A1111-совместимые поверхности ([docs/image-generation.md](docs/image-generation.md)).

> **🚦 Быстрый старт** (новое железо или миграция): см. [docs/ru/hardware-presets.md](docs/ru/hardware-presets.md) / [docs/en/hardware-presets.md](docs/en/hardware-presets.md) — 4 готовых пресета для RTX 30xx/40xx/50xx и A10. Скрипт `python scripts/apply-hardware-preset.py <name>` за 30 секунд правит `.env.bundled-with-agent` под вашу GPU.

> **📦 v1.0 — релиз исходников.** Что реально работает, что работает частично и что
> стоит заглушкой (sharded/rpc — этап P2, pipeline/expert parallelism и continuous
> batching — post-1.0): [docs/v1.0-release-notes.md](docs/v1.0-release-notes.md).
> Перед финальным тегом остался ручной smoke на A10:
> [docs/v1.0-smoke-checklist.md](docs/v1.0-smoke-checklist.md).
> Собрать архив исходников без бинарников и образов:
> `pwsh -File scripts/release-sources.ps1`.
> **Важно:** каталог `c/llama.cpp` — git-сабмодуль, в архиве он пуст; для сборки
> без `-tags llama_stub` выполните `git submodule update --init c/llama.cpp`.

## AutoTune (R54–R55.2, 2026-08-24)

Автономный оптимизатор параметров загруженных моделей. Детектит
sub-optimal состояния, вычисляет optimal params, auto-применяет через
async reload. Ручное управление сохранено (per-model `autoTune: false`).

**Что умеет:**
- R54.1: Detect sub-optimal n_ctx, KV cache (q4_0→f16), num_gpu_layers.
- R54.2 + R54.7: Global + per-model toggle (Settings page).
- R54.4: Autonomous reload + circuit breaker (60s cool-down, 300s stable).
- R54.6: Manual apply endpoint (`POST /api/v1/admin/autotune/{id}/apply`).
- R54.8: WebSocket live updates + toast notifications.
- R54.9: Workload-aware KV cache (p95 num_ctx → f16/q4_0).
- R55.2: Event history log (ring buffer 500 entries) + WebUI timeline.

**Endpoints:**
- `GET /api/v1/admin/autotune` — global state per backend.
- `POST /api/v1/admin/autotune/{backendID}/apply` — manual apply.
- `GET /api/v1/admin/autotune/history` — recent events.
- `GET/PUT /api/v1/admin/autotune/config` — global + per-model toggles.

См. [docs/api.md](docs/api.md#autotune-api-r541--r552-2026-08-24) для деталей.

## Что нового

**v0.7.65 — 2026-10-09** (образы `balancer` `r83-submodule-v125`, `webui` `r83-submodule-v127`,
`cppworker-gpu` `r83-submodule-v79`, [полный CHANGELOG](CHANGELOG.md)):

- **Живой инвентарь моделей.** Воркер сам сверяет каталог `.gguf` с диском
  (`CPPWORKER_MODELS_RESCAN_SEC`, по умолчанию 30 с) и пишет в лог, что изменилось
  (`added`/`removed`/`resized`); балансер перечитывает листинги
  (`LB_MODEL_CATALOG_REFRESH_SEC`, по умолчанию 60 с) и публикует изменение состава
  в ленту событий WebUI. Добавление или удаление файла видно **без перезапуска
  контейнеров**; кнопка «Проверить наличие» на вкладке «Модели» и
  `POST /api/models/refresh` делают это немедленно.
- **WebUI больше не показывает модели-призраки.** Пустой ответ воркера применяется
  как есть (раньше список не очищался), недоступный каталог отдаётся как
  `count=0` + `dirError` вместо старого кэша, а на вкладке видно, когда каталог
  проверяли и что изменилось.
- **Страница «GGUF модели» больше не пропадает.** Видимость страниц движка
  (GGUF/Image) определяется составом кластера (`backendTypeCounts`), а не текущим
  фильтром типа; режим в сайдбаре всегда определён — включая смешанный
  «llama.cpp + image.cpp».
- **Монитор: переключатель типа бэкенда работает**, поле топологии расширяется под
  данные (полоса прокручивается, узлы не наезжают и не прячутся за «+N»),
  анимация потока запросов видна на путях клиент→балансер→бэкенд.
- **Холодная модель: параллельные запросы больше не получают 503.** Дедупликация
  авто-загрузки не считается провалом, поэтому N одновременных запросов
  дожидаются загрузки и обслуживаются по очереди; ошибка загрузки отдаётся
  клиенту actionable-текстом (`model … not found in ./models`).
- **Предупреждение о неиспользованной параллельности**: если воркер держит больше
  слотов, чем пропускает балансер, Monitor показывает баннер с конкретными
  настройками — изменения остаются за оператором.

**v0.7.17 — 2026-10-06** (релиз `balancer` `r83-submodule-v96`, [полный CHANGELOG](CHANGELOG.md)):

- **Второй формат вызова модели поддержан**: `[TOOL_CALLS]=[…]` (со знаком «=»
  после маркера) — раньше такой ответ уходил в чат кодом вместо картинки.
- **Прощающее имя модели**: если модель передала заученный ярлык
  (`stable-diffusion.cpp`, `stabilityai/stable-diffusion:…`), балансер рисует уже
  загруженной моделью и честно сообщает о подмене, вместо отказа.
- **Диагностика формата**: нераспознанный маркер вызова теперь виден в логе
  (WARN с превью content) — дефект формата не выглядит как «модель не вызвала
  инструмент».

**v0.7.16 — 2026-10-06** (релиз `balancer` `r83-submodule-v94`, [полный CHANGELOG](CHANGELOG.md)):

- **Исправлен живой дефект: модель отдавала вызов инструмента текстом.** Qwen3
  вернул `[TOOL_CALLS][{"id":…,"type":"function","function":{"name":"generate_image",…}}]`
  в `content` — детектор не понимал вложенный `function` (объект вместо строки),
  вызов не распознавался, и клиент видел его как обычный текст. Теперь вложенная
  форма читается, и картинка генерируется.
- **Мусор шаблона Qwen3 вычищается**: `[]`, `[TOOL_CALLS][]`, `[TOOL_CALLS]=[]` в
  ответе больше не показываются клиенту вместо текста.
- **Модель больше не выдумывает имена моделей**: пустой результат
  `list_image_models` отдаёт список доступных имён, а `generate_image` с неизвестным
  именем отвечает понятной ошибкой со списком (вместо 404 от воркера).

**v0.7.15 — 2026-10-06** (релизы `balancer` `r83-submodule-v92` и `webui` `r83-submodule-v93`, [полный CHANGELOG](CHANGELOG.md)):

- **Ожидание загрузки image-модели теперь правится из WebUI** — поле «Ожидание
  загрузки модели, с» в карточке политики (таб «Image-модели» → «Обзор»), рядом с
  галочкой автозагрузки. Раньше лимит жил только в
  `LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC`: поднять его можно было лишь правкой compose и
  перезапуском.
- Приоритет: значение из WebUI (файл `/app/data/image-resources.json`, переживает
  рестарт) → переменная окружения → дефолт 600 с. Границы 1…86400 с; `0`
  отклоняется («не ждать вовсе»).
- Это нужно для больших моделей: `qwen-image-2.1` (4.7 ГБ) не укладывается в 600 с
  на медленном диске — теперь лимит поднимается из UI без перезапуска.

**v0.7.14 — 2026-10-06** (релиз `balancer` `r83-submodule-v91`, [полный CHANGELOG](CHANGELOG.md)):

- **Инструмент генерации изображений работает и на Ollama-поверхности** `POST /api/chat`
  — то есть при подключении Open WebUI как **Ollama** (по умолчанию), а не только в
  режиме OpenAI. Раньше модель не видела инструменты именно поэтому: объявление жило
  только на `/v1/chat/completions`.
- На `/api/chat` инструменты клиента сохраняются, `tool_choice: "none"` уважается,
  ответ стримится NDJSON-потоком с `done: true`.
- **Диагностика в одну строку**: на каждый запрос чата балансер пишет INFO-строку с
  путём, числом инструментов клиента и причиной отказа объявления. Проверка:
  `docker logs ol-stack-balancer | grep "image tool:"`.

**v0.7.13 — 2026-10-06** (релизы `balancer` `r83-submodule-v90` и `webui` `r83-submodule-v92`, [полный CHANGELOG](CHANGELOG.md)):

- **Текстовая модель видит каталог image-моделей и поднимает нужную сама**: новый
  инструмент `list_image_models` отдаёт имя, семейство, состояние, требования к VRAM,
  дефолты и описание сильных сторон, а `generate_image` умеет загрузить выбранную
  модель, которой нет в VRAM, и сгенерировать картинку за один вызов. GPU каталог не
  занимает.
- **`GET /api/v1/image/models/catalog`** — тот же каталог для оператора и клиентов:
  бэкенды, модели, лимиты, признаки «модель загружена» и «ответ из кэша», причины
  пропуска бэкендов.
- **Описания моделей — данные**: поле `strengths` в профиле (плюс уже бывшее `notes`)
  правится из WebUI и попадает и в API, и в инструмент; приоритет — профиль
  оператора → каталог пресетов → характеристика семейства.
- **Гейт изменился**: при `LB_IMAGE_TOOL_ALLOW_LOAD=on` (по умолчанию) инструмент
  объявляется, если есть здоровый image-бэкенд и **есть что грузить**; `off`
  возвращает прежнее поведение (нужна загруженная модель). Переключается и галочкой в
  WebUI; таймаут загрузки — `LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC` (600 с).
- Время загрузки возвращается в tool-результате (`loadSeconds`), чтобы модель
  объяснила пользователю паузу; починен CI-флаг `-race`
  (`handlers_image_models_test.go`).

**v0.7.11 — 2026-10-06** (релизы `balancer` и `webui` `r83-submodule-v89`, [полный CHANGELOG](CHANGELOG.md)):

- **Политика сосуществования image-генерации с текстом правится из WebUI**: карточка
  «Политика сосуществования с текстом» на табе «Обзор» («Image-модели») — политика
  (`exclusive` / `offload` / `dedicated`), резерв VRAM, ожидание GPU, предохранитель
  лока, «блокировать при неизвестной оценке VRAM», «выключить гейт», кнопки
  «Сохранить» и «Сбросить к встроенным».
- Раньше эти значения жили **только** в `config/config.json` (в стенде он
  смонтирован read-only), env-переменных для них не было, а WebUI показывал политику
  только для чтения. Теперь переопределение хранится в записываемом томе
  (`/app/data/image-resources.json`) и применяется **без перезапуска**.
- Валидация на входе: опечатка в политике отклоняется (а не молча становится
  `exclusive`); «Сбросить» возвращает ровно значения из `config.json`.
- API: `GET/PUT/DELETE /api/v1/image/resources` (токен + rate-limit).

**v0.7.10 — 2026-10-06** (релиз образа balancer `r83-submodule-v88`, [полный CHANGELOG](CHANGELOG.md)):

- Живая проверка инструмента `generate_image` на стенде: гейт (нет загруженной
  модели — инструмента нет), принудительный вызов, отдача картинки и лента запросов
  `surface=chat-tool` в Monitor — всё подтверждено.
- Починен дефект ссылки: воркер может вернуть абсолютный URL
  (`http://localhost:18093/images/…`), и публичный маршрут склеивался в мусор
  (`/v1/images/files/http://localhost:18093/images/…`). Имя файла теперь берётся по
  последнему `/images/` и валидируется.
- Замечание по 8 ГБ VRAM: одновременное проживание крупной текстовой модели и
  image-модели оставляет под генерацию ~0.5 ГБ, и гейт сосуществования честно
  отказывает; с моделью поменьше (Qwen3 2.5 ГБ) генерация идёт 5 с.

**v0.7.9 — 2026-10-03** (релиз образа balancer `r83-submodule-v87`, [полный CHANGELOG](CHANGELOG.md)):

- **Текстовая модель может нарисовать картинку сама**: балансер объявляет ей
  инструмент `generate_image`, если в кластере есть здоровый image-бэкенд и на нём
  **загружена** модель, и **сам исполняет вызов** — генерирует изображение, кладёт
  результат в диалог и просит модель закончить ответ. Клиенту ничего настраивать не
  нужно: он получает текст плюс markdown-ссылку на готовую картинку.
- Работает и для обычного чата, и для агентов: инструмент добавляется в `tools[]`
  запроса к cppworker, `tool_calls` в ответе клиенту не отдаются (их исполняет
  балансер). `tool_choice="none"` и собственный `generate_image` клиента
  уважаются.
- Гейт «только когда есть кому исполнять»: нет здорового image-бэкенда или модель
  не загружена — инструмент не объявляется, запрос идёт как раньше.
- Картинки отдаются по `GET /v1/images/files/{name}` (без токена, как остальные
  клиентские поверхности генерации). Тул-генерации видны в Monitor
  (`surface=chat-tool`). Флаги: `LB_IMAGE_TOOL`, `LB_IMAGE_TOOL_MAX_CALLS`,
  `LB_IMAGE_TOOL_TIMEOUT_SEC`, `LB_IMAGE_TOOL_BASE_URL`.

**v0.7.8 — 2026-10-03** (релиз образа webui `r83-submodule-v85`, [полный CHANGELOG](CHANGELOG.md)):

- **Одна кнопка «Обновить» на всё приложение** (в шапке) вместо россыпи кнопок по
  страницам: на «Image-моделях» их было три одновременно, а `#imgTestRefreshBtn` не
  имела обработчика вообще и не делала ничего.
- **Индикатор свежести** рядом с кнопкой: «Обновлено только что · авто» /
  «Обновлено 12 с назад · пауза» / «не удалось обновить: HTTP 502». Клик по нему —
  обновить сейчас; соседняя иконка ставит авто-обновление на паузу (состояние
  запоминается). Теперь видно разницу между «данные свежие» и «обновление сломалось».
- **Обновление идёт по провайдерам страниц**: кнопка в шапке дёргает источники
  АКТИВНОЙ страницы — метрики монитора, список моделей image-бэкенда, GGUF-список,
  агентов, лог прокси, а не только общий cluster state.
- **Пауза и скрытая вкладка останавливают весь опрос**: было 41 запрос за 7 с
  «в паузе», стало 0 (монитор в iframe, поллинги загрузок GGUF/HF и статус операций
  тоже слушают общее правило).
- Кнопки сохранены там, где это отдельный ресурс: «перечитать список моделей» у
  выбранного бэкенда, снимок загрузок HF, диагностика автотюна.

**v0.7.7 — 2026-10-03** (релизы образов `imageworker` `r83-submodule-v78`+`v80` и `webui` `r83-submodule-v79`+`v81`, [полный CHANGELOG](CHANGELOG.md)):

- **Пометки по файлам на табе «HuggingFace»**: кнопка «Проверить» у каждого файла
  (для самого крупного — автоматически после выбора репозитория) читает Range-запросом
  первые 512 КБ заголовка и показывает, **какое семейство и какую версию узнаёт движок**
  (`Qwen Image 2.1`, `Flux`, `SD1.x`, `SDXL`, …) и нужен ли файлу `--diffusion-model`.
  Весов не скачивается ни байта.
- **Семейство профиля подставляется по заголовку файла** — это лечение ошибки
  «get sd version from file failed»: причина была не в «чужом формате», а в том, что
  DiT-модель (FLUX/SD3/Qwen-Image/Z-Image/Chroma) подключалась как all-in-one
  (`--model`). Движок определяет версию по именам тензоров, и `--diffusion-model`
  добавляет нужный им префикс — см. [docs/image-generation.md §8.2](docs/image-generation.md).
  Ручной выбор семейства оператором не перебивается, расхождение показывается
  предупреждением.
- **Список файлов репозитория стал рекурсивным**: VAE и text encoder из подкаталогов
  (`vae/`, `text_encoders/`) теперь видны и получают правильную роль (`llm` для
  LLM-энкодеров Qwen-Image/Z-Image) — без этого bundle DiT-модели было не собрать.
  Главным файлом репозитория считается самый крупный **diffusion**-файл, а не
  text encoder (в Qwen-Image энкодер 17.5 ГБ против 14.2 ГБ модели).
- Прежнее объяснение («GGUF собран для ComfyUI — sd.cpp его не читает») **опровергнуто
  на живом движке** и убрано из UI и документации: экспорт под ComfyUI читается,
  если набор тензоров совпадает с ожидаемым.

**v0.7.5 — 2026-10-03** (релиз образа webui `r83-submodule-v76`, [полный CHANGELOG](CHANGELOG.md)):

- Новый таб **«Тест»** в «Image-моделях» (релиз v0.7.6 — он же был отдельной страницей в 0.7.5) — инструмент для проверки настроек модели
  генерации: выбор бэкенда и модели, «Загрузить/Выгрузить», полный набор
  параметров (prompt/negative/размер/шаги/cfg/sampler/scheduler/seed/batch),
  подстановка дефолтов профиля («Из профиля»), показ **живого JSON запроса** и
  curl, результат с картинкой и метаданными (время, HTTP, модель, seed), история
  прогонов в памяти вкладки. Запрос идёт **клиентским путём** балансера
  (`/v1/images/generations`, `/sdapi/v1/txt2img`), поэтому проходит VRAM-гейт и
  виден в Monitor. Таб живёт в «Image-моделях», а те появляются **только при
  наличии image_cpp-бэкенда**. Это единственное место WebUI, где показывается
  сгенерированное изображение — «Image-модели» остаются страницей настройки.
- Ошибки движка теперь объясняются по-человечески: «get sd version from file
  failed» → подсказка, что GGUF собран для ComfyUI и sd.cpp его не прочитает;
  «no image model is loaded» → загрузите модель; OOM → уменьшите размер/шаги или
  включите offload.

**v0.7.4 — 2026-10-03** (релиз образа webui `r83-submodule-v74`, [полный CHANGELOG](CHANGELOG.md)):

- Блок **«🔑 Подключение клиентов»** теперь в трёх местах: на странице
  **«Бэкенды»** (раскрытая строка любого бэкенда), в **«Image-модели» → «Обзор»**
  (для выбранного image-бэкенда) и в **«GGUF модели» → «О бэкенде»**. Внутри —
  ключ (маскирован, «Показать»/«Копировать»), эндпоинты под тип бэкенда
  (`:18079` картинки, `:18080` текст) и готовый `curl`. Клиенты (SillyTavern,
  Open WebUI, n8n) обычно требуют непустое поле «API key» — теперь значение
  видно и копируется из UI.

**v0.7.3 — 2026-10-03** (релиз образа webui `r83-submodule-v73`, [полный CHANGELOG](CHANGELOG.md)):

- На странице **«Бэкенды»** в раскрытой строке каждого бэкенда появился блок
  **«Подключение клиентов»**: ключ (маскирован, с кнопками «Показать»/«Копировать»),
  эндпоинты под тип бэкенда (`:18079` для картинок, `:18080` для текста) и готовый
  `curl`.

**v0.7.2 — 2026-10-03** (релиз образов `r83-submodule-v72`, [полный CHANGELOG](CHANGELOG.md)):

R-Image Phase 5–9 — **генерация изображений вторым типом бэкенда**, в одном
стенде и за одним балансером с текстом:

- воркер `image_cpp` (stable-diffusion.cpp / `sd-server`) регистрируется сам и
  обслуживает те же клиенты через OpenAI-совместимую (`:18079`,
  `/v1/images/generations`), A1111-совместимую и нативную поверхности; VRAM-гейт
  и политика сосуществования с текстом — на стороне балансера;
- WebUI: страница **«Image-модели»** в стиле «GGUF модели» — табы «Обзор»
  (CRUD бэкендов, политика, счётчики), «HuggingFace» (поиск репозиториев, файлы
  с **предложенными сервером ролями** `.gguf/.safetensors/.ckpt`, сборка и
  скачивание bundle), «Модели на диске» (состав по ролям, удаление с диска),
  «Загруженные» (SSE-прогресс загрузки), «Загрузки» (активные, история,
  остаточные файлы) и «Настройки» (параметры бэкенда + профили моделей);
- кнопка **«Проверка бэкенда»** — 1 шаг 64×64 клиентским путём, в UI только
  результат, время и модель; показ сгенерированных картинок из WebUI убран
  (это задача клиентов), но поток запросов к image-бэкендам виден в Monitor и
  в `/api/v1/metrics`;
- на странице «GGUF модели» появилось **видимое состояние выгрузки модели**
  («Выгружается… Ns» в карточке, сайдбаре и панели «Модели на диске») — на
  больших моделях выгрузка идёт десятки секунд и раньше выглядела как «ничего
  не происходит»;
- единый Docker-стенд: `docker compose -f docker-compose.stack.yml --profile full up -d --build`
  поднимает балансер, WebUI, cppworker и imageworker (CUDA-сборка sd.cpp в образе),
  проверяется `scripts/docker-stack-smoke.ps1`.

**v0.5.22 — 2026-08-13** ([полный CHANGELOG](CHANGELOG.md)):

Round 35 (коммиты `6140a59` + `df7962e` + `f22dc13`) — crash-loop "model
loaded then immediately reset" полностью закрыт. CppWorker bundled-with-agent
r35 image deployed, no SIGSEGV в логах 26+ минут uptime.

- 🐛 **Cline 65K → reload→load fallback** — раньше cppworker
  `/api/models/reload` возвращал 404 "model not currently loaded"
  когда cppworker только что стартовал (после SIGSEGV auto-restart) и
  ещё не успел загрузить модель, а балансер уже верил в stale
  `loaded: true` от llamaCppMetricsPoller. Cline получал 503+Retry-After
  loop до 120s → ECONNREFUSED. Fix: async reload path переключён с
  `/api/models/reload` (требует loaded state) на `/api/models/load`
  (идемпотентный, handles all 3 cases: not-loaded → load,
  different params → unload+load, same params → 200 dedup).
  Sync reload path получил 404 → load fallback после existing
  202→poll handling. Reuses same payload minus "force" field.

- 🐛 **C++ chat template cgo SIGSEGV** — `common_chat_templates_apply`
  (c/bridge/csrc/chat_thinking.cpp) периодически SIGSEGV'ит в cgo
  execution → exit code 2 → crash-loop. Stack trace: `runtime.cgocall`
  → `bridge_apply_chat_template` в `bridge.go:834`. SIGSEGV-в-cgo
  catchable через `defer recover()` в том же goroutine (Go runtime
  конвертирует через `runtime.sigpanic`). Fix: `func() { defer
  recover() }()` wrapper в `cmd/cppworker/handlers_chat.go:281-310`
  → при cgo SIGSEGV fallback на простой C API `llama_chat_apply_template`
  (стабильный, без C++ common::chat). Для gemma-4 нативный путь
  всё равно ничего не даёт (template не поддерживает enable_thinking),
  так что C++ путь создавал crash opportunities без пользы.

- 🐛 **IdleUnloadManager SIGSEGV** — `checkAndUnload` падал в
  `bridge_free_model` если `LastUsedAt.IsZero()` (модель только что
  загружена). `now.Sub(time.Time{})` = 631 трлн наносекунд
  (2025 лет) → idleTime > idleTimeout → unload сразу →
  use-after-free в C-bridge. Fix: `IsZero()` guard ПЕРЕД `Sub()` в
  `internal/cppbackend/model_manager.go:538-549` (skip unload + debug
  log). Также `config/cppworker-defaults.json:41` `idleUnloadMinutes:
  120 → 0` (off по дефолту; client `keep_alive` достаточно для bundled
  use case).

- 🐛 **4-phase preflight (Round 34 follow-up, commit `6140a59`)** —
  реализация была в working tree с прошлой сессии, восстановлена
  отдельным коммитом. Phase 0: compose env vars (`LB_NCTX_*`) для
  async reload mode. Phase 1: stream dialog с SSE/NDJSON keepalives
  во время async reload (Cline не таймаутит). Phase 2: profile
  mismatch detection (kv_cache_type, flash_attn, use_mmap, не только
  n_ctx). Phase 3: SetLastKnownNCtx on cppworker load/unload
  callbacks. Phase 4: optimal auto-tune params (gpuLayers=-2,
  flashAttn=-1, useMmap=true) в reload payload. Plus 5 cascading
  bug fixes: orphan `mu.Unlock()` (Balancer crash через 75 min),
  `LB_NCTX_PREFLIGHT_ENABLED` env override, preflight для OpenAI
  path (Cline bypass'ил Ollama preflight), `NumCtx` field в
  `openAIChatRequestRaw`, `RequestedNCtxOverride` check в
  `DecidePreflight`.

- 🏗️ **Cppworker image `gpu-86-abort-r35`** (3.86GB, ~18 мин CUDA
  build, deployed 2026-08-12 22:54 UTC). Cppworker bundled r35
  image. Деплой через `docker compose up -d --no-build --no-deps`
  (compose пытается rebuild из source если `build:` секция есть в
  compose, нужно `--no-build` для использования freshly-tagged
  image). Env override в BOTH `deployments/.env` AND
  `deployments/.env.bundled-with-agent` (compose auto-loads `.env`
  без суффикса).

**v0.5.19 — 2026-08-10** ([полный CHANGELOG](CHANGELOG.md)):

- 🐛 **Gemma-4 `<|channel>thought` reasoning tag leak** — при включённой настройке «применять размышления» через балансировщик в ответе приходил смешанный поток `thought\n[text]\n<channel|>` без отделённого reasoning. Gemma-4 GGUF tokenizer содержит special tokens `<|channel>`, `<channel|>`, `<|think|>`, `<think|>` (найдены через `python -c "data.find(b'<|channel|>')"`); gemma-4 chat template рендерит reasoning как `<|channel>thought\n[text]\n<channel|>` **только когда `message.get('tool_calls')`**. cppworker `thinkTagPairs` содержал только 4 стандартных пары (`<think>`/`<thinking>`/`<reasoning>`/`<analysis>`) — `SplitReasoningContent` не split'ил channel format. Fix: добавлены 5 новых tag pairs в **обоих** местах — `cmd/cppworker/reasoning_content.go:thinkTagPairs` + `internal/balancer/llamacpp_translate_resp.go:stripReasoningTags` (defensive double-coverage). balancer получил `stripWithBoundary()` helper (prefix-safe orphan strip, иначе `<think` сожрал бы `<think|>`) + `sharedClose[]` boolean array (root cause первой попытки фикса: `closeTags` для thought и analysis одинаковые `\n<channel|>`, `ReplaceAll` для iter 1 сжирал close, нужный для iter 2 → analysis пара не split'алась). 33 новых test case'а (7 cppworker + 8 standalone pkg + 12 balancer strip + 6 cppworker split standalone). Live verify: gemma-4 с plain-text prompt НЕ эмитит channel format (модель использует plain text reasoning для обычного chat) — это ожидаемо, фикс активируется через Cline с tools или Qwen3.x с native thinking mode.
- ⚡ **Cancel latency 8× faster + prefill heartbeat** — Round 31 #6 (v0.5.16) детектил cancel за 8 секунд на Windows из-за FIN-only close. Fix: **C-bridge n_batch 512 → 64** (8x чаще abort check в prefill loop, ~100-250ms вместо 1-2s; prefill total time не меняется, CUDA amortizes kernel launch overhead; gen phase не зависит от n_batch, использует batch=1). 3 точки: `c/bridge/bridge.c:1381, 1644` + `c/bridge/bridge.go:108-117` + `c/bridge/bridge_stub.go:82-83`. Plus **prefill heartbeat** (immediate client feedback пока C-bridge обрабатывает 5-30s prompt phase): SSE comment `: prefill_started_at=...` сразу после `WriteHeader(200)` в `cmd/cppworker/handlers_openai.go:770-785` (RFC §4.4 keep-alive, клиенты игнорируют но connection alive) + NDJSON `{"done":false}` в `handlers_chat.go:562-577` + `handlers_generate.go:412-419` (Ollama-формат, OpenWebUI tolerates строки без "message" поля). Live verify: heartbeat emitted BEFORE first data token (5K-token prompt, gemma-4), cancel latency **0ms** (с 8s на v0.5.16), TTFB 2-4s на gemma-4 reasoning (prefill-bound, не cancel bug).
- ✨ **Webui cancel button + cross-tab BroadcastChannel sync** — раньше из webui нельзя было отменить активную генерацию (приходилось закрывать Cline/чат-клиент чтобы balancer увидел TCP close), состояние между вкладками отличалось на 5s (polling interval). Fix: **inline `<button class="gguf-cancel-gen-btn">` в busy badge** на loaded model card + `cancelActiveGeneration()` handler (visual feedback: disable + "⏳" + toast + re-enable через 1s). API: `webui/js/modules/api.js:cppworkerCancelGeneration.post(backendId, modelName, userId?)` — POST `/api/cancel` через существующий `GgufApi.requestViaBackend` proxy (идентично `cancelDownloadViaBackend`, `deleteDownloadedFileViaBackend`). Body: `{model, user_id?}` matches cppworker `handleCancel`. **Cross-tab sync**: `BroadcastChannel('ollama-legion-sync')` с lazy init + `typeof BroadcastChannel === 'undefined'` fallback; `backendsEqual` dedup чтобы broadcast шёлся только при реальных изменениях (не каждый 5s poll); `handleCrossTabMessage` триггерит `fetchClusterState()` + `refreshActiveQueriesCrossTab()`; cancel broadcasts `generationCancelled` → другие вкладки обновляются немедленно. `state._activeQueriesTickFn` pattern: store closure ref в state чтобы external modules вызывали `tick()` напрямую для immediate refresh вместо 3s polling. 6 новых i18n ключей (`gguf.busy_badge`, `busy_badge_title`, `active_short`, `cancel_generation`, `generation_cancelled`, `cancelled_short`) на ru.js + en.js. 37 unit-тестов (5 cancel API + 6 cancel button + 9 BroadcastChannel + 12 i18n + 2 dedup + 1 broadcast count) all pass. Live verify: `cancelled=1, by=all` за 11ms через webui proxy path, end-to-end 22 tokens generated → cancel → generation aborts → `/api/infer/active` count=0. 4 контейнера healthy: balancer v32r1 (`0794a4344ab8`) + cppworker:gpu-86-abort-v2 (`b52a4568096a`) + webui:cppworker-bundled (`feda98d7118a`) + agent. **3 коммита** на github/centurion: `3352d59` (channel format), `299bb66` (cancel latency + heartbeat), `15c84bc` (webui cancel + cross-tab). Round 32 #1+#2 = полный Bug 2 fix end-to-end.

**v0.5.16 — 2026-08-09**:

- ✨ **Round 31 #6: C-bridge Abort API — полноценный cancel для streaming inference**. Раньше (v0.5.15) cancel через callback работал только между токенами в gen phase. Теперь cppworker прерывает in-flight `bridge_infer_stream` **на ближайшей check point** (между `llama_decode` batches) — включая **prompt phase** (раньше: длинный prompt 20K токенов блокировал cancel на 1-3 секунды), **multi-slot `bridge_batched_decode`** (раньше: вообще не было cancel-хука), и **gen phase**. C-bridge получил `atomic_int abort_requested` в `InternalModel` — set из Go cgo thread (bridge_request_abort), read из C thread, relaxed memory ordering. Go-side `bridge.RequestAbort(model)` можно вызывать из любой горутины. `cmd/cppworker/abort_watcher.go` (NEW, 81 строка) — goroutine, блокирующаяся на `<-ctx.Done()`, вызывает `bridge.RequestAbort`. Интегрирован в 5 точках: `handleChat`, `writeChatStreamResponseWithTools`, `handleGenerate`, `handleOllamaGenerate`, `writeOpenAIChatStream`, `writeOpenAICompletionStream`. Wire protocol improvement: cancelled response теперь содержит `cancelled: true` (custom field) + `done_reason: "cancelled"` (Ollama) или `finish_reason: "stop"` (OpenAI backward-compat) вместо generic `error`. 6 abort check points в C-bridge: `bridge_infer` prompt + gen, `bridge_infer_stream` prompt + gen, `bridge_batched_decode`, callback cancel path. Backend: `GetHandle(name)` + `GetAllHandles()` для abort_watcher и shutdown. Internal: `BRIDGE_ERR_ABORTED = -100` (negative для однозначного отличия от positive int token count), `ErrCodeAborted = -100`, `ErrAborted` sentinel для `errors.Is`. Backend refactor: существующий callback cancel path теперь возвращает `BRIDGE_ERR_ABORTED` вместо 0 (status code distinction для telemetry). 21 unit-тест (14 Go + 7 C standalone, gcc -Wall -Wextra clean, race-free). Live E2E: `ollama-legion/cppworker:gpu-86-abort-v2` (RTX 3070 sm_86) + Qwen3-Instruct-2507-q4km (Q4_K_M, 36 layers) — реальный C-bridge, реальная модель, VRAM 7201 MiB, 3 AbortWatcher fires зафиксированы. End-to-end chain: TCP close → ctx.Done → abort_watcher → bridge.RequestAbort → C atomic flag → BRIDGE_ERR_ABORTED. Cancel latency: 1-2s для first CUDA batch (warmup), ~85ms per token для subsequent gen batches. 10 sequential cancels + 5 concurrent — все корректно отменяются, model переиспользуется после abort. 4 Docker images готовы: `stub-abort` (171MB), `cpu-abort` (190MB), `gpu-86-abort` (3.86GB), `gpu-86-abort-v2` (3.86GB + wire protocol). Hard cancel (pthread_kill) НЕ реализован — явно отложен как опасен, Round 31 #1+#6 покрывают 99% use cases. Sanitizers (ASan, TSan) — недоступны в MinGW, требуют Linux. Plan: `plans/cppworker-abort-api/PLAN.md` (33KB, 6 phases, 10-17h estimated). Closed Round 31 #6.

**v0.5.15 — 2026-08-05**:

- 🐛 **Order-of-checks: gemma-4 / disabled-модели теперь возвращают чистую ошибку** — в v0.5.14 follow-up мы добавили `disabled: true` в профиль, но проверка была слишком глубоко в коде. В mixed-mode main proxy (`/api/chat` → `ServeHTTP → routeRequest → selectBackend → proxyRequest`) `ensureModelLoadedOnBackend` не вызывается вообще, поэтому gemma-4 с `profile.disabled=true` всё равно летела в cppworker → SIGABRT в `ggml-backend.cpp:1367` (`GGML_ASSERT n_inputs < GGML_SCHED_MAX_SPLIT_INPUTS`). А в single-mode `isModelReadyOnBackend` возвращал `true` для уже загруженной gemma-4 (cppworker запущен с `n_ctx=32768`) → функция возвращала success ДО disabled-check. Плюс circuit breaker открывался на disabled-моделях после 3-х refused-ответов. Fix: disabled-check переехал в самый верх `ServeHTTP` (сразу после `parseRequestBody`), теперь срабатывает ВСЕГДА — для любого backend type, любого routing path, любого состояния модели. Live verify: 5× `gemma-4` → 5× HTTP 503 с чистым `model "gemma-4-E4B-it-Q4_K_M" is marked as disabled in profile`, 0 SIGABRT, 0 breaker-ошибок. В `ensureModelLoadedOnBackend` остался второй disabled-check (перед `isModelReadyOnBackend` и breaker'ом) для direct-вызовов из warmup scheduler. 3 новых unit-test'а (all PASS).
- 🐛 **Bug #4: OpenWebUI cancel** — добавлен `r.Context().Done()` check в streaming loop (Round 6 Fix 5). Round 31 #6 (v0.5.16) полностью заменил этот workaround на C-bridge Abort API — см. v0.5.16.

**v0.5.14 — 2026-08-05**:

- 🐛 **cppworker `/api/chat` keepalive must be NDJSON, not SSE comment** — Round 6 Fix 5 слал `: keepalive\n\n` каждые 100ms для TCP idle-prevention, но `/api/chat` это **NDJSON** (Ollama native), а не SSE. Строгие NDJSON-клиенты (Cline CLI, ollama-python) бросали `invalid json: : keepalive` на КАЖДОЙ строке (сотни ошибок за inference). Fix: `{"keepalive":true}\n` (валидный NDJSON) + интервал 100ms → 15s (через `getHeartbeatInterval(15s)`). OpenAI-compat `/v1/chat/completions` (SSE) — без изменений, SSE-комменты корректны для EventSource-клиентов. Live verify: `0 SSE comment lines, 0 invalid JSON` для обоих путей.
- 🐛 **bundled-full: дублирующийся backend при рестарте** — `entrypoint.sh` запускал `register-with-balancer.sh` несмотря на `CPPWORKER_REGISTER_DISABLE=true` (disable действовал только на Go-side). Дубль `cppworker-gpu-bundled` (`weight=10`, hard-coded в shell-скрипте) перебивал реальный `cppworker-gpu-bundled-agent` (`weight=1`) при routing. Fix: добавлена проверка `CPPWORKER_REGISTER_DISABLE` в `entrypoint.sh` для shell-скрипта. Существующий дубль удаляется через `DELETE /api/v1/backends/{id}`.
- ✅ **Cline CLI v2.16.0 + Qwen3 (E2E)** — keepalive-фикс обязателен. Настройка: `ollama` provider (НЕ `openai-compatible` — тот проксирует через cline.ai и требует платный Cline balance), `baseUrl=http://your-host:18092`. Cline system prompt 16K токенов → нужен `n_ctx=32768`.
- ✅ **Qwen3.6-35B-A3B-UD-Q4_K_M (20.6GB) inference** — MOE (35B total / 3B active). На 8GB VRAM + 24GB RAM с `numGpuLayers=20` (фактически 7/40 на GPU, 33/40 на CPU): load 12.7 мин, inference ~0.91 tok/s (медленно из-за CPU offload), простые запросы работают (`3+5=8` за 12.7s). Полная GPU-загрузка требует ≥24GB VRAM.

**v0.5.13 — 2026-08-04**:

- ✨ **WebUI busy badge + async apply profile (Round 26)** — при генерации ответа моделью cppworker'овский `handleReloadModel` блокировал на `inflight.WaitZero` 30-60+ сек. WebUI не мог ни показать, ни изменить настройки. Теперь: cppworker endpoint `/api/models/active-queries` показывает busy count, **api server детектит busy ДО apply и возвращает HTTP 202 + Location + SSE progress** (events каждые 500ms), WebUI показывает `🔴 Generating (N active)` на loaded model card + per-backend progress modal с auto-fallback на polling.
- ✨ **n_ctx overflow detection + X-Model-Context-Warning header (Round 26)** — preflight check перед каждой генерацией (`ComputeContextWarning`). Уровни: `ok` / `approaching` (>80% n_ctx) / `overflow` (prompt+n_predict>n_ctx, clamp до 0) / `impossible` (prompt>n_ctx). Auto-clamp n_predict (без reload-loop). HTTP headers в `/api/chat`, `/api/generate`, `/v1/chat/completions` — клиент (OpenWebUI/Cline/Hermes) видит предупреждение ДО обрыва.
- 🐛 **Long-conversation cutoff fix** — юзер подтвердил cutoff во ВСЕХ клиентах (OpenWebUI, Cline, Curl, Roo Code, IDE plugins, Hermes) → server-side. Config defaults: `streamingIdleTimeout: 600→1800`, `streamTimeout: 0→1800` (10 мин → 30 мин). Per-model profile override (через WebUI wizard) остаётся приоритетным.

**v0.5.12 — 2026-08-04**:

- ✨ **SSE load progress** — WebUI переключился с polling каждые 1.5s на `EventSource`. Cppworker шлёт `text/event-stream` push-events каждые 500ms с `{state, elapsedMs, loadingSizeBytes}`. Heartbeat `:keepalive` каждые 15s. Auto-close на terminal state. **Live verify**: 30 events за 15s direct, 22 events за 11s через balancer API proxy (без буферизации).
- ✨ **Measured load time cache** — динамическая оценка `estimatedLoadTimeMs` на основе реальных измерений (weighted average последних 20 load'ов, stale-фильтр 7d, zero-filter). После первого load'а Qwen3 (2.5GB, 99s) estimate становится ~70s вместо хардкода 28s. Более реалистичный feedback для клиента.
- 🔧 **WebUI auto-cleanup** — `pagehide` + `visibilitychange` listeners останавливают все polling/SSE при уходе со страницы. Без этого фоновые EventSource'ы удерживали cppworker'а после закрытия вкладки.
- 🔧 **Balancer SSE proxy** — `internal/api/gguf_backend_proxy.go` детектит `Content-Type: text/event-stream` и стримит напрямую через `streamCopy` (без буферизации). `X-Accel-Buffering: no` для nginx.

**v0.5.11 — 2026-08-04**: Dynamic / async model loading + Bug fixes #1, #2.

- ✨ **Dynamic / async model loading** — gemma-4 (5GB) и другие большие модели больше НЕ ломаются по client timeout. `POST /api/models/load` теперь возвращает **HTTP 202 Accepted + Location за <100ms** с динамической оценкой `estimatedLoadTimeMs` (compute из `size / 100MB/s + ctx + 2s overhead`). Реальный load идёт в background goroutine, polling через `/api/models/load/progress`. `?wait=true` для legacy sync. **Live verify**: Qwen3-Instruct-2507-q4km (2.5GB) async load = 101ms response + 99s background, end-to-end chat = 44s (load + gen), 2.1s на already-loaded.
- 🐛 **Bug #1: WebUI settings UI hang** — `handleReloadModel` теперь async (default). Apply с новыми n_ctx больше не зависает UI на 30-60+ сек при reload'е reasoning-модели.
- 🐛 **Bug #2: WebUI "active/Unload" badge на unloaded моделях** — `isLoaded` check сломан при несовпадении имён (`m.name` с `.gguf` vs `lm.name` без). Fix: `stripGGUF` helper + normalized match + path basename. Устойчиво к load через WebUI/API/balancer warmup.
- 🔧 **Balancer integration** — `executeLlamaCppLoad` детектит 202 + `status=loading` и поллит до `state=loaded` (max = estimated × 1.5 + 10s). `ensureModelLoadedOnBackend` deadline 5s → 5min (метрики кэшируются с 1-2s задержкой; 5s давал false positive).

**v0.5.10 — 2026-08-04**: Round 23 — bug fixes (5 из 9) + persistent ccache infrastructure.

- 🐛 **Bug #6+#8: Smart-skip reload** — клиент (Cline/OpenWebUI) шлёт `num_ctx=32000` "на всякий случай", но реальный prompt "2+2?" = 1 токен. Balancer теперь проверяет `estimated_prompt + n_predict` — если помещается в loaded_n_ctx, **patch'ит body** и proxy'ит БЕЗ reload. Live verify: 1.3s/req (было 30-50s + retry-loop).
- 🐛 **Bug #4: OpenWebUI cancel** — добавлен `r.Context().Done()` check в streaming loop. Cancel теперь останавливает генерацию <1s (было: модель работает до natural completion).
- 🐛 **Bug #7: Think block leak** — убран L3 "give up" на 1024 chars. Auto-detect теперь ВСЕГДА проверяет весь outputBuf на любой из 4 think-тегов.
- 🐛 **Bug #5: WebUI -2 GPU layers** — `GPU_LAYERS_MIN -1 → -2`, display "AUTO" для auto-рассчёта.
- 🏗️ **Persistent ccache** — `DOCKER_BUILDKIT=1` + `docker buildx create --driver docker-container` + `.dockerignore build-*/`. ccache теперь действительно persistent: 47 мин cold, **<2 мин warm** для Go-изменений (было 20+ мин каждый build).

**v0.5.9 — 2026-08-04**: Documentation audit + 3 pre-existing test-bug fixes (percentiles off-by-one, racy concurrent test, sanitize expectations) + security fix (real GitHub PAT removed).

- 🐛 **CRITICAL bug fix: `temperature=0` от клиента теперь honor'ится** — раньше Go-слой игнорировал `temperature=0` (Cline/Aider/Continue все шлют greedy) и подставлял default `0.7`, что приводило к не-детерминированным tool calls. Pointer types в request structs (`*float64` / `*int`) различают "не задано" от "explicit 0". 8 unit-тестов.
- 🟡 **P1 — 4 code-review fix'а**: rename misleading function, BatchedScheduler head-of-line blocking (TokenCh buffer 8→128 + non-blocking send + drop counter), sampleFromLogits silent fallback → logging + counter, UnloadModel infinite wait → 10s timeout.
- 🧹 **Dead code cleanup** — `im->sampler` removed из C-bridge (после sampler hotfix стал no-op).
- 🧪 11 новых unit-тестов (build params, sample stats). Production verified: 5/5 unique до фикса, 1/5 после на `temperature=0` (greedy).

**v0.5.1 — 2026-07-30**: Batched Parallel Inference (Round 15.1) + Round 15.2 (multi-token prefill, temperature sampling, vocab-aware EOG) + **Multi-tool Recovery** (Qwen3-4B-Instruct quirk с trailing `}]}`).

**v0.5.0 — 2026-07-30**: Batched Parallel Inference functional baseline + 6 unit-тестов.

**v0.4.12 — 2026-07-29**: Native `enable_thinking` для Qwen3-thinking + Round 13 inference fix.

**v0.4.6 — v0.4.11** (2026-07-28): WebUI settings, api token auth, Cline/Roo совместимость через `X-API-Token`, Phase 8 RPC Coordinator (production), n_parallel > 1 (state isolation), preflight n_ctx reload.

**v0.4.5** (2026-06-25): Cluster-level model management + cascade fallback + graceful reload.

**v0.2.0-adaptive** (2026-06-22): адаптивная загрузка, KV-cache fallback, авто-reload n_ctx, bundled deployment.

**v0.1.0** (2026-05-14): initial release.

Все 12+ релизов с v0.2.0+ документированы в [CHANGELOG.md](CHANGELOG.md). Каждый коммит
помечен тегом — `git log v0.5.2..HEAD` показывает unpushed changes.

## Быстрый старт (Bundled)

Требуется: Docker 24+ с поддержкой Compose v2, Git, NVIDIA драйвер + NVIDIA Container Toolkit (для GPU-режима).

**Канонический compose — `deployments/docker-compose.stack.yml`.** Один файл,
четыре сценария через профили (подробно: [docs/deployment-stack.md](docs/deployment-stack.md)):

| Сценарий | Команда | Что поднимается |
|---|---|---|
| Всё на одной машине | `--profile full` | cppworker + imageworker + balancer + webui |
| Только бэкенды, балансер на другой машине | `--profile worker` | cppworker + imageworker |
| Только управление, бэкенды подключаются сами | `--profile balancer` | balancer + webui |
| Откат: внешний сборщик метрик вместо встроенного | `--profile worker --profile legacy-agent` | то же, что `worker`, метрики собирает контейнер `agent` |

> **Где живёт агент метрик (с 2026-10-07).** У ОБОИХ воркеров он ВСТРОЕН и пишет
> в ту же запись бэкенда, поэтому отдельных контейнеров нет: cppworker —
> `CPPWORKER_AGENT_EMBEDDED=on`, порт метрик **18034**; imageworker —
> `IMAGE_WORKER_AGENT_EMBEDDED=on`, порт метрик **18033**. Внешний `agent` нужен
> только для **Ollama**-бэкендов (профиль `legacy-agent`): у Ollama нет нашего
> Go-кода, метрики собрать некому. Два сборщика на одном бэкенде ставить нельзя —
> метрики «мигают», в частности `gpu.uuids`, по которому балансер понимает, что
> несколько бэкендов делят одну физическую GPU (иначе суммарная VRAM удваивается).
>
> Если image-бэкенд виден в WebUI, но показывает `0.0%` / `0 MB` — встроенный
> агент на этой машине не запущен: проверьте `IMAGE_WORKER_AGENT_EMBEDDED=on` и
> строку `embedded agent started` в логе воркера. До 0.7.32 у imageworker это
> умолчание было `off`, а у cppworker — `on`.

```bash
# 1. Клонировать репозиторий (имя каталога — произвольное).
#    Флаг --recurse-submodules ОБЯЗАТЕЛЕН: c/llama.cpp/ — git submodule
#    pinned на upstream commit 1d2869c6e (ggml 0.19.0, 2026-08-13).
git clone --recurse-submodules https://github.com/BarsSky/ollamalegion.git
cd ollamalegion

# Если уже клонировали без --recurse-submodules:
#   git submodule update --init --recursive

# 2. Подготовить .env (один раз)
cp deployments/.env.example deployments/.env                                  # интерполяция compose
cp deployments/.env.bundled-with-agent.example deployments/.env.bundled-with-agent   # env_file: модели/GPU/лимиты
# API-токен задаётся ТОЛЬКО в deployments/.env (CPPWORKER_API_TOKEN) —
# один на balancer/cppworker/agent/webui. В .env.bundled-with-agent он НЕ влияет
# (`environment:` переопределяет `env_file`) — смените его там и получите 401.
# Остальное в .env.bundled-with-agent правьте под свою машину.
```

> 🔑 **Токен — ровно одно место: `CPPWORKER_API_TOKEN` в `deployments/.env`.**
> Compose выводит из него `LB_API_TOKEN`, `API_TOKEN`/`CPPWORKER_API_TOKEN`
> (cppworker и agent) и `BALANCER_TOKEN`. Править `auth.tokens` в
> `config/config.json` **бесполезно**: при заданном `LB_API_TOKEN` список из
> файла полностью заменяется. Балансер говорит об этом в шапке логов
> (`Auth tokens: LB_API_TOKEN … ЗАМЕНЯЕТ config.json`) и предупреждением.
> Подробности — [docs/deployment-stack.md](docs/deployment-stack.md), п. 5.1.

> 🏷 **Какая сборка запущена — в шапке логов.** Балансер, агент и cppworker
> печатают тег образа и коммит первой строкой:
> `║Version: r83-submodule-v23 (commit 5731414, 2026-09-28)║`.
> Если тег не тот, который вы собирали, — в контейнере старый образ, и искать
> дефект в коде бессмысленно.

**Windows (PowerShell):**

```powershell
$env:DOCKER_BUILDKIT=1
$env:CUDA_ARCH=86        # sm_86 для RTX 30xx, sm_89 для RTX 40xx, sm_90 для RTX 50xx
# --env-file НЕ нужен: compose сам читает deployments/.env, а .env.bundled-with-agent
# подключён через env_file:. Флаг --env-file ПОДМЕНЯЛ .env целиком, из-за чего
# терялись CUDA_ARCH (сборка под 9 архитектур ~40 мин вместо ~3) и токен.
docker compose -f deployments/docker-compose.stack.yml --profile full up -d
```

**Linux / macOS / WSL2:**

```bash
export DOCKER_BUILDKIT=1
export CUDA_ARCH=86
docker compose -f deployments/docker-compose.stack.yml --profile full up -d
```

После запуска:
- Балансер: http://localhost:18080 (Ollama API) / :18081 (Management API)
- CppWorker: http://localhost:18092 (llama.cpp inference)
- WebUI: http://localhost:18083

> 🐳 **Свой репозиторий образов — одна переменная `IMAGE_REGISTRY`** в
> `deployments/.env` (пусто = стандартный путь). Значение — адрес registry
> **со слэшем на конце**, иначе compose склеит строки буквально:
> ```bash
> IMAGE_REGISTRY=local-docker-hub:5000/
> # → local-docker-hub:5000/ollama-legion/balancer:<тег> и так же для остальных
> ```
> Скрипты сборки читают ту же переменную, поэтому собранный образ получает ровно
> то имя, которое ищет compose:
> `powershell -File scripts/build-containers.ps1 -Tag <тег>`.
> Что получится, можно проверить без запуска:
> `docker compose -f deployments/docker-compose.stack.yml config | grep image:`,
> а разбор префикса — `powershell -File scripts/lib-image-registry.tests.ps1`.
> Подробности — [docs/deployment-stack.md](docs/deployment-stack.md), п. 5.3.

> **Только бэкенд на этой машине** (балансер уже есть): задайте две переменные в
> `deployments/.env.bundled-with-agent` — `BALANCER_URL=http://<хост-балансера>:18081`
> (ADMIN API, не 18080) и `BACKEND_HOST=<адрес этой машины, видимый с балансера>`,
> затем `docker compose -f deployments/docker-compose.stack.yml --profile worker up -d`.
> Подробности и диагностика: [docs/deployment-stack.md](docs/deployment-stack.md).

> ⚠️ **Имена переменных агента.** Код читает `BACKEND_TYPE`, `CPPWORKER_URL`,
> `NODE_LABELS`, `GPU_MODE`, `COLLECT_INTERVAL`, `HEARTBEAT_INTERVAL`. Имена вида
> `AGENT_BACKEND_TYPE` / `AGENT_CPPWORKER_URL` / `AGENT_NODE_LABELS` / `AGENT_MODE`
> (встречаются в старых compose-файлах) **не читаются**: бэкенд зарегистрируется
> как Ollama. `docker-compose.stack.yml` использует правильные имена.

> ⚠️ **Вместимость.** `AGENT_MAX_CONCURRENT_REQUESTS` должен совпадать с реальным
> `n_parallel` cppworker (по умолчанию 1). Завышенное значение даёт ложные
> «свободные слоты» и отказы/зависания у клиента (Cline). Балансер обязан быть
> **`r83-submodule-v23+`**: в нём вместимость применяется на всех трёх путях
> регистрации, и унаследованное из `state.json` значение больше не переживает
> пересборку. Если в `/api/v1/backends` видите `maxConcurrentRequests` больше
> заданного — рецепт в [docs/deployment-stack.md](docs/deployment-stack.md), п. 7.1.

> ⏱ **Первая загрузка 27B — минуты, а не секунды** (на A10 наблюдалось 311 с).
> Клиент (Cline) с коротким таймаутом порвёт соединение раньше, чем закончится
> загрузка, и это будет выглядеть как «cppworker не работает». Загружайте модель
> заранее через `POST /api/models/load-with-params?wait=false` — подробности и
> диагностика в [docs/deployment-stack.md](docs/deployment-stack.md), п. 2.1.

> Если нужна более простая сборка **без sidecar-агента метрик**, используйте
> `deployments/docker-compose.cppworker-bundled.yml` + готовые скрипты:
> `.\scripts\start-bundled.ps1` (Windows) или `./scripts/start-bundled.sh` (Linux/macOS).

## Архитектура

```
Клиент (Cline/OpenWebUI)
  → Balancer (:18080) — routing, session affinity, n_ctx auto-reload
    ├── Preflight (Round 35) — 4-phase n_ctx + profile mismatch detection
    │   ├── Phase 0: env vars (LB_NCTX_*)
    │   ├── Phase 1: stream dialog with SSE/NDJSON keepalives
    │   ├── Phase 2: kv_cache_type / flash_attn / use_mmap mismatch
    │   ├── Phase 3: SetLastKnownNCtx on cppworker load/unload
    │   └── Phase 4: optimal auto-tune params in reload payload
    ├── Reload→Load fallback — if /api/models/reload returns 404
    │   (model not currently loaded), use /api/models/load (idempotent)
    └── CppWorker (:18092) — llama.cpp inference
      ├── Adaptive Loader — SelectStrategy: f16→q8_0→q4_0, MoE, partial offload
      ├── KV-cache fallback — работает без GGUF metadata
      ├── Auto-reload API — /api/models/reload + /api/models/load
      └── Chat template — native C++ path wrapped in recover() (Round 35)
    → Agent (:18032 внешний для Ollama / :18034 встроенный в cppworker) — NVML GPU/CPU/RAM метрики, health check
  → ImageWorker (:18093) — sd.cpp image_cpp бэкенд, метрики встроенного агента на :18033
  → WebUI (:18083) — дашборд, мониторинг, sparkline
```

## Порты

| Порт | Компонент | Назначение |
|------|-----------|------------|
| 18080 | Balancer | Ollama API proxy (и `/v1/*` поверхность) |
| 18081 | Balancer | Management API + WebSocket (клиентских поверхностей нет) |
| 18079 | Balancer | OpenAI-поверхность (`/v1/chat/completions`, `/v1/images/generations`, `/sdapi/v1/*`) |
| 18092 | CppWorker | llama.cpp inference |
| 18093 | ImageWorker | API sdworker (`/api/image/*`, `/api/hf/*`) — R-Image |
| 18034 | CppWorker | Метрики **встроенного** агента (`CPPWORKER_AGENT_EMBEDDED=on`) |
| 18033 | ImageWorker | Метрики **встроенного** агента (`IMAGE_WORKER_AGENT_EMBEDDED=on`) |
| 18032 | Agent (внешний) | Метрики GPU/CPU/RAM для **Ollama**-бэкендов |
| 18083 | WebUI | Дашборд |

Порты встроенных агентов разнесены специально: два процесса не должны слушать
один порт, а балансер опрашивает метрики по `agentPort` конкретной записи.
Оба встроенных агента включены по умолчанию; `off` имеет смысл только когда
метрики собирает внешний контейнер `agent` — и никогда одновременно с ним.

Порт движка изображений (по умолчанию 18094) слушает только localhost внутри
контейнера воркера — снаружи он не публикуется, см. [docs/image-generation.md](docs/image-generation.md#7-порты-и-переменные-окружения).

## Сборка

Все команды выполняются из **корня репозитория** (каталог, в который вы склонировали проект).

**Windows (PowerShell):**

```powershell
$env:DOCKER_BUILDKIT=1
$env:CUDA_ARCH=86     # 61 = GTX 10xx (Pascal), 75 = RTX 20xx, 86 = RTX 30xx, 89 = RTX 40xx, 90 = RTX 50xx
# CUDA_ARCH читается compose'ом из deployments/.env — флаг --env-file его ПОДМЕНЯЛ,
# и сборка уходила на 9 архитектур (~40 мин вместо ~3). Задайте CUDA_ARCH в .env.
# ⚠️ CUDA 13 снял поддержку Pascal (GTX 10xx): для 1070/1080 оставайтесь на CUDA 12.x
#    (в проекте это 12.2) и собирайте с CUDA_ARCH=61.

# ── СБОРКА: канонический stack.yml собирает ВСЁ САМ ───────────────────────────
# У всех пяти сервисов (loadbalancer, webui, cppworker-gpu, imageworker, agent)
# есть build:, поэтому `up -d` САМ СОБЕРЁТ отсутствующий образ — предварительная
# сборка не обязательна и локальный реестр не нужен.
docker compose -f deployments/docker-compose.stack.yml --profile full build          # всё
docker compose -f deployments/docker-compose.stack.yml --profile full build cppworker-gpu
docker compose -f deployments/docker-compose.stack.yml --profile full build imageworker
docker compose -f deployments/docker-compose.stack.yml --profile full build loadbalancer
docker compose -f deployments/docker-compose.stack.yml --profile full build webui
docker compose -f deployments/docker-compose.stack.yml --profile legacy-agent build agent
```

**Linux / macOS / WSL2:**

```bash
export DOCKER_BUILDKIT=1
export CUDA_ARCH=86          # 61 для GTX 10xx (Pascal)

docker compose -f deployments/docker-compose.stack.yml --profile full build
```

BuildKit=1 обязателен — с ним Go-only изменения собираются за ~30 сек (cached CUDA layers).

> **Альтернатива — скрипт `scripts/build-containers.ps1`**, он сам подставляет
> тег и префикс реестра:
> `-CppWorker -CudaArch 61` (текст), `-Balancer`, `-WebUI`, `-Agent`,
> `-Tag <р83-тег>`. **Image-воркера в скрипте нет** — его собирайте через
> `docker compose ... build imageworker` (команда выше).

> **Карты прошлых поколений (GTX 1070 и родственные).** Для генерации картинок
> подходят: `imageworker` собирается на **Vulkan** и работает независимо от
> CUDA-архитектуры. Для текста тоже подходят — `cppworker` собирается на CUDA 12.2,
> где Pascal (`sm_61`) поддерживается; собирайте с `CUDA_ARCH=61`. Оговорки
> (fp16 на Pascal медленный, 8 ГБ требуют offload, память между картами НЕ
> объединяется) — в [docs/deployment-stack.md](docs/deployment-stack.md) §4.3.

## Воркеры: вместе и порознь

Текстовый (`cppworker`) и image-воркер (`imageworker`) — **независимые сервисы**:
у каждого свой порт, свой каталог моделей, свой движок. Поднимать их можно как
вместе, так и по отдельности, и любой из них работает без второго.

### Вместе (текст + картинки)

```bash
cd deployments
docker compose -f docker-compose.stack.yml --profile full up -d
docker ps --format '{{.Names}}\t{{.Status}}' | grep ol-stack
```

Оба пула встают за ОДНИМ балансером: текст маршрутизируется по типу бэкенда,
картинки — по явному признаку запроса (`/v1/images/*`, `/sdapi/v1/*`,
`/api/image/*`, префикс модели `sd:`). Проверка:

```bash
curl -s localhost:18081/api/v1/backends | jq '.backends[] | {id, type}'
#  → {"id":"cppworker-gpu-bundled-agent","type":"llama_cpp"}
#    {"id":"imageworker","type":"image_cpp"}
```

### Только текстовый воркер

```bash
cd deployments
docker compose -f docker-compose.stack.yml --profile full up -d loadbalancer cppworker-gpu webui
# или на отдельной машине, с балансером на другой (BALANCER_URL в .env!):
docker compose -f docker-compose.stack.yml --profile worker up -d cppworker-gpu
```

### Только image-воркер

```bash
cd deployments
docker compose -f docker-compose.stack.yml --profile full up -d loadbalancer imageworker webui
# отдельно от балансера — SDWORKER_BALANCER_URL указывает на внешний:
SDWORKER_BALANCER_URL=http://192.168.1.10:18081 \
  docker compose -f docker-compose.stack.yml --profile worker up -d imageworker
```

### Оба воркера на РАЗНЫХ машинах

На каждой машине — свой `--profile worker` и адрес общего балансера:

```bash
# машина 1 (текст):
#   .env: BALANCER_URL=http://<хост-балансера>:18081
docker compose -f docker-compose.stack.yml --profile worker up -d cppworker-gpu

# машина 2 (картинки):
#   .env: SDWORKER_BALANCER_URL=http://<хост-балансера>:18081
docker compose -f docker-compose.stack.yml --profile worker up -d imageworker

# машина 3 (управление) уже поднята профилем balancer; воркеры регистрируются сами
```

Балансер сам сведёт обе машины в один пул и будет учитывать VRAM каждой
отдельно. Одинаковые бэкенды на разных хостах — это НЕ дубли: дедупликация
идёт по `host` + порт, а совпадение физической карты определяется по её UUID
(см. `gpu.uuids` в `/api/v1/metrics`).

**Проверка, что воркер доехал до балансера** (любой сценарий):

```bash
docker logs <контейнер> 2>&1 | grep -E 'registered with balancer|Agent registered'
curl -s localhost:18081/api/v1/backends | jq '.backends[] | {id, host, type, hasAgent, agentPort}'
```

## Конфигурация

### .env (ключевые переменные)

Дефолты — из `deployments/.env.bundled-with-agent.example` и `config/cppworker-defaults.json` (defaultCtxSize=32768).

```bash
CPPWORKER_CTX_SIZE=32768       # контекст по умолчанию
CPPWORKER_GPU_LAYERS=20        # слоёв на GPU (-1=все, -2=авто)
CPPWORKER_AUTO_OFFLOAD=true    # авто-расчёт gpuLayers
CPPWORKER_AUTO_TUNE_NCTX=true  # авто-подбор n_ctx
CPPWORKER_AUTO_KV_CACHE=true   # авто-выбор kvCacheType (q4_0/q8_0/f16)
CPPWORKER_RAM_FALLBACK_N_CTX=true  # RAM fallback при нехватке VRAM
```

Дополнительные (в .env-файле, но редко меняются):
`CPPWORKER_BATCH_SIZE`, `CPPWORKER_FLASH_ATTN_TYPE`, `CPPWORKER_N_THREADS`,
`CPPWORKER_NUMA`, `CPPWORKER_USE_MMAP`, `CPPWORKER_SPLIT_MODE`,
`CPPWORKER_RAM_FALLBACK_GPU_LAYERS`, `CPPWORKER_RAM_FALLBACK_MAX_N_CTX`,
`CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS`, `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD`,
`CPPWORKER_DEFAULT_N_PREDICT_REASONING` (reasoning-модели, default 8192),
`OLLAMALEGION_HEARTBEAT_MS`.

### Балансер (config.json)

Полный пример — `config/config.example.json`. Минимальный релевантный фрагмент
(с реальными дефолтами из `config/config.example.json`):

```json
{
  "balancing": {
    "firstByteTimeout": 30,
    "nctxReload": {
      "auto_reload_n_ctx": true,
      "auto_reload_max_n_ctx": 131072,
      "auto_reload_vram_safety_factor": 0.85,
      "auto_reload_timeout_sec": 120
    }
  }
}
```

> ENV-overrides для `nctxReload` (приоритет над config.json):
> `LB_NCTX_RELOAD_ENABLED`, `LB_NCTX_RELOAD_MAX_N_CTX`,
> `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR`, `LB_NCTX_RELOAD_TIMEOUT_SEC`.
> Читаются в `internal/balancer/nctx_reload_config_bridge.go`.

## Структура проекта

```
cmd/
  balancer/          — HTTP proxy + Management API (port 18080/18081)
  cppworker/         — llama.cpp inference server (port 18092)
    adaptive_loader.go     — SelectStrategy: авто-подбор параметров
    adaptive_integration.go — adaptive API routes, NaN-healer
    handlers_model.go      — load/reload с адаптивной стратегией
    lazyload.go            — ленивая загрузка моделей
    reasoning_content.go   — reasoning extraction (qwen3.5, deepseek-r1, gemma-4)
    balancer_register.go   — auto-register в балансер
    nctx_clamp.go          — preflight clamp по VRAM
  agent/             — GPU/CPU метрики (NVML, port 18032)
  monitor/           — встроенный монитор-страница (HTML UI)
  rpcworker/         — RPC worker для distributed inference (Phase 8 P.1)

internal/
  api/               — HTTP handlers, routes, auth, rate-limit
    routes.go              — все REST endpoints балансера
    gguf_backend_proxy.go  — proxy /api/v1/gguf/backends/{id}/proxy/*
  balancer/
    nctx_reload.go          — auto-reload n_ctx
    nctx_reload_adaptive.go — запрос стратегии у cppworker
    proxy.go, ollama_router.go, llamacpp_router.go — routing
    scoring.go              — resource-aware scoring
    session_manager.go      — session affinity, stickiness
    rpc_coordinator_dispatcher.go — Phase 8 P.1 RPC coord
  cppbackend/
    backend.go        — Go-обёртка над C-bridge, params, KVCacheType
  agent/             — общие типы для agent
  config/            — config loading, validation
  modelreplication/  — per-model replication groups
  rpccoordinator/    — RPC coordinator state + workers
  rpcworker/         — RPC worker runtime
  rptensor/          — tensor parallelism (P.3, research)
  virtualmodel/      — Phase 8 P.2 virtual models (alias-on-pool)

c/                   — C-bridge к llama.cpp (build-msvc-cuda / build-cpu-mingw)
  llama.cpp/         — submodule: исходники llama.cpp
  bridge/            — Go ↔ C bridge
  ggml/              — submodule: GGML tensor library

deployments/
  docker-compose.stack.yml                — канонический: профили full / worker / balancer / legacy-agent
  .env.example                            — переменные для stack (копируется в .env)
  docker-compose.agent.yml                — только агент (то же, что --profile legacy-agent)
  …остальные docker-compose.*.yml         — исторические/узкоспециальные, см. docs/deployment-stack.md §9

config/
  config.example.json         — шаблон основного config балансера
  config.bundled.json         — bundled-конфиг для docker-compose
  cppworker-defaults.json     — дефолты для CppWorker (defaultCtxSize=32768)
  cppworker.example.env       — env-файл для локального cppworker
  agent.example.env           — env-файл для локального agent

scripts/             — утилиты: start-bundled.{ps1,sh}, build-containers, e2e-тесты
docs/                — полная документация (RU + EN)
plans/               — roadmap, ADR, фазовые отчёты
```

## Документация

- 🇷🇺 [docs/README.md](docs/README.md) — индекс документации (RU)
- 🇬🇧 [docs/en/README.md](docs/en/README.md) — English documentation index
- 🇷🇺 [docs/installation.md](docs/installation.md) — установка и сборка
- 🇬🇧 [docs/en/installation.md](docs/en/installation.md) — installation & build
- 🇷🇺 [docs/deployment.md](docs/deployment.md) — развёртывание
- 🇬🇧 [docs/en/deployment.md](docs/en/deployment.md) — deployment
- 🇷🇺 [docs/api.md](docs/api.md) — REST API
- 🇬🇧 [docs/en/api.md](docs/en/api.md) — REST API (English)
- [plans/README.md](plans/README.md) — roadmap

## Безопасность перед публикацией стенда

- **Смените API-токен.** Значение по умолчанию
  (`changeme-bundled-with-agent-token`) известно всем, кто видит этот репозиторий.
  Пока оно не изменено, любой, кто дотянется до admin-порта (18081), получает
  полный доступ к управлению кластером: `deployments/.env` →
  `CPPWORKER_API_TOKEN` (он же `LB_API_TOKEN` в контейнерах), затем
  `docker compose ... up -d --force-recreate`.
- **Не публикуйте реальные адреса и токены.** В документации используются
  адреса из RFC 5737 (`192.0.2.x`) и публичные примеры; свои адреса, токены и
  пути к моделям держите в `deployments/.env` (файл в `.gitignore`).
- **Админ-порт наружу не выставляйте.** 18081 — управляющий API; для клиентов
  нужны 18079 (OpenAI/image) и 18080 (Ollama-совместимый).

## Коммиты и авторство
Автор коммита определяется git-конфигом: локальный `user.name`/`user.email` в этом
репозитории имеет приоритет над глобальным.

```bash
git config --local user.name  "BarsSky"
git config --local user.email "knagaenko@mail.ru"     # или глобальные значения
```

Если коммиты делает ассистент/скрипт, задайте для него ту же или служебную
идентичность осознанно: GitHub считает контрибьюторов **по полю author**, поэтому
каждая новая identity с отдельной почтой превращается в отдельного контрибьютора
в сайдбаре репозитория.

Исторические служебные идентичности агента сводятся к владельцу файлом
[`.mailmap`](.mailmap) (переписывания истории не требуется — SHA, теги и релизы не
меняются):

```
BarsSky <knagaenko@mail.ru> Claude <noreply@anthropic.com>
BarsSky <knagaenko@mail.ru> Mavis <Mavis@mavis.local>
BarsSky <knagaenko@mail.ru> cline <cline@local>
```

Проверить, как идентичности сводятся локально:

```bash
git shortlog -sne github/centurion | head
```

## Лицензия

MIT


**v0.5.10 — 2026-08-04**: Round 23 — bug fixes (5 из 9) + persistent ccache infrastructure.
