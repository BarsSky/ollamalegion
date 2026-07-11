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

### Streaming

For streaming requests (`stream=true`):
- **Ollama paths** (`/api/generate`, `/api/chat`): NDJSON chunks
- **OpenAI paths** (`/v1/chat/completions`, `/v1/completions`): SSE events + `[DONE]` terminator
- Per-slice stats in response header: `X-Rpc-Slice-Stats: [{slice_id, worker_id, latency_ms, success}, ...]`

### Auth

- `Authorization: Bearer <token>` header
- `X-API-Token: <token>` header (configurable)
- `?token=<token>` query parameter (WebSocket-style)
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

## Related Documentation

- `plans/2026-q3-production-ready-plan.md` §2 — original P.1 plan
- `docs/phase-8-rpc-coordinator.md` — implementation log
- `internal/rpccoordinator/` — source code
- `internal/balancer/rpc_coordinator_dispatcher.go` — dispatcher implementation
