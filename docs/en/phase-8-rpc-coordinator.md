# Phase 8 — P.1 rpc_coordinator production mode (2026-07-10)

> **Branch:** `centurion` (from current HEAD `25d614c`)
> **Period:** Session 1 of 2-3 (1 session ~3-4 h)
> **Priority:** 🔴 P0 (blocks 1.0 release)
> **Session 1 scope:** Step 1 (config expansion) + Step 4 (Circuit Breaker) + Step 2 skeleton (types only)
> **Sessions 2-3:** Step 2 full (ServeHTTP + streaming + 6 unit tests) + Step 3 (main.go wiring)

---

## Context

After Sessions 5-12 (B1-B8) the RPC Model Distribution framework is implemented:
- `cmd/rpcworker` (HTTP+gRPC, 7 endpoints)
- `internal/rpccoordinator.ModelCoordinator` (heartbeat, selector, binary protocol, metrics, TP)
- `internal/balancer/rpc_modules.go` (`initRpcModules()` creates `p.rpcCoordinator` if `cfg.RpcCoordinator.Enabled`)

`/api/v1/rpc/workers`, `/api/v1/rpc/models`, etc. — management endpoints work (B2).

**What does NOT work** (Phase 8 will close):
- **OperatingMode=rpc_coordinator** for `POST /api/generate` does NOT route inference through `ModelCoordinator.executePipeline()` — falls back to regular `proxyRequest` (see plan §P.1, line 103-104: "❌ Missing: production-grade integration in `Proxy.ServeHTTP` for `/api/generate` → `executePipeline()`").

---

## Current state (what already exists)

From exploration (Phase 8.0):

| Component | File | Status |
|---|---|---|
| `OperatingMode string` field | `pkg/types/balancing.go:81` | ✅ |
| `RpcCoordinatorConfig` struct (basic) | `pkg/types/rpc_variants.go:41-48` | ✅ (needs expansion) |
| `cfg.RpcCoordinator.Enabled` | `internal/balancer/rpc_modules.go:125` | ✅ |
| `initRpcModules()` creates `p.rpcCoordinator` | `internal/balancer/rpc_modules.go:124-134` | ✅ |
| `p.HasDistributedModel(modelName)` | `internal/balancer/rpc_modules.go:143-148` | ✅ |
| `p.InferDistributed(ctx, model, prompt, params)` | `internal/balancer/rpc_modules.go:151-161` | ✅ (non-streaming) |
| `p.rpcDispatcher` field | — | ❌ missing |
| `OperatingMode=rpc_coordinator` → `executePipeline` routing | `Proxy.ServeHTTP` | ❌ missing |
| `CircuitBreaker` (Closed/Open/HalfOpen) | — | ❌ missing |
| `isRpcCoordinatorMode(cfg)` helper | — | ❌ missing |
| `cfg.example.json` rpc_coordinator section | `config/config.example.json` | ❌ missing |
| `cmd/balancer/main.go` `embedded` mode wiring | — | ❌ missing |

---

## Session 1 scope (Phase 8.1 + 8.2 + 8.3 + 8.4-skeleton)

### Phase 8.1: OperatingMode helper + RpcCoordinatorConfig expansion (~80 LOC)

**Files:**
- `pkg/types/balancing.go` — add `OperatingModeRpcCoordinator = "rpc_coordinator"` constant (if not already present) + `isRpcCoordinatorMode(mode string) bool` helper.
- `pkg/types/rpc_variants.go` — extend `RpcCoordinatorConfig`:
  ```go
  type RpcCoordinatorConfig struct {
      Enabled         bool          `json:"enabled"`
      CoordinatorURL  string        `json:"coordinatorURL"`   // for external mode
      Embedded        bool          `json:"embedded"`         // true = start ModelCoordinator inside balancer
      Workers         []string      `json:"workers"`          // initial worker URLs (Embedded mode)
      WorkerPort      int           `json:"workerPort"`       // (legacy) port for rpcworker discovery
      FailoverPolicy  string        `json:"failoverPolicy"`   // "circuit_breaker" | "retry" | "fail_fast"
      RequestTimeout  time.Duration `json:"requestTimeout"`   // default 30s
      StreamTimeout   time.Duration `json:"streamTimeout"`    // default 5min
      Protocol        string        `json:"protocol"`         // (legacy) "http" | "grpc"
      Timeout         string        `json:"timeout"`          // (legacy) "30s"
      MaxRetries      int           `json:"maxRetries"`       // (legacy)
  }
  ```
- `internal/balancer/operating_modes.go` (new file, ~30 LOC):
  ```go
  // isRpcCoordinatorMode returns true if balancer is in rpc_coordinator mode.
  // (Phase 8: P.1 — dispatcher routing).
  func isRpcCoordinatorMode(mode string) bool {
      return mode == "rpc_coordinator" || mode == "rpc-coordinator"  // legacy alias
  }
  ```
- `internal/balancer/operating_modes_test.go` (new, ~50 LOC, 3 tests):
  - `TestIsRpcCoordinatorMode_True`: "rpc_coordinator" → true
  - `TestIsRpcCoordinatorMode_False`: "standard", "replication", "" → false
  - `TestIsRpcCoordinatorMode_LegacyAlias`: "rpc-coordinator" → true (legacy hyphenated)

### Phase 8.2: config.example.json rpc_coordinator section (~30 LOC)

**Files:**
- `config/config.example.json` — add the `"rpcCoordinator": {...}` section with examples of all fields + comments.
- (optional) `config/config.bundled.json` — if a bundled deployment should have rpc_coordinator disabled (default — `enabled: false`).

### Phase 8.3: Circuit Breaker (~150 LOC + 8 tests)

**Files:**
- `internal/rpccoordinator/circuit_breaker.go` (new, ~120 LOC):
  ```go
  // CircuitBreaker protects against cascading failures to a single worker.
  // Implements Closed/Open/HalfOpen state machine with configurable failure
  // threshold and reset timeout. (Phase 8: P.1 — required for production
  // rpc_coordinator mode where flaky workers can OOM the coordinator.)
  type CircuitBreaker struct {
      mu               sync.Mutex
      state            CBState          // Closed/Open/HalfOpen
      failureCount     int
      successCount     int
      failureThreshold int              // default 5
      resetTimeout     time.Duration    // default 30s
      openSince        time.Time        // when state -> Open
      halfOpenInFlight int              // number of test requests in HalfOpen
  }

  type CBState int
  const (
    StateClosed CBState = iota
    StateOpen
    StateHalfOpen
  )

  func NewCircuitBreaker(failureThreshold int, resetTimeout time.Duration) *CircuitBreaker
  func (cb *CircuitBreaker) Allow() bool
  func (cb *CircuitBreaker) RecordSuccess()
  func (cb *CircuitBreaker) RecordFailure()
  func (cb *CircuitBreaker) State() CBState
  func (cb *CircuitBreaker) OnStateChange(callback func(from, to CBState))
  ```
- `internal/rpccoordinator/circuit_breaker_test.go` (new, ~200 LOC, 8 tests):
  - `TestCircuitBreaker_InitialState_Closed`
  - `TestCircuitBreaker_NFailures_Opens`
  - `TestCircuitBreaker_AfterTimeout_HalfOpen`
  - `TestCircuitBreaker_HalfOpen_Success_Closes`
  - `TestCircuitBreaker_HalfOpen_Failure_Reopens`
  - `TestCircuitBreaker_ConcurrentAllow_ThreadSafe`
  - `TestCircuitBreaker_OnStateChange_Fires`
  - `TestCircuitBreaker_CustomThresholds`

### Phase 8.4: RpcCoordinatorDispatcher skeleton (~100 LOC, no tests yet)

**Files:**
- `internal/balancer/rpc_coordinator_dispatcher.go` (new, ~100 LOC):
  ```go
  // RpcCoordinatorDispatcher routes inference requests through ModelCoordinator
  // when balancer is in rpc_coordinator mode. For Phase 8.4 we only define
  // the type + routing decision; full ServeHTTP + streaming comes in Phase 9.
  type RpcCoordinatorDispatcher struct {
      coordinator *rpccoordinator.ModelCoordinator
      proxy       *Proxy
      fallback    *http.Client
      logger      *zap.SugaredLogger
  }

  // NewRpcCoordinatorDispatcher creates a dispatcher.
  func NewRpcCoordinatorDispatcher(coord *rpccoordinator.ModelCoordinator, p *Proxy) *RpcCoordinatorDispatcher

  // IsRpcPath returns true if path is one of the intercepted RPC paths.
  func (d *RpcCoordinatorDispatcher) IsRpcPath(path string) bool

  // ShouldRoute returns true if dispatcher should handle the request
  // (model is distributed and circuit breaker for coordinator is closed).
  func (d *RpcCoordinatorDispatcher) ShouldRoute(modelName string) bool

  // InferNonStreaming performs a non-streaming RPC inference.
  // (Phase 9 will add streaming + full HTTP handler.)
  func (d *RpcCoordinatorDispatcher) InferNonStreaming(ctx context.Context, req *InferRequest) (*InferResponse, error)

  // InferRequest / InferResponse — type aliases for rpccoordinator package.
  type InferRequest = rpccoordinator.InferRequest
  type InferResponse = rpccoordinator.InferResponse
  ```
- **Tests:** deferred to Phase 9 (needs ServeHTTP mock which requires Stream wiring first).

### Phase 8.5 (minimal): Proxy.rpcDispatcher field + getter + Proxy.ServeHTTP interception scaffold

**Files:**
- `internal/balancer/proxy.go` — add field `rpcDispatcher *RpcCoordinatorDispatcher` + `GetRpcCoordinatorDispatcher()` getter.
- `internal/balancer/proxy.go:ServeHTTP` — add scaffold block:
  ```go
  // Phase 8.5: rpc_coordinator mode interception (scaffold).
  // Full routing + streaming — Phase 9.
  if isRpcCoordinatorMode(p.config.Balancing.OperatingMode) && p.rpcDispatcher != nil {
      if p.rpcDispatcher.IsRpcPath(r.URL.Path) {
          // Full dispatcher.ServeHTTP in Phase 9.
          // For now, fall back to regular proxy so we don't break existing flows.
          // (Phase 9 will replace with: p.rpcDispatcher.ServeHTTP(w, r); return)
      }
  }
  ```
  This is safe: if dispatcher == nil (default) or path != rpc path — execution continues as usual.

**Tests:** minimal, verify that existing proxy tests are not broken.

---

## Session 1 acceptance criteria

1. **Phase 8.1** ✅
   - `OperatingModeRpcCoordinator = "rpc_coordinator"` const exists in `pkg/types/balancing.go`.
   - `isRpcCoordinatorMode(mode)` helper in `internal/balancer/operating_modes.go` (3 unit tests pass).
   - `RpcCoordinatorConfig` extended with Embedded, Workers, FailoverPolicy, RequestTimeout, StreamTimeout.

2. **Phase 8.2** ✅
   - `config/config.example.json` contains the example rpc_coordinator section with all fields + comments.
   - `config/config.bundled.json` rpc_coordinator disabled (default) — bundled deployment continues to work in standard mode.

3. **Phase 8.3** ✅
   - `CircuitBreaker` implements Closed/Open/HalfOpen.
   - 8 unit tests in `circuit_breaker_test.go` PASS (initial state, N failures, timeout -> half-open, half-open success/failure, concurrent safety, state change callback, custom thresholds).

4. **Phase 8.4** ✅
   - `RpcCoordinatorDispatcher` type created with `IsRpcPath`, `ShouldRoute`, `InferNonStreaming`.
   - Compile OK (`go build ./...`).

5. **Phase 8.5** ✅
   - `p.rpcDispatcher` field + `GetRpcCoordinatorDispatcher()` getter.
   - `Proxy.ServeHTTP` has a scaffold block for future integration (without changing behavior).
   - All existing proxy tests PASS (no regression).

6. **Build + tests** ✅
   - `go build -tags llama_stub ./cmd/{cppworker,balancer,agent}/` — exit 0.
   - `go test -tags llama_stub -count=1 ./internal/... ./cmd/... ./pkg/...` — all PASS.
   - `go test -tags llama_stub -count=1 -race ./internal/rpccoordinator/` — all PASS (8 CB + existing 60+).

---

## Files (Session 1)

### Created
- `internal/balancer/operating_modes.go` (~30 LOC)
- `internal/balancer/operating_modes_test.go` (~50 LOC)
- `internal/balancer/rpc_coordinator_dispatcher.go` (~100 LOC)
- `internal/rpccoordinator/circuit_breaker.go` (~120 LOC)
- `internal/rpccoordinator/circuit_breaker_test.go` (~200 LOC)
- `docs/phase-8-rpc-coordinator.md` (this file)

### Modified
- `pkg/types/balancing.go` — `OperatingModeRpcCoordinator` const (if not already present)
- `pkg/types/rpc_variants.go` — extend `RpcCoordinatorConfig` (Embedded, Workers, FailoverPolicy, RequestTimeout, StreamTimeout)
- `internal/balancer/proxy.go` — `rpcDispatcher` field + `GetRpcCoordinatorDispatcher()` + scaffold in `ServeHTTP`
- `config/config.example.json` — rpc_coordinator section
- `config/config.bundled.json` — `rpcCoordinator: {enabled: false}` (default — disabled)

### Not touched (Session 2+)
- `cmd/balancer/main.go` (Step 3 — Session 2)
- Streaming + full ServeHTTP (Step 2 full — Session 2)
- Dispatcher unit tests (Session 2 — needs full ServeHTTP mock)

---

## Estimate

- Phase 8.1 (config): 30 min
- Phase 8.2 (config.example): 20 min
- Phase 8.3 (Circuit Breaker + 8 tests): 60-90 min
- Phase 8.4 (Dispatcher skeleton): 30 min
- Phase 8.5 (Proxy scaffold): 20 min
- Verification + commit: 15 min

**Total: ~3-3.5 h**

---

## Sessions 2-3 (deferred)

### Session 2 (~3-4 h): Step 2 full + Step 3
- `RpcCoordinatorDispatcher.ServeHTTP(w, r)` — full implementation (250 LOC)
  - Parse `/api/generate`, `/api/chat`, `/v1/chat/completions`, `/v1/completions`
  - Read `model` from body, check `ShouldRoute(modelName)`
  - If yes: `coordinator.Infer(ctx, req)` (non-streaming) or `coordinator.InferStream` (streaming SSE passthrough)
  - If no: `p.ServeHTTP(w, r)` (transparent fallback)
  - Error handling: 503 if all workers unhealthy, 502 if worker 5xx, retry on timeout
  - 6 unit tests (NonDistributedModel_Fallback, DistributedModel_Infer, AllWorkersUnhealthy_503, Streaming_Passthrough, CircuitBreaker_Opens, AuthMissing_401)
- `cmd/balancer/main.go` — `embedded` mode wiring
  - If `cfg.RpcCoordinator.Embedded && OperatingMode == "rpc_coordinator"` — start `ModelCoordinator` locally
  - Start `HeartbeatLoop` for workers
  - Register `RpcCoordinatorDispatcher` in `Proxy`

### Session 3 (~2-3 h): polish + e2e + docs
- `tests/rpc_coordinator_e2e_test.go` — end-to-end via httptest (2 workers + coordinator + dispatcher)
- `docs/rpc-coordinator.md` — extend the "Production mode" section with docker-compose + env examples
- `CHANGELOG.md` — subsection `### Added (Session P.1 — rpc_coordinator production mode)`
- Coverage check: `go test -coverprofile` for `rpccoordinator` (target >85%)

---

## Related documents

- [`plans/2026-q3-production-ready-plan.md`](../plans/2026-q3-production-ready-plan.md) — main plan §P.1
- [`plans/2026-q3-roadmap.md`](../plans/2026-q3-roadmap.md) — section 3.2
- [`docs/rpc-coordinator.md`](../docs/rpc-coordinator.md) — B1-B8 details (to be extended in Session 2-3)
- [`docs/phase-7-style-compliance.md`](../docs/phase-7-style-compliance.md) — previous phase (style cleanup)

---

**Prepared:** 2026-07-10 22:40 MSK (Phase 8 — P.1 rpc_coordinator production mode, Session 1 of 3)
**After Session 1:** ready for Session 2 (Step 2 full + Step 3).

---

## Update 2026-07-11: P.1 COMPLETE (Sessions 2 + 3 done)

Session 2 (`9fe639f`) — full non-streaming `ServeHTTP` + main.go wiring.
Session 3 (`143b69b`) — e2e tests + streaming SSE + circuit breaker + auth.

**P.1 (rpc_coordinator production mode) — CLOSED.**

Production-ready: balancer in `OperatingMode=rpc_coordinator` routes
all 6 inference paths (Ollama generate/chat, OpenAI chat/completion) through
`ModelCoordinator`, supports streaming SSE passthrough, circuit breaker
per worker, and token-based auth.

See:
- `docs/rpc-coordinator.md` — user guide (full documentation).
- `CHANGELOG.md` section "Phase 8: rpc_coordinator production mode (P.1)" — version history.
- 21 e2e tests in `internal/balancer/rpc_coordinator_dispatcher_e2e_test.go`.
