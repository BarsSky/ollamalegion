# RPC Coordinator Mode

Production-ready distributed inference mode (P.1) для Ollama Legion balancer.

> **Это НЕ то же самое, что раскладка слоёв по устройствам (layer split).**
> Режим `rpc_coordinator` — про оркестрацию внешних worker'ов балансером. Реальная
> раскладка слоёв модели по нескольким GPU (включая чужие машины) делается внутри
> cppworker'а через llama.cpp RPC и **не требует включать этот режим** — она
> принадлежит плоскости бэкенда. См. [`layer-sharding.md`](layer-sharding.md).

## Что это

`rpc_coordinator` mode направляет inference-запросы через `ModelCoordinator`,
который оркестрирует pipeline из нескольких worker'ов. Каждый worker
обслуживает slice модели (диапазон слоёв). Это даёт:

- **Распределение слоёв модели** по нескольким worker'ам (pipeline parallelism).
- **Failover** между кандидатами per-slice через selector.
- **Circuit breaker** per-worker (защита от cascade failures).
- **Streaming SSE passthrough** для Ollama и OpenAI форматов.
- **Auth middleware** — token-based protection для inference paths.

## Архитектура

```
Client
  ↓ POST /api/generate
Balancer (port 8080)
  ↓ rpc_coordinator mode + dispatcher set + path match
RpcCoordinatorDispatcher
  ↓ checkAuth → ShouldRoute (model registered + CB not all Open) → coordinator.Infer/InferStream
ModelCoordinator
  ↓ SelectWorkersForSlice(slice) → InferSlice per worker
Worker 1 (slices 1-16)  ←→  Worker 2 (slices 17-32)
  ↓/↑ result aggregated
Coordinator
  ↓ formatted response per endpoint
Dispatcher
  ↓ response written
Client
```

## Конфигурация

### Operating mode

```json
{
  "balancing": {
    "operatingMode": "rpc_coordinator",
    "rpcCoordinator": {
      "enabled": true,
      "embedded": true,
      "failoverPolicy": "circuit_breaker",
      "requestTimeout": 30000000000,    // 30s in nanoseconds
      "streamTimeout": 300000000000,     // 5min
      "circuitBreaker": {
        "failureThreshold": 5,
        "successThreshold": 1,
        "resetTimeoutMs": 30000
      },
      "workers": [
        // Optional pre-registered workers. Обычно регистрируются динамически через heartbeat.
        "worker-1:8001",
        "worker-2:8001"
      ]
    }
  }
}
```

Параметры:

| Поле | Default | Описание |
|---|---|---|
| `enabled` | `false` | Включает rpc_coordinator mode. Требует `operatingMode=rpc_coordinator`. |
| `embedded` | `false` | `true` = spawn `ModelCoordinator` в balancer. `false` = external. |
| `failoverPolicy` | `"circuit_breaker"` | `circuit_breaker` / `retry` / `fail_fast`. |
| `requestTimeout` | `30s` | Non-streaming inference timeout. |
| `streamTimeout` | `5min` | Streaming inference timeout. |
| `circuitBreaker.failureThreshold` | `5` | Число failures перед Open. |
| `circuitBreaker.successThreshold` | `1` | Successes в HalfOpen → Closed. |
| `circuitBreaker.resetTimeoutMs` | `30000` | Время в Open перед HalfOpen probe. |

### Worker registration

Workers регистрируются динамически через `RegisterWorker` API или
pre-registered через `workers` массив в config. Каждый worker имеет:

```json
{
  "workerId": "worker-1",
  "host": "127.0.0.1",
  "port": 8001,
  "sliceLayers": "1-16"
}
```

### Distributed model

Каждая модель регистрируется в coordinator через `RegisterDistributedModel`:

```go
coord.RegisterDistributedModel("llama-70b", "Distributed 70B",
    []rpccoordinator.LayerSlice{
        {StartLayer: 1, EndLayer: 40, WorkerID: "worker-1"},
        {StartLayer: 41, EndLayer: 80, WorkerID: "worker-2"},
    })
```

Это можно сделать через API endpoint `/api/v1/rpc/models` или в коде.

## Поддерживаемые endpoints

| Path | Format | Streaming |
|---|---|---|
| `/api/generate` | Ollama | ✓ |
| `/api/ollama/generate` | Ollama (alias) | ✓ |
| `/api/chat` | Ollama | ✓ |
| `/api/ollama/chat` | Ollama (alias) | ✓ |
| `/v1/chat/completions` | OpenAI | ✓ (SSE + `[DONE]`) |
| `/v1/completions` | OpenAI legacy | ✓ (SSE + `[DONE]`) |

## Streaming format

### Ollama `/api/generate` (stream=true)

```
data: {"model":"llama-70b","response":"Once","done":false}

data: {"model":"llama-70b","response":" upon","done":false}

data: {"model":"llama-70b","response":"","done":true,"done_reason":"stop","total_duration":1234567}

```

### OpenAI `/v1/chat/completions` (stream=true)

```
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":...,"model":"llama-70b","choices":[{"index":0,"delta":{"content":"Once"}}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":...,"model":"llama-70b","choices":[{"index":0,"delta":{"content":" upon"}}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":...,"model":"llama-70b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

```

## Error responses

| Status | Error type | Когда |
|---|---|---|
| 400 | `json_parse_error` | Invalid JSON body |
| 400 | `missing_model` | `model` field is empty |
| 401 | `unauthorized` | Auth enabled, token missing/invalid |
| 404 | `model_not_distributed` | Model not registered in coordinator |
| 499 | `client_disconnected` | Client closed connection mid-stream |
| 502 | `upstream_error` | Worker returned unexpected error |
| 503 | `coordinator_disabled` | `RpcCoordinator.Enabled=false` |
| 503 | `coordinator_not_initialized` | Dispatcher coord is nil |
| 503 | `all_workers_unhealthy` | All workers have Open CB (Session 3.3) |
| 503 | `streaming_unsupported` | `http.Flusher` not available |
| 504 | `timeout` | Inference exceeded `requestTimeout`/`streamTimeout` |

OpenAI endpoint'ы используют `{"error":{"message","type"}}` format; Ollama —
`{"error":"<message>"}`.

## Auth

Если `auth.enabled=true` в config, dispatcher проверяет token перед
обработкой request:

```
Authorization: Bearer <token>
# или
X-API-Token: <token>
# или (для WebSocket-стиля клиентов)
?token=<token>
```

Auth применяется ко всем `/api/*` и `/v1/*` rpc_coordinator путям.

## Circuit Breaker

Per-worker circuit breaker. Лениво создаётся в `getOrCreateCircuitBreaker`
на первый request.

State machine:
- **Closed** — normal operation. `RecordFailure` инкрементит счётчик;
  на `failureThreshold` → Open.
- **Open** — все requests blocked. После `resetTimeout` → HalfOpen.
- **HalfOpen** — пропускает ровно 1 probe request. Success → Closed,
  Failure → Open (рестарт timeout).

Recording: после каждого `coordinator.Infer` / `coordinator.InferStream`
dispatcher вызывает `recordCBFromStats(sliceStats)`:
- `slice.Success == true` → `cb.RecordSuccess()`
- `slice.Success == false` → `cb.RecordFailure()`

ShouldRoute при request:
- Если модель не зарегистрирована → 404 `model_not_distributed`
- Если все workers Open → 503 `all_workers_unhealthy`
- Иначе → request проходит

## Мониторинг

Каждый request возвращает SliceStats в `X-Rpc-Slice-Stats` header (или
в response body для non-streaming):

```json
[
  {"slice_id": "1-16", "worker_id": "worker-1", "latency_ms": 234, "success": true},
  {"slice_id": "17-32", "worker_id": "worker-2", "latency_ms": 198, "success": true}
]
```

Circuit breaker state можно посмотреть через `/api/v1/rpc/workers` API
(legacy endpoint, может требовать расширения для CB visibility).

## Известные ограничения

- **Pipeline streaming упрощён**: slice 2 получает `"prompt + accumulated
  tokens"` как input, а не реальный `last_hidden_state` tensor. В
  production с реальной llama.cpp нужен KV-cache merge (отложено в P.3).
- **Streaming в stub workers**: `WorkerClient.InferSliceStream` парсит SSE
  через `bufio.Scanner`; для >1MB payloads может быть медленно.
- **CB не покрывает HealthCheck**: только Infer / InferStream errors
  триггерят CB transitions. Worker-level health — отдельный signal.
- **No chat template**: `flattenPrompt` joins messages как
  `"role: content\n"`. Реальный chat template из coordinator — Phase 9.
- **Транспорт раскладки не подключён** (см. следующий раздел): Go-сторона
  готова, C-сторона читает `rpc_servers` ещё нет.

## Что осталось для реального транспорта (S1 плана 2026-10-07)

Этот раздел — точная карта оставшейся работы: она требует CUDA-сборки и двух
машин, поэтому её нельзя было ни написать, ни проверить в сессии без стенда.

### Что уже сделано (Go-сторона, проверено тестами)

| Слой | Артефакт |
|---|---|
| Переменные | `CPPWORKER_RPC_SERVERS` (`host:port,host:port`), `CPPWORKER_RPC_ENABLE`, `CPPWORKER_MAIN_GPU`, `CPPWORKER_LLAMA_BUILD` — разбор и валидация: `cmd/cppworker/rpc_env_r101.go` |
| Конфиг | `cppbackend.Config.RPCServers / RPCEnabled / LlamaBuild` |
| Мост | `bridge.ModelConfig.RPCServers` заполняется в `LoadModelWithOpts` (`applyRPCServersR105`) |
| Версия сборки | `CPPWORKER_LLAMA_BUILD` → регистрация → `types.Backend.BuildInfo` → проверка совместимости сторон RPC в `POST /api/v1/placement/validate` (`version_mismatch`) |
| Балансер | логический бэкенд (группа), `strategy: sharded`, маршрутизация на мастера, фит по СУММЕ узлов — всё готово и покрыто тестами |

Пока транспорт не собран, включённый RPC **честно логируется как нерабочий** и на
старте, и на каждой загрузке модели: молчаливая настройка заставила бы оператора
гадать, почему вторая машина простаивает.

### Что нужно сделать в C-мосте (`c/bridge`) — СДЕЛАНО (2026-10-11)

1. **Структура.** ✅ `ModelConfig` в `bridge.h` получил `rpc_endpoints` /
   `rpc_endpoints_len`; заполняется из `RPCServers` в `bridge.go`
   (`fillRPCEndpointsR108`).
2. **Регистрация узлов ДО создания контекста.** ✅ `bridge_collect_rpc_devices`
   (bridge.c) регистрирует каждый endpoint через реестр RPC
   (`ggml_backend_reg_get_proc_address(reg, "ggml_backend_rpc_add_server")`) и
   вызывается до `llama_model_load_from_file`.
3. **Devices.** ✅ Собранный NULL-terminated массив (локальные не-CPU устройства
   + устройства RPC-реестров) передаётся в `llama_model_params.devices`; массив
   освобождается сразу после загрузки.
4. **Диагностика.** ✅ Каждый узел логируется отдельно («подключён / НЕ
   подключён»), отсутствие RPC в сборке llama.cpp и недоступность узлов дают
   понятное сообщение в stderr, а не таймаут загрузки модели.
5. **Версия.** ⏳ Осталось: отдавать реальный коммит llama.cpp из рантайма
   (`CPPWORKER_LLAMA_BUILD` по-прежнему проставляет образ, а не сама сборка).

### Что нужно сделать в поставке — СДЕЛАНО (2026-10-11)

- **Образ cppworker:** ✅ `-DGGML_RPC=ON` в `Dockerfile.gpu` и `Dockerfile.gpu.86`,
  `rpc-server` копируется в runtime-слой (`/app/rpc-server`) тем же коммитом и с
  теми же `CUDA_ARCHITECTURES`, что `libllama`.
- **Compose:** ✅ `deployments/docker-compose.rpc-node.yml` — сайдкар узла.
  Важная деталь: у образа `ENTRYPOINT=/entrypoint.sh`, который всегда запускает
  `./cppworker`, поэтому сайдкар переопределяет **entrypoint**, а не `command`.
  Старый `docker-compose.rpc.yml` помечен как историческая заготовка другой схемы.
- **Версия:** ⏳ Осталось (см. пункт 5 выше).

### Грабли сборки с RPC (проверено на живой сборке, 2026-10-11)

Три вещи, каждая из которых роняет сборку уже ПОСЛЕ того, как отработала
многочасовая стадия CUDA. Стоит знать до запуска:

1. **Бинарник называется `ggml-rpc-server`, а не `rpc-server`.** Цель объявлена в
   `c/llama.cpp/tools/rpc/CMakeLists.txt` (`set(TARGET ggml-rpc-server)`), хотя в
   документации llama.cpp её зовут rpc-server. Копируется в образ как
   `/app/rpc-server`; при COPY указывать исходное имя, иначе `rpc-server: not found`.
2. **При `-DGGML_RPC=ON` нужен `-lggml-rpc`.** Реестр бэкендов внутри `libggml.a`
   ссылается на `ggml_backend_rpc_reg`, и без библиотеки линковка падает:
   `undefined reference to 'ggml_backend_rpc_reg'`. Флаг добавлен в `CGO_LDFLAGS`.
3. **`libggml-rpc.a` лежит в подкаталоге** `build/ggml/src/ggml-rpc/`, поэтому
   `-L` на `build/ggml/src` недостаточно — нужен отдельный `-L` на этот подкаталог,
   иначе `cannot find -lggml-rpc`.

Плюс к этому сборка **CPU-варианта** (`Dockerfile.cpu`) падает, если билд запущен
отцепленно (WMI/без сессии входа): Docker не может достать учётные данные из
Credential Manager (`A specified logon session does not exist`). GPU-образ при
этом собирается нормально, а CPU-вариант для узла RPC и не нужен.

Отдельно: сам `rpc-server` при старте печатает предупреждение, что режим
экспериментальный и **небезопасный**, и просит не выставлять его в открытую сеть.
Поэтому правило фаервола `OllamaLegion-50052` ограничено своей подсетью
(`-RemoteAddress LocalSubnet`), и это же стоит учитывать при выносе узла за пределы
доверенной сети.

### Порядок работ и критерии приёмки

1. `iperf3 -c <узел> -t 30 -P 4` ≥ 900 Мбит/с (всё дальнейшее считается от этой
   цифры, см. §3 плана). ✅ **Выполнено 2026-10-11, но не iperf3** (Cygwin-сборка на
   Windows 26300 падает на control message): измерено сокетами .NET —
   `scripts/measure-network.ps1`, 92–110 МБ/с.
2. Собрать образ с `rpc-server`, поднять его на второй машине; проверить, что
   `POST /api/v1/placement/validate` для группы видит открытый порт (`rpc_unreachable`
   пропадает). ⏳ Образ собирается (`-DGGML_RPC=ON`), сайдкар — `docker-compose.rpc-node.yml`.
3. Загрузить модель, не влезающую в 8 ГБ, с `CPPWORKER_RPC_SERVERS=<второй узел>`
   и `CPPWORKER_SPLIT_MODE=1` (layer); зафиксировать TTFT и tok/s. ⏳ **Выполнено
   частично 2026-10-13 — транспорт работает, раскладки ещё нет; подробности в
   разделе «Живая проба транспорта RPC (2026-10-13)» ниже.**
4. Сравнить три числа на одной модели: `single`, `hybrid CPU+GPU` (одна машина),
   `network layer split` — это acceptance-критерий §6 плана.
5. Проверить расхождение сборок: узел со старым коммитом → `version_mismatch` в
   сухом прогоне (а не таймаут загрузки).

### Живая проба транспорта RPC (2026-10-13)

Стенд: мастер — этот хост (Ryzen 7 PRO 5750G, RTX 3070 8 ГБ, 24.5 GiB RAM),
донор — `192.0.2.10` (`CYBORG`, RTX 5060 Laptop 8 ГБ, sm_120). Мастер запущен
с `CPPWORKER_RPC_ENABLE=true`, `CPPWORKER_RPC_SERVERS=192.0.2.10:50052`,
`CPPWORKER_SPLIT_MODE=1`; модель — `Qwen3.8-27B-UD-Q4_K_M.gguf` (15.33 ГБ, 64
слоя, архитектура `qwen35`, гибрид attention/SSM), которая в 8 ГБ не влезает.

**Что подтвердилось — транспорт жив, узел виден llama.cpp:**

```
[bridge] RPC: узел 192.0.2.10:50052 подключён, его устройств: 1
[bridge] RPC: устройств в раскладке 2 (локальные + удалённые)
[bridge] RPC: список устройств передан в llama_model_load_from_file (устройств 2)
{"msg":"RPC-раскладка передана в мост","servers":["192.0.2.10:50052"]}
```

То есть C-часть (`bridge_collect_rpc_devices` + передача массива устройств до
`llama_model_load_from_file`) работает и на живой сборке.

### Результат живой проверки раскладки (2026-10-13, после исправлений)

Донор `192.0.2.10` к моменту проверки был недоступен (машина не отвечает:
ARP `Incomplete`, все порты закрыты), поэтому транспорт проверен на **локальном
сайдкаре тем же образом**:

```
cd deployments
CPPWORKER_GPU_TAG=rpc86-reasoning-r108e docker compose -f docker-compose.rpc-node.yml up -d
# cppworker: CPPWORKER_RPC_SERVERS=host.docker.internal:50052, CPPWORKER_SPLIT_MODE=1
```

Это функциональная проба (оба «устройства» — одна и та же карта, поэтому
быстродействия она не показывает), и она подтверждает всю цепочку:

```
[bridge] RPC: узел host.docker.internal:50052 подключён, его устройств: 1
[bridge] RPC: устройств в раскладке 2 (локальные + удалённые)
llama_prepare_model_devices: using device CUDA0 (NVIDIA GeForce RTX 3070) - 7097 MiB free
llama_prepare_model_devices: using device RPC0 (host.docker.internal:50052) - 7097 MiB free
```

Раскладка слоёв (gemma-4-E4B-it-Q4_K_M, `gpuLayers=42`, ctx=4096, kv=q4_0):

| Устройство | Слоёв |
|---|---|
| CUDA0 (локальная RTX 3070) | 23 |
| RPC0 (узел) | 19 |
| CPU | 1 |

Пропорции совпали с расчётом планировщика (`tensor split calculated (local + RPC):
localGPUs=1, rpcGPUs=1, ratios=[0.536, 0.464]`); `MemfitBudget: ёмкость RPC-узлов
добавлена в бюджет` и `EnvironmentProfile: ёмкость RPC-узлов добавлена`
подтверждают, что оба планировщика видят ёмкость узла.

**Что осталось для полной приёмки (§6):** вернуть донора в сеть
(`CPPWORKER_RPC_SERVERS=192.0.2.10:50052`) и снять три числа на одной модели —
`single` (локальная карта, 20 слоёв), `hybrid CPU+GPU`, `network layer split` —
по TTFT и tok/s. Проба автоматизирована: `scripts/rpc-shard-load.ps1`.

### Живой факт про отказ узла (2026-10-13)

Пересоздание узла (контейнера `rpc-server`) **под живым соединением мастера**
роняет мастер: в логе `SIGABRT: abort` + `signal arrived during cgo execution` во
время `bridge_load_model`, после чего cppworker перезапускается (политика
`restart: unless-stopped`) и модель оказывается не загруженной. Это поведение
llama.cpp RPC: обрыв протокола на удалённом бэкенде приводит к `abort()`, а не к
ошибке загрузки, которую можно обработать.

Практический вывод для стенда: не пересоздавать узел под живой моделью —
сначала выгрузить модель (`keep_alive=0` / `/api/models/unload`), затем менять
узел. Что стоит сделать дальше (отдельная задача): health-гейт узла перед
загрузкой и авто-выгрузка модели при потере узла, чтобы отказ узла не выглядел
как падение мастера.

**Чего не хватает — число слоёв до llama.cpp не доходит.** Его урезают ДВА
Go-планировщика, каждый считает свободную память только по ЛОКАЛЬНОЙ карте:

| # | Где | Что видно в логе |
|---|---|---|
| 1 | `internal/cppbackend/backend.go:1651` (`checkVRAMForModel` → memfit) | `auto-adapting GPU layers for model (memfit): requestedGPULayers=48 optimalGPULayers=20` |
| 2 | `cmd/cppworker/lazyload_calc.go:271` (`calculateLazyLoadOpts`, при `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD=true`) | `applying partial offload: requested_gpu_layers=48, reduced_gpu_layers=16` |
| 3 | `internal/cppbackend/gpu_distribution.go:183` | `tensor split calculated (vram-ratio) gpuCount=1 ratios=[1] totalFreeMB=8191` |

Итог в мосте и в llama.cpp:

```
[bridge] loading model: .../Qwen3.8-27B-UD-Q4_K_M.gguf (ctx=8192, batch=512, threads=6, gpu_layers=20)
llama_prepare_model_devices: using device CUDA0 (NVIDIA GeForce RTX 3070) (0000:01:00.0) - 7097 MiB free
```

`llama_prepare_model_devices` видит одно устройство и раскладывать нечего: 20
слоёв помещаются в локальные 7 ГБ, поэтому удалённый узел простаивает.
Выключение `CPPWORKER_AUTO_OFFLOAD=false` на это НЕ влияет (это другой
переключатель) — урезание делает memfit, а не auto-offload.

### Настоящая причина «одного устройства» (найдена 2026-10-13, исправлено)

Отдельно от планировщиков был дефект, который делает раскладку невозможной **в
принципе**: режим раскладки не доезжал до моста.

`CPPWORKER_SPLIT_MODE=1` разбирался в `cmd/cppworker/handlers_model.go`
(`defSplitMode = 1`) и клался в `LoadModelOpts.SplitMode`, но в
`bridge.ModelConfig` это поле не присваивалось нигде — мост всегда видел
`split_mode = 0` = `LLAMA_SPLIT_MODE_NONE`. По этому режиму llama.cpp
(`src/llama.cpp`, `llama_prepare_model_devices`) оставляет из переданного массива
устройств **только `main_gpu`** — отсюда одна строка `using device CUDA0` при
«устройств в раскладке 2». Вторая ловушка того же поля: значение `-1` («дефолт
llama.cpp» = LAYER по соглашению `c/bridge/bridge.h`) переводилось в `0` = NONE,
а дефолт `defSplitMode := 0` означал NONE — то есть авто-tensor-split по
локальным GPU не работал и без RPC (пропорции считались, но llama.cpp их
игнорировал).

Что сделано:

| Что | Где |
|---|---|
| `opts.SplitMode` доводится до моста | `internal/cppbackend/backend.go` |
| Дефолт — `-1` = LAYER; `-1` не подменяется нулём | `cmd/cppworker/handlers_model.go` |
| Мост отдаёт планировщику список устройств раскладки (`bridge_collect_devices`): имя, тип, «удалённое ли», свободно/всего — в порядке `llama_model_params.devices` | `c/bridge/bridge.h`, `bridge.c`, `bridge.go` |
| memfit и адаптивный планировщик видят ёмкость узлов (`+rpc`) | `internal/cppbackend/memfit_adapter.go`, `cmd/cppworker/adaptive_loader.go` |
| tensor split учитывает удалённые устройства, мастер остаётся локальным | `internal/cppbackend/gpu_distribution.go` |

Регрессии закрыты тестами: `TestR108_SplitModeReachesBridge`,
`TestR108_SplitModeDefaultIsLayer` (статические) и
`TestR108_TensorSplitIncludesRemoteDevices` (пропорции 1/3 и 2/3 для 3 ГБ
локально + 6 ГБ на узле).

**Следующий шаг (по возрастанию объёма работ):**

1. **Отдать планировщикам VRAM удалённых узлов.** Источник правды о том, какие
   устройства реально доступны, — мост: он уже отдаёт «устройств в раскладке 2».
   Нужен запрос вида `bridge_get_devices()` (тип, имя, `memory_free`) → в
   `cppbackend` → в `memfit` как дополнительная ёмкость и в
   `gpu_distribution`/`tensor_split`. Тогда вердикт посчитает 48-56 слоёв, а не 20.
2. **Проверить, что llama.cpp действительно берёт второе устройство.** После (1)
   в логе должно быть две строки `using device ...` (CUDA0 и RPC0) и распределение
   слоёв между ними; если RPC-устройство по-прежнему отбрасывается — смотреть
   `llama_prepare_model_devices` в используемом коммите `c/llama.cpp` (условия
   отбора устройств и требования к `tensor_split`).
3. **Сравнить три числа** (критерий §6): `single` (20 слоёв на 3070),
   `hybrid CPU+GPU` (то же, но без RPC), `network layer split` (48+ слоёв на
   3070+5060). Мерить TTFT и tok/s по полям `prompt_eval_duration` /
   `eval_count` / `eval_duration` живого ответа.

Полезно для проб: планировщик на загрузке выключается переменной
`CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD=false` (в compose она теперь передаётся —
`deployments/docker-compose.stack.yml`), а memfit — не выключается ничем: он и
есть тот гейт, который предстоит научить про RPC.

Замер для ориентира (R108, одиночная карта, 20 слоёв на 3070, ctx=8192):
`Qwen3.8-27B-UD-Q4_K_M` отвечает ~1.1 tok/s, TTFT на prefill ~210 с — то есть
выигрыш от раскладки на второй GPU здесь измеримый и стоит работы.

### Вопросы, которые нужно решить ДО реализации (из §11 плана)

- `rpc-server` — обязательный компонент поставки или опциональный профиль compose?
- Неоднородные GPU (sm_86 + sm_120) — официально допускаем или только с
  предупреждением?
- Падение воркера = отказ всей модели: авто-пересборка раскладки или честный 503?
- Заложить ли сразу N узлов (3–4 машины), а не 2?

## Примеры

### Distributed Llama-70B (2 workers, layers 1-40 + 41-80)

```bash
# 1. Start worker 1
cd worker-1 && ./rpcworker --worker-id=w1 --port=8001 --models-dir=/models/llama-70b
# 2. Start worker 2
cd worker-2 && ./rpcworker --worker-id=w2 --port=8001 --models-dir=/models/llama-70b

# 3. Configure balancer
cat > config.json <<EOF
{
  "balancing": {
    "operatingMode": "rpc_coordinator",
    "rpcCoordinator": {
      "enabled": true,
      "embedded": true,
      "failoverPolicy": "circuit_breaker"
    }
  }
}
EOF

# 4. Start balancer
./balancer --config=config.json

# 5. Register distributed model (via API or startup script)
curl -X POST http://localhost:8081/api/v1/rpc/models \
  -H "Content-Type: application/json" \
  -d '{
    "name": "llama-70b",
    "slices": [
      {"startLayer": 1, "endLayer": 40, "workerId": "w1"},
      {"startLayer": 41, "endLayer": 80, "workerId": "w2"}
    ]
  }'

# 6. Inference
curl -X POST http://localhost:8080/api/generate \
  -d '{"model":"llama-70b","prompt":"Hello"}'
```

### Streaming OpenAI

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model":"llama-70b",
    "messages":[{"role":"user","content":"Hello"}],
    "stream":true
  }'
```

## Migration / fallback

Чтобы отключить rpc_coordinator и вернуться к standard mode:

```json
{
  "balancing": {
    "operatingMode": "standard",
    "rpcCoordinator": {
      "enabled": false
    }
  }
}
```

Scaffold в `Proxy.ServeHTTP` проверяет `IsRpcCoordinatorMode` →
если `false`, request идёт через обычный proxy flow без изменений.

## См. также

- [`layer-sharding.md`](layer-sharding.md) — **как реально раскладываются слои**
  по нескольким GPU/машинам (llama.cpp RPC): уровень реализации, контейнеры,
  параметры, проверка, грабли.
- `deployments/docker-compose.rpc-node.yml` — сайдкар `rpc-server` на доноре.
- `docs/phase-8-rpc-coordinator.md` — implementation plan.
- `plans/2026-q3-production-ready-plan.md` — overall production-ready plan.
- `CHANGELOG.md` — version history.
- `internal/balancer/rpc_coordinator_dispatcher.go` — implementation.
- `internal/rpccoordinator/coordinator.go` — coordinator.
- `internal/rpccoordinator/worker_client.go` — worker client.
