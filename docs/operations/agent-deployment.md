# Deploying the Ollama Load Balancer Agent

A detailed guide to deploying the metrics collection agent on servers running Ollama. The agent supports two operating modes: **GPU** (with an NVIDIA GPU and NVML) and **CPU** (no GPU, system metrics only).

> **Cross-platform:** Command examples are given for **bash** (Linux/macOS) and **PowerShell** (Windows). Choose the variant for your OS.

---

## Table of Contents

1. [System requirements](#system-requirements)
2. [Agent operating modes](#agent-operating-modes)
3. [Deployment in GPU mode](#deployment-in-gpu-mode)
4. [Deployment in CPU mode](#deployment-in-cpu-mode)
5. [Environment variables](#environment-variables)
6. [Configuration examples](#configuration-examples)
7. [Health checks](#health-checks)
8. [Generating Docker containers](#generating-docker-containers)
9. [Troubleshooting](#troubleshooting)

---

## System requirements

### General requirements (for any mode)

| Requirement | Minimum | Recommended |
|-------------|---------|-------------|
| **OS** | Linux (Ubuntu 20.04+, Debian 11+, CentOS 8+) | Ubuntu 22.04 LTS, Debian 12 |
| **CPU** | 1 core | 2 cores |
| **RAM** | 128 MB | 256 MB |
| **Disk** | 50 MB | 100 MB |
| **Docker** | 20.10+ | 24.0+ |
| **Ollama** | Installed and running on port `11434` | Latest stable version |
| **Network** | HTTP/HTTPS access to the balancer | 1 Gbps |

### Additional requirements for GPU mode

| Requirement | Minimum | Recommended |
|-------------|---------|-------------|
| **NVIDIA GPU** | Compute Capability 5.0+ | Compute Capability 7.0+ (Turing/Ampere) |
| **NVIDIA Driver** | 470.x | 535.x or newer |
| **NVIDIA Container Toolkit** | 1.12+ | 1.14+ |
| **CUDA** | 11.0+ | 12.0+ |

> **Important:** Without the NVIDIA Container Toolkit, GPU passthrough into Docker does not work. The agent will automatically switch to CPU mode, but accurate GPU metrics will be unavailable.

### Windows: Docker Desktop + WSL2 + GPU

To run GPU containers on Windows through Docker Desktop:

1. **Docker Desktop** → Settings → General → ✅ Use the WSL 2 based engine
2. **Docker Desktop** → Settings → Resources → WSL Integration → ✅ Enable integration with my default WSL distro
3. **Docker Desktop** → Settings → Resources → WSL Integration → ✅ Enable NVIDIA Container Toolkit (appears if an NVIDIA driver with WSL2 support is installed)
4. **NVIDIA Driver** for WSL2: https://developer.nvidia.com/cuda/wsl (install on the Windows host, NOT inside WSL)

Verifying the GPU in Docker Desktop:
```powershell
docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi
```

If the `nvidia/cuda:12.2.0-base-ubuntu22.04` image is not available locally, Docker will pull it automatically — this may take several minutes the first time.

---

## Agent operating modes

The agent determines its operating mode through the `GPU_MODE` variable:

| Mode | Description | When to use |
|------|-------------|-------------|
| **`auto`** | Automatically detects GPU and NVML availability | Default; recommended for mixed clusters |
| **`gpu`** | Forces GPU mode, requires NVML | Servers with an NVIDIA GPU where accurate GPU metrics are critical |
| **`cpu`** | Forces CPU mode, GPU metrics disabled | Servers without a GPU, virtual machines, CPU-only inference |

> **Tip:** For production clusters with heterogeneous hardware, use `auto`. For stability in CI/CD or on purely CPU servers, specify `cpu` explicitly.

---

## Deployment in GPU mode

### Step 1: Verify requirements

```bash
# Check Docker
docker --version

# Check NVIDIA Driver
nvidia-smi

# Check NVIDIA Container Toolkit
docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi

# Check Ollama
curl http://localhost:11434/api/tags
```

If the `docker run --rm --gpus all ...` command fails, see the [Installing the NVIDIA Container Toolkit](#installing-the-nvidia-container-toolkit) section below.

### Step 2: Create the .env file

**Linux/macOS (bash):**
```bash
# From the repository root
cp config/agent.example.env deployments/.env

# Edit deployments/.env — replace the placeholder values
code deployments/.env
```

**Windows (PowerShell):**
```powershell
# From the repository root
Copy-Item "config\agent.example.env" "deployments\.env"

# Edit deployments/.env — replace the placeholder values
notepad "deployments\.env"
```

Key variables for GPU:
```env
AGENT_ID=gpu-1
BALANCER_URL=http://192.0.2.10:18081
AGENT_PUBLIC_HOST=192.0.2.20
GPU_MODE=gpu
NVML_ENABLED=true
ENABLE_NVML=true
OLLAMA_URL=http://host.docker.internal:11434
```

### Step 3: Start via Docker Compose (GPU)

Use the **override file** `docker-compose.agent.gpu.yml` — it automatically wires in the NVIDIA Container Toolkit and the CUDA runtime:

**Linux/macOS (bash):**
```bash
cd deployments

# Start with GPU passthrough
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml --env-file .env up -d --build

# View logs
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml logs -f

# Check status
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml ps
```

**Windows (PowerShell):**
```powershell
Set-Location deployments

# Start with GPU passthrough
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml --env-file .env up -d --build

# View logs
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml logs -f

# Check status
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml ps
```

> **Windows + GPU:** Docker Desktop must use the WSL2 backend with the "Use the WSL 2 based engine" and "Enable NVIDIA Container Toolkit" options enabled under Settings > Resources > WSL Integration.

> **Why two files?** The base `docker-compose.agent.yml` holds the common configuration. The override file `docker-compose.agent.gpu.yml` adds only GPU-specific settings (`deploy.resources.reservations.devices` and `BASE_IMAGE=nvidia/cuda...`). This lets the same base file run on CPU and GPU without modifications.

### Step 4: Alternative — automation script (recommended)

**Linux/macOS (bash):**
```bash
# Auto-detect GPU and start with the correct parameters
bash scripts/deploy-agent-docker.sh --env-file deployments/.env

# Force GPU (if auto-detection failed)
bash scripts/deploy-agent-docker.sh --gpu --env-file deployments/.env

# Restart only, without rebuilding
bash scripts/deploy-agent-docker.sh --no-build --env-file deployments/.env
```

**Windows (PowerShell):**
```powershell
# Auto-detect GPU and start with the correct parameters
.\scripts\deploy-agent-docker.ps1 -EnvFile "deployments\.env"

# Force GPU
.\scripts\deploy-agent-docker.ps1 -Gpu -EnvFile "deployments\.env"

# Restart only, without rebuilding
.\scripts\deploy-agent-docker.ps1 -NoBuild -EnvFile "deployments\.env"

# With pulling of base images
.\scripts\deploy-agent-docker.ps1 -Pull -Build -EnvFile "deployments\.env"
```

The script automatically:
- Detects GPU availability via `nvidia-smi`
- Checks the NVIDIA Container Toolkit
- Selects the correct compose file (CPU or GPU)
- Validates the required variables in `.env`

### Step 5: Alternative — docker run

```bash
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  --gpus all \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://192.0.2.10:18081 \
  -e AGENT_PUBLIC_HOST=192.0.2.20 \
  -e AGENT_PORT=18032 \
  -e OLLAMA_URL=http://192.0.2.20:11434 \
  -e GPU_MODE=gpu \
  -e NVML_ENABLED=true \
  -e COLLECT_INTERVAL=5 \
  -e HEARTBEAT_INTERVAL=3s \
  -p 18032:18032 \
  ollama-legion/agent:latest
```

### Step 6: Alternative — systemd service

See the [deployment guide](../getting-started/deployment.md) → section "Deploying the agent as a binary file" for instructions on building and creating a systemd unit.

---

## Deployment in CPU mode

### Step 1: Verify requirements

```bash
# Check Docker
docker --version

# Check Ollama
curl http://localhost:11434/api/tags
```

### Step 2: Create the .env file

**Linux/macOS (bash):**
```bash
# From the repository root
cp config/agent.example.env deployments/.env

# Edit deployments/.env
code deployments/.env
```

**Windows (PowerShell):**
```powershell
Copy-Item "config\agent.example.env" "deployments\.env"
notepad "deployments\.env"
```

Key variables for CPU:
```env
AGENT_ID=cpu-1
BALANCER_URL=http://192.0.2.10:18081
AGENT_PUBLIC_HOST=192.0.2.30
GPU_MODE=cpu
NVML_ENABLED=false
ENABLE_NVML=false
OLLAMA_URL=http://host.docker.internal:11434
```

### Step 3: Start via Docker Compose (CPU)

The base `docker-compose.agent.yml` file contains no GPU-specific settings — start it without the override:

**Linux/macOS (bash):**
```bash
cd deployments

docker compose -f docker-compose.agent.yml --env-file .env up -d --build

# View logs
docker compose -f docker-compose.agent.yml logs -f
```

**Windows (PowerShell):**
```powershell
Set-Location deployments

docker compose -f docker-compose.agent.yml --env-file .env up -d --build

# View logs
docker compose -f docker-compose.agent.yml logs -f
```

> **Important:** For CPU mode do not use `docker-compose.agent.gpu.yml` — it will try to initialize the NVIDIA Container Toolkit and fail with `nvidia-container-cli: driver not loaded` on a machine without a GPU.

### Step 4: Alternative — automation script

**Linux/macOS (bash):**
```bash
# Auto-detection (on a CPU server it starts in CPU mode)
bash scripts/deploy-agent-docker.sh --env-file deployments/.env

# Force CPU
bash scripts/deploy-agent-docker.sh --cpu --env-file deployments/.env
```

**Windows (PowerShell):**
```powershell
# Auto-detection (on a CPU server it starts in CPU mode)
.\scripts\deploy-agent-docker.ps1 -EnvFile "deployments\.env"

# Force CPU
.\scripts\deploy-agent-docker.ps1 -Cpu -EnvFile "deployments\.env"
```

### Step 5: Alternative — docker run

```bash
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  -e AGENT_ID=cpu-1 \
  -e BALANCER_URL=http://192.0.2.10:18081 \
  -e AGENT_PUBLIC_HOST=192.0.2.30 \
  -e AGENT_PORT=18032 \
  -e OLLAMA_URL=http://192.0.2.30:11434 \
  -e GPU_MODE=cpu \
  -e NVML_ENABLED=false \
  -e COLLECT_INTERVAL=5 \
  -e HEARTBEAT_INTERVAL=3s \
  -p 18032:18032 \
  ollama-legion/agent:latest
```

### Step 6: Alternative — systemd service

For CPU servers the agent build is simpler — NVML is not required:

```bash
# Build without NVML
CGO_ENABLED=0 go build -o bin/agent ./cmd/agent

# Copy and create the systemd unit
# ... (see the deployment guide)
```

---

## Environment variables

### Required

| Variable | Description | Example |
|----------|-------------|---------|
| `AGENT_ID` | Unique identifier of the agent in the cluster | `gpu-1`, `cpu-1`, `node-01` |
| `BALANCER_URL` | Balancer URL for registration and heartbeat | `http://192.0.2.10:18081` |

### Network settings

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_PORT` | `18032` | Port for the agent's local metrics |
| `AGENT_PUBLIC_HOST` | `localhost` | Public IP/hostname reachable by the balancer |
| `OLLAMA_URL` | `http://localhost:11434` | URL of the local Ollama for collecting model metrics |

### Operating mode

| Variable | Default | Description |
|----------|---------|-------------|
| `GPU_MODE` | `auto` | `auto` / `gpu` / `cpu` — metrics collection mode |
| `NVML_ENABLED` | `false` | `true` — enable NVML (GPU mode only) |

> **Compatibility:** If `GPU_MODE=cpu`, the `NVML_ENABLED` variable is ignored. If `GPU_MODE=gpu` and `NVML_ENABLED=false`, the agent will try to use NVML, but it will not crash on an initialization error — it will switch to fallback methods.

### Intervals and timeouts

| Variable | Default | Description |
|----------|---------|-------------|
| `COLLECT_INTERVAL` | `5` | Metrics collection interval (seconds). `METRICS_INTERVAL` is also accepted (deprecated, kept for compatibility) |
| `HEARTBEAT_INTERVAL` | `3` | Interval of heartbeat signals to the balancer (seconds) |

### Logging

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `text` | `text` or `json` (json is recommended for production) |

---

## Configuration examples

### GPU server (production)

```bash
AGENT_ID=gpu-server-prod-01
BALANCER_URL=http://192.0.2.100:18081
AGENT_PUBLIC_HOST=192.0.2.101

GPU_MODE=gpu
NVML_ENABLED=true

AGENT_PORT=18032
OLLAMA_URL=http://host.docker.internal:11434

METRICS_INTERVAL=5s
HEARTBEAT_INTERVAL=3s

LOG_LEVEL=info
LOG_FORMAT=json
```

### CPU server (production)

```bash
AGENT_ID=cpu-server-prod-01
BALANCER_URL=http://192.0.2.100:18081
AGENT_PUBLIC_HOST=192.0.2.102

GPU_MODE=cpu
NVML_ENABLED=false

AGENT_PORT=18032
OLLAMA_URL=http://host.docker.internal:11434

METRICS_INTERVAL=5s
HEARTBEAT_INTERVAL=3s

LOG_LEVEL=info
LOG_FORMAT=json
```

### Mixed cluster (auto mode)

```bash
AGENT_ID=auto-node-01
BALANCER_URL=http://lb.example.com:18081
AGENT_PUBLIC_HOST=node-01.example.com

GPU_MODE=auto
NVML_ENABLED=false  # auto will decide whether to enable NVML

AGENT_PORT=18032
OLLAMA_URL=http://host.docker.internal:11434

METRICS_INTERVAL=5s
HEARTBEAT_INTERVAL=3s

LOG_LEVEL=info
LOG_FORMAT=text
```

---

## Health checks

### 1. Local check of the agent metrics

```bash
curl http://localhost:18032/metrics
```

Expected response for **GPU** mode:

```json
{
  "agent_id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": {
    "usagePercent": 15,
    "memoryTotal": 24576,
    "memoryUsed": 4096,
    "temperature": 45,
    "powerUsage": 120,
    "powerLimit": 450
  },
  "system": {
    "cpuUsagePercent": 25,
    "memoryTotal": 65536,
    "memoryUsed": 8192
  },
  "ollama": {
    "runningModels": ["llama3.1:8b"],
    "activeRequests": 2,
    "requestsPerSecond": 0.5
  }
}
```

Expected response for **CPU** mode:

```json
{
  "agent_id": "cpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": null,
  "system": {
    "cpuUsagePercent": 35,
    "memoryTotal": 32768,
    "memoryUsed": 12288
  },
  "ollama": {
    "runningModels": ["llama3.1:8b"],
    "activeRequests": 1,
    "requestsPerSecond": 0.2
  }
}
```

### 2. Checking through the balancer API

```bash
# Cluster status (should show the registered agent)
curl http://<balancer-ip>:18081/api/v1/cluster

# List of backends
curl http://<balancer-ip>:18081/api/v1/backends
```

### 3. Checking the agent logs

```bash
# Docker
docker logs -f ollama-agent

# Docker Compose
docker-compose -f docker-compose.agent.yml logs -f
```

In GPU mode the logs should contain the following lines:

```
INFO  nvml initialized successfully
INFO  gpu metrics collector started
```

In CPU mode the logs should contain:

```
INFO  running in CPU mode, gpu metrics disabled
INFO  system metrics collector started
```

### 4. Verifying GPU passthrough (GPU only)

```bash
# Inside the agent container
docker exec ollama-agent nvidia-smi

# Or directly on the host
nvidia-smi
```

---

## Generating Docker containers

### Building the agent image

**Linux/macOS (bash):**
```bash
# CPU build (faster, smaller size)
docker build -f docker/agent/Dockerfile \
  --build-arg ENABLE_NVML=false \
  --build-arg BASE_IMAGE=ubuntu:22.04 \
  -t ollama-legion/agent:cpu-latest .

# GPU build (with NVML + CUDA runtime)
docker build -f docker/agent/Dockerfile \
  --build-arg ENABLE_NVML=true \
  --build-arg BASE_IMAGE=nvidia/cuda:12.2.0-runtime-ubuntu22.04 \
  -t ollama-legion/agent:gpu-latest .
```

**Windows (PowerShell):**
```powershell
# CPU build
docker build -f docker\agent\Dockerfile `
  --build-arg ENABLE_NVML=false `
  --build-arg BASE_IMAGE=ubuntu:22.04 `
  -t ollama-legion/agent:cpu-latest .

# GPU build
docker build -f docker\agent\Dockerfile `
  --build-arg ENABLE_NVML=true `
  --build-arg BASE_IMAGE=nvidia/cuda:12.2.0-runtime-ubuntu22.04 `
  -t ollama-legion/agent:gpu-latest .
```

### Building via Docker Compose

**Linux/macOS (bash):**
```bash
cd deployments

# CPU — base file
docker compose -f docker-compose.agent.yml build

# GPU — with the override
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml build
```

**Windows (PowerShell):**
```powershell
Set-Location deployments

# CPU — base file
docker compose -f docker-compose.agent.yml build

# GPU — with the override
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml build
```

### Push to registry

```bash
# Tag and push
docker tag ollama-legion/agent:latest registry.example.com/ollama-legion/agent:latest
docker push registry.example.com/ollama-legion/agent:latest
```

### Generating multi-arch images (amd64 + arm64)

**Linux/macOS (bash):**
```bash
# Create the builder (once)
docker buildx create --name ollama-legion-builder --use

# Build and push multi-arch
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -f docker/agent/Dockerfile \
  --build-arg ENABLE_NVML=false \
  -t registry.example.com/ollama-legion/agent:latest \
  --push .
```

**Windows (PowerShell):**
```powershell
# Create the builder (once)
docker buildx create --name ollama-legion-builder --use

# Build and push multi-arch
docker buildx build `
  --platform linux/amd64,linux/arm64 `
  -f docker\agent\Dockerfile `
  --build-arg ENABLE_NVML=false `
  -t registry.example.com/ollama-legion/agent:latest `
  --push .
```

---

## Troubleshooting

### Error: base name (${BASE_IMAGE}) should not be blank

**Cause:** Docker BuildKit does not see `BASE_IMAGE` as a build-arg when building through docker-compose.

**Solution:** The sample Dockerfile has already been fixed in `docker/agent/Dockerfile` — `ARG BASE_IMAGE` is declared after `FROM`. Make sure you are using `docker compose` (v2) instead of `docker-compose` (v1):

```bash
# Correct (v2)
docker compose -f docker-compose.agent.yml build

# Incorrect (v1, may have problems with build args)
docker-compose -f docker-compose.agent.yml build
```

### Error: nvidia-container-cli: driver not loaded

**Cause:** `docker-compose.agent.gpu.yml` with `deploy.resources.reservations.devices` was used on a CPU server.

**Solution:** For CPU servers do not use the GPU override:

```bash
# Correct (CPU)
docker compose -f docker-compose.agent.yml up -d

# Incorrect (CPU with the GPU override)
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml up -d
```

### Agent cannot initialize NVML (GPU mode)

**Symptoms:** The logs contain the error `nvml initialization failed`, GPU metrics are missing.

**Solutions:**

1. Check the NVIDIA Container Toolkit:
   ```bash
   docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi
   ```

2. Make sure both compose files are started:
   ```bash
   docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml up -d --build
   ```

3. Check the variables in `.env`:
   ```env
   ENABLE_NVML=true
   BASE_IMAGE=nvidia/cuda:12.2.0-runtime-ubuntu22.04
   ```

4. If GPU passthrough is impossible (for example, a cloud VPS without a GPU), switch to CPU mode:
   ```bash
   GPU_MODE=cpu
   NVML_ENABLED=false
   ENABLE_NVML=false
   ```

5. Recreate the container:
   ```bash
   docker compose -f docker-compose.agent.yml down
   docker compose -f docker-compose.agent.yml up -d --build
   ```

### Agent cannot connect to the balancer

**Symptoms:** The logs contain connection errors to `BALANCER_URL`.

**Solutions:**

1. Check that the balancer is reachable:
   ```bash
   curl -v http://<balancer-ip>:18081/api/v1/health
   ```

2. Make sure that `BALANCER_URL` contains the **public IP** of the balancer, not `localhost` or a Docker service name (if the agent and the balancer are on different hosts).

3. Check the firewall:
   ```bash
   sudo ufw status
   sudo iptables -L -n | grep 18081
   ```

### The balancer does not see the agent metrics

**Symptoms:** The agent is registered, but the metrics are empty or stale.

**Solutions:**

1. Check `AGENT_PUBLIC_HOST` — the balancer contacts the agent at this address:
   ```bash
   curl http://<AGENT_PUBLIC_HOST>:18032/metrics
   ```

2. Make sure the `AGENT_PORT` port is published and reachable from the balancer host.

3. Check the Docker network mode:
   - `bridge` — requires `ports:` and `AGENT_PUBLIC_HOST` = host IP
   - `host` — requires `network_mode: host`, the public IP of the server

### A CPU server shows zero or empty GPU metrics

**Cause:** The `GPU_MODE` variable is not set to `cpu`, so the agent tries to collect GPU metrics that do not exist.

**Solution:** Specify explicitly:

```bash
GPU_MODE=cpu
NVML_ENABLED=false
```

After changing `.env`, recreate the container:

```bash
docker-compose -f docker-compose.agent.yml down
docker-compose -f docker-compose.agent.yml up -d
```

---

## Installing the NVIDIA Container Toolkit

If `docker run --rm --gpus all ...` does not work:

### Ubuntu / Debian

```bash
# Add the repository
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | \
  sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit.gpg

distribution=$(. /etc/os-release;echo $ID$VERSION_ID)
curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
  sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit.gpg] https://#g' | \
  sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list

# Install
sudo apt-get update
sudo apt-get install -y nvidia-container-toolkit

# Configure the Docker runtime
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker

# Verify
docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi
```

### CentOS / RHEL / Fedora

```bash
# Add the repository
curl -s -L https://nvidia.github.io/libnvidia-container/stable/rpm/nvidia-container-toolkit.repo | \
  sudo tee /etc/yum.repos.d/nvidia-container-toolkit.repo

# Install
sudo yum install -y nvidia-container-toolkit

# Configure the Docker runtime
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
```

---

## Additional resources

- [deployment guide](../getting-started/deployment.md) — General guide to deploying all components
- [docs/getting-started/deployment.md](../getting-started/deployment.md) — Docker Compose, production deployment, scaling
- [docs/getting-started/configuration.md](../getting-started/configuration.md) — Configuring the balancer and the agent
- [docs/operations/troubleshooting.md](troubleshooting.md) — General troubleshooting guide
