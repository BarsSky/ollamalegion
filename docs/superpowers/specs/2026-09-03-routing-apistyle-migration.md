# Routing: migrate `isLlamaCppBackend` → `EffectiveAPIStyle()` (R56)

**Date:** 2026-09-03
**Author:** Mavis (R56 brainstorming round, post-playwright-baseline)
**Status:** Design approved, awaiting TDD-driven implementation
**Branch:** `centurion`
**Depends on:** R51.2 (Backend.ApiStyle field + EffectiveAPIStyle() method) — already shipped in R51.2 commit `347379e`
**Related commits:** R51.1 (`8ebd841` writeJSON/copyResponse), R51.2 (`347379e` ApiStyle field), R50 spec (`e5242fa`)

---

## 1. Background

`pkg/types/backend_type.go:60` defines `(*Backend).EffectiveAPIStyle()` (R51.2, 2026-08-20) with the priority:

1. Explicit `Backend.ApiStyle` field if valid (`ollama-native` / `openai-compatible`)
2. Fallback: infer from `Backend.Type` (`llama_cpp` → `openai-compatible`, else `ollama-native`)

But the **routing layer in `internal/balancer/` still uses the OLD inference by `Type`**, not the new method. The 4 call sites of `isLlamaCppBackend` continue to look at `Type == BackendTypeLlamaCpp` directly. This means:

- An operator who sets `ApiStyle: "ollama-native"` on a `Type: "llama_cpp"` backend (a legitimate config — running a cppworker that natively speaks Ollama API) **still gets routed through the OpenAI-compat translation path**, because the routing doesn't check `EffectiveAPIStyle()`.
- The `ApiStyle` field is dead weight for the dispatcher — it appears in `/api/v1/backends` response (as `effectiveApiStyle`) but doesn't actually change behavior.

This is exactly the "каждый раз при нахождении ошибки формировался новый код не учитывая существующие реализации" pattern the user flagged: the R51.2 commit added the field but didn't wire it through the routing.

## 2. Goal

Replace 4 call sites of `(*Proxy).isLlamaCppBackend` and 2 call sites of `(*ModelManager).isLlamaCppBackend` with `backend.EffectiveAPIStyle() == types.APIStyleOpenAICompatible`. The semantic meaning stays **identical for existing configs** (Type-based fallback matches the old behavior), but operators can now override via the explicit `ApiStyle` field.

## 3. Acceptance criteria

1. **Behavior preservation**: all existing tests pass without modification.
2. **Field actually used**: a backend with `Type: "llama_cpp"` AND `ApiStyle: "ollama-native"` is routed through `proxyRequestOllama`, not `proxyRequestLlamaCpp` (verified by a new unit test).
3. **TDD-driven**: a new test `TestProxy_IsLlamaCppBackend_UsesApiStyle` is added in `backend_state_test.go` (or a new file) BEFORE the implementation change. Test fails on the current code, passes after.
4. **No new public API**: `isLlamaCppBackend` is kept (with deprecation comment) as a thin wrapper, so 30+ existing call sites don't have to change in this commit.
5. **Wire-format preserved**: `EffectiveAPIStyle()` exposed via `/api/v1/backends` already works (verified R51.2 live, R55.11 deployment).

## 4. Files to change

| File | Change | Lines |
|------|--------|-------|
| `internal/balancer/backend_state.go:98` | `isLlamaCppBackend` body → `backend.EffectiveAPIStyle() == types.APIStyleOpenAICompatible` | 1 line + 1 doc-comment |
| `internal/balancer/model_management.go:131` | same migration on `*ModelManager` receiver | 1 line + 1 doc-comment |
| `internal/balancer/backend_state_test.go` (new file) | add `TestProxy_IsLlamaCppBackend_UsesApiStyle` (TDD) | ~30 lines, 4 sub-cases |
| `internal/balancer/proxy_request.go:167, 169` | call site already uses `p.isLlamaCppBackend(state.Backend)` — no change needed, gets the new behavior via wrapper | 0 |
| `docs/superpowers/specs/2026-08-19-balancer-api-routing-design.md` | update §2 to mark "explicit field used by routing" as ✅ | 1-line status flip |

## 5. Sub-cases for the unit test (TDD)

```go
func TestProxy_IsLlamaCppBackend_UsesApiStyle(t *testing.T) {
    p := &Proxy{}  // no setup needed — isLlamaCppBackend is pure function
    cases := []struct {
        name        string
        backend     *types.Backend
        wantLlamaCpp bool
    }{
        {
            name: "Type=llama_cpp, ApiStyle empty (R50 default)",
            backend: &types.Backend{Type: types.BackendTypeLlamaCpp},
            wantLlamaCpp: true,  // fallback: llama_cpp → openai-compatible
        },
        {
            name: "Type=ollama, ApiStyle empty (R50 default)",
            backend: &types.Backend{Type: types.BackendTypeOllama},
            wantLlamaCpp: false,  // ollama → ollama-native
        },
        {
            name: "Type=llama_cpp, ApiStyle=ollama-native (operator override)",
            backend: &types.Backend{Type: types.BackendTypeLlamaCpp, ApiStyle: types.APIStyleOllamaNative},
            wantLlamaCpp: false,  // explicit override wins
        },
        {
            name: "Type=ollama, ApiStyle=openai-compatible (operator override)",
            backend: &types.Backend{Type: types.BackendTypeOllama, ApiStyle: types.APIStyleOpenAICompatible},
            wantLlamaCpp: true,
        },
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            got := p.isLlamaCppBackend(c.backend)
            if got != c.wantLlamaCpp {
                t.Errorf("isLlamaCppBackend() = %v, want %v", got, c.wantLlamaCpp)
            }
        })
    }
}
```

The first two cases pass on current code. The third and fourth (operator override) FAIL — that's the gap R51.2 left open.

## 6. Out of scope (this commit)

- **Method-vs-case dispatch unification** (R50 §4.2). The R50 spec recommends method-based dispatch for both routers. This is a larger refactor that touches ~10 files and ~200 lines; we keep `OllamaRouter` method-based and `LlamaCppRouter` case-based for now, just make them consult the new helper.
- **`api_style` field on `/api/v1/backends` POST/PUT round-trip** — already works (R51.2), no change.
- **Wave B (split backend.go 117KB, split gguf-renderer.js 172KB)** — separate rounds, separate specs.
- **Wave A.2 (parallel LlamaCollector)** — separate round, easy.

## 7. Risk analysis

| Risk | Mitigation |
|------|------------|
| Existing 4 call sites have subtle behavior differences between `Type==LlamaCpp` and `EffectiveAPIStyle()==OpenAICompatible` | For current deployments (all using `ApiStyle=""`), behavior is bit-identical: `Type==LlamaCpp` ⇔ `EffectiveAPIStyle()==OpenAICompatible` per the fallback in `backend_type.go:67-71`. |
| Test fixtures use `Type` only, never `ApiStyle` | First two TDD cases are bit-identical to current behavior; existing 30+ test runs still pass. |
| Concurrency: `EffectiveAPIStyle()` reads `b.ApiStyle` and `b.Type` without locks | Same as current `isLlamaCppBackend` (also unlocked). Caller (`proxyRequest`) holds `state.mu` already; the Backend pointer is immutable after registration. |
| Docker image re-build required? | Yes, but buildx ccache (R23) makes Go-only changes <2 min warm. Image tag R56.0 → re-deploy. |

## 8. Rollout

1. Land commit on `centurion` branch.
2. Build balancer image: `docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml build loadbalancer` (warm, <2 min).
3. Restart: `docker compose -f deployments/docker-compose.cppworker-bundled-with-agent.yml up -d --no-build --no-deps loadbalancer`.
4. Live verify: 5× curl `/api/v1/backends`, check `effectiveApiStyle` field still present and consistent.
5. Bump image tag in compose file to `cppworker-bundled-r56.0`.

## 9. Playwright baseline

Run `npx playwright test --config=playwright.baseline.config.js` BEFORE the code change to establish the current UI behavior. After the change, run again to confirm no UI regression. The `density.spec.js` snapshot tests are NOT in scope here (they have their own baseline; this commit doesn't touch WebUI).

The baseline run also surfaced an unrelated issue: the WebUI on port 18083 takes >60s for Chromium to render the home page (24 JS files + 185KB app.js). This is the user's "форма не актуализируется" pain and is addressed in Wave B.3, not in this commit.
