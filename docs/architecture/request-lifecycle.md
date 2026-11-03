# Жизненный цикл запроса: нормативная схема (R402)

**Статус:** действующая схема. Любая правка пути запроса/ответа обязана обновить этот
файл в том же коммите (см. §14).

**Зачем этот документ.** Серия правок «по месту» (R394–R401) лечила симптомы, но не
фиксировала контракт: где считается окно контекста, кто и когда его меняет, какие
поля и в каком порядке уходят клиенту, где именно принимается решение о завершении.
Из-за этого один и тот же класс дефекта всплывал на другом маршруте (native
`/api/chat` починен в R394, а OpenAI-стрим продолжал врать про `finish_reason`).
Здесь зафиксированы **все** маршруты, их ответы и точки решений, с привязкой к коду.

**Как читать.** §0 — инварианты (нарушение = дефект). §1 — единицы измерений
(главный источник путаницы). §4–§9 — конвейер по стадиям. §10 — реестр ошибок.
§11 — таблица «симптом → где смотреть». §12 — карта кода.

**Второй документ:** `request-lifecycle.ru.md` — то же для человека, с Mermaid/UML
диаграммами. Интерактивная версия — WebUI → Настройки → Справка → «Схема работы».

---

## 0. Инварианты

| # | Инвариант | Где закреплён |
|---|---|---|
| I1 | **Ровно один терминатор** на ответ: native `done:true`-чанк, OpenAI `choices[0].finish_reason` + `[DONE]`, не-stream — финальный JSON. Дублирование терминатора запрещено. | `cmd/cppworker/handlers_chat.go` (`writeChatStreamResponse`), `handlers_openai.go` (`writeOpenAIChatStream`), `internal/balancer/llamacpp_transport.go` (`stripNativeDoneContent`) |
| I2 | **Окно клиента — на последовательность (слот).** Суммарное окно модели = окно слота × число слотов (`parallel`). Клиентский `num_ctx` сравнивается с окном слота. | `internal/balancer/preflight_nctx.go` (`perClientToTotal`), `internal/cppbackend` (`resolveNParallel`, `ContextPerSlot`) |
| I3 | **Потолок клиента** = `min(GGUF_max, max(contextLengthMax, contextLength), env_потолок)`. Если `contextLengthMax` не задан (0) — профиль не ограничивает (R68); заявленное окно не режется устаревшим максимумом (R400). | `internal/balancer/preflight_helper.go` (`resolvePhysicalMaxContext`, `profileCapCtx`), `nctx_reload_handlers.go` (`resolveModelMaxContext`) |
| I4 | **Политика окна:** холодный запрос грузит модель под окно запроса; простаивающая копия растёт до окна запроса, если физика позволяет; занятая копия — не растёт (клиент получает 503 с числами); **вниз окно никогда не растёт** ради меньшего запроса. | `internal/balancer/preflight_nctx.go` (ветки (3)–(5)), `nctx_reload.go` (`RunPreflight`), `stream_growth_r385.go` |
| I5 | **usage честный:** `eval_count`/`completion_tokens` включают все сгенерированные токены, включая размышления; `prompt_eval_count`/`prompt_tokens` — весь промпт запроса. | `cmd/cppworker/handlers_chat.go`, `handlers_openai.go` (`buildUsage`, `emitOpenAIUsageChunkSafe`) — исправлено в R402 |
| I6 | **Причина завершения честная:** упор в бюджет вывода или незакрытый блок размышлений даёт `length`/`finish_reason=length`, а не `stop`. Считается токенизатором по сырому выводу, **не** числом callback'ов. | `handlers_chat.go` (`toolsBudgetExhaustedR394`), `handlers_openai.go` (R402), `handlers_generate.go` (R402) |
| I7 | **Отказ всегда с числами и кодом:** `requested/loaded/ceiling/limited_by` + машиночитаемый `error`. Без «попробуйте позже» без причины. | `nctx_reload.go` (`makeRejectPlan`), `preflight_nctx.go` (`makeWindowTooBigReject`), `nctx_ceiling_r267.go` |
| I8 | **Флаг `primary` не действует** (R401): ни рост, ни подмена `num_ctx`, ни AutoTune по нему не решаются. | `internal/balancer/primary_model_policy.go` (`isPrimaryModel` → false) |

---

## 1. Единицы: окно «на слот» и суммарное

Главный источник ошибок в отчётах клиентов — смешение двух чисел.

| Величина | Смысл | Кто задаёт | Где используется |
|---|---|---|---|
| `num_ctx` / `options.num_ctx` (клиент) | окно **одной** последовательности | клиент | `ResolveNumCtx`, preflight |
| `contextLength` профиля | заявленное окно модели (для загрузки) | оператор | `applyProfileLoadParams`, `declaredWindowTotal` |
| `contextPerSeq` | окно слота при загрузке | загрузка | `model_management.go`, cppworker `loadWithParams` |
| `contextSize` (суммарное) | `contextPerSeq × slots` | cppworker | `/api/models`, `/api/show` (поле `context_size_total`) |
| `parallel` / `num_parallel` | число слотов | профиль / `CPPWORKER_N_PARALLEL` (по умолчанию **1**, R403) | загрузка, preflight |
| GGUF training context | максимум модели (суммарно) | GGUF-заголовок | потолок |
| `LB_NCTX_RELOAD_MAX_N_CTX` | защитный потолок АВТО-роста | env | `effectiveAutoReloadCeiling` |

Практическое следствие: при `parallel=2` и training-context 131072 **один** клиент
может получить максимум 65536 на слот; чтобы одна сессия (Cline) получила все
131072, модель должна быть загружена с `parallel=1`. Это не дефект балансера, а
следствие политики слотов; §10 объясняет, что видит клиент.

---

## 2. Входные точки (клиентские маршруты)

| Маршрут | Протокол ответа | Первый байт/финал | Терминатор |
|---|---|---|---|
| `POST /api/chat` | Ollama NDJSON | стрим `message.content`/`message.thinking` | `done:true` + `done_reason` |
| `POST /api/generate` | Ollama NDJSON | `response` | `done:true` + `done_reason`, `context[]` |
| `POST /v1/chat/completions` | OpenAI SSE / JSON | `data: {...delta...}` | `finish_reason` → `data: [DONE]` |
| `POST /v1/completions` | OpenAI SSE / JSON | `text` | `finish_reason` → `[DONE]` |
| `POST /v1/messages` | Anthropic SSE/JSON | `content_block_delta` | `message_stop` (трансляция в Anthropic) |
| `POST /api/embeddings`, `/v1/embeddings` | JSON | — | один JSON |
| `POST /api/show`, `/api/tags`, `/api/ps` | JSON | — | один JSON |
| `/api/image/*` | JSON/SSE | — | imageworker |

Балансер проксирует маршрут по пути; native-пути сохраняют формат клиента
(`internal/api/api_v1_forward*.go`, `internal/balancer/llamacpp_transport.go`).
Анthropic-маршрут переводится в OpenAI внутри (`anthropic_r247.go`) и обратно.

---

## 3. Стадия A — вход и допуск (балансер)

1. **Разбор запроса** `extractRequestMeta` (`internal/balancer/preflight_helper.go`):
   модель, `num_ctx`/`contextPerSeq`, `num_predict`/`max_tokens`, tools, картинки,
   оценка промпта. Результат — `RequestMeta`.
2. **Выбор бэкенда** (`internal/balancer/admission_queue.go`, `unified_queue_r73.go`,
   `a`-селекторы): модель/тип/метки/роль донора, свободные слоты, очередь допуска.
3. **Занятость**: если слотов нет — запрос ждёт в **очереди допуска**
   (`LB_ADMISSION_WAIT_SEC`, default 300 c; FIFO по сессиям), либо получает 503
   с `X-Queue-*`.
4. **Нет ни одного подходящего бэкенда** (все offline/disabled/доноры) — запрос
   ждёт в **отдельной очереди ожидания бэкенда** (`LB_BACKEND_WAIT_SEC`,
   default 120 c; FIFO по модели, R403). Это не то же самое, что очередь слота:
   там бэкенд уже выбран, здесь его ещё нет. После таймаута — 503 `no_backend`
   с `Retry-After`, `X-Backend-Queue-Position`, `X-Backend-Queue-Wait-Ms`.
   Тумблер «не использовать бэкенд» (R250) действует и на llama.cpp-пути
   (R403.1/R403.2): выключенный узел не выбирается ни обработчиками роутера, ни
   выбором «копии под окно», ни live-пробой R303, и запрос встаёт в эту очередь.
   В смешанном режиме (`mode=standard`) тот же смысл выполняет единая
   admission-очередь (`unified_queue_r73.go`): она ждёт «любой бэкенд + слот» с
   бюджетом `max(LB_ADMISSION_WAIT_SEC, LB_AUTO_LOAD_WAIT_SEC)`; отдельная
   очередь R403 применяется на llama.cpp-router пути (engine-locked) и в его
   обработчиках.
5. **preflight** (§5) — до проксирования: может отказать (413/503), перезагрузить
   модель (202/асинхронно) или пропустить как есть.

**Границы:** балансер не считает токены модели; он принимает решение по окну и
памяти, но фактическую генерацию и честные счётчики даёт cppworker.

---

## 4. Стадия B — где модель и в каком состоянии

- `backendStatus`/`models` — поллер `/api/models` (30 c) + push-факты загрузки.
- Для каждой копии известны: `contextSize` (суммарное), `contextPerSeq`, `parallel`,
  `kvCacheType`, `reasoning`, `feasibleMaxContext`, `ggufMaxContext`, `loadedAt`.
- «Свободная копия» = `activeQueries == 0` и нет активных стримов по модели
  (`growthBlockersR288`, `beginClientStreamR271`).
- Источники окна: `loadedWindowNCtx` (метрики + координатор), `nctxReload.LastKnownNCtx`
  (живёт на бэкенд, для per-model решений не используется — R398.1).

---

## 5. Стадия C — решение об окне (сердце схемы)

Порядок проверок в `preflightNCtxReloadIfNeeded`
(`internal/balancer/nctx_reload_handlers.go`) и `RunPreflight`
(`internal/balancer/nctx_reload.go`):

```
(0) модель отсутствует на бэкенде            → 404 + available_models (R119)
(1) primary                                   → ОТКЛЮЧЁН (R401)
(2) клиент не сообщил окно:
      окно выведено из промпта (R253/R294)    → кандидат на рост
      иначе                                   → NoOp
(3) модель не загружена                       → NoOp: cppworker грузит по окну запроса
(4) окно слота >= запроса                     → NoOp (вниз не перезагружаем)
(5) нужен рост:
      target_total = per_seq × slots
      target_total > physical_ceiling         → 413 (числа + причина)
      target_total > effective_ceiling        → 413 (env/profile, limited_by)
      иначе                                   → async reload (202/503), клиент ждёт/повторяет
```

**Формулы потолков** (единая точка — `preflight_helper.go`):

```
profile_cap       = contextLengthMax, но не ниже contextLength (R400); 0 = профиль не ограничивает
env_cap           = LB_NCTX_RELOAD_MAX_N_CTX (0 = без ограничения)
physical_ceiling  = min( GGUF_max, max(profile_cap, env_cap) )
effective_ceiling = max(env_cap, declared_window)          // для авто-роста (R255/R267)
client_cap        = min( physical_ceiling, effective_ceiling )
```

**Решения по росту окна:**

| Ситуация | Действие | Что видит клиент |
|---|---|---|
| модель не загружена | загрузка под окно запроса (auto-load) | ожидание/202, затем 200 |
| загружена, окно слота < запроса, копия свободна, физика позволяет | async reload до окна запроса | 503 + Retry-After с числами; повтор → 200 |
| загружена, окно слота < запроса, есть активный стрим | рост отложен | обслуживание в текущем окне или 503 с числами |
| загружена, окно слота >= запроса | без перезагрузки | 200 |
| физика не позволяет | отказ 413 (`n_ctx_infeasible` / `prompt_exceeds_context`) | числа: requested/loaded/ceiling/limited_by |
| слотов нет | очередь/503 | `X-Queue-*`/Retry-After |

Коды отказа — §10. Сообщение обязано содержать числа и код (I7), а не общий совет.

---

## 6. Стадия D — загрузка/перезагрузка

- **Холодная загрузка**: `ensureModelLoadedOnBackend` → `loadWithParams`
  (`internal/balancer/model_management.go`): профиль заполняет только недостающие поля
  (R397), окно запроса имеет приоритет.
- **Перезагрузка по окну**: `nctxReload` координатор; `reloadDedup` не даёт запустить
  вторую; тело reload собирается из хинтов профиля (`reloadHintsFor`): kvCacheType,
  parallel, reasoning, gpuLayers, useMmap, override-тензоры.
- **Асинхронность**: `LB_NCTX_RELOAD_ASYNC` → 202/503 + `progressUrl`; клиент либо
  ждёт внутри балансера (`LB_AUTO_LOAD_WAIT_SEC`), либо повторяет.
- **Гейты**: memfit (`internal/cppbackend/memfit_adapter.go`, `internal/memfit`),
  R297 `WindowFeasibility` (до разрушительного unload), R383 layout heal, R380/R382.
- **Отказ не имеет права оставить стенд без модели** (R297): проверка до unload.

---

## 7. Стадия E — генерация и состав ответа (cppworker)

1. `handleChat`/`handleV1ChatCompletions` → `bridge.GenerateStream*`.
2. Стрим-парсер `FinalizeForStream`/`rsParser` делит вывод на `thinking` и `content`,
   отслеживает незакрытый блок размышлений (`Unclosed`) и длину (`ReasoningChars`).
3. Инструменты: буферизация, `parseToolCallsFromOutput`, удержание маркеров
   (`toolsStreamHoldback`), терминальный чанк `tool_calls`.
4. **Размышления на tools-пути (R404).** `thinking` — не вызов инструмента,
   поэтому уходит клиенту **дельтами сразу** (`message.thinking`, `done:false`),
   не дожидаясь закрытия блока. Удержание остаётся только для `content`: там
   действительно может начаться разметка tool call. До R404 старт блока
   размышлений включал удержание всего вывода: при исчерпании бюджета ВНУТРИ
   блока клиент минутами не получал ни байта, затем получал пустой финальный
   чанк и повтор R188 с тем же бюджетом. Повтор теперь не запускается, если
   клиенту уже ушёл хотя бы один кадр (thinking или content).
4. Пустой видимый ответ при непустых размышлениях → пояснение клиенту
   (`emptyAnswerNoticeForNativeR314`) или `error`-терминатор.

---

## 8. Стадия F — матрица причин завершения

| Путь | Поле | `stop` | `length` | `tool_calls` | `error` |
|---|---|---|---|---|---|
| native `/api/chat` non-stream | `done_reason` | модель закончила | бюджет/незакрытый think | — | пустой вывод |
| native `/api/chat` stream | `done_reason` | модель закончила | бюджет (`toolsBudgetExhaustedR394`) или `Unclosed` | — | пустой вывод |
| OpenAI `/v1/chat/completions` stream | `finish_reason` | модель закончила | бюджет (R402: токены, не callback'и) или `Unclosed` | вызовы инструментов | пустой вывод/обрыв |
| OpenAI non-stream | `finish_reason` | закончила | бюджет | tool_calls | error |
| `/api/generate` stream | `done_reason` | закончила | бюджет/Unclosed (R402) | — | пустой вывод |

Правило: «кап бюджета» считается **токенизатором по сырому выводу**
(`countTokensSafe(modelName, fullOutput)`), потому что callback'ов меньше, чем
токенов (живой факт R394: `num_predict=16` → `eval_count=17`, callback'ов 14).

---

## 9. Стадия G — выход и usage

### 9.1 usage-поля

| Поле | Значение | Источник |
|---|---|---|
| `prompt_eval_count` / `prompt_tokens` | токены всего промпта | `countTokensSafe(model, prompt)` |
| `eval_count` / `completion_tokens` | все сгенерированные токены, **включая reasoning** | `countTokensSafe(model, fullOutput)` (R402 — native stream и generate) |
| `total_tokens` | prompt + completion | `handlers_openai.go` |
| `eval_duration`, `prompt_eval_duration` | время генерации / TTFT | замер в хендлере |

Клиенты (Cline/OpenWebUI) считают «заполненность контекста» из этих полей; поэтому
занижение `eval_count` (без reasoning) приводило к переполнению реального окна и
к ошибке «увеличьте контекст вручную».

### 9.2 `/api/show`

| Поле | Смысл | R402 |
|---|---|---|
| `context_size` | окно, доступное **одному клиенту** (на слот) | исправлено: `total/parallel`; раньше отдавалось суммарное |
| `context_size_total` | суммарное окно загруженной модели | добавлено |
| `parallel` | число слотов | добавлено |
| `model_info.<arch>.context_length` | training context из GGUF | без изменений (физический максимум модели) |
| `model_info.context_size` | то же, что `context_size` | для совместимости |
| `parameters` | Modelfile-параметры: `num_ctx <окно>` | R403: раньше здесь лежал размер модели («3.3B»), и клиенты, читающие `num_ctx` из `parameters`, не получали окно вовсе. `details.parameter_size` остаётся размером |

### 9.3 Трансформации

- OpenAI→Ollama: `translateUsageChunkToOllama`, `stripNativeDoneContent`
  (`internal/balancer/llamacpp_transport.go`): один терминатор, usage из OpenAI-чанка.
- Ollama→OpenAI: `llamacpp_translate_resp.go`; Anthropic — `anthropic_r247.go`.
- Egress-фильтр `filterAssistantEgressForClient` режет служебные токены, не теряя текст.

---

## 10. Реестр отказов

| Код / HTTP | Когда | Тело (обязательные поля) | Где строится |
|---|---|---|---|
| 404 `model_missing` | модели нет на бэкенде | `available_models` | `nctx_reload_handlers.go` |
| 413 `n_ctx_infeasible` | окно выше физики/потолка | `requested_n_ctx`, `current_n_ctx`, `max_vram_n_ctx`, `safe_max_n_ctx`, `suggestion`, `limited_by` | `makeRejectPlan` (`nctx_reload.go`) |
| 413 `prompt_exceeds_context` | промпт + вывод > текущего окна, расти некуда | те же + объяснение «историю/инструменты/системный промпт» | там же |
| 413 `auto_reload_ceiling_reason` | окно выше `effective_ceiling` | `auto_reload_max_n_ctx`, `limited_by` | `preflight_nctx.go` |
| 503 `reload_pending` | идёт рост окна | `estimated_prompt_tokens`, `loaded_window_tokens`, `required_tokens`, Retry-After | `nctx_reload_handlers.go` |
| 503 `no_free_slots` / очередь | нет слотов | `X-Queue-Wait-Ms`, Retry-After | `admission_queue.go` |
| 503 `no_backend` | нет ни одного подходящего бэкенда (offline/disabled/донор), ожидание истекло | `waited_ms`, `position`, `X-Backend-Queue-*`, Retry-After | `backend_wait_queue_r403.go` |
| 400/413 `params_mismatch` | клиент просит параметры, несовместимые с загруженной копией (tools/reasoning/vision) | `differences` | `makeParamMismatchReject` |

**Правило R402:** сообщение об отказе роста обязано объяснять **единицы**: окно на
слот, `parallel`, суммарное окно и training context. Общий совет «сохраните профиль и
перезагрузите вручную» запрещён, если балансер мог вырасти сам (I4/I7).

---

## 11. Диагностика: симптом → доказательство

| Симптом клиента | Что смотреть в логе балансера | Файлы |
|---|---|---|
| «ответ обрезан, но завершён» | `finish_reason`, `done_reason`, `budget_exhausted`, `generated_tokens`, `tokens_sent` | `handlers_openai.go`, `handlers_chat.go`, `handlers_generate.go` |
| «клиент не получает маркеров Think, потом обрыв» | `tools_reasoning_split` → появились ли `message.thinking`-дельты; `retry skipped — stream already started (R404)`; `thinking_len` в summary | `handlers_chat.go` (`writeChatStreamResponseWithTools`), `tools_stream_reasoning_r404.go` |
| «не тот объём контекста» | `prompt_eval_count`/`eval_count`, `/api/show.context_size`, `parallel` | `handlers_chat.go`, `handlers_model.go` |
| «не даёт поднять окно» | `physical_ceiling`, `limited_by`, `auto_reload_max_n_ctx`, `context_per_seq`, `slots` | `preflight_nctx.go`, `nctx_reload.go` |
| «окно не растёт» | `growth_blockers`, `active_streams`, `reload_pending`, `WindowFeasibility` | `growth_guard_r299.go`, `growth_stream_guard_r301.go` |
| «модель не загрузилась» | `memfit` verdict, `feasible`, `stage`, `reason` | `internal/memfit`, `memfit_gate.go` |
| «primary мешает» | `primary is deprecated and ignored (R401)` | `primary_model_policy.go` |

---

## 12. Карта кода

| Блок | Файл (пакет) | Ключевые функции |
|---|---|---|
| Разбор запроса | `internal/balancer/preflight_helper.go` | `extractRequestMeta`, `collectPreflightState`, `profileCapCtx`, `resolvePhysicalMaxContext` |
| Решение об окне | `internal/balancer/preflight_nctx.go` | `RunPreflight`, `perClientToTotal`, `makeWindowTooBigReject` |
| Перезагрузка окна | `internal/balancer/nctx_reload.go`, `nctx_reload_handlers.go`, `nctx_reload_adaptive.go` | `RunPreflight`, `preflightNCtxReloadIfNeeded`, `makeRejectPlan` |
| Потолки | `nctx_ceiling_r267.go`, `nctx_ceiling_r319.go`, `nctx_declared_r255.go` | `effectiveAutoReloadCeiling`, `ReloadCeiling` |
| Рост на стриме | `stream_growth_r385.go`, `growth_stream_guard_r301.go` | `streamGrowthCheckR385`, `growthVerdictR301` |
| Загрузка/профиль | `model_management.go` | `applyProfileLoadParams`, `resolveDeclaredWindow` |
| Память | `internal/memfit/*`, `internal/cppbackend/memfit_adapter.go`, `memfit_gate.go` | `Evaluate`, `Ceilings`, `Policy` |
| Генерация (native) | `cmd/cppworker/handlers_chat.go` | `handleChat`, `writeChatStreamResponse*` |
| Генерация (OpenAI) | `cmd/cppworker/handlers_openai.go` | `handleV1ChatCompletions`, `writeOpenAIChatStream`, `emitOpenAIUsageChunkSafe` |
| Генерация (`/api/generate`) | `cmd/cppworker/handlers_generate.go` | `handleGenerate`, стрим-ветка |
| Терминальные причины | `cmd/cppworker/tools_terminal_reason_r394.go` | `toolsBudgetExhaustedR394`, `toolsTerminalReasonR394` |
| Метаданные модели | `cmd/cppworker/handlers_model.go` | `handleOllamaShow`, `writeOllamaShowLoadedResponse` |
| Транспорт/трансляции | `internal/balancer/llamacpp_transport.go`, `llamacpp_translate_resp.go`, `anthropic_r247.go` | `stripNativeDoneContent`, `translateUsageChunkToOllama` |
| Очередь/допуск | `internal/balancer/admission_queue.go`, `unified_queue_r73.go` | admission, `X-Queue-*` |
| Ожидание бэкенда | `internal/balancer/backend_wait_queue_r403.go` | `waitBackendR403`, `waitForLlamaCppBackendR403` |
| Статистика токенов | `internal/balancer/stats*.go`, `llamacpp_transport.go` | `recordTokenUsage` |

---

## 13. Открытые решения (требуют явного выбора оператора)

1. **Семантика суммарного окна.** Сейчас: клиентский `num_ctx` — на слот, суммарное =
   слот × `parallel` (как у Ollama при загрузке), но `/api/show` и часть расчётов
   отдают суммарное как «окно клиента». Варианты:
   (a) оставить и доносить до клиента «на слот» (сделано в R402 для `/api/show`);
   (b) перейти к «`num_ctx` = на слот, суммарное не ограничивается training-context»
       (как Ollama) — потребует пересчёта memfit и проверки VRAM.
2. **`parallel` по умолчанию = 1** (R403). Больше оператор ставит сам в панели
   (профиль модели `parallel`) или в `CPPWORKER_N_PARALLEL`; слоты делят окно.
3. **Семантика окна по протоколам.** Решено: переключение семантики (per-seq как
   в Ollama vs total как сейчас) не должно быть глобальным — оно изолируется по
   протоколу (ollama-путь / openai-путь), чтобы правка одного не меняла решения
   другого, а новые маршруты выбирали семантику явно. Механизм: отдельный
   резолвер семантики по `request path`; **реализация — следующий шаг**, до неё
   оба пути работают на текущей (total) семантике.
4. **Авто-снижение `parallel`** под запрос одного клиента (если других сессий нет) —
   не реализовано; обсуждается.
5. **Очередь «нет свободного бэкенда»** — реализована в R403
   (`backend_wait_queue_r403.go`, `LB_BACKEND_WAIT_SEC`).

---

## 14. Чек-лист изменения пути запроса/ответа

1. Найди стадию (§4–§9) и функцию в §12; не меняй формулу в двух местах.
2. Обнови инвариант/таблицу в этом файле и диаграмму в `.ru.md`.
3. Обнови `webui/docs/scheme.json` (узел/ребро/поля), если меняется контракт.
4. Тесты: native + OpenAI + generate (минимум три пути), плюс регресс на единицы
   (per-slot vs total).
5. Живая проверка: `/api/show`, стрим с малым бюджетом, запрос на рост окна.
6. Только после этого — деплой и отчёт.
