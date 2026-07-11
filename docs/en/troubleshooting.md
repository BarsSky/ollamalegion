# OllamaLegion Troubleshooting Guide

> **Version:** 3.1 (2026-06-29) — added §10 "Live metrics = 0 for cppworker backend" + link to [`cppworker-metrics-collection.md`](cppworker-metrics-collection.md)
> **Related documents:** [`runbook-tools.md`](runbook-tools.md) — detailed runbook for tools/tool_calls, [`cppworker-model-params.md`](cppworker-model-params.md) — n_ctx/Per-Model Profiles, [`cppworker-metrics-collection.md`](cppworker-metrics-collection.md) — metrics architecture, [`audit-2026-06.md`](audit-2026-06.md), [`../.clinerules`](../.clinerules)

## Table of Contents

1. [Common Load Balancer Errors](#1-common-load-balancer-errors)
2. [n_ctx Overflow: Diagnostics and Resolution](#2-n_ctx-overflow-diagnostics-and-resolution)
3. [ReloadLoopLimitError (HTTP 413)](#3-reloadlooplimiterror-http-413)
4. [Backend Marked as Unhealthy](#4-backend-marked-as-unhealthy)
5. [HTTP 5xx / "Server disconnected" on Heavy Models](#5-http-5xx--server-disconnected-on-heavy-models)
6. [Tools/tool_calls — Reset or Empty Response](#6-toolstool_calls--reset-or-empty-response)
7. [TransferEncodingError During Streaming](#7-transferencodingerror-during-streaming)
8. [NVIDIA Container Toolkit Issues](#8-nvidia-container-toolkit-issues)
9. [Agent → Ollama: Limitations](#9-agent--ollama-limitations)
10. [Live metrics = 0 for cppworker backend](#10-live-metrics--0-for-cppworker-backend)
11. [Logging and Debugging](#11-logging-and-debugging)
12. [Related Documents](#12-related-documents)

---

## 1. Common Load Balancer Errors

### 1.1 Agent does not connect to the load balancer

**Symptoms:**
- The agent does not appear in the backends list.
- The agent logs show `connection refused` / `timeout`.

**Solution:**

```bash
# 1. Verify the load balancer is reachable
curl -v http://<balancer-ip>:18081/api/v1/health

# 2. Check the firewall
sudo ufw status  # Linux
netsh advfirewall show allprofiles  # Windows

# 3. Inspect the agent logs
docker logs deployments-agent-1

# 4. Check BALANCER_URL inside the container
docker exec deployments-agent-1 sh -c 'echo $BALANCER_URL'
```

**Possible causes:**
- Incorrect URL.
- Firewall blocking port 18081.
- The load balancer is not running.
- Docker network issue between containers (for cppworker auto-registration).

### 1.2 Backend is marked as unhealthy

**Symptoms:** `b.status === 'unhealthy'` or `'offline'` in `/api/v1/backends`.

**Solution:**

```bash
# 1. Check Ollama directly
curl http://<ollama-host>:11434/api/tags

# 2. Check the agent (if present)
curl http://<agent-host>:18032/health

# 3. Check the network
ping <ollama-host>
telnet <ollama-host> 11434
```

**See also:** [`agent-deployment.md`](agent-deployment.md), Troubleshooting section.

### 1.3 503 "model is loading"

**Symptom:** Right after the first request to a large model, the response is 503.

**Cause:** cppworker is blocked on `LoadModel` (30-60 sec), so the next request sees `errModelIsLoading`.

**Solution:**
- Wait for the model to finish loading (cppworker will return `loading: true, retryAfterMs: 3000`).
- Or increase `Balancing.RequestTimeout` to 300-600 sec (see [`configuration.md`](configuration.md)).

---

## 2. n_ctx Overflow: Diagnostics and Resolution

### 2.1 Symptoms

```
"requested n_ctx=128000 exceeds model's effective n_ctx=4096"
```

or

```
HTTP 400 / 413: n_ctx too large for current load
```

### 2.2 Full Problem Flow

```
Cline → {"model":"gemma-4-E4B-...","options":{"num_ctx":128000}}
  ↓
Load balancer:
  1. ExtractNumCtxFromBody(body) = 128000
  2. ResolveNumCtx(model, body, backendID):
     - Tier 1 (body): 128000
     - Tier 2 (maxNumCtxForModel):
       ├─ GetModelProfileNumCtx() = 0 → skip
       ├─ GetDefaultModelProfileNumCtx() = ??? ← ⚠️ If 4096 — clamping!
       └─ getModelLoadedCtxFromMetrics() — not reached
     - Result: clamped 128000 → 4096
  3. ApplyCppCtxHeader → X-Cpp-Ctx: 4096
  ↓
CppWorker:
  1. buildGenerationParams: NCtxOverride = 128000 (from body)
  2. applyCppCtxHeader: body 128000 > headerLimit 4096 → CLAMP → 4096
  3. Inference with n_ctx = 4096
  4. Prompt > 4096 → bridge code 2 (ErrNCtxNeedsReload)
  ↓
Load balancer:
  1. ParseCppWorkerError → NCtxError
  2. handleNCtxReload → DecideReloadBackend:
     - AutoReloadNCtx = false → DecisionNoOp
  3. HTTP 413/400 to client
```

### 2.3 Step-by-Step Diagnostics

```bash
# 1.1 Check defaultModelProfile.contextLength in config
curl -s http://localhost:18081/api/v1/config | jq '.defaultModelProfile.contextLength'
# Expected: 0 or null
# If 4096 — config was not updated!

# 1.2 Check the RAM fallback on cppworker
docker exec deployments-cppworker-gpu-1 sh -c 'echo $CPPWORKER_RAM_FALLBACK_N_CTX'
# Expected: "true"

# 1.3 Check n_ctx auto-reload on the load balancer
curl -s http://localhost:18081/api/v1/config | jq '.balancing.nctxReload'
# Expected: an object with autoReloadNCtx: true

# 1.4 Direct generation test with a large num_ctx
curl -X POST http://localhost:18081/api/generate \
  -H 'Content-Type: application/json' \
  -H 'X-API-Token: <token>' \
  -d '{"model":"<model.gguf>","prompt":"Hello","options":{"num_ctx":32000}}'
```

### 2.4 Resolution

#### Option A: Per-Model Profile

```bash
# Create a profile for a specific model
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -d '{"contextLength": 32768, "batchSize": 1024, "numGpuLayers": -1}'

# Apply (reload on all backends)
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply
```

See also: [`cppworker-model-params.md` §3-4](cppworker-model-params.md).

#### Option B: Update `config.json`

Remove `defaultModelProfile.contextLength` (or set it to 0):

```diff
- "defaultModelProfile": {
-   "contextLength": 4096,
-   ...
- }
+ "defaultModelProfile": {
+   "contextLength": 0,
+   ...
+ }
```

#### Option C: Enable RAM fallback + auto-reload

```env
# .env.bundled
CPPWORKER_RAM_FALLBACK_N_CTX=true
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000
LB_NCTX_RELOAD_ENABLED=true
LB_NCTX_RELOAD_MAX_N_CTX=131072
LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR=0.85
```

---

## 3. ReloadLoopLimitError (HTTP 413)

### 3.1 Symptoms

```
"reload attempts exceeded maximum of 3 per 60 sec window"
```

or in cppworker logs:
```
RAM fallback: cycle limit reached, refusing reload
```

### 3.2 Cause

cppworker tried to reload 3 times within 60 sec, but every time it hit the same error (n_ctx overflow). This is a guard against infinite reload loops.

### 3.3 Resetting the Counter (R-6)

**Current state:** the only way is `docker restart deployments-cppworker-gpu-1`.

**After R-6** (in progress):

```bash
# Load balancer (counter on the balancer side):
curl -X POST http://localhost:18081/api/v1/nctx-reload/cppworker-gpu-1/reset

# CppWorker (counter on the cppworker side):
curl -X POST http://localhost:18081/api/v1/cppworker/reset-reload-counter
```

### 3.4 Before Resetting

1. Eliminate the root cause (e.g. lower `num_ctx` in the request, add VRAM, unload other models).
2. Check `max_vram_n_ctx` in the logs.
3. Restart only after the cause is resolved.

---

## 4. Backend Marked as Unhealthy

### 4.1 Diagnostics

```bash
# Status of all backends
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends | jq

# Specific backend (by ID)
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends/cppworker-gpu-1
```

### 4.2 Causes and Solutions

| Cause | Symptom | Solution |
|---|---|---|
| Ollama is not running | `curl 11434` → connection refused | `systemctl restart ollama` |
| Firewall | timeout | open the port |
| Wrong host/port | 503 on health check | fix in WebUI → Backends → Edit |
| Backend is overloaded | `status=healthy`, but responses return 503 | lower `MaxConcurrentRequests` |
| Agent is not registering | `b.hasAgent === false` | verify `BALANCER_URL` |

---

## 5. HTTP 5xx / "Server disconnected" on Heavy Models

### 5.1 Cause

`Balancing.RequestTimeout` (default 30 sec) fires before `clampNPredictToFitContext` finishes a long generation.

### 5.2 Solution

```json
// config/config.json
{
  "balancing": {
    "requestTimeout": 600   // 10 minutes
  }
}
```

Or via ENV: `LB_REQUEST_TIMEOUT=600`.

You can also increase `CPPWORKER_WRITE_TIMEOUT`:

```env
CPPWORKER_WRITE_TIMEOUT=1800  # 30 minutes
```

---

## 6. Tools/tool_calls — Reset or Empty Response

**This is the most common problem when working with OpenWebUI / Cline / Roo Code.** A detailed step-by-step runbook is in [`runbook-tools.md`](runbook-tools.md).

### 6.1 Quick Scenario Summary

| Symptom | Scenario | Where to look |
|---|---|---|
| HTTP 413 on the first request with tools | **A** — RAM fallback disabled for tools | `cmd/cppworker/inference.go:491` |
| First request OK, second one returns empty NDJSON | **B** — clamping n_predict with `has_tools=true` | `internal/balancer/nctx_clamp.go` |
| Infinite reload loop, 503 | **C** — `ReloadLoopLimitError` | `cmd/cppworker/inference.go` |
| Tool_call appears in `content`, but `tool_calls=null` | **D** — Hermes/Mistral detection | `internal/balancer/llamacpp_toolcall_detector.go` |
| HTTP 5xx / "reset by peer" | **E** — WriteTimeout/RequestTimeout timeouts | see §5 |

### 6.2 Acceptance Criteria (Before Debugging)

1. `CPPWORKER_VERBOSE=true` on cppworker.
2. `data/state.json` (balancer) — list of backends and their statuses.
3. Load balancer logs: `parsed request`, `[BALANCER → BACKEND]`, `heartbeat write failed`.
4. cppworker logs: `clamping n_predict`, `RAM fallback`, `bridge code N`.
5. Basic transport tests:
   ```bash
   go test ./tests -run "TestDebugOpenWebUI_ToolCalls_Scenario" -tags llama_stub -count=1 -v
   ```
6. Inference tests:
   ```bash
   go test ./cmd/cppworker -run "TestApplyCppCtxHeader|TestClampNPredict|TestReloadDisabledForTools|TestParseToolCalls" -tags llama_stub -count=1 -v
   ```

---

## 7. TransferEncodingError During Streaming

### 7.1 Symptoms

```
Error: write tcp: ..."write: broken pipe"
or
TransferEncodingError: chunked transfer encoding failed
```

### 7.2 Cause (Fixed on 2026-06-08)

Previously: cppworker returned a JSON error for a streaming request, the load balancer tried to proxy it as NDJSON, but Go added `Transfer-Encoding: chunked`, which broke the client.

### 7.3 Current Behavior

`internal/balancer/llamacpp_transport.go:proxyRequestLlamaCpp` checks the upstream response `Content-Type`:
- If `application/json` (error) → proxy as JSON, **do not** enter the SSE reader.
- If `text/event-stream` → normal streaming.

`resp.Header.Del("Transfer-Encoding")` was removed — Go manages this automatically.

### 7.4 If the Error Persists

Check the `Content-Type` from cppworker:
```bash
curl -v -X POST http://localhost:18092/api/chat -H "Content-Type: application/json" -d '{"model":"x","stream":true}' 2>&1 | grep -E "^< Content-Type"
```

The expected value is `text/event-stream` or `application/x-ndjson`, **not** `text/plain` and not missing.

---

## 8. NVIDIA Container Toolkit Issues

### 8.1 GPU Not Visible Inside the Container

```bash
# Check on the host
nvidia-smi

# Check inside the container
docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi
```

If the second command fails → NVIDIA Container Toolkit is not installed.

**Installation:** see https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html

### 8.2 Windows: GPU Is Not Passed Through

1. Docker Desktop → Settings → General → ✅ Use WSL 2.
2. Resources → WSL Integration → ✅ Enable integration.
3. Install the NVIDIA Driver for WSL2: https://developer.nvidia.com/cuda/wsl.

See also: [`agent-deployment.md` §Windows](agent-deployment.md).

---

## 9. Agent → Ollama: Limitations

> **This is an architectural limitation of Ollama, not a bug in OllamaLegion.**

Ollama **does not expose a public REST API** for changing its runtime config. The agent can:
- ✅ Read metrics (`/api/ps`, `/api/tags`, `/api/show`).
- ✅ Send metrics to the load balancer.
- ✅ Apply limits via heartbeat.
- ❌ Change Ollama's `numGPU`, `num_parallel`, `num_threads` directly.

The only ways to change Ollama config:
- Environment variables when Ollama is started (`OLLAMA_NUM_PARALLEL`, `OLLAMA_MAX_LOADED_MODELS`).
- The `~/.ollama/config.json` file.
- CLI arguments when running `ollama serve`.

**What is NOT implemented (and not planned):** an Agent → Ollama config mechanism via heartbeat.

---

## 10. Live metrics = 0 for cppworker backend

> **This is expected poller behavior, not a bug.** Detailed architecture: [`cppworker-metrics-collection.md`](cppworker-metrics-collection.md).

**Symptom:** on the Dashboard / Backends / Models / GGUF Models tabs, the `cppworker-gpu` backend shows `GPU Usage: 0%`, `VRAM Used: 0 MB`, `CPU: 0%`, `RAM: 0 MB`. The list of loaded models (LoadedModels) **may** still be populated.

**Root cause:** the poller in the load balancer (`internal/balancer/llamacpp_metrics_poller.go`) only collects `GET /api/models` and `GET /api/models/load/progress`. It has no NVML access to the cppworker GPU, it does not sample CPU/RAM from the cppworker host, and it does not publish `requests_per_second`. For live hardware metrics, an **agent sidecar** is required.

**Quick diagnostics:**

```bash
# 1. Check whether the backend has an agent
curl -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/backends | \
  jq '.backends[] | select(.id | contains("cppworker")) | {id, type, hasAgent}'
# If hasAgent=false — the agent is not running, see below.

# 2. If hasAgent=true but metrics are 0:
docker exec <agent-container> nvidia-smi
# If "command not found" — the image was built without nvidia-container-toolkit.
```

**Solution:** deploy an agent alongside cppworker:

| Scenario | Compose file |
|---|---|
| Sidecar to an existing balancer | `deployments/docker-compose.cppworker-with-agent.yml` |
| Standalone (all-in-one) | `deployments/docker-compose.cppworker-with-agent.standalone.yml` |

For the full architecture of poller vs agent, the legacy `BackendType=""`, and all troubleshooting scenarios — see [`cppworker-metrics-collection.md`](cppworker-metrics-collection.md).

**NB:** do **not** modify the poller (`llamacpp_metrics_poller.go` is read-only). This is an intentional separation of responsibilities: the poller handles models, the agent handles hardware.

---

## 11. Logging and Debugging

### 11.1 Log Levels

| ENV | Level |
|---|---|
| `LOG_LEVEL=debug` | maximum verbosity |
| `LOG_LEVEL=info` (default) | basic |
| `LOG_LEVEL=warn` | warnings only |
| `LOG_LEVEL=error` | errors only |

### 11.2 Format

| ENV | Format |
|---|---|
| `LOG_FORMAT=json` (default) | structured JSON |
| `LOG_FORMAT=text` | human-readable |

### 11.3 Viewing Logs

```bash
# Load balancer
docker logs -f deployments-loadbalancer-1

# CppWorker with verbose
docker logs -f deployments-cppworker-gpu-1 2>&1 | grep -E "clamping|RAM fallback|bridge code|tool_calls"

# PowerShell
docker logs -f deployments-loadbalancer-1 2>&1 | Select-String "tools|chat_id|stream|/v1/chat"
```

### 11.4 NVML Debugging (Linux)

```bash
# Check nvidia-smi in the agent
docker exec deployments-agent-gpu-1 nvidia-smi

# Enable NVML verbose
docker exec deployments-agent-gpu-1 sh -c 'NVML_DEBUG=1 ./agent'
```

### 11.5 Useful Debug ENV Variables

```bash
# Load balancer
LOG_LEVEL=debug LOG_FORMAT=text
BALANCING_DEBUG=true  # does not exist; example only

# CppWorker
CPPWORKER_VERBOSE=true
```

---

## 12. Related Documents

- [`runbook-tools.md`](runbook-tools.md) — **detailed step-by-step runbook for tools/tool_calls** (scenarios A-E).
- [`cppworker-model-params.md`](cppworker-model-params.md) — n_ctx + Per-Model Profiles.
- [`cppworker-metrics-collection.md`](cppworker-metrics-collection.md) — **architecture of cppworker metrics collection: poller vs agent**.
- [`agent-deployment.md`](agent-deployment.md) — agent deployment.
- [`installation.md`](installation.md) — installation.
- [`audit-2026-06.md`](audit-2026-06.md) — implementation status.
- [`../.clinerules`](../.clinerules) §14 — common pitfalls.

---

## Connection Issues

### Cannot connect to load balancer

**Symptoms:** `curl http://localhost:18081/api/v1/health` fails

**Solutions:**
1. Verify the balancer is running:
   ```bash
   docker ps | grep loadbalancer
   # or
   ps aux | grep ollama-balancer
   ```
2. Check logs:
   ```bash
   docker logs ollamalegion_loadbalancer_1
   ```
3. Verify the port is not in use:
   ```bash
   netstat -tuln | grep -E '18080|18081'
   ```
4. Check firewall rules:
   ```bash
   sudo ufw status  # Ubuntu
   sudo firewall-cmd --list-all  # CentOS/RHEL
   ```

### CORS errors in WebUI

**Symptoms:** Browser console shows CORS errors

**Solutions:**
1. Access via nginx proxy (not directly as `file://`)
2. Use Docker Compose which includes the nginx reverse proxy
3. If accessing directly, add the API base:
   ```
   http://localhost:18080/?api_base=http://localhost:18081
   ```

---

## Backend Problems

### Backend shows as unhealthy

**Symptoms:** Backend status is "unhealthy" in WebUI

**Solutions:**
1. Verify Ollama is running on the backend:
   ```bash
   curl http://backend-ip:11434/api/tags
   ```
2. Check if the agent is running:
   ```bash
   curl http://backend-ip:18032/metrics
   ```
3. Verify network connectivity:
   ```bash
   ping backend-ip
   telnet backend-ip 11434
   ```
4. Check backend logs:
   ```bash
   docker logs ollama-agent-1
   ```

### Models not appearing in the list

**Symptoms:** The Models tab shows no models or an incomplete list

**Solutions:**
1. Verify models are loaded on the backends:
   ```bash
   curl http://backend-ip:11434/api/tags
   ```
2. Restart backend registration:
   ```bash
   # Delete and re-add the backend
   curl -X DELETE http://localhost:18081/api/v1/backends/backend-id
   curl -X POST http://localhost:18081/api/v1/backends -d '{"id":"...","host":"..."}'
   ```

---

## Performance Issues

### High queue depth

**Symptoms:** Many requests waiting in the queue

**Solutions:**
1. Check whether the backends are overloaded:
   ```bash
   curl http://localhost:18081/api/v1/cluster | jq '.backends[].activeRequests'
   ```
2. Force a rebalance:
   ```bash
   curl -X POST http://localhost:18081/api/v1/queue/rebalance
   ```
3. Add more backends
4. Increase `maxConcurrentRequests` on the backends:
   ```bash
   curl -X PUT http://localhost:18081/api/v1/backends/backend-id \
     -d '{"maxConcurrentRequests": 16}'
   ```

### Slow response times

**Symptoms:** Requests take unusually long

**Solutions:**
1. Check GPU metrics:
   ```bash
   curl http://localhost:18081/api/v1/cluster | jq '.backends[].gpu'
   ```
2. Verify the model is loaded (not swapping):
   ```bash
   curl http://backend-ip:11434/api/ps
   ```
3. Check disk I/O on the backends
4. Reduce `maxConcurrentRequests` if GPUs are overloaded

---

## Docker Issues

### Container won't start

**Symptoms:** `docker compose up` fails

**Solutions:**
1. Check port conflicts:
   ```bash
   netstat -tuln | grep -E '18080|18081|6379|5432'
   ```
2. Verify Docker Compose version (2.0+):
   ```bash
   docker compose version
   ```
3. Pull latest images:
   ```bash
   docker compose pull
   ```
4. Rebuild:
   ```bash
   docker compose build --no-cache
   ```

### GPU not available in the container

**Symptoms:** The agent shows no GPU metrics

**Solutions:**
1. Verify the NVIDIA Container Toolkit:
   ```bash
   nvidia-ctk --version
   ```
2. Test GPU access:
   ```bash
   docker run --rm --gpus all nvidia/cuda:12.0-base nvidia-smi
   ```
3. Check the Docker GPU runtime:
   ```bash
   docker info | grep -i runtime
   ```
4. Add `--gpus all` or use `docker-compose.agent.gpu.yml`

---

## WebUI Issues

### Blank page or incomplete rendering

**Symptoms:** WebUI page is empty or only partially loaded

**Solutions:**
1. Clear the browser cache (Ctrl+Shift+R)
2. Check the JavaScript console for errors (F12)
3. Verify the API is accessible:
   ```bash
   curl http://localhost:18081/api/v1/health
   ```
4. Force a refresh: add `?v=2` to the URL

### Monitor page shows "No data"

**Symptoms:** The Monitor tab displays no information

**Solutions:**
1. Wait for the first data fetch (2-5 seconds)
2. Click the "Retry" button
3. Enable demo mode to verify the UI works:
   - Open the browser console: `enterDemoMode()`
4. Check the CORS configuration
5. Verify the API base URL:
   - Open the console: `API_BASE`
   - Set it manually: `localStorage.setItem('monitorApiBase', 'http://localhost:18081')`

### Theme not applying correctly

**Symptoms:** The dark/light theme does not switch

**Solutions:**
1. Clear `localStorage`:
   ```javascript
   localStorage.removeItem('ollamalegion_theme')
   ```
2. Hard refresh (Ctrl+Shift+R)
3. Check whether `localStorage` is enabled in the browser

---

## Getting Help

1. **Check logs first:**
   ```bash
   docker compose logs loadbalancer
   docker compose logs agent
   ```

2. **Verify API health:**
   ```bash
   curl -v http://localhost:18081/api/v1/health
   ```

3. **Enable debug logging:**
   ```bash
   LOG_LEVEL=debug docker compose up
   ```

4. **Report issues:** [GitHub Issues](https://github.com/BarsSky/ollamalegion/issues)
