# Подсистема решений о памяти: `internal/memfit`

**Статус:** шаг 1 сделан (пакет + инварианты + golden-тесты). Шаги 2–4 — план.
**Контекст:** живая проверка R83 на реальном GPU (RTX 3070 + Qwen3.8-27B),
находки V1–V8 / R1–R12 — см. `plans/2026-09-25-qwen38-load-context-dup-and-infra.md` §11.

## 1. Это не набор багов, а один класс ошибок

Найденные дефекты выглядят разными (веса, KV, MemTotal, тег в имени, cgroup), но у
них шесть общих корней. Пока корни не убраны, следующий такой же баг появится на
новом поле (например, при добавлении второго GPU или mmap-режима).

| Корень | Как проявился |
|---|---|
| **Р1. Одна величина считается в нескольких местах** | VRAM/RAM оценивались в 5 местах: `CalculateResourceLimits`, `CalculateOptimalGPULayers`, `calculateLazyLoadOpts`, `EstimateGPUMemoryForModel`, C-bridge. Разошлись и по формулам, и по знаку ошибки |
| **Р2. Перегруженный ноль** | `MaxVRAMNCtx == 0` означает и «VRAM неизвестна», и «веса не влезают», и «нет GPU»; `Request.GPULayers == 0` — «ноль слоёв» вместо «авто» (эту же ловушку поймали в собственном новом коде тестом) |
| **Р3. Неявный fail-open** | `Known=false` → проверка пропускается; `fallback_no_fit` → «всё равно пробуем»; некорректный env RAM → 0 → «проверку пропускаем». Система в неопределённости выбирала «разрешить» и молчала |
| **Р4. Единицы не типизированы** | `ctxSize * 256 / 1024 / 1024` — байты там, где комментарий обещал килобайты (ошибка 1000×). Ни один тест не поймал, потому что тесты проверяли разницу между вызовами, а не абсолют |
| **Р5. Оценки зависят от окружения внутри функции** | `getSystemRAMGB()`, `/proc/meminfo`, NVML читаются посреди расчёта → тесты недетерминированы (на Windows жёсткие 8192), а поведение зависит от того, где запущено |
| **Р6. Нет ни одного теста на реальных числах** | Тесты — синтетические фикстуры и «стало больше/меньше». Поэтому `×0.7`, `256 Б/токен`, `MemTotal` и слепой к тегам lookup жили годами |

## 2. Принципы (зафиксированы в коде пакета)

1. **Одна формула на вопрос.** KV-cache, веса, сплит слоёв, потолки и вердикт
   считаются в `memfit`. Вызывающие только передают данные и читают `Verdict`.
2. **Единицы типизированы** (`Bytes`). Ошибка масштаба невозможна по типу.
3. **«Неизвестно» — отдельное состояние.** `Budget` несёт `Known`-флаги, стадия
   `unknown` явная, а `Policy.UnknownIsFatal` задаёт fail-closed. Ноль больше нигде
   не означает «не знаю».
4. **Один предикат на всё.** `computeSplit` — единственное место, где решается
   «помещается ли». Потолки (`Ceilings`) считаются по нему же (двоичный поиск), а
   не параллельной формулой: именно расхождение двух формул давало «рекомендуем
   n_ctx ≤ X» при «на X не помещается» (на A10 расхождение 393 MiB из-за
   целочисленного сплита слоёв — поймано тестом `TestInvariant_CeilingsMatchEvaluate`).
5. **Чистая функция + голден-тесты на реальном железе.** `Evaluate` не читает
   окружение и не логирует, поэтому её поведение проверяется абсолютными числами
   живого стенда (27B и gemma-4 на 3070 и A10).

## 3. Что уже есть

`internal/memfit` — без cgo и без зависимостей от `cppbackend`/`Bridge`, поэтому
тестируется обычным `go test`:

- `units.go` — типизированные байты, насыщающее вычитание, пропорции;
- `kv.go` — **единственная** формула KV (`2 × L × kv_heads × head_dim × bytes/elem`,
  f16 = 2, q8_0 = 34/32, q4_0 = 18/32 — числа llama.cpp, а не «/2 и /4 на глаз»);
- `budget.go` — `Budget` с `Known`-флагами, `Policy` (доля + резерв + fail-closed),
  `ProbeRAM` (env → `/proc/meminfo` → cgroup v2/v1);
- `fit.go` — `computeSplit`, `Ceilings`, `Evaluate`, `Verdict` с машинными
  `ReasonCode` и текстом-подсказкой, построенным из причин.

Тесты (22, все зелёные):

| Тест | Что фиксирует |
|---|---|
| `TestKVBytesPerToken_RealModels` | абсолютные числа KV для 27B и gemma-4 |
| `TestEvaluate_Golden_*` | вердикты на реальных бюджетах: 27B на 3070 (32768 q4_0 → partial/22; 131072 → отказ; 32768 f16 → отказ **с подсказкой про q4_0**), gemma-4 → partial/34, 131072 на A10 с q4_0 → partial/57 |
| `TestRegression_WeightsCountedInVRAMCeiling` | V1: потолок VRAM обязан учитывать веса (прежняя формула давала 103 389 на 8 GiB карте) |
| `TestRegression_NoArbitraryWeightFactor` | V3: потолок = «вес + ровно 1 GiB» / kvPerToken, без коэффициента 0.7 |
| `TestRegression_CgroupLimitRespected` | R5: лимит контейнера ограничивает бюджет |
| `TestInvariant_MonotonicInCtx` | если n_ctx не помещается, то и больший не помещается |
| `TestInvariant_SplitIsConsistentAndWithinBudget` | части не двоятся, каждая в своём бюджете, стадия соответствует числу слоёв |
| `TestInvariant_CeilingsMatchEvaluate` | потолок и вердикт согласованы (этот тест уже поймал ошибку в первой версии пакета) |
| `TestEvaluate_UnknownBudget_NeverClaimsFit` | из отсутствия данных нельзя вывести «поместится» (анти-fail-open) |
| `TestPolicy_MakesTradeoffExplicit` | резерв и доля — явные параметры, а не три разных закона в трёх местах |

## 4. Миграция (по шагам, без «большого взрыва»)

**Шаг 2. Адаптер `Budget` и shadow-режим — СДЕЛАНО.**
- `internal/cppbackend/memfit_adapter.go`: `MemfitBudget()` (VRAM из живого снимка
  `gpuDevices`, RAM из `memfit.ProbeRAM`, резервы 2048/4096), `MemfitSpec(name)`
  (резолв внешнего имени тем же путём, что и загрузка), `MemfitSpecFromValues`,
  `EffectiveKVCacheType`, `MemfitKVType`, `CompareLegacyVsMemfit`, счётчики.
- Хуки: `checkNCtxBeforeLoad` (гейт n_ctx) и `checkVRAMForModel` (решение об
  оффлоаде). Решение принимает **прежний** код; расхождение → `level=warn` +
  `cppworker_memfit_shadow_{comparisons,disagreements}_total` в `/metrics`.
- Тесты: `internal/cppbackend/memfit_adapter_test.go` (spec, бюджет, детекция
  расхождений, счётчики, effective KV).

**Живая проверка shadow-режима (RTX 3070, Qwen3.8-27B, контейнер рядом с рабочим
стеком).** Запрос `n_ctx=80000` (прежний гейт разрешил, загрузка пошла — 202):

```
level=warn memfit shadow: РАСХОЖДЕНИЕ с прежним решением гейта n_ctx
  model=qwen3.8:latest requested_n_ctx=80000 kv_cache_type=q4_0
  legacy_stage=exact_fit legacy_known=true legacy_max_ram_n_ctx=90531
  detail: вердикт: прежний=allow, memfit=does_not_fit
    stage=does_not_fit kv=q4_0 weights=15.33 GiB kv_total=4.64 GiB gpu_layers=19/65
    vram_used=5.84 GiB/6.00 GiB ram_used=14.14 GiB/13.27 GiB
    max_exact_fit_n_ctx=0 max_hard_n_ctx=66049
    reason=not_enough_ram_for_cpu_part(CPU-части нужно 14.14 GiB, доступно 13.27 GiB — не хватает 888 MiB)
```

Счётчики после этого: `comparisons_total 2`, `disagreements_total 2` (второй хук —
внутри загрузки; расхождение объясняется тем же V3: прежний код считает веса как
0.7×размер, memfit — полностью).

Что это доказывает на факте, а не на рассуждении:
1. прежний гейт назвал `stage=exact_fit` запрос 80 000 токенов на карте 8 GiB с
   весами 15.7 GiB — это V1 (потолок VRAM без весов) в чистом виде;
2. memfit отказывает и объясняет нехватку числом (888 MiB);
3. никакого влияния на поведение: запрос обслужен прежним кодом, контейнер убран,
   рабочий стек не тронут.

**Что осталось для перехода к шагу 3:** набрать статистику на A10 (там числа
прежнего кода наиболее далеки от реальности) и убедиться, что расхождения
объяснены — каждое либо дефект прежнего кода (их и должен поймать shadow), либо
ошибка в `memfit`. Критерий: расхождений, не объяснённых известными дефектами, нет.

**Шаг 3. Переключение решений.**
- `checkNCtxBeforeLoad` → `memfit.Evaluate`; 422 строится из `Verdict`
  (`ReasonCode` → код ошибки, `Suggestion` → текст).
- `checkVRAMForModel` → `memfit.Evaluate` (сплит слоёв вместо бинарного поиска),
  `DiagnosticsInfo` заполняется из `Verdict`.
- `calculateLazyLoadOpts` → `memfit.Evaluate`; `fallback_no_fit` превращается в
  настоящий отказ с `insufficient_resources` (R8).
- `/api/models`: `max_vram_n_ctx` = `Verdict.MaxExactFitCtx`,
  `max_ram_n_ctx` = `Verdict.MaxHardCtx`, **плюс новый явный `vram_known`** (V1).
- Балансер: читать `vram_known`/стадию вместо «0 = неизвестно»
  (`preflight_nctx.go:287,480`, `nctx_reload_handlers.go:96,424`).
- Удалить дубли: `EstimateGPUMemoryForModel` (V3/V4) и `EstimateModelVRAM` (V5)
  — либо делегировать в `memfit`, либо удалить вместе с тестами;
  `cmd/cppworker.availableRAMBytes` → `memfit.ProbeRAM` (R7).

**Шаг 4. Замок на будущее (CI).**
- `go test ./internal/memfit/` — обязательный и быстрый (без cgo).
- Новое правило: изменение оценок памяти без golden-кейса на реальной модели или
  нового инварианта не принимается.
- Проверка «нет магических чисел в оценках вне `memfit`»: grep-шаг в CI по
  `0.7`, `256`, `MemTotal`, `8192` в путях оценки (V3/V4/R6) — как
  предупреждение на ревью, затем как ошибка.
- `cppworker -fit <model> -ctx N [-kv q4_0]` — тот же код на стенде без загрузки
  (расширение существующего `-feasible`). Это делает «проверить на живом железе»
  дешёвым на будущее: `-feasible` даёт потолки, `-fit` — вердикт для конкретного
  запроса.

## 5. Что закрывается структурно, а что остаётся ручной работой

| Находка | Как закрывается |
|---|---|
| V1 веса не в VRAM-потолке | `Ceilings` вычитает веса; regression-тест |
| V2 своя оценка в bridge | `Budget` — единственный вход; bridge-оценка остаётся кросс-проверкой |
| V3 коэффициент 0.7 | веса учитываются полностью; `TestRegression_NoArbitraryWeightFactor` |
| V4 KV 256 Б/токен | `KVBytesPerToken` — одна формула с числами llama.cpp; `TestInvariant_KVOrdering` |
| V5 `EstimateModelVRAM` | удаляется на шаге 3 |
| V6 два оверхеда | один `Budget.VRAMReserve` из одного конфига |
| V7 NVML-слепота контейнеров | не решается в пакете: нужен арбитр (балансер) или запрет двух cppworker на одной GPU — **открытый вопрос** |
| V8 совет не знает текущий KV | подсказка строится `ReasonKVQuantLever` из фактического типа KV |
| R1–R4 (сделано ранее) | резолв имени и GGUF-контекст — вне пакета, но `Verdict` теперь это отражает |
| R5 cgroup | `ProbeRAM` читает v2/v1; regression-тест |
| R6 Windows | `ProbeRAM` возвращает `Known=false` вместо константы 8192 — вызывающий обязан решить политикой |
| R7 два ридера RAM | один `ProbeRAM` |
| R8 `fallback_no_fit` | шаг 3: настоящий отказ из `Verdict` |
| R9 `fallback_no_meta` | **ручная работа**: ветка обязана читать GGUF (в пакет не входит) |
| R10 0.7 на CPU-весах | веса считаются полностью |
| R11 неиспользуемый `useMmap` | в `memfit` флага нет вообще: рабочий набор — все веса (см. ниже) |

**Отдельное инженерное решение, зафиксированное в пакете:** mmap не уменьшает
требование к RAM. При инференсе все слои читаются на каждом токене, то есть рабочий
набор — вся модель; mmap лишь избавляет от обязательной копии, но при нехватке
памяти начинаются повторные чтения с диска (ровно это и наблюдалось: 13.97 GiB
resident, CPU 0.13 %, загрузка минутами — упор в I/O).

## 6. Решения, которые нужны от вас

1. **Политика по умолчанию:** `RAMUtil = 0.8` + резерв 4 GiB (как сейчас в
   `CalculateResourceLimits`) или `RAMUtil = 1.0` без резерва? Это прямая
   развилка «отказывать раньше» / «пробовать до конца». Сейчас в пакете
   зафиксировано 0.8 + резерв, но параметр явный.
2. **`UnknownIsFatal`:** считать ли отсутствие данных о VRAM/RAM отказом
   (fail-closed) или предупреждением. Рекомендую `true` для гейта загрузки и
   `false` для информационных полей `/api/models`.
3. **Точность сплита:** сейчас сплит пропорционален числу слоёв (как llama.cpp).
   Альтернатива — учитывать, что embedding/output тензоры не делятся. Даёт
   ±0.5 слоя; уточнять по живым замерам на A10.

## 7. Число слоёв для KV: разобрано и сведено к одному источнику

**Что было не так.** Для `Qwen3.8-27B-UD-Q4_K_M.gguf` в одном логе соседствовали
три числа, и KV считался не по тому:

| Величина | Значение | Что это |
|---|---|---|
| `qwen35.block_count` | **65** | `n_layer_all`: все блоки, включая MTP |
| `llama_model_n_layer()` | **64** | `n_layer()` = `n_layer_all − n_layer_nextn` (MTP = 1) |
| слоёв с KV-кэшем | **16** | только НЕрекуррентные (attention) слои; MTP-слой в кэш не входит — ИЗМЕРЕНО |

Откуда 16 (и почему не 17) — по исходникам llama.cpp и по живому замеру:
`src/models/qwen35.cpp:16` (nextn = 1), `:21-27` (ключа
`attention.recurrent_layers` в файле нет → правило по умолчанию: рекуррентные —
каждый слой, кроме каждого `full_attention_interval`-го, интервал = 4; MTP-слой не
рекуррентный → 48 рекуррентных из 64);
`src/llama-memory-hybrid.cpp:48` (attention-кэш строится с фильтром
`!hparams.is_recr(il)`); `src/llama-kv-cache.cpp:163-167` (слои без KV
пропускаются).

ПРЯМОЕ ИЗМЕРЕНИЕ (живой стенд): llama.cpp при загрузке напечатал
`llama_kv_cache: size = 144.00 MiB (8192 cells, 16 layers, 1/1 seqs), K (q4_0): 72.00 MiB, V (q4_0): 72.00 MiB`
и построчно `layer 3: dev = CPU` / `layer 0..2, 4..5: filtered`. Расчёт
2 × 16 × 4 × 256 × 18/32 × 8192 = 150 994 944 Б = 144.00 MiB совпадает точно.

**Насколько это было важно.** Формула «2 × block_count × kv_heads × (n_embd/n_heads)»
давала 221 520 Б/токен (f16) и 61 344 Б/токен (q4_0) — **в 3.38 / 3.33 раза** больше
фактических 65 536 / 18 432 Б/токен. Плюс `head_dim` KV берётся из
`attention.key_length` = **256**, а не из `n_embd/n_heads` = 213.

Следствия, которые это объясняет:

1. Потолки n_ctx (`max_vram_n_ctx`, `max_ram_n_ctx`) были занижены втрое.
2. Решение об оффлоаде уходило в `gpu_layers=0`: KV «съедал» всю VRAM, и модель
   грузилась целиком на CPU. Отсюда наблюдение «32768 работает, 65536/128000 — нет»
   на A10: при 32768 оценка ещё проходила, при 65536 KV по наивной формуле не
   оставлял места слоям.
3. На A10 с исправленным KV запрос `131072` с `kvCacheType=q4_0` становится
   **exact_fit**: веса 15 701 + KV 2 304 = 18 005 MiB при 20 980 MiB доступной
   VRAM, то есть все 65 слоёв на GPU. На f16 тот же запрос — частичный оффлоад 57/65.

**Что сделано.**

- `internal/cppbackend/kv_layers.go` — единственный источник: `KVLayersFor`
  (правило llama.cpp с точными ссылками) и `KVHeadDimFor`. Если правило
  неприменимо (архитектура не из проверенного набора: gemma4/gemma3n задают KV
  иначе; либо в файле есть явный список рекуррентных слоёв, который парсер не
  разбирает) — возвращается **верхняя оценка с `exact=false`**, а не выдуманное
  число.
- Парсер GGUF читает `attention.key_length`, `attention.value_length`,
  `nextn_predict_layers`, `full_attention_interval` и фиксирует наличие
  `attention.recurrent_layers`.
- `CalculateResourceLimits` (потолки n_ctx у гейта) и `internal/memfit` считают KV
  через эти значения. `estimateKVCacheMB`/`CalculateOptimalGPULayers` (решение о
  числе слоёв на GPU) и оценка C-bridge пока остаются на наивной формуле — это
  шаг 3: там изменение влияет на раскладку слоёв, и его надо делать вместе с
  переключением, имея shadow-данные.
- Тесты: `internal/cppbackend/kv_layers_test.go` (правило на числах реального
  файла, защита от угадывания, чтение ключей парсером),
  `TestRegression_KVUsesAttentionLayersNotBlockCount` в memfit (наивная формула
  обязана быть ~3.2× больше), golden-тесты обновлены под реальные числа.

**Чем подтверждено.** Метаданные прочитаны независимым парсером
(`tools/ggufprobe`, удалён после использования): `block_count=65`,
`nextn_predict_layers=1`, `key_length=256`, ключа `recurrent_layers` нет, тензоры
`blk.0..blk.64`. llama.cpp на живой загрузке печатает `print_info: n_layer = 64` и
`n_layer_all = 65` — оба счётчика совпадают с разбором. Дополнительно: загрузка
65 536/131 072 на наивной формуле считалась невыполнимой, но физически проходила —
это согласуется с KV в 3 раза меньше расчётного.

## 8. Шаг 3: решения переключены (сделано)

**Что теперь решает memfit, а не собственная формула:**

| Точка | Было | Стало |
|---|---|---|
| `Backend.CalculateResourceLimits` (потолки n_ctx) | своя формула: KV по всем блокам, head_dim из n_embd/n_heads, MemTotal | адаптер над `memfit.Ceilings` — один источник |
| `checkNCtxBeforeLoad` (гейт загрузки) | `CalculateResourceLimits` + `evaluateNCtxFeasibility`, fail-open при нулевых лимитах | `memfit.Evaluate` → 422 только при `does_not_fit`; форма ответа сохранена (`code=n_ctx_infeasible`, те же поля) |
| Подсказка в отказе | шаблон без учёта KV | из `Verdict`: потолок + причина `kv_quantization_would_make_it_fit`, когда q4_0 спасает запрос |
| `/api/models` | `max_vram_n_ctx=0` означал и «нет данных», и «веса не влезают» | добавлен явный `vram_known` (per-model и top-level), `n_ctx_degraded` учитывает его |
| 422 «выше обучающего контекста» | потолок 98 105 (завышенный KV) | 262 144 — реальный обучающий контекст модели |

**Живая проверка (RTX 3070, Qwen3.8-27B, образ `r83-gpu-local4`):**

```
POST load-with-params {"name":"qwen3.8:latest","contextSize":999999}
→ 422 hard_max_n_ctx=262144  suggestion="Укажите n_ctx <= 262144 (обучающий контекст модели)"

POST load-with-params {"name":"qwen3.8:latest","contextSize":131072}
→ 202 (раньше 422), лог: stage=degraded — физически помещается в partial_offload
```

**Заодно исправлена политика (важно).** В первой версии memfit применял к RAM
ОДНОВРЕМЕННО резерв 4 GiB и долю 0.8 — двойная скидка. На стенде это отказывало в
загрузке 27B (бюджет падал до 13.6 GiB при весах 15.7 GiB), хотя модель физически
грузится и работает. Те 0.8 выбирались по числам, посчитанным на ЗАВЫШЕННОМ KV
(до исправления KV-слоёв, ×3.38), поэтому запас стал избыточным. Теперь по
умолчанию одна скидка на ресурс: VRAM − 2 GiB, RAM − 4 GiB; `RAMUtil` остаётся
явным рычагом для тех, кому нужен дополнительный запас.

**Что осталось (следующий инкремент, сознательно не в этом):**

1. `checkVRAMForModel` → `memfit.Evaluate` (раскладка слоёв на GPU вместо
   бинарного поиска по `EstimateGPUMemoryForModel`). Меняет число слоёв и `useMmap`,
   поэтому делается с shadow-счётчиками (`cppworker_memfit_shadow_*`), которые для
   этого шага и оставлены.
2. `calculateLazyLoadOpts`: `fallback_no_fit` — сейчас метка в логе, а не отказ.
3. Удаление дублей после (1): `EstimateGPUMemoryForModel` (V3/V4),
   `EstimateModelVRAM` (V5), `cmd/cppworker.availableRAMBytes` → `memfit.ProbeRAM` (R7).
4. Балансер: читать `vram_known`/стадию вместо «0 = неизвестно»
   (`preflight_nctx.go:287,480`, `nctx_reload_handlers.go:96,424`).
5. Собственная оценка C-bridge (`VRAM-estimated max n_ctx`) — остаётся вторым
   мнением; после (1) её надо либо использовать как источник, либо убрать.

## 9. Шаг 3.1: раскладка слоёв тоже на memfit (сделано)

`checkVRAMForModel` больше не использует `CalculateOptimalGPULayers`: вердикт
`memfit.Evaluate` решает и «влезает ли модель», и сколько слоёв отдать на GPU, и
нужен ли mmap. Старая цепочка (`EstimateGPUMemoryForModel` с весами ×0.7, KV по
всем блокам, head_dim = n_embd/n_heads, сравнение RAM с MemTotal) в проде больше
не вызывается.

**Сохранено осознанно:**
- `stage == unknown` (нет метаданных) — НЕ отказ: опции остаются как запрошены.
  Это поймали существующие тесты (`backend_metrics_tokens_r66d_test.go`,
  `concurrent_generate_test.go`, `empty_stream_response_test.go`): жёсткий отказ
  по «неизвестно» ломал загрузку моделей без файла и stub-режим.
- «model > 70% свободной VRAM ⇒ mmap=true» — оставлено как страховка от OOM.

**Живая проверка (RTX 3070, Qwen3.8-27B, ctx=32768, gpuLayers=-1):**

```
auto-adapting GPU layers for model (memfit)
  requestedGPULayers=-1 optimalGPULayers=22 stage=partial_offload
  vramRequiredMB=5973 vramAvailableMB=6143
  ramRequiredMB=11675 ramAvailableMB=18154
[bridge] loading model: ... (ctx=32768, batch=512, threads=6, gpu_layers=22)
```

До переключения тот же запрос давал `gpu_layers=0` — модель грузилась целиком на
CPU. Теперь 22/65 слоёв на GPU при 5 973 из 6 143 MiB VRAM.

**Замечание на проверку:** в авто-режиме решение приняло KV-тип f16 (по числам:
total ≈ 17.7 GiB соответствует f16, а не q4_0). Гейт при этом логирует q4_0
(`EffectiveKVCacheType("")`). Надо выяснить, кто именно заполняет
`LoadModelOpts.KVCacheType` на пути загрузки, и свести к одному источнику — иначе
раскладка слоёв и потолки считаются на разных типах KV.

**Осталось:**
1. `calculateLazyLoadOpts` (lazyload_calc.go) — `fallback_no_fit` как метка вместо
   отказа; `availableRAMBytes` (vram_detect.go) → `memfit.ProbeRAM`; те же
   значения нужны в `adaptive_loader.go`, `auto_tune_nctx.go`, `inference.go`.
2. Удалить продово-мёртвое: `CalculateOptimalGPULayers`,
   `EstimateGPUMemoryForModel`, `backwardCompatEstimateGPUMemoryForModel`,
   `EstimateModelVRAM` (вместе с их тестами R52/R67a/gpu_distribution).
3. Балансер: читать `vram_known`/стадию вместо «0 = неизвестно»
   (`preflight_nctx.go:287,480`, `nctx_reload_handlers.go:96,424`).
4. Оценка C-bridge (`VRAM-estimated max n_ctx`) — использовать как источник или
   убрать.
