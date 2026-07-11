# OllamaLegion Backend Type Isolation (Ollama vs llama.cpp)

> **Version:** 1.3 (2026-06-22)  
> **Related documents:** [`api.md`](api.md), [`audit-2026-06.md`](audit-2026-06.md), [`../.clinerules`](../.clinerules) §6  
> **Tests:** `tests/backend_type_isolation_test.go` (15 tests), `tests/operating_mode_dispatch_test.go`

## Table of Contents

1. [Why type isolation](#1-why-type-isolation)
2. [Backend types and engines](#2-backend-types-and-engines)
3. [OperatingMode compatibility](#3-operatingmode-compatibility)
4. [Request routing](#4-request-routing)
5. [Filtering in selectBackend/expandCandidates](#5-filtering-in-selectbackendexpandcandidates)
6. [Request flow with filtering](#6-request-flow-with-filtering)
7. [Validation on CRUD](#7-validation-on-crud)
8. [WebUI: badges and filters](#8-webui-badges-and-filters)
9. [Deployment options](#9-deployment-options)
10. [Key files and tests](#10-key-files-and-tests)

---

## 1. Why type isolation

OllamaLegion supports two **different** types of backends:

| Type | API | Port | Icon |
|---|---|---|---|
| **Ollama** | `/api/generate`, `/api/chat`, `/api/tags`, etc. | 11434 | 🦙 |
| **llama.cpp** (via CppWorker) | `/api/generate`, `/api/chat` (Ollama-compat), `/v1/*` (OpenAI-compat) | 18091 | 🦒 |

Without isolation, a client `/api/generate` request could accidentally land on a `llama_cpp` backend, and `/v1/chat/completions` on Ollama. This leads to:
- Loss of features (OpenAI tools in Ollama).
- Incompatible responses (different streaming formats).
- Extra latency (double translation).

**Isolation solves this at 3 levels:** filtering in `selectBackend`/`expandCandidates`, protection in `proxyRequest`, validation in CRUD.

---

## 2. Backend types and engines

### 2.1 `BackendType`

File: `pkg/types/backend_type.go`

| Constant | String value | API |
|---|---|---|
| `BackendTypeOllama` | `"ollama"` | `/api/*` Ollama REST |
| `BackendTypeLlamaCpp` | `"llama_cpp"` | `/v1/*` OpenAI + Ollama-compat |

An empty `Backend.Type` (legacy) is normalized to `"ollama"` via `internal/balancer.normalizeBackendType` (backward compatibility).

### 2.2 `BackendEngine`

| Engine | Description |
|---|---|
| `BackendEngineOllamaAPI` | Ollama REST API (`ollama_api`) |
| `BackendEngineLlamaCPP` | llama.cpp + cppworker (`llama_cpp`) |

`ResolveEngine(type)` maps `BackendType → BackendEngine`.

### 2.3 Backend port

`internal/balancer/backend_state.go:getBackendPort(backend)`:
- For `BackendEngineLlamaCPP`: returns `backend.CppWorkerPort` (default 18091), **without fallback** to `OllamaPort`.
- For `BackendEngineOllamaAPI`: returns `backend.OllamaPort` (default 11434).

---

## 3. OperatingMode compatibility

`pkg/types.ModeBackendTypes` and `IsModeCompatibleWithBackendType(mode, bt)`:

| OperatingMode | Allowed types |
|---|---|
| `standard` | `ollama`, `llama_cpp` |
| `replication` | `ollama`, `llama_cpp` |
| `rpc_coordinator` | `ollama`, `llama_cpp` |
| `virtual_router` | `llama_cpp` (only) |
| `distributed_inference` | `llama_cpp` (only) |

When attempting to add an Ollama backend to `virtual_router` — the API returns 400 (see §7).

---

## 4. Request routing

### 4.1 `determineRequestBackendType(r)` — `internal/balancer/proxy.go`

| URL path | Type |
|---|---|
| `/v1/*` (OpenAI-compatible) | `BackendTypeLlamaCpp` (always) |
| `/api/*` in `standard`/`replication`/`rpc_coordinator` | `""` (empty — `selectBackend` decides by model/affinity) |
| `/api/*` in `virtual_router`/`distributed_inference` | `BackendTypeLlamaCpp` |
| Others | From `ModeBackendTypes[OperatingMode]` |

### 4.2 Main flow for `/api/chat` and `/api/generate`

In mixed modes (`standard`/`replication`/`rpc_coordinator`), `/api/chat` and `/api/generate` go through the **main flow** `ServeHTTP` / `selectBackend`, **not** intercepted by specialized routers (`llamacpp_router.go` / `ollama_router.go`). This allows:
- In a mixed cluster, to route an Ollama request to an Ollama backend (not llama.cpp).
- In pure-llama_cpp mode — to llama.cpp.

---

## 5. Filtering in `selectBackend`/`expandCandidates`

### 5.1 Signature

```go
func (p *Proxy) selectBackend(model string, bt types.BackendType) string
func (p *Proxy) expandCandidates(modelName string, allowedTypes []types.BackendType) CandidateGroups
```

Both methods accept `allowedTypes` and filter candidates via `isBackendTypeAllowed`:

```go
func isBackendTypeAllowed(bt types.BackendType, allowedTypes []types.BackendType) bool {
    if len(allowedTypes) == 0 { return true }  // backward compatibility
    for _, at := range allowedTypes {
        if bt == at { return true }
    }
    return false
}
```

### 5.2 Helper methods (all accept `allowedTypes`)

- `findBackendWithModel(modelName, allowedTypes)`
- `findBackendWithModelExcluding(modelName, exclude, allowedTypes)`
- `selectByResources(allowedTypes)`
- `selectByResourcesExcluding(exclude, allowedTypes)`
- `selectFreeBackendAny(allowedTypes)`
- `findFreeBackendForModelUnsafe(model, allowedTypes)`
- `findLessLoadedBackendWithModel(...)`
- `findLessLoadedBackendAny(...)`

### 5.3 Building `allowedTypes`

`getAllowedTypesList(bt)` + `getDefaultAllowedTypes()`:
- From `ModeBackendTypes[p.config.OperatingMode]` (list of allowed types).
- If `bt != ""` — the explicitly requested type is added.

---

## 6. Request flow with filtering

```
HTTP Request → ServeHTTP()
  ├─ determineRequestBackendType(r):
  │   /v1/*   → BackendTypeLlamaCpp
  │   /api/*  → "" (in standard/replication/rpc_coordinator) or BackendTypeLlamaCpp
  │   else    → from OperatingMode
  │
  ├─ selectBackend(model, bt) → filtering via allowedTypes:
  │   ├─ expandCandidates(model, allowedTypes)       ← candidate.go: P1-P4 filter
  │   ├─ findBackendWithModel(model, allowedTypes)    ← affinity check
  │   ├─ selectByResources(allowedTypes)              ← resource-aware
  │   └─ selectFreeBackendAny(allowedTypes)           ← fallback
  │
  └─ proxyRequest(w, r, backendID)
      └─ IsModeCompatibleWithBackendType(normalizeBackendType(state.Backend.Type), mode)
          └─ getBackendPort(backend) — returns the correct port depending on engine
```

**Guarantee:** after `selectBackend`, a candidate will **never** be of the opposite type, because filtering in `expandCandidates` cuts them off before scoring/selection.

---

## 7. Validation on CRUD

`internal/api/handlers_backends.go`:

### 7.1 `handleAddBackend`

```go
// Check type compatibility with OperatingMode
if !types.IsModeCompatibleWithBackendType(bt, p.config.Balancing.OperatingMode) {
    return 400 Bad Request
}
```

Example: attempting to add `type: "ollama"` in `virtual_router` mode → 400.

### 7.2 `handleUpdateBackend`

- Type change is forbidden if `ActiveReqs > 0`.
- Re-checks compatibility with OperatingMode.

### 7.3 `Engine` is computed automatically

```go
backend.Engine = types.ResolveEngine(backend.Type)
```

---

## 8. WebUI: badges and filters

### 8.1 Badges 🦙 / 🦒 (implemented)

- `webui/js/modules/utils.js:getBackendTypeBadge(backend)` — renders the badge.
- Applied in: `backendsTable` (Dashboard), `backendsPage` (Backends management), `modelsGrid` (Models).
- `webui/js/monitor/backend-type-badges.js` — Monitor (`renderBackendsTable`, `renderModelsInMemory`).

### 8.2 Filters by type (in backlog, R-2)

Not yet implemented in the WebUI. After R-2 they will be:
- Dashboard: buttons "All | 🦙 Ollama | 🦒 llama.cpp" above the backends table.
- Backends management: similar buttons.
- Monitor: type switcher (the skeleton in `ui-renderer.js:renderBackendTypeSwitcher` already exists).

### 8.3 `BackendTypeFilter` + `toggleAgentsTab()`

`webui/js/modules/backend-type-filter.js`:
- In `llama_cpp` mode, hides the Agents tab.
- In Settings, hides sections specific to Ollama (`.settings-section-ollama`).

---

## 9. Deployment options

### 9.1 Ollama only (standard)

```bash
docker compose -f deployments/docker-compose.yml up -d
```

Backends are registered via WebUI or API with type `ollama`.

### 9.2 llama.cpp only

```bash
docker compose -f deployments/docker-compose.yml -f deployments/docker-compose.cppworker.yml up -d
```

Backends are registered with `"type": "llama_cpp"` and `"cppWorkerPort": 18091`.

### 9.3 Mixed cluster (Ollama + llama.cpp)

```bash
docker compose \
  -f deployments/docker-compose.yml \
  -f deployments/docker-compose.cppworker.yml \
  -f deployments/docker-compose.agent.yml \
  up -d
```

Backends of different types work in parallel. The load balancer:
- `/v1/*` → llama.cpp backends.
- `/api/generate`, `/api/chat` → selection by `selectBackend` (Ollama or llama.cpp).
- `/api/tags`, `/api/show`, `/api/copy`, `/api/create`, `/api/pull`, `/api/delete`, `/api/push` → specialized routers.

### 9.4 OperatingMode modes

```bash
# Replication (ollama + llama_cpp)
# In WebUI: Settings → Operating Mode = replication

# Virtual Router (llama.cpp only)
# In WebUI: Settings → Operating Mode = virtual_router
```

### 9.5 Adding a backend via API

```bash
# Ollama backend
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -d '{"id":"ollama-gpu-1","host":"192.168.1.10","ollamaPort":11434,"type":"ollama"}'

# llama.cpp backend
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -d '{"id":"llamacpp-1","host":"192.168.1.20","cppWorkerPort":18091,"type":"llama_cpp"}'
```

**Auto-detect:** backends with `cppWorkerPort > 0` are automatically detected as `llama_cpp`, even if `type` is not set explicitly.

---

## 10. Key files and tests

### 10.1 Code

| File | Purpose |
|---|---|
| `pkg/types/backend_type.go` | `BackendType`, `BackendEngine`, `ModeBackendTypes`, `IsModeCompatibleWithBackendType`, `ResolveEngine` |
| `internal/balancer/router.go` | `routeRequest`, `determineRequestBackendType`, `isChatOrGenerateRequest` |
| `internal/balancer/proxy.go` | `ServeHTTP`, calls to `selectBackend`, `queueRequest` |
| `internal/balancer/backend_selector.go` | `selectBackend`, `getAllowedTypesList`, `getDefaultAllowedTypes` |
| `internal/balancer/candidate.go` | `expandCandidates(modelName, allowedTypes)` |
| `internal/balancer/backend_type_filter.go` | `normalizeBackendType`, `filterBackendsByType` |
| `internal/balancer/proxy_request.go` | `IsModeCompatibleWithBackendType` protection |
| `internal/balancer/backend_state.go` | `getBackendPort`, `resolveBackendEngine` |
| `internal/api/handlers_backends.go` | CRUD type validation |
| `internal/api/routes.go` | Registration of `/api/v1/backends` |

### 10.2 Tests

| Test | File | What it checks |
|---|---|---|
| `TestSelectBackend_FiltersByType_OllamaOnly` | `tests/backend_type_isolation_test.go` | Explicit `BackendTypeOllama` does not select llama.cpp |
| `TestSelectBackend_FiltersByType_LlamaCppOnly` | `tests/backend_type_isolation_test.go` | Explicit `BackendTypeLlamaCpp` does not select Ollama |
| `TestSelectBackend_MixedCluster_NoCrossContamination` | `tests/backend_type_isolation_test.go` | 100 iterations of Ollama requests do not select llama.cpp |
| `TestExpandCandidates_FiltersByType` | `tests/backend_type_isolation_test.go` | expandCandidates filters by type |
| `TestServeHTTP_OllamaRequest_RoutedToOllamaBackend` | `tests/backend_type_isolation_test.go` | Full HTTP flow `/api/generate` → Ollama |
| `TestServeHTTP_LlamaCppRequest_RoutedToLlamaCppBackend` | `tests/backend_type_isolation_test.go` | Full HTTP flow `/v1/chat/completions` → llama.cpp |
| `TestOperatingMode_DispatchQueue_Standard` | `tests/operating_mode_dispatch_test.go` | `/api/chat` in standard mode → main flow |
| `TestOperatingMode_DispatchQueue_Replication` | `tests/operating_mode_dispatch_test.go` | `/api/chat` in replication mode → main flow |
| `TestDetermineRequestBackendTypeForTest` | `tests/backend_type_init_test.go` | Logic of `determineRequestBackendType` |

### 10.3 Running tests

```bash
go test ./tests -run TestBackendType -count=1 -v
go test ./tests -run TestOperatingMode -count=1 -v
```
