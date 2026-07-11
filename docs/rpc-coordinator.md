# RPC Coordinator Mode

Production-ready distributed inference mode (P.1) для Ollama Legion balancer.

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

- `docs/phase-8-rpc-coordinator.md` — implementation plan.
- `plans/2026-q3-production-ready-plan.md` — overall production-ready plan.
- `CHANGELOG.md` — version history.
- `internal/balancer/rpc_coordinator_dispatcher.go` — implementation.
- `internal/rpccoordinator/coordinator.go` — coordinator.
- `internal/rpccoordinator/worker_client.go` — worker client.
