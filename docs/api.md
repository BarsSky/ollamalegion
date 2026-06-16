# API Документация Ollama Load Balancer

Полная документация по REST API и WebSocket endpoints.

## Содержание

1. [Обзор API](#обзор-api)
2. [Аутентификация](#аутентификация)
3. [REST API Endpoints](#rest-api-endpoints)
4. [WebSocket API](#websocket-api)
5. [Примеры запросов](#примеры-запросов)
6. [OpenAPI спецификация](#openapi-спецификация)

---

## Обзор API

Балансировщик предоставляет REST API для управления кластером и получения метрик.

### Базовый URL

```
# Management API (управление кластером, метрики, сессии)
http://localhost:18081

# HTTPS (если включен TLS)
https://localhost:8443
```

### CppWorker (llama.cpp) API

CppWorker предоставляет следующие API помимо проксируемых Ollama эндпоинтов:

| Endpoint | Метод | Описание | Совместимость |
|----------|-------|----------|---------------|
| `/api/generate` | `POST` | Генерация текста (NDJSON) | Ollama-совместимый |
| `/api/chat` | `POST` | Чат (NDJSON, streaming) | Ollama-совместимый |
| `/api/embeddings` | `POST` | Эмбеддинги | Ollama-совместимый |
| `/api/tags` | `GET` | Список моделей (GGUF) | Ollama-совместимый |
| `/api/ollama/generate` | `POST` | Генерация с options | Ollama-совместимый |
| `/api/ollama/tags` | `GET` | Список моделей | Ollama-совместимый |
| `/v1/chat/completions` | `POST` | Chat completions (SSE) | **OpenAI-совместимый** |
| `/v1/completions` | `POST` | Text completions (SSE) | **OpenAI-совместимый** |
| `/v1/embeddings` | `POST` | Embeddings | **OpenAI-совместимый** |
| `/v1/models` | `GET` | Список моделей | **OpenAI-совместимый** |
| `/health` | `GET` | Health check | |
| `/api/models/load` | `POST` | Загрузка GGUF (защита от двойной загрузки) | |
| `/api/models/unload` | `POST` | Выгрузка модели | |
| `/api/models/reload` | `POST` | Unload + load с новыми параметрами (n_ctx, batchSize, numGpuLayers) | |
| `/api/hf/*` | `GET/POST` | HuggingFace интеграция | |

### CppWorker: Ollama-совместимые поля `/api/generate` и `/api/ollama/generate`

Полный маппинг `options.*` на параметры llama.cpp (через `bridge.GenerationParams`):

| Поле запроса | Параметр llama.cpp | Примечание |
|--------------|--------------------|------------|
| `options.temperature` | `Temperature` | |
| `options.top_p` | `TopP` | |
| `options.top_k` | `TopK` | |
| `options.min_p` | `MinP` | |
| `options.typical_p` | `TypicalP` | |
| `options.tfs_z` | `TfsZ` | |
| `options.num_predict` | `NPredict` | |
| `options.num_keep` | `NKeep` | |
| `options.repeat_penalty` | `RepeatPenalty` | |
| `options.frequency_penalty` | `FrequencyPenalty` | |
| `options.presence_penalty` | `PresencePenalty` | |
| `options.repeat_last_n` | `RepeatLastN` | |
| `options.mirostat` | `Mirostat` | |
| `options.mirostat_tau` | `MirostatTau` | |
| `options.mirostat_eta` | `MirostatEta` | |
| `options.seed` | `Seed` | `0` — валидное значение |
| `options.num_ctx` | `NCtxOverride` | per-request n_ctx |
| `options.stop` | `Antiprompts` | string или []string |

Топ-уровневые алиасы: `temperature`, `topP`, `topK`, `minP`, `typicalP`, `tfsZ`, `maxTokens`, `repeatPenalty`, `frequencyPenalty`, `presencePenalty`, `seed`, `numCtx`.

Дополнительные поля: `system` (подставляется перед prompt, если `raw != true`), `template`, `raw`, `format` (принимается для совместимости), `keep_alive` (принимается, unload-таймер не реализован), `context` (принимается, KV-cache follow-up не реализован), `images` (не поддерживается, принимается для совместимости).

### CppWorker: статистика `/api/generate`

Ответ содержит реальные метрики:

- `load_duration` — время с момента загрузки модели до начала генерации (μs).
- `prompt_eval_count` — число токенов prompt, подсчитанное через tokenizer модели (`bridge_count_tokens` / `Backend.CountTokens`).
- `eval_count` — число токенов в ответе.
- `total_duration`, `prompt_eval_duration`, `eval_duration` — длительность в μs.

Если модель вернула пустой `response: ""`, CppWorker возвращает HTTP 500 с полем `error`.

### CppWorker: `/api/chat`

- Поддерживает `messages` с ролями `system`, `user`, `assistant`.
- Для формирования prompt используется `backend.ApplyChatTemplate` (chat template из GGUF); при отсутствии template — fallback на naive формат (`<|user|>` / `<start_of_turn>user`).
- Поддерживает `stream`, `temperature`, `max_tokens`, `num_ctx`, `options.stop`.
- Возвращает `message.role="assistant"`, `done=true` в финальном чанке.

### CppWorker: защита от повторной загрузки

`POST /api/models/load` проверяет `backend.GetModel(name)` перед вызовом `LoadModelWithOpts`:

- Если модель уже загружена с тем же путём и параметрами (`ContextSize`, `BatchSize`, `GPULayers`, `FlashAttnType`, `NUMA`, `UseMmap`, `TensorSplit`) — возвращает `status: "already_loaded"` и не трогает VRAM.
- Если путь или параметры отличаются — сначала вызывается `UnloadModel`, затем `LoadModelWithOpts` (reload-in-place).
- Две одновременные загрузки одной модели сериализуются через `IsModelLoading`: вторая получает 503 с `loading: true`.

### CppWorker: per-model profiles (управление n_ctx)

**Полная документация:** [cppworker-model-params.md](./cppworker-model-params.md).

Эти endpoint'ы позволяют задать `n_ctx` и другие параметры инференса для конкретной модели на cppworker. Используется для решения проблемы `n_ctx overflow` в Cline/OpenWebUI.

| Endpoint | Метод | Описание | Auth |
|----------|-------|----------|------|
| `/api/v1/cppworker/model-profiles` | `GET` | Список всех per-model профилей | ✅ |
| `/api/v1/cppworker/model-profiles/{name}` | `GET` | Получить профиль для модели | ✅ |
| `/api/v1/cppworker/model-profiles/{name}` | `PUT` | Создать/обновить профиль + save в config.json | ✅ |
| `/api/v1/cppworker/model-profiles/{name}` | `DELETE` | Удалить профиль | ✅ |
| `/api/v1/cppworker/model-profiles/{name}/apply` | `POST` | Save + reload модели на всех llama_cpp бэкендах | ✅ |

**3-tier resolver (приоритет при прокидывании n_ctx в cppworker):**

1. `body.options.num_ctx` (Ollama) / `body.num_ctx` (OpenAI) — **всегда побеждает** (Tier 1)
2. `config.LlamaCppModelProfiles[modelName].ContextLength` — Tier 2
3. `state.Backend.CppWorkerConfig.ContextLength` (per-backend default) — Tier 3

Резолв передаётся в cppworker через HTTP-header `X-Cpp-Ctx: <value>`.

**Пример (создать профиль с n_ctx=32768 для gemma-4):**

```bash
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-token" \
  -d '{
    "contextLength": 32768,
    "batchSize": 1024,
    "numGpuLayers": -1,
    "flashAttn": true,
    "notes": "Cline + long system prompt"
  }'
```

**Пример (применить — save + reload на всех бэкендах):**

```bash
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-token" \
  -d '{ "contextLength": 65536 }'
```

**Валидация профиля:** `contextLength` ∈ `[256, 262144]` (256K — нативный max для gemma-4), `batchSize >= 1`, `numGpuLayers >= -1` (`-1` = все слои).

## GGUF Backend Proxy API (через балансер)

Белансировщик предоставляет **универсальный proxy** для всех эндпоинтов CppWorker, чтобы WebUI и внешние клиенты могли общаться с CppWorker **через балансер**, не делая прямые HTTP-запросы (которые ломаются в Docker-окружении из-за CORS и недоступности `host.docker.internal` в браузере).

**Формат URL:**

```
/api/v1/gguf/backends/{backendId}/proxy/<cppworker-path>
```

**Примеры:**

| Запрос | Backend `llama_gpu` | CppWorker |
|---|---|---|
| `GET /api/v1/gguf/backends/llama_gpu/proxy/info` | llama_gpu | `GET /info` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/gpu` | llama_gpu | `GET /api/gpu` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/models` | llama_gpu | `GET /api/models` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/models/files` | llama_gpu | `GET /api/models/files` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/hf/search?query=...` | llama_gpu | `GET /api/hf/search?query=...` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/hf/files?modelId=...` | llama_gpu | `GET /api/hf/files?modelId=...` |
| `POST /api/v1/gguf/backends/llama_gpu/proxy/api/hf/download` | llama_gpu | `POST /api/hf/download` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/hf/progress?modelId=...&filename=...` | llama_gpu | `GET /api/hf/progress?modelId=...&filename=...` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/hf/downloads` | llama_gpu | `GET /api/hf/downloads` |
| `POST /api/v1/gguf/backends/llama_gpu/proxy/api/hf/cancel` | llama_gpu | `POST /api/hf/cancel` |
| `POST /api/v1/gguf/backends/llama_gpu/proxy/api/models/load` | llama_gpu | `POST /api/models/load` |
| `POST /api/v1/gguf/backends/llama_gpu/proxy/api/models/unload` | llama_gpu | `POST /api/models/unload` |
| `POST /api/v1/gguf/backends/llama_gpu/proxy/api/models/delete` | llama_gpu | `POST /api/models/delete` |

### Особенности

- **Проксирование прозрачно**: HTTP-метод, headers (включая `X-HF-Token`), query string и JSON body передаются в CppWorker без изменений.
- **Хост автоматически резолвится**: если бэкенд зарегистрирован с `host=host.docker.internal`, прокси заменяет его на `localhost` (в браузере `host.docker.internal` не резолвится).
- **Таймаут**: 90 секунд (HF search/download могут занимать до 60-90s, особенно search с параллельной загрузкой файлов).
- **Понятные сообщения об ошибках**: при сетевых проблемах возвращается `502 Bad Gateway` / `504 Gateway Timeout` с указанием адреса CppWorker.

### Примеры curl

```bash
# Получить версию CppWorker
curl http://localhost:18081/api/v1/gguf/backends/llama_gpu/proxy/info

# Скачать модель с HuggingFace (с HF token для gated репозиториев)
curl -X POST http://localhost:18081/api/v1/gguf/backends/llama_gpu/proxy/api/hf/download \
  -H 'Content-Type: application/json' \
  -H 'X-HF-Token: hf_xxxxxxxxxxxx' \
  -d '{"modelId":"TheBloke/Llama-2-7B-GGUF","filename":"llama-2-7b.Q4_K_M.gguf","revision":"main"}'

# Получить список активных загрузок
curl http://localhost:18081/api/v1/gguf/backends/llama_gpu/proxy/api/hf/downloads

# Удалить модель
curl -X POST http://localhost:18081/api/v1/gguf/backends/llama_gpu/proxy/api/models/delete \
  -H 'Content-Type: application/json' \
  -d '{"name":"llama-3-8b-q4_K_M.gguf"}'
```

### Коды ответов

| Код | Причина |
|---|---|
| 200 / 202 | Успешный ответ от CppWorker (статус и тело копируются дословно) |
| 400 | Невалидный URL (не указана секция `proxy/`) |
| 404 | Бэкенд с указанным ID не найден или имеет тип не `llama_cpp` |
| 502 | CppWorker недоступен (connection refused, host unresolvable) |
| 504 | CppWorker не отвечает (timeout) |
| Метод != GET/POST/DELETE/PUT | 405 Method Not Allowed |

> **Форматы стриминга:**
> - `/api/generate`, `/api/chat`, `/api/ollama/generate` → **NDJSON** (`application/x-ndjson`)
> - `/v1/chat/completions`, `/v1/completions` → **SSE** (`text/event-stream`) с финальным `[DONE]`

### Проксирование Ollama API

Балансировщик проксирует стандартные Ollama API endpoint'ы на порту `18080`. Все запросы к Ollama проходят через балансировщик с session stickiness, model affinity, queue management и retry/failover.

**Базовый URL:**
```
http://localhost:18080
```

| Endpoint | Метод | Описание | Особенности проксирования |
|----------|-------|----------|--------------------------|
| `/api/generate` | `POST` | Генерация текста | Session stickiness, streaming/SSE, retry до 3 попыток |
| `/api/chat` | `POST` | Чат | Session stickiness, streaming/SSE, retry до 3 попыток |
| `/api/embed` | `POST` | Эмбеддинги | Session stickiness, retry до 3 попыток |
| `/api/embeddings` | `POST` | Эмбеддинги (legacy) | Session stickiness, retry до 3 попыток |
| `/api/tags` | `GET` | Список моделей | **Агрегация** со всех бэкендов, дедупликация |
| `/api/version` | `GET` | Версия Ollama | Возвращает версию балансировщика |
| `/api/ps` | `GET` | Статус загруженных моделей | **Агрегация** со всех бэкендов |
| `/api/show` | `POST` | Информация о модели | Роутинг на бэкенд с загруженной моделью |
| `/api/create` | `POST` | Создание модели | Роутинг на бэкенд с макс. свободными ресурсами |
| `/api/pull` | `POST` | Загрузка модели | Роутинг на бэкенд с макс. свободными ресурсами |
| `/api/delete` | `DELETE` | Удаление модели | **Broadcast** на все бэкенды с моделью |
| `/api/copy` | `POST` | Копирование модели | Роутинг на бэкенд с исходной моделью |
| `/api/push` | `POST` | Публикация модели | Роутинг на бэкенд с моделью |

**Заголовки для управления сессиями:**
- `X-Client-ID` — приоритетный стабильный идентификатор клиента (рекомендуется)
- `X-Session-ID` — альтернативный идентификатор сессии
- `X-Client-Name` — имя клиента (Cline, OpenWebUI, etc.) для мониторинга

> **Примечание:** `X-Client-ID` без эфемерного порта гарантирует стабильность сессии при NAT. Если заголовки не переданы, используется IP без порта.

### Детальное описание Ollama endpoint'ов

#### GET /api/tags

Агрегация списка локальных моделей со всех доступных бэкендов (включая `healthy` и `degraded`). Модели дедуплицируются по полю `name` — если одна и та же модель присутствует на нескольких бэкендах, в итоговом списке она появляется один раз. Результат сортируется по имени для детерминированного порядка.

**Особенности проксирования:**
- Параллельные запросы ко всем бэкендам (timeout 5 секунд на каждый)
- Дедупликация по полю `name`
- Включаются `healthy` и `degraded` бэкенды

**Пример ответа:**

```json
{
  "models": [
    {
      "name": "llama3.1:8b",
      "model": "llama3.1:8b",
      "modified_at": "2024-06-15T10:30:00Z",
      "size": 4928300000,
      "digest": "sha256:abc123...",
      "details": {
        "family": "llama",
        "parameter_size": "8B",
        "quantization_level": "Q4_0"
      }
    },
    {
      "name": "qwen2.5:14b",
      "model": "qwen2.5:14b",
      "modified_at": "2024-06-15T11:00:00Z",
      "size": 8965234567,
      "digest": "sha256:def456...",
      "details": {
        "family": "qwen",
        "parameter_size": "14B",
        "quantization_level": "Q4_K_M"
      }
    }
  ]
}
```

#### GET /api/ps

Агрегация списка запущенных моделей со всех бэкендов. Показывает модели, которые в данный момент загружены в VRAM.

**Пример ответа:**

```json
{
  "models": [
    {
      "name": "llama3.1:8b",
      "size": 4928300000,
      "digest": "sha256:abc123...",
      "expires_at": "2024-06-15T11:00:00Z",
      "size_vram": 6000000000
    }
  ]
}
```

#### GET /api/version

Возвращает версию балансировщика + версии всех доступных бэкендов.

**Пример ответа:**

```json
{
  "version": "ollamalegion-1.0.0",
  "ollamaVersions": {
    "backend-1": "0.3.0",
    "backend-2": "0.3.0"
  }
}
```

#### POST /api/show

Информация о модели. Запрос маршрутизируется на бэкенд, где модель уже загружена (RunningModels). Если модель не найдена — fallback на любой healthy бэкенд.

**Запрос:**

```json
{
  "name": "llama3.1:8b"
}
```

**Особенности проксирования:**
- Роутинг через `findBackendWithModel()` → `RunningModels`
- Fallback на `selectAnyHealthy()` если модель не загружена

#### POST /api/create

Создание модели. Запрос маршрутизируется на бэкенд с максимальными свободными ресурсами.

**Запрос:**

```json
{
  "name": "custom-model",
  "modelfile": "FROM llama3.1:8b\nPARAMETER temperature 0.7"
}
```

**Особенности проксирования:**
- Роутинг через `selectBackendByResources()`
- SSE streaming ответ с прогрессом создания

#### POST /api/pull

Загрузка модели. Запрос маршрутизируется на бэкенд с максимальными свободными ресурсами.

**Запрос:**

```json
{
  "name": "llama3.1:8b"
}
```

**Особенности проксирования:**
- Роутинг через `selectBackendByResources()`
- SSE streaming ответ с прогрессом загрузки (status + completed/total)

#### DELETE /api/delete

Удаление модели со всех бэкендов, где она присутствует.

**Запрос:**

```json
{
  "name": "llama3.1:8b"
}
```

**Особенности проксирования:**
- **Broadcast** на все бэкенды с моделью (`findBackendsWithModel`)
- Параллельные запросы, возвращается первый успешный ответ
- Body закрывается для всех ненужных ответов (защита от утечки соединений)

#### POST /api/copy

Копирование модели. Запрос маршрутизируется на бэкенд с исходной моделью.

**Запрос:**

```json
{
  "source": "llama3.1:8b",
  "destination": "llama3.1:8b-custom"
}
```

**Особенности проксирования:**
- Роутинг через `findBackendWithModel(req.Source)`
- Body восстанавливается для проксирования (`readBody` + `io.NopCloser`)

#### POST /api/push

Публикация модели. Запрос маршрутизируется на бэкенд с моделью.

**Запрос:**

```json
{
  "name": "llama3.1:8b"
}
```

**Особенности проксирования:**
- Роутинг через `findBackendWithModel()`
- SSE streaming ответ с прогрессом публикации

### Формат данных

- **Request**: JSON
- **Response**: JSON
- **Content-Type**: `application/json`

### Коды ответов

| Код | Описание |
|-----|----------|
| `200` | Успешный запрос |
| `201` | Ресурс создан |
| `400` | Неверный запрос |
| `401` | Неавторизовано |
| `403` | Доступ запрещен |
| `404` | Ресурс не найден |
| `409` | Конфликт (ресурс уже существует) |
| `500` | Внутренняя ошибка сервера |
| `503` | Сервис недоступен |

---

## Аутентификация

API использует токен-аутентификацию на основе заголовка `X-API-Token`. Аутентификацию можно **включить или отключить** в конфигурации балансера (`config.json` → `auth.enabled`).

| Состояние auth | Поведение |
|----------------|-----------|
| `enabled: false` | Все endpoint'ы публичные, токен не требуется |
| `enabled: true` | Защищённые endpoint'ы требуют валидный токен в заголовке |

### Включение/отключение аутентификации

В `config.json`:

```json
{
  "auth": {
    "enabled": true,
    "tokens": ["your-master-token"],
    "headerName": "X-API-Token"
  }
}
```

- `enabled: false` — публичный доступ ко всем endpoint'ам (удобно для разработки и внутренних сетей)
- `tokens` — список валидных токенов; **первый токен** = master (используется для генерации новых токенов)
- `headerName` — имя HTTP-заголовка (по умолчанию `X-API-Token`)

### Матрица endpoint'ов и аутентификации

| Endpoint | Метод | Auth требуется | Примечание |
|----------|-------|----------------|------------|
| `GET /api/v1/health` | — | ❌ Нет | Публичный, используется WebUI для определения настроек |
| `GET /api/v1/ratelimit/status` | — | ❌ Нет | Публичный |
| `GET /api/v1/auth/status` | — | ✅ Да | Требует токен |
| `POST /api/v1/auth/token` | — | ✅ Master only | Генерация нового токена |
| `GET /api/v1/cluster` | — | ✅ Да | |
| `GET /api/v1/backends` | — | ✅ Да | |
| `POST /api/v1/backends` | — | ✅ Да | |
| `GET /api/v1/metrics` | — | ✅ Да | |
| `GET /api/v1/models` | — | ✅ Да | |
| `GET /api/v1/sessions` | — | ✅ Да | |
| `GET /api/v1/agents/stats` | — | ✅ Да | |
| `GET /api/v1/queue/stats` | — | ✅ Да | Статистика очереди запросов |
| `WebSocket /ws/metrics` | — | ✅ Если auth enabled | Токен передаётся через query parameter |

### Передача токена в HTTP

```bash
# cURL
curl -X GET http://localhost:18081/api/v1/cluster \
  -H "X-API-Token: your-api-token"

# Python requests
import requests
headers = {"X-API-Token": "your-api-token"}
response = requests.get("http://localhost:18081/api/v1/cluster", headers=headers)

# JavaScript fetch
fetch('http://localhost:18081/api/v1/cluster', {
  headers: {'X-API-Token': 'your-api-token'}
})
```

### Передача токена в WebSocket

Браузер **не поддерживает** установку произвольных HTTP-заголовков при создании WebSocket. Поэтому токен передаётся через **query parameter**:

```javascript
// Правильный способ
const ws = new WebSocket('ws://localhost:18081/ws/metrics?token=your-api-token');

// Неправильный — заголовки игнорируются браузером
const ws = new WebSocket('ws://...'); // new WebSocket(url, protocols) не поддерживает headers
```

На стороне сервера (`wsMetricsHandler`):
- Если `auth.enabled: false` → WebSocket upgrade выполняется без проверки токена
- Если `auth.enabled: true` → требуется непустой `?token=...`; токен валидируется через `Authenticate()`

---

## REST API Endpoints

### Health

#### GET /api/v1/health

Проверка здоровья API. **Не требует аутентификации** — используется WebUI и внешними health check'ами для определения состояния системы.

В ответе передаются метаданные, необходимые клиенту для корректного подключения:

| Поле | Тип | Описание |
|------|-----|----------|
| `status` | string | Состояние API: `"healthy"` |
| `timestamp` | string | ISO 8601 время на сервере |
| `version` | string | Версия API |
| `authEnabled` | boolean | `true` — аутентификация включена, `false` — отключена |
| `authHeader` | string | Имя заголовка для токена (по умолчанию `"X-API-Token"`) |
| `wsEndpoint` | string | Путь WebSocket endpoint'а (по умолчанию `"/ws/metrics"`) |

**Пример ответа при auth отключена:**

```json
{
  "status": "healthy",
  "timestamp": "2024-01-15T10:30:00Z",
  "version": "1.0.0",
  "authEnabled": false,
  "authHeader": "X-API-Token",
  "wsEndpoint": "/ws/metrics"
}
```

**Пример ответа при auth включена:**

```json
{
  "status": "healthy",
  "timestamp": "2024-01-15T10:30:00Z",
  "version": "1.0.0",
  "authEnabled": true,
  "authHeader": "X-API-Token",
  "wsEndpoint": "/ws/metrics"
}
```

> **💡 Принцип работы WebUI:** При загрузке dashboard сначала вызывается `GET /api/v1/health` **без токена**. Если `authEnabled: false` — приложение стартует сразу. Если `authEnabled: true` — показывается модальное окно для ввода токена.

---

### Cluster

#### GET /api/v1/cluster

Получение информации о состоянии кластера.

**Ответ:**

```json
{
  "timestamp": "2024-01-15T10:30:00Z",
  "totalBackends": 2,
  "healthyBackends": 2,
  "totalRequests": 1500,
  "activeRequests": 5,
  "queuedRequests": 0,
  "backends": [
    {
      "id": "gpu-1",
      "timestamp": "2024-01-15T10:30:00Z",
      "status": "healthy",
      "gpu": {
        "usagePercent": 45.5,
        "memoryTotal": 24576,
        "memoryUsed": 12000,
        "temperature": 65,
        "powerUsage": 250,
        "powerLimit": 450
      },
      "system": {
        "cpuUsagePercent": 30.2,
        "memoryTotal": 65536,
        "memoryUsed": 20000
      },
      "ollama": {
        "runningModels": [
          {"name": "llama3.1:70b", "vramUsage": 18000}
        ],
        "activeRequests": 3,
        "requestsPerSecond": 12.5
      }
    }
  ]
}
```

---

### Metrics

#### GET /api/v1/metrics

Получение метрик всех бэкендов кластера.

**Ответ:**

```json
{
  "timestamp": "2024-01-15T10:30:00Z",
  "totalBackends": 2,
  "healthyBackends": 2,
  "backends": [
    {
      "id": "gpu-1",
      "timestamp": "2024-01-15T10:30:00Z",
      "gpu": {...},
      "system": {...},
      "ollama": {...}
    }
  ]
}
```

#### GET /api/v1/metrics/{backend_id}

Получение метрик конкретного бэкенда.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `backend_id` | path | ID бэкенда |

**Ответ:**

```json
{
  "id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": {
    "usagePercent": 45.5,
    "memoryTotal": 24576,
    "memoryUsed": 12000,
    "memoryFree": 12576,
    "temperature": 65,
    "powerUsage": 250,
    "powerLimit": 450,
    "gpuClock": 1800,
    "memClock": 10000
  },
  "system": {
    "cpuUsagePercent": 30.2,
    "memoryTotal": 65536,
    "memoryUsed": 20000,
    "memoryFree": 45536,
    "diskTotal": 1000000,
    "diskUsed": 500000,
    "diskFree": 500000,
    "networkRX": 1048576,
    "networkTX": 524288
  },
  "ollama": {
    "runningModels": [
      {
        "name": "llama3.1:70b",
        "size": 70000000000,
        "vramUsage": 18000,
        "expiresAt": "2024-01-15T11:00:00Z",
        "digest": "sha256:..."
      }
    ],
    "activeRequests": 3,
    "totalRequests": 1500,
    "avgResponseTime": 250.5,
    "requestsPerSecond": 12.5
  }
}
```

---

### Backends

#### GET /api/v1/backends

Получение списка всех бэкендов.

**Ответ:**

```json
{
  "backends": [
    {
      "id": "gpu-1",
      "name": "GPU Server 1",
      "host": "192.168.13.66",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 1,
      "maxConcurrentRequests": 10,
      "labels": ["nvidia", "rtx4090"],
      "status": "healthy",
      "lastHealthCheck": "2024-01-15T10:30:00Z",
      "consecutiveFailures": 0,
      "activeRequests": 3
    }
  ],
  "total": 2
}
```

#### POST /api/v1/backends

Добавление нового бэкенда.

**Тело запроса:**

```json
{
  "id": "gpu-3",
  "name": "GPU Server 3",
  "host": "192.168.13.80",
  "ollamaPort": 11434,
  "agentPort": 18032,
  "weight": 1,
  "maxConcurrentRequests": 10,
  "labels": ["nvidia", "a100"]
}
```

**Ответ:**

```json
{
  "success": true,
  "backend": {...},
  "message": "Backend added successfully. Health check will run automatically."
}
```

#### GET /api/v1/backends/{backend_id}

Получение информации о бэкенде.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `backend_id` | path | ID бэкенда |

#### PUT /api/v1/backends/{backend_id}

Обновление параметров бэкенда.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `backend_id` | path | ID бэкенда |

**Тело запроса:**

```json
{
  "name": "GPU Server 1 (Updated)",
  "weight": 2,
  "maxConcurrentRequests": 20,
  "labels": ["nvidia", "rtx4090"]
}
```

**Ответ:**

```json
{
  "success": true,
  "backend": {...},
  "message": "Backend updated successfully"
}
```

#### POST /api/v1/backends/{backend_id}/reconfigure

Переформирование бэкенда с новыми переменными окружения (envVars). Требует `force: true` для подтверждения.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `backend_id` | path | ID бэкенда |

**Тело запроса:**

```json
{
  "envVars": {
    "OLLAMA_NUM_PARALLEL": "4",
    "OLLAMA_MAX_LOADED_MODELS": "2"
  },
  "force": true
}
```

**Ответ:**

```json
{
  "success": true,
  "message": "Reconfiguration initiated"
}
```

#### GET /api/v1/backends/{backend_id}/launch-config

Получение конфигурации запуска Ollama для бэкенда (runtime flags, env vars).

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `backend_id` | path | ID бэкенда |

**Ответ:**

```json
{
  "backend_id": "gpu-1",
  "launch_config": {
    "numGpuLayers": -1,
    "contextLength": 4096,
    "numParallel": 4,
    "numThreads": 8,
    "batchSize": 512,
    "cpuOnly": false,
    "flashAttention": false,
    "kvSize": 512,
    "tensorSplit": null,
    "mainGpu": 0
  },
  "env_vars": {
    "OLLAMA_NUM_PARALLEL": "4",
    "OLLAMA_MAX_LOADED_MODELS": "2"
  }
}
```

#### GET /api/v1/backends/{backend_id}/models

Получение списка моделей на конкретном бэкенде (RunningModels).

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `backend_id` | path | ID бэкенда |

**Ответ:**

```json
{
  "backend_id": "gpu-1",
  "models": [
    {
      "name": "llama3.1:8b",
      "digest": "sha256:abc123...",
      "size": 4928300000,
      "vram_usage": 6000000000,
      "expires_at": "2024-06-15T11:00:00Z"
    }
  ]
}
```

#### DELETE /api/v1/backends/{backend_id}

Удаление бэкенда.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `backend_id` | path | ID бэкенда |

**Ответ:**

```json
{
  "success": true,
  "id": "gpu-1",
  "message": "Backend removed successfully"
}
```

---

### Sessions

#### GET /api/v1/sessions

Получение списка активных сессий.

**Ответ:**

```json
{
  "sessions": [],
  "total": 0
}
```

#### DELETE /api/v1/sessions

Очистка всех сессий.

**Ответ:**

```json
{
  "success": true,
  "message": "All sessions cleared"
}
```

#### DELETE /api/v1/sessions/{session_id}

Удаление конкретной сессии.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `session_id` | path | ID сессии |

---

### Models

#### GET /api/v1/models

Получение списка запущенных моделей на всех бэкендах.

**Ответ:**

```json
{
  "models": {
    "gpu-1": ["llama3.1:70b", "mistral:7b"],
    "gpu-2": ["llama3.1:8b"]
  }
}
```

#### GET /api/v1/models/operations

Получение статуса активных операций с моделями (pull, create, delete).

**Ответ:**

```json
{
  "operations": [
    {
      "id": "pull-llama3.1-8b",
      "type": "pull",
      "model": "llama3.1:8b",
      "backend_id": "gpu-1",
      "status": "in_progress",
      "progress": 65,
      "started_at": "2024-01-15T10:30:00Z"
    }
  ],
  "total_active": 1
}
```

---

### Proxy Logs

#### GET /api/v1/proxy/logs

Получение логов проксированных запросов (HTTP access log).

**Ответ:**

```json
{
  "logs": [
    {
      "timestamp": "2024-01-15T10:30:00Z",
      "method": "POST",
      "path": "/api/generate",
      "model": "llama3.1:8b",
      "backend_id": "gpu-1",
      "status_code": 200,
      "duration_ms": 250,
      "client_ip": "192.168.1.100"
    }
  ],
  "total": 1500
}
```

---

### Candidates

#### GET /api/v1/candidates

Получение групп бэкендов-кандидатов по приоритетам для всех моделей. Используется в мониторе для отображения секции "Candidate Backends".

**Ответ:**

```json
{
  "models": {
    "llama3.1:8b": {
      "P1_loaded": ["gpu-1", "gpu-2"],
      "P2_warming": [],
      "P3_free": ["gpu-3"],
      "P4_fallback": ["gpu-4"]
    }
  }
}
```

| Приоритет | Описание |
|-----------|----------|
| `P1_loaded` | Бэкенды с уже загруженной моделью |
| `P2_warming` | Бэкенды, где модель подгружается |
| `P3_free` | Бэкенды со свободными ресурсами (можно загрузить) |
| `P4_fallback` | Все healthy бэкенды (resource-based scoring) |

---

### Agents

#### GET /api/v1/agents/stats

Получение статистики всех зарегистрированных агентов.

**Ответ:**

```json
{
  "totalAgents": 2,
  "healthyAgents": 2,
  "agents": [
    {
      "id": "gpu-1",
      "hostname": "gpu-server-1",
      "status": "healthy",
      "lastHeartbeat": "2024-01-15T10:30:00Z"
    }
  ]
}
```

#### GET /api/v1/agents/{agent_id}

Получение информации о конкретном агенте.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `agent_id` | path | ID агента |

---

### Agents (Registration & Metrics)

#### POST /api/v1/agents/register

Регистрация агента в системе балансировки. Агент отправляет этот запрос при запуске для автоматической регистрации в кластере.

**Аутентификация:** Требуется (API Token)

**Request Body:**
```json
{
  "agentId": "gpu-1",
  "hostname": "gpu-server-1",
  "host": "192.168.1.100",
  "ollamaPort": 11434,
  "agentPort": 18032,
  "gpuCount": 1,
  "name": "GPU Server 1",
  "labels": ["nvidia", "rtx4090"]
}
```

**Параметры запроса:**

| Параметр | Тип | Обязательный | Описание |
|----------|-----|--------------|----------|
| `agentId` | string | Да | Уникальный идентификатор агента |
| `hostname` | string | Нет | Имя хоста агента |
| `host` | string | Нет | IP-адрес агента (если не указан, используется hostname или RemoteAddr) |
| `ollamaPort` | int | Нет | Порт Ollama API (по умолчанию: 11434) |
| `agentPort` | int | Нет | Порт агента (по умолчанию: 18032) |
| `gpuCount` | int | Нет | Количество GPU |
| `name` | string | Нет | Отображаемое имя агента |
| `labels` | string[] | Нет | Метки для классификации агента |

**Response:** 201 Created (новый агент) или 200 OK (обновление существующего)
```json
{
  "success": true,
  "action": "created",
  "agentId": "gpu-1",
  "backend": {
    "id": "gpu-1",
    "name": "GPU Server 1",
    "host": "192.168.1.100",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "weight": 1,
    "maxConcurrentRequests": 10,
    "labels": ["nvidia", "rtx4090"],
    "status": "starting"
  },
  "message": "Agent registered successfully. Backend added to the pool."
}
```

**Пример запроса (cURL):**
```bash
curl -X POST http://localhost:18081/api/v1/agents/register \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-api-token" \
  -d '{
    "agentId": "gpu-1",
    "host": "192.168.1.100",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "name": "GPU Server 1",
    "labels": ["nvidia", "rtx4090"]
  }'
```

---

#### POST /api/v1/agents/metrics

Отправка метрик от агента балансировщику. Агент должен периодически отправлять метрики для мониторинга состояния.

**Аутентификация:** Требуется (API Token)

**Headers:**

| Header | Значение | Описание |
|--------|----------|----------|
| `X-Agent-ID` | string | ID агента (обязательно) |

**Request Body — Полная структура метрик от агента:**

> **Важно:** Поля `activeRequests`, `totalRequests`, `avgResponseTime`, `requestsPerSecond` в секции `ollama` возвращаются агентом как `0` и **переопределяются балансером** на основе внутреннего proxy-счётчика.

```json
{
  "id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": {
    "usagePercent": 45.5,
    "memoryTotal": 24576,
    "memoryUsed": 12000,
    "memoryFree": 12576,
    "temperature": 65,
    "powerUsage": 250,
    "powerLimit": 450,
    "gpuClock": 1800,
    "memClock": 10000
  },
  "system": {
    "cpuUsagePercent": 30.2,
    "memoryTotal": 65536,
    "memoryUsed": 20000,
    "memoryFree": 45536,
    "diskTotal": 1000000,
    "diskUsed": 500000,
    "diskFree": 500000,
    "networkRX": 1048576,
    "networkTX": 524288
  },
  "ollama": {
    "runningModels": [
      {
        "name": "llama3.1:70b",
        "size": 70000000000,
        "vramUsage": 18000,
        "expiresAt": "2024-01-15T11:00:00Z",
        "digest": "sha256:...",
        "family": "llama",
        "parameterSize": "70B",
        "quantization": "Q4_0"
      }
    ],
    "availableModels": [
      {
        "name": "mistral:7b",
        "size": 4000000000,
        "family": "mistral",
        "parameterSize": "7B",
        "quantization": "Q4_0"
      }
    ],
    "activeRequests": 0,
    "totalRequests": 0,
    "avgResponseTime": 0,
    "requestsPerSecond": 0,
    "maxModels": -1,
    "maxConcurrentRequests": -1,
    "freeSlots": 0
  }
}
```

**Response:** 200 OK
```json
{
  "status": "received"
}
```

**Пример запроса (cURL):**
```bash
curl -X POST http://localhost:18081/api/v1/agents/metrics \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-api-token" \
  -H "X-Agent-ID: gpu-1" \
  -d '{
    "id": "gpu-1",
    "timestamp": "2024-01-15T10:30:00Z",
    "gpu": {
      "usagePercent": 45.5,
      "memoryTotal": 24576,
      "memoryUsed": 12000
    },
    "system": {...},
    "ollama": {...}
  }'
```

---

### Predictions (Прогнозирование)

Балансер автоматически рассчитывает прогнозы критических состояний для каждого бэкенда на основе истории метрик (до 120 точек, окно 2 минуты).

#### GET /api/v1/predictions

Получение прогнозов для всех бэкендов.

**Ответ:**
```json
{
  "gpu-1": {
    "backendId": "gpu-1",
    "secondsToCritical": 300,
    "criticalReason": "vram",
    "gpuUsageTrend": 0.5,
    "vramUsageTrend": 2.1,
    "ramUsageTrend": 0.3,
    "freeSlotsTrend": -0.8,
    "requestCapacity": 65.5
  }
}
```

| Поле | Тип | Описание |
|------|-----|----------|
| `secondsToCritical` | float | Секунд до критического состояния. `-1` = нет угрозы |
| `criticalReason` | string | Причина: `gpu_usage`, `vram`, `ram`, `concurrent_requests`, `models_capacity`, `none` |
| `gpuUsageTrend` | float | Тренд загрузки GPU (%/мин) |
| `vramUsageTrend` | float | Тренд использования VRAM (%/мин) |
| `ramUsageTrend` | float | Тренд использования RAM (%/мин) |
| `freeSlotsTrend` | float | Тренд свободных слотов (штук/мин) |
| `requestCapacity` | float | Текущая загрузка бэкенда (0-100%) |

---

#### POST /api/v1/agents/heartbeat

Отправка heartbeat сигнала от агента для подтверждения активности.

**Аутентификация:** Требуется (API Token)

**Headers:**

| Header | Значение | Описание |
|--------|----------|----------|
| `X-Agent-ID` | string | ID агента (обязательно) |

**Request Body:** Не требуется (может быть пустым)

**Response:** 200 OK
```json
{
  "status": "ok"
}
```

**Пример запроса (cURL):**
```bash
curl -X POST http://localhost:18081/api/v1/agents/heartbeat \
  -H "X-API-Token: your-api-token" \
  -H "X-Agent-ID: gpu-1"
```

---

### Auth

#### GET /api/v1/auth/status

Получение статуса системы аутентификации.

**Ответ:**

```json
{
  "enabled": true,
  "headerName": "X-API-Token",
  "tokenCount": 3
}
```

#### POST /api/v1/auth/token

Создание нового API токена. Требует master token.

**Ответ:**

```json
{
  "success": true,
  "token": "newly-generated-token-here",
  "message": "Token generated successfully"
}
```

#### DELETE /api/v1/auth/token

Отзыв (удаление) API токена. Требует master token.

**Параметры:**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `token` | query/body | Токен для отзыва |

**Ответ:**

```json
{
  "success": true,
  "message": "Token revoked successfully"
}
```

---

### Queue

#### GET /api/v1/queue/stats

Статистика очереди запросов и dispatch-метрики балансировщика.

**Ответ:**

```json
{
  "current_size": 0,
  "max_size": 100,
  "processed_total": 15420,
  "avg_wait_time_ms": 12,
  "workers": 4,
  "timeout_sec": 30,
  "dispatch_by_affinity": 8750,
  "dispatch_by_load": 4520,
  "dispatch_by_config": 2150
}
```

| Поле | Тип | Описание |
|------|-----|----------|
| `current_size` | int | Текущее количество запросов в очереди |
| `max_size` | int | Максимальный размер очереди |
| `processed_total` | int64 | Всего обработано запросов |
| `avg_wait_time_ms` | int64 | Среднее время ожидания в очереди (мс) |
| `workers` | int | Количество worker-ов |
| `timeout_sec` | int | Таймаут ожидания бэкенда |
| `dispatch_by_affinity` | int64 | Запросов, направленных через **Model Affinity** (модель уже загружена на бэкенде) |
| `dispatch_by_load` | int64 | Запросов, направленных через **Resource-Aware** выбор (свободные ресурсы) |
| `dispatch_by_config` | int64 | Запросов, направленных по **весам/конфигурации** (fallback) |

> **💡 Как работает dispatch:**
> 1. **Model Affinity** — запрос идёт на бэкенд, где модель уже загружена (этапы 1–3 в `selectBackend`)
> 2. **Resource-Aware** — выбор по свободным ресурсам (GPU, VRAM, CPU) с prediction bonus
> 3. **Config/Weight** — fallback-выбор по весам бэкенда (когда `UseEnhancedScoring=false`)

---

### Replication (Model Replication Groups API)

Управление группами репликации моделей (Variant A). Позволяет настроить автоматическое масштабирование реплик моделей на бэкендах.

#### GET /api/v1/replication/groups

Получение списка всех групп репликации.

**Аутентификация:** Требуется (API Token)

**Ответ:**
```json
{
  "groups": [
    {
      "modelName": "llama3.1:70b",
      "minReplicas": 2,
      "maxReplicas": 4,
      "targetBackends": ["gpu-1", "gpu-2"]
    }
  ]
}
```

#### POST /api/v1/replication/groups

Создание новой группы репликации.

**Тело запроса:**
```json
{
  "modelName": "llama3.1:70b",
  "minReplicas": 2,
  "maxReplicas": 4,
  "targetBackends": ["gpu-1", "gpu-2"]
}
```

**Ответ:** 201 Created
```json
{
  "message": "group created",
  "group": { "...config..." }
}
```

#### GET /api/v1/replication/groups/{modelName}

Получение детальной информации о группе репликации для указанной модели.

**Ответ:**
```json
{
  "config": { "...group config..." },
  "states": [ "...instance states..." ],
  "stats": { "...group statistics..." }
}
```

#### PUT /api/v1/replication/groups/{modelName}

Обновление конфигурации группы репликации.

#### DELETE /api/v1/replication/groups/{modelName}

Удаление группы репликации.

**Ответ:**
```json
{
  "message": "group 'llama3.1:70b' deleted"
}
```

#### GET /api/v1/replication/stats

Получение расширенной статистики репликации.

**Ответ:**
```json
{
  "enabled": true,
  "groupCount": 2,
  "stats": [ "...group stats..." ],
  "autoReconcile": true
}
```

#### POST /api/v1/replication/reconcile

Принудительный запуск reconcile для всех групп репликации.

**Ответ:**
```json
{
  "message": "reconciliation triggered"
}
```

---

### Virtual Models (Virtual Model API)

Управление виртуальными моделями (Variant C) — разбиение модели на срезы (slices) и распределённый инференс.

#### GET /api/v1/virtualmodels

Получение списка всех виртуальных моделей.

**Аутентификация:** Требуется (API Token)

**Ответ:**
```json
{
  "enabled": true,
  "count": 2,
  "models": [ "...virtual model objects..." ]
}
```

#### GET /api/v1/virtualmodels/{name}

Получение статуса конкретной виртуальной модели.

**Ответ:**
```json
{
  "name": "gpt-large",
  "enabled": true,
  "slices": [...],
  "activeJobs": 3,
  "throughputPerSlice": 12.5
}
```

---

### Autopull (Automatic Model Pull)

Управление автоматической загрузкой моделей на бэкенды.

#### GET /api/v1/autopull

Получение конфигурации и статуса автопулла.

**Аутентификация:** Требуется (API Token)

**Ответ:**
```json
{
  "enabled": true,
  "maxConcurrent": 2,
  "pullTimeout": "10m",
  "retryCount": 3,
  "activePulls": [...]
}
```

#### PUT /api/v1/autopull

Обновление конфигурации автопулла.

**Тело запроса:**
```json
{
  "enabled": true,
  "maxConcurrent": 2,
  "pullTimeout": "10m",
  "retryCount": 3
}
```

**Ответ:**
```json
{
  "success": true,
  "config": {},
  "message": "Auto-pull configuration updated successfully"
}
```

#### GET /api/v1/autopull/status

Получение статуса активных и завершённых загрузок моделей.

**Ответ:**
```json
{
  "enabled": true,
  "activePulls": [],
  "totalActive": 0
}
```

---

### CORS (Cross-Origin Resource Sharing)

API поддерживает CORS для всех origin'ов. Дополнительно, WebSocket использует whitelist допустимых origin'ов:

| Origin | Разрешён |
|--------|----------|
| `http://localhost:3000` | ✅ |
| `http://localhost:8080` | ✅ |
| `http://localhost:18030` | ✅ |
| `http://localhost:18081` | ✅ |
| `http://127.0.0.1:18030` | ✅ |
| `http://127.0.0.1:18081` | ✅ |
| `same-origin` | ✅ |

**WebSocket `CheckOrigin`:** При подключении через браузер, origin проверяется по whitelist. Если origin не в whitelist'е — соединение отклоняется.

**HTTP CORS-заголовки (от `ServeHTTP`):**
```
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS
Access-Control-Allow-Headers: Content-Type, Authorization, X-Agent-ID, X-API-Token
```

### Rate Limit

#### GET /api/v1/ratelimit/status

Получение статуса rate limiter. Не требует аутентификации.

**Ответ:**

```json
{
  "tokens": 100.0,
  "max_tokens": 100,
  "refill_rate": 10.0
}
```

---

## WebSocket API

### GET /ws/metrics

WebSocket подключение для получения метрик в реальном времени.

**Протокол:** WebSocket
**Формат сообщений:** JSON
**Аутентификация:** Требуется токен в query параметре `?token=xxx`

### Аутентификация

Токен должен быть передан в query параметре URL. Это необходимо для защиты от DoS-атак, так как проверка аутентификации выполняется ДО WebSocket upgrade.

**Важно:** При отсутствии или невалидности токена сервер вернет HTTP 401 ошибку до установления WebSocket соединения.

### Подключение

```javascript
// Передача токена через query параметр
const ws = new WebSocket('ws://localhost:18081/ws/metrics?token=your-api-token');

ws.onopen = () => {
  console.log('Connected to metrics stream');
};

ws.onmessage = (event) => {
  const metrics = JSON.parse(event.data);
  console.log('Received metrics:', metrics);
  
  // metrics содержит:
  // - id: идентификатор бэкенда
  // - timestamp: время сбора метрик
  // - gpu: метрики GPU (usagePercent, memoryTotal, memoryUsed, temperature, etc.)
  // - system: системные метрики (cpuUsagePercent, memoryTotal, memoryUsed, etc.)
  // - ollama: метрики Ollama (runningModels, activeRequests, requestsPerSecond, etc.)
};

ws.onerror = (error) => {
  console.error('WebSocket error:', error);
};

ws.onclose = () => {
  console.log('Connection closed');
};
```

### Примеры подключения

```javascript
// Browser JavaScript
const token = 'your-api-token';
const ws = new WebSocket(`ws://localhost:18081/ws/metrics?token=${token}`);

// Node.js с библиотекой ws
const WebSocket = require('ws');
const token = 'your-api-token';
const ws = new WebSocket(`ws://localhost:18081/ws/metrics?token=${token}`);

// Secure WebSocket (если включен TLS)
const wss = new WebSocket(`wss://localhost:8443/ws/metrics?token=${token}`);
```

### Пример ответа WebSocket

```json
{
  "id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": {
    "usagePercent": 45.5,
    "memoryTotal": 24576,
    "memoryUsed": 12000,
    "memoryFree": 12576,
    "temperature": 65,
    "powerUsage": 250,
    "powerLimit": 450,
    "gpuClock": 1800,
    "memClock": 10000
  },
  "system": {
    "cpuUsagePercent": 30.2,
    "memoryTotal": 65536,
    "memoryUsed": 20000,
    "memoryFree": 45536,
    "diskTotal": 1000000,
    "diskUsed": 500000,
    "diskFree": 500000,
    "networkRX": 1048576,
    "networkTX": 524288
  },
  "ollama": {
    "runningModels": [
      {
        "name": "llama3.1:70b",
        "size": 70000000000,
        "vramUsage": 18000
      }
    ],
    "activeRequests": 3,
    "totalRequests": 1500,
    "avgResponseTime": 250.5,
    "requestsPerSecond": 12.5
  }
}
```

---

## Примеры запросов

### cURL

```bash
# Health check
curl http://localhost:18081/api/v1/health

# Cluster status
curl -X GET http://localhost:18081/api/v1/cluster \
  -H "X-API-Token: your-token"

# List backends
curl -X GET http://localhost:18081/api/v1/backends \
  -H "X-API-Token: your-token"

# Add backend
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-token" \
  -d '{
    "id": "gpu-3",
    "name": "GPU Server 3",
    "host": "192.168.13.80",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "weight": 1,
    "maxConcurrentRequests": 10
  }'

# Delete backend
curl -X DELETE http://localhost:18081/api/v1/backends/gpu-1 \
  -H "X-API-Token: your-token"

# Generate new token (requires master token)
curl -X POST http://localhost:18081/api/v1/auth/token \
  -H "X-API-Token: your-master-token"

# Revoke token
curl -X DELETE "http://localhost:18081/api/v1/auth/token?token=token-to-revoke" \
  -H "X-API-Token: your-master-token"
```

### Python

```python
import requests

# Настройка сессии с токеном
session = requests.Session()
session.headers.update({'X-API-Token': 'your-api-token'})

# Health check (без токена)
response = requests.get('http://localhost:18081/api/v1/health')
print(response.json())

# Cluster status
response = session.get('http://localhost:18081/api/v1/cluster')
print(response.json())

# List backends
response = session.get('http://localhost:18081/api/v1/backends')
print(response.json())

# Add backend
new_backend = {
    'id': 'gpu-3',
    'name': 'GPU Server 3',
    'host': '192.168.13.80',
    'ollamaPort': 11434,
    'agentPort': 18032,
    'weight': 1,
    'maxConcurrentRequests': 10
}
response = session.post('http://localhost:18081/api/v1/backends', json=new_backend)
print(response.json())

# Delete backend
response = session.delete('http://localhost:18081/api/v1/backends/gpu-1')
print(response.json())

# Generate token
response = session.post('http://localhost:18081/api/v1/auth/token')
print(response.json())
```

### JavaScript/Node.js

```javascript
const axios = require('axios');

const API_BASE = 'http://localhost:18081';
const TOKEN = 'your-api-token';

const api = axios.create({
  baseURL: API_BASE,
  headers: {
    'X-API-Token': TOKEN
  }
});

// Health check
async function healthCheck() {
  const response = await axios.get(`${API_BASE}/api/v1/health`);
  console.log(response.data);
}

// Cluster status
async function getClusterStatus() {
  const response = await api.get('/api/v1/cluster');
  console.log(response.data);
}

// List backends
async function listBackends() {
  const response = await api.get('/api/v1/backends');
  console.log(response.data);
}

// Add backend
async function addBackend(backend) {
  const response = await api.post('/api/v1/backends', backend);
  console.log(response.data);
}

// Delete backend
async function deleteBackend(id) {
  const response = await api.delete(`/api/v1/backends/${id}`);
  console.log(response.data);
}

// WebSocket connection (token required in query parameter)
const WebSocket = require('ws');
const TOKEN = 'your-api-token';
const ws = new WebSocket(`ws://${API_BASE}/ws/metrics?token=${TOKEN}`);

ws.on('message', (data) => {
  const metrics = JSON.parse(data);
  console.log('Received metrics:', metrics);
});
```

---

## OpenAPI спецификация

Полная OpenAPI 3.0.3 спецификация доступна в файле [`openapi.yaml`](openapi.yaml).

### Просмотр через Swagger UI

```bash
# Запуск Swagger UI через Docker
docker run -d -p 8080:8080 -e SWAGGER_JSON=/api/swagger.json \
  -v $(pwd)/docs:/api swaggerapi/swagger-ui

# Затем откройте http://localhost:8080
```

### Использование Swagger UI

1. Откройте https://editor.swagger.io
2. Загрузите файл `openapi.yaml`
3. Просматривайте и тестируйте API

---

## Сводная таблица endpoints

| Метод | Endpoint | Описание | Аутентификация |
|-------|----------|----------|----------------|
| `GET` | `/api/v1/health` | Health check | ❌ |
| `GET` | `/api/v1/cluster` | Статус кластера | ✅ |
| `GET` | `/api/v1/metrics` | Метрики всех бэкендов | ✅ |
| `GET` | `/api/v1/metrics/{id}` | Метрики бэкенда | ✅ |
| `GET` | `/api/v1/backends` | Список бэкендов | ✅ |
| `POST` | `/api/v1/backends` | Добавить бэкенд | ✅ |
| `GET` | `/api/v1/backends/{id}` | Информация о бэкенде | ✅ |
| `PUT` | `/api/v1/backends/{id}` | Обновить бэкенд | ✅ |
| `POST` | `/api/v1/backends/{id}/reconfigure` | Переформировать бэкенд | ✅ |
| `GET` | `/api/v1/backends/{id}/launch-config` | Конфиг запуска бэкенда | ✅ |
| `GET` | `/api/v1/backends/{id}/models` | Модели на бэкенде | ✅ |
| `DELETE` | `/api/v1/backends/{id}` | Удалить бэкенд | ✅ |
| `GET` | `/api/v1/sessions` | Список сессий | ✅ |
| `DELETE` | `/api/v1/sessions` | Очистить сессии | ✅ |
| `DELETE` | `/api/v1/sessions/{id}` | Удалить сессию | ✅ |
| `GET` | `/api/v1/models` | Запущенные модели | ✅ |
| `GET` | `/api/v1/models/operations` | Активные операции с моделями | ✅ |
| `POST` | `/api/v1/agents/register` | Регистрация агента | ✅ |
| `POST` | `/api/v1/agents/metrics` | Отправка метрик от агента | ✅ |
| `POST` | `/api/v1/agents/heartbeat` | Heartbeat сигнал агента | ✅ |
| `GET` | `/api/v1/agents/stats` | Статистика агентов | ✅ |
| `GET` | `/api/v1/agents/{id}` | Информация об агенте | ✅ |
| `GET` | `/api/v1/auth/status` | Статус аутентификации | ✅ |
| `POST` | `/api/v1/auth/token` | Создать токен | ✅ (master) |
| `DELETE` | `/api/v1/auth/token` | Отозвать токен | ✅ (master) |
| `GET` | `/api/v1/queue/stats` | Статистика очереди | ✅ |
| `GET` | `/api/v1/queue/details` | Детали очереди (pending+processing) | ✅ |
| `GET` | `/api/v1/queue/history` | История выполненных запросов | ✅ |
| `GET` | `/api/v1/predictions` | Прогнозы для всех бэкендов | ✅ |
| `GET` | `/api/v1/predictions/{id}` | Прогноз для бэкенда | ✅ |
| `GET` | `/api/v1/models/capacity` | Глобальная ёмкость моделей | ✅ |
| `GET` | `/api/v1/backends/{id}/capacity` | Ёмкость бэкенда | ✅ |
| `PUT` | `/api/v1/backends/{id}/limits` | Обновить runtime-лимиты бэкенда | ✅ |
| `GET` | `/api/v1/cluster/config` | Текущая конфигурация кластера | ✅ |
| `PUT` | `/api/v1/cluster/config` | Сменить алгоритм/настройки кластера | ✅ |
| `POST` | `/api/v1/admin/restart` | Перезапуск балансировщика | ✅ |
| `GET` | `/api/v1/proxy/logs` | Логи проксированных запросов | ✅ |
| `GET` | `/api/v1/candidates` | Группы кандидатов по моделям | ✅ |
| `GET` | `/monitor` | HTML-страница монитора | ✅ |
| `GET` | `/api/v1/ratelimit/status` | Статус rate limiter | ❌ |
| `GET` | `/api/v1/replication/groups` | Список групп репликации | ✅ |
| `POST` | `/api/v1/replication/groups` | Создать группу репликации | ✅ |
| `GET` | `/api/v1/replication/groups/{modelName}` | Детали группы репликации | ✅ |
| `PUT` | `/api/v1/replication/groups/{modelName}` | Обновить группу репликации | ✅ |
| `DELETE` | `/api/v1/replication/groups/{modelName}` | Удалить группу репликации | ✅ |
| `GET` | `/api/v1/replication/stats` | Статистика репликации | ✅ |
| `POST` | `/api/v1/replication/reconcile` | Принудительный reconcile | ✅ |
| `GET` | `/api/v1/virtualmodels` | Список виртуальных моделей | ✅ |
| `GET` | `/api/v1/virtualmodels/{name}` | Статус виртуальной модели | ✅ |
| `GET` | `/api/v1/autopull` | Конфигурация автопулла | ✅ |
| `PUT` | `/api/v1/autopull` | Обновить конфигурацию автопулла | ✅ |
| `GET` | `/api/v1/autopull/status` | Статус загрузок автопулла | ✅ |
| `GET` | `/ws/metrics?token=xxx` | WebSocket метрики | ✅ (token в query) |

---

## Следующие шаги

- [Troubleshooting](troubleshooting.md) — Решение проблем
- [Конфигурация](configuration.md) — Настройка аутентификации и TLS
- [Развертывание](deployment.md) — Production deployment
