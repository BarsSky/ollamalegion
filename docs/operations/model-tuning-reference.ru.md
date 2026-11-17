# Справка по настройке моделей: ручки, значения и почему

**Назначение.** Один документ, по которому можно (а) понять, что делает каждая
ручка профиля модели, (б) взять готовый профиль для уже разобранной модели и
(в) перенести правило на новую модель. Числа взяты из живых замеров, где они
есть; где замера не было — это сказано прямо.

**Где живут настройки:**

1. **Профиль модели** (`model_profiles` балансера, WebUI → «Модели» или
   `PUT /api/v1/cppworker/model-profiles/<имя>`) — постоянные параметры загрузки:
   окно, KV-кэш, слои, батч, слоты, размышления. Применяется через
   `POST …/apply` (перезагрузка модели на бэкендах).
2. **Env воркера** (`CPPWORKER_*`) — то, что нельзя менять по модели:
   `CPPWORKER_PREFIX_CACHE` (кэш префилла, R493), `CPPWORKER_ENABLE_REASONING`
   (глобальный выключатель размышлений), `CPPWORKER_LLAMA_BUILD` (идентичность).
3. **Env балансера** (`LB_*`) — политика окна/очереди: `LB_NCTX_RELOAD_MAX_N_CTX`,
   `LB_AGENT_WINDOW_BUDGET`, `LB_AGENT_OUTPUT_RESERVE_TOKENS`,
   `LB_NCTX_GROW_FOR_AGENTS`, `LB_PLACE_OVER_RELOAD`, `LB_SESSION_STICKINESS`.
4. **Клиент** — то, что сервер изменить не может: «Reasoning Effort» и «Model
   Context Window» в Cline и подобных агентах. Инструкцию «Reasoning effort is
   set to xhigh» вписывает САМ клиент в системный промпт, сервер её не убирает.

---

## 1. Ручки профиля: что делает и как выбирать

| Поле | Что делает | Как выбирать | Ошибка, которую ловит |
|---|---|---|---|
| `contextPerSeq` | окно НА КЛИЕНТА (на слот) | не больше, чем реально влезает в VRAM; для агента — с запасом под ответ | слишком большое окно уводит слои в CPU (медленно), слишком маленькое даёт 503+reload посреди хода |
| `contextLength` / `contextLengthMax` | суммарное окно / потолок для авто-роста | потолок ≤ физического предела (KV+веса); иначе балансер пытается вырасти в недостижимое | `LB_NCTX_RELOAD_MAX_N_CTX=262144` при 22 ГБ VRAM — обещание, которое не выполняется |
| `parallel` | слотов на модель | 1 для агентов (предсказуемая скорость); 2 — если нужен параллелизм и окно делится | `parallel=2` втихую делит окно: каждый клиент получает `contextPerSeq`, а сумма — вдвое больше VRAM |
| `kvCacheType` | `f16` / `q8_0` / `q4_0` | `q8_0` — баланс (по умолчанию для длинных контекстов); `q4_0` — когда VRAM в обрез (27B на 24 ГБ) | `f16` на 100k+ контексте съедает VRAM и вытесняет слои |
| `numGpuLayers` | сколько слоёв в VRAM (`-1`/`-2` = все/по memfit) | не ограничивать вручную; проверить, что в `/api/models` `device_layers` = все CUDA | «тихий» partial_offload: 4 слоя на CPU дают −30…50 % скорости на каждом токене |
| `batchSize` | батч префилла | 512 — база; 1024 — если VRAM позволяет (префилл ускоряется на 10–20 %) | 2048 при свободных 2 ГБ VRAM → вытеснение слоёв |
| `flashAttn` | flash attention | `true` всегда, если поддерживается картой | без него KV-память и скорость хуже |
| `useMmap` | mmap весов | `true` (быстрее старт, меньше RAM-пик) | — |
| `enableReasoning` | нативные размышления (`<think>`) | `false` для агентских циклов (Cline/Roo): размышления едят бюджет хода; `true` — когда нужен разбор сложной задачи человеком | 2169 токенов в размышления → `done_reason=length` и пустой ответ (живой случай) |
| `maxTokens` | верхняя граница вывода | ≥ бюджета, который реально нужен инструменту; меньше — обрезает вызов | вызов инструмента обрезан → клиент не распознаёт tool_call |
| `primary` | «настройки зафиксированы админом» | `true` для боевых моделей: клиентский `num_ctx` не поднимает окно | клиентский запрос втихую поднимает контекст и выживает слоты |
| `notes` | человекочитаемая заметка (видна в WebUI и API) | писать сюда короткую «справку по модели» (см. §3) | — |

**Порядок применения:** профиль → `POST …/apply` → проверить
`/api/models` (`contextLength`, `device_layers`, `kvCacheType`,
`reasoningEnabled`) и только потом мерить.

---

## 1.1 Инвариант: `contextLength` ≤ `contextLengthMax`

Это не пожелание, а следствие того, как балансер читает профиль.

- При `contextLengthAuto: false` (Tier 1) окно задаёт **`contextLength`**, и
  `contextLengthMax` его **не урезает**. Если максимум меньше заявленного окна,
  балансер пишет предупреждение и всё равно берёт заявленное:
  `internal/balancer/nctx_reload_handlers.go:2140-2146` —
  `declared window exceeds contextLengthMax, honouring declared window`.
- При `contextLengthAuto: true` (Tier 2) потолок авто-роста =
  `max(contextLengthMax, contextLength)` и дополнительно ограничен максимумом
  GGUF (`:2168-2178`), то есть тоже не может урезать заявленное окно.

**Почему расхождение опасно.** Одно и то же имя модели на двух узлах с разными
`contextLength`/GGUF-максимумами даёт РАЗНЫЕ загруженные окна, и запрос, который
помещается на одном узле, на другом получает отказ или перезагрузку. Живой
случай (второй стенд, архив 2026-11-16): в профиле `contextLength=194296` при
`contextLengthMax=131072`; узлы поднялись с окнами `194304` (все слои в VRAM) и
`131072` (A10) — для одной модели. Признак в логе:
`grep 'declared window exceeds contextLengthMax' logs.txt`.

**Правило.** Ставьте `contextLength` и `contextLengthMax` **равными** — тогда
заявленное окно и потолок авто-роста говорят одно и то же, и поведение узлов не
зависит от того, какой профиль/GGUF-максимум попался первым. Для агентского
стенда второго стенда это `131072` и `131072` (см. §3.1, §3.7).

---

## 2. Env: что влияет на скорость и на «молчание»

| Переменная | Действие | Рекомендация |
|---|---|---|
| `CPPWORKER_PREFIX_CACHE` | **R493:** переиспользование KV-кэша префилла между запросами (по умолчанию `1`) | `1`; `0` — если нужно исключить кэш при разборе аномалии. Ожидаемо: ход агента 213 с → 30–40 с (второй стенд) |
| `CPPWORKER_ENABLE_REASONING` | глобальный выключатель размышлений (`0` — выключить) | `0` для агентских стендов; профильный `enableReasoning` перекрывает точечно |
| `CPPWORKER_REASONING_BUDGET` | **R493:** порог токенов на размышления: при достижении — повтор без thinking | `512` для агентов; `0` — без порога |
| `LB_NCTX_RELOAD_MAX_N_CTX` | до какого окна разрешён авто-рост | ставить по физическому пределу железа (для 27B Q4 на A10 — 188416), не 262144 |
| `LB_AGENT_WINDOW_BUDGET` | как считать «нужное окно» агента (`minimal` — только промпт+256) | `minimal` (перезагрузок меньше); `client` — только если нужен полный `max_tokens` клиента ценой reload'ов |
| `LB_AGENT_OUTPUT_RESERVE_TOKENS` | резерв вывода в расчёте окна | 4096 для агентов |
| `LB_SESSION_STICKINESS` | привязка сессии к бэкенду | `true`: без неё смена узла обнуляет кэш префилла |
| `CPPWORKER_LLAMA_BUILD` | идентичность сборки для диагностики | обновлять вместе с пином llama.cpp (иначе `llama_build_mismatch=true`) |

---

## 3. Готовые профили с пояснениями

### 3.0 Сводная таблица рекомендованных значений

Все числа ниже — из §3.1–3.7; `contextLength` намеренно равен
`contextLengthMax` (см. §1.1). Единицы: окна — токены, `batchSize` — токены,
`numGpuLayers` — слои (`-1`/`-2` = все/по memfit).

| Класс модели | `contextLength`=`contextLengthMax` | `contextPerSeq` | `parallel` | `kvCacheType` | `batchSize` | `numGpuLayers` | `flashAttn` | `enableReasoning` |
|---|---|---|---|---|---|---|---|---|
| 27B Q4 на 24 ГБ (A10) | 131072 | 131072 | 1 | `q4_0` | 1024 | `-2` | `true` | `false` |
| 4B Q4 на 8+ ГБ | 32768 | 32768 | 1 | `f16` | 512 | `-1` | `true` | `false` |
| 30B+ класс на 24 ГБ | 16384 | 16384 | 1 | `q8_0` | 512 | `-2` | `true` | `false` |
| gemma-4 / gemma-4-E4B | 32768 | 32768 | 1 | `q8_0` | 512 | `-1` | `true` | `false` |
| DeepSeek-R1-Distill 7–32B | 32768 | 32768 | 1 | `q8_0` | 512 | `-2` | `true` | `true` + `maxTokens: 8192` |
| 8B–14B класс (Qwen3-8B, Llama-3.1-8B, Mistral-Nemo, Phi-4-14B) | 32768 | 32768 | 1 | `f16` | 512 | `-1` | `true` | `false` |

Два правила поверх таблицы:

- `useMmap: true` — везде (быстрее старт, меньше пик RAM);
- `primary: true` — для боевых моделей: клиентский `num_ctx` больше не поднимает
  окно втихую.

### 3.1 `Qwen3.8-27B-UD-Q4_K_M` — 27B Q4 на A10 24 ГБ (второй стенд, разобрано)

```json
{
  "contextLength": 131072, "contextPerSeq": 131072, "contextLengthMax": 131072,
  "contextLengthAuto": true, "primary": true, "parallel": 1,
  "kvCacheType": "q4_0", "batchSize": 1024, "numGpuLayers": -2,
  "flashAttn": true, "useMmap": true, "enableReasoning": false
}
```

- `q4_0` — потому что 100k+ контекст с `q8_0` не влезает вместе с весами
  (15.3 GiB) в 22.5 ГБ.
- `enableReasoning: false` — агент (Cline) шлёт 80–90k промпта на ход; бюджет
  вывода в худшем случае был 2169 токенов, и все ушли в `<think>`: 8 минут и
  пустой ответ. Размышления здесь не окупаются.
- `batchSize: 1024` — префилл это ~200 с из 213 с хода.
- Клиент: Model Context Window **96k** (при серверных 131k) и Reasoning Effort
  **minimal** — тогда всегда есть запас на ответ и нет 503+reload.
- Проверено на стенде: окно 180224/188416 грузится, 2 узла по 1 слоту.

### 3.2 `Qwen3-4B-Instruct-2507` — 4B на GPU 8+ ГБ (мастер-стенд, разобрано)

```json
{
  "contextLength": 32768, "contextPerSeq": 32768, "parallel": 1,
  "kvCacheType": "f16", "batchSize": 512, "numGpuLayers": -1,
  "flashAttn": true, "enableReasoning": false
}
```

- `f16` KV: при 32k контексте и 4B весах память позволяет, качество лучше.
- `enableReasoning: false` по умолчанию: на 4B размышления чаще портят
  tool-call, чем помогают.

### 3.3 `Qwen3-32B-Instruct-2507` / `Qwen3.6-35B-A3B-UD-Q4_K_M` — 30B+ класс

```json
{
  "contextLength": 16384, "contextPerSeq": 16384, "parallel": 1,
  "kvCacheType": "q8_0", "batchSize": 512, "numGpuLayers": -2,
  "flashAttn": true, "useMmap": true, "enableReasoning": false
}
```

- Окно скромное: на 24 ГБ 30B-класс держит ~16k с `q8_0`; больше — оффлоад
  слоёв и падение скорости в 2–3 раза. MoE (`35B-A3B`) переносит окно легче
  (активных параметров меньше), но KV считается по полному числу слоёв.

### 3.4 `gemma-4` / `gemma-4-E4B-it-Q4_K_M` — reasoning-теги в контенте

```json
{
  "contextLength": 32768, "contextPerSeq": 32768, "parallel": 1,
  "kvCacheType": "q8_0", "batchSize": 512, "numGpuLayers": -1,
  "flashAttn": true, "enableReasoning": false
}
```

- У gemma-4 размышления приходят тегами `<|channel>thought`, а не `<think>`;
  парсер их понимает, но при `enableReasoning: true` теги попадали в `content`
  (R216/R219). Профиль по умолчанию — выключено, включать только вручную.

### 3.5 `DeepSeek-R1-Distill-*` (7B/14B/32B) — рассуждающая модель

```json
{
  "contextLength": 32768, "contextPerSeq": 32768, "parallel": 1,
  "kvCacheType": "q8_0", "batchSize": 512, "numGpuLayers": -2,
  "flashAttn": true, "enableReasoning": true, "maxTokens": 8192
}
```

- Здесь `enableReasoning: true` осмысленно (модель рассуждающая по природе), но
  `maxTokens` обязателен: иначе размышления съедают весь бюджет и агент получает
  `done_reason=length`. С R493 при исчерпании бюджета размышлений срабатывает
  один повтор без thinking — клиент получает ответ, а не пустоту.
- На агентских циклах всё равно рекомендуется `enableReasoning: false` +
  `maxTokens` поменьше: скорость хода важнее глубины.

### 3.6 Малые модели: `Qwen3-8B`, `Llama-3.1-8B`, `Mistral-Nemo`, `Phi-4-14B`

```json
{
  "contextLength": 32768, "contextPerSeq": 32768, "parallel": 1,
  "kvCacheType": "f16", "batchSize": 512, "numGpuLayers": -1,
  "flashAttn": true, "enableReasoning": false
}
```

- Правило простое: если веса + KV при 32k влезают в VRAM с запасом ~15 % —
  `f16` и все слои на GPU; как только перестают — `q8_0`, затем `q4_0`
  (в этом порядке: качество деградирует мягче, чем при оффлоаде слоёв).

### 3.7 Cline на втором стенде: `131072` / `131072`

Тот же профиль, что §3.1, но записанный явно — как ответ на вопрос «какие
значения ставить, чтобы не было 503 и перезагрузок посреди хода»:

```json
{
  "contextLength": 131072, "contextLengthMax": 131072,
  "contextPerSeq": 131072, "contextLengthAuto": true, "primary": true,
  "parallel": 1, "kvCacheType": "q4_0", "batchSize": 1024,
  "numGpuLayers": -2, "flashAttn": true, "useMmap": true,
  "enableReasoning": false
}
```

- Оба окна равны `131072` — иначе узлы разъедутся (§1.1).
- В клиенте (Cline) — Model Context Window **96k** и Reasoning Effort
  **minimal**: серверный потолок выше клиентского запроса, значит место под
  ответ есть всегда, а `num_ctx` клиента не тянет окно вверх.
- `enableReasoning: false`: 2169 токенов размышлений в худшем ходе дали
  `done_reason=length` и пустой ответ (живой случай второго стенда).

---

## 4. Как проверить, что настройка сработала (без «на глаз»)

```bash
# профиль применился и что реально загружено
curl -s -H 'X-API-Token: <token>' :18081/api/v1/cppworker/model-profiles/<модель> | jq .profile
curl -s :18092/api/models | jq '.loadedModels[0] | {contextLength, kvCacheType, numGpuLayers, device_layers_exact, reasoningEnabled}'

# кэш префилла работает: во втором ходе агента в логе воркера появляется строка
grep 'prefix cache: reused' logs.txt | tail -3
#   ожидаем: reused N of M tokens, где N — почти весь предыдущий промпт

# размышления отключены: в сводке хода thinking_len≈0, hold_kept_reasoning=false
grep 'stream response summary' logs.txt | tail -1
```

## 5. Границы документа

- §1.1 — правило с ссылками на код (`internal/balancer/nctx_reload_handlers.go`),
  а не замер: расхождение `contextLength` и `contextLengthMax` действительно
  только логируется предупреждением.
- §3.0 — сводка чисел из §3.1–3.7, отдельных замеров за ней нет.
- §3.1, §3.2 и §3.7 собраны на живых замерах (второй стенд — архив 2026-11-16;
  мастер — замеры 2026-11-15).
- §3.3–3.6 — правила, а не замеры: конкретный потолок окна надо снимать
  `cppworker -feasible <модель>` и сверять с `/api/models`.
- Клиентские настройки (Reasoning Effort, Model Context Window) сервер изменить
  не может — их надо выставлять в Cline/Roo/OpenWebUI отдельно.
