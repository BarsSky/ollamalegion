# R83 §9.4: протокол замены legacy-оценок памяти на memfit

**Дата:** 2026-09-26 · **База:** `r83-submodule-v13` · **Статус:** ВЫПОЛНЕНО
целиком (шаги 1, 1б, 2, 3, 3б, 4 — теги v15…v20); остаётся только осознанно
отложенное: консервативная `estimateKVCacheBytes` на путях без метаданных
(см. конец шага 2)

Документ отвечает на вопрос «как делать §9.4, не работая вслепую»: что измерено,
какие проверки запускать на каждом шаге, что считать регрессом и как откатиться.

---

## 1. Зачем вообще трогать (измеренный факт, а не догадка)

Тест `internal/cppbackend/estimator_compare_r83_test.go` прогоняет **оба** пути на
числах живого стенда. Результат на эталонном сценарии (RTX 3070 8 GB,
Qwen3.8-27B-UD-Q4_K_M, 16 464 440 224 байт, ctx=32768):

| сценарий | legacy `CalculateOptimalGPULayers` | memfit `Evaluate` |
|---|---|---|
| 3070 8 GB, `q8_0` (профиль Qwen3.8) | **18 слоёв** (mmap=true) | **23** (`partial_offload`) |
| 3070 8 GB, `f16` (дефолт конфига) | **0 слоёв** (CPU-only!) | **22** (`partial_offload`) |
| A10 24 GB, `q8_0` | 64 (все) | 64 (`exact_fit`) |
| 3070 8 GB, модель 2.5 GB, `q4_0` | 36 (все) | 36 (`exact_fit`) |

Legacy-диагностика при этом печатает буквально:

```
cannot load model: model requires ~19684 MB total memory (15.3 GB model + 3983 MB KV cache),
but VRAM=8191 MB + RAM=8192 MB insufficient. Best GPU layers=18 (CPU layers=46, needs 9891 MB RAM)
```

**Вывод, который меняет приоритет.** Legacy **занижает** число GPU-слоёв, а не
завышает: фактор весов ×0.7 уменьшает вес, но KV «256 Б/токен» (≈3.9 GB против
реальных 1.1 GB у Qwen3.8 с 16 KV-слоями и head_dim 256) перекрывает это
с запасом. Именно поэтому в проде и наблюдалось «gpu_layers=0 ⇒ модель целиком
на CPU» — это и есть исторический симптом из `plans/2026-09-25-memory-fit-subsystem.md`.

**Что увидит оператор после замены:** в auto-offload/reload-путях число слоёв
вырастет (18 → 22-24 на этом стенде), случаи «0 слоёв» почти исчезнут, загрузка
перестанет уходить в CPU-only. Это ожидаемое изменение поведения, его нужно
отразить в CHANGELOG и проверить на живом стенде (§4 ниже).

**Что НЕ меняется:** путь, который уже считает memfit
(`Backend.checkVRAMForModel` → `Evaluate`), и гейт n_ctx. Их трогать не нужно.

---

## 2. Что уже готово (можно опираться, не переделывая)

1. `internal/cppbackend/estimator_compare_r83_test.go` — эталон «до»: обе оценки
   на 4 сценариях + инварианты. После рефактора колонка legacy обязана совпасть
   с колонкой memfit.
2. `scripts/verify-r83-section9.ps1` — живая сверка инвариантов §9 (vram_known,
   ring buffer, отказ auto-load, preflight при известной VRAM, отсутствие
   SIGABRT). `-Strict` делает информационные проверки обязательными.
3. `scripts/verify-r83-v3-v4.ps1` — теги, гейт n_ctx, загрузка, restarts.
4. Все правки §9.1/9.2/9.3/9.5 (в т.ч. `availableRAMBytes` → `memfit.ProbeRAM`)
   уже раскатаны: рефактор §9.4 идёт уже поверх них.

---

## 3. Порядок работ и проверка на каждом шаге

Общее правило: **один шаг — один коммит — один релиз — одна живая сверка.**
Не объединять шаги: у каждого свой набор вызывающих, и регресс иначе не
локализуется.

### Шаг 1. `CalculateOptimalGPULayers` → вердикт memfit

**Вызывающие (продовые):** `cmd/cppworker/auto_offload.go:55`
(`calculateOptimalGPULayersForModel`) ← `handlers_model.go:1735` (reload) и
`handlers_config.go:582` (update-config). Объявление — `backend.go:1184`.

**Что СДЕЛАНО (v15):**

- `calculateOptimalGPULayersForModel` сначала спрашивает memfit
  (`memfitGPULayersForModel`: `backend.MemfitSpec` + `backend.MemfitBudget` +
  `memfit.Evaluate` → поле `GPULayers`); при `ok=false` уходит в
  `legacyCalculateOptimalGPULayersForModel` — прежняя формула, переименована и
  оставлена как fallback;
- `currentConfig == nil` → возвращаем `-1` (auto): судить не о чем, решение
  примет `checkVRAMForModel` через memfit. Без этой ветки тест ловил
  nil-pointer — то есть проверка нашла реальную дыру, а не формальность;
- в reload-пути (`handlers_model.go`) ноль от memfit теперь **применяется**:
  флаг `memfitDecided` отличает осознанный CPU-only («веса не влезают») от
  прежнего «legacy не смог»; без него условие `calculated > 0` пропускало ноль
  и оставляло старое (слишком большое) число слоёв. В лог добавлено
  `source=memfit|legacy`.

**РЕШЕНО И СДЕЛАНО (v19, 2026-09-26): вариант B+D** — владелец выбрал его из
предложения `plans/2026-09-26-step1b-fit-refusal-proposal.md`. Итог:

* **B** — отказ (HTTP 413 `insufficient_resources`, `code=6`) только на
  `StageDoesNotFit` и только когда в запросе нет явного `gpuLayers`. Числа в
  ответе берутся из того же вердикта, что и лог (`insErrFromVerdict`), поэтому
  `max_viable_n_ctx` и строка в логе не могут разойтись. Причина доведена до
  клиента машиночитаемо (`not_enough_ram` / `weights_exceed_vram` /
  `kv_too_large`) вместе с подсказкой.
* **D** — `StageCPUOnly` загрузку **не** блокирует: RAM хватает, модель
  отработает на CPU. Оператор получает запись в отдельном реестре
  `degradedLoads` (не в `load_failures` — загрузка удалась), поле
  `load_degraded` в `/api/models`, заголовки
  `X-CppWorker-Degraded[-Stage]` в ответе и warning-событие в WebUI
  (`PublishLoadDegradedTransition`, дедупликация с префиксом `degraded:`,
  чтобы не сталкиваться с провалом той же модели).
* **Escape-hatch** — `force: true` в теле или `?force=true` в query
  (`forceReloadRequest`), а также явный `gpuLayers`: явное намерение всегда
  побеждает автоматику.

**Важное наблюдение при реализации (стоит помнить).** В memfit-ветке гейт
`checkNCtxBeforeLoad` стоит **первым** и уже отдаёт 422 `n_ctx_infeasible`,
когда веса не помещаются никуда (`MaxHardCtx == 0`, потому что `Ceilings`
монотонны и при `ctx=0` веса по-прежнему не влезают). Поэтому отказ по
`does_not_fit` из 413-ветки на практике срабатывает как **страховка**: он нужен
там, где гейт и раскладка разошлись по входным данным (тип KV-cache из профиля
против явного, другой n_ctx), а также на путях без гейта. Сквозной тест
`TestReload_InfeasibleIsRefusedNotLoaded` принимает любой из двух 4xx и
проверяет главное: загрузка не началась, ответ содержит числа. Убирать 413-ветку
как «недостижимую» не нужно — это ровно тот случай, когда дублирующая проверка
дешевле пропущенного отказа.

**Проверка шага 1:** тесты `auto_offload_memfit_r83_test.go` (границы
fallback / disabled / no-config), `degraded_load_r83_test.go` (13 тестов
1б: реестр, вердикт→ответ, заголовки, сквозные reload-сценарии), полный набор
`cmd/cppworker` + `cppbackend` + `balancer` + `api` + `agent` — зелёный;
живая сверка — §4 (ожидание 22-24 слоя вместо 18).

**Что получилось проверить живьём (v15), а что нет — честно:**

* Проверено: **продовый путь раскладки** — `auto-adapting GPU layers for model
  (memfit) … optimalGPULayers:20, stage:partial_offload`, затем
  `[bridge] loading model … gpu_layers=20` и фактическая аллокация
  `llama_kv_cache: size = 1088.00 MiB (32768 cells, 16 layers, K/V (q8_0))` —
  сходится с memfit-оценкой `34 816 Б/токен × 32 768 = 1088 МиБ`. Это C-сторона
  (`checkVRAMForModel`), она работала и раньше, но теперь весь стек согласован.
* Проверено: **вызов новой функции** через `PUT /api/v1/cppworker/config/update`
  (`applied:["defaultCtxSize"], reload_started:["qwen3.8:latest"]`) — перезагрузка
  пошла, `[bridge] loading model … gpu_layers=20`.
* **НЕ удалось надёжно увидеть лог `reload: auto-offload recalculated gpu_layers
  … source=memfit`**: в `handleReloadModel` эта ветка требует одновременно
  `opts.GPULayers == -2` и отсутствия блокировки single-flight, а на стенде
  загрузки шли одна за другой (по 8-10 минут), поэтому вызовы попадали в ветку
  «уже грузится». Тот же код покрыт юнит-тестами
  (`auto_offload_memfit_r83_test.go`) и вызывается из
  `config/update`, где эффект виден (20 слоёв). Если нужна именно живая строка —
  запускать reload на **простаивающем** стенде (модель загружена, нет активных
  загрузок) и смотреть `docker logs` сразу после ответа 202.

**Побочная находка:** `PUT /api/v1/cppworker/config/update` с телом
`{"defaultGPULayers":20,"defaultCtxSize":32768}` переписал
`config/cppworker-defaults.json`, обнулив `defaultNuma` и `enableReasoning`
(файл был восстановлен через `git checkout`). Это поведение самого хендлера, к
§9.4 не относится, но при экспериментах с ним нужно проверять `git status`.

### Шаг 2. Удалить `EstimateGPUMemoryForModel` / `backwardCompat…` — СДЕЛАНО (v20)

**Что оказалось при проверке.** `EstimateGPUMemoryForModel` был не «остатком
продового пути», а **полностью мёртвым кодом**: единственные упоминания вне
самой функции — её же тест и комментарии. Legacy-фоллбэк `auto_offload` считал
KV своей функцией `estimateKVCacheBytes` (block_count + n_embd/n_heads), а не
этой. Продовых вызовов не было ни одного:

```
internal\cppbackend\model_manager.go:801,819   — определение
internal\cppbackend\memfit_adapter.go:109      — комментарий
internal\memfit\units.go:13                    — комментарий
tests\cppbackend_test.go:338-368               — тест
```

**Сделано (v20):**

- функция удалена вместе с `tests/cppbackend_test.go:TestEstimateGPUMemoryForModel`;
- `backwardCompatEstimateGPUMemoryForModel` был удалён ещё на шаге 1,
  `CalculateOptimalGPULayers` и `EstimateModelVRAM` — на шагах 1 и 3;
- **настоящий дефект шага 2 оказался в другом месте**: legacy-фоллбэк считал KV
  по `block_count` и `n_embd/n_heads`, то есть завышал его на гибридных моделях
  (Qwen3.8-27B: 64×213 против реальных 16 слоёв × 256 и 4 голов KV). Исправлено:
  `(*Backend).KVLayersForModel` + `estimateKVCacheBytesForLayers`;
- **константа «байт на элемент» теперь одна на стек** —
  `memfit.KVBytesPerElement` (f16 2/1, q8_0 34/32, q4_0 18/32). До этого
  cppworker держал свою таблицу и считал q8_0 ровно как 2 байта.

**Как проверено (без живого стенда, но на абсолютных числах):**

- `TestR83_Step2_KVMatchesLlamaCpp` — 1088 MiB при ctx=32768, 16 слоях KV,
  head_dim=256 и q8_0 (это ровно то, что печатает llama.cpp на живом стенде);
- `TestR83_Step2_KVMatchesMemfit` — совпадение с `memfit.KVBytesPerToken` для
  f16/q8_0/q4_0 (иначе снова два источника истины);
- `TestR83_Step2_OldFormulaWasOverestimating` — прежняя формула действительно
  завышает (защита от возврата дефекта);
- `TestR83_Step2_RealMetadataWins` — head_dim берётся из `attention.key_length`
  (256), а не из `n_embd/n_heads` (213);
- `TestR83_Step2_IncompleteMetadataIsConservative` — без метаданных
  `KVLayersForModel` возвращает ok=false, а KV-функция — 0 («не знаю»), а не
  выдуманное число.

**Осталось по шагу 2 (осознанно, не долг):** `estimateKVCacheBytes` живёт как
консервативная оценка сверху для путей, где известны только параметры модели
(`auto_tune_nctx`, `inference.go`, `lazyLoad`). Переводить их на реальные
метаданные — отдельная задача: там `GGUFModelMeta` под рукой не всегда, а
ошибка в эту сторону (завышение) безопаснее занижения. Функция помечена
предупреждением в doc-комментарии.

### Шаг 3. `EstimateModelVRAM` → `WeightsOnGPUBytes` — ДЕЛАЕТСЯ СЕЙЧАС (v16)

**Что выяснилось при проверке:** у `cppbackend.EstimateModelVRAM` продовых
вызовов НЕТ вообще — все `estimateModelVRAM(...)` в
`backend_selector.go:566`, `model_instance_controller.go:178`,
`prewarm_controller.go:239` — это **своя** функция балансера в
`scoring.go:284` (эвристика по имени модели). Тест
`tests/gpu_distribution_test.go:TestEstimateModelVRAM` — единственный потребитель.

**Сделано:**
- добавлен `cppbackend.WeightsOnGPUBytes(sizeBytes, gpuLayers, totalLayers)` —
  ровно та пропорция, что в `memfit.computeSplit`
  (`ModelSpec.SizeBytes.Scale(g, n_layers)`); 0 = CPU-only, -1 = все слои;
  тест `tests/gpu_distribution_test.go:TestWeightsOnGPUBytes` (монотонность,
  границы, защита от мусора). **Тест сразу поймал ошибку автора:** первая версия
  трактовала `gpuLayers <= 0` как «все слои», то есть `gpuLayers=0` давал весь
  файл вместо нуля;
- `EstimateGPUMemoryForModel` больше не считает «70% модели»: пропорцию весов
  отдаёт `WeightsOnGPUBytes`. **Замер после правки:** legacy на живом стенде
  стал 12 слоёв вместо 18 — то есть ещё консервативнее (его KV «256 КБ/токен»
  больше ничем не компенсируется). Это безопасное направление: legacy остаётся
  путём «memfit не может судить»;
- `tests/cppbackend_test.go:TestEstimateGPUMemoryForModel` — обновлён под новую
  арифметику (было «≈5.5 GB» из-за ×0.7, стало 6.0 GB = 5 GiB весов + 1 GB
  compute; KV при 4096 даёт ≈1 MB).

**Что осталось (шаг 3б — удаление):** удалить `cppbackend.EstimateModelVRAM`
(нет продовых вызовов) и `backwardCompatEstimateGPUMemoryForModel` (нет вызовов
вообще). `Backend.CalculateOptimalGPULayers` — следующий кандидат, но сначала
надо переписать `backend_r52_test.go` и `kv_cache_type_r67a_test.go` на memfit
(сейчас они — единственные его вызывающие).

**Находка для отдельной задачи:** в legacy-KV «256 КБ на токен» заложено
завышение в 1024×: реальный `ctxMemoryMB` = `ctxSize × 256 / 1024 / 1024`, то
есть при ctx=32768 это 8 MB, а не 8 GB, как написано в комментарии
(`model_manager.go:842-848`). Оценка остаётся консервативной (перестраховка по
весам), но комментарий врёт — при следующем касании этого места KV нужно
считать через `KVLayersFor`/`KVHeadDimFor`, как в C-bridge и memfit.

### Шаг 4. Уборка `vram_detect.go` — ДЕЛАЕТСЯ СЕЙЧАС (v17)

`availableRAMBytes` уже делегирует в `memfit.ProbeRAM` (§9.4a).

**Найденный дефект (шаг 4):** `availableVRAMBytes()` возвращает **полную ёмкость
карты** (bridge отдаёт `VRAMTotalMB`), а не свободную VRAM — и именно её
спрашивал legacy-фоллбэк раскладки слоёв. На живом стенде (3070 8 GB) это
8191 MB против ~1033 MB фактически свободных: оценка была оптимистична в 8 раз.
`freeVRAMBytes()` рядом читает `VRAMFreeMB` — то же, что `memfit.Budget`.

**Сделано:**
- `legacyCalculateOptimalGPULayersForModel` теперь берёт `freeVRAMBytes()`
  (fallback `tryNvidiaSMIFree()`, и только если free неизвестен — полная
  ёмкость **с предупреждением** в лог, что оценка оптимистична);
- удалён дубль `nvidiaSmiVRAMBytes` из `auto_offload.go` (то же, что
  `tryNvidiaSMIFree`, но с `memory.free` в имени функции под словом «VRAM»);
- `availableVRAMBytes` больше НЕ используются в расчётах — только для
  диагностики/логов (`handlers_model.go`, `lazyload*.go`, `auto_tune_nctx.go`);
  в комментарии к функции это записано явно;
- тесты `vram_detect_free_r83_test.go`: free и total — разные величины, каждая
  читает свою переменную окружения; на стенде с загруженной моделью
  free < total.

**Что осталось (шаг 4б):** сводить `availableVRAMBytes` с `MemfitBudget` не
нужно — она честно отвечает на вопрос «сколько всего», а расчёты на неё больше
не опираются. Если понадобится, оставшиеся вызовы (7) — только логи.

---

## 4. Приёмочные критерии (что считать успехом)

1. `estimator_compare_r83_test.go` зелёный, и колонки legacy/memfit совпадают
   (после того как legacy-вызов в тесте переключён на новую обёртку).
2. На живом стенде reload с `gpuLayers=-2` для Qwen3.8 даёт **≥22** слоя
   (вместо 18) и **не** даёт 0 слоёв на `f16`.
3. `scripts/verify-r83-section9.ps1 -Strict` — без FAIL.
4. `scripts/verify-r83-v3-v4.ps1` — все проверки пройдены, `restarts=0`,
   нет `SIGABRT` / «VRAM insufficient».
5. В логе загрузки нет новых `cannot load model: insufficient` там, где модель
   фактически грузится (проверять по `/api/models` → `state=loaded`).

---

## 5. Как откатывать

* Каждый шаг — отдельный коммит и **отдельный тег**; откат = вернуть тег в
  `deployments/.env` + `docker compose up -d` (образы остаются в демоне по id,
  см. `release-manifest.json`).
* Если регресс виден по слоям, но модель грузится — быстрый рычаг без отката:
  `CPPWORKER_AUTO_OFFLOAD=false` (вернёт `DefaultGPULayers`), тогда
  `calculateOptimalGPULayersForModel` не вызывается вовсе.
* Если регресс в RAM-пути — `CPPWORKER_AVAILABLE_RAM_BYTES` фиксирует значение
  явно (этот рычаг читают и `availableRAMBytes`, и `memfit.ProbeRAM`).

---

## 6. Что осталось за рамками этого протокола

- **§9.6 D-A/D-B/D-C** (асинхронность, реестр, unload-барьер) — другие
  инварианты, к оценкам памяти не относятся. D-C проще всего проверить
  сценарием: unload во время загрузки → `/api/models` не должен показывать
  `state=loading` на удалённую модель.
- **§9.7** — решение владельца контракта (дублирование текста в `done`-чанке).
- **§9.8** — не воспроизводится; инструкция по поиску уже в §9 хендоффа.
- **`writeToolCallsStream` / `writeStaticTextStream`** (`handlers_openai.go:736,820`)
  пишут в `w` напрямую и без проверки обрыва. Гонки там сейчас нет (вызываются
  из однопоточного non-stream пути, `handlers_openai.go:548,552`, без heartbeat),
  поэтому трогать их без симптома не нужно — но при появлении параллельных
  записей в этих путях это первый кандидат (ср. §0.5.45: там именно гонка
  heartbeat × токены ломала NDJSON).
