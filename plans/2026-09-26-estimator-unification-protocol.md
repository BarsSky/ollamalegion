# R83 §9.4: протокол замены legacy-оценок памяти на memfit

**Дата:** 2026-09-26 · **База:** `r83-submodule-v13` · **Статус:** не начато
(подготовка выполнена — см. «Что уже готово»)

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

**Что осталось на шаг 1b (решение по отказу):** если `!v.Fits()`, вернуть отказ
`insufficient_resources`, а не `DefaultGPULayers` — согласованно с §9.3
(`insufficientResourcesFromFallbackNoFit`). Сейчас в этом случае возвращается
`v.GPULayers` (обычно 0 = CPU-only), и загрузка идёт через partial offload.
Проверка: сценарий из `lazyload_nofit_refusal_r83_test.go`, но через reload.

**Проверка шага 1:** тесты `auto_offload_memfit_r83_test.go` (границы
fallback / disabled / no-config), полный набор `cmd/cppworker` + `cppbackend` +
`balancer` + `api` — зелёный; живая сверка — §4 (ожидание 22-24 слоя вместо 18).

### Шаг 2. Удалить `EstimateGPUMemoryForModel` / `backwardCompat…`

**Порядок:** сначала шаг 1 (он перестаёт вызывать), затем проверить остаток:

```
Select-String -Path internal\cppbackend\*.go,cmd\cppworker\*.go `
  -Pattern 'EstimateGPUMemoryForModel|backwardCompatEstimateGPUMemoryForModel'
```

Продовых вызовов быть не должно (в `backend.go` они были только внутри
`CalculateOptimalGPULayers`; в `gpu_distribution.go:406-411` — внутри
`EstimateModelVRAM`, см. шаг 3).

**Тесты:** `tests/cppbackend_test.go:TestEstimateGPUMemoryForModel` — удалить
вместе с функцией; `backend_r52_test.go`, `kv_cache_type_r67a_test.go` — они
тестируют `CalculateOptimalGPULayers`; после шага 1 либо переписать на memfit,
либо удалить (их покрытие теперь даёт `estimator_compare_r83_test.go`).

### Шаг 3. `EstimateModelVRAM`

**Особенность:** его потребители (`backend_selector.go:566`,
`model_instance_controller.go:178`, `prewarm_controller.go:239`,
`scoring.go:283-284`) сравнивают **числа**, а не берут вердикт: им нужна оценка
«сколько VRAM займут веса при N слоях». Вердикт memfit (`v.GPUWeights`) даёт
ровно это — но только для одного ctx/раскладки за вызов.

**Что делать:** добавить в `memfit_adapter.go` маленький хелпер
`WeightsOnGPUBytes(sizeBytes, nLayers, gpuLayers)` (чистая пропорция, как
`ModelSpec.Weights.Scale(g, n_layers)` в `computeSplit`) и заменить
`EstimateModelVRAM` на него. Это единственное место, где допустима формула мимо
`Evaluate`: она считает не решение, а часть решения, и та же формула уже живёт в
`memfit.computeSplit`.

**Проверка:** `tests/gpu_distribution_test.go:TestEstimateModelVRAM` — переписать
на новый хелпер; сравнить значения на тех же входных данных (golden-таблица).

### Шаг 4. Уборка `vram_detect.go`

`availableRAMBytes` уже делегирует в `memfit.ProbeRAM` (§9.4a). Остаётся решить,
что делать с `availableVRAMBytes`/`freeVRAMBytes` и `nvidiaSmiVRAMBytes`:
кандидат — вынести чтение VRAM в `memfit` по аналогии с `ProbeRAM`, тогда
`vram_detect.go` схлопывается до GPU-discovery через bridge.

**Проверка:** `cmd/cppworker` тесты (`ram_fallback_env_test.go`,
`auto_tune_nctx_test.go`), плюс живой `-Strict` прогон §9 (он смотрит
`vram_known`).

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
