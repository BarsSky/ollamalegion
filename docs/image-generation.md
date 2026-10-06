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
  `diffusion`. Учитывается и **каталог**: `vae/...` → `vae`,
  `text_encoders/qwen3vl_8b_bf16.safetensors` → `llm` (LLM-энкодер Qwen-Image), а
  `qwen-image-2.1-UC-Q4_0.gguf` в корне остаётся `diffusion` — правило про LLM
  работает только внутри каталога энкодеров;
- список файлов **рекурсивный** (`tree-API?recursive=true`): DiT-репозитории
  кладут VAE и text encoder в подкаталоги (`vae/`, `text_encoders/`,
  `split_files/`), и до 2026-10-03 такие файлы в UI не попадали — bundle DiT-модели
  было не собрать. Ответ HF постраничный, страницы обходятся по `cursor` из
  заголовка `Link` (без ухода с настроенного зеркала), файлы скачиваются по
  исходному пути, а на диск кладутся под своим базовым именем;
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

### 8.2 Как движок определяет версию модели (и откуда «get sd version from file failed»)

`general.architecture` в GGUF движок НЕ читает: версию модели он выводит по
**именам тензоров** (`ModelLoader::get_sd_version`, `src/model_loader.cpp`
пинованной `master-929-3f8527a`). А имена зависят от того, каким флагом файл
подключён (`src/pipeline/diffusion_engine.cpp`:726 и :733):

| флаг | что делает с именами тензоров | для кого |
| --- | --- | --- |
| `--diffusion-model f.gguf` | добавляет префикс `model.diffusion_model.` | DiT: FLUX/FLUX2/SD3/Qwen-Image/Z-Image/Chroma |
| `--model f.gguf` | оставляет имена как в файле | all-in-one: SD1.x/SD2.x/SDXL |

Отсюда обе ловушки, на которые мы уже наступали:

- **DiT-файл, подключённый как all-in-one.** «Голые» имена (`transformer_blocks.*`,
  `double_blocks.*`) движок в этом режиме не узнаёт → `get sd version from file
  failed` после скачивания гигабайтов. Лечится семейством профиля: для DiT-семейств
  воркер сам передаёт `--diffusion-model` (`pkg/types/image_model.go`, `IsDiTFamily`).
- **«ComfyUI-сборку sd.cpp не читает» — неверно.** Проверено на движке
  `master-929-3f8527a` синтетическими GGUF с реальными именами тензоров с HF: и
  сборка `leejet/*` (fused `img_mlp.gate_up`), и экспорт для ComfyUI (раздельные
  `img_mlp.gate_layer` + `img_mlp.proj`, `general.architecture="qwen_image21"`)
  дают `Version: Qwen Image 2.1` — движок поддерживает обе раскладки
  (`src/model/diffusion/qwen_image_2_1.hpp`).

**Пометки в UI (чтобы это не выяснялось на загрузке).** На табе «HuggingFace»
работает пред-проверка заголовка: кнопка «Проверить» у файла (и автоматически —
для самого крупного файла сразу после выбора репозитория) вызывает
`GET /api/hf/probe?modelId=&filename=&revision=`. Воркер читает Range-запросом
первые 512 КБ (`internal/cppbackend/hf_probe.go`, ни одного байта весов) и
отвечает фактами:

- `verdict=supported` + `family` + `versionLabel` — какое семейство и какую
  версию в файле узнаёт движок (например `qwen_image` / «Qwen Image 2.1»);
- `dit=true` — файл подключается как `--diffusion-model`, значит семейство
  профиля обязано быть DiT-семейством. UI подставляет его сам, если оператор не
  выбирал семейство вручную, и отдельно предупреждает при расхождении
  («в профиле `other`, движок ответит get sd version from file failed»);
- `verdict=unknown` — по заголовку не определить (VAE, text encoder, LoRA или
  незнакомое семейство). Приговоров «движок это не прочитает» пред-проверка НЕ
  выносит: 512 КБ заголовка не содержат всего, что нужно движку.

### 8.3 Прочие ограничения

- **Одна модель на процесс.** Смена модели = перезапуск `sd-server` (в воркере это `load`/`unload`).
- **Генерация сериализована** одним мьютексом: параллельные запросы встают в очередь.
- **Отмена в полёте и прогресс по шагам недоступны** (примитивы в C-API есть, в сервер не проброшены).
- **Раскладка тензоров важнее автора сборки.** `city96/*`, `unsloth/*` и прочие
  ComfyUI-ориентированные репозитории читаются, если набор тензоров совпадает с
  ожидаемым движком (см. §8.2) — проверяйте кнопкой «Проверить», а не по имени
  автора. Незнакомая раскладка (например, другая реализация того же семейства)
  даст `get sd version from file failed`.
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
| **HuggingFace** | Поиск репозиториев (запрос + фильтр `text-to-image`), список файлов с предложенными ролями, отметки и изменение роли, имя/семейство bundle, `HF token`, «Скачать bundle». У каждого файла — пред-проверка заголовка («Проверить»): какое семейство узнаёт движок и нужен ли `--diffusion-model`; семейство профиля подставляется по файлу автоматически (§8.2) |
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

### 12.4 Таб «Тест» — проверить настройки и увидеть результат

Седьмой таб страницы «Image-модели» (кнопка «Тест»), доступный **только когда в
кластере есть бэкенд `image_cpp`** — как и вся страница.

ЗАЧЕМ ОТДЕЛЬНЫЙ ТАБ: остальные табы — про настройку и состояние, показ
сгенерированного там сознательно убран (см. §12.3). Таб «Тест» — ровно наоборот:
это единственное место WebUI, где выводится картинка, и он не мешает остальным,
пока не открыт.

Что умеет:

- выбор бэкенда (`image_cpp`) и модели, кнопки «Загрузить/Выгрузить модель»,
  «Из профиля» — подставить дефолты профиля модели (steps/cfg/sampler/размер) в форму;
- выбор поверхности запроса: OpenAI `/v1/images/generations`, A1111
  `/sdapi/v1/txt2img`, нативный `/api/image/generate`; параметры — prompt,
  negative, width, height, steps, cfg, sampler, scheduler, seed, batch
  (лимиты подтягиваются из `/api/image/capabilities`);
- блок «Что уйдёт в запрос» — живой JSON и curl: видно, какие именно настройки
  применяются, а не приходится угадывать по картинке;
- результат: картинка, время, HTTP-статус, `model`, `seed`, размер; история
  прогонов **в памяти вкладки** (сравнить настройки, «В форму» вернуть параметры) —
  без localStorage и галереи;
- запрос идёт **клиентским путём** балансера, поэтому проходит VRAM-гейт и
  считается в метриках: свои проверки видно в Monitor;
- ошибки движка объясняются: «get sd version from file failed» → семейство профиля
  не совпало с файлом (DiT-модель подключена как all-in-one, см. §8.2 — семейство
  подставляется на табе HuggingFace кнопкой «Проверить»); «no image model is
  loaded» → нажмите «Загрузить модель»; OOM → уменьшите размер/шаги/batch или
  включите offload; «port still busy» → движок ещё отпускает сокет, подождите.

## 14. Инструмент `generate_image`: текстовая модель сама рисует картинки

R84 (2026-10-03). Если в кластере есть здоровый image-бэкенд и на нём **загружена**
модель, балансер объявляет текстовым моделям (llama.cpp / cppworker) инструмент
`generate_image` и **сам исполняет его вызов**: генерирует изображение, кладёт
результат в диалог как tool-сообщение и просит модель закончить ответ. Клиент
(Open WebUI, LibreChat, Cline, ваш агент) ничего настраивать не должен — он
получает текст плюс markdown-ссылку на картинку.

### 14.1 Почему исполняет балансер, а не клиент

Инструмент объявляет ПРОКСИ, а не клиент: клиент про него не знает и исполнить не
может. Поэтому вызов перехватывается на балансере. Если клиент САМ объявил
инструмент с именем `generate_image`, балансер отступает и ничего не перехватывает —
исполняет сторона клиента.

### 14.2 Гейт «есть кому исполнять» (требование оператора)

Инструмент добавляется в запрос только когда выполнены ВСЕ условия:

- есть здоровый бэкенд `image_cpp`;
- снимок его состояния достоверен (`contractOK`, без `lastErr`);
- на нём **загружена** модель (`state=loaded`) — иначе вызов упёрся бы в
  «no image model is loaded» уже после вызова инструмента.

Иначе тело запроса остаётся байт-в-байт прежним, а в ответе нет заголовка
`X-Image-Tool`. Загрузить модель можно заранее (`POST /api/v1/image/backends/{id}/models/load`)
или из WebUI (таб «Image-модели» → «Обзор» → «Загрузить модель»).

### 14.3 Как это выглядит для клиента

```
POST /v1/chat/completions            # как обычно, tools можно не передавать
{ "model": "gemma-4-E4B-it-Q4_K_M.gguf",
  "messages": [{"role":"user","content":"Нарисуй рыжего кота на подоконнике"}] }
```

- в ответе — заголовок `X-Image-Tool: generate_image` (инструмент был объявлен);
- модель вызывает инструмент → балансер генерирует картинку (VRAM-гейт, очередь) →
  в `messages` уходит `assistant(tool_calls)` + `tool(результат)`;
- клиент получает финальный текст и markdown-блок с изображением; `tool_calls` в
  ответе клиенту НЕ отдаются — исполнять их не нужно.

Ссылка на картинку: `GET /v1/images/files/{name}` — та же клиентская поверхность,
что и генерация (токен не требуется; имя файла случайное). Абсолютный адрес
собирается из `Host` запроса клиента либо из `LB_IMAGE_TOOL_BASE_URL` (нужен, когда
балансер стоит за TLS-прокси).

### 14.4 Что видно оператору

Тул-генерации идут через ту же ленту, что и обычные: в Monitor у них
`surface=chat-tool` и путь `/v1/chat/completions→generate_image`. Занимают VRAM-гейт
и очередь воркера, поэтому не мешают прямым запросам клиентов.

### 14.5 Флаги

| Флаг | По умолчанию | Смысл |
| --- | --- | --- |
| `LB_IMAGE_TOOL` | `on` | `off` — не объявлять инструмент вовсе |
| `LB_IMAGE_TOOL_MAX_CALLS` | `2` | сколько картинок на один запрос (кап 8) |
| `LB_IMAGE_TOOL_TIMEOUT_SEC` | `600` | таймаут одной генерации |
| `LB_IMAGE_TOOL_BASE_URL` | Host клиента | внешний адрес балансера для ссылок |

R85 добавил ещё два флага (каталог моделей и автозагрузка) — см. §16.3.

### 14.6 Ограничения (честно)

- turn 1 всегда нестриминговый: решение «исполнять ли вызов» принимается по целому
  ответу. Клиенту, просившему стрим, ответ отдаётся синтезированными SSE-чанками
  (cppworker и так буферизует tools-путь с holdback-окном);
- работает на поверхности `/v1/chat/completions` (llama.cpp/cppworker). Ollama
  `/api/chat` и Anthropic `/v1/messages` — не покрыты;
- n_ctx-переполнение на tools-запросе обрабатывается авто-реload'ом, но обычный
  ретрай сетевого уровня на первом turn не делается (ошибка уходит клиенту как есть);
- при `LB_IMAGE_TOOL_ALLOW_LOAD=off` инструмент объявляется только когда модель уже
  загружена; при `on` (по умолчанию, R85) — см. §16.

## 16. Каталог моделей, `list_image_models` и автозагрузка (R85)

R85 (2026-10-06). Дополняет §14: `generate_image` отвечает «нарисуй»,
`list_image_models` — «покажи, из чего выбирать», а сам вызов при необходимости
поднимает нужную модель.

### 16.1 Зачем каталог и второй инструмент

Enum параметра `model` сообщает только ИМЕНА моделей. Для осмысленного выбора нужны
семейство, требования к VRAM, размер картинки, число шагов и человеческое описание
(«для чего эта модель хороша»). Каталог — это данные, а не код: описания живут в
профиле модели (`strengths`/`notes`), правятся из WebUI и попадают и в API, и в
инструмент.

**Гейт объявления** (изменился в R85):

| `LB_IMAGE_TOOL_ALLOW_LOAD` | Что объявляется | Условие |
| --- | --- | --- |
| `on` (по умолчанию) | `generate_image` + `list_image_models` | здоровый `image_cpp` **и есть что грузить** (модели видны в снимке) |
| `off` | только `generate_image` | здоровый `image_cpp` **и модель уже в VRAM** (поведение R84) |

**Автозагрузка.** При `on` вызов `generate_image` с `model=X`, где `X` ещё не в
VRAM, поднимает модель сам: `POST /api/image/models/load` → опрос состояния до
`state=loaded` → VRAM-гейт → генерация. В tool-результат добавляются `loadSeconds`
(время загрузки) и `loadedNow`/`modelState`, чтобы модель объяснила пользователю
паузу, а не молчала. Таймаут — `LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC` (по умолчанию 600 с);
по его истечении в tool-сообщение уходит причина, а не обрыв.

**Ответ инструмента** (сжатый, читаемый моделью):

```json
{ "status": "ok", "count": 3, "hasLoadedModel": true, "loadedModel": "sd15-q8-0",
  "limits": { "minSide": 64, "maxSide": 4096, "sizeMultiple": 64, "minSteps": 1, "maxSteps": 100 },
  "models": [ { "name": "sd15-q8-0", "backendId": "imageworker", "family": "sd15",
                "state": "loaded", "loaded": true, "vramEstimateMb": 2100,
                "defaults": { "steps": 25, "width": 512, "height": 512 },
                "strengths": "эталон качества SD1.5 при малом VRAM" } ],
  "summary": "Доступные image-модели (3):\n- sd15-q8-0 | семейство sd15 | состояние: loaded (уже в VRAM) | ~2100 MB VRAM | …" }
```

GPU инструмент **не занимает**: генерации нет, VRAM-гейт не берётся, в ленте
image-запросов ничего не появляется.

### 16.2 `GET /api/v1/image/models/catalog`

Один ответ по всем живым `image_cpp`-бэкендам. Его же читает инструмент, поэтому
каталог и решение о генерации не могут разойтись: оба источника — одни и те же
снимки состояния воркеров.

```bash
curl -sS -H "X-API-Token: $LB_API_TOKEN" \
  http://localhost:28081/api/v1/image/models/catalog | jq .
```

```json
{
  "generatedAt": "2026-10-06T11:20:00Z",
  "cached": false, "cacheTtlSeconds": 5,
  "backends": [{ "id": "imageworker", "host": "127.0.0.1", "port": 18093,
                 "status": "healthy", "state": "loaded", "currentModel": "sd15-q8-0",
                 "contractOk": true, "models": ["sd15-q8-0", "flux-schnell-q3-k"] }],
  "models": [{
    "name": "sd15-q8-0", "backendId": "imageworker", "family": "sd15",
    "state": "loaded",            // loaded | loading | not_loaded | error
    "sizeBytes": 1760000000, "vramEstimateMb": 2100, "vramSource": "profile",
    "loaded": true,
    "defaults": { "steps": 25, "cfgScale": 7, "sampler": "euler_a",
                  "width": 512, "height": 512, "batchCount": 1, "seed": -1 },
    "strengths": "эталон качества SD1.5 при малом VRAM (2.1 GB): лучший выбор по умолчанию для 4-6 GB",
    "notes": "Эталон качества SD1.5 (1.76 GB) при пике 2.1 GB @512 по замерам sd.cpp…",
    "source": "profile"           // profile | catalog | worker
  }],
  "limits": { "minSide": 64, "maxSide": 4096, "sizeMultiple": 64, "minSteps": 1, "maxSteps": 100 },
  "hasLoadedModel": true
}
```

Поля:

| Поле | Источник | Смысл |
| --- | --- | --- |
| `state`, `loaded` | снимок воркера | `loaded` / `loading` / `not_loaded` / `error` |
| `sizeBytes` | снимок воркера | размер bundle'а на диске |
| `vramEstimateMb`, `vramSource` | профиль → воркер → размеры файлов | пиковая VRAM: `profile` / `catalog` / `worker` / `files` |
| `defaults` | профиль → каталог пресетов | с какими шагами/размером модель запускается |
| `strengths` | профиль → каталог пресетов → семейство | «для чего модель хороша» — ЧИТАЕТ МОДЕЛЬ |
| `notes` | профиль → каталог пресетов | подробное описание — читает оператор |
| `source` | — | откуда взяты `defaults`/описания |

**Описания — данные, а не код.** Поля `notes` и `strengths` правятся в профиле
модели (WebUI → Image-модели → профиль, `PUT /api/v1/image/model-profiles/{name}`):

```bash
curl -sS -X PUT -H "X-API-Token: $LB_API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"strengths":"быстро и дёшево: 512x512 за секунды","notes":"наш внутренний выбор"}' \
  http://localhost:28081/api/v1/image/model-profiles/sd15-q8-0
```

Приоритет источников: **профиль оператора → каталог пресетов
(`config/image-model-catalog.json`) → характеристика семейства**. Профиль
приоритетнее каталога, потому что это живое намерение оператора; каталог пресетов
намеренно уступает, чтобы правка из WebUI не «перетиралась» поставляемым файлом.
Если файла каталога на диске нет (балансер запущен не из корня репозитория),
используется вшитая в бинарь копия; если файл есть, но битый — `GET
/api/v1/image/model-catalog` отдаёт 500 с причиной, а не тихо подменяет данные.

### 16.3 Флаги автозагрузки

| Флаг | По умолчанию | Смысл |
| --- | --- | --- |
| `LB_IMAGE_TOOL_ALLOW_LOAD` | `on` | `off` — вызов не поднимает модель и `list_image_models` не объявляется (поведение R84) |
| `LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC` | `600` | сколько ждать загрузку модели, после чего в tool-сообщение уходит причина |

### 16.4 Сценарий «пользователь просит картинку»

```
Пользователь: «нарисуй кота на подоконнике, и чтобы было красиво»
  → cppworker (gemma-4/Qwen3) получает инструменты generate_image + list_image_models
  → модель вызывает list_image_models → балансер отдаёт каталог (GPU не тратится)
  → модель выбирает модель под «красиво» (например flux-schnell-q3-k, ~5.2 GB)
  → вызывает generate_image{model:"flux-schnell-q3-k", prompt:"…"}
  → балансер: модель не в VRAM → POST /api/image/models/load → ждёт state=loaded
              (VRAM-гейт и очередь соблюдаются) → генерация
  → в tool-сообщении: url, markdown, loadSeconds (~40 с), modelState=loaded
  → модель пишет финальный ответ и предупреждает о времени загрузки
  → клиент видит текст + markdown-картинку
```

Что видно оператору в Monitor: обе генерации с `surface=chat-tool` (если их две) и
запись о загрузке модели; вызов каталога в ленту НЕ попадает — он ничего не
генерировал.

### 16.5 Диагностика

```bash
# какие модели видит балансер и что про них знает
curl -sS -H "X-API-Token: $LB_API_TOKEN" \
  http://localhost:28081/api/v1/image/models/catalog | jq '.models[] | {name, state, vramEstimateMb, source}'

# профиль + фактический argv, который уйдёт в sd-server
curl -sS -H "X-API-Token: $LB_API_TOKEN" \
  http://localhost:28081/api/v1/image/model-profiles/sd15-q8-0 | jq '{profile, serverArgs}'

# поднять модель вручную (то же, что делает автозагрузка)
curl -sS -X POST -H "X-API-Token: $LB_API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"flux-schnell-q3-k"}' \
  http://localhost:28081/api/v1/image/backends/imageworker/models/load
```

Частые причины «каталог пуст»: бэкенд нездоров (в `warnings` будет причина), воркер
ответил не контрактом (`contractOk=false`), модель не скачана в воркер.


