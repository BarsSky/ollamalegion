# OllamaLegion Deployment

> **Version:** 3.0 (2026-07-31, updated for v0.5.2)
> **Related documents:** [`installation.md`](installation.md), [`agent-deployment.md`](../operations/agent-deployment.md), [`backend-type-isolation.md`](../architecture/backend-type-isolation.md), [`../../CHANGELOG.md`](../../CHANGELOG.md)

## What's new in v0.5.x (compared to v0.2.0)

List of features that appeared since the previous version of this document (2026-06-22):

- **Round 13 (n_parallel > 1)** — several concurrent sequences on one model without race conditions
- **Round 14 (native enable_thinking)** — for Qwen3-thinking models without a `<think>` block
- **Round 15.1 (Batched Parallel Inference)** — true parallel llama_decode via BatchedScheduler
- **Round 15.2 (multi-token prefill + temperature sampling + vocab-aware EOG)** — production-ready sampling
- **Round 16 (Round 16 code review)** — CRITICAL `temperature=0` fix, 4 P1 observability/resilience fixes
- **WebUI Sprint 30** — version display wired to git tag
- **Auth improvements** — `X-API-Token` for Cline/Roo compatibility, Cline-style streaming
- **Bundle with sidecar agent** — `docker-compose.cppworker-bundled-with-agent.yml` for production

Details: see [CHANGELOG.md](../../CHANGELOG.md).

## Table of Contents

1. [Deployment scenarios](#1-deployment-scenarios)
2. [Bundled stack (cppworker-gpu + balancer + webui)](#2-bundled-stack-cppworker-gpu--balancer--webui)
3. [Production configuration](#3-production-configuration)
4. [Scaling](#4-scaling)
5. [Healthcheck and observability](#5-healthcheck-and-observability)
6. [Port table](#6-port-table)

---

## 1. Deployment scenarios

### 1.1 Minimal (balancer + WebUI only)

```bash
cd deployments
docker compose up -d --build
```

Brings up:
- `loadbalancer` (18080, 18081)
- `webui` (18083)

Backends (Ollama) are added manually via the WebUI or the API.

### 1.2 Bundled (cppworker-gpu + balancer + webui)

The most common production scenario. See §2 for details.

```bash
./scripts/start-bundled.sh  # Linux/macOS/WSL
# or
.\scripts\start-bundled.ps1  # Windows PowerShell
```

### 1.3 CppWorker standalone

```bash
# GPU
cd deployments
docker compose -f docker-compose.cppworker.yml --profile gpu up -d --build

# CPU
docker compose -f docker-compose.cppworker.yml --profile cpu up -d --build

# Stub (for tests without llama.cpp)
docker compose -f docker-compose.cppworker.yml --profile stub up -d --build
```

### 1.4 Agent on a separate Ollama server

```bash
# Create .env
cp config/agent.example.env deployments/.env
# edit .env (BALANCER_URL, AGENT_PUBLIC_HOST, GPU_MODE)

cd deployments
docker compose -f docker-compose.agent.yml --env-file .env up -d --build
```

More details: [`agent-deployment.md`](../operations/agent-deployment.md).

### 1.5 Mixed cluster

```bash
# Balancer + WebUI + CppWorker (GPU) + Agent
docker compose \
  -f deployments/docker-compose.yml \
  -f deployments/docker-compose.cppworker.yml --profile gpu \
  -f deployments/docker-compose.agent.yml --env-file .env \
  up -d --build
```

More details: [`backend-type-isolation.md` §9](../architecture/backend-type-isolation.md#9-deployment-options).

---

## 2. Bundled stack (cppworker-gpu + balancer + webui)

### 2.1 Architecture

```
┌─────────────────────────────────────────────────────┐
│                  Docker Compose                     │
│                                                      │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────┐ │
│  │ loadbalancer │  │   webui      │  │cppworker- │ │
│  │ :18080       │◀─┤  :18083      │  │gpu        │ │
│  │ :18081       │  └──────────────┘  │ :18092    │ │
│  └──────▲───────┘                   └─────▲─────┘ │
│         │                                 │       │
│         │       ┌──────────────┐          │       │
│         └───────┤  agent (sidecar) ├──────┘       │
│                 │  :18032       │                  │
│                 └──────────────┘                  │
└─────────────────────────────────────────────────────┘
```

Network: `ollama-legion-net` (external), `cppworker-net` (internal).

### 2.2 Preparing `.env.bundled`

```bash
cd deployments
cp .env.bundled.example .env.bundled
# Edit .env.bundled:
```

| ENV | Purpose | Default |
|---|---|---|
| `CPPWORKER_API_TOKEN` | token for auto-registration | required |
| `CUDA_ARCH` | GPU architecture (86 for RTX 3070/3080/3090, 89 for 4090) | 86 |
| `CPPWORKER_GPU_LAYERS` | -1 (all), 0 (CPU), N | -1 |
| `CPPWORKER_RAM_FALLBACK_N_CTX` | reload into RAM on OOM | true | CLI flag `--ram-fallback-n-ctx` takes priority (R60.18 F4) |
| `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` | upper bound | 128000 | CLI flag `--ram-fallback-max-n-ctx` takes priority (R60.18 F4) |
| `LB_NCTX_RELOAD_ENABLED` | auto-reload on the balancer | true |
| `BALANCER_URL` | for cppworker auto-registration | http://loadbalancer:18081 |
| `CPPWORKER_ADVERTISE_HOST` | DNS name for registration | cppworker-gpu |
| `CPPWORKER_PORT` (internal) | cppworker port | 18091 |
| `CPPWORKER_ADVERTISED_PORT` | port for the balancer | 18091 |

### 2.3 Starting

```bash
# Linux/macOS/WSL
./scripts/start-bundled.sh rebuild    # rebuild + start

# Windows PowerShell (Legacy builder is mandatory!)
$env:DOCKER_BUILDKIT=0
.\scripts\start-bundled.ps1 -Rebuild

# Stop
./scripts/start-bundled.sh down

# Logs
./scripts/start-bundled.sh logs
```

### 2.4 Post-start verification

```bash
# 1. Balancer health (liveness)
curl http://localhost:18081/api/v1/ping
# Expected: 200 OK

# 2. CppWorker health (direct)
curl http://localhost:18092/health
# Expected: {"status":"ok"}

# 3. List of backends (via the API)
curl -H 'X-API-Token: <CPPWORKER_API_TOKEN>' http://localhost:18081/api/v1/backends
# Expected: contains cppworker-gpu-bundled

# 4. Test generation
curl -X POST http://localhost:18081/api/generate \
  -H "Content-Type: application/json" \
  -H "X-API-Token: <token>" \
  -d '{"model":"<model.gguf>","prompt":"hello","stream":false}'

# 5. WebUI Dashboard
open http://localhost:18083
```

### 2.5 CppWorker auto-registration

On startup cppworker registers itself in the balancer automatically:

```
POST http://loadbalancer:18081/api/v1/backends
{
  "id": "cppworker-gpu",
  "host": "cppworker-gpu",  // DNS inside compose
  "cppWorkerPort": 18091,
  "type": "llama_cpp"
}
```

**Important:**
- `CPPWORKER_ADVERTISE_HOST` — the DNS name under which the balancer can reach cppworker.
- In the bundled compose the default is `cppworker-gpu` (the service name in compose).
- To register **by IP** (outside the Docker network): set `BALANCER_URL=http://<host-ip>:18081` and `CPPWORKER_ADVERTISE_HOST=<host-ip>`.

### 2.6 Healthcheck in the bundled stack

- Balancer healthcheck: `GET /api/v1/ping` (always 200).
- CppWorker healthcheck: `/app/cppworker -healthcheck` (the binary itself reads `CPPWORKER_PORT` and does `GET /health` on localhost).

In `deployments/docker-compose.cppworker-bundled.yml`:
```yaml
healthcheck:
  test: ["CMD", "/app/cppworker", "-healthcheck"]
```

---

## 3. Production configuration

### 3.1 `config/config.json`

The base template is `config/config.example.json`. Key sections:

```json
{
  "loadBalancer": {
    "host": "0.0.0.0",
    "port": 18080,
    "apiPort": 18081,
    "tlsPort": 8443,
    "tlsApiPort": 8444
  },
  "balancing": {
    "algorithm": "resource-aware",
    "modelAffinity": true,
    "sessionStickiness": true,
    "useEnhancedScoring": true,
    "prewarm": { "enabled": true },
    "autoPull": { "enabled": true },
    "nctxReload": {
      "autoReloadNCtx": true,
      "maxNCtx": 131072,
      "vramSafetyFactor": 0.85,
      "timeoutSec": 120
    },
    "requestTimeout": 600,
    "queueTimeout": 300,
    "queueMaxSize": 100
  },
  "operatingMode": "standard",
  "defaultModelProfile": {
    "contextLength": 0,    // ⚠️ must be 0 or null
    "batchSize": 512,
    "numGpuLayers": -1,
    "flashAttn": true
  },
  "llamaCppModelProfiles": {
    // per-model overrides (see cppworker-model-params.md)
  }
}
```

More details: [`configuration.md`](configuration.md) — will be rewritten soon.

### 3.2 ENV variables

Every `config.json` setting can be overridden via ENV with the `LB_` prefix or by its direct name:

```bash
LB_HOST=0.0.0.0
LB_PORT=18080
LB_API_PORT=18081
LB_ALGORITHM=resource-aware
LB_MODEL_AFFINITY=true
LB_SESSION_STICKINESS=true
LB_HEALTH_CHECK_INTERVAL=10
LB_METRICS_INTERVAL=5
LB_REQUEST_TIMEOUT=600
LB_QUEUE_TIMEOUT=300
LB_QUEUE_MAX_SIZE=100
LB_GPU_MAX_USAGE=90
LB_GPU_MAX_VRAM=85
LB_GPU_MAX_TEMP=85
LB_CPU_MAX_USAGE=80
LB_MEMORY_MAX_USAGE=85
LB_LOG_LEVEL=info
LB_LOG_FORMAT=json
```

### 3.3 Backend via ENV

```bash
BACKEND_0_ID=gpu-1
BACKEND_0_HOST=192.0.2.66
BACKEND_0_PORT=11434
BACKEND_0_AGENT_PORT=18032
BACKEND_0_TYPE=ollama   # or llama_cpp
```

### 3.4 Authentication

```bash
AUTH_ENABLED=true
AUTH_TOKENS=master-token,client-token-1,client-token-2
AUTH_HEADER_NAME=X-API-Token
```

### 3.5 TLS

```bash
TLS_ENABLED=true
TLS_CERT_FILE=/etc/ssl/certs/server.crt
TLS_KEY_FILE=/etc/ssl/private/server.key
TLS_AUTO_CERT=true   # self-signed for tests
```

---

## 4. Scaling

### 4.1 Horizontal (multiple backends)

Add backends via WebUI → Backends → Add, or via the API:

```bash
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -H "X-API-Token: <token>" \
  -d '{"id":"gpu-2","host":"192.0.2.70","ollamaPort":11434,"type":"ollama"}'
```

### 4.2 Replication (Variant A)

Enable in `config/config.json`:

```json
{
  "modelReplication": {
    "enabled": true,
    "groups": [
      {
        "modelName": "llama3.1-8b",
        "minInstances": 2,
        "maxInstances": 4,
        "targetBackends": ["gpu-1", "gpu-2", "gpu-3"],
        "idleUnloadSec": 300
      }
    ]
  }
}
```

More details: [`backend-type-isolation.md` §3](../architecture/backend-type-isolation.md).

### 4.3 Virtual Router (Variant C, skeleton)

```json
{
  "virtualModels": [
    {
      "name": "llama-mega",
      "description": "Pipeline: embed + middle + output",
      "slices": [
        { "id": "embed",   "modelName": "nomic-embed", "ordinal": 0, "targetBackends": ["gpu-1"] },
        { "id": "middle",  "modelName": "llama3-8b",   "ordinal": 1, "targetBackends": ["gpu-2"] },
        { "id": "output",  "modelName": "llama3-8b",   "ordinal": 2, "targetBackends": ["gpu-3"] }
      ],
      "coordination": { "mode": "sequential", "timeoutMs": 60000 }
    }
  ]
}
```

> **Status:** the skeleton is implemented (`internal/virtualmodel/`), but pipeline execution is not finished. See [`audit-2026-06.md` KL-7](../../.cline/audit-2026-06.md#3-известные-ограничения-known-limitations).

### 4.4 RPC Coordinator (Variant B)

Skeleton in `internal/rpccoordinator/`. Requires separate worker instances with HTTP `/rpc/*` endpoints. **Not in the current release.**

---

## 5. Healthcheck and observability

### 5.1 Liveness vs Readiness

| Endpoint | Purpose | Docker healthcheck |
|---|---|---|
| `GET /api/v1/ping` | liveness (always 200 if the process is alive) | yes |
| `GET /api/v1/health` | readiness (503 in degraded) | no (otherwise restart loop) |

> **Important:** in the Docker `healthcheck` use `/api/v1/ping`, **not** `/api/v1/health`. The latter may return 503 before the backends have registered, which will cause a restart loop.

### 5.2 WebSocket metrics

`/ws/metrics` — real-time streaming:

```javascript
const ws = new WebSocket("ws://localhost:18081/ws/metrics?token=...");
ws.onmessage = (e) => {
  const metrics = JSON.parse(e.data);
  console.log(metrics);
};
```

### 5.3 Prometheus-style metrics

The balancer exports JSON snapshots via `GET /api/v1/metrics`:

```bash
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/metrics
```

Contains: `ActiveRequests`, `FreeSlots`, `CalculatedRPS`, `QueueStats`, histograms (`modelLoadTime`, `queueWaitTime`).

---

## 6. Port table

| Port | Component | Description |
|---|---|---|
| **18080** | Load Balancer | Ollama/OpenAI API Proxy (external) |
| **18081** | Load Balancer | Management API + WebSocket |
| **18083** | Web UI | Dashboard |
| **18032** | Agent | Local agent metrics |
| **11434** | Ollama | Ollama API (on backends) |
| **18091** | CppWorker (internal) | Ollama-compat + OpenAI API |
| **18092** | CppWorker (bundled host port) | mapping to 18091 inside the container |
| **8443** | Load Balancer | HTTPS Proxy (TLS) |
| **8444** | Load Balancer | HTTPS Management API (TLSPort+1) |

---

## 7. Related documents

- [`installation.md`](installation.md) — installation and build.
- [`agent-deployment.md`](../operations/agent-deployment.md) — agent deployment.
- [`backend-type-isolation.md`](../architecture/backend-type-isolation.md) — deployment options with different backend types.
- [`cppworker-model-params.md`](../features/cppworker-model-params.md) — n_ctx + Per-Model Profiles.
- [`audit-2026-06.md`](../../.cline/audit-2026-06.md) — implementation status.
- [`../.clinerules`](../../.clinerules) §15 — bundled stack instructions.
