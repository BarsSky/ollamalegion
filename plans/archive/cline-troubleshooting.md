# Cline (VS Code) — диагностика и устранение проблем через OllamaLegion Balancer

> **Связанные документы:**
> - [cppworker-model-params.md](./cppworker-model-params.md) — **per-model profiles и 3-tier resolver n_ctx**
> - [llamacpp-proxy-guide.md](./llamacpp-proxy-guide.md) — архитектура проксирования
> - [troubleshooting.md](./troubleshooting.md) — общий troubleshooting
> - [api.md](./api.md) — справка по API

## Содержание

1. [Обзор проблемы](#обзор-проблемы)
2. [Быстрая диагностика](#быстрая-диагностика)
3. [Типичные симптомы и причины](#типичные-симптомы-и-причины)
   - [Пустой ответ / ошибка парсинга](#1-пустой-ответ--ошибка-парсинга)
   - [Cline зависает на "Loading..."](#2-cline-зависает-на-loading)
   - [Ответ приходит, но в нём мусор](#3-ответ-приходит-но-в-нём-мусор-вроде-end_of_turn)
   - [Cline получает ответ от неожиданной модели](#4-cline-получает-ответ-от-неожиданной-модели)
   - [Первая подсказка кода в Cline идёт мимо](#5-первая-подсказка-кода-в-cline-идёт-мимо)
4. [Что нужно знать о Cline](#что-нужно-знать-о-cline)
5. [Настройка cppworker для Cline](#настройка-cppworker-для-cline)
6. [Настройка chat template в GGUF](#настройка-chat-template-в-gguf)
7. [n_ctx overflow (контекст переполнен)](#n_ctx-overflow-контекст-переполнен)
8. [Тестирование](#тестирование)
9. [Чек-лист "Cline работает"](#чек-лист-cline-работает)

---

## Обзор проблемы

**Cline** (VS Code extension) использует библиотеку `ollama-js` (по умолчанию) или прямое OpenAI API для общения с LLM. Когда Cline подключён к **OllamaLegion Balancer** с cppworker (llama.cpp) на бэкенде, между клиентом и моделью появляется **слой трансляции протоколов**:

```
┌────────┐    Ollama /api/chat (NDJSON)    ┌───────────┐    OpenAI /v1/chat/completions (SSE)    ┌────────────┐
│  Cline │ ──────────────────────────────► │ Balancer  │ ─────────────────────────────────────► │ cppworker  │
│        │ ◄─────────────────────────────  │           │ ◄───────────────────────────────────── │ (llama.cpp)│
└────────┘    application/x-ndjson         └───────────┘    text/event-stream                   └────────────┘
```

Эта трансляция исторически была источником трёх основных проблем:

1. **Служебные токены** (`<end_of_turn>`, `<|eot_id|>` и т.д.) попадали в `content`, и парсер Cline падал.
2. **Unix timestamp** в `created` (от llama.cpp) вместо **RFC3339** (нужен Ollama-протоколу).
3. **Неполный chat template** в GGUF, из-за которого модель эмитит токены формата как обычный текст.

Все три исправлены в `internal/balancer/llamacpp_transport.go`.

---

## Быстрая диагностика

Запустите E2E тест маршрутизации:

```bash
python scripts/cline_routing_test.py --balancer http://localhost:18080 --verbose
```

**Если балансер недоступен:**

```
❌ T01_balancer_reachable — не удалось подключиться к http://localhost:18080
```

Проверьте, что балансер запущен:

```bash
curl http://localhost:18080/api/tags
docker ps | grep balancer
docker logs ollama-balancer --tail 50
```

**Если тест T03 падает на "Content-Type должен быть application/x-ndjson"** — балансер не транслирует протокол корректно. Проверьте логи:

```bash
docker logs ollama-balancer --tail 100 | grep -i "proxy\|cpp\|chat"
```

**Если тест T04 находит `<end_of_turn>`** — фильтр служебных токенов не сработал. Проверьте, что используется актуальная версия `llamacpp_transport.go` с `shouldFilterLlamaCppContent`.

**Если тест T05 находит `created_at` не в RFC3339** — не сработала `convertCreatedToRFC3339`. Проверьте, что версия `llamacpp_transport.go` свежая.

---

## Типичные симптомы и причины

### 1. Пустой ответ / ошибка парсинга

**Симптом в VS Code (Cline output):**
```
[ERROR] Failed to parse response from Ollama
[ollama-js] Unexpected end of JSON input
```

**Лог балансера:**
```
2026-06-03 22:14:01 200 1931ms POST /api/chat gemma-4-E4B-it-Q4_K_M ollama-js/0.5.18
```

Ответ 200 OK, но Cline не может его разобрать. **Причина:** один из трёх сценариев выше (служебные токены / Unix timestamp / неполный шаблон).

**Решение:**

1. Запустите `python scripts/cline_routing_test.py --verbose` — какой из T04/T05 упал, та и причина.
2. Если T04 — проверьте, что фильтр в `llamacpp_transport.go` не закомментирован.
3. Если T05 — проверьте, что `convertCreatedToRFC3339` применяется.
4. Если модель — Gemma и тест T04 падает с `<end_of_turn>` — пересохраните GGUF с правильным chat template (см. ниже).

### 2. Cline зависает на "Loading..."

**Симптом:** Cline показывает спиннер "Loading..." бесконечно, потом timeout.

**Причина:** Стрим идёт, но Cline его не закрывает. Обычно из-за того, что **last chunk** не содержит `"done": true` или пустой по `choices`.

**Решение:**

```bash
# Подключиться к cppworker напрямую и посмотреть последний чанк
curl -N http://localhost:18092/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}],"stream":true}' \
  | tail -c 500
```

Должно быть `data: [DONE]\n\n` в самом конце. Если нет — проблема в cppworker, а не в балансере.

### 3. Ответ приходит, но в нём мусор (вроде `<end_of_turn>`)

**Симптом:** Cline показывает ответ, в котором помимо нормального текста видны служебные токены:

```
Привет! Я — AI-ассистент, готовый помочь.<end_of_turn>
```

**Причина:** Модель не знает, где остановиться. Это значит, что в GGUF не встроен правильный **chat template**, и модель не различает user/assistant turn boundaries.

**Решение (по приоритету):**

1. **Пересохраните GGUF с правильным chat template** (см. [Настройка chat template в GGUF](#настройка-chat-template-в-gguf)).
2. **Если пересохранение не вариант** — установите `--chat-template` флаг при запуске llama.cpp сервера, передав шаблон в виде строки.
3. **Используйте другую модель** (для Gemma рекомендуется `gemma-3n-E4B-it-Q4_K_M` с правильным токенизатором).

**Дополнительно:** фильтр в балансере (см. `shouldFilterLlamaCppContent`) уже умеет убирать `<end_of_turn>`, `<|eot_id|>`, `<|im_end|>` и др. из ответа. Но это **костыль** — правильное решение — правильный chat template.

### 4. Cline получает ответ от неожиданной модели

**Симптом:** Cline настроен на `gemma-4-E4B-it-Q4_K_M`, но в ответе видны признаки другой модели (другой стиль, другие токены).

**Причина:** Backend selection ошибся — направил запрос на Ollama-бэкенд (где модель называется по-другому) или на cppworker с другой моделью.

**Решение:**

1. Проверьте `curl http://localhost:18080/api/tags` — какие модели видит балансер.
2. Проверьте `X-Backend-ID` заголовок в ответе (если балансер его пробрасывает):

```bash
curl -i http://localhost:18080/api/chat \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M","stream":false,"messages":[{"role":"user","content":"hi"}]}' \
  | head -20
```

3. Запустите тест T06 из `cline_routing_test.py` — он проверит session stickiness.

### 5. Первая подсказка кода в Cline идёт мимо

**Симптом:** Cline при добавлении файла в чат отправляет большой system prompt + кусок кода, но в ответе модель "не видит" код.

**Причина:** В llama.cpp `n_predict` слишком мал, и модель обрезает ответ на первом же токене, не успев "прочитать" system prompt.

**Решение:**

Увеличьте `num_predict` в настройках Cline (Ollama Provider → Context Window Size / Max Tokens).

В запросе от Cline это выглядит так:

```json
{
  "options": {
    "num_predict": 80   // ← вот это
  }
}
```

Увеличьте до 500-2000.

---

## Что нужно знать о Cline

### Используемая библиотека

Cline (>= 3.x) использует [`ollama-js`](https://github.com/ollama/ollama-js) для общения с Ollama-совместимыми серверами. На Windows это Node.js 22.x, на Mac/Linux — то же.

```bash
# Можно проверить версию в логах VS Code:
# Output → Cline → ...
# Должно быть что-то вроде: ollama-js/0.5.18 (x64 win32 Node.js/v22.22.1)
```

### User-Agent

Cline через ollama-js отправляет такой User-Agent:

```
ollama-js/0.5.18 (x64 win32 Node.js/v22.22.1)
```

Этот заголовок **пробрасывается** через балансер к cppworker. Полезно для отладки в логах.

### Что парсер Cline ожидает

Парсер `ollama-js` ждёт:

| Поле              | Тип      | Формат                  | Обязательно |
|-------------------|----------|-------------------------|-------------|
| `model`           | string   | точное имя модели       | да          |
| `created_at`      | string   | **RFC3339** (`2026-06-03T22:14:01Z`) | да          |
| `message.role`    | string   | `"assistant"`           | да          |
| `message.content` | string   | чистый текст            | да          |
| `done`            | bool     | true на последнем чанке | да          |
| `done_reason`     | string   | `"stop"`                | опц.        |

**Недопустимо в `message.content`:**
- `<end_of_turn>`, `<start_of_turn>` (Gemma)
- `<|eot_id|>`, `<|im_end|>` (Llama3 / ChatML)
- `<|end_of_text|>` (Qwen)
- Любые другие служебные токены формата чата

**Недопустимо в `created_at`:**
- Unix timestamp (число)
- Любой другой формат кроме RFC3339 / ISO 8601

---

## Настройка cppworker для Cline

### Минимальный конфиг `cppworker.example.env`:

```bash
# Порт cppworker (НЕ Ollama-порт)
CPPWORKER_PORT=18092

# Модель по умолчанию
DEFAULT_MODEL=gemma-4-E4B-it-Q4_K_M

# ВАЖНО: выставить правильный chat template
# Для Gemma (новые версии с begin_of_turn):
CHAT_TEMPLATE="<start_of_turn>user\n{{ .Prompt }}<end_of_turn>\n<start_of_turn>model\n"

# Контекст (Cline обычно шлёт длинные system prompts)
CONTEXT_SIZE=8192

# Сколько токенов генерировать (по умолчанию у Cline 80, можно увеличить)
MAX_PREDICT=2048

# Чтобы llama.cpp стартовал быстрее (preload)
PRELOAD_DEFAULT_MODEL=true
```

### Запуск llama.cpp сервера напрямую (без cppworker, для теста):

```bash
./llama-server \
  -m models/gemma-4-E4B-it-Q4_K_M.gguf \
  --port 18092 \
  --host 0.0.0.0 \
  --ctx-size 8192 \
  --chat-template-file chat-template-gemma.txt \
  --jinja  # включает грамматику chat template
```

### Запуск через cppworker (рекомендуется):

```bash
./cppworker \
  --config config/cppworker.json
```

В `cppworker.json`:

```json
{
  "default_model": "gemma-4-E4B-it-Q4_K_M",
  "port": 18092,
  "context_size": 8192,
  "max_predict": 2048,
  "chat_template": "<start_of_turn>user\n{{ .Prompt }}<end_of_turn>\n<start_of_turn>model\n"
}
```

---

## Настройка chat template в GGUF

### Что такое chat template

Chat template — это Jinja2-шаблон, который объясняет модели, как форматировать сообщения. Он встраивается в метаданные GGUF при конвертации.

### Проверить, есть ли chat template в GGUF:

```bash
# Используя llama.cpp:
./llama-cli --model models/gemma-4-E4B-it-Q4_K_M.gguf --info 2>&1 | grep -A 5 "chat template"
```

Если вывод пустой — **шаблона нет**, и модель будет эмитить токены формата в content.

### Правильный chat template для Gemma 3 / Gemma 3n:

```jinja
{{ bos_token }}
{% for message in messages %}
{% if message['role'] == 'user' %}
{{ '<start_of_turn>user\n' + message['content'] + '<end_of_turn>\n' }}
{% elif message['role'] == 'assistant' %}
{{ '<start_of_turn>model\n' + message['content'] + '<end_of_turn>\n' }}
{% elif message['role'] == 'system' %}
{{ '<start_of_turn>system\n' + message['content'] + '<end_of_turn>\n' }}
{% endif %}
{% endfor %}
{% if add_generation_prompt %}
{{ '<start_of_turn>model\n' }}
{% endif %}
```

### Для Llama-3.x:

```jinja
{{ bos_token }}
{% for message in messages %}
{% if message['role'] == 'system' %}
{{ '<|start_header_id|>system<|end_header_id|>\n\n' + message['content'] + '<|eot_id|>' }}
{% elif message['role'] == 'user' %}
{{ '<|start_header_id|>user<|end_header_id|>\n\n' + message['content'] + '<|eot_id|>' }}
{% elif message['role'] == 'assistant' %}
{{ '<|start_header_id|>assistant<|end_header_id|>\n\n' + message['content'] + '<|eot_id|>' }}
{% endif %}
{% endfor %}
{% if add_generation_prompt %}
{{ '<|start_header_id|>assistant<|end_header_id|>\n\n' }}
{% endif %}
```

### Как добавить chat template в существующий GGUF

```bash
# Скачать llama.cpp tools
git clone https://github.com/ggerganov/llama.cpp
cd llama.cpp

# Скомпилировать
make

# Сконвертировать GGUF с новым chat template
./llama-gguf-hash --in-place --chat-template chat-template-gemma.txt models/gemma-4-E4B-it-Q4_K_M.gguf

# Или используя Python gguf lib:
pip install gguf
python -c "
from gguf import GGUFWriter
writer = GGUFWriter('models/gemma-4-E4B-it-Q4_K_M.gguf.append', 'gemma-4-E4B-it')
with open('chat-template-gemma.txt') as f:
    writer.add_chat_template(f.read())
writer.write_header_to_file()
"
```

### Если GGUF трогать нельзя — передать шаблон через CLI:

```bash
./llama-server \
  -m models/gemma-4-E4B-it-Q4_K_M.gguf \
  --chat-template chat-template-gemma.txt \
  --port 18092
```

### Универсальный шаблон "на все случаи":

Если модель неизвестна, llama.cpp с флагом `--jinja` пытается угадать формат:

```bash
./llama-server -m model.gguf --jinja --port 18092
```

Но `--jinja` не всегда срабатывает корректно — лучше явно указывать `--chat-template`.

---

## n_ctx overflow (контекст переполнен)

**Симптом в VS Code (Cline output):**

```
[ERROR] n_ctx overflow: requested 24576, max 8192
[ERROR] Context size exceeded
```

Или в логах cppworker:

```
pre-flight check failed: ctx_capacity=8192 < required=24576
```

**Причина:** Cline прислал запрос с длинным system prompt + код, и **effective n_ctx** в llama.cpp меньше, чем нужно. `n_ctx` immutable после загрузки модели.

**Решение (рекомендуемое):** per-model profile + reload. Полная документация — [cppworker-model-params.md](./cppworker-model-params.md).

**Краткая версия:**

```bash
# 1. Создать/обновить профиль с большим n_ctx
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -d '{ "contextLength": 32768 }'

# 2. Применить (reload на всех бэкендах)
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply
```

Или через WebUI: **Settings → CppWorker → Model Profiles → Add Profile**, слайдер до 256K, **Apply**.

**Как работает resolver (3-tier, приоритет body > profile > backend):**

1. Если Cline шлёт `options.num_ctx` в body — это всегда побеждает (Tier 1).
2. Иначе берётся значение из per-model profile (Tier 2).
3. Иначе — per-backend default (`LLAMA_CTX_SIZE`).
4. Если ничего не задано — cppworker использует свой `defaultCtxSize`.

Balancer прокидывает резолв в cppworker через HTTP header `X-Cpp-Ctx: 32768`. cppworker применяет его к `params.NCtxOverride` через C-bridge.

**Допустимый диапазон:** `[256, 262144]` (256K — нативный max для gemma-4).

**Pre-flight check:** cppworker **до** загрузки модели проверяет, хватит ли VRAM (`n_ctx × layers × head_dim × 2 (fp16)`). Если нет — вернёт 500 с описанием.

---

## Тестирование

### Автоматический E2E тест

```bash
# Базовый запуск
python scripts/cline_routing_test.py

# С подробным выводом
python scripts/cline_routing_test.py --verbose

# На другой URL
python scripts/cline_routing_test.py --balancer http://192.168.1.10:18080

# С другой моделью
python scripts/cline_routing_test.py --model llama3.1:8b

# Не пропускать тесты при 404
python scripts/cline_routing_test.py --no-skip-on-404
```

Тест проверяет:

| # | Тест                              | Что проверяет                                |
|---|-----------------------------------|----------------------------------------------|
| T01 | balancer reachable               | Балансер запущен и отвечает                  |
| T02 | model available                  | Модель загружена                             |
| T03 | cline routes to cppworker        | POST /api/chat → /v1/chat/completions, NDJSON |
| T04 | no service tokens                | Нет `<end_of_turn>` и т.п. в content         |
| T05 | non-stream response               | created_at — RFC3339                         |
| T06 | session stickiness               | Несколько запросов → один бэкенд             |
| T07 | direct /v1/chat/completions      | OpenAI-формат тоже работает                  |
| T08 | user agent handled               | User-Agent ollama-js принимается             |

### Ручная проверка через curl

```bash
# 1. Проверить, что модель есть в /api/tags
curl http://localhost:18080/api/tags | python -m json.tool

# 2. Отправить запрос, имитирующий Cline
curl -N http://localhost:18080/api/chat \
  -H "Content-Type: application/json" \
  -H "User-Agent: ollama-js/0.5.18 (x64 win32 Node.js/v22.22.1)" \
  -d '{
    "model": "gemma-4-E4B-it-Q4_K_M",
    "stream": true,
    "messages": [{"role": "user", "content": "Привет"}]
  }'

# 3. Проверить created_at в non-stream
curl http://localhost:18080/api/chat \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemma-4-E4B-it-Q4_K_M",
    "stream": false,
    "messages": [{"role": "user", "content": "Привет"}]
  }' | python -m json.tool
```

### Сравнить "напрямую vs через балансер"

```bash
# Прямой запрос к cppworker
curl -N http://localhost:18092/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemma-4-E4B-it-Q4_K_M",
    "stream": true,
    "messages": [{"role": "user", "content": "Привет"}]
  }'

# Тот же запрос через балансер
curl -N http://localhost:18080/api/chat \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemma-4-E4B-it-Q4_K_M",
    "stream": true,
    "messages": [{"role": "user", "content": "Привет"}]
  }'
```

Различия должны быть только в формате (SSE ↔ NDJSON), но **содержимое должно совпадать**.

---

## Чек-лист "Cline работает"

Пошаговый чек-лист для проверки работоспособности:

- [ ] cppworker запущен, порт 18092 (или другой) отвечает
- [ ] Модель загружена: `curl http://localhost:18092/api/models` показывает её
- [ ] Балансер запущен, порт 18080 (или другой) отвечает
- [ ] Балансер видит модель: `curl http://localhost:18080/api/tags` показывает её
- [ ] E2E тест `cline_routing_test.py` все T01-T08 = ✅ (или SKIP для тех, что требуют загруженной модели)
- [ ] В логах балансера запрос от Cline User-Agent: `ollama-js/0.5.18`
- [ ] В логах cppworker запрос с тем же User-Agent (проброс)
- [ ] Cline в VS Code настроен:
  - API Provider: Ollama
  - Base URL: `http://localhost:18080` (порт балансера)
  - Model: `gemma-4-E4B-it-Q4_K_M` (точное имя)
  - Context Window: 8192+
  - Max Tokens: 500+
- [ ] Тест "Привет" в Cline возвращает осмысленный ответ за < 5 секунд
- [ ] Тест "Расскажи о себе" в Cline возвращает развёрнутый ответ без `<end_of_turn>` в середине

---

## Дополнительные материалы

- [llama.cpp server docs](https://github.com/ggerganov/llama.cpp/tree/master/examples/server)
- [ollama-js protocol reference](https://github.com/ollama/ollama-js)
- [Gemma chat templates](https://ai.google.dev/gemma/docs/formatting)
- [Llama-3 chat templates](https://llama.meta.com/docs/model-cards-and-prompt-formats/meta-llama-3/)

---

> **Если проблема не решается по этому гайду**, соберите следующую информацию и приложите к issue:
> 1. Вывод `python scripts/cline_routing_test.py --verbose`
> 2. `docker logs ollama-balancer --tail 200`
> 3. `docker logs ollama-cppworker --tail 200`
> 4. Прямой `curl` к cppworker и к балансеру (см. секцию "Ручная проверка")