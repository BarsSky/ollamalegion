# Measuring the gain of the RPC layout on qwen3.8 (R231, 2026-10-24)

The question this document answers: **does laying out layers onto a second node
(RPC, port 50052) give a real gain on a large model — or is it only "more fits",
while speed comes out worse?**

The measurement is live, on the test bench: same prompt, same model, same
load parameters; only the layout mode differs (`off` / `on`).

## Test bench and request

| Parameter | Value |
|---|---|
| Master | 8 GB VRAM (local card), cppworker + balancer |
| Donor | second node, 8 GB VRAM, `rpc-server` on `:50052` |
| Master → donor link | measured by `scripts/verify-...` / `POST /api/v1/backends/{id}/measure-link` — **≈8.9 Gbit/s** |
| Model | `Qwen3.8-27B-UD-Q4_K_M` (15.33 GiB) |
| Load | `contextSize=16384`, `kvCacheType=q8_0`, layers — auto |
| Request | `/api/chat`, `num_predict=96`, `temperature=0`, prompt 58 tokens |
| Build | llama.cpp `0eadefebd` on master and donor (the "one commit" rule) |

Switching the mode — operator toggle:
`PUT /api/v1/backends/{backendID}/rpc-layout` with `{"mode":"off"|"on"|"auto"}`.
`auto` — default behavior: the layout turns on if the donor is available and the
commit/architecture match; when the node is lost, the master falls back to local loading.

## Results

| Metric | RPC `off` | RPC `on` | Difference |
|---|---|---|---|
| Layout | CUDA0 **20/66**, CPU 46 | CUDA0 **26** + RPC0 **22** = **48/66**, CPU 18 | +28 layers on GPU |
| Model load | 278.5 s | 285.4 s | +2.5 % |
| 96 tokens (3 runs) | 146.7 / 125.5 s | 49.2 / 44.7 / 45.0 s | — |
| Generation speed | 0.65–0.77 tok/s (avg. 0.71) | 1.95–2.15 tok/s (avg. 2.08) | **≈ ×2.9 (+193 %)** |
| Average response latency | 136.1 s | **46.3 s** | **−66 %** |
| Free master VRAM after load | 623 MB | 774 MB (the donor took ~5.6 GB of weights + ~0.6 GB of buffers) | — |

**Conclusion:** on this configuration the RPC layout gives **≈2.9× in generation speed**
with practically no extra load cost. The reason is visible in the layout: without the
donor, 46 of 66 layers were computed on CPU, with the donor — only 18; 22 layers went
to the donor's RPC0, and 6 more layers fit on the local card (26 vs 20).

Why the gain is exactly this:

- an RPC layer is computed by the **donor's GPU**, not the master's CPU; activation
  exchange goes over the fast link (~8.9 Gbit/s), so network overhead does not eat the gain;
- weights are transferred to the donor once at load time (≈5.6 GB), so load time
  barely changed (+7 s);
- with a **slow** link (hundreds of Mbit/s) the picture may reverse: transferring
  weights and activations becomes the bottleneck. Measure the link
  (`POST .../measure-link`) and compare on your model/context.

## How to reproduce

```powershell
# 1) the donor must be online; check the link
curl -X POST http://<balancer>:18081/api/v1/backends/<backend>/measure-link

# 2) both modes, 3 measurements per mode
pwsh -File scripts/bench-rpc-qwen38.ps1 -Mode both -Runs 3 -NumPredict 96 -LoadTimeoutSec 1300
```

The script does everything: sets mode `off`, loads the model, does a warm-up and
measurements; then sets `on`, repeats; in the end returns `auto` and unloads the
model. The run artifact is JSON in `_diag/` (outside git). Admin access variables —
`LB_ADMIN_USER` / `LB_ADMIN_PASSWORD` in the environment.

**Mandatory for large models:** `CPPWORKER_LOAD_STALL_SEC=1200` in
`deployments/.env`. With the default 300 s, loading a 15-GB model from a slow disk
is aborted by the watchdog (`model load cancelled`), and the measurement "does not start".

## Limitations and what to check next

- The numbers were obtained for `ctx=16384`. On a larger context (for example, 32768
  from the profile) the RPC gain is usually **larger**: without the donor, KV and
  layers go to RAM, with the donor they fit on the GPU. This is a separate measurement.
- The donor is a mandatory condition: when it is turned off, the guard returns local
  loading (see `docs/architecture/rpc-layout-rules.ru.md`), not an error.
- The speed difference depends on the link and on how many layers can actually be
  moved (donor VRAM, `n_ctx`, KV type). The script prints the layout and tok/s,
  so on other hardware the comparison stays just as reproducible.

See also: [rpc-layout-rules.md](../architecture/rpc-layout-rules.ru.md) (layout rules),
[rpc-coordinator.md](../architecture/rpc-coordinator.md) (RPC internals),
[rpc-donor-vram-lifecycle.md](../operations/rpc-donor-vram-lifecycle.ru.md) (donor VRAM lifecycle).
