# OllamaLegion Balancing Guide

Complete guide to load balancing strategies, session management, and queue handling.

## Table of Contents

1. [Balancing Algorithms](#balancing-algorithms)
2. [Session Stickiness](#session-stickiness)
3. [Model Affinity](#model-affinity)
4. [Queue Management](#queue-management)
5. [Resource Limits](#resource-limits)
6. [Failure Handling](#failure-handling)

---

## Balancing Algorithms

### Round Robin
Distributes requests evenly across all healthy backends in rotation.

```json
{
  "balancing": {
    "algorithm": "roundrobin"
  }
}
```

**Best for:** Homogeneous backends with similar hardware and identical model sets.

### Least Connections
Routes to the backend with the fewest active requests.

```json
{
  "balancing": {
    "algorithm": "leastconn"
  }
}
```

**Best for:** Mixed workloads with varying request durations.

### Resource-Aware (Default)
Routes based on weighted resource scoring considering GPU usage, VRAM, CPU, and active requests.

```json
{
  "balancing": {
    "algorithm": "resource-aware"
  }
}
```

**Scoring factors:**
- GPU utilization (weight: 0.35)
- VRAM usage (weight: 0.30)
- CPU usage (weight: 0.15)
- Active request count (weight: 0.20)

**Best for:** Heterogeneous clusters with varying GPU/CPU configurations.

### Model Affinity
Routes requests to backends that already have the requested model loaded in memory.

```json
{
  "balancing": {
    "algorithm": "model-affinity"
  }
}
```

**Best for:** Clusters where different backends serve different model sets.

---

## Session Stickiness

Sessions bind a client to a specific backend for the duration of a conversation or generation task.

### How It Works

1. Client sends first request with model name
2. Balancer selects optimal backend and creates session
3. Subsequent requests from same session route to same backend
4. Session expires after idle timeout

### Session Lifecycle

| Stage | Duration | Description |
|-------|----------|-------------|
| Active | During requests | Session is in use |
| Idle | Configurable | Waiting for next request |
| Expired | After idle timeout | Session removed |

### Configuration

```json
{
  "balancing": {
    "sessionTimeout": "30m",
    "maxSessionsPerBackend": 50,
    "sessionAffinityMode": "strict"
  }
}
```

| Parameter | Default | Description |
|-----------|---------|-------------|
| `sessionTimeout` | `30m` | Idle session lifetime |
| `maxSessionsPerBackend` | `50` | Max sessions per backend |
| `sessionAffinityMode` | `strict` | `strict` or `preferred` |

---

## Queue Management

When all suitable backends are at capacity, requests are placed in a queue.

### Queue Workflow

```
Client Request → Select Backend → Slot Available?
                                  ├─ Yes → Route to Backend
                                  └─ No → Queue → Backend Free → Dequeue → Route
```

### Configuration

```json
{
  "balancing": {
    "queueMaxSize": 100,
    "queueWorkers": 4,
    "queueTimeout": 60
  }
}
```

| Parameter | Default | Description |
|-----------|---------|-------------|
| `queueMaxSize` | `100` | Maximum queue depth |
| `queueWorkers` | `4` | Concurrent queue processors |
| `queueTimeout` | `60s` | Max wait time in queue |

### Force Rebalance

When requests are stuck in queue despite available capacity:

```bash
curl -X POST http://localhost:18081/api/v1/queue/rebalance
```

This redistributes pending requests across available backends, temporarily bypassing session affinity.

---

## Resource Limits

Limits prevent backend overload.

```json
{
  "resources": {
    "gpu": {
      "maxUsagePercent": 95,
      "maxVRAMUsagePercent": 95,
      "maxTemperature": 90
    },
    "cpu": {
      "maxUsagePercent": 95
    },
    "memory": {
      "maxUsagePercent": 95
    },
    "disk": {
      "minFreeMB": 1024
    }
  }
}
```

When a backend exceeds any limit:
1. New requests are not routed to it
2. Existing sessions continue to use it
3. Backend is marked as degraded
4. Health checks continue monitoring

---

## Failure Handling

### Retry Logic

| Scenario | Action |
|----------|--------|
| Backend unavailable | Retry on next healthy backend |
| Timeout | Retry with exponential backoff |
| Stream error | Reconnect to alternate backend |
| Queue timeout | Return 503 Service Unavailable |
| All backends down | Return 503 with wait estimate |

### Health Checks

Backends are continuously monitored:

```json
{
  "balancing": {
    "healthCheckInterval": 5,
    "healthCheckTimeout": 10,
    "maxConsecutiveFailures": 3
  }
}
```

**Backend states:**
- **Healthy:** Passing health checks, accepting requests
- **Degraded:** Resource limits exceeded, accepting existing sessions only
- **Unhealthy:** Failing health checks, not accepting requests
- **Offline:** Manually disabled or not responding

### Recovery

After `maxConsecutiveFailures` (default 3), backend is marked unhealthy. After 3 consecutive successful checks, it returns to healthy.

---

## Best Practices

1. **Resource-Aware for heterogeneous clusters:** Automatically balances based on real-time metrics
2. **Model Affinity for specialized backends:** Keeps models on dedicated hardware
3. **Queue limits prevent overload:** Set `queueMaxSize` based on cluster capacity
4. **Monitor queue depth:** Alert when `current_size > max_size * 0.8`
5. **Use agents on all backends:** Provides accurate real-time metrics for optimal routing
6. **Session timeout tuning:** Shorter for API usage, longer for interactive chat