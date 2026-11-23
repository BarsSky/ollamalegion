# Inkling-2 loop export: loading the file without an upstream upgrade (R709, 2026-11-25; verified on the bench)

The question this document answers: **the `Inkling-2-Mini-UD-Q4_K_M.gguf` file is a
loop-expanded export; can it be loaded by the current build, without waiting for a
newer llama.cpp?**

Short answer: yes, but not by metadata alone. The file is already an export of the
same model: it declares visits instead of physical layers and keeps the cross-pass
queries as two global matrices. Our build (`cppworker` `gpu-r261120-r710`,
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

The dry run costs one header read: 4.4 s in a container on this bench. The full pass
reads 16610543328 bytes and writes 16610544736 bytes, so the destination needs
15.46 GiB free; the script measures that before it starts and refuses with the exact
"need / free" numbers instead of filling the disk halfway.

The slow side is the source, not the output. Reading the file through a Windows
bind mount gave 19 MB/s, so the pass took 883 s (14 min 43 s); a docker volume
writes at 342 MB/s and reads the same file back at 414 MB/s. Write the output
straight into a volume and load the model from that volume: a bind mount reads the
same file at about 30 MB/s, and with that rate the load did not reach `loaded`
before the 300 s stall watchdog (R227) fired.

```bash
docker volume create inkling-test
docker run --rm \
  -v "$PWD/scripts:/scripts:ro" \
  -v "/d/ollama-legion-models:/in:ro" \
  -v inkling-test:/out \
  python:3.12-slim python /scripts/gguf_loop_meta_patch.py \
    --src /in/Inkling-2-Mini-UD-Q4_K_M.gguf \
    --dst /out/inkling2-loop.gguf --fuse-loop-queries
```

The run ends with the verified header facts, and nothing else has to be re-checked
by hand:

```text
готово: {"arch": "inkling2_loop", "block_count": 24, "tensors": 744,
         "data_start": 11979872, "visits": 48, "loop": [6, 18, 3], "msg_query": 12}
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

On the bench, after the patch (`docker logs` of the one-shot worker):

```text
print_info: file format = GGUF V3 (latest)
print_info: file type   = Q4_K - Medium
print_info: file size   = 15.46 GiB (5.09 BPW)
print_info: arch                  = inkling2_loop
print_info: n_ctx_train           = 1048576
print_info: n_embd_inp            = 2304
print_info: n_embd                = 2304
print_info: n_layer               = 48
print_info: n_layer_all           = 48
print_info: n_head                = 24
print_info: n_head_kv             = 8
print_info: n_swa                 = 1024
print_info: is_swa_any            = 1
print_info: n_embd_head_k         = 128
print_info: n_embd_head_v         = 128
print_info: n_ff                  = 1152
print_info: n_expert              = 128
print_info: n_expert_used         = 8
print_info: model params          = 26.07 B
print_info: n_vocab               = 200058
```

`n_layer_all` is 48 on purpose: the loader expands 24 physical layers to 48 cache
slots, and the KV plan reports 48 slots, of which 8 are global and 40 are windowed
with a window of 1024. The worker publishes the same plan in `/api/models`:
`kv_layers: 48`, `kv_global_layers: 8`, `kv_swa_layers: 40`, `kv_swa_window: 1024`,
`kv_plan_source: "plan"`, `kv_plan_exact: true`.

## Live result

A one-shot worker (`cppworker` `gpu-r261120-r710`, `--gpus all`, the volume mounted
at `/app/models`, `--memory=20g`, RTX 3070 8 GiB, 6 visible vCPU, Docker Desktop VM
with 24 GiB RAM):

```bash
docker run -d --name inkling-diag --gpus all -p 18097:18092 \
  -v inkling-test:/app/models \
  -e CPPWORKER_MODELS_DIR=/app/models -e API_TOKEN=<token> \
  -e CPPWORKER_REGISTER_DISABLE=true \
  -e CPPWORKER_LOAD_STALL_SEC=3600 -e CPPWORKER_LOAD_WAIT_MAX_SEC=3600 \
  -e CPPWORKER_LOAD_STALE_SEC=7200 \
  --memory=20g ollama-legion/cppworker:gpu-r261120-r710
curl -s -X POST http://127.0.0.1:18097/api/models/load \
  -H "Authorization: Bearer <token>" -H "Content-Type: application/json" \
  -d '{"name":"inkling2-loop","contextSize":8192,"kvCacheType":"q8_0","gpuLayers":6}'
```

The load answers 202 with a `progressUrl`; `GET /api/models` flips the model to
`state=loaded` when the read is done.

| Step | Result |
|---|---|
| Metadata-only file, load | 409 `model_schema_unsupported`, missing tensor `blk.6.loop_msg_query.weight` |
| Fused file, patch on this bench | 883 s, 19 MB/s source read, 744 tensors, data from byte 11979872 |
| Fused file, load at ctx 8192 | `state=loaded`, 50341 ms, 6 of 49 layers on GPU, KV 680 MiB (612 CPU + 68 GPU) |
| Same model, generation at ctx 8192 | 5 prompt tokens, 24 tokens, 119.6 s, 0.2 tokens/s, answer `Paris.` |
| Fused file, load at ctx 32768 | `state=loaded`, 32119 ms, 7 of 49 layers on GPU, KV 2720 MiB (2380 CPU + 340 GPU) |
| Same model, generation at ctx 32768 | 6 prompt tokens, 24 tokens, 109.8 s, 0.22 tokens/s |
| VRAM held at 6-7 GPU layers | 7806 MiB used of 8192 MiB, 211 MiB free; 1238 MiB after unload |
| Mapped CPU weights | 15359.88 MiB, model buffer on the card 469.33 MiB |
| Peak cgroup memory at ctx 8192 | 16.02 GB |
| Peak cgroup memory at ctx 32768 | 19.66 GB |

The 409 response is not a dead end for the operator: the worker already classifies
this failure as a schema problem and names the arch `inkling2_loop`. The first row
is from the R709 session; every other row was measured in this run.

## Memory: what fits on the RTX 3070

The weights are 15.46 GiB, so a full GPU offload into 7 GB is impossible; the
worker can only shard layers between VRAM and RAM.

| Item | Value |
|---|---|
| File size | 16610544736 bytes, 15.46 GiB |
| Weights mapped in RAM (CPU share, 43 of 49 layers) | 15359.88 MiB |
| Weights on the card (6-7 layers) | 469.33 MiB |
| memfit verdict at ctx 32768 | 7 layers, 4803 MB required, 5007 MB usable, 11666 MB RAM needed against 2050 MB free |
| KV whole cache, ctx 8192, q8_0 | 680 MiB |
| KV whole cache, ctx 32768, q8_0 | 2720 MiB |
| GPU compute buffer printed at ctx 8192 / 32768 | 8034.12 MiB / 10536.76 MiB |
| Measured VRAM delta for 6-7 layers | about 6.6 GiB |

KV grows with the context, not with the layer count of the GPU share: from ctx 8192
to ctx 32768 it goes 680 MiB -> 2720 MiB, while the offloaded share stays at 6-7 of
49 layers. The printed compute buffer (8034.12 MiB at ctx 8192) is the worst-case
reserve for the whole graph and is larger than the card; what the process actually
holds is the measured 6.6 GiB, and with a desktop already using 1238 MiB the card
ends up full at 6 layers.

RAM is the real ceiling: 15359.88 MiB of mapped weights plus 680-2720 MiB of KV
means about 16.5 GiB at ctx 8192 and about 18.6 GiB at ctx 32768. The worker ran
under a 20 GiB cgroup and peaked at 16.02 GB and 19.66 GB respectively, so ctx 8192
has room and ctx 32768 is at the edge of that limit.

## What is still open

- The R704 hint is wrong for this case: for a file that already declares
  `inkling2_loop` and lacks a core controller, the response says
  `physical_blocks: 6` and suggests re-export as `inkling2_loop_loop` with
  `block_count=6`. The hint needs a branch for "arch is already a loop schema".
- The metadata-only variant cannot work by construction; the fused variant is the
  only path on this build.
- The fold follows the converter, and a short generation is coherent, but a full
  perplexity comparison against the reference implementation was not run.
- The throughput is not usable interactively on this bench: 0.2 tokens/s at both
  ctx values, because 43 of 49 layers of a 26.07 B model stay on 6 vCPU. The GPU
  share (6-7 layers) does not change the measured rate. A 24 GB card for a full
  offload, or a smaller quant, is the way to a workable rate.
- The patched file lives in the docker volume `inkling-test` (`/out/inkling2-loop.gguf`
  inside it). The D11 UI scenario can be run against it without repeating the
  15-minute patch; `docker volume rm inkling-test` removes it.
