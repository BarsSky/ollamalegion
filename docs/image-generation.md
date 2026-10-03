# Генерация изображений в OllamaLegion (image-бэкенд)

> **Версия:** 1.0 (2026-10-02)
> **Статус:** Phase 1 реализована (типы, OpenAI-порт, маршрутизация, клиентский smoke)
> **Движок:** `stable-diffusion.cpp` (`sd-server`), контракт написан под `master-929-3f8527a`
> **Связанные документы:** [`backend-type-isolation.md`](backend-type-isolation.md), [`../plans/2026-09-27-image-generation-backend-plan.md`](../plans/2026-09-27-image-generation-backend-plan.md), [`../docs/research-sdcpp-lowvram-integration.md`](research-sdcpp-lowvram-integration.md)

## 1. Что это

OllamaLegion умеет обслуживать **два разных класса запросов** на разных портах:

| Порт | Назначение | Кто ходит |
|---|---|---|
| **18080** | универсальный прокси (как раньше): Ollama `/api/*`, `/v1/*`, трансляции | OpenWebUI, CLI, Ollama-клиенты |
| **18079** | **OpenAI-поверхность**: `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`, `/v1/images/*`, `/sdapi/v1/*` | OpenAI-совместимые клиенты и генераторы картинок |
| 18081 | управляющий API (`/api/v1/*`) | WebUI, скрипты |
| 18093 | image-воркер (`sdworker` + `sd-server`) — отдельный процесс | балансер |

Отличия OpenAI-поверхности (18079):

- стриминг включается **только** явным полем `stream` в теле запроса (на 18080 сохранился legacy-дефолт `stream: true`);
- Ollama-нативные пути (`/api/chat`, `/api/tags`, `/api/pull`…) здесь **не обслуживаются** — в ответ приходит ошибка со ссылкой на 18080;
- запросы генерации изображений (`/v1/images/*`, `/sdapi/v1/*`, префикс модели `sd:`) уходят **только** на бэкенды типа `image_cpp`;
- текстовые запросы (`/v1/chat/completions` и т.п.) — только на `llama_cpp`/Ollama.

Состояние (сессии, очередь, скоринг, метрики) у 18079 и 18080 **общее**: это один и тот же `*Proxy`, просто две поверхности.

## 2. Регистрация image-бэкенда

```bash
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -H "X-API-Token: $LB_TOKEN" \
  -d '{"id":"image-1","name":"image worker","host":"192.0.2.30","imagePort":18093,"backendType":"image_cpp"}'
```

- `imagePort` по умолчанию **18093** (18091/18092 заняты cppworker'ами); если поле не задано — берётся `cppWorkerPort`, иначе 18093.
- Health-probe балансера для image-бэкенда — `GET /sdcpp/v1/capabilities` (у `sd-server` нет `/health`).
- Тип `image_cpp` допустим в режимах `standard`, `replication`, `rpc_coordinator`. В `virtual_router`/`distributed_inference` — нет (эти режимы жёстко llama.cpp).

## 3. Как подключать клиентов

### 3.1 SillyTavern (источник «stable-diffusion.cpp server»)

SillyTavern умеет общаться с `sd-server` **нативно** — отдельного адаптера не нужно.

1. Extensions → Image Generation → Source: **stable-diffusion.cpp server**.
2. URL: `http://<балансер>:18079` (через балансер) или `http://<воркер>:18093` (напрямую).
3. Кнопка **Connect** дергает `OPTIONS /v1/images/generations` — балансер отвечает `204`.
4. Список моделей берётся из `GET /v1/models` (в нём есть `sd-cpp-local`).
5. Генерация идёт в `POST /sdapi/v1/txt2img`.

Ограничения движка, о которых стоит знать: смена модели через `POST /sdapi/v1/set-model` вернёт 500 (у `sd-server` нет ручки смены модели — модель выбирается при старте процесса); часть сэмплеров из списка SillyTavern не маппится в sd.cpp и молча заменяется дефолтным.

### 3.2 Open WebUI

Оба режима поддерживаются:

```env
ENABLE_IMAGE_GENERATION=true
# Вариант A: OpenAI Images
IMAGE_GENERATION_ENGINE=openai
IMAGES_OPENAI_API_BASE_URL=http://<балансер>:18079/v1
IMAGES_OPENAI_API_KEY=any
IMAGE_GENERATION_MODEL=dall-e-2      # любое из /v1/models
IMAGE_SIZE=512x512                   # по умолчанию 512x512
IMAGE_STEPS=20                       # ВНИМАНИЕ: дефолт Open WebUI — 50 (на CPU это минуты)

# Вариант B: A1111
# IMAGE_GENERATION_ENGINE=automatic1111
# AUTOMATIC1111_BASE_URL=http://<балансер>:18079
```

`IMAGES_OPENAI_API_BASE_URL` **обязан** включать `/v1`.

### 3.3 LibreChat

```env
# A1111-путь (Stable Diffusion tool)
SD_WEBUI_URL=http://<балансер>:18079

# OpenAI Images (image tools агента)
IMAGE_GEN_OAI_BASEURL=http://<балансер>:18079/v1
IMAGE_GEN_OAI_API_KEY=any
IMAGE_GEN_OAI_MODEL=dall-e-2
```

### 3.4 AnythingLLM

Провайдер `openai` жёстко ходит на api.openai.com, поэтому используйте **`localai`**:

```env
IMAGE_GEN_PROVIDER=localai
IMAGE_GEN_LOCALAI_BASE_PATH=http://<балансер>:18079/v1
IMAGE_GEN_LOCALAI_API_KEY=any
IMAGE_GEN_MODEL_PREF=sd-cpp-local
IMAGE_GEN_SIZE_PREF=512x512
```

### 3.5 n8n

Credential типа OpenAI с `url = http://<балансер>:18079/v1`. Работают обе ноды: legacy `OpenAI (image → create)` и LangChain `Image → Generate an Image`. Дропдаун моделей фильтрует id по префиксу `dall-`, поэтому балансер специально отдаёт алиасы `dall-e-2`/`dall-e-3`.

### 3.6 OpenAI SDK

```python
from openai import OpenAI
client = OpenAI(base_url="http://<балансер>:18079/v1", api_key="any")
r = client.images.generate(model="dall-e-2", prompt="a cat", size="512x512", n=1)
print(len(r.data[0].b64_json))   # url будет None: sd.cpp отдаёт только b64_json
```

### 3.7 Кому не подходит

- **Home Assistant** — работает только с официальным OpenAI Responses API, `base_url` не поддерживается.
- **Jan** — генерации изображений нет как функции.
- **ComfyUI-транспорт** в LobeChat/Cherry Studio — ожидает именно ComfyUI.

## 4. Что нужно знать про запросы (нормализация)

| Что | Как обрабатывается |
|---|---|
| `model` | принимается любой и **игнорируется** движком (у `sd-server` одна модель на процесс) |
| `size` | `"auto"`/`""`/`"WxH"`; зажимается в 64…4096 и округляется до кратного 64 |
| `n` / `batch_size` | 1…8 (движок молча клампит); в ответе ровно столько элементов `data[]` |
| `steps` | 1…100 |
| `seed` | если не задан — резолвится в случайное положительное число. **Это обязательная нормализация:** OpenAI-хендлер sd.cpp не читает `seed` вообще и по умолчанию берёт 42, то есть без неё все картинки были бы одинаковыми |
| `response_format` | `b64_json` и отсутствие — b64; `url` — сохраняем PNG и отдаём ссылку (sd.cpp умеет только b64) |
| `output_format`, `output_compression` | пробрасываются (`png`/`jpeg`/`webp`, 0…100) |
| входные изображения (init/mask) | PNG и JPEG проходят как есть; **WebP декодируется и перекодируется в PNG** (контракт движка — PNG/JPEG). Если webp не декодировался, ответ внятно просит PNG, а маска в движок не отправляется вовсе |
| `quality`, `style`, `user`, `background` | проглатываются |
| ошибки | в OpenAI-конверте `{"error":{"message","type","code"}}` (sd.cpp отдаёт `{"error":"строка"}`) |

Отмена: доступна только для задач в статусе `queued`; генерацию в полёте `sd-server` прервать не может (409) — это ограничение движка, а не балансера. Прогресса по шагам движок тоже не отдаёт.

## 5. Гейт VRAM и сосуществование с текстовым инференсом

Диффузионная модель и текстовая LLM на одной карте в VRAM обычно не сосуществуют:
FLUX Q4 — пики 3.7–6.4 GB, Q8 — до 12 GB, а рядом живут веса LLM и её KV-cache.
Поэтому балансер умеет (1) проверять, влезает ли image-модель, и (2) разводить
два класса нагрузки по времени.

Конфиг — секция `balancing.image`:

```json
{
  "balancing": {
    "image": {
      "coexistence": "exclusive",
      "vramHeadroomMb": 512,
      "blockOnUnknownVramEstimate": false,
      "queueWaitTimeoutSec": 30,
      "exclusiveLockTimeoutSec": 600,
      "gateDisabled": false
    }
  }
}
```

| Поле | Смысл |
|---|---|
| `coexistence` | `exclusive` (по умолчанию) — на время генерации image-бэкенд владеет картой, текстовые слоты на том же хосте не выдаются; `offload` — генерация с offload в RAM, работаем совместно; `dedicated` — под image отдельная GPU, ограничения не нужны |
| `vramHeadroomMb` | сколько VRAM оставить свободной сверх оценки модели |
| `blockOnUnknownVramEstimate` | `false` (по умолчанию) — если оценить потребность не удалось, генерация **разрешается** (в лог идёт WARN); `true` — строгий режим, отказ `unknown_vram_estimate` |
| `queueWaitTimeoutSec` | сколько ждать освобождения GPU в `exclusive`, прежде чем ответить `429 image_gpu_busy` + `Retry-After` |
| `exclusiveLockTimeoutSec` | предохранитель: лок снимается принудительно (с WARN), чтобы зависшая генерация не блокировала карту навсегда |
| `gateDisabled` | полностью выключить гейт и лок (например, image работает на CPU) |

Как считается оценка (по приоритету): профиль модели (`vramEstimateMb` в
`image-model-profiles.json`) → `vram_estimate_mb` от воркера → размеры файлов
bundle'а × 1.15 → «неизвестно». Свободная VRAM: данные воркера → снимок
`nvidia-smi` из его `/api/image/capabilities` → `available_vram_mb` текстового
соседа того же хоста.

Гейт блокирует **ровно один осмысленный случай**: оценка известна, свободная
VRAM известна, и модель в неё не влезает. Ответ — `503` с кодом
`insufficient_vram`, текстом и **hint по лестнице снижения памяти**:
понизить квант → `--diffusion-fa` → текст-энкодер на CPU (`--backend te=cpu`) →
`--vae-tiling` → `--vae-conv-direct` → `--taesd` → `--offload-to-cpu` → меньше
разрешение.

**Что именно сравнивается с свободной VRAM (важно).** Когда модель уже загружена
(а гейт работает именно в этот момент — веса резидентны), с `free` сравнивается
**рабочий набор генерации** (20% веса, но не меньше 256 MB и не больше 1 GB),
а не полный вес модели: полный вес занят самой моделью, и требовать его повторно
означало бы отказывать в генерации на любой карте, где после загрузки модели
осталось меньше её веса. Вопрос «влезут ли веса» решается на этапе загрузки
модели. Это исправлено после живого прогона: SD1.5 Q4 (2.6 GB) при свободных
895 MB получала `503` на каждый запрос, хотя движок был готов.

Лок работает по хосту бэкенда, а если у бэкенда **известен** `gpuIndex` — по паре «хост + карта». Это важно на multi-GPU машине: занятая генерацией карта больше не блокирует текстовый трафик, идущий на другую карту того же хоста. Если индекс известен только у одной стороны, конфликт считается по всему хосту (консервативно).

`gpuIndex` — поле в карточке бэкенда, и у него **три состояния**, а не два (одного `int` для этого не хватает: `0` — валидный индекс первой карты, и в `int` он неотличим от «не задано»):

| Значение поля | Смысл | Ключ лока |
|---|---|---|
| поля нет / `null` | индекс **неизвестен**: какой картой пользуется бэкенд, не знаем | `host` (весь хост) |
| `0` | **явно первая карта** | `host#gpu0` |
| `N > 0` | явно карта N | `host#gpuN` |

- `PUT /api/v1/backends/{id}` различает все три случая: ключа в теле нет → «не менять» (частичный PUT из WebUI не стирает настройку), `"gpuIndex": null` → **сброс** в «неизвестно», `"gpuIndex": 0` → явная первая карта. Отрицательный индекс → `400`.
- `POST /api/v1/backends` принимает то же поле (в том числе `0`); отсутствие ключа = «неизвестно».
- `GET /api/v1/backends` отдаёт `gpuIndex` **числом**, когда индекс задан явно, и **не отдаёт поля**, когда индекс неизвестен, — разницу обязан видеть потребитель.
- `gpuLockHeldFor` (ждёт ли текстовый запрос генерацию) использует то же правило: неизвестный индекс у любой из сторон = лок на весь хост.
- Для llama.cpp при отсутствии явного `gpuIndex` индекс берётся из `cppWorkerConfig.mainGpu`, но **только при `mainGpu > 0`**: ноль там означает «авто/не задано», и трактовать его как «явно карта 0» значило бы сузить лок по догадке.

Ожидание текстового запроса, заблокированного генерацией, ограничено `queueWaitTimeoutSec` (а не общей admission-очередью): по истечении клиент получает штатный `503` с `Retry-After`, заголовком `X-Queue-Wait-Reason: image_gpu_lock` и `waitReason` в теле.

## 6. Discovery: что спросить, чтобы не читать исходники

| Endpoint | Что отдаёт |
|---|---|
| `GET /api/v1/image/contract` | «расшифровка»: фактические порты, список endpoints, поля запроса с правилами нормализации, лимиты, доступные модели с дефолтами, curl-примеры, **JSON-Schema инструмента `generate_image`** и инструкция для LLM-агента (какой URL дёргать и что картинка приходит в `b64_json`) |
| `GET /api/v1/image/capabilities` | агрегат по всем здоровым image-бэкендам: samplers/schedulers/loras/upscalers (объединение), консервативный мёрж лимитов (`min_*` = max, `max_*` = min, очереди = сумма), модели по бэкендам, отчёт об ошибках опроса |

**Где image-бэкенд виден в общем мониторинге:** `GET /api/v1/metrics` и
`GET /api/v1/cluster` отдают у бэкенда `backendType: "image_cpp"` поле
`imagePort` (порт воркера) и блок `image` со состоянием воркера, текущей моделью,
всеми известными моделями (`models[]`: имя, состояние, family, размер, оценка
VRAM, активные запросы) и VRAM хоста. WebUI (Backends/Dashboard/Monitor/Models)
показывает эти данные, включая бейдж 🎨 image.cpp.

Оба — под `X-API-Token` (порт 18081). Кэш агрегата — 8 с, с инвалидацией при
смене состава/статуса бэкендов.

## 7. Порты и переменные окружения

| Переменная | Значение по умолчанию | Смысл |
|---|---|---|
| `LB_OPENAI_PORT` | 18079 | OpenAI-поверхность балансера; отрицательное значение — выключить слушатель |
| `SDWORKER_PORT` | 18093 | порт image-воркера |
| `SDWORKER_SD_SERVER_BIN` | `sd-server` | путь к бинарю движка |
| `SDWORKER_IMAGE_MODELS_DIR` | `models/image` | каталог image-моделей |
| `LB_ALLOW_IMAGE_TIMEOUT_SEC` | 0 (без капа) | opt-in кап на генерацию изображения |

## 8. Ограничения движка (важно для ожиданий)

### 8.1 Какие файлы видно в поиске и что реально грузится

Список файлов на табе **HuggingFace** — это НЕ «всё содержимое репозитория», а
отфильтрованный набор **весов**: воркер отдаёт только расширения
`ModelWeightExtensions = .gguf, .safetensors, .sft, .ckpt`
(`internal/cppbackend/hf_bundle.go`), поэтому `model_index.json`, README и
конфиги scheduler'ов в списке не появляются. При запуске bundle расширение
проверяется ещё раз (`validateBundleRequest`) — подсунуть произвольный файл
нельзя.

**`.safetensors` распознаётся, а не просто показывается:**

- роль файла выводится из имени серверной эвристикой
  (`internal/sdbackend/models.go` → `roleFromFilename`, она же отдаётся UI полем
  `suggestedRole`): `ae.safetensors` → `vae`, `clip_l.safetensors` → `clip_l`,
  `clip_g.safetensors` → `clip_g`, `t5xxl*.safetensors/.gguf` → `t5xxl`,
  `*taesd*` → `taesd`, остальное (включая `flux1-schnell.safetensors`) →
  `diffusion`;
- движок грузит safetensors нативно, потому что sd.cpp работает через ggml:
  all-in-one `.ckpt/.safetensors/.gguf` передаётся флагом `--model`, отдельные
  файлы — `--vae` (`ae.safetensors`), `--clip_l/--clip_g`, `--t5xxl`, `--taesd`,
  `--llm` (см. `docs/research-sdcpp-lowvram-integration.md` §«Форматы и
  раскладка файлов»);
- практическое следствие: для SD 1.5/SDXL достаточно одного all-in-one
  `.safetensors`, а для DiT-семейств (FLUX/SD3/Qwen-Image/Z-Image) нужен набор
  `diffusion` (обычно GGUF от `leejet/*`) + `ae.safetensors` + text encoders.

Оговорка: `.sft` проходит фильтр файлов (наследие общего HF-хелпера), но
engine-поддержку именно `.sft` для sd.cpp мы не проверяли — считайте его
«на свой риск» и предпочитайте `.gguf`/`.safetensors`.

### 8.2 Прочие ограничения

- **Одна модель на процесс.** Смена модели = перезапуск `sd-server` (в воркере это `load`/`unload`).
- **Генерация сериализована** одним мьютексом: параллельные запросы встают в очередь.
- **Отмена в полёте и прогресс по шагам недоступны** (примитивы в C-API есть, в сервер не проброшены).
- **`city96/*` GGUF для FLUX не грузятся** в sd.cpp (это формат ComfyUI-GGUF) — брать сборки `leejet/*`.
- **`/v1/images/variations` реализован поверх img2img** (пустой промпт + `strength` 0.5): у sd.cpp нет отдельного режима вариаций, а поведение движка на пустом промпте живьём не проверялось (в тестовом окружении нет реального sd.cpp) — если он откажется, клиент получит его ошибку как есть.
- **`--vae-on-cpu` даёт штраф ~5×** — предпочитайте `--vae-tiling`; на AMD/RADV тайлинг обязателен.
- Релизы sd.cpp выходят ежедневно, имена флагов менялись (`--host/--port` → `--listen-ip/--listen-port`) — версия пинована в `types.PinnedSDServerRevision`.

## 9. Диагностика

```bash
# здоровье image-бэкенда
curl -s http://localhost:18081/api/v1/backends | grep -i image_cpp

# возможности движка и лимиты (напрямую у воркера; агрегация на балансере — Phase 4)
curl -s http://<воркер>:18093/api/image/capabilities

# список моделей (должны быть sd-cpp-local и dall-e-*)
curl -s http://localhost:18079/v1/models

# контракт и возможности кластера (нужен X-API-Token)
curl -s -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/image/contract | head -c 400
curl -s -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/image/capabilities | head -c 400

# генерация напрямую
curl -s -X POST http://localhost:18079/v1/images/generations \
  -H 'Content-Type: application/json' \
  -d '{"model":"dall-e-2","prompt":"a cat","size":"512x512","n":1}' | head -c 200
```

Если `/v1/images/generations` отвечает `503 image_backend_unavailable` — не зарегистрирован ни один здоровый бэкенд типа `image_cpp`. Ответ `503 insufficient_vram` означает, что модель не влезает в текущую свободную VRAM — смотри поле `hint` в теле ответа.

## 10. Живой стенд для проверки

`scripts/image-e2e-smoke.ps1` собирает мок `sd-server` (`tools/mock-sdserver`), `sdworker` и балансер, поднимает их реальными процессами и прогоняет 21 проверку. Занятые порты скрипт сдвигает сам, а в `finally` гасит все процессы.

```powershell
# протокольный прогон на моке движка (секунды)
powershell -ExecutionPolicy Bypass -File scripts/image-e2e-smoke.ps1

# прогон на РЕАЛЬНОМ движке и реальной модели (Vulkan/CPU, минуты на CPU)
powershell -ExecutionPolicy Bypass -File scripts/image-e2e-smoke.ps1 -Real
```

В режиме `-Real` движок и модель скачиваются автоматически, если их нет:
`tools/fetch-sdcpp` умеет забирать релиз `stable-diffusion.cpp` с GitHub и файлы
с HuggingFace (докачка по Range, повторы, `HF_TOKEN`):

```powershell
go run ./tools/fetch-sdcpp list-release leejet/stable-diffusion.cpp win-vulkan
go run ./tools/fetch-sdcpp get-release  leejet/stable-diffusion.cpp win-vulkan .\bin\sdcpp
go run ./tools/fetch-sdcpp hf-list  second-state/stable-diffusion-v1-5-GGUF
go run ./tools/fetch-sdcpp hf-get   second-state/stable-diffusion-v1-5-GGUF stable-diffusion-v1-5-pruned-emaonly-Q4_0.gguf .\bin\hf-models
```

Что именно проверяет `-Real` (то, чего мок доказать не может): реальный `sd.cpp`
принимает наши запросы, и **картинка, полученная через балансер, — валидный PNG
нужного размера** (проверка B8; файл сохраняется в рабочий каталог стенда).
Проверки, опирающиеся на лог мока (нормализация seed, `init_image` у img2img),
в этом режиме помечаются `SKIP`.

Этот же стенд запускается в CI (job `test-self-hosted`, шаг «Image chain E2E»),
а in-process версия клиентских сценариев — на ubuntu-fallback
(`go test -run TestImageSmoke ./tests/`).

## 11. Docker

Образ image-воркера собирается из релиза `sd.cpp` (Vulkan — основной путь;
Linux-CUDA-релизов у проекта нет, CUDA подключается своим бинарём через
`docker/imageworker/vendor/`).

```bash
# bundled-full: image-воркер поднимается ПО УМОЛЧАНИЮ (без профилей)
docker compose -f deployments/docker-compose.bundled-full.yml up -d --build

# stack: image-воркер включается профилем worker|full
docker compose -f deployments/docker-compose.stack.yml --profile full up -d --build imageworker

# только image-воркер (самодостаточный файл)
docker compose -f deployments/docker-compose.imageworker.yml --profile vulkan up -d

# выключить
docker compose -f deployments/docker-compose.stack.yml --profile full rm -sf imageworker
```

Балансер публикует OpenAI-поверхность (`18079`) — именно на неё шлют запросы
клиенты: `POST http://<хост>:18079/v1/images/generations`. Проверить, что
бэкенд зарегистрировался как image-бэкенд:

```bash
curl -H "X-API-Token: $TOKEN" http://<хост>:18081/api/v1/image/backends
```

Для NVIDIA нужен nvidia-container-runtime с `NVIDIA_DRIVER_CAPABILITIES=compute,utility,graphics`
(без `graphics` в контейнер не пробрасывается Vulkan-ICD); для AMD/Intel — проброс
`/dev/dri` и `group_add: video,render` (см. `docker-compose.imageworker.yml`).

### 11.1 Единый стенд: текст и картинки за одним балансером

Профиль `full` в `deployments/docker-compose.stack.yml` поднимает **оба** пула
бэкендов за одним балансером:

```bash
cd deployments
docker compose -f docker-compose.stack.yml --profile full up -d --build
```

| Сервис | Тип бэкенда | Порт | Роль |
|---|---|---|---|
| `loadbalancer` | — | 18080 / 18081 / **18079** | точка входа, admin API, OpenAI-поверхность картинок |
| `webui` | — | 18083 | операторский UI (в т.ч. страница «Image-бэкенды») |
| `cppworker-gpu` | `llama_cpp` | 18092 | текст |
| `imageworker` | `image_cpp` | 18093 | генерация изображений (sd.cpp) |
| `agent` | — | 18032 | метрики GPU/RAM и регистрация текстового бэкенда |

Оба воркера регистрируются в балансере САМИ (метка `auto-registered`), поэтому
разделять стенд на два балансера не нужно: маршрутизация идёт по типу запроса
(текст → `llama_cpp`, `/v1/images/*` и `/sdapi/v1/*` → `image_cpp`). Каталог
моделей общий: `${MODELS_DIR}/image/<имя>/` — bundle'ы картинок, `${MODELS_DIR}/*.gguf`
— текстовые модели.

Что учтено именно для единого стенда:

- `imageworker` ждёт **готовый** балансер (`depends_on: condition: service_healthy`),
  иначе первый POST регистрации уходил бы в закрытый порт;
- **перерегистрация с изменившимся адресом** обновляет запись вместо «409 и
  забыли»: пересозданный контейнер (новый host/порт) больше не оставляет в
  балансере мёртвую запись (см. `isReRegistrationOfAutoBackend`);
- профили image-моделей пишутся в `/app/data/image-model-profiles.json`
  (`LB_IMAGE_MODEL_PROFILES_PATH`), потому что `../config` смонтирован `:ro` —
  иначе сохранение профиля из WebUI падало бы на read-only ФС;
- смоук стенда: `powershell -File scripts\docker-stack-smoke.ps1` (проверяет
  состав профиля, оба типа бэкендов, `cluster.image`, страницу WebUI, а с
  `-Generate` — реальную генерацию и рост метрик).

```bash
# что именно видит балансер после старта
curl -s -H "X-API-Token: $TOKEN" http://<хост>:18081/api/v1/backends | jq '.backends[] | {id, type, status}'
curl -s -H "X-API-Token: $TOKEN" http://<хост>:18081/api/v1/cluster  | jq '.cluster.image.requests'
```

**Важно про сборку образов.** В `docker-compose.stack.yml` секция `build:` есть
у `loadbalancer`, `webui` и `imageworker`, поэтому `--profile full up -d --build`
пересобирает именно их; `cppworker-gpu` и `agent` берутся из образов (их код в
этой фазе не менялся, а пересборка llama.cpp занимает десятки минут). Без
`build:` у балансера и WebUI команда `--build` пересобирала бы один image-воркер,
а балансер/UI оставались бы старыми образами — ровно это выглядит как «в докере
новый бэкенд не активен» и «WebUI не видит image-бэкенд».

**Про `?v=` у WebUI.** Dockerfile подменяет токен `?v=` во всех html на
`WEBUI_VERSION` (build-arg), а nginx отдаёт js с `expires 1y`. Дефолт в compose —
`0.7.0`; при неизменном токене браузер оператора оставит СТАРЫЕ модули из кэша
даже после пересборки. Меняйте `WEBUI_VERSION` при выпуске нового фронтенда.

#### GPU внутри контейнера: проверять, а не предполагать

`nvidia-smi` в контейнере видит карту (то есть compute/utility проброшены), но
для sd.cpp нужен **Vulkan**, а ICD NVIDIA приезжает в контейнер только при
capability `graphics` и наличии Vulkan-ICD в driver-store хоста:

```bash
docker exec ol-stack-imageworker sh -c 'ls /usr/share/vulkan/icd.d/'
# нет nvidia_icd.json  -> Vulkan-NVIDIA в контейнере недоступен
docker exec ol-stack-imageworker nvidia-smi -L   # GPU проброшен (CUDA), но не Vulkan
```

Замер на живом стенде (Windows + Docker Desktop/WSL2, RTX 3070): в контейнере
512×512 / 8 шагов — **251 с** (движок уходит на CPU/software Vulkan), на том же
хосте нативный `sd-server` с Vulkan — **9–76 с**.

**Решение: CUDA-сборка sd.cpp прямо в образе.** Linux-CUDA-релиза у проекта нет,
поэтому `docker/imageworker/Dockerfile` умеет собирать движок из исходников, а
выбор варианта делается ЦЕЛЬЮ сборки:

| Цель | Движок | Когда нужна |
|---|---|---|
| `imageworker-vulkan` (дефолт) | релизный Vulkan-ассет | Linux-хосты с nvidia-ICD, AMD/Intel |
| `imageworker-cuda` | sd.cpp, собранный с CUDA (`-DSD_CUDA=ON`) | там, где Vulkan-NVIDIA в контейнер не приезжает (Windows + Docker Desktop/WSL2) |

```bash
# deployments/.env
IMAGE_WORKER_BUILD_TARGET=imageworker-cuda
IMAGE_WORKER_RUNTIME_BASE=dockerhub.timeweb.cloud/nvidia/cuda:12.2.0-runtime-ubuntu22.04
IMAGE_WORKER_CUDA_ARCH=86          # compute capability: 86 = RTX 30xx
IMAGE_WORKER_TAG=cuda12            # локальная сборка; выпуск (release-all.ps1) ставит сюда тег релиза

cd deployments
docker compose -f docker-compose.stack.yml --profile full build imageworker
docker compose -f docker-compose.stack.yml --profile full up -d
```

При выпуске тег `IMAGE_WORKER_TAG` меняет `scripts/release-all.ps1` (сервис
`imageworker`): он собирает образ через compose-цель и записывает в
`deployments/.env` релизный тег — тот же, что у балансера и WebUI. Старое имя
образа (`:cuda12`) при этом остаётся в локальном демоне как предыдущая версия,
так что откат — это вернуть прежнее значение `IMAGE_WORKER_TAG` и выполнить
`docker compose ... up -d`.

Почему именно CUDA 12.2 и ubuntu 22.04: Linux-CUDA-сборки sd.cpp не существует,
а `nvidia/cuda:12.2.0-devel/runtime-ubuntu22.04` уже есть в локальном кэше (на нём
собран текстовый `cppworker`), поэтому сборка не тянет многогигабайтный образ.
Бинарь собирается на devel-базе, работает на runtime-базе той же версии;
`SD_BUILD_SHARED_LIBS=OFF` (дефолт sd.cpp) даёт статический ggml/stable-diffusion —
в рантайме нужны только CUDA-рантайм и libstdc++/libgomp.

Альтернатива, если собирать не хочется: **нативный image-воркер на хосте + тот же
балансер** — запустить `sdworker` вне Docker с
`SDWORKER_BALANCER_URL=http://<хост>:18081`,
`SDWORKER_BALANCER_TOKEN=<токен стека>`,
`SDWORKER_ADVERTISE_HOST=host.docker.internal` и
`SDWORKER_REGISTER_DISABLE=true` у контейнерного воркера — тогда балансер
(в контейнере) проксирует картинки на нативный воркер с GPU-Vulkan.

## 12. Метрики image-запросов и управление бэкендами в UI

### 12.1 Что именно считается

Счётчики ведутся балансером по **генерации**: `POST /v1/images/*`,
`POST /sdapi/v1/txt2img|img2img`, `POST /api/image/generate`. Управляющие вызовы
(список моделей, `load`/`unload`, `capabilities`) в метрики НЕ попадают — иначе
RPS и «среднее время» показывали бы служебный трафик вместо генерации.

Исходов пять, и они означают разное:

| Статус | Что произошло |
|---|---|
| `ok` | синхронная генерация отдала картинку |
| `failed` | запрос дошёл до воркера/движка и упал (5xx, разрыв соединения) |
| `rejected` | отказал гейт балансера (нет модели, нехватка VRAM, занят лок, таймаут очереди, нет бэкенда) |
| `accepted` | асинхронная постановка (`202`), генерация ещё идёт |
| `finished` | асинхронная генерация завершилась (per-job исход балансер не отслеживает — воркер отдаёт только `active_queries`) |

### 12.2 Где смотреть

- `GET /api/v1/metrics` (нужен `X-API-Token`) → блок `image`:
  `requests` (агрегат по пулу + `recent` — общая лента) и `backends[<id>].requests`
  (счётчики конкретного воркера);
- `GET /api/v1/cluster` → `cluster.image` (тот же агрегат + общая лента) и
  `cluster.backends[<id>].image.requests` (per-backend). Именно это читает Monitor,
  поэтому панель запросов не требует второго запроса на каждом тике;
- `GET /api/v1/image/backends/{id}/models` → состояние моделей воркера.

Поля агрегата: `inFlight`, `total` (сколько запросов стартовало), `ok`,
`failed`, `rejected`, `accepted`, `finished`, `rps` (окно 60 с), `avgDurationMs`,
`p50DurationMs`, `p95DurationMs`, `lastDurationMs`, `lastRequestAt`,
`failuresByCode` (коды ошибок движка), `gateDeniedByCode` (причины отказов гейта).
Память ограничена жёстко: лента 20 записей на бэкенд и 50 общих, выборка
длительностей 512 значений.

### 12.3 UI

WebUI-часть — одна страница **«Image-модели»** (`#image-page`), устроенная как
страница «GGUF модели»: пункт навигации появляется, когда в кластере есть хотя бы
один бэкенд типа `image_cpp`, а внутри — шесть табов.

| Таб | Что делает |
|---|---|
| **Обзор** | CRUD image-бэкендов, порт воркера, индекс GPU, состояние, текущая модель, VRAM, счётчики запросов, политика сосуществования с текстом; кнопка **«Проверка бэкенда»** — если модель не загружена, сначала грузит её, затем делает 1 шаг 64×64 клиентским путём `POST /v1/images/generations`; в UI попадают только результат, время и имя модели, изображение не отображается |
| **HuggingFace** | Поиск репозиториев (запрос + фильтр `text-to-image`), список файлов с предложенными ролями, отметки и изменение роли, имя/семейство bundle, `HF token`, «Скачать bundle» |
| **Модели на диске** | Таблица bundle'ов: имя, состояние, размер, семейство, **состав по ролям**, активные запросы, оценка VRAM; действия — загрузить, выгрузить, **удалить с диска** |
| **Загруженные** | Состояние воркера и текущая модель + прогресс загрузки (стадия, время) по SSE `/api/image/models/load/progress/stream` с откатом на polling |
| **Загрузки** | Активные bundle- и одиночные загрузки, история, остаточные `.download`-файлы с очисткой, отмена |
| **Настройки** | Параметры выбранного бэкенда (вход в его карточку) и профили image-моделей (редактор `image-profiles.js`) |

Почему так:

- **Показ сгенерированных картинок из WebUI убран.** WebUI — панель настройки
  балансера и понимания состояния системы; показ результата — задача клиентов
  (OpenAI/A1111 на `:18079`), у которых есть и превью, и своя история. Форма
  генерации, «Результат» и галерея с localStorage удалены; осталась «Проверка
  бэкенда» (1 шаг 64×64, без изображения) — она отвечает на вопрос «движок жив?»,
  а не заменяет клиента.
- **Роли файлов считает сервер.** В списке файлов репозитория воркер отдаёт
  `suggestedRole` (`internal/sdbackend.SuggestRole`), UI только показывает и
  позволяет поправить. Если бы эвристика «какой файл есть VAE» жила в JS, правила
  разъехались бы с воркером и bundle собирался бы неправильно.
- **Автоотметки осторожные.** По умолчанию отмечаются `diffusion`, `vae`,
  `clip_l`, `clip_g`; `t5xxl`/`llm` (3–9 ГБ) — только вручную.
- **Удаление bundle** идёт через `POST /api/image/models/delete`: только внутри
  `ModelsDir`, загруженный bundle — `409` с подсказкой про `unload`
  (движок держит веса открытыми), после удаления реестр воркера перечитывается.
- **Monitor** остаётся вторым окном наблюдения: панель «Запросы к image-бэкендам»
  (агрегаты + лента последних запросов с путём, моделью, размером, шагами,
  длительностью и статусом), а в таблице бэкендов у `image_cpp` колонки
  Active/RPS/Avg RT заполняются из `image.requests`.
