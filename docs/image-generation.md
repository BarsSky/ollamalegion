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
      "exclusiveLockTimeoutSec": 0,
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
| `exclusiveLockTimeoutSec` | предохранитель: лок снимается принудительно (с WARN), чтобы зависшая генерация не блокировала карту навсегда. **`0` (по умолчанию с R88) = предохранитель снят**: доктрина запрещает duration-кап на работу — при 600 с лок снимался посреди легитимной генерации (2048×2048/40 шагов = 22m30s) и текстовый трафик шёл на занятую карту. Лок освобождается по жизненному циклу запроса; взводите кап только если нужен «будильник» (на стенде выставлено 6000) |
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
- работает на двух поверхностях: `/v1/chat/completions` (OpenAI) и `/api/chat`
  (Ollama, включая префиксы `/ollama/*` и `/openai/*`). Anthropic `/v1/messages`
  не покрыт;
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

**Оба значения правятся из WebUI без перезапуска** (таб «Image-модели» → «Обзор» →
карточка политики): галочка «разрешить инструменту загружать image-модель» и поле
«Ожидание загрузки модели, с». Приоритет: **значение из WebUI** (файл
`/app/data/image-resources.json`) → переменная окружения → дефолт.

Это нужно ровно тогда, когда модель большая: `qwen-image-2.1` (4.7 ГБ) на медленном
диске не укладывается в 600 с, и вызов вернёт «модель не поднялась за 600s» — тогда
поднимите поле до 1800–3600 с. Границы: 1…86400 с; **0 отклоняется**, потому что это
«не ждать вовсе» (инструмент гарантированно не смог бы поднять модель).

Проверить действующие значения:

```bash
curl -sS -H "X-API-Token: $LB_API_TOKEN" http://localhost:28081/api/v1/image/resources \
  | jq '{allow: .allowToolLoad, loadTimeout: .toolLoadTimeout, limits: .limits}'
```

```json
{ "allow": {"effective": true, "overridden": false, "env": "LB_IMAGE_TOOL_ALLOW_LOAD"},
  "loadTimeout": {"effectiveSec": 600, "overridden": false, "env": "LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC"},
  "limits": {"minToolLoadTimeoutSec": 1, "maxToolLoadTimeoutSec": 86400} }
```

`overridden: false` означает «действует переменная окружения / дефолт»; после
сохранения из WebUI там будет `true`, а `effectiveSec` — ваше значение.

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

### 16.5 Две поверхности: `/v1/chat/completions` и `/api/chat` (R86)

**Почему это важно для Open WebUI.** Инструменты объявляются на ДВУХ поверхностях
чата, потому что клиенты ходят по-разному:

| Клиент / режим подключения | Путь | Инструмент |
| --- | --- | --- |
| Open WebUI, подключение типа **OpenAI** | `POST /v1/chat/completions` | объявляется |
| Open WebUI, подключение типа **Ollama** (по умолчанию) | `POST /api/chat` | объявляется (R86) |
| Cline / Roo / Continue / OpenAI SDK | `POST /v1/chat/completions` | объявляется |
| curl к `/api/chat` напрямую | `POST /api/chat` | объявляется |

До R86 на `/api/chat` инструментов не было вовсе — именно поэтому модель «не
видела инструменты» при работе через Open WebUI. Теперь на Ollama-поверхности
работает тот же цикл: объявление `tools`, перехват `tool_calls` из NDJSON-ответа,
исполнение (каталог/генерация с автозагрузкой) и финальный ответ модели.

Отличия в поведении на `/api/chat` (сознательные):

- **инструменты клиента сохраняются** — в Open WebUI у пользователя обычно
  включены свои (веб-поиск и т.п.), и они продолжают работать вместе с нашими;
- если клиент объявил инструмент **с нашим именем** (`generate_image` или
  `list_image_models`) — наши не добавляются совсем: исполняет его сторона;
- `tool_choice: "none"` уважается: инструменты не объявляются вовсе;
- клиенту, просившему `stream: true`, ответ отдаётся NDJSON-потоком с
  обязательным `done: true` (turn 1 всё равно нестриминговый — решение
  принимается по целому ответу).

### 16.6 Диагностика: «модель не видит инструменты»

На каждый запрос чата балансер пишет INFO-строку решения — по ней причина видна
сразу, без чтения исходников:

```bash
docker logs ol-stack-balancer --since 30m 2>&1 | grep "image tool:"
```

Примеры:

```json
{"msg":"image tool: инструменты объявлены модели","path":"/api/chat","model":"Qwen3-...","client_tools":3,"injected":true,"injected_tools":"generate_image,list_image_models"}
{"msg":"image tool: инструменты НЕ объявлены","path":"/v1/chat/completions","client_tools":1,"injected":false,"reason":"клиент прислал tool_choice=\"none\" — инструменты запрещены в этом запросе"}
{"msg":"image tool: инструменты НЕ объявлены","path":"/api/chat","client_tools":0,"injected":false,"reason":"на image-бэкенде нет моделей (нечего генерировать)"}
```

Возможные `reason`: `LB_IMAGE_TOOL=off`, нет здорового `image_cpp`, нет моделей на
воркере, нет загруженной модели при `LB_IMAGE_TOOL_ALLOW_LOAD=off`,
`tool_choice="none"` от клиента, клиент сам объявил наши инструменты.

**Если модель вызвала инструмент, а картинки нет** — смотрите строки
`image tool (ollama):` / `image tool:` подряд: там видно, распознан ли вызов
(`генерация по вызову модели`), какой бэкенд и модель выбраны и чем закончилось
(`генерация не удалась: …`). Частые случаи:

- `загрузка модели "X" прервана: context canceled` — не хватило времени загрузки:
  поднимите «Ожидание загрузки модели, с» в карточке политики (§16.3) или задайте
  `model` явно;
- `модели "X" нет на image-воркере. Доступные модели: …` — модель выдумала имя.
  В ответе инструменту перечислены доступные имена, и модель может исправиться на
  следующем turn; если она повторяет выдуманное имя — попросите её вызвать
  `list_image_models` и выбрать из списка;
- в ответе клиенту видно `[]` или `[TOOL_CALLS]=[]` — это артефакт шаблона
  Qwen3; балансер его вычищает (если он всё же виден, значит запрос шёл не через
  балансер, а напрямую в cppworker).

Если строки `image tool:` нет вообще — запрос не похож на чат с инструментами
(например, пришёл на `/api/generate`) либо клиент ходит не на тот порт: OpenAI и
Ollama-поверхности слушают **18079** и **18080** соответственно, management API —
18081.

### 16.7 Прочие проверки

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

### 16.8 «Паспорт репозитория»: что скачать, в каком режиме и что делать

R86-follow-up (2026-10-06). Поиск моделей показывает файлы, но не говорил главного:
**какой набор движок реально запустит и в каком режиме**. Живой случай: в
репозитории были и diffusion, и VAE, и text encoder, но в bundle попал только
diffusion — движок ответил «VAE tensor … not in model metadata» и не поднялся.

Теперь при выборе репозитория в карточке (WebUI → Image-модели → HuggingFace)
сразу показывается паспорт — тот же ответ отдаёт API:

```bash
curl -sS -H "X-API-Token: $LB_API_TOKEN" \
  'http://localhost:28081/api/v1/image/backends/imageworker/hf/plan?modelId=<repo>'
```

Что в нём важно:

| Поле | Смысл |
| --- | --- |
| `family`, `versionLabel` | что движок узнаёт по заголовку главного файла |
| `engineMode` | `model` — all-in-one (`--model`); `diffusion-model` — DiT (`--diffusion-model` + отдельные VAE/text encoder) |
| `roles[]` | обязательные роли: ✅ найден файл (с путями), ❌ нет и что искать |
| `missing[]` | чего не хватает — без этого загрузка упадёт |
| `verdict`, `summary` | `ready` / `incomplete` / `unsupported` / `unknown` одной строкой |
| `steps[]` | порядок действий в WebUI |
| `modelHint` | **что передавать текстовой модели** |

**Порядок действий оператора** (он же в `steps[]`):

1. отметить в списке файлов то, что предлагает паспорт (diffusion + vae + llm);
2. проверить роли у файлов (подставляются по именам, можно изменить);
3. выбрать **семейство** из паспорта — для DiT это обязательно, иначе движок
   получит `--model` и ответит «get sd version from file failed»;
4. «Скачать bundle» → файлы лягут в один каталог модели;
5. на вкладке «Модели на диске» нажать «Загрузить»;
6. если загрузка упала — прочитать текст ошибки: движок перечисляет недостающие
   тензоры (VAE/conditioner), значит в наборе не хватает роли.

**Что важно модели, которая сама поднимает модель.** В `modelHint` (и в описании
инструмента, см. §16.1) сказано: в `generate_image` надо передавать **имя каталога
bundle** (`model="qwen-image-2.1-UC-Q4_K_M"`), а не «stable-diffusion.cpp» и не имя
репозитория; список имён возвращает `list_image_models`. Если имя указано неверно,
балансер подставляет уже загруженную модель и сообщает об этом в результате
(`requestedModel`/`modelNote`), но корректное имя экономит целый turn.

### 16.9 «Модель скачалась, но не загружается» — разбор

Ошибку загрузки видно в WebUI (и в логе воркера). Живые случаи со стенда:

- `unsupported dtype "U32" (tensor "img_in.weight")` + `new_sd_ctx_t failed` —
  скачан **не тот формат**: `*.safetensors` с MLX-квантованием (в имени обычно
  `MLX`, в шапке файла — тензоры `U32` вместе с `scales`/`biases`).
  stable-diffusion.cpp читает **GGUF** и не понимает MLX-кванты. Проверить просто:

  ```bash
  head -c 3000 файл.safetensors | tr -d '\0' | grep -o '"dtype":"[A-Z0-9]*"' | sort -u
  ```

  Если у `*.weight` стоит `U32` — файл не подойдёт; нужны GGUF-кванты для роли
  `diffusion` (VAE при этом может оставаться safetensors — он читается);
- `GGUF files cannot be loaded by stable-diffusion.cpp — img_in.weight is stored
  reshaped` — известная проблема части публикаций Qwen-Image 2.1 в GGUF
  ([обсуждение на HF](https://huggingface.co/realrebelai/Qwen-Image-2.1_GGUFs/discussions/2)):
  тензор переупакован, движок его не читает — нужен другой источник квантов;
- эталон рабочей конфигурации на стенде — `sd15-q4` (all-in-one GGUF): если он
  грузится, дело в конкретном файле, а не в воркере.

### 16.10 Рабочий набор для Qwen-Image 2.1: какие именно файлы нужны

Раздел написан по ЖИВОМУ прогону (2026-10-07): набор собран, загружен и дал
картинку. Здесь — точный состав и то, на чём легко ошибиться.

**Состав набора (три роли, все обязательны).** Qwen-Image 2.1 — DiT-семейство,
поэтому одного файла недостаточно: движок грузится флагами `--diffusion-model` +
`--vae` + `--llm`.

| роль | файл (пример рабочего) | флаг | замечание |
| --- | --- | --- | --- |
| `diffusion` | `qwen-image-2.1-UC-Q4_K_M.gguf` (4.29 ГБ) | `--diffusion-model` | только **GGUF**; safetensors-варианты этого семейства движок не читает |
| `vae` | `vae/qwen_image_2.1_vae_bf16.safetensors` (0.63 ГБ) | `--vae` | обычный safetensors, читается штатно |
| `llm` | `text_encoders/qwen3vl_8b_int8_convrot.safetensors` (8.71 ГБ) | `--llm` | именно текстовый энкодер Qwen3-VL; формат — int8-квантование (I8 + scale) |

**Почему подходит именно этот формат энкодера.** Движок грузит файл из `--llm`
так (`src/pipeline/diffusion_engine.cpp`:

```cpp
model_loader.init_from_file(sd_ctx_params->llm_path, "text_encoders.llm.");
```

то есть к именам тензоров добавляется префикс `text_encoders.llm.`, после чего
движок ищет `text_encoders.llm.model.embed_tokens.weight`. Файл, у которого
тензоры уже названы `model.embed_tokens.weight*`, подходит; MLX-файлы того же
репозитория не подходят по типу весов (`U32`).

**ЛОВУШКА: файлы с одинаковыми именами в шапке, но разного происхождения.**
Если в наборе нет роли `llm`, движок падает не с «нет файла», а с проверкой
метаданных:

```
Conditioner model tensor 'text_encoders.llm.model.embed_tokens.weight' not in model metadata
Conditioner model tensor 'text_encoders.llm.model.norm.weight' not in model metadata
diffusion_engine.cpp: model metadata validation failed
```

Это сообщение означает РОВНО ОДНО: роль `llm` не подключена (или подключён файл,
тензоры которого не совпали). Не «модель битая» и не «формат не тот» — сначала
проверьте, что в `argv` воркера есть `--llm`.

**Как проверить, что набор полный — до загрузки.** В WebUI: вкладка HuggingFace →
«Паспорт» у репозитория (`GET /api/hf/plan`) показывает `engineMode` и роли.
На стенде для `abenzerps/Qwen-Image-2.1-Uncensored-GGUF` паспорт отвечает:
`engineMode=diffusion-model`, обязательные роли `diffusion`, `vae`, `llm`.

**Размер и место на диске.** Полный набор — ~13.6 ГБ. Держите каталог моделей на
диске, где этот объём действительно есть (`MODELS_DIR` в `deployments/.env`):
на C: стенда свободного места было меньше 5 ГБ, и первая же попытка скачать
энкодер упёрлась в «no space left on device».

**Проверка «набора целиком» одной командой.** После загрузки модели воркер
печатает в лог `spawning sd-server` с полным `argv`. В нём обязаны быть все три
флага:

```bash
docker logs ol-stack-imageworker 2>&1 | grep -o '\-\-\(diffusion-model\|vae\|llm\) [^ ]*'
```

**Рабочий прогон (для сравнения ожиданий).** RTX 3070 8 ГБ, набор выше,
`--offload-to-cpu --max-vram -1 --diffusion-fa --vae-tiling`:

- загрузка модели — единицы секунд (модель грузится лениво, `sd-server`
  стартует и отвечает readiness сразу);
- генерация 512×512, 8 шагов, cfg 2.5 — **159 с** (без GPU-конкурента);
- результат — валидный PNG 512×512 (проверено визуально).

Скорость именно такая потому, что часть весов идёт через CPU-offload: набор
целиком (13.6 ГБ) в 8 ГБ VRAM не влезает ни при каком квантовании диффузии.
Если нужна скорость — берите меньший квант энкодера или больше VRAM.

---

## 17. Долгая генерация: цепочка таймаутов и RAM-offload (R87)

### 17.1 Симптом: «2048×2048 падает с HTTP 504, а ресурсов вроде хватает»

512×512 проходит за секунды, 2048×2048 с 40 шагами «падает с 504», при этом в
логе воркера видно завершённую генерацию. Это не нехватка памяти: картинка
считается и отдаётся воркером — обрывается только путь
«браузер → nginx панели → балансер → воркер».

Замер на эталонном стенде (RTX 3070 8 ГБ, Qwen-Image-2.1 Q4_K_M,
`--offload-to-cpu --max-vram -1 --diffusion-fa --vae-tiling`,
2048×2048, 40 шагов):

```
sdbackend/jobs.go   image generation completed  model=qwen-image-2.1-uncensored-gguf
                    images=1 seed=73172649 size=2048x2048 steps=40 duration_ms=1304815
sdworker/utils.go   POST /v1/images/generations status=200 duration=21m44.846s
```

**21 минута 45 секунд** — столько живёт один запрос на 2048². Всё, что срабатывает
раньше этого срока, рвёт соединение, хотя генерация продолжается.

**Проверка после фикса (тот же стенд, запрос через панель — nginx :18083):**
`HTTP 200` за **1350 с (22m30s)**, в ответе валидный PNG **2048×2048** (9.0 МБ).
Пики за прогон: **VRAM 7447 / 8192 МиБ** (91%), **RAM воркера 13.0 / 25.0 ГиБ** (52%),
загрузка GPU до 100%. До фикса этот же запрос обрывался ровно на 600-й секунде (504).

### 17.2 Цепочка таймаутов: кто что режет

| Звено | Значение по умолчанию | Где меняется | Что будет, если не трогать |
|---|---|---|---|
| Браузер (панель), таб «Тест» | **капа нет** (`TIMEOUT_GENERATE_MS = 0`) | `webui/js/modules/image-test-page.js` | панель рвала запрос на 10-й минуте, хотя воркер продолжал считать |
| nginx панели, рабочие пути | **86400 с** («без капа») | `webui/nginx.conf` | 600 с на `/v1/` = 504 на 2048×2048 |
| nginx панели, ошибки 502/503/504 | JSON с `error_type` + `hint` | `webui/nginx.conf` (`error_page`) | клиент видел HTML-страницу и «HTTP 504» без объяснения |
| Балансер, кап на генерацию | **нет** (`0`) | `LB_ALLOW_IMAGE_TIMEOUT_SEC` | по умолчанию ждём терминального состояния движка |
| Балансер, предохранитель GPU-лока | **снят** (`0`) | `/api/v1/image/resources` → `exclusiveLockTimeoutSec` | при 600 с лок снимался ПОСРЕДИ генерации (`reason=fuse_timeout` в логе) |
| Балансер, инструмент `generate_image` | **капа нет** (`0`) | `LB_IMAGE_TOOL_TIMEOUT_SEC` / `LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC` | вызов инструмента на 2048² обрывался на 10-й минуте |
| Воркер, ожидание джобы | **капа нет** (`0`) | `SDWORKER_GENERATION_TIMEOUT_SEC` | взведённый кап вернёт `generation_timeout` → HTTP 504 |
| Воркер, кап из `profile.json` (`timeoutSec`) | **игнорируется** | `SDWORKER_ALLOW_PROFILE_TIMEOUTS=on` | профиль молча обрезал бы легитимную генерацию |
| Воркер, readiness sd-server | **капа нет** (`0`) | `SDWORKER_STARTUP_TIMEOUT_SEC` | ждём ready ИЛИ смерть процесса (внятная ошибка в логе) |
| Движок, `sd-server` | без капа | — | считает, пока не закончит |

Порядок диагностики при 504: сначала смотреть `docker logs <imageworker>` —
если там `image generation completed`, виноват таймаут ЗВЕНА-КЛИЕНТА (панель,
nginx, балансерный кап, лок), а не движок.

```bash
# что реально ответил воркер и сколько это заняло
docker logs ol-stack-imageworker 2>&1 | grep -E 'image generation completed|/v1/images/generations'
# сработал ли предохранитель лока на балансере
docker logs ol-stack-balancer 2>&1 | grep -E 'image GPU lock (acquired|released)'
```

### 17.3 RAM: как движок её использует (и почему её «подключение» уже включено)

Вопрос «можно ли задействовать ещё и RAM» имеет короткий ответ: **она уже
задействована**. В `argv` воркера для Qwen-Image стоит `--offload-to-cpu` — это
шорткат `--params-backend '*=cpu'`: веса живут в оперативной памяти, а в VRAM
попадают посегментно, по мере вычисления (автоматический graph-cut движка).
Поэтому набор 14.6 ГБ и работает на карте с 8 ГБ.

Что важно знать:

- **Правило Unsloth (жёсткое):** доступная **RAM + VRAM** должна превышать размер
  GGUF-файла, иначе модель не загрузится даже с offload. Для набора Qwen-Image
  это 14.6 ГБ против 24.5 ГБ RAM на стенде — запас есть.
- **RAM тратится не только на веса:** сам ОС-процесс `sd-server`, буферы вывода и
  (при `--params-backend disk`) файловый кеш. Смотрите фактический пик, а не
  оценку.
- **Offload — не «медленный режим»:** офиц. формулировка sd.cpp — «без потери
  скорости»; цена — трафик по PCIe на подкачку сегментов. На практике 512×512
  8 шагов = 159 с, 2048×2048 40 шагов = 21m45s (тот же стенд).

Рычаги памяти в `runtime` профиля (правится в `profile.json` модели и в WebUI,
раздел image-моделей):

| Поле | Флаг движка | Когда применять |
|---|---|---|
| `offloadToCpu: true` | `--offload-to-cpu` (= `--params-backend '*=cpu'`) | базовый режим для 8 ГБ: веса в RAM |
| `paramsBackend: "diffusion=disk"` | `--params-backend` | RAM тоже кончается: веса читаются с диска (минимум и VRAM, и RAM, но медленно) |
| `maxVram: "-1"` / `"6"` | `--max-vram` | бюджет VRAM: `-1` = резерв ~1 ГБ от свободной; `6` = жёсткий бюджет для graph-cut |
| `vaeTiling: true`, `vaeTileSize` | `--vae-tiling`, `--vae-tile-size` | главный рычаг против OOM на VAE-decode (обязателен на больших размерах) |
| `vaeConvDirect: true` | `--vae-conv-direct` | снижает VRAM на decode (иногда медленнее) |
| `backend: "te=cpu"` | `--backend te=cpu` | убрать текстовый энкодер из VRAM (для Qwen это int8-энкодер ~8 ГБ) |
| `threads` | `--threads` | сколько ядер отдать CPU-части (ускоряет offload-ветку) |
| `taesd` | `--taesd` | tiny-VAE вместо полного (для Qwen — TAEHV, `--tae`): быстрый decode ценой качества |

Проверить, что флаги действительно ушли в движок (а не «должны были»):

```bash
docker logs ol-stack-imageworker 2>&1 | grep 'spawning sd-server' | tail -1
```

### 17.4 Что делать, если 22 минуты — это долго

1. **Считать 1024×1024 и добивать `--hires`** (`--hires-upscaler`,
   `--hires-scale`): базовый проход в 4 раза дешевле по пикселям, апскейл
   латентов заметно быстрее полного пересчёта на 2048².
2. **Уменьшить шаги.** 40 шагов ≠ обязательное качество: для Qwen-Image
   рекомендованный `flow-shift` 2–3 и 20–30 шагов дают близкий результат.
3. **Разгрузить VRAM, а не RAM:** `--backend te=cpu` (энкодер int8 ~8 ГБ уходит
   из VRAM) освобождает бюджет под активации diffusion — самую тяжёлую часть на
   больших разрешениях.
4. **Взять меньший квант энкодера** (Q4_K_M вместо int8) — меньше и RAM, и
   трафик подкачки.

### 17.5 Что было исправлено (R87: `webui` r83-submodule-v104)

- `location /v1/`: `proxy_read_timeout` 600s → **3600s** — это и был тот 504 на
  2048×2048 (600 с < 21m45s);
- добавлен `location /api/v1/image/` (**3600s**, `proxy_buffering off`) — раньше
  он попадал в catch-all `/api/` с 120 с и буферизацией: загрузка модели 14.6 ГБ
  и SSE-поток прогресса загрузки не переживали этого;
- добавлен `location /api/image/` (**3600s**) — раньше уходил в admin API
  (18081), который нативную image-поверхность не обслуживает → 404;
- добавлен `location /sdapi/` (**3600s**) — раньше падал в `location /` и отдавал
  `index.html` вместо JSON;
- `location /api/worker/`: 300s → **3600s** (долгие операции воркера: загрузка
  модели, скачивание HF-bundle).

### 17.6 Доктрина R88: кап — только opt-in, ошибка — только с объяснением

R87 убрал конкретный 600-секундный кап, но таких капов на image-плоскости было
ещё несколько (воркер, инструмент, предохранитель лока, браузер). R88 переносит
на image-плоскость **ту же доктрину, что уже работает у текстового бэкенда**
(`internal/balancer/timeout_policy.go`, R83/v67).

**Правило (одно на весь проект).**

- Разрешены таймауты **опроса состояния**: «каждые N секунд проверить, готово
  ли» (интервал поллинга джобы/readiness, heartbeat, idle-соединение). Такой
  таймер задаёт частоту взгляда и **не отменяет** работу.
- Запрещены duration-капы **на работу**: всё, что по истечении срока обрывает
  генерацию, загрузку модели, pull/reload. Признак дефекта узнаваем: сервер
  вернул ошибку, а работа продолжается — состояние расходится с ответом.
- Вместо капа — ждать **терминального состояния** и вернуть конкретную ошибку.
- Если кап всё-таки нужен — он включается **только явно** и печатает WARN в лог.

**Реестр opt-in переменных image-плоскости** (все по умолчанию выключены):

| Переменная | Что взводит |
|---|---|
| `SDWORKER_GENERATION_TIMEOUT_SEC` | кап ожидания джобы генерации (воркер) |
| `SDWORKER_STARTUP_TIMEOUT_SEC` | кап ожидания readiness sd-server |
| `SDWORKER_ALLOW_PROFILE_TIMEOUTS` | разрешить `profile.timeoutSec` обрывать генерацию |
| `LB_ALLOW_IMAGE_TIMEOUT_SEC` | кап HTTP-запроса image-генерации на балансере |
| `LB_IMAGE_TOOL_TIMEOUT_SEC` / `LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC` | капы вызова инструмента `generate_image` |
| `exclusiveLockTimeoutSec` (`/api/v1/image/resources`) | предохранитель GPU-лока |

**Вторая половина требования — «явное объяснение ошибок».** Ошибка обязана
содержать не только код, но и действие. Форма ответа едина у всех звеньев:

```json
{
  "error": "backend imageworker is unavailable (connection_refused); retry in 30s",
  "error_type": "connection_refused",
  "backend_id": "imageworker",
  "model": "qwen-image-2.1-uncensored-gguf",
  "detail": "Post \"http://imageworker:18093/v1/images/generations\": dial tcp ...: connect: connection refused",
  "hint": "узел не отвечает: проверьте контейнер (docker ps/logs) и статус бэкенда в WebUI",
  "retry_after": 30
}
```

Кто что добавляет:

- **воркер** — `hint` для каждого кода (`model_not_loaded`, `generation_timeout`,
  `sd_server_startup_failed`, `invalid_size`, …; см. `hintForCode`);
- **балансер** — транспортные ошибки классифицируются и отдаются как
  `503`/`504`/`502` с `error_type`, `detail`, `hint`, `retry_after`
  (`writeUpstreamError`), а отказы гейта — с OOM-лестницей (`writeGateError`);
- **nginx панели** — собственные 502/503/504 отдаёт JSON'ом, а не HTML-страницей
  (`error_page` + `@panel_upstream_*`); ответы самого балансера НЕ перехватывает,
  чтобы не подменить его объяснение;
- **панель** — показывает `hint` сервера раньше собственных эвристик по тексту
  (`errorHint(text, body)`), а `TIMEOUT_GENERATE_MS = 0` больше не рвёт запрос.

Проверка на стенде: `POST /v1/images/generations` (2048×2048/40 шагов) через
портал панели → `HTTP 200` за 22m30s; остановленный бэкенд → 503 с
`error_type=connection_refused` и `hint` про контейнер, а не «502 + dial-текст».


