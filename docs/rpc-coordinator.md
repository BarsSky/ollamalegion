# External RPC Coordinator (Вариант B)

## Обзор

External RPC Coordinator — распределённый inference engine для моделей, не помещающихся на одну GPU. Координатор управляет worker'ами, каждый из которых обслуживает срез (slice) модели — набор слоёв трансформера.

## Архитектура

```
Client → Balancer → ModelCoordinator
                      ├── Split Logic
                      ├── Pipeline Manager (sequential/parallel/tree)
                      ├── Merge Logic
                      └── KV Cache Manager
                            ↓
              Worker-1 (слои 1-40)  ←→  Worker-2 (слои 41-80)
```

## Компоненты

### Core Coordinator (`internal/rpccoordinator/coordinator.go`)

- `ModelCoordinator` — основной движок распределённого inference
- `DistributedModel` — модель, разбитая на срезы по worker'ам
- `LayerSlice` — диапазон слоёв на конкретном worker'е
- `InferenceJob` — контекст выполнения запроса

### Worker Client (`internal/rpccoordinator/worker_client.go`)

- `WorkerClient` — HTTP клиент для взаимодействия с worker'ом
- API worker'а:
  - `POST /rpc/health` — health check
  - `POST /rpc/load` — загрузка среза модели
  - `POST /rpc/unload` — выгрузка среза
  - `POST /rpc/infer` — inference среза
  - `GET /rpc/metrics` — метрики worker'а

### Split/Merge Engine (`internal/rpccoordinator/splitmerge.go`)

- `Splitter` — разбиение prompt на сегменты
  - Strategies: `sentence`, `paragraph`, `token`, `equal`
- `Merger` — сборка output token'ов
  - Strategies: `concat`, `last`, `join_json`
- `ParallelMergeContext` — контекст параллельного merge'а

### KV Cache Manager (`internal/rpccoordinator/kv_cache.go`)

- `DistributedKVCache` — распределённый KV cache
- `KVCacheShard` — шард кеша для сессии
- LRU eviction, TTL cleanup, worker-to-worker sync

## Конфигурация

```json
{
  "balancing": {
    "rpcCoordinator": {
      "enabled": false,
      "coordinatorURL": "http://localhost:18090",
      "workerPort": 18080,
      "timeout": "30s",
      "protocol": "http",
      "maxRetries": 3
    }
  }
}
```

## Pipeline Execution

1. Координатор получает запрос от балансировщика
2. Сортирует срезы по ordinal (startLayer)
3. Последовательно вызывает worker'ов:
   - Slice N получает output от Slice N-1 как input
   - Каждый worker обрабатывает свои слои
4. Собирает статистику (latency, errors)
5. Возвращает финальный output клиенту

## Retry Logic

- При ошибке среза — retry с exponential backoff
- `maxRetries` из конфигурации (default: 3)
- Если все retries исчерпаны — возвращает ошибку

## Тесты

Файл: `internal/rpccoordinator/coordinator_test.go`

| Тест | Описание |
|------|----------|
| `TestNewModelCoordinator` | Создание координатора |
| `TestRegisterWorker_*` | Регистрация worker'ов |
| `TestRegisterDistributedModel_*` | Регистрация моделей |
| `TestInfer_*` | Inference pipeline |
| `TestInfer_PipelineTwoSlices` | Pipeline из 2 срезов |
| `TestInfer_WorkerFailure` | Failover при ошибке |
| `TestConcurrentInferAndCancel` | Race conditions |

## Статус реализации

| Компонент | Статус | Покрытие тестами |
|-----------|--------|------------------|
| Core Coordinator | ✅ Реализован | 25 тестов |
| Worker Client | ✅ Реализован | ✅ |
| Split/Merge | ✅ Реализован | partial |
| KV Cache | ✅ Реализован | partial |
| Balancer Integration | ✅ Реализован | ✅ |
| Configuration | ✅ Реализован | ✅ |

## Ограничения

- Требуется кастомный worker backend (Ollama не поддерживает частичную загрузку)
- Pipeline parallelism добавляет latency (N сетевых вызовов для N срезов)
- KV cache sync между слоями — опционален, добавляет overhead

## Roadmap

1. **Short-term:** Worker HTTP server (standalone или sidecar)
2. **Medium-term:** gRPC protocol для worker'ов
3. **Long-term:** Tensor parallelism (разбиение матриц внутри слоя)

---

## B1 — Worker HTTP Server (DONE 2026-06-26)

---

## B4 — Streaming Pipeline + KV-cache sync (DONE 2026-06-26)

### Что сделано

Реализованы:
1. **In-memory KV-store на worker'е** (`internal/rpcworker/kv_store.go`) —
   thread-safe хранилище KV-shard'ов с TTL (30m default) и capacity limit (1000).
2. **Реальная реализация /rpc/kv_sync и /rpc/kv_fetch** (раньше были 501 stub).
3. **SSE-streаming /rpc/infer** через query `?stream=true`
   (text/event-stream, чанки по 8 байт).

### KVStore API (in-memory на worker'е)

```go
type KVShard struct {
    SessionID, WorkerID string
    KeyTensor, ValueTensor []byte  // opaque B4
    SeqLen int
    CreatedAt, LastAccess time.Time
}

func NewKVStore(workerID string, cfg KVStoreConfig) *KVStore
func (k *KVStore) Save(shard *KVShard) error
func (k *KVStore) Load(sessionID string) *KVShard
func (k *KVStore) Delete(sessionID string) bool
func (k *KVStore) List() []string
func (k *KVStore) Sweep() int  // удаляет просроченные по TTL
func (k *KVStore) Stats() KVStoreStats
```

Сериализация: opaque bytes через base64 (`encoded: "key_b64:value_b64"`).
В production это будет safetensors/bincode сериализация llama.cpp cache.

### Контракт `POST /rpc/kv_sync`

```json
{
  "session_id": "abc-123",
  "seq_len": 42,
  "encoded": "aGVsbG86d29ybGQ=",
  "layers": [1, 2, 3, 4]
}
```

Response 200 OK:
```json
{ "status": "saved", "session_id": "abc-123", "seq_len": 42, "worker_id": "rpc-worker-1" }
```

### Контракт `GET /rpc/kv_fetch?session_id=X`

Response 200 OK:
```json
{
  "session_id": "abc-123", "worker_id": "rpc-worker-1", "seq_len": 42,
  "encoded": "aGVsbG86d29ybGQ=", "layers": [1,2,3,4],
  "created_at": "2026-06-26T20:30:00Z", "last_access": "2026-06-26T20:35:00Z"
}
```

Response 404 если shard не найден.

### SSE-streаming `/rpc/infer?stream=true`

Content-Type: `text/event-stream`. Каждый чанк (8 байт) — отдельный event:

```
event: start
data: {"worker_id":"rpc-worker-1","slice_id":"1-32","model":"llama-3-70b"}

event: token
data: {"token":"Hello","token_index":0}

event: token
data: {"token":", wor","token_index":1}

...

event: done
data: {"worker_id":"rpc-worker-1","slice_id":"1-32","tokens":12,"latency_ms":42,"status":"completed"}
```

### Edge cases (покрыто тестами)

- KVStore: 400 при пустом body / отсутствии session_id.
- KVStore: 404 при fetch несуществующего shard.
- KVStore: 507 Insufficient Storage при переполнении MaxEntries.
- KVStore: overwrite не считается за новый (Stats.Stores не ++).
- KVStore: Sweep удаляет просроченные по TTL.
- SSE-streаming: chunksize 8 байт; last `done` event всегда отправляется.
- handleInferStream: если ResponseWriter не поддерживает Flusher → 500.
- handleInferStream: client disconnect не паникует (Write возвращает error → return).

### Файлы

- `internal/rpcworker/kv_store.go` — новый, ~220 LOC (KVStore + KVShard + stats).
- `internal/rpcworker/server.go` — добавлено поле `kvStore` + `KVStore()` геттер.
- `internal/rpcworker/handlers.go` — реальные `handleKvSync` / `handleKvFetch`;
  добавлен `handleInferStream` для SSE.
- `internal/rpcworker/server_test.go` — обновлены B1-stub тесты (501 → 200/400/404);
  добавлены KVStore unit-тесты (Save/Load/Delete, overwrite, capacity, TTL).

### Acceptance criteria

- `go test -tags llama_stub -count=1 ./internal/rpcworker` — PASS (1.0s).
- `go test -tags llama_stub -count=1 ./internal/{rpcworker,rpccoordinator,api}` — PASS.
- KVStore: Save/Load/Delete/Sweep все работают.
- KVStore: overwrite не считается новым.
- KVStore: capacity (MaxEntries) → IsKVStoreFullError.
- /rpc/kv_sync: 400 без session_id, 200 с shard.
- /rpc/kv_fetch: 404 если нет shard, 200 с shard.
- /rpc/infer?stream=true: Content-Type: text/event-stream, chunks of 8 bytes.

### Что вне scope B4

- **Streaming через coordinator** (`coord.Infer()` возвращает SSE) — coordinator
  пока использует обычный JSON через `coord.Infer()`. Streaming-версия будет
  добавлена в B6 (Load Balancing) или в отдельной фазе, когда потребуется
  реальный pipeline parallelism (межсрезная передача hidden states).
- **Реальная llama.cpp cache file** (safetensors/bincode) — на B4 opaque bytes.

---

## B3 — Heartbeat & Auto-Discovery (DONE 2026-06-26)

### Что сделано

`HeartbeatLoop` — фоновая горутина в `ModelCoordinator`, которая
периодически вызывает `WorkerClient.HealthCheck()` для каждого
зарегистрированного worker'а и помечает unhealthy после превышения
порога неудач.

### Архитектура

- `HeartbeatConfig{Interval, UnhealthyThreshold}` — настройки (defaults: 30s, 3).
- `HeartbeatLoop` — фоновый ticker (Start/Stop идемпотентны).
- `heartbeatState{consecutiveFailures, lastSuccessAt, lastFailureAt}` — per-worker.

### Метрики heartbeat'а

| Поле | Описание |
|---|---|
| `consecutive_failures` | Сколько подряд неудач HealthCheck для worker'а |
| `last_success_at` | RFC3339 последнего успешного check (или 0) |
| `last_failure_at` | RFC3339 последнего failed check (или 0) |
| `unhealthy_threshold` | Сколько failures подряд → worker помечается unhealthy |

### Поведение

1. Каждые `Interval` секунд heartbeat делает `HealthCheck()` для всех worker'ов.
2. **Healthy check** (`ok=true`): `consecutive_failures=0`, `last_success_at=now`.
3. **Failed check** (`ok=false`): `consecutive_failures++`, `last_failure_at=now`.
4. После `consecutive_failures >= UnhealthyThreshold`: `worker.setHealthy(false)`,
   балансер видит unhealthy state через `WorkerClient.IsHealthy()`.
5. При восстановлении (`ok=true` после failures): автоматически возвращается healthy.

### Файлы

- `internal/rpccoordinator/heartbeat.go` — `HeartbeatLoop` (~250 LOC).
- `internal/rpccoordinator/heartbeat_test.go` — 8 unit-кейсов (build tag `llama_stub`).

### Acceptance criteria

- `go test -tags llama_stub -count=1 -run TestHeartbeat ./internal/rpccoordinator` — все 8 кейсов PASS.
- `go test -tags llama_stub -count=1 ./internal/{rpcworker,rpccoordinator,api}` — все три пакета PASS.
- Start/Stop идемпотентны (повторный вызов — no-op).
- Closed server помечает worker unhealthy при `RegisterWorker` (initial health check).
- Healthy worker остаётся healthy после 5+ tick'ов.

### Что вне scope B3

- **Auto-discovery через UDP multicast** — отдельная задача, не реализована.
- **Coordinator → Worker push-уведомления** (например, при reload config) — только
  heartbeat (Worker → Coordinator).

---

## B2 — RPC Management API (DONE 2026-06-26)

### Что сделано

Добавлены management endpoints в `internal/api/handlers_rpc.go`,
позволяющие управлять worker'ами и distributed моделями **через балансировщик**
без прямого доступа к worker'ам.

### Endpoints

| Method | Path | Назначение |
|---|---|---|
| GET    | `/api/v1/rpc/workers`              | Список всех зарегистрированных worker'ов |
| POST   | `/api/v1/rpc/workers/register`     | Зарегистрировать или обновить worker |
| GET    | `/api/v1/rpc/workers/{id}`         | Инфо по worker'у (host, port, slice_layers) |
| DELETE | `/api/v1/rpc/workers/{id}`         | Удалить worker (идемпотентно) |
| GET    | `/api/v1/rpc/models`               | Список distributed моделей (через coordinator) |
| POST   | `/api/v1/rpc/models/{name}/infer`  | Inference через coordinator (вызывает WorkerClient.InferSlice) |

Все endpoints требуют `X-API-Token` (через `AuthMiddleware`) и подвержены
rate-limiting (`RateLimitMiddleware`).

### Контракт `POST /api/v1/rpc/workers/register`

```json
{
  "worker_id":    "gpu-1",
  "host":         "10.0.0.5",
  "port":         18080,
  "slice_layers": "1-40"
}
```

Ответ:
```json
{ "status": "registered", "worker_id": "gpu-1" }
```

### Контракт `POST /api/v1/rpc/models/{name}/infer`

```json
{
  "prompt":     "Hello, world!",
  "session_id": "abc-123",
  "params":     {"max_tokens": "256"}
}
```

Ответ (200 OK):
```json
{
  "model_name":  "llama-3-70b",
  "output":      "...",
  "total_ms":    1234,
  "slice_stats": [...]
}
```

### HTTP-коды ответов

- **200 OK** — успех.
- **400 Bad Request** — невалидный JSON / отсутствуют обязательные поля.
- **404 Not Found** — worker или distributed модель не найдена.
- **405 Method Not Allowed** — неподдерживаемый метод.
- **503 Service Unavailable** — RPC Coordinator отключён в конфиге (`Balancing.RpcCoordinator.Enabled=false`).

### Edge cases

- **DELETE /workers/{id}** для несуществующего worker'а — возвращает 200 (идемпотентно).
- **POST /infer** для незарегистрированной distributed модели — возвращает 404.
- **GET /infer** — возвращает 405 (только POST поддерживается).
- **RPC Coordinator = nil** — все endpoints возвращают 503 с `error: "rpc_disabled"`.

### Файлы

- `internal/api/handlers_rpc.go` — handlers (worker list/register/get/delete, models list, infer).
- `internal/api/routes.go` — регистрация роутов с `AuthMiddleware + RateLimitMiddleware`.
- `internal/api/handlers_rpc_test.go` — 13 unit-кейсов (включая subtests):
  - 6 subtests для проверки 503 на каждом endpoint при RPC disabled.
  - GET empty list / bad JSON / missing fields / invalid port.
  - Worker item: GET 404 / DELETE idempotent / PUT 405.
  - Models: GET empty / Infer GET 405 / Infer missing prompt / Infer model not found.

### Acceptance criteria

- `go test -tags llama_stub -count=1 -run TestRpc ./internal/api` — все PASS.
- `go test -tags llama_stub -count=1 ./internal/{rpcworker,rpccoordinator,api}` — все три пакета PASS.
- При RPC disabled: все 6 endpoints возвращают 503 с `error: "rpc_disabled"`.
- При RPC enabled: bad JSON / missing fields → 400; missing model → 404; bad method → 405.

---

## B1 — Worker HTTP Server (DONE 2026-06-26)

### Что сделано

Реализован **минимальный viable worker** для RPC Coordinator:

- `cmd/rpcworker/main.go` — бинарь с поддержкой флагов и ENV-переменных.
- `internal/rpcworker/{config,model_manager,server,metrics,handlers}.go` — пакет worker'а.
- 7 эндпоинтов: `/rpc/{health,load,unload,infer,metrics,kv_sync,kv_fetch}`.
- Build с `-tags llama_stub` (без реальной llama.cpp).
- 22+ unit-теста + 8+ e2e-теста (httptest.Server + ModelCoordinator).
- `docker/rpcworker/Dockerfile` + `entrypoint.sh`.
- `deployments/docker-compose.rpc.yml` — стек из coordinator + 2 workers.

### Использование

```bash
# Сборка stub-бинарника
go build -tags llama_stub -o rpcworker ./cmd/rpcworker

# Запуск
RPC_WORKER_PORT=18080 RPC_WORKER_LAYERS=1-40 ./rpcworker

# Docker
cd deployments
docker compose -f docker-compose.rpc.yml up -d --build

# Healthcheck
curl http://localhost:18080/rpc/health
# {"status":"ok","worker_id":"...","version":"rpcworker-0.1.0-stub",
#  "loaded_slices":null,"gpu_info":{"available":false,...},"stub_mode":true,...}
```

### ENV-флаги

| Env / flag | Default | Назначение |
|---|---|---|
| `RPC_WORKER_HOST` (`--host`) | `0.0.0.0` | Адрес прослушивания |
| `RPC_WORKER_PORT` (`--port`) | `18080` | HTTP-порт |
| `RPC_WORKER_ID` (`--worker-id`) | `os.Hostname()` | Идентификатор |
| `RPC_WORKER_LAYERS` (`--layers`) | `""` (все) | Слои, например `1-40` |
| `RPC_WORKER_COORDINATOR_URL` | `""` (off) | URL для heartbeat-регистрации |
| `RPC_WORKER_TOKEN` (`--token`) | `""` | Bearer-токен для защищённых эндпоинтов |
| `RPC_WORKER_MODELS_DIR` | `./models` | Где искать `.gguf` |
| `RPC_WORKER_HEALTHCHECK_INTERVAL` | `30s` | Интервал heartbeat |
| `RPC_WORKER_WRITE_TIMEOUT` | `5m` | HTTP write timeout |
| `RPC_WORKER_LOG_LEVEL` | `info` | Уровень логирования |
| `RPC_WORKER_STUB_MODE` (`--stub`) | `false` | Принудительный stub |

### Известные ограничения (явно вне scope B1)

- Реальный pipeline parallelism с передачей hidden states — задача **B4** (Streaming Pipeline).
- `/rpc/kv_sync` и `/rpc/kv_fetch` возвращают **501 not_implemented** с пометкой `phase=B1, next=B4`.
- На B1 worker возвращает полный output (как если бы это был последний срез в pipeline). Межсрезная передача — в B4.

### Что дальше (B2-B8)

- **B2** (3-5 дней): Management API `/api/v1/rpc/workers`, `/api/v1/rpc/models`, etc.
- **B3** (2-3 дня): Heartbeat & Auto-Discovery.
- **B4** (5-7 дней): Streaming Pipeline + KV-cache sync.
- **B5** (5-7 дней): gRPC протокол.
- **B6** (1-2 дня): Load Balancing между срезами.
- **B7** (3-5 дней): Prometheus метрики + WebUI.
- **B8** (20-30 дней): Tensor Parallelism (требует custom backend).

## B5 — gRPC-style binary protocol через net/rpc (DONE Session 9)

### Что сделано

Параллельно с HTTP (`/rpc/*`) worker'ы теперь могут слушать **net/rpc (gob)** — бинарный
протокол, совместимый по семантике с HTTP API. Это B5-stub (без `google.golang.org/grpc`):

- Не требует `protoc-gen-go` или `google.golang.org/grpc` (которых нет в `go.mod`).
- Даёт binary wire protocol через `encoding/gob`.
- Те же 7 методов, что и HTTP (`HealthCheck`, `LoadSlice`, `UnloadSlice`, `Infer`,
  `Metrics`, `KvSync`, `KvFetch`) — реализованы через единый `WorkerRPCService`.
- Поддерживает lazy reconnect на клиенте (на любую RPC-ошибку соединение закрывается,
  следующий вызов переоткрывает).
- HTTP и net/rpc могут работать одновременно на одном worker'е (разные порты).

### Зачем stub, а не настоящий gRPC

- `google.golang.org/grpc` в `go.mod` отсутствует, и его нельзя просто добавить —
  он тянет за собой `protoc`-генерацию .pb.go файлов и сложную кодогенерацию.
- `net/rpc/gob` даёт **ту же модель** (typed RPC + binary wire) с минимальным
  кодом. Покрывает ~80% use-cases B5 (call/return).
- B5.real (grpc-go + protobuf) — отдельная задача после B8, когда появится
  production deployment с реальной llama.cpp.

### Протокол

```
Protocol: rpc-v0.1.0-stub
Transport: TCP
Encoding: encoding/gob
Service: WorkerRPCService
Methods:
  HealthCheck(args *HealthCheckArgs, reply *HealthCheckReply) error
  LoadSlice(args *LoadSliceArgs, reply *LoadSliceReply) error
  UnloadSlice(args *UnloadSliceArgs, reply *UnloadSliceReply) error
  Infer(args *InferArgs, reply *InferReply) error
  Metrics(args *MetricsArgs, reply *MetricsReply) error
  KvSync(args *KvSyncArgs, reply *KvSyncReply) error
  KvFetch(args *KvFetchArgs, reply *KvFetchReply) error
```

Все типы — в `pkg/protocol/rpc_protocol.go`.

### Архитектура

```
                     ┌────────────────────────────┐
                     │       WorkerServer         │
                     │  (rpcworker.WorkerServer)  │
                     │  HTTP /rpc/{health,...}    │
                     └─────────┬──────────────────┘
                               │ (общий Manager/Metrics/KVStore)
                     ┌─────────▼──────────────────┐
                     │    WorkerRPCService        │
                     │  (pkg/protocol)            │
                     │  7 net/rpc методов         │
                     └─────────┬──────────────────┘
                               │ gob over TCP
                     ┌─────────▼──────────────────┐
                     │    net/rpc.Server          │
                     │  port: HTTP_PORT + 1000    │
                     │  (default 19080)           │
                     └────────────────────────────┘

            ┌────────────────────────────────────────┐
            │     WorkerRPCClient (lazy reconnect)   │
            │  (internal/rpccoordinator)             │
            └────────────────────────────────────────┘
```

### Файлы

- `pkg/protocol/rpc_protocol.go` — `WorkerRPCService` + args/reply типы + `StartRPCServer()`.
- `internal/rpcworker/server.go` — helper-методы `WorkerID()`, `Version()`, `HandleInferRPC()`,
  `MetricsRPC()`, `KvStoreSaveRPC()`, `KvStoreLoadRPC()` (RPC-обёртки).
- `internal/rpcworker/metrics.go` — тип `MetricsSnapshot` (gob-сериализуемый аналог map).
- `cmd/rpcworker/main.go` — флаги `--rpc-host`, `--rpc-port` + ENV `RPC_WORKER_RPC_*`.
- `internal/rpccoordinator/worker_rpc_client.go` — `WorkerRPCClient` с lazy dial и reconnect.

### ENV / CLI-флаги

| ENV | Flag | Default | Что делает |
|---|---|---|---|
| `RPC_WORKER_RPC_HOST` | `--rpc-host` | = `--host` | RPC listen host |
| `RPC_WORKER_RPC_PORT` | `--rpc-port` | HTTP_PORT + 1000 | RPC listen port (18080 → 19080). 0 = отключить RPC |

### Acceptance criteria

1. `go build -tags llama_stub ./cmd/rpcworker` — exit 0.
2. `go test -tags llama_stub ./pkg/protocol/` — все unit-тесты PASS (9+).
3. `go test -tags llama_stub -run TestE2E_RPC_ ./internal/rpccoordinator/` — все e2e PASS (7+).
4. Worker слушает HTTP (18080) и net/rpc (19080) одновременно.
5. `WorkerClient` (HTTP) и `WorkerRPCClient` (gob) дают идентичные результаты для одинаковых операций.
6. Lazy reconnect: после перезапуска worker'а клиент автоматически переоткрывает соединение.
7. HTTP /rpc/metrics и RPC Metrics возвращают эквивалентные данные (HTTP — map, RPC — `MetricsSnapshot`).

### Файлы тестов

- `pkg/protocol/rpc_protocol_test.go` — 9 unit-тестов:
  - `TestProtocolVersion`
  - `TestStartRPCServer_HealthCheck`, `_LoadSlice_NoModel`, `_UnloadSlice_NotLoaded`,
    `_Infer_NotLoaded`, `_Metrics`, `_KvSync_KvFetch_RoundTrip`,
    `_KvFetch_NotFound`, `_KvSync_MissingSessionID`, `_ConcurrentCalls`, `_ListenerClosed`.
- `internal/rpccoordinator/e2e_rpc_test.go` — 7 e2e-тестов:
  - `TestE2E_RPC_HealthCheck`, `_FullPipeline`, `_TwoWorkersIsolated`,
    `_ConcurrentClients`, `_ReconnectAfterServerRestart`,
    `_LoadSliceInvalidModel`, `_MetricsContainsAllFields`.

### Известные ограничения (явно вне scope B5)

- **No streaming**: net/rpc поддерживает streaming через `net/rpc.Stream`, но B5-stub
  использует только call/reply. Для больших inference ответов (длинные генерации)
  клиент пока получает весь output за один `Infer`-вызов. SSE streaming по HTTP
  (`?stream=true`) остаётся основным путём для streaming.
- **No TLS**: net/rpc соединения plain TCP. В production обязательно обернуть в TLS.
- **No bidirectional streaming**: B5.real (grpc-go) добавит `BidiStreaming Infer`.
- **No server reflection**: клиент должен знать имена методов во время компиляции.

### Что вне scope B5 (next: B6-B8)

- **B6** (1-2 дня): Load Balancing между срезами.
- **B7** (3-5 дней): Prometheus метрики + WebUI.
- **B8** (20-30 дней): Tensor Parallelism (требует custom backend).
- **B5.real**: миграция на `google.golang.org/grpc` + protobuf (после B8).
