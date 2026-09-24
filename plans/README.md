# OllamaLegion — Roadmap (живой документ)

> **Дата обновления:** 2026-09-24 (Round 74 — placement policy P1: pool/replicated исполняются политикой)
> **Назначение:** единственный источник правды по реализованному и оставшемуся в проекте OllamaLegion.
> Все устаревшие/завершённые планы — в `plans/archive/`.
> **HEAD:** `19fed24` on branch `centurion` + R74 (placement P1: `MatchVirtualRequest`, группы репликации по политике, auto-подмножество §4).
> **Live stack:** `ol-bundled-balancer:r73-submodule-v11` (R74-образ `r74-submodule-v12` проверен на throwaway-стенде) + `ol-bundled-cppworker-gpu:gpu-r70-submodule-v3` + `ol-bundled-webui:r70-submodule-v1` + `ol-bundled-cppworker-gpu-agent:cppworker-bundled-r41-agent-x-api-token` (4 healthy).
> **Проверено на живом стенде (R68/R69/R71/R72/R73/R74):** Cline (VS Code, провайдер ollama, Model Context Window = 65536) получает 200 и ответ модели; модель грузится на `ctx=65536, kv=q4_0` на RTX 3070 8 GB; агент применяет `maxConcurrentRequests=1`; placement-политика резолвится и видна в `/api/v1/placement`; единая очередь отдаёт `X-Queue-*`; при `operatingMode=standard` политика обслуживает алиас через пул и модель через реплики.

---

## 0. Планы в работе (WIP)

| План | Файл | Сессия | Статус |
|------|------|--------|--------|
| **[Admission-очередь и per-user сессии](2026-09-23-admission-queue-and-sessions.md)** | `plans/2026-09-23-admission-queue-and-sessions.md` | R67a/R67b (сентябрь 2026) | ✅ **РЕАЛИЗОВАНО** (R67b): admission-очередь слотов (`LB_ADMISSION_WAIT_SEC`, `X-Queue-Position`/`X-Queue-Wait-Ms`, 503 только при исчерпанном ожидании), per-session справедливость, различение пользователей/чатов OpenWebUI (`X-User-Id`/`X-OpenWebUI-User-Id`/тело+`metadata`), блок `admission` в `/api/v1/queue/stats`, 7 тестов + живая проверка (3 пользователя → 3 сессии; 10 запросов при 4 слотах → 200 с ожиданием). Осталось (не в жалобе): keepalive для streaming-ожидающих, связь `maxConcurrentReqs` ↔ `n_parallel`, карточка очереди в WebUI |
| [Session F — UI/UX quick wins (5.3, 5.4, 5.5, 5.7)](2026-q3-session-f-quick-wins.md) | `plans/2026-q3-session-f-quick-wins.md` | Session F (июнь 2026) | ✅ Session F ПОЛНОСТЬЮ ЗАКРЫТ (F.0a/b/α/β/γ + F.4) |
| **[7.1a — Self-hosted CI runner](2026-q3-roadmap.md#7-cicd-и-тестирование)** | `scripts/setup-runner.ps1` + `scripts/check-runner.ps1` + `docs/ci/self-hosted-runner.md` | Месяц 1 (июль 2026) | ✅ DONE 2026-06-28 (commit `01b2afc`) |
| **[7.1 — GitHub Actions CI workflow](2026-q3-roadmap.md#7-cicd-и-тестирование)** | `.github/workflows/ci.yml` + `.golangci.yml` | Месяц 1 (июль 2026) | ✅ DONE 2026-06-28 (commit `64e100d`) |
| [Production-ready (P.1-P.4)](2026-q3-production-ready-plan.md) | `plans/2026-q3-production-ready-plan.md` | Phase 8 (июль 2026) | ✅ **DONE 2026-07-11**: P.1 ✅ P.2 ✅ P.3 ✅ (research) P.4 ✅ |
| **[Round 35 — CppWorker bundled-with-agent r35 (2026-08-12)](round-35-cppworker-bundled-r35.md)** | `plans/round-35-cppworker-bundled-r35.md` | Round 35 (август 2026) | ✅ **DONE 2026-08-13**: 4-phase preflight + reload→load fallback + cgo SIGSEGV recover + idleUnload SIGSEGV guard |
| **[Round 35c — env-tunable async load polling (2026-08-13)](round-35c-env-tunable-polling.md)** | `plans/round-35c-env-tunable-polling.md` | Round 35c (август 2026) | ✅ **DONE 2026-08-13**: 22GB Qwen3.6 на 3070 hit 8m7s timeout. Fix: `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` / `_WAIT_MULTIPLIER` / `_WAIT_BUFFER_SEC` env vars. Image r35c. |
| **[Placement Policy — совместная работа режимов при 2+ бэкендах](2026-09-23-multi-backend-placement-policy.md)** | `plans/2026-09-23-multi-backend-placement-policy.md` | R66d (сентябрь 2026) | 🟡 **PROPOSAL**: per-model политика размещения (`single`/`pool`/`replicated`/`sharded`/`rpc` + `auto`) вместо глобального `operatingMode`; стандартная балансировка остаётся дефолтом, каждый нынешний режим выражается политикой. Открытый вопрос — транспорт раскладки (llama.cpp RPC vs B8.7) |

**Все планы Q3 W3-4 + Phase 8 + Round 35 реализованы; далее R36…R70** (см.
`CHANGELOG.md`): R67a/b — потолок n_ctx по KV-cache/VRAM, ожидание авто-загрузки,
admission-очередь и per-user сессии; R68 — профиль модели больше не потолок для
клиента, reload дожидается; R69 — реальный Cline (`max_output_tokens`), единый
gate reload'ов, AutoTune не режет клиентский n_ctx. R70 закрыл гигиену тестов,
keepalive ожидающих streaming-клиентов, карточку admission-очереди в
`/monitor`, KV-хинт в адаптивную стратегию и первую половину хвоста
«`maxConcurrentReqs` ↔ `n_parallel`». R71 закрыл этот хвост полностью:
вместимость бэкенда больше не переписывается «эхом» heartbeat агента.
R72 открыл placement policy (этап P0: resolution + наблюдаемость).
R73 свёл Ollama-путь в admission-очередь (единая очередь, хвост R70/R71 закрыт).
R74 реализовал placement policy P1: `pool`/`replicated` исполняются политикой,
`auto` — детерминированное подмножество §4.

### R74 (2026-09-24) — текущий раунд

Placement policy, этап P1 (`plans/2026-09-23-multi-backend-placement-policy.md`):

| # | Направление | Статус |
|---|-------------|--------|
| 1 | `pool` по политике: `VirtualRouter` включается для конкретной модели без `operatingMode=virtual_router` (`MatchVirtualRequest`, роутер при `virtualModels.enabled`) | ✅ сделано |
| 2 | `replicated` по политике: группа репликации создаётся политикой (идемпотентно), менеджер поднимается по требованию, работает штатный `replicationSelector` | ✅ сделано |
| 3 | `auto`: алиас → pool, `prefer=replicated` + хватает бэкендов → replicated, иначе single (флаг `refined`, причина в `reason`) | ✅ сделано |
| 4 | Наблюдаемость: блок `replication` в `GET /api/v1/placement`, `X-LB-Placement*` = исполненная стратегия | ✅ сделано (образ `r74-submodule-v12`) |
| 5 | Живая проверка до/после (2 заглушки, `operatingMode=standard`): алиас → `r74-physical` через пул, `r74-repl` → реплики A/B/A | ✅ проверено (CHANGELOG 0.5.32) |
| 6 | P1.5: полное правило `auto` по свободному VRAM/размеру модели | ⏳ следующий этап |
| 7 | P2 placement policy: `sharded`/`rpc` на реальном транспорте | ⏳ нужен выбор (llama.cpp RPC vs B8.7) |
| 8 | Гигиена: self-hosted CI-раннер `skyworker-ci` | ✅ раннер online, джоба success (R73) |

### R73 (2026-09-24) — закрытый раунд

Единая очередь (`plans/2026-09-23-admission-queue-and-sessions.md`):

| # | Направление | Статус |
|---|-------------|--------|
| 1 | `waitForInferenceBackend`: ожидание слота на любом бэкенде в admission-очереди (ключ `any`, per-session приоритеты) | ✅ сделано |
| 2 | `ServeHTTP` больше не использует legacy `queueRequest`/`QueueManager`; `503 + Retry-After + X-Queue-*` при таймауте | ✅ сделано |
| 3 | Backpressure (`≥90% queueMaxSize`) перенесён на admission-очередь | ✅ сделано |
| 4 | `QueueManager.RecordUnified` — `processed_total` и `/api/v1/queue/history` не «замерзают» | ✅ сделано |
| 5 | Живая проверка до/после (throwaway-стенд, вместимость 1, 4 параллельных запроса) | ✅ проверено (образ `r73-submodule-v11`, CHANGELOG 0.5.31) |
| 6 | P1 placement policy: исполнение `pool`/`replicated`/`auto` | ✅ сделано в R74 |
| 7 | P2 placement policy: `sharded`/`rpc` на реальном транспорте | ⏳ нужен выбор (llama.cpp RPC vs B8.7) |
| 8 | Гигиена: self-hosted CI-раннер `skyworker-ci` | ✅ **раннер снова online** (`gh api .../actions/runners` → `online`), джоба «Test (self-hosted Windows)» — success на коммите R73; все 7 проверок CI зелёные |

### R72 (2026-09-24) — закрытый раунд

Placement policy, этап P0 (`plans/2026-09-23-multi-backend-placement-policy.md`):

| # | Направление | Статус |
|---|-------------|--------|
| 1 | Типы и разбор `balancing.placement` (models/classes/fallback/override), маски имён, выражения размера | ✅ сделано |
| 2 | Резолвер `ResolvePlacement` с приоритетом запрос → модель → класс → глобальный дефолт + `reason`/`source`/`executable` | ✅ сделано |
| 3 | Валидация с понятными ошибками (`ValidateConfigOnLoad` + лог при старте) | ✅ сделано |
| 4 | Наблюдаемость: `GET /api/v1/placement`, заголовки `X-LB-Placement*`, лог решения | ✅ сделано (образ `r72-submodule-v9`) |
| 5 | Живая проверка P0 (endpoint + заголовки + временная политика на стенде) | ✅ проверено (см. CHANGELOG 0.5.30) |
| 6 | Гигиена окружения: entrypoint предупреждает о рассинхроне `config/config.json` ↔ `/app/data/config.json`, `LB_CONFIG_SYNC=1` | ✅ сделано (образ `r72-submodule-v10`) |
| 7 | Свести `QueueManager` (Ollama-путь) с admission-очередью | ✅ закрыто в R73 |

### R71 (2026-09-24) — закрытый раунд

Продолжение R70 (направления 6 и 7 из таблицы ниже):

| # | Направление | Статус |
|---|-------------|--------|
| 1 | Heartbeat агента не пишет вместимость; агент получает эффективную вместимость (`EffectiveMaxConcurrentRequests`) | ✅ сделано (образ `r71-submodule-v8`) |
| 2 | Персистентный признак «вместимость от ноды» + восстановление runtime-лимитов в `LoadState` + самоисцеление старого `state.json` | ✅ сделано |
| 3 | Единое правило вместимости вместо 4 копий (slot manager, `tryAcquireSlot`, least-loaded, метрики, `/api/v1/backends`) | ✅ сделано |
| 4 | Повторная регистрация агента больше не обнуляет runtime-поля бэкенда | ✅ сделано |
| 5 | Живая проверка: лимит оператора не откатывается, очередь считает слоты по нему | ✅ проверено (см. CHANGELOG 0.5.29) |
| 6 | Свести `QueueManager` (Ollama-путь) с admission-очередью | ⏳ перенесено в R72 |
| 7 | Placement policy (мультибэкенд) | ✅ P0 в R72 (`2026-09-23-multi-backend-placement-policy.md`) |

### R70 (2026-09-24) — закрытый раунд

Выбранные направления (по запросу пользователя после проверки Cline):

| # | Направление | Статус |
|---|-------------|--------|
| 1 | Гигиена: тесты не пачкают `tests/testdata/state.json` | ✅ сделано (`3b1e049`) |
| 2 | Гигиена: self-hosted CI-раннер `skyworker-ci` | ✅ **закрыто в R73**: раннер снова `online`, джоба «Test (self-hosted Windows)» — success |
| 3 | KV-хинт в адаптивную стратегию cppworker | ✅ сделано (образ `r70-submodule-v7`, CHANGELOG 0.5.28) |
| 4 | Keepalive для streaming-ожидающих (`LB_ADMISSION_KEEPALIVE_SEC`) | ✅ сделано (`395e301`) |
| 5 | Карточка очереди (`admission`) в WebUI `/monitor` | ✅ сделано (`721dead`) |
| 6 | Единая очередь: `maxConcurrentReqs` ↔ `n_parallel`, свести `QueueManager` | 🟡 вместимость = `n_parallel` ✅ (`2e3cf0b`) + эхо heartbeat ✅ (R71); свести `QueueManager` ⏳ |
| 7 | Placement policy (мультибэкенд) | ⏳ план (`2026-09-23-multi-backend-placement-policy.md`) |
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

## 4. Quality metrics (Phase 8 final + Round 35)

| Метрика | Значение |
|---------|----------|
| **Coverage: balancer** | 53.3% (Phase 8) → TBD (post Round 35) |
| **Coverage: rpccoordinator** | 71.1% |
| **Coverage: rptensor** | 89.0% |
| **Total scenario tests** | 231+ (Round 35 added preflight_nctx, nctx_reload, cppworker SIGSEGV tests) |
| **Total test files** | 153+ (across 10 packages) |
| **Build OK** | ✅ all 3 binaries (balancer, cppworker, agent) — Round 35 builds verified |
| **Lint OK** | ✅ (em-dash, i18n parity) |
| **EN docs** | 19 (parity with RU) |
| **i18n keys** | 1072 (en.js / ru.js perfect parity) |
| **WebUI pages** | 7 (Dashboard, Backends, Models, Sessions, Queue, Logs, Settings, Monitor, Virtual Models) |
| **Live binary** | `cppworker:gpu-86-abort-r35` (3.86GB) + `balancer:cppworker-bundled-r35` (61.6MB) |
| **Uptime after Round 35 deploy** | 26+ min, no SIGSEGV/panic |

---

## 5. v1.0 Release Plan

1. ✅ Phase 8 complete (P.1-P.4 + smoke + i18n + tests) — 2026-07-11
2. ✅ Tag `v1.0-rc1` released
3. ✅ Round 35 (4-phase preflight + 5 cascading bug fixes + cgo
   SIGSEGV recover + idleUnload SIGSEGV guard) — 2026-08-13
4. ⏳ Manual hardware smoke (A10 + Qwen3-A3B) — user
5. ⏳ Tag `v1.0` final — after smoke
6. ⏳ Real ggml/NCCL TP fix (P.3 post-1.0) — Q4 2026+
