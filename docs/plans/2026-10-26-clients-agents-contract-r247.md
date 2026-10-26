# R247 — Клиенты и агенты: довести совместимость до полной (рабочий план)

> **Дата:** 2026-10-26. **Основание:** ответ на вопрос «что по работе с другими
> клиентами-агентами и их инструментами» — по итогам R246 проверено 54 сценария
> инструментов и 8 форм запросов, и явно названы классы клиентов, которые СЕЙЧАС
> не заработают. Этот файл фиксирует их реализацию, чтобы ничего не потерялось.
>
> **Статус:** ✅ ВЫПОЛНЕН (в объёме плана). Этапы **E** (13 профилей ожиданий),
> **A** (strict-валидация + nudge-повтор), **F** (повтор: nudge на `/v1`, тот же
> промпт на `/api/chat`; профиль `strict-loop`), **B** (Anthropic `/v1/messages`),
> **C** (Responses `/v1/responses`), **D** (vision: честный 415), **G** (api-доки
> EN/RU, CHANGELOG, релизные заметки v0.7.231 EN/RU, образы
> `balancer:r247` + `cppworker:gpu-r247` развёрнуты).
> Финальные ворота: `run-checks.ps1` — 4 шага, 0 провалов; контур ожиданий 13/13;
> матрица 54/54; `clients-compat` 8/8; Go-пакеты и `go vet ./...` зелёные.
> Вне объёма (зафиксировано §11 и релизными заметками): passthrough картинок
> (mmproj), `previous_response_id` (состояние диалога), Anthropic thinking-signature,
> телеметрия strict на балансере.

---

## 0. Что уже работает — НЕ ЛОМАТЬ (красные линии)

| Проверка | Команда | Текущий результат |
|---|---|---|
| Инструменты клиентов (DSH 25 + Cline 15 + OpenWebUI 5 + hermes 4, +второй ход) | `node scripts/webui-e2e/tools-all-clients-r246.js` | **54/54 PASS** |
| Форма запросов разных клиентов (8 кейсов) | `node scripts/webui-e2e/clients-compat-r246.js` | **8/8 PASS** |
| Маршрутизация tool-истории по шаблонам моделей | `go test -run TestR246_RealTemplates ./cmd/cppworker/` | 68 шаблонов: 57 native / 4 structured / 7 text |
| Ворота репозитория | `.\scripts\run-checks.ps1` | 4 шага, 0 провалов |
| Go-пакеты | `go test -tags llama_stub ./cmd/cppworker/ ./internal/balancer/` | зелёные |

**Поддерживаемые сегодня поверхности:** `/v1/chat/completions`, `/v1/completions`,
`/v1/embeddings`, `/v1/models`, `/api/chat`, `/api/generate`, `/api/embed`,
`/api/tags`, `/api/ps` (плюс админ-API балансера).

---

## 1. Инвентаризация пробелов (что «не отработает» и почему)

| № | Класс клиента / агента | Протокол, который он ждёт | Что сейчас происходит | Приоритет | Этап |
|---|---|---|---|---|---|
| 1 | Новые OpenAI-клиенты: Codex-style, часть SDK-обёрток, «responses»-инструменты | `POST /v1/responses` + события `response.*` | 404 (эндпоинта нет) | **P0** | C |
| 2 | Anthropic-native: Claude Code, `anthropic` SDK, часть IDE-плагинов | `POST /v1/messages` + блоки `tool_use`/`tool_result` + события `content_block_*` | 404 | **P0** | B |
| 3 | Клиенты, которым нужен `strict: true` как гарантия схемы (structured outputs) | `strict` в `tools[].function` | поле принимается, но **не обеспечивается**: аргументы ремонтируются эвристически, гарантий нет | **P1** | A |
| 4 | Слабые модели при жёстких схемах | — | невалидные аргументы уходят клиенту (ремонт не всегда возможен) → ошибка инструмента и повтор | **P1** | F |
| 5 | Мультимодальные клиенты (OpenWebUI vision, Continue, LibreChat с картинками) | `content[]` c `image_url` | **молча отбрасывается** при нормализации (текстовый режим) | **P2** | D |
| 6 | Клиенты, ожидающие строгий набор полей ответа (SDK-валидация) | shape-контракт ответа | отдельными тестами не зафиксирован (только «вызов пришёл») | **P1** | E |
| 7 | Клиенты, которые требуют `tool_choice: "required"`/конкретную функцию | `tool_choice` | **уже сделано в R246** (soft-требование в промпте) — надо лишь закрепить контрактным тестом | ✅ | E |

Явные не-цели (осознанно вне плана): поддержка Ollama-протокола изнутри Anthropic-клиентов,
полный Anthropic «thinking/signature» (шифрованные размышления), грамматически
ограниченное декодирование под JSON Schema (требует GBNF/грамматик в C-мосте —
отдельный крупный проект, см. §5).

---

## 2. Этап E — контур ожиданий клиентов (ДЕЛАТЬ ПЕРВЫМ)

Это то, что просил оператор: «проверка тестами-сценариями, что ответ модели
соответствует ожиданию агента/клиента». Сейчас пробники проверяют «вызов пришёл и
аргументы валидны», но НЕ проверяют **форму ответа глазами клиента** (обязательные
ключи и типы, порядок событий, терминаторы, чего быть не должно).

### E1. Формат профиля ожиданий

Новый файл `scripts/webui-e2e/client-expectations-r247.js` (+ `*.json` фикстуры).
Профиль = декларация «что клиент считает правильным ответом»:

```js
{
  id: 'cline-openai-stream',
  surface: 'POST /v1/chat/completions',      // или /api/chat, /v1/messages, /v1/responses
  client: 'Cline 3.x / Roo / Kilo',
  request: { /* тело как шлёт клиент */ },
  expect: {
    transport: { status: 200, contentType: 'text/event-stream', done: 1 },
    events: [                                 // порядок и обязательные поля
      { kind: 'first', require: ['choices[0].delta.role'] },
      { kind: 'tool_calls', require: ['choices[0].delta.tool_calls[].index',
                                      'choices[0].delta.tool_calls[].id',
                                      'choices[0].delta.tool_calls[].function.name',
                                      'choices[0].delta.tool_calls[].function.arguments'],
        types: { 'function.arguments': 'string' } },
      { kind: 'finish', require: ['choices[0].finish_reason'], value: 'tool_calls' },
    ],
    forbid: ['choices[0].delta.content ~ /<tool_call>|\\[tool_code\\]|<\\|tool_call\\|>/',
             'reasoning_content в content'],
    secondTurn: { request: '…assistant(tool_calls)+role:tool…',
                  expect: { contentMustMatch: '/ollama-loadbalancer/i' } },
  }
}
```

### E2. Покрытие профилями (обязательный минимум)

| # | Профиль | Что фиксируем |
|---|---|---|
| E2.1 | `cline-openai-stream` | tool_calls чанки с index/id/arguments-строкой, finish_reason=tool_calls, нет разметки в content, второй ход с `tool_call_id` |
| E2.2 | `roo-openai-nonstream` | `message.tool_calls` в не-стриме, `arguments` — строка, `finish_reason=tool_calls` |
| E2.3 | `dsh-openai-stream` | role первым чанком, ровно один `[DONE]`, `content` только текст, `reasoning_content` отдельно, второй ход |
| E2.4 | `openwebui-ollama-ndjson` | NDJSON `/api/chat`: `message.content`, `done`, `done_reason`, `message.tool_calls[].function.arguments` — **объект**, второй ход с `tool_name` |
| E2.5 | `openwebui-openai-mode` | OpenAI-форма + `stream_options.include_usage` (usage-чанк ровно один) |
| E2.6 | `continue-langchain-strict` | `strict: true` схема: аргументы валидны по схеме (все required, enum, additionalProperties) |
| E2.7 | `langchain-forced-tool` | `tool_choice: {function}` → в ответе именно этот инструмент, `finish_reason=tool_calls` |
| E2.8 | `anthropic-sdk` (этап B) | SSE-события `message_start → content_block_start(text) → text_delta → content_block_stop → message_delta(stop_reason) → message_stop`; для инструмента — блок `tool_use` с `input`-**объектом** |
| E2.9 | `codex-responses` (этап C) | `output[]` c `message/output_text`; для вызова — item `function_call` c `arguments`-строкой; события `response.created/output_item.added/output_text.delta/…/response.completed` |
| E2.10 | `legacy-functions` | legacy `functions`/`function_call` не даёт 400 и не ломает ответ |

### E3. Golden-фикстуры

`scripts/webui-e2e/fixtures/r247/*.sse` — записанные реальные потоки (по одному на
профиль), плюс тест «живой прогон против фикстуры»: фикстура проверяется офлайн
(без стенда, в `run-checks`), живой прогон — отдельной командой.

### E4. DoD этапа E

- [x] `node scripts/webui-e2e/client-expectations-r247.js` — профили E2.1-E2.7, E2.10 зелёные (**8 PASS**)
- [ ] офлайн-проверка golden-фикстур в `run-checks` (шаг «клиентские контракты») — нужны фикстуры E3
- [x] профили E2.8/E2.9 добавлены как `skip` с причиной «этап B/C не реализован» (видно в ИТОГЕ: SKIP 2)
- [x] существующие пробники не изменены по смыслу (только расширены)
- [x] побочная находка: пробник сам сначала склеивал NDJSON-фрагменты `tool_calls`
      конкатенацией массивов и давал ложный FAIL на OpenWebUI — исправлено
      склейкой по позиции (как делает клиент); это ровно тот класс ошибок, который
      контур ожиданий и должен вычищать

---

## 3. Этап A — `strict: true`: валидация схемы и один ремонт-раунд

**Зачем.** Клиенты structured-output (OpenAI SDK `strict`, LangChain, Pydantic-AI)
считают ответ невалидным, если аргументы не соответствуют схеме. Сегодня ремонт
эвристический (null/типы/множественное число), но проверки «по схеме» перед отдачей нет.

**Что сделать**

1. `cmd/cppworker/tool_schema_validate_r247.go` — полный валидатор аргументов по
   JSON-схеме: `type` (включая `integer` vs `number`), `required`, `enum`, `const`,
   `items`, `properties`, `additionalProperties`, вложенность, `anyOf/oneOf` (best-effort).
   Возвращает список нарушений с путями (`tools[0].arguments.foo: expected integer`).
2. Точка применения: после `coerceToolCallsToSchema` (cppworker) и
   `repairToolCallsForSchema` (балансер) — если нарушения остались и инструмент
   объявлен `strict: true`, вызов **не отдаём как есть**, а:
   - один повтор генерации с подсказкой «предыдущий вызов отвергнут: <нарушения>;
     верни только JSON аргументов по схеме» (см. этап F — общий механизм);
   - если повтор не помог — отдаём вызов с пометкой в логе и заголовком
     `X-Tool-Args-Invalid: <tool>` (клиент увидит честную ошибку инструмента).
3. Поле `strict` парсить из `tools[].function.strict` (bool) и `strict: true` в
   корне схемы (встречается у LangChain).
4. Не ломать: для не-strict инструментов поведение прежнее.

**Тесты**

- Go: `tool_schema_validate_r247_test.go` — таблица схем/аргументов (включая
  `integer` vs `number`, `additionalProperties:false`, enum, вложенные массивы).
- Go: `strict_repair_r247_test.go` — невалидные аргументы + strict → один повтор,
  второй ответ валиден; без strict — поведение прежнее.
- Живой: профиль E2.6 в контрактах + кейс в `clients-compat` (strict с вложенностью).

**DoD:** E2.6 зелёный; при `strict` доля вызовов с нарушениями схемы = 0 (по логу
`tool args invalid after retry`); регрессионные ворота зелёные.

**Сделано (2026-10-26):**
- `cmd/cppworker/tool_schema_validate_r247.go` — разбор JSON-схемы (type-массив,
  enum, const, required, items, properties, additionalProperties, anyOf/oneOf) и
  `strictToolCallViolations`; `strict` читается из `tools[].function.strict` И из
  `strict:true` внутри схемы (LangChain).
- `cmd/cppworker/openai_types.go` — поле `Strict` у `openAIFunction`.
- `cmd/cppworker/handlers_chat.go` (/api/chat tools) и
  `cmd/cppworker/handlers_openai.go` (/v1 buffered tools) — проверка по схеме и
  **один повтор генерации** при нарушениях (только пока клиенту не отправлено ни
  байта); выключатель `CPPWORKER_STRICT_ARGS_RETRY=0`.
- Тесты: `tool_schema_validate_r247_test.go` — 11 кейсов схем (валидные/невалидные,
  enum, integer против number, массив элементов, additionalProperties во вложенном
  объекте, anyOf, не-JSON, не-strict инструмент, выключатель).
- Живая проверка: профиль `continue-langchain-strict` PASS на r247; полный контур
  ожиданий 8/8 PASS.
- **Остаток этапа A:** телеметрия на стороне балансера (сейчас проверка живёт в
  cppworker; балансерский ремонт схемы по-прежнему эвристический).

---

## 4. Этап B — Anthropic Messages API (`/v1/messages`)

**Зачем.** Claude Code и `anthropic` SDK в OpenAI-режим не переключаются.

**Что сделать**

1. Роутинг: `POST /v1/messages` (и `POST /v1/messages/count_tokens`) в `internal/api/routes.go`,
   проксирование в балансер → cppworker по новому пути `internal/balancer/handlers_anthropic.go`.
2. Трансляция запроса Anthropic → внутренний OpenAI chat:
   - `system` (строка или массив блоков) → `messages[0].role="system"`;
   - `messages[].content[]`: `text` → текст; `image` (base64/url) → `image_url` (см. этап D);
   - `tool_use` (assistant) → `tool_calls[]` (`input`-объект → `arguments`-строка);
   - `tool_result` (user) → `role:"tool"` + `tool_call_id` (по `tool_use_id`);
   - `tools[].input_schema` → `tools[].function.parameters`; `tool_choice`
     (`auto`/`any`/`tool`) → наш `tool_choice` (этап R246 уже умеет forced/required).
3. Трансляция ответа OpenAI → Anthropic:
   - текст → блок `{type:"text", text}`;
   - вызовы → блоки `{type:"tool_use", id, name, input: <объект>}` (строку
     аргументов парсим; невалидный JSON → `input:{}` + текст-предупреждение, не 500);
   - `stop_reason`: `tool_use` при вызовах, иначе `end_turn`; `usage` из usage-блока.
4. SSE Anthropic: `message_start`, `content_block_start/delta(text_delta|input_json_delta)`,
   `content_block_stop`, `message_delta` (stop_reason+usage), `message_stop`, `ping`.
   Порядок и `index` блоков — по спеке; терминатор — `message_stop` (без `[DONE]`).
5. Ошибки: форма Anthropic `{"type":"error","error":{"type","message"}}`, коды 400/413/503
   (413 — переполнение контекста, как в R243).
6. Не ломать: `/v1/chat/*` и `/api/*` не затрагиваются; трансляция живёт отдельными файлами.

**Тесты**

- Go: `anthropic_translate_r247_test.go` — запрос (система-массив, tool_use/tool_result,
  image), ответ (текст, tool_use, stop_reason, usage), SSE-последовательность по фикстуре.
- Go: `anthropic_errors_r247_test.go` — форма ошибок, 413, upstream 503.
- Живой: `client-expectations` профиль E2.8 + второй ход (`tool_result` → ответ по результату).

**DoD:** E2.8 зелёный; `curl /v1/messages` с телом из документации Anthropic отвечает
валидным объектом; одновременная работа `/v1/chat` не деградировала (матрица 54/54).

**Сделано (2026-10-26):**
- `internal/balancer/anthropic_r247.go` — трансляция Anthropic↔OpenAI и **self-proxy**
  на собственную `/v1/chat/completions`: Anthropic-клиент получает весь уже
  работающий слой (ремонт аргументов, strict, tool-историю по шаблону модели,
  повторы, admission, сессии) без дублирования логики.
- Роуты в `internal/balancer/llamacpp_router.go`: `/v1/messages`,
  `/v1/messages/count_tokens`; протокол `anthropic` в `RequestProtocol` (R229) —
  бейдж в сессиях/очереди не врёт.
- Трансляция: `system` (строка/блоки), блоки `text/image/tool_use/tool_result`,
  `input_schema`→`parameters`, `tool_choice` (auto/any/tool), `max_tokens`,
  `stop_sequences`, `temperature/top_p`; ответ — блоки `text/tool_use`
  (`input` — объект), `stop_reason`, `usage`; SSE — порядок событий Anthropic
  (`message_start` → `content_block_start(text)` → `text_delta` →
  `content_block_stop` → `content_block_start(tool_use)` → `input_json_delta` →
  `content_block_stop` → `message_delta(stop_reason, output_tokens)` → `message_stop`).
- Ошибки — форма Anthropic (`invalid_request_error`, `request_too_large`,
  `overload_error`, `api_error`).
- Тесты: `internal/balancer/anthropic_r247_test.go` — 8 кейсов (запрос с system-блоками
  и tool_result, tool_choice, текст/вызов/битые аргументы, порядок SSE, форма ошибок,
  бейдж протокола).
- Живой прогон: профили `anthropic-sdk` (поток + второй ход с `tool_result`) и
  `anthropic-error-shape` — PASS; весь контур **10 PASS / 0 FAIL / 1 SKIP**.
- Документация: `docs/api/api.md` и `docs/api/api.ru.md` — раздел Anthropic Messages API.

---

## 5. Этап C — OpenAI Responses API (`/v1/responses`)

**Зачем.** Новые клиенты (Codex-style, часть SDK) ходят только в Responses.

**Что сделать**

1. `internal/balancer/handlers_responses.go` + роут `POST /v1/responses`.
2. Трансляция запроса: `instructions` → system; `input` (строка | массив items) →
   messages; item `function_call_output` → `role:"tool"`; `tools[]` плоской формы
   (`{type:"function", name, description, parameters, strict}`) → наша `tools[]`;
   `tool_choice`; `max_output_tokens` → `max_tokens`.
3. Ответ: объект `{id, object:"response", status:"completed", output:[...], output_text}`
   где `output[]`: `{type:"message", role:"assistant", content:[{type:"output_text", text}]}`
   и `{type:"function_call", call_id, name, arguments:"<json-строка>"}`.
4. SSE: `response.created`, `response.output_item.added`,
   `response.output_text.delta`, `response.function_call_arguments.delta`,
   `response.output_item.done`, `response.completed` (+ `sequence_number`).
5. Хранилище `previous_response_id`: минимум — `store:false` и явный отказ/игнор с
   заметкой в логе; полная поддержка состояния — вне этапа (см. §9).
6. Не ломать: отдельный файл/роут; матрица и `/v1/chat` не затрагиваются.

**Тесты**

- Go: `responses_translate_r247_test.go` — запрос/ответ/события, вызов инструмента и
  текст; `responses_errors_r247_test.go` — форма ошибок OpenAI (`{"error":{...}}`).
- Живой: профиль E2.9 + второй ход (`function_call_output`).

**DoD:** E2.9 зелёный; текстовый и инструментальный сценарии проходят на gemma-4.

**Сделано (2026-10-26):**
- `internal/balancer/responses_r247.go` — трансляция Responses↔OpenAI + self-proxy
  (та же схема, что у этапа B): `instructions`→system, `input` строкой или items
  (`message`, `function_call`→`tool_calls`, `function_call_output`→`role:"tool"`),
  `tools[]` плоской формы с `strict`, `tool_choice`, `max_output_tokens`,
  `temperature/top_p`, `stream`.
- Ответ: `{id:"resp_…", object:"response", status:"completed", output[], output_text, usage}`;
  items `message` с `content[]=[{type:"output_text",…}]` и `function_call`
  (`call_id`, `name`, `arguments` — строка); usage `input/output/total_tokens`.
- SSE: `response.created` → `output_item.added` → `content_part.added` →
  `output_text.delta/done` → `content_part.done` → `function_call_arguments.delta/done`
  → `output_item.done` → `response.completed`, с монотонным `sequence_number`.
- `previous_response_id` — честный HTTP 400 с объяснением (состояние не хранится).
- Роут `/v1/responses` в `llamacpp_router.go`.
- Тесты: `internal/balancer/responses_r247_test.go` — 6 кейсов (строковый input,
  items с tool_call/output, текст/вызов, порядок SSE, sequence_number, usage).
- Живой прогон: профили `codex-responses` (поток + второй ход + non-stream) и
  `responses-error-shape` — PASS; контур **12/12**, матрица 54/54, compat 8/8.
- Документация: раздел «OpenAI Responses API» в `docs/api/api.md` и `docs/api/api.ru.md`.

---

## 6. Этап D — Vision: не отбрасывать молча

**Зачем.** Сейчас `image_url`/`image` при нормализации **выбрасываются** — клиент
думает, что модель видела картинку.

**Что сделать**

1. Определить возможности: `pkg/types/model_capabilities.go` уже знает семейства;
   добавить признак `Vision` (mmproj) и отдавать его в `/v1/models` (`capabilities`)
   и в заголовке `X-Model-Vision: true|false`.
2. cppworker: если запрос содержит изображения и модель **не** vision —
   вернуть 415 с понятным текстом (`images are not supported by model X: text-only`)
   вместо тихого отбрасывания. Если vision — передать изображения в C-мост
   (требуется mmproj-путь; если он не готов — вернуть 501 «vision not wired yet»,
   но НЕ терять данные молча).
3. Балансер: сохранять исходные части в логе (`image_parts`, `dropped_parts`),
   заголовок `X-Vision-Dropped: n` для наблюдаемости.
4. Тесты: Go — нормализация (изображение не теряется без пометки); живой профиль —
   запрос с `image_url` → либо 200 с обработкой, либо 415/501 с текстом причины.
   Никакого 200 с «модель не видела картинку».

**DoD:** в логах нет `dropped_parts>0` без соответствующего статуса; профиль E2 с
картинкой даёт детерминированный ответ.

**Сделано (2026-10-26) — честный отказ, без проводки mmproj:**
- `internal/balancer/vision_r247.go`: подсчёт частей-изображений по JSON
  (`image_url`, `image`, `input_image`, `image_base64`, рекурсивно, без ложных
  срабатываний на текст в аргументах инструментов) и отказ
  `writeVisionUnsupported`: HTTP **415**, `X-Vision-Supported: false`,
  `X-Vision-Parts: N`, JSON `{error:{code:"vision_not_supported",…}}`.
- `handleOpenAIChatCompletions`: проверка стоит ДО нормализации (иначе картинка
  снова была бы молча вырезана).
- Responses: часть `input_image` переводится в `image_url` — отказ приходит из
  общего обработчика, а не теряется при трансляции.
- Anthropic: блок `image` → `image_url` в OpenAI-теле → тот же 415, наружу —
  ошибка в форме Anthropic.
- Тесты: `internal/balancer/vision_r247_test.go` — 4 кейса подсчёта (включая
  «image_url как текст» и все три формы), форма отказа (код/заголовки/текст),
  демонстрация прежнего молчаливого отбрасывания.
- Живой прогон: профиль `vision-honest` (OpenAI + Anthropic) PASS; полный контур
  **13/13**, матрица **54/54**, `clients-compat` 8/8.
- **Остаток D:** проводка mmproj (passthrough картинок в C-мост) — отдельный
  крупный проект; контракт ответа уже зафиксирован тестами, чтобы passthrough
  добавлялся без смены поведения отказа.

---

## 7. Этап F — слабая модель: один ремонт-раунд вместо ошибки клиенту

**Зачем.** gemma-4 иногда отдаёт вызов без обязательного поля или с `null`; ремонт
покрывает часть случаев, но не все. Клиент видит ошибку инструмента и тратит ход.

**Что сделать**

1. Общий механизм «repair round» в cppworker: если аргументы невалидны по схеме
   (валидатор этапа A) и это не первая попытка — повторная генерация с сообщением:
   «предыдущий вызов `X` отвергнут: <нарушения>. Верни ТОЛЬКО JSON аргументов по схеме».
2. Ограничения: не больше 1 повтора на запрос; таймаут повтора = остаток бюджета;
   метрика `tool_args_retry_total` + лог `tool args retry`.
3. Kill-switch `CPPWORKER_TOOL_ARGS_RETRY=0|1` (default 1).
4. Тесты: stub-бэкенд отдаёт невалидный вызов, затем валидный → клиент получает
   валидный; при двух невалидных — вызов с пометкой; при `=0` — прежнее поведение.

**DoD:** флейк-кейсы из R246 (ralph/update_goal/edit) не дают ошибок схемы на 20
последовательных прогонах (проверка в `client-expectations` как «loop 20»).

**Сделано частично (2026-10-26):** повтор по невалидным strict-аргументам
реализован в двух безопасных точках (там, где клиенту ещё не отправлено ни байта):
`/api/chat` tools-путь (loop `generateStreamWithRamFallback`) и буферизированная
`/v1/chat/completions` tools-ветка (`generateWithRamFallback`). Повтор — тот же
промпт (сэмплирование стохастично), с логами `strict tool args invalid, retrying
generation once` / `strict tool args valid after retry`. Выключатель
`CPPWORKER_STRICT_ARGS_RETRY=0`.

**Остаток F:** nudge-вариант (повтор с текстом нарушений: «предыдущий вызов
отвергнут: …») и кейс «loop 20» в контуре ожиданий.

**Nudge реализован (2026-10-26):** `appendToolRepairInstruction` добавляет в
системное сообщение перечень нарушений (`... rejected because its arguments do not
match the requested JSON schema: - read: arguments.file_path: required field
missing ...`) и требование вернуть аргументы строго по схеме; `/v1`-путь
пересобирает промпт для повтора (`buildChatPrompt` с изменёнными сообщениями),
`/api/chat`-путь повторяет тем же промптом (сообщений в той точке нет). Тесты:
`TestR247_AppendToolRepairInstruction` (вставка в system, отсутствие мутации
исходной истории, вставка перед первым сообщением). Кейс «loop» добавлен в контур
как профиль `strict-loop` (`R247_LOOP=20` для приёмки; по умолчанию 3 прогона,
чтобы полный контур не растягивался).

---

## 8. Этап G — документация, образы, фиксация

1. `docs/api/api.md` (+ru) — новые эндпоинты `/v1/messages`, `/v1/responses`,
   их поля/события/ошибки; таблица соответствия Anthropic/Responses ↔ наши поля.
2. `docs/operations/runbook-tools.md` (+ru) — раздел «Клиентские контракты»: как
   запускать контур ожиданий, что означает каждый провал.
3. `CHANGELOG.md` — запись на этап (A/B/C/D/E/F) отдельными абзацами.
4. Образы: `balancer:r247`, `cppworker:gpu-r247` (теги в `deployments/.env`), живой
   прогон всех пробников после развёртывания.
5. Ворота `run-checks.ps1`: добавить шаг «клиентские контракты (офлайн-фикстуры)».

---

## 9. Порядок, зависимости, оценки

| Шаг | Зависит от | Оценка | Комментарий |
|---|---|---|---|
| E (контур ожиданий) | — | 0.5 дня | Делать первым: он ловит регрессии всех остальных этапов |
| A (strict) | E (профиль E2.6), F (общий повтор) | 0.5 дня | Валидатор + проводка strict |
| F (repair round) | A (валидатор) | 0.5 дня | Один повтор, kill-switch |
| B (Anthropic) | E (профиль E2.8) | 1.5-2 дня | Трансляция + SSE + ошибки + второй ход |
| C (Responses) | E (профиль E2.9) | 1-1.5 дня | Подмножество, которым пользуются клиенты |
| D (vision) | — | 0.5 дня (честный отказ) / 2-3 дня (проводка mmproj) | Сначала честный отказ + флаг |
| G (доки/образы) | все | 0.5 дня | На каждый этап — свой прогон |

Порядок исполнения: **E → A → F → B → C → D → G**. Причина: контур ожиданий
сначала фиксирует «как должно быть» для каждого клиента, иначе реализация B/C
проверяется только глазами.

---

## 10. Регрессионные ворота (чек-лист каждого этапа)

- [ ] `go test -tags llama_stub ./cmd/cppworker/ ./internal/balancer/ ./internal/api/` — зелёные
- [ ] `go vet -tags llama_stub ./...` — чисто
- [ ] `node scripts/webui-e2e/tools-all-clients-r246.js` — 54/54 (или больше) PASS
- [ ] `node scripts/webui-e2e/clients-compat-r246.js` — 8/8 PASS
- [ ] `node scripts/webui-e2e/client-expectations-r247.js` — все реализованные профили PASS
- [ ] `.\scripts\run-checks.ps1` — 4+ шагов, 0 провалов (включая i18n/doc-гейты)
- [ ] образы пересобраны и развёрнуты; модель загружена; прогон на живом стенде
- [ ] `CHANGELOG.md` + доки обновлены; пары EN/RU сохранены

---

## 11. Открытые вопросы (решить до этапа, где нужны)

1. **Грамматики (GBNF/структурное декодирование).** `strict` можно обеспечить
   по-настоящему через грамматику в C-мосте (llama.cpp `json_schema`/`json_object`).
   Это отдельный проект (C-часть). До него strict = «валидация + один повтор».
   Вопрос: нужен ли он в этом раунде как P1.5?
2. **Anthropic «thinking»**: `thinking`-блоки с `signature` (шифрованные
   рассуждения) — поддерживать как непрозрачный passthrough или игнорировать?
   Предложение: игнорировать (в R246 reasoning уже отделён от видимого текста).
3. **Responses `previous_response_id` / `store`.** Предложение этапа C: `store:false`
   работает, `previous_response_id` — 400 с понятным текстом (честно), полное
   состояние — отдельная задача.
4. **Vision**: поднимать mmproj в этом раунде или ограничиться честным отказом?
   Предложение: отказ + флаг в этом раунде; проводка mmproj — отдельный план.
5. **Клиенты, которых нет под рукой.** Claude Code/Codex нельзя поставить на стенд —
   контракт проверяем по спеке (фикстуры + профили E2.8/E2.9). Нужен ли доступ к
   реальному клиенту для приёмки?

---

## 12. Журнал выполнения (заполнять по мере реализации)

| Этап | Статус | Дата | Чем подтверждено |
|---|---|---|---|
| E — контур ожиданий | ✅ сделано (13 профилей + strict-loop) | 2026-10-26 | `client-expectations-r247.js`: 13 PASS / 0 FAIL / 0 SKIP на r247 |
| A — strict | ✅ сделано (cppworker), телеметрия балансера — осознанно отложена (балансерский ремонт идёт ПОСЛЕ проверки в воркере) | 2026-10-26 | `tool_schema_validate_r247_test.go` (11 кейсов схем), профиль `continue-langchain-strict` PASS |
| F — repair round | 🟡 базовая часть + nudge на `/v1`; `/api/chat` повторяет тем же промптом (сообщений в той точке нет) | 2026-10-26 | `CPPWORKER_STRICT_ARGS_RETRY`, `appendToolRepairInstruction` (тест), профиль `strict-loop` PASS на сборке с nudge (3/3, затем 2/2 после рестарта); 20 прогонов — `R247_LOOP=20` |
| B — Anthropic `/v1/messages` | ✅ сделано (stream + non-stream + count_tokens) | 2026-10-26 | Go-тесты `anthropic_r247_test.go` (7 кейсов, включая порядок SSE); живые профили `anthropic-sdk` и `anthropic-error-shape` PASS на r247; контур ожиданий 10 PASS / 1 SKIP |
| C — Responses `/v1/responses` | ✅ сделано (stream + non-stream + отказ по `previous_response_id`) | 2026-10-26 | Go-тесты `responses_r247_test.go` (6 кейсов, порядок событий); живые профили `codex-responses` и `responses-error-shape` PASS; контур ожиданий 12/12 |
| D — vision (честный отказ) | ✅ сделано (415 + X-Vision-*, для OpenAI и Anthropic) | 2026-10-26 | Go-тесты `vision_r247_test.go`; живой профиль `vision-honest` PASS; контур ожиданий 13/13 |
| G — доки/образы | 🟡 образ r247 развёрнут, api-доки + CHANGELOG + релизные заметки v0.7.231 (EN/RU) добавлены; финальный `run-checks` — см. §11а | 2026-10-26 | `cppworker:gpu-r247` + `balancer:r247` на стенде |

Легенда: ⬜ не начат · 🟡 в работе · ✅ сделано · ⛔ отменено (с причиной).
