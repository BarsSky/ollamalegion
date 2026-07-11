# Virtual Router Mode

Production-ready `virtual_router` mode (P.2) for Ollama Legion balancer.

## Overview

`virtual_router` mode provides a simple alias-on-pool pattern: a virtual model
= an alias for a pool of backends running the same physical model. The balancer
selects a backend via a Selector (round-robin / least-loaded / random)
and proxies the request. The user sees one model (`virtual:my-gpt`),
while the balancer distributes load across all backends that have that model.

## Architecture

```
Client
  ↓ POST /api/generate {"model": "virtual:my-gpt", ...}
Balancer (port 8080)
  ↓ OperatingMode=virtual_router + VirtualRouter + IsVirtualPathRequest + MatchesVirtualRequest
VirtualRouter.ServeHTTP
  ↓ parse body → lookup registry → Selector.Select()
  ↓ rewrite model in body: "virtual:my-gpt" → "llama-70b"
  ↓ add X-Original-Backend, X-Virtual-Model headers
  ↓ proxy to http://selected-backend:port/api/generate
Backend 1 / Backend 2 / Backend 3 (all with llama-70b)
  ↓ response
VirtualRouter (copies headers + body)
  ↓ add X-Original-Backend to response
Client
```

## VirtualModel Modes

A `VirtualModelConfig` can be in one of two modes:

### Alias-on-pool mode (Phase 8 P.2 — this document)

```json
{
  "name": "virtual:my-gpt",
  "description": "Llama-70B distributed across 3 backends",
  "selection": "round_robin",
  "backendPool": ["backend-1:11434", "backend-2:11434", "backend-3:11434"],
  "modelName": "llama-70b"
}
```

Uses VirtualRouter (Phase 8 P.2) — interceptor in `Proxy.ServeHTTP`.

### Pipeline mode (legacy)

```json
{
  "name": "virtual:distributed-llama",
  "slices": [
    {"workerId": "worker-1", "startLayer": 1, "endLayer": 16},
    {"workerId": "worker-2", "startLayer": 17, "endLayer": 32}
  ],
  "coordination": {
    "mode": "sequential",
    "timeoutMs": 30000
  }
}
```

Routes through the legacy `virtualmodel.Router` + `ExecutePipeline`
(deprecated, not recommended for new deployments).

## Configuration

### Operating mode

```json
{
  "balancing": {
    "operatingMode": "virtual_router",
    "virtualModels": {
      "enabled": true,
      "models": [
        {
          "name": "virtual:my-gpt",
          "selection": "round_robin",
          "backendPool": ["backend-1:11434", "backend-2:11434"],
          "modelName": "llama-70b"
        }
      ]
    }
  }
}
```

### Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Virtual model name (e.g. `virtual:my-gpt`) |
| `description` | string | no | Human-readable description |
| `selection` | string | yes | `round_robin` / `least_loaded` / `random` |
| `backendPool` | []string | yes | List of `host:port` strings |
| `modelName` | string | yes (alias-on-pool) | Physical model on backends |
| `slices` | []object | yes (pipeline) | Layer ranges per worker |
| `coordination.mode` | string | no | `sequential` (default) |
| `coordination.timeoutMs` | int | no | Per-slice timeout in milliseconds |

## Selection Strategies

### Round Robin

Distributes requests evenly across all backends in the pool. Uses atomic
counter to track the next backend. Simple, predictable, no per-backend state.

### Least Loaded

Selects the backend with the most free slots. Free slots are computed
as `MaxConcurrentReqs - ActiveReqs`. Requires a `LoadProvider` callback
to be wired in `main.go` (post-Phase-8-P.2:

```go
router.SetLoadProvider(func(backendID string) (int, bool) {
    return proxy.GetBackendFreeSlots(backendID)
})
```

If no LoadProvider is set, falls back to `FreeSlots=1` (effectively round-robin).

### Random

Random selection with uniform distribution. Good for very large pools
where round-robin state may be expensive to maintain.

## Endpoints

VirtualRouter intercepts the same paths as standard proxy:

| Endpoint | Method | Notes |
|----------|--------|-------|
| `/api/generate` | POST | Ollama |
| `/api/ollama/generate` | POST | Ollama alias |
| `/api/chat` | POST | Ollama |
| `/api/ollama/chat` | POST | Ollama alias |
| `/v1/chat/completions` | POST | OpenAI |
| `/v1/completions` | POST | OpenAI legacy |

Plus management endpoints (via REST API):

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/virtual-models` | GET | List all virtual models |
| `/api/v1/virtual-models` | POST | Create new virtual model |
| `/api/v1/virtual-models/{name}` | GET | Get specific virtual model |
| `/api/v1/virtual-models/{name}` | DELETE | Unregister virtual model |
| `/api/v1/virtual-models/{name}/infer` | POST | Test inference (1 request) |

## Behaviors

### Model rewrite

When a request comes in with `model: "virtual:my-gpt"`, the router:
1. Looks up the registry: `virtual:my-gpt` → `VirtualModelConfig`
2. Picks a backend via the configured selector
3. Rewrites the request body: `"model": "virtual:my-gpt"` → `"model": "llama-70b"`
4. Adds headers: `X-Original-Backend`, `X-Virtual-Model`
5. Proxies to the selected backend

### Response headers

In the response, VirtualRouter adds:
- `X-Original-Backend: <host:port>` — which backend served the request
- `X-Virtual-Model: <original-model-name>` — what the client requested

In the **request to backend**, the dispatcher adds:
- `X-Backend-Selected: <host:port>` — same as X-Original-Backend
- `X-Selection-Strategy: <strategy>` — round_robin/least_loaded/random
- `X-Virtual-Model: <original-model-name>` — original model name (for backend logging)

### Error mapping

| Scenario | Status | Notes |
|----------|--------|-------|
| Unknown virtual model | 404 `unknown_virtual_model` | Model not in registry |
| No healthy backends in pool | 503 `no_healthy_backends` | All backends unhealthy |
| Backend 5xx | 502 (or retry on failover) | See Auto-failover |
| Backend 4xx | passthrough | Client error — don't retry |
| Wrong operating mode | 400 `wrong_virtual_mode` | `mode != virtual_router` but virtual model requested |

### Auto-failover (Phase 8 P.2 backlog)

Since commit `554926e`, VirtualRouter implements automatic failover:
- **5xx response** → retry next backend in pool
- **4xx response** → passthrough (no retry — client error)
- **Network error** → retry next backend
- All backends fail → `502 all_backends_failed`
- Response header `X-Failover-Attempts: <count>` indicates failover count

### Streaming

Streaming requests (`stream: true`) pass through transparently. VirtualRouter
copies SSE events / NDJSON chunks from the backend to the client.

## Metrics

`VirtualRouter.GetMetrics().Snapshot()` returns:

```go
map[string]interface{}{
    "inferenceTotal":     int64,   // total inferences
    "inferenceErrors":    int64,   // errors
    "streamPassThrough":  int64,   // streaming requests
    "selectionSkips":     int64,   // skipped (no backend available)
    "backendSelections":  map[string]int64,  // per-backend counts
}
```

## Auth

Auth is applied to VirtualRouter when `auth.enabled=true`. Token in
`X-API-Token` header (or `?token=` for WebSocket). Per-endpoint error
format: Ollama `{"error": "..."}`, OpenAI `{"error": {"message": "...", "type": "unauthorized"}}`.

## Limitations (post-P.2 backlog)

- **No model affinity**: a model's clients can be routed to different backends on different requests
- **No warmup**: cold backends may take seconds to load model
- **No per-model circuit breaker**: failover is based on per-request response, not per-model CB
- **No per-model rate limit**: global rate limit only

## Related Documentation

- `plans/2026-q3-production-ready-plan.md` §3 — original P.2 plan
- `internal/balancer/virtual_router.go` — implementation
- `internal/virtualmodel/` — selector + registry
- `internal/api/handlers_virtual_crud.go` — REST API
