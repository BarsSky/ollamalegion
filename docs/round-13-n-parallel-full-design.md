# Round 13 — n_parallel > 1 FULL: Design Proposal

**Status**: PROPOSAL (awaiting user sign-off)
**Date**: 2026-07-28
**Builds on**: v0.4.10 (Round 12 — defaultNParallel foundation)

## Goal

Make `defaultNParallel > 1` actually deliver concurrent inference throughput
(within the limits of llama.cpp's "one decode at a time per context" model).

## Current state (post-Round 12)

- C-bridge allocates `n_seq_max` slots in KV-cache (config-driven, via `cfg.NParallel`)
- `bridge_infer` and `bridge_infer_stream` ALWAYS use seq_id 0 (via `llama_batch_get_one`)
- `reset_inference_state()` clears ALL KV-cache on every call
- Go side holds `inst.mu` through entire `inst.handle.Infer()` (Round 8 fix)
- **Result**: n_parallel > 1 only consumes more VRAM; no actual concurrency

## Constraints (from llama.cpp research)

1. **`llama_decode(ctx, batch)` is NOT thread-safe per-context.** Concurrent calls
   from different goroutines on the same context race → SIGABRT (Round 8 root cause).

2. **Multiple sequences can coexist within ONE context** via different `llama_seq_id`
   values, but they share the single forward-pass pipeline (see
   `c/llama.cpp/examples/parallel/parallel.cpp`).

3. **True batched inference** (multiple sequences in one `llama_decode` call) is
   what gives 2-4× throughput, but requires:
   - Building mixed-seq batches (see `common_batch_add` in `parallel.cpp`)
   - Interleaving tokens from multiple in-flight sequences
   - Per-seq sampling state
   - **This is a major refactor (1-2 weeks). NOT Round 13 scope.**

## Round 13 SCOPE (minimal viable multi-slot)

**Pragmatic target**: "n_parallel > 1 with state isolation, serialized forward pass".

Two users with n_parallel=2 can have independent KV-caches; llama_decode is
still serialized via `inst.mu`, but the model state doesn't need to be reset
between their requests. This eliminates the "reset between every call" cost
and enables **state-preserving concurrent sessions**.

### Changes

**1. C-bridge (`c/bridge/bridge.h`, `c/bridge/bridge.c`):**

   a. Add `int seq_id` to `GenerationParams` (default 0 = single-slot legacy).
   b. New helper `build_batch_with_seq(tokens, n_tokens, seq_id, logits_last_only)`:
      - Uses `llama_batch_init(n_tokens, 0, 1)` for explicit seq_id control
      - Sets `n_seq_id[i] = 1`, `seq_id[i][0] = seq_id` per token
      - Sets `logits[i] = 1` only for the last token (for sampling)
   c. Replace `llama_batch_get_one(...)` calls in `bridge_infer` and `bridge_infer_stream`
      with `build_batch_with_seq(...)`.
   d. **Conditional reset**: only call `llama_memory_clear(mem, true)` when `seq_id == 0`
      (legacy single-slot). For `seq_id > 0`, use `llama_memory_seq_rm(mem, seq_id, -1, -1)`
      to clear only that slot's KV-cache.
   e. **Conditional sampler reset**: only call `llama_sampler_reset` when `seq_id == 0`.
      For `seq_id > 0`, sampler state is per-call anyway (no carry-over).
   f. **Backward compatibility**: `seq_id=0` behaves EXACTLY as today (no regression).

**2. Go bridge stub (`c/bridge/bridge_stub.go`):**

   - Update `BridgeInferStream` / `BridgeInfer` signatures to accept `seq_id int`.
   - Stub ignores seq_id (test infra doesn't simulate KV-cache state).

**3. Go slot manager (new file `internal/cppbackend/slot_manager.go`):**

   ```go
   type SlotManager struct {
       mu          sync.Mutex
       cond        *sync.Cond
       maxSlots    int           // from ModelInfo.Parallel
       inUse       []bool        // [false, false, ...]
       waiters     []chan int    // FIFO queue of waiters
   }
   
   func (sm *SlotManager) Acquire(ctx context.Context) (slot int, release func(), err error)
   func (sm *SlotManager) Release(slot int)
   func (sm *SlotManager) ActiveCount() int
   ```

   - `Acquire`: if free slot → return immediately. If all busy → wait on FIFO queue
     (or fail with `ErrAllSlotsBusy` if `ctx.Done()`).
   - `Release`: mark slot free, notify next waiter.
   - `ActiveCount`: for tests / metrics.

**4. Go inference wiring (`internal/cppbackend/backend.go`):**

   - `LoadModel` / `LoadModelWithOpts`: initialize SlotManager with `Parallel` slots.
   - `Generate` / `GenerateStream`:
     1. `slot, release, err := inst.slots.Acquire(ctx)` — blocks if all busy
     2. `inst.mu.Lock()` — still serialize the actual `llama_decode` call
        (Round 8 invariant — llama.cpp context is not thread-safe)
     3. `bridge_infer(handle, prompt, &params{seq_id: slot, ...})`
     4. `inst.mu.Unlock()`
     5. `release()` — mark slot free
   - `UnloadModel`: wait for all slots to be released before tearing down.

**5. RWMutex migration for `inst.mu` (per-Phase 19 plan):**

   - Keep `inst.mu` as `sync.Mutex` for now — it's held during `llama_decode`,
     so RWMutex would not help (no read-only access to the context).
   - **DEFER** RWMutex migration. The per-slot `SlotManager` already gives
     us the "wait for slot" semantics that RWMutex was meant to provide.
   - Document in CHANGELOG that RWMutex is no longer needed.

### What Round 13 does NOT do

- **True batched parallel inference** (multiple sequences in ONE `llama_decode`).
  Deferred to Round 14+ (~1-2 weeks). Round 13 is "serialized forward pass with
  state isolation" — simpler, correct, ships value.
- **Per-slot sampler state**. Sampler is reset at start of every call (cheap).
- **Auto-failover when one slot is slower than another**. Not relevant for
  serialized forward pass.

### Backward compatibility

- `defaultNParallel=0` (default) → SlotManager with 1 slot, seq_id always 0,
  behavior IDENTICAL to today. Round 8 fix preserved (inst.mu held during decode).
- `defaultNParallel=1` → SlotManager with 1 slot, equivalent to default.
- `defaultNParallel=2..8` → 2-8 slots, queue if all busy.

### Success criteria

1. **All existing 14 smoke tests still pass** (no regression).
2. **New test**: 2 concurrent `Generate` to gemma-4 with `defaultNParallel=2`:
   - Both succeed (no SIGABRT).
   - Slot count is `≤ 2` at any time (verified via test snapshot).
   - Outputs are different (proves they're independent sessions, not duplicates).
3. **New test**: 4 concurrent calls with `defaultNParallel=2`:
   - 2 succeed immediately, 2 wait in queue.
   - All complete without error.
   - No deadlock if a slot is held for 30+ seconds (timeout via context).
4. **`SlotManager` unit tests**:
   - `Acquire` returns immediately when slot free.
   - `Acquire` blocks when all busy, `Release` wakes one waiter.
   - FIFO ordering of waiters.
   - `ctx.Done()` unblocks with `ErrAllSlotsBusy`.
5. **C-bridge tests** (via stub):
   - `bridge_infer` with `seq_id=0` and `seq_id=1` both work.
   - `build_batch_with_seq` produces correct batch structure.

### Effort estimate

- C-bridge changes: 1-2 hours (straightforward refactor)
- Go slot manager: 2-3 hours (with tests)
- Wiring in `backend.go`: 1-2 hours
- CHANGELOG + commits: 30 min
- **Total**: 1 working day (5-8 hours)
- Build: ~30 min (CUDA), 5-10 min (Go + linker)
- Verification: 30 min

### Risk

- **C-bridge bug → silent state corruption or KV-cache overflow.** Mitigation:
  test with `n_parallel=2` + concurrent gemma-4 calls, compare output to
  single-slot baseline.
- **Deadlock if SlotManager wait + decode lock ordering is wrong.** Mitigation:
  always `Acquire` BEFORE `inst.mu.Lock`, always `release` AFTER `inst.mu.Unlock`.
  Add deadlock-detection test (5-min timeout, all slots held, expect graceful
  queue timeout).
- **Per-slot memory not released if Release is missed.** Mitigation: defer
  release in `Generate` so panics don't leak slots.

## What comes after Round 13 (deferred)

- **Round 14+**: True batched parallel inference (multiple sequences in ONE
  `llama_decode` call). Requires:
  - Per-call mixed-seq batch construction
  - Interleaved sampling state
  - Probably 1-2 weeks
  - **NOT Round 13 scope**
- **Native C-bridge `enable_thinking`** via `common::chat::common_chat_templates_apply`
  (also deferred, ~1-2 days).
- **Crash recovery** with preserved session state (~2-3 days).

## Open questions for user

1. **Scope OK?** Pragmatic target (state isolation, serialized forward pass) or
   stretch goal (true batched parallel, 1-2 weeks)?
2. **Slot acquisition timeout** — what should the default be? 30s? 60s?
   (Balancer would 504 after 30s, so 25s is safe default.)
3. **Error semantics for "all slots busy"**:
   - 503 Service Unavailable (HTTP), or
   - 408 Request Timeout (with `Retry-After` header)?
4. **Should `defaultNParallel=2` be the new default** (vs current `0`)? Pros:
   users see immediate benefit on existing models. Cons: 2x KV-cache memory.
