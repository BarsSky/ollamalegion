# Передача в следующую сессию: R83, память и надёжность загрузки

**Дата передачи:** 2026-09-25
**Состояние:** всё в рабочем дереве, **не закоммичено** (последний коммит `30f28ed`).
**Развёрнутый стек:** тег `r83-submodule-v2` (4 контейнера healthy), модели не загружены, VRAM свободна.

Документ самодостаточен: диагностику повторять не нужно, все замеры и локации ниже.

---

## 1. Что произошло (кратко, с фактами)

Исходная жалоба: `Qwen3.8-27B-UD-Q4_K_M` не грузится с окном 65536/128000 (на A10 24 GB), при 32768 работает; OpenWebUI дублировал ответ; позже контейнер cppworker **упал**.

Диагностика на живом железе (RTX 3070 8 GB, тот же файл модели, 16 464 440 224 байт):

| Что | Факт |
|---|---|
| KV-кэш считался неверно | llama.cpp отдаёт KV **16 слоям** из 65 (рекуррентные SSM-слои KV не хранят, MTP-слой в кэш не входит), `head_dim` KV = **256** (`attention.key_length`), а не 213 (`n_embd/n_heads`). Наивная формула завышала KV в **3.38×** (221 520 против 65 536 Б/токен при f16; 61 344 против 18 432 при q4_0). Подтверждено прямо: `llama_kv_cache: size = 144.00 MiB (8192 cells, 16 layers)` = 2×16×4×256×18/32×8192 |
| Решение об оффлоаде | из-за завышенного KV давало `gpu_layers=0` → модель грузилась целиком на CPU. После правки: **22–24 слоя на GPU** (замерено на живом стеке) |
| Гейт n_ctx | был fail-open для холодной модели (лимиты приезжали нулями из-за точного lookup имени с тегом) → 131072 проходил как 202 без причины; и потолок считался по `MemTotal` без весов |
| Падение контейнера | **не OOM и не Go-паника**: `malloc(): unaligned tcache chunk detected` → glibc `abort()` → **SIGABRT** → рестарт (`OOMKilled=false`). Триггер: перекрывающиеся загрузки (два плана: `gpu_layers=33→29` и `-2→18`) при `free=0 MB` VRAM |
| Симптом «таймаут вместо ожидания» | балансер ждал 180 с (`LB_AUTO_LOAD_WAIT_SEC`, default) и отдавал «auto-load failed», пока cppworker продолжал грузить; повторный запрос запускал вторую загрузку |

---

## 2. Что уже сделано (в дереве, не закоммичено)

### 2.1. Единая подсистема решений о памяти `internal/memfit`

Новый пакет без cgo: типизированные байты, **одна** формула KV, `Budget` с `Known`-флагами, один `ProbeRAM` (env → `/proc/meminfo` → cgroup v2/v1), явная `Policy`, **один предикат** `computeSplit` (потолки считаются по нему же двоичным поиском), `Verdict` с машинными `ReasonCode`.

Тесты (17 функций, без тегов, 0.4 с): golden на реальных числах 3070 и A10, регрессы на конкретные дефекты, инварианты (монотонность по n_ctx, согласованность сплита, «потолок ⇔ вердикт», порядок KV, «из отсутствия данных не следует поместится»).

**Политика: одна скидка на ресурс** — VRAM − 2 GiB (`CPPWORKER_VRAM_OVERHEAD_MB`), RAM − 4 GiB. `RAMUtil` по умолчанию 1.0: сочетание 0.8 и резерва давало двойную скидку и отказывало в загрузке 27B там, где она физически грузится.

### 2.2. Правки в cppbackend / cppworker

- `kv_layers.go` — единственный источник числа KV-слоёв и `head_dim` KV (`KVLayersFor`/`KVHeadDimFor`), со ссылками на исходники llama.cpp и защитой от угадывания (`exact=false` для gemma4/gemma3e и явного списка рекуррентных слоёв).
- Парсер GGUF читает `attention.key_length`, `attention.value_length`, `nextn_predict_layers`, `full_attention_interval`, фиксирует наличие `attention.recurrent_layers`.
- `CalculateResourceLimits` — теперь адаптер над `memfit.Ceilings` (+ новое поле `VRAMKnown`).
- `checkVRAMForModel` — решение (влезает/сколько слоёв/mmap) принимает `memfit.Evaluate`; `stage == unknown` (нет метаданных) — **не отказ** (иначе ломались stub-тесты и модели без файла).
- `checkNCtxBeforeLoad` — решение по `memfit.Evaluate`, форма 422 сохранена (`code=n_ctx_infeasible`, те же поля), подсказка из вердикта (с причиной «поможет q4_0»).
- **Single-flight загрузок**: `Backend.loadSingleFlight` в `LoadModelWithOpts` — вторая загрузка ждёт первую (тест `load_singleflight_r83_test.go`, без мьютекса падает).
- `/api/models`: `vram_known` (per-model и top-level), `n_ctx_degraded` учитывает «VRAM известна».
- Удалены: shadow-сравнение решений, счётчики `cppworker_memfit_shadow_*`, `memfit_shadow.go`.

### 2.3. Правки в балансере

- `autoload_wait.go`: ожидание загрузки — **датчик бездействия**. Пока `elapsedMs` растёт, дедлайн продлевается; общий предел `autoLoadHardCeiling = 30 min`. Без прогресса таймаут как раньше (тесты: новый `autoload_wait_progress_r83_test.go` + прежний `TestWaitForModelLoad_Timeout_R67a`).
- `autotune.go`: перед `executeAutoTuneReload` — проверка `p.nctxReload.IsReloadPending(...)` (тот же реестр, что в `llamacpp_backend_helpers.go:631`, `nctx_reload_handlers.go:627`).
- `model_name_match.go`: срезание ollama-тега (иначе размер модели для таймаут-эвристики не находился → idle 120 s вместо 1800 s).

### 2.4. Инфраструктура

- `scripts/release-all.ps1`: в `.env` пишется тег **без** префикса `gpu-` (compose добавляет сам) — исправлен `gpu-gpu-<tag>`.
- `deployments/.env`: `CPPWORKER_GPU_TAG=r83-submodule-v2` и остальные три тега.

### 2.5. Живые проверки, которые уже пройдены

```
гейт:      contextSize=999999 → 422, hard_max_n_ctx=262144 (обучающий контекст)
           contextSize=131072 → 202, stage=degraded (раньше 422 из-за завышенного KV)
раскладка: gpuLayers=-1 → optimalGPULayers=22..24, stage=partial_offload,
           [bridge] loading model ... gpu_layers=24   (было 0)
надёжность: два перекрывающихся запроса → restarts=0, SIGABRT=0,
           "VRAM insufficient (free=0 MB)"=0, [bridge] loading model=1,
           лог: "model is already being loaded by another request; waiting"
```

---

## 3. Что осталось (в порядке приоритета)

### 3.1. Выпустить `r83-submodule-v3` (готовые правки не на стенде)

Раскатанный `r83-submodule-v2` **не содержит** guard автотюна и top-level `vram_known`.

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\release-all.ps1 -Tag r83-submodule-v3
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\check-image-tags.ps1
```

### 3.2. C-bridge: клампить `gpu_layers`, а не только предупреждать

**Где:** `c/bridge/bridge.c`, функция оценки `max_vram_n_ctx` — блок, печатающий
`[bridge] WARNING: VRAM insufficient for KV-cache with current gpu_layers=%d (free=%lld MB, gpu_model=%llu MB)`.
**Что:** при `estimated_max == 0` (свободной VRAM нет) уменьшить запрошенные `gpu_layers` до помещающихся либо вернуть ошибку — не отдавать план в llama.cpp.
**Критерий:** запрос заведомо непомещающегося плана не приводит к аллокации в переподписке.
**Примечание:** после single-flight + memfit-клампа путь больше не воспроизводится на живом стеке, это страховка. Требует пересборки cppworker (llama.cpp-слой берётся из кэша, ~1–2 мин).

### 3.3. `LB_AUTO_LOAD_WAIT_SEC` — тир по размеру модели

**Где:** `internal/balancer/autoload_wait.go`, `AutoLoadWaitTimeout()` (сейчас default 180 с, модель размера не знает).
**Что:** резерв на случай, если прогресс недоступен: масштабировать бюджет по размеру GGUF (как сделано для idle/first-byte тиров) либо сделать per-request с учётом модели.

### 3.4. Свести KV-тип между гейтом и раскладкой

**Симптом:** гейт логирует `kv_cache_type=q4_0` (`EffectiveKVCacheType("")`), а раскладка в том же процессе посчитала по **f16** (по числам: total ≈17.7 GiB для ctx=32768 соответствует f16).
**Что:** найти, кто заполняет `LoadModelOpts.KVCacheType` на пути загрузки (`cmd/cppworker/handlers_model.go`, `lazyload.go`, `internal/cppbackend/backend.go`), и свести к одному источнику.

### 3.5. `fallback_no_fit` — сделать отказом

**Где:** `cmd/cppworker/lazyload_calc.go` (ветка «model doesn't fit even in CPU-only mode»): сейчас ставит `rationale.Source = "fallback_no_fit"` и всё равно пробует.
**Что:** вернуть отказ с `insufficient_resources` + suggestion либо, минимум, warning-событие в WebUI (данные для него есть).

### 3.6. Удалить продово-мёртвые оценки (после 3.2)

`EstimateGPUMemoryForModel` (веса ×0.7, KV как 256 Б/токен), `backwardCompatEstimateGPUMemoryForModel`, `EstimateModelVRAM` (нет продовых вызовов), `CalculateOptimalGPULayers` (используется теперь только тестами), `cmd/cppworker.availableRAMBytes` (в `vram_detect.go`; заменить на `memfit.ProbeRAM`; те же значения нужны в `adaptive_loader.go:116,202`, `auto_tune_nctx.go:162,210`, `inference.go:768,878,917`).
Вместе с ними удалить/переписать их тесты: `backend_r52_test.go`, `kv_cache_type_r67a_test.go`, `tests/cppbackend_test.go:TestEstimateGPUMemoryForModel`, `tests/gpu_distribution_test.go:TestEstimateModelVRAM`.

### 3.7. Балансер: читать `vram_known` вместо «0 = неизвестно»

**Где:** `internal/balancer/preflight_nctx.go:287,480`, `nctx_reload_handlers.go:96,424` — сейчас `MaxVRAMNCtx <= 0` трактуется как «нет данных». После появления `vram_known` в `/api/models` различать два случая (нет данных / веса не влезают).
Плюс домен `/api/models` в `pkg/types` — добавить поле `vram_known`.

### 3.8. Устаревшие профили моделей

В логе живого cppworker видны попытки грузить несуществующие `r81-model.gguf`, `m-auto.gguf`, `llama-3-8b.gguf`. В конфигах репозитория (`config/*.json`, `deployments/*.json`) этих имён **нет** → источник вне репозитория (состояние балансера/WebUI, сохранённые профили). Найти и почистить.

### 3.9. Прочие открытые дефекты (из более раннего разбора, §11 плана)

- **D-A**: `load-with-params` рекламирует асинхронность (202 + `progressUrl`), но фактически блокирует запрос до конца загрузки (в логе HTTP: `status:202 duration:9m6.6s`).
- **D-B**: расхождение реестра после перекрывающихся load/unload (`model loaded` в логе, `/api/models` → `count=0`).
- **D-C**: `unload` не барьер (200 за 824 µs при идущей загрузке 16 GB).
- **Механизм A дублирования в OpenWebUI**: `cmd/cppworker/handlers_chat.go:1397-1407` кладёт полный текст в `done`-чанк (замерено 2.00× на `/api/chat`); решение по контракту — за владельцем.
- **SSE ring-buffer**: `internal/api/handlers_events.go:197` — события без подключённого клиента теряются из snapshot и `/api/v1/health`.

---

## 4. Команды

```powershell
# сборка и тесты (тег llama_stub обязателен для всего, что тянет c/bridge)
go build -tags llama_stub ./cmd/...
go test -tags llama_stub -p 1 ./internal/memfit/ ./internal/cppbackend/ ./cmd/cppworker/ ./internal/balancer/ ./internal/api/
go test ./internal/memfit/            # пакет без cgo, 0.4 с — годится в CI

# выпуск и сверка тегов
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\release-all.ps1 -Tag r83-submodule-v3
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\check-image-tags.ps1

# живая проверка (токен из deployments/.env: CPPWORKER_API_TOKEN)
#   гейт: выше обучающего контекста → 422
curl.exe -s -o resp.json -w '%{http_code}' -X POST 'http://127.0.0.1:18092/api/models/load-with-params' `
  -H 'Content-Type: application/json' -H 'Authorization: Bearer changeme-bundled-with-agent-token' `
  --data-binary "@body.json"      # body.json: {"name":"qwen3.8:latest","contextSize":999999}
#   раскладка/загрузка: ожидать "auto-adapting GPU layers for model (memfit)" и gpu_layers=22..24
docker logs ol-bundled-cppworker-gpu 2>&1 | Select-String 'memfit|\[bridge\] loading model'
#   репро инцидента: два перекрывающихся запроса, затем проверить restarts/SIGABRT
docker inspect ol-bundled-cppworker-gpu --format '{{.RestartCount}}'
docker logs ol-bundled-cppworker-gpu 2>&1 | Select-String 'SIGABRT|VRAM insufficient'
#   выгрузить модель и/или прервать тестовую загрузку
docker restart ol-bundled-cppworker-gpu
```

---

## 5. Грабли окружения (проверено на этой машине)

- **Только Windows PowerShell 5.1** (`pwsh` нет). `"$var:"` в двойных кавычках — ошибка парсинга, писать `${var}:`.
- **PowerShell пишет файлы с CRLF** → `gofmt` видит весь файл изменённым. После правок через PowerShell нормализовать: `[System.IO.File]::ReadAllText($f).Replace("`r`n","`n")` и `WriteAllText`.
- **Не запускать `gofmt -w` на файлах с предсуществующим форматированием** (`cmd/cppworker/handlers_metrics.go` и др.) — иначе в дифф попадает весь файл. Проверять: `gofmt -l` на версии из HEAD.
- **JSON в curl**: `--data-binary "@file"` с UTF-8 без BOM (`New-Object System.Text.UTF8Encoding($false)`); `-d $body` ломает кавычки.
- **Фоновые job'ы**: `| Select-Object -Last N` буферизует весь вывод — прогресс не видно до конца; проверять результат по факту (образ/контейнер).
- `go build ./...` **без** тега падает на `c/bridge` (`llama.h: No such file`) — это нормально, нужен `-tags llama_stub`.
- **Docker Desktop/WSL2**: `nvidia-smi` на хосте не видит процессы GPU; контейнеры не видят память друг друга (NVML слепнет к соседям) → два cppworker на одной GPU переподписывают VRAM. RAM контейнера ≈24.5 GB (лимит WSL), не 50 GB хоста.
- **Модели на bind-mount Windows**: чтение 16 GB идёт минутами, упор в I/O (CPU 0.1%). Отсюда `estimatedLoadTimeMs` ≈175 с не соответствует фактическим минутам.
- Живой стек: balancer `:18080/:18081`, cppworker `:18092`, webui `:18083`, agent без порта; сеть `ollama-legion-bundled-net`; токен `changeme-bundled-with-agent-token`.

---

## 6. Готовые стартовые промпты для новой сессии

- «Прочитай `plans/2026-09-25-handoff-r83-next-session.md` и выполни §3.1 (выпуск `r83-submodule-v3`) с проверкой `check-image-tags.ps1`».
- «Возьми §3.2: в `c/bridge/bridge.c` при `estimated_max == 0` клампи `gpu_layers` вместо предупреждения, пересобери cppworker и проверь на живом стенде».
- «Возьми §3.4: сведи KV-тип между гейтом и раскладкой слоёв — найди, кто заполняет `LoadModelOpts.KVCacheType`».
- «Возьми §3.6: удали продово-мёртвые оценки вместе с их тестами, прогони полный набор».

---

## 7. Ключевые файлы документации

- `plans/2026-09-25-memory-fit-subsystem.md` — подсистема памяти: принципы, инварианты, миграция, §8 шаг 3, §9 шаг 3.1.
- `plans/2026-09-25-qwen38-load-context-dup-and-infra.md` — исходный план R83, §11 живая проверка на GPU, §11.6 учёт RAM.
- `CHANGELOG.md` — `0.5.42` и ниже: KV-слои, учёт RAM, шаги 3 / 3.1, инцидент SIGABRT, ожидание по прогрессу, тест надёжности.
