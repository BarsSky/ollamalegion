# Совместная работа режимов при нескольких бэкендах — Placement Policy

**Статус:** P0 (R72), P1 (R74) и P1.5 (R75) РЕАЛИЗОВАНЫ; открыт P2 (`sharded`/`rpc`
на реальном транспорте — нужен выбор: llama.cpp RPC vs собственный B8.7)
**Автор:** сессия R66d
**Связанные документы:** `plans/b8-tensor-parallelism-plan.md` (TP: этапы B8.1-B8.9),
`plans/rpc-model-distribution-plan.md` (варианты B и C), `plans/README.md`,
`docs/rpc-coordinator.md`, `docs/backend-type-isolation.md`
**Связанный код:** `internal/balancer/placement.go` (R72: резолвер),
`pkg/types/placement.go` (R72: конфиг и валидация),
`internal/api/handlers_placement.go` (R72: `/api/v1/placement`),
`internal/balancer/proxy.go` (интерцепторы режимов + хук решения),
`internal/balancer/operating_modes.go`, `internal/virtualmodel`,
`internal/modelreplication`, `internal/rpccoordinator`, `internal/rpcworker`,
`internal/rptensor`, `internal/cppbackend/batched_scheduler.go`,
`internal/cppbackend/model_manager.go` (GGUF-мета), `internal/balancer/cluster_state.go` (VRAM).

---

## 1. Проблема

Сегодня режим работы балансера — **один глобальный переключатель**
`balancing.operatingMode` (`standard` / `replication` / `rpc_coordinator` /
`virtual_router` / `distributed_inference`). Он выбирается один раз на всю
установку и обслуживает **все** модели одинаково.

При этом специфичные режимы имеют смысл только там, где бэкендов **больше
одного**, и потребность в них **разная для разных моделей**:

* 2.3 ГБ модель отлично живёт на одном бэкенде — ей нужно обычное
  распределение нагрузки (`standard`);
* 15-30 ГБ модель не влезает в один бэкенд — её надо **разложить по
  нескольким** (слои/тензоры) или держать **несколько реплик**;
* популярную модель при 3 одинаковых бэкендах выгодно **реплицировать**;
* набор бэкендов может быть **неоднородным** (разные GPU, разный VRAM).

Сейчас эти сценарии взаимоисключающие: включив `virtual_router`, мы теряем
`replication`; включив `rpc_coordinator`, ломаем стандартный путь для мелких
моделей. В коде это буквально последовательность «первый подходящий
интерцептор выигрывает» (`internal/balancer/proxy.go:606` и `:622`), после
которой идёт обычный путь (`parseRequestBody → determineRequestBackendType →
selectBackend`).

**Цель:** ввести **политику размещения (placement policy)** — слой, который для
каждой модели решает, *как* её обслуживать, оставляя стандартное
распределение дефолтом и обеспечивая заявленную вами идею: «стандартная
балансировка + специфичная раскладка тяжёлой модели по нескольким бэкендам,
когда бэкендов больше одного».

---

## 2. Что уже есть (факты, не пожелания)

| Стратегия | Код | Состояние |
|---|---|---|
| `standard` (балансировка между загруженными копиями, алгоритмы resource-aware и др.) | `internal/balancer/backend_selector.go`, `candidate.go` | production, основной путь |
| `pool` (алиас → набор бэкендов, round_robin/least_loaded) | `internal/virtualmodel`, `internal/balancer/virtual_router.go` | production; проверено на стенде R66d (алиас `virtual:modec` → ответ модели, счётчик `selections`) |
| `replicated` (N копий модели на N бэкендах, группы) | `internal/modelreplication` (84.5% coverage), API `/api/v1/replication/*` | production; проверено на стенде R66d (реальный ответ, `stats.enabled=true`) |
| `rpc_coordinator` (координатор распределённого инференса) | `internal/rpccoordinator` (71.1%), `rpc_coordinator_dispatcher.go` | координатор поднимается и отдаёт метрики; **воркеров в деплое нет** — end-to-end не проверен |
| `distributed_inference` (раскладка слоёв по воркерам) | `internal/rpcworker` (`LoadSlice`, `SliceLayers` "1-40", `handlers_tp.go`), `internal/rptensor` (AllReduce/Megatron, 89%), `pkg/types` `distInference` | **scaffold**: воркер грузит модель ЦЕЛИКОМ через `bridge.LoadModel` (`internal/rpcworker/model_manager.go:85`), диапазон слоёв — только метаданные; реального транспорта активаций (NCCL/ggml-rpc) нет; `GGML_RPC`/`rpc-server` в образах не собирается; по плану B8 это этап **B8.7** (оценка 10-15 дней) |
| Параллельные последовательности в ОДНОМ процессе | `internal/cppbackend/batched_scheduler.go` | production (opt-in), дополняет, но не заменяет межбэкендную раскладку |

**Вывод:** из «разложить тяжёлую модель по нескольким бэкендам» сегодня реально
работает только **`replicated`** (копии) и **`pool`** (выбор бэкенда из пула).
Настоящая раскладка одной модели (pipeline/tensor parallel) — незакрытый этап;
для неё есть два пути (см. §7, P2) и это главный открытый вопрос.

---

## 3. Целевая модель

### 3.1 Уровни конфигурации (приоритет сверху вниз)

1. **Запрос** (опционально): заголовок/поле `X-LB-Placement: <strategy>` —
   только если включён `placement.allowRequestOverride`.
2. **Модель**: `balancing.placement.models[]` — запись на модель (или маску
   `model: "Qwen3.8-27B*"`).
3. **Группа/класс**: `balancing.placement.classes[]` — например «все модели
   > 12 ГБ → sharded», «все модели < 4 ГБ → standard».
4. **Глобальный дефолт**: `balancing.operatingMode` (как сейчас) — для моделей,
   не попавших ни в одну запись. Так сохраняется обратная совместимость:
   существующие конфиги работают без изменений.

### 3.2 Стратегии

| Стратегия | Смысл | Требует |
|---|---|---|
| `single` | модель живёт на одном бэкенде, дальше — обычная балансировка | — |
| `pool` | набор бэкендов-кандидатов (виртуальная модель / alias) | ≥1 бэкенд, список |
| `replicated` | N копий модели на N бэкендах, запрос идёт в любую | ≥N бэкендов с VRAM под копию |
| `sharded` | модель разрезана (слои/тензоры) между бэкендами | воркеры + транспорт (см. §7) |
| `rpc` | запрос уходит координатору, он решает | координатор + воркеры |
| `auto` | выбрать одну из стратегий по правилам §4 | метрики + GGUF-мета |

### 3.3 Пример конфигурации

```jsonc
"balancing": {
  "operatingMode": "standard",            // дефолт для всех, кто не описан ниже

  "placement": {
    "enabled": true,
    "allowRequestOverride": false,
    "fallback": "error",                  // "error" | "single" — что делать, если стратегия недоступна

    "classes": [
      { "match": { "sizeGB": ">12" }, "strategy": "auto" },       // крупные — auto (см. §4)
      { "match": { "sizeGB": "<4" },  "strategy": "single" }
    ],

    "models": [
      {
        "model": "gemma-4-E4B-it-Q4_K_M",
        "strategy": "single",
        "reason": "3.3B Q4_K_XL влезает в 8 ГБ VRAM, репликация не нужна"
      },
      {
        "model": "Qwen3.8-27B-UD-Q4_K_M",
        "strategy": "auto",
        "auto": {
          "prefer": ["sharded", "replicated", "single"],
          "minBackends": 2,
          "maxVramPerBackendMB": 7000,
          "requireHomogeneous": true,
          "allowDegraded": false,
          "maxShardCount": 3
        }
      },
      {
        "model": "virtual:prod-chat",
        "strategy": "pool",
        "pool": ["cpp-a:18092", "cpp-b:18092"],
        "selection": "least_loaded"
      }
    ]
  }
}
```

### 3.4 Правила композиции (что заменяет «первый интерцептор выигрывает»)

Явная цепочка стадий в `ServeHTTP`:

```
1. alias/virtual-resolution   (если имя модели → виртуальная модель/алиас)
2. placement-resolve          (запрос → модель → класс → глобальный дефолт)
3. availability-check         (модель уже загружена? где? сколько копий?)
4. strategy-execute:
     single       → существующий путь (selectBackend)
     pool         → virtual_router (как сегодня)
     replicated   → modelreplication (группы/выбор копии)
     sharded/rpc  → coordinator/dispatcher
5. fallback                   (по матрице §6; НИКОГДА не «молча в standard»,
                               если placement.fallback = "error")
```

Ключевое отличие от текущего поведения: решение **логируется с причиной и
возвращается в метриках** (см. §5), а не выводится из того, какой интерцептор
сработал первым.

---

## 4. Алгоритм `auto` (правило выбора)

Входы (всё уже есть в коде):

* мета модели: `nLayers`, `nEmbd`, `nHeads`, `nKvHeads`, `contextLength`,
  размер файла — `internal/cppbackend/model_manager.go` (`GGUFModelMeta`);
* требование по контексту: профиль модели (`LlamaCppModelProfile.ContextLength`,
  `ContextLengthAuto/Max`) и `num_ctx` из запроса;
* ресурсы бэкендов: VRAM total/free, число слотов — `internal/balancer/cluster_state.go`
  (`/api/v1/metrics`), `resource_limits`;
* текущая загрузка: `ActiveReqs`, `MaxConcurrentReqs`, длина очереди;
* однородность: список бэкендов и их VRAM/модель GPU;
* готовность воркеров: `/api/v1/rpc/workers`, `distInference.workers`.

Решающее правило (упрощённо):

```
need = weightsBytes(model) + kvBytes(nCtx) + overhead
if exists backend with freeVram >= need and strategy allowed:
        → single           (стандартное распределение; при нескольких
                            подходящих — копии загружаются по мере спроса)
elif strategy in {replicated} and count(free backends >= need) >= minBackends:
        → replicated(n = min(maxInstances, подходящие бэкенды))
elif strategy in {sharded, rpc} and workersReady and fitAcross(backends, need):
        → sharded / rpc
else:
        → error с actionable-сообщением (сколько не хватает, какие варианты есть)
```

Правило кэшируется на N секунд (как сейчас делают метрики) и пересчитывается
при изменении состава бэкендов/метрик; результат и причина — в
`cluster_state`, чтобы WebUI мог показать «почему так».

---

## 5. Наблюдаемость и UX

* `/api/v1/metrics` (и `cluster_state`): для каждой модели —
  `placement: {strategy, reason, backends[], degraded, replicas[], shards[]}`.
* Логи: одна строка на решение — как уже сделано для `virtual_router:
  selected backend` и `replication`.
* WebUI:
  * вкладка моделей: колонка «Размещение» (single / 2 реплики / слои 0-20:0-20);
  * карточка модели: где лежит, сколько VRAM ест каждый бэкенд;
  * страница «Режимы»: что активно, для каких моделей, почему (из `reason`).
* `/api/v1/models/capacity`, `/api/v1/balancer/load-backoff` (R66d) — источники
  данных для «фита» и для честных ошибок.

---

## 6. Отказоустойчивость и деградация

| Ситуация | Поведение |
|---|---|
| Модель не влезает ни в один бэкенд | явная ошибка (413/503) с числами: нужно X МБ, максимум свободно Y МБ; предложить `sharded`/меньший ctx |
| Стратегия из политики недоступна (нет воркеров / мало бэкендов) | при `fallback: error` — ошибка; при `fallback: single` — standard + WARN в логах и `degraded: true` в метриках |
| Воркер/шард упал | пометить шард unhealthy, попытаться пересобрать раскладку; при `allowDegraded: false` — отказ вместо тихой выдачи мусора |
| Один из бэкендов реплик unhealthy | `replication` исключает его из выбора (как сегодня через группы) |
| Провал загрузки копии/шарда | существующий circuit breaker (`loadBackoff`, R66d) + `state=failed` в `/api/models/load/progress` — клиент получает причину, а не «подожди 90с» |
| Запрос при частичной готовности раскладки | `Retry-After` с реальной оценкой (у нас уже есть estimated load time) |

Принцип: **никогда не выдавать 200 с неполным/чужим ответом и не уходить в
другую стратегию молча** — это прямое продолжение правок R66d про честные
ошибки загрузки.

---

## 7. Этапы работ и критерии готовности

### P0 — Resolution + observability (1-2 дня, поведение не меняется) — ✅ СДЕЛАНО (R72)
* Типы и парсинг `balancing.placement` (модели/классы/дефолт), валидация с
  понятными ошибками.
* Функция `ResolvePlacement(model) → {strategy, reason, source}`; по умолчанию
  (`placement.enabled=false`) всегда возвращает текущий `operatingMode`.
* Вывод `placement` в `/api/v1/metrics` + лог решения.
* Тесты: таблица приоритетов (запрос > модель > класс > дефолт), маски имён,
  обратная совместимость конфигов без `placement`.
* **Готово, когда:** на стенде в метриках видно стратегию и причину для каждой
  модели, а поведение маршрутизации не изменилось ни на одном существующем тесте.

**Факт по R72 (2026-09-24, `pkg/types/placement.go`,
`internal/balancer/placement.go`, `internal/api/handlers_placement.go`):**

* конфиг `balancing.placement` (models/classes/fallback/allowRequestOverride),
  стратегии `single|pool|replicated|sharded|rpc|auto`, маски имён `*`/`?`,
  выражения размера `>12` / `<=8` / `4-8`;
* `ResolvePlacement` (чистое ядро `ResolvePlacementDecision`) с приоритетом
  запрос → модель → класс → глобальный дефолт и признаком `executable`
  (`sharded`/`rpc` — этап P2);
* валидация в `ValidateConfigOnLoad` + лог предупреждений при старте
  (`NewProxy`), ошибки не блокируют запуск;
* наблюдаемость: `GET /api/v1/placement` (сводка/предупреждения/решения,
  `?model=&sizeGB=&strategy=` для отладки), заголовки `X-LB-Placement` /
  `X-LB-Placement-Source` в ответе, Debug-лог решения при включённой политике;
* вместо блока `placement` в `/api/v1/metrics` (P0-формулировка) выбран
  отдельный endpoint: `/api/v1/metrics` имеет контрактные тесты на точный
  набор полей (`tests/api_monitor_test.go`), а решение по конкретной модели
  там не помещается без ломки контракта. Требование «стратегия и причина
  видны оператору» выполнено endpoint'ом + логом + заголовками;
* счётчики решений не вводились (в P0 решение ещё не влияет на маршрутизацию) —
  они появятся в P1 вместе с исполнением стратегий;
* тесты: 20 (4 файла), регрессии `./internal/... -race`, `./cmd/...`,
  `./tests/... -short` — зелёные.

### P1 — `pool` и `replicated` из политики + `auto` для однородного случая (3-5 дней) — ✅ СДЕЛАНО (R74)
* Перенос `virtual_router` и `modelreplication` под решение политики (сейчас они
  управляются глобальным `operatingMode`).
* `auto`: single ↔ replicated по свободному VRAM и требуемому контексту.
* Тесты: 2-3 бэкенда (throwaway-стенд), «мелкая модель → single», «не влезает →
  replicated(2)», «воркеров нет → error/degraded по конфигу».
* **Готово, когда:** `operatingMode=standard` больше не мешает репликации
  конкретной модели, а счётчики `selections`/групп видны в метриках.

**Факт по R74 (2026-09-24, коммит `R74: placement policy P1`, образ
`r74-submodule-v12`):**

* `pool`: перехват `VirtualRouter` включается политикой для конкретной модели
  (`strategy=pool`, в т.ч. через `auto` для алиаса) — `operatingMode` остаётся
  `standard`. Добавлен `MatchVirtualRequest` (возвращает имя модели); роутер
  создаётся и при `virtualModels.enabled=true` (`cmd/balancer/main.go`). Живая
  проверка: алиас `virtual:r74pool` уходит в пул, на бэкенды доезжает физическое
  имя `r74-physical`, ответы по кругу A/B/A.
* `replicated`: группа создаётся политикой (идемпотентно, при старте и при
  `SetPlacementSettings`), менеджер репликации поднимается по требованию;
  далее работает штатный `replicationSelector`. Живая проверка: `r74-repl`
  обслуживают реплики A/B/A, `X-LB-Placement: replicated`.
* `auto`: детерминированное подмножество §4 — алиас → pool; `prefer` содержит
  replicated и здоровых бэкендов ≥ `minBackends` → replicated; иначе → single.
  (VRAM-fit добавлен в R75/P1.5 — см. ниже.)
* Наблюдаемость: блок `replication` (`managerReady`, `hasGroup`, `candidates`)
  в `GET /api/v1/placement`; заголовки `X-LB-Placement*` отражают исполненную
  стратегию.

### P1.5 — `auto` по свободному VRAM и размеру модели — ✅ СДЕЛАНО (R75)

**Факт по R75 (2026-09-24, коммит `R75: placement P1.5`, образ
`r75-submodule-v13`):**

* `placementFitForModel` (`internal/balancer/placement_fit_r75.go`):
  `need ≈ размер модели × 1.25` (KV и накладные), проверка каждого healthy
  бэкенда — «загружена» (влезает по определению) / «метрики VRAM известны и
  `memoryFree ≥ need`» (МБ → байты) / «метрик нет» (CPU-only, ollama без агента →
  неизвестно, выбор не блокируется);
* `auto` перебирает `auto.prefer` (по умолчанию `[single]`): single →
  replicated → (sharded/rpc помечаются как пропущенные, этап P2); при провале —
  `single` + `degraded: true` и числа в причине (`need≈…, свободно максимум …`);
  при неизвестном размере — single с пометкой «VRAM-fit не проверен»;
* группы репликации создаются и для `auto`-правил с `prefer`, содержащим
  replicated (`minInstances ≥ 2`);
* живая проверка (2 заглушки, профили 4/8/30 ГБ): без метрик VRAM —
  `single` / `replicated` / `replicated`; после сообщения 512 МБ свободного VRAM
  — все три `single` с `degraded: true` и точными числами (см. CHANGELOG 0.5.33);
* тесты: `placement_auto_r75_test.go` (6);
* осталось: `requireHomogeneous` (однородность GPU) и `fallback=error`
  (жёсткий отказ вместо degraded) — вынесены в P3 (отказоустойчивость и UX).

### P2 — `sharded` на реальном транспорте (1-2 недели, главный открытый вопрос)
Два варианта, нужно выбрать:
* **(A) llama.cpp RPC**: включить `GGML_RPC`/`rpc-server` в сборке образов,
  cppworker как RPC-клиент (`--rpc host:port`), воркеры — `rpc-server` на
  бэкендах. Плюс: настоящая раскладка слоёв «из коробки», поддержка upstream.
  Минус: зависимость от upstream-сборки и сетевой чувствительности.
* **(B) Собственный путь B8.7** (`rptensor` + `rpcworker`): реализовать реальную
  загрузку слоёв в `rpcworker` (`LoadSlice` сейчас грузит модель целиком —
  `internal/rpcworker/model_manager.go:85`), транспорт активаций, allreduce.
  Плюс: контроль над протоколом и метриками. Минус: 10-15 дней по плану B8 и
  необходимость NCCL/иного транспорта.
* Критерии готовности обоих: 27B-модель обслуживается двумя бэкендами (по
  ~7 ГБ VRAM каждый), `/api/v1/metrics` показывает шарды, инференс отвечает,
  падение одного воркера даёт явную ошибку.

### P3 — Отказоустойчивость и UX (≈1 неделя) — 🟡 ЧАСТИЧНО (R76)
* Матрица §6 целиком, включая `degraded` в метриках и WebUI-колонку «Размещение».
* Автопересборка раскладки при изменении состава бэкендов.

**Факт по R76/R77 (2026-09-24):**

* ✅ §6 «не уходить в другую стратегию молча»: `fallback=error` (default) →
  503 с причиной и блоком `placement`, отказ до маршрутизации; `fallback=single`
  → обслуживание обычным путём + `X-LB-Placement-Fallback` с заявленной
  стратегией; `auto.allowDegraded=true` разрешает деградацию;
* ✅ `auto.requireHomogeneous`: набор бэкендов сужается до группы с одинаковой
  «личностью» GPU (метка `gpu:*`/`sm_*`, иначе объём VRAM), ограничение видно в
  `reason`;
* ✅ `degraded` отдаётся в решении (`/api/v1/placement`, R75), в логе WARN и в
  карточке WebUI (R77);
* ✅ WebUI-колонка «Размещение» (R77): панель `panelPlacement` на `/monitor` —
  статистика политики, таблица решений (модель · стратегия · источник ·
  fallback · причина), пометки `деградация`/`не исполняется (P2)`/`auto`, группы
  репликации с кандидатами и предупреждения валидации конфига
  (образ `webui:r77-submodule-v2`);
* ⏳ осталось: автопересборка раскладки при изменении состава бэкендов (сейчас
  группы реконсилит `GroupController` каждые 10 с) и `degraded` в общих метриках
  (`/api/v1/metrics`).

### P4 — Нагрузочные KPI (2-3 дня)
* Стенд из 2-3 бэкендов, метрики: tok/s на single vs replicated vs sharded,
  потребление VRAM, время failover, поведение под 4 параллельными запросами
  (тут же пригодится `batched_scheduler`).

---

## 8. Тест-план

* **Юнит:** resolution (приоритеты и маски), fit-калькуляция (по GGUF-мете и
  VRAM), матрица fallback — без сети, на существующих хелперах
  (`GGUFModelMeta`, `calculateResourceLimits`).
* **Интеграция (Go):** `tests/` — 2-3 mock-бэкенда, проверка выбора стратегии,
  репликации и отказа шарда.
* **Стенд (throwaway):** как в R66d (`_diag/modes_verify.ps1`): balancer +
  `cppworker-gpu` + 1-2 контейнера `rpcworker`; проверяется реальный инференс,
  `selections`/группы/шарды и явные ошибки при нехватке ресурсов.
* **Регресс:** существующие тесты режимов (`tests/operating_mode_dispatch_test.go`,
  `tests/scenarios_*`) обязаны остаться зелёными при `placement.enabled=false`.

---

## 9. Открытые вопросы (нужны решения)

1. **Транспорт раскладки:** llama.cpp RPC (A) или собственный B8.7 (B)? Это
   определяет P2 и всё, что после.
2. Однородность: считаем ли раскладку допустимой только между одинаковыми GPU
   (`requireHomogeneous`), или разрешаем «слабый хвост»?
3. Дефолт для новых установок с 2+ бэкендами: `auto` или `standard`?
4. Нужен ли per-request override (`X-LB-Placement`) для отладки?
5. Что делать при неполном наборе воркеров: очередь до готовности, деградация на
   `single` (медленно, но работает) или отказ?

---

## 10. Приложение: соответствие текущим режимам

| `operatingMode` | Эквивалент в placement policy |
|---|---|
| `standard` | дефолт: `strategy: single` для всех моделей |
| `virtual_router` | `strategy: pool` (модели с `pool`) + `single` для остальных |
| `replication` | `strategy: replicated` для выбранных моделей + `single` для остальных |
| `rpc_coordinator` | `strategy: rpc` для моделей, помеченных как распределяемые |
| `distributed_inference` | `strategy: sharded` (тот же транспорт, что в P2) |

То есть **новая модель является надмножеством текущей**: каждый существующий
режим выражается политикой, но перестаёт быть глобальным ограничением. Это
позволяет внедрять её по шагам (P0 не меняет поведение) и не ломать работающие
установки.
