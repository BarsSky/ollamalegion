# План: image-generation backend (diffusion) в OllamaLegion

> **Дата:** 2026-09-27 (rev. 2 — учтены данные исследования `docs/research-sdcpp-lowvram-integration.md`)
> **Статус:** proposal (код не менялся; разведка выполнена чтением)
> **Связанные документы:** [`research-sdcpp-lowvram-integration.md`](../docs/research-sdcpp-lowvram-integration.md) (детальные замеры, флаги, размеры моделей), [`backend-type-isolation.md`](../docs/backend-type-isolation.md), [`round-22-balancer-general-audit.md`](round-22-balancer-general-audit.md), [`2026-09-23-multi-backend-placement-policy.md`](2026-09-23-multi-backend-placement-policy.md), [`2026-09-25-memory-fit-subsystem.md`](2026-09-25-memory-fit-subsystem.md), [`.clinerules`](../.clinerules) §3, §6, §7, §9

---

## 0. Постановка задачи

Научить OllamaLegion обслуживать **генерацию изображений** рядом с текстовым инференсом:

1. К кластеру из N LLM-бэкендов (Ollama / llama.cpp) добавляется **image-бэкенд** (движок диффузии), который по промпту генерирует картинку и возвращает её.
2. Балансер **сам** понимает, что пришёл запрос генерации — **по явному признаку** (endpoint / имя модели / явное поле), без эвристик по тексту промпта. Текстовые запросы не попадают на image-бэкенд, и наоборот.
3. Клиент либо (A) сам шлёт image-запрос, получив от балансера «расшифровку» формата, либо (B) LLM получает **инструмент** `generate_image` и картинка возвращается клиенту через балансер.
4. Модели качаются с **HuggingFace** так же, как GGUF сейчас (токен, зеркало, прогресс, resume).
5. Приоритет — **небольшие модели и слабые GPU (4–8 GB VRAM)** с offload в RAM/CPU.
6. Правки — **лаконичные**: встраиваемся в существующие механизмы (типы бэкендов, маршрутизация по пути, HF-прокси, профили, метрики), а не переписываем балансер.
7. **Топология (решение заказчика):** балансер остаётся проксирующим распределителем — сессии, распределение запросов, определение нагрузки и доступных бэкендов **не меняются**. Добавляется **отдельная OpenAI-поверхность балансера на порту 18079** (стиль OpenAI сразу и для llama.cpp, и для генерации) и **полностью отдельный image-бэкенд на своём порту** (18093) со своими моделями. Порт 18080 (универсальный прокси) и 18081 (management API) — без изменений; проксирование Ollama-путей на OpenAI-порту оставляем как задел на будущее, не приоритет.
8. **Принятое решение по объёму (2026-09-27): идём по вариантам A+B** — внешний `sd-server` + наш Go-враппер `sdworker`. Уровень C (патчи upstream) **не делаем** до отдельного запроса. Обязательное условие: **свой клиент не пишем** — сначала проверяем, какие существующие клиенты отрабатывают, а недостающую совместимость закрываем нормализацией в враппере/`openai_surface` (см. §12).

---

## 1. Выбор движка: кратко

**Рекомендуется `stable-diffusion.cpp` (`sd-server` / `sd-cli`)** как единственный движок, который даёт GGUF-квантование **с реальной экономией VRAM** (в torch-UI GGUF — storage-only и, по их же докам, «not compatible with model offloading»). Готовые релизы: **Vulkan 30–37 MB**, CPU 17–26 MB, win-cuda12 338 MB (+ cudart 564 MB); Linux-CUDA-релиза нет — под CUDA собирается из исходников.

Критичные ограничения движка (определяют дизайн враппера — источник: `research-sdcpp-lowvram-integration.md` §0, §4):

| Ограничение | Следствие для нас |
|---|---|
| **Одна модель на процесс**, hot-swap отсутствует; `sd_ctx` создаётся до `listen()` | load = spawn, unload = kill, reload = kill+spawn. Никаких «переключений на лету» |
| `POST /sdapi/v1/options` **не существует** (только GET) | A1111-способ смены чекпойнта не работает — не проектировать под него |
| **Всё сериализуется одним мьютексом** (`sd_ctx_mutex`) | параллелизм на одной карте = 1. Наша очередь, а не надежда на многопоточность движка |
| **Отмена в полёте не поддерживается**: `cancel_generating: false`, `POST /sdcpp/v1/jobs/{id}/cancel` для `generating` → **409** | отменяем только `queued`; в UI нельзя обещать «отменить генерацию» |
| **Прогресса по шагам нет** (в job нет step/fraction) | прогресс только `queue_position` + «generating» + elapsed; процент шагов рисовать нечем |
| TTL завершённых job'ов **600 с** → потом **410 Gone** | результат забирать сразу; иначе хранить картинку у себя |
| Лимиты (константы): размеры **64…4096**, `max_batch_count` **8**, `max_queue_size` **64**, upscale 8192; переполнение → **429** | валидировать на входе, не полагаться на движок |
| `--host/--port` → **`--listen-ip/--listen-port`** (после `master-600`); неизвестный флаг = падение | валидировать флаги/пиновать версию (`master-929-3f8527a`) + contract smoke-тест |
| `seed: -1` → известный integer overflow → `generate_image returned no results` + зависание | **всегда** резолвить seed в явное положительное число на нашей стороне |
| `<lora:...>` в промпте намеренно не поддерживается во всех трёх API | LoRA только структурированным полем `lora[]` |

⚠️ **Не опираться на Ollama**: генерация изображений была добавлена в v0.14.3 и **удалена в v0.32.6**; при этом `/api/tags` до сих пор врёт про `capabilities:["image"]`. Схему `keep_alive`/`/api/ps` скопировать стоит, саму фичу — нет.

---

## 2. Что уже есть в проекте (факты из кода)

### 2.1 Типы и маршрутизация

| Что | Где | Состояние |
|---|---|---|
| `BackendType` — закрытый enum из 2 значений | `pkg/types/backend_type.go:6-9` | расширить |
| `BackendEngine`, `ResolveEngine`, `ModeBackendTypes`, `ModeEngines`, `AllBackendTypes`, `Label`, `Emoji` | `pkg/types/backend_type.go:14-18, 149-158, 188-203, 222-279` | расширить |
| Три нормализатора, **молча** превращающие неизвестный тип в `ollama` | `internal/balancer/backend_type_filter.go:79-89`, `internal/api/backend_type_handlers.go:194-204`, `internal/config/backend_type_validation.go:136-149` | **главная ловушка** |
| Резолв порта (`default → OllamaPort`) | `internal/balancer/backend_state.go:54-69` | добавить ветку |
| Тип по пути запроса (любой `/v1/*` → `llama_cpp`) | `internal/balancer/proxy.go:480-512` (`:508-510`) | добавить явный признак |
| **Ранний 404 на `/v1/images/*`** | `internal/balancer/router.go:207-228` (`case` `:214`), вызов `:69-77`, счётчик `metrics.go:175-176` | снять/сузить |
| Интерфейс `BackendRouter` (`Route/BackendType/Name`) + диспетчер | `router_interface.go:23-42`, `router.go:106-160`, `llamacpp_router.go:73-159`, `ollama_router.go:36-75` | шаблон для `ImageRouter` |
| Фильтрация кандидатов по типу уже есть | `candidate.go:35-137`, `backend_selector.go:13-43` | переиспользуем |
| CRUD бэкендов, allow-list `"ollama, llama_cpp"` | `internal/api/handlers_backends.go:500-681` (`:568-575`, health `:655-681`), `backend_type_handlers.go:63-126` | расширить |
| Capabilities есть, но **в выборе бэкенда не участвуют** | `pkg/types/model_capabilities.go:17-90`, заполнение `llamacpp_metrics_poller.go:255-353`, чтение только для заголовков `capabilities_headers.go:19-70` | критерий маршрутизации вводим сами |

### 2.2 Переиспользуем «как есть»

- **HF-загрузчик**: `internal/cppbackend/hf_downloader.go` — токен (`:228-230`), зеркало `HF_MIRROR` (`:193-208`), Range-resume (`:764-805`), прогресс 100 мс (`:883-908`), история/orphans/cancel/delete (`:1107-1262`), колбэк (`:127-135, 1097-1099`); эндпоинты `cmd/cppworker/handlers_hf.go:17-207`.
- **HF-прокси через балансер**: `internal/api/gguf_backend_proxy_handlers.go:55-104`, `gguf_backend_proxy.go:31-327` (90 s, проброс `Authorization` для `/api/hf/*` `:160-162`, SSE `:276-295`) — логика **не знает про GGUF**.
- **Профили моделей**: механизм pull-синка `internal/api/handlers_cppworker_profiles.go:55-220`, `cmd/cppworker/sync_profile.go:140-351`; схема `LlamaCppModelProfile` (`pkg/types/balancing.go:267`) — тексто-специфична, контейнер/API переиспользуемы.
- **Idle unload / active queries / состояния загрузки**: `internal/cppbackend/model_manager.go:883-987`, `backend.go:67-131, 2044-2142`.
- **HTTP-каркас воркера** (`/health`, `/metrics`, SSE-прогресс, middleware): `cmd/cppworker/router.go:11-107`.
- **Метрики/регистрация**: `cmd/cppworker/balancer_register.go:166-273`, поллинг `internal/balancer/llamacpp_metrics_poller.go:99-264`.
- **Docker-каркас**: `deployments/docker-compose.cppworker.yml:107-182`.
- **Admission-очередь, параметризованная типом**: `internal/balancer/unified_queue_r73.go:48-83`, `admission_queue.go`.
- **Схема hot-swap у Ollama** (`keep_alive`, `/api/ps`) — как образец семантики load/unload; у LocalAI стоит скопировать `--max-active-backends=1` + LRU + «не вытеснять модель с активным запросом».

### 2.3 Чего нет

- Упоминаний image/diffusion/sd-server/`images/generations` в Go-коде нет (только 404-заглушка).
- Мульти-движковой абстракции в `c/bridge` нет: `cppbackend` импортирует `bridge` напрямую (`backend.go:394`), C-интерфейс llama-центричен.
- Расчёта памяти для диффузии нет: `internal/memfit/*`, `nctx_*`, `kv_layers.go` считают KV-cache LLM — **неприменимы**.
- Запуска субпроцессов нет: `exec.Command` только для `nvidia-smi` (`cmd/cppworker/vram_detect.go:90`); поле `HuggingFaceCLIPath` (`internal/cppbackend/config.go:131-133`) — мёртвое.

### 2.4 Ловушки, подтверждённые кодом

1. **Тихая нормализация типа в `ollama`** (3 функции).
2. **`Stream: true` по умолчанию** для любого POST без поля `stream` (`proxy.go:1576`) + fallback true (`proxy_request.go:930`) → image-запрос уедет в streaming/NDJSON-путь.
3. **llama-специфичные шаги в общем flow**: preflight n_ctx (`llamacpp_transport.go:86-111`), AutoTune (`:146`), workload tracking (`:117-121`), `normalizeOpenAIBody` вырезает `image_url` (`openai_normalize.go:99`).
4. **Prewarm / unload / warmup / AutoPull жёстко текстовые**: `warmupModel` (`proxy_request.go:1045-1215`), `prewarm_controller.go:187`, `unload_scheduler.go:162-289`, AutoPull (`proxy.go:1151-1198`).
5. **VRAM-гейты** считают текстовые метрики: `slot_manager.go:91-158`, `scoring.go:23-55`.
6. `ValidateBackendTypeConsistency` (`internal/config/backend_type_validation.go:12-38`) написан под «все бэкенды одного типа».
7. Health-probe завязан на engine (`internal/balancer/health.go:195-207`).

---

## 3. Порты, маршрутизация «по явно присутствующему запросу» и расшифровка

### 3.0 Раскладка портов (решение заказчика, зафиксировано)

Балансер остаётся **проксирующим распределителем**: логика сессий, распределения запросов, определения нагрузки и доступных бэкендов **не меняется**. Меняется только то, что появляется **отдельная поверхность API в стиле OpenAI** и **полностью отдельный бэкенд генерации на своём порту**.

| Порт | Кто | Что обслуживает | Изменение |
|---|---|---|---|
| **18080** | балансер, прокси (`LoadBalancer.Port`, `cmd/balancer/main.go:365-366`) | универсальный прокси: Ollama `/api/*` + `/v1/*` + трансляции | **не трогаем** (поведение байт-в-байт, регрессионные тесты) |
| **18081** | балансер, management API (`LoadBalancer.APIPort`, `main.go:413-414`) | `/api/v1/*` (настройки, бэкенды, профили, кластер) | **не трогаем** |
| **18079** | балансер, **новая OpenAI-поверхность** | только OpenAI-стиль: `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`, `/v1/images/*` | **новое** |
| **18093** | **image-бэкенд** (отдельный процесс/контейнер `sdworker` + `sd-server`) | свой порт, свои модели генерации, свой каталог моделей | **новое** |

Смысл OpenAI-порта: к llama.cpp и к моделям генерации можно обращаться **напрямую в стиле OpenAI**, без проксирования «не-Ollama модели под Ollama API» и без лишних трансляций. Заодно это нормализует работу с cppworker (сейчас `/v1/*` через 18080 идёт по Ollama-ориентированному пути с трансляциями и авто-стримом).

**Как это делается технически (дёшево):** один и тот же экземпляр `*Proxy` (единое состояние: бэкенды, сессии, очередь, метрики, скоринг) монтируется на **второй** `http.Server` через тонкую обёртку-адаптер поверхности (`openaiSurface`):

1. `/api/v1/*` → форвард на management API (как сегодня, `router.go:57-63`);
2. `/health`, `/metrics` → как есть;
3. `/v1/*` → **строгий OpenAI-режим**: стриминг решается **только** полем `stream` в теле (сейчас `proxy.go:1576` ставит `Stream: true` по умолчанию, а `isStreamingRequest` даёт fallback true — на 18079 это отключаем); никаких Ollama-трансляций; модель резолвится по списку `/v1/models`;
4. Ollama-нативные пути (`/api/chat`, `/api/generate`, `/api/tags`, `/api/ps`, `/api/show`, `/api/pull`, …) → **понятная ошибка** с указанием «используйте порт 18080»;
5. **Проксирование Ollama-путей на OpenAI-порту — на будущее, не приоритет** (задел оставляем: тот же `Proxy`, тот же роутинг, только выключенный allow-лист путей).

Конфиг: `LoadBalancerSettings.OpenAIPort int json:"openAiPort"` (`pkg/types/config.go:29-39`), env `LB_OPENAI_PORT`, default **18079**, дефолты/сброс в `internal/config/config.go:136-137, 320-323`, баннер `cmd/balancer/main.go:151-154`, дефолты в отдаваемом конфиге `internal/api/handlers_cluster.go:20-23`. TLS-вариант (если понадобится) — по образцу `TLSPort`/`TLSPort+1`, то есть `+2`.

### 3.1 Маршрутизация по явному признаку

Порт + endpoint — **главный** явный признак; префикс модели и явное поле — вторичные (для нестандартных клиентов и для режима B с инструментом).

| # | Признак | Пример | Класс |
|---|---|---|---|
| 1 | OpenAI-порт, image-endpoint | `18079` + `POST /v1/images/generations`, `/v1/images/edits` | image |
| 2 | OpenAI-порт, image-модель | `18079` + `"model": "sd:sdxl-turbo-q8"` на любом `/v1/*` | image |
| 3 | WebUI-endpoint (через 18080 или 18093) | `POST /sdapi/v1/txt2img`, `/sdapi/v1/img2img` | image |
| 4 | Наш endpoint | `POST /api/image/generate`, `GET /api/image/*`, `GET /v1/images/capabilities` | image |
| 5 | Явное поле тела | `"capability": "image"` | image |
| 6 | OpenAI-порт, текстовые endpoints | `18079` + `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings` | text |
| 7 | Порт 18080 (как сегодня) | `/api/chat`, `/api/generate`, `/v1/*` | по существующим правилам, без изменений |

Обратное правило (image → только image-бэкенд, text → только текстовый) обеспечивается существующим `isBackendTypeAllowed` (`backend_selector.go:33-43`) при условии, что тип определён верно. `determineRequestBackendType` (`proxy.go:480-512`) получает ещё один вход — «поверхность запроса» (какой порт обслужил), и на OpenAI-поверхности классифицирует `/v1/images/*` и `sd:`-модели как `BackendTypeImage`, а остальные `/v1/*` — как `BackendTypeLlamaCpp`.

### 3.2 Выбор API внутри (важное уточнение после исследования)

- **Внутренний контракт враппер↔движок — нативный async `/sdcpp/v1/*`**, а не OpenAI-совместимый. Причины: только он даёт `queue_position`, `capabilities.limits`, полный набор параметров и polling; `openai`/`sdapi`-семейства синхронные и не дают ни очереди, ни статуса.
- **Наружу** (клиентам) — на OpenAI-поверхности **18079**: `POST /v1/images/generations` (sync-обёртка над submit+poll, `data[].b64_json`), `POST /sdapi/v1/txt2img` (passthrough), нативный `POST /api/image/generate` + `GET /api/image/jobs/{id}` (через image-воркер/18093 или 18080).
- Свою очередь держим **мы** (64-слотовую очередь sd-server не используем как основной механизм — иначе двойная очередь и потеря контроля), но её лимиты читаем из `GET /sdcpp/v1/capabilities` при старте.
- Наружу честно сообщаем `cancel_generating: false` (отмена только для `queued`) и не показываем процент шагов.

### 3.3 «Расшифровка» для клиента/LLM (discovery)

- `GET /api/v1/image/contract` — endpoints, обязательные/опциональные поля, **лимиты движка** (64…4096, batch ≤8, queue ≤64), список доступных моделей с дефолтами (steps/cfg/sampler/size), примеры curl, JSON-Schema инструмента `generate_image`.
- `GET /v1/images/capabilities` — агрегированные capabilities image-бэкендов (samplers, schedulers, loras, upscalers, limits, defaults_by_mode, output_formats_by_mode).
- `GET /api/v1/image/backends` — по образцу `gguf_backends_handler.go:44-185`.
- `/v1/models` — image-модели в общем списке с пометкой capability (клиент видит `sd:<id>`).

### 3.4 Два режима вызова

- **A (клиент напрямую):** клиент читает `/api/v1/image/contract` и шлёт `POST /v1/images/generations`. Ничего нового в архитектуре не нужно.
- **B (инструмент у LLM):** на MVP — **на стороне клиента/агента** (клиент сам вызывает балансер и подкладывает картинку в диалог), это повторяет существующий паттерн tool_calls (`internal/balancer/llamacpp_transport.go`) и не трогает балансер.
  **Серверный tool-loop** (LLM вернул tool_call → балансер сам сходил в image-бэкенд) — Phase 6 как **явный** режим `operatingMode: image_tool_loop`. По умолчанию не включать: балансер сейчас stateless-прокси, tool-loop ломает модель сессий/стриминга/таймаутов.

---

## 4. Варианты реализации

### Вариант A — sd-server как внешний бэкенд (MVP)

Пользователь сам поднимает `sd-server` на **отдельном порту (18093)** со своим каталогом моделей, регистрируем его как бэкенд типа `image_cpp` с `APIStyle = openai-compatible`; балансер проксирует `/v1/images/*` (в т.ч. через новый OpenAI-порт 18079).

- ➕ 2–3 дня (вместе с OpenAI-портом 18079), ноль C++, контракт уже OpenAI-совместимый.
- ➕ Мгновенная проверка всей маршрутизации/изоляции типов.
- ➖ Нет управления моделями/HF/load-unload из UI; очередь и лимиты — на движке.

### Вариант B — наш воркер `sdworker` (супервизор sd-server) — **рекомендуется как целевой**

Go-бинарник `cmd/sdworker` по образцу `cmd/cppworker` — **отдельный процесс на своём порту (18093) со своим каталогом моделей**: spawn `sd-server` с флагами модели, ожидание `GET /sdcpp/v1/capabilities`, keep-warm, idle kill, прокси наружу, HF-bundle-загрузка, метрики и авторегистрация в балансере.

- ➕ Ноль правок C++/CMake/Dockerfile cppworker; движок — внешний бинарь (Vulkan-сборка 37 MB).
- ➕ Переиспользуем HF, idle unload, метрики/регистрацию, профили, SSE, middleware.
- ➕ Изоляция падений (OOM движка не роняет балансер), простой kill.
- ➖ 4–6 дней.

### Вариант C — cgo-встраивание sd.cpp в cppworker

Второй cgo-пакет + CMake-таргет + новый stub-тег, правки `docker/cppworker/Dockerfile.gpu:113-150, 184-199` и `Dockerfile.stub:36-39`.

- ➕ Единый процесс, hot-swap (в отличие от серверного режима движка).
- ➖ 10–15 дней; sd.cpp использует **патченный форк ggml** — общий `ggml` с llama.cpp требует `SD_USE_UPSTREAM_GGML`, что отключает часть оптимизаций (FP8→F16, INT8 tensorwise off, FP8 GGUF отвергаются).
- ℹ️ Встраиваемость доказана (KoboldCpp собирает llama.cpp + sd.cpp + TTS.cpp в одном CMakeLists), но выигрыш для HTTP-задачи мал.
- **Не рекомендуется** для первой итерации; держим как запасной путь.

### Вариант D — чужие SD-серверы как совместимые бэкенды

AUTOMATIC1111/Forge (`/sdapi/v1/txt2img`), ComfyUI, Fooocus API. sd-server **сам** отдаёт `/sdapi/v1/*`, так что один контракт покрывает оба мира.

- ➕ +0.5–1 день к Варианту A; гибкость «принимаем любые».
- ➖ Тяжёлые (Python/torch), хуже на слабых GPU, GGUF там storage-only и блокирует offload.

### Вариант E — удалённый провайдер как fallback

Бэкенд `image_remote` (OpenAI Images API / HF Inference). ➕ спасает при занятой/слабой GPU. ➖ внешние зависимости, деньги, приватность. ~1 день, после основной интеграции.

| Критерий | A | B (**выбор**) | C | D | E |
|---|---|---|---|---|---|
| Объём | 2–3 дн | 4–6 дн | 10–15 дн | +0.5–1 дн | ~1 дн |
| Правки C++/сборки | нет | нет | много | нет | нет |
| Управление моделями/HF | нет | да | да | частично | н/д |
| Слабые GPU (offload, GGUF) | да | да | да | хуже | н/д |
| «Лаконично вписать» | ★★★ | ★★ | ★ | ★★ | ★★ |

**Вердикт (принято 2026-09-27):** делаем **A → B**; уровень C (патчи/форк upstream) не делаем — см. §11; контракт наружу — OpenAI-совместимый (порт 18079), внутри — нативный `/sdcpp/v1/*`. Вариант D — бонус за полдня (единый `/sdapi/v1/*` покрывает оба мира). E — после.

---

## 5. Рекомендуемая архитектура (детально)

### 5.1 Типы и константы

`pkg/types/backend_type.go`:
```go
const (
    BackendTypeOllama   BackendType = "ollama"
    BackendTypeLlamaCpp BackendType = "llama_cpp"
    BackendTypeImage    BackendType = "image_cpp"   // sd.cpp / sd-server
)
const (
    EngineOllamaAPI BackendEngine = "ollama_api"
    EngineLlamaCPP  BackendEngine = "llama_cpp"
    EngineImageCPP  BackendEngine = "image_cpp"
    EngineAuto      BackendEngine = "auto"
)
```
Новые `case` в: `EffectiveAPIStyle:60-72`, `ToBackendType:149-158`, `ModeBackendTypes:188-194` (image в `standard`/`replication`/`rpc_coordinator`), `ModeEngines`, `ResolveEngine:222-232`, `AllBackendTypes:252-255`, `Label` (`🖼 image.cpp`), `Emoji` (🎨), + `IsModeImage` по аналогии с `IsModeLlamaCpp:244-250`.

**Порт image-бэкенда:** это **отдельный бэкенд со своим портом** (default **18093**; 18091/18092 заняты cppworker/легаси). Вводим явное поле `Backend.ImagePort int json:"imagePort"` (`pkg/types/backend.go:95-96` рядом с `CppWorkerPort`) с fallback на `CppWorkerPort`, если `imagePort == 0` — так отдельная семантика есть, но старые конфиги/UI не ломаются. Цена: +4 точки правок (`backendRequest`, форма WebUI, дефолты `internal/config/config.go:501-532`, валидация) — приемлемо, потому что тип «полностью отдельный» и правила cppworker на него не распространяются: у него нет `/api/tags`, `/api/pull` (501), chat-шаблонов, n_ctx, reasoning/tools и KV-cache.

### 5.2 Поток запроса

Клиенты на **18079 (OpenAI-поверхность)**:

```
POST :18079 /v1/images/generations
  → openaiSurface.ServeHTTP (обёртка над тем же *Proxy)
      ├─ /api/v1/*   → management API (18081), как сегодня
      ├─ строгий OpenAI: stream только из тела запроса
      └─ Proxy.ServeHTTP
           → routeRequest (router.go:48)
               ├─ isUnsupportedOpenAIEndpoint?  ← УБРАТЬ case "/v1/images/" (router.go:214)
               └─ bt = determineRequestBackendType(r, surface=openai)
                      ← /v1/images/* | model "sd:*"  → BackendTypeImage
                      ← прочие /v1/*                  → BackendTypeLlamaCpp
           → buildRoutersForDispatch(bt)   ← ДОБАВИТЬ p.imageRouter
           → ImageRouter.Route(w, r)       ← НОВЫЙ (образец llamacpp_router.go:73-159)
               ├─ non-streaming dispatch
               ├─ selectBackend(model, allowedTypes={image_cpp})   ← общий скоринг/нагрузка/сессии
               ├─ своя очередь/лимиты (Phase 6)
               └─ image-воркер :18093 → POST /sdcpp/v1/img_gen → poll → b64_json
```

Клиенты на **18080** — без изменений: Ollama `/api/*`, `/v1/*`, существующие роутеры, очереди, сессии.

**Не делать:** не пропускать image-запрос через `proxyRequestLlamaCpp` (preflight n_ctx, AutoTune, workload tracking, `normalizeOpenAIBody`); не включать streaming-ветку; не вызывать warmup текстовой модели; не менять поведение 18080.

### 5.3 API воркера

| Endpoint | Назначение |
|---|---|
| `POST /v1/images/generations` | OpenAI-совместимый (sync-обёртка над job-API), `data[].b64_json` |
| `POST /v1/images/edits` | img2img/inpaint (multipart) — если модель поддерживает |
| `POST /sdapi/v1/txt2img` | passthrough WebUI-совместимого (для A1111-клиентов) |
| `POST /api/image/generate` | наш нативный: `{model, prompt, negativePrompt, width, height, steps, cfg, seed, sampler, scheduler, batch, outputFormat}` |
| `GET /api/image/jobs/{id}` · `POST …/cancel` | async (обёртка `/sdcpp/v1/*`); cancel честно только для `queued` |
| `GET /api/image/capabilities` | агрегированные caps + локальные image-модели |
| `GET /api/image/models` · `/load` · `/unload` · `/load/progress` | жизненный цикл (load = spawn, unload = kill) |
| `GET /api/hf/*` | HF-операции (переиспользуем 1:1) |
| `GET /health`, `/metrics` | как у cppworker |

Обязательная серверная валидация на входе: `width/height ∈ [64,4096]` и кратны 64, `batch ∈ [1,8]`, `steps ∈ [1,100]`, `cfg ∈ [0,30]`, **`seed` всегда резолвится в положительное число** (иначе overflow-баг движка).

### 5.4 Модели: «bundle» + профиль

Одна диффузионная модель — это **набор файлов**, а не один GGUF:
- **all-in-one** (`--model`) — только SD1.x/2.x/SD-Turbo/SDXL;
- **всё остальное** (SD3+, FLUX, Chroma, Qwen-Image, Z-Image, FLUX.2) — `--diffusion-model` + `--vae` + text encoder'ы (`--clip_l`, `--clip_g`, `--t5xxl`, `--llm`, `--llm_vision`) + опционально `--taesd`.

```go
// pkg/types/image_model.go
type ImageModelFile struct {
    Role      string `json:"role"`      // diffusion | vae | clip_l | clip_g | t5xxl | llm | taesd | lora | upscaler
    Repo      string `json:"repo"`      // HF repo id
    Filename  string `json:"filename"`
    SizeBytes int64  `json:"sizeBytes,omitempty"`
    Revision  string `json:"revision,omitempty"`
}

type ImageModelProfile struct {
    Name     string           `json:"name"`      // z-image-turbo-q3k
    Family   string           `json:"family"`    // sd15|sd21|sdxl|sd3|flux|flux2|chroma|qwen_image|z_image
    Files    []ImageModelFile `json:"files"`
    Defaults ImageGenDefaults `json:"defaults"`  // steps/cfg/sampler/scheduler/width/height/batch/negative
    Runtime  ImageRuntime     `json:"runtime"`   // backend-плейсмент и offload (см. 5.6)
    VramEstimateMB    int     `json:"vramEstimateMb"`
    TimeoutSec        int     `json:"timeoutSec"`        // 0 = дефолт по family
    IdleUnloadMinutes int     `json:"idleUnloadMinutes"`
    Disabled          bool    `json:"disabled"`
}
```

- Хранение: `config/image-model-profiles.json` на балансере + pull-синк в воркер (паттерн `cmd/cppworker/sync_profile.go`).
- Валидация — по образцу `validateModelProfile` (`internal/api/handlers_cppworker_profiles.go:841`).
- Дефолты по family (то, что «выставляет пользователь» пресетом):
  - `sd15`/`sd_turbo`: `steps 4–8 (turbo — 1–4)`, `cfg 1.0`, `euler`, `512×512`;
  - `sdxl`/`sdxl_turbo`: `steps 1–6`, `cfg 1.0`, `512–768`, `--vae-tiling` при ≥1024;
  - `flux`/`flux2`: `steps 4`, `cfg 1.0`, `euler`, `512–1024`, `--backend te=cpu` + `--offload-to-cpu` при <8 GB;
  - `z_image`: `steps 8`, `cfg 1.0`, `euler` + `smoothstep` (⚠️ `smoothstep` ломает текст на картинке).

### 5.5 HF-загрузка и каталог

Расширяем существующий загрузчик, не переписываем:
1. Параметризовать фильтр расширений (`internal/cppbackend/hf_downloader.go:515, 572` — сейчас жёстко `.gguf`) и снять bias «самый маленький файл» (`:639-654`) для bundle-режима.
2. `StartBundleDownload(ctx, files, bundleID)` — N файлов в `<modelsDir>/<bundleID>/`, **атомарная** регистрация после успеха всех (иначе битый bundle выглядит рабочей моделью).
3. Валидация состава (наличие VAE/TE для DiT-моделей, соответствие family) с понятной ошибкой в UI.
4. Пресеты в `config/image-model-catalog.json`.

⚠️ **Правильные репозитории (проверено):**
- Для sd.cpp — **`leejet/*`** (`leejet/Z-Image-Turbo-GGUF`, `leejet/FLUX.1-schnell-gguf` и т.п.). **`city96/*` GGUF для FLUX в sd.cpp НЕ грузятся** (`new_sd_ctx_t failed`) — это формат ComfyUI-GGUF.
- `ggerganov/*` для изображений **не существует**; `city96/stable-diffusion-xl-base-1.0-gguf`, `city96/SDXL-Turbo*-gguf`, `city96/sd-turbo-gguf` тоже **не существуют** (HF отдаёт 401/gated вместо 404 — маскирует отсутствие, проверять явно).
- SD1.5 all-in-one: `second-state/*` (Q4_0/Q5_0/Q8_0/f16; **K-квантов не существует**).
- SDXL-Turbo: единственный q8_0-файл (OlegSkutte), ~4.10 GB.
- SD3.5-Large: K-квантов нет (`city96` Q4_0/Q8_0/f16), альтернатива под sd.cpp — `stduhpf/SD3.5-Large-GGUF-mixed-sdcpp`.
- VAE для FLUX: `black-forest-labs/FLUX.1-schnell` → `ae.safetensors`.
- Text encoder: T5-XXL — **обязательно квантованный** (fp16 = 9.79 GB против Q4_K_M = 2.90 GB); Qwen3-4B для Z-Image — Q4_K_M.
- `madebyollin/taehv` на HF **gated (401)** — веса брать с GitHub.

### 5.6 Ресурсы: VRAM, offload, очередь, таймауты

Плейсмент в sd.cpp управляется не «нашими» флагами, а его собственными — профиль модели должен их хранить:

```go
type ImageRuntime struct {
    Backend       string `json:"backend"`        // "te=cpu,vae=cuda0,diffusion=cuda0" | "" (auto)
    ParamsBackend string `json:"paramsBackend"`  // "cpu" | "disk" | "diffusion=disk" — отключает --auto-fit
    MaxVRAM       string `json:"maxVram"`        // "6" | "-1" | "cuda0=6,vulkan0=2"
    OffloadToCPU  bool   `json:"offloadToCpu"`   // = --params-backend '*=cpu'
    AutoFit       string `json:"autoFit"`        // on|off
    DiffusionFA   bool   `json:"diffusionFa"`    // -600 MB (flux 768²), -1.4 GB (SD2 768²)
    VaeTiling     bool   `json:"vaeTiling"`      // + --vae-tile-size (В ПИКСЕЛЯХ изображения!)
    VaeConvDirect bool   `json:"vaeConvDirect"`
    Taesd         bool   `json:"taesd"`
    SplitMode     string `json:"splitMode"`      // layer|row
    NGpuLayers    int    `json:"nGpuLayers"`     // -ngl
}
```

**Обязательные правила (из исследования):**
- **Никогда не использовать `--vae-on-cpu` по умолчанию — штраф ~5×** (RTX 3060: 29 с → 150 с). Сначала `--vae-tiling` (для AMD/RADV это вообще обязательно: Mesa рапортует `maxMemoryAllocationSize` = 4 GiB, без тайлинга decode роняет сервер).
- `--backend te=cpu` — дешёвый способ убрать тяжёлый text encoder из VRAM (Qwen3-4B на CPU ≈ 823 мс).
- `--offload-to-cpu` + `--max-vram -1` — базовые флаги для 4–6 GB; требует ~128 MiB headroom и 512 MiB device scratch; авто-retry при OOM есть **только для decode VAE**, для encode — нет.
- Порядок действий при OOM: понизить квант → `--diffusion-fa` → `--backend te=cpu` → `--vae-tiling --vae-tile-size 512` → `--vae-conv-direct` → `--taesd` → `--offload-to-cpu` → понизить разрешение.
- **RAM + VRAM должна превышать размер GGUF** (иначе модель не загрузится вообще).
- Для AMD Polaris: `RADV_PERFTEST=nogttspill` даёт ~2× (16 → 7.3 s/it).
- Всегда явный положительный `seed`.

**Совместная работа с LLM на одной карте** — три политики (default `exclusive`):
1. `exclusive` — image-воркер владеет GPU на время генерации; текстовые слоты на этой же карте не выдаются (ресурсный лок по `host`+GPU в `slot_manager`/admission).
2. `offload` — image-воркер с `--offload-to-cpu`/`--backend te=cpu`, живёт в RAM+части VRAM, допускается совместная работа (медленнее).
3. `dedicated` — отдельная GPU под image.

**Оценка памяти.** `internal/memfit` (KV/n_ctx) неприменим. Вводим `ImageVramEstimate(profile)` = веса (файлы) + latent/VAE-буфер от `width×height` + запас; калибруем по факту (`nvidia-smi` уже читается в `cmd/cppworker/vram_detect.go:90`). Опорные официальные пики: FLUX.1-dev q8_0 **12068 MB**, q4_0/q4_k **6395 MB**, q3_k **4888 MB**, q2_k **3736 MB**; SD1.x @512²: f16 2.3 GB, q8_0 2.1 GB, q4/q5 2.0 GB (с `--diffusion-fa` — 1.9/1.6/1.5 GB). Гейт: оценка > свободной VRAM → 503 `insufficient_vram` с подсказкой.

**Очередь.** Авторитет — **наш воркер** (одна модель, один мьютекс в движке); лимиты и `queue_position` читаем/отдаём, 429 + `Retry-After` пробрасываем (`upstream_error_response.go:36` — готовый паттерн). «Свою» очередь sd-server на 64 слота как основной механизм не используем.

**Idle unload** переиспользуем (`IdleUnloadMinutes`), но помним: unload = kill субпроцесса, load = spawn + прогрев (несколько секунд, показываем прогресс).

**Таймауты.** Диффузия — секунды-минуты (см. таблицу §6). Нужны профильный `TimeoutSec`, отключение streaming-детекта, увеличенный `WriteTimeout`; отдельный first-byte timeout тут не поможет — генерация идёт одним ответом.

### 5.7 WebUI

По существующим шаблонам:
- select типа бэкенда (`webui/index.html:1625-1656`) + порт/URL воркера; бейдж 🎨 (`utils.js:93-102`, `monitor/backend-type-badges.js:41-52`);
- видимость вкладок/полей (`backend-type-filter.js:378-394, 448-458`);
- страница **Image**: prompt, negative, размер, шаги, CFG, sampler/scheduler, seed, batch; кнопка Generate; статус (`queued`/`generating` + elapsed; **без процента шагов**); галерея с PNG и скачиванием; история в localStorage;
- секция image-моделей: список bundle'ов, load/unload/delete, pull с HF и прогрессом — переиспользуем `gguf-api.js:999-1090`, `gguf-load-progress.js:44-74`, `gguf-renderer-refresh.js:364-417`;
- редактор image-профилей по образцу `cppworker-params.js:112-128`;
- i18n — **парные** ключи `en.js`/`ru.js` (паритет проверяется `internal/api/lint_css_i18n_test.go:215-250`).

---

## 6. Модели под 4–8 GB VRAM (данные исследования)

Размеры — точные (HF API, GiB). «Пик» — официально измеренный sd.cpp.

| Модель | Файлы | Размер | Пик VRAM | 4 GB | 6 GB | Замечания |
|---|---|---|---|---|---|---|
| **SD 1.5** all-in-one (`second-state`) | один | Q4_0 **1.57** · Q5_0 1.62 · Q8_0 1.76 | 512²: q8_0 **2.1 GB** (с FA 1.6) | ✔ | ✔ | K-квантов нет; база качества/веса |
| **SD-Turbo / LCM** | один | ~1.6–1.8 | ~1.5–2.1 GB | ✔ | ✔ | 1–4 шага, cfg 1.0 — самый быстрый |
| **SDXL-Turbo** | один | q8_0 **4.10** | не измерен | ⚠ 512 + offload | ✔ | единственный q8_0-файл |
| **Z-Image-Turbo** (`leejet`) + Qwen3-4B TE + `ae.safetensors` | 3 | диффузия Q3_K **3.14** / Q4_0 3.68 + TE Q4_K_M **2.50** | ~3–5 GB | ✔ (Q3_K + offload) | ✔ | официальный рецепт 4 GB: `--offload-to-cpu --diffusion-fa`, 512×1024, 8 шагов |
| **FLUX.2-klein-4B** | 3 (+VAE 336 MB + TE 2.50) | Q4_0 **2.46** · Q8_0 **4.30** | ~1.4 с/шаг (RTX 2060 6GB, 512²) | ✔ Q4_0 | ✔ | лучший «новый» кандидат под слабые карты |
| **FLUX.1-schnell** (leejet) | 4 | Q2_K ~4.0 · Q3_K_S ~5.2 · Q4_0 **6.77** | q4_0 **6395 MB** · q3_k 4888 · q2_k 3736 | ⚠ только 512/Q3 + offload | ✔ Q3/Q4 | 4 шага cfg 1.0; TE → CPU обязательно |
| **FLUX.1-dev** | 4 | q4_0 6.79 · q8_0 12.71 | q8_0 **12068 MB** | ✖ | ⚠ q2/q3 + offload | 20+ шагов, тяжелее |
| **SD3.5-Large** | 4 | Q4_0 **4.77** | — | ✖ | ⚠ | K-квантов нет; альтернатива `stduhpf/…mixed-sdcpp` q2_k_4_0 **4.69** |
| **Chroma** | 4 | Q3_K_L 4.99 · Q4_K_M **6.12** | — | ✖ | ⚠ | «4 GB без offload» противоречит размеру файла |
| **Qwen-Image 20B** | 3 | Q2_K **7.06** + TE Qwen2.5-VL-7B 4.68 | VAE OOM на 4 GB → авто-тайлинг | ✖ | ⚠ | GTX 1650 4GB: 512² ≈ 38 с после тайлинга |
| **FLUX.2-dev** | — | Q2_K **12.86** | — | ✖ | ✖ | не влезает ни в 8 GB |
| **T5-XXL** (TE для FLUX/SD3) | — | fp16 9.79 → **Q4_K_M 2.90** | — | — | — | квантовать обязательно |

**Реальные замеры (для планирования таймаутов):**

| Конфигурация | Время |
|---|---|
| RTX 3060 12GB · FLUX-schnell Q4_K_S · 512² · 4 шага | **9.6 с** CUDA / 11 с Vulkan |
| RTX 3060 12GB · Z-Image-Turbo Q8 · 688×1024 · 12 шагов | ~29 с (с `--vae-on-cpu` — **150 с**) |
| RTX 2060 6GB · FLUX.2-klein-4B Q8_0 · 512² | ~1.4 с/шаг |
| GTX 1650 4GB · Qwen-Image-2.1 Q2_K · 512² | автолтайлинг VAE (9 тайлов) → **~38 с** |
| GTX 1060 6GB · Z-Image base Q3_K_M + Qwen3-4B TE · 512×1024 · 20 шагов | **341 с** |
| RX 580 8GB Vulkan · SD1.5 DreamShaper · 512² · 50 шагов | **71.5 с** |
| RX 580 8GB Vulkan · flux-schnell q4_k · 512² · 4 шага | ~95 с (sampling 52 с); 1024² — **~14 мин** |
| 4 GB laptop + 8 GB RAM · FLUX-schnell Q3 · 1024² · 4 шага | ~50 с (вытеснение в RAM) |
| CPU-only (любая DiT) | **80 с/шаг** — практически бессмысленно; только SD1.5 |

**Стартовый набор пресетов для слабого парка:** `sd15-q8` (быстро, 512²), `sd-turbo-q8` (1–4 шага), `z-image-turbo-q3k` (4 GB-рецепт), `flux2-klein-q4` (качество при ~2.5 GiB), `flux-schnell-q3k` (6 GB). Плюс `taesd` для быстрого decode (TAESD ≈ 1.22M параметров против ~49M у VAE; Steam Deck SDXL 1024²: 360 → 120 с).

---

## 7. Фазы внедрения

### Phase 1 — MVP: OpenAI-порт + внешний image-бэкенд и маршрутизация (2–3 дня)

1. **OpenAI-порт 18079**: `LoadBalancerSettings.OpenAIPort` (`pkg/types/config.go:29-39`), env `LB_OPENAI_PORT` + дефолт/сброс (`internal/config/config.go:136-137, 320-323`), баннер (`cmd/balancer/main.go:151-154`), дефолты в отдаваемом конфиге (`internal/api/handlers_cluster.go:20-23`), третий `http.Server` (`cmd/balancer/main.go:365-500`) с тем же `*Proxy` и обёрткой `openaiSurface`.
2. **`openaiSurface`** (новый файл `internal/balancer/openai_surface.go`): allow-list `/v1/*` + `/health` + `/metrics`, форвард `/api/v1/*` на management API, понятная ошибка на Ollama-путях («используйте 18080»), **строгий `stream` из тела** (не наследовать default-true из `proxy.go:1575-1611`).
3. `pkg/types/backend_type.go` — `BackendTypeImage` + engine + все `switch`/`map` (§5.1); `Backend.ImagePort` + fallback (§5.1).
4. Три нормализатора — `case` для нового типа: `backend_type_filter.go:79-89`, `backend_type_handlers.go:194-204`, `backend_type_validation.go:136-149`.
5. `backend_state.go:54-69` — порт для `EngineImageCPP` (`ImagePort`, fallback `CppWorkerPort`).
6. `router.go` — убрать `case "/v1/images/"` (`:214`), добавить роутер в `buildRoutersForDispatch`.
7. `proxy.go:480-512` — `determineRequestBackendType(r, surface)`: `/v1/images/*` и `sd:`-модели → image; прочие `/v1/*` на OpenAI-поверхности → llama_cpp; на 18080 — как сегодня.
8. Новый `internal/balancer/image_router.go` (`ImageRouter`): `selectBackend(model, allowedTypes={image_cpp})` с **общим** скорингом/нагрузкой/сессиями, non-streaming passthrough.
9. `health.go:195-207` — probe: `GET /sdcpp/v1/capabilities` (fallback `/health`).
10. `handlers_backends.go:568-575, 620-622`, `backend_type_handlers.go:63-126`.
11. Исключить image-бэкенд из текстовых циклов: warmup/prewarm/unload/AutoPull.
12. **Маршрутизация `/sdapi/v1/*` → image-бэкенд** на 18079 и на 18080 (SillyTavern, LibreChat SD tool, Open WebUI в A1111-режиме) — см. §12.1.
13. **`GET /v1/models` на 18079** — наши image-модели + алиасы (`sd-cpp-local`, `dall-e-2`, `dall-e-3`, `gpt-image-1`) поверх существующего агрегатора `/v1/models`; иначе дропдауны клиентов (n8n фильтрует `^dall-`) пустые.
14. **CORS + `OPTIONS` на 18079** (Origin-echo, `Allow-Credentials`, `Allow-Methods/Headers: *`, `OPTIONS → 204`) — нужен для web-клиентов и кнопки Connect в SillyTavern.

**Acceptance:**
- `POST :18079/v1/images/generations` (через балансер) отдаёт валидный PNG в `data[0].b64_json` с image-бэкенда; `POST :18079/v1/chat/completions` уходит на llama.cpp/Ollama-бэкенд.
- На 18079 Ollama-нативные пути дают понятную ошибку со ссылкой на 18080; `/api/v1/*` форвардится на management API.
- Стриминг на 18079 включается **только** при `stream: true` в теле (тест на запрос без поля `stream` → единый JSON-ответ).
- **Поведение 18080 не изменилось**: существующие регрессионные тесты (`tests/backend_type_isolation_test.go`, `tests/operating_mode_dispatch_test.go`, `tests/llama_cpp_proxy_test.go`) зелёные без правок.
- При 2 LLM + 1 image бэкенде чат **никогда** не уходит на image и наоборот (100 итераций).
- Общие механизмы не дублируются: у 18079 и 18080 **один** `*Proxy` (тест: сессии/нагрузка/очередь видны с обоих портов).
- `go build -tags llama_stub ./...` зелёный; три нормализатора не превращают тип в `ollama` (unit-тесты).
- **Клиентский smoke (§12.5, минимум 5 клиентов)**: SillyTavern (источник `stable-diffusion.cpp server`) подключается к 18079, видит модели и генерирует картинку; Open WebUI в режиме `openai`; LibreChat (OpenAI tools); AnythingLLM (`localai`); OpenAI SDK/curl. Правки в клиентах не требуются.
- **Защита от ловушки seed** (openai-путь sd.cpp не читает `seed`, дефолт 42): два запроса с разным seed дают разные изображения.

### Phase 2 — image-модели: bundle + HF (3–4 дня)

1. `pkg/types/image_model.go` (§5.4).
2. `hf_downloader.go` — параметризация расширений (`:515, 572`), снятие bias (`:639-654`), `StartBundleDownload` + подкаталог + атомарная регистрация.
3. `internal/api/handlers_image_profiles.go` (образец `handlers_cppworker_profiles.go:55-220`) + маршруты в `routes.go` под `AuthMiddleware`+`RateLimitMiddleware` (см. комментарий `routes.go:292-308`).
4. `config/image-model-catalog.json` — пресеты из §6 с проверенными репозиториями (leejet/second-state/OlegSkutte/black-forest-labs/stduhpf).
5. `gguf_backend_proxy.go:31-50` — `resolveCppWorkerURL` сейчас требует `Type==LlamaCpp`; разрешить image-воркер (или зеркальный `/api/v1/image/backends/{id}/proxy/`).

**Acceptance:** из UI/curl находится репо, выбираются файлы (diffusion+vae+te), bundle качается с прогрессом, профиль создаётся; неполный bundle **не** регистрируется как модель (негативный тест); несуществующий репо даёт понятную ошибку (не 401-маскировку).

### Phase 3 — воркер `sdworker` (4–6 дней)

1. `cmd/sdworker/` — каркас (копия `cmd/cppworker/router.go:11-107`): router, middleware, `/health`, `/metrics`, SSE; дефолтный порт **18093**, отдельный каталог моделей (`IMAGE_MODELS_DIR`), конфиг/env по образцу `internal/cppbackend/config.go`.
2. Супервизор субпроцесса `sd-server`: spawn с флагами профиля (§5.6), ожидание `GET /sdcpp/v1/capabilities`, kill+spawn при смене модели, kill по idle, сбор stdout/stderr, **валидация имён флагов** (`--listen-ip`/`--listen-port` vs старые `--host`/`--port`) и **пинование версии** (`master-929-3f8527a`) + contract smoke-тест.
3. `internal/sdbackend/` — реестр моделей/состояний, single-flight load/unload, `load/progress` (SSE), active queries.
4. Прокси: `/v1/images/generations`, `/v1/images/edits`, `/sdapi/v1/txt2img`, `/api/image/*`, `/api/hf/*`; внутренний контракт — `/sdcpp/v1/img_gen` + poll + cancel(queued).
5. Авторегистрация (`backendType: "image_cpp"`, `imagePort: 18093`) — образец `cmd/cppworker/balancer_register.go:166-273`.
6. `docker/imageworker/Dockerfile` (multi-stage; Vulkan-сборка как основной лёгкий путь, CUDA — опционально) + `deployments/docker-compose.imageworker.yml` (образец `docker-compose.cppworker.yml:107-182`: тома моделей, `HF_TOKEN`, `HF_MIRROR`, авто-регистрация, healthcheck).
7. **Нормализация под клиентов (§12.4, пп. 1–6)**: спавн `sd-server` с `--seed -1` + проброс per-request seed через `<sd_cpp_extra_args>`; clamp+round `size`; clamp `steps`/`n` с корректной длиной `data[]`; `response_format: url`; проброс `output_format`/`output_compression`; A1111-заглушки (`POST /sdapi/v1/options`, `/progress`, `/interrupt`, `/sd-vae`, `/sd-modules`) **без** `forge_preset` в `GET /sdapi/v1/options`.

**Acceptance:** compose up → воркер регистрируется; модель качается с HF; load/unload/reload работают (load = spawn, unload = kill); `POST /v1/images/generations` через балансер отдаёт PNG; idle unload останавливает sd-server; OOM даёт понятную ошибку (не 500) с подсказкой из §5.6; `seed: -1` не приводит к зависанию.

### Phase 4 — discovery и «расшифровка» (2 дня)

`GET /api/v1/image/contract` (поля, лимиты, примеры, JSON-Schema инструмента `generate_image`), `GET /v1/images/capabilities` (агрегация), `GET /api/v1/image/backends` (образец `gguf_backends_handler.go:44-185`), image-модели в `/v1/models`.

**Acceptance:** клиент, прочитав contract, формирует валидный запрос без чтения кода; LLM-агент по схеме инструмента успешно генерирует картинку (режим B, client-side).

### Phase 5 — WebUI (4–5 дней)

См. §5.7. **Acceptance:** генерация из браузера, галерея, статус без фальшивого процента шагов, pull image-модели с HF из UI, редактор профилей, парные i18n-ключи, `TestI18nKeyParity_EN_RU` зелёный.

### Phase 6 — ресурсы, очередь, наблюдаемость (3–4 дня)

1. Политики `exclusive|offload|dedicated` + ресурсный лок по GPU, учёт в `slot_manager.go:91-158`.
2. `ImageVramEstimate` + гейт `insufficient_vram` (503) с подсказками из OOM-лестницы.
3. Профильные таймауты, non-streaming, проброс 429/`Retry-After`, TTL 600 с (не отдавать `410 Gone` клиенту как «ошибку» — хранить результат).
4. Метрики `images/min`, `sec/image`, `queue_depth`, `vram_peak`, `oom_total` в `/metrics`; расширение приёмника `llamacpp_metrics_poller.go:198-264`.
5. Опционально `operatingMode: image_tool_loop` (§3.3).

**Acceptance:** на одной GPU LLM и генерация не выбивают друг друга (нагрузочный тест); при нехватке VRAM — 503 с кодом; метрики видны в Monitor; 10 параллельных генераций → очередь + 429, без 500.

### Phase 7 — сборка, CI, документация (2 дня)

`docs/image-generation.md` (+ `docs/en/…`, RU/EN паритет), апдейт `backend-type-isolation.md` (третий тип), `.clinerules` §3/§6, CHANGELOG, README; тесты: unit (типы/маршрутизация/валидация) + smoke с mock sd-server через `httptest` (образец `internal/api/gguf_backend_proxy_test.go:26-90`) + отдельный GPU-workflow (roadmap 7.2).

**Итого:** MVP (Phase 1) — 2–3 дня; 1+2 — ~6 дней; целевая (1–4) — ~2 недели; полная (1–6) — ~3–4 недели на одного разработчика.

---

## 8. Чеклист точек правок

**Типы:** `pkg/types/backend_type.go` (6-9, 14-18, 60-72, 149-158, 188-203, 222-279), `pkg/types/backend.go:95-96` (новое `ImagePort` + fallback), `pkg/types/config.go:29-39` (`OpenAIPort`), новый `pkg/types/image_model.go`.

**Порты/слушатели:** `internal/config/config.go:136-137, 320-323` (env + default 18079), `cmd/balancer/main.go:151-154` (баннер), `:365-500` (третий `http.Server` + TLS-вариант `TLSPort+2`), `internal/api/handlers_cluster.go:20-23` (дефолты в отдаваемом конфиге), новый `internal/balancer/openai_surface.go`.

**Балансер:** `router.go:117-160, 207-228`; `proxy.go:480-512, 1575-1611, 1151-1198`; `backend_state.go:54-69`; новый `image_router.go`; `health.go:195-207`; `backend_type_filter.go:79-89`; `backend_selector.go:13-43`; `candidate.go`; `proxy_request.go:159-208, 930, 1045-1215`; `unload_scheduler.go:162-289`; `prewarm_controller.go:187`; `slot_manager.go:91-158`; `metrics.go:175-176`.

**API/config:** `handlers_backends.go:500-681`; `backend_type_handlers.go:63-126, 194-204`; `backend_type_validation.go:12-38, 136-149`; `config.go:501-532`; `routes.go`; `gguf_backend_proxy.go:31-50`; новые `handlers_image_*.go`; `gguf_backends_handler.go` (эталон).

**HF/воркер:** `internal/cppbackend/hf_downloader.go:515, 572, 639-654, 1097-1099`; новый `cmd/sdworker/*` + `internal/sdbackend/*`; `cmd/cppworker/balancer_register.go` (эталон); `docker/imageworker/*`; `deployments/docker-compose.imageworker.yml`.

**WebUI:** `index.html:77-112, 1625-1656, 1714-1799, 1847-1896`; `utils.js:66-102`; `backend-type-filter.js:35-160, 378-458`; `renderers.js:496-540, 1169-1351`; `app-modals.js:194-291`; `api.js` (`Api.image*`); новый `modules/image-*.js`; `monitor/backend-type-badges.js:41-52`; `i18n/en.js`, `i18n/ru.js`.

---

## 9. Риски и ловушки

| # | Риск | Митигация |
|---|---|---|
| 1 | Неизвестный тип молча становится `ollama` (3 места) | правки в трёх нормализаторах + unit-тесты |
| 2 | Image-запрос уходит в streaming/NDJSON (`Stream: true` по умолчанию) | явный non-streaming dispatch в `ImageRouter` |
| 3 | Image-запрос попадает в preflight n_ctx/AutoTune/`normalizeOpenAIBody` | не пускать через `proxyRequestLlamaCpp` |
| 4 | Warmup/unload/prewarm/AutoPull дёргают image-бэкенд | явное исключение по типу во всех четырёх циклах |
| 5 | **VRAM-конфликт LLM↔диффузия на одной GPU** | политики `exclusive/offload/dedicated` + ресурсный лок + гейт `insufficient_vram` |
| 6 | Таймауты балансера убивают долгую генерацию (до 341 с и больше) | профильный `TimeoutSec`, не-streaming, `WriteTimeout` |
| 7 | Неполный bundle (нет VAE/TE) выглядит рабочей моделью | атомарная регистрация + валидация состава |
| 8 | Двойная очередь (балансер + sd-server 64 слота) | авторитет — воркер; лимиты только читаем |
| 9 | **`seed: -1` → overflow → «empty results» + зависание** | всегда резолвить положительный seed на нашей стороне |
| 10 | **`city96` GGUF для FLUX не грузятся**; несуществующие репо маскируются 401 | каталог с проверенными репо (`leejet`), явная диагностика загрузки GGUF |
| 11 | **`--vae-on-cpu` даёт 5× штраф** | дефолт — `--vae-tiling`; `--vae-on-cpu` только как ручной override |
| 12 | **RADV (AMD) рапортует лимит 4 GiB** → decode падает/роняет сервер | `--vae-tiling` обязателен на AMD; `RADV_PERFTEST=nogttspill` |
| 13 | Флаги движка меняются (`--host`→`--listen-ip`, релизы ежедневно) | пиновать тег `master-929-3f8527a` + контракт smoke-тест |
| 14 | В движке нет прогресса шагов и отмены в полёте (409) | честный UI/контракт: `cancel_generating: false`, без процента шагов |
| 15 | TTL 600 с → `410 Gone` | забирать результат сразу, хранить у себя |
| 16 | `--offload-to-cpu` требует headroom (128 MiB + 512 MiB scratch) | учитывать в `ImageVramEstimate` |
| 17 | `TestI18nKeyParity_EN_RU` падает при односторонних ключах | парные ключи в одном коммите |
| 18 | Linux-CUDA релиза нет; Vulkan-регрессии между коммитами (до 2.3×) | Vulkan как основной лёгкий путь, CUDA — своя сборка; фиксировать версию |
| 19 | Лицензии и вес моделей (десятки GB) | показывать лицензию/размер в UI, лимит диска |
| 20 | `ValidateBackendTypeConsistency` («все бэкенды одного типа») | ослабить: текстовые + image вместе допустимы |
| 21 | **Два слушателя на одном `*Proxy`** — риск разъезда состояния, если случайно создать второй экземпляр | 18079/18080 обслуживает **один** `*Proxy`; тест «сессии/нагрузка видны с обоих портов» |
| 22 | Пользователи по ошибке шлют Ollama-запросы на 18079, а OpenAI — на 18080 | на 18079 явная ошибка с указанием порта; на 18080 поведение не меняем, но задокументировать в `docs/` |
| 23 | Клиенты на 18080 ждут старого поведения `/v1/*` (авто-стрим, трансляции) | 18080 не трогаем; строгий режим только на 18079 |
| 24 | **OpenAI-путь sd.cpp не читает `seed` (дефолт 42)** → все картинки одинаковые | `--seed -1` при спавне + проброс seed через `<sd_cpp_extra_args>`; тест «разные seed → разные картинки» |
| 25 | Клиент ждёт `data[].url` (response_format), sd.cpp отдаёт только `b64_json` | реализовать `response_format: url` в враппере |
| 26 | `width/height` не валидируются движком на HTTP-уровне | clamp 64…4096 + округление до 64 в враппере |
| 27 | У SillyTavern смена модели (`POST /sdapi/v1/set-model`) → 500; `/interrupt`, `/progress`, `/sd-vae` отсутствуют | A1111-заглушки в враппере (§12.4, п.6) |
| 28 | Open WebUI по умолчанию шлёт `steps: 50`, `IMAGE_SIZE=512x512`; n8n фильтрует модели по `^dall-` | предупредить в docs; отдавать алиасы `dall-e-*` в `/v1/models` |

---

## 10. Открытые вопросы (решить до Phase 3)

**Уже решено:** (а) image-бэкенд — полностью отдельный процесс на своём порту (18093), правила cppworker на него не распространяются; (б) на балансере появляется отдельная **OpenAI-поверхность на 18079**, 18080 остаётся универсальным прокси без изменений; (в) логика сессий/распределения/нагрузки не меняется — один `*Proxy` на оба слушателя; (г) идём по **A+B**, уровень C (патчи upstream) не делаем; (д) **свой клиент не пишем** — опираемся на готовые (§12), недостающее закрываем нормализацией.

1. **TLS на 18079:** нужен сразу (по образцу `TLSPort`/`TLSPort+1` → `+2`) или на первой итерации только HTTP?
2. **Ollama-пути на 18079:** сразу отдавать понятную ошибку (рекомендуется) или всё-таки проксировать на 18080? Задел оставляем в любом случае.
3. **Sync-обёртка vs passthrough** для `/v1/images/generations`: sync удобнее клиентам, но держит HTTP до конца генерации (до минут) — нужны профильные таймауты и обход `Stream`-детекта.
4. **Политика GPU по умолчанию:** `exclusive` (безопасно, LLM простаивает во время генерации) или `offload` (совместно, но медленно)?
5. **Server-side tool-loop** (Phase 6) — нужен ли, или достаточно режимов A/B?
6. **Стартовый каталог:** фиксируем пресеты `sd15-q8`, `sd-turbo-q8`, `z-image-turbo-q3k`, `flux2-klein-q4`, `flux-schnell-q3k`?
7. **Удалённый fallback** (OpenAI Images / HF Inference) — нужен ли в локальном кластере?
8. **Порт image-воркера:** 18093 как дефолт — устраивает? (нужен свободный и в докере, и на хосте)

---

## 11. Расширяемость движка: три уровня

Вопрос «можно ли расширить функционал» имеет три разных ответа — важно не смешивать. Детали и ссылки — `docs/research-sdcpp-lowvram-integration.md` §1–§5.

### Уровень A — уже есть в движке (конфигурация, кода не пишем)

| Возможность | Как включается |
|---|---|
| ~25 семейств моделей (SD1.x/2.x, SD-Turbo, SDXL/Turbo, SD3/3.5, FLUX.1/2, Chroma, Qwen-Image, Z-Image, Krea2, PixArt, …) | выбор файлов + флаги `--model`/`--diffusion-model`/`--vae`/`--clip_l`/`--clip_g`/`--t5xxl`/`--llm` |
| Режимы | `-M img_gen \| adetailer \| vid_gen \| upscale \| convert \| metadata` |
| img2img / inpaint / edit / reference | `--init-img`, `--mask`, `--ref-image`, `--strength`, модели Kontext/Edit |
| ControlNet (SD1.5), IP-Adapter (SD1.5/SDXL/Plus), PhotoMaker | `--control-net`, `--ip-adapter` + `--clip_vision` |
| LoRA | `--lora-model-dir` + структурированное `lora[]` в запросе; **каталог пересканируется на каждом запросе — LoRA можно докидывать без рестарта** |
| Апскейл/детализация | `--upscale-model` (ESRGAN), `-M upscale`, `POST /sdcpp/v1/upscale`, hires fix (`--hires*`), latent-режимы, ADetailer |
| Ускорение | `--diffusion-fa`, `--sage-attn` (CUDA), `--diffusion-conv-direct`, `--vae-conv-direct`, TAESD/TAEHV, cache-mode (`easycache`, `ucache`, `dbcache`, `taylorseer`, `cache-dit`, `spectrum`) |
| Полный контроль сэмплинга | 12+ сэмплеров, шедулеры, `--rng`, `--flow-shift`, `--shifted-timestep`, guidance (txt/img/distilled/SLG), `--clip-skip`, custom sigmas |
| Память и размещение | `--backend module=device`, `--params-backend`, `--max-vram`, `--auto-fit`, `--offload-to-cpu`, `--vae-tiling` (размер в пикселях), `--split-mode layer\|row`, `--mmap` |
| Мульти-GPU / распределённо | `--rpc-servers host:port` (ggml RPC; тот же пул воркеров, что у llama.cpp) |
| Метаданные/форматы | `embed_image_metadata`, png/jpeg/webp, webm/avi для видео |

Эндпоинт `/sdcpp/v1/capabilities` сообщает, что именно доступно **в этой сборке** (samplers, schedulers, loras, upscalers, limits, features) — расширяемость «уровня A» надо читать оттуда, а не хардкодить.

### Уровень B — расширяем наш враппер (основной путь)

Серверная часть у sd.cpp намеренно тонкая (всего 7 серверных опций: `--listen-ip`, `--listen-port`, `--serve-html-path`, `--color`, `-h`, `--log-level`, `-v`), поэтому **вся оркестрация — наша**. Что сюда ложится:

- **Каталог моделей и bundle'ы** (несколько файлов = одна модель), HF-pull с прогрессом, локальный инвентарь;
- **Профили моделей**: дефолты генерации + runtime-флаги (плейсмент, offload, tiling, FA, TAESD) — пользователь настраивает через WebUI/API;
- **Очередь и приоритеты** (своя, как у cppworker), лимиты, 429/Retry-After, батч `n>1` как fan-out;
- **Конвейеры**: txt2img → `-M adetailer` → `-M upscale` как цепочка job'ов (режимы изолированы, второй проход — либо повторный вызов, либо второй процесс);
- **Промпт-инжиниринг**: расширение/перевод промпта через LLM-бэкенд балансера (chat уже есть) — «улучши промпт» перед генерацией, стилевые пресеты;
- **Интеграции**: tool `generate_image` для LLM, галерея, метаданные/watermark, метрики (`images/min`, `sec/image`, VRAM peak);
- **Мульти-модельность**: несколько image-бэкендов с разными моделями → балансер выбирает по `model` (маршрутизация по явному признаку уже есть).

### Уровень C — патч/форк upstream (лицензия MIT это позволяет)

Примитивы в C-API (`stable-diffusion.h`) **уже есть**, но в сервер не проброшены. Что имеет смысл:

| Патч | Что даёт | Цена |
|---|---|---|
| Проброс `sd_progress_cb_t` в job-статус | прогресс шагов и превью в UI | малый патч, но нужен свой форк |
| Проброс `sd_cancel_generation()` для `generating` | настоящая отмена в полёте (сейчас 409) | малый патч |
| Hot-swap модели / переиспользование контекста | быстрая смена модели без рестарта | существенно: `sd_ctx` один на процесс |
| Параллельные генерации на одной GPU | throughput | существенно: единый `sd_ctx_mutex` |
| Новые архитектуры/сэмплеры/шедулеры | свои модели | большой объём, требует знания ggml |

⚠️ Цены и риски уровня C: sd.cpp использует **патченный форк ggml**, релизы выходят по нескольку раз в день, Vulkan-регрессии между коммитами достигают 2.3×. Поэтому: держать патчи **минимальными**, пиновать тег, вести contract smoke-тест и по возможности отправлять изменения в upstream PR, а не жить в форке. `--upstream-ggml`-режим (`SD_USE_UPSTREAM_GGML`) для общей сборки с llama.cpp отключает часть оптимизаций — для нас это довод за субпроцесс.

**Вывод:** расширяемость закрывается уровнями A и B почти полностью; уровень C — только два точечных патча (прогресс и отмена), и лишь если они действительно нужны продукту.

**Решение (2026-09-27):** уровень C **не делаем**. Пока идём на A+B и не обещаем в контракте `cancel_generating` и процент шагов. Возврат к C — только если по итогам эксплуатации A+B выяснится, что mid-flight cancel или прогресс критичны для UX (тогда это отдельная задача с приоритетом выше «своего клиента»).

---

## 12. Клиенты и совместимость (проверено по исходникам клиентов и sd.cpp)

### 12.1 Главный вывод

**Свой клиент не пишем.** Готовые клиенты покрывают практически всё: OpenAI-поверхность (18079) закрывает Open WebUI, LobeChat, Cherry Studio, AnythingLLM, LibreChat (OpenAI tools), n8n и OpenAI SDK; A1111-совместимый путь закрывает SillyTavern, LibreChat (SD tool), Open WebUI (A1111-режим) и KoboldLite. Не закрываются ничем: **Home Assistant** (работает только с официальным Responses API, `base_url` не поддерживает) и **Jan** (генерации изображений нет как функции).

Отсюда уточнение к §3.0: на 18079 нужно маршрутизировать **и `/v1/*`, и `/sdapi/v1/*`** → image-бэкенд. A1111-путь — не «бонус» (Вариант D), а обязательный: без него отваливается SillyTavern, у которой есть **родной источник «stable-diffusion.cpp server»** (`sd_sdcpp_url`, по умолчанию `http://127.0.0.1:1234`).

### 12.2 Матрица клиентов

| Клиент | Движки | Куда настраивается | Шлёт `model` | `response_format` | Показывает b64 |
|---|---|---|---|---|---|
| **SillyTavern** | **родной `sdcpp`**, `auto` (A1111) | UI: `stable-diffusion.cpp URL` / `SD Web UI URL` | да (`sd-cpp-local`) | нет | да (`images[]`) |
| **Open WebUI** | openai / automatic1111 / comfyui / gemini | env `IMAGES_OPENAI_API_BASE_URL` (обязан включать `/v1`), `AUTOMATIC1111_BASE_URL` | да (`IMAGE_GENERATION_MODEL`, дефолт `dall-e-2`) | да, `b64_json` (кроме `^gpt-image`) | да (url → fallback b64) |
| **LibreChat** | OpenAI tools; SD tool (A1111) | `IMAGE_GEN_OAI_BASEURL`; `SD_WEBUI_URL` | да / нет | нет | да |
| **AnythingLLM** | провайдер **`localai`** (не `openai` — там baseURL не задаётся) | `IMAGE_GEN_LOCALAI_BASE_PATH` (включая `/v1`) | да | нет | да |
| **LobeChat** | OpenAI Images / ComfyUI | `OPENAI_PROXY_URL` + модель типа `image` | да | только если id содержит `dall-e` | да |
| **Cherry Studio** | OpenAI Images | тип `openai-image-generation` + отдельное поле «Image Generation Base URL» | да | `b64_json` (кроме `gpt-image-*`) | да |
| **n8n** | OpenAI Images (legacy `openAi` + LangChain) | credential URL | да | да, `b64_json` по умолчанию | да |
| **OpenAI SDK** (Py/Node) | OpenAI Images | `base_url`/`baseURL` (обязан кончаться на `/v1`) | да (опц.) | только если задан явно | да (`img.url is None`) |
| **KoboldCpp** | это сервер, сам отдаёт A1111 + OpenAI-ветку | — | — | — | — |
| **Home Assistant** | 🔴 только Responses API | — | — | — | — |
| **Jan** | 🔴 генерации нет | — | — | — | — |

Ключевое: `sd-server` **молча игнорирует неизвестные JSON-поля** (`model`, `response_format`, `quality`, `style`, `user`, `background`) — 400 не будет; `size: "auto"` (дефолт LibreChat и LobeChat) тоже безопасен, размеры остаются дефолтными. То есть большинство клиентов заработает «как есть» уже на Варианте A.

### 12.3 Ловушки движка, критичные для совместимости

| # | Ловушка | Следствие | Что делаем |
|---|---|---|---|
| 1 | **OpenAI-путь вообще не читает `seed`**: `gen_params` из `default_gen_params`, где `seed = 42`; рандом включается только при `seed < 0` | через `/v1/images/generations` **все картинки одинаковые** | спавнить `sd-server` с `--seed -1`; per-request `seed` пробрасывать через `<sd_cpp_extra_args>{"seed":N}</sd_cpp_extra_args>` (A1111-путь уже рандомный по умолчанию) |
| 2 | `data[].url` не отдаётся никогда; `response_format` не поддержан | клиенты, читающие только `url`, получат пустоту | реализовать `response_format: "url"` в враппере (сохранить PNG + отдать наш URL) |
| 3 | Ошибки в форме `{"error":"строка"}`, а не OpenAI-конверт | часть SDK не распарсит | отдавать `{"error":{"message","type","code"}}` |
| 4 | CORS/OPTIONS есть у sd-server, но клиенты бьют в 18079 | кнопка «Connect» в ST и web-клиенты | CORS+OPTIONS в нашей поверхности; `OPTIONS /v1/images/generations` обязателен для ST |
| 5 | `width/height` на HTTP-уровне **не валидируются вообще** | мусорные размеры уходят в модель → OOM | clamp 64…4096 + округление до кратного 64 |
| 6 | `batch_count` молча клампится до 8, `steps` до 100 | рассинхрон длины `data[]` | зажать самим и вернуть ровно столько элементов, сколько сгенерировано |
| 7 | Нет `POST /sdapi/v1/options`, `GET /sdapi/v1/progress`, `POST /sdapi/v1/interrupt`, `GET /sdapi/v1/sd-vae|/sd-modules` | у ST смена модели → 500; Open WebUI — не фатально (try/except) | достроить заглушки (см. §12.4) |
| 8 | Список сэмплеров в ST для `sdcpp` захардкожен, часть имён не маппится | молчаливый откат на дефолтный сэмплер | задокументировать; при желании — алиасы в враппере |
| 9 | Open WebUI по умолчанию шлёт `steps: 50` (`IMAGE_STEPS=50`), `IMAGE_SIZE=512x512` | на слабых GPU это минуты | предупредить в `docs/` + дефолты профиля; `IMAGE_STEPS` задаётся клиентом |
| 10 | n8n-нода фильтрует модели по `id.startsWith('dall-')` | наш `sd-cpp-local` не появится в дропдауне | отдавать алиасы `dall-e-2`, `dall-e-3`, `gpt-image-1` в `/v1/models` |
| 11 | ST переключается на Forge-ветку при наличии `forge_preset` в `GET /sdapi/v1/options` | лишние вызовы несуществующих ручек | **не** добавлять `forge_preset` |

### 12.4 Что добавляем в наш слой (нормализация)

**В `sdworker` (image-воркер):**
1. **seed**: `--seed -1` при спавне + проброс per-request seed через `<sd_cpp_extra_args>`;
2. **`size`**: принять `"auto"`/`""`/`WxH`, зажать 64…4096, округлить до 64;
3. **`steps`**: зажать 1…100; **`n`**: зажать 1…8 и вернуть ровно `len(data)`;
4. **`response_format`**: `url` → сохранить PNG и отдать наш абсолютный URL; `b64_json`/отсутствие — как сейчас;
5. **пробрасывать** `output_format` (`png|jpeg|webp`) и `output_compression` (0…100); **проглатывать** `quality`, `style`, `user`, `background`, `moderation`;
6. **A1111-заглушки**: `POST /sdapi/v1/options` (принять `sd_model_checkpoint`, 200), `GET /sdapi/v1/progress` → `{"progress":0,"state":{"job_count":0}}`, `POST /sdapi/v1/interrupt` → 204, `GET /sdapi/v1/sd-vae`/`sd-modules` → `[]` (без `forge_preset`).

**В `openai_surface` балансера (18079):**
7. **`GET /v1/models`** — отдавать наши image-модели + алиасы (`sd-cpp-local`, `dall-e-2`, `dall-e-3`, `gpt-image-1`, `flux…`), чтобы дропдауны клиентов наполнялись;
8. **маршрутизация `/sdapi/v1/*`** → image-бэкенд (SillyTavern, LibreChat SD, Open WebUI A1111);
9. **CORS + `OPTIONS`** (Origin-echo, `Allow-Credentials`, `Allow-Methods/Headers: *`, `OPTIONS → 204`);
10. **OpenAI-конверт ошибок** и гарантированные `created` + `data[]` в успешном ответе.

### 12.5 Программа проверки на клиентах (smoke приёмки)

| Клиент | Как подключаем | Что проверяем |
|---|---|---|
| **SillyTavern** | источник `stable-diffusion.cpp server`, URL → `18093` (напрямую) и → `18079` (через балансер) | кнопка Connect (OPTIONS), список моделей (`GET /v1/models`), генерация через `/sdapi/v1/txt2img`, **разные seed → разные картинки** |
| **Open WebUI** | `IMAGE_GENERATION_ENGINE=openai` + `IMAGES_OPENAI_API_BASE_URL=http://lb:18079/v1`; и `=automatic1111` + `AUTOMATIC1111_BASE_URL=http://lb:18079` | картинка в чате, отсутствие фатальных ошибок на `POST /sdapi/v1/options` |
| **LibreChat** | `IMAGE_GEN_OAI_BASEURL=http://lb:18079/v1`; и `SD_WEBUI_URL=http://lb:18079` | генерация через agent-tool, парсинг `info` (SD-путь) |
| **AnythingLLM** | `IMAGE_GEN_PROVIDER=localai`, `IMAGE_GEN_LOCALAI_BASE_PATH=http://lb:18079/v1` | картинка без `response_format` |
| **n8n** | credential URL → `18079/v1` (legacy openAi node и LangChain node) | `b64_json` → binary; фильтр `dall-` не ломает |
| **OpenAI SDK / curl** | `OPENAI_BASE_URL=http://lb:18079/v1` | b64-only ответ, `response_format: url` (после п.4) |

**Acceptance §12:** минимум **5 клиентов** из таблицы генерируют картинку **без правок клиента**; для ST подтверждено, что два запроса с разным seed дают разные изображения (защита от ловушки №1).
