# OllamaLegion Metrics

## Description

The agent collects **all available data** from the Ollama server and the host, forwards it to the balancer for decision-making and to the WebUI for visualization. The balancer additionally computes proxy metrics (more accurate than the agent's).

---

## 📊 Metrics sources

| Source | Data | Collector |
|----------|--------|--------------|
| **NVML / nvidia-smi** | GPU utilization, VRAM, temperature, frequency, TDP | Agent (GPU mode) |
| **OS (/proc, gopsutil)** | CPU, RAM, Disk, Network | Agent (CPU+GPU mode) |
| **Ollama API** (`/api/tags`) | Model list, sizes, details | Agent + fallback balancer |
| **Proxy counters** | ActiveRequests, RPS | **Balancer** (proxy-calculated) |
| **Predictor** | Trends, forecasts | **Balancer** |

---

## 1. GPU Metrics (`GPUMetrics`)

| Field | Type | Unit | Source | WebUI display | Description |
|------|-----|---------|----------|---------------|----------|
| `usagePercent` | float64 | % | NVML | ✅ | GPU load |
| `memoryTotal` | uint64 | MB | NVML | ✅ | Total VRAM |
| `memoryUsed` | uint64 | MB | NVML | ✅ | VRAM used |
| `memoryFree` | uint64 | MB | NVML | ✅ (computed) | VRAM free |
| `temperature` | int | °C | NVML | ✅ | GPU temperature |
| `powerUsage` | int | W | NVML | ✅ | Power consumption |
| `powerLimit` | int | W | NVML | ❌ | Power limit (TDP) |
| `gpuClock` | int | MHz | NVML | ❌ | GPU frequency |
| `memClock` | int | MHz | NVML | ❌ | Memory frequency |

> **Note:** `powerLimit`, `gpuClock`, `memClock` are collected by the agent but **not displayed in the WebUI**.

---

## 2. System Metrics (`SystemMetrics`)

| Field | Type | Unit | Source | WebUI display | Description |
|------|-----|---------|----------|---------------|----------|
| `cpuUsagePercent` | float64 | % | gopsutil | ✅ | Total CPU load |
| `memoryTotal` | uint64 | MB | gopsutil | ✅ | Total RAM |
| `memoryUsed` | uint64 | MB | gopsutil | ✅ | RAM used |
| `memoryFree` | uint64 | MB | gopsutil | ✅ (computed) | RAM free |
| `diskTotal` | uint64 | MB | gopsutil | ❌ | Total disk |
| `diskUsed` | uint64 | MB | gopsutil | ❌ | Disk used |
| `diskFree` | uint64 | MB | gopsutil | ❌ | Disk free |
| `networkRX` | uint64 | bytes | gopsutil | ❌ | Bytes received |
| `networkTX` | uint64 | bytes | gopsutil | ❌ | Bytes sent |

### 2.1 CPU Details (`SystemMetrics.CPU` / `CPUMetrics`)

| Field | Type | Unit | Source | WebUI display | Description |
|------|-----|---------|----------|---------------|----------|
| `usagePercent` | float64 | % | gopsutil | ✅ | Total CPU load |
| `usagePerCore` | []float64 | % | /proc/stat | ✅ | Per-core load |
| `coreCount` | int | count | /proc/cpuinfo | ✅ | Number of cores |
| `threadCount` | int | count | /proc/cpuinfo | ✅ | Number of threads |
| `model` | string | — | /proc/cpuinfo | ✅ | CPU model |
| `loadAverage1` | float64 | — | /proc/loadavg | ✅ | Load avg 1 min |
| `loadAverage5` | float64 | — | /proc/loadavg | ✅ | Load avg 5 min |
| `loadAverage15` | float64 | — | /proc/loadavg | ✅ | Load avg 15 min |
| `temperature` | int | °C | /sys/class/thermal | ✅ | CPU temperature |
| `throttled` | bool | — | /sys/devices/system/cpu | ✅ | CPU throttling |

---

## 3. Ollama Metrics (`OllamaMetrics`)

| Field | Type | Unit | Source | WebUI display | Description |
|------|-----|---------|----------|---------------|----------|
| `runningModels` | []RunningModel | — | Ollama API | ✅ | Running models |
| `availableModels` | []RunningModel | — | Ollama API | ❌ | Available models |
| `activeRequests` | int | count | **Proxy** | ✅ | Active requests |
| `totalRequests` | int64 | count | **Proxy** | ❌ | Total requests |
| `avgResponseTime` | float64 | ms | — | ❌ | Average response time |
| `requestsPerSecond` | float64 | RPS | **Proxy** | ✅ | Requests per second |
| `maxModels` | int | count | — | ❌ | Model limit |
| `maxConcurrentRequests` | int | count | — | ❌ | Request limit |
| `freeSlots` | int | count | **Proxy** | ✅ | Free slots (`MaxConcurrent - ActiveRequests`) |
| `availableSlots` | int | count | **Proxy** | ❌ | Available slots with headroom (reserved for future expansion) |

> **⚠️ Critically important:** `activeRequests`, `totalRequests`, `requestsPerSecond`, `freeSlots` are computed by the **balancer** (proxy-calculated), not by the agent. The agent sends `0` for these fields; the balancer overwrites them with accurate values.

### 3.1 Running Model (`RunningModel`)

| Field | Type | Unit | Source | WebUI display | Description |
|------|-----|---------|----------|---------------|----------|
| `name` | string | — | Ollama API | ✅ | Model name |
| `size` | uint64 | bytes | Ollama API | ❌ | Model size |
| `vramUsage` | uint64 | MB | heuristic | ✅ | VRAM estimate |
| `ramUsage` | uint64 | MB | heuristic | ✅ | RAM estimate (CPU) |
| `expiresAt` | time.Time | — | Ollama API | ✅ | Unload time |
| `digest` | string | — | Ollama API | ✅ | Model hash |
| `loadCount` | int | count | — | ❌ | Number of loads |
| `family` | string | — | Ollama API | ❌ | Family (llama, mistral...) |
| `format` | string | — | Ollama API | ❌ | Format (gguf...) |
| `parameterSize` | string | — | Ollama API | ❌ | Parameter size (7B, 70B...) |
| `quantization` | string | — | Ollama API | ❌ | Quantization (Q4_0...) |

> **Note:** The fields `family`, `format`, `parameterSize`, `quantization` are collected by the agent from `/api/ps` → `details` with `/api/tags` as fallback. They are exposed in `BackendMetrics.Ollama.RunningModels` and available in the WebUI.

---

## 4. Proxy-Calculated Metrics (Balancer)

| Field | Type | Unit | Algorithm | Description |
|------|-----|---------|----------|----------|
| `CalculatedRPS` | float64 | RPS | `len(RequestHistory) / 60.0` | 60-second sliding window |
| `ActiveRequests` | int | count | `ActiveReqs++ / --` | Exact HTTP counter |
| `FreeSlots` | int | count | `MaxConcurrent - ActiveRequests` | Free slots |
| `AvailableSlots` | int | count | `FreeSlots - headroom` | Available slots with headroom reservation |
| `QueueStats.current_size` | int | count | `len(queue)` | Current queue size |
| `QueueStats.processed_total` | int64 | count | counter | Total processed |
| `QueueStats.dispatch_by_affinity` | int64 | count | `atomic.AddInt64` | Requests via Model Affinity |
| `QueueStats.dispatch_by_load` | int64 | count | `atomic.AddInt64` | Requests via Resource-Aware |
| `QueueStats.dispatch_by_config` | int64 | count | `atomic.AddInt64` | Requests via Weight/Config |

---

> **Dispatch metrics** reflect which balancing mechanism was used to select the backend:
> 1. **Model Affinity** — the model is already loaded on the backend (stages 1–3 in `selectBackend`)
> 2. **Resource-Aware** — selection by free resources (GPU, VRAM, CPU) + prediction bonus
> 3. **Config/Weight** — fallback selection by backend weights (when `UseEnhancedScoring=false`)

### 4.1 Dispatch-by-Group Metrics (Model Replication)

Additional metrics that appear when model replication is enabled (Variant A):

| Field | Type | Unit | Description |
|------|-----|---------|----------|
| `dispatch_by_group` | int64 | count | Requests routed via group-based dispatch |
| `replication_group_count` | int | count | Number of active replication groups |
| `replicas_actual` | int | count | Actual number of replicas |
| `replicas_target` | int | count | Target number of replicas (min/max) |
| `replication_reconcile_count` | int64 | count | Number of reconcile cycles performed |

Group-based dispatch works on top of the main mechanisms (Model Affinity, Resource-Aware). If a model belongs to a replication group, the balancer takes the min/max replica count into account when selecting a backend.

---

## 5. Prediction Metrics (Balancer)

| Field | Type | Description |
|------|-----|----------|
| `secondsToCritical` | float64 | Seconds until critical state |
| `criticalReason` | string | Reason: gpu_usage, vram, ram, concurrent_requests, models_capacity, none |
| `gpuUsageTrend` | float64 | GPU load trend (%/min) |
| `vramUsageTrend` | float64 | VRAM trend (%/min) |
| `ramUsageTrend` | float64 | RAM trend (%/min) |
| `freeSlotsTrend` | float64 | Free slots trend |
| `requestCapacity` | float64 | Backend load (0-100%) |

### 5.1 Use of the predictor in balancing

**Filtering (5.1):** Backends predicted to reach a critical state **within 5 minutes** (`0 < secondsToCritical < 300`) are **excluded from the pool** for new requests. This applies to all backend selection paths:
- `selectByResources()` — Resource-Aware selection
- `findBackendWithModel()` — Model Affinity
- `findBackendWithModelExcluding()` — Model Affinity with exclusions

**Scoring (5.2):** In `calculateScore()` a `predictionBonus` is added:
- `secondsToCritical < 0` or `>= 600` → **+3.0** (backend stable for >10 min)
- `300 <= secondsToCritical < 600` → **+1.5** (stable for >5 min)
- `0 < secondsToCritical < 120` → **-5.0** (critical state in <2 min — penalty)

> **Note:** The fields `gpuUsageTrend`, `vramUsageTrend`, `ramUsageTrend`, `freeSlotsTrend` are computed and stored in `BackendState.Prediction`, but are **not directly used** for routing decisions. They are available via WebSocket/WebUI for visual analysis.

---


## 6. Summary table: Designed vs Implemented

| Component | Designed | Implemented | Gap |
|-----------|----------|-------------|-----|
| **Agent GPU** | All NVML fields | ✅ All 9 fields | None |
| **Agent CPU** | Per-core, load avg, temp, throttling | ✅ All fields | None |
| **Agent Disk** | Total/Used/Free | ✅ | None |
| **Agent Network** | RX/TX | ✅ | None |
| **Agent Ollama** | All model fields + details | ✅ family, format, parameterSize, quantization | **None** |
| **Agent modelSize** | Collect size from `/api/tags` | ✅ Size uint64 in RunningModel, ModelSizes map | None |
| **Balancer RPS** | 60s sliding window | ✅ | None |
| **Balancer Queue** | Stats API | ✅ | None |
| **Balancer Prediction** | Filtering + Scoring | ✅ | None |
| **Balancer Queue Dispatch** | dispatch_by_affinity/load/config counters | ✅ Implemented in backend_selector.go | Displayed in monitor.html Dispatch Stats |
| **WebUI GPU** | usage, VRAM, temp, power | ✅ 7/9 main | powerLimit, gpuClock, memClock — displayed in gpuCard and backendsTable |
| **WebUI System** | CPU%, RAM, CPU details | ✅ CPU%, RAM, coreCount, loadAvg 1/5/15, model, temperature, throttling | Disk total/used/free, Network RX/TX — collected by agent, not displayed (planned) |
| **WebUI GPU Hidden** | powerLimit, gpuClock, memClock | ✅ Collected by agent | ✅ Displayed in gpuCard and backendsTable |
| **WebUI Ollama** | runningModels, activeRequests, RPS, freeSlots | ✅ 4/4 main | family, format, parameterSize, quantization — collected, available in tooltip |
| **WebUI Queue Dispatch** | dispatch_by_affinity/load/config | ✅ Displayed in monitor.html | ✅ Counters implemented in backend |

---

## 7. WebUI Functionality

### 7.1 Dashboard
- **GPU Cluster**: cards for each backend with GPU metrics (usage, VRAM, temp, power)
- **System Overview**: CPU%, RAM, active requests, RPS, free slots
- **Prediction Alerts**: warnings for backends with `secondsToCritical < 300`
- **Real-time**: WebSocket updates every 5 seconds

### 7.2 Backends
- Table of all backends with filtering and search
- Columns: ID, status, GPU%, VRAM, RAM, CPU%, RPS, active requests, free slots
- CRUD operations: add, edit, delete backends
- Connect/disconnect agents

### 7.3 Models
- List of running models by backend
- Details: family, format, parameterSize, quantization (in tooltip)
- Search by model name

### 7.4 Sessions
- Table of active sessions with backend affinity
- Search by session ID

### 7.5 Queue
- Current queue size, processed total
- Timeouts and workers configuration

### 7.6 Logs
- Event log with filtering by level (info, warning, error)
- Export to file

### 7.7 Settings
- View current balancer configuration

---

## 8. Metrics data flow

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│   Ollama API    │────▶│  Agent Collector │────▶│ Agent HTTP POST │
│  (/api/tags)    │     │  (GPU+System)    │     │ /api/v1/agents  │
└─────────────────┘     └──────────────────┘     │    /metrics     │
                                                  └────────┬────────┘
                                                           │
                               ┌───────────────────────────┘
                               ▼
                     ┌─────────────────┐
                     │  Load Balancer  │
                     │   Proxy + API   │
                     └────────┬────────┘
                              │
               ┌─────────────┼─────────────┐
               ▼             ▼             ▼
         ┌─────────┐   ┌─────────┐   ┌─────────┐
         │ Queue   │   │ Predictor│   │ WS/API  │
         │ Manager │   │          │   │ Handlers│
         └─────────┘   └─────────┘   └────┬────┘
                                          │
                               ┌─────────┴──────────┐
                               ▼                    ▼
                         ┌──────────┐         ┌──────────┐
                         │  Web UI  │         │ REST API │
                         │ Dashboard│         │  Client  │
                         └──────────┘         └──────────┘
```

---

## 9. Balancer decision algorithm

### 9.1 Backend selection (`selectBackend`)

```
Pre-step: Model Replication — if the model is in a replication group, select from replicas

1. Model Affinity (P1 — LOADED)
   → expandCandidates → backend with the model in memory
   → loadRatio < triggerLoadThreshold (default 80%)

2. Model Warming (P2 — WARMING)
   → expandCandidates → backend in the process of loading
   → Wait for readiness (polling) with ModelLoadTimeout

3. Sync Model Load (P3 — FREE)
   → If SyncModelLoad.Enabled
   → Trigger warmupModel() on a free backend
   → Wait for readiness with timeout

4. Resource-Aware scoring (P4 — FALLBACK)
   → selectByResources()
   - GPU free * 0.30
   - VRAM free * 0.20
   - CPU free * 0.15
   - modelLoadedBonus, predictionBonus
   - Request penalty (active/max)
   - queueDepthPenalty, errorRatePenalty
   - Weight multiplier
   - Prediction-based filtering: secondsToCritical < 300 → excluded
```

### 9.2 Resource limits (`checkResourceLimits`)

| Resource | Limit | Action when exceeded |
|--------|-------|------------------------|
| GPU Usage | > maxUsagePercent | Exclude from pool |
| VRAM Usage | > maxVramUsagePercent | Exclude from pool |
| CPU Usage | > maxUsagePercent | Exclude from pool |
| RAM Usage | > maxUsagePercent | Exclude from pool |
| Disk Free | < minFreeMB | Exclude from pool |
| Active Requests | >= MaxConcurrentReqs | Exclude from pool |
| Models Count | >= MaxModels | Exclude from pool |

### 9.3 Queue (`QueueManager`)

```
- maxSize = config.Balancing.QueueMaxSize
- numWorkers = config.Balancing.QueueWorkers
- timeout = config.Balancing.QueueTimeout

If no backend is selected:
  1. Put in the queue
  2. Worker tries to find a backend every 100ms
  3. On timeout → 503 Service Unavailable
```

---

## 10. WebSocket format

```json
{
  "id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "status": "healthy",
  "hasAgent": true,
  "gpu": {
    "usagePercent": 45.5,
    "memoryTotal": 24576,
    "memoryUsed": 12000,
    "memoryFree": 12576,
    "temperature": 65,
    "powerUsage": 250
  },
  "system": {
    "cpuUsagePercent": 30.2,
    "memoryTotal": 65536,
    "memoryUsed": 20000,
    "memoryFree": 45536
  },
  "ollama": {
    "runningModels": [
      {
        "name": "llama3.1:70b",
        "vramUsage": 18000
      }
    ],
    "activeRequests": 3,
    "requestsPerSecond": 12.5,
    "freeSlots": 7
  },
  "prediction": {
    "secondsToCritical": 300,
    "criticalReason": "vram",
    "requestCapacity": 65.5
  }
}
```

---

## 11. Extended Ollama monitoring

### 11.1 Ollama launch flags (`OllamaRuntimeFlags`)

The agent collects Ollama process launch flags from three sources (by priority):

| Priority | Source | How it is collected |
|-----------|----------|----------------|
| 1 | Process arguments | `ps aux` (Linux/macOS), PowerShell `Get-Process` (Windows) |
| 2 | Environment variables | `OLLAMA_NUM_GPU`, `OLLAMA_CONTEXT_LENGTH`, `OLLAMA_NUM_PARALLEL`, `OLLAMA_NUM_THREADS`, `OLLAMA_KV_CACHE_TYPE` |
| 3 | Default values | `numGpuLayers=-1` (auto), `contextLength=2048`, `numParallel=1`, `numThreads=0` (auto), `batchSize=512` |

**Supported flags:**

| Field | CLI flag | Description |
|------|----------|----------|
| `numGpuLayers` | `-ngl`, `--num-gpu-layers` | GPU layers |
| `contextLength` | `-c`, `--ctx-size` | Context size |
| `numParallel` | `-np`, `--parallel` | Parallel requests |
| `numThreads` | `-t`, `--threads` | CPU threads |
| `batchSize` | `-b`, `--batch-size` | Batch size |
| `gpuSplitMode` | `--split-mode` | GPU split mode |
| `mainGpu` | `--main-gpu` | Main GPU |
| `lowVram` | `--low-vram` | Low VRAM mode |
| `f16kv` | `--no-kv-offload` (disables) | FP16 for KV cache |
| `kvCacheQuant` | `--cache-type-k` | KV cache quantization |
| `flashAttention` | `--flash-attn` | Flash Attention |

### 11.2 Model context (`ModelContextInfo`)

For each loaded model, the agent collects:

| Field | Description |
|------|-------------|
| `contextLength` | Context size (tokens) |
| `contextSource` | Source: `modelfile` / `env` / `runtime` / `default` |
| `effectiveContext` | Effective context (max of model and flags) |
| `contextMemoryMB` | Context memory (batch overhead) |
| `kvCacheMemoryMB` | KV cache memory |
| `modelMemoryMB` | Model memory |
| `totalMemoryMB` | Total memory (model + context) |
| `numLayers` | Number of layers |
| `hiddenSize` | Hidden layer size |
| `precisionBits` | KV precision (16 or 32 bits) |

**Context resolution priority:**
1. `runtime` — flag `-c`/`--ctx-size` (overrides everything)
2. `modelfile` — `num_ctx` from `/api/show` → `parameters`
3. `env` — `OLLAMA_CONTEXT_LENGTH`
4. `default` — 2048 tokens

**Context memory formula:**
```
Context Memory = batch_size × effective_context × hidden_size × precision_bits / 8 / 1024²  (MB)
KV Cache Memory = num_layers × 2 (K+V) × hidden_size × effective_context × precision_bits / 8 / 1024²  (MB)
```

Architecture and layer sizes are determined from `model_info.general.architecture` via `/api/show`. Fallback tables for `llama`, `qwen2`, `mistral`, `mixtral`, `phi`.

### 11.3 Backend capacity (`BackendCapacity`)

The agent estimates which models can be loaded on the backend:

| Field | Description |
|------|-------------|
| `freeVram` | Free VRAM (MB) |
| `guaranteedVram` | Guaranteed free (90% of free) |
| `loadedModelVram` | VRAM of already loaded models |
| `contextOverheadMB` | Total context memory |
| `loadableModelCount` | Number of models that can be loaded |
| `mode` | `gpu` or `cpu` |

**Loadability estimation algorithm:**
1. `guaranteedVRAM = freeVRAM × 0.9` (10% reserve)
2. For each available model from `/api/tags`:
   - `estimatedVRAM = modelSizeMB + contextMemory + kvCacheMemory`
   - `canLoad = estimatedVRAM ≤ guaranteedVRAM`
3. Sort by `estimatedVRAM`

For a CPU agent, the same is done with RAM, without GPU layers.

### 11.4 API Endpoints

New endpoints:

- `GET /api/v1/backends/{id}/capacity` — detailed backend capacity with flags, contexts and available models
- `GET /api/v1/models/capacity` — global summary across all backends: `totalLoadable`, `backends[]` with `loadableModelCount`, `runtimeFlags`, `availableModels`

### 11.5 WebUI

The Dashboard shows:
- **Ollama Runtime** — compact flag badges: `GPU:47 | C:8192 | NP:4 | T:8 | B:512`
- **Context** — next to the model: `Ctx: 8192 (modelfile)`, tooltip with memory details
- **Capacity Bar** — VRAM progress bar: loaded / contexts / free / guaranteed
- **Available to Load** — list of models with green/red loadability indicators
- **Loadable Count** — badge with the total number of models available for loading

### 11.6 CPU Mode

When `platformMode = "cpu"`:
- VRAM is not collected, RAM is used instead
- GPU layers are ignored
- `canLoad` is estimated from `System.MemoryFree`
- Precision bits remain 16 (F16KV) or 32

---

*Document version 1.1.0 | Updated: 2025-04-27*
