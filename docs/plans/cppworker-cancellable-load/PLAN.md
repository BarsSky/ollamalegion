# R60.57 cppworker Cancellable Load — Detailed Implementation Plan

**Status**: Plan
**Created**: 2026-09-13
**Owner**: TBD (C developer needed)
**Related**: `README.md`, `spec.md`, `../cppworker-abort-api/PLAN.md`

---

## Phase 1: C-bridge changes (bridge.c, bridge.h)

### Task 1.1: Add abort flag to InternalModel

**File**: `c/bridge/bridge.c`

**Change** (around line 55-61, alongside existing `abort_requested`):

```c
typedef struct InternalModel {
    // ... existing fields ...
    atomic_int abort_requested;      // Round 31 #6 — inference abort
    atomic_int load_abort_requested;  // R60.57 — load abort
    int embeddings_mode;
} InternalModel;
```

**Also** in `bridge_load_model` (around line 1066-1069):

```c
// R60.57: init load abort flag (atomic). 0 = not aborted.
atomic_init(&im->load_abort_requested, 0);
```

### Task 1.2: Add checkpoint in bridge_load_model

**File**: `c/bridge/bridge.c`

**Goal**: Identify safe abort points. Recommended: after each tensor
group is loaded (embedding, each transformer layer, output).

**Implementation sketch** (insert at identified checkpoints):

```c
// R60.57: load abort checkpoint
if (atomic_load(&im->load_abort_requested)) {
    // Free all partial state (mmap, vocab, tensors already loaded)
    llama_model_free(model);
    llama_context_free(ctx);  // if ctx was created
    set_error("model load aborted by user (bridge_load_model)");
    return BRIDGE_ERR_ABORTED;  // mirror inference return code
}
```

**Checkpoint locations** (need C-developer review):

1. After `llama_model_load_from_file` returns, before `llama_new_context_with_model`
2. After `llama_new_context_with_model`, before any other setup
3. After KV cache allocation
4. After model registration (if any post-load hooks)

If 4 checkpoints is too coarse, add more between layer loading loops
(need to identify the loop in llama.cpp that processes tensor groups).

### Task 1.3: Add bridge_request_load_abort Go-binding

**File**: `c/bridge/bridge.h`, `c/bridge/bridge.c`

**Header declaration**:

```c
// R60.57: Cancel an in-progress model load.
// SAFETY: thread-safe (atomic_store with relaxed memory ordering).
// Can be called from any goroutine. Checked at load checkpoints in
// bridge_load_model. Idempotent — multiple calls are safe.
int bridge_request_load_abort(ModelHandle model);
```

**Implementation**:

```c
int bridge_request_load_abort(ModelHandle model) {
    if (model == NULL) return -1;
    InternalModel *im = (InternalModel *)model;
    atomic_store(&im->load_abort_requested, 1);
    return 0;
}
```

**Also** add `bridge_request_load_abort_all` for API completeness
(mirror `bridge_request_abort_all` — no-op in C, Go iterates registry).

---

## Phase 2: Go-bridge bindings (c/bridge/bridge.go)

### Task 2.1: Add Go wrapper

**File**: `c/bridge/bridge.go`

```go
// R60.57: Request cancel of in-progress model load.
// Returns nil on success, error if handle is nil or invalid.
func RequestLoadAbort(model *ModelHandle) error {
    if model == nil {
        return fmt.Errorf("bridge.RequestLoadAbort: nil model handle")
    }
    if model.ptr == nil {
        return fmt.Errorf("bridge.RequestLoadAbort: model not loaded (ptr is nil)")
    }
    rc := C.bridge_request_load_abort(model.ptr)
    if rc != 0 {
        return fmt.Errorf("bridge_request_load_abort failed: rc=%d", rc)
    }
    return nil
}
```

### Task 2.2: Add BRIDGE_ERR_ABORTED constant if not exposed

**File**: `c/bridge/bridge.go`

Check if `BRIDGE_ERR_ABORTED` is already exported. If not, add:

```go
const (
    // ... existing constants ...
    ErrAborted = -100  // R60.57: load/inference cancelled by user
)
```

(May need corresponding `c/bridge/bridge.h` definition.)

---

## Phase 3: cppbackend LoadModelWithOpts (internal/cppbackend/backend.go)

### Task 3.1: Accept context in LoadModelWithOpts

**File**: `internal/cppbackend/backend.go`

**Current signature** (approximately):
```go
func (b *Backend) LoadModelWithOpts(name, path string, opts LoadModelOpts) error
```

**New signature**:
```go
func (b *Backend) LoadModelWithOpts(ctx context.Context, name, path string, opts LoadModelOpts) error
```

**Implementation**:
1. At function entry, spawn watcher goroutine:
   ```go
   done := make(chan struct{})
   go func() {
       select {
       case <-ctx.Done():
           // ctx cancelled — request abort on the model
           // (model handle will be created in C-bridge during load)
           // ...
       case <-done:
           // load completed normally — exit watcher
       }
   }()
   defer close(done)
   ```

2. The watcher needs access to the ModelHandle being created. Options:
   - Pass ModelHandle to watcher after creation (race: handle may not exist yet when ctx is cancelled)
   - Have bridge_load_model itself spawn internal watcher thread (more complex)
   - Use atomic flag in C-bridge that the watcher can flip

   **Recommendation**: Bridge-load registers the abort flag before
   load starts, so the watcher can flip it via `RequestLoadAbort`
   on whatever handle exists at watcher-spawn time. If handle is
   nil (very early in load), the watcher retries on a ticker.

   Cleaner: defer the abort until first handle is available, then
   call RequestLoadAbort. Use sync mechanism (channel) to signal
   "handle is ready".

---

## Phase 4: cppworker lazyload.go

### Task 4.1: Pass context from HTTP handler to LoadModelWithOpts

**File**: `cmd/cppworker/lazyload.go`

Currently `LoadModelWithOpts` is called without context. Need to:
1. Plumb r.Context() through `ensureModelLoaded`
2. Or create a new background context with timeout derived from
   per-model load-timeout profile

For HTTP-triggered lazy loads (lazyload.go:97 handler path): use
`r.Context()` so client disconnect triggers abort.

For programmatic loads (handlers_model.go:222, etc.): use context
that respects caller's cancellation.

### Task 4.2: Revert R60.56 in WaitForLoad

Once Go can rely on explicit error from load, WaitForLoad reverts to
plain `<-ch` (R60.56 already reverted in commit 7fec377, just confirm).

---

## Phase 5: Tests

### Task 5.1: C-bridge stub test for load abort

**File**: `c/bridge/bridge_abort_load_test.go` (new)

```go
//go:build llama_stub

func TestRequestLoadAbort_NilHandle(t *testing.T) { ... }
func TestRequestLoadAbort_NilPtr(t *testing.T) { ... }
```

### Task 5.2: cppbackend integration test

**File**: `internal/cppbackend/load_abort_r60_57_test.go` (new)

Scenario:
1. Start LoadModelWithOpts with stub bridge that hangs (infinite loop)
2. Cancel ctx after 100ms
3. Assert load returns within <500ms with ErrAborted

### Task 5.3: cppworker end-to-end test

**File**: `cmd/cppworker/handlers_openai_load_abort_test.go` (new)

Scenario:
1. Pre-populate backend with stub that hangs in load
2. Send HTTP request → ensureModelLoaded called → load hangs
3. Cancel request via /api/cancel
4. Assert: load abort fires within <1s, handler returns 503
5. Verify next request succeeds (model loaded by different path or
   after backend recovery)

---

## Phase 6: Rollout

1. Implement C-side (Phase 1) — requires C developer
2. Implement Go-binding + backend (Phase 2-3) — Go-side
3. Implement cppworker integration (Phase 4) — Go-side
4. Add tests (Phase 5) — Go-side
5. Build cppworker image `gpu-86-r60.57-cancellable-load`
6. Deploy, verify normal loads still work (regression test)
7. Verify abort works (force a stuck load, observe abort)
8. Document in CHANGELOG.md

---

## Estimated effort

- C side (Phase 1): 4-8 hours (depends on checkpoint granularity)
- Go side (Phase 2-4): 2-4 hours
- Tests (Phase 5): 2-4 hours
- Rollout + verification (Phase 6): 2 hours

Total: ~10-18 hours of focused work.

---

## Dependencies / blockers

- **C developer availability**: Phase 1 requires someone familiar with
  CGo and the bridge.c file structure
- **llama.cpp API**: `llama_model_load_from_file` does not expose
  intermediate progress — we may be limited to coarse-grained
  checkpoints (after load returns, before context creation, etc.)

---

## Open questions for C developer

1. What's the right granularity for checkpoints? Per tensor group
   or coarser (per phase: load, context, kv cache)?
2. Is there any partial state in llama.cpp that CAN'T be safely
   freed mid-load? (memory leaks, GPU resources that need cleanup)
3. Does `bridge_load_model` need to be called from a single thread,
   or can the cancel come from another thread? (atomic_load is
   thread-safe but if llama.cpp uses thread-local state, this matters)

If unclear, start with ONE checkpoint (right before context creation)
and add more if abort latency is unacceptable.
