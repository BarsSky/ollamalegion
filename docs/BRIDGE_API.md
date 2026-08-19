# C-bridge API Reference

Полный reference C API для `c/bridge/`. Используется cppworker через cgo.

> **Source of truth**: см. [c/bridge/bridge.h](../c/bridge/bridge.h) — этот
> документ может отставать. Если нашли несоответствие — bridge.h правильный.

## Оглавление

1. [Lifecycle](#lifecycle)
2. [Model loading](#model-loading)
3. [Inference](#inference)
4. [Streaming inference](#streaming-inference)
5. [Batched decode (multi-slot)](#batched-decode-multi-slot)
6. [Embeddings](#embeddings)
7. [Tokenization](#tokenization)
8. [Chat template](#chat-template)
9. [Abort API](#abort-api) *(Round 31 #6)*
10. [Error handling](#error-handling)
11. [Metadata](#metadata)
12. [GPU info](#gpu-info)

---

## Lifecycle

### `void bridge_init(void)`

Initialize llama.cpp backend. Call once at cppworker startup, before any
other bridge function. Safe to call multiple times (idempotent).

```c
bridge_init();
```

---

## Model loading

### `ModelHandle bridge_load_model(const ModelConfig* config, char** error_msg)`

Load a GGUF model from disk into memory.

**Parameters**:
- `config` — see [ModelConfig](#modelconfig) below
- `error_msg` — output, C-allocated error string (caller frees via `bridge_free_string`)

**Returns**: opaque `ModelHandle` (NULL on error, `*error_msg` set).

```c
ModelConfig cfg = {0};
cfg.model_path = "/app/models/qwen3-q4km.gguf";
cfg.n_ctx = 8192;
cfg.n_batch = 512;
cfg.n_gpu_layers = 999;  // all on GPU

char* err = NULL;
ModelHandle model = bridge_load_model(&cfg, &err);
if (!model) {
    fprintf(stderr, "load failed: %s\n", err);
    bridge_free_string(err);
    return 1;
}
```

### `void bridge_free_model(ModelHandle model)`

Unload model and free all associated resources. After this call, `model`
is invalid. Safe to call with NULL (no-op).

---

## Inference

### `InferenceResult bridge_infer(ModelHandle model, const char* prompt, const GenerationParams* params)`

Synchronous inference. Blocks until generation completes or abort.

**Returns**: `InferenceResult.output` (C string, free via `bridge_free_inference_result`),
`status: 0` = success, `1` = error, **or negative** = special code (see
[Error codes](#error-codes)).

**Special returns**:
- `BRIDGE_ERR_ABORTED` (-100) — abort was requested, generation cancelled
- `BRIDGE_ERR_N_CTX_NEEDS_RELOAD` (2) — n_ctx too small
- `BRIDGE_ERR_PROMPT_TOO_LONG` (3) — prompt+n_predict > n_ctx
- `BRIDGE_ERR_GPU_OOM` (4) — not enough VRAM

---

## Streaming inference

### `int bridge_infer_stream(ModelHandle model, const char* prompt, const GenerationParams* params, StreamCallback callback, void* user_data)`

Streaming inference. Calls `callback(token, token_len, user_data)` for each
generated token.

**Callback return**: `1` = continue, `0` = stop. Note: callback is called
*before* the abort check in some code paths — see [Abort semantics](#abort-semantics).

**Returns**: Same codes as `bridge_infer`. The stream may emit partial output
before `BRIDGE_ERR_ABORTED` is returned.

### `StreamCallback` typedef

```c
typedef int (*StreamCallback)(const char* token, int token_len, void* user_data);
```

---

## Batched decode (multi-slot)

(Used by `BatchedScheduler` for parallel slot generation.)

### `int bridge_batched_decode(...)`

Multi-slot batched inference. See [Round 13 changelog](../CHANGELOG.md)
for seq_id semantics. Each slot can be independently aborted via
`bridge_request_abort` (cancel applies to all slots of the model — v1).

---

## Embeddings

### `InferenceResult bridge_get_embeddings(ModelHandle model, const char* text)`

Generate embeddings vector for input text. Output is a JSON-like string
representation; caller parses to float array as needed.

---

## Tokenization

### `int32_t bridge_count_tokens(ModelHandle model, const char* text)`

Returns number of tokens for text, or -1 on error.

### `int32_t bridge_tokenize(ModelHandle model, const char* text, int32_t* out_buf, int32_t out_buf_size)`

Tokenize text into caller-allocated int32 buffer. Returns count written
(>= 0), or -1/-2 on error. If return == out_buf_size, retry with larger buffer.

### `int32_t bridge_token_to_piece(ModelHandle model, int32_t token, char* out_buf, int32_t buf_size)`

Convert single token to UTF-8 bytes. Returns bytes written, or -1/-2 on error.

---

## Chat template

### `int32_t bridge_apply_chat_template(...)`

Apply `tokenizer.chat_template` from GGUF metadata. Returns bytes written
to `out_buf`, -1 on error, **-2 if template not present in GGUF**.

(Детали параметров см. в [bridge.h](../c/bridge/bridge.h))

---

## Abort API *(Round 31 #6)*

> Added 2026-08-09. See [plans/cppworker-abort-api/PLAN.md](../plans/cppworker-abort-api/PLAN.md)
> для полного design rationale.

### `int bridge_request_abort(ModelHandle model)`

Cooperative cancel for the current/next inference on this model.

**Semantics**:
- Sets `atomic_int abort_requested = 1` in `InternalModel`.
- Checked between `llama_decode` batches in `bridge_infer` /
  `bridge_infer_stream` / `bridge_batched_decode`.
- **Model is NOT unloaded** — stays in memory for next call.
- Thread-safe (`atomic_store` with `memory_order_relaxed`).
- Idempotent (multiple calls have same effect as one).

**Returns**: `0` on success, `-1` if `model == NULL`.

**Cancel latency**: <100ms (single batch time). For long prompts, may take
up to one full batch decode (~1-2s on CUDA, longer on CPU).

### `void bridge_request_abort_all(void)`

**DECISION Q1 (2026-08-09)**: No-op in C. cppworker shutdown iterates its
Go-side registry (`Backend.models`) and calls `bridge_request_abort` per model.
C-function exists for API completeness only.

### `bool bridge_is_aborted(ModelHandle model)`

Diagnostic. Returns `true` if abort was requested (and not yet consumed by
next infer's flag reset), `false` otherwise. Returns `false` for NULL.

### Abort semantics

1. `bridge_request_abort` is called (atomic store).
2. Next `llama_decode` in current/next infer checks flag.
3. If set: free current batch, sampler, output buffer; return `BRIDGE_ERR_ABORTED`.
4. The flag is reset to 0 at the **start** of each new `bridge_infer[_stream]`
   call. So after abort, calling infer again works without manual cleanup.

### Wire protocol (cancelled responses)

When `BRIDGE_ERR_ABORTED` propagates to Go:

- **Ollama API**: `done_reason: "cancelled"` + `cancelled: true`
- **OpenAI API**: `finish_reason: "stop"` + `cancelled: true` (backward compat)

This is **per-request**: if multiple in-flight requests on the same model
share a cancel signal, all of them see `BRIDGE_ERR_ABORTED` (v1 limitation).
Per-slot abort is deferred (PLAN.md Q5).

---

## Error handling

### Error codes

| Code | Name | Description |
|------|------|-------------|
| 0 | `BRIDGE_OK` | Success |
| 1 | `BRIDGE_ERR_GENERIC` | Unstructured error (see `bridge_last_error()`) |
| 2 | `BRIDGE_ERR_N_CTX_NEEDS_RELOAD` | n_ctx_override > current n_ctx, consider reload |
| 3 | `BRIDGE_ERR_PROMPT_TOO_LONG` | prompt + n_predict > n_ctx, hard error |
| 4 | `BRIDGE_ERR_GPU_OOM` | Not enough VRAM |
| 5 | `BRIDGE_ERR_BAD_REQUEST` | Invalid params (e.g. n_ctx <= 0) |
| **-100** | **`BRIDGE_ERR_ABORTED`** | **Cooperative cancel (R31 #6)** |

### `BridgeErrorInfo` struct

```c
typedef struct {
    int  code;                  // one of BRIDGE_ERR_*
    int  current_n_ctx;
    int  required_n_ctx;
    int  actual_tokens;
    int  n_predict;
    int  n_ctx_override;
    int  max_vram_n_ctx;
    char message[768];
} BridgeErrorInfo;
```

### `const BridgeErrorInfo* bridge_get_last_error_info(void)`

Get structured last error info. Pointer is to static thread-local storage;
**do not free**. Valid until next `bridge_infer` / `bridge_infer_stream` /
`bridge_load_model` call.

### `const char* bridge_last_error(void)`

Get last error as human-readable C string. Legacy API, prefer
`bridge_get_last_error_info()` for new code.

---

## Metadata

### `ModelMetadata bridge_get_model_metadata(ModelHandle model)`

Get GGUF metadata: architecture, layer counts, vocab size, etc.

```c
typedef struct {
    char* description;      // GGUF metadata
    char* architecture;     // model arch name
    int context_length;
    int n_layers;
    int n_heads;
    int n_head_kv;         // GQA kv heads
    int n_embd_head_k;
    int n_embd_head_v;
    int n_embd;
    int n_vocab;
    uint64_t size_total;   // file size in bytes
} ModelMetadata;
```

Call `bridge_free_model_metadata` after use.

---

## GPU info

### `int bridge_get_gpu_count(void)`

Number of available CUDA devices. Returns 0 if no GPU / CUDA not compiled.

### `int bridge_get_gpu_info(int gpu_index, GPUDeviceInfo* info)`

Get info about GPU at index. Fills `GPUDeviceInfo` (vram_total/free, name,
compute capability). Returns 0 on success, -1 on error.

---

## `ModelConfig` (load params)

> See [c/bridge/bridge.h](../c/bridge/bridge.h) для full struct. Ключевые поля:

| Field | Description | Default |
|-------|-------------|---------|
| `model_path` | Path to GGUF file | required |
| `n_ctx` | Context size | 4096 |
| `n_batch` | Batch size | 512 |
| `n_ubatch` | Micro-batch size | n_batch |
| `n_threads` | CPU threads | #cores |
| `n_gpu_layers` | Layers on GPU (-1 = all) | 0 (CPU) |
| `n_parallel` | Multi-slot KV-cache | 1 (single-slot) |
| `kv_cache_type` | 0=f16, 8=q8_0, 2=q4_0 | 0 (F16) |
| `split_mode` | 0=NONE, 1=LAYER, 3=TENSOR | -1 (default LAYER) |
| `override_tensor_patterns` | Per-tensor GPU/CPU override | NULL |
| `vocab_only` | Load only vocab (no weights) | 0 |
| `use_mmap` | Memory-map model file | 1 |
| `use_mlock` | Lock in RAM (no swap) | 0 |
| `flash_attn_type` | -1=auto, 0=off, 1=on | -1 |
| `tensor_split` | Multi-GPU proportions | NULL (auto) |
| `main_gpu` | Main GPU index | 0 |

---

## `GenerationParams` (infer params)

| Field | Description | Default |
|-------|-------------|---------|
| `n_predict` | Max tokens (-1 = no limit) | -1 |
| `n_keep` | Tokens to keep from initial prompt | 0 |
| `temperature` | Sampling temperature | 0.8 |
| `top_p` | Nucleus sampling | 0.95 |
| `top_k` | Top-K sampling | 40 |
| `repeat_penalty` | Repetition penalty | 1.1 |
| `frequency_penalty` | OpenAI frequency penalty | 0.0 |
| `presence_penalty` | OpenAI presence penalty | 0.0 |
| `seed` | RNG seed (-1 = random) | -1 |
| `antiprompts` | NULL-terminated C-string array | NULL |
| `n_ctx_override` | Per-request n_ctx (0 = inherit) | 0 |
| `seq_id` | Slot ID (0 = legacy single-slot) | 0 |
| `token_timings` | Per-token latency timing | 0 |

---

## Thread safety

- `bridge_init` — idempotent, safe to call multiple times.
- `bridge_request_abort` / `bridge_is_aborted` — thread-safe (atomic ops).
- `bridge_load_model` — **NOT thread-safe**. Call sequentially at startup.
- `bridge_infer` / `bridge_infer_stream` — **NOT thread-safe per model**.
  Same model can be used by one infer at a time. Use `bridge_batched_decode`
  with `seq_id` for parallel slot generation.
- `bridge_get_last_error_info` — thread-local, safe to call concurrently.

---

## Changelog

- **v0.5.16 (2026-08-09)** — Abort API (R31 #6): `bridge_request_abort`,
  `bridge_request_abort_all`, `bridge_is_aborted`, `BRIDGE_ERR_ABORTED=-100`.
- **Round 25-31** — batched_decode, kv_cache_type, override-tensors,
  chat template enhancements.
- **Round 13** — multi-slot via `seq_id` and `bridge_batched_decode`.

См. [CHANGELOG.md](../CHANGELOG.md) для полной истории.
