# Phase 8 — P.1 rpc_coordinator production mode (2026-07-10)

> **Branch:** `centurion` (от текущего HEAD `25d614c`)
> **Период:** Session 1 of 2-3 (1 сессия ~3-4 ч)
> **Приоритет:** 🔴 P0 (блокирует 1.0 release)
> **Скоуп session 1:** Step 1 (config expansion) + Step 4 (Circuit Breaker) + Step 2 skeleton (types only)
> **Sessions 2-3:** Step 2 full (ServeHTTP + streaming + 6 unit tests) + Step 3 (main.go wiring)

---

## Контекст

После Sessions 5-12 (B1-B8) реализован RPC Model Distribution framework:
- `cmd/rpcworker` (HTTP+gRPC, 7 endpoints)
- `internal/rpccoordinator.ModelCoordinator` (heartbeat, selector, binary protocol, metrics, TP)
- `internal/balancer/rpc_modules.go` (`initRpcModules()` создаёт `p.rpcCoordinator` если `cfg.RpcCoordinator.Enabled`)

`/api/v1/rpc/workers`, `/api/v1/rpc/models`, etc. — management endpoints работают (B2).

**Что НЕ работает** (Phase 8 закроет):
- **OperatingMode=rpc_coordinator** при `POST /api/generate` НЕ маршрутизирует inference через `ModelCoordinator.executePipeline()` — fallback на обычный `proxyRequest` (см. plan §P.1, строка 103-104: "❌ Отсутствует: production-grade integration в `Proxy.ServeHTTP` для `/api/generate` → `executePipeline()`").

---

## Текущее состояние (что уже есть)

Из exploration (Phase 8.0):

| Компонент | Файл | Статус |
|---|---|---|
| `OperatingMode string` field | `pkg/types/balancing.go:81` | ✅ |
| `RpcCoordinatorConfig` struct (basic) | `pkg/types/rpc_variants.go:41-48` | ✅ (нужно расширить) |
| `cfg.RpcCoordinator.Enabled` | `internal/balancer/rpc_modules.go:125` | ✅ |
| `initRpcModules()` создаёт `p.rpcCoordinator` | `internal/balancer/rpc_modules.go:124-134` | ✅ |
| `p.HasDistributedModel(modelName)` | `internal/balancer/rpc_modules.go:143-148` | ✅ |
| `p.InferDistributed(ctx, model, prompt, params)` | `internal/balancer/rpc_modules.go:151-161` | ✅ (non-streaming) |
| `p.rpcDispatcher` field | — | ❌ отсутствует |
| `OperatingMode=rpc_coordinator` → `executePipeline` routing | `Proxy.ServeHTTP` | ❌ отсутствует |
| `CircuitBreaker` (Closed/Open/HalfOpen) | — | ❌ отсутствует |
| `isRpcCoordinatorMode(cfg)` helper | — | ❌ отсутствует |
| `cfg.example.json` rpc_coordinator section | `config/config.example.json` | ❌ отсутствует |
| `cmd/balancer/main.go` `embedded` mode wiring | — | ❌ отсутствует |

---

## Session 1 скоуп (Phase 8.1 + 8.2 + 8.3 + 8.4-skeleton)

### Phase 8.1: OperatingMode helper + RpcCoordinatorConfig expansion (~80 LOC)

**Файлы:**
- `pkg/types/balancing.go` — добавить `OperatingModeRpcCoordinator = "rpc_coordinator"` константу (если ещё нет) + `isRpcCoordinatorMode(mode string) bool` helper.
- `pkg/types/rpc_variants.go` — расширить `RpcCoordinatorConfig`:
  ```go
  type RpcCoordinatorConfig struct {
      Enabled         bool          `json:"enabled"`
      CoordinatorURL  string        `json:"coordinatorURL"`   // для external mode
      Embedded        bool          `json:"embedded"`         // true = поднять ModelCoordinator в balancer
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
- `internal/balancer/operating_modes.go` (новый файл, ~30 LOC):
  ```go
  // isRpcCoordinatorMode returns true if balancer is in rpc_coordinator mode.
  // (Phase 8: P.1 — dispatcher routing).
  func isRpcCoordinatorMode(mode string) bool {
      return mode == "rpc_coordinator" || mode == "rpc-coordinator"  // legacy alias
  }
  ```
- `internal/balancer/operating_modes_test.go` (новый, ~50 LOC, 3 теста):
  - `TestIsRpcCoordinatorMode_True`: "rpc_coordinator" → true
  - `TestIsRpcCoordinatorMode_False`: "standard", "replication", "" → false
  - `TestIsRpcCoordinatorMode_LegacyAlias`: "rpc-coordinator" → true (legacy hyphenated)

### Phase 8.2: config.example.json rpc_coordinator section (~30 LOC)

**Файлы:**
- `config/config.example.json` — добавить секцию `"rpcCoordinator": {...}` с примерами всех полей + комментарии.
- (опционально) `config/config.bundled.json` — если bundled deployment должен иметь rpc_coordinator отключенным (default — `enabled: false`).

### Phase 8.3: Circuit Breaker (~150 LOC + 8 tests)

**Файлы:**
- `internal/rpccoordinator/circuit_breaker.go` (новый, ~120 LOC):
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
- `internal/rpccoordinator/circuit_breaker_test.go` (новый, ~200 LOC, 8 тестов):
  - `TestCircuitBreaker_InitialState_Closed`
  - `TestCircuitBreaker_NFailures_Opens`
  - `TestCircuitBreaker_AfterTimeout_HalfOpen`
  - `TestCircuitBreaker_HalfOpen_Success_Closes`
  - `TestCircuitBreaker_HalfOpen_Failure_Reopens`
  - `TestCircuitBreaker_ConcurrentAllow_ThreadSafe`
  - `TestCircuitBreaker_OnStateChange_Fires`
  - `TestCircuitBreaker_CustomThresholds`

### Phase 8.4: RpcCoordinatorDispatcher skeleton (~100 LOC, no tests yet)

**Файлы:**
- `internal/balancer/rpc_coordinator_dispatcher.go` (новый, ~100 LOC):
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

**Файлы:**
- `internal/balancer/proxy.go` — добавить поле `rpcDispatcher *RpcCoordinatorDispatcher` + `GetRpcCoordinatorDispatcher()` getter.
- `internal/balancer/proxy.go:ServeHTTP` — добавить scaffold-блок:
  ```go
  // Phase 8.5: rpc_coordinator mode interception (scaffold).
  // Full routing + streaming — Phase 9.
  if isRpcCoordinatorMode(p.config.Balancing.OperatingMode) && p.rpcDispatcher != nil {
      if p.rpcDispatcher.IsRpcPath(r.URL.Path) {
          // Полный dispatcher.ServeHTTP в Phase 9.
          // Пока fallback на обычный прокси чтобы не сломать existing flows.
          // (Phase 9 заменит на: p.rpcDispatcher.ServeHTTP(w, r); return)
      }
  }
  ```
  Это безопасно: если dispatcher == nil (default) или path != rpc path — выполнение идёт дальше как обычно.

**Тесты:** minimal, проверить что не сломал existing proxy tests.

---

## Session 1 acceptance criteria

1. **Phase 8.1** ✅
   - `OperatingModeRpcCoordinator = "rpc_coordinator"` const exists in `pkg/types/balancing.go`.
   - `isRpcCoordinatorMode(mode)` helper in `internal/balancer/operating_modes.go` (3 unit tests pass).
   - `RpcCoordinatorConfig` extended with Embedded, Workers, FailoverPolicy, RequestTimeout, StreamTimeout.

2. **Phase 8.2** ✅
   - `config/config.example.json` содержит пример rpc_coordinator секции со всеми полями + комментарии.
   - `config/config.bundled.json` rpc_coordinator disabled (default) — bundled deployment продолжает работать в standard mode.

3. **Phase 8.3** ✅
   - `CircuitBreaker` реализует Closed/Open/HalfOpen.
   - 8 unit-тестов в `circuit_breaker_test.go` PASS (initial state, N failures, timeout -> half-open, half-open success/failure, concurrent safety, state change callback, custom thresholds).

4. **Phase 8.4** ✅
   - `RpcCoordinatorDispatcher` type создан с `IsRpcPath`, `ShouldRoute`, `InferNonStreaming`.
   - Compile OK (`go build ./...`).

5. **Phase 8.5** ✅
   - `p.rpcDispatcher` field + `GetRpcCoordinatorDispatcher()` getter.
   - `Proxy.ServeHTTP` имеет scaffold-блок для будущей интеграции (без изменения поведения).
   - Все существующие proxy tests PASS (no regression).

6. **Build + tests** ✅
   - `go build -tags llama_stub ./cmd/{cppworker,balancer,agent}/` — exit 0.
   - `go test -tags llama_stub -count=1 ./internal/... ./cmd/... ./pkg/...` — все PASS.
   - `go test -tags llama_stub -count=1 -race ./internal/rpccoordinator/` — все PASS (8 CB + существующие 60+).

---

## Файлы (Session 1)

### Создаются
- `internal/balancer/operating_modes.go` (~30 LOC)
- `internal/balancer/operating_modes_test.go` (~50 LOC)
- `internal/balancer/rpc_coordinator_dispatcher.go` (~100 LOC)
- `internal/rpccoordinator/circuit_breaker.go` (~120 LOC)
- `internal/rpccoordinator/circuit_breaker_test.go` (~200 LOC)
- `docs/phase-8-rpc-coordinator.md` (этот файл)

### Изменяются
- `pkg/types/balancing.go` — `OperatingModeRpcCoordinator` const (если ещё нет)
- `pkg/types/rpc_variants.go` — расширить `RpcCoordinatorConfig` (Embedded, Workers, FailoverPolicy, RequestTimeout, StreamTimeout)
- `internal/balancer/proxy.go` — `rpcDispatcher` field + `GetRpcCoordinatorDispatcher()` + scaffold в `ServeHTTP`
- `config/config.example.json` — секция rpc_coordinator
- `config/config.bundled.json` — `rpcCoordinator: {enabled: false}` (default — disabled)

### Не трогаем (Session 2+)
- `cmd/balancer/main.go` (Step 3 — Session 2)
- Streaming + full ServeHTTP (Step 2 full — Session 2)
- Dispatcher unit tests (Session 2 — needs full ServeHTTP mock)

---

## Оценка

- Phase 8.1 (config): 30 мин
- Phase 8.2 (config.example): 20 мин
- Phase 8.3 (Circuit Breaker + 8 tests): 60-90 мин
- Phase 8.4 (Dispatcher skeleton): 30 мин
- Phase 8.5 (Proxy scaffold): 20 мин
- Verification + commit: 15 мин

**Итого: ~3-3.5 ч**

---

## Sessions 2-3 (deferred)

### Session 2 (~3-4 ч): Step 2 full + Step 3
- `RpcCoordinatorDispatcher.ServeHTTP(w, r)` — full implementation (250 LOC)
  - Parse `/api/generate`, `/api/chat`, `/v1/chat/completions`, `/v1/completions`
  - Read `model` from body, check `ShouldRoute(modelName)`
  - If yes: `coordinator.Infer(ctx, req)` (non-streaming) or `coordinator.InferStream` (streaming SSE passthrough)
  - If no: `p.ServeHTTP(w, r)` (transparent fallback)
  - Error handling: 503 if all workers unhealthy, 502 if worker 5xx, retry on timeout
  - 6 unit tests (NonDistributedModel_Fallback, DistributedModel_Infer, AllWorkersUnhealthy_503, Streaming_Passthrough, CircuitBreaker_Opens, AuthMissing_401)
- `cmd/balancer/main.go` — `embedded` mode wiring
  - Если `cfg.RpcCoordinator.Embedded && OperatingMode == "rpc_coordinator"` — поднять `ModelCoordinator` локально
  - Поднять `HeartbeatLoop` для workers
  - Зарегистрировать `RpcCoordinatorDispatcher` в `Proxy`

### Session 3 (~2-3 ч): polish + e2e + docs
- `tests/rpc_coordinator_e2e_test.go` — end-to-end через httptest (2 workers + coordinator + dispatcher)
- `docs/rpc-coordinator.md` — дополнить секцию "Production mode" с примерами docker-compose + env
- `CHANGELOG.md` — подсекция `### Added (Session P.1 — rpc_coordinator production mode)`
- Coverage check: `go test -coverprofile` для `rpccoordinator` (target >85%)

---

## Связанные документы

- [`plans/2026-q3-production-ready-plan.md`](../plans/2026-q3-production-ready-plan.md) — главный план §P.1
- [`plans/2026-q3-roadmap.md`](../plans/2026-q3-roadmap.md) — секция 3.2
- [`docs/rpc-coordinator.md`](../docs/rpc-coordinator.md) — B1-B8 детали (для расширения в Session 2-3)
- [`docs/phase-7-style-compliance.md`](../docs/phase-7-style-compliance.md) — предыдущая фаза (style cleanup)

---

**Подготовлено:** 2026-07-10 22:40 MSK (Phase 8 — P.1 rpc_coordinator production mode, Session 1 of 3)
**После Session 1:** готов к Session 2 (Step 2 full + Step 3).

---

## Update 2026-07-11: P.1 COMPLETE (Sessions 2 + 3 done)

Session 2 (`9fe639f`) — full non-streaming `ServeHTTP` + main.go wiring.
Session 3 (`143b69b`) — e2e tests + streaming SSE + circuit breaker + auth.

**P.1 (rpc_coordinator production mode) — CLOSED.**

Production-ready: balancer в `OperatingMode=rpc_coordinator` маршрутизирует
все 6 inference paths (Ollama generate/chat, OpenAI chat/completion) через
`ModelCoordinator`, поддерживает streaming SSE passthrough, circuit breaker
per worker, и token-based auth.

См.:
- `docs/rpc-coordinator.md` — user guide (полная документация).
- `CHANGELOG.md` секция "Phase 8: rpc_coordinator production mode (P.1)" — version history.
- 21 e2e теста в `internal/balancer/rpc_coordinator_dispatcher_e2e_test.go`.
