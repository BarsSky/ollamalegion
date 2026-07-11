# Phase 8 P.3 — Real ggml / NCCL Tensor Parallelism (Research Spike)

> **Status:** research-spike complete (2026-07-11).
> **Scope:** survey llama.cpp TP API + assess integration into existing
> `internal/rptensor/` stub infrastructure. **NO code changes in this
> spike** — pure research + planning. Implementation is post-1.0
> per `plans/2026-q3-production-ready-plan.md` §P.3.

## TL;DR

* llama.cpp получил **first-class tensor parallelism** через "meta
  backend" (PR #19378, ~April 2026) под флагом `--split-mode tensor`.
* Default mode **`--split-mode layer`** (pipeline parallel) уже
  production-ready, работает поверх `tensor_split` config.
* Текущий наш `StubTPRuntime` (internal/rptensor/tp_runtime.go) — чистый
  stub, не вызывает реальный llama.cpp.
* C bridge (c/bridge/bridge.h) уже имеет `tensor_split` config, но
  не пробрасывает `split_mode` в `llama_context_params`.
* Реальная интеграция требует: 1) C bridge functions для
  multi-GPU split, 2) wiring в `RealTPRuntime` (Go side), 3) build
  matrix с `-DGGML_CUDA_NCCL=ON`, 4) CI multi-GPU runner.
* **Рекомендация:** для 1.0 release остаёмся на `StubTPRuntime`.
  Layer-mode TP (pipeline parallel) — в P.4 (post-1.0 enhancement).
  True tensor split — в P.5 (research track, requires CUDA/NCCL team).

---

## 1. llama.cpp TP API Survey

### 1.1 Split modes (3 режима)

| Mode | Description | Status | Recommendation |
|---|---|---|---|
| `none` | Single GPU only. Use `--main-gpu` to pick. | Stable | Use for small models. |
| `layer` (default) | **Pipeline parallelism.** Each GPU holds a contiguous slice of layers. KV cache for layer *l* lives on the GPU that owns that layer. | Stable, production-ready | **Use for our 1.0 release** — compatible, well-tested, works for MoE. |
| `row` | Older row-split TP path. Splits only dense weights. | Deprecated | Avoid. |
| `tensor` | **New experimental TP** (PR #19378, ~April 2026). Splits weights AND KV across GPUs via "meta device" abstraction. Requires Flash Attention, KV cache quantization off. | Experimental, only stable for 2 equal-VRAM GPUs | Post-1.0 research. |

### 1.2 Build flags

```bash
# Default (NCCL включён по умолчанию для CUDA):
cmake -DGGML_CUDA=ON ...

# Manual enable NCCL (default in recent versions):
cmake -DGGML_CUDA=ON -DGGML_CUDA_NCCL=ON ...

# AMD alternative:
cmake -DGGML_HIP=ON -DGGML_HIP_RCCL=ON ...   # RCCL disabled by default
```

**NCCL install requirement:** NCCL is NOT auto-distributed with CUDA.
Must install manually (or check CMake log for `NCCL found`).

### 1.3 Runtime API surface

**Command-line:**
```bash
llama-server -m model.gguf \
  --split-mode tensor \
  --tensor-split 0.5,0.5 \
  -ctk f16 -ctv f16 \  # cache types for K/V (fp16 required for TP)
  --flash-attn        # required for tensor mode
  --no-kv-cache-quant  # required for tensor mode
  -c 4096             # explicit context (no -fit for TP)
```

**C API (llama.h, b4500+):**
- `llama_model_load_from_file()` — load model, internally detects multi-GPU
- `llama_new_context_with_model()` — `n_gpu_layers` controls layer split
- `tensor_split` parameter in `llama_context_params` (array of `float[n_gpu]`)
- `llama_model_params.split_mode` (NEW, with PR #19378) — `LLAMA_SPLIT_MODE_LAYER | LLAMA_SPLIT_MODE_ROW | LLAMA_SPLIT_MODE_TENSOR | LLAMA_SPLIT_MODE_NONE`

### 1.4 Constraints / known issues

* **`--split-mode tensor` only works for dense models** (no MoE). Our
  Qwen3-A3B is MoE — tensor mode would NOT work.
* Flash Attention required for `tensor` mode.
* KV cache quantization must be off (Q8_0 not allowed).
* Multi-GPU requires p2p or NVLink for good performance. PCIe-only
  setups can be slower than `layer` mode.
* 2-GPU equal-VRAM is the well-tested target. 4+ GPUs is experimental.
* Upstream API may change — PR #19378 was recently merged.

---

## 2. Current State in Repo

### 2.1 Go side: `internal/rptensor/`

```
internal/rptensor/
├── tp_runtime.go        — TPRuntime interface + StubTPRuntime
├── allreduce.go         — AllReduceConcat / SumBytes / XorBytes (stub math)
├── sharded_model.go     — ShardedModel (in-memory struct, no real model)
├── megatron.go          — Megatron-LM partitioning helpers (concept-level)
└── coordinator.go       — TensorParallelCoordinator (orchestration)
```

**StubTPRuntime contract** (tp_runtime.go:22-43):

```go
type TPRuntime interface {
    InferShard(ctx context.Context, model *ShardedModel, rank int, input []byte) ([]byte, error)
    AllReduce(partials map[int][]byte, worldSize int) ([]byte, error)
    Name() string
    Close() error
}
```

**Stub implementation** (tp_runtime.go:54-128):
- `InferShard`: returns rank-marked bytes (`out[i] = byte(rank+1)`).
- `AllReduce`: concatenates partials in rank order.
- No real math, no real NCCL calls.

### 2.2 C bridge: `c/bridge/`

**`GpuSplitConfig` (bridge.h:73-79):**
```c
typedef struct {
    int num_gpus;
    float* tensor_split;    // array[num_gpus] of proportions
    int main_gpu;
    int n_gpu_layers;       // -1 = all
} GpuSplitConfig;
```

**`ModelConfig` (bridge.h:82-...):**
- Has `tensor_split` and `tensor_split_len` (for multi-GPU proportions).
- Has `n_gpu_layers` (controls layer offload).
- **MISSING:** `split_mode` parameter. Bridge assumes `--split-mode layer`
  by default. No way to pass `LLAMA_SPLIT_MODE_TENSOR`.

**`CMakeLists.txt`:** basic CUDA detection but no explicit `-DGGML_CUDA_NCCL=ON` flag.

### 2.3 cppworker usage

* `cmd/cppworker/handlers.go` calls `bridge.LoadModel(config)` with
  `config.GpuSplitConfig`.
* No TP-specific code in worker — purely layer-split (pipeline).
* Workers run on individual GPUs; TP coordination would happen at
  worker level (multi-GPU per worker) OR coordinator level (multi-worker
  pipeline via `internal/rpccoordinator/` — already implemented in
  rpc_coordinator mode, P.1).

### 2.4 What we DO have working

* **Pipeline parallelism (P.1, rpc_coordinator mode)** — coordinator
  orchestrating multiple workers, each with its own slice of layers.
  This is equivalent to llama.cpp `--split-mode layer` at cluster
  level. **Production-ready, all tests pass.**
* **Megatron-style partitioning helpers** (`megatron.go`) — describe
  how to split Q/K/V/MLP/lm_head tensors. Conceptual, no real math.

---

## 3. Integration Plan (for post-1.0 implementation)

### 3.1 Minimum viable: `--split-mode layer` (pipeline) at worker level

**Goal:** worker on multi-GPU box uses `tensor_split` for layer
distribution. No NCCL needed (layer mode uses CUDA streams only).

**Changes:**
1. `c/bridge/bridge.c`: in `LoadModel`, pass `tensor_split` array to
   `llama_context_params`. Verify the existing `tensor_split` field
   works (currently may be silently ignored).
2. `internal/rptensor/`: new `LayerTPRuntime` that calls
   `bridge.LoadModel(tensor_split=[0.5, 0.5])` for 2-GPU host.
   Returns success/failure to coordinator.
3. `cmd/cppworker/main.go`: parse `LB_TENSOR_SPLIT=0.5,0.5` env var,
   pass to bridge.
4. Tests: unit + e2e with stub GPU (skip real GPU tests in CI).

**Effort:** 2-3 days (no CUDA/NCCL expertise required, mostly bridge
plumbing).

### 3.2 Medium: `--split-mode tensor` (true TP) at worker level

**Goal:** worker on 2-GPU box uses tensor parallelism for dense models.

**Changes (on top of 3.1):**
1. C bridge: add `split_mode` parameter to `ModelConfig` + `LoadModel`.
2. Verify llama.cpp PR #19378 API in current upstream (may need patch
   if API changed).
3. Build matrix: `-DGGML_CUDA_NCCL=ON` flag in CI.
4. Test on real 2-GPU hardware (no CI stub).
5. Document Flash Attention + no-KV-quant requirements.

**Effort:** 5-7 days (CUDA/NCCL expertise required, real-hardware testing).

### 3.3 Full: replace StubTPRuntime with RealTPRuntime in rptensor

**Goal:** `internal/rptensor.RealTPRuntime` implements the
`TPRuntime` interface backed by llama.cpp.

**Changes (on top of 3.2):**
1. `internal/rptensor/tp_runtime.go`: add `RealTPRuntime` constructor.
2. CGo: call `llama_model_load + llama_new_context + tensor_split +
   split_mode` from Go.
3. Wire into `internal/rpccoordinator/`: replace `StubTPRuntime` with
   `RealTPRuntime` when `cfg.TPRuntimeMode = "real"`.
4. CLI: `--tp-runtime=stub|real` flag.
5. Performance benchmarks vs single-GPU baseline.

**Effort:** 10-15 days (CUDA/NCCL expertise, requires multi-GPU test
rig, performance tuning).

### 3.4 What we DON'T need to build

* **Cross-host NCCL** (cluster-level TP across multiple machines) —
  extremely complex, low ROI. Our rpc_coordinator already provides
  pipeline parallelism across hosts.
* **CPU-only TP** — ggml has Metal/Vulkan but our bridge is
  CUDA-focused.
* **Dynamic resharding** (model moves between ranks at runtime) —
  out of scope, too complex.

---

## 4. Risks & Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Upstream llama.cpp API changes (PR #19378 is recent) | High | Pin to specific commit/tag; track upstream in our fork |
| NCCL not available on CI runners | High | Skip real-GPU tests in CI; only test stub |
| Flash Attention / KV cache quantization constraints | Medium | Document requirements; add validation in worker startup |
| MoE models don't work with `--split-mode tensor` | Medium | Use `layer` mode for MoE (Qwen3-A3B); document |
| Performance regression vs single-GPU | Medium | Benchmark before/after; only enable TP via opt-in |
| 4+ GPU support immature | Low | Document as 2-GPU only; warn for higher counts |
| Build time increase (CUDA + NCCL) | Low | Cache build artifacts in CI; pre-built images |

---

## 5. Recommended Path Forward

**For 1.0 release (current focus):**
- Stay on `StubTPRuntime`. P.1 rpc_coordinator provides cluster-level
  pipeline parallelism (already production-ready).
- Document `tensor_split` env var support in worker (3.1) as a
  small enhancement that doesn't break the stub.

**Post-1.0 backlog:**
- **P.4: Layer-mode TP at worker level** (3.1, 2-3 days, low risk) —
  multi-GPU boxes get better memory distribution for single-model
  inference.
- **P.5: Tensor-mode TP** (3.2, 5-7 days, requires CUDA/NCCL team) —
  dense models on 2-GPU get sub-linear latency scaling.
- **P.6: RealTPRuntime in rptensor** (3.3, 10-15 days, research-grade)
  — replaces stub, but only valuable if we have CUDA/NCCL team.

**Don't pursue:**
- Cross-host NCCL (too complex, low ROI).
- CPU-only TP (CUDA-focused bridge).

---

## 6. Decision Criteria for Going Beyond Stub

Before starting 3.1/3.2/3.3, ask:
1. **Hardware:** Do we have 2+ GPU boxes available for development?
   (User mentioned RTX 3070 + A10 24GB. Both single-GPU. Would need
   multi-GPU dev box or cloud instance.)
2. **Use case:** What's the actual benefit? Pipeline parallelism
   (P.1, working) handles the "model > 1 GPU" case via cluster.
   Tensor parallelism is "latency optimization for 1 huge model on
   1 box".
3. **Models:** Do we have dense models in our use case? Qwen3-A3B
   is MoE (incompatible with tensor mode). gemma-3-4B is dense but
   fits on 1 GPU.
4. **Expertise:** Is there CUDA/NCCL expertise on the team?

If all answers are "yes, yes, yes, yes" — proceed with 3.1 then 3.2.
Otherwise: defer to research track.

---

## 7. References

* llama.cpp multi-GPU docs: https://github.com/ggml-org/llama.cpp/blob/master/docs/multi-gpu.md
* PR #19378 (meta-backend TP): https://github.com/ggml-org/llama.cpp/pull/19378
* ik_llama.cpp TP discussion: https://github.com/ikawrakow/ik_llama.cpp/discussions/1247
* Megatron-LM paper (concept reference): https://arxiv.org/abs/1909.08053
* NCCL docs: https://docs.nvidia.com/deeplearning/nccl/

## 8. See Also

* `internal/rptensor/tp_runtime.go` — current TPRuntime interface + StubTPRuntime.
* `internal/rptensor/megatron.go` — Megatron-LM partitioning helpers.
* `c/bridge/bridge.h` — current C bridge API surface.
* `docs/phase-8-rpc-coordinator.md` — P.1 sibling (already done).
* `docs/virtual-router.md` — P.2 sibling (already done).
* `plans/2026-q3-production-ready-plan.md` §P.3 — original plan section.
