# OllamaLegion Installation and Build

> **Version:** 2026-06-22  
> **Related documents:** [`deployment.md`](deployment.md), [`configuration.md`](configuration.md), [`audit-2026-06.md`](audit-2026-06.md)

## Table of Contents

1. [System Requirements](#1-system-requirements)
2. [Docker Installation (recommended)](#2-docker-installation)
3. [Local Installation (from source)](#3-local-installation-from-source)
4. [Building CppWorker](#4-building-cppworker)
5. [Operating Modes Without an Agent](#5-operating-modes-without-an-agent)
6. [Smoke Verification](#6-smoke-verification)

---

## 1. System Requirements

### Load Balancer

| Component | Minimum | Recommended |
|---|---|---|
| OS | Linux / Windows / macOS | Linux (Ubuntu 22.04+, Debian 12+) |
| CPU | 2 cores | 4 cores |
| RAM | 512 MB | 1 GB |
| Disk | 100 MB | 500 MB |
| Go | 1.21+ | 1.21+ |
| Network | 100 Mbps | 1 Gbps |

### Agent (on each server running Ollama/CppWorker)

| Component | Minimum | Recommended |
|---|---|---|
| OS | Linux (Ubuntu 20.04+, Debian 11+) | Ubuntu 22.04 LTS |
| CPU | 1 core | 2 cores |
| RAM | 128 MB | 256 MB |
| Disk | 50 MB | 100 MB |
| Docker | 20.10+ | 24.0+ |

**GPU mode** additionally requires:

- NVIDIA GPU (Compute Capability 5.0+, recommended 7.0+).
- NVIDIA Driver 470.x+ (recommended 535.x+).
- NVIDIA Container Toolkit 1.12+.
- CUDA 11.0+ (recommended 12.0+).

### WebUI (Nginx + static)

| Component | Minimum |
|---|---|
| CPU | 1 core |
| RAM | 256 MB |
| Disk | 100 MB |

---

## 2. Docker Installation

Recommended approach for production.

### 2.1 Cloning

```bash
git clone https://github.com/BarsSky/ollamalegion.git
cd ollamalegion
```

### 2.2 Preparing `deployments/.env`

```bash
cp config/config.example.json config/config.json
cp deployments/.env.bundled.example deployments/.env.bundled  # optional
# edit to match your parameters:
# - LB_PORT, LB_API_PORT (default 18080, 18081)
# - CPPWORKER_API_TOKEN (for auto-registration)
# - BALANCER_URL (for cppworker auto-registration)
```

### 2.3 Starting the bundled stack

```bash
# Linux/macOS/WSL
./scripts/start-bundled.sh

# Windows PowerShell (Legacy builder is required!)
$env:DOCKER_BUILDKIT=0
.\scripts\start-bundled.ps1
```

The stack brings up:
- `cppworker-gpu` (port 18092)
- `loadbalancer` (ports 18080, 18081)
- `webui` (port 18083)

### 2.4 Alternative compose files

```bash
# Balancer + WebUI only (without CppWorker)
cd deployments && docker compose up -d

# CppWorker only (CPU)
docker compose -f docker-compose.cppworker.yml --profile cpu up -d --build

# CppWorker only (GPU)
docker compose -f docker-compose.cppworker.yml --profile gpu up -d --build

# CppWorker only (stub — for tests without llama.cpp)
docker compose -f docker-compose.cppworker.yml --profile stub up -d --build

# Agent on a separate server
docker compose -f docker-compose.agent.yml --env-file .env up -d --build
```

### 2.5 Building images manually

```powershell
$env:DOCKER_BUILDKIT=0  # Legacy builder is required on Windows 11 + Docker Desktop

docker build -t ollama-legion/balancer:latest --target production -f docker/balancer/Dockerfile .
docker build -t ollama-legion/cppworker:cpu --target runtime -f docker/cppworker/Dockerfile.cpu .
docker build -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .
docker build -t ollama-legion/cppworker:stub --target runtime -f docker/cppworker/Dockerfile.stub .
docker build -t ollama-legion/agent:cpu --target agent-cpu -f docker/agent/Dockerfile .
docker build -t ollama-legion/agent:gpu --target agent-gpu -f docker/agent/Dockerfile .
docker build -t ollama-legion/webui:latest -f docker/webui/Dockerfile .
```

| Image | Size | When to use |
|---|---|---|
| `cppworker:cpu` | ~150 MB | Production CPU inference |
| `cppworker:gpu` | ~3.2 GB | Production GPU (CUDA) |
| `cppworker:stub` | ~100 MB | CI/tests without llama.cpp |
| `balancer` | ~54 MB | Load balancer |
| `agent:cpu` | ~16 MB | Metrics without GPU |
| `agent:gpu` | ~395 MB | Metrics with NVML |
| `webui` | ~101 MB | Web UI dashboard |

---

## 3. Local Installation (from source)

### 3.1 Building the load balancer

```bash
go build -o balancer ./cmd/balancer
./balancer --port 18080 --api-port 18081 --config ./config/config.json
```

### 3.2 Building the agent

```bash
go build -o agent ./cmd/agent
AGENT_ID=local-1 BALANCER_URL=http://localhost:18081 ./agent
```

### 3.3 Stub build of CppWorker (for tests, without llama.cpp)

```bash
# Linux/macOS
go build -tags llama_stub -o cppworker-stub ./cmd/cppworker
./cppworker-stub --port 18091 --models-dir ./models

# Windows (binary is separate, see .clinerules §12)
go test -c ./cmd/cppworker -tags llama_stub -o cppworker_test.exe
.\cppworker_test.exe
```

---

## 4. Building CppWorker (with real llama.cpp)

Only for GPU/CPU inference. Stub builds are sufficient for 90% of development.

### 4.1 Requirements

- CMake 3.20+
- C++17 compiler (gcc 9+, clang 10+, MSVC 19.30+)
- CUDA 12.2+ (GPU only)
- ~30 minutes for a full build with llama.cpp

### 4.2 GPU build (CUDA)

```bash
cd docker/cppworker
docker build -f Dockerfile.gpu --target runtime -t ollama-legion/cppworker:gpu ..
```

### 4.3 CPU build (Alpine)

```bash
cd docker/cppworker
docker build -f Dockerfile.cpu --target runtime -t ollama-legion/cppworker:cpu ..
```

### 4.4 GPU architectures

- The `CUDA_ARCH` argument controls the `:gpu-<arch>` image tag.
- Supported: `80` (A100), `86` (RTX 3070/3080/3090), `89` (RTX 4090), `90` (H100), `all` (multi-arch).
- Example: `CUDA_ARCH=86 ./scripts/build-containers.sh cppworker`.

---

## 5. Operating Modes Without an Agent

The load balancer **can** operate without an agent, but with limited functionality.

### 5.1 What works without an agent

| Feature | Works? | Comment |
|---|---|---|
| HTTP proxying (`/api/generate`, `/api/chat`, `/api/embeddings`) | ✅ | Direct passthrough to backend |
| Round-robin balancing | ✅ | Does not require metrics |
| Health check | ✅ | Via `/api/tags` on Ollama |
| Session stickiness | ✅ | Stored in balancer memory |
| Queue / backpressure | ✅ | `ActiveReqs` counters |
| Retry / failover | ✅ | To any healthy backend |
| Least-connections | ✅ | By `ActiveReqs` (without agent) |
| Basic scoring | ✅ | Simplified formula |

### 5.2 What does NOT work without an agent

| Feature | Requires agent | Reason |
|---|---|---|
| Model Affinity | ✅ | The `RunningModels` list comes from the agent |
| Resource-aware scoring v2 | ✅ | GPU/VRAM/CPU/RAM metrics |
| Prewarm Controller | ✅ | Needs `freeVRAM` from the agent |
| Auto-pull (Pull-on-Demand) | ✅ | Checks `RunningModels` |
| Headroom Reservation | ✅ | Needs VRAM usage metrics |
| Predictor | ✅ | Metrics history |
| Model Replication (Variant A) | ✅ | Extended metrics |
| Virtual Model Router (Variant C) | ✅ | Pipeline slicing metrics |
| Adaptive Weight Tuner | ✅ | Latency/success history |
| Unload Scheduler (smart eviction) | ✅ | Metrics |
| Agent actions in WebUI (Restart/Logs/Config) | ✅ | No endpoints without an agent |

### 5.3 Minimal config without an agent

```json
{
  "balancing": {
    "algorithm": "roundrobin",
    "modelAffinity": false,
    "sessionStickiness": true,
    "useEnhancedScoring": false,
    "prewarm": { "enabled": false },
    "autoPull": { "enabled": false }
  }
}
```

---

## 6. Smoke Verification

```bash
# 1. Balancer health (liveness, always 200)
curl http://localhost:18081/api/v1/ping

# 2. Balancer health (readiness, 503 in degraded)
curl http://localhost:18081/api/v1/health

# 3. CppWorker health (via balancer)
curl http://localhost:18092/health

# 4. List of backends
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends

# 5. Direct generation via CppWorker
curl -X POST http://localhost:18092/api/generate \
  -H "Content-Type: application/json" \
  -d '{"model":"model.gguf","prompt":"Hello","stream":false}'

# 6. WebUI Dashboard
open http://localhost:18083  # or http://localhost:18030 if not using Docker
```

If all 6 steps pass — installation is successful.

---

## 7. Related Documents

- [`deployment.md`](deployment.md) — Docker Compose deployment.
- [`agent-deployment.md`](agent-deployment.md) — CPU/GPU agent deployment.
- [`audit-2026-06.md`](audit-2026-06.md) — current state of the code.
- [`../.clinerules`](../.clinerules) §12 — useful commands (section "Local Development").
