# OllamaLegion Installation Guide

Complete step-by-step installation guide for the Ollama load balancer.

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Quick Start (Docker Compose)](#quick-start-docker-compose)
3. [Manual Installation](#manual-installation)
4. [Agent Installation](#agent-installation)
5. [Post-Installation](#post-installation)

---

## Prerequisites

### System Requirements

| Component | Minimum | Recommended |
|-----------|---------|-------------|
| CPU | 2 cores | 4+ cores |
| RAM | 4 GB | 8+ GB |
| Disk | 10 GB free | 20+ GB SSD |
| Docker | 20.10+ | 24.0+ |
| Docker Compose | 2.0+ | 2.20+ |

### GPU Backend Requirements

- NVIDIA GPU with CUDA support
- NVIDIA drivers 525.60.13+
- NVIDIA Container Toolkit

```bash
# Install NVIDIA Container Toolkit (Ubuntu)
distribution=$(. /etc/os-release;echo $ID$VERSION_ID)
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -s -L https://nvidia.github.io/libnvidia-container/$distribution/libnvidia-container.list | \
  sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | \
  sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
sudo apt-get update
sudo apt-get install -y nvidia-container-toolkit
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
```

---

## Quick Start (Docker Compose)

### 1. Clone the Repository

```bash
git clone https://github.com/BarsSky/ollamalegion.git
cd ollamalegion
```

### 2. Configure Backends

Edit `config/backends.example.json` and save as `config/backends.json`:

```json
[
  {
    "id": "gpu-node-1",
    "name": "GPU Node 1",
    "host": "192.168.1.100",
    "ollama_port": 11434,
    "agent_port": 9090,
    "weight": 100,
    "max_concurrent_requests": 10,
    "labels": ["gpu", "production"]
  },
  {
    "id": "gpu-node-2",
    "name": "GPU Node 2",
    "host": "192.168.1.101",
    "ollama_port": 11434,
    "agent_port": 9090,
    "weight": 80,
    "max_concurrent_requests": 8
  }
]
```

### 3. Start Services

```bash
# Development environment
docker compose -f deployments/docker-compose.yml up -d

# With GPU support
docker compose -f deployments/docker-compose.yml -f deployments/docker-compose.agent.gpu.yml up -d
```

### 4. Verify Installation

```bash
# Check health
curl http://localhost:18081/api/v1/health

# Open WebUI
open http://localhost:18080
# or
xdg-open http://localhost:18080
```

---

## Manual Installation

### Build from Source

```bash
# Install Go 1.21+
wget https://go.dev/dl/go1.21.5.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.21.5.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin

# Build
cd cmd/balancer
go build -o ollama-balancer

# Run
./ollama-balancer --config config/config.json
```

### Systemd Service

```ini
# /etc/systemd/system/ollamalegion.service
[Unit]
Description=OllamaLegion Load Balancer
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
User=ollama
WorkingDirectory=/opt/ollamalegion
ExecStart=/usr/local/bin/ollama-balancer --config /etc/ollamalegion/config.json
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable ollamalegion
sudo systemctl start ollamalegion
```

---

## Agent Installation

The monitoring agent collects GPU, system, and Ollama metrics from each backend.

### Docker

```bash
docker run -d \
  --name ollama-agent \
  --network host \
  -e AGENT_ID=gpu-node-1 \
  -e BALANCER_URL=http://loadbalancer:18081 \
  -e OLLAMA_URL=http://localhost:11434 \
  -e AGENT_PORT=18032 \
  -e NVML_ENABLED=true \
  ollamalegion/agent:latest
```

### Using Deploy Script

```bash
# Linux/Mac
./scripts/deploy-agent-docker.sh \
  --balancer-url http://192.168.1.10:18081 \
  --agent-id gpu-node-1

# Windows PowerShell
.\scripts\deploy-agent-docker.ps1 `
  -BalancerUrl "http://192.168.1.10:18081" `
  -AgentId "gpu-node-1"
```

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `AGENT_ID` | Yes | — | Unique agent identifier |
| `BALANCER_URL` | Yes | — | Load balancer URL |
| `OLLAMA_URL` | No | `http://localhost:11434` | Local Ollama URL |
| `AGENT_PORT` | No | `18032` | Agent metrics port |
| `NVML_ENABLED` | No | `false` | Enable NVML GPU metrics |
| `METRICS_INTERVAL` | No | `5s` | Metrics collection interval |
| `HEARTBEAT_INTERVAL` | No | `3s` | Heartbeat interval |
| `LOG_LEVEL` | No | `info` | Logging level |
| `GPU_MODE` | No | `auto` | GPU mode: auto, gpu, cpu |

---

## Post-Installation

### Access Points

| Service | URL | Description |
|---------|-----|-------------|
| WebUI | `http://localhost:18080` | Main dashboard |
| Management API | `http://localhost:18081` | REST API |
| Prometheus Metrics | `http://localhost:18081/api/v1/metrics` | Prometheus endpoint |
| Agent Metrics | `http://backend:18032/metrics` | Per-backend metrics |

### Health Verification

```bash
# Cluster health
curl http://localhost:18081/api/v1/health | jq

# Backend status
curl http://localhost:18081/api/v1/backends | jq

# Model listing
curl http://localhost:18081/api/v1/models | jq

# Queue status
curl http://localhost:18081/api/v1/queue | jq

# Active sessions
curl http://localhost:18081/api/v1/sessions | jq
```

### Adding More Backends

Via API:
```bash
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -d '{
    "id": "gpu-node-3",
    "host": "192.168.1.102",
    "ollamaPort": 11434,
    "agentPort": 9090
  }'
```

Via WebUI: Navigate to Settings → Backends → Add Backend

### Troubleshooting

See [Troubleshooting Guide](../troubleshooting.md) for common issues.