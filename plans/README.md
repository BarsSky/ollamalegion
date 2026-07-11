# OllamaLegion — Roadmap (живой документ)

> **Дата обновления:** 2026-07-11 (Phase 8 final)
> **Назначение:** единственный источник правды по реализованному и оставшемуся в проекте OllamaLegion.
> Все устаревшие/завершённые планы — в `plans/archive/`.
> **HEAD:** `4c706b0` on branch `centurion` (v1.0-rc1 released).

---

## 0. Планы в работе (WIP)

| План | Файл | Сессия | Статус |
|------|------|--------|--------|
| [Session F — UI/UX quick wins (5.3, 5.4, 5.5, 5.7)](2026-q3-session-f-quick-wins.md) | `plans/2026-q3-session-f-quick-wins.md` | Session F (июнь 2026) | ✅ Session F ПОЛНОСТЬЮ ЗАКРЫТ (F.0a/b/α/β/γ + F.4) |
| **[7.1a — Self-hosted CI runner](2026-q3-roadmap.md#7-cicd-и-тестирование)** | `scripts/setup-runner.ps1` + `scripts/check-runner.ps1` + `docs/ci/self-hosted-runner.md` | Месяц 1 (июль 2026) | ✅ DONE 2026-06-28 (commit `01b2afc`) |
| **[7.1 — GitHub Actions CI workflow](2026-q3-roadmap.md#7-cicd-и-тестирование)** | `.github/workflows/ci.yml` + `.golangci.yml` | Месяц 1 (июль 2026) | ✅ DONE 2026-06-28 (commit `64e100d`) |
| [Production-ready (P.1-P.4)](2026-q3-production-ready-plan.md) | `plans/2026-q3-production-ready-plan.md` | Phase 8 (июль 2026) | ✅ **DONE 2026-07-11**: P.1 ✅ P.2 ✅ P.3 ✅ (research) P.4 ✅ |

**Все планы Q3 W3-4 + Phase 8 реализованы.** Проект готов к 1.0 release (требуется manual hardware smoke).

### Phase 8 deliverables (полный список, 2026-07-11)

| Компонент | Коммиты | Статус |
|-----------|---------|--------|
| **P.1 rpc_coordinator** (дистрибутивный inference) | `b10560e` (foundation) → `1ea5ba3` (docs) | ✅ production mode |
| **P.2 virtual_router** (alias-on-pool) | `a320f3a` → `7c79572` (WebUI) | ✅ 7 steps complete |
| **P.2 backlog: auth** | `1b0e537` | ✅ |
| **P.2 backlog: auto-failover** | `554926e` | ✅ |
| **P.3 research** (real ggml/NCCL) | `418f008` | ✅ research, post-1.0 |
| **P.4 (3.1) layer-mode TP** | `85b0be7` | ✅ wired via env vars |
| **Smoke test 1.0** | `cc20ee3` | ✅ 8 sub-flows + GracefulShutdown |
| **Mobile-responsive** | `d8ac0d4` | ✅ Monitor tab + standalone pages |
| **Item 2: LoadProvider** | `8e9ab04` | ✅ MaxConcurrentReqs - ActiveReqs |
| **Item 7: nav link Virtual Models** | `809db07` | ✅ |
| **Test coverage expansion** | `b08f9ce` (133 tests) + `08dda56` (52) + `87ba924` (80) | ✅ 231 total scenarios |
| **cppworker simulator + heavy model tests** | (this commit) | ✅ 18 new tests |
| **i18n EN docs** | `02c4c58` (10 EN translations) + `4c706b0` (audit) | ✅ 19 EN docs |
| **v1.0-rc1** | `tag v1.0-rc1` | ✅ released |

### Acceptance criteria (§8 production-ready plan)

| AC | Статус |
|----|--------|
| P.1 end-to-end + circuit breaker + SSE | ✅ |
| P.2 round-robin/least-loaded + CRUD + WebUI | ✅ |
| P.4 CI + coverage + badges | ✅ (workflow) + ⚠️ (coverage 53.3% / 71.1% / 89.0%) |
| Все P.1-P.2 тесты PASS (~40+ unit + e2e) | ✅ **231 scenarios + 95+ new** |
| Build OK | ✅ `go build -tags llama_stub ./...` |
| CHANGELOG секции для каждого P.x | ✅ |
| Documentation обновлена | ✅ RU + EN (19 docs each, parity ~85%) |
| Real production configs | ✅ `docker-compose.cppworker-bundled-with-agent.yml` |

---

## 1. Реализовано (подтверждено кодом и тестами)

### 1.1 Phase 8 — production-ready (2026-07-11)

| Компонент | Файл | Тест | Статус |
|-----------|------|------|--------|
| **P.1 RpcCoordinatorDispatcher** (4 response shapes + streaming + auth + CB) | `internal/balancer/rpc_coordinator_dispatcher.go` | `rpc_coordinator_dispatcher_e2e_test.go` (21 tests) | ✅ production mode |
| **P.2 VirtualRouter** (3 selectors + model rewrite + headers) | `internal/balancer/virtual_router.go` | `virtual_router_test.go` (50+ tests) | ✅ production mode |
| **P.2 VirtualRouter failover** (5xx retry, 4xx passthrough) | `internal/balancer/virtual_router.go:proxyToBackend` | `virtual_router_test.go:TestVR_Scenario_Failover_*` | ✅ |
| **Auth (P.1 + P.2)** | `internal/balancer/auth_checker.go` | `auth_checker_test.go` | ✅ |
| **Circuit Breaker per worker** | `internal/rpccoordinator/circuit_breaker.go` | `circuit_breaker_test.go` (8 tests) | ✅ |
| **Per-model profiles** (3-tier n_ctx resolver) | `internal/balancer/num_ctx_resolver.go` | `num_ctx_resolver_test.go` | ✅ |
| **Tool calling (7 formats)** | `internal/balancer/llamacpp_toolcall_detector*.go` | `llamacpp_toolcall_detector_test.go` | ✅ |
| **LoadProvider** (MaxConcurrentReqs - ActiveReqs) | `internal/balancer/backend_registry.go:GetBackendFreeSlots` | `scenarios_backend_lifecycle_test.go` | ✅ |
| **Scenario tests** (213 total) | `internal/balancer/scenarios_*.go` | (6 files) | ✅ |
| **Smoke test 1.0** (8 sub-flows) | `internal/balancer/smoke_test_1_0_test.go` | `TestSmoke_1_0Release_*` | ✅ |
| **cppworkerSimulator** (full lifecycle) | `internal/balancer/cppworker_simulator_test.go` | (16 cppworker tests) | ✅ |
| **Heavy model scenarios** (70B split, 72B load) | `internal/balancer/scenarios_cppworker_test.go` | `TestVirtualRouter_HeavyModel_*`, `TestRpcCoordinator_70B_*` | ✅ |

### 1.2 Backend Type Isolation (commit `affb4e8`)

| Компонент | Файл | Тест |
|---|---|---|
| `selectBackend(model, bt)` — фильтрация по типу | `internal/balancer/backend_selector.go` | `TestSelectBackend_FiltersByType_OllamaOnly` |
| `expandCandidates(model, allowedTypes)` | `internal/balancer/candidate.go` | `TestExpandCandidates_FiltersByType` |
| `proxyRequest` — проверка `IsModeCompatibleWithBackendType` | `internal/balancer/proxy_request.go` | `TestServeHTTP_LlamaCppRequest_RoutedToLlamaCppBackend` |
| `determineRequestBackendType(r)` | `internal/balancer/proxy.go` | `DetermineRequestBackendTypeForTest` |
| CRUD валидация при `addBackend`/`updateBackend` | `internal/api/handlers_backends.go` | (integration) |
| WebUI бейджи 🦙/🦒 в Dashboard/Backends/Monitor | `webui/js/modules/renderers.js`, `monitor/ui-renderer.js`, `monitor/backend-type-badges.js` | manual |

### 1.3 CppWorker: Ollama-совместимость + защита от двойной загрузки

| Endpoint / фича | Файл | Статус |
|---|---|---|
| `/api/generate`, `/api/ollama/generate` — `runGenerateCore` | `cmd/cppworker/handlers_generate.go` | ✅ |
| `/api/chat` — chat template из GGUF, tool_calls | `cmd/cppworker/handlers_chat.go` | ✅ |
| `/api/embeddings` | `c/bridge/bridge.go` (`GetEmbeddings`) | ✅ |
| `/api/tags`, `/api/show`, `/api/copy`, `/api/create`, `/api/pull` (hf:), `/api/push` (HTTP 501), `/api/delete` | `internal/balancer/llamacpp_handlers_*.go` | ✅ |
| `handleLoadModel` — dedup via `sameLoadOptions` | `cmd/cppworker/handlers_model.go` | ✅ |
| `applyCppCtxHeader` — X-Cpp-Ctx upper-limit | `cmd/cppworker/main.go` | ✅ |
| `applyKeepAlive` — keep_alive == "0" → UnloadModel | `cmd/cppworker/handlers_generate.go` | ✅ |
| `tryRamFallbackReload` — reload on bigger n_ctx | `cmd/cppworker/inference.go` | ✅ |
| Tools/tool_calls (7 formats) | `cmd/cppworker/tool_calls.go` | ✅ |
| `ReloadLoopLimitError` (3 attempts / 60s) | `cmd/cppworker/inference.go` | ✅ |
| **GGUF Backend Proxy** (14 endpoints via balancer) | `internal/api/gguf_backend_proxy.go` | ✅ |

### 1.4 Per-Model Profiles + n_ctx

| Endpoint | Файл | Тест |
|---|---|---|
| `GET/PUT/DELETE/apply /api/v1/cppworker/model-profiles/{name}` | `internal/api/handlers_cppworker_profiles.go` | `handlers_cppworker_profiles_test.go` |
| 3-tier n_ctx resolver (body → profile → backend default) | `internal/balancer/num_ctx_resolver.go` | `num_ctx_resolver_test.go` |
| `clampNPredictToFitContext` | `internal/balancer/nctx_clamp.go` | (unit) |
| `NCtxReloadCoordinator` | `internal/balancer/nctx_reload.go` | `nctx_reload_test.go` |
| Reload dedup (3 attempts / 60s) | `internal/balancer/nctx_reload_dedup.go` | `nctx_reload_sync_test.go` |
| Preflight reload | `internal/balancer/preflight_nctx*.go` | (unit) |

### 1.5 WebUI (P0–P2 — 100%)

| Компонент | Файл | Статус |
|---|---|---|
| Dashboard, Backends, Models, Sessions, Queue, Logs, Settings | `webui/index.html`, `webui/js/modules/renderers.js` | ✅ |
| **Virtual Models page** (Phase 8 P.2) | `webui/virtual-models.html` | ✅ CRUD UI + i18n |
| **Monitor tab** + mobile-responsive | `webui/monitor.html`, `webui/css/monitor.css` | ✅ |
| **Language switcher** (EN/RU) | `webui/js/i18n/index.js` | ✅ 1072 keys, perfect parity |
| i18n parity test | `internal/api/lint_css_i18n_test.go:TestI18nKeyParity_EN_RU` | ✅ |

### 1.6 Metrics + Monitoring (Phase 7 + 8)

- Two independent mechanisms: poller (cppworker `/api/models`) + agent (NVML + /proc)
- 264 `data-i18n` attributes in index.html
- 3 metrics broker tests + cluster state tests

### 1.7 CI/CD

- `.github/workflows/ci.yml` — 4 jobs
- `.golangci.yml` — 8 linters
- `scripts/setup-runner.ps1` + `docs/ci/self-hosted-runner.md`
- `TestLintCSSAndI18nNoEmDash` + `TestI18nKeyParity_EN_RU`

---

## 2. В работе / Post-1.0 backlog

| План | Файл | Статус |
|------|------|--------|
| **P.3 Real ggml/NCCL** (Tensor Parallelism) | `plans/b8-tensor-parallelism-plan.md` | ⏳ research-grade, post-1.0 (Q4 2026+) |
| Pipeline Parallelism (4.1) | (post-1.0) | ⏳ |
| Expert Parallelism для MoE (4.2) | (post-1.0) | ⏳ |
| Continuous batching (4.5) | (post-1.0) | ⏳ |
| Security hardening (mTLS, rate limiting, audit) | (post-1.0) | ⏳ |
| Manual hardware smoke (A10 + Qwen3-A3B) | (требуется user) | ⏳ блокирует v1.0 final |

---

## 3. Архив (завершённые планы)

Все 44 файла в `plans/archive/` отражают завершённые фазы (Sessions A-F, 1-19, аудит-планы, etc.).

---

## 4. Quality metrics (Phase 8 final)

| Метрика | Значение |
|---------|----------|
| **Coverage: balancer** | 53.3% |
| **Coverage: rpccoordinator** | 71.1% |
| **Coverage: rptensor** | 89.0% |
| **Total scenario tests** | 231 (3.2s runtime) |
| **Total test files** | 153+ (across 10 packages) |
| **Build OK** | ✅ all 3 binaries (balancer, cppworker, agent) |
| **Lint OK** | ✅ (em-dash, i18n parity) |
| **EN docs** | 19 (parity with RU) |
| **i18n keys** | 1072 (en.js / ru.js perfect parity) |
| **WebUI pages** | 7 (Dashboard, Backends, Models, Sessions, Queue, Logs, Settings, Monitor, Virtual Models) |

---

## 5. v1.0 Release Plan

1. ✅ Phase 8 complete (P.1-P.4 + smoke + i18n + tests) — 2026-07-11
2. ✅ Tag `v1.0-rc1` released
3. ⏳ Manual hardware smoke (A10 + Qwen3-A3B) — user
4. ⏳ Tag `v1.0` final — after smoke
