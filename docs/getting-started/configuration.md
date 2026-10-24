# Ollama Load Balancer Configuration

A complete guide to configuring all components of the system.

## Table of Contents

1. [Balancer configuration](#balancer-configuration)
2. [Agent configuration](#agent-configuration)
3. [Web UI configuration](#web-ui-configuration)
4. [TLS/SSL setup](#tlsssl-setup)
5. [Authentication and Rate Limiting](#authentication-and-rate-limiting)
6. [Operating modes](#operating-modes)
7. [Model Replication (Variant A)](#model-replication-variant-a)
8. [RPC Coordinator (Variant B)](#rpc-coordinator-variant-b)
9. [Virtual Model Router (Variant C)](#virtual-model-router-variant-c)

---

## Balancer configuration

The balancer can be configured in two ways:
- Through a JSON configuration file
- Through environment variables

### JSON configuration

Example configuration file [`config.example.json`](../../config/config.example.json):

```json
{
  "loadBalancer": {
    "host": "0.0.0.0",
    "port": 18080,
    "apiPort": 18081,
    "tlsHost": "",
    "tlsPort": 8443
  },
  "backends": [
    {
      "id": "gpu-1",
      "name": "GPU Server 1",
      "host": "192.0.2.66",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 1,
      "maxConcurrentRequests": 10,
      "labels": ["nvidia", "rtx4090"]
    },
    {
      "id": "gpu-2",
      "name": "GPU Server 2",
      "host": "192.0.2.70",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 1,
      "maxConcurrentRequests": 10,
      "labels": ["nvidia", "rtx4090"]
    }
  ],
  "balancing": {
    "algorithm": "resource-aware",
    "modelAffinity": true,
    "sessionStickiness": true,
    "healthCheckInterval": 10,
    "metricsInterval": 5,
    "requestTimeout": 120,
    "queueTimeout": 300,
    "queueMaxSize": 100
  },
  "resources": {
    "gpu": {
      "maxUsagePercent": 90,
      "maxVramUsagePercent": 85,
      "maxTemperature": 85
    },
    "cpu": {
      "maxUsagePercent": 80
    },
    "memory": {
      "maxUsagePercent": 85
    },
    "disk": {
      "minFreeMB": 10240
    }
  },
  "logging": {
    "level": "info",
    "format": "json"
  },
  "api": {
    "rateLimit": 100,
    "rateBurst": 200
  },
  "tls": {
    "enabled": false,
    "certFile": "certs/server.crt",
    "keyFile": "certs/server.key",
    "minVersion": "TLS12",
    "autoCert": true
  },
  "auth": {
    "enabled": false,
    "tokens": [
      "your-master-token-here-change-in-production"
    ],
    "headerName": "X-API-Token"
  }
}
```

### Balancer configuration parameters

#### LoadBalancer settings

| Parameter | Type | Default | Description |
|----------|-----|--------------|----------|
| `host` | string | `0.0.0.0` | Host to listen for HTTP requests |
| `port` | int | `18080` | Port for proxying the Ollama API |
| `apiPort` | int | `18081` | Management API port |
| `tlsHost` | string | `""` | Host for HTTPS (optional) |
| `tlsPort` | int | `8443` | Port for the HTTPS proxy |
| `tlsApiPort` | int | `tlsPort+1` | Port for the HTTPS Management API (computed as `tlsPort + 1`) |

#### Backend settings

| Parameter | Type | Default | Description |
|----------|-----|--------------|----------|
| `id` | string | - | **Required**. Unique backend identifier |
| `name` | string | - | Human-readable backend name |
| `host` | string | - | **Required**. IP or hostname of the GPU server |
| `ollamaPort` | int | `11434` | Ollama API port on the backend |
| `agentPort` | int | `18032` | Agent port for metrics |
| `weight` | int | `1` | Backend weight for balancing |
| `maxConcurrentRequests` | int | `10` | Max. number of concurrent requests |
| `labels` | array | `[]` | Labels for classifying backends |

#### Balancing settings

| Parameter | Type | Default | Description |
|----------|-----|--------------|----------|
| `algorithm` | string | `resource-aware` | Balancing algorithm |
| `modelAffinity` | bool | `true` | Enable model affinity |
| `sessionStickiness` | bool | `true` | Enable session affinity |
| `useEnhancedScoring` | bool | `true` | Use enhanced scoring with weights |
| `modelLoadTimeout` | int | `120` | Model load timeout on the backend (sec) |
| `healthCheckInterval` | int | `10` | Health check interval (sec) |
| `metricsInterval` | int | `5` | Metrics refresh interval (sec) |
| `requestTimeout` | int | `120` | Request timeout (sec) |
| `queueTimeout` | int | `300` | Queue wait timeout (sec) |
| `queueMaxSize` | int | `100` | Max. queue size |

#### Resource limits

| Parameter | Type | Default | Description |
|----------|-----|--------------|----------|
| `gpu.maxUsagePercent` | float | `90` | Max. GPU utilization (%) |
| `gpu.maxVramUsagePercent` | float | `85` | Max. VRAM usage (%) |
| `gpu.maxTemperature` | int | `85` | Max. GPU temperature (°C) |
| `cpu.maxUsagePercent` | float | `80` | Max. CPU utilization (%) |
| `memory.maxUsagePercent` | float | `85` | Max. RAM usage (%) |
| `disk.minFreeMB` | uint | `10240` | Min. free disk space (MB) |

#### Logging settings

| Parameter | Type | Default | Description |
|----------|-----|--------------|----------|
| `level` | string | `info` | Logging level (debug, info, warn, error) |
| `format` | string | `json` | Log format (json, text) |

### Balancer environment variables

An alternative to the JSON configuration is to use environment variables:

```bash
# Core settings
LB_HOST=0.0.0.0
LB_PORT=18080
LB_API_PORT=18081
LB_TLS_HOST=
LB_TLS_PORT=8443

# Balancing algorithm
LB_ALGORITHM=resource-aware
LB_MODEL_AFFINITY=true
LB_SESSION_STICKINESS=true

# Intervals (seconds)
LB_HEALTH_CHECK_INTERVAL=10
LB_METRICS_INTERVAL=5

# Timeouts (seconds)
LB_REQUEST_TIMEOUT=120
LB_QUEUE_TIMEOUT=300
LB_QUEUE_MAX_SIZE=100
LB_QUEUE_WORKERS=4

# Resource limits
LB_GPU_MAX_USAGE=90
LB_GPU_MAX_VRAM=85
LB_GPU_MAX_TEMP=85
LB_CPU_MAX_USAGE=80
LB_MEMORY_MAX_USAGE=85
LB_DISK_MIN_FREE_MB=10240

# Logging
LB_LOG_LEVEL=info
LB_LOG_FORMAT=json

# API Rate Limiting
API_RATE_LIMIT=100
API_RATE_BURST=200
```

### Configuring backends via environment variables

```bash
# Backend 1
BACKEND_0_ID=gpu-1
BACKEND_0_NAME=GPU Server 1
BACKEND_0_HOST=192.0.2.66
BACKEND_0_PORT=11434
BACKEND_0_AGENT_PORT=18032
BACKEND_0_WEIGHT=1
BACKEND_0_MAX_REQS=10

# Backend 2
BACKEND_1_ID=gpu-2
BACKEND_1_NAME=GPU Server 2
BACKEND_1_HOST=192.0.2.70
BACKEND_1_PORT=11434
BACKEND_1_AGENT_PORT=18032
BACKEND_1_WEIGHT=1
BACKEND_1_MAX_REQS=10
```

---

## Agent configuration

The agent is configured exclusively through environment variables.

### Required variables

| Variable | Description | Example |
|------------|----------|--------|
| `AGENT_ID` | Unique agent identifier | `gpu-1`, `server-a100` |
| `BALANCER_URL` | Balancer URL | `http://192.0.2.100:18081` |

### Optional variables

| Variable | Default | Description |
|------------|--------------|----------|
| `AGENT_PORT` | `18032` | Port for local agent metrics |
| `OLLAMA_URL` | `http://localhost:11434` | Local Ollama URL |
| `NVML_ENABLED` | `true` | Enable NVML support (required for GPU metrics) |
| `COLLECT_INTERVAL` | `5` | Metrics collection interval (seconds). `METRICS_INTERVAL` is also accepted (deprecated, for compatibility) |
| `HEARTBEAT_INTERVAL` | `3` | Heartbeat interval (seconds) |
| `LOG_LEVEL` | `info` | Logging level (debug, info, warn, error) |
| `LOG_FORMAT` | `text` | Log format (text, json) |
| `CONFIG_PATH` | - | Path to the configuration file |
| `CONNECTION_TIMEOUT` | `30` | Connection timeout to the balancer (sec) |
| `OLLAMA_TIMEOUT` | `120` | Ollama API request timeout (sec) |

### Example .env file for the agent

```bash
# ============================================
# Ollama Load Balancer - Agent Configuration
# ============================================

# Required parameters
AGENT_ID=gpu-1
BALANCER_URL=http://192.0.2.100:18081

# Port settings
AGENT_PORT=18032
OLLAMA_URL=http://localhost:11434

# NVML settings
NVML_ENABLED=true

# Intervals
COLLECT_INTERVAL=5
HEARTBEAT_INTERVAL=3

# Logging
LOG_LEVEL=info
LOG_FORMAT=text
```

### Starting the agent with a .env file

```bash
# Docker Compose
docker-compose -f docker-compose.agent.yml --env-file .env up -d

# Docker run
docker run --env-file .env ollama-legion/agent:latest

# Binary file
set -a; source .env; set +a; ./bin/agent
```

---

## Web UI configuration

Web UI uses nginx to serve static files and proxy API requests.

### nginx configuration

File [`docker/webui/nginx.conf`](../../webui/nginx.conf):

```nginx
server {
    listen 80;
    server_name localhost;
    root /usr/share/nginx/html;
    index index.html;

    # Static files
    location / {
        try_files $uri $uri/ /index.html;
    }

    # API proxy to balancer
    location /api/ {
        proxy_pass http://loadbalancer:18081;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection 'upgrade';
        proxy_set_header Host $host;
        proxy_cache_bypass $http_upgrade;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }

    # WebSocket proxy to balancer
    location /ws/ {
        proxy_pass http://loadbalancer:18081;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "Upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

### Web UI environment variables

| Variable | Default | Description |
|------------|--------------|----------|
| `WEBUI_PORT` | `18030` | Port for the Web UI |

---

## TLS/SSL setup

The balancer supports HTTPS for a secure connection with clients.

### Enabling TLS

#### Via JSON configuration

```json
{
  "tls": {
    "enabled": true,
    "certFile": "certs/server.crt",
    "keyFile": "certs/server.key",
    "minVersion": "TLS12",
    "autoCert": true
  }
}
```

#### Via environment variables

```bash
TLS_ENABLED=true
TLS_CERT_FILE=certs/server.crt
TLS_KEY_FILE=certs/server.key
TLS_MIN_VERSION=TLS12
TLS_AUTO_CERT=true
LB_TLS_PORT=8443
```

### TLS parameters

| Parameter | Type | Default | Description |
|----------|-----|--------------|----------|
| `enabled` | bool | `false` | Enable TLS/SSL |
| `certFile` | string | `certs/server.crt` | Path to the SSL certificate |
| `keyFile` | string | `certs/server.key` | Path to the SSL key |
| `minVersion` | string | `TLS12` | Minimum TLS version (TLS12, TLS13) |
| `autoCert` | bool | `false` | Automatic generation of a self-signed certificate |

When TLS is enabled, the proxy is available on `tlsPort` (8443), and the HTTPS Management API on `tlsPort+1` (8444).

### Generating a self-signed certificate

```bash
# Create the directory for certificates
mkdir -p certs

# Generate a private key and certificate
openssl req -x509 -nodes -days 365 -newkey rsa:4096 \
  -keyout certs/server.key \
  -out certs/server.crt \
  -subj "/C=US/ST=State/L=City/O=Organization/CN=localhost"

# Generate with SAN (Subject Alternative Names)
openssl req -x509 -nodes -days 365 -newkey rsa:4096 \
  -keyout certs/server.key \
  -out certs/server.crt \
  -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,DNS:ollama-legion,IP:127.0.0.1"
```

### Using with Docker Compose

```yaml
services:
  loadbalancer:
    image: ollama-legion/balancer:latest
    ports:
      - "18080:18080"
      - "8443:8443"
    volumes:
      - ./certs:/app/certs:ro
      - ./config.json:/app/config.json:ro
    environment:
      - TLS_ENABLED=true
      - TLS_CERT_FILE=certs/server.crt
      - TLS_KEY_FILE=certs/server.key
      - TLS_AUTO_CERT=false
```

### Connecting over HTTPS

```bash
# cURL with a self-signed certificate
curl -k https://localhost:8443/api/v1/health

# cURL with a trusted certificate
curl --cacert certs/server.crt https://localhost:8443/api/v1/health

# Python requests
import requests
response = requests.get('https://localhost:8443/api/v1/health', verify=False)
```

---

## Authentication and Rate Limiting

### Authentication

The balancer supports **token authentication** with the ability to disable it completely. This is convenient for development, internal networks and Docker environments.

#### How it works

| Mode | `auth.enabled` | Behavior | When to use |
|-------|----------------|-----------|-------------------|
| **Disabled** | `false` | All endpoints are public. WebSocket works without a token. | Development, internal network, Docker Compose |
| **Enabled** | `true` | Protected endpoints require a token. WebSocket — via `?token=`. | Production, public access |

> **💡 Recommendation:** For production, always enable authentication. For local development or internal networks it can be disabled.

#### Enabling/disabling authentication

**JSON configuration (`config.json`):**

```json
{
  "auth": {
    "enabled": false,
    "tokens": [],
    "headerName": "X-API-Token"
  }
}
```

**For production:**

```json
{
  "auth": {
    "enabled": true,
    "tokens": [
      "secure-master-token-at-least-32-chars-long",
      "client-api-token-for-dashboard"
    ],
    "headerName": "X-API-Token"
  }
}
```

**Environment variables:**

```bash
# Disable auth (default)
AUTH_ENABLED=false

# Enable auth
AUTH_ENABLED=true
AUTH_TOKENS=master-token,client-token-1,client-token-2
AUTH_HEADER_NAME=X-API-Token
```

#### Authentication parameters

| Parameter | Type | Default | Description |
|----------|-----|--------------|----------|
| `enabled` | bool | `false` | Enable/disable authentication |
| `tokens` | array | `[]` | List of valid API tokens |
| `headerName` | string | `X-API-Token` | Name of the HTTP header for the token |

#### Master Token

The first token in the list is considered the **master token**. The master token has extended privileges:

- ✅ Create new tokens via `POST /api/v1/auth/token`
- ✅ Revoke other tokens via `DELETE /api/v1/auth/token`
- ❌ Cannot revoke itself via the API

> **🔒 Security:** Store the master token in a secure vault. Do not commit it to the repository.

#### WebSocket and authentication

The browser **does not support** setting arbitrary HTTP headers when creating a WebSocket. That is why the token is passed via a **query parameter**:

```javascript
// Correct — token in the URL
const ws = new WebSocket('ws://localhost:18081/ws/metrics?token=your-api-token');

// Incorrect — headers are ignored
const ws = new WebSocket('ws://...', null, { headers: { 'X-API-Token': '...' } }); // ❌
```

On the server side:
- If `auth.enabled: false` → the WebSocket upgrade is performed **without token validation**
- If `auth.enabled: true` → `?token=...` is required; an empty token → HTTP 401

#### How WebUI determines the auth settings

WebUI uses the **public** endpoint `GET /api/v1/health` for auto-detection:

```json
{
  "status": "healthy",
  "authEnabled": true,
  "authHeader": "X-API-Token",
  "wsEndpoint": "/ws/metrics"
}
```

WebUI flow:
1. Page load → `GET /api/v1/health` (without a token)
2. If `authEnabled: false` → the application starts immediately
3. If `authEnabled: true` → the saved token in `localStorage` is checked
4. If the token is valid → it starts; if not — a modal input window is shown

#### Public endpoints (no token required)

| Endpoint | Description |
|----------|----------|
| `GET /api/v1/health` | Health check + auth metadata |
| `GET /api/v1/ratelimit/status` | Rate limiter status |

All other endpoints require a token when `auth.enabled: true`. The full endpoint matrix is in the [API documentation](../api/api.md#endpoint--authentication-matrix).

#### Managing tokens via the API

```bash
# Check authentication status (any valid token)
curl -H "X-API-Token: your-token" \
  http://localhost:18081/api/v1/auth/status

# Generate a new token (master token only)
curl -X POST -H "X-API-Token: your-master-token" \
  http://localhost:18081/api/v1/auth/token

# Revoke a token (master token only)
curl -X DELETE \
  -H "X-API-Token: your-master-token" \
  "http://localhost:18081/api/v1/auth/token?token=token-to-revoke"
```

#### Generating a token manually

A token can be generated via the API or with a utility:

```bash
# Via the API (requires a master token)
curl -X POST -H "X-API-Token: your-master-token" \
  http://localhost:18081/api/v1/auth/token

# Via openssl (locally)
openssl rand -hex 32
# Result: a1b2c3d4e5f6... (64 hex characters)
```

> **📏 Token requirements:** Minimum 16 characters. 64 hex characters are recommended (32 bytes of entropy).

### Rate Limiting

Rate limiting protects the API from abuse.

#### Rate Limiting configuration

**JSON configuration:**

```json
{
  "api": {
    "rateLimit": 100,
    "rateBurst": 200
  }
}
```

**Environment variables:**

```bash
API_RATE_LIMIT=100
API_RATE_BURST=200
```

#### Rate Limiting parameters

| Parameter | Type | Default | Description |
|----------|-----|--------------|----------|
| `rateLimit` | float | `100` | Requests per second |
| `rateBurst` | float | `200` | Burst capacity (max. tokens) |

#### Checking Rate Limiter status

```bash
GET /api/v1/ratelimit/status

# Response:
{
  "tokens": 100.0,
  "max_tokens": 100,
  "refill_rate": 10.0
}
```

---

## Complete configuration example for production

```json
{
  "loadBalancer": {
    "host": "0.0.0.0",
    "port": 18080,
    "apiPort": 18081,
    "tlsPort": 8443
  },
  "backends": [
    {
      "id": "prod-gpu-1",
      "name": "Production GPU Server 1",
      "host": "192.0.2.10",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 2,
      "maxConcurrentRequests": 20,
      "labels": ["nvidia", "a100", "production"]
    },
    {
      "id": "prod-gpu-2",
      "name": "Production GPU Server 2",
      "host": "192.0.2.11",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 2,
      "maxConcurrentRequests": 20,
      "labels": ["nvidia", "a100", "production"]
    }
  ],
  "balancing": {
    "algorithm": "resource-aware",
    "modelAffinity": true,
    "sessionStickiness": true,
    "healthCheckInterval": 10,
    "metricsInterval": 5,
    "requestTimeout": 180,
    "queueTimeout": 600,
    "queueMaxSize": 200
  },
  "resources": {
    "gpu": {
      "maxUsagePercent": 85,
      "maxVramUsagePercent": 80,
      "maxTemperature": 80
    },
    "cpu": {
      "maxUsagePercent": 75
    },
    "memory": {
      "maxUsagePercent": 80
    },
    "disk": {
      "minFreeMB": 20480
    }
  },
  "logging": {
    "level": "info",
    "format": "json"
  },
  "api": {
    "rateLimit": 200,
    "rateBurst": 400
  },
  "tls": {
    "enabled": true,
    "certFile": "/etc/ssl/certs/ollama-legion.crt",
    "keyFile": "/etc/ssl/private/ollama-legion.key",
    "minVersion": "TLS13",
    "autoCert": false
  },
  "auth": {
    "enabled": true,
    "tokens": [
      "secure-master-token-change-in-production",
      "client-api-token-1",
      "client-api-token-2"
    ],
    "headerName": "X-API-Token"
  }
}
```

---

## Operating modes

The balancer supports several operating modes, switched via `PUT /api/v1/config/mode` or through the WebUI Setup Wizard.

### Configuration parameters

| Field | Type | Default | Description |
|------|-----|-------------|----------|
| `balancing.operating_mode` | string | `"auto"` | Operating mode: `auto`, `single_node`, `cluster_balancing` |
| `balancing.auto_detect_mode` | bool | `true` | Auto-detect the mode at startup |

### Modes

| Mode | Description |
|-------|----------|
| **`auto` (default)** | Automatically determines the mode: if there are ≥2 healthy backends — `cluster_balancing`, otherwise `single_node` |
| **`single_node`** | Direct proxying with balancing disabled |
| **`cluster_balancing`** | Full balancing with backend selection, queue and session stickiness |

### Environment override

```env
LB_OPERATING_MODE=cluster_balancing
LB_AUTO_DETECT_MODE=true
```

### Resetting the configuration to factory defaults

`POST /api/v1/config/reset` resets the configuration to the default values.

---

## Model Replication (Variant A)

Automatic replication of models across several backends.

| Field | Type | Default | Env variable | Description |
|------|-----|-------------|----------------|----------|
| `balancing.model_replication.enabled` | bool | `false` | `LB_MODEL_REPLICATION_ENABLED` | Enable replication |
| `balancing.model_replication.default_min_instances` | int | `1` | `LB_MODEL_REPLICATION_MIN_INSTANCES` | Min. replicas |
| `balancing.model_replication.default_max_instances` | int | `3` | `LB_MODEL_REPLICATION_MAX_INSTANCES` | Max. replicas |
| `balancing.model_replication.idle_unload_after` | duration | `"15m"` | `LB_MODEL_REPLICATION_IDLE_UNLOAD` | Idle time before unload |

---

## RPC Coordinator (Variant B)

Distributed inference through external RPC workers.

| Field | Type | Default | Env variable | Description |
|------|-----|-------------|----------------|----------|
| `balancing.rpc_coordinator.enabled` | bool | `false` | `LB_RPC_COORDINATOR_ENABLED` | Enable RPC coordination |
| `balancing.rpc_coordinator.coordinator_url` | string | `""` | `LB_RPC_COORDINATOR_URL` | Coordinator URL |
| `balancing.rpc_coordinator.worker_port` | int | `18050` | `LB_RPC_COORDINATOR_PORT` | RPC worker port |
| `balancing.rpc_coordinator.protocol` | string | `"http"` | `LB_RPC_COORDINATOR_PROTOCOL` | Protocol |

---

## Virtual Model Router (Variant C)

Virtual models as a pipeline of several physical models.

| Field | Type | Default | Env variable | Description |
|------|-----|-------------|----------------|----------|
| `balancing.virtual_models.enabled` | bool | `false` | `LB_VIRTUAL_MODELS_ENABLED` | Enable virtual models |

---

## Next steps

- [Deployment](deployment.md) — Docker Compose and production deployment
- [API documentation](../api/api.md) — Using the REST API and WebSocket
- [Troubleshooting](../operations/troubleshooting.md) — Solving problems

---

## Round 56-59 (2026-09-03): New ENV vars and endpoints

### `LB_STREAMING_NEVER_TIMEOUT` (R58.1)

Disables ALL streaming timeouts (idle, first-byte, request, total).
The balancer does NOT cut streaming connections on timeout — it waits for a cancel
from the client (r.Context().Done()) or EOF from the backend. Useful for
long-running inference (Qwen3.6-35B prefill 10+ min) or when the client
manages cancel itself via ctx.

```bash
# Enable
LB_STREAMING_NEVER_TIMEOUT=1
# Supports: "1", "true", "yes" (case-insensitive)
# "0" / unset = defaults (120s idle, 600s total, etc.)
```

Default values are preserved (for existing deployments — bit-identical).
Opt-in via a single ENV toggle.

### `LB_NCTX_RELOAD_*` (Round 34-37, 2026-08-12)

Adaptive auto-reload of `n_ctx` on `prompt_too_long` (code 3) /
`n_ctx_needed` (code 2) from cppworker.

| ENV | Default | Purpose |
|---|---|---|
| `LB_NCTX_RELOAD_ENABLED` | `true` | Enable adaptive reload |
| `LB_NCTX_RELOAD_MAX_N_CTX` | `131072` | Max n_ctx for reload (128K) |
| `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR` | `0.95` | VRAM safety margin (0.85 = rejected Gemma-4, 0.95 = OK) |
| `LB_NCTX_RELOAD_TIMEOUT_SEC` | `300` | Reload operation timeout |

See `deployments/.env.bundled-with-agent.example:44-50`.

### `LB_NCTX_PREFLIGHT_*` (Round 35, 2026-08-12)

Preflight check of `n_ctx` BEFORE a synchronous model load. Without
`LB_NCTX_PREFLIGHT_ENABLED=true` the preflight short-circuits → Cline 65K →
413 sync instead of 503+Retry-After (infinite retry loop).

| ENV | Default | Purpose |
|---|---|---|
| `LB_NCTX_PREFLIGHT_ENABLED` | `true` | Master switch |
| `LB_NCTX_PREFLIGHT_ASYNC_RELOAD` | `true` | Async instead of blocking reload |
| `LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC` | `30` | `Retry-After` header in 503 |
| `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` | `900` (15 min) | Max polling time for loading. **22GB Qwen3.6 on a 3070** requires `1800` (30 min) — see Round 35c |
| `LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER` | `2` | cppworker estimate multiplier |
| `LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC` | `60` | Additional buffer |

See `deployments/.env.bundled-with-agent.example:54-74`.

### `CPPWORKER_RAM_FALLBACK_*` (Round 38, 2026-08-17)

3-tier cascade for models that do not fit into VRAM: full VRAM → partial
offload → CPU-only. With `8GB VRAM` (RTX 3070) most large models do not
fit — partial offload or CPU-only is mandatory.

| ENV | Default | Purpose |
|---|---|---|
| `CPPWORKER_RAM_FALLBACK_N_CTX` | `true` | Enable the cascade |
| `CPPWORKER_RAM_FALLBACK_GPU_LAYERS` | `-2` (auto offload) | Layers during RAM fallback |
| `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` | `64000` (8GB) / `128000` (16-24GB) | Max n_ctx in CPU-only mode |

See `config/hardware-presets/*.json` for presets.

### `Backend.ApiStyle` (R56, EffectiveAPIStyle)

New field `Backend.ApiStyle` (`ollama-native` / `openai-compatible`).
Previously routing used `Backend.Type` directly, which blocked
operators from overriding the API style (for example, a cppworker with the native Ollama API
still went through the OpenAI-compat path).

EffectiveAPIStyle (R56) — helper:
- If `ApiStyle` is set explicitly → it is used
- Otherwise it is derived from `Type` (llama_cpp → openai-compatible, otherwise ollama-native)

Routing uses EffectiveAPIStyle() instead of isLlamaCppBackend().
Backward compat: for existing deployments without ApiStyle = bit-identical.

### Cluster AutoDistribute (R59, R59.1)

New endpoints for automatic distribution of models across backends:

```bash
# R59: get suggestions (read-only)
GET /api/v1/admin/cluster/autosuggest
→ 200 OK {
    "timestamp": "...",
    "backends": [...],
    "suggestions": [
      {"id": "sug-1", "type": "move", "fromBackend": "b1",
       "toBackend": "b2", "model": "gemma-4-9b", "priority": 1, ...}
    ],
    "summary": {"totalBackends": 3, "overloadedCount": 1, ...}
  }

# R59.1: apply suggestions (operator-confirmed)
POST /api/v1/admin/cluster/autosuggest/apply
Body: {"suggestion_ids": ["sug-1", "sug-3"]}
→ 200 OK {
    "applied": 1,
    "failed": 1,
    "details": [
      {"suggestionId": "sug-1", "status": "applied", ...},
      {"suggestionId": "sug-99", "status": "failed", "error": "unknown suggestion id"}
    ]
  }
```

Algorithm:
- Identifies overloaded backends (busyScore > 0.8 OR modelCount > 0.9)
- Finds underloaded backends (busyScore < 0.3 AND modelCount < 0.5)
- Suggests moving loaded models from overloaded to underloaded backends of the same Type
- Cross-type moves are forbidden (ollama ↔ llama_cpp breaks the load format)
- Max 3 suggestions per source (do not flood the operator)

Validation (R59.1):
- Re-computes suggestions from the current state (does not trust the client)
- Verifies that suggestion_id exists
- Verifies that source/dest are healthy
- Verifies cross-type
- Verifies that the model is loaded on the source

### Hardware Presets (R58.2)

`config/hardware-presets/{rtx30-8gb,rtx40-24gb,a10-24gb,rtx50-32gb}.json` —
4 ready-made presets for different GPUs. How to apply:

```bash
# List presets
python scripts/apply-hardware-preset.py --list

# Apply (prints KEY=VALUE for copy-paste)
python scripts/apply-hardware-preset.py rtx40-24gb

# Setup Wizard UI (R58.3): Settings → Setup Wizard → step 4
# dropdown "Hardware Preset" → choose RTX 4090 / A10 / RTX 5090
# → auto-fill vramMaxUsage/gpuMaxUsage + hint with CPPWORKER_* env
```

More details: [docs/getting-started/hardware-presets.ru.md](hardware-presets.md).

### Parallel LlamaCollector (R58)

`internal/agent/llama_collector.go::Collect()` — 3 sequential HTTP calls
to cppworker (/api/gpu, /api/models, /api/info) were reworked into parallel
calls via `sync.WaitGroup`. ~3x speedup (603ms → 210ms on typical
endpoints). Closes the user pain: "the agent waits for the cppworker main thread".

### Setup Wizard Hardware Preset Dropdown (R58.3)

WebUI: Settings → Setup Wizard → Step 4 (General Settings) →
a new "Hardware Preset" dropdown. When a preset is selected (RTX 4090 / A10 /
RTX 5090), 4 fields are auto-filled (vramMaxUsage / gpuMaxUsage / cpuMaxUsage
/ ramMaxUsage) and a hint with CPPWORKER_* env vars is shown.
