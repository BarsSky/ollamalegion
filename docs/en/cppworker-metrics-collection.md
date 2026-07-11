# OllamaLegion CppWorker Metrics: poller vs agent

> This document describes **exactly how** live metrics are collected for
> `BackendType=llama_cpp` (cppworker) backends in OllamaLegion. Target audience —
> developers who see "everything at zero" on the Dashboard / Backends / Models /
> GGUF Models tabs and want to understand what to enable and where to look.

---

## 1. Metrics collection architecture

In OllamaLegion there are **two independent** metric collection mechanisms for
the cppworker backend. They complement each other: the poller provides **only**
the model list, the agent provides everything else (GPU/CPU/RAM).

```
┌─────────────────────┐         ┌──────────────────────┐
│     cppworker       │  GET    │  balancer            │
│   :18092 (host)     │◀────────│  :18081              │
│  /api/models        │ 30s/2s  │                      │
│  /api/models/load/  │         │  ┌────────────────┐  │
│       progress      │         │  │ llamaCpp-      │  │
└─────────────────────┘         │  │ MetricsPoller  │  │
        ▲                       │  │ (in-process)   │  │
        │ GET                   │  └────────────────┘  │
        │ /api/models           │           │           │
        │ /metrics              │           ▼           │
┌─────────────────────┐         │  metricsMgr.llama    │
│   agent sidecar     │         │  Metrics[b.id] = {   │
│   :18032 (host)     │────────▶│    LoadedModels,     │
│   LlamaCollector    │ POST    │    LoadingModels,    │
│   (NVML, CPU, RAM)  │ /api/v1 │    BackendMetrics    │
└─────────────────────┘  backends│  }                   │
                                 └──────────────────────┘
                                           │
                                           ▼
                                 ┌──────────────────────┐
                                 │  WebUI / REST API    │
                                 │  Dashboard, Backends │
                                 │  /api/v1/backends    │
                                 └──────────────────────┘
```

**Key points:**

1. **The poller lives inside the balancer** (`internal/balancer/llamacpp_metrics_poller.go`).
   It starts automatically when `cmd/balancer` starts. It queries cppworker
   over HTTP. It does not require a separate container and does not use an agent.

2. **The agent is a separate container** (`cmd/agent`), launched next to cppworker
   in the same pod. It contains NVML bindings for GPU metrics and reads `/proc/stat`
   and `/proc/meminfo` for the host. It registers the backend via `POST /api/v1/backends`.

3. **Data is aggregated** in `internal/balancer/cluster_state.go:60-148`:
   - `metricsMgr.metrics[id]` (from agent) → `BackendMetrics.GPU`, `System`, `Ollama`.
   - `metricsMgr.llamaMetrics[id]` (from poller) → `LlamaCppMetrics.LoadedModels/LoadingModels`.

---

## 2. What `llamaCppMetricsPoller` collects

**File:** `internal/balancer/llamacpp_metrics_poller.go` (read-only — **do not modify**).

The poller makes **only two HTTP requests** to each registered cppworker
backend:

| Endpoint | Method | Interval | What it returns |
|---|---|---|---|
| `/api/models` | GET | 30 sec (idle) / 2 sec (when loading) | Array `models[]` with state, VRAM, RAM, contextSize, gpuLayers, quantization |
| `/api/models/load/progress` | GET | (only when loading) | Array `models[]` with state, elapsedMs, error |

**Adaptive interval:**
- If loading models were observed in the previous poll → the next poll runs
  after `fastInterval=2s` (see `lastLoadingSeen`).
- Otherwise → `interval=30s`, to avoid overloading cppworker.

**What lands in `metricsMgr.llamaMetrics[b.id]`:**
- `LoadedModels []LlamaCppModel` — all models (any state: loaded/loading/error).
  Filtering by `state==loaded` is done on the UI side.
- `LoadingModels []LlamaCppModel` — models in the process of loading (state=loading).
  Used by the UI to display a spinner and elapsed time "Loading model-name 25s".

**What the poller does NOT collect** (important!):
- ❌ GPU metrics (usage%, VRAM used/total, temperature, power, clocks) — no NVML.
- ❌ Host CPU/RAM — the poller runs on the balancer host, not cppworker's.
- ❌ `requests_per_second`, `avg_response_time` — cppworker does not expose them.
- ❌ `total_queries`, `active_queries` (available in `/api/models`, but not forwarded).

**NB:** for these metrics, an **agent sidecar** is required (see §3).

---

## 3. What the agent collects (Ollama agent + `LlamaCollector`)

**Files:** `internal/agent/collector.go`, `internal/agent/llama_collector.go`.

The agent runs **next to cppworker** (sidecar pattern — shared network namespace
or bridge network). It is configured via `BackendType`:

```go
// internal/agent/collector.go:81-89
if config.BackendType == types.BackendTypeLlamaCpp {
    cppURL := config.CppWorkerURL
    if cppURL == "" {
        cppURL = "http://localhost:18091"
    }
    a.llamaCollector = NewLlamaCollector(cppURL)
}
```

**`LlamaCollector` collects:**

| Metric | Source | Lands in |
|---|---|---|
| `gpu[].usage` (%) | NVML `nvmlDeviceGetUtilizationRates` (Linux) or `nvidia-smi` (Windows fallback) | `BackendMetrics.GPU.Usage` |
| `gpu[].memoryUsed` / `memoryTotal` (MB) | NVML `nvmlDeviceGetMemoryInfo` | `BackendMetrics.GPU.MemoryUsed/MemoryTotal` |
| `gpu[].temperature` (°C) | NVML `nvmlDeviceGetTemperature` | `BackendMetrics.GPU.Temperature` |
| `gpu[].powerDraw` (W) | NVML `nvmlDeviceGetPowerUsage` | `BackendMetrics.GPU.PowerDraw` |
| `gpu[].clocks.graphics/memory` (MHz) | NVML `nvmlDeviceGetClockInfo` | `BackendMetrics.GPU.Clocks*` |
| `cpuPercent` (%) | `/proc/stat` (Linux) / `wmic` (Windows) | `BackendMetrics.System.CPUPercent` |
| `memUsed` / `memTotal` (MB) | `/proc/meminfo` / `wmic` | `BackendMetrics.System.MemUsed/MemTotal` |
| `models[]` | `GET http://cppworker:18091/api/models` | `BackendMetrics.LlamaCpp.LoadedModels` (duplicates poller, but agent comes first) |

**When the agent is active** (`BackendType=llama_cpp` and `hasAgent=true` in
`/api/v1/backends`) — `data/state.json` shows an entry like:

```json
{
  "id": "cppworker-gpu-with-agent",
  "type": "llama_cpp",
  "hasAgent": true,
  "host": "cppworker-gpu",
  "port": 18092
}
```

`hasAgent=true` means: `metricsMgr.metrics[id]` is populated, UI/WebUI draws
live GPU/CPU/RAM values.

---

## 4. When to use sidecar vs standalone

| Scenario | Recommendation | Compose |
|---|---|---|
| Production with a GPU node, full monitoring needed | **Sidecar** — agent in the same pod | `docker-compose.cppworker-with-agent.yml` |
| Standalone demo / single host without external balancer | **Bundled sidecar** | `docker-compose.cppworker-with-agent.standalone.yml` |
| Bundled stack already deployed (`docker-compose.cppworker-bundled.yml`) | **Nothing to change** — bundled stack works, agent is not required for inference | `docker-compose.cppworker-bundled.yml` |
| Legacy cppworker without monitoring (metrics = 0, but inference works) | **Add a sidecar agent** | see §6 Troubleshooting |
| CPU-only cppworker (no GPU) | **Optional** — agent will provide CPU/RAM, but NVML metrics will be empty | any sidecar compose |

**Sidecar checklist:**

- [ ] `BackendType=llama_cpp` in the `agent` configuration.
- [ ] `CppWorkerURL` points to the **internal** address of cppworker (for example,
      `http://localhost:18091` in shared network namespace, or `http://cppworker-gpu:18091` in bridge).
- [ ] `nvidia-container-toolkit` is installed on the GPU host (for NVML).
- [ ] The agent registers the backend via `POST /api/v1/backends` with the correct `id`
      and `host`/`port` that the balancer can resolve.
- [ ] If using a bundled stack — set `CPPWORKER_REGISTER_DISABLE=true` on
      cppworker (issue 2026-06-25 about duplicates).

---

## 5. Backward compatibility: legacy `BackendType=""`

Before `BackendType` was introduced, the field was empty. For legacy backends in
`data/state.json` (which have not yet been migrated), normalization is applied:

```go
// internal/balancer/normalizeBackendType
if bt == "" {
    return types.BackendTypeOllama // legacy default
}
```

**This means:** if your `data/state.json` has a backend `cppworker-gpu` without
a `type` field, the balancer treats it as an **Ollama backend**. The agent sidecar
still works (LlamaCollector is selected by `BackendType` from the **agent config**,
not from state.json), but `data/state.json` will show `type="ollama"` and
`hasAgent=true` without a `LlamaCpp` block.

**Migration:** `POST /api/v1/backends/{id}` with `{"type": "llama_cpp"}` →
the balancer will rewrite state.json with the correct `BackendType`.

**Verification:** `curl -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/backends | jq '.backends[] | {id, type, hasAgent}'`

---

## 6. Troubleshooting

### 6.1 "Metrics are zero for cppworker backend"

**Step 1.** Check that the backend is registered with `BackendType=llama_cpp`:

```bash
curl -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/backends | jq '.backends[] | select(.id | contains("cppworker")) | {id, type, hasAgent}'
```

If `type="ollama"` or empty — this is a legacy backend, see §5.

**Step 2.** If `type="llama_cpp"` and `hasAgent=false`:

- Check that the agent sidecar is running: `docker ps | grep agent`.
- Check agent logs: `docker logs <agent-container> 2>&1 | grep -E "registered|error"`.
- Make sure the agent can reach cppworker:
  `docker exec <agent-container> curl http://cppworker-gpu:18091/health`.

**Step 3.** If `type="llama_cpp"` and `hasAgent=true`, but metrics are still 0:

- Check that NVML is available inside the agent container:
  `docker exec <agent-container> nvidia-smi`.
- If `command not found` — the image was built without `nvidia-container-toolkit`.
  Rebuild with `--gpus all` and `runtime: nvidia` in compose.
- On Windows: the agent uses `nvidia-smi` via `exec.Command` (see
  `internal/agent/nvml_windows.go`). If `nvidia-smi` is not in PATH —
  metrics will be zero.

**Step 4.** Check that the poller is actually working:

```bash
# The balancer logs should contain lines like:
grep "llamaCppMetricsPoller" data/balancer.log
# "llamaCppMetricsPoller started" — OK
# "llamaCppMetricsPoller: updated llama.cpp metrics" — every 30s/2s
```

If the poller does not start — check that `llamaCppRouter` is initialized
(should be present when `OperatingMode` ∈ {`standard`, `replication`, `rpc_coordinator`}).

### 6.2 "LoadedModels is empty even though a model is loaded"

- Check `GET http://cppworker-host:18092/api/models` directly:
  it should return JSON with `count > 0`.
- If cppworker returns models, but the poller shows empty — possibly
  `cppworker` is behind NAT, and the `host:port` in state.json is unreachable from the balancer.
- Check `BackendMetrics` in the WebUI: Backends tab → "View metrics" →
  `loadedModels.length` should match `count` from `/api/models`.

### 6.3 "Loading progress does not update (UI shows 0%)"

`/api/models/load/progress` was implemented in cppworker ≥ 2026-06-25. On older
images the endpoint will return 404 — the poller ignores it (see the code in
`pollLoadingProgress`: "404 — endpoint may not be implemented in older cppworker
versions. Silently ignored."). Solution: rebuild the cppworker image after
2026-06-25 or wait for loading to complete (the poller will still update
`LoadedModels` after `/api/models` within 2 seconds).

### 6.4 "After balancer restart, metrics are gone"

The poller does an **immediate first poll** on startup (`loop()` line 101),
but agent metrics (`metricsMgr.metrics[id]`) are only updated when the agent
sends its next heartbeat. If the agent is configured with a large
`heartbeat_interval` (default 30 sec) — after a balancer restart there will be
a "hole" of ~30 sec.

---

## 7. Related documents

- `docs/issues/2026-06-29-llamacpp-agent-sidecar.md` — full postmortem
  on the sidecar (environment variables, compose files, limitations).
- `docs/issues/2026-06-29-big-model-20gb-fix.md` — auto_tune_nctx + 3-stage
  cascade (VRAM metrics are critical for n_ctx selection).
- `docs/runbook-tools.md` — runbook for tools/tool_calls (if you need to debug
  inference issues, load metrics are also useful).
- `docs/ru/docker-compose-guide.md` §4.1 — sidecar stack: cppworker + agent.
- `internal/balancer/llamacpp_metrics_poller.go` — the poller itself (read-only).
- `internal/agent/collector.go:81-89` — `BackendType=llama_cpp` switch.
- `pkg/types/backend_type.go` — `BackendTypeLlamaCpp` / `BackendTypeOllama` constants.

---

## 8. Summary table: where each value comes from

| Field in WebUI / API | Source | Endpoint / method |
|---|---|---|
| Backend list (`/api/v1/backends`) | `data/state.json` | agent `POST /api/v1/backends` on registration |
| `BackendMetrics.GPU.Usage` (%) | **agent** | NVML (Linux) / `nvidia-smi` (Windows) |
| `BackendMetrics.GPU.MemoryUsed` | **agent** | NVML |
| `BackendMetrics.GPU.Temperature` | **agent** | NVML |
| `BackendMetrics.System.CPUPercent` | **agent** | `/proc/stat` / `wmic` |
| `BackendMetrics.System.MemUsed` | **agent** | `/proc/meminfo` / `wmic` |
| `LlamaCppMetrics.LoadedModels[].Name` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].State` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].ContextLength` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].NumGPULayers` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].Quantization` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].VRAMUsage` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].RAMUsage` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadingModels[].ElapsedMs` | **poller** | `GET /api/models/load/progress` |
| `LlamaCppMetrics.LoadingModels[].Error` | **poller** | `GET /api/models/load/progress` |

**Main conclusion:** for **complete** live metrics, the **agent sidecar** is required.
The poller alone will not provide GPU/CPU/RAM — only the model list and loading progress.
