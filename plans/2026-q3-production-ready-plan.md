# Production-Ready план (Sessions G+): rpc_coordinator + virtual_router + real ggml + CI/CD

> **Branch:** новая ветка `feature/q3-production` от `integration/q3-w3-4`
> **Date:** 2026-06-28
> **Status:** 🆕 Новая сессия — главный план 1.0 release
> **Приоритет:** 🔴 P0 (блокирует 1.0 release)
> **Период:** август-сентябрь 2026 (~25-30 рабочих дней)

---

## 0. Контекст

В Q3 W3-4 (Sessions 13-19 + A-E) **полностью закрыты** все user-facing фичи из
roadmap sections 2 (Models tab gaps) и 5 (UI/UX quick wins via Session F).
Проект готов к 1.0 release с точки зрения пользовательского интерфейса.

Однако для **production-ready** развёртывания (где критичны failover, observability,
CI/CD, и multi-GPU inference через TP) требуется закрыть 4 крупных архитектурных
задачи из roadmap:

| # | Задача | Источник | Оценка | Приоритет |
|---|---|---|---:|---|
| **P.1** | `rpc_coordinator` production mode | roadmap 3.2 | 5-8 дней | 🔴 P0 |
| **P.2** | `virtual_router` production mode | roadmap 3.3 | 5-7 дней | 🔴 P0 |
| **P.3** | Real ggml/NCCL integration в TPRuntime | roadmap B8.7 | 10-15 дней | 🟡 P1 (research) |
| **P.4** | CI/CD scaffolding (GitHub Actions) | roadmap 7.1-7.5 | 1-2 дня | 🔴 P0 |

**Итого:** 21-32 рабочих дня. Цель — release 1.0 в сентябре 2026 с полным production-ready набором.

P.3 (real ggml) — **research-grade** задача, явно post-1.0, оставлена как backlog в этом плане.

---

## 1. Архитектурная картина

```
                ┌─────────────────────────────────┐
                │   Client (Cline/OpenWebUI/etc)  │
                └────────────────┬────────────────┘
                                 │
                  ┌──────────────▼──────────────┐
                  │   Balancer (OperatingMode)  │
                  │                              │
                  │  • standard                 │
                  │  • replication              │
                  │  • rpc_coordinator  ◄── P.1 │
                  │  • virtual_router    ◄── P.2 │
                  │  • distributed_inference    │
                  └──────────────┬───────────────┘
                                 │
        ┌────────────────────────┼─────────────────────────┐
        │                        │                         │
   ┌────▼─────┐           ┌──────▼──────┐           ┌─────▼─────┐
   │ Ollama   │           │  CppWorker  │           │ RPC Coord │
   │ backends │           │  backends   │           │ + Workers │
   └──────────┘           └─────────────┘           └─────┬─────┘
                                                          │
                                              ┌───────────┴────────────┐
                                              │                        │
                                         ┌────▼────┐             ┌─────▼────┐
                                         │ Worker0 │             │ Worker1  │
                                         │ (GPU 0) │             │ (GPU 1)  │
                                         └─────────┘             └──────────┘
                                              │
                                              ▼
                                         ┌─────────┐
                                         │ Tensor  │  ◄── P.3 (post-1.0)
                                         │ Parallel│      (StubTPRuntime для 1.0)
                                         │ (B8)    │
                                         └─────────┘

         ▲
         │
    ┌────┴────────────────────────────┐
    │   GitHub Actions CI (P.4)       │  ◄── тесты на каждом PR
    │   • go test -race ./...         │
    │   • golangci-lint               │
    │   • build artifacts             │
    └─────────────────────────────────┘
```

---

## 2. P.1 — `rpc_coordinator` production mode (5-8 дней)

### Цель

Поднять `ModelCoordinator` в `cmd/balancer/main.go` если `OperatingMode == "rpc_coordinator"`,
и прокачать inference через RPC pipeline. Когда пользователь шлёт `POST /api/generate` или
`POST /v1/chat/completions` на balancer в этом режиме — запрос маршрутизируется не напрямую
на backend, а через `ModelCoordinator.executePipeline()`, который распределяет работу
по rpcworker'ам, синхронизирует KV-cache и собирает результат.

### Текущее состояние (что уже есть)

После Sessions 5-12 (B1-B8) реализовано:
- ✅ `cmd/rpcworker` поднимает HTTP+gRPC-сервер с 7 endpoints.
- ✅ `internal/rpccoordinator.ModelCoordinator` с heartbeat (B3), selector (B6),
  binary protocol (B5), Prometheus metrics (B7), Tensor Parallelism (B8).
- ✅ `internal/api/handlers_rpc.go` — management endpoints для RPC (B2):
  `/api/v1/rpc/workers`, `/api/v1/rpc/models`, etc.
- ✅ `internal/balancer/rpc_modules.go` — `initRpcModules()` (cond на `cfg.RpcCoordinator.Enabled`).
- ❌ **Отсутствует**: production-grade integration в `Proxy.ServeHTTP` для
  `/api/generate` → `executePipeline()`.

### План реализации

#### Шаг 1 (день 1): Конфигурация и activation logic

**Файлы:**
- `internal/balancer/operating_modes.go` — добавить `OperatingModeRpcCoordinator = "rpc_coordinator"`
  в enum + helper `isRpcCoordinatorMode(cfg)`.
- `pkg/types/balancing.go` — расширить `RpcCoordinatorConfig`:
  ```go
  type RpcCoordinatorConfig struct {
      Enabled         bool          `json:"enabled"`
      CoordinatorURL  string        `json:"coordinatorURL"` // для proxy mode (когда coordinator на отдельной машине)
      Embedded        bool          `json:"embedded"`       // если true — поднимаем ModelCoordinator в balancer
      Workers         []string      `json:"workers"`        // адреса rpcworker'ов для initial registration
      FailoverPolicy  string        `json:"failoverPolicy"` // "circuit_breaker" | "retry" | "fail_fast"
      RequestTimeout  time.Duration `json:"requestTimeout"` // default 30s
      StreamTimeout   time.Duration `json:"streamTimeout"`  // default 5min
  }
  ```
- `config/config.example.json` — добавить example секцию.

#### Шаг 2 (день 2-3): Dispatcher

**Файлы:**
- `internal/balancer/rpc_coordinator_dispatcher.go` (новый, ~250 LOC):
  - `RpcCoordinatorDispatcher` struct с `*rpccoordinator.ModelCoordinator` + `*Proxy`.
  - `ServeHTTP(w, r)` — interceptor для `/api/generate`, `/api/chat`,
    `/v1/chat/completions`, `/v1/completions`.
  - Определяет `model` из body → `coordinator.GetModel(name)`.
  - Если модель распределена (есть `DistributedModel`) → `coordinator.Infer(...)`.
  - Иначе → fallback на обычный `proxy.ServeHTTP(w, r)` (transparent passthrough).
  - **Streaming**: для `stream=true` → использовать `coordinator.InferStream()` с
    SSE passthrough (chunk-by-chunk relay to client).
  - **Error handling**:
    - Все workers unhealthy → HTTP 503 + `{"error":"no rpc workers available"}`.
    - Worker timeout → circuit breaker (открывается на 30s, потом half-open).
    - Partial pipeline failure → retry slice на secondary worker (если есть в candidates).
  - **Auth**: `X-API-Token` обязателен для rpc_coordinator mode.

- `internal/balancer/rpc_coordinator_dispatcher_test.go` (новый, ~200 LOC):
  - 6 unit-тестов:
    - `TestRpcCoordinatorDispatcher_NonDistributedModel_Fallback`
    - `TestRpcCoordinatorDispatcher_DistributedModel_Infer`
    - `TestRpcCoordinatorDispatcher_AllWorkersUnhealthy_503`
    - `TestRpcCoordinatorDispatcher_Streaming_Passthrough`
    - `TestRpcCoordinatorDispatcher_CircuitBreaker_Opens`
    - `TestRpcCoordinatorDispatcher_AuthMissing_401`

#### Шаг 3 (день 4): Production-mode main.go wiring

**Файлы:**
- `cmd/balancer/main.go`:
  - При старте проверить `OperatingMode == "rpc_coordinator"`.
  - Если да:
    1. Создать `rpccoordinator.NewModelCoordinator(cfg.RpcCoordinator)`.
    2. Поднять `WorkerClient` для каждого URL в `cfg.RpcCoordinator.Workers`.
    3. Зарегистрировать workers через heartbeat.
    4. Запустить `HeartbeatLoop.Start(ctx)`.
    5. Создать `RpcCoordinatorDispatcher(coordinator, proxy)`.
    6. Зарегистрировать его в `mux.HandleFunc("/api/generate", dispatcher.ServeHTTP)`
       (или через wrapping в `proxy.go`).
  - Graceful shutdown: `HeartbeatLoop.Stop()`, `coordinator.Close()`.

- `internal/balancer/proxy.go`:
  - Расширить `Proxy.ServeHTTP` для intercept'а при `p.rpcDispatcher != nil`:
    ```go
    if p.rpcDispatcher != nil && (isRpcPath(r.URL.Path)) {
        p.rpcDispatcher.ServeHTTP(w, r)
        return
    }
    ```

#### Шаг 4 (день 5): Circuit breaker для workers

**Файлы:**
- `internal/rpccoordinator/circuit_breaker.go` (новый, ~120 LOC):
  - `CircuitBreaker` struct с состояниями Closed/Open/HalfOpen.
  - `Allow() bool` — пропускает ли worker запрос.
  - `RecordSuccess()` / `RecordFailure()` — обновляют счётчики.
  - `OnStateChange(callback)` — уведомление при переходе Closed→Open, Open→HalfOpen, etc.
- `internal/rpccoordinator/circuit_breaker_test.go` (новый, ~150 LOC):
  - 8 unit-тестов:
    - Initial state = Closed.
    - N failures → Open.
    - Reset after timeout → HalfOpen.
    - Single success in HalfOpen → Closed.
    - Single failure in HalfOpen → Open (reset timeout).
    - Concurrent Allow() calls thread-safe.
    - OnStateChange callback fires correctly.
    - Custom thresholds (failureCount, timeout).

- `internal/rpccoordinator/coordinator.go` — интегрировать `CircuitBreaker` в
  `executePipeline` для каждого worker'а:
  - Перед вызовом worker'а: `if !cb.Allow() { skip worker }`.
  - После вызова: `cb.RecordSuccess()` или `cb.RecordFailure()`.
  - Если все worker'ы fail (`Allow()=false`) → возвращается ошибка
    `"no healthy workers available"`.

#### Шаг 5 (день 6-7): E2E тесты + production config

**Файлы:**
- `tests/rpc_coordinator_e2e_test.go` (новый, ~400 LOC):
  - 4-5 e2e-тестов через `httptest.NewServer`:
    - `TestRpcCoordinatorMode_FullPipeline` — поднять 2 rpcworker'а, прокачать inference через balancer в rpc_coordinator mode, проверить результат.
    - `TestRpcCoordinatorMode_StreamingFallback` — streaming с SSE passthrough.
    - `TestRpcCoordinatorMode_WorkerFailover` — primary worker 500 → secondary.
    - `TestRpcCoordinatorMode_CircuitBreakerOpens` — 3 sequential failures → circuit opens.
    - `TestRpcCoordinatorMode_NonDistributedFallback` — обычная Ollama модель → fallback на `Proxy.ServeHTTP`.

- `deployments/docker-compose.rpc.yml` (обновить):
  - Добавить `OperatingMode=rpc_coordinator` в env балансера.
  - Пример с 4 rpcworker'ами + coordinator'ом.

- `deployments/.env.rpc.example` (новый):
  - Полный example с комментариями.

- `docs/configuration.md` (обновить):
  - Секция "rpc_coordinator mode" с примером конфига + flow diagram.

#### Шаг 6 (день 8): Documentation + CHANGELOG

- `docs/rpc-coordinator.md` — секция "Production deployment" (по аналогии с B1-B8).
- `docs/configuration.md` — добавить OperatingMode в основную секцию.
- `CHANGELOG.md` — подсекция `### Added (Session G — rpc_coordinator production mode)`.

### Acceptance criteria

1. `OperatingMode = "rpc_coordinator"` end-to-end inference работает через RPC pipeline.
2. Все workers unhealthy → HTTP 503 + `{"error":"no rpc workers available"}`.
3. Worker fails mid-request → failover на secondary через circuit breaker.
4. Non-distributed model → transparent fallback на обычный `Proxy.ServeHTTP`.
5. Streaming через SSE работает с latency < 200ms per chunk.
6. Real production config (`deployments/.env.rpc.example` + `docker-compose.rpc.yml`)
   поднимается одной командой.
7. Circuit breaker открывается после 3 failures, закрывается после 30s timeout.
8. 6 unit-тестов dispatcher + 8 unit-тестов circuit breaker + 5 e2e-тестов PASS.
9. Build OK: `go build -tags llama_stub ./cmd/balancer/`.
10. CHANGELOG подсекция готова.

---

## 3. P.2 — `virtual_router` production mode (5-7 дней)

### Цель

Реализовать `virtual_router` режим балансировщика, где виртуальная модель = алиас
на пул реальных бэкендов с автоматическим выбором (round-robin / least-loaded).
Пользователь видит одну модель (например, `virtual:my-gpt`), а балансировщик
распределяет запросы между всеми бэкендами с этой моделью.

### Текущее состояние

- ✅ `internal/virtualmodel/` — каркас создан (31 тест PASS):
  - `VirtualModel`, `SliceExecutionContext`, `ExecutePipeline` (sequential).
  - `Registry` (CRUD).
  - `Router` (базовая маршрутизация).
- ❌ Отсутствует:
  - `ParallelExecutor` и `TreeExecutor` (фаза C1 не начата).
  - Production-grade integration в `Proxy.ServeHTTP`.

### План реализации

#### Шаг 1 (день 1): Расширение типов

**Файлы:**
- `pkg/types/types.go` — расширить `VirtualModelConfig`:
  ```go
  type VirtualModelConfig struct {
      Name         string
      Description  string
      Slices       []ModelSliceConfig
      Coordination CoordinationConfig
      Selection    SelectionStrategy `json:"selection"` // "round_robin" | "least_loaded" | "random"
      BackendPool  []string          `json:"backendPool"` // pool of real backends
      ModelName    string            `json:"modelName"`   // physical model on those backends
  }
  
  type SelectionStrategy string
  const (
      SelectionRoundRobin   SelectionStrategy = "round_robin"
      SelectionLeastLoaded  SelectionStrategy = "least_loaded"
      SelectionRandom       SelectionStrategy = "random"
  )
  ```
- `internal/virtualmodel/virtual_model.go` — добавить `Selection` поле в `VirtualModel`.

#### Шаг 2 (день 2-3): Selection strategies

**Файлы:**
- `internal/virtualmodel/selector.go` (новый, ~150 LOC):
  - `Selector` interface с методами `Select(ctx, pool) (backendID, error)`.
  - `RoundRobinSelector` — atomic counter, увеличивается на каждый Select.
  - `LeastLoadedSelector` — кэширует метрики `FreeSlots` per backend, выбирает max.
  - `RandomSelector` — `math/rand` (для тестов).

- `internal/virtualmodel/selector_test.go` (новый, ~200 LOC):
  - 9 unit-тестов (по 3 на каждый selector: empty pool, single backend, multiple backends + concurrency).

- `internal/virtualmodel/registry.go` — расширить `Registry.Register(config)`:
  - Валидация `BackendPool` (все backends существуют и registered).
  - Валидация `ModelName` (модель доступна хотя бы на одном backend'е).
  - Возврат ошибки при дубликате имени.

#### Шаг 3 (день 4): Virtual router

**Файлы:**
- `internal/balancer/virtual_router.go` (новый, ~300 LOC):
  - `VirtualRouter` struct с `*virtualmodel.Registry` + `*Proxy`.
  - `ServeHTTP(w, r)` — interceptor для `POST /v1/chat/completions` /
    `POST /v1/completions` с `model="virtual:..."`.
  - Парсит `model`, извлекает virtual name (после префикса `virtual:`).
  - `registry.Get(name)` → `VirtualModel`.
  - Выбирает backend через `selector.Select()`.
  - Подменяет `model` в body на реальное имя, добавляет `X-Original-Backend`
    header (для debug), проксирует на выбранный backend через `Proxy.ServeHTTP`.
  - Metrics: `virtual_inference_total{vm_name="..."}`,
    `virtual_backend_selections{backend_id="..."}`, `virtual_inference_errors_total`.

- `internal/balancer/virtual_router_test.go` (новый, ~250 LOC):
  - 7 unit-тестов:
    - `TestVirtualRouter_NonVirtualModel_Fallback` (обычная модель → Proxy).
    - `TestVirtualRouter_RoundRobinSelection` — 3 запроса → 3 разных backend'а.
    - `TestVirtualRouter_LeastLoadedSelection` — backend с max FreeSlots wins.
    - `TestVirtualRouter_AllBackendsDown_503`.
    - `TestVirtualRouter_UnknownVirtualModel_404`.
    - `TestVirtualRouter_StreamingPassthrough` — SSE через selected backend.
    - `TestVirtualRouter_MetricsRecorded` — проверка Prometheus metrics.

#### Шаг 4 (день 5): CRUD REST API

**Файлы:**
- `internal/api/handlers_virtual.go` (новый, ~200 LOC):
  - `GET /api/v1/virtual-models` — список (с name, description, backendPool, modelName, selection).
  - `POST /api/v1/virtual-models` — создать (валидация + register).
  - `GET /api/v1/virtual-models/{name}` — детали.
  - `DELETE /api/v1/virtual-models/{name}` — удалить.
  - `POST /api/v1/virtual-models/{name}/infer` — test endpoint (проксирует один запрос).

- `internal/api/handlers_virtual_test.go` (новый, ~200 LOC):
  - 8 unit-тестов (CRUD операции + auth + validation).

- `internal/api/routes.go` — регистрация routes.

#### Шаг 5 (день 6): WebUI страница

**Файлы:**
- `webui/virtual-models.html` (новый, ~300 LOC):
  - Self-contained dark-mode панель (по аналогии с `rpc-status.html`).
  - Список virtual models с карточками.
  - Форма создания: name, description, modelName, selection strategy, backendPool (multi-select).
  - Кнопки Edit/Delete/Infer (test).
  - Auto-refresh 10s.
- `webui/index.html` — добавить header link "Virtual Models" (рядом с "RPC").
- `webui/js/i18n/{en,ru}.js` — ключи `virtual_models.*` (~12 × 2).
- `webui/css/components.css` — стили для virtual models page.

#### Шаг 6 (день 7): Documentation + CHANGELOG

- `docs/virtual-router.md` (новый) — архитектура + usage examples.
- `docs/api.md` — добавить новые endpoints.
- `CHANGELOG.md` — подсекция `### Added (Session H — virtual_router production mode)`.

### Acceptance criteria

1. `POST /v1/chat/completions` с `model="virtual:my-gpt"` маршрутизируется на любой
   доступный backend из `backendPool`.
2. Round-robin selection: 5 запросов → равномерно распределены по 3 backend'ам.
3. Least-loaded selection: backend с max FreeSlots выбирается.
4. Все backends unhealthy → HTTP 503 + `{"error":"no healthy backends in pool"}`.
5. CRUD API работает: GET/POST/DELETE /api/v1/virtual-models.
6. WebUI `virtual-models.html` показывает список, позволяет создать/удалить.
7. Metrics: `virtual_inference_total`, `virtual_backend_selections`, `virtual_inference_errors_total`
   экспортируются в `/api/v1/rpc/metrics` (добавить в MetricsAggregator).
8. 9 unit-тестов selector + 7 unit-тестов virtual_router + 8 unit-тестов API = 24 PASS.
9. Build OK.
10. CHANGELOG подсекция готова.

---

## 4. P.3 — Real ggml/NCCL integration в TPRuntime (10-15 дней) — RESEARCH

> ⚠️ **ЯВНО POST-1.0**. Этот раздел оставлен как backlog для research-grade версии
> после релиза 1.0. В 1.0 release используется `StubTPRuntime` (уже реализован в Session 12).
> Оценка 10-15 дней — для команды с CUDA/NCCL экспертизой.

### Цель

Заменить `StubTPRuntime` на реальный backend с llama.cpp binding для tensor parallelism.
Это позволит развернуть одну модель (например, llama-3-70B) на нескольких GPU через
TP, получив sub-linear latency scaling.

### Почему отложено в post-1.0

- Требует глубокой CUDA/NCCL экспертизы (10-15 дней — нижняя граница).
- Зависит от upstream llama.cpp API (`llama_tensor_split`, `ggml_all_reduce`),
  который ещё нестабилен.
- Тестирование требует multi-GPU runner'а в CI (отдельная инфраструктурная задача).
- 1.0 release работает на `StubTPRuntime` — фреймворк готов, реальная интеграция
  является drop-in replacement.

### План реализации (для будущей сессии после 1.0)

#### Шаг 1 (день 1-2): C-bridge functions

**Файлы:**
- `c/bridge/tp_bridge.h` (новый, ~50 LOC) — header:
  ```c
  typedef struct {
      int rank;
      int world_size;
      void* comm; // NCCL communicator handle (opaque)
  } tp_context_t;
  
  int bridge_load_shard(const char* path, int layer_idx, int rank, int world_size, void** out_handle);
  int bridge_all_reduce_sum(void* data, size_t bytes, tp_context_t* ctx);
  int bridge_all_reduce_concat(void** shards, size_t* shard_sizes, int num_shards, void* out, tp_context_t* ctx);
  ```
- `c/bridge/tp_bridge.c` (новый, ~500 LOC) — реализация поверх NCCL:
  - `ncclCommInitRank` для каждого worker'а.
  - `ncclAllReduce` для sum/concat операций.
  - CUDA IPC handles для KV-cache sharing между rank'ами.

#### Шаг 2 (день 3-5): GGUF shard loading

**Файлы:**
- `internal/rptensor/ggml_shard.go` (новый, ~600 LOC):
  - `LoadShard(path string, layerIdx int, rank int, worldSize int) (*Shard, error)`.
  - Парсит GGUF header через существующий `cppbackend.ReadGGUFHeader`.
  - Определяет offset+size для каждого тензора в слое `layerIdx`.
  - Загружает только нужные тензоры для данного `rank` (по partition strategy).
  - Возвращает `*Shard` с указателями на GPU memory.

#### Шаг 3 (день 6-8): TPRuntime implementation

**Файлы:**
- `internal/rptensor/cuda_runtime.go` (новый, ~800 LOC):
  - `CudaRuntime` struct реализует `TPRuntime` interface:
    - `Init(ctx, config)` → инициализация NCCL, CUDA context.
    - `LoadModel(path, strategy, worldSize)` → партиционирование + shard loading.
    - `RunLayer(layerIdx, input, kvShard) (output, newKVShard, error)`:
      - Forward pass на текущем rank'е.
      - All-reduce результатов (sum для column partition, concat для row partition).
      - Update KV-cache shard (через CUDA IPC).
    - `Close()` → cleanup NCCL, освобождение GPU memory.

- `internal/rptensor/coordinator.go` — заменить `StubTPRuntime` на `CudaRuntime`
  при наличии env `TPRUNTIME=cuda`. По умолчанию остаётся `StubTPRuntime`.

#### Шаг 4 (день 9-10): Tests с реальной CUDA

**Файлы:**
- `internal/rptensor/cuda_runtime_test.go` (новый, ~400 LOC):
  - Build tag `cuda && linux` (только на GPU runner'ах).
  - `t.Skip` если CUDA недоступна.
  - 4 e2e-теста:
    - `TestCudaRuntime_LoadShard_Llama8B` (4-rank Megatron partition).
    - `TestCudaRuntime_RunLayer_AllReduceSum` (проверка sum через 2 rank'а).
    - `TestCudaRuntime_RunLayer_AllReduceConcat` (concat для row partition).
    - `TestCudaRuntime_KVCacheIPC_ShareBetweenRanks`.

- `.github/workflows/gpu.yml` (новый) — self-hosted runner с CUDA:
  ```yaml
  name: GPU Tests
  on: [push, pull_request]
  jobs:
    cuda:
      runs-on: [self-hosted, gpu, cuda]
      steps:
        - uses: actions/checkout@v4
        - uses: actions/setup-go@v5
        - run: go test -tags "cuda llama_cublas" ./internal/rptensor/...
  ```

#### Шаг 5 (день 11-15): Integration + benchmarks

**Файлы:**
- `internal/rpccoordinator/e2e_tp_cuda_test.go` (новый, ~300 LOC):
  - E2E через httptest с реальной CUDA.
  - 2-rank TP inference llama-3-8B на 2× RTX 4090.
  - Verify: output text совпадает с sequential (tolerance 1e-4).
  - Benchmark: latency scaling (2-rank должен быть ~0.55x от sequential).

- `docs/tp-cuda-deployment.md` (новый) — hardware requirements,
  NCCL installation, multi-GPU setup.

- `CHANGELOG.md` — `### Added (post-1.0 — Real ggml/NCCL TP integration)`.

### Acceptance criteria

1. Реальная 2-rank TP inference модели 8B на 2× RTX 4090 работает.
2. Latency all-reduce < 5ms per layer.
3. Output text совпадает с sequential (tolerance 1e-4).
4. E2E-тесты с реальной CUDA на self-hosted GPU runner'е.
5. `TPRUNTIME=cuda` env-flag активирует real backend, default — `StubTPRuntime`.
6. Build OK с `go build -tags "cuda llama_cublas"`.
7. Documentation для hardware setup.

---

## 5. P.4 — CI/CD scaffolding (1-2 дня)

### Цель

GitHub Actions pipeline для `go build`/`go test`/`go vet`/`golangci-lint` на каждом PR +
coverage badge. Это **P0** задача, потому что без CI любой merge может вернуть pre-existing
failures (PF-8, PF-9, ...).

### Предусловие — 7.1a Self-hosted runner

**Перед началом P.4** необходимо подготовить инфраструктуру — настроить Windows-машину
разработчика как GitHub Actions self-hosted runner (см. [roadmap §7.1a](2026-q3-roadmap.md#7-cicd-и-тестирование)).
**Почему:** проект использует cgo + custom C-bridge + Windows nvml build tag, а также
`c/llama.cpp` subtree, что делает GitHub-hosted runners неоптимальными (cold-cache
15–20 мин build llama.cpp, нет Windows nvml, ephemeral cache).

Self-hosted runner решает:
- **Persistent build cache** — incremental build за секунды вместо cold-cache.
- **Windows nvml тесты** — `internal/agent/nvml_unix.go` (build tag `nvml && windows`).
- **Stub-режим** — `c/bridge/bridge_stub.go` собирается без C-исходников llama.cpp.
- **Fallback на GitHub-hosted** — для случая когда self-hosted runner оффлайн.

**Файлы 7.1a:** `scripts/setup-runner.ps1` + `scripts/check-runner.ps1` + `docs/ci/self-hosted-runner.md`.
**Оценка 7.1a:** 0.5–1 день. **Блокирует P.4.**

### План реализации

#### Шаг 1 (день 1): GitHub Actions workflows

**Файлы:**
- `.github/workflows/ci.yml` (новый):
  ```yaml
  name: CI
  on:
    push:
      branches: [main, integration/q3-w3-4]
    pull_request:
  
  jobs:
    test:
      runs-on: ubuntu-latest
      steps:
        - uses: actions/checkout@v4
        - uses: actions/setup-go@v5
          with:
            go-version: '1.22'
        - name: Build
          run: |
            go build -tags llama_stub ./cmd/balancer/
            go build -tags llama_stub ./cmd/cppworker/
            go build -tags llama_stub ./cmd/agent/
        - name: Test
          run: |
            go test -race -tags llama_stub -timeout 120s ./internal/...
            go test -tags llama_stub -timeout 300s ./tests/...
            go test -tags llama_stub -timeout 60s ./cmd/cppworker/...
        - name: Vet
          run: |
            go vet -tags llama_stub ./...
        - name: golangci-lint
          uses: golangci/golangci-lint-action@v3
          with:
            version: v1.55
  
    coverage:
      runs-on: ubuntu-latest
      steps:
        - uses: actions/checkout@v4
        - uses: actions/setup-go@v5
          with:
            go-version: '1.22'
        - name: Coverage
          run: |
            go test -tags llama_stub -coverprofile=coverage.out -covermode=atomic ./internal/...
            go tool cover -func=coverage.out | tail -1
        - uses: codecov/codecov-action@v3
          with:
            file: coverage.out
  ```

- `.github/workflows/gpu.yml` (новый) — опциональный self-hosted runner для CUDA-тестов
  (см. P.3 шаг 4).

#### Шаг 2 (день 1): golangci-lint config

**Файлы:**
- `.golangci.yml` (новый):
  ```yaml
  linters:
    enable:
      - govet
      - errcheck
      - staticcheck
      - unused
      - gosimple
      - ineffassign
      - misspell
      - gofmt
      - goimports
  
  linters-settings:
    misspell:
      locale: US,Russian
  
  issues:
    exclude-rules:
      - path: _test\.go
        linters:
          - errcheck
  ```

#### Шаг 3 (день 2): Coverage badge + README update

**Файлы:**
- `README.md` — добавить badges в шапку:
  ```markdown
  [![CI Status](https://github.com/BarsSky/ollamalegion/workflows/CI/badge.svg)](https://github.com/BarsSky/ollamalegion/actions)
  [![codecov](https://codecov.io/gh/BarsSky/ollamalegion/branch/main/graph/badge.svg)](https://codecov.io/gh/BarsSky/ollamalegion)
  [![Go Report Card](https://goreportcard.com/badge/github.com/BarsSky/ollamalegion)](https://goreportcard.com/report/github.com/BarsSky/ollamalegion)
  ```
- `.github/workflows/release.yml` (новый) — при push тега создаёт GitHub Release с
  бинарниками (balancer, cppworker, agent).

#### Шаг 4 (день 2): Coverage reporting в PR

- Codecov integration — автоматический comment в PR с coverage diff.
- Fail PR если coverage < 75% для `rpccoordinator`, `rptensor`, `balancer`
  (Q3 метрика успеха).

### Acceptance criteria

1. Каждый PR запускает CI (build + race-тесты + vet + lint).
2. CI fail если любой тест fail (включая PF regression).
3. Coverage badge виден в README.
4. PR comment с coverage diff от Codecov.
5. golangci-lint проходит без warnings.
6. Build OK для всех 3 бинарников.
7. Coverage >75% для `rpccoordinator`, `rptensor`, `balancer`.

---

## 6. Зависимости между P.1-P.4

```
P.4 (CI/CD) ──────────────────────┐
                                  │
                                  ▼
P.1 (rpc_coordinator) ──────► production ──────► 1.0 release
                                  │
P.2 (virtual_router) ───────────┤
                                  │
                                  ▼
                              release 1.0
                                  │
                                  ▼
P.3 (real ggml/NCCL) ──────► research-grade v1.1+
```

- **P.4 (CI/CD)** — независим, должен быть первым (1-2 дня).
- **P.1 (rpc_coordinator)** — независим от P.2, может идти параллельно (5-8 дней).
- **P.2 (virtual_router)** — независим от P.1, может идти параллельно (5-7 дней).
- **P.3 (real ggml)** — research, post-1.0 (10-15 дней, отдельный timeline).

**Рекомендуемый порядок:**
1. **Неделя 1-2**: P.4 CI/CD + начать P.1 rpc_coordinator (параллельно).
2. **Неделя 3-4**: P.1 финализация + начать P.2 virtual_router.
3. **Неделя 5-6**: P.2 финализация + bugfixes + 1.0 release prep.
4. **После 1.0** (Q4 2026+): P.3 real ggml.

---

## 7. Рекомендуемый план работ (3-4 месяца)

### Месяц 1 (июль 2026) — UI/UX финализация + CI scaffolding
1. **Неделя 1-2**: Session F (F.1+F.4, F.2, F.3 — UI/UX quick wins).
2. **Неделя 3**: P.4 CI/CD scaffolding.
3. **Неделя 4**: багфиксы из CI feedback, polish.

### Месяц 2 (август 2026) — production-ready режимы
1. **Неделя 1-2**: P.1 `rpc_coordinator` production mode (5-8 дней).
2. **Неделя 3-4**: P.2 `virtual_router` production mode (5-7 дней).

### Месяц 3 (сентябрь 2026) — release 1.0
1. **Неделя 1-2**: bugfixes, regression testing, documentation review.
2. **Неделя 3**: release candidates (RC1, RC2), community testing.
3. **Неделя 4**: **RELEASE 1.0** 🎉

### Месяц 4+ (Q4 2026) — research-grade v1.1+
1. P.3 Real ggml/NCCL integration.
2. Pipeline Parallelism (roadmap 4.1).
3. Expert Parallelism для MoE (4.2).
4. Continuous batching (4.5).
5. Security hardening (mTLS, rate limiting, audit).

---

## 8. Acceptance criteria (Production-ready целиком)

1. **P.1**: `OperatingMode=rpc_coordinator` end-to-end inference работает через RPC pipeline,
   circuit breaker защищает от cascading failures, streaming через SSE passthrough.
2. **P.2**: `virtual_router` режим с round-robin/least-loaded selection, CRUD API,
   WebUI страница, metrics.
3. **P.4**: каждый PR запускает CI, coverage >75% для критических пакетов,
   badges в README.
4. Все P.1-P.2 тесты PASS (~40+ unit + e2e).
5. Build OK: `go build -tags llama_stub ./cmd/balancer/`.
6. CHANGELOG секции для каждого P.x.
7. Documentation обновлена (`docs/rpc-coordinator.md`, `docs/configuration.md`,
   `docs/virtual-router.md`).
8. Real production config (`deployments/.env.rpc.example`, `docker-compose.rpc.yml`)
   поднимается одной командой.

---

## 9. Связанные документы

- [plans/2026-q3-roadmap.md](2026-q3-roadmap.md) — главный roadmap (sections 3, 7).
- [plans/README.md](README.md) — общий статус (Q3 W3-4 ЗАКРЫТ).
- [plans/2026-q3-session-f-quick-wins.md](2026-q3-session-f-quick-wins.md) — предыдущий этап (UI/UX).
- [plans/b8-tensor-parallelism-plan.md](b8-tensor-parallelism-plan.md) — Tensor Parallelism (для P.3).
- [docs/rpc-coordinator.md](../docs/rpc-coordinator.md) — B1-B8 детали.

---

## 10. Метрики успеха 1.0 release

- ✅ Все user-facing фичи реализованы (Q3 W3-4 + Session F).
- ✅ Production-ready режимы (rpc_coordinator, virtual_router) end-to-end работают.
- ✅ CI/CD pipeline зелёный на каждом PR.
- ✅ Coverage >75% для критических пакетов.
- ✅ Real production configs (`docker-compose.rpc.yml`) поднимаются одной командой.
- ✅ Documentation полная (api.md, configuration.md, runbook-tools.md, troubleshooting.md).
- ✅ Все pre-existing failures закрыты (PF-1..PF-7).
- ✅ Bundle build работает (`./scripts/build-containers.sh`).
- ✅ WebUI dark+light theme переключается без багов.
- ✅ OpenAI-compatible API для Cline/OpenWebUI/Roo Code без regression.

**После выполнения всех AC → RELEASE 1.0** 🚀

---

**Подготовлено:** 2026-06-28 (после Session E, commit `dc49912`)
**Ветка:** `integration/q3-w3-4` → новая ветка `feature/q3-production`
**Период:** август-сентябрь 2026 (3 месяца до 1.0)