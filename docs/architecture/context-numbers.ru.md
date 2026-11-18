# Числа контекста: три вида, кто что отдаёт и что читает клиент (2026-11-16)

Разбор долга **12c** (`debts.ru.md`) по живому архиву второго стенда
`ollamalegion-diag-…-20261116-145334.zip` (снят `2026-11-16T14:53:34Z`).
Образы в архиве: балансер `r261115-1635` / commit `e98c26c9` (`identity.json`),
воркеры `gpu-r261115-1637` / commit `e98c26c9`, llama.cpp `b1b770d90`
(2026-11-13). Модель на обоих узлах одна: `Qwen3.8-27B-UD-Q4_K_M`, `parallel=1`,
`max_slots=1`, `kvCacheType=q4_0`, GGUF-потолок `262144`.

Дата в записи — день архива. Числа в тексте взяты из архива и из кода; всё, что
не доказано архивом или кодом, помечено словами «не проверено» или «оценка».

**Итог одной строкой.** Три величины — не дефект: это разные вопросы
(«сколько всего», «сколько одному клиенту», «сколько попросил клиент»), и на
этом стенде при `parallel=1` суммарное и на слот совпали до токена
(`180224/180224`, `188416/188416`). Дефект — в **цепочке**: в одном архиве
два разных значения одного потолка, поверхность `/api/models` теряет поле
окна на слот, лог «upgrading» описывает действие, которого не было, а решение о
росте окна посчитано по оценке промпта, завышенной ровно в 1.9–2.0 раза.
Поэтому 12c **не закрыт**, а сужен до четырёх конкретных пунктов (§5).

---

## 1. Девять чисел, которые все называются «контекст»

| Величина | Что это | Где вычисляется / откуда берётся |
|---|---|---|
| **total n_ctx** | суммарное окно инстанса; слоты его делят | `internal/cppbackend/backend.go:924` (`EffectiveContextSize`), загрузка — `internal/cppbackend/backend.go:906` (`ContextPerSlot`) для обратного пересчёта |
| **context_per_seq** | окно ОДНОГО слота (`n_ctx_seq` у llama.cpp) | воркер: `cmd/cppworker/handlers_model.go:1529` (`context_per_seq`), `internal/cppbackend/backend.go:906` |
| **client num_ctx** | что попросил клиент в теле (`options.num_ctx` / `num_ctx`) | `internal/balancer/num_ctx_resolver.go:50` (`ExtractNumCtxFromBody`) |
| **declared (профиль)** | окно, заявленное оператором в карточке модели | `profile.contextLength` / `profile.contextPerSeq` / `contextLengthMax`; см. `summary.json` → `model_profiles.*.profile` |
| **growth ceiling** | до какого окна разрешён АВТО-рост | `internal/balancer/nctx_declared_r255.go:139` (`effectiveAutoReloadCeiling`) = `max(env, declared)` |
| **effectiveContextLength** | то же число в терминах профиля (для WebUI) | `internal/balancer/nctx_ceiling_r267.go:87` (`ProfileEffectiveWindow`) |
| **GGUF max** | физический потолок модели из метаданных | `cmd/cppworker/handlers_model.go:1505` (`gguf_context_length`), зеркало в балансере — `internal/balancer/llamacpp_metrics_poller.go:518` |
| **feasible / max_vram_n_ctx** | сколько окна влезает в VRAM при текущей раскладке | `cmd/cppworker/nctx_feasibility.go:347` (`nctxModeFields`), `internal/cppbackend/backend.go:2154` |
| **needed (роста)** | «сколько токенов нужно запросу» = оценка промпта + бюджет вывода, но не меньше запрошенного клиентом | `internal/balancer/growth_needed_rule_r330.go:33` (`neededWindowTokensR330`) |

Правило llama.cpp, на котором всё стоит:
`n_ctx_seq = GGML_PAD(n_ctx / n_seq_max, 256)`, шаг 256 — `ContextPadStep`
(`internal/cppbackend/backend.go:879`), выравнивание — `PadContext`
(`internal/cppbackend/backend.go:892`). Арифметика проверена тестами
`internal/balancer/context_math_guard_r493_test.go`; в этом архиве она **не
нарушалась ни разу** (при `parallel=1` делить нечего).

ЗАЧЕМ столько разных чисел. Это не «одно число в трёх видах», а четыре
независимых ответа: физика модели (GGUF), политика оператора (профиль),
политика авто-роста (потолок), потребность конкретного запроса (needed).
Ошибка начинается там, где одно из них выдают за другое.

---

## 2. Что отдаёт каждая поверхность

| Поверхность | Поле | Какую величину несёт | Код |
|---|---|---|---|
| `/api/ps` | — | **ничего**: окна в ответе нет вообще | `internal/balancer/llamacpp_handlers_readonly.go:710`, структура `OllamaProcess` — `internal/balancer/ollama_router_tags.go:32` |
| `/api/tags` | — | **ничего**: имя, размер, digest, details | `internal/balancer/llamacpp_handlers_readonly.go:163`; воркер — `cmd/cppworker/handlers_model.go:2873` |
| `/api/show` | `model_info.context_size` | окно **на слот** (R402) | воркер `cmd/cppworker/handlers_model.go:3263` |
| `/api/show` | `model_info.context_size_total` | суммарное n_ctx | `cmd/cppworker/handlers_model.go:3272` |
| `/api/show` | `model_info.parallel` | число слотов | `cmd/cppworker/handlers_model.go:3272` |
| `/api/show` | `model_info.<arch>.context_length` | GGUF-потолок | `cmd/cppworker/handlers_model.go:3278` |
| `/api/show` | `parameters` = `num_ctx N` | окно **на слот** (как Modelfile у Ollama) | `cmd/cppworker/handlers_model.go:3287` |
| `/v1/models` | `context_length` | окно **на слот** загруженной копии | `internal/balancer/llamacpp_handlers_readonly.go:917` |
| `/v1/models` | `max_context_length` | `max(maxContext, ggufMaxContext)` | `internal/balancer/llamacpp_handlers_readonly.go:920` |
| ответ на инференс | `X-Model-Context-Length` | окно **на слот** (`min(ContextLength, ContextPerSeq)`) | `internal/balancer/capabilities_headers.go:57`, `:90` |
| ответ на инференс | `X-Model-Max-Context` | действующий потолок обслуживания **на слот** (R469 + R505) | `internal/balancer/capabilities_headers.go:45`, `internal/balancer/effective_window_advertise_r469.go:35` |
| ответ на инференс | `X-Model-Max-Context-Total` | тот же потолок **суммарно** (окно×слоты), выставляется при `slots>1` (R505) | `internal/balancer/capabilities_headers.go:48` |
| ответ на инференс | `X-Model-Gguf-Max-Context` | GGUF-потолок, только если он выше действующего | `internal/balancer/capabilities_headers.go:48` |
| ответ воркера | `X-Model-Context-Warning` | `ok/approaching/overflow/impossible`, `nCtx` = **эффективное окно запроса** (override клиента, если он есть) | `cmd/cppworker/nctx_clamp.go:247`, `:260`, `:342`; проброс — `internal/balancer/llamacpp_native_path.go:124` |
| воркер `/api/models` | `context_size` | суммарное n_ctx | `cmd/cppworker/handlers_model.go:1504` |
| воркер `/api/models` | `context_per_seq` | окно на слот | `cmd/cppworker/handlers_model.go:1529` |
| воркер `/api/models` | `parallel`, `max_slots` | слоты заведены / слоты реально обслуживают | `cmd/cppworker/handlers_model.go:1521`, `:1557` |
| воркер `/api/models` | `gguf_context_length`, `feasible_max_context`, `max_vram_n_ctx`, `n_ctx_degraded`, `n_ctx_headroom` | GGUF / физика / признак оффлоада | `cmd/cppworker/handlers_model.go:1505`, `:1677`–`:1694`, `cmd/cppworker/nctx_feasibility.go:353` |
| воркер `/api/info` | `models[].contextSize`, `contextPerSeq`, `parallel` | те же суммарное и на слот | `cmd/cppworker/utils.go` (снимок модели), архив: `backends/<id>.info.json` |
| балансер `/api/models` (агрегат) | `contextSize` | **только суммарное**; `context_per_seq`, `parallel`, `max_slots`, `gguf_context_length` в ответ НЕ попадают | `internal/balancer/llamacpp_handlers_readonly.go:609` (структура ответа), `:662` |
| `/api/v1/backends`, `diag/summary` | `llamaCpp.loadedModels[].contextLength`, `context_per_seq`, `parallel`, `maxSlots`, `maxContext`, `capabilities.maxContext`, `capabilities.loadedContextSize` | суммарное + на слот + GGUF + факт | поллер `internal/balancer/llamacpp_metrics_poller.go:493`–`:539` |
| `/api/v1/backends` | `loadedModels[].parameters` | ПРОИСХОЖДЕНИЕ параметров ЗАГРУЗКИ (`request`/`profile`/`backend`) | `internal/balancer/load_provenance_r119.go:161`, `:211` |
| API профилей | `profile.contextLength`, `effective.effectiveContextLength`, `effective.limitedBy`, `effective.limitReason` | заявленное оператором и потолок авто-роста | `internal/api/handlers_cppworker_profiles.go:64`, `:70` |
| дамп воркера | `last-prompt.json` → `n_ctx_override`, `n_ctx_loaded` | что клиент попросил и с каким суммарным окном работал запрос | `cmd/cppworker/debug_last_prompt.go:58` |

Два вывода из таблицы, которые не очевидны:

1. **`/api/ps` и `/api/tags` не отвечают на вопрос про окно вообще.** Клиент,
   который «посмотрел модели» через привычные ollama-ручки, не узнал ни одного
   числа контекста. Это не дефект (так устроен протокол), но объясняет, почему
   клиент берёт окно из своей настройки.
2. **Суммарное окно отдаётся ровно тремя ручками** (воркер `/api/models`,
   `/api/show.context_size_total`, `last-prompt.n_ctx_loaded`) и ещё двумя в
   агрегатах (балансер `/api/models`, `/api/v1/backends`). Остальные поверхности
   говорят «на слот». Путаница «суммарное против на слот» возможна только там,
   где `parallel > 1`.

---

## 3. Живые числа стенда (архив 2026-11-16T14:53:34Z)

| Величина | `cppworker-gpu` (мастер) | `A10-126` |
|---|---|---|
| total n_ctx (`context_size`, `n_ctx_loaded`) | **180224** | **188416** (до 14:47:30 — 180224) |
| `context_per_seq` | 180224 | 188416 |
| `parallel` / `max_slots` | 1 / 1 | 1 / 1 |
| GGUF (`gguf_context_length`) | 262144 | 262144 |
| `feasible_max_context` | 230058 | 262144 |
| `max_vram_n_ctx` | 162247 | 0 (`vram_known=true`) |
| профиль: `contextLength` / `contextPerSeq` / `contextLengthMax` | 131072 / 131072 / 131072 | то же |
| профиль: `effective.effectiveContextLength` | **262144** (`limitedBy=env`) | то же |
| потолок авто-роста (runtime) | 262144 | то же |
| `summary.json → nctx_reload.auto_reload_max_n_ctx` | **131072** | то же |
| клиент: `options.num_ctx` (тело запроса) | 131072 | 131072 |
| воркер: `n_ctx_override` (`last-prompt.json`) | 131072 | 131072 |
| воркер: `prompt_tokens` (`last-prompt.json`) | 89794 | 103031 |
| `loadedModels[].parameters.contextPerSeq` | `{source: request, value: 131072}` | `{source: profile, value: 131072}` |

Что из этого получил клиент (по коду и этим метрикам, в момент 14:53):

- `X-Model-Context-Length: 180224` (мастер) и `188416` (A10) — то есть одно и
  то же имя модели отдавало **разное** окно с разных копий (класс 12e, но для
  окна копии, а не для потолка);
- `X-Model-Max-Context: 262144` (`min(GGUF 262144, maxContext 262144, потолок
  262144)`), `X-Model-Gguf-Max-Context` не выставлялся (GGUF не выше
  действующего);
- `/v1/models`: `context_length` 180224 / 188416, `max_context_length` 262144;
- `/api/show`: `context_size` = `context_size_total` = 180224 / 188416,
  `parameters = "num_ctx 180224"` / `"num_ctx 188416"`,
  `qwen35.context_length = 262144`;
- балансер `/api/models`: `contextSize` 180224 / 188416 (поля на слот нет — §5.3);
- `/api/ps`, `/api/tags`: чисел контекста нет.

Пара, которая и составляла «расхождение» из записи 12c: **профиль и тело
говорят 131072, загружено 180224**. Это не ошибка поля: профиль — ХИНТ
первичной загрузки (R43/R68, `internal/balancer/num_ctx_resolver.go:749`), а окно
растёт по потребности до потолка 262144. Воркер это подтверждает дословно:

```
14:47:30.086 handleLoadModel: applied model profile (R364) ctxSize=188416
           gpuLayers=-2 kvCacheType=q4_0 parallel=1 profile_ctx=131072
14:47:30.086 reloading model because parameters changed changed=["contextSize 180224→188416"]
```

`profile_ctx=131072` и `ctxSize=188416` в одной строке — это нормальная картина
«хинт против факта», а не два мнения об одном пределе.

---

## 4. Кто что читал: факты из логов

| Кто | Запросов | Что читал про контекст |
|---|---|---|
| агент (`192.168.151.151`) | 58 × `POST /api/chat` | **ничего**: ни `GET /api/tags`, ни `/api/ps`, ни `/api/show`, ни `/v1/models` за весь буфер лога (2 ч 51 мин, 12:01:12→14:52:55) нет |
| WebUI (`172.19.0.1`) | 2 × `/api/tags`, 2 × `/api/ps` | эти две ручки чисел контекста не несут (§2) |

Тело запроса агента (из `backends/cppworker-gpu-bundled-agent.bundle/request-last.json`):

```json
"options": {"temperature": 0, "num_predict": 0, "num_ctx": 131072, ...}
```

То есть **131072 — это политика самого клиента**, а не прочитанное у нас число:
наши поверхности в тот момент отдавали 180224/188416 и 262144. Совпадает оно с
заявленным в профиле окном (131072) — вероятный источник (конфиг клиента или
карточка модели в WebUI), но доказать это архивом нельзя: дампа клиента в
архиве нет. **Это остаётся неясным** (см. §5.6).

Все 58 запросов пришли с `num_ctx=131072`: 57 из них при загруженных 180224,
один — при 188416. `num_predict=0`, поэтому бюджет вывода в расчётах окна — 4096
(`internal/balancer/preflight_agent_window_r253.go:85`,
`LB_AGENT_OUTPUT_RESERVE_TOKENS=4096`).

---

## 5. Ответ на вопрос 12c: семантика или дефект

### 5.1 Что дефектом НЕ является

1. **Три величины существуют одновременно** — так и должно быть: это разные
   вопросы (§1).
2. **`contextLength=180224 ≠ profile.contextLength=131072`** — хинт против
   факта; ровно этого требует R43/R68/R385 (профиль не предел обслуживания).
3. **`effectiveContextLength=262144` выше профиля** — это `max(env, declared)`
   при `env=262144` (`internal/balancer/nctx_ceiling_r267.go:104`–`:110`),
   потолок АВТО-роста, а не обещание окна. Имя поля неудачное, но число верное.
4. **Разные окна у копий** (180224 против 188416) после роста — следствие
   независимого роста по узлам; это 12e/12d, уже записано.

### 5.2 ДЕФЕКТ 1: в одном архиве два потолка авто-роста, и оба выдаются за истину

`summary.json` содержит два блока:

- `nctx_reload` — это **сырая секция конфига** (`internal/api/diag_summary_r476.go:113`
  кладёт `s.config.Balancing.NCtxReload`): `auto_reload_max_n_ctx=131072`,
  `preflight_enabled=false`, `preflight_async_reload=false`;
- `nctx_ceiling` — это **действующее значение** (`:121` → `ClusterNCtxCeiling` →
  конфиг координатора после env-override,
  `internal/balancer/nctx_reload_config_bridge.go:126`): `envCeiling=262144`.

Что было на самом деле, доказывают строки того же архива:
`preflightNCtxReload(async): starting reload … target_n_ctx=188416` — то есть
preflight работал, был асинхронным и поднял окно **выше** 131072. Три поля блока
`nctx_reload` противоречат поведению, которое видно в том же архиве, и ни одно
из них не подписано «это config.json до env-override».

Цена: оператор, читая архив, делает вывод «авто-рост ограничен 131072» и
ищет причину роста в другом месте. Это ровно тот класс, из которого выросли
R267/R469 («два мнения об одном потолке»).

### 5.3 ДЕФЕКТ 2: `/api/models` балансера теряет окно на слот

Воркер отдаёт `context_size`, `context_per_seq`, `parallel`, `max_slots`
(`cmd/cppworker/handlers_model.go:1504`–`:1569`). Агрегат балансера
`/api/models` (`internal/balancer/llamacpp_handlers_readonly.go:609`–`:622`)
перечисляет только `contextSize` — остальные поля **не парсятся и не отдаются**.
На этом стенде это невидимо (`parallel=1`, суммарное = на слот), но при
`parallel=2` та же поверхность вернёт суммарное окно, выдав его за окно модели —
буквально дефект R83 (`ContextPerSlot` введён именно против него), только на
другой ручке. Проверить живьём нельзя: в архиве нет ни одного узла с
`parallel>1`. Это **не проверено**, но код читается однозначно.

### 5.4 ДЕФЕКТ 3: «ResolveNumCtx … upgrading» не доезжает до запроса

В логе 58 строк вида:

```
ResolveNumCtx: R60.37 — body num_ctx < loaded, upgrading
  body_n_ctx=131072 loaded_per_seq_n_ctx=180224
```

Слово «upgrading» обещает, что запрос поедет с 180224. Факт воркера на обоих
узлах: `n_ctx_override=131072` (`last-prompt.json`), то есть запрос работал с
числом клиента. Причина двойная:

1. generic-путь (`/api/chat`) использует значение резолвера **только для
   автозагрузки** (`internal/balancer/proxy_request.go:260`), заголовок
   `X-Cpp-Ctx` там не ставится вообще (единственные два места —
   `internal/balancer/num_ctx_resolver.go:1006` и
   `internal/balancer/nctx_reload_handlers.go:314`, оба на пути обработчиков);
2. даже на пути обработчиков `X-Cpp-Ctx` — это **верхний предел**, а не
   предписание: `cmd/cppworker/nctx_clamp.go:106`–`:114` поднимает
   `NCtxOverride` только когда в теле `num_ctx` нет, а меньшее значение из тела
   не трогает (это закреплено тестом
   `cmd/cppworker/nctx_clamp_internal_test.go:105` «body n_ctx not raised by
   header»).

Следствие по числам: эффективное окно запроса = `min(клиент, загруженное)`, а
балансер считает и пишет в лог, что обслужил загруженным. Для этого архива
вреда нет (промпт 89–103k влезал в 131072), но для промпта 150k при загруженных
180224 клиент упрётся в свои 131072 — а лог при этом будет утверждать, что окно
поднято. Второе следствие: `X-Model-Context-Warning` считает проценты от
`n_ctx_override` (`cmd/cppworker/nctx_clamp.go:260`–`:265`), то есть тоже от
числа клиента.

### 5.5 ДЕФЕКТ 4: решение о росте посчитано по оценке, завышенной в 1.98 раза

Единственный 503 в архиве (1 из 63 трасс; 62 ответа 200):

```
14:47:30.085 R385: стриминговый запрос требует большего окна backend=A10-126
             needed_tokens=184100 target_per_client=188416
14:47:30.717 proxyRequestLlamaCpp: preflight triggered async n_ctx reload, returning 503
             msg="needs 184100 tokens of context; the loaded window is 180224"
```

Арифметика восстановлена по коду до токена:
`184100 = 180004 (оценка промпта) + 4096 (бюджет вывода)` по
`neededWindowTokensR330` (`internal/balancer/growth_needed_rule_r330.go:33`);
`184320 = PAD256(184100)`, `188416 = 23 × 8192` — ступень роста
`nctxGrowthStepR493` (`internal/balancer/nctx_growth_target_r493.go:44`).

А факт — в логе через 3 секунды: `prompt tokens: … estimated_tokens=180004
exact_tokens=93891`. Это строка повтора того же хода (клиент повторил запрос
через 2 с), и оценка у него совпала с оценкой отказа — **180004 до токена**, то
есть текст тот же. Значит **оценка 180004 против факта 93891 (×1.917)**, и по
факту запросу нужно `97987` при загруженных `180224` — окно покрывало его с
запасом 1.84×, роста не требовалось вовсе.

Почему решение взяло оценку: точный счёт идёт через
`POST /api/tokenize` с жёстким лимитом **1.5 с**
(`internal/balancer/exact_prompt_tokens_r473.go:55`), а при таймауте/ошибке
функция возвращает `false` **молча** (`:217`–`:223`): ни строки в логе, ни
счётчика. По всему архиву 40 строк точного подсчёта на 58 запросов; у 18
строки нет вовсе, и различить «таймаут» и «кэш» (кэш пишется в Debug, а
`LB_LOG_LEVEL=info`) по архиву нельзя.

Насколько систематично завышение (все 40 измерений архива):

| | значение |
|---|---|
| отношение оценка/факт | 1.917 … 2.010 |
| среднее | 1.979 |
| примеры | 132508/67009 = 1.977; 192426/95778 = 2.009; 180004/93891 = 1.917 |

Цена ошибки конкретно здесь: перезагрузка 27B на A10 (принята 14:47:30.215,
`model loaded` 14:47:33.594 — 3.38 с; при `useMmap=true` веса не перечитываются
с диска, пересоздаётся KV-cache), при этом загрузка пошла с предупреждением
`R83 n_ctx выше VRAM-границы … max_vram_n_ctx=0 … stage=degraded` и с
`gpuLayers=62` из 64 (у мастера на той же модели 65 слоёв на GPU,
`offloadDegraded` не выставлен). То есть лишний рост не только «стоил
перезагрузки», но и оставил копию в частичном оффлоаде.

Клиент после 503 повторил запрос через **2 с** (при `Retry-After: 60`), повтор
шёл 246 с и закончился обрывом на стороне клиента (`tokens_sent=0`,
`reason=client_cancel`). Утверждать, что эти 246 с — следствие нового окна,
нельзя: мастер в том же интервале отдал ход за 244.9 с (это уже 12p, кэш
префикса).

### 5.6 Что осталось неясным

- **Откуда клиент взял 131072.** Доказано только, что это его собственное
  число (он не читал наших поверхностей в этом окне). Совпадение с
  `profile.contextLength` — гипотеза.
- **Почему у 18 из 58 запросов нет строки точного подсчёта** — таймаут 1.5 с
  или кэш; лог не различает (Debug не пишется).
- **Поведение при `parallel>1`** — на стенде таких узлов нет, §5.3 проверен
  только чтением кода.
- **Как клиент реагирует на `X-Model-Context-Warning`** — в архиве нет ни
  одного запроса, где уровень был бы не `ok` (при 103031/131072 = 78.6 % порог
  80 % не достигнут).

### 5.7 Все пути отказа считают ОДНИМ правилом (R447, 2026-11-17)

Дополнение к §5.5. Дефект 4 разобран был как «решение посчитано по завышенной
оценке», но у него была вторая половина: **отказывал не тот механизм, который
должен был**. Правило R447 («окна мало» ≠ «окно надо растить») жило только в
двух местах — вердикте стриминга (`growth_stream_guard_r301.go:317`) и
preflight-гейте агентского окна (`preflight_nctx.go:770`). А решение
«отказать/ждать» в четырёх остальных считалось сравнением `needed > loaded` (или
просто по «нужен рост»), где `needed = оценка + 4096`:

| место (до правки) | условие | что делало |
|---|---|---|
| `growth_guard_r299.go:88` | `needed <= loaded` → не вмешиваемся; иначе при стриме/перезагрузке → отказ | 503 «cannot grow right now» до preflight |
| `context_growth_r288.go:114` | `needed > loaded` + занятость | 503 вместо постановки в admission-очередь |
| `growth_stream_guard_r301.go:330` | «нужен рост» (R447) + занятость, не-стриминг | 503 «… cannot grow right now (active streams=1 …)» — ровно текст из архива (`proxy.go:1577`, `nctx_reload_handlers.go:798`) |
| `window_decision_r493.go:174` | рост нужен + `Active>0`/`ReloadPending` | вид `defer` (503 на не-стриминговом пути) |

Числа живого случая (этот же архив): окно **180224**, оценка промпта **180200**,
needed **184296** = 180200 + 4096, чужой стрим, клиент окно не просил (в
§5.5-варианте — просил 131072, что меньше загруженного и потому окно не
требует). Промпт влезал, и воркер обслужил бы запрос укороченным ответом.

Теперь правило одно на все четыре места — `windowServeAsIsR447`
(`internal/balancer/window_serve_as_is_r447.go`):

- отказ/ожидание оправданы, только если **промпт не влезает** в окно слота;
- или клиент **явно попросил окно больше загруженного** (иначе воркер отвергнет
  чужой `num_ctx`: «requested n_ctx=16384 exceeds … n_ctx=8192», живой дефект
  R301);
- иначе запрос обслуживается загруженным окном, даже когда «нужен рост».

Правило РОСТА при этом не менялось: его по-прежнему считает `growthNeedR447`
(запас под ответ 256 токенов — причина растить, а не причина отказывать).

Проверено тестами (все — без живого стенда, `&Proxy{}` + `httptest`):
`TestR447_ServeAsIsRule` (таблица правила), `TestR447_LiveDefectNoRefusalWhenWindowServesAsIs`
(окно 180224: оценка 180200 → отказа НЕТ ни на guard'е, ни на вердикте R301
(стриминг и не-стриминг); оценка 200000 → отказ остался),
`TestR447_ShortPromptSmallWindowNoGrowthNoReload` (промпт «hi» при окне 2048:
needed 4098 > 2048 — ни роста, ни отказа), `TestR447_UnifiedDecisionAgreesWithGuard`
(единое решение R493 и guard дают один вид), `TestR447_BlockerServesAsIs`
(blocker R288). Файл — `internal/balancer/window_serve_as_is_r447_test.go`.
Четыре мутации (по одной на каждое место) роняют свой тест; разбор —
`docs/plans/2026-11-17-window-growth-unification.md`.

**Остаток, который этой правкой не закрыт:** когда росту ничто не мешает, окно
всё ещё растят, если под ответ остаётся меньше 256 токенов (`growthNeedR447` п.3,
R253) — и это решение по-прежнему опирается на ОЦЕНКУ, когда токенизатор не
успел (§5.5). То есть «лишняя перезагрузка» из §5.5 остаётся возможной, а
«ложный 503» — уже нет.

---

## 6. Как читать числа оператору

| Вопрос | Где смотреть | Не смотреть |
|---|---|---|
| сколько токенов у модели ВСЕГО | воркер `/api/models` → `context_size`; `/api/show` → `context_size_total`; дамп воркера → `n_ctx_loaded` | `/v1/models`, `/api/show.context_size` (это на слот), `/api/ps` |
| сколько получит ОДИН клиент | `/v1/models` → `context_length`; `X-Model-Context-Length`; `/api/show` → `context_size`; `/api/v1/backends` → `context_per_seq` | суммарное окно (при `parallel>1` оно больше) |
| какое окно ЯВНО ЗАПРОСИЛ клиент | тело запроса `options.num_ctx`; дамп воркера → `n_ctx_override` | `X-Model-Context-Length` (это факт инстанса, а не запрос) |
| сколько окна РАЗРЕШЕНО вырастить | профиль модели → `effective.effectiveContextLength`; `X-Model-Max-Context` | `summary.json → nctx_reload` (это config.json, не действующее значение — §5.2) |
| физический предел модели | `gguf_context_length`, профиль → `<arch>.context_length` | `feasible_max_context` (это про VRAM сейчас, не про модель) |
| сколько влезает в VRAM сейчас | `max_vram_n_ctx` + `vram_known`; `n_ctx_degraded` | `0` без `vram_known` — это два разных «0» (`pkg/types/llama_cpp_metrics.go:84`) |
| почему клиент получил 503 | лог: `R385 … needed_tokens/target_per_client`; тело: `requiredTokens`/`loadedWindowTokens` | фраза `upgrading` в `ResolveNumCtx` — она не гарантирует, что запрос поедет с этим числом (§5.4) |

Три правила, чтобы не повторить разбор заново:

1. **Суммарное и на слот — разные поля, а не разные правды.** Если `parallel=1`,
   они равны; если нет — сравнивать нужно то, что нужно: клиентский запрос
   всегда сравнивается с окном НА СЛОТ.
2. **Профиль — хинт, а не потолок.** Расхождение «профиль 131072, загружено
   180224» — штатное.
3. **`needed` в отказе — это оценка промпта, а не факт.** Перед выводом «окна не
   хватило» проверьте строку `prompt tokens: … exact_tokens=` рядом по времени;
   если её нет — число могло быть завышено вдвое.

---

## 7. Следующий шаг (по убыванию цены ошибки)

1. **Диагностика честного `nctx_reload`** (§5.2). Самый дешёвый и самый
   вредный: в `diag/summary` подписывать источник (`nctx_reload_config` —
   как в конфиге, `nctx_ceiling` — как действует) либо отдавать действующее
   значение в обоих блоках. Проверка: новый архив, в котором
   `nctx_reload.auto_reload_max_n_ctx` совпадает с `nctx_ceiling.envCeiling`
   при заданном `LB_NCTX_RELOAD_MAX_N_CTX`.
2. **Резолвер и запрос должны говорить одно** (§5.4): либо резолвер не пишет
   «upgrading», когда значение никуда не едет, либо значение доезжает (патч
   тела/заголовка с явной семантикой «поднять до загруженного»). Нужен
   сторож: «если в логе `upgrading`, то `n_ctx_override` в `last-prompt.json`
   равен поднятому числу». Сначала — решение о семантике: поднимать окно
   запроса до загруженного или позволять клиенту ограничивать себя.
3. **Точный подсчёт не должен проигрывать молча** (§5.5): при
   таймауте/неудаче `POST /api/tokenize` писать Info/Warn с
   `estimated_tokens`, и/или не принимать решение о росте окна по оценке,
   пока факт не получен (в пределе — увеличить лимит 1.5 с или кэшировать по
   префиксу). Проверка: в новом архиве на каждый запрос ровно одна строка
   `prompt tokens` с явным `source`/`fallback`.
4. **`/api/models` балансера — отдать `context_per_seq`/`parallel`/`max_slots`**
   (§5.3). Тест: при `parallel=2` `contextSize` не должен читаться как окно
   клиента. Цена низкая, но правка затрагивает поверхность, которой пользуются
   и WebUI, и внешние клиенты; делать после 1–3.

Go-код в этой задаче не менялся — это решение следующего шага, а не текущего.
Исключение — §5.7 (R447, 2026-11-17): правило отказа вынесено в одну функцию
`windowServeAsIsR447` и применено в четырёх местах; пункт 3 выше остаётся в силе
только для решения «расти».

### 5.8 Что из §7 уже сделано (R493, 2026-11-17)

| Пункт §7 | Что сделано | Чем проверено |
|---|---|---|
| 3 — точный подсчёт не должен проигрывать молча | (а) R493-12c(4): повтор токенизации при таймауте + Warn с причиной. (б) **R493 (эта правка):** точный подсчёт больше НЕ зависит от хранилища калибровки — в `applyPromptCalibration` ранний выход по `p.promptCalib == nil` стоял ДО вызова `applyExactPromptTokensR473`, поэтому без калибровки (тесты, лёгкая сборка, прокси без state) все решения шли по оценке ×1.92–2.01 | `internal/balancer/exact_tokens_without_calibration_r493_test.go`: два теста (без калибровки и с ней), мутация «вернуть ранний выход» → красный |
| 2 — резолвер и запрос говорят одно | R493-12c(3): лог подъёма окна стал честным (`X-Cpp-Ctx` — верхний предел), поля `effective_request_n_ctx`, `header_role` | сторож `autoload_window_source_r127_test.go` |
| — (новое, из архива второго стенда) | **Перезагрузка не начинается, пока на бэкенде идёт чужой ответ.** Проверки занятости стоят в решателях ДО решения, но между решением и unload+load клиент успевает начать ход. Живой факт: reload в 06:24:53 и 06:25:43 при идущих ходах Cline, узел в 06:25:02 с `partial offload 62/64` | `internal/balancer/reload_busy_guard_r493_test.go`: отказ хука = NoOp и 0 reload-запросов к воркеру; разрешение = reload запускается (положительный контроль). Мутация «условие отключено» → красный |
| — (новое) | **Противоречие профиля называется при сохранении**: `contextLength > contextLengthMax` даёт `warnings[].code=context_length_exceeds_max` в ответе PUT и Warn в лог. R400 по-прежнему отдаёт приоритет заявленному окну, но молчаливого сохранения больше нет | `internal/api/profile_window_conflict_r493_test.go`: таблица случаев (живой 194296/131072 → предупреждение, равные → нет, нули → нет) + страж «ветка рабочая». Мутация «ветка мертва» → красный |
| — (новое) | **`bridge_info` больше не выдаёт нули за факт**: у блока появился `known:false` + `note`, когда числа пришли не от C-моста (раньше клиент видел `current_n_ctx: 0, required_n_ctx: 0` рядом с настоящим `code` и читал это как «окно 0») | `cmd/cppworker/error_bridge_info_r493_test.go`: `known=false`, `note` не пуст, числовые ключи сохранены. Мутация «убрать known» → красный |

### 5.9 Отказ декода: «нули как факт» и сравнение с суммарным окном (R493, 2026-11-17)

Пользователь принёс новый архив (`…-4c13682715b5-20261117-145351.zip`) с тем же
текстом, что и раньше:

```
stream inference failed with code 1: llama_decode failed for prompt batch
(likely n_ctx overflow, prompt too long, or n_batch > n_ctx)
(current_n_ctx=0, required_n_ctx=0, max_vram_n_ctx=218989)
```

Три отдельные причины, все воспроизведены по архиву:

1. **Текст видимого отказа собирает НЕ `cmd/cppworker`.** Предыдущая правка
   (`bridge_info.known=false`, см. §5.8, последняя строка) закрыла JSON-ошибку
   синхронного пути, но стриминговый отказ приходит строкой в done-чанке, и её
   формировал `c/bridge/bridge.go` (`stream inference failed with code %d: …`).
   Там числа печатались безусловно: `current_n_ctx`/`required_n_ctx` — из полей,
   которые при generic-ошибке декода мост вообще не заполняет, а
   `max_vram_n_ctx` — из «последнего известного» `g_max_vram_n_ctx` (218989 к
   запросу отношения не имеет). То есть ноль и чужое число выдавались за
   измерение, и оператор третий раз искал причину в окне. Живой факт:
   `A10-126/last-prompt.json` — `prompt_tokens=80878`, `n_ctx_loaded=131072`, то
   есть **в окно запрос влезал** и причина была другая (59/64 слоёв на GPU при
   2025 MB свободной VRAM, `n_batch=512`).
2. **Классификация шла по суммарному окну, а не по окну слота.** При
   `parallel>1` `llama_n_ctx` — это окно на слот. Запрос, который в суммарное
   окно влезает, а в окно слота нет, доходил до декода и отдавался как
   generic code 1 без шанса на верное решение (reload/413).
3. **`n_batch` не урезался до окна слота**, хотя в самом тексте ошибки
   `n_batch > n_ctx` назван одной из трёх причин.

**Сделано:** чистая логика вынесена в `c/bridge/window_fit.{c,h}` и покрыта
ctest — `c/bridge/tests/test_window_fit.c`, 16 проверок:

- `bridge_window_effective_seq(n_ctx, n_ctx_seq)` — окно слота (`llama_n_ctx_seq`)
  или суммарное, если слотов один;
- `bridge_window_fit_kind(...)` → `OK` / `RELOAD` (клиент просит больше окна
  слота, даже если меньше суммарного) / `TOO_LONG`;
- `bridge_window_suggest_batch(...)` — `n_batch` не больше окна слота;
- `bridge_decode_failure_message(...)` — текст с настоящими числами
  (`prompt_tokens`, `n_predict`, `n_batch`, `n_ctx`, `per slot`) и прямым
  выводом: если по числам запрос влезает, текст говорит «причина не в окне, а в
  памяти/раскладке; проверьте VRAM и уменьшите n_batch (напр. 128)».

`c/bridge/bridge.c`: `bridge_check_ctx_capacity` классифицирует по окну слота;
оба места (потоковый и не-потоковый prefill) урезают `n_batch` до окна слота;
оба места отказа декода берут текст из `bridge_decode_failure_message`.
`c/bridge/bridge.go`: `formatBridgeNumbers` печатает только непустые числа, а
когда их нет — «(числа не измерены: ошибка не про окно…)». Балансер
(`internal/balancer/llamacpp_error.go`) в `NCtxError.Error()` печатает только
измеренные `current_n_ctx`/`required_n_ctx`, иначе — «числа окна не измерены».

**Чем проверено (мутации):**

| Что проверяли | Мутация | Результат |
|---|---|---|
| Окно слота в классификации | `bridge_window_fit_kind`: сравнение с `n_ctx_total` вместо окна слота (ровно прежнее поведение) | красный: 2 из 16 (`TOO_LONG` и `RELOAD` не распознаны) |
| Вывод «причина не в окне» | `bridge_decode_failure_message`: `fits = 0` (всегда заявлять переполнение) | красный: 1 из 16 |
| Числа в тексте балансера | `NCtxError.Error()`: печать `current_n_ctx=%d, required_n_ctx=%d` без проверки на ноль | красный: воспроизведён живой текст `… (current_n_ctx=0, required_n_ctx=0)`, 3 из 3 подтестов |

Мутации проверялись и в Go (`internal/balancer`), и в C (`gcc -std=c11
-I c/bridge c/bridge/tests/test_window_fit.c c/bridge/window_fit.c` + запуск:
`OK (16 проверок)` до и после восстановления; `fc /b` подтвердил, что файл
вернулся байт в байт).

**Остаётся не покрытым:** сам `c/bridge/bridge.c` (встраивание хелперов в
пути декода) юнит-тестом не покрыт — он проверяется сборкой (CI c-bridge job,
образ cppworker) и живым прогоном. Это записано в `debts.ru.md` (12u).

**Живая проверка (2026-11-17, образ `cppworker:gpu-r261117-r493d` = коммит
43f0c9aa, RTX 3070, `Qwen3-4B-Instruct-2507-Q4_K_M`, загруженное окно 4096):**

```
POST /api/generate {"options":{"num_ctx":8192,"num_predict":64}}
→ HTTP 400
  error: generate failed: inference failed: code=2 requested n_ctx=8192 exceeds the
         per-slot window=4096 (n_ctx=4096, slots derived from it) …
         (current_n_ctx=4096, required_n_ctx=8192, actual_tokens=15,
          n_predict=64, max_vram_n_ctx=63996)
  bridge_info: known=true, code=2, current_n_ctx=4096, required_n_ctx=8192
```

Тот же запрос потоком даёт ровно тот же текст в done-чанке
(`stream inference failed with code 2: …`) — то есть в той строке, где раньше
стояли `current_n_ctx=0, required_n_ctx=0`. Нулевых полей нет ни в одном ответе,
«окно слота» названо словами, `n_batch` (512) в тексте присутствует.

Проверено также, что предохранитель Go-уровня не даёт дойти до декода напрасно:
промпт 33600 символов (8411 реальных токенов) при окне 4096 и `num_predict=256`
→ **413** с `actual_prompt_tokens=8411` и `deficit_tokens=4828` (это путь
`prompt exceeds n_ctx even with minimum n_predict floor`, он работал и раньше).

**Не проверено живьём:** generic-отказ декода (code 1) — его нельзя вызвать
нарочно, не ломая стенд; текст для него покрыт `test_window_fit` и Go-тестом.

**Отдельный дефект стенда, найденный в этом же архиве:**
`A10-126/info.json` — `llama_build_env=0eadefebd` против `llama_build=b1b770d90`,
`llama_build_mismatch=true`. Причина — в `.env` второго стенда:
`CPPWORKER_LLAMA_BUILD=0eadefebd`. В репозитории `deployments/.env.example`
исправлен на `b1b770d90` с комментарием, почему расхождение опасно (переменная
попадает в образ как ярлык сборки, и любой guard «одна ли сборка» начинает
врать). Сам `.env` стенда репозиторием не управляется — его правит владелец
стенда.

---

## 8. Чем проверено

```powershell
# архив распакован в .diag12c (в git не попадает); ключевые выписки:
#   summary.json → cluster.backends[].llamaCpp.*, model_profiles.*.effective
#   balancer-logs.txt → ResolveNumCtx / prompt tokens / R385 / preflight
#   backends/*.info.json → models[].contextSize/contextPerSeq
#   backends/*.bundle.zip → last-prompt.json (n_ctx_override/n_ctx_loaded),
#                           request-last.json (options.num_ctx),
#                           logs.txt (contextSize A→B, R83 warning)
```

Счётчики архива: `ResolveNumCtx` — 58 строк (тело `131072` во всех, загруженное
`180224` в 57 и `188416` в 1); `prompt tokens` с точным подсчётом — 40 строк;
`request trace` — 63 (62 × 200, 1 × 503 на стадии `preflight`); `reloading model
because parameters changed` — 1 строка (`contextSize 180224→188416`).

Проверка ссылок документации: `python scripts/check_docs_claims.py` →
`расхождений нет`.

Проверка правила отказа (§5.7, R447):

```
go test -tags llama_stub ./internal/balancer/ -run 'TestR447_' -count=1 -v
#   TestR447_ServeAsIsRule, TestR447_LiveDefectNoRefusalWhenWindowServesAsIs,
#   TestR447_ShortPromptSmallWindowNoGrowthNoReload,
#   TestR447_UnifiedDecisionAgreesWithGuard, TestR447_BlockerServesAsIs
# полный пакет: go test -tags llama_stub ./internal/balancer/ -count=1  → ok
```

Проверка отказа декода (§5.9, R493):

```
# C: чистая логика окна (ctest в CI, локально — gcc):
gcc -std=c11 -I c/bridge -o test_window_fit c/bridge/tests/test_window_fit.c c/bridge/window_fit.c
./test_window_fit                      # → OK (16 проверок)

# Go: числа в тексте ошибки
go test -tags llama_stub ./internal/balancer/ -run 'TestNCtxError_ErrorPrintsOnlyMeasuredNumbers_R493' -count=1 -v
```

## Память: VRAM и RAM в решении о загрузке (R512, 2026-11-18)

**Вопрос оператора:** «на r508c балансера и r493d воркера неправильно считает доступную
память, игнорируя оперативную — проверь, что это учитывается в новых версиях».

**Ответ по коду HEAD:** системная память учитывается, и вот где:

| Слой | Что читает RAM | Где |
|---|---|---|
| Бюджет | `ProbeRAM()` → `RAMAvail`, `RAMTotal`, `RAMLimit` (cgroup), резерв `RAMReserve=4096 МиБ`, доля `RAMUtil` | `internal/memfit/budget.go`, `internal/cppbackend/memfit_adapter.go:254-300` |
| Потолки | `Ceilings()` возвращает `exactFitCtx` (только VRAM) и `hardCtx` (VRAM + RAM, частичный offload через `computeSplit`) | `internal/memfit/fit.go:229-280` |
| Вердикт | `Evaluate()` считает, сколько слоёв на GPU/CPU, и отказывает с причиной вида «CPU-части нужно X, доступно Y» | `internal/memfit/fit.go:286+` |
| Гейт n_ctx у воркера | `MaxVRAMNCtx = v.MaxExactFitCtx`, `MaxRAMNCtx = v.MaxHardCtx` | `cmd/cppworker/memfit_gate.go:126-134` |

**Проверка на числах стенда** (A10 24 ГБ: VRAM 22 502 МиБ, свободно 22 268; RAM 21 162 МиБ,
доступно 19 583; модель 16 464 440 224 Б, 64 слоя, 4 KV-головы, head_dim 213):

| KV | KV-слоёв | KV на токен | потолок VRAM (`exactFit`) | потолок с RAM (`hard`) |
|---|---|---|---|---|
| f16 | 64 | 213.0 КиБ | 24 356 | 118 147 |
| f16 | 17 | 56.6 КиБ | 91 694 | 262 144 |
| q8_0 | 64 | 113.2 КиБ | 45 847 | 222 456 |
| **q8_0** | **17** | **30.1 КиБ** | **172 603** | 262 144 |
| q4_0 | 64 | 59.9 КиБ | 86 599 | 262 144 |

**Что это значит для живого случая.** Загрузка шла с `q8_0` и 17 KV-слоями → потолок VRAM
172 603 токена, окно 131 072 проходит; фактически занято ~19.5 ГиБ из 22, свободно 2.4 ГиБ.
`llama_decode` на промпте 65–71k упал не из-за RAM (свободно 19.6 ГиБ) и не из-за окна, а
из-за **буферов декода**: фиксированный резерв `CPPWORKER_VRAM_OVERHEAD_MB=1500` не растёт с
окном. Это записано долгом **12z** (`docs/architecture/debts.ru.md`) с планом измерения.

**Чего НЕ утверждаем.** Что причина доказана измерением пика VRAM — нет: вывод следует из
чисел (свободно 2.4 ГиБ, отказ на длинном промпте), но профиля VRAM при декоде мы не снимали.
Рекомендация до измерения: на 24 ГБ и 27B Q4 держать окно ≤ 100k либо поднять
`CPPWORKER_VRAM_OVERHEAD_MB` до ≈ 3000.

**Старый образ r493d** этой правки не содержит: там гейт считал своими формулами (до
объединения в `memfit`, R498), и в логе видно `max_ram_n_ctx=262144` — «RAM-путь» заявлял
любой контекст до обучающего. То есть на стенде действительно работала старая арифметика;
на HEAD число считается через `Ceilings` и таблица выше — его результат.
