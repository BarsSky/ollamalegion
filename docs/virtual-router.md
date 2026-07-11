# Virtual Router Mode

Production-ready `virtual_router` mode (P.2) для Ollama Legion balancer.

## Что это

`virtual_router` mode даёт простой alias-on-pool pattern: virtual model
= алиас на пул backends с одной и той же физической моделью. balancer
выбирает backend через Selector (round-robin / least-loaded / random)
и проксирует request. Пользователь видит одну модель (`virtual:my-gpt`),
а balancer распределяет нагрузку по всем backends с этой моделью.

## Архитектура

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
Backend 1 / Backend 2 / Backend 3 (все с llama-70b)
  ↓ response
VirtualRouter (копирует headers + body)
  ↓ add X-Original-Backend в response
Client
```

## Режимы VirtualModel

VirtualModelConfig может быть в одном из двух режимов:

### Alias-on-pool mode (Phase 8 P.2 — этот документ)
```json
{
  "name": "virtual:my-gpt",
  "description": "Llama-70B distributed across 3 backends",
  "selection": "round_robin",
  "backendPool": ["backend-1:11434", "backend-2:11434", "backend-3:11434"],
  "modelName": "llama-70b"
}
```

Использует VirtualRouter (Phase 8 P.2) — interceptor в Proxy.ServeHTTP.

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

Использует `virtualmodel.Router` + `VirtualModel.ExecutePipeline` (legacy).

## Конфигурация

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

Параметры `Selection`:

| Стратегия | Поведение |
|---|---|
| `round_robin` (default) | Atomic counter, mod по pool. Равномерное распределение. |
| `least_loaded` | Выбирает backend с max FreeSlots (через LoadProvider). При равенстве — round-robin. |
| `random` | math/rand, для тестов и stress testing. |

`LoadProvider` (для `least_loaded`): callback, возвращающий `FreeSlots` для backend'а.
Phase 8 P.2: stub fallback (= 1 для всех, round-robin по сути).
Future (Phase 9): wired с реальным `Proxy.GetBackendMetrics()`.

## Поддерживаемые endpoints

| Path | Format | Notes |
|---|---|---|
| `/api/generate` | Ollama | rewritten `model` field |
| `/api/ollama/generate` | Ollama (alias) | same |
| `/api/chat` | Ollama | same |
| `/api/ollama/chat` | Ollama (alias) | same |
| `/v1/chat/completions` | OpenAI | same |
| `/v1/completions` | OpenAI legacy | same |

Метод: только `POST`. GET (`/api/tags`, `/v1/models`) идёт в стандартный flow.

## Response headers

После прохождения через VirtualRouter, response содержит:

- `X-Original-Backend: <host:port>` — какой backend обработал request.
- `X-Virtual-Model: <original_name>` — original virtual model name (для debug).

Request содержит:

- `X-Original-Backend: <host:port>` — selected backend.
- `X-Virtual-Model: <original_name>` — original name (для backend'а).
- `X-Backend-Selected: <host:port>` — duplicate of X-Original-Backend.
- `X-Selection-Strategy: <round_robin|least_loaded|random>` — какая стратегия.

## Error responses

| Status | Error type | Когда |
|---|---|---|
| 400 | `json_parse_error` | Body не JSON или truncated |
| 400 | `wrong_virtual_mode` | Virtual model в pipeline mode (не alias-on-pool) |
| 404 | `unknown_virtual_model` | Model name not in registry |
| 502 | `backend_unreachable` | Connection refused / timeout |
| 503 | (no error type) | virtual_router not active (registry disabled) |
| 503 | `no_healthy_backends` | All backends down + no candidates |
| 502 | (passthrough) | Backend returned 4xx/5xx (status copied) |

OpenAI endpoint'ы используют `{"error":{"message","type"}}` format; Ollama —
`{"error":"<message>"}`.

## Метрики

`VirtualRouter.GetMetrics().Snapshot()` возвращает:

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

Phase 8 P.2: встроенные counters. Phase 9: интеграция в `/api/v1/metrics`
endpoint и Prometheus exporter.

## Примеры

### Round-robin распределение

```bash
# 3 backends с одинаковой llama-70b.
# Config: selection=round_robin, backendPool=["b1:11434","b2:11434","b3:11434"]

# 3 последовательных запроса → 3 разных backend'а.
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
# (c LoadProvider возвращающим FreeSlots=[2, 10, 5] соответственно)

# Все запросы пойдут на b2 (max FreeSlots=10).
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
# SSE stream от выбранного backend'а, headers X-Original-Backend
# показывают куда ушёл request.
```

### Failover (legacy behavior)

Если `selection=round_robin` и backend down — connection refused → 502.
Round-robin НЕ делает автоматический failover на следующий backend.
Для failover используйте `least_loaded` с health checks (Phase 9) или
комбинируйте с `replication` mode (P.2 legacy alias).

## Migration / fallback

Чтобы отключить virtual_router и вернуться к standard mode:

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

Intercepts в `Proxy.ServeHTTP` падают через `IsVirtualRouterMode` check.
Default bundled config (OperatingMode="" / "standard") → не срабатывает.

## Известные ограничения

- **No automatic failover**: round-robin / random / least-loaded не
  повторяют на следующий backend при ошибке. Backend unreachable → 502.
  Phase 9: добавить retry-with-next-candidate logic.
- **LoadProvider stub**: `least_loaded` без настроенного `LoadProvider`
  использует fallback FreeSlots=1, что эквивалентно round-robin.
  Phase 9: wired с реальными metrics.
- **No model validation**: VirtualRouter не проверяет что physical
  model действительно загружена на backend'е до запроса. Если backend
  вернёт 404 "model not found" — passthrough как backend error.
- **Pipeline mode не использует VirtualRouter**: только alias-on-pool
  mode (Phase 8 P.2). Pipeline mode (Slices + Coordination) →
  legacy `virtualmodel.Router` path.

## Сравнение с P.1 (rpc_coordinator)

| Aspect | P.1 rpc_coordinator | P.2 virtual_router |
|---|---|---|
| Pattern | Pipeline parallelism (multi-slice) | Alias-on-pool (1 model, N backends) |
| Worker model | Multiple workers, each handles slice | All backends have full model |
| Selection | Per-slice candidates | Per-request from pool |
| Streaming | Yes (full SSE passthrough) | Yes (passthrough) |
| Circuit breaker | Per-worker | Not yet (Phase 9) |
| Auth | Per-rpc path | Not yet (Phase 9) |
| Use case | Model > 1 GPU | High availability / load balancing |

## См. также

- `docs/phase-8-rpc-coordinator.md` — P.1 sibling.
- `plans/2026-q3-production-ready-plan.md` — overall plan §P.2.
- `CHANGELOG.md` — version history (Phase 8 P.2 section).
- `internal/balancer/virtual_router.go` — implementation.
- `internal/virtualmodel/selector.go` — selection strategies.
- `internal/virtualmodel/registry.go` — existing registry (legacy pipeline mode).
- `pkg/types/rpc_variants.go` — VirtualModelConfig + SelectionStrategy types.
