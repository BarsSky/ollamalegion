# Аудит согласованности: «одна величина — разные источники» (2026-11-19)

**Дата:** 2026-11-19. **Статус:** 🟠 **АУДИТ ЗАВЕРШЁН, РАБОТЫ НЕ НАЧАТЫ** — передано в поток разработки.
**Номер раунда:** не закреплён. `R496` занят параллельным потоком (`cmd/cppworker/tool_guard_r496.go` в рабочем дереве на момент аудита).
**Зачем:** после R495 S1 (`gpu_diagnosis` в `/api/metrics`, коммит `be958404`) остались места, где **одно и то же
понятие считается из нескольких независимых источников**, и они расходятся. Именно этот класс дефекта в проекте
уже приводил к живым поломкам (R66d, R71, R83, R117, R121, R447, R451, R493).

**Как проверялось.** Чтение кода (`read`/`grep`) по четырём областям параллельно + `go build -tags llama_stub ./...`
— зелёный, то есть ломающих сборку нестыковок нет: все находки семантические. Каждый пункт помечен
**«проверено»** (перечитан по строкам) или **«гипотеза»** (требует стенда). Документация проекта принималась
как есть, расхождения «док ↔ код» фиксируются отдельно, а не «код прав, док виноват».

---

## 1. Карта находок

Обозначения: **К** — критично (ломает обслуживание или даёт ложные числа для решений),
**В** — важно (расхождение, которое аукнется при следующей правке), **М** — мелочь.

### 1.1 Вместимость и конкурентность

| # | Сер. | Где | Суть |
|---|---|---|---|
| C-1 | К | `backend.go:450-457`, `collector.go:621` | `NodeClaimedCapacity` (заявка узла, посчитанная **один раз** при старте агента) стоит в резолвере выше статики и оператора; «замерзает» до рестарта агента |
| C-2 | К | 20+ мест: `candidate.go:98`, `slot_manager.go:138`, `backend_selector.go:609,932`, `scoring.go:44`, `backend_registry.go:525` | Читают **статический** `MaxConcurrentReqs` вместо `EffectiveMaxConcurrentRequests()`; операторский лимит (`RuntimeMaxConcurrentRequests`) этими путями не соблюдается |
| C-3 | К | `slot_manager.go:140`, `backend_selector.go:609,677`, `availability_first_r493.go:51`, `ui-renderer.js:894` | Ноль трактуется **пятью** способами (без лимита / слотов нет / лимит 1 / 10 в UI); `-1` документирован как «без лимита» (`handlers_backends.go:1802-1809`), а на части путей даёт «слотов нет» |
| C-4 | В | `backend_registry.go:626-642` | `UpdateBackendLimits` пишет поля бэкенда под `p.mu` без `state.mu` — нарушение контракта R493e; `SetBackendRole` — то же |
| C-5 | В | `cluster_state.go:256` | `/api/v1/cluster` в ветке без агента отдаёт **статическую** вместимость рядом с эффективной (та же итерация, `:92/:99` считают эффективную) |
| C-6 | В | `state.go:326-328` | `reconcileNodeCapacity` объявляет операторский лимит «от ноды» (`runtimeMax == MaxConcurrentReqs`), после чего его перетирает заявка узла |
| C-7 | М | `backend.go:450-457` | Отчёт врёт об источнике: заявка узла подписывается как `CapacitySourceOperator` |

### 1.2 Метрики (единицы и двойной счёт)

| # | Сер. | Где | Суть |
|---|---|---|---|
| M-1 | К | `handlers_autotune_admin.go:242-244` | МБ подаются в поля `...Bytes` → `autotuneReserveBytes(total 8192)` = 268 МБ > `FreeVRAMBytes` = 7097 → «бюджет 0.0 ГБ, модель не влезает». В соседнем файле конверсия есть: `handlers_backends.go:947-959` (`MBToBytes`) |
| M-2 | К | `collector.go:1013-1017` | `mergeGPUMetrics` затирает **сумму по всем картам** значением GPU 0 (`nvml_unix.go:220`, `gpu_common.go:108`): хост 2×8 ГБ видит 8192 вместо 16384, при `UUIDs` из двух карт |
| M-3 | В | `cluster_state.go:254` | `TotalGPUUsage` суммирует по бэкендам без дедупа по UUID → 196 % на двух бэкендах одной карты |
| M-4 | В | `scoring.go:24-30`, `:245-252` | Нулевые метрики (агент прислал пустую структуру) читаются как «максимально свободен»: `baseScore=65`, штраф за загрузку 5 вместо 20 |
| M-5 | В | `backend_selector_vram_r248.go:36-47`, `image_resources.go:1362-1377` | Снимок VRAM движка используется как живой без проверки возраста; для бэкендов без агента не очищается никогда |
| M-6 | В | `setup_plan_r96.go:204-221` | Дедуп по UUID не применён в суммировании VRAM для плана раскладки → sharded-группа на одной карте |
| M-7 | В | `adaptive_loader.go:991-1115` | `EnvironmentProfile` — снимок VRAM на старте процесса, только GPU 0; `Refresh()` не вызывается ниоткуда |
| M-8 | М | `collector_ollama.go:181-186` | Агент всегда отдаёт `ActiveRequests=0` → `freeSlots` на `/api/v1/backends` расходится с прокси-счётчиком |
| M-9 | М | `handlers_backends.go:892-894` | В `usagePercent` подставляется процент занятой **памяти** |
| M-10 | М | `composition_r95.go:258-263` | МБ/с → байт/с через `1000*1000`, остальной код — `1024*1024` |

### 1.3 Настройки без потребителя

| # | Сер. | Настройка | Где читается → почему не влияет |
|---|---|---|---|
| S-1 | К | `AdaptiveWeightTuner` (веса) | `weight_tuner.go:36,273` → `GetWeights()` только в тестах; скоринг берёт `p.config.Balancing.Scoring` (`scoring.go:107`). На llama.cpp-пути `RecordOutcome` вообще недостижим (`proxy_request.go:355,358` раньше `:737`) |
| S-2 | К | `LB_GPU_MAX_USAGE`, `LB_CPU_MAX_USAGE`, `LB_MEMORY_MAX_USAGE`, `ramHeadroomPercent` | `config.go:188-206` → пороги в `slot_manager.go:191` (`>90`), `:214` (`>95`), `:227` (`>98`) зашиты; конфиг-значение используется только как выключатель (`>0`) |
| S-3 | К | `LB_GPU_MAX_TEMP` / `maxTemperature` | `config.go:190` → потребителя нет вовсе |
| S-4 | В | `balancing.scoring.modelLoadingCost` | `pkg/types/balancing.go:249` → единственное вхождение в non-test Go; реальный штраф зашит 5/10/20 (`scoring.go:244-252`). Документирован как рабочий вес (`config.example.json:157`, `balancing-guide.ru.md:202`) |
| S-5 | В | `LB_QUEUE_WORKERS`, `LB_QUEUE_TIMEOUT` | `queue_manager.go:24-25,135` → только поля в `/api/v1/queue/stats`; пула воркеров нет с R78 |
| S-6 | В | `balancing.scoring.*` = 0 | `defaultIfZero` (`scoring.go:161-166`) подменяет ≤0 дефолтом: «выключить» компонент нулём нельзя |
| S-7 | В | `apply_suggestions.go:111-126` | `applyFn == nil` → статус всё равно `applied` |
| S-8 | М | `LB_AUTOTUNE_APPLY` | Работает, но документирован только в `feature-switches.ru.md:96`, в `.env.example` нет |

### 1.4 Таймауты

| # | Сер. | Где | Суть |
|---|---|---|---|
| T-1 | К | `first_byte_default_r189.go:28`, `llamacpp_transport.go:383-385` | Действующий first-byte = **180 с**, а загрузка 16–27B — 140–485 с по вашему же репозиторию (`autoload_wait_r325_1.go:15`, `llamacpp_transport_nonstream.go:130`). Заголовки воркер отдаёт только **после** ленивой загрузки (`handlers_chat.go:304→313`), значит таймаут считает загрузку |
| T-2 | К | `autoload_wait.go:38` vs `feature-switches.ru.md:66` / `stack.yml:573` / `.env.example:570` | Три дефолта одной переменной: 180 / 900 / 180 |
| T-3 | К | `autoload_wait.go:73` vs `:142` (=`autoLoadBudgetCeiling`) | Один `-1` («ждать сколько нужно») даёт **30 мин** на одном пути и **24 ч** на другом |
| T-4 | К | `proxy_request_openai_auto_stream_r258.go:43`, `stream_stall_r297.go:51` | Два капа по 300 с взведены по умолчанию вопреки доктрине «таймаут только opt-in» (`timeout_policy.go:20-23`) |
| T-5 | К | `feature-switches.ru.md:46` vs `proxy_timeout.go:40-47`, `stack.yml:357` | Док объявляет `LB_STREAMING_NEVER_TIMEOUT` «вкл», код — выключено |
| T-6 | В | `nctx_reload.go:224-227` vs `preflight_nctx.go:1587-1609` | `Retry-After` клампится на 120 с при оценке reload ≈186 с (16.4 ГБ / 131072) и живых 280–480 с → второй reload |
| T-7 | В | `session_manager.go:255-262` | Порог «зомби» 120 с удаляет сессию с **живым** стримом (`LastRequestAt` не обновляется в стриме) |
| T-8 | В | `llamacpp_handlers_inference.go:347-351` vs `llamacpp_transport.go:383` | Дефолт 180 с применяется только на нативном пути: один запрос, разное поведение по URL |
| T-9 | В | `loading_retry.go:42,45`; `cppbackend/load_stall_r227.go:31-45` | Ожидание загрузки 30 с и сторож 300 с против живых 140–485 с; комментарий `:16` говорит «не прерывает load», код `:76-82` вызывает `onStall()` |
| T-10 | В | `model_latency_tracker.go` | «Адаптивные тиры» не получают семплов никогда (`RecordSample` только в тестах), а код ссылается на них как на рабочие (`proxy_timeout.go:183-200`) |
| T-11 | В | `cmd/cppworker/main.go:299`; `agent/embedded.go:295`; `collector.go:305` | `CPPWORKER_WRITE_TIMEOUT=1800` и `HEARTBEAT_INTERVAL=3s` молча не парсятся; документированный `COLLECT_INTERVAL=5` ниже минимума 10 с |
| T-12 | М | `config.json:33-36`; `balancing-guide.ru.md:227`; `unload_scheduler.go:24-38` | `requestTimeout`/`firstByteTimeout`/`streamingIdleTimeout` из конфига игнорируются; RAM-порог в доке 85 %, в коде 98 %; `idleUnloadAfter` в доке «15m» по умолчанию, в коде выключен |

### 1.5 Окно контекста и KV-cache

| # | Сер. | Где | Суть |
|---|---|---|---|
| N-1 | К | `stream_growth_r385.go:143-145` + `nctx_growth_target_r493.go:64,77` + `:212` | В одной функции три единицы: `ceiling` — СУММАРНЫЙ, `loaded` — на слот, `needed` — на клиента. Потолок превышается до ×slots (27B, parallel=2, потолок 131072 → reload на 262144) |
| N-2 | К | `nctx_reload.go:749-750,779,1241` vs `cmd/cppworker/utils.go:551-568` | Реактивный `prompt_too_long`: требование воркера (на слот) едет в `contextSize` как TOTAL → reload без выигрыша, повторный 413 |
| N-3 | К | `cmd/cppworker/adaptive_loader.go:580-587` | Двойной множитель: таблица `bytesPerKVCacheType{f16:4}` уже включает K+V, а код умножает ещё на 2 → KV ×2, `maxViableNCtx` вдвое меньше, окно молча урезано. Тот же класс прямо описан в `auto_offload.go:62-64` |
| N-4 | В | `adaptive_loader.go:781` | Формула `cpu_only` без `kv_layers`/`key_length` → завышение до 3.13× (Qwen3.8: 17/65 слоёв, head_dim 256); корректная формула — на 80 строк выше в той же функции |
| N-5 | В | `autotune_budget_r230.go:110-129` | Четвёртая формула KV: `q4_0=/4`, `q8_0=/2` против memfit 18/32 и 34/32 → завышение до 3–6×, AutoTune советует урезать окно зря |
| N-6 | В | `preflight_nctx.go:1422,1450,1475,1560` vs `nctx_reload.go:1436` | `LastKnownNCtx` пишут в разных единицах (на слот / суммарно); читатели считают суммарным → результат зависит от порядка записи |
| N-7 | В | `preflight_helper.go:195-199` | `cacheLoadedContextLength(per-client)` пишется в `ContextLength` (TOTAL); `ContextPerSeq` остаётся старым → ложные «не влезает» |
| N-8 | В | `capabilities_headers.go:36-48`, `effective_window_advertise_r469.go:35-59` | Объявляем клиенту **суммарное** окно (`X-Model-Max-Context`), а `num_ctx` клиента трактуем как на слот → клиент, поставивший наше же число, получает 413 или 2× reload |
| N-9 | В | `preflight_agent_window_r253.go:36`, `growth_request_need_r447.go:61` vs `cmd/cppworker/inference.go:468-500` | «Влезает ли промпт»: запас 256 у балансера против жёстких 512 и `minNPredictClamp` у воркера → для room 300–511 балансер решает «обслужить», воркер отвечает 413 |
| N-10 | В | `cmd/cppworker/inference.go:864-878` | RAM-fallback грузит модель окном **запроса** (на слот) как суммарным; при `parallel>1` слот получает вдвое меньше — молча |
| N-11 | В | `placement_over_reload_r293.go:85,149` vs `:197-210` | Размещение: `requiredPerSeq` (на слот) против `backendWindowCeilingR293` (total) |
| N-12 | М | `nctx_reload_handlers.go:1215-1246`, `growth_needed_rule_r330.go:37-44` | Остаток «оценка + бюджет вывода (4096)» в решении о росте (класс R451) |
| N-13 | В | `window_decision_r493.go:87,142,216,222` + `window_decision_paths_r493.go:88` | Поле `Ceiling` объявлено «потолком НА КЛИЕНТА», но получает **суммарный**; R466-урезание тот же `Ceiling` **делит на слоты** (`:142`), а отказ и цель сравнивают без деления (`:216,:222`). **Ловушка для реализации S4:** «починить по комментарию» = разделить на слоты дважды. Активно при `LB_WINDOW_UNIFIED=1/2` |
| N-14 | В | `cmd/cppworker/inference.go:55-67,434-448` | `loadedModelNCtx` возвращает суммарное окно (`ContextSize`) и подставляется как «окно запроса» при `NCtxOverride<=0` → при `parallel>1` промпт в слот не влезает; ловит только C-классификация (`c/bridge/bridge.c:1051`) уже после HTTP 200 (класс «пустой ответ») |

### 1.6 Дефект вне пяти областей, найденный попутно

| # | Сер. | Где | Суть |
|---|---|---|---|
| X-1 | К | `proxy.go:703-705` + `cmd/balancer/main.go:243-244,447-449` | Планировщик выгрузки и тюнер весов создаются **дважды** и оба запускаются; подмена указателя не останавливает первый → его `lastUsed` не обновляется никогда |
| X-2 | К | `unload_scheduler.go:228-251,275-303` + `candidate.go:99` | «Защита от выгрузки активных моделей» читает legacy-списки, которые после R78 всегда пусты; «выгрузка» = метка `WarmingUpModels[lru_unload]` на 2 мин, а такая метка **исключает модель из кандидатов**. `RecordModelUse` — только по завершении запроса (`proxy_request.go:734`). При `idleUnloadAfter` модель под нагрузкой снимается с маршрута |
| X-3 | В | `backend_selector.go:869,875` vs `model_name_match.go:32` | Два правила сравнения имён: канонический `modelNameMatches` (нормализует `.gguf`, тег, регистр) и ad-hoc `strings.Contains` в 8+ точках решений |

---

## 2. Корень: четыре класса, а не сорок дефектов

1. **Одна величина — несколько источников.** Вместимость: `MaxConcurrentReqs` / `RuntimeMaxConcurrentRequests` /
   `NodeClaimedCapacity` / `RuntimeModelSlots` / `EffectiveMaxConcurrentRequests()`. Окно: `ContextLength` /
   `ContextPerSeq` / `LastKnownNCtx` / `TargetNCtx`. Лечится не правкой формул, а **одним геттером** и запретом
   читать поле напрямую (страж-тест по исходнику, как в R493e).
2. **Единицы.** Насос: total↔per-slot (окно), МБ↔байты (AutoTune), МиБ↔МБ, байт/элемент ↔ байт/токен (KV),
   сек↔мс↔нс (таймауты), `1000`↔`1024`. Правило: имя переменной или поля обязано нести единицу.
3. **Настройка без потребителя.** 8 имён + 2 поля конфига. Это ровно тот класс, из-за которого удаляли
   `balancing.algorithm`. Лечится тестом-стюардом, который проверяет не «есть ли имя в файле», а «есть ли
   потребитель» (сейчас стюард видит только буквальные `os.Getenv("LB_…")` — `feature_switches_steward_test.go:28`).
4. **Частный фикс вместо общего правила.** R83 починил KV в `auto_offload.go`, но в `adaptive_loader.go` осталась
   своя таблица; R66d завёл `modelNameMatches`, но `backendHasModel` осталась на `Contains`; R447 применён на
   четырёх путях, а формула «оценка + 4096» осталась в решении о росте.

---

## 3. План (S1 → S4), каждый шаг — отдельный коммит

### S1. Ложные числа без риска для маршрутизации (самое дешёвое и самое заметное)
- M-1 (МБ→байты в AutoTune), N-3/N-4 (множитель KV в `adaptive_loader` — привести обе ветки к `memfit.KVBytesPerElement`),
  N-5 (удалить четвёртую формулу KV, звать memfit).
- T-1: `LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC` по умолчанию **0** либо из `EstimateFirstByteTimeoutFromModelSize`
  (эстиматор уже написан и не используется).
- T-2/T-3: один дефолт и один смысл `-1`; T-5: док привести к коду.
- Критерий: `go test ./internal/... ./cmd/... -race` зелёные; на стенде AutoTune показывает реальные ГБ,
  холодная загрузка 16 ГБ перестаёт обрываться на 180-й секунде.

### S2. Настройки: либо потребитель, либо удаление
- S-2/S-3: `slot_manager.go` читает `Resources.GPU.MaxUsagePercent` / `CPU` / `Memory` / `MaxTemperature` /
  `ramHeadroomPercent` вместо констант 90/95/98.
- S-1: тюнер весов либо подключается в `computeEnhancedComponents`, либо удаляется вместе с логом «weights adjusted»
  и `GetWeightTuner` (решение владельца: скорее удалить — на llama.cpp-пути `RecordOutcome` недостижим).
- S-4/S-5/S-7: `modelLoadingCost` — потребитель или удаление из конфига и доков; `LB_QUEUE_WORKERS/TIMEOUT` — удалить;
  `apply_suggestions` — `applied` только после реального применения.
- Страж: тест «у каждой `LB_*` из `.env.example` есть потребитель» (расширить `feature_switches_steward_test.go`
  на `env.Get*`/`intFromEnv`/`envSeconds`/`settings.HotInt`).

### S3. Вместимость под флагом (как R493 `selection_order`)
- C-1/C-2: один геттер `EffectiveMaxConcurrentRequests()`; 20+ точек перевести на него;
  `NodeClaimedCapacity` — только при `LB_CAPACITY_FROM_MODEL_SLOTS=true` и с TTL/пометкой возраста.
- C-3: единая функция `capacityFromValue(n) → (value, known)` вместо пяти трактовок нуля; `-1` — либо запретить
  в API, либо трактовать как «без лимита» везде.
- C-4: `state.mu` в `UpdateBackendLimits`/`SetBackendRole`; C-5/C-6 — исправить подпись источника.
- Флаг `LB_CAPACITY_SINGLE_SOURCE=off|shadow|on`, дефолт `shadow`, чтобы включение было осознанным.

### S4. Окно и выгрузка
- N-1/N-2: привести все точки роста и реактивного reload к одной единице (per-slot), с тестом-инвариантом
  «total ≤ потолка И per-slot ≥ нужного».
- **Осторожно (N-13):** `decideWindowR493.Ceiling` сейчас одновременно total (источник) и per-client (контракт).
  Прежде чем «делить на слоты», зафиксировать **одну** семантику поля и покрыть тестом оба чтения;
  иначе получим деление на слоты дважды.
- N-14: `loadedModelNCtx` не должен подставляться как окно запроса при `parallel>1` (либо делить на слоты, либо
  брать `ContextPerSeq`).
- N-6/N-7/N-8: одно поле для «последнего известного окна» и одна единица; объявление `X-Model-Max-Context` — либо
  на слот, либо с явным суффиксом.
- N-9: выровнять запас 256/512 (одна константа на оба конца).
- X-1/X-2: убрать двойное создание контроллеров; «активные запросы» для выгрузки брать из admission-очереди,
  а не из legacy-списков; метка выгрузки не должна исключать модель из кандидатов.
- X-3: `backendHasModel` на `modelNameMatches`.

---

## 4. Тесты и мутации (минимум для приёмки)

1. МБ→байты: `AutoTune` на бэкенде с `MemoryFree=7097` даёт бюджет > 5 ГБ (мутация: убрать `MBToBytes` — красный).
2. KV: `maxViableNCtx` совпадает с `memfit.KVCacheBytesForModel` на фикстуре Qwen3.8 (мутация: вернуть `2*` — красный).
3. Окно: `parallel=2`, потолок 131072, запрос 131072 → цель reload ровно 131072, не 262144.
4. First-byte: при `LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC` пустом холодная загрузка не обрывается до 300 с.
5. Вместимость: `PUT /limits=1` при `MaxConcurrentReqs=4` → все пути видят 1 (мутация: вернуть статику — красный).
6. Ноль/`-1`: таблица значений × все точки (`/api/v1/backends`, `/api/v1/cluster`, admission, слоты) — одна трактовка.
7. Стюард env: переменная без потребителя → красный тест.
8. Выгрузка: модель с активным стримом не получает метку `lru_unload` и не выпадает из кандидатов.

---

## 5. Чего НЕ делать

- **Не переписывать формулы целиком.** Каждый пункт — точечная замена источника/единицы + тест-инвариант.
- **Не включать новые правила по умолчанию.** Как в R493: шов + флаг + `shadow`.
- **Не «делать доки правыми» вместо кода** там, где док описывает задуманное поведение (T-1, S-2, N-1): док в этих
  местах — это спецификация, а не опечатка.
- **Не трогать `EffectiveMaxConcurrentRequests` из классификатора R495** (см. §5.2 плана R495) — вместимость
  принадлежит R71/R83/R117/R121.
- **Не смешивать S1–S4 в одном коммите:** S1 и S2 меняют числа, которые оператор уже видит, их надо проверять
  на стенде отдельно.

---

## 6. Пять проверок на стенде (без правок кода)

```powershell
# 1) вместимость: три поверхности должны совпасть
curl -s localhost:8081/api/v1/backends | jq '.backends[]|{id,max:.maxConcurrentReqs,eff:.effectiveMaxConcurrentRequests,src:.maxConcurrentRequestsSource}'
curl -s localhost:8081/api/v1/cluster  | jq '.backends[]|{id,max:.maxConcurrentRequests,eff:.effectiveMaxConcurrentRequests}'
# после: PUT /limits=1 при статике 4 — все пути обязаны видеть 1

# 2) AutoTune: МБ против «0.0 GB»
curl -s localhost:8081/api/v1/metrics | jq '.gpu_diagnosis'
curl -s -X POST localhost:8081/api/v1/admin/autotune/plan -d '{"backendId":"…","modelName":"…"}' | jq '.recommendations[].reason'

# 3) окно: parallel=2, потолок 131072, клиент просит 131072
curl -s -X POST localhost:8081/api/v1/admin/autotune/plan -d '{"backendId":"…","modelName":"…"}' >/dev/null
# смотреть в логе «window decision … target_per_client» и body reload: contextSize должен быть ≤ 131072

# 4) first-byte: unload 16+ ГБ, затем streaming /api/chat
docker exec cppworker curl -s -X POST localhost:8080/api/models/unload -d '{"model":"…"}'
# ожидание: обрыв на ~180 с («upstream first-byte timeout»), воркер продолжает загрузку;
# контроль: LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC=0 — запрос доходит до конца

# 5) единицы env
# CPPWORKER_WRITE_TIMEOUT=1800 → строки «applied … from env» в логе быть НЕ должно (ParseDuration падает)
# COLLECT_INTERVAL=5 → WARNING о clamp до 10 с; HEARTBEAT_INTERVAL=3s → остаётся 5 с
```

---

## 7. Уже закрыто и что отклонено после проверки

- **R495 S1 закрыт** (`be958404`): `MemBusyPercent`/`ThrottleReasons`/`PState`/`PCIeLink*` доезжают до балансера
  и публикуются как `gpu_diagnosis`. Задача S2–S4 R495 остаётся отдельной.
- **Отклонено как дефект:** «перерегистрация агента обнуляет `NodeClaimedCapacity` и откатывает лимит оператора» —
  пересборка записи (`handlers_agents.go:316-395`) действительно не переносит поле, но сразу после неё вызывается
  `AdoptCapacityFromNode` (`:412-414`), а `RuntimeMaxConcurrentRequests`/`RuntimeCapacityFromNode` переносятся явно.
  Остаётся узкое окно до следующего heartbeat (внесено в C-1 как следствие, не как отдельная находка).
- **Отклонено:** `_ = os.Rename` в `llama_upstream_r269.go:317` — `persist` документированно best-effort, к решениям
  не относится.
- **Проверено как корректное (не дефект):** ns/ms/sec-конверсии в статистике токенов (`stats_windows_r493.go:183-188,285`,
  `llamacpp_translate_resp.go:331-340,816`), формула KV в `internal/memfit/kv.go:139-203` (совпадает с llama.cpp,
  включая SWA; «забыли K и V» здесь нет), насыщающая арифметика KV-бюджета (`autotune_budget_r230.go:44-60`),
  R447 на четырёх путях отказа (`context_growth_r288.go:135`, `growth_guard_r299.go:109`,
  `growth_stream_guard_r301.go:345`, `window_decision_r493.go:192`), `DecidePreflight` (4)/(5)
  — сравнения per-slot↔per-slot и total↔total (`preflight_nctx.go:847-882`), C-классификация окна
  (`c/bridge/window_fit.c:10-38`, использует `n_ctx_seq`).
