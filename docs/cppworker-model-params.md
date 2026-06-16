# Per-Model Profiles и управление n_ctx (cppworker)

> **Связанные документы:**
> - [cline-troubleshooting.md](./cline-troubleshooting.md) — диагностика Cline при n_ctx overflow
> - [api.md](./api.md) — полная справка по API
> - [configuration.md](./configuration.md) — структура `config.json`
> - [llamacpp-proxy-guide.md](./llamacpp-proxy-guide.md) — архитектура проксирования

## Содержание

1. [Зачем нужны per-model profiles](#зачем-нужны-per-model-profiles)
2. [3-tier resolver n_ctx](#3-tier-resolver-n_ctx)
3. [Per-model profile (структура)](#per-model-profile-структура)
4. [Header `X-Cpp-Ctx`](#header-x-cpp-ctx)
5. [Reload модели (когда n_ctx нельзя просто так поднять)](#reload-модели)
6. [API endpoints (CRUD + apply)](#api-endpoints)
7. [WebUI мастер настройки](#webui-мастер-настройки)
8. [Типичные сценарии](#типичные-сценарии)
9. [Ограничения и edge cases](#ограничения-и-edge-cases)

---

## Зачем нужны per-model profiles

### Проблема

`n_ctx` (context size) в llama.cpp — **immutable после загрузки модели**. Если модель загружена с `n_ctx=4096`, увеличить до 16384 без перезагрузки **нельзя** — придёт `n_ctx overflow` от Cline/OpenWebUI при попытке использовать длинный system prompt.

Раньше администратору приходилось:

1. Менять `LLAMA_CTX_SIZE` в `.env` cppworker.
2. Перезапускать cppworker-контейнер (даунтайм ~10-30 секунд).
3. Надеяться, что 8K хватит всем моделям.

Это плохо, потому что:

- **gemma-4-E4B-it-Q4_K_M** просит 32K-128K (большие системные промпты, long context)
- **llama-3.1-8b** комфортно работает на 8K
- **qwen2.5-coder-7b** нужно 16K для редактирования файлов
- **embedding-модели** вообще не нуждаются в длинном контексте

### Решение

**Per-model profile** + **3-tier resolver** + **reload endpoint** дают:

- Задать `n_ctx` для каждой модели независимо.
- Применить новый профиль **на лету** (reload без перезапуска контейнера).
- Клиент может override-нуть через `options.num_ctx` в body (Ollama) или `num_ctx` (OpenAI).

---

## 3-tier resolver n_ctx

При обработке каждого запроса balancer вычисляет эффективный `n_ctx` через **3-tier resolver** (`internal/balancer/num_ctx_resolver.go`):

```
┌────────────────────────────────────────────────────────────────────┐
│ Tier 1: request body (наивысший приоритет)                         │
│   Ollama:   {"options": {"num_ctx": 4096}}                         │
│   OpenAI:   {"num_ctx": 4096}                                      │
│   → клиент явно попросил → это всегда побеждает                    │
├────────────────────────────────────────────────────────────────────┤
│ Tier 2: per-model profile (config.LlamaCppModelProfiles[name])    │
│   → если админ настроил профиль для gemma-4 → 32768                │
├────────────────────────────────────────────────────────────────────┤
│ Tier 3: per-backend default (state.Backend.CppWorkerConfig.ContextLength) │
│   → fallback из дефолта cppworker (LLAMA_CTX_SIZE из .env)         │
└────────────────────────────────────────────────────────────────────┘
```

**Важно:**

- Tier 1 всегда побеждает — клиент имеет последнее слово.
- Tier 2 побеждает Tier 3 — профиль модели приоритетнее общего дефолта бэкенда.
- Если ни один tier не дал значение (0), cppworker использует свой `defaultCtxSize`.

После резолва balancer выставляет header `X-Cpp-Ctx` в запросе к cppworker (если `Value > 0`).

---

## Per-model profile (структура)

Профиль живёт в `config.json` под ключом `llamaCppModelProfiles`:

```json
{
  "llamaCppModelProfiles": {
    "gemma-4-E4B-it-Q4_K_M": {
      "contextLength": 32768,
      "batchSize": 1024,
      "numGpuLayers": -1,
      "flashAttn": true,
      "numa": false,
      "useMmap": true,
      "notes": "Cline long context + system prompt"
    },
    "llama-3.1-8b-instruct-q4_K_M": {
      "contextLength": 8192,
      "batchSize": 512,
      "numGpuLayers": -1
    }
  }
}
```

### Поля профиля

| Поле             | Тип            | Обязательно | Default | Описание                                       |
|------------------|----------------|-------------|---------|------------------------------------------------|
| `contextLength`  | int            | **да**      | —       | Размер контекста. Диапазон `[256, 262144]` (256K). |
| `batchSize`      | int            | нет         | 512     | Размер батча. `0` = не задано (использовать cppworker default). |
| `numGpuLayers`   | int            | нет         | -1      | Сколько слоёв на GPU. `-1` = все, `0` = CPU-only, `N` = конкретное число. |
| `flashAttn`      | bool (ptr)     | нет         | true    | Flash Attention. `null` = не переопределять.   |
| `numa`           | bool (ptr)     | нет         | false   | NUMA-оптимизация.                              |
| `useMmap`        | bool (ptr)     | нет         | true    | Memory mapping.                                |
| `notes`          | string         | нет         | ""      | Свободный комментарий для админа.              |

> **Почему bool-поля — указатели?** Это позволяет PATCH-семантику: если в body `flashAttn: false`, это перезапишет `true` в профиле. Если поле отсутствует — старое значение сохранится.

### Валидация

- `contextLength` ∈ `[256, 262144]`. 256K — потолок для gemma-4 (native 262144).
- `contextLength = 0` в профиле **недопустимо** (бессмысленно для resolver'а).
- `batchSize < 1` — ошибка (если задан).
- `numGpuLayers < -1` — ошибка.

### Верхняя граница (262144)

Число 262144 (256 × 1024) — это **нативный max context** для gemma-4. Больше llama.cpp просто не сможет обработать. WebUI слайдер ограничен этим значением.

---

## Header `X-Cpp-Ctx`

Когда balancer резолвит `n_ctx > 0` из профиля или backend default, он добавляет в запрос к cppworker заголовок:

```
X-Cpp-Ctx: 32768
```

cppworker в `cmd/cppworker/main.go` (функция `applyCppCtxHeader`) читает этот header **и** `body.options.num_ctx` (Ollama) / `body.num_ctx` (OpenAI), и применяет к `params.NCtxOverride` (Go-embed для C-bridge).

**Приоритет в cppworker (внутри одного запроса):**

```
1. body.options.num_ctx (Ollama) / body.num_ctx (OpenAI) — самый высокий
2. X-Cpp-Ctx header от balancer — fallback
3. defaultCtxSize (из LLAMA_CTX_SIZE) — последний fallback
```

> **Body > Header.** Это правило общее для обоих уровней (balancer→cppworker, и внутри cppworker). Body — это явный per-request override, header — это политика.

**Пример прокидывания:**

```
Cline → POST /api/chat (no num_ctx)
         ↓
Balancer: resolver → profile[gemma-4] = 32768 → X-Cpp-Ctx: 32768
         ↓
cppworker: header 32768 → params.NCtxOverride = 32768
         ↓
llama.cpp: n_ctx = 32768 (override дефолта 8192)
```

---

## Reload модели

Если модель уже **загружена** в llama.cpp с `n_ctx=8192`, а админ выставил профиль с `contextLength=32768`, ничего не произойдёт до reload. `n_ctx` immutable до полной перезагрузки.

cppworker предоставляет endpoint:

```
POST /api/models/reload
Content-Type: application/json

{
  "name": "gemma-4-E4B-it-Q4_K_M",
  "contextSize": 32768,
  "batchSize": 1024,
  "numGpuLayers": -1,
  "flashAttn": true
}
```

Что делает:

1. `unload` текущей модели (если загружена).
2. `load` с новыми параметрами (через C-bridge `bridge_load_model`).
3. Pre-flight check (`bridge_check_ctx_capacity`) перед загрузкой.
4. Возвращает `200 OK` после успешной загрузки.

**Время reload:** 5-30 секунд в зависимости от размера модели и диска.

> **См. также:** [plans/cppworker-preflight-nctx-session-report.md](../plans/cppworker-preflight-nctx-session-report.md) — детали реализации.

---

## API endpoints

Все endpoints защищены `AuthMiddleware` (если `API_TOKEN` задан) и `RateLimitMiddleware`.

### `GET /api/v1/cppworker/model-profiles`

Список всех профилей.

**Ответ 200:**

```json
{
  "models": {
    "gemma-4-E4B-it-Q4_K_M": {
      "contextLength": 32768,
      "batchSize": 1024,
      "numGpuLayers": -1,
      "flashAttn": true,
      "numa": false,
      "useMmap": true,
      "notes": "Cline long context"
    }
  },
  "total": 1
}
```

### `GET /api/v1/cppworker/model-profiles/{name}`

Получить профиль для одной модели. `404` если профиля нет.

### `PUT /api/v1/cppworker/model-profiles/{name}`

Создать или обновить профиль. **Persist в `config.json`.**

**Body:**

```json
{
  "contextLength": 32768,
  "batchSize": 1024,
  "numGpuLayers": -1,
  "flashAttn": true,
  "notes": "Cline long context"
}
```

**Ответ 200:**

```json
{
  "status": "ok",
  "model": "gemma-4-E4B-it-Q4_K_M",
  "profile": { "contextLength": 32768, ... }
}
```

**Ошибки:**

- `400 invalid JSON body` — тело не JSON или содержит unknown fields.
- `400 invalid profile` — `contextLength` вне `[256, 262144]`, `batchSize < 1` и т.д.
- `400 model name required` — пустое имя.

### `DELETE /api/v1/cppworker/model-profiles/{name}`

Удалить профиль. **Persist в `config.json`.**

**Ответ 200:** `{ "status": "ok", "model": "..." }` (или `404` если профиля не было).

### `POST /api/v1/cppworker/model-profiles/{name}/apply`

**Главный endpoint для админа:** применить профиль с reload на всех бэкендах.

**Body (опционально):** можно передать patch — поля из body мерджатся с текущим профилем (zero-value поля не перезаписывают существующие).

**Алгоритм:**

1. Прочитать body, смержить с текущим профилем.
2. Валидировать merged профиль.
3. Сохранить в config.
4. Для каждого llama_cpp бэкенда:
   - Если модель **не загружена** → `skipped: "model not currently loaded on this backend"`.
   - Если загружена → POST `/api/models/reload` на cppworker. Успех → `reloaded`, ошибка → `error`.
5. Вернуть агрегированный результат.

**Ответ 200:**

```json
{
  "model": "gemma-4-E4B-it-Q4_K_M",
  "profile": {
    "contextLength": 32768,
    "batchSize": 1024,
    "numGpuLayers": -1
  },
  "backends": [
    { "backendId": "llama-gpu-1", "status": "reloaded" },
    { "backendId": "llama-cpu-1", "status": "skipped", "message": "model not currently loaded on this backend" }
  ]
}
```

> **Идемпотентность:** apply с теми же параметрами — no-op (reload всё равно произойдёт, но без изменений). Чтобы только сохранить профиль без reload, используйте `PUT`.

### Примеры curl

```bash
# 1. Список профилей
curl http://localhost:18081/api/v1/cppworker/model-profiles

# 2. Создать профиль для gemma-4
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -d '{
    "contextLength": 32768,
    "batchSize": 1024,
    "numGpuLayers": -1,
    "flashAttn": true,
    "notes": "Cline + long system prompt"
  }'

# 3. Применить (сохранить + reload на всех бэкендах)
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply \
  -H "Content-Type: application/json" \
  -d '{ "contextLength": 65536 }'

# 4. Patch (только contextLength, остальное не трогаем)
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply \
  -H "Content-Type: application/json" \
  -d '{ "contextLength": 65536 }'

# 5. Удалить профиль
curl -X DELETE http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M
```

---

## WebUI мастер настройки

В **Settings → CppWorker → Model Profiles** доступен визуальный мастер:

1. **Список профилей** — отображает все настроенные модели.
2. **Add Profile** — открывает wizard:
   - Поле "Model name" (обязательно).
   - Слайдер n_ctx от **256** до **262144** (256K) с пресетами:
     - 4K (4096)
     - 8K (8192)
     - 16K (16384)
     - 32K (32768)
     - 64K (65536)
     - 128K (131072)
     - 256K (262144)
     - **Custom** (ручной ввод)
   - Поля batchSize, numGpuLayers, notes.
3. **Apply** — вызывает `POST /.../apply`, показывает **progress bar** с шагами:
   - Step 1: Save profile (HTTP PUT)
   - Step 2: Reload on backends (HTTP POST /apply)
   - Step 3: Done
4. **Edit / Delete** — иконки рядом с профилем.

> **i18n:** все строки переведены в `webui/js/i18n/ru.js` и `en.js` (ключ `settings.profiles.*`).

**Прогресс-бар:** показывает реальный статус reload, обновляется через polling ответа `/apply`. Если хотя бы на одном бэкенде ошибка — отображается toast с описанием.

---

## Типичные сценарии

### Сценарий 1: Cline жалуется на n_ctx overflow

**Симптом:** в логах cppworker или Cline видно "context size exceeded" / "n_ctx overflow".

**Решение:**

```bash
# 1. Создать профиль с большим n_ctx
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -d '{ "contextLength": 32768 }'

# 2. Применить (reload)
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply
```

### Сценарий 2: gemma-4 нужен 256K

```bash
# 1. Убедиться, что у GPU хватает VRAM (256K для 4B модели = ~2GB KV-cache)
# 2. Создать профиль
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -d '{ "contextLength": 262144, "batchSize": 512 }'

# 3. Применить
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply
```

### Сценарий 3: Клиент хочет override для одного запроса

```bash
# Клиент (Cline) шлёт:
curl -X POST http://localhost:18080/api/chat \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemma-4-E4B-it-Q4_K_M",
    "options": { "num_ctx": 16384 },
    "messages": [...]
  }'
```

Balancer видит `options.num_ctx = 16384` в body, использует его (Tier 1) вместо профиля.

### Сценарий 4: Несколько бэкендов, профиль общий

`apply` шлёт reload на **все** llama_cpp бэкенды, где модель загружена. Если на каком-то бэкенде не загружена — `skipped` (не ошибка, просто нечего перезагружать).

---

## Ограничения и edge cases

### 1. Reload = даунтайм для этой модели

Пока модель перезагружается (5-30 сек), запросы к ней получат ошибку или будут отправлены на другой бэкенд (если есть реплика).

**Рекомендация:** используйте Model Replication (`LLAMA_ENFORCE_REPLICATION=true`) — balancer переключит нагрузку на реплику во время reload.

### 2. n_ctx не может быть уменьшен без reload

Если профиль меняется с 32K на 8K, всё равно нужен reload. cppworker не может "сжать" KV-cache на лету.

### 3. Pre-flight check может отклонить загрузку

Если `n_ctx × layers × head_dim × 2 (fp16) > VRAM`, C-bridge вернёт ошибку **до** загрузки. cppworker проксирует 500 с описанием.

**См. также:** [plans/cppworker-preflight-nctx-session-report.md](../plans/cppworker-preflight-nctx-session-report.md) — реализация pre-flight check.

### 4. Ollama-бэкенды игнорируются

Tier 3 resolver возвращает 0 для backend.Type != `llama_cpp`. Per-model profile применимо только к llama.cpp.

### 5. Bool-поля в PATCH

`flashAttn: false` в body перезапишет `true` в профиле. Чтобы **не трогать** поле, его нужно **опустить** в JSON.

### 6. Header `X-Cpp-Ctx` не отменяет body

Если клиент отправил `options.num_ctx=4096`, а профиль = 32768, balancer **не** перезапишет body. Tier 1 всегда побеждает.

### 7. Авторизация

Все `/api/v1/cppworker/model-profiles/*` endpoints защищены `AuthMiddleware`. Если `API_TOKEN` в `config.json` не задан, endpoints открыты (для dev-среды).

---

## Связанные env-переменные (cppworker)

`config/cppworker.example.env`:

```bash
# Default context (Tier 3 fallback)
LLAMA_CTX_SIZE=8192

# Default batch size
LLAMA_BATCH_SIZE=512

# Default num GPU layers
LLAMA_N_GPU_LAYERS=-1

# Flash Attention
LLAMA_FLASH_ATTN=true
```

Эти значения используются **только** когда нет per-model profile и body не задал `num_ctx`. Profile всегда побеждает.

---

> **См. также:**
> - [plans/cppworker-preflight-nctx-session-report.md](../plans/cppworker-preflight-nctx-session-report.md) — полный отчёт о 9-шаговом плане.
> - [internal/balancer/num_ctx_resolver.go](../internal/balancer/num_ctx_resolver.go) — реализация resolver'а.
> - [internal/balancer/num_ctx_resolver_test.go](../internal/balancer/num_ctx_resolver_test.go) — 22 unit-теста.
> - [internal/api/handlers_cppworker_profiles.go](../internal/api/handlers_cppworker_profiles.go) — HTTP handlers.