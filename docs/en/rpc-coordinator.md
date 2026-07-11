# RPC Coordinator Mode

Production-ready distributed inference mode (P.1) for Ollama Legion balancer.

## Overview

`rpc_coordinator` mode routes inference requests through `ModelCoordinator`,
which orchestrates a pipeline of multiple workers. Each worker serves a
slice of the model (a range of layers). This provides:

- **Distribution of model layers** across multiple workers (pipeline parallelism).
- **Failover** between candidates per-slice via selector.
- **Circuit breaker** per worker (protection against cascading failures).
- **Streaming SSE passthrough** for both Ollama and OpenAI formats.
- **Auth middleware** — token-based protection for inference paths.

## Architecture

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

## Configuration

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
      "workers": ["worker-1:18092", "worker-2:18092"],
      "circuitBreaker": {
        "failureThreshold": 5,
        "successThreshold": 1,
        "resetTimeoutMs": 30000
      }
    }
  }
}
```

### Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Activate RPC coordinator mode |
| `embedded` | bool | `false` | Spawn `ModelCoordinator` in-process in the balancer |
| `coordinatorUrl` | string | `""` | External coordinator URL (when `embedded=false`) |
| `workers` | []string | `[]` | Pre-registered worker URLs |
| `failoverPolicy` | string | `"circuit_breaker"` | `circuit_breaker` / `retry` / `fail_fast` |
| `requestTimeout` | duration | `30s` | Non-streaming inference timeout |
| `streamTimeout` | duration | `5m` | Streaming inference timeout |

### Circuit Breaker (CB)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `failureThreshold` | int | `5` | Failures before opening CB |
| `successThreshold` | int | `1` | Successes in HalfOpen to close CB |
| `resetTimeoutMs` | int | `30000` | Time before HalfOpen transition (ms) |

States: **Closed** → (N failures) → **Open** → (cooldown) → **HalfOpen** → (success) → **Closed**.

### Worker registration

Workers are registered dynamically via the `RegisterWorker` API, or
pre-registered through the `workers` array in the config. Each worker has:

```json
{
  "workerId": "worker-1",
  "host": "127.0.0.1",
  "port": 8001,
  "sliceLayers": "1-16"
}
```

### Distributed model

Each model is registered in the coordinator via `RegisterDistributedModel`:

```go
coord.RegisterDistributedModel("llama-70b", "Distributed 70B",
    []rpccoordinator.LayerSlice{
        {StartLayer: 1, EndLayer: 40, WorkerID: "worker-1"},
        {StartLayer: 41, EndLayer: 80, WorkerID: "worker-2"},
    })
```

This can be done through the `/api/v1/rpc/models` API endpoint or in code.

## Endpoints

RPC coordinator mode intercepts the following endpoints (when enabled + model is registered):

| Endpoint | Method | Streaming | Format |
|----------|--------|-----------|--------|
| `/api/generate` | POST | yes (NDJSON) | Ollama |
| `/api/ollama/generate` | POST | yes (NDJSON) | Ollama alias |
| `/api/chat` | POST | yes (NDJSON) | Ollama |
| `/api/ollama/chat` | POST | yes (NDJSON) | Ollama alias |
| `/v1/chat/completions` | POST | yes (SSE) | OpenAI |
| `/v1/completions` | POST | yes (SSE) | OpenAI legacy |

The dispatcher also accepts internal endpoints:
- `POST /api/v1/rpc/models` — register distributed model
- `POST /api/v1/rpc/workers` — workers visibility (legacy)

## Behaviors

### Path routing (`ShouldRoute`)

The dispatcher intercepts a request only if ALL of these are true:
1. `mode == rpc_coordinator` (operating mode is set)
2. `dispatcher != nil` (dispatcher is wired)
3. Path matches Ollama or OpenAI inference path
4. Model is registered with the coordinator
5. **At least one** worker's CB is not `Open`

### Per-worker circuit breaker

Each worker has its own CB. The dispatcher:
- Records CB state from `X-Worker-Stats` header
- For each slice, picks the candidate via `selector.SelectWorkersForSlice(slice)`
- Skips workers with `Open` CB
- After each request, updates CB based on the response status

### Error mapping

| Backend status | Dispatcher response | Notes |
|----------------|---------------------|-------|
| 2xx | 200 + body passthrough | Success |
| 4xx (except 408, 429) | passthrough | Client error — don't retry |
| 408, 429 | 502 | Retryable |
| 5xx | 502 `upstream_error` | Retryable — try next candidate |
| Network error | 502 | Connection refused, timeout |
| Timeout | 504 `request_timeout` | Per `requestTimeout` / `streamTimeout` |
| All workers CB Open | 503 `all_workers_unhealthy` | Degraded |
| Model not registered | 404 `model_not_distributed` | Config error |
| Client disconnect | 499 `client_disconnected` | User closed connection |

### Error responses

| Status | Error type | When |
|--------|------------|------|
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

OpenAI endpoints use the `{"error":{"message","type"}}` format; Ollama
uses `{"error":"<message>"}`.

### Streaming

For streaming requests (`stream=true`):
- **Ollama paths** (`/api/generate`, `/api/chat`): NDJSON chunks
- **OpenAI paths** (`/v1/chat/completions`, `/v1/completions`): SSE events + `[DONE]` terminator
- Per-slice stats in response header: `X-Rpc-Slice-Stats: [{slice_id, worker_id, latency_ms, success}, ...]`

#### Streaming format

Ollama `/api/generate` (stream=true):

```
data: {"model":"llama-70b","response":"Once","done":false}

data: {"model":"llama-70b","response":" upon","done":false}

data: {"model":"llama-70b","response":"","done":true,"done_reason":"stop","total_duration":1234567}

```

OpenAI `/v1/chat/completions` (stream=true):

```
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":...,"model":"llama-70b","choices":[{"index":0,"delta":{"content":"Once"}}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":...,"model":"llama-70b","choices":[{"index":0,"delta":{"content":" upon"}}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":...,"model":"llama-70b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

```

### Auth

If `auth.enabled=true` in the config, the dispatcher checks the token before
processing the request:

```
Authorization: Bearer <token>
# or
X-API-Token: <token>
# or (for WebSocket-style clients)
?token=<token>
```

Auth applies to all `/api/*` and `/v1/*` rpc_coordinator paths.

- Enforced when `auth.enabled=true` in balancer config
- Per-endpoint error format: Ollama `{"error": "..."}`, OpenAI `{"error": {"message": "...", "type": "unauthorized"}}`

### Pipeline model

Currently the pipeline implementation is **simplified**:
- Slice 2 receives `prompt + accumulated tokens` (no real `hidden_state` propagation)
- Suitable for models where downstream layers can be conditioned on text only
- For real hidden-state propagation, see Phase 8 P.3 (Tensor Parallelism, post-1.0)

## Metrics

Per-slice Prometheus counters in `internal/rpccoordinator/metrics.go`:
- Total requests per slice
- Latency histogram per worker
- CB state transitions
- Streaming chunks sent
- Failover attempts

## Monitoring

Every request returns SliceStats in the `X-Rpc-Slice-Stats` header (or in
the response body for non-streaming):

```json
[
  {"slice_id": "1-16", "worker_id": "worker-1", "latency_ms": 234, "success": true},
  {"slice_id": "17-32", "worker_id": "worker-2", "latency_ms": 198, "success": true}
]
```

Circuit breaker state can be inspected via the `/api/v1/rpc/workers` API
(legacy endpoint, may require extension for CB visibility).

## Health Check

The coordinator's `/rpc/health` endpoint (per worker) returns:
- `200 OK` — healthy
- `503 Service Unavailable` — unhealthy

The dispatcher queries each worker's health before routing. If all workers are unhealthy, returns `503 all_workers_unhealthy`.

## Failover Scenarios

| Scenario | Dispatcher behavior |
|----------|---------------------|
| Worker A returns 500 for slice 1 | Try next candidate for slice 1 |
| Worker A times out (no response) | Try next candidate, 502 if all fail |
| Worker A disconnects mid-stream | Client sees partial response, no retry (stream already started) |
| Slice 1 fails on ALL workers | 502 `all_slices_failed` |
| Worker B returns 200 for slice 2 | Aggregate with slice 1 result |

## Limitations

- **Single coordinator**: only one `ModelCoordinator` per balancer (no HA)
- **No real hidden_state propagation**: slice 2 sees text + tokens only
- **No automatic model distribution**: model must be pre-registered via API or config
- **Workers must be pre-registered**: no dynamic discovery (yet)

Additional limitations from the implementation:

- **Simplified pipeline streaming**: slice 2 receives `"prompt + accumulated
  tokens"` as input, not a real `last_hidden_state` tensor. Production
  setups with real llama.cpp need KV-cache merge (deferred to P.3).
- **Streaming in stub workers**: `WorkerClient.InferSliceStream` parses SSE
  via `bufio.Scanner`; for >1MB payloads it may be slow.
- **CB does not cover HealthCheck**: only `Infer` / `InferStream` errors
  trigger CB transitions. Worker-level health is a separate signal.
- **No chat template**: `flattenPrompt` joins messages as
  `"role: content\n"`. A real chat template from the coordinator is Phase 9.

## Examples

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

### Migration / fallback

To disable `rpc_coordinator` and return to standard mode:

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

The scaffold in `Proxy.ServeHTTP` checks `IsRpcCoordinatorMode`; if
`false`, the request flows through the normal proxy path unchanged.

## Related Documentation

- `plans/2026-q3-production-ready-plan.md` §2 — original P.1 plan
- `docs/phase-8-rpc-coordinator.md` — implementation log
- `internal/rpccoordinator/` — source code
- `internal/balancer/rpc_coordinator_dispatcher.go` — dispatcher implementation
