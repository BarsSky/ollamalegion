# OllamaLegion — a load balancer for the llama.cpp / Ollama stack

[Русская версия](README.ru.md)

OllamaLegion pools one or more consumer-GPU machines into a single inference pool behind one
address. The balancer distributes requests between `llama.cpp` (text) and
`stable-diffusion.cpp` (images) backends, chooses a node by free resources on its own, brings a
model up where it is not in memory, and ships a WebUI for managing the cluster. Clients keep
using the familiar APIs: **Ollama**, **OpenAI**, and A1111 (`/sdapi/v1/*`).

What the balancer takes on:

- **Routing** — backend selection by GPU/VRAM/CPU/RAM, model affinity, and client session
  pinning.
- **Admission queue** — instead of an instant 503, wait for a slot (`LB_ADMISSION_WAIT_SEC`) or
  for a backend (`LB_BACKEND_WAIT_SEC`).
- **Per-slot context window** — with several slots the total window is split between them.
- **Cold load** — a model that is not in memory is brought up on a free backend.
- **WebUI** — nodes and layer layout, queue, sessions, models, logs, diagnostics, `admin`/`viewer`
  roles.

Architecture in brief: client -> balancer (`:18080` Ollama, `:18079` OpenAI) -> backend selection,
window preflight, and queue -> `cppworker` (llama.cpp, `:18092`) or `imageworker` (sd.cpp,
`:18093`); some of the layers can go to a donor node over llama.cpp RPC. For details see the
[request lifecycle](docs/architecture/request-lifecycle.ru.md) and
[balancing and sessions](docs/architecture/balancing-guide.ru.md).

## Quick start

### Requirements

- Docker 24+ with Compose v2; for the GPU worker, an NVIDIA driver and `nvidia-container-toolkit`.
- Git with submodules: `c/llama.cpp` is a pinned submodule, and `cppworker` cannot be built
  without it.
- The smallest ready preset is 8 GB of VRAM (`rtx30-8gb`); the balancer itself gets by on about
  512 MB of RAM.
- The commands are written for Windows/PowerShell 7 (`pwsh`); bash equivalents are in the
  comments.

### Environment (once)

```powershell
git clone --recurse-submodules https://github.com/BarsSky/ollamalegion.git
cd ollamalegion
Copy-Item deployments/.env.example deployments/.env              # bash: cp deployments/.env.example deployments/.env
Copy-Item deployments/.env.bundled-with-agent.example deployments/.env.bundled-with-agent
```

Required in `deployments/.env`:

| Variable | Why |
|---|---|
| `CPPWORKER_API_TOKEN` | the single stack token (balancer, workers, WebUI); the public default `changeme-bundled-with-agent-token` — change it |
| `LB_ADMIN_USER`, `LB_ADMIN_PASSWORD` | the first WebUI administrator (the login lives in the balancer, not in the WebUI) |
| `BALANCER_TAG`, `WEBUI_TAG`, `CPPWORKER_GPU_TAG`, `IMAGE_WORKER_TAG`, `AGENT_TAG` | image tags; the release script writes them itself |

`CUDA_ARCH` (in the example `75;86;120`) is set in the same place and decides which `cppworker`
build architectures are compiled.

### Launch

```powershell
pwsh -File scripts/release-all.ps1 -Tag <tag>   # build with identity, tags into .env, roll out
```

When images with the required tags already exist, compose alone is enough:

```bash
docker compose -f deployments/docker-compose.stack.yml --profile full up -d
```

**From scratch (no images yet):** run `pwsh -File scripts/up-stack.ps1` (or
`./scripts/up-stack.sh`) instead of a raw `docker compose up -d`. It computes the git build
identity (required by the R486 guard in the Dockerfiles), builds only the images that are missing
and brings the stack up; see [up-from-scratch.md](docs/operations/up-from-scratch.md).

A plain `up -d` on a machine without images builds them itself, but the build needs the identity
build-args (`GIT_COMMIT` and others) from the process environment; without them the guard in the
Dockerfile stops the build.

### Addresses after startup

| Address | What it is |
|---|---|
| `http://localhost:18080` | Ollama-native: `/api/chat`, `/api/generate`, `/api/tags`, `/api/ps` |
| `http://localhost:18079` | OpenAI: `/v1/chat/completions`, `/v1/models`, `/v1/images/*`, `/sdapi/v1/*` |
| `http://localhost:18081` | admin API: backends, queue, metrics, WebSocket; do not expose it publicly |
| `http://localhost:18083` | WebUI |
| `:18092`, `:18093` | `cppworker` and `imageworker` — service ports for the internal network |

The external ports are `LB_PORT`, `LB_OPENAI_PORT`, `LB_ADMIN_PORT`, `WEBUI_PORT`,
`CPPWORKER_PORT_HOST`, `SDWORKER_PORT`.

### Check and first model

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
# PowerShell: $TOKEN = ((Get-Content deployments/.env | ? { $_ -match '^CPPWORKER_API_TOKEN=' }) -split '=',2)[1]
curl -s http://localhost:18081/api/v1/ping                                # 200 — the balancer is alive
curl -s http://localhost:18081/api/v1/health                              # health, no token needed
curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends   # backends and statuses
curl -s http://localhost:18080/api/tags | jq -r '.models[].name'          # models on disk
```

The WebUI is at `http://localhost:18083`; sign in with the
`LB_ADMIN_USER`/`LB_ADMIN_PASSWORD` pair. The first request to a model that is not loaded is served
for as long as loading takes (a 27B Q4 on an A10 — minutes): the balancer waits up to
`LB_AUTO_LOAD_WAIT_SEC` (900 s in this compose), then returns `503` with `Retry-After`.

```bash
curl -s -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"<name>","messages":[{"role":"user","content":"hi"}],"stream":false}' \
  http://localhost:18080/api/chat
```

### Stop and upgrade

```bash
docker compose -f deployments/docker-compose.stack.yml --profile full down     # volumes are kept
docker compose -f deployments/docker-compose.stack.yml --profile full down -v  # together with the state
```

To upgrade, cut a new release with `scripts/release-all.ps1 -Tag <new-tag>`; to re-pin the tags
only, without a build and rollout, use the same script with `-SkipBuild -NoDeploy`.

## Documentation

The table of contents for all documentation is [docs/README.ru.md](docs/README.ru.md).

| Document | What it covers |
|---|---|
| [installation.ru.md](docs/getting-started/installation.ru.md) | hardware requirements, build, stub mode |
| [deployment.ru.md](docs/getting-started/deployment.ru.md) | deployment: profiles, environments, ports |
| [configuration.ru.md](docs/getting-started/configuration.ru.md) | the balancer's `config.json` and env overrides |
| [hardware-presets.ru.md](docs/getting-started/hardware-presets.ru.md) | presets for RTX 30xx/40xx/50xx and A10 |
| [deployment-stack.ru.md](docs/operations/deployment-stack.ru.md) | profiles, ports, remote workers, checks |
| [docker-compose-guide.ru.md](docs/operations/docker-compose-guide.ru.md) | which compose file is responsible for what |
| [deployment-troubleshooting.ru.md](docs/operations/deployment-troubleshooting.ru.md) | live outages and environment traps |
| [troubleshooting.ru.md](docs/operations/troubleshooting.ru.md) | general failure triage by symptom |
| [deployments/README.md](deployments/README.md) | what lives in `deployments/`, which file is canonical |
| [request-lifecycle.ru.md](docs/architecture/request-lifecycle.ru.md) | the request lifecycle and its stages |
| [balancing-guide.ru.md](docs/architecture/balancing-guide.ru.md) | backend selection and session pinning |
| [context-numbers.ru.md](docs/architecture/context-numbers.ru.md) | the context window: where the numbers come from |
| [availability-first-r493.ru.md](docs/architecture/availability-first-r493.ru.md) | why a request waits while nodes are free |
| [window-align-r494.ru.md](docs/architecture/window-align-r494.ru.md) | aligning the windows of one model's copies |
| [feature-switches.ru.md](docs/architecture/feature-switches.ru.md) | switches: what turns on what |
| [env-vars.ru.md](docs/api/env-vars.ru.md) | every environment variable with its default |
| [api.ru.md](docs/api/api.ru.md) | the management REST API |
| [rpc-benchmark-qwen38.ru.md](docs/features/rpc-benchmark-qwen38.ru.md) | layer layout over RPC, benchmarked on 27B |
| [webui-auth.ru.md](docs/features/webui-auth.ru.md) | login, roles, CSRF, sessions |
| [webui-demo.ru.md](docs/features/webui-demo.ru.md) | demo mode: the WebUI without a cluster |
| [image-generation.ru.md](docs/features/image-generation.ru.md) | image generation on the same balancer |
| [debts.ru.md](docs/architecture/debts.ru.md) | the register of deliberately undone work |

Release and image-tag history is in [CHANGELOG.md](CHANGELOG.md).

## Operations and management

The key variable groups (the complete list is in [env-vars.ru.md](docs/api/env-vars.ru.md)):

| Group | Variables |
|---|---|
| Routing and queue | `LB_MODEL_AFFINITY`, `LB_SESSION_STICKINESS`, `LB_ADMISSION_WAIT_SEC`, `LB_BACKEND_WAIT_SEC`, `LB_UNIFIED_ADMISSION` |
| Context window | `LB_NCTX_RELOAD_ENABLED`, `LB_NCTX_RELOAD_MAX_N_CTX`, `LB_NCTX_PREFLIGHT_ENABLED`, `LB_PREFLIGHT_EXACT_TOKENS`, `LB_R459_NEED_RESERVE`, `LB_WINDOW_ALIGN_MAX_N_CTX` |
| Access | `CPPWORKER_API_TOKEN`, `LB_ADMIN_USER`, `LB_ADMIN_PASSWORD`, `LB_ADMIN_ENV_SYNC`, `LB_SESSIONS_FILE` |
| Worker | `CPPWORKER_CTX_SIZE`, `CPPWORKER_GPU_LAYERS`, `CPPWORKER_N_PARALLEL`, `CPPWORKER_VRAM_OVERHEAD_MB` |
| Image tags | `BALANCER_TAG`, `WEBUI_TAG`, `CPPWORKER_GPU_TAG`, `IMAGE_WORKER_TAG`, `AGENT_TAG`, `CUDA_ARCH` |

Logs and diagnostics (in the WebUI these are the "Backends", "Queue", "Logs", and "Diagnostics"
pages):

```bash
docker compose -f deployments/docker-compose.stack.yml logs -f loadbalancer cppworker-gpu
curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/queue/stats
curl -s -H "X-API-Token: $TOKEN" -o diag.zip http://localhost:18081/api/v1/diag/bundle
```

Release and checks:

| Script | What it does |
|---|---|
| `scripts/release-all.ps1` | build, tag, roll out; writes `deployments/.env` and the manifest |
| `scripts/check-image-tags.ps1` | tag consistency across compose, `.env`, and the manifest (no docker needed) |
| `scripts/check_webui_assets.py`, `scripts/check_webui_i18n_keys.py`, `scripts/check_webui_refs.js` | WebUI statics: assets without CDN, i18n keys, function calls |
| `scripts/check_docs_claims.py` | the documentation references no nonexistent files or variables |

## Limitations and known behavior

- **Window growth only on an explicit client declaration.** The prompt does not fit and
  `options.num_ctx` was not sent (Cline does not send it) — the model is not reloaded, and the
  client gets 413 `context_window_exhausted_no_growth_request` with the numbers `prompt_tokens`,
  `loaded_window_tokens`, `required_window_tokens`, `requested_n_ctx` and the caps; a declaration
  above the loaded window grows the window in the background
  ([context-numbers.ru.md](docs/architecture/context-numbers.ru.md)).
- **Non-streaming preflight keeps the old contract:** a declared window above the loaded one means
  a synchronous reload (or `413` when auto-reload is off); there is no background growth there.
- **Slots share the window:** `CPPWORKER_N_PARALLEL=1` by default, and with `parallel=2` a session
  gets half of the total window.
- **Authentication belongs to the balancer:** users and sessions are files on the `balancer_data`
  volume (`LB_USERS_FILE`, `LB_SESSIONS_FILE`); `LB_ADMIN_*` overwrite the password when
  `LB_ADMIN_ENV_SYNC=1`, and deleting the volume means losing the login.
- **Timeouts are off by default:** inference is unbounded (`LB_REQUEST_TIMEOUT_SEC=0`), a stalled
  stream is caught by `LB_STREAM_STALL_ABORT_SEC`, the first byte by
  `LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC`.

## Status and license

- The last entry in [CHANGELOG.md](CHANGELOG.md) is 0.7.477 (R679, 2026-11-21).
- MIT license — [LICENSE.md](LICENSE.md); the documentation is maintained in Russian, and some
  files exist only in that language.
