# Fix: Bug 13 - nKvHeads=0 in /api/models for gemma-4 (C-bridge ModelMetadata)

## Symptom
Even with Bug 12 fix (ggufModelArchs += gemma4), /api/models shows nKvHeads: 0 for gemma-4.
Reason: GGUF v3 gemma-4 has no <gemma4>.attention.head_count_kv key in metadata. nKvHeads is computed by llama.cpp from tensor shapes, not stored as GGUF KV pair.

## Fix
Extend C-bridge ModelMetadata to expose NKvHeads/HeadDimK/HeadDimV from llama.cpp public API.

### Files changed (4)
1. c/bridge/bridge.h:160-169 - Add n_head_kv/n_embd_head_k/n_embd_head_v fields to typedef struct ModelMetadata
2. c/bridge/bridge.c:1195-1199 - bridge_get_model_metadata fills them from llama_model_n_head_kv() + n_embd/n_heads (head_dim computed inline)
3. c/bridge/bridge.go:250-264 - Add NKvHeads/HeadDimK/HeadDimV to Go struct + populate in GetMetadata()
4. internal/cppbackend/backend.go:536 - inst.info.NKvHeads = meta.NKvHeads (from C-bridge)

### Critical: n_embd_head_k/v are NOT in public llama.h
Functions llama_model_n_embd_head_k() and llama_model_n_embd_head_v() are NOT declared in c/llama.cpp/include/llama.h. Only llama_model_n_head/n_head_kv/n_layer/n_embd/n_ctx_train are public. So head_dim must be computed inline: head_dim = n_embd / n_heads.

### Critical: avoid UTF-8 BOM
When editing Go source with PowerShell Set-Content, BOM (EF BB BF) is added. Go compiler rejects: "invalid BOM in the middle of the file". Use [IO.File]::WriteAllBytes() with byte array, or [System.Text.UTF8Encoding]::new($false) to write without BOM.

### Critical: docker build context
For docker build, use "." (current dir) NOT "..". Using ".." makes Docker include 5GB of parent dir and may fail on locked files like python_check/temp_build_env.

## Verified live (2026-07-06, post-rebuild)
Tested with qwen3.6-35B (no GGUF v3 metadata issues for n_head_kv):

qwen3.6-35B loaded, /api/models:
- arch: qwen35moe
- nLayers: 40
- nHeads: 16
- nKvHeads: 2    <-- was 0 before fix, now 2 from C-bridge
- nEmbd: 2048
- ggufContextLength: 262144
- contextSize: 8192
- gpuLayers: 8
- kvCacheType: q4_0
- max_vram_n_ctx: 157260
- max_ram_n_ctx: 262144
- available_vram_mb: 8191

Chat with num_ctx=8192: HTTP 200, response in 3s, reasoning_content="\nHere".

## Side findings
- balancer /api/v1/backends shows gpu: {memoryTotal:0,...} zeros because cppworker top-level fields are 0 (BUG, separate from 13)
- balancer /api/v1/gguf/backends (via agent sidecar) shows correct gpuMemory: {totalMB:8192, usedMB:5600, freeMB:2592}
- ModelInfo struct (Go) has NKvHeads JSON tag but missing HeadDimK/HeadDimV JSON tags - need patch in /api/models handler to expose

## Build
Image: ollama-legion/cppworker:gpu-86 id=3ea8c5172549 (2026-07-06 14:00 UTC, ~4 min build)