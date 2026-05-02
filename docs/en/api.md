# OllamaLegion API Reference

Complete REST API and WebSocket endpoints documentation.

## Table of Contents

1. [API Overview](#api-overview)
2. [Authentication](#authentication)
3. [REST API Endpoints](#rest-api-endpoints)
4. [WebSocket API](#websocket-api)
5. [Request Examples](#request-examples)
6. [OpenAPI Specification](#openapi-specification)

---

## API Overview

The load balancer provides a REST API for cluster management and metrics retrieval.

### Base URL

```
# Management API (cluster management, metrics, sessions)
http://localhost:18081

# HTTPS (if TLS is enabled)
https://localhost:8443
```

### Ollama API Proxy

The balancer proxies standard Ollama API endpoints on port `18080`. All Ollama requests pass through the balancer with session stickiness, model affinity, queue management, and retry/failover.

**Base URL:**
```
http://localhost:18080
```

| Endpoint | Method | Description | Proxy Features |
|----------|--------|-------------|----------------|
| `/api/tags` | GET | List available models | Aggregated from all backends |
| `/api/version` | GET | Ollama version | Aggregated from all backends |
| `/api/ps` | GET | Running processes | Aggregated per backend |
| `/api/show` | POST | Model details | Routed to backend with model |
| `/api/generate` | POST | Text generation | Session stickiness, queue, retry |
| `/api/chat` | POST | Chat completion | Session stickiness, queue, retry |
| `/api/create` | POST | Create model | Routed to selected backend |
| `/api/pull` | POST | Pull model | Routed to selected backend |
| `/api/push` | POST | Push model | Routed to selected backend |
| `/api/delete` | DELETE | Delete model | Broadcast to all backends |
| `/api/copy` | POST | Copy model | Routed to selected backend |

---

## Authentication

The API uses token-based authentication via the `X-API-Token` header.

```bash
# With token
curl -H "X-API-Token: your-token-here" http://localhost:18081/api/v1/cluster

# Without token (if auth disabled)
curl http://localhost:18081/api/v1/cluster
```

### Token Management

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/auth/status` | GET | Authentication status |
| `/api/v1/auth/token` | POST | Generate new token (requires master token) |
| `/api/v1/auth/revoke` | POST | Revoke a token (requires master token) |

---

## REST API Endpoints

### Health Check

```http
GET /api/v1/health
```

**Response:**
```json
{
  "status": "ok",
  "timestamp": "2025-01-01T00:00:00Z",
  "uptime": "2h30m15s",
  "backends": 3,
  "healthyBackends": 2
}
```

### Cluster State

```http
GET /api/v1/cluster
```

Returns comprehensive cluster state including all backends, their metrics, queue status, and active sessions.

**Response:**
```json
{
  "backends": [...],
  "queue": { "current_size": 5, "max_size": 100 },
  "rps": 12.5,
  "totalRequests": 1500
}
```

### Backends Management

```http
GET    /api/v1/backends        # List all backends
POST   /api/v1/backends        # Add a new backend
GET    /api/v1/backends/:id    # Get backend details
PUT    /api/v1/backends/:id    # Update backend config
DELETE /api/v1/backends/:id    # Remove a backend
```

**Add Backend Request:**
```json
{
  "id": "gpu-server-2",
  "name": "GPU Server 2",
  "host": "192.168.1.101",
  "ollamaPort": 11434,
  "agentPort": 9090,
  "weight": 100,
  "maxConcurrentRequests": 10,
  "labels": ["gpu", "a100"]
}
```

### Model Management

```http
GET /api/v1/models      # List all models across all backends
```

### Sessions

```http
GET    /api/v1/sessions           # List all active sessions
GET    /api/v1/sessions/:id       # Get session details
DELETE /api/v1/sessions/:id       # Force-close a session
DELETE /api/v1/sessions           # Close all sessions
```

### Queue Management

```http
GET  /api/v1/queue          # Queue statistics
GET  /api/v1/queue/details  # Detailed queue state with items
POST /api/v1/queue/rebalance # Force rebalance of pending requests
```

### Agent Endpoints

```http
POST /api/v1/agents/register     # Register an agent
POST /api/v1/agents/:id/metrics  # Submit metrics
POST /api/v1/agents/:id/heartbeat # Heartbeat signal
GET  /api/v1/agents/:id/stats    # Agent statistics
```

### Metrics

```http
GET /api/v1/metrics       # Prometheus metrics
GET /api/v1/metrics/stream # SSE stream of real-time metrics
```

### Backend Capacity

```http
GET /api/v1/capacity           # Cluster-wide capacity overview
GET /api/v1/capacity/:id       # Per-backend capacity details
GET /api/v1/capacity/stream    # SSE stream of capacity metrics
```

---

## WebSocket API

### Real-time Metrics Stream

```
ws://localhost:18081/api/v1/ws/metrics
```

Streams real-time metrics updates as JSON messages:

```json
{
  "type": "metrics_update",
  "timestamp": "2025-01-01T00:00:00Z",
  "data": {
    "backendId": "gpu-1",
    "gpu": { "usagePercent": 45 },
    "vram": { "usagePercent": 62 },
    "activeRequests": 2,
    "rps": 1.5
  }
}
```

---

## Request Examples

### cURL

```bash
# Get cluster state
curl http://localhost:18081/api/v1/cluster

# Add a backend
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -d '{"id":"gpu-3","host":"192.168.1.102","ollamaPort":11434}'

# Delete a backend
curl -X DELETE http://localhost:18081/api/v1/backends/gpu-3

# Force rebalance queue
curl -X POST http://localhost:18081/api/v1/queue/rebalance
```

### Python

```python
import requests

# Get cluster state
resp = requests.get("http://localhost:18081/api/v1/cluster")
data = resp.json()
print(f"Backends: {len(data['backends'])}")
print(f"Queue size: {data['queue']['current_size']}")

# Add a backend
resp = requests.post("http://localhost:18081/api/v1/backends", json={
    "id": "gpu-3",
    "host": "192.168.1.102",
    "ollamaPort": 11434
})
```

### JavaScript

```javascript
// Get cluster state
const resp = await fetch('http://localhost:18081/api/v1/cluster');
const data = await resp.json();
console.log(`Backends: ${data.backends.length}`);
console.log(`Queue size: ${data.queue.current_size}`);

// Add a backend
await fetch('http://localhost:18081/api/v1/backends', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({
    id: 'gpu-3',
    host: '192.168.1.102',
    ollamaPort: 11434
  })
});
```

---

## OpenAPI Specification

The full OpenAPI 3.0 specification is available at:

- **YAML:** [openapi.yaml](../openapi.yaml)
- **JSON:** [swagger.json](../swagger.json)

### Using Swagger UI

```bash
# Run Swagger UI locally
docker run -p 8080:8080 -v $(pwd)/docs/openapi.yaml:/openapi.yaml \
  -e SWAGGER_JSON=/openapi.yaml swaggerapi/swagger-ui