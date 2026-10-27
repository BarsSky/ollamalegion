# R248 — Vision passthrough (mmproj) и отмена upstream при разрыве клиента в tools-пути

> **Дата:** 2026-10-27. **Основание:** итоги R247 — два пункта, вынесенные из него
> отдельным раундом: (1) в R247 vision сделан ЧЕСТНЫМ ОТКАЗОМ (415), картинки в
> модель не проводятся; (2) на стенде замечено, что отвалившийся клиент в
> буферизированном tools-пути оставляет занятый слот (`active_queries=1`), и новые
> запросы ждут до рестарта воркера.
>
> **Статус:** 🟡 В РАБОТЕ. **A** (отмена upstream) сделана и проверена живьём;
> **B0** (инвентаризация multimodal API), **B1** (признак `capabilities.vision` +
> роутинг passthrough/415) и **B2** (линковка `libmtmd` в C-мост + API загрузки
> mmproj) сделаны. Осталось: **B3** (декод и вставка image-эмбеддингов),
> **B4** (живая VLM-проверка, нужна малая VLM-модель на стенде).
> Правило: каждый под-этап закрывается тестами и зелёными
> регрессионными воротами R247 (см. §5).

---

## 0. Не ломать (регрессионные ворота)

| Проверка | Команда | Сейчас |
|---|---|---|
| Матрица инструментов | `node scripts/webui-e2e/tools-all-clients-r246.js` | 54/54 |
| Формы запросов клиентов | `node scripts/webui-e2e/clients-compat-r246.js` | 8/8 |
| Контур ожиданий | `node scripts/webui-e2e/client-expectations-r247.js` | 13 профилей + strict-loop |
| Ворота репозитория | `.\scripts\run-checks.ps1` | 4 шага, 0 провалов |

Любая правка этого раунда обязана оставить все четыре зелёными.

---

## 1. Часть A — отмена upstream при разрыве клиента в tools-пути (P0)

**Симптом (живой, R247).** Клиент получает SSE-запрос с `tools[]`, идёт
буферизированная генерация (`/v1/chat/completions`, ветка `stream + tools`).
Клиент отваливается (закрытие вкладки, перезапуск клиента, таймаут). Воркер
продолжает генерацию: `GET /api/models` показывает `active_queries: 1`,
следующий запрос ждёт освобождения слота (в нашем прогоне — до 350-460 с на кейс),
лечится только `docker compose restart cppworker-gpu`.

**Почему это важно.** Один «мёртвый» слот = простой стенда для всех клиентов;
на 8 ГБ и n_parallel=1 это ровно один пользователь.

**Что проверить (диагностика до правок).**

1. Где теряется контекст запроса в `/v1` tools-ветке: `handleV1ChatCompletions`
   → `generateWithRamFallback(actualModel, prompt, params, hasTools)` — принимает
   ли он `context.Context` от `r.Context()` или контекст фона; если фон — это и
   есть причина.
2. cppworker: `abort_watcher.go`, `cancel_tracking.go`, `cancel_watcher_order_r83_test.go`
   — взводится ли abort-флаг на `ctx.Done()` для НЕ-потоковой генерации; кто
   снимает `active_queries`.
3. Балансер: для не-потокового проксирования детектится ли закрытие клиента
   (сейчас polling `SetReadDeadline` известен на hijack/SSE пути —
   `proxy_request_hijack.go`, `cancel_client_r229.go`).
4. Что делает `llama_decode` при взведённом флаге: прерывается ли на следующем
   токене и освобождается ли слот в `defer`.

**Что сделать.**

- Пробросить `r.Context()` до `generateWithRamFallback` и до C-моста
  (`GenerationParams`/abort-флаг) на ОБОИХ путях: `/v1` (buffered, tools) и
  `/api/chat` (stream + tools).
- Воркер: на `ctx.Done()` — взводить abort, дождаться выхода генерации,
  декрементировать `active_queries`; в логе: `generation aborted: client
  disconnected` c `model`, `elapsed_ms`, `tokens_generated`.
- Балансер: закрытие клиента в не-потоковом пути — отменять upstream-запрос
  (как в `cancel_upstream_r184.go`), не дожидаясь конца генерации.
- Не менять поведение успешных запросов и стриминга с tools (там уже работает).

**Тесты.**

- Go (stub-мост): «медленная генерация + отмена контекста» → генерация
  завершается в пределах ~2 с, `active_queries` = 0, следующий запрос
  обслуживается; тест на оба пути.
- Go: «отмена до первого токена» и «отмена в середине» — слот освобождается в
  обоих случаях.
- Живой пробник `scripts/webui-e2e/tools-cancel-r248.js`: стартует долгий
  tools-запрос, рвёт соединение через 2-3 с, затем сразу второй запрос —
  время ожидания второго < 30 с, `active_queries` = 0; печатает оба числа.

**DoD:** `active_queries` возвращается в 0 в пределах 5 с после разрыва; второй
запрос не ждёт дольше 30 с; матрица/контур/ворота зелёные.

---

## 2. Часть B — Vision passthrough (mmproj) (P1, крупнее)

**Сейчас.** R247 отдаёт честный 415 (`vision_not_supported`) для любого запроса с
картинкой — это правильное поведение для текст-онли моделей, но у нас НЕТ пути
для моделей, которые изображения умеют. Клиенты (OpenWebUI, Continue, LibreChat,
Anthropic-клиенты) шлют `image_url`/`image` — они должны работать.

**Что проверить (инвентаризация до кода).**

1. Доступен ли в нашем сабмодуле `c/llama.cpp` multimodal-API: `mtmd`
   (`common/mtmd/*`, `tools/mtmd`) или старый `clip`; какие функции дают
   эмбеддинги изображения и замену image-токенов в промпте.
2. Есть ли в C-мосте (`c/bridge`) batch-API с эмбеддингами (`llama_batch` с
   `embd`), и как добавлять image-эмбеддинги в последовательность токенов.
3. Формат mmproj-файла и параметры загрузки (какой файл грузить рядом с моделью,
   как это делает llama.cpp server: `--mmproj`).
4. Какая маленькая VLM-модель влезает в 8 ГБ (кандидаты: Qwen2.5-VL-3B,
   gemma-3-4b-it vision, SmolVLM-500M/2B) — нужен GGUF + mmproj на стенде.

**Что сделать (по результатам инвентаризации).**

1. Capability-флаг: `pkg/types` — `Vision` у модели/профиля; cppworker отдаёт
   его в `/api/models` и `/api/ps`, балансер — в `/v1/models` (`capabilities`) и
   заголовком `X-Model-Vision: true|false`.
2. C-мост: загрузка mmproj + путь «prompt с image-эмбеддингами» (batch с
   `embd`), минимум: одно изображение на запрос; несколько — по возможности.
3. cppworker: части контента `image_url` (data URL) → декод base64 → эмбеддинги →
   вставка в промпт; ошибки — честные (`vision_not_supported`, `image_decode_failed`,
   `too_many_images`), без 200 с «не видел».
4. Балансер: в `normalizeOpenAIBody` пропускать картинки БЕЗ потери (сейчас
   текст-онли нормализация их вырезает), а `vision_r247` отказ оставить только
   для моделей без vision.
5. Anthropic/Responses поверхности: блок `image`/`input_image` → `image_url`
   (уже сделано) → passthrough у vision-модели.

**Тесты.**

- Go: capability-роутинг (vision-модель → passthrough, текст-онли → 415),
  разбор data URL (png/jpeg, base64, предел размера), отказ при битом base64.
- Живой: изображение с крупным словом («OK») → ответ модели содержит это слово
  (не «не вижу картинку»); текст-онли модель по-прежнему 415.
- Регрессия: запрос без картинок не изменился (матрица 54/54).

**DoD:** на стенде поднята VLM-модель с mmproj; запрос с картинкой возвращает
осмысленный ответ, зависящий от содержимого картинки; текст-онли модель даёт 415;
все ворота §0 зелёные.

**Ограничения заранее:** видео/аудио не поддерживаем; мультикартинки — только если
даст API; RAM/VRAM-бюджет на 8 ГБ ограничивает размер VLM.

---

## 3. Порядок и оценка

| Шаг | Зависит от | Оценка |
|---|---|---|
| A — отмена upstream (P0) | — | 0.5-1 день |
| B0 — инвентаризация mtmd/clip в сабмодуле | — | 0.5 дня (может закрыть B как «нецелесообразно») |
| B1 — capability-флаг + роутинг 415/passthrough | B0 | 0.5 дня |
| B2 — C-мост: mmproj + image-эмбеддинги | B0 | 2-4 дня |
| B3 — cppworker/балансер: декод и вставка, ошибки | B2 | 1-2 дня |
| B4 — живая проверка на малой VLM | B3 + модель | 0.5-1 день |

Порядок: **A → B0 → B1 → (B2/B3/B4 или фиксация «невозможно/нецелесообразно»)**.
A делается первой: она снимает простой стенда, который мешает любой проверке.

---

## 4. Риски

- `mtmd` может отсутствовать/быть несовместимым с нашей сборкой llama.cpp →
  B2 может превратиться в обновление сабмодуля (риск для всех моделей) или
  отказ от B с обоснованием.
- Image-эмбеддинги меняют KV/память: на 8 ГБ окно придётся уменьшать.
- Отмена upstream может «проглотить» частичный ответ там, где клиент ещё ждёт —
  правка строго по `ctx.Done()` и только для разрыва соединения.
- Рестарты воркера при проверках конкурируют с другими сессиями на стенде.

---

## 5. Журнал

| Под-этап | Статус | Подтверждение |
|---|---|---|
| A — отмена upstream в tools-пути | ✅ сделано | `generateBufferedWithCancel` (потоковый путь с abort по ctx) на всех буферизированных точках: `/v1` stream+tools, strict-повтор, `/v1` non-stream tools, `/api/chat` non-stream tools, `/v1/completions`; Go-тесты `TestR248_*`; живой `tools-cancel-r248.js`: слот освобождён через **323 мс**, следующий запрос **400 мс**, `active_queries=0`; отдельно подтверждено: убитый форграунд-прогон матрицы оставил `active_queries=0` (до правки было 1). Ворота после A: матрица **54/54**, `clients-compat` **8/8**, контур ожиданий 13/14 (единственный провал `openwebui-ollama-ndjson` — стохастика модели: одиночный повтор PASS), `run-checks` — 4 шага, 0 провалов. Образ `cppworker:gpu-r248` на стенде |
| B0 — инвентаризация multimodal API | ✅ сделано | В сабмодуле есть `tools/mtmd` (`clip.h/cpp`, `clip-model.h`, `clip-impl.h`, `clip-graph.h`); `common.h` знает `common_params_model mmproj`, `mmproj_use_gpu`, `mtmd_batch_max_tokens`. Dockerfile: `LLAMA_BUILD_COMMON=ON`, examples OFF. **В runtime-образе `libmtmd` и `clip.h` НЕТ** (`find` по контейнеру пуст, в `/app` только cppworker/rpc-server) → B2 включает: собрать `libmtmd` в стадии llama-builder, скопировать в bridge-builder, слинковать с `c/bridge`; проверить API `llama_batch` с `embd` для вставки image-эмбеддингов |
| B1 — capability + роутинг | ✅ сделано | `cppworker`: `vision_capability_r248.go` + `capabilities.vision` в `/v1/models`; `balancer`: `vision_r248.go` (кэш 30 с, опрос `/v1/models` бэкендов, `visionShouldReject`), `capabilities.vision` в своём `/v1/models`, 415 только для моделей без vision (passthrough не нормализует картинки). Тесты `vision_r248_test.go` (логика решения, парсинг capability, дефолт false). Живьём на `balancer:r248` + `cppworker:gpu-r248`: `/v1/models` отдаёт `capabilities.vision=false`, запрос с картинкой — **415 + X-Vision-Supported=false**; профили `vision-honest` и `cline-openai-stream` — PASS |
| B2 — C-мост mmproj | ✅ сделано | `libmtmd.a` слинкована в C-мост и в Go-бинарь: `Dockerfile.gpu` (`-DLLAMA_BUILD_MTMD=ON`, COPY `tools/mtmd` + `build/tools/mtmd` в bridge-builder и go-builder, `-lmtmd` в `CGO_LDFLAGS`), `Dockerfile.cpu` (`-DLLAMA_BUILD_MTMD=ON`, `-lmtmd`), `Dockerfile.gpu.86` (MTMD ON, `-lmtmd`, `-DMTMD_LIB` для bridge-tests), CI job `test-c-bridge` (собирает `mtmd`, передаёт `-DMTMD_LIB`). `c/bridge/CMakeLists.txt` находит libmtmd (без неё — `FATAL_ERROR`), добавлен include `tools/mtmd`; новый `csrc/vision_mtmd.{h,cpp}` (`bridge_mtmd_load/free/vision_supported`, C++17, только extern "C" граница) + C API `bridge_load_mmproj`/`bridge_mmproj_loaded` и Go `LoadMMProj`/`MMProjLoaded` (stub-версии возвращают явную ошибку). `bridge_free_model` освобождает проектор ДО модели. `c/bridge/tests` компилируют `vision_mtmd.cpp` и линкуют mtmd. Проверено: образ `cppworker:gpu-r248` собран, `grep -a -c mtmd_init_from_file /app/cppworker` = **2**, стенд поднят на нём: `/v1/models` → `capabilities.vision=false`, запрос с картинкой → **415 + X-Vision-Supported=false**, `clients-compat` **8/8**, Go-тесты `internal/balancer`/`cmd/cppworker`/`c/bridge` + `go vet` зелёные, новые `TestR248_Stub*` (3). CPU-образ: bridge-стадия с mtmd собралась, финальная alpine-стадия блокируется сетью стенда (`apk add` exit 255) — ограничение окружения, не правки |
| B3 — cppworker/балансер passthrough | 🟡 частично | **Мост** (коммит `444d176`): `bridge_infer_images`/`bridge_mtmd_prefill`/`bridge_mmproj_marker`, линковка `vendor-hash`. **Загрузка проектора** (коммит `d33f448`): `LoadModelOpts.MmprojPath` → `handle.LoadMMProj` после загрузки модели (ошибка проектора не отменяет загрузку: текст работает, vision выключен, причина в `/api/models.visionError`), `ModelInfo.Vision/MmprojPath/VisionError`, `/v1/models` берёт capability из факта, `mmprojPath` в `/api/models/load`, `/load-with-params`, `/api/models/reload` (reload сохраняет проектор). **Гейт публикации**: `capability.vision = MMProjLoaded() && CPPWORKER_VISION_MMPROJ=1` — проектор загружен ≠ картинки обслуживаются, поэтому дефолт выключен. **Живьём (SmolVLM-256M + его mmproj на стенде)**: `clip_model_loader: has vision encoder`, `[bridge] mmproj loaded: vision=1`, маркер `<__media__>`, загрузка 4,9 с, модель в `/v1/models` с `vision:false` (гейт). Тесты: `TestR248_VisionInferenceEnabled`, `TestR248_MmprojPath*`, `TestR248_VisionCapability*` (5 новых). **Осталось**: декод `image_url`/`images` (data URL → байты, лимиты) и вставка маркеров в промпт на позициях картинок, включение гейта по умолчанию, ответ по картинке |
| B4 — живая VLM-проверка | 🟡 подготовлена | Модель на стенде: `smolvlm/SmolVLM-256M-Instruct-Q8_0.gguf` + `mmproj-SmolVLM-256M-Instruct-Q8_0.gguf` (в `D:/ollama-legion-models`, видны в контейнере как `/app/models/smolvlm/`), грузится с проектором, vision-энкодер поднимается. Проверка ответа по картинке (слово «OK» на изображении) ждёт приёма картинок из B3 |
