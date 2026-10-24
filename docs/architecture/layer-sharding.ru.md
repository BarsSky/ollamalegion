# Раскладка слоёв по нескольким бэкендам (llama.cpp RPC layer split)

> **Версия:** 1.0 (2026-10-13)
> **Относится к:** cppworker + C-мост `c/bridge`, образ `ollama-legion/cppworker:gpu-*`
> **Связанные документы:** [`rpc-coordinator.md`](rpc-coordinator.ru.md) (история и живые пробы),
> [`deployment-stack.md`](../operations/deployment-stack.ru.md), [`ENV_VARS.md`](../api/env-vars.ru.md),
> [`cppworker-model-params.md`](../features/cppworker-model-params.ru.md)

Этот документ отвечает на четыре вопроса:

1. [На каком уровне это реализовано — нужен ли ещё один контейнер?](#1-на-каком-уровне-это-реализовано)
2. [Это отдельный режим или свойство бэкенда?](#2-это-режим-или-плоскость-бэкенда)
3. [Как это работает внутри (цепочка от ENV до слоёв)](#3-как-это-работает-внутри)
4. [Что нужно, как запустить, какие параметры, как проверить](#4-что-нужно-запустить-и-проверить)

---

## 1. На каком уровне это реализовано

**Раскладку делает САМ cppworker, который грузит модель.** Отдельного образа и
отдельного контейнера «для раскладки» на машине-мастере не существует и не
требуется: это тот же процесс, который обычно обслуживает `/api/chat` и
`/api/generate`, просто с подключёнными удалёнными ggml-устройствами.

```
Машина-мастер (например 192.0.2.20)          Машина-донор (например 192.0.2.10)
┌───────────────────────────────────────┐      ┌──────────────────────────────┐
│ контейнер ol-stack-cppworker-gpu      │      │ контейнер ol-rpc-server      │
│  ./cppworker                          │      │  /app/rpc-server -H 0.0.0.0  │
│   ├─ C-мост c/bridge                  │      │  -p 50052                    │
│   │   └─ ggml_backend_rpc_add_server ─┼─────►│  (отдаёт свои GPU как        │
│   ├─ llama.cpp (этот же процесс)      │ TCP  │   ggml-устройство RPC0)      │
│   │   └─ llama_model_load_from_file   │50052 └──────────────────────────────┘
│   │       devices=[CUDA0, RPC0]       │
│   │       split_mode=LAYER            │
│   └─ HTTP :18091/:18092 (Ollama API)  │
└───────────────────────────────────────┘
```

Итого:

| Где | Что нужно | Обязательно? |
|---|---|---|
| Мастер (тот, кто грузит модель) | **ничего дополнительно** — уже работающий контейнер cppworker + два ENV | — |
| Донор (чья GPU используется) | контейнер **`rpc-server`** из того же образа cppworker | да, если нужна чужая GPU |
| Локальная проба без второй машины | тот же сайдкар рядом с мастером | только для функциональной проверки |

Почему сайдкар, а не «просто открыть порт»: llama.cpp умеет использовать чужую
GPU **только** через свой протокол `rpc-server` (`ggml_backend_rpc_*`). Ни
CUDA, ни NCCL, ни docker-«GPU sharing» здесь не участвуют. Поэтому на каждой
донорской машине поднимается именно `rpc-server` — бинарь лежит в том же образе
cppworker, чтобы обе стороны протокола были из одного коммита llama.cpp
(протокол чувствителен к версии).

Запуск сайдкара: [`deployments/docker-compose.rpc-node.yml`](../../deployments/docker-compose.rpc-node.yml).

---

## 2. Это режим или плоскость бэкенда?

**Плоскость бэкенда.** Раскладка — свойство конкретного cppworker'а
(«какие устройства я вижу и как по ним раскладываю слои»), а не режима работы
балансера. Балансер о раскладке ничего не знает: он видит один обычный
llama.cpp-бэкенд, который умеет больше, чем помещается в одну карту.

Отсюда практические следствия:

* `operatingMode = rpc_coordinator` (режим B) для раскладки **не нужен**. Это
  другая функция — координация внешних воркеров с распределённой моделью
  (см. [`rpc-coordinator.md`](rpc-coordinator.ru.md)); включать его ради layer split
  — распространённая ошибка.
* `balancing.rpcCoordinator.enabled`, группы раскладки и
  `POST /api/v1/placement/validate` — про планирование размещения, а не про
  транспорт `rpc-server`.
* Переключение `operatingMode` раскладку **не включает и не выключает**
  (с R112 режим вообще не управляет типами бэкендов и движком: см.
  [`backend-type-isolation.md`](backend-type-isolation.ru.md)).

---

## 3. Как это работает внутри

Цепочка «ENV → устройства → слои»:

| # | Шаг | Где | Что происходит |
|---|---|---|---|
| 1 | ENV | `cmd/cppworker/main.go:302-327` | `CPPWORKER_RPC_ENABLE`, `CPPWORKER_RPC_SERVERS`, `CPPWORKER_SPLIT_MODE`, `CPPWORKER_TENSOR_SPLIT`, `CPPWORKER_MAIN_GPU` попадают в `ModelConfig` |
| 2 | Подключение узлов | `c/bridge/bridge.c` → `bridge_collect_rpc_devices` | на каждый `host:port` вызывается `ggml_backend_rpc_add_server`; устройство кэшируется по endpoint (повторная загрузка не переподключается) |
| 3 | Сбор устройств | `bridge_collect_devices` | один список: `[CUDA0, CUDA1, …, RPC0, RPC1, …]` (`is_remote` = 1 у RPC) |
| 4 | Планирование | `internal/cppbackend/gpu_distribution.go` → `CalculateTensorSplitWithRPC` | пропорции по **свободной** памяти всех устройств (min 5 % на устройство), `main_gpu = 0` |
| 5 | Передача в llama.cpp | `internal/cppbackend/backend.go` → `bridge_load_model` | `llama_model_params.devices` = локальные (кроме CPU) + RPC; `split_mode = LAYER` |
| 6 | Раскладка | внутри llama.cpp | слои режутся по `tensor_split`; ожидаемый лог — `llama_prepare_model_devices: using device …` × N |

Живой лог успешной раскладки (gemma-4-E4B, `gpuLayers=42`, ctx=4096, kv=q4_0,
два «устройства» — локальная RTX 3070 и сайдкар):

```
[bridge] RPC: узел host.docker.internal:50052 подключён, его устройств: 1
[bridge] RPC: устройств в раскладке 2 (локальные + удалённые)
tensor split calculated (local + RPC): localGPUs=1, rpcGPUs=1, ratios=[0.536, 0.464]
llama_prepare_model_devices: using device CUDA0 (NVIDIA GeForce RTX 3070) - 7097 MiB free
llama_prepare_model_devices: using device RPC0 (host.docker.internal:50052) - 7097 MiB free
```

| Устройство | Слоёв |
|---|---|
| CUDA0 (локальная RTX 3070) | 23 |
| RPC0 (узел) | 19 |
| CPU | 1 |

Планировщики вместимости тоже видят удалённую память: `MemfitBudget: ёмкость
RPC-узлов добавлена в бюджет`, `EnvironmentProfile: ёмкость RPC-узлов
добавлена`, снимок устройств кэшируется на `rpcDeviceTTL = 30s`
(`internal/cppbackend/memfit_adapter.go`).

---

## 4. Что нужно, запустить и проверить

### 4.1 Что нужно

* **Образ cppworker, собранный с RPC** (`GGML_RPC=ON`). Обычный CUDA-образ без
  RPC соберётся, но `rpc-server` в нём не будет; признак — `docker run --rm
  <image> ls /app/rpc-server`.
* **Одинаковый тег образа на мастере и доноре.** Протокол RPC чувствителен к
  коммиту llama.cpp; разные теги = `version_mismatch` (это показывает сухой
  прогон `POST /api/v1/placement/validate`).
* **Сеть мастер → донор, TCP 50052** (на доноре порт публикует compose, фаервол
  должен его пропускать). Проверка с мастера:
  `powershell -File scripts\stand-ports.ps1 -Action Probe -Target <ip> -Ports 50052`
  → ожидаем `CONNECTED`.
* **Модель, которая НЕ влезает в локальную карту целиком.** Иначе раскладывать
  нечего (см. грабли 4.4).

### 4.2 Запуск на доноре

```powershell
# на машине-доноре, из каталога deployments
printf 'CPPWORKER_GPU_TAG=rpc86-retry-r111\nRPC_PORT=50052\n' > .env.rpc
docker compose -f docker-compose.rpc-node.yml --env-file .env.rpc up -d
docker logs ol-rpc-server --tail 20   # ожидаем строку о listening на 0.0.0.0:50052
```

### 4.3 Запуск на мастере

В `deployments/.env` (значения — пример):

```dotenv
CPPWORKER_RPC_ENABLE=true
CPPWORKER_RPC_SERVERS=192.0.2.10:50052      # csv: host:port[,host:port]
CPPWORKER_SPLIT_MODE=1                          # 1 = LAYER (раскладка слоями)
CPPWORKER_MAIN_GPU=0
CPPWORKER_GPU_LAYERS=99                         # не меньше, чем всего слоёв модели
# CPPWORKER_TENSOR_SPLIT=0.6,0.4                # обычно НЕ нужен: считается по свободной VRAM
```

```powershell
cd deployments
docker compose -f docker-compose.stack.yml up -d cppworker-gpu
docker logs -f ol-stack-cppworker-gpu   # ищем "RPC: узел … подключён" и "using device RPC0"
```

### 4.4 Параметры

| Переменная | Значение | Смысл |
|---|---|---|
| `CPPWORKER_RPC_ENABLE` | `true` / `false` | Включает RPC-транспорт в мосте. Без него `CPPWORKER_RPC_SERVERS` игнорируется |
| `CPPWORKER_RPC_SERVERS` | `host:port[,host:port]` | Узлы-доноры. Каждый = отдельное ggml-устройство `RPC0`, `RPC1`, … |
| `CPPWORKER_SPLIT_MODE` | `-1` auto / `0` none / `1` layer / `2` row | Для раскладки по устройствам нужен `1` (layer). `0` = всё на одно устройство |
| `CPPWORKER_MAIN_GPU` | `0` (индекс среди `devices`) | Основное устройство; при `devices=[CUDA0, RPC0]` это локальная карта |
| `CPPWORKER_TENSOR_SPLIT` | `0.6,0.4` | Явные пропорции. Пусто = расчёт по свободной VRAM всех устройств (min 5 %) |
| `CPPWORKER_GPU_LAYERS` | число | Сколько слоёв отдать GPU. **Должно превышать то, что влезает локально** — иначе memfit урежет до локального объёма и делить будет нечего |
| `CPPWORKER_AUTO_OFFLOAD` | `true` / `false` | Другой переключатель (авто-оффлоад), на раскладку не влияет |
| `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD` | `true` / `false` | Подгонка контекста под память; считает ёмкость с учётом RPC, но может уменьшить n_ctx |
| `RPC_PORT` (донор) | `50052` | Публикуемый порт сайдкара |

Порт `50052` — не контракт, а значение по умолчанию в compose и правилах
фаервола стенда; менять нужно **парой** (compose донора и `CPPWORKER_RPC_SERVERS`).

### 4.5 Проверка, что раскладка действительно произошла

```powershell
# 1. функциональная проба целиком (поднимает модель и печатает раскладку)
powershell -File scripts\rpc-shard-load.ps1 -Model gemma-4-E4B-it-Q4_K_M.gguf
```

Признаки успеха — одновременно все четыре:

1. `[bridge] RPC: узел <host:port> подключён, его устройств: N`;
2. `llama_prepare_model_devices: using device RPC0 …` (устройство реально
   участвует, а не только подключено);
3. `tensor split calculated (local + RPC): … ratios=[…]` — пропорции с ненулевой
   долей каждого устройства;
4. в таблице слоёв (`CUDA0 / RPC0 / CPU`) у RPC-устройства **ненулевое** число
   слоёв.

Если `RPC0` подключён, но слоёв у него 0 — смотрите грабли ниже.

### 4.6 Грабли (все проверены на живом стенде)

| Симптом | Причина | Что делать |
|---|---|---|
| `using device CUDA0` и всё, хотя RPC-узел подключён | модель (или урезанный `gpu_layers`) влезает в локальную карту: `checkVRAMForModel`/`calculateLazyLoadOpts` уменьшили `gpu_layers` до локального объёма | поднять `CPPWORKER_GPU_LAYERS`, взять модель крупнее, проверить `AUTO_TUNE_NCTX_ON_LOAD` |
| Раньше: раскладки нет «в принципе» | `bridge.ModelConfig.SplitMode` не заполнялся (режим `NONE`), llama.cpp видел одно устройство | исправлено (`cfg.SplitMode = opts.SplitMode`), защищено тестом `rpc_split_mode_wiring_r108_test.go` |
| Раньше: `panic: cgo argument has Go pointer to unpinned Go pointer` при загрузке | массив `tensor_split` не был закреплён перед передачей в C | исправлено (`runtime.Pinner`), тест `TestR108_TensorSplitArrayIsPinned` |
| **Загрузка отменяется с текстом «RPC-узел раскладки … недоступен»** | R114: гейт перед загрузкой не нашёл TCP-соединение с узлом | это штатное поведение (лучше, чем `SIGABRT` ниже). Поднять узел: на доноре `docker compose -f docker-compose.rpc-node.yml up -d`, проверить `scripts/stand-ports.ps1 -Action Probe -Target <ip> -Ports 50052` |
| **В логе «RPC-узел потерян: модель будет выгружена в простое»** | R114: узел пропал ПОСЛЕ загрузки; watchdog выгружает модель, чтобы следующий запрос не уронил мастер | узел вернулся → следующий запрос загрузит модель заново (гейт проверит узлы). Ничего делать не нужно |
| Мастер падает с `SIGABRT: abort` + `signal arrived during cgo execution` | узел пересоздали/убили **под живой моделью**: обрыв протокола RPC в llama.cpp = `abort()`; abort приходит в момент обрыва сокета, ДО тика watchdog (проверено: контейнер перезапускается) | сначала выгрузить модель (`keep_alive=0`, `/api/models/unload`), потом трогать узел. R114 снимает соседний класс падений (см. ниже) |
| Сайдкар в `docker ps` показывает `unhealthy` при рабочем RPC | унаследованный HEALTHCHECK образа проверяет HTTP 18092 | в compose донора `healthcheck: disable: true` (уже стоит) |
| Раскладка «работает», но быстрее не стало | проба на **локальном** сайдкаре: оба «устройства» — одна и та же карта, это функциональная проверка | для приёмки §6 нужен реальный донор и замер TTFT/tok/s |
| **Стенд ругается `version_unknown` на узлах группы** | узел не сообщил версию llama.cpp (`buildInfo` пуст) | R114: версия печётся в образ (`ARG LLAMA_BUILD`) и уходит в регистрацию; в `deployments/.env` держите `CPPWORKER_LLAMA_BUILD` = `git -C c/llama.cpp rev-parse --short HEAD`. Разные значения у мастера и донора → `version_mismatch` (LevelError) в `POST /api/v1/placement/validate` |

### 4.7 Что осталось

* **Приёмка §6 (производительность):** донор `192.0.2.10` в сети, три числа
  на одной модели — `single` (локальная карта), `hybrid CPU+GPU`, `network layer
  split` по TTFT и tok/s. Автоматизировано в `scripts/rpc-shard-load.ps1`.
* **`nctx_feasibility`:** `max_vram_n_ctx=0` в отчёте — ёмкость RPC учитывается в
  бюджете, но не в этом расчёте.
* **Окно между проверкой и подключением.** R114 убирает два сценария отказа
  (узел недоступен до загрузки — гейт; узел пропал после загрузки — watchdog), но
  если узел падает РОВНО в момент подключения внутри `bridge_load_model`, `abort()`
  по-прежнему приходит из llama.cpp: обработать его в C-мосте нельзя, потому что
  это не ошибка, а `abort()`. Практический вывод: не трогать узел под живой
  моделью.


## Грабли: встроенный узел (R123) против отдельного контейнера (R170, 2026-10-17)

Живой разбор после обновления llama.cpp (пин `0eadefeb`, qwen4exp). Симптом:

```
sched_reserve: RPC0[192.0.2.10:50052] compute buffer size = 542.79 MiB
ggml-rpc.cpp:566: Remote RPC server crashed or returned malformed response
Aborted
```

Проверено по шагам (загрузка с `gpuLayers=10`, `ctx=4096`, `kv=q4_0`):

| Конфигурация | Загрузка | Чат |
|---|---|---|
| встроенный узел, клиент и узел на ОДНОЙ машине | ок | **200 за 2.2 с** |
| отдельный контейнер `rpc-server` на другой машине | ок | **200 за 4.4 с** |
| отдельный `rpc-server` с env-файлом воркера | ок | **200 за 5.0 с** |
| **встроенный** узел на другой машине | ок | падение клиента (3/3) |

Вывод: llama.cpp и сеть исправны; ломается встроенный узел (дочерний процесс воркера,
R123), когда клиент находится на другой машине. Узел при этом здоров: слушает порт,
`RestartCount=0`, в логе только `Accepted client connection / Client connection closed`.

**Рабочий рецепт (проверен живьём)**

```powershell
# на узле-доноре: отдельный контейнер, не встроенный узел
docker run -d --name rpc-node --runtime nvidia --entrypoint /app/rpc-server `
  -p 50055:50055 <образ cppworker> -H 0.0.0.0 -p 50055

# на мастере: включить раскладку на этот узел
PUT /api/v1/cppworker/config/update {"rpcEnabled":true,"rpcServers":["192.0.2.10:50055"]}

# загрузка с ограниченным числом слоёв на GPU (иначе модель не влезет в 2×8 ГБ)
POST /api/models/load-with-params {"name":"…","contextSize":4096,"gpuLayers":10,"kvCacheType":"q4_0"}
```

Проверка факта — `device_layers_exact` в `/api/models`: там должно появиться устройство
`RPC0/RPC1` с адресом узла.

**Почему встроенный узел всё же нужен и что с ним делать.** Он удобнее (не нужен второй
контейнер, порт публикует compose, балансер сам даёт приказ — R133/R164), но при
удалённом клиенте сейчас непригоден. Флаги у него те же (`-H … -p …`), что у standalone,
поэтому искать надо не в аргументах, а в контексте процесса: cgroup/PID-namespace,
унаследованный CUDA-контекст родителя, дескрипторы. До исправления для раскладки между
машинами используйте standalone-узел.


## Управление внешним узлом (R171, 2026-10-17)

Рабочий рецепт из раздела выше («standalone-узел») теперь управляется штатно, а не руками.

### Что где включается

| Где | Что | Зачем |
|---|---|---|
| узел (донор) | `docker compose --profile rpc-node up -d rpc-node` | отдельный контейнер-узел, порт `${RPC_NODE_PORT:-50055}` |
| env воркера узла | `CPPWORKER_RPC_EXTERNAL_ADDR=<host>:${RPC_NODE_PORT:-50055}` | воркер не порождает свой процесс, а проверяет внешний |
| балансер | роль бэкенда `donor` | приказ «узел должен работать» (R133/R164) |
| мастер | `PUT /api/v1/cppworker/config/update {"rpcEnabled":true,"rpcServers":["<host>:<port>"]}` | включить раскладку на этот узел |

Пример обновления узла одной командой (образ + env за один проход):

```powershell
scripts/upgrade-donor-node.ps1 -NewImage ollama-legion/cppworker:gpu-r171-external-node `
  -TransferImage `
  -ExtraEnv 'CPPWORKER_RPC_EXTERNAL_ADDR=192.0.2.10:50055'
```

### Что видно в статусе

`/api/models` на узле:

```json
"rpc_server": {"enabled":true,"running":true,"listening":true,
               "external":true,"addr":"192.0.2.10:50055",
               "notes":["внешний узел доступен (192.0.2.10:50055)"]}
```

`GET /api/v1/backends` на балансере:

```json
"rpcNode": {"role":"donor","wanted":true,"enabled":true,"running":true,"listening":true,
            "addr":"192.0.2.10:50055","port":50055,"reachable":true}
```

Правило, которое здесь зашито: **адрес из отчёта воркера главнее догадки балансера**.
До R171 балансер проверял `host:50052` (свой `rpcNodePort`) и писал `reachable=false` при
работающем узле на 50055, а мастеру уходил неверный список `rpcServers`.

### Границы

* Воркер **не запускает** чужой контейнер: у него нет docker-сокета, и давать его —
  не тот обмен. `Stop()` в режиме external только снимает желание и пишет в лог, что
  гасить может оператор.
* Порт публикует контейнер-узел, а не балансер (свойство хоста, как и в R118).
* Протокол RPC чувствителен к коммиту llama.cpp: узел и мастер обязаны быть из одной
  сборки. Проверка — `llama_build`/`llama_build_env`/`llama_build_mismatch` в `/api/info`
  (R167): расхождение теперь видно, а не растворяется в «версии вроде совпали».

### Живая проверка (2026-10-17)

Роль `donor` → приказ балансера → воркер отчитался `external=true`,
`addr=192.0.2.10:50055`; балансер — `reachable=true`; мастер загрузил модель с
раскладкой (`CPU:33 CUDA0:6 RPC0:4` — 4 слоя на донорской карте) и **ответил 200 за 5.83 с
без падения** (`RestartCount=0`).


## Диагностика узла: exit-статус, лог, отладочный режим, автоперезапуск (R172, 2026-10-18)

### Почему понадобилось: сообщение клиента врёт

На аварии стенда мастер писал:

```
ggml-rpc.cpp:566: Remote RPC server crashed or returned malformed response
Aborted
```

Это **клиентское** сообщение, и оно покрывает два разных диагноза. В `ggml-rpc.cpp:35`
макрос `RPC_STATUS_ASSERT` поднимает его на **любой** ответ сервера со статусом false, а
сервер закрывает сессию на первой же неудачной операции (`if (!server.op(...)) return;`,
`ggml-rpc.cpp:1857+`). То есть одинаково выглядят:

| Что случилось на самом деле | Что видит оператор |
|---|---|
| сервер разбился (segfault / OOM / abort) | `Remote RPC server crashed…` |
| операция не удалась: регион тензора вне буфера, неизвестный тип, отказ аллокации | `Remote RPC server crashed…` |

Настоящую причину печатает **сервер** — через `GGML_LOG_ERROR` в stderr. До R172 этот
вывод уходил только в `docker logs` контейнера, поэтому: (а) при обновлении образа
контейнер пересоздаётся и лог теряется, (б) на хосте без доступа никто его не читает.

### Что теперь есть

| Что | Где | Зачем |
|---|---|---|
| exit-статус прошлого процесса (`lastExit`, `lastExitPid`, `lastExitAt`) | статус узла (`/api/models` → `rpc_server`, `/api/v1/cppworker/config` → `rpcServer`) | отличает «сервер разбился» от «сервер ответил false» |
| последние строки вывода узла (`logTail` в статусе; полный буфер — в ручке) | `GET /api/v1/cppworker/rpc-server/log?lines=200` | серверная причина отказа, с временем каждой строки |
| `logLines` / `logDropped` | там же | «лог обрезан» — факт, а не тишина |
| отладочный режим протокола | `CPPWORKER_RPC_SERVER_DEBUG=1` | `GGML_RPC_DEBUG=1` в дочернем процессе: построчный лог каждой операции |
| автоперезапуск | `CPPWORKER_RPC_SERVER_RESTART` (по умолчанию **включён**) | паритет с `restart: unless-stopped` отдельного контейнера |
| `restarting` / `restartInMs` | статус узла | «узел лежит» ≠ «узел поднимается через 1.5 с» |

### Как включить встроенный узел (без второго контейнера)

В bundled-compose настройки узла теперь пробрасываются из `deployments/.env` (до R172 их
там не было вовсе, поэтому включить узел из `.env` было нельзя):

```dotenv
# deployments/.env — узел раскладки внутри этого же воркера
CPPWORKER_RPC_SERVER_ENABLED=true
CPPWORKER_RPC_SERVER_PORT=50052      # должен совпадать с публикацией порта в compose
# CPPWORKER_RPC_SERVER_DEBUG=true    # временно, для разбора: построчный лог протокола
# CPPWORKER_RPC_SERVER_RESTART=0     # выключить автоперезапуск на время разбора
```

Порт публикуется строкой `"${CPPWORKER_RPC_SERVER_PORT:-50052}:${CPPWORKER_RPC_SERVER_PORT:-50052}"`
— она стоит всегда, но без включённого узла за ней просто нет процесса (порт слушает
docker-proxy, а не `rpc-server`: это не признак работающего узла, истина — `running`/
`listening` в статусе).

Пример:

```powershell
curl.exe -s -H "X-API-Token: $token" "http://192.0.2.10:18092/api/v1/cppworker/rpc-server/log?lines=50"
```
```json
{"enabled":true,"running":false,"external":false,"pid":0,"addr":"0.0.0.0:50052",
 "lastExit":"signal: segmentation fault (core dumped)","lastExitPid":4242,
 "lastExitAt":"2026-10-18T07:41:52Z","restartEnabled":true,"restarting":true,
 "restartInMs":1500,"debug":false,"logLines":812,"logDropped":0,
 "lines":[{"at":"2026-10-18T07:41:51.9Z","text":"Accepted client connection"},
          {"at":"2026-10-18T07:41:52.1Z","text":"ggml-rpc: [server] tensor region out of buffer bounds"}]}
```

Как читать результат:

* `lastExit` = `signal: …` → процесс **умер**; смотреть сигнал (`segmentation fault` —
  падение в ядре/бэкенде, `killed` — OOM-киллер, `abort` — `GGML_ABORT`);
* `lastExit` = `exit status N` → процесс завершился сам (`rpc-server` выходит, если не смог
  создать сокет/bind);
* `lastExit` пуст, а клиент всё равно упал → сервер **жив**, операция вернула `false`;
  причина в `lines` (регион тензора вне буфера = рассинхрон протокола/несовместимая сборка;
  отказ аллокации = не хватило VRAM).

### Границы и что честно неизвестно

* Захват выводит дублирование в stdout контейнера, поэтому `docker logs` работает как
  раньше; буфер — добавка, а не замена.
* Хвост лога попадает в **статус** только когда есть на что смотреть (узел лежит,
  уже падал, или включена отладка) — иначе каждый опрос балансера тащил бы лог.
* Автоперезапуск ограничен пятью в час с растущей паузой (0.5 → 1 → 2 → 4 с, максимум 30 с):
  цикл бесконечных падений на 8-гигабайтной карте сам становится причиной отказов.
  Причина при этом не прячется — `lastExit` и `logTail` остаются в статусе.
* `GGML_RPC_DEBUG` объёмный: на загрузке модели это десятки тысяч строк, поэтому по
  умолчанию выключен.
* Сам по себе перезапуск **не лечит** сбой: если операция падает из-за нехватки VRAM,
  узел поднимется и упадёт снова — бюджет это и ограничивает.


## Диагностика раскладки после reload (R176-R179, 2026-10-19)

**Что было.** «Применить профиль» из WebUI перезагружал модель **без узла**: слои
уезжали на локальную карту. Настоящая причина — reload не передавал `SplitMode`,
поэтому мост получал `0 = LLAMA_SPLIT_MODE_NONE`, а при NONE llama.cpp оставляет из
списка устройств только `main_gpu` и RPC-узел отбрасывает. Свежая загрузка режим
передавала — поэтому раскладка работала там и пропадала на reload.

**Как теперь проверяется факт (а не догадка).**

| Поле / сообщение | Где | Смысл |
|---|---|---|
| `layout_degraded`, `layout_degraded_reason` | `GET /api/models` (модель) | узлы переданы, но ни один не стал устройством модели: раскладки НЕТ, причина названа |
| `раскладка: какие устройства видит планировщик` | лог воркера | `local_gpus`, `remote_devices`, `active_servers`, `admitted_servers`, `snapshot_devices` |
| `узел раскладки готов принять загрузку` | лог воркера | узел ответил на реальную команду протокола (не TCP-connect) перед загрузкой |
| `using device RPC0` | лог llama.cpp | ЕДИНСТВЕННОЕ доказательство, что узел участвует в загрузке |

Правило чтения: если `layout_degraded=true` — смотреть `layout_degraded_reason`; если
её нет, а скорость упала — искать `using device RPC0` в логе: его отсутствие при
переданных серверах и есть потеря раскладки.

**Границы.** Список узлов и пропорции сплита при reload берутся из прошлого инстанса
(пересчитываются при смене числа устройств). Узел обслуживает одно соединение —
перед загрузкой ставится короткая пауза и проверка готовности, что снижает, но не
устраняет гонку при одновременном подключении клиента.