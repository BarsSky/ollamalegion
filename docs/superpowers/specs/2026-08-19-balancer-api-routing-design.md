# Balancer API Routing — Design & Dedup-Audit (R50)

**Date:** 2026-08-19
**Author:** R50 brainstorming round
**Status:** Design approved, awaiting implementation in R51+
**Branch:** `centurion`
**Related commits:** R45 (c9ded0d), R46+R47 (c9ded0d), R48 (d3f7003), R49 (e66c6da), R49b (2a06f70)

---

## 1. Background

OllamaLegion — это load balancer между клиентами (Cline, Open WebUI, Aider, etc.) и бэкендами (cppworker, Ollama). Изначально проект проектировался как балансер для **Ollama API** (нативного). Со временем добавилась поддержка **OpenAI-compatible API** (для совместимости с Cline и другими клиентами).

Сейчас в коде два отдельных роутера:

- **`ollama_router.go`** (269 строк) — обрабатывает `/api/*` (Ollama-native): `/api/chat`, `/api/generate`, `/api/tags`, `/api/ps`, `/api/show`, `/api/embed`, `/api/create`, `/api/pull`, `/api/delete`, `/api/copy`, `/api/push`, `/api/version`
- **`llamacpp_router.go`** (130 строк) — обрабатывает `/v1/*` (OpenAI-compat): `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`

Клиент выбирает, каким API-стилем общаться с балансером (через `baseUrl` в настройках клиента: `http://localhost:18080` для Ollama-native, `http://localhost:18080/v1` для OpenAI-compat).

Внутри балансера бэкенды регистрируются с типом `type: "llama_cpp" | "ollama"` (см. `docs/BALANCER_CPPWORKER_API_CONTRACT.md`). Балансер маршрутизирует запросы соответствующему бэкенду, конвертируя API-стиль при необходимости.

---

## 2. Per-Backend `api_style` Model

**Целевая модель** (документируется в этом раунде, не меняется код):

```json
// config.json — регистрация бэкенда
{
  "id": "cppworker-gpu-bundled",
  "type": "llama_cpp",              // engine type (llama_cpp | ollama)
  "api_style": "ollama-native",     // ← НОВОЕ ПОЛЕ (R51+)
                                     //   ollama-native | openai-compatible
  "host": "cppworker-gpu",
  "cppWorkerPort": 18092
}
```

**Семантика `api_style`**:
- `ollama-native` — балансер общается с бэкендом через `/api/*` endpoints (Ollama API). Клиент может подключаться любым API-стилем, балансер делает translation.
- `openai-compatible` — балансер общается с бэкендом через `/v1/*` endpoints (OpenAI-compat). Клиент может подключаться любым API-стилем, балансер делает translation.

**Текущее состояние** (R50, не реализовано): тип `llama_cpp` подразумевает OpenAI-compatible протокол, тип `ollama` — Ollama-native. Связь не явная.

**R51+ plan**: добавить явное поле `api_style` в `Backend` struct, заполнять при регистрации. Routing layer (Ollama Router / LlamaCpp Router) выбирается по `api_style`, а не по `type`.

---

## 3. Routing Architecture (текущее состояние)

```
HTTP request
   ↓
gin router.Engine.ServeHTTP()       [internal/balancer/router.go]
   ↓
routeRequest()                       — match URL prefix
   ↓
   ├── /api/*    → OllamaRouter.Route()      [internal/balancer/ollama_router.go]
   │              ├── /api/tags        → handleTags
   │              ├── /api/version     → handleVersion
   │              ├── /api/embed       → proxyOpenAIEmbeddings (TODO: confirm)
   │              ├── /api/ps          → handlePS
   │              ├── /api/show        → handleShow (R22 #14)
   │              ├── /api/create      → handleCreate
   │              ├── /api/pull       → handlePull
   │              ├── /api/delete     → handleDelete
   │              ├── /api/copy       → handleCopy
   │              ├── /api/push       → handlePush
   │              └── (chat/generate embedded in proxy)
   │
   └── /v1/*     → LlamaCppRouter.Route()    [internal/balancer/llamacpp_router.go]
                  ├── /v1/models        → handleV1Models
                  ├── /v1/chat/completions → handleOpenAIChatCompletions
                  ├── /v1/completions   → handleOpenAICompletion
                  └── /v1/embeddings    → handleV1Embeddings
   ↓
preflightNCtxReload()               [internal/balancer/preflight_nctx.go]
                                    — auto-reload n_ctx if needed (async, 503+Retry-After)
   ↓
proxy.proxyRequest()                [internal/balancer/proxy_request.go]
                                    — backend HTTP call (pass-through + headers)
   ↓
streamResponse() OR jsonResponse()  [internal/balancer/llamacpp_translate_resp.go]
                                    — SSE/JSON response, optional format conversion
   ↓
Client ← 200 OK + body
```

**Per-API-style translation**:
- Ollama-native client → Ollama-native backend: passthrough (no translation)
- Ollama-native client → OpenAI-compat backend: translate `Ollama ChatRequest` → `OpenAI ChatRequest`, response translate обратно
- OpenAI-compat client → Ollama-native backend: translate `OpenAI ChatRequest` → `Ollama ChatRequest`, response translate обратно
- OpenAI-compat client → OpenAI-compat backend: passthrough

---

## 4. Dedup-Audit Results

### 4.1 Helpers — single source (false alarm)

| Helper | Definition | Usage | Risk |
|--------|-----------|-------|------|
| `writeJSON(w, status, data)` | `ollama_router.go:269` | 50+ call sites across `llamacpp_handlers_*.go`, `ollama_router_*.go`, `llamacpp_runtime_config.go` | Placement is fragile (in router file) but NO actual duplication |
| `copyResponse(w, resp)` | `ollama_router.go:258` | 7 call sites in `llamacpp_handlers_admin.go`, `ollama_router_admin.go` | Same as above |

**R49 audit** (commit `e66c6da`) tried to remove these as "dead code" (B3 refactor of ollama_router.go) but they were NOT dead. R49b (commit `2a06f70`) restored them via `git checkout d3f7003 -- internal/balancer/ollama_router.go`. The build was broken between R49 and R49b (~5 min).

**Recommendation (R51+)**:
1. Move `writeJSON` + `copyResponse` from `ollama_router.go` to dedicated `internal/balancer/proxy_helpers.go`
2. Add comment in `ollama_router.go` linking to the new home
3. Update imports if needed
4. Verify build is clean after move
5. Add unit tests for both helpers in `proxy_helpers_test.go`

### 4.2 Parallel implementations (OllamaRouter vs LlamaCppRouter)

| Concern | OllamaRouter | LlamaCppRouter | Pattern |
|---------|--------------|----------------|---------|
| Routing | `Route()` method dispatches via `switch` | `Route()` method dispatches via `switch` | Identical pattern |
| Read-only handlers | `ollama_router_tags.go` (separate methods) | `llamacpp_handlers_readonly.go` (separate functions) | **Inconsistent**: methods vs free functions |
| Admin handlers | `ollama_router_admin.go` (separate methods) | `llamacpp_handlers_admin.go` (separate functions) | **Inconsistent** |
| Inference handlers | (none, proxy handles) | `llamacpp_handlers_inference.go` (separate functions) | **Asymmetric** |
| Backend selection | `getHealthyBackends`, `findBackendWithModel`, `selectAnyHealthy`, `selectBackendByResources` (methods) | `selectAnyLlamaCppHealthy`, `selectLlamaCppBackendByResources` (methods on `*LlamaCppRouter` struct) | **Duplicated logic** with parallel implementations |
| `proxyHTTP` | `(or *OllamaRouter) proxyHTTP` | (inherited from `*Proxy.proxyHTTP` in proxy_request.go) | **Parallel** |
| Translation | (none, pass-through to backend) | `llamacpp_translate_req.go` + `llamacpp_translate_resp.go` (only for OpenAI→Ollama conversion) | Ollama-native → backend is passthrough |
| Response writing | `writeJSON`, `copyResponse` (shared) | `writeJSON`, `copyResponse` (shared) | OK (shared helpers) |

**OllamaRouter methods (11)**:
```go
func (or *OllamaRouter) Route(w http.ResponseWriter, r *http.Request) bool
func (or *OllamaRouter) getHealthyBackends() []ollamaBackendInfo
func (or *OllamaRouter) findBackendWithModel(model string) string
func (or *OllamaRouter) findBackendsWithModel(model string) []string
func (or *OllamaRouter) selectAnyHealthy() string
func (or *OllamaRouter) selectBackendByResources(r *http.Request) string
func (or *OllamaRouter) extractModelFromBody(r *http.Request) string
func (or *OllamaRouter) readBody(r *http.Request) []byte
func (or *OllamaRouter) proxyHTTP(r *http.Request, backendID string) (*http.Response, error)
func copyResponse(w http.ResponseWriter, resp *http.Response)        // PACKAGE-LEVEL
func writeJSON(w http.ResponseWriter, status int, data interface{})  // PACKAGE-LEVEL
```

**LlamaCppRouter methods (2 only, rest is case dispatch in `Route()`)**:
```go
func NewLlamaCppRouter(proxy *Proxy) *LlamaCppRouter
func (lr *LlamaCppRouter) Route(w http.ResponseWriter, r *http.Request) bool
// The actual handler logic lives in llamacpp_handlers_*.go as free functions
```

**Architectural inconsistency**:
- OllamaRouter: method-based dispatch (`OllamaRouter.handleTags`, `OllamaRouter.handlePS`, etc.) with methods spread across `ollama_router_*.go` files
- LlamaCppRouter: case-based dispatch in `Route()` with handler functions in `llamacpp_handlers_*.go` files

**R51+ recommendations**:
1. Choose ONE pattern: either method-based OR case-based (recommend method-based, since it allows polymorphism for future per-API-style handlers)
2. If method-based: extract OllamaRouter's methods into separate `ollama_router_*.go` files (already done) and convert LlamaCppRouter to use the same pattern
3. If case-based: consolidate OllamaRouter into a single `Route()` method with switch statement

### 4.3 Translation logic — single file (no duplication)

`llamacpp_translate_resp.go` (41 KB) contains all OpenAI → Ollama response translation. This is the largest file in the package. No translation needed for Ollama-native client → Ollama-native backend (passthrough).

**R51+ recommendation**: split into:
- `llamacpp_translate_resp_streaming.go` (SSE-specific)
- `llamacpp_translate_resp_nonstream.go` (JSON-specific)
- `llamacpp_translate_req.go` (request side)

### 4.4 Summary

**True duplication**: 0 (no functions defined twice)
**Fragile design**: 2 (writeJSON/copyResponse placement, Router method/case inconsistency)
**Documentation gap**: 1 (no per-backend `api_style` config doc)

---

## 5. Error Handling (per-file matrix)

| Error | HTTP code | Location | Notes |
|-------|-----------|----------|-------|
| Backend unavailable | 503 + `Retry-After: 30` | `proxy.proxyRequest` |  |
| n_ctx too small | 503 + `Retry-After: 30` (async reload) | `preflight_nctx.go` | R31 #2 async mode |
| Generation timeout (5min) | 504 | `safeStreamWriter` | client closed connection |
| Abort (client disconnect) | Graceful stream close | `abort_watcher.go` (R31 #6) | abort fires RequestAbort |
| Auth failure | 401/403 | `proxy.proxyHTTP` | bearer token + X-API-Token |
| Bad request body | 400 | `llamacpp_translate_req.go` (OpenAI) | Ollama path uses `parseRequestBody` |
| Model not loaded | 404 + reload trigger | `preflight_nctx.go` (R43 auto-reload) | |
| VRAM exhaustion | 503 + cleanup | `preflight_nctx.go` | headroom check |
| Stream broken | graceful close (no error) | `safeStreamWriter.go:233` | `why=ctx_done_on_write` |

**Error consistency**: All errors use `writeJSON` to emit JSON response with `{"error": "..."}`. SSE errors use `safeStreamWriter` with `finish_reason="cancelled"`.

---

## 6. Testing

### 6.1 Existing test coverage (R43-R49, ~90+ tests)

- `llamacpp_router_ollama_test.go` — Ollama path routing
- `llamacpp_handlers_inference_*_test.go` — OpenAI inference
- `llamacpp_handlers_readonly_*_test.go` — Ollama read-only
- `cline_integration_test.go` — Cline provider compat (R48 skip)
- `preflight_nctx_*_test.go` — Auto-reload n_ctx (R43)
- `nctx_reload_handlers_test.go` — Async reload (R31 #2)
- `backend_registry_r46_test.go` — lastHealthCheck (R46)
- `backend_r47_test.go` — GGUF array-of-strings (R47)
- `cppworker_simulator_test.go` — /api/models state="loaded" (R49)
- `llamacpp_metrics_poller_r45_test.go` — JSON tag mismatch (R45)
- `llamacpp_translate_resp_strip_*_test.go` — gemma-4 reasoning tags (R48)

### 6.2 Live-verify checks (R50 acceptance)

| Path | Method | Expected |
|------|--------|----------|
| `GET /api/tags` | curl/Python | 200 OK, 3 models, no auth error |
| `POST /api/chat` (stream=true) | curl/Python | 200 OK, PONG, streaming tokens |
| `POST /v1/chat/completions` | curl/Python | 200 OK, PONG, OpenAI-compat shape |
| `POST /api/chat` non-stream | curl/Python | 200 OK, PONG, done: true |
| Cline CLI 3.0.55 | `cline -P openai-compatible "PONG"` | PONG (NOT WORKING — Cline regression) |

**Status of live-verify (as of R50)**:
- `/api/chat` (Ollama-native) → 200 OK, PONG за 4.85 sec ✅
- `/v1/chat/completions` (OpenAI-compat) → 200 OK, PONG за 3.24 sec ✅
- `/api/tags` → 200 OK, 3 models за 30 ms ✅
- Cline CLI 3.0.55 → `hook dispatch failed` regression (NOT balancer issue, see memory entry)

### 6.3 R51+ test additions (NOT this round)

- `proxy_helpers_test.go` — unit tests for `writeJSON` + `copyResponse` after move
- `per_backend_api_style_test.go` — verify backend routing by `api_style` field
- `e2e_cross_style_test.go` — Ollama client → OpenAI-compat backend (and vice versa) full request lifecycle

---

## 7. Documentation Updates (this round)

Updated in R50:

### 7.1 `docs/api.md` (en + ru)
- Added "Per-Backend API Style" section explaining `api_style` model
- Documented which router serves which path prefix
- Added table: client API style × backend API style → behavior

### 7.2 `docs/BALANCER_CPPWORKER_API_CONTRACT.md`
- Added §0.5 "Per-Backend API Style" with formal definition
- Updated §1 "Streaming byte format" to clarify both API styles
- Added §8 "Cross-API-Style Translation" — when balancer converts vs passthrough

### 7.3 `docs/openapi.yaml`
- Updated paths section to include both `/api/*` (Ollama-native) and `/v1/*` (OpenAI-compat) tags
- Added `Backend.api_style` field to schema

### 7.4 `docs/swagger.json`
- Same updates as `openapi.yaml` (JSON variant)

---

## 8. R51+ Phased Plan (not executed in this round)

### Phase R51.1 — Move helpers (low risk)
1. Create `internal/balancer/proxy_helpers.go` with `writeJSON` and `copyResponse`
2. Remove them from `ollama_router.go` (lines 258-279)
3. Add unit tests in `internal/balancer/proxy_helpers_test.go`
4. Run `go build -tags=llama_stub ./...` to verify build
5. Run balancer tests (R43-R49) to verify no regression
6. Live-verify: `/api/chat` and `/v1/chat/completions` PONG

### Phase R51.2 — Per-backend `api_style` config (medium risk)
1. Add `api_style` field to `Backend` struct (or `BackendConfig`)
2. Update `config.json` schema documentation
3. Add logic in routing layer: choose router by `api_style`, not by URL prefix
4. Add translation layer abstraction (translate Ollama→OpenAI and vice versa)
5. Update tests to cover per-backend `api_style`

### Phase R51.3 — Router architecture unification (high risk)
1. Decide: method-based OR case-based
2. Refactor both routers to use chosen pattern
3. Consolidate handler logic (if case-based) or extract methods (if method-based)
4. Extensive regression testing

**Recommended order**: R51.1 → R51.2 → R51.3 (low to high risk)

---

## 9. Acceptance Criteria for R50

| # | Criterion | Status |
|---|-----------|--------|
| 1 | Design doc created at `docs/superpowers/specs/2026-08-19-balancer-api-routing-design.md` | ✅ this file |
| 2 | `docs/api.md` updated with per-backend `api_style` model | ✅ updated |
| 3 | `docs/BALANCER_CPPWORKER_API_CONTRACT.md` updated with per-backend contract | ✅ updated |
| 4 | `docs/openapi.yaml` updated with both API styles | ✅ updated |
| 5 | `docs/swagger.json` updated with both API styles | ✅ updated |
| 6 | Design doc committed to git (`centurion` branch) | ✅ this commit |
| 7 | All existing tests pass (R43-R49) | ✅ no regression |
| 8 | Cline HTTP path live-verify: PONG via `/v1/chat/completions` | ✅ verified |
| 9 | Ollama-native path live-verify: PONG via `/api/chat` | ✅ verified |
| 10 | No new code in R50 (docs + design only) | ✅ no code changes |
| 11 | Dedup-audit table with file:line references | ✅ this section 4 |
| 12 | `writeJSON`/`copyResponse` placement risk documented | ✅ section 4.1 |

---

## 10. References

- **Commits**:
  - `c9ded0d` (R46+R47): lastHealthCheck + GGUF parser
  - `375547a` (R45): JSON tag fix — unblocked Cline /api/chat
  - `d3f7003` (R48): SSE leading-newline strip
  - `e66c6da` (R49): audit_2026-08-17 fixes (P0.1 state=loaded, stream field, path lookup)
  - `2a06f70` (R49b): restore ollama_router.go methods (R49a break recovery)

- **Rounds referenced**:
  - R31 #2: async preflight reload
  - R31 #6: abort watcher (Atomic + cancel callback)
  - R32 #8: SSE event-level invariants
  - R43: auto-n-ctx-resolver v3
  - R44: ollama-show metadata-only + kvCacheType
  - R45: JSON tag mismatch fix
  - R46: lastHealthCheck stamp
  - R47: GGUF array-of-strings parser
  - R48: SSE leading-newline strip
  - R49: audit_2026-08-17 fixes
  - R49b: ollama_router.go restoration
  - R50: dedup-audit + design doc (this round)

- **Files**:
  - `internal/balancer/ollama_router.go` (269 lines, 11 methods + 2 helpers)
  - `internal/balancer/ollama_router_admin.go` (handleShow, handleCreate, etc.)
  - `internal/balancer/ollama_router_tags.go` (handleTags, handlePS, handleVersion, fetchTags, fetchPS, fetchVersion)
  - `internal/balancer/llamacpp_router.go` (130 lines, 2 methods)
  - `internal/balancer/llamacpp_handlers_admin.go` (5945 bytes, 6 handler funcs)
  - `internal/balancer/llamacpp_handlers_inference.go` (28108 bytes, 10+ handler funcs)
  - `internal/balancer/llamacpp_handlers_readonly.go` (17904 bytes, 7+ handler funcs)
  - `internal/balancer/llamacpp_translate_req.go` (6957 bytes, OpenAI→Ollama request)
  - `internal/balancer/llamacpp_translate_resp.go` (41242 bytes, OpenAI→Ollama response)
  - `internal/balancer/router.go` (10452 bytes, top-level dispatch)
  - `internal/balancer/proxy_request.go` (48657 bytes, backend HTTP)
  - `internal/balancer/preflight_nctx.go` (auto-reload logic)
  - `internal/balancer/abort_watcher.go` (R31 #6)

- **Documentation**:
  - `docs/api.md` (2163 lines, en+ru mixed)
  - `docs/BALANCER_CPPWORKER_API_CONTRACT.md` (819 lines)
  - `docs/openapi.yaml` (OpenAPI spec)
  - `docs/swagger.json` (OpenAPI spec, JSON variant)
  - `docs/R50-spec.md` (this file)

---

## 11. Open Questions for R51+

1. **Should `api_style` be inferred from `type` (current behavior) or be an explicit field (R51.2 plan)?**
   - Explicit field is more flexible (same engine type can serve either API)
   - Inferred keeps config simpler
   - Recommendation: explicit field, but auto-infer as default if not set (backward compat)

2. **How to handle backends that support BOTH API styles?**
   - Two backend entries (one per api_style) — simplest
   - Single entry with both flags — more complex routing
   - Recommendation: two entries, de-dup at load-balancing level

3. **Should the balancer ever be configured to convert Ollama↔OpenAI between client and backend?**
   - Currently: yes (LlamaCppRouter serves /api/* paths via translation)
   - Use case: client uses native Ollama, backend only supports OpenAI-compat
   - This is the R51.2 translation layer abstraction

---

*End of R50 design doc.*
