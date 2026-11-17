# Справка-рекомендации и шаблоны параметров: инвентаризация и проект контракта

**Статус документа.** Только инвентаризация существующих механизмов и проект
контракта новой страницы WebUI. Кода не менялось, ничего не коммитилось.
Каждое утверждение ниже снабжено ссылкой `файл:строка`. Где проверка не
выполнялась — написано «**не проверено**».

> **Обновление 2026-11-17 (реализация).** Контракт реализован на сервере.
> §6 переписан: вместо вопросов там РЕШЕНИЯ человека, которые уточняют контракт
> (пять пунктов). §9 — список того, что реализовано (файлы и ручки), и явный
> перечень мест, где реализация отошла от первоначального текста документа.
> Отчёт о реализации: `docs/plans/2026-11-17-reference-api-implementation.md`.

**Задача, под которую написан документ.** Оператор просит страницу WebUI с двумя
сущностями:

1. **записи-рекомендации** — значения параметров загрузки с комментарием и
   обоснованием, РЕДАКТИРУЕМЫЕ из UI;
2. **шаблоны** — именованные наборы параметров, которые можно ЯВНО применить на
   тот бэкенд, где загружена конкретная модель.

Сейчас это живёт в markdown-доках (`docs/operations/model-tuning-reference.ru.md`)
и в разрозненных JS-пресетах, не связанных с профилями моделей.

**Проверялось на живом стенде** (балансер на `127.0.0.1:18081`, токен взят из
`deployments/.env`, в отчёт не копируется; имя переменной — `CPPWORKER_API_TOKEN`).
Стенд отвечал, `GET /api/v1/auth/status` → `{"enabled":true,"headerName":"X-API-Token","tokenCount":1}`.

---

## 1. Что уже есть

### 1.1 Сводная таблица

| Механизм | Файл:строки | Что хранит | Кто применяет | Чего не хватает |
|---|---|---|---|---|
| Профили моделей cppworker (CRUD+apply) | `internal/api/routes.go:502-503`, `internal/api/handlers_cppworker_profiles.go:9-13,110-786` | `types.LlamaCppModelProfile` в памяти + `config.json` (`configSaver`) + fallback `data/profiles.json` | сам балансер: `POST …/apply` → reload на llama.cpp-бэкендах | нет комментария/обоснования к набору; нет версий; нет явного «шаблона»; нет dry-run-эндпоинта |
| Тип профиля (набор полей) | `pkg/types/balancing.go:283-469` | 20+ полей загрузки | — | нет «единиц измерения» и допустимых границ в самом типе (границы — в валидаторе) |
| Валидация профиля | `internal/api/handlers_cppworker_profiles.go:1211-1266` | границы значений | вызывается в PUT и apply | нет публичного эндпоинта «проверь набор, не сохраняя» |
| Проверка памяти (422) | `internal/api/handlers_cppworker_profiles.go:355-382`, `internal/api/profile_memory_check_r116.go:31-118` | — (спрашивает cppworker `GET /api/models/effective`) | вызывается внутри PUT/apply | не вынесена как отдельная dry-run-ручка балансера |
| Персист профилей на диск | `internal/balancer/profiles_persistence.go:36-58,67-153,162-205` | `{version, profiles{}, defaultProfile}` | `LoadProfilesFromFile` при старте, `SaveProfilesToFile` при PUT/DELETE/apply | путь жёстко «рядом со state.json», нет env-override |
| Быстрые пресеты WebUI (Speed/Memory/Context/CPU) | `webui/js/app.js:610-645`, `webui/index.html:1788-1797` | 4 набора чисел ПРЯМО В JS | заполняют поля формы `#section-llama-cpp`; сохраняются как ГЛОБАЛЬНЫЙ override бэкенда | это не профиль модели и не шаблон: ни id, ни комментария, ни применения к загруженной модели |
| Пресеты железа мастера настройки | `webui/js/modules/setup-wizard-presets.js:19-97,160-194` | лимиты балансера + `cppworkerHint` (env-подсказки) | частично — поля мастера; cppworker-часть только ПОДСКАЗКА | hint не применяется автоматически; источник истины — `config/hardware-presets/*.json` (4 файла) |
| Пресеты раскладки | `webui/js/modules/setup-wizard-presets.js:121-158` | стратегии placement | мастер настройки | к параметрам загрузки отношения не имеет |
| Профили в странице GGUF (`Настройки` бэкенда) | `webui/js/modules/gguf-renderer-detail-render.js:3857-3870`, `:2118,2145,2156`; `webui/js/modules/cppworker-params.js:78-94` | ничего своего — рисует серверные профили | `window.CppWorkerParams` → `PROFILES.upsert` / `PROFILES.apply` | только список профилей; нет справочных записей и шаблонов |
| Реестр полей параметров в UI | `webui/js/modules/gguf-renderer-detail-render.js:2490-2524` | `field`/`loadKey`/`type`/`min`/`max`/`options` для 11 параметров | рендер двух таблиц карточки модели | это ЛУЧШИЙ кандидат в единственный источник метаданных полей для новой страницы |
| Документ-справка | `docs/operations/model-tuning-reference.ru.md:1-184` | таблица «поле → что делает → как выбирать» (:28-45), env-таблица (:49-61), 6 готовых профилей с обоснованием (:64-160) | человек читает | нет машиночитаемого эквивалента, нет API, нет UI-редактора |
| Документ «числа контекста» | `docs/architecture/context-numbers.ru.md:1-391` | разбор 9 видов «контекста», кто что отдаёт | человек читает | то же |
| Troubleshooting | `docs/operations/troubleshooting.ru.md:1-1540` | рецепты по симптомам, много curl-примеров | человек читает | то же |
| Секция Help в WebUI | `webui/index.html:2380-2425` | 5 ссылок на доки/схему/GitHub | — | **редактируемых полей нет вообще** |
| Серверное хранение заметок/шаблонов | — | **не найдено** (см. §1.6) | — | отсутствует как класс |
| Реестр «горячих» настроек | `internal/settings/settings.go:302-372`, `internal/api/handlers_settings_r278.go:33-196` | `settings-overrides.json` рядом со state.json | применяется на лету | по конструкции — только `int64`-ключи `LB_*`; для записей-структур не годится |

### 1.2 Профили моделей cppworker — детально

**Ручки и обработчики.**

| Метод и путь | Обработчик | Строки |
|---|---|---|
| `GET /api/v1/cppworker/model-profiles` | `handleListModelProfiles` | `handlers_cppworker_profiles.go:113-137` |
| `GET /api/v1/cppworker/model-profiles/{name}` | `getModelProfile` | `:221-245` |
| `PUT /api/v1/cppworker/model-profiles/{name}` | `upsertModelProfile` | `:251-433` |
| `DELETE /api/v1/cppworker/model-profiles/{name}` | `deleteModelProfile` | `:459-509` |
| `POST /api/v1/cppworker/model-profiles/{name}/apply` | `applyModelProfile` | `:536-786` |
| `POST …/apply` при занятом бэкенде | `handleAsyncApply` (202 + applyId) | `handlers_cppworker_apply_async.go:126-307` |
| `GET …/apply/progress?applyId=` | `handleApplyProfileProgress` (SSE) | `handlers_cppworker_apply_async.go:308-423` |
| `GET …/apply/status/{applyId}` | `handleApplyProfileStatus` | `handlers_cppworker_apply_async.go:424-460` |

Диспетчер подпутей — `handlers_cppworker_profiles.go:146-217`. Регистрация с
`AuthMiddleware(RateLimitMiddleware(...))` — `internal/api/routes.go:502-503`.

**Где и в каком формате хранятся.**

- В памяти: `config.LlamaCppModelProfiles` (`map[string]types.LlamaCppModelProfile`),
  доступ через снимки под замком — `internal/balancer/model_profiles_guard_r423.go:35-58`.
  Замок обязателен: админка пишет, горячий путь читает (`:1-31`).
- `config.json` — через `s.configSaver()`; в bundled-стенде `/app/config` примонтирован
  **read-only**, поэтому запись может не удаться (`handlers_cppworker_profiles.go:388-399`).
- Fallback: `/app/data/profiles.json`, формат
  `{"version":1,"profiles":{...},"defaultProfile":{...}}` —
  `internal/balancer/profiles_persistence.go:36-43`, путь вычисляется от
  `statePath`: `profilesFilePath()` `:52-58`.
- `statePath` — `LB_STATE_PATH`, дефолт `data/state.json` (`internal/config/config.go:155`);
  в compose переменная пробрасывается (`deployments/docker-compose.stack.yml:464`),
  том `/app/data` — `:573`. То есть фактический файл — `/app/data/profiles.json`
  (реальное содержимое файла на стенде **не проверено** — внутрь контейнера не
  заходил).
- `config.json` пишется **не атомарно**: `os.WriteFile` без tmp+rename
  (`internal/config/config.go:294-310`, права `0644`). Профили — атомарно (см. §1.6).

**Поля профиля** — `pkg/types/balancing.go:283-469`: `contextLength` (:284),
`batchSize` (:285), `numGpuLayers` (:286), `flashAttn` (:287), `numa` (:288),
`useMmap` (:289), `notes` (:290), `contextLengthAuto` (:312), `contextLengthMax` (:313),
`disabled` (:319), `ignoreDefaults` (:337), `primary` (:357), `streamingTimeoutSec` (:366),
`streamingIdleTimeoutSec` (:371), `requestTimeoutSec` (:376), `autoTune` (:388),
`firstByteTimeoutSec` (:390), `sizeBytes` (:395), `parallel` (:413),
`enableReasoning` (:418), `contextPerSeq` (:426), `kvCacheType` (:443),
`maxTokens` (:453), `overrideTensors`/`overrideTensorBufts` (:467-468).

**Нюансы контракта, важные для новой страницы.**

- Тело PUT/apply декодируется с `DisallowUnknownFields` (`:276`, `:568`) → шаблон
  обязан содержать только эти поля, иначе 400 `invalid JSON body`.
- PUT — **PATCH-like мерж**, а не замена (`mergeProfileUpdate` `:1040-1106`,
  `mergeModelProfile` `:1112-1206`); для `contextLengthAuto`/`contextLengthMax`/
  `disabled`/`primary`/`ignoreDefaults`/`enableReasoning`/`numGpuLayers`/`parallel`/
  `contextPerSeq` есть отдельный `profilePresence` (`:979-1019`), потому что там
  «явный 0/false» отличим от «не передано».
- Имя модели нормализуется на границе API: снимается `.gguf`, затем
  `balancer.NormalizeModelProfileKey` — `:156-182`. Любой новый код применения
  обязан идти через ту же нормализацию, иначе получим второй ключ профиля.
- Новый профиль без `contextLength` не падает 400, а подставляет факт из рантайма
  (`:298-314`) либо дефолт балансера/env (`:329-339`, `defaultContextLengthForNewProfile`
  `:444-456`).

**Валидация** — `validateModelProfile` `:1211-1266`:

| Поле | Правило | Строка |
|---|---|---|
| `contextLength` | обязателен, `[256, 262144]` | `:1212-1217` |
| `batchSize` | `0` или `>= 1` | `:1218-1220` |
| `numGpuLayers` | `[-2, 200]` (`-2`=AUTO, `-1`=все слои, `0`=CPU) | `:1229-1234` |
| `parallel` | `[0, 8]` | `:1238-1240` |
| `contextPerSeq` | `[0, 262144]`, 0 = не задано | `:1243-1245` |
| `kvCacheType` | `{"", "f16", "q8_0", "q4_0"}` | `:1246-1251` |
| `overrideTensors`/`overrideTensorBufts` | равная длина; buft = `CPU` или `CUDA<N>` | `:1256-1264` |

Отдельно: проверка памяти с вердиктом cppworker — `:355-382`; отказ →
**422 `profile_does_not_fit_memory`** с блоком `memory`
(`profile_memory_check_r116.go:51-79`). Деградация (часть слоёв в RAM) — не отказ,
только warn (`:374-381`).

**Коды ошибок PUT/apply** (`handlers_cppworker_profiles.go`): 400 `model name
required` (:150,:180), 400 `invalid JSON body` (:277-282,:569-574), 400
`invalid profile` (:343-353,:596-602), 400 `no profile to apply` (:587-594),
404 `profile not found` (:224-230,:469-475), 405 `method not allowed` (:115,:215),
422 `profile_does_not_fit_memory` (:367-373), 200 + `warning`, если не удалась
запись ни в config.json, ни в profiles.json (:406-412,:491-497).

**Что делает apply.** Семантика зафиксирована в комментарии `:511-524`:
(1) мерж тела в профиль и сохранение, (2) **reload** модели через cppworker
`POST /api/models/reload` (`reloadModelOnCppWorker` `:795-968`) на llama.cpp-бэкендах,
где модель загружена. Ключевые детали:

- `?backend=<id>` ограничивает скоуп (`matchesBackendFilter` `:531-534`, `:538`);
  пустой фильтр = «на всех» (`:527-530`).
- В тело reload уходит `force: true` (`:826`) — иначе cppworker отвечает
  `already_loaded` и НЕ меняет `n_ctx` (`:817-825`).
- Перед reload проверяется busy: если на бэкенде есть активные запросы → 202 +
  фоновая задача (`:629-684`).
- Оффлайн-бэкенд → `skipped` (`:705-714`), «модели нет на узле» (404/not loaded)
  → `skipped`, а не `error` (`:726-742`).
- Ответ 200: `{model, profile, backends:[{backendId,status,message}]}`,
  `status ∈ reloaded|skipped|error` (`:96-108`, `:781-785`).

**Обратная сторона (cppworker).** Профили не хранятся в воркере: cppworker
ПОДТЯГИВАЕТ их с балансера при старте, при load, при reload и раз в 5 минут —
`cmd/cppworker/sync_profile.go:1-27,139-...` (`GET /api/v1/cppworker/model-profiles`).
То есть балансер — единственный источник истины по значениям.

### 1.3 Пресеты WebUI — четыре независимых источника чисел

**(а) Быстрые пресеты `PRESETS`** — `webui/js/app.js:612-617`:

```
speed   {ctxSize:4096,  gpuLayers:-2, kvCacheType:'f16',  batchSize:512, idleUnloadMinutes:0}
memory  {ctxSize:2048,  gpuLayers:-2, kvCacheType:'q4_0', batchSize:256, idleUnloadMinutes:5}
context {ctxSize:32768, gpuLayers:-2, kvCacheType:'q8_0', batchSize:512, ropeScalingType:'yarn', ropeScalingFactor:4}
cpu     {ctxSize:2048,  gpuLayers:0,  kvCacheType:'q8_0', batchSize:256, nThreads:8}
```

`applyGgufPreset(name)` (`:618-641`) просто **заполняет поля формы** по карте id
(`:622-627`), подсвечивает кнопку и показывает тост. Значения нигде не
сохраняются: сохранение — обычное «Сохранить» страницы настроек. Кнопки —
`webui/index.html:1788-1797` (все с `data-admin-only`), живут внутри секции
`#section-llama-cpp` (`index.html:1774`); поля формы — `#gpuLayers`
(`index.html:1810`), `#ctxSize` (`:1849`), `#kvCacheType` (`:2009`).

Важно: это **не профиль модели**. Это глобальная llama.cpp-секция конфига
балансера, которая сохраняется общим «Сохранить» через `Api.updateConfig`
(`webui/js/modules/api.js:216-222` → `PUT /api/v1/cluster/config`) в
`/app/data/runtime-overrides/llama-cpp.json`
(`internal/api/handlers_llama_cpp_overrides.go:1-17`). Применяется она к бэкендам
как их «дефолт», а не к конкретной загруженной модели.

**(b) Пресеты железа мастера** — `webui/js/modules/setup-wizard-presets.js:19-97`
(`rtx30-8gb`, `rtx40-24gb`, `a10-24gb`, `rtx50-32gb`). У каждого: лимиты балансера
(`vramMaxUsage`, `gpuMaxUsage`, …) — они применяются к полям мастера, и
`cppworkerHint` (`:35-41`, `:53-59`, `:71-77`, `:89-95`) — набор env
`CPPWORKER_CTX_SIZE` / `CPPWORKER_BATCH_SIZE` / `CPPWORKER_GPU_LAYERS` /
`CPPWORKER_KV_CACHE_TYPE` / `CPPWORKER_RAM_FALLBACK_MAX_N_CTX`. По комментарию
самого файла (`:8-13`) hint **не применяется**: пользователю предлагается
запустить `scripts/apply-hardware-preset.py` отдельно. Источник истины —
`config/hardware-presets/*.json` (в репозитории 4 файла: `a10-24gb.json`,
`rtx30-8gb.json`, `rtx40-24gb.json`, `rtx50-32gb.json`).

**(c) Пресеты contextLength в редакторе профиля** —
`webui/js/modules/cppworker-params.js:37-46` (`CTX_PRESETS` 4K…256K + Custom),
границы `CTX_MIN=256`/`CTX_MAX=262144` (`:47-48`), `GPU_LAYERS_MIN=-2` /
`GPU_LAYERS_MAX=200` (`:64-65`), `BATCH_SIZE_MIN=0`/`BATCH_SIZE_MAX=4096` (`:66-67`).
Это уже per-model: пресет — только кнопка-заполнялка слайдера, значение уходит в
`PROFILES.upsert` (`:1071-1075`), а применение — `PROFILES.apply`
(`applyProfileWithProgress` `:1132-...`, вызов `:1207`) с модалкой прогресса по
бэкендам.

**(d) Профили в настройках GGUF** — `mountProfilesInSettings`
(`gguf-renderer-detail-render.js:3857-3870`) привязывает кнопку «+» к
`CppWorkerParams.openWizard` и вызывает `CppWorkerParams.loadAndRender()`.
Контейнеры создаются динамически при рендере панели: `#cppProfilesList` (`:2118`),
`[data-role="cpp-load-defaults"]` (`:2145`), `[data-role="cpp-model-catalog"]`
(`:2156`). Вызывающий — `gguf-renderer-detail.js:95`.

**Дублирование значений (факт, не догадка).** Одно и то же число живёт в 3–5
местах, и они не связаны:

| Значение | app.js PRESETS | setup-wizard-presets cppworkerHint | env cppworker (стенд) | профиль модели (стенд) | docs |
|---|---|---|---|---|---|
| окно контекста | 2048/4096/32768 (`app.js:613-616`) | 32768/65536 (`setup-wizard-presets.js:36,54,72,90`) | `defaultCtxSize: 65536` (живой `model-catalog`) | 21 профиль: 32768×15, 16384×4, 65536×1, 131072×1 | таблица и 6 профилей (`model-tuning-reference.ru.md:28-160`) |
| KV-cache | `f16`/`q4_0`/`q8_0` | `q4_0`/`f16` | `defaultKvCacheType: "q8_0"` | `q8_0`×8, `q4_0`×2, у 11 — не задано (наследование) | `:33` |
| gpu layers | `-2`/`0` | `20`/`99` | `defaultGpuLayers: -1` | `-2`/`-1`/`20` | `:34` |
| batch | 256/512 | 512/1024 | `defaultBatchSize: 512` | 256/512 | `:35` |
| слоты (`parallel`) | — | — | `defaultNParallel: 1` | задан только у 2 профилей (значение `1`) | `:32` |

Ни один из этих источников не знает про остальные; ни у одного нет ни id, ни
комментария, ни версии.

### 1.4 Как UI понимает, «где загружена модель»

| Эндпоинт | Файл:строки | Формат | Пригодность |
|---|---|---|---|
| `GET /api/v1/cluster/models/loaded` | `routes.go:132`, `handlers_cluster_models.go:70-132`, тип `:20-53` | `{count, models:[{name, backendId, backendType, engine, contextLength, batchSize, numGpuLayers, quantization, vramUsage, ramUsage, size, state}], modelsPerBackend:{id:N}}` (`modelsPerBackend` — `omitempty`, `handlers_cluster_models.go:52`) | **ПОДХОДИТ**: одна запись на пару (модель, бэкенд) ⇒ привязка модель→бэкенды явная |
| `GET /api/v1/models` | `routes.go:124`, `handlers_sessions.go:103-139` | `{backends:[{backendId, backendName, backendStatus, hasAgent, models:[...]}], total}` | **НЕ подходит для cppworker**: отдаёт `metrics.Ollama.RunningModels` (`:131`), а у llama.cpp-бэкенда это `null` (живой ответ ниже) |
| `GET /api/v1/gguf/backends` | `routes.go:418` (публичный), живой ответ | `{backendType, backends:[{id,type,status,host,cppWorkerPort,models:[],gpuMemory{...},hasAgent}], operatingMode, total}` | годится как «какие узлы вообще есть», `models` у cppworker пустой (поле заполняется только под ollama) |
| `GET /api/v1/models/catalog` | `routes.go:100` | `{backends:[{backendId,type,status,host,files:[...],known,ageSec}], refreshIntervalSec, total}` | «что лежит на диске у узла», не «что загружено» |
| `GET /api/v1/cppworker/model-catalog` | `routes.go:512` | `{cppworker{backendId,available,config,outputBudget}, defaultModelProfile, files:[{name,sizeBytes,quantization,modifiedAt,loaded,hasProfile,profileIgnored,profile}]}` | лучший per-model срез «профиль vs файл vs загружено» для одного (главного) воркера |

Внутри Go то же решение принимается через `IsLlamaCppModelLoaded`
(`internal/balancer/num_ctx_resolver.go:1052-1077`, сравнение имён
`modelNameMatches`) и `LoadedContextForModel` (`:1115-1132`). В WebUI —
`Api.clusterModels.loaded()` (`webui/js/modules/api.js:760-767`) и
`window.CPPWORKER_LOADED_MODELS.models` (`webui/js/modules/cppworker-params.js:1109-1120`).

**Живые ответы стенда (2026-11-17, сокращённо).** Моделей на стенде СЕЙЧАС не
загружено, поэтому непустого примера получить не удалось:

```
GET /api/v1/cluster/models/loaded
{ "count": 0, "models": [] }

GET /api/v1/models
{ "backends": [
    {"backendId":"imageworker","backendName":"imageworker","backendStatus":"healthy","hasAgent":true,"models":null},
    {"backendId":"cppworker-gpu-bundled-agent","backendName":"cppworker-gpu-bundled-agent","backendStatus":"healthy","hasAgent":true,"models":null},
    {"backendId":"cppworker-cyborg-bundled-agent","backendName":"cppworker-cyborg-bundled-agent","backendStatus":"unhealthy","hasAgent":false,"models":[]}
  ], "total": 3 }

GET /api/v1/gguf/backends
{ "backendType":"llama_cpp",
  "backends":[{"id":"cppworker-gpu-bundled-agent","type":"llama_cpp","status":"healthy",
               "host":"cppworker-gpu","cppWorkerPort":18092,"models":[],
               "gpuMemory":{"totalMB":8192,"usedMB":1233,"freeMB":6959},
               "vramUsagePercent":15.05,"activeRequests":0,"maxConcurrentReqs":1,"hasAgent":true}],
  "operatingMode":"standard","total":1 }

GET /api/v1/models/catalog
{ "backends":[{"backendId":"cppworker-gpu-bundled-agent","type":"llama_cpp","status":"healthy",
   "files":["Qwen3-4B-Instruct-2507-Q4_K_M.gguf","Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M.gguf",
            "gemma-4-E4B-it-Q4_K_M.gguf"],"known":true,"ageSec":23}, ...],
  "refreshIntervalSec":60,"total":3 }
```

Профили (21 запись), пример реального элемента и хвост ответа:

```
GET /api/v1/cppworker/model-profiles
{ "models": {
    "Qwen3-4B-Instruct-2507-Q4_K_M": {
      "contextLength":32768,"batchSize":512,"numGpuLayers":-1,"flashAttn":true,"useMmap":true,
      "contextLengthAuto":true,"contextLengthMax":65536,"ignoreDefaults":true,
      "parallel":1,"enableReasoning":false,"kvCacheType":"q8_0" },
    "Qwen3.6-35B-A3B-UD-Q4_K_M": { "contextLength":65536,"batchSize":256,"numGpuLayers":-2, ... },
    ... },
  "total":21,
  "effectiveContext": { "gemma-4": {"effectiveContextLength":65536,"limitedBy":"env",
      "limitReason":"окно модели задано 32768, но LB_NCTX_RELOAD_MAX_N_CTX=65536 разрешает авто-рост только до него",
      "envCeiling":65536}, ... },
  "nctxCeiling": { "envCeiling":65536,"limitedBy":"env","reason":"потолок авто-роста окна LB_NCTX_RELOAD_MAX_N_CTX=65536; …" } }
```

(Потолок `65536` совпадает с `LB_NCTX_RELOAD_MAX_N_CTX=65536` в
`deployments/.env:85`. Поле `effectiveContext` введено R267 —
`handlers_cppworker_profiles.go:34-48`, `nctxCeiling` — R319, `:50-66`.)

Ещё два полезных живых среза:

```
GET /api/v1/cppworker/load-defaults
{ "defaultModelProfile": {"contextLength":32768,"batchSize":512,"numGpuLayers":30,
    "flashAttn":true,"useMmap":true,"contextLengthAuto":true,"contextLengthMax":131072},
  "effective": {"batchSize":512,"contextLength":32768,"contextPerSlot":32768,
    "enableReasoning":false,"flashAttn":1,"kvCacheType":"q8_0","numGpuLayers":30,
    "numPredictCeiling":16384,"numPredictReasoning":8192,"parallel":1,"useMmap":true},
  "source": {"contextLength":"defaultProfile","kvCacheType":"cppworker-env",
    "parallel":"cppworker-env","batchSize":"defaultProfile", ...},
  "cppworker": {"backendId":"cppworker-gpu-bundled-agent","available":true,
    "config": {"defaultCtxSize":65536,"defaultKvCacheType":"q8_0","defaultGpuLayers":-1,
               "defaultBatchSize":512,"defaultNParallel":1, ...},
    "outputBudget": {"ceiling":16384,"reasoningDefault":8192, ...}} }
```

Именно формат `effective` + `source` (значение + откуда взялось) — готовый
образец того, как новая страница должна показывать «предлагаемое значение vs
действующее».

### 1.5 Где сейчас «справка/рекомендации»

| Источник | Объём | Структура | Редактируется из UI |
|---|---|---|---|
| `docs/operations/model-tuning-reference.ru.md` | 184 строки | §1 таблица «Поле / Что делает / Как выбирать / Ошибка, которую ловит» (`:28-45`); §2 env-таблица (`:49-61`); §3 шесть готовых профилей блоками JSON + обоснование (`:64-160`); §4 как проверить (`:162-176`) | **нет** |
| `docs/architecture/context-numbers.ru.md` | 391 строка | 9 чисел контекста (`:24`), кто что отдаёт (`:52`), живые числа стенда (`:94`), 4 дефекта (`:184-303`), как читать оператору (`:319`) | **нет** |
| `docs/operations/troubleshooting.ru.md` | 1540 строк | симптом → диагностика → решение; десятки curl-рецептов (напр. `:159-164` — PUT+apply профиля) | **нет** |
| Секция Help в WebUI | `webui/index.html:2380-2425` | 5 ссылок (`scheme.html`, README, api.md, balancing-guide, GitHub), ключи `help.*` | **нет**: ни одного `input`/`textarea` в секции |
| `notes` в профиле модели | `pkg/types/balancing.go:290` | одна строка текста на модель; мержится как `update.Notes != ""` (`handlers_cppworker_profiles.go:1135-1137`); показывается в списке профилей (`cppworker-params.js:566`) | да, но это ОДНА строка без структуры, без источника и без версий |

Вывод: рекомендации существуют только как проза в markdown; машиночитаемого
эквивалента нет; единственное редактируемое «поле для человека» — `notes`
профиля, и оно не связано ни с параметрами, ни с доками.

**Живое подтверждение потребности.** На стенде `notes` заполнен у **1 профиля из
21** — и содержит ровно то, что просит оператор (обоснование с замерами):

```
Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M:
"R415/R418: Qwen3.8-9B distill uncensored, Q4_K_M 5.38GB. 8GB VRAM +
 CPPWORKER_VRAM_OVERHEAD_MB=512: 32k и 64k грузятся всеми слоями (33/32).
 contextLengthMax=65536 подтверждён живой загрузкой."
```

То есть оператор УЖЕ пишет рекомендацию в единственное доступное поле — связки
«номер раунда + замер + вывод» там нет, проверить её нельзя, и на странице Help она
не видна.

### 1.6 Серверное хранение заметок/шаблонов и запись состояния

**Чего нет.** Поиск по `internal/` (`grep -i 'annotation|userNotes|templates.json|presets.json'`)
дал только ответы OpenAI Responses API (`internal/balancer/responses_r247.go:288,471,497,503,508`
— заглушки `"annotations": []`) и chat-template в cppworker
(`internal/cppbackend/backend.go:3419-3520`). Хранилища пользовательских заметок,
аннотаций или шаблонов параметров **не существует**. `template` в коде — всегда
`chat template`, не «набор параметров».

**Как балансер пишет файлы состояния (это образец для нового хранилища).**

| Хранилище | Путь | Источник пути | Запись | Права |
|---|---|---|---|---|
| Профили моделей | `/app/data/profiles.json` | рядом со `statePath` (`profiles_persistence.go:52-58`), `statePath` = `LB_STATE_PATH` | **атомарно**: `os.WriteFile(tmp, data, 0644)` + `os.Rename` (`:186-200`) | `0644` файл, `0755` каталог (`:172,193`) |
| Горячие настройки | `/app/data/settings-overrides.json` | `settings.FileName` рядом со state.json (`cmd/balancer/main.go:233-237`), `internal/settings/settings.go:303` | **атомарно**: tmp+rename (`settings.go:348-372`) | `0644`, `0755` (`:363-368`) |
| image-профили | `/app/data/image-model-profiles.json` | `LB_IMAGE_MODEL_PROFILES_PATH` (`internal/config/image_model_profiles.go:37-41`, compose `:522`) | **атомарно**: tmp+rename (`:243-247`) | — |
| Калибровка промпта | `/app/data/prompt-calibration.json` | рядом со state.json (`prompt_calibration_r302.go:192-199`) | **атомарно** (`:436-441`) | — |
| Роли бэкендов | `/app/data/runtime-overrides/backend-roles.json` | `runtimeoverrides.DirName` (`internal/runtimeoverrides/backend_roles_r118.go:13`) | **атомарно** (`:78-82`) | — |
| Отключённые бэкенды | `/app/data/runtime-overrides/backend-disabled.json` | там же | **атомарно** (`:95-99`) | — |
| Пользователи | `/app/data/users.json` | `LB_USERS_FILE` (`deployments/docker-compose.stack.yml:169`) | **атомарно** (`internal/api/auth_session.go:449-453`) | — |
| Сессии | `/app/data/sessions.json` | `LB_SESSIONS_FILE` (`compose:173`) | **атомарно** (`auth_session.go:738`) | — |
| `config.json` | `-config` флаг, дефолт `config/config.json` (`cmd/balancer/main.go:31`) | флаг | **НЕ атомарно** (`internal/config/config.go:294-310`) | `0644` |

Устойчивая конвенция репозитория: **вспомогательное состояние — отдельный файл в
записываемом томе `/app/data`, запись атомарная, путь при необходимости
переопределяется env, отсутствие файла — не ошибка** (пример: `profiles_persistence.go:66-79`,
`settings.go:322-346`). Ломать эту конвенцию новым хранилищем не нужно.

### 1.7 Админские API и права

- Префикс `api/v1/...`; регистрация — `internal/api/routes.go:1-552`. Защищённые
  ручки оборачиваются `AuthMiddleware(RateLimitMiddleware(handler, s.rateLimiter), s.authenticator)`
  (пример: `:502-503`).
- `AuthMiddleware` — `internal/api/auth.go:181-232`:
  1. auth выключен → пропустить (`:183-187`);
  2. **приоритет серверной сессии** (`:189-214`): если метод не read-only и роль
     не `admin` → **403** `admin_required_viewer` (`:191-200`); затем проверка
     `X-CSRF-Token` (`:201-209`); дальше `X-Authenticated-Token: session:<user>`;
  3. иначе токен: неверный → **401** (`:216-225`), верный → `X-Authenticated-Token`.
- Read-only методы (`GET/HEAD/OPTIONS`) доступны роли `viewer` — `isReadOnlyMethod`
  `auth.go:253-259`. Роли только две: `admin`/`viewer` (`auth_session.go:504-505`).
- Ручки управления пользователями — `routes.go:31-40` (login/logout/me/users/sessions),
  `/api/v1/auth/status` `:39`.
- Как ходит WebUI: `Api` собирает заголовки в `getOptions()` — `X-API-Token` из
  `WEBUI_CONFIG.API_TOKEN` (`webui/js/modules/api.js:63-72`); таймауты: чтение
  ограничено, запись — без таймаута (`:75-126`, комментарий `:34-60`); CSRF для
  сессионных мутаций добавляет обёртка fetch в `auth.js` (`api.js:913-917`).
- Видимость админских элементов: разметка помечается `data-admin-only`,
  `auth.js:358-391` (`isAdminNow` `:367-370`, `applyRoleVisibility` `:372-391`),
  плюс `MutationObserver` для динамически нарисованных кнопок (`:397-407`).
  Явно подчёркнуто, что это только UX, сервер всё равно проверяет права (`:361-362`).

**Практическое следствие для новой страницы:** отдельный механизм прав не нужен.
`GET` новых ручек автоматически доступен viewer, `PUT/POST/DELETE` — только
admin+CSRF. Аварийный вход по токену даёт полные права (`auth.js:367-370`,
`auth.go:216-228`).

---

## 2. Контракт хранения

### 2.1 Файл и путь

**Один файл** (рекомендации и шаблоны живут и меняются вместе, один замок, одна
атомарная запись):

```
<dir(LB_STATE_PATH)>/model-reference.json        # в контейнере: /app/data/model-reference.json
```

- Путь вычисляется как `filepath.Join(filepath.Dir(LB_STATE_PATH), "model-reference.json")`
  — ровно как `profilesFilePath` (`internal/balancer/profiles_persistence.go:52-58`)
  и `settingsPath` (`cmd/balancer/main.go:233-237`).
- Override: `LB_MODEL_REFERENCE_PATH` (по образцу `LB_IMAGE_MODEL_PROFILES_PATH`,
  `internal/config/image_model_profiles.go:37-41`).
- Каталог `balancer_data:/app/data` уже примонтирован (`deployments/docker-compose.stack.yml:573`).
  **`config.json` не трогаем:** `/app/config` в bundled-стенде read-only
  (`handlers_cppworker_profiles.go:388-390`, `profiles_persistence.go:1-15`).

Права — по конвенции: файл `0644`, создаваемый каталог `0755`.

### 2.2 Формат

```json
{
  "version": 1,
  "recommendations": [ ... ],
  "templates": [ ... ]
}
```

`version` обязателен: неизвестная версия → warn и **не читать** (как
`profiles_persistence.go:86-90`), а не падать.

### 2.3 Запись-рекомендация

```json
{
  "id": "qwen3-4b-8gb-agent",
  "title": "4B Q4 на 8 ГБ VRAM: окно 32K, KV q8_0, один слот",
  "appliesTo": {
    "modelPattern": "Qwen3-4B-Instruct-2507*",
    "backendType": "llama_cpp",
    "quantization": "Q4_K_M",
    "vramMinMB": 8192,
    "vramMaxMB": 0
  },
  "params": [
    { "name": "contextLength", "value": 32768, "unit": "tokens" },
    { "name": "contextPerSeq", "value": 32768, "unit": "tokens" },
    { "name": "parallel",      "value": 1 },
    { "name": "kvCacheType",   "value": "q8_0" },
    { "name": "batchSize",     "value": 512 },
    { "name": "numGpuLayers",  "value": -1, "comment": "-1 = все слои, влезает по живому замеру" },
    { "name": "flashAttn",     "value": true },
    { "name": "enableReasoning","value": false }
  ],
  "comment": "Агентский цикл: размышления едят бюджет хода, окно 32K держит сессию Cline без reload.",
  "sources": [
    { "doc": "docs/operations/model-tuning-reference.ru.md", "section": "3.2" }
  ],
  "author": "operator",
  "createdAt": "2026-11-17T10:00:00Z",
  "updatedAt": "2026-11-17T10:00:00Z",
  "revision": 1
}
```

Правила:

- `id` — slug `^[a-z0-9][a-z0-9._-]{2,63}$`, задаётся при создании, **не меняется**
  (в URL). Не уникальность заголовка, а id — ключ.
- `params[].name` — **только** имена полей `types.LlamaCppModelProfile`
  (`pkg/types/balancing.go:284-468`). Никаких «своих» имён: иначе набор нельзя
  применить. Значение — JSON-примитив, тип выводится по метаданным поля
  (см. 2.6).
- `unit` — метка для человека (`tokens`, `layers`, `слоты`), **не** участвует в
  вычислениях. По умолчанию — каноническая единица поля, поэтому может отсутствовать.
- `comment` есть у записи и, опционально, у отдельного параметра.
- `sources[].doc` — путь от корня репозитория; проверка существования файла — **не
  проверено/не реализовано** (в контейнере доков нет).
- `appliesTo` — только для подбора/подсветки в UI. **Никогда не используется как
  разрешение на автоприменение.**
- `revision` — целое, растит сервер при каждом успешном PUT.

### 2.4 Шаблон

```json
{
  "id": "agent-8gb",
  "name": "Агент на 8 ГБ VRAM",
  "version": 2,
  "params": {
    "contextLength": 32768,
    "contextPerSeq": 32768,
    "parallel": 1,
    "kvCacheType": "q8_0",
    "batchSize": 512,
    "numGpuLayers": -1,
    "flashAttn": true,
    "useMmap": true,
    "enableReasoning": false
  },
  "comment": "Годится для 4B–8B Q4 на 8 ГБ; на 3B можно поднять окно до 64K.",
  "basedOn": "qwen3-4b-8gb-agent",
  "author": "operator",
  "createdAt": "...", "updatedAt": "...", "revision": 1
}
```

Отличия от рекомендации намеренные:

- `params` — **плоский объект в форме тела `PUT /api/v1/cppworker/model-profiles/{name}`**,
  потому что применяется он именно этим телом (мерж + валидация + `DisallowUnknownFields`).
  Второй структуры «набор параметров» не заводим.
- `version` — версия содержимого шаблона для человека (при применении пишется в
  лог и в ответ), `revision` — служебный счётчик для оптимистичной блокировки.
- `basedOn` — ссылка на рекомендацию, из которой шаблон вырос (может не быть).

### 2.5 Атомарная запись, отсутствие файла, миграция, дефолты

- Запись: `MarshalIndent` → `os.WriteFile(path+".tmp", 0644)` → `os.Rename`
  (образец: `profiles_persistence.go:186-200`, `settings.go:348-372`). Пакетный
  `sync.Mutex` вокруг записи — как `profilesFileMu` (`profiles_persistence.go:45-47`).
- Чтение при старте **до** первого HTTP-запроса; ошибки не фатальны: файла нет →
  пусто (`profiles_persistence.go:72-80`), битый JSON → warn + пусто
  (`settings.go:336-340`).
- **Отсутствие файла = валидное состояние**: `{"version":1,"recommendations":[],"templates":[]}`,
  GET отдаёт пустые списки (не 404, не 503). Иначе UI будет выглядеть сломанным до
  первой записи.
- **Миграция**: `version` читается первым; неизвестная → warn, работаем без данных.
  При добавлении поля — только опциональные поля, старые файлы читаются как есть
  (тот же приём, что `DefaultProfile *...` в `profiles_persistence.go:33-40`).
- **Дефолты (seed)**: по решению человека (см. §6, вопрос 4) либо (а) пусто, либо
  (б) однократный импорт готовых профилей из `docs/operations/model-tuning-reference.ru.md:64-160`
  в виде набора записей, помеченных `author:"docs-import"`. Автоматическое
  перечитывание доков на каждом старте — **не надо** (доки станут вторым
  источником истины).
- Значения из файла **не** попадают в `LlamaCppModelProfiles` автоматически. Файл —
  справочник и заготовки; истина по действующим значениям — только профили (§7).

### 2.6 Границы значений — откуда берутся, чтобы не завести третью копию

Единственный серверный валидатор — `validateModelProfile`
(`internal/api/handlers_cppworker_profiles.go:1211-1266`). Новая ручка
**обязана вызывать его**, а не свою копию границ. В UI метаданные поля берутся из
уже существующего реестра `MODEL_PARAM_SPECS`
(`webui/js/modules/gguf-renderer-detail-render.js:2490-2524`): `field`, `type`,
`min`, `max`, `step`, `options`, `label`(i18n), `hint`(i18n). Это уже проверено
тестами и совпадает с `LlamaCppModelProfile`.

Расхождение, которое надо явно решить (не блокер, но факт):

| Параметр | `validateModelProfile` | `MODEL_PARAM_SPECS` | `cppworker-params.js` |
|---|---|---|---|
| `contextLength` | 256–262144 (`:1215`) | 256–1048576 (`:2491`) | `CTX_MAX=262144` (`:48`) |
| `parallel` | 0–8 (`:1238`) | 0–32 (`:2503`) | 0–8 (`:1043`) |
| `batchSize` | ≥1 (`:1218`) | 1–4096 (`:2500`) | 0–4096 (`:66-67`) |

Сервер — арбитр; UI-границы могут быть шире только если сервер их принимает.

---

## 3. Контракт API

Префикс `/api/v1/reference/...`. Все ручки регистрируются через
`AuthMiddleware(RateLimitMiddleware(...))` — `routes.go:502-503` как образец.
Отдельных проверок ролей не добавляем: `GET` доступен viewer, мутации — admin+CSRF
(`auth.go:189-214`).

### 3.1 Рекомендации

| Метод, путь | Тело | Ответ | Ошибки |
|---|---|---|---|
| `GET /api/v1/reference/recommendations` | — | `{items:[Recommendation], total, schemaVersion}` | 200 |
| `GET /api/v1/reference/recommendations/{id}` | — | `{recommendation, relatedTemplates:[{id,name}]}` | 404 `recommendation_not_found` |
| `PUT /api/v1/reference/recommendations/{id}` | `Recommendation` без `createdAt/author/revision` (или с ними — сервер игнорирует) | `{status:"ok", recommendation}` | 400 `invalid_id`, 400 `invalid_json`, 400 `unknown_param` (`{"error":"unknown_param","field":"nCtx","allowed":[...]}`), 400 `invalid_value` (`{"field":"contextLength","min":256,"max":262144,"got":1310720}`), 409 `revision_conflict` (`{"current":3,"sent":2}`) |
| `DELETE /api/v1/reference/recommendations/{id}` | — | `{status:"ok", id}` | 404 |
| `POST /api/v1/reference/recommendations/{id}/to-template` | `{"templateId":"...","name":"..."}` (опционально) | `{status:"created", template}` | 404, 409 (id занят) |

`PUT` идемпотентен и upsert-ит; `revision` в теле — необязательная оптимистичная
блокировка (если передан и не совпал с текущим → 409).

### 3.2 Шаблоны

| Метод, путь | Тело | Ответ | Ошибки |
|---|---|---|---|
| `GET /api/v1/reference/templates` | — | `{items:[Template], total, schemaVersion}` | 200 |
| `GET /api/v1/reference/templates/{id}` | — | `{template, usage:{usedByProfiles:[...]}}` | 404 `template_not_found` |
| `PUT /api/v1/reference/templates/{id}` | `Template` | `{status:"ok", template}` | те же + 400 `invalid_params` (обёртка над `validateModelProfile`: `{"error":"invalid_params","message":"parallel must be in [0, 8], got 12"}`) |
| `DELETE /api/v1/reference/templates/{id}` | — | `{status:"ok", id}` | 404 |
| `POST /api/v1/reference/templates/{id}/preview` | см. 3.3 | отчёт | 400/404/422/503 |
| `POST /api/v1/reference/templates/{id}/apply` | см. 3.4 | результат | 400/404/409/422/502 |

### 3.3 Предпросмотр (dry-run)

```
POST /api/v1/reference/templates/{id}/preview
{ "model": "Qwen3-4B-Instruct-2507-Q4_K_M",
  "backendId": "cppworker-gpu-bundled-agent"    // опционально
}
```

Ответ 200:

```json
{
  "ok": true,
  "model": "Qwen3-4B-Instruct-2507-Q4_K_M",
  "profile": { "contextLength": 32768, "...": "..." },        // merge существующего профиля с params шаблона
  "valid": { "ok": true, "violations": [] },                   // validateModelProfile
  "diff": [ {"field":"contextLength","current":16384,"next":32768},
            {"field":"kvCacheType","current":"f16","next":"q8_0"} ],
  "targets": [ {"backendId":"cppworker-gpu-bundled-agent","loaded":true,"contextLength":16384,"activeQueries":0},
               {"backendId":"cppworker-cyborg-bundled-agent","loaded":false} ],
  "memory": { "feasible": true, "degraded": false, "stage": "vram",
              "message": "профиль помещается целиком" },        // блок memory от GET /api/models/effective
  "warnings": []
}
```

Реализация предпросмотра **переиспользует существующее**: мерж — `mergeProfileUpdate`
(`handlers_cppworker_profiles.go:1040-1106`), границы — `validateModelProfile`
(`:1211-1266`), память — `checkProfileMemory` → cppworker
`GET /api/models/effective?model=&ctx=&kvCacheType=`
(`profile_memory_check_r116.go:1-118`, `cmd/cppworker/handlers_effective_r116.go:1-50`,
`router.go:40`). Ничего не сохраняет, reload не запускает.

`ok:false` (200, не ошибка) — когда нарушения есть, но запрос корректен:
`{"ok":false,"valid":{"ok":false,"violations":[{"field":"parallel","message":"..."}]}}`.
Коды 4xx — только для некорректного запроса/отсутствующей сущности.

### 3.4 Применение шаблона

```
POST /api/v1/reference/templates/{id}/apply
{ "model": "Qwen3-4B-Instruct-2507-Q4_K_M",
  "backendId": "cppworker-gpu-bundled-agent",   // "" или отсутствует = все llama.cpp, где загружена
  "mode": "profile+reload",                     // "profile-only" | "profile+reload"
  "dryRun": false,
  "confirmNonFeasible": false                   // обязателен, если память даёт feasible=false
}
```

**Прямой ответ на вопрос из задания: применение шаблона — это СОХРАНЕНИЕ ПРОФИЛЯ
ПЛЮС существующий reload, а не новая операция.** Реализация обязана быть
последовательностью двух уже существующих вызовов, а не вторым путём
сохранения/перезагрузки:

- `mode:"profile-only"` → семантика `PUT /api/v1/cppworker/model-profiles/{name}`
  (`handlers_cppworker_profiles.go:251-433`): мерж + валидация + проверка памяти +
  запись профиля и его персист.
- `mode:"profile+reload"` → семантика `POST …/apply?backend=<id>`
  (`:536-786`): тот же мерж + reload через `POST /api/models/reload` с `force:true`
  (`:826`), проверка busy и 202-async (`handlers_cppworker_apply_async.go`).

Ответ 200 (синхронный путь):

```json
{ "status": "applied",
  "model": "Qwen3-4B-Instruct-2507-Q4_K_M",
  "templateId": "agent-8gb", "templateVersion": 2,
  "profile": { "...": "..." },
  "backends": [ {"backendId":"cppworker-gpu-bundled-agent","status":"reloaded","message":"reloaded"} ] }
```

Ответ 202 (занятый бэкенд) — проксируется как есть из async-пути:
`{applyId, status:"queued", progressUrl:"/api/v1/cppworker/model-profiles/{name}/apply/progress?applyId=…", statusUrl:…, backends:[…]}`.
UI продолжает опрос существующими `Api.cppworkerApplyProgress`
(`webui/js/modules/api.js:678-751`); **третьего механизма прогресса не заводим**.

Ошибки применения:

| Код | `error` | Когда |
|---|---|---|
| 400 | `invalid_json`, `model_required`, `invalid_mode` | тело/режим |
| 404 | `template_not_found`, `profile_not_found`* | *для `profile-only` при отсутствии профиля и отсутствии `params` |
| 409 | `revision_conflict` | шаблон изменился между preview и apply (если клиент передал `revision`) |
| 422 | `profile_does_not_fit_memory` | вердикт cppworker `feasible=false`; тело — как в существующем PUT: `{error,message,model,memory}` (`handlers_cppworker_profiles.go:367-373`) |
| 502 | `reload_failed` | все целевые бэкенды вернули `status:"error"` (частичный успех — 200 с массивом, как сейчас) |
| 503 | `store_unavailable`, `no_loaded_targets` | хранилище не инициализировано / модель нигде не загружена при `mode:"profile+reload"` |

Отдельно: **`no_loaded_targets` — не ошибка конфигурации**, а честный ответ «некуда
применять сейчас». В существующем apply такой случай даёт 200 с `skipped`-ами
(`handlers_cppworker_profiles.go:523-524`); для явного действия оператора лучше
409/503 с внятным текстом.

### 3.5 Список «где загружена модель» — новый эндпоинт НЕ нужен

UI берёт целиком `GET /api/v1/cluster/models/loaded` (`handlers_cluster_models.go:70-132`)
и группирует по `name` → `[backendId]`. Это единственный честный источник привязки
модель→бэкенд(и). `GET /api/v1/models` для llama.cpp бесполезен (`models:null`,
`handlers_sessions.go:131`). Дублировать этот ответ под новым путём — вредно.

Если понадобится «одно поле для выбора бэкенда», допускается тонкая обёртка:

```
GET /api/v1/reference/targets?model=<name>
{ "model":"...", "targets":[{"backendId":"...","backendType":"llama_cpp","status":"healthy",
   "loaded":true,"contextLength":16384,"hasProfile":true,"activeQueries":0}] }
```

— но только как проекция `cluster/models/loaded` + `IsLlamaCppModelLoaded` +
`GetActiveQueriesForModel` (последние два уже используются в apply,
`handlers_cppworker_profiles.go:646-678`).

---

## 4. UI

### 4.1 Куда встраивать

Страница как отдельный раздел (не внутри «Настроек»): рекомендаций и шаблонов много,
нужен свой список и редактор, а `#section-help` — это только ссылки
(`index.html:2380-2425`).

1. Пункт меню: новый `<a href="#reference" class="nav-item" data-page="reference">`
   с `data-i18n="nav.reference"` — по образцу `index.html:88-132`.
2. Контейнер страницы: `<div class="page" id="reference-page">` по образцу
   `index.html:653` (`#gguf-page`).
3. Роутер: `switchPage(page)` (`webui/js/app.js:180`) + `case 'reference':` в блоке
   `switch` (`app.js:274-...`), по образцу `case 'settings':` (`app.js:378-399`) —
   вызвать `ReferencePage.mount()` и `ReferencePage.load()`.
4. Модуль: `webui/js/modules/reference-page.js` (один владелец состояния, как
   `settings-registry.js:25-29,76-87`), подключается `<script defer>` до `app.js`
   (порядок — как у остальных модулей, `index.html:2780-2782`).
5. i18n: ключи `nav.reference`, `reference.*` в `webui/js/i18n/ru.js` и `en.js`
   (формат плоских точечных ключей, напр. `"settings.section.help": "Справка"` —
   `ru.js:628`, `en.js:1370`).
6. HTTP: обёртки в `webui/js/modules/api.js` рядом с `cppworkerModelProfiles`
   (`api.js:492-577`) — новый объект `reference` с `list/get/upsert/remove/preview/apply`.
   Заголовки и таймауты не собирать вручную (`api.js:390-395` — явное предупреждение
   против этого).

### 4.2 Структура страницы

```
Reference (admin-editable, viewer read-only)
├── Вкладка «Рекомендации»
│   ├── Фильтры: поиск по title/комментарию; «применимо к модели …» (подсветка по appliesTo)
│   ├── Список записей: title, чипы параметров (contextLength=32K, kv=q8_0…), автор/дата
│   └── Редактор (drawer/modal):
│       ├── id (readonly при редактировании), title
│       ├── appliesTo: modelPattern / backendType / quantization / vramMin..Max
│       ├── таблица параметров: select из MODEL_PARAM_SPECS → поле значения
│       │   (тип/min/max/step/options оттуда) → unit → построчный комментарий
│       ├── comment/обоснование (textarea)
│       ├── sources[]: ссылка на доку (строка пути)
│       └── «Сохранить» / «Удалить» / «Создать шаблон из записи»
└── Вкладка «Шаблоны»
    ├── Список: name, version, число параметров, «чем отличается от текущего профиля»
    └── Карточка шаблона:
        ├── таблица параметров + comment
        ├── селектор модели (те, что в профилях/каталоге) и БЭКЕНДА (из cluster/models/loaded,
        │   только где модель загружена; «все, где загружена» — отдельным пунктом)
        ├── «Предпросмотр» → diff + вердикт памяти + список целей
        └── «Применить» → существующая модалка прогресса CppWorkerParams
```

Кнопка **«применить к загруженной модели»** обязана:

- не быть активной, если в `targets` нет ни одной загруженной цели (иначе оператор
  нажмёт и получит «некуда»);
- показывать выбранный бэкенд явно, а не «применить везде» по умолчанию
  (живой дефект R398: «применить» задевало чужой/недоступный узел —
  `handlers_cppworker_profiles.go:638-645`, `:527-530`);
- для занятого бэкенда переходить на async-прогресс, не блокируя форму
  (`:629-684`, `cppworker-params.js:1225-1335`).

### 4.3 Что переиспользуется (без второй копии знаний)

| Что | Откуда берём | Почему не пишем заново |
|---|---|---|
| Метаданные полей (label i18n, тип, min/max, options) | `MODEL_PARAM_SPECS` — `gguf-renderer-detail-render.js:2490-2524` | уже согласован с `LlamaCppModelProfile` и покрыт тестами |
| Пресеты окна контекста | `CTX_PRESETS`, `CTX_MIN/MAX` — `cppworker-params.js:37-48` | те же числа, что в редакторе профиля |
| Редактор профиля + валидация + модалка прогресса | `CppWorkerParams.openWizard` / `applyProfileWithProgress` — `cppworker-params.js:743,1132`; экспорт `:1408-1417` | применение шаблона = существующий профиль+reload |
| Список «где загружена» | `Api.clusterModels.loaded()` — `api.js:760-767`; `window.CPPWORKER_LOADED_MODELS` — `cppworker-params.js:1109-1120` | единственный источник привязки модель→бэкенд |
| Архитектура «модуль — владелец состояния» | `settings-registry.js:25-29,56-58,761-810` (делегирование на контейнер, `mount/load/apply`, ожидание `window.__authReady`) | проверенный шаблон, иначе два состояния разойдутся |
| admin-only | `data-admin-only` + `auth.js:358-407` | сервер всё равно проверяет, вёрстка только подсказывает |
| i18n | `window.I18N.t(key, fallback)` — используется во всех модулях, напр. `cppworker-params.js:34,82` | — |
| Загрузка/ошибки HTTP | `Api.request/getJson` — `api.js:63-140` | таймауты и токен уже решены |

**Не** переиспользуем `settings-registry.js` для хранения самих записей: он
спроектирован под плоские `int64`-ключи реестра (`internal/settings/settings.go:57-114`),
и это единственная причина не класть записи туда.

---

## 5. Риски и открытые вопросы

### 5.1 Риски

1. **Вторая правда о значениях.** Если новая страница будет показывать «рекомендуемые»
   числа как «действующие», оператор снова получит расхождение, как с `env` vs
   `override` (история в `settings-registry.js:4-17`). Митигация: на странице всегда
   два разных визуальных статуса — «рекомендация» и «действует (профиль/env)», причём
   действующее берётся из `GET model-profiles` и `GET load-defaults` (`source`-блок).
2. **Второй путь apply.** Любая реализация «применить шаблон» своим кодом reload
   воспроизведёт уже закрытые дефекты: `numGpuLayers` вместо `gpuLayers`
   (`handlers_cppworker_profiles.go:807-816`), отсутствие `force` (`:817-826`),
   потерю токена cppworker (`:912-938`), скоуп «на все бэкенды» (`:527-534`).
   Митигация: только вызов существующих функций.
3. **Нормализация имени модели.** `.gguf`-суффикс и `NormalizeModelProfileKey`
   (`:156-182`) — иначе появится второй ключ профиля (живой дефект R136/R138,
   `profiles_persistence.go:104-117`). Новая ручка обязана нормализовать так же.
4. **`config.json` read-only в bundled-стенде** (`:388-399`) — новое хранилище
   обязано жить в `/app/data` и не пытаться писать в `config/`.
5. **Несколько экземпляров балансера.** Файловое хранилище не синхронизируется между
   инстансами (в отличие от профилей, которые хотя бы подтягиваются воркерами,
   `cmd/cppworker/sync_profile.go`). Для одного стенда это норма; для кластера
   балансеров — известное ограничение. **Не проверено**, используется ли где-то
   несколько балансеров.
6. **Конкурентная правка.** Два оператора + `revision`-блокировка → 409. Без
   revision последний писатель затирает (как сейчас в `UpdateModelProfile`,
   `model_profiles_guard_r423.go:79-96`, где защищено только одно поле).
7. **Доки как источник.** Если записи будут автоматически перечитывать markdown,
   доки станут вторым источником истины, и правка в доке молча поменяет поведение.
   Митигация: только явный однократный импорт (см. §6, вопрос 4).
8. **i18n записей.** `comment`/`title` — данные, а не ключи i18n. Русский текст в
   файле при английском UI — осознанное решение (как `notes` профиля), но его надо
   назвать вслух.
9. **Размер/шум.** Десятки записей с параметрами утяжеляют страницу; список должен
   быть ленивым (список → деталь по требованию), иначе получим «страница думает».
   **Не проверено** реальное время отклика — не мерил.
10. **`GET /api/v1/cluster/models/loaded` пуст, когда моделей нет** (живой ответ
    `count:0`) — UI обязан различать «нет загруженных» и «ошибка загрузки данных».

### 5.2 Открытые вопросы (требуют проверки/решения)

- Нужно ли показывать, ЧЕМ рекомендация отличается от действующего профиля
  автоматически (diff), или достаточно двух таблиц рядом?
- Есть ли на других стендах загруженные модели для проверки сценария «модель на двух
  бэкендах»? На текущем стенде `count:0`, поэтому ветка «применить к одному из двух
  узлов» **не проверена живьём**.
- Экспорт/импорт шаблонов между стендами (файл JSON через `GET/PUT`) — нужен или
  достаточно, что файл лежит в томе?
- Должны ли рекомендации попадать в `GET /api/v1/cppworker/model-catalog`
  (`hasProfile`/`profileIgnored`), чтобы оператор видел их в карточке модели?
  (Сейчас нет; расширение формата — отдельное решение.)

---

## 6. Решения человека (зафиксированы; контракт ими уточнён)

Ранее здесь стояли пять открытых вопросов. Человек дал ответы — они ОБЯЗАТЕЛЬНЫ
и УТОЧНЯЮТ контракт §2–§3, а не отменяют его. Всё, что ниже, реализовано;
отклонения от остального текста документа перечислены в §9.2.

### 6.1 Решение 1: `appliesTo` НЕОБЯЗАТЕЛЕН — одна схема, один вид записи

Запись-рекомендация без `appliesTo` — «общая справка» (значения с обоснованием,
годятся к чему угодно). Запись с `appliesTo` — «для класса моделей/пер-модели»
(glob по имени, тип бэкенда, числовые границы VRAM). Отдельного поля «вид
записи» и отдельной второй схемы нет: различие только в наличии `appliesTo`.
Следствия: обязательных полей у `AppliesTo` нет; сервер не использует
`appliesTo` ни для автоприменения, ни для запретов — только для подбора и
подсветки в UI (как и было в §2.3).

### 6.2 Решение 2: правка — только `admin`, третьей роли НЕТ

Мутации (`PUT`/`DELETE`/`POST`) закрыты существующим
`AuthMiddleware(LimitMiddleware(...))` + CSRF, чтение доступно `viewer`.
Третья роль вида `editor` не вводится: ролей по-прежнему две
(`internal/api/auth_session.go:504-505`), вход и все `data-admin-only` не
затронуты.

### 6.3 Решение 3: `mode:"profile+reload"` требует ЯВНЫЙ `backendId`

`POST …/templates/{id}/apply` с `mode:"profile+reload"` и пустым `backendId` →
**400 `backend_required`** с текстом «выберите бэкенд, где загружена модель:
применение на все сразу задевает чужие/недоступные узлы». Бэкенд обязан
существовать (иначе 404 `backend_not_found`). Если модель на выбранном бэкенде
НЕ загружена — это **не отказ**: ответ 200, а в массиве `backends[]` запись со
`status:"skipped"` и причиной (существующая семантика apply,
`internal/api/handlers_cppworker_profiles.go:726-742`). `mode:"profile-only"`
`backendId` не требует (и игнорирует, если он передан).

Следствие для §3.4: код **`no_loaded_targets` (503) не выдаётся** — «модель не
загружена на этом узле» теперь честный `skipped`, а не отказ. Код оставлен в
таблице §3.4 как историческая ссылка и не используется.

### 6.4 Решение 4: однократный сид из доков при ПЕРВОМ старте

Если файла хранилища нет, при старте один раз записываются рекомендации из
`docs/operations/model-tuning-reference.ru.md` (готовые профили §3.1–3.6 и
таблица ориентиров §1): `author:"docs-import"`,
`sources:[{doc:"docs/operations/model-tuning-reference.ru.md", section:"<раздел>"}]`,
`id` — слаги. Сид выполняется **только когда файла нет**; существующий файл сид
НЕ перезаписывает никогда. Автоматическое перечитывание доков при каждом старте
не делается (§5.1 п.7). `notes` профилей моделей не трогаются и никуда не
переносятся (§7 п.4 остаётся в силе).

### 6.5 Решение 5: единицы — только канонические единицы поля профиля

В `params[].value` хранятся ТОЛЬКО канонические единицы поля профиля
(токены — `contextLength`/`contextPerSeq`/`contextLengthMax`/`maxTokens`,
слои — `numGpuLayers`, байты — `sizeBytes`). `unit` — необязательная подпись для
человека (`tokens`, `layers`). `unit` НЕ участвует ни в вычислениях, ни в diff,
ни в сравнении с профилем: сравнение идёт по `name`+`value`.

---

## 7. Что НЕ надо делать

1. **Не заводить второй источник правды для значений.** Действующие значения живут
   в профилях (`config.LlamaCppModelProfiles`), а не в новом файле. Файл — справочник
   и заготовки. Никакой автоматической подстановки значений из файла в профиль при
   загрузке модели.
2. **Не делать второй apply/reload.** Применение шаблона обязано идти через
   существующие `PUT` + `POST …/apply` (со всей их защитой: `force:true`, `gpuLayers`,
   токен, `?backend=`, busy→202). Новый код перезагрузки = повторение шести уже
   закрытых дефектов (§5.1 п.2).
3. **Не хранить те же числа в JS-пресетах.** Существующие `PRESETS`
   (`app.js:612-617`), `cppworkerHint` (`setup-wizard-presets.js:35-95`) и
   `CTX_PRESETS` не должны получить пятую копию; либо они читают серверные шаблоны,
   либо остаются как есть (они про другое — глобальная секция и подсказки мастера).
4. **Не превращать `notes` профиля в хранилище рекомендаций.** Одна строка без
   структуры и версий не заменяет запись и не должна с ней сливаться «наполовину».
5. **Не писать новое состояние в `config.json`** и не пытаться писать в
   `/app/config` (read-only в bundled-стенде, `handlers_cppworker_profiles.go:388-390`).
   Отдельный файл в `/app/data` + атомарная запись.
6. **Не делать новый механизм авторизации.** Права уже даёт `AuthMiddleware`
   (`auth.go:181-259`) и `data-admin-only` (`auth.js:358-391`).
7. **Не вводить regex/сложный матчинг моделей** в `appliesTo`. Glob по имени +
   тип бэкенда + числовые границы VRAM достаточно; regex — источник необъяснимых
   «почему показали/не показали».
8. **Не делать markdown-редактор и wiki.** Записи — структурированные данные
   (`params[]`), а не свободный текст. `sources[].doc` — ссылка, не копия дока.
9. **Не добавлять новый эндпоинт «загруженные модели».** Уже есть
   `/api/v1/cluster/models/loaded` с нужной привязкой модель→бэкенд.
10. **Не дублировать границы значений.** Валидатор один —
    `validateModelProfile` (`handlers_cppworker_profiles.go:1211-1266`); UI берёт
    метаданные из `MODEL_PARAM_SPECS` (`gguf-renderer-detail-render.js:2490-2524`).
11. **Не делать прогресс применения заново.** SSE `…/apply/progress` и статус
    `…/apply/status/{applyId}` уже есть (`handlers_cppworker_apply_async.go:308-460`).

---

## 8. Итог одной таблицей: что переиспользуем и что добавляем

| Слой | Переиспользуем | Добавляем |
|---|---|---|
| Хранение значений | `LlamaCppModelProfiles` + `profiles.json` (`profiles_persistence.go`) | `model-reference.json` (рекомендации + шаблоны, атомарная запись, `LB_MODEL_REFERENCE_PATH`) |
| Валидация | `validateModelProfile` + `checkProfileMemory` | обёртка, отдающая нарушения списком (для preview) |
| Применение | `upsertModelProfile` + `applyModelProfile` (+ async/SSE) | ручка `…/templates/{id}/apply`, которая их вызывает |
| Источник «где загружена» | `GET /api/v1/cluster/models/loaded` | ничего (максимум — тонкая проекция) |
| Метаданные полей в UI | `MODEL_PARAM_SPECS` | ничего |
| Редактор и прогресс | `CppWorkerParams` (wizard, модалка) | страница-список и редактор записей |
| Права | `AuthMiddleware` (viewer=GET, admin=мутации+CSRF) | ничего |
| i18n | `I18N.t`, файлы `ru.js`/`en.js` | ключи `nav.reference`, `reference.*` |

---

**Список проверенного живьём** (стенд, `127.0.0.1:18081`): `GET /api/v1/auth/status`,
`/api/v1/cluster/models/loaded` (count 0), `/api/v1/models`, `/api/v1/gguf/backends`,
`/api/v1/models/catalog`, `/api/v1/cppworker/model-profiles` (21 запись,
`effectiveContext`, `nctxCeiling=65536`), `/api/v1/cppworker/model-catalog`,
`/api/v1/cppworker/load-defaults`. **Мутирующие вызовы (PUT/apply) на стенде
намеренно не выполнялись** — задача запрещает изменения; поведение PUT/apply
описано по коду с номерами строк. **Не проверено:** содержимое
`/app/data/profiles.json` на диске (внутрь контейнера не заходил), поведение при
непустом `/api/v1/cluster/models/loaded`, работа с несколькими балансерами,
реальное время отклика новой страницы.

---

## 9. Что реализовано (2026-11-17)

### 9.1 Файлы и ручки

**Пакет хранения — `internal/reference/`** (решение по связности: хранилище ничего
не знает про HTTP и про рантайм профилей, поэтому может импортировать только
`pkg/types` и `pkg/logger`; HTTP-слой `internal/api` вызывает его и существующие
функции профилей — так исключается цикл импортов и второй путь применения):

| Файл | Что содержит |
|---|---|
| `internal/reference/reference.go` | `Doc` (`{version, recommendations, templates}`), `Recommendation`, `Template`, `Param`, `AppliesTo`, `Source`; `SchemaVersion=1`, `FileName="model-reference.json"`, `EnvPathOverride="LB_MODEL_REFERENCE_PATH"` |
| `internal/reference/store.go` | `Store` под одним `sync.RWMutex`; `ResolvePath` (env → каталог `LB_STATE_PATH` → `data/`); `Load` (чтение при старте, сид только если файла нет, битый JSON/неизвестная версия → warn + пусто); атомарная запись tmp+rename (файл `0644`, каталог `0755`); `RevisionConflictError` |
| `internal/reference/validate.go` | `ValidateID` (`^[a-z0-9][a-z0-9._-]{2,63}$`), `KnownParamNames`/`UnknownParamNames` — reflection по json-тегам `types.LlamaCppModelProfile` (второго списка имён нет), `ParamsToObject`/`DecodeParamsObject`, `DuplicateParamName` |
| `internal/reference/seed.go` | однократный сид: 6 готовых профилей (§3.1–3.6 документа `model-tuning-reference.ru.md`) + 1 общая справка из таблицы ориентиров (§1), `author:"docs-import"`, `sources[].doc/section` |
| `internal/reference/store_test.go` | путь/env, битый JSON не фатален, неизвестная версия игнорируется, атомарность+права, рост `revision` и `createdAt` |

**HTTP-слой — `internal/api/`:**

| Файл | Что содержит |
|---|---|
| `internal/api/handlers_reference.go` | диспетчеры и CRUD рекомендаций/шаблонов, `to-template`, `prepareReferenceParams` (единственная проверка набора: имена → типы → границы `validateModelProfile`), `usage.usedByProfiles` |
| `internal/api/handlers_reference_apply.go` | `preview` (dry-run), `apply`, переиспользование `upsertModelProfile`/`applyModelProfile` через синтетический запрос + `referenceCaptureWriter`, нормализация имени модели как на границе профилей |
| `internal/api/handlers_reference_test.go` | 8 тестов из задания + тест 202-проксирования async-пути (httptest + `t.TempDir()`, без живого стенда) |
| `internal/api/handlers.go` | поле `referenceStore` + `SetReferenceStore`/`ReferenceStore` |
| `internal/api/routes.go` | 4 маршрута `/api/v1/reference/...` с `AuthMiddleware(RateLimitMiddleware(...))` |
| `cmd/balancer/main.go` | инициализация хранилища после `NewServer`: `reference.ResolvePath(conf.LoadBalancer.StatePath)` → `NewStore` → `Load()` → `SetReferenceStore` |

**Зарегистрированные ручки** (`internal/api/routes.go`):

| Метод и путь | Назначение |
|---|---|
| `GET /api/v1/reference/recommendations` | `{items,total,schemaVersion}` |
| `GET/PUT/DELETE /api/v1/reference/recommendations/{id}` | чтение / upsert (revision) / удаление |
| `POST /api/v1/reference/recommendations/{id}/to-template` | создать шаблон из записи (201) |
| `GET /api/v1/reference/templates` | `{items,total,schemaVersion}` |
| `GET/PUT/DELETE /api/v1/reference/templates/{id}` | чтение (+`usage`) / upsert (revision) / удаление |
| `POST /api/v1/reference/templates/{id}/preview` | dry-run: `diff` + `valid.violations` + `targets` + `memory`, ничего не пишет |
| `POST /api/v1/reference/templates/{id}/apply` | `mode:"profile-only"` → PUT профиля; `mode:"profile+reload"` + явный `backendId` → apply профиля (reload) |

### 9.2 Где реализация отошла от первоначального текста §2–§3

Все пункты ниже — либо следствие решений из §6, либо сознательное уточнение,
которое видно клиенту и потому перечислено здесь явно.

1. **`no_loaded_targets` (503) не выдаётся** (следствие решения №3): «модель не
   загружена на выбранном узле» — это 200 + `status:"skipped"` в `backends[]`,
   как в существующем apply. Из таблицы ошибок §3.4 код фактически выпал.
2. **Добавлены коды ошибок**, которых в §3 не было: 404 `backend_not_found`
   (явный `backendId` не зарегистрирован), 400 `backend_not_llama_cpp`,
   400 `params_required` (пустой набор), 400 `title_required`,
   400 `duplicate_param`, 400 `invalid_json` для пустого тела,
   500 `store_write_failed`/`encode_failed`, 405 `method_not_allowed`.
3. **`invalid_value` не содержит `min`/`max`.** Границы значений не
   дублируются (§7 п.10): в ответе `{"error":"invalid_value","field":…,"got":…,
   "message":…}`, где `message` — дословный текст `validateModelProfile`
   («contextLength must be in [256, 262144], got …»). Второй таблицы границ нет.
4. **`mode` обязателен** (пустой → 400 `invalid_mode` со списком `allowed`).
   Молчаливое «по умолчанию» было бы враньём: оператор увидел бы «применено»,
   а модель не перезагрузилась бы.
5. **`confirmNonFeasible` действует только на `mode:"profile+reload"`.**
   Для `profile-only` гейт памяти живёт ВНУТРИ `upsertModelProfile`
   (`handlers_cppworker_profiles.go:365-382`), и обойти его этим флагом нельзя —
   второго пути сохранения профиля не заводится (§7 п.2).
6. **`to-template` отвечает 201** (в теле `status:"created"`), занятый id — 409
   `template_id_taken`.
7. **Сид — 7 записей** (6 готовых профилей + общая справка из §1). Записи §3.3 и
   §3.6 остались без `appliesTo` (у этих классов нет одного glob'а) — это ровно
   «общая справка» из решения №1.
8. **Хранилище инициализируется из `cmd/balancer/main.go`, а не из `NewServer`.**
   Если `SetReferenceStore` не вызван, ручки отвечают 503 `store_unavailable`.
   Причина: `NewServer` вызывается в тестах `internal/api` десятки раз, и
   автоинициализация писала бы `model-reference.json` в каталог репозитория при
   каждом прогоне.
9. **`usage.usedByProfiles` — совпадение значений, а не «шаблон применён»**:
   подсказка, откуда ничего не следует про источник значений (§7 п.1).