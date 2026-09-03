# Hardware Presets (R58.2, 2026-09-03)

> Ready-made configuration presets for typical GPUs. Closes the user pain
> point: "not convenient for an ordinary user when trying to configure for
> a different (more powerful) GPU."

## Available presets

| Preset | VRAM | sm_arch | Where | When to use |
|--------|------|---------|-------|-------------|
| `rtx30-8gb.json` | 8 GB | 86 | RTX 3050/3060/3070/3080 8GB | Small models (3B-7B), aggressive RAM fallback |
| `rtx40-24gb.json` | 16-24 GB | 89 | RTX 4080 16GB, RTX 4090 24GB | Medium models (7B-13B), 70B partial offload |
| `a10-24gb.json` | 24 GB | 86 | NVIDIA A10/A10G (AWS G5, Lambda, RunPod) | Cloud deploy, sm_86 = no rebuild |
| `rtx50-32gb.json` | 32 GB | 120 | RTX 5090 | Largest models (70B Q4, 405B partial), rebuild required for sm_120 |

## How to apply a preset

### Option 1 — manual copy-paste (5 minutes)
Open `config/hardware-presets/<preset>.json`, copy the values into your
`deployments/.env.bundled-with-agent` (or your own `.env`):

```bash
# Example for RTX 30xx 8GB:
cat config/hardware-presets/rtx30-8gb.json | \
    jq -r '.docker, .cppworker, .ram_fallback, .auto, .balancer | to_entries | .[] | "\(.key)=\(.value)"' \
    >> deployments/.env.bundled-with-agent

# Then rebuild the image if CUDA_ARCH changed:
cd deployments && docker compose -f docker-compose.cppworker-bundled-with-agent.yml \
    --env-file .env.bundled-with-agent build cppworker
docker compose -f docker-compose.cppworker-bundled-with-agent.yml \
    --env-file .env.bundled-with-agent up -d
```

### Option 2 — via `scripts/apply-hardware-preset.py` (1 minute)

```bash
python scripts/apply-hardware-preset.py rtx30-8gb
# Generated deployments/.env.bundled-with-agent from rtx30-8gb.json
#   CUDA_ARCH=86
#   CPPWORKER_GPU_TAG=86-abort-r35
#   CPPWORKER_CTX_SIZE=32768
#   ... 15 lines
#
# Next: rebuild cppworker if CUDA_ARCH changed:
#   cd deployments && docker compose -f docker-compose.cppworker-bundled-with-agent.yml \
#       --env-file .env.bundled-with-agent build cppworker
```

### Option 3 — via WebUI (Phase 1.3b, not yet implemented)
Setup Wizard → "Apply Hardware Preset" → dropdown → select RTX 4090 →
preset values loaded into the form → Save.

## What a preset changes

Each preset includes:

1. **Docker build args**:
   - `CUDA_ARCH` — for image rebuild
   - `CPPWORKER_GPU_TAG` — which image to use

2. **CPPWORKER runtime params**:
   - `CPPWORKER_CTX_SIZE` — max n_ctx
   - `CPPWORKER_GPU_LAYERS` — how many layers on GPU (99 = all)
   - `CPPWORKER_KV_CACHE_TYPE` — f16 (fast) vs q4_0 (saves VRAM)
   - `CPPWORKER_BATCH_SIZE` — batch size

3. **RAM fallback cascade** (3-tier):
   - `CPPWORKER_RAM_FALLBACK_N_CTX` — enable the cascade
   - `CPPWORKER_RAM_FALLBACK_GPU_LAYERS` — how many layers to keep on GPU
   - `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` — max n_ctx even under fallback

4. **Balancer tuning**:
   - `LB_NCTX_RELOAD_MAX_N_CTX` — max n_ctx for auto-reload
   - `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR` — how much VRAM to keep free

## When NOT to use a preset

Presets are a **starting point**, not a final configuration. Each piece of
hardware and use case is unique. After applying a preset:

1. Load your models through the WebUI (`/gguf` page → `Add` → choose GGUF)
2. AutoTune (R54.x) will automatically tune `n_ctx`, `kv_cache_type`,
   `num_gpu_layers` per model
3. If a model doesn't fit, the preset's `RAM_FALLBACK_*` will activate

## FAQ

**Q: I have an RTX 3060 12GB. Which preset?**
A: 12GB is between 8GB and 16GB. Start with `rtx30-8gb.json`, then manually
   increase `CPPWORKER_GPU_LAYERS` if RAM isn't being used.

**Q: A100 40GB / 80GB?**
A: These presets don't cover A100. Use `rtx50-32gb.json` as a starting
   point, change `CUDA_ARCH=80` (A100), and increase `CPPWORKER_CTX_SIZE` to 131072.

**Q: H100 80GB?**
A: Same path as A100. `CUDA_ARCH=90` (Hopper). Preset values for KV cache
   and batch should be doubled vs the 32GB tier.

**Q: I want to add my own preset. How?**
A: Copy `rtx40-24gb.json`, rename it, update the values. The
   `apply-hardware-preset.py` script will pick it up automatically
   (it reads from `config/hardware-presets/*.json`).

## Source of values

All values come from real deployments:
- RTX 3070 8GB (BarsSky, 2026-08) — production baseline
- RTX 4090 24GB (tests, 2026-08) — for batched inference models
- A10 24GB (Round 35c notes, 2026-08-13) — cloud deploy
- RTX 5090 32GB (specs + analogy with RTX 4090) — not tested in production

If a preset works poorly on your hardware — open an issue with logs
from `cppworker-load.log` + `balancer.log`, and we'll update the preset
based on the data.

---

## R58.3: Setup Wizard dropdown (WebUI)

R58.3 added a hardware preset dropdown in Setup Wizard → Step 4
(General Settings). When you select a preset (RTX 4090 / A10 / RTX 5090),
it auto-fills 4 fields (vramMaxUsage / gpuMaxUsage / cpuMaxUsage / ramMaxUsage)
and shows a hint with CPPWORKER_* env vars for manual application to
cppworker.
