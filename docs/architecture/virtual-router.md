# Virtual Router Mode

Production-ready `virtual_router` mode (P.2) for the Ollama Legion balancer.

## What it is

`virtual_router` mode gives a simple alias-on-pool pattern: a virtual model is an alias
for a pool of backends that all serve the same physical model. The balancer picks a
backend through a Selector (round-robin / least-loaded / random) and proxies the request.
The user sees one model (`virtual:my-gpt`), while the balancer spreads the load across
all backends that have this model.

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
  ↓ add X-Original-Backend to the response
Client
```

## VirtualModel modes

A VirtualModelConfig can be in one of two modes:

### Alias-on-pool mode (Phase 8 P.2 - this document)
```json
{
  "name": "virtual:my-gpt",
  "description": "Llama-70B distributed across 3 backends",
  "selection": "round_robin",
  "backendPool": ["backend-1:11434", "backend-2:11434", "backend-3:11434"],
  "modelName": "llama-70b"
}
```

Uses VirtualRouter (Phase 8 P.2) - an interceptor inside Proxy.ServeHTTP.

### Pipeline mode (legacy)
```json
{
  "name": "virtual:my-pipeline",
  "slices": [
    {"id": "emb", "modelName": "embedding-model", "ordinal": 0, "targetBackends": ["b1"]},
    {"id": "gen", "modelName": "llama-70b", "ordinal": 1, "targetBackends": ["b2", "b3"]}
  ],
  "coordination": {"mode": "sequential", "timeoutMs": 30000}
}
```

Uses `virtualmodel.Router` + `VirtualModel.ExecutePipeline` (legacy).

## Configuration

```json
{
  "balancing": {
    "operatingMode": "virtual_router",
    "virtualModels": {
      "enabled": true,
      "models": [
        {
          "name": "virtual:my-gpt",
          "description": "Llama-70B distributed",
          "selection": "round_robin",
          "backendPool": ["backend-1:11434", "backend-2:11434", "backend-3:11434"],
          "modelName": "llama-70b"
        },
        {
          "name": "virtual:fast-llama",
          "description": "Fast llama for short prompts",
          "selection": "least_loaded",
          "backendPool": ["backend-a:11434", "backend-b:11434"],
          "modelName": "llama-3-8b"
        }
      ]
    }
  }
}
```

`Selection` strategies:

| Strategy | Behaviour |
|---|---|
| `round_robin` (default) | Atomic counter, mod over the pool. Even distribution. |
| `least_loaded` | Picks the backend with the highest FreeSlots (through LoadProvider). Ties are broken by round-robin. |
| `random` | math/rand, for tests and stress testing. |

`LoadProvider` (for `least_loaded`): a callback returning `FreeSlots` for a backend.
Phase 8 P.2: stub fallback (= 1 for every backend, i.e. round-robin in effect).
Future (Phase 9): wired to the real `Proxy.GetBackendMetrics()`.

## Supported endpoints

| Path | Format | Notes |
|---|---|---|
| `/api/generate` | Ollama | rewritten `model` field |
| `/api/ollama/generate` | Ollama (alias) | same |
| `/api/chat` | Ollama | same |
| `/api/ollama/chat` | Ollama (alias) | same |
| `/v1/chat/completions` | OpenAI | same |
| `/v1/completions` | OpenAI legacy | same |

Method: `POST` only. GET (`/api/tags`, `/v1/models`) goes through the standard flow.

## Response headers

After passing through VirtualRouter, the response contains:

- `X-Original-Backend: <host:port>` - which backend processed the request.
- `X-Virtual-Model: <original_name>` - the original virtual model name (for debugging).

The request contains:

- `X-Original-Backend: <host:port>` - the selected backend.
- `X-Virtual-Model: <original_name>` - the original name (for the backend).
- `X-Backend-Selected: <host:port>` - duplicate of X-Original-Backend.
- `X-Selection-Strategy: <round_robin|least_loaded|random>` - which strategy was used.

## Error responses

| Status | Error type | When |
|---|---|---|
| 400 | `json_parse_error` | Body is not JSON or is truncated |
| 400 | `wrong_virtual_mode` | Virtual model in pipeline mode (not alias-on-pool) |
| 404 | `unknown_virtual_model` | Model name not in registry |
| 502 | `backend_unreachable` | Connection refused / timeout |
| 503 | (no error type) | virtual_router not active (registry disabled) |
| 503 | `no_healthy_backends` | All backends down + no candidates |
| 502 | (passthrough) | Backend returned 4xx/5xx (status copied) |

OpenAI endpoints use the `{"error":{"message","type"}}` format; Ollama uses
`{"error":"<message>"}`.

## Metrics

`VirtualRouter.GetMetrics().Snapshot()` returns:

```json
{
  "inferenceTotal": 1247,
  "inferenceErrors": 3,
  "streamPassThrough": 412,
  "selectionSkips": 0,
  "backendSelections": {
    "backend-1:11434": 416,
    "backend-2:11434": 415,
    "backend-3:11434": 416
  }
}
```

Phase 8 P.2: built-in counters. Phase 9: integration into the `/api/v1/metrics`
endpoint and a Prometheus exporter.

## Examples

### Round-robin distribution

```bash
# 3 backends with the same llama-70b.
# Config: selection=round_robin, backendPool=["b1:11434","b2:11434","b3:11434"]

# 3 consecutive requests → 3 different backends.
for i in 1 2 3; do
  curl -X POST http://balancer:8080/api/generate \
    -d '{"model":"virtual:my-gpt","prompt":"hello"}' \
    -D - 2>&1 | grep X-Original-Backend
done
# X-Original-Backend: b1:11434
# X-Original-Backend: b2:11434
# X-Original-Backend: b3:11434
```

### Least-loaded

```bash
# Config: selection=least_loaded, backendPool=["b1:11434","b2:11434","b3:11434"]
# (with a LoadProvider returning FreeSlots=[2, 10, 5] respectively)

# All requests go to b2 (max FreeSlots=10).
```

### Streaming passthrough

```bash
curl -X POST http://balancer:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model":"virtual:my-gpt",
    "messages":[{"role":"user","content":"Stream me"}],
    "stream":true
  }'
# SSE stream from the selected backend; the X-Original-Backend headers
# show where the request went.
```

### Failover (legacy behaviour)

If `selection=round_robin` and a backend is down, the connection is refused → 502.
Round-robin does NOT fail over to the next backend automatically.
For failover use `least_loaded` with health checks (Phase 9) or combine it with
`replication` mode (P.2 legacy alias).

## Migration / fallback

To disable virtual_router and return to standard mode:

```json
{
  "balancing": {
    "operatingMode": "standard",
    "virtualModels": {
      "enabled": false
    }
  }
}
```

Interceptors in `Proxy.ServeHTTP` bail out through the `IsVirtualRouterMode` check.
The default bundled config (OperatingMode="" / "standard") does not trigger it.

## Known limitations

- **No automatic failover**: round-robin / random / least-loaded do not
  retry on the next backend after an error. Backend unreachable → 502.
  Phase 9: add retry-with-next-candidate logic.
- **LoadProvider stub**: `least_loaded` without a configured `LoadProvider`
  falls back to FreeSlots=1, which is equivalent to round-robin.
  Phase 9: wire it to real metrics.
- **No model validation**: VirtualRouter does not check that the physical
  model is actually loaded on the backend before the request. If the backend
  returns 404 "model not found", it is passed through as a backend error.
- **Pipeline mode does not use VirtualRouter**: only alias-on-pool
  mode does (Phase 8 P.2). Pipeline mode (Slices + Coordination) goes through the
  legacy `virtualmodel.Router` path.

## Comparison with P.1 (rpc_coordinator)

| Aspect | P.1 rpc_coordinator | P.2 virtual_router |
|---|---|---|
| Pattern | Pipeline parallelism (multi-slice) | Alias-on-pool (1 model, N backends) |
| Worker model | Multiple workers, each handles a slice | All backends have the full model |
| Selection | Per-slice candidates | Per-request from the pool |
| Streaming | Yes (full SSE passthrough) | Yes (passthrough) |
| Circuit breaker | Per-worker | Not yet (Phase 9) |
| Auth | Per-rpc path | Not yet (Phase 9) |
| Use case | Model > 1 GPU | High availability / load balancing |

## See also

- `docs/archive/rounds/phase-8-p1-rpc-coordinator.md` - P.1 sibling.
- `docs/plans/2026-q3-production-ready-plan.md` - overall plan §P.2.
- `CHANGELOG.md` - version history (Phase 8 P.2 section).
- `internal/balancer/virtual_router.go` - implementation.
- `internal/virtualmodel/selector.go` - selection strategies.
- `internal/virtualmodel/registry.go` - existing registry (legacy pipeline mode).
- `pkg/types/rpc_variants.go` - VirtualModelConfig + SelectionStrategy types.
