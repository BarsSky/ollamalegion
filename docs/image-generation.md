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
| `quality`, `style`, `user`, `background` | проглатываются |
| ошибки | в OpenAI-конверте `{"error":{"message","type","code"}}` (sd.cpp отдаёт `{"error":"строка"}`) |

Отмена: доступна только для задач в статусе `queued`; генерацию в полёте `sd-server` прервать не может (409) — это ограничение движка, а не балансера. Прогресса по шагам движок тоже не отдаёт.

## 5. Порты и переменные окружения

| Переменная | Значение по умолчанию | Смысл |
|---|---|---|
| `LB_OPENAI_PORT` | 18079 | OpenAI-поверхность балансера; отрицательное значение — выключить слушатель |
| `SDWORKER_PORT` | 18093 | порт image-воркера |
| `SDWORKER_SD_SERVER_BIN` | `sd-server` | путь к бинарю движка |
| `SDWORKER_IMAGE_MODELS_DIR` | `models/image` | каталог image-моделей |
| `LB_ALLOW_IMAGE_TIMEOUT_SEC` | 0 (без капа) | opt-in кап на генерацию изображения |

## 6. Ограничения движка (важно для ожиданий)

- **Одна модель на процесс.** Смена модели = перезапуск `sd-server` (в воркере это `load`/`unload`).
- **Генерация сериализована** одним мьютексом: параллельные запросы встают в очередь.
- **Отмена в полёте и прогресс по шагам недоступны** (примитивы в C-API есть, в сервер не проброшены).
- **`city96/*` GGUF для FLUX не грузятся** в sd.cpp (это формат ComfyUI-GGUF) — брать сборки `leejet/*`.
- **`--vae-on-cpu` даёт штраф ~5×** — предпочитайте `--vae-tiling`; на AMD/RADV тайлинг обязателен.
- Релизы sd.cpp выходят ежедневно, имена флагов менялись (`--host/--port` → `--listen-ip/--listen-port`) — версия пинована в `types.PinnedSDServerRevision`.

## 7. Диагностика

```bash
# здоровье image-бэкенда
curl -s http://localhost:18081/api/v1/backends | grep -i image_cpp

# возможности движка и лимиты (напрямую у воркера; агрегация на балансере — Phase 4)
curl -s http://<воркер>:18093/api/image/capabilities

# список моделей (должны быть sd-cpp-local и dall-e-*)
curl -s http://localhost:18079/v1/models

# генерация напрямую
curl -s -X POST http://localhost:18079/v1/images/generations \
  -H 'Content-Type: application/json' \
  -d '{"model":"dall-e-2","prompt":"a cat","size":"512x512","n":1}' | head -c 200
```

Если `/v1/images/generations` отвечает `503 image_backend_unavailable` — не зарегистрирован ни один здоровый бэкенд типа `image_cpp`.
