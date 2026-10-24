# OllamaLegion - Adaptive LLM Inference Cluster

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE.md)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg)](https://www.docker.com/)
[![CI test](https://github.com/BarsSky/ollamalegion/actions/workflows/test.yml/badge.svg?branch=centurion)](https://github.com/BarsSky/ollamalegion/actions/workflows/test.yml)

> **English** (current) | [Russian](README.ru.md)

OllamaLegion turns one or more machines with consumer GPUs into a single self-tuning
inference cluster behind one endpoint. It load-balances `llama.cpp` backends (text) and
`stable-diffusion.cpp` backends (images), picks safe model parameters on its own (context,
KV-cache type, GPU layers), shards layers to a donor node over `llama.cpp` RPC, and ships a
WebUI with dashboards, monitoring, roles and a demo mode.

Clients keep the APIs they already use: **Ollama**, **OpenAI**, and A1111 (`/sdapi/v1/*`).

## Contents

- [Why OllamaLegion](#why-ollamalegion)
- [Quick start](#quick-start)
- [Architecture](#architecture)
- [Ports](#ports)
- [Features](#features)
- [Performance](#performance)
- [Build](#build)
- [Deployment profiles](#deployment-profiles)
- [Configuration](#configuration)
- [Project layout](#project-layout)
- [Documentation](#documentation)
- [Security](#security)
- [Commits and authorship](#commits-and-authorship)
- [License](#license)
- [Latest changes](#latest-changes)

## Why OllamaLegion

- **One endpoint instead of N backends.** Clients (Cline, Open WebUI, curl, SDKs) talk to a
  single host; the balancer picks the backend by resources, not by round-robin. Session
  affinity and model affinity are built in.
- **No manual parameter tuning.** AutoTune reads real VRAM/RAM, the model architecture from
  `/api/gguf` and `/api/show`, and sets `n_ctx`, KV-cache type and `num_gpu_layers` to what
  actually fits - then verifies the applied plan against the fact.
- **More model on the same hardware.** Layer sharding sends part of the layers to a donor
  node over `llama.cpp` RPC: measured **20/66 -> 48/66 layers on GPU** and **~2.9x generation
  speed** on a 27B Q4 model with two 8 GB cards ([measurement](docs/features/rpc-benchmark-qwen38.ru.md)).
  If the donor disappears, the guard falls back to local loading instead of failing.
- **Text and images behind the same balancer.** An `image_cpp` worker (`sd-server`)
  registers next to texters; routing is by backend type and by explicit request markers
  (`/v1/images/*`, `/sdapi/v1/*`, model prefix `sd:`).
- **Degrades instead of dying.** VRAM/RAM fallbacks, KV-cache type ladder
  (`f16 -> q8_0 -> q4_0`), partial offload, reload->load fallback, n_ctx clamp and a
  watchdog that explains why a load was cancelled.
- **You can see and show it.** WebUI with GPU/CPU/RAM dashboards, queue, sessions, modeling
  ops, auto-tune history, login with roles (admin / viewer) and CSRF, plus a **demo mode**
  that renders a full synthetic cluster scenario for guests with zero stand access.
- **No cloud, no account, no vendor.** Everything runs on your own machines; MIT license.
  CPU/stub mode (`-tags llama_stub`) lets you build, test and demo without a GPU.

## Quick start

Requirements: Docker 24+ with Compose v2, Git, NVIDIA driver + NVIDIA Container Toolkit
(GPU mode). CPU-only builds are possible, see [Build](#build).

```bash
# 1. Clone. --recurse-submodules is REQUIRED: c/llama.cpp is a pinned submodule.
git clone --recurse-submodules https://github.com/BarsSky/ollamalegion.git
cd ollamalegion
# already cloned without it? git submodule update --init --recursive

# 2. Prepare env files (once)
cp deployments/.env.example deployments/.env
cp deployments/.env.bundled-with-agent.example deployments/.env.bundled-with-agent
```

```bash
# 3. Set the API token - in ONE place only
#    deployments/.env -> CPPWORKER_API_TOKEN
#    compose derives LB_API_TOKEN / API_TOKEN / BALANCER_TOKEN from it.

# 4. Build and run everything on one machine
export DOCKER_BUILDKIT=1
export CUDA_ARCH=86          # 61 = GTX 10xx, 75 = RTX 20xx, 86 = RTX 30xx, 89 = RTX 40xx, 90 = RTX 50xx
docker compose -f deployments/docker-compose.stack.yml --profile full up -d
```

PowerShell equivalent: `$env:DOCKER_BUILDKIT=1`, `$env:CUDA_ARCH=86`, then the same
`docker compose ... up -d` line. Do **not** pass `--env-file`: compose already reads
`deployments/.env`, and the flag replaces it (you lose `CUDA_ARCH` and the token).

After start:

| URL | What |
|---|---|
| http://localhost:18080 | Ollama-compatible API (clients point here) |
| http://localhost:18079 | OpenAI-compatible API + image APIs |
| http://localhost:18081 | Management API (admin only - do not expose) |
| http://localhost:18083 | WebUI |

Health check:

```bash
curl -s localhost:18081/api/v1/backends | jq '.backends[] | {id, host, type, status}'
```

New hardware or migrating? Start with a hardware preset (RTX 30xx/40xx/50xx, A10):
`python scripts/apply-hardware-preset.py <name>` - see
[RU](docs/getting-started/hardware-presets.md) / [EN](docs/getting-started/hardware-presets.md).

## Architecture

```
Client (Cline / Open WebUI / SDK / curl)
  -> Balancer :18080 (Ollama) / :18079 (OpenAI, images) / :18081 (management)
       |-- routing: resource-aware scoring, session + model affinity
       |-- n_ctx preflight: 4 phases, auto-reload with VRAM safety factor
       |-- layer sharding: RPC coordinator -> donor node (llama.cpp RPC :50052)
       |-- CppWorker :18092  llama.cpp inference (adaptive loader, KV fallback, reload/load)
       |     + embedded metrics agent :18034
       |-- ImageWorker :18093  sd.cpp (image_cpp) + embedded metrics agent :18033
       +-- WebSocket :18081/ws  live events (queue, backends, autotune)
  -> WebUI :18083 dashboards, monitoring, queue, sessions, settings, demo mode
```

Agent metrics for plain **Ollama** backends come from a separate container on `:18032`
(profile `legacy-agent`) - see [Ports](#ports).

## Ports

| Port | Component | Purpose |
|---|---|---|
| 18080 | Balancer | Ollama API proxy (+ `/v1/*` surface) |
| 18079 | Balancer | OpenAI surface (`/v1/chat/completions`, `/v1/images/generations`, `/sdapi/v1/*`) |
| 18081 | Balancer | Management API + WebSocket (keep private) |
| 18092 | CppWorker | llama.cpp inference |
| 18093 | ImageWorker | sdworker API (`/api/image/*`, `/api/hf/*`) |
| 18034 | CppWorker | **Embedded** metrics agent (`CPPWORKER_AGENT_EMBEDDED=on`) |
| 18033 | ImageWorker | **Embedded** metrics agent (`IMAGE_WORKER_AGENT_EMBEDDED=on`) |
| 18032 | Agent (external) | Metrics for **Ollama** backends only |
| 18083 | WebUI | Dashboard |

Embedded agent ports are different on purpose: two processes must not share a port, and
the balancer polls `agentPort` of the specific backend record. Never run the external agent
and an embedded one on the same backend - metrics flicker, including `gpu.uuids`, which the
balancer uses to detect two backends sharing one physical GPU.

## Features

### AutoTune - parameters set from facts

Detects sub-optimal state of loaded models and applies a better plan via async reload
(manual control kept per model). Budget = free VRAM + the model's own occupancy (reload
unloads it), units are bytes/MB consistently, architecture (`n_kv_heads`, `head_dim`) comes
from `/api/show`, KV type is chosen for the real `n_ctx`, and a plan is only reported as
`success` if the configuration actually changed. Global and per-model toggles, live
WebSocket updates, event history and a WebUI timeline. Manual apply:

```bash
curl -s -X POST localhost:18081/api/v1/admin/autotune/<backendID>/apply
```

Docs: [API (RU)](docs/api/api.md#autotune-api-r541--r552-2026-08-24), live check
`pwsh -File scripts/verify-autotune-r230.ps1`.

### RPC layer sharding (donor node)

Layers are split between the master GPU, a donor node and CPU. Both sides must run the same
`llama.cpp` commit and the same architecture; the feature has a kill switch and its own
capacity accounting. Rules learned from real crashes:
[RU](docs/architecture/rpc-layout-rules.ru.md) / lifecycle: [RU](docs/operations/rpc-donor-vram-lifecycle.ru.md).
Updating a donor node and rolling back: [RU](docs/operations/donor-update.ru.md).

### Image generation on the same balancer

`imageworker` (stable-diffusion.cpp / `sd-server`, Vulkan build - works on old cards without
CUDA) registers as an `image_cpp` backend. Clients use OpenAI images, A1111 and the sdworker
API. Models: GGUF/safetensors from Hugging Face, per-model profiles.
Docs: [RU](docs/features/image-generation.md) / [EN](docs/features/image-generation.md).

### WebUI

Dashboards (GPU/CPU/RAM, queue, sessions, model ops), topology and RPC layout panel, live
logs, settings, user management with roles (`admin` / `viewer`) and CSRF, plus a **demo
mode** (`WEBUI_DEMO_MODE=1`, env only) that shows a deterministic synthetic scenario -
clients -> balancer -> queue -> RPC donor -> AutoTune - to guests with no login and no
requests to the real cluster.
Docs: [auth and roles (RU)](docs/features/webui-auth.ru.md), [demo mode (RU)](docs/features/webui-demo.ru.md).

### Client compatibility

Ollama, OpenAI (chat/completions, tools, streaming) and A1111 surfaces are served by the
same balancer. Streaming follows one contract: reasoning is not duplicated into `content`,
`</think>`-style markers never reach the client, and `[DONE]` arrives exactly once, last.
Checks: `node scripts/verify-stream-r231.js`, `node scripts/clients/contract-suite.js`,
agent loop: [RU](docs/features/cline-agent-loop.ru.md).

## Performance

RPC layer sharding, one request (58 prompt tokens, `num_predict=96`, `temperature=0`),
model `Qwen3.8-27B-UD-Q4_K_M` (15.33 GiB), `ctx=16384`, KV `q8_0`. Master: 8 GB VRAM.
Donor: second node, 8 GB VRAM, RPC `:50052`, link ~8.9 Gbit/s.

| | RPC off | RPC on | Delta |
|---|---|---|---|
| layers on GPU | 20/66 (46 in RAM) | **26 CUDA + 22 RPC = 48/66**, CPU 18 | +28 layers |
| model load | 278 s | 285 s | +2.5 % |
| 96 tokens | 126-147 s | **45-49 s** | - |
| throughput | **0.65-0.77 tok/s** | **1.95-2.15 tok/s** | **~2.9x** |
| mean latency | 136 s | **46 s** | **-66 %** |

Method and reproduction: [RU](docs/features/rpc-benchmark-qwen38.ru.md),
`pwsh -File scripts/bench-rpc-qwen38.ps1 -Mode both`.

> Large models: loading a 15 GB model from a slow disk takes longer than the default
> watchdog - set `CPPWORKER_LOAD_STALL_SEC=1200` in `deployments/.env`, otherwise the load
> is cancelled with "model load cancelled" at 300 s.

## Build

All commands run from the repository root. `DOCKER_BUILDKIT=1` matters: Go-only changes
then rebuild in ~30 s thanks to cached CUDA layers.

```bash
export DOCKER_BUILDKIT=1
export CUDA_ARCH=86   # 61 = Pascal (GTX 10xx), 75, 86, 89, 90
docker compose -f deployments/docker-compose.stack.yml --profile full build            # all
docker compose -f deployments/docker-compose.stack.yml --profile full build cppworker-gpu
docker compose -f deployments/docker-compose.stack.yml --profile full build imageworker
docker compose -f deployments/docker-compose.stack.yml --profile full build loadbalancer
docker compose -f deployments/docker-compose.stack.yml --profile full build webui
```

Every service has `build:`, so `up -d` builds a missing image itself - a local registry is
not required. Helper script (fills the tag and registry prefix):
`pwsh -File scripts/build-containers.ps1 -CppWorker -CudaArch 86 -Tag <tag>`;
`imageworker` is built through compose (`scripts/build-containers.ps1` has no flag for it).

Notes:

- **CUDA 13 dropped Pascal** (GTX 10xx). For 1070/1080 stay on CUDA 12.x and build with
  `CUDA_ARCH=61`; image generation uses Vulkan and is independent from CUDA arch.
- **Custom image registry** - one variable `IMAGE_REGISTRY` in `deployments/.env`
  (address **with a trailing slash**). Build scripts read the same variable, so the built
  image gets exactly the name compose looks for.
- CPU/stub build without a GPU: `-tags llama_stub` (used by unit tests and CI).

## Deployment profiles

One compose file, `deployments/docker-compose.stack.yml`, four scenarios
([details](docs/operations/deployment-stack.ru.md)):

| Scenario | Command | What starts |
|---|---|---|
| Everything on one machine | `--profile full` | cppworker + imageworker + balancer + webui |
| Backends only, balancer elsewhere | `--profile worker` | cppworker + imageworker |
| Management only, backends register themselves | `--profile balancer` | balancer + webui |
| Rollback: external metrics collector | `--profile worker --profile legacy-agent` | worker set + `agent` container |

Remote workers need two variables in `deployments/.env.bundled-with-agent`:
`BALANCER_URL=http://<balancer-host>:18081` (management API, not 18080) and
`BACKEND_HOST=<address visible from the balancer>`.

```bash
# text worker only, on its own machine
docker compose -f deployments/docker-compose.stack.yml --profile worker up -d cppworker-gpu
# image worker only, pointing to the same balancer
SDWORKER_BALANCER_URL=http://<balancer-host>:18081 \
  docker compose -f deployments/docker-compose.stack.yml --profile worker up -d imageworker
```

Both workers are independent services: own port, own model directory, own engine. Any of
them works without the other, and both can live on different machines behind one balancer.

> **Agent env names.** The agent reads `BACKEND_TYPE`, `CPPWORKER_URL`, `NODE_LABELS`,
> `GPU_MODE`, `COLLECT_INTERVAL`, `HEARTBEAT_INTERVAL`. Names like `AGENT_BACKEND_TYPE`
> are **not** read: the backend would register as Ollama. The canonical compose file uses
> the right names.
>
> **Capacity.** `AGENT_MAX_CONCURRENT_REQUESTS` must match the worker's real `n_parallel`
> (default 1). A too-high value produces phantom free slots and client hangs (Cline).

## Configuration

Defaults come from `deployments/.env.bundled-with-agent.example` and
`config/cppworker-defaults.json` (`defaultCtxSize=32768`).

```bash
CPPWORKER_CTX_SIZE=32768            # default context
CPPWORKER_GPU_LAYERS=20             # layers on GPU (-1 = all, -2 = auto)
CPPWORKER_AUTO_OFFLOAD=true         # auto gpuLayers
CPPWORKER_AUTO_TUNE_NCTX=true       # auto n_ctx
CPPWORKER_AUTO_KV_CACHE=true        # auto kvCacheType (q4_0/q8_0/f16)
CPPWORKER_RAM_FALLBACK_N_CTX=true   # RAM fallback when VRAM is short
CPPWORKER_LOAD_STALL_SEC=1200       # load watchdog for 15 GB models
```

Balancer config (`config/config.example.json`), n_ctx auto-reload:

```json
{
  "balancing": {
    "firstByteTimeout": 30,
    "nctxReload": {
      "auto_reload_n_ctx": true,
      "auto_reload_max_n_ctx": 131072,
      "auto_reload_vram_safety_factor": 0.85,
      "auto_reload_timeout_sec": 120
    }
  }
}
```

ENV overrides of `nctxReload` win over the file: `LB_NCTX_RELOAD_ENABLED`,
`LB_NCTX_RELOAD_MAX_N_CTX`, `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR`,
`LB_NCTX_RELOAD_TIMEOUT_SEC`.

Full variable reference: [ENV_VARS.md](docs/api/env-vars.ru.md).

## Project layout

```
cmd/
  balancer/      HTTP proxy + management API (18080/18081/18079)
  cppworker/     llama.cpp inference server (18092): adaptive loader, lazyload, n_ctx clamp
  agent/         GPU/CPU/RAM metrics (18032) for Ollama backends
  imageworker/   sd.cpp image backend (18093) + embedded metrics agent
  rpcworker/     RPC worker for distributed inference
internal/
  api/           REST handlers, routes, auth, rate limit
  balancer/      routing, scoring, sessions, n_ctx reload, RPC dispatch
  cppbackend/    Go wrapper over the C bridge (params, KV cache)
  rpccoordinator/ rpcworker/ rptensor/  distributed inference
  modelreplication/ virtualmodel/       replication groups, alias-on-pool
c/               C bridge to llama.cpp; llama.cpp and ggml are submodules
deployments/     docker-compose.stack.yml + env examples (profiles full/worker/balancer)
config/          balancer config, cppworker defaults, env examples
scripts/         build, bench, e2e, doctor, demo verification
docs/            documentation (RU + EN)
plans/           roadmap and phase reports
```

## Documentation

- **[Documentation index (English)](docs/README.ru.md)** - one table of contents for every document
- **[Индекс документации (Русский)](docs/README.ru.md)**
- [Installation](docs/getting-started/installation.md) /
  [Deployment (RU)](docs/getting-started/deployment.ru.md) /
  [Configuration (RU)](docs/getting-started/configuration.ru.md) /
  [API](docs/api/api.md) / [Metrics](docs/api/metrics.md) /
  [Troubleshooting](docs/operations/troubleshooting.md)
- [Image generation](docs/features/image-generation.md) /
  [Hardware presets](docs/getting-started/hardware-presets.md) /
  [Model parameters](docs/features/cppworker-model-params.md)
- [Environment variables (RU)](docs/api/env-vars.ru.md) - every variable with default and effect
- Russian-only deep dives: [RPC layout rules](docs/architecture/rpc-layout-rules.ru.md),
  [RPC benchmark (27B)](docs/features/rpc-benchmark-qwen38.ru.md),
  [WebUI demo mode](docs/features/webui-demo.ru.md),
  [WebUI auth](docs/features/webui-auth.ru.md),
  [Cline agent loop](docs/features/cline-agent-loop.ru.md)
- [Roadmap and session reports](docs/plans/README.md) (RU) | [Archive](docs/archive/README.md) (original language)

## Security

- **Change the API token.** The default (`changeme-bundled-with-agent-token`) is public:
  anyone who reaches the admin port (18081) gets full cluster control. Set
  `CPPWORKER_API_TOKEN` in `deployments/.env` and recreate the containers.
- **Keep 18081 private.** Clients only need 18079 (OpenAI/images) and 18080 (Ollama).
- **Do not publish real addresses or tokens.** Docs use RFC 5737 addresses (`192.0.2.x`);
  keep your own values in `deployments/.env`, which is git-ignored.
- **Personal data stays out of git.** `.env`, `_diag/`, `debug/` (session dumps, traces)
  are ignored; use placeholders (`example.com`, `192.0.2.x`, `<user>@<host>`).
- **WebUI has real auth.** Sessions, roles and CSRF: [auth docs](docs/features/webui-auth.ru.md).
  The emergency token is entered by the operator, never baked into the image.

## Commits and authorship

The commit author comes from git config: the local `user.name`/`user.email` in this
repository wins over the global one.

```bash
git config --local user.name  "Your Name"
git config --local user.email "you@example.com"
```

GitHub counts contributors **by the author field**, so each new identity with its own email
becomes a separate contributor. Historical service identities are mapped to the repository
owner through [`.mailmap`](.mailmap) without rewriting history:
`git shortlog -sne github/centurion | head`.

If an assistant or script commits, give it the same or an explicitly chosen service identity.

## License

MIT - see [LICENSE.md](LICENSE.md).

## Latest changes

Full history with dates, image tags and the defect behind each fix:
[CHANGELOG.md](CHANGELOG.md). The last three:

- **R232 (v0.7.211, 2026-10-24) - WebUI demo mode.** `WEBUI_DEMO_MODE=1` (env only) turns
  the panel into a guest-visible, read-only showcase: a deterministic 150 s scenario
  (clients -> balancer -> queue -> RPC donor -> AutoTune) with banner, counters and live
  event feed. All `/api/`, WebSocket and SSE traffic is intercepted in the browser, nothing
  reaches the real balancer. [Docs](docs/features/webui-demo.ru.md)
- **v1.0-rc2 (v0.7.210, 2026-10-24) - source release.** README with the RPC measurement,
  documentation brought up to the real state (AutoTune R230, Cline stream contract R231,
  ENV vars, troubleshooting), personal data and stand artifacts removed from the tree.
  [Release notes](docs/releases/v1.0-rc2-release-notes.ru.md)
- **R230 / R231 (v0.7.208-209) - AutoTune memory correctness and the Cline stream contract.**
  AutoTune stopped seeing "0 GB free" and recommending `-2/q4_0` (units, reload budget,
  architecture from `/api/show`, applied-plan verification); streaming no longer duplicates
  reasoning into `content`, drops `</think>` markers and sends exactly one final `[DONE]`.
