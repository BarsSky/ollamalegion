# LlamaCpp Proxy Guide — особенности работы через балансер

## Обзор

Балансер ollamalegion поддерживает работу с llama.cpp бэкендами (cppworker) через полную
трансляцию Ollama API в OpenAI-совместимые эндпоинты. Это позволяет использовать любой
Ollama-совместимый клиент (OpenWebUI, Ollama CLI, etc.) с llama.cpp серверами.

## Архитектура

```
Клиент (OpenWebUI) → Балансер (ollamalegion) → CppWorker (llama.cpp)
     Ollama API           Трансляция форматов        OpenAI API
  /api/chat           →  /v1/chat/completions
  /api/generate       →  /v1/completions
  /api/tags           →  /v1/models (или метрики)
```

## Требования к запуску

### 1. CppWorker должен быть запущен с профилем

```bash
# GPU профиль:
docker compose -f deployments/docker-compose.full.yml --profile gpu up -d

# CPU профиль:
docker compose -f deployments/docker-compose.full.yml --profile cpu up -d
```

Без профиля cppworker не запускается, и балансер не сможет проксировать запросы.

### 2. WebUI nginx должен иметь доступ к cppworker

В `docker-compose.full.yml` переменные окружения для webui:
```yaml
CPPWORKER_HOST=cppworker-gpu   # имя контейнера в Docker-сети (или внешний хост)
CPPWORKER_PORT=18092           # порт внутри контейнера (или внешний)
```

Если cppworker запущен вне Docker-сети:
```yaml
CPPWORKER_HOST=192.0.2.20   # реальный IP хоста с cppworker
CPPWORKER_PORT=18091
```

## API Трансляция

### /api/chat → /v1/chat/completions

**Тело запроса (Ollama → OpenAI):**
- `model` — без изменений
- `messages` — без изменений (Ollama совместим с OpenAI)
- `stream` — без изменений
- `options.temperature` → `temperature`
- `options.top_p` → `top_p`
- `options.num_predict` → `max_tokens`
- `options.stop` → `stop`

**Ответ (OpenAI → Ollama, non-streaming):**
```json
{
  "model": "model-name",
  "message": {"role": "assistant", "content": "..."},
  "done": true,
  "eval_count": 123,
  "prompt_eval_count": 45
}
```

**Streaming:** Каждый SSE-чанк `data: {"choices":[{"delta":{"content":"..."}}]}` 
конвертируется в NDJSON `{"message":{"content":"..."},"done":false}\n`

### /api/generate → /v1/completions

**Ответ (OpenAI → Ollama):**
```json
{
  "model": "model-name",
  "response": "сгенерированный текст",
  "done": true
}
```

### /api/tags

Сначала собирается из метрик `LlamaCpp.LoadedModels` (быстро).
Если метрики пустые — балансер делает fallback-запрос `GET /v1/models` к первому
здоровому llama.cpp бэкенду и конвертирует ответ OpenAI (`{"data":[{"id":"model"}]}`)
в Ollama формат (`{"models":[{"name":"model"}]}`).

## Страница GGUF Models в WebUI

### Функциональность
- Отображает зарегистрированные llama.cpp бэкенды со статусом
- Показывает модели, загруженные на каждом бэкенде
- Фильтр выбора бэкенда через выпадающий список

### API эндпоинты
- `GET /api/v1/gguf/backends` — список llama.cpp бэкендов с метриками (проксируется через nginx к балансеру)
- `GET /api/worker/info` — прямое подключение к cppworker для тестирования соединения (через nginx)

### Известные ограничения
- `GET /api/worker/info` возвращает 502 если cppworker не доступен в Docker-сети webui контейнера 
  (например, при запуске без профиля `gpu` или `cpu`)
- Для работы страницы GGUF Models достаточно `/api/v1/gguf/backends` — прямое соединение с cppworker опционально

## Проксирование запросов от клиента

### Non-streaming запросы
Балансер читает Ollama-формат тела запроса, транслирует в OpenAI формат,
отправляет на cppworker, получает ответ, конвертирует обратно в Ollama формат.

### Streaming запросы
Балансер отправляет OpenAI-совместимый запрос на cppworker, читает SSE поток
(`data: {...}\n\n`), конвертирует каждый чанк в Ollama NDJSON формат и 
отправляет клиенту. В конце добавляет `{"done":true}\n`.

### Модель
Модель определяется из поля `model` в теле запроса и передаётся в контекст запроса.
Балансер выбирает подходящий бэкенд на основе того, где модель загружена
(используя метрики `LlamaCpp.LoadedModels`).

## Диагностика

### Проверка доступности cppworker
```bash
curl http://192.0.2.20:18091/health
curl http://192.0.2.20:18091/v1/models
```

### Проверка балансера
```bash
curl http://localhost:18081/api/v1/gguf/backends
curl http://localhost:18081/api/tags
curl -X POST http://localhost:18081/api/chat -H "Content-Type: application/json" \
  -d '{"model":"gemma","messages":[{"role":"user","content":"Hi!"}],"stream":false}'
```

### Логи балансера
```bash
docker logs ollama-legion-balancer -f | grep proxyRequestLlamaCpp