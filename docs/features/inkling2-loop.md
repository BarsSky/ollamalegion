# Inkling-2 loop export: loading the file without an upstream upgrade (R709, 2026-11-25)

The question this document answers: **the `Inkling-2-Mini-UD-Q4_K_M.gguf` file is a
loop-expanded export; can it be loaded by the current build, without waiting for a
newer llama.cpp?**

Short answer: yes, but not by metadata alone. The file is already an export of the
same model: it declares visits instead of physical layers and keeps the cross-pass
queries as two global matrices. Our build (`cppworker` `gpu-r261120-r708`,
llama.cpp `5bacb7c868`) knows the loop scheme as a separate architecture
`inkling2_loop` and expects a different tensor layout. `scripts/gguf_loop_meta_patch.py`
rewrites the metadata and folds the two query matrices into the per-block
tensors the loader wants; the patched file loads and generates coherent text.

## What the file declares

| Fact | Value |
|---|---|
| `general.architecture` | `inkling2` |
| `inkling2.block_count` | 48 |
| Physical blocks in the tensors | 24 (`blk.0`..`blk.23`) |
| `inkling2.num_loops` | 3 |
| `inkling2.loop.core_start` / `core_end` | 6 / 18 |
| `inkling2.loop.gate_rank` | 64 |
| `inkling2.attention.head_count` | array of 48, every value 24 |
| `inkling2.attention.head_count_kv` | array of 48, every value 8 |
| `inkling2.attention.sliding_window_pattern` | array of 48, pattern 5 true + 1 false, repeated 8 times |
| Tensors | 734 |
| Loop controllers `blk.{6..17}.loop_{attn,k,v,mlp}_{down,gain,forget}` | all present (144 tensors) |
| `loop_res_query.weight` / `loop_kv_res_query.weight` | both present, `[2304, 24]` F32 |
| `blk.{6..17}.loop_msg_query.weight` | **absent** |
| `inkling2.loop.start` / `loop.end` / `loop.count` | **absent** |

So the metadata is 48 visits (6 prelude + 12 core x 3 passes + 6 postlude), the
tensors are 24 physical layers, and the loop keys are in the old spelling. The
loader reads `block_count` as the number of physical layers, walks blocks `0..47`
and stops at the first tensor that does not exist:

```text
[llama.cpp] llama_model_load: error loading model: check_tensor_dims:
            tensor 'blk.24.attn_q.weight' not found
```

## What the patch changes

| Item | Before | After |
|---|---|---|
| `general.architecture` | `inkling2` | `inkling2_loop` |
| Key prefix | `inkling2.*` | `inkling2_loop.*` |
| `block_count` | 48 (visits) | 24 (physical layers) |
| Three per-visit arrays | length 48 | length 24 (the first 24 visits are physical layers 0..23) |
| Controller names | `loop_attn_*`, `loop_mlp_*` | `loop_q_ctrl_*`, `loop_ffn_ctrl_*` |
| Loop keys | `num_loops`, `loop.core_start`, `loop.core_end` | plus canonical `loop.start`, `loop.end`, `loop.count`, `loop.varying` |
| Query matrices | 2 tensors, 221184 bytes each | 12 tensors `blk.{6..17}.loop_msg_query.weight`, 36864 bytes each, in the same bytes |

The arrays are truncated only after the script proves that they are per-visit:
visit `j` must repeat the value of the physical layer it maps to. For this file the
visit order is prelude, core, core, core, postlude, so slots 0..23 are exactly
physical layers 0..23.

The two query matrices are replaced in place, not appended: the GGUF reader
requires tensor offsets to run contiguously from zero in the order the tensor
descriptions appear, and the two matrices are the first two descriptions in this
file. Their total size equals 12 tensors of 36864 bytes exactly, so no data moves.

## What the patch does not do

- It does not re-quantize, re-order or copy weights: every original data byte keeps
  its offset. Only the two query regions are rewritten.
- The 12 `loop_msg_query` tensors are a row selection of the two matrices that are
  already in the file, exactly as the converter does it
  (`c/llama.cpp/conversion/inkling2.py`):

```text
msg_query[i] = stack([q[2i], kv[2i], kv[2i+1], q[2i+1]])   # q = loop_res_query, kv = loop_kv_res_query
```

- It does not invent values: if the two matrices are missing, the script refuses
  instead of synthesizing them.

## How to run

```bash
# what the file says, and what would change
python scripts/gguf_loop_meta_patch.py --src <in.gguf> --dry-run --fuse-loop-queries

# write a patched copy (the source is not modified)
python scripts/gguf_loop_meta_patch.py --src <in.gguf> --dst <out.gguf> --fuse-loop-queries

# facts only, and a byte-level data comparison
python scripts/gguf_loop_meta_patch.py --check <file.gguf>
python scripts/gguf_loop_meta_patch.py --verify-data <in.gguf> <out.gguf>
```

Without `--fuse-loop-queries` the script writes metadata only. Such a file gets
past the architecture and the per-visit arrays, but stops at the missing
controller tensor:

```text
[llama.cpp] llama_model_load: error loading model: check_tensor_dims:
            tensor 'blk.6.loop_msg_query.weight' not found
```

That is the boundary of a metadata-only fix: the weights of the message queries
must come from the file, and only the query matrices can supply them.

## How to verify

```bash
python scripts/test_gguf_loop_meta_patch.py
```

The test builds a synthetic mini-GGUF with 3 physical layers and 5 visits and
checks four things: the patched header has the right keys and array lengths; the
fused file has the 12 query tensors with the exact fold; the metadata-only file
still has no query tensors; a per-visit array that does not repeat its layer is
rejected.

On the bench, after the patch:

```text
print_info: arch                  = inkling2_loop
print_info: n_ctx_train           = 1048576
print_info: n_layer               = 48
print_info: n_layer_all           = 48
print_info: n_head                = 24
print_info: n_head_kv             = 8
print_info: n_embd_head_k         = 128
print_info: n_expert              = 128
print_info: n_expert_used         = 8
```

`n_layer_all` is 48 on purpose: the loader expands 24 physical layers to 48 cache
slots, and the KV plan reports 48 slots, of which 8 are global and 40 are windowed
with a window of 1024.

## Live result

| Step | Result |
|---|---|
| Metadata-only file, load | 409 `model_schema_unsupported`, missing tensor `blk.6.loop_msg_query.weight` |
| Fused file, load at ctx 4096 | `state=loaded`, 502186 ms, 48 slots, 0 GPU layers |
| Same file, generation | 16 prompt tokens, 91 tokens, 213.8 s, 0.43 tok/s, coherent answer |
| Fused file, load at ctx 16384 | `state=loaded`, 500953 ms |
| RAM held by the model at ctx 4096 | 15.46 GiB |
| RAM held by the model at ctx 16384 | 17.5 GiB |
| VRAM held by the model | 0 MB (CPU-only load) |

The 409 response is not a dead end for the operator: the worker already classifies
this failure as a schema problem and names the arch `inkling2_loop`.

## Memory: what fits on the RTX 3070

The weights are 15.46 GiB, so a full GPU offload into 7 GB is impossible; the
worker can only shard layers between VRAM and RAM.

| Item | Value |
|---|---|
| File size | 16610544736 bytes, 15.46 GiB |
| memfit verdict at ctx 4096 (7 GB free) | 7 of 49 layers on GPU, 4664 MB required, 5049 MB usable |
| KV slots | 48: 8 global + 40 windowed (window 1024) |
| KV per token, flat f16 | 196608 bytes |
| KV whole cache, ctx 8192, f16 | 416 MiB |
| KV whole cache, ctx 16384, f16 | 672 MiB |
| KV whole cache, ctx 32768, f16 | 1184 MiB |

With the worker plan, VRAM is not the binding constraint for the offloaded share:
the 7 GPU layers include only one global layer, so each extra 4096 tokens of
context adds roughly 17 MB of VRAM, and ctx 32768 stays under the 5049 MB usable.
The binding constraints are RAM for the 41 CPU layers (about 13.9 GiB of weights)
and the read throughput of the bench.

## What is still open

- The R704 hint is wrong for this case: for a file that already declares
  `inkling2_loop` and lacks a core controller, the response says
  `physical_blocks: 6` and suggests re-export as `inkling2_loop_loop` with
  `block_count=6`. The hint needs a branch for "arch is already a loop schema".
- The metadata-only variant cannot work by construction; the fused variant is the
  only path on this build.
- The fold follows the converter, and a short generation is coherent, but a full
  perplexity comparison against the reference implementation was not run.
- The load is disk-bound on this bench: 16610544736 bytes over a Docker Desktop
  bind mount took 502186 ms, about 33 MB/s. On Linux the same file maps directly.
