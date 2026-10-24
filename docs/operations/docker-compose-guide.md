# Docker Compose: deployment files guide

OllamaLegion uses a **multi-file** Docker Compose structure. Each file is responsible for its own component or run mode. All the files live in `deployments/`.

## Quick reference

| File | What it starts | When to use |
|------|---------------|-------------------|
| `docker-compose.yml` | Balancer + WebUI | **Main start** — the core of the system |
| `docker-compose.full.yml` | Balancer + WebUI + CppWorker (CPU/GPU/STUB) | **Full stack with profiles** — everything on one host |
| `docker-compose.agent.yml` | Agent (CPU or GPU) | On every node with Ollama where monitoring is needed |
| `docker-compose.agent.gpu.yml` | Agent overlay (production GPU) | On top of `agent.yml` for production GPU nodes |
| `docker-compose.llama.cpp.yml` | CppWorker + Agent (llama.cpp, GPU) | A separate node with llama.cpp instead of Ollama |
| `docker-compose.llama.cpu.yml` | CppWorker + Agent (llama.cpp, CPU-only) | The same thing, but without a GPU |
| `docker-compose.cppworker.yml` | CppWorker only (CPU/GPU/STUB) | Isolated cppworker run with profiles |
| `docker-compose.cppworker-with-agent.yml` | CppWorker + Agent (sidecar) | Sidecar to an existing balancer; the agent registers the backend |
| `docker-compose.cppworker-with-agent.standalone.yml` | Balancer + WebUI + CppWorker + Agent | A complete self-contained stack with a cppworker+agent pair |
| `docker-compose.cocoindex.yml` | CocoIndex (search service) | A specialized component |

---

## 1. Building Docker images

Each component is built from its own Dockerfile in `docker/<component>/Dockerfile`. The build is run **from the repository root** (`c:/Ollama/ollamalegion`).

### 1.1. Prerequisites

- **Docker Desktop** / **Docker Engine** version 24+
- **BuildKit** (enabled by default in Docker Desktop 4.x+)
- **Go 1.24+** (only if you build on the host; not needed for Docker)
- **NVIDIA Container Toolkit** — for GPU targets: [instructions](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html)

### 1.2. Balancer image

Built from `docker/balancer/Dockerfile`. Multi-stage build: the Go builder compiles the binary, then copies it into a minimal `alpine`.

```powershell
# In the repository root (c:\Ollama\ollamalegion)
docker build -t ollama-legion/balancer:latest -f docker/balancer/Dockerfile .
```

| Target | Description | Image size |
|--------|----------|---------------|
| `default` | Balancer (the only target) | ~25 MB |

**Build arguments (--build-arg):**

| Argument | Default | Purpose |
|----------|-------------|------------|
| `CGO_ENABLED` | `0` | CGO disabled (static linking) |
| `GOOS` | `linux` | Target OS |
| `GOARCH` | `amd64` | Target architecture |

**What the build needs:** only Go and the sources in the root. The dependencies (`go.sum`, `go.mod`) are copied into the container automatically.

---

### 1.3. CppWorker images (llama.cpp worker)

Three independent Dockerfiles in `docker/cppworker/`. Each one is for its own mode.

```powershell
# CPU — real llama.cpp, without CUDA (real inference):
docker build -t ollama-legion/cppworker:cpu --target runtime -f docker/cppworker/Dockerfile.cpu .

# GPU — real llama.cpp + CUDA (multi-GPU inference, ~10-15 minutes):
docker build -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .

# GPU — accelerated build for your GPU only (3-4 times faster):
# RTX 4060/4070/4080/4090 (Ada Lovelace):
docker build --build-arg CUDA_ARCH="89" -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .
# RTX 3060/3070/3080/3090 (Ampere):
docker build --build-arg CUDA_ARCH="86" -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .
# GTX 1660/RTX 2060 (Turing):
docker build --build-arg CUDA_ARCH="75" -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .

# STUB — a stub without llama.cpp (CI/tests only):
docker build -t ollama-legion/cppworker:stub --target runtime -f docker/cppworker/Dockerfile.stub .
```

| Image | Dockerfile | Description | Size | Base image |
|-------|-----------|----------|--------|---------------|
| `cppworker:cpu` | `Dockerfile.cpu` | Real llama.cpp CPU-only | ~150 MB | `alpine:latest` |
| `cppworker:gpu` | `Dockerfile.gpu` | Real llama.cpp + CUDA multi-GPU | ~350 MB | `nvidia/cuda:12.2-runtime-ubuntu` |
| `cppworker:stub` | `Dockerfile.stub` | Stub (llama_stub, CGO_ENABLED=0) | ~100 MB | `alpine:latest` |

> **Important:** the CPU image uses **real llama.cpp** (CGo calls, without the `llama_stub` tag). The STUB image is only for CI and functional tests, not for production.

**What is included in the image:**
- Python 3 + `huggingface_hub` (automatic GGUF download from HuggingFace at startup)
- `wget`, `curl`, `ca-certificates`
- Entrypoint: `docker/cppworker/entrypoint.sh` (automatic model download)

---

### 1.4. Web UI image

Built from `docker/webui/Dockerfile`. Static files → `nginx:alpine`.

```powershell
docker build -t ollama-legion/webui:latest -f docker/webui/Dockerfile .
```

| Target | Description | Size |
|--------|----------|--------|
| `default` | nginx + static files + entrypoint | ~65 MB |

**Entrypoint:**
- Generates `nginx.conf` from a template with the `API_HOST`, `API_PORT`, `CPPWORKER_HOST`, `CPPWORKER_PORT` variables
- Generates `js/modules/config.js` with the runtime parameters (`WEBUI_CONFIG`)
- Starts nginx on port `${NGINX_PORT}` (default 80)

**Verification after the build:**
```powershell
docker run --rm -p 18030:80 -e API_HOST=localhost -e API_PORT=18081 ollama-legion/webui:latest
# WebUI is available at: http://localhost:18030
```

---

### 1.5. Agent image (monitoring agent)

Built from `docker/agent/Dockerfile`. Runs on nodes with Ollama or llama.cpp.

```powershell
# CPU version:
docker build -t ollama-legion/agent:latest --target agent -f docker/agent/Dockerfile .

# GPU version:
docker build -t ollama-legion/agent:latest-gpu --target agent-gpu -f docker/agent/Dockerfile .
```

| Target | Description | Size |
|--------|----------|--------|
| `agent` | CPU-only agent | ~30 MB |
| `agent-gpu` | Agent with NVIDIA utilities for GPU metrics | ~300 MB |

---

### 1.6. Building the whole stack with a single command

Use `deployments/docker-compose.full.yml` with profiles to build and start the balancer + WebUI + CppWorker on one network:

```powershell
# In the repository root:

# CPU (default, real llama.cpp):
docker compose -f deployments/docker-compose.full.yml --profile default build

# GPU (llama.cpp + CUDA):
docker compose -f deployments/docker-compose.full.yml --profile gpu build

# STUB (CI/tests):
docker compose -f deployments/docker-compose.full.yml --profile stub build

# Build without cache (clean rebuild):
docker compose -f deployments/docker-compose.full.yml --profile default build --no-cache

# Build a specific service:
docker compose -f deployments/docker-compose.full.yml --profile default build cppworker-cpu
docker compose -f deployments/docker-compose.full.yml --profile gpu build cppworker-gpu
docker compose -f deployments/docker-compose.full.yml --profile default build loadbalancer
docker compose -f deployments/docker-compose.full.yml --profile default build webui
```

---

### 1.7. Quick start with a single command (build + run)

```powershell
# Full stack — CPU (real llama.cpp, default):
docker compose -f deployments/docker-compose.full.yml up -d --build

# Full stack — GPU (llama.cpp + CUDA):
$env:COMPOSE_PROFILES="gpu"
docker compose -f deployments/docker-compose.full.yml up -d --build

# Full stack — STUB (CI/tests only):
docker compose -f deployments/docker-compose.full.yml --profile stub up -d --build

# With automatic model download from HuggingFace:
$env:HF_TOKEN="hf_xxx"
$env:HF_AUTO_DOWNLOAD_REPO="unsloth/gemma-4-E4B-it-GGUF"
$env:HF_AUTO_DOWNLOAD_FILE="gemma-4-E4B-it-Q4_K_M.gguf"
docker compose -f deployments/docker-compose.full.yml up -d --build
```

---

### 1.8. Typical build errors

| Error | Cause | Solution |
|--------|---------|---------|
| `go: module not found` | `go.mod` or `go.sum` were not copied into the container | Make sure the build is run from the repository root |
| `unable to evaluate symlinks` | Symlinks or path problems on Windows | Use the WSL2 backend (`docker context use wsl`) |
| `CUDA not found` | The NVIDIA Container Toolkit is missing | Install [`nvidia-container-toolkit`](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html) |
| `no space left on device` | Ran out of space for Docker images | `docker system prune -a` (careful: it deletes all unused images!) |
| `SecretsUsedInArgOrEnv: ENV "HF_TOKEN"` | A warning, not an error | The token is passed as an ENV — fine for a dev environment; in production use [Docker secrets](https://docs.docker.com/compose/use-secrets/) |
| `BuildKit not available` | An old Docker version | Update Docker to 24+, or `export DOCKER_BUILDKIT=0` to disable it |

---

### 1.9. Inspecting layers and sizes

```powershell
# Size of all ollama-legion images
docker images | Select-String "ollama-legion"

# Layer history of a specific image
docker history ollama-legion/cppworker:cpu

# Clean the builder cache (free 10+ GB)
docker builder prune -a --force
```

---

## 2. Main stack: balancer + WebUI

```bash
cd deployments
docker compose up -d
```

Starts:
- **loadbalancer** — ports `18080` (proxy), `18081` (API)
- **webui** — port `18030` (dashboard)

Backends are registered later — through the WebUI, the API, or agents.

---

## 3. Agent on a node with Ollama

The agent runs **separately** on every server with Ollama. It collects metrics (GPU, VRAM, CPU) and sends them to the balancer.

### CPU server (default):
```bash
cp .env.llama.example .env
# edit .env: AGENT_ID, BALANCER_URL, AGENT_PUBLIC_HOST
docker compose -f docker-compose.agent.yml up -d
```

### GPU server (with NVIDIA):
```bash
docker compose -f docker-compose.agent.yml --profile gpu up -d
```

### Production GPU (with host networking):
```bash
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml --profile gpu up -d
```

**Important:** the agent and the balancer **do not have to** be on the same Docker network. Only IP reachability over HTTP is needed:
- Agent → Balancer: `BALANCER_URL`
- Balancer → Agent: `AGENT_PUBLIC_HOST:18032`
- Balancer → Ollama: `AGENT_PUBLIC_HOST:11434`

---

## 4. Standalone llama.cpp backend with an agent

This is the **main way** to bring up a self-contained inference node on llama.cpp (instead of Ollama). It runs on any machine with a GPU or CPU.

### Step 1: Preparation

```bash
cd deployments
cp .env.llama.example .env
```

Edit `.env`:
```ini
# Node name (becomes AGENT_ID and the container name)
NODE_NAME=llama-gpu-1

# Balancer URL (required!)
BALANCER_URL=http://192.0.2.10:18081

# Directory with GGUF models
MODELS_DIR=/data/models

# llama.cpp parameters
LLAMA_N_GPU_LAYERS=-1    # -1 = all layers on the GPU, 0 = CPU-only
LLAMA_CTX_SIZE=8192
LLAMA_BATCH_SIZE=512
LLAMA_FLASH_ATTN=true

# Ports (optional)
LLAMA_HTTP_PORT=18091
LLAMA_GRPC_PORT=19000
AGENT_PORT=18032
```

### Step 2: Run (GPU)

```bash
docker compose -f docker-compose.llama.cpp.yml up -d
```

The **NVIDIA Container Toolkit** is required: [installation instructions](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html).

### Step 2a: Run (CPU-only)

```bash
docker compose -f docker-compose.llama.cpu.yml up -d
```

### Step 3: Verification

```bash
# Check that CppWorker responds
curl http://localhost:18091/api/v1/cppworker/health

# Check that the agent has registered on the balancer
curl http://<BALANCER_IP>:18081/api/v1/backends
```

After a successful start the node registers itself on the balancer automatically. In the WebUI, a new backend of type `🦒 llama.cpp` appears on the dashboard.

### What exactly runs

```
llama-net (bridge):
├── cppworker (18091 — REST API, 19000 — gRPC)
│   └── /models → GGUF models (read-only)
└── agent (18032 — heartbeat + metrics)
    └── BACKEND_TYPE=llama_cpp
    └── CPPWORKER_URL=http://cppworker:18091
    └── BALANCER_URL=... (registration)
```

The agent inside this stack uses `BACKEND_TYPE=llama_cpp`, so the balancer understands that this is a llama.cpp node and routes requests through `CPPWORKER_URL` rather than through the Ollama API.

---

## 4.1. Sidecar stack: cppworker + agent (cppworker-with-agent)

If you already have an OllamaLegion balancer deployed (via `docker-compose.yml` or `docker-compose.cppworker-bundled.yml`) and you want to add a llama.cpp node to it as a "cppworker + agent" sidecar pair, use `docker-compose.cppworker-with-agent.yml`. This stack is a **minimalist alternative** to `docker-compose.llama.cpp.yml`, focused on tightly coupling cppworker and agent in a single compose network.

### Step 1: Preparation

```powershell
cd deployments
cp .env.bundled.example .env.cppworker-agent
```

Edit `.env.cppworker-agent`:
```ini
# === Balancer token (the same as in the balancer) ===
LB_TOKEN=your_balancer_token

# === URL of the existing balancer (required!) ===
# On the host where the balancer runs:
BALANCER_URL=http://host.docker.internal:18081

# === Path to the models on the host ===
MODELS_DIR=C:\Ollama\models

# === CppWorker ===
CPPWORKER_PORT=18091
CPPWORKER_ADVERTISED_PORT=18092
CPPWORKER_GPU_LAYERS=99

# === Agent ===
AGENT_PORT=18032
```

### Step 2: Run

```powershell
$env:DOCKER_BUILDKIT=0
docker compose -f docker-compose.cppworker-with-agent.yml `
  --env-file .env.cppworker-agent up -d --build
```

### Step 3: Verification

```powershell
# Health cppworker:
curl http://localhost:18092/health

# Health agent:
curl http://localhost:18032/health

# Backend registered on the balancer:
curl -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/backends
# The response will contain "cppworker-gpu-with-agent" with hasAgent=true
```

### What exactly runs

```
cppworker-agent-net (bridge):
├── cppworker-gpu (port 18092 → 18091 in the container)
│   ├── CPPWORKER_REGISTER_DISABLE=true (Go-side registration disabled)
│   ├── listens on :18091 on 0.0.0.0
│   └── /models → GGUF models (read-only)
└── agent (port 18032)
    ├── AGENT_BACKEND_TYPE=llama_cpp
    ├── AGENT_CPPWORKER_URL=http://cppworker-gpu-with-agent:18091
    ├── BALANCER_URL=http://host.docker.internal:18081
    └── depends on cppworker (depends_on: service_healthy)
```

### Alternative standalone stack

If you **do not have** an existing balancer and you want a self-contained `balancer + webui + cppworker + agent` stack in a single `docker compose`, use `docker-compose.cppworker-with-agent.standalone.yml`. It brings up all four services and connects them into two bridge networks (`balancer-net` + `cppworker-agent-net`). The same `CPPWORKER_REGISTER_DISABLE=true` is used to prevent duplicate backends.

```powershell
docker compose -f docker-compose.cppworker-with-agent.standalone.yml `
  --env-file .env.cppworker-standalone up -d --build
# WebUI: http://localhost:18083
# The backend cppworker-gpu-standalone appears automatically with hasAgent=true
```

### Key differences from `docker-compose.llama.cpp.yml`

| Aspect | `llama.cpp.yml` | `cppworker-with-agent.yml` |
|--------|----------------|---------------------------|
| Registration with the balancer | agent → `BALANCER_URL` | agent → `BALANCER_URL` |
| `CPPWORKER_REGISTER_DISABLE` | false (default) | **true** (explicitly disabled) |
| Go-side duplicate registration | Possible | **Prevented** |
| Suitable for the bundled stack | No | **Yes** (no conflicts with the bundled cppworker) |
| Healthcheck | curl/wget | Built-in `-healthcheck` binary |
| `extra_hosts: host.docker.internal` | As needed | **Always** (to reach a balancer running on the host) |

### Acceptance criteria

1. After `docker compose up -d --build` the `cppworker-gpu-with-agent` container starts and responds at `http://localhost:18092/health` with HTTP 200.
2. The `cppworker-gpu-agent` container starts after cppworker (depends_on) and responds at `http://localhost:18032/health`.
3. In the agent logs the line `backend registered successfully` is visible with `backendId=cppworker-gpu-with-agent` and `backendType=llama_cpp`.
4. `GET /api/v1/backends` shows exactly one backend `cppworker-gpu-with-agent` with `hasAgent=true`. **There are no duplicates.**
5. In WebUI → Dashboard → Backends, live GPU/CPU/RAM metrics are visible (via the agent), not zeros.

---

## 5. Summary: run modes for different scenarios

### Scenario A: "Everything local on one machine with Ollama"
```bash
# Terminal 1 — balancer + WebUI
docker compose up -d

# Terminal 2 — agent for the local Ollama
docker compose -f docker-compose.agent.yml up -d
```

### Scenario B: "Balancer on a server, backends on GPU machines with Ollama"
```bash
# On the balancer server:
docker compose up -d

# On each GPU machine:
docker compose -f docker-compose.agent.yml --profile gpu up -d
```

### Scenario C: "Balancer + llama.cpp node (without Ollama)"
```bash
# On the balancer server:
docker compose up -d

# On the GPU machine with llama.cpp:
docker compose -f docker-compose.llama.cpp.yml up -d
```

### Scenario D: "Mixed cluster (Ollama + llama.cpp)"
```bash
# Balancer:
docker compose up -d

# GPU machine 1 with Ollama:
docker compose -f docker-compose.agent.yml --profile gpu up -d

# GPU machine 2 with llama.cpp:
docker compose -f docker-compose.llama.cpp.yml up -d

# CPU machine 3 with llama.cpp:
docker compose -f docker-compose.llama.cpu.yml up -d
```

The balancer automatically determines the type of each backend (Ollama vs llama.cpp) and routes requests correctly. A marker for the current engine appears in the WebUI sidebar.

---

## 6. How to tell which engine is being used right now

Right after connecting to the balancer, an **engine type marker** is displayed in the left sidebar of the WebUI under the logo:

| Marker | Meaning |
|--------|----------|
| 🦙 **Ollama API** | All backends are Ollama |
| 🦒 **llama.cpp** | All backends are llama.cpp |
| 🔌 **Auto-detection** | A mixed cluster, or the engine is not set |

Hovering over the marker shows a tooltip with details: the number of backends of each type.
