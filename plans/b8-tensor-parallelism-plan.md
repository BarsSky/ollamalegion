# B8 — Tensor Parallelism (ПЛАН для следующей сессии)

> **Статус:** ⬜ НЕ НАЧАТО (требует 20-30 дней реальной работы, не влезает в одну сессию)
> **Зависимости:** Custom backend (реальная llama.cpp интеграция с matrix sharding)
> **Предыдущая фаза:** B7 — Prometheus Metrics + WebUI (DONE Session 11, commit `3a03b88`)

---

## 1. Контекст и мотивация

### Что такое Tensor Parallelism (TP)?

В текущей реализации (Pipeline Parallelism, B1-B6) **каждый worker обслуживает целые слои** модели. Для 70B модели:
- Worker 1: слои 1-25
- Worker 2: слои 26-50
- Worker 3: слои 51-70
- Latency = O(N) (sequential pipeline)

В Tensor Parallelism **каждый worker обслуживает часть матриц внутри каждого слоя**:
- Worker 1: слои 1-70, но только Q/K/V/O матрицы с rank=0 (column split)
- Worker 2: слои 1-70, но только Q/K/V/O матрицы с rank=1
- Все workers параллельно обрабатывают ОДИН и тот же input
- Latency = O(1) (все workers работают одновременно)
- Reduce step: агрегация частей output

### Реальные примеры

- **Megatron-LM** (NVIDIA) — column + row split для QKV/MLP.
- **tensor_parallel в llama.cpp** (`-ts` параметр) — splits tensors across multiple GPUs.
- **DeepSpeed ZeRO-Inference** — partition optimizer state + activations.

### Зачем нужен B8

Pipeline parallelism не использует GPU workers параллельно — bottleneck на самом медленном worker'е. Tensor parallelism даёт:
- **Линейное ускорение** по числу workers (latency ~1/workers).
- **Эффективное использование** нескольких GPU (каждая обрабатывает свой shard).
- **Низкая latency** для интерактивных use-cases (Cline/OpenWebUI streaming).

---

## 2. Текущее состояние (что уже есть после B1-B7)

- ✅ `internal/rpcworker/server.go` — `WorkerServer` с `/rpc/infer` endpoint.
- ✅ `internal/rpcworker/handlers.go` — `handleInfer` (synchronous, single slice).
- ✅ `internal/rpcworker/handleInferStream` (B4) — SSE streaming.
- ✅ `internal/rpcworker/kv_store.go` — KV-cache shard storage.
- ✅ `internal/rpccoordinator/coordinator.go` — `executePipeline` (sequential pipeline).
- ✅ `internal/rpccoordinator/selector.go` (B6) — `LeastLoadedSelector` для pipeline.
- ❌ **Нет**: TP sharding, all-reduce, split-by-rank в coordinator.

---

## 3. Архитектурный дизайн B8

### 3.1 Модель разделения (model partitioning)

```
Layer i:  Q/K/V/O + MLP_up + MLP_down matrices
            ↓ split по row/column ↓
Shard 0:  Q[0:25%], K[0:25%], V[0:25%], O[0:25%], MLP_up[0:25%], MLP_down[full]
Shard 1:  Q[25:50%], K[25:50%], V[25:50%], O[25:50%], MLP_up[25:50%], MLP_down[full]
Shard 2:  Q[50:75%], K[50:75%], V[50:75%], O[50:75%], MLP_up[50:75%], MLP_down[full]
Shard 3:  Q[75:100%], K[75:100%], V[75:100%], O[75:100%], MLP_up[75:100%], MLP_down[full]
```

Attention: column-parallel (split Q/K/V → each shard имеет свой head).
MLP: row-parallel для up_projection, column-parallel для down_projection.
All-reduce между слоями.

### 3.2 Coordinator ↔ Worker контракт (новый)

```
┌────────────────────────┐                    ┌────────────────────────┐
│ TensorCoordinator      │                    │ Worker (rank=0..N-1)   │
│                        │  TP_InferRequest   │                        │
│  - plan tensor graph   │ ─────────────────→ │  - holds shard i       │
│  - schedule per layer  │                    │  - runs layer          │
│  - barrier between      │  TP_ShardResult    │  - returns shard output│
│  - all-reduce           │ ←───────────────── │                        │
└────────────────────────┘                    └────────────────────────┘
```

**TP_InferRequest:**
```json
{
  "model_name": "llama-3-70b",
  "prompt": "...",
  "tier_count": 4,
  "rank": 0,                          // which shard this worker handles
  "layer_range": "1-80",
  "input": "<base64 hidden states>",
  "session_id": "abc",
  "kv_shard": "<base64 KV cache>"
}
```

**TP_ShardResult:**
```json
{
  "rank": 0,
  "output": "<base64 partial output>",
  "kv_shard": "<base64 partial KV cache>",
  "needs_reduce": true,
  "needs_gather": true
}
```

### 3.3 PipelineCoordinator (новый orchestrator)

```go
type TensorParallelCoordinator struct {
    // ... поля аналогично ModelCoordinator ...
    shardedModel *ShardedModel           // graph: layer → [shards per layer]
}

func (c *TensorParallelCoordinator) Infer(ctx, req) (*Response, error) {
    // 1. Шлём параллельно через goroutines каждому worker'у (rank).
    // 2. Barrier после каждого layer (sync.WaitGroup).
    // 3. All-reduce: агрегация partial outputs (пока — concat bytes, в real — сумма).
    // 4. Broadcast результата следующему layer.
}
```

---

## 4. Пошаговый план реализации (B8)

Этапы упорядочены по сложности. Каждый этап = 1 под-сессия с коммитом.

### Этап B8.1 — Domain types и ShardedModel (3-5 дней, ~1500 LOC)

**Цель:** базовая структура данных для tensor parallelism без реальной математики.

**Файлы:**
- `internal/rptensor/sharded_model.go` — `ShardedModel`, `TensorShard`, `LayerPartitions`.
- `internal/rptensor/sharded_model_test.go` — unit-тесты (5-8 кейсов).

**API:**
```go
type PartitionStrategy string
const (
    ColumnPartition  PartitionStrategy = "column"
    RowPartition     PartitionStrategy = "row"
    MegatronPartition PartitionStrategy = "megatron"  // column + row
)

type TensorShard struct {
    Rank        int                    // 0..N-1
    Layer       int                    // 0..L-1
    MatrixName  string                 // "q_proj", "k_proj", "v_proj", "o_proj", "mlp_up", "mlp_down"
    Shape       []int                  // [rows, cols] of the shard
    Strategy    PartitionStrategy
}

type ShardedModel struct {
    Name         string
    HiddenSize   int                    // e.g., 8192 for 70B
    NumLayers    int                    // e.g., 80
    NumHeads     int                    // e.g., 64
    WorldSize     int                    // number of ranks (TP degree)
    LayerShards  map[int][]TensorShard  // layer_idx → shards for all ranks
}

func (m *ShardedModel) Partition(strategy PartitionStrategy, worldSize int) error
func (m *ShardedModel) ShardForRank(layer, rank int) []TensorShard
```

**Acceptance criteria:**
- Megatron partition для 8B модели (hidden=4096, layers=32, heads=32) с world_size=4 → каждый rank получает 1/N каждой матрицы.
- Column partition: матрицы 4096x4096 делятся на 4 шарда по 1024x4096.
- Unit-тесты: `TestMegatronPartition_8B_4GPUs`, `TestColumnPartition_4096x4096`.

**Commit:** `feat(rptensor): B8.1 - ShardedModel + PartitionStrategy`

---

### Этап B8.2 — TensorParallelCoordinator skeleton (5-7 дней, ~2000 LOC)

**Цель:** coordinator с параллельными goroutines + barrier + all-reduce stub.

**Файлы:**
- `internal/rptensor/coordinator.go` — `TensorParallelCoordinator`, `LayerJob`, `RankResult`.
- `internal/rptensor/coordinator_test.go` — unit-тесты (5-10 кейсов).
- `internal/rptensor/allreduce.go` — `AllReduceConcat`, `AllReduceSum` (stub).
- `internal/rptensor/allreduce_test.go` — unit-тесты.

**API:**
```go
type TensorParallelCoordinator struct {
    mu          sync.RWMutex
    workers     map[int]*WorkerClient      // rank → client
    shardedModel *ShardedModel
    metrics     *MetricsAggregator
}

type LayerJob struct {
    Layer       int
    RankResults map[int][]byte            // rank → partial output
    Barrier     *sync.WaitGroup
}

func (c *TensorParallelCoordinator) Infer(ctx context.Context, req TPInferRequest) (*TPInferResponse, error)
func (c *TensorParallelCoordinator) executeLayer(layer int, input []byte) ([]byte, error)
func (c *TensorParallelCoordinator) allReduce(partials map[int][]byte) ([]byte, error)
```

**Алгоритм `executeLayer`:**
1. Создать WaitGroup по числу ranks для layer'а.
2. Для каждого rank запустить goroutine: `worker.InferSlice(rank=layer)`.
3. Каждая goroutine пишет результат в `LayerJob.RankResults[rank]` (с мьютексом).
4. Barrier: `wg.Wait()`.
5. AllReduce результатов → финальный output для layer.
6. Передать output как input для следующего layer.

**Acceptance criteria:**
- 4 rank'а, 4 слоя: каждый layer обрабатывается параллельно (verify через `time.Now()` — должно быть O(layers), не O(layers*ranks)).
- AllReduceConcat: outputs конкатенируются в порядке rank.
- Unit-тесты: `TestParallelExecution_4Ranks_4Layers`, `TestAllReduceConcat_4Ranks`.

**Commit:** `feat(rptensor): B8.2 - TensorParallelCoordinator with parallel goroutines + barrier`

---

### Этап B8.3 — TP inference protocol (5-7 дней, ~800 LOC)

**Цель:** новые endpoints на rpcworker для TP: `/rpc/tp/infer`, `/rpc/tp/kv_sync`.

**Файлы:**
- `internal/rpcworker/handlers_tp.go` — `handleTPInfer`, `handleTPKvSync`, `handleTPKvFetch`.
- `internal/rpcworker/handlers_tp_test.go` — unit-тесты (5 кейсов).
- `internal/rpcworker/server.go` — регистрация `/rpc/tp/*` роутов.

**API worker'а:**
```
POST /rpc/tp/infer
Body: {
  "model_name": "...",
  "rank": 0,
  "world_size": 4,
  "layer": 5,
  "input": "<base64>",
  "kv_shard": "<base64>",
  "session_id": "abc"
}

Response 200:
{
  "rank": 0,
  "output": "<base64 partial output>",
  "kv_shard": "<base64 partial KV cache>",
  "needs_reduce": true
}
```

**Acceptance criteria:**
- Worker обслуживает только свой rank (определяется по `rank` в request).
- KV-cache shard хранится отдельно от обычного KV-cache (новый метод `KVStore.SaveShard`).
- Stub-mode: worker возвращает детерминированный partial output (длина = `input.len / world_size`).
- Unit-тесты: `TestHandleTPInfer_StubRank0`, `TestHandleTPInfer_ShardRouting`.

**Commit:** `feat(rpcworker): B8.3 - TP inference endpoints (/rpc/tp/*)`

---

### Этап B8.4 — WorkerClient с TP support (3-5 дней, ~600 LOC)

**Цель:** `WorkerClient.TPInferSlice(rank)` метод + unit-тесты.

**Файлы:**
- `internal/rpccoordinator/worker_client.go` — добавлены `TPInferSlice`, `TPKvSync`, `TPKvFetch`.
- `internal/rpccoordinator/worker_client_tp_test.go` — unit-тесты (4 кейса).

**API:**
```go
type TPInferRequest struct {
    ModelName string `json:"model_name"`
    Rank      int    `json:"rank"`
    WorldSize int    `json:"world_size"`
    Layer     int    `json:"layer"`
    Input     []byte `json:"input"`
    KVRank    []byte `json:"kv_shard,omitempty"`
    SessionID string `json:"session_id,omitempty"`
}

func (wc *WorkerClient) TPInferSlice(ctx context.Context, req TPInferRequest) (*TPInferResponse, error)
func (wc *WorkerClient) TPKvSync(ctx context.Context, sessionID string, rank int, shard []byte) error
func (wc *WorkerClient) TPKvFetch(ctx context.Context, sessionID string, rank int) ([]byte, error)
```

**Acceptance criteria:**
- `TPInferSlice` шлёт POST на `/rpc/tp/infer` worker'а.
- `TPKvSync/Fetch` работают с `KVStore.SaveShard/LoadShard` (rank-keyed).
- Unit-тесты через httptest mock worker.

**Commit:** `feat(rpccoordinator): B8.4 - WorkerClient.TPInferSlice + TPKvSync/Fetch`

---

### Этап B8.5 — End-to-end TP pipeline (5-7 дней, ~1000 LOC)

**Цель:** реальный e2e pipeline: input → 4 workers (rank 0-3) → 4 layers → output.

**Файлы:**
- `internal/rpccoordinator/e2e_tp_test.go` — e2e тесты.
- `internal/rpccoordinator/coordinator_tp_integration_test.go` — integration тесты.

**Сценарий e2e:**
1. Поднять 4 mock worker'а (httptest.Server) на разных портах.
2. Зарегистрировать их как ranks 0-3 в TensorParallelCoordinator.
3. Вызвать `TensorParallelCoordinator.Infer()` с реальным prompt.
4. Verify: каждый worker получил ровно 1 вызов per layer (4 слоя × 4 rank = 16 total calls).
5. Verify: latency < sequential × 4 (хотя бы 2x speedup).
6. Verify: финальный output содержит concatenated shards в правильном порядке.

**Acceptance criteria:**
- `TestE2E_TensorParallel_4Workers_4Layers` PASS.
- `TestE2E_TensorParallel_FailoverOnRankFailure` — если rank=2 упал, остальные продолжают (с degraded output).
- `TestE2E_TensorParallel_KVShardSync` — KV-cache shard корректно прокидывается между слоями.

**Commit:** `feat(rpccoordinator): B8.5 - E2E TP pipeline (4 workers × 4 layers)`

---

### Этап B8.6 — Mgatron sharding (5-7 дней, ~600 LOC)

**Цель:** настоящая логика Megatron-LM style sharding (column + row).

**Файлы:**
- `internal/rptensor/megatron.go` — `MegatronPartition` (column для QKV, row для MLP_up, column для MLP_down + all-reduce).
- `internal/rptensor/megatron_test.go` — unit-тесты (3-5 кейсов).

**Acceptance criteria:**
- Для 8B модели (hidden=4096): rank 0 имеет 1/4 каждой Q/K/V/O (1024 cols each), full MLP_down.
- Rank 0 + rank 1 + rank 2 + rank 3 → complete model.
- Unit-тесты: `TestMegatron_8B_4Ranks_QKV_ColumnSplit`.

**Commit:** `feat(rptensor): B8.6 - Megatron-style column + row partitioning`

---

### Этап B8.7 — Real llama.cpp integration (10-15 дней, ~3000 LOC)

**Цель:** реальная llama.cpp интеграция: tensor split + all-reduce через `ggml`.

**Файлы:**
- `c/bridge/tp_bridge.c` — C-bridge для tp_split / all_reduce.
- `c/bridge/tp_bridge.h` — header.
- `internal/rpcworker/tp_runtime.go` — Go wrapper над C-bridge.
- `internal/rpcworker/tp_runtime_test.go` — integration тесты (требуют llama.cpp).

**Зависимости:**
- llama.cpp source tree (c/llama.cpp).
- MPI или NCCL для all-reduce (overlapped с CUDA streams).
- ggml context с правильным `n_threads`/`n_gpu_layers` per rank.

**Acceptance criteria:**
- `TestTPReal_8B_4GPU` — реальная модель разбивается на 4 rank'а, инференс работает.
- Latency < 0.5x sequential (sub-linear scaling).
- Output text совпадает с sequential inference (до tolerance 1e-4).

**Commit:** `feat(rpcworker): B8.7 - Real llama.cpp TP integration (Megatron via ggml)`

---

### Этап B8.8 — API + WebUI для TP (3-5 дней, ~800 LOC)

**Цель:** новый endpoint `POST /api/v1/rpc/tp/infer` + WebUI панель tensor parallelism.

**Файлы:**
- `internal/api/handlers_rpc_tp.go` — `handleRPCModelTPInfer` endpoint.
- `internal/api/routes.go` — регистрация `/api/v1/rpc/tp/*`.
- `webui/tp-pipeline.html` — WebUI панель с визуализацией ranks/layers.

**Acceptance criteria:**
- Endpoint принимает `POST /api/v1/rpc/tp/infer` с теми же полями что и worker'ы.
- WebUI показывает grid: layers × ranks, цветом выделяет активные/завершённые.

**Commit:** `feat(api): B8.8 - TP API endpoint + WebUI tensor parallelism panel`

---

### Этап B8.9 — Docs + Session 12 commit (1 день, ~500 LOC)

**Цель:** документация и финальный commit.

**Файлы:**
- `docs/rpc-coordinator.md` — секция "B8 — Tensor Parallelism".
- `plans/README.md` — Session 12 в хронологии.
- `plans/b8-tensor-parallelism-plan.md` — отметить все этапы как DONE.

**Commit:** `docs(rpc-coordinator): B8 - Tensor Parallelism section (DONE Session 12)`

---

## 5. Acceptance criteria (B8 целиком)

1. `go build -tags llama_stub ./internal/rptensor/ ./internal/rpccoordinator/ ./internal/rpcworker/` — exit 0.
2. `go test -tags llama_stub ./internal/rptensor/ ./internal/rpccoordinator/` — все тесты PASS.
3. 4 worker'а (rank 0-3) могут параллельно обработать один input за O(1) latency.
4. Output rank'ов корректно агрегируется через all-reduce.
5. Megatron partition для 8B модели с world_size=4 — каждый rank получает ровно 1/4 параметров.
6. `POST /api/v1/rpc/tp/infer` через balancer возвращает корректный output.
7. WebUI `/tp-pipeline.html` показывает tensor parallelism grid.
8. С real llama.cpp: latency ≤ 0.5x sequential для 4-rank inference.

---

## 6. Что вне scope B8 (next: B5.real + R-5 + Q1 1.0)

- **Expert Parallelism** (MoE) — отдельная фаза, требует MoE routing.
- **Pipeline Parallel + TP hybrid** (3D parallelism) — отдельная фаза.
- **ZeRO optimizer state sharding** — это training, не inference.
- **NVLink/NVSwitch direct GPU-to-GPU** — инфраструктурный уровень.

---

## 7. Ожидаемый итог (когда B8 будет полностью завершён)

Roadmap to 1.0 — **полностью завершён**. После B8:

| Фаза | Задача | Статус |
|------|--------|--------|
| B1   | Worker HTTP Server | ✅ Session 5 |
| B2   | Management API | ✅ Session 6 |
| B3   | Heartbeat & Discovery | ✅ Session 7 |
| B4   | Streaming Pipeline | ✅ Session 8 |
| B5   | gRPC Protocol | ✅ Session 9 |
| B6   | Load Balancing | ✅ Session 10 |
| B7   | Prometheus Metrics | ✅ Session 11 |
| B8   | Tensor Parallelism | ⬜ Session 12 (planned) |
| R-5  | cocoindex.js llama_cpp | ⬜ Backlog (после 1.0) |

После B8 остаётся только **R-5 (backlog после 1.0)** + **Q1 1.0 release** (тесты, docker-compose, release notes).

---

## 8. Команды для новой сессии (start prompt)

Когда начнёте новую сессию для B8, передайте Cline следующий промпт:

```
B8 — Tensor Parallelism (DONE Session 12).

См. детальный план в plans/b8-tensor-parallelism-plan.md. План разбит на 9 этапов
(B8.1-B8.9), каждый со своими файлами и acceptance criteria.

Начни с B8.1 (ShardedModel + PartitionStrategy), потом B8.2 (TensorParallelCoordinator),
далее по плану. После каждого этапа — git commit с сообщением вида
"feat(rptensor): B8.N - <описание> (DONE Session 12)".

Build с тегом llama_stub. Unit-тесты обязательны. Документация в docs/rpc-coordinator.md
после B8.8, plans/README.md после B8.9.
```

---

**Подготовлено (WIP):** 2026-06-27 (Session 11 end)
**DONE (DONE Session 12):** 2026-06-27 — все 9 этапов реализованы (B8.1–B8.9). См. подробности в `docs/rpc-coordinator.md` § B8 и `plans/README.md` § 5.
**Roadmap to 1.0 — полностью завершён** (B1–B8 все DONE).
