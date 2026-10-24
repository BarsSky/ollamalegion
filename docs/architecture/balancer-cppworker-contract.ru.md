# Контракт API balancer ↔ cppworker

**Статус:** Авторитетная спецификация всего взаимодействия между `ollama-loadbalancer` и `cppworker`. Реализует: Раунд 36 (Фаза 1 структурной перестройки).

**Аудитория:** Все, кто изменяет `cmd/cppworker/**`, `internal/balancer/llamacpp_*`, `internal/balancer/proxy_request*` или любой клиентский код, который обращается к `/api/models/*`, `/v1/chat/completions`, `/api/chat`, `/api/generate` на стороне cppworker.

---

## 0. Версионирование и совместимость

### 0.1 Версия протокола
Контракт идентифицируется семантической версией `MAJOR.MINOR.PATCH` (например, `1.4.0`).
- Скачок **MAJOR** → ломающее изменение (поле удалено, статус-код изменён, байтовый формат изменён)
- Скачок **MINOR** → аддитивное изменение (новое необязательное поле, новый необязательный заголовок, новый эндпоинт)
- Скачок **PATCH** → только документация, без изменения кода

### 0.2 Матрица совместимости
- Balancer `N` ДОЛЖЕН поддерживать cppworker `N-1` и `N`
- cppworker `N` ДОЛЖЕН поддерживать balancer `N-1` и `N`
- Несовпадение версий логируется при старте, но не блокирует работу (деградированный режим)

### 0.3 Эндпоинт версии
- cppworker: `GET /api/version` возвращает `{"version": "1.4.0", "build": "<commit>"}`
- Balancer: `GET /api/v1/version` возвращает те же поля плюс `{"protocol_version": "1.4.0", "balancer_build": "<commit>", "compatible_with": ["1.3.0", "1.4.0"]}`

### 0.4 Журнал изменений
| Версия | Дата | Изменение |
|---------|------|--------|
| 1.4.0 | 2026-08-17 | Исходный формальный контракт (Раунд 36, Фаза 1) |
| pre-1.4 | 2026-08-13 | Неформальный контракт, сложившийся за 35+ раундов багфиксов |

### 0.5 Стиль API отдельного бэкенда (R50 + R51.2)

Когда бэкенд регистрируется в балансере (через `config.json` или POST `/api/v1/backends`), оператор выбирает стиль API, на котором этот бэкенд будет говорить:

```json
{
  "id": "cppworker-gpu-bundled",
  "type": "llama_cpp",              // engine type (informational)
  "apiStyle": "ollama-native",      // ← R51.2 explicit field; optional (auto-inferred from type)
  "host": "cppworker-gpu",
  "cppWorkerPort": 18092
}
```

**Допустимые значения `apiStyle`:**

| Значение | Что отдаёт бэкенд | Что может присылать клиент | Нужен ли перевод? |
|-------|----------------|------------------|-----------------------|
| `ollama-native` | `/api/*` (Ollama API) | `/api/*` (Ollama) | Нет (проброс как есть) |
| `ollama-native` | `/api/*` (Ollama API) | `/v1/*` (OpenAI-compat) | ДА — OpenAI→Ollama в запросе, обратно в ответе |
| `openai-compatible` | `/v1/*` (OpenAI API) | `/v1/*` (OpenAI-compat) | Нет (проброс как есть) |
| `openai-compatible` | `/v1/*` (OpenAI API) | `/api/*` (Ollama) | ДА — Ollama→OpenAI в запросе, обратно в ответе |

**R50 (вывод по типу) → R51.2 (явное поле):**

| Состояние поля | Поведение R50 | Поведение R51.2 |
|-------------|--------------|----------------|
| `apiStyle` не задан, `type: "ollama"` | Выводится → `ollama-native` | `Backend.EffectiveAPIStyle()` возвращает `ollama-native` (то же самое) |
| `apiStyle` не задан, `type: "llama_cpp"` | Выводится → `openai-compatible` | `Backend.EffectiveAPIStyle()` возвращает `openai-compatible` (то же самое) |
| `apiStyle: "ollama-native"` (при любом типе) | НЕ поддерживается | Явное переопределение; используется как есть |
| `apiStyle: "openai-compatible"` (при любом типе) | НЕ поддерживается | Явное переопределение; используется как есть |
| `apiStyle` задан недопустимым значением | НЕ поддерживается | HTTP 400 от POST/PUT `/api/v1/backends`. Внутренний помощник `EffectiveAPIStyle()` откатывается к выводу по типу (устойчивость state.json) |

**Где это в коде (R51.2):**
- Тип и константы: `pkg/types/backend_type.go:14-44` — `APIStyle`, `APIStyleOllamaNative`, `APIStyleOpenAICompatible`, `IsValidAPIStyle()`
- Метод: `pkg/types/backend_type.go:69-87` — `(*Backend).EffectiveAPIStyle()`
- Поле: `pkg/types/backend.go:53-58` — `Backend.ApiStyle` (JSON-тег `apiStyle,omitempty`)
- HTTP-обработчики: `internal/api/handlers_backends.go` — `addBackend` (~строка 320), `updateBackend` (~строка 528), `listBackends` (~строка 188)
- Тесты: `pkg/types/backend_type_test.go` — 21 сценарий на значения констант, валидацию, приоритет вывода, JSON round-trip, `omitempty`
- Пример: `config/backends.example.json` — по одной записи ollama-native и openai-compatible

**План R51.3+:** перевести слой маршрутизации (`proxy_request.go:isLlamaCppBackend`) на выбор маршрутизатора по `EffectiveAPIStyle()`, а не по `Type`. Добавляет путь «клиент Ollama → бэкенд OpenAI» (R52+). Модель описана в §2, поэтапный план миграции — в §8 документа [docs/archive/superpowers/specs/2026-08-19-balancer-api-routing-design.md](../archive/superpowers/specs/2026-08-19-balancer-api-routing-design.md).

**Контракт перевода:**
- Преобразование запроса Ollama→OpenAI: поля `Ollama ChatRequest` отображаются в `OpenAI ChatRequest` (model, messages, options → messages, max_tokens, temperature и т.д.)
- Преобразование запроса OpenAI→Ollama: обратное
- Преобразование ответа: отображается формат потокового и непотокового ответа с сохранением семантики `finish_reason`

Слой перевода находится в `internal/balancer/llamacpp_translate_req.go` и `llamacpp_translate_resp.go`. Сейчас он преобразует только случай, когда клиент использует `/v1/*`, а бэкенд — Ollama-native; обратное направление (клиент Ollama → бэкенд OpenAI) пока не реализовано, но устройство это позволяет.

---

## 1. Байтовый формат стриминга (КРИТИЧНО)

Этот раздел **нормативен**. Любое отклонение — баг.

### 1.1 SSE (Server-Sent Events) — используется в `/v1/chat/completions` и `/v1/completions`

#### 1.1.1 Заголовки ответа (ОБЯЗАТЕЛЬНО)
```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no
```

**Запрещённые заголовки:** `Transfer-Encoding: chunked`, `Content-Length: N`. SSE — это долгоживущий поток; соединение закрывается, когда сервер решит, что ответ закончен. Любой заголовок `Content-Length` или `Transfer-Encoding: chunked`, выставленный вручную в hijack-путях, неверен (Раунд 32 #10, #16, #17). Либо используйте `http.ResponseWriter` из Go (он делает это сам), либо не выставляйте ни тот, ни другой.

#### 1.1.2 Байтовая последовательность события (ОБЯЗАТЕЛЬНО)

Каждое SSE-событие выглядит ровно так:

```
data: <json>\n
\n
```

Два символа в конце — `\n\n` — часть **границы SSE-события**, и оба ДОЛЖНЫ присутствовать. Структура:
- Строка 1: `data: <json>`, завершённая одиночным `\n`
- Строка 2: пустая строка, завершённая одиночным `\n`

**Запрещённые последовательности:**
- `data: <json>` без завершающих `\n\n` (граница события не закрыта → клиент не может разобрать следующее событие)
- Голый `\n`, записанный как отдельное событие (это был баг Раунда 35e: `bufio.ReadBytes('\n')` возвращал завершающий `\n` как отдельный `[]byte{0x0A}`; балансер писал его как одно-байтовый chunked-чанк, что ломало строгие парсеры aiohttp)
- Один chunked-чанк, содержащий несколько SSE-событий (каждое событие должно быть в своём chunked-чанке, если нижележащий транспорт chunked)

#### 1.1.3 Завершение (ОБЯЗАТЕЛЬНО)

Финальное событие ДОЛЖНО быть ровно `data: [DONE]\n\n` (литеральная строка `[DONE]` как полезная нагрузка, а не JSON). После `[DONE]` сервер закрывает TCP-соединение.

**Запрещённое завершение:**
- Закрытие соединения без `[DONE]` (строгие клиенты поднимают `TransferEncodingError` или «unexpected EOF»)
- Отправка JSON-события `{"done": true}` без `[DONE]` (клиенты, совместимые с OpenAI, ожидают терминатор `[DONE]`)

#### 1.1.4 Требование к `finish_reason` (ОБЯЗАТЕЛЬНО)

Событие **непосредственно перед** `[DONE]` ДОЛЖНО содержать поле `choices[].finish_reason`, равное одному из:
- `"stop"` — естественное завершение
- `"length"` — достигнут `max_tokens`
- `"tool_calls"` — модель вызвала инструмент
- `"content_filter"` — заблокировано фильтром безопасности (зарезервировано, пока не используется)
- `"cancelled"` — запрос отменён (Раунд 31 #6: клиент отключился, балансер послал abort)

В последнем событии, отличном от `[DONE]`, `finish_reason` НЕ ДОЛЖЕН быть `null`, `""` или отсутствовать. Клиенты OpenAI опираются на это при учёте токенов.

#### 1.1.5 Инварианты уровня события (ОБЯЗАТЕЛЬНО)

Для каждого события между первым и последним (не считая `[DONE]`):
- Если `done: false` (вариант NDJSON) или терминатор ещё не пришёл (вариант SSE): событие ДОЛЖНО иметь непустой content, ИЛИ явный `finish_reason`, ИЛИ массив `tool_calls`. Событие «потерянного токена» (`content: ""` при `done: false`, без `tool_calls` и без `finish_reason`) — нарушение протокола. (Это находка Раунда 32 #8 по тестам OpenWebUI.)
- Событие `tool_calls` с `content: ""` допустимо (вызовы инструментов — это не «content»).
- Событие `reasoning_content` с `content: ""` допустимо (reasoning — это не «content»).

#### 1.1.6 Keepalive / heartbeat (ЖЕЛАТЕЛЬНО)

Для длинных потоков (>30 секунд без токена) серверу СЛЕДУЕТ отправлять keepalive, чтобы предотвращать таймауты на стороне клиента. Принятые форматы:
- SSE-комментарий: `: keepalive\n\n` (игнорируется всеми SSE-клиентами, корректен по спецификации HTML § 4.4)
- NDJSON-heartbeat: `{"done": false, "_hb": "<rfc3339nano>"}\n` (Раунд 32 #2 ввёл это для `/api/chat`)

**Запрещённый keepalive:**
- `data: {"keepalive": true}\n\n` — строгие клиенты пытаются разобрать это как настоящее событие
- Голый `\n` без строки SSE-комментария — это был баг Раунда 35e

### 1.2 NDJSON — используется в `/api/chat` и `/api/generate` (Ollama)

#### 1.2.1 Заголовки ответа (ОБЯЗАТЕЛЬНО)
```
HTTP/1.1 200 OK
Content-Type: application/x-ndjson
Cache-Control: no-cache
Connection: keep-alive
```

**Запрещено:** то же, что в 1.1.1.

#### 1.2.2 Байтовая последовательность события (ОБЯЗАТЕЛЬНО)

Каждое событие — ровно один JSON-объект, завершённый одиночным `\n`:
```
<json>\n
<json>\n
<json>\n
```

**Запрещённые последовательности:**
- Несколько JSON-объектов в одной строке
- Голый `\n` (пустая строка) без предшествующего heartbeat `{"done": false}`
- Терминаторы строк `\r\n` (Раунд 32 #20: балансер в некоторых путях ошибочно выдавал `\r\n`)

#### 1.2.3 Завершение (ОБЯЗАТЕЛЬНО)

Финальное событие ДОЛЖНО иметь `"done": true` и содержать либо полный ответ модели, либо объект ошибки. После `{"done": true, ...}` сервер закрывает TCP-соединение.

#### 1.2.4 Схема отдельного события (NDJSON /api/chat, ОБЯЗАТЕЛЬНО)

```json
{
  "model": "<model name>",
  "created_at": "<RFC 3339 timestamp>",
  "message": {
    "role": "assistant",
    "content": "<delta text or empty if reasoning or tool_calls>",
    "thinking": "<reasoning delta or empty>",
    "tool_calls": [<array of tool calls or empty>]
  },
  "done": false,
  "done_reason": null
}
```

Для **финального** события:
```json
{
  "model": "<model name>",
  "created_at": "<RFC 3339 timestamp>",
  "message": {
    "role": "assistant",
    "content": "<final text or empty>",
    "thinking": "<final reasoning or empty>"
  },
  "done": true,
  "done_reason": "stop" | "length" | "load" | "unload",
  "total_duration": <ns>,
  "load_duration": <ns>,
  "prompt_eval_count": <int>,
  "prompt_eval_duration": <ns>,
  "eval_count": <int>,
  "eval_duration": <ns>
}
```

Значения `done_reason`:
- `"stop"` — естественное завершение
- `"length"` — достигнут `num_predict`
- `"load"` — модель пришлось загрузить (этот ответ получен уже после загрузки)
- `"unload"` — модель была выгружена посреди ответа (редко; только при отмене клиентом во время загрузки)

---

## 2. Семантика асинхронной загрузки

### 2.1 Эндпоинты
- `POST /api/models/load` — основной, поддерживает синхронный и асинхронный режим через `?wait=` и `?waitTimeoutSec=`
- `POST /api/models/load-with-params` — принимает больше параметров, та же асинхронная семантика
- `POST /api/models/reload` — **УСТАРЕЛО для preflight** (возвращает 404, если модель сейчас не загружена). Используйте вместо него `/api/models/load`. (Исправление Раунда 35.)
- `GET /api/models/load/progress?model=<name>` — эндпоинт опроса
- `GET /api/models/load/progress/stream?model=<name>` — SSE-поток прогресса

### 2.2 Поведение по умолчанию (ОБЯЗАТЕЛЬНО)

Без query-параметров `POST /api/models/load` работает **асинхронно**. Он возвращает HTTP 202 Accepted с заголовком `Location`, указывающим на URL прогресса. Это поведение по умолчанию с Раунда 24.

### 2.3 Синхронный режим (?wait=true, ЖЕЛАТЕЛЬНО)

Если клиент добавляет `?wait=true&waitTimeoutSec=N`, сервер блокируется до `N` секунд в ожидании завершения загрузки, а затем возвращает либо 200 OK (с загруженной моделью), либо 202 Accepted со `status: "loading_after_timeout"`, если загрузка не успела завершиться.

Этот режим существует для **устаревших клиентов**, которые не реализуют опрос (например, простые клиенты Ollama). Новым клиентам СЛЕДУЕТ использовать асинхронный режим по умолчанию и опрашивать прогресс.

### 2.4 Тело ответа для 202 Accepted (ОБЯЗАТЕЛЬНО)

```json
{
  "status": "loading" | "loading_after_timeout",
  "name": "<model name>",
  "path": "<absolute file path>",
  "loadingSizeBytes": <int>,
  "estimatedLoadTimeMs": <int>,
  "progressUrl": "/api/models/load/progress?model=<urlencoded name>",
  "model": { /* ModelInfo object, may be partial */ },
  "message": "<human-readable>"
}
```

Заголовок `Location` ДОЛЖЕН быть выставлен в абсолютный URL: `http(s)://<host><progressUrl>` или корне-относительный `<progressUrl>`.

### 2.5 Тело ответа для 200 OK после загрузки (ОБЯЗАТЕЛЬНО)

```json
{
  "status": "loaded" | "already_loaded" | "dedup",
  "name": "<model name>",
  "path": "<absolute file path>",
  "model": { /* full ModelInfo */ }
}
```

`status: "dedup"` означает, что загрузку пропустили, потому что модель с тем же `path` уже была загружена под другим именем. (Исправление Раунда 32 #8 #2.)

### 2.6 Контракт опроса (ОБЯЗАТЕЛЬНО)

`GET /api/models/load/progress?model=<name>` возвращает **полный** список моделей с их текущим состоянием. Клиенты ДОЛЖНЫ искать в списке свою запрошенную модель. НЕЛЬЗЯ предполагать, что в ответе только одна модель.

Форма ответа:
```json
{
  "models": [
    {
      "name": "<model name>",
      "state": "unloaded" | "loading" | "loaded" | "unloading" | "error",
      "path": "<absolute file path>",
      "loadedContextSize": <int or null>,
      "loadedGpuLayers": <int or null>,
      "loadedKvCacheType": "f16" | "q8_0" | "q4_0" | null,
      "loadedFlashAttnType": -1 | 0 | 1 | null,
      "loadedUseMmap": true | false | null,
      "error": "<error message>" | null,
      "loadProgress": {
        "elapsedMs": <int>,
        "sizeBytes": <int>,
        "loadHistory": [{"timestampMs": <int>, "sizeBytes": <int>}, ...]
      } | null
    }
  ],
  "count": <int>
}
```

### 2.7 Машина состояний (ОБЯЗАТЕЛЬНО)

```
unloaded ──POST /load──> loading ──success──> loaded
                            │                    │
                            │                    └─POST /unload──> unloading ──> unloaded
                            │
                            └──failure──> error ──POST /load──> loading
```

`state: "loaded"` требуется для того, чтобы балансер счёл модель готовой к инференсу. `state: "loading"` — переходное состояние; балансер ДОЛЖЕН опрашивать прогресс, пока `state` не станет `"loaded"` или `"error"`.

### 2.8 Семантика URL прогресса (ОБЯЗАТЕЛЬНО)

`progressUrl` — относительный URL (корне-относительный путь со строкой запроса). Заголовок `Location` в ответах 202 ДОЛЖЕН быть абсолютным URL, собранным из `r.Host` и схемы, выведенной из `r.TLS`. Клиенты МОГУТ использовать любой из двух, но абсолютные URL предпочтительнее из соображений переносимости.

---

## 3. Запрос и ответ Chat / Generate

### 3.1 `/api/chat` (Ollama) — запрос (ОБЯЗАТЕЛЬНО)

```json
{
  "model": "<model name>",
  "messages": [
    {"role": "system" | "user" | "assistant" | "tool", "content": "<text>", "images": [<base64>] | null, "tool_calls": [...] | null, "tool_name": "<name>" | null}
  ],
  "stream": true | false,
  "format": "json" | null,
  "options": {
    "num_ctx": <int>,
    "num_predict": <int>,
    "temperature": <float>,
    "top_p": <float>,
    "top_k": <int>,
    "seed": <int>,
    "stop": [<string>],
    "repeat_penalty": <float>,
    "repeat_last_n": <int>,
    "mirostat": <int>,
    "mirostat_eta": <float>,
    "mirostat_tau": <float>,
    "tfs_z": <float>,
    "num_gpu": <int>,
    "main_gpu": <int>,
    "low_vram": true | false,
    "vocab_only": true | false,
    "use_mmap": true | false,
    "use_mlock": true | false,
    "num_thread": <int>
  },
  "keep_alive": "<duration>" | 0 | -1,
  "tools": [<Ollama tool schema>] | null
}
```

Неизвестные поля ДОЛЖНЫ отвергаться через `json.Decoder.DisallowUnknownFields()` (шаблон Раунда 24, применён повсеместно).

### 3.2 `/api/chat` (Ollama) — непотоковый ответ (ОБЯЗАТЕЛЬНО)

```json
{
  "model": "<model name>",
  "created_at": "<RFC 3339>",
  "message": {
    "role": "assistant",
    "content": "<final text or empty>",
    "thinking": "<final reasoning or empty>",
    "tool_calls": [<array> | null]
  },
  "done": true,
  "done_reason": "stop" | "length" | "load" | "unload",
  "total_duration": <ns>,
  "load_duration": <ns>,
  "prompt_eval_count": <int>,
  "prompt_eval_duration": <ns>,
  "eval_count": <int>,
  "eval_duration": <ns>
}
```

`message.content` ДОЛЖЕН быть конкатенацией всех потоковых дельт `content`. `message.thinking` ДОЛЖЕН быть конкатенацией всех потоковых дельт `thinking`. `tool_calls` ДОЛЖЕН быть объединённым финальным массивом вызовов инструментов.

**R113 (2026-10-13) — `arguments` вызова.** `tool_calls[].function.arguments`
обязан быть JSON-**ОБЪЕКТОМ** по схеме инструмента из запроса (Ollama-форма) или
JSON-строкой такого объекта (OpenAI-форма). Иначе клиент (Cline, OpenAI SDK,
ollama-python) не может выполнить вызов, считает сообщение финальным ответом и
завершает агентский цикл. Живые формы, которые приходили на стенд и приводились
к объекту по схеме: массив (`["dir"]` → `{"commands":["dir"]}`), голая строка
(`"c:\\path"` → `{"path":"c:\\path"}`), объект-обёртка (`{"args":{…}}`), двойное
кодирование. Разметка вызова (`<|tool_call>…`, `[TOOL_CALLS]`, JSON-форма) НЕ
должна попадать в `content` ни в одном чанке, включая случай, когда модель
повторяет вызов, присланный историей. Подробности и порядок проверки:
[`cline-agent-loop.md`](../features/cline-agent-loop.ru.md).

**R113 — запрос к загружаемой модели.** Пока модель грузится/перезагружается,
запрос ДОЛЖЕН ждать готовности (предел ожидания — `CPPWORKER_LOAD_WAIT_SEC`,
по умолчанию 900 с, или отмена/дедлайн клиента). Ответ
`{"done": true, "done_reason": "error"}` на transient-состояние запрещён: клиент
трактует его как завершённый ход. Если готовности не дождались — 503 +
`Retry-After`.

### 3.3 `/v1/chat/completions` (OpenAI) — запрос (ОБЯЗАТЕЛЬНО)

```json
{
  "model": "<model name>",
  "messages": [
    {"role": "system" | "user" | "assistant" | "tool", "content": "<text or array of content parts>", "name": "<name>" | null, "tool_calls": [...] | null, "tool_call_id": "<id>" | null}
  ],
  "stream": true | false,
  "stream_options": {"include_usage": true | false} | null,
  "temperature": <float> | null,
  "top_p": <float> | null,
  "n": 1,
  "max_tokens": <int> | null,
  "max_completion_tokens": <int> | null,
  "stop": <string> | [<string>] | null,
  "presence_penalty": <float> | null,
  "frequency_penalty": <float> | null,
  "seed": <int> | null,
  "logit_bias": <map> | null,
  "logprobs": true | false | null,
  "top_logprobs": <int> | null,
  "user": "<user id>" | null,
  "tools": [<OpenAI tool schema>] | null,
  "tool_choice": "auto" | "none" | "required" | {"type": "function", "function": {"name": "<name>"}} | null,
  "response_format": {"type": "json_object" | "text"} | null,
  "reasoning_effort": "low" | "medium" | "high" | null
}
```

`reasoning_effort` — нестандартное расширение OpenAI, поддерживаемое cppworker. Оно управляет тем, сколько модель рассуждает перед финальным ответом.

### 3.4 `/v1/chat/completions` (OpenAI) — потоковый ответ (ОБЯЗАТЕЛЬНО)

Каждое SSE-событие — это `data: <json>\n\n`, где JSON имеет форму:

```json
{
  "id": "<chatcmpl-XXXX>",
  "object": "chat.completion.chunk",
  "created": <unix epoch seconds>,
  "model": "<model name>",
  "choices": [
    {
      "index": 0,
      "delta": {
        "role": "assistant" | null,
        "content": "<delta or null>",
        "reasoning_content": "<delta or null>",
        "tool_calls": [<delta> | null]
      },
      "finish_reason": null | "stop" | "length" | "tool_calls" | "content_filter" | "cancelled"
    }
  ],
  "usage": null | {"prompt_tokens": <int>, "completion_tokens": <int>, "total_tokens": <int>}
}
```

`object: "chat.completion.chunk"` **ОБЯЗАТЕЛЕН** в каждом потоковом событии. В баге Раунда 32 #10 балансер выдавал в потоковых ответах `"object": "chat.completion"` (не-chunk вариант); это нарушение протокола.

Поле `usage` не равно `null` ТОЛЬКО если `stream_options.include_usage` равно true И это **последнее** событие перед `[DONE]`. В остальных случаях `usage` ДОЛЖНО быть `null` или отсутствовать.

### 3.5 `/v1/chat/completions` (OpenAI) — непотоковый ответ (ОБЯЗАТЕЛЬНО)

```json
{
  "id": "<chatcmpl-XXXX>",
  "object": "chat.completion",
  "created": <unix epoch seconds>,
  "model": "<model name>",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "<final text or null if reasoning or tool_calls>",
        "reasoning_content": "<final reasoning or null>",
        "tool_calls": [<array> | null]
      },
      "finish_reason": "stop" | "length" | "tool_calls" | "content_filter" | "cancelled"
    }
  ],
  "usage": {"prompt_tokens": <int>, "completion_tokens": <int>, "total_tokens": <int>}
}
```

Для непотоковых ответов требуется `object: "chat.completion"` (НЕ `.chunk`).

### 3.6 Формат массива `tool_calls` (ОБЯЗАТЕЛЬНО, оба API)

Каждый объект вызова инструмента:
```json
{
  "id": "<call_XXXX or generated>",
  "type": "function",
  "function": {
    "name": "<function name>",
    "arguments": "<JSON-encoded argument object as a string>"
  }
}
```

`function.arguments` — это **строка** с JSON, а не вложенный объект. Это частая клиентская ошибка, которую надо обрабатывать. (Исправление Раунда 32 #8 #1 разбирало gemma-4, выдававшую аргументы без кавычек вокруг ключей — `{"path": "/foo"}` против `{path: "/foo"}`; см. Раздел 8.)

### 3.7 `/api/generate` (Ollama) — запрос и ответ

Та же форма, что у `/api/chat`, но промпт — одна строка в поле `prompt` вместо `messages`. Поле `response` заменяет `message`.

### 3.8 Политика неизвестных полей (ОБЯЗАТЕЛЬНО)

Все JSON-запросы ДОЛЖНЫ разбираться через `json.NewDecoder(r.Body).DisallowUnknownFields()`. Это отлавливает клиентские баги с опечаткой в имени поля (например, `max_tokens` против `maxTokens`). Ответ с ошибкой ДОЛЖЕН быть HTTP 400 с телом `{"error": "json: unknown field \"<fieldname>\""}`.

Сейчас это реализовано для `handlers_cppworker_profiles.go:155-163` и ДОЛЖНО быть распространено на все обработчики в `cmd/cppworker/`.

**Исключение — эхо-поля reasoning из истории (0.7.67).** Поля, которые сервер сам выдаёт, а клиент присылает обратно внутри истории диалога, ДОЛЖНЫ приниматься (не 400), хотя для сборки промпта они не используются: `thinking` и `reasoning_content` в объектах сообщений `/api/chat` и `reasoning_content` / `thinking` в `/v1/chat/completions`. Их отвержение делает невозможным продолжение сохранённой сессии, потому что клиент не может вырезать то, что получил (живой дефект 2026-10-11: Cline → балансер `:18080`, `invalid JSON: json: unknown field "thinking"`). См. `cmd/cppworker/history_reasoning_echo_test.go`.

---

## 4. Содержимое рассуждений

### 4.1 Имя поля в зависимости от API (ОБЯЗАТЕЛЬНО)

| API | Имя поля рассуждений | Поле финального ответа |
|-----|---------------------|-------------------|
| Ollama `/api/chat` (NDJSON-стриминг) | `message.thinking` | `message.content` |
| Ollama `/api/chat` (непотоковый) | `message.thinking` | `message.content` |
| Ollama `/api/generate` | `response.thinking` | `response.content` |
| OpenAI `/v1/chat/completions` (потоковая дельта) | `delta.reasoning_content` | `delta.content` |
| OpenAI `/v1/chat/completions` (непотоковое сообщение) | `message.reasoning_content` | `message.content` |

OpenWebUI использует пути Ollama; Cline/Aider/Continue используют пути OpenAI. Балансер ДОЛЖЕН корректно переводить оба направления. (Раунд 29 исправил это — это был исходный баг «OpenWebUI не получает рассуждения».)

**R108 (2026-10-13) — поле и разбор.** cppworker выдаёт указанное выше каноническое имя и, в дополнение
только, устаревший псевдоним `message.reasoning` на нативном `/api/chat` (потоковые дельты и финальный чанк).
Клиенты, читающие любое из имён, получают рассуждения; больше ничего не должно зависеть от псевдонима.

Разделение на рассуждения/content **безусловно и определяется только парами тегов из §4.2** — никогда
не именем модели. `IsReasoningModel` / `IsReasoningEnabledForRequest` отвечают на вопрос «выдаёт ли эта модель
рассуждения вообще» (политика промпта, значения по умолчанию `n_predict`) и НЕ ДОЛЖНЫ служить вентилем для выдачи: живой дефект был
у модели вне этого белого списка (Qwen3.8-27B), чей блок ` thinking…` доезжал до Cline внутри
`message.content`. Проверка на уровне исходников (`TestR108_ResponsePathsDoNotGateReasoningByModelName`) валит
сборку, если такой вентиль возвращается в `handlers_chat.go`, `handlers_generate.go` или `handlers_openai.go`.

### 4.2 Распознавание тегов в сыром выводе модели (ответственность cppworker)

cppworker ДОЛЖЕН распознавать следующие пары тегов рассуждений в сыром выводе модели и направлять содержимое между каналами `thinking` и `content`:

| Пара тегов | Где используется |
|----------|---------|
| `<think>...</think>` | Qwen3, gemma-4 (рассуждения на английском) |
| `<thinking>...</thinking>` | Некоторые кастомные модели |
| `<reasoning>...</reasoning>` | Некоторые кастомные модели |
| `<analysis>...</analysis>` | Некоторые кастомные модели |
| `<\|channel>thought\n...\n<channel\|>` | gemma-4 (канонический chat template) |
| `<\|channel>thought...<channel\|>` | gemma-4 (без завершающего перевода строки) |
| `<\|channel>analysis\n...\n<channel\|>` | gemma-4 (вариант analysis) |
| `<\|channel>analysis...<channel\|>` | gemma-4 (analysis, без завершающего перевода строки) |
| `<\|channel>...<channel\|>` | gemma-4 (голый формат канала) |
| `<\|think>...<think\|>` | Qwen-стиль с пайп-разделителями |

Порядок распознавания ДОЛЖЕН быть таким: сначала самый длинный префикс (например, `<|channel>thought\n` раньше `<|channel>thought`, а тот раньше голого `<|channel>`). Иначе голый шаблон жадно совпадёт с префиксом более специфичного шаблона.

Постобработка `stripReasoningTags` в балансере ДОЛЖНА обрабатывать те же самые пары тегов (защита в глубину).

### 4.3 Обработка ведущих и хвостовых пробельных символов (ОБЯЗАТЕЛЬНО)

Если за блоком рассуждений следует content, модель может выдать ведущий `\n` как разделитель после `</think>`. Функция `stripReasoningTags` в балансере НЕ ДОЛЖНА сливать этот `\n` в пробел, НЕ ДОЛЖНА срезать ведущие переводы строк у легитимного content и НЕ ДОЛЖНА схлопывать `\n\n` в `\n` (исправление Раунда 32 #20).

Прежнее «защитное» поведение со схлопыванием пробельных символов было корневой причиной «блоки кода отрендерены одной строкой» в OpenWebUI. Защитная чистка НЕ нужна, потому что `SplitReasoningContent` в cppworker корректно определяет границы тегов.

### 4.4 Инструкция о рассуждениях с учётом языка (ответственность cppworker)

Когда в параметрах загрузки выставлен `enableThinking: true`, cppworker ДОЛЖЕН внедрить в системный промпт инструкцию, просящую модель рассуждать. Инструкция ДОЛЖНА быть на том же основном языке, что и последние сообщения пользователя. Алгоритм определения:

1. Просмотреть последние 4 сообщения пользователя/ассистента
2. Посчитать буквы кириллицы (U+0400–U+04FF) и латиницы (a-z, A-Z)
3. Если кириллицы ≥ 2 × латиницы → русский
4. Если латиницы > 0 → английский
5. Иначе → универсальная короткая инструкция (английский с оговоркой «respond in user's language»)

Порог 2× защищает от ложных срабатываний на английских технических терминах в русском тексте (HTML, CSS, JSON и т.п.). (Исправление Раунда 32 #12.)

---

## 5. Вызовы инструментов

### 5.1 Ответственность за разбор (cppworker)

cppworker ДОЛЖЕН разбирать вызовы инструментов из сырого вывода модели, даже если в теле запроса нет массива `tools: []`. Проверка «if `len(req.Tools) > 0`» неверна: многие клиенты (Cline, Aider, Continue) описывают инструменты в системном промпте и не присылают массив `tools`. Парсер ДОЛЖЕН работать по выводу модели безусловно. (Исправление Раунда 32 #8 #1.)

### 5.2 Обёртка тегами

Модель может выдавать вызовы инструментов в любом из следующих форматов:
- `<|tool_call|>{"name": "...", "arguments": {...}}</|tool_call|>` — канонический
- `<tool_call>{"name": "...", "arguments": {...}}</tool_call>` — вариант
- `<tool_call>{"name": "...", "arguments": {...}}</tool_call>` — вариант (Qwen)
- Голый JSON в выводе (стиль Hermes)

cppworker ДОЛЖЕН обрабатывать все варианты. Стратегия 1b `extractGemmaToolCalls` обрабатывает формат gemma-4, где имя функции стоит ПЕРЕД JSON-объектом: `<|tool_call|>name(json)</|tool_call|>`.

### 5.3 Нормализация аргументов

gemma-4 может выдавать свободный JSON без кавычек вокруг ключей: `{path: "/foo"}` вместо `{"path": "/foo"}`. cppworker ДОЛЖЕН применять нормализатор-автомат (`fixGemmaArgs`), который отслеживает состояние «внутри строки» и добавляет кавычки вокруг неэкранированных ключей. Нормализатор НЕ ДОЛЖЕН менять содержимое внутри строковых значений и НЕ ДОЛЖЕН менять числовые, булевы значения и null. (Исправление Раунда 32 #8 #1.)

### 5.4 Потоковые вызовы инструментов

В стриминге OpenAI вызовы инструментов выдаются серией дельт, каждая добавляет один фрагмент вызова (id, имя, затем фрагменты аргументов). Балансер ДОЛЖЕН объединять эти дельты в полный объект вызова инструмента, прежде чем пересылать его клиенту. (Существующая логика балансера в `llamacpp_translate_resp.go`.)

В стриминге Ollama поле `message.tool_calls` равно `null` для промежуточных событий и содержит полный массив в финальном событии.

---

## 6. Логика preflight (проверка балансера перед инференсом)

### 6.1 Матрица решений

Балансер выполняет preflight ПЕРЕД пересылкой любого запроса на инференс в cppworker. Дерево решений:

```
is_model_loaded_on_backend(model) ?
├── yes → params_match(current_params, request_params) ?
│        ├── yes → NOOP (forward to backend immediately)
│        └── no → RELOAD (background or sync, see 6.3)
└── no → LOAD (background or sync, see 6.2)
```

### 6.2 Модель не загружена → LOAD

1. POST `/api/models/load` (по умолчанию асинхронно)
2. Если 202: опрашивать `/api/models/load/progress`, пока `state: "loaded"` или `state: "error"`
3. Если 200: модель уже загружена (dedup или already_loaded)
4. Если ошибка: вернуть клиенту 503 с телом `{"error": "model load failed: <reason>", "model": "<name>"}`

### 6.3 Модель загружена, но параметры не совпадают → RELOAD

Reload означает «обновить рабочие параметры загруженной модели». Эндпоинт — `POST /api/models/reload` с новыми параметрами. Если reload возвращает 404 (модель тем временем выгрузили), нужно откатиться к `POST /api/models/load` (исправление Раунда 35).

### 6.4 Сравниваемые параметры (ОБЯЗАТЕЛЬНО)

Балансер ДОЛЖЕН сравнивать эти рабочие параметры. Несовпадение любого из них запускает перезагрузку:

| Параметр | Источник | Примечания |
|-----------|--------|-------|
| `n_ctx` / `num_ctx` / `contextSize` | профиль ИЛИ тело запроса | Размер контекста для конкретной модели |
| `kv_cache_type` | профиль ИЛИ тело запроса | `f16` / `q8_0` / `q4_0` |
| `flash_attn` | профиль ИЛИ тело запроса | `-1` (авто) / `0` (выкл) / `1` (вкл) |
| `use_mmap` | профиль ИЛИ тело запроса | bool |
| `gpu_layers` | профиль ИЛИ тело запроса | int, `-1` = все, `-2` = авто-подбор |

Исправление Раунда 32 #26 (коммит `bd9943b`) сравнивало только `n_ctx`. Теперь оно расширено на сравнение всех параметров. (Работа Раунда 36, Фаза 2.)

### 6.5 Выбор между reload и load

- Для явной перезагрузки админом/из UI (оператор знает текущее состояние): использовать `POST /api/models/reload`
- Для preflight (балансер не может доверять текущему состоянию): использовать `POST /api/models/load` (идемпотентен, покрывает все 3 случая: не загружена, другие параметры, те же параметры)

Это обходит баг Раунда 35, где preflight вызывал `/api/models/reload` и получал 404, когда cppworker был перезапущен с устаревшим кэшем метрик.

### 6.6 Асинхронный preflight с потоковым диалогом

Если запрос потоковый И preflight требует загрузки, балансеру СЛЕДУЕТ удерживать соединение открытым потоковым диалогом:
- Перехватить соединение (hijack)
- Отправлять SSE keepalive каждые 5 секунд
- Дождаться завершения загрузки
- Затем продолжить инференс

Это обходит `503 + Retry-After` (для которого клиент обязан реализовать логику повторов). Исправление Раунда 32 #26 добавило это для потоковых клиентов.

Если запрос непотоковый, вернуть `503 + Retry-After: <seconds>` и дать клиенту повторить. (Исправление Раунда 31 #2.)

### 6.7 Таймаут опроса

Максимальное ожидание загрузки у балансера вычисляется так:
```
maxWait = min(max(2 × estimatedMs + 60s, cap), clamp)
```
где `estimatedMs` — это `estimatedLoadTimeMs` из ответа 202 от cppworker, `cap` по умолчанию равен 900s (15 мин), а сам предел настраивается через `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` (clamp [5, 3600]).

Для MoE-моделей с частичным выносом оценка обычно ошибается в 2× — множитель надо подстраивать. (Исправление Раунда 35c.)

---

## 7. Цепочка отмены (клиент → балансер → cppworker → C-bridge)

### 7.1 Порядок распространения (ОБЯЗАТЕЛЬНО)

Когда клиент закрывает соединение (FIN или RST), отмена ДОЛЖНА распространяться в таком порядке, с максимальной задержкой 100ms на переход:

1. **Клиент → Балансер**: закрытие TCP обнаруживается средствами `net/http` из Go
   - При `ReadTimeout: 5s` (исправление Раунда 31 #6) `r.Context()` балансера отменяется в пределах 5s
   - При TCP keepalive (Раунд 31 #6) обнаружение происходит в пределах 6s на Linux, ~25s на Windows
2. **Балансер → cppworker**: балансер закрывает клиентское HTTP-соединение к cppworker
   - Реализовано в `proxy_request_hijack.go` через `polling goroutine` (Раунд 31 #6)
3. **cppworker → C-bridge**: `AbortWatcher` в cppworker (cmd/cppworker/abort_watcher.go) замечает `ctx.Done()` и вызывает `bridge.RequestAbort(handle)`
   - Задержка: 50-200ms
4. **C-bridge → llama_decode**: атомарный флаг отмены проверяется в начале каждого вызова `llama_decode`
   - Задержка: 1 цикл llama_decode (~100-500ms для типичного батча)

Итого: от ~6s (Linux) до ~30s (Windows), прежде чем отмена подействует на модель. Это жёсткий пол, заданный ОС; код приложения быстрее быть не может.

### 7.2 Ответ клиенту (ОБЯЗАТЕЛЬНО)

Когда отмена доходит до конца:
- Потоковый запрос: балансер отправляет `data: {"choices": [{"finish_reason": "cancelled"}]}\n\ndata: [DONE]\n\n` и закрывает соединение
- Непотоковый: балансер возвращает HTTP 200 с `{"choices": [{"finish_reason": "cancelled", "message": {"content": ""}}]}` (клиент может трактовать это как нормальное завершение)

Значение `finish_reason` равное `"cancelled"` было добавлено в Раунде 31 #6, чтобы отличать настоящее завершение от принудительной отмены. (Исправление Раунда 35e сделало это устойчивым к багу hijack с пустой строкой.)

### 7.3 Heartbeat для длинных потоков

Для потоков, которые могут превысить типичные клиентские таймауты (рассуждающие модели, 90s+), cppworker СЛЕДУЕТ отправлять keepalive каждые 5s. См. Раздел 1.1.6. (Исправление Раунда 32 #2.)

### 7.4 Контракт таймаутов

Значения по умолчанию для таймаутов балансера:

| Таймаут | Значение по умолчанию | Настраивается через | Назначение |
|---------|---------|------------------|---------|
| `LB_REQUEST_TIMEOUT_SEC` | 120 | env | общий таймаут запроса |
| `LB_STREAMING_IDLE_TIMEOUT_SEC` | 120 | env | таймаут между чанками (отдельно от общего) |
| `LB_STREAMING_FIRST_BYTE_TIMEOUT_SEC` | 30 | env | ожидание первого байта от cppworker |
| `LB_NCTX_RELOAD_TIMEOUT_SEC` | 300 | env | время ожидания перезагрузки |
| `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` | 900 | env | максимальное ожидание асинхронной загрузки |
| `LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER` | 2 | env | множитель для оценки cppworker |
| `LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC` | 60 | env | буфер после умножения |
| `LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC` | 5 | env | значение Retry-After для 503 |

Двухуровневый таймаут (общий + простой) критичен: если cppworker стримит, но застрял (нет новых токенов 2 минуты), срабатывает таймаут простоя. Если cppworker продвигается медленно, но ровно, срабатывает общий таймаут.

---

## 8. Антипромпты и поведение конкретных моделей

### 8.1 Настройка антипромптов

cppworker ведёт для каждой модели список антипромптов (токенов, сигнализирующих, что модель закончила ответ). Наборы по умолчанию:

- **gemma-2** (без рассуждений): `<end_of_turn>`, `<start_of_turn>user`, `<start_of_turn>model`
- **gemma-4** (с рассуждениями): `<start_of_turn>user`, `<start_of_turn>model` — **НЕ включать `<end_of_turn>`, потому что gemma-4 выдаёт этот токен как часть своего вывода рассуждений** (исправление Раунда 32 #27)
- **Qwen3**: `<|im_end|>`, `<|endoftext|>`
- **Llama-3**: `<|eot_id|>`, `<|end_of_text|>`

### 8.2 Почему gemma-4 отличается

Chat template gemma-4 оформляет блоки рассуждений спецтокенами `<|channel>` и `<|end|>`. Модель может выдавать эти токены буквальным текстом, когда рассуждает о структуре собственного вывода (например, «ответ должен заканчиваться тегом `</reasoning>`»). Если `<end_of_turn>` есть в списке антипромптов, модель останавливается посреди рассуждений на литеральном токене, и для пользователя это выглядит как «ответ обрезан на 85 токенах».

Исправление — включать только антипромпты с явными маркерами хода (`<start_of_turn>user`, `<start_of_turn>model`) и позволить `n_predict` или токенам конца генерации обработать остальное.

### 8.3 Контракт детокенизации

Вывод модели проходит через `llama_token_to_piece` с `special=true`. Результат ДОЛЖЕН быть:
- Корректным UTF-8
- Переводы строк (`\n`, U+000A) ДОЛЖНЫ быть настоящими переводами строк, а НЕ литеральными `\\n` (2 символа: обратный слэш + n)
- Табуляции (`\t`, U+0009) ДОЛЖНЫ быть настоящими табуляциями
- Спецтокены (например, `<|end_of_text|>`) ДОЛЖНЫ отфильтровываться (это ответственность cppworker, а не модели)

Раунд 32 #19 исправил баг детокенизации gemma-4 Q4_K_M, где выдавался `\\n` (2 символа) вместо `\n` (1 символ). Постпроцессор в `handlers_chat.go:281-310` заменяет любой `\\n` (2 символа) на `\n` (1 символ) перед пересылкой. Это **обходной путь**; долгосрочное решение — научить детокенизатор C-bridge обрабатывать артефакт квантизации Q4_K_M.

---

## 9. Модель ошибок

### 9.1 Тело ответа с ошибкой (ОБЯЗАТЕЛЬНО)

Все ответы с ошибкой от cppworker (и от эндпоинтов балансера, которые проксируют в cppworker) ДОЛЖНЫ использовать такую форму:

```json
{
  "error": {
    "message": "<human-readable>",
    "type": "<error class>",
    "code": "<machine-readable code>" | null,
    "param": "<parameter that caused the error>" | null
  }
}
```

Для эндпоинтов, совместимых с OpenAI, плоская форма `{"error": "<message>"}` также принимается ради обратной совместимости.

### 9.2 Коды состояния HTTP (ОБЯЗАТЕЛЬНО)

| Код | Когда |
|------|------|
| 200 | Успех (синхронный режим) |
| 202 | Принято (асинхронная загрузка, требуется опрос) |
| 400 | Некорректный запрос (невалидный JSON, отсутствует обязательное поле, неизвестное поле при DisallowUnknownFields) |
| 401 | Отсутствует или неверна аутентификация (несовпадение X-API-Token) |
| 403 | Профиль отключён, превышена квота, забанен |
| 404 | Модель не найдена, эндпоинт не найден |
| 409 | Конфликт состояний (например, выгрузка во время загрузки) |
| 413 | Запрос слишком велик (prompt > n_ctx и обрезать нельзя) |
| 429 | Превышен лимит частоты (возвращается Retry-After) |
| 500 | Внутренняя ошибка cppworker |
| 502 | Ошибка бэкенда (модели) во время инференса |
| 503 | Сервис недоступен (модель грузится, бэкенд недостижим) |
| 504 | Таймаут (превышен общий таймаут или таймаут первого байта) |

### 9.3 Семантика `Retry-After`

- 429 (лимит частоты): `Retry-After` — число секунд, которое клиент должен подождать
- 503 (модель грузится): `Retry-After` — оценочное оставшееся время загрузки
- 504 (таймаут): `Retry-After` обычно опускается; решение принимает клиент

---

## 10. Контракт аутентификации

### 10.1 Заголовок аутентификации cppworker (ОБЯЗАТЕЛЬНО)

cppworker принимает любой из двух вариантов:
- `Authorization: Bearer <token>` (устаревший, для бэкендов, зарегистрированных балансером)
- `X-API-Token: <token>` (предпочтительный, совпадает с `cfg.Auth.HeaderName` в cppworker)

cppworker ДОЛЖЕН проверять ОБА заголовка. (Раунд 24 исправил это для `nctx_reload.go`; Раунд 32 #8 должен был распространить на `handlers_cppworker_profiles.go`, но не сделал этого — работа Раунда 36, Фаза 2.)

Если переменная окружения `API_TOKEN` задана, запросы без любого из заголовков ДОЛЖНЫ отвергаться с 401.

### 10.2 Проброс аутентификации балансер → cppworker

Когда балансер проксирует запрос в cppworker, он ДОЛЖЕН переслать заголовок аутентификации из исходного запроса (если он есть) ИЛИ выставить его из сохранённого у бэкенда `CppWorkerApiToken` (из регистрации).

Если нет ни того, ни другого, балансер ДОЛЖЕН выставить `X-API-Token: <backend_stored_token>` (а не опускать заголовок). Это гарантирует, что круговой обмен работает, даже если клиент не прислал аутентификацию.

### 10.3 Изоляция между арендаторами

`user_id` в cppworker (из заголовка `X-User-Id` или из claim'а токена аутентификации) ДОЛЖЕН использоваться для:
- Лимитов параллельности на пользователя (`MaxParallelPerUser`)
- Журнала аудита
- Статистики расхода токенов на пользователя

Балансер НЕ ДОЛЖЕН смешивать user_id при пересылке запросов на общий бэкенд.

---

## 11. Распространение состояния (между компонентами)

### 11.1 Колбэки загрузки (cppworker → балансер)

Когда модель загружается или выгружается в cppworker, cppworker СЛЕДУЕТ уведомлять балансер через эндпоинты-колбэки:

- `POST <balancer>/api/v1/internal/llama-model-loaded` с телом `{"name": "...", "path": "...", "context_size": N, "gpu_layers": N, "kv_cache_type": "...", "flash_attn": N, "use_mmap": bool}`
- `POST <balancer>/api/v1/internal/llama-model-unloaded` с телом `{"name": "..."}`

Балансер ДОЛЖЕН обновлять свой кэш `lastKnownNCtx` и список `metrics.Models` по этим колбэкам. Это обходит баг Раунда 32 #15 / Раунда 33, где устаревший кэш метрик приводил к неверным решениям preflight после перезапуска cppworker.

Если колбэк не удался (например, балансер недостижим), cppworker ДОЛЖЕН залогировать ошибку, но НЕ повторять бесконечно (одной попытки достаточно; запасной вариант — поллер метрик с периодом 30s).

### 11.2 Поллер метрик (балансер → cppworker, запасной вариант)

Балансер периодически опрашивает метрики, вызывая `GET /api/models` на каждом бэкенде каждые 30s. Это **запасной** механизм по отношению к колбэкам из 11.1. Поллер обновляет `lastKnownNCtx`, `metrics.Models` и `metrics.LlamaCpp.LoadedModels`.

Оба механизма ДОЛЖНЫ быть активны; колбэки сокращают окно устаревания, поллер гарантирует eventual consistency.

---

## 12. Чек-лист реализации

Этот контракт обеспечивается:

- **Фаза 2 (типобезопасные модели)**: Go-структуры с `DisallowUnknownFields()` в каждом обработчике. См. `pkg/types/llamacpp_contract.go` (Раунд 36, Фаза 2).
- **Фаза 3 (тесты соответствия)**: golden-файлы в `testdata/cppworker_golden/*.json` фиксируют точные байтовые последовательности. См. `internal/balancer/contract_conformance_test.go` (Раунд 36, Фаза 3).
- **Фаза 4 (наблюдаемость)**: проброс X-Request-Id даёт сквозную трассировку. См. `internal/observability/request_id.go` (Раунд 36, Фаза 4).

Любое изменение протокола ДОЛЖНО обновлять:
1. Этот документ
2. Go-структуры из Фазы 2
3. golden-файлы из Фазы 3
4. CHANGELOG с записью «PROTOCOL: bump to 1.X.Y»

---

## 13. Ссылки

- Раунд 32 #1: теги рассуждений в канальном формате gemma-4
- Раунд 32 #8: голый шаблон `<|channel>` + дедупликация LoadModelWithOpts по пути к файлу
- Раунд 32 #9: повышение эвристики таймаутов потока/простоя
- Раунд 32 #10: убрать сломанный `Transfer-Encoding: chunked` (позже отменено)
- Раунд 32 #11: дальнейшее повышение таймаутов под длинные рассуждения
- Раунд 32 #12: инструкция о рассуждениях с учётом языка
- Раунд 32 #13: явные CODE RULES в инструкциях о рассуждениях
- Раунд 32 #16: SSE в chunked-обрамлении в hijack-пути
- Раунд 32 #17: граница SSE-события (`\n\n`) в chunked hijack
- Раунд 32 #18: исправить `\\n` в системном промпте
- Раунд 32 #19: обход детокенизатора gemma-4 Q4_K_M
- Раунд 32 #20: убрать схлопывание `\n\n` и обрезку ведущего `\n` в stripReasoningTags
- Раунд 32 #26: исправление preflight из 4 фаз
- Раунд 32 #27: удалён антипромпт `<end_of_turn>` для gemma-4
- Раунд 35: откат reload→load
- Раунд 35c: настраиваемый через env опрос асинхронной загрузки
- Раунд 35e: пустые строки SSE в hijack-пути
