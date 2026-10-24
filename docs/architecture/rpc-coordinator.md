# RPC Coordinator Mode

Production-ready distributed inference mode (P.1) for Ollama Legion balancer.

> **This is NOT the same thing as splitting layers across devices (layer split).**
> The `rpc_coordinator` mode is about the balancer orchestrating external workers. The real
> distribution of a model's layers across several GPUs (including other machines) is done
> inside the cppworker via llama.cpp RPC and **does not require enabling this mode** — it
> belongs to the backend plane. See [`layer-sharding.md`](layer-sharding.md).

## What this is

`rpc_coordinator` mode routes inference requests through `ModelCoordinator`,
which orchestrates a pipeline of multiple workers. Each worker serves a
slice of the model (a range of layers). This provides:

- **Distribution of model layers** across multiple workers (pipeline parallelism).
- **Failover** between candidates per-slice via selector.
- **Circuit breaker** per worker (protection against cascading failures).
- **Streaming SSE passthrough** for both Ollama and OpenAI formats.
- **Auth middleware** — token-based protection for inference paths.

## Architecture

```
Client
  ↓ POST /api/generate
Balancer (port 8080)
  ↓ rpc_coordinator mode + dispatcher set + path match
RpcCoordinatorDispatcher
  ↓ checkAuth → ShouldRoute (model registered + CB not all Open) → coordinator.Infer/InferStream
ModelCoordinator
  ↓ SelectWorkersForSlice(slice) → InferSlice per worker
Worker 1 (slices 1-16)  ←→  Worker 2 (slices 17-32)
  ↓/↑ result aggregated
Coordinator
  ↓ formatted response per endpoint
Dispatcher
  ↓ response written
Client
```

## Configuration

### Operating mode

```json
{
  "balancing": {
    "operatingMode": "rpc_coordinator",
    "rpcCoordinator": {
      "enabled": true,
      "embedded": true,
      "failoverPolicy": "circuit_breaker",
      "requestTimeout": 30000000000,    // 30s in nanoseconds
      "streamTimeout": 300000000000,     // 5min
      "circuitBreaker": {
        "failureThreshold": 5,
        "successThreshold": 1,
        "resetTimeoutMs": 30000
      },
      "workers": [
        // Optional pre-registered workers. Normally they register dynamically via heartbeat.
        "worker-1:8001",
        "worker-2:8001"
      ]
    }
  }
}
```

Parameters:

| Field | Default | Description |
|---|---|---|
| `enabled` | `false` | Enables rpc_coordinator mode. Requires `operatingMode=rpc_coordinator`. |
| `embedded` | `false` | `true` = spawn `ModelCoordinator` inside the balancer. `false` = external. |
| `failoverPolicy` | `"circuit_breaker"` | `circuit_breaker` / `retry` / `fail_fast`. |
| `requestTimeout` | `30s` | Non-streaming inference timeout. |
| `streamTimeout` | `5min` | Streaming inference timeout. |
| `circuitBreaker.failureThreshold` | `5` | Number of failures before Open. |
| `circuitBreaker.successThreshold` | `1` | Successes in HalfOpen → Closed. |
| `circuitBreaker.resetTimeoutMs` | `30000` | Time spent in Open before the HalfOpen probe. |

Types: `enabled` / `embedded` are bool, `requestTimeout` / `streamTimeout` are Go durations,
`workers` is a `[]string` of `host:port`, `failoverPolicy` is a string. With
`embedded=false` the balancer talks to an external coordinator whose URL comes from
`coordinatorUrl` (default `""`); the `workers` array (default `[]`) pre-registers worker
URLs, while normally workers register themselves dynamically.

### Worker registration

Workers are registered dynamically via the `RegisterWorker` API, or
pre-registered through the `workers` array in the config. Each worker has:

```json
{
  "workerId": "worker-1",
  "host": "127.0.0.1",
  "port": 8001,
  "sliceLayers": "1-16"
}
```

### Distributed model

Each model is registered in the coordinator via `RegisterDistributedModel`:

```go
coord.RegisterDistributedModel("llama-70b", "Distributed 70B",
    []rpccoordinator.LayerSlice{
        {StartLayer: 1, EndLayer: 40, WorkerID: "worker-1"},
        {StartLayer: 41, EndLayer: 80, WorkerID: "worker-2"},
    })
```

This can be done through the `/api/v1/rpc/models` API endpoint or in code.

## Supported endpoints

RPC coordinator mode intercepts the following endpoints (when enabled + the model is
registered with the coordinator); each of them is a `POST`:

| Path | Format | Streaming |
|---|---|---|
| `/api/generate` | Ollama | ✓ |
| `/api/ollama/generate` | Ollama (alias) | ✓ |
| `/api/chat` | Ollama | ✓ |
| `/api/ollama/chat` | Ollama (alias) | ✓ |
| `/v1/chat/completions` | OpenAI | ✓ (SSE + `[DONE]`) |
| `/v1/completions` | OpenAI legacy | ✓ (SSE + `[DONE]`) |

The dispatcher also accepts internal endpoints: `POST /api/v1/rpc/models` — register a
distributed model; `POST /api/v1/rpc/workers` — workers visibility (legacy).

## Streaming format

For streaming requests (`stream=true`) the Ollama paths emit NDJSON chunks and the OpenAI
paths emit SSE events plus a `[DONE]` terminator. Per-slice stats come back in the response
header: `X-Rpc-Slice-Stats: [{slice_id, worker_id, latency_ms, success}, ...]`.

### Ollama `/api/generate` (stream=true)

```
data: {"model":"llama-70b","response":"Once","done":false}

data: {"model":"llama-70b","response":" upon","done":false}

data: {"model":"llama-70b","response":"","done":true,"done_reason":"stop","total_duration":1234567}

```

### OpenAI `/v1/chat/completions` (stream=true)

```
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":...,"model":"llama-70b","choices":[{"index":0,"delta":{"content":"Once"}}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":...,"model":"llama-70b","choices":[{"index":0,"delta":{"content":" upon"}}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":...,"model":"llama-70b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

```

## Error responses

| Status | Error type | When |
|---|---|---|
| 400 | `json_parse_error` | Invalid JSON body |
| 400 | `missing_model` | `model` field is empty |
| 401 | `unauthorized` | Auth enabled, token missing/invalid |
| 404 | `model_not_distributed` | Model not registered in coordinator |
| 499 | `client_disconnected` | Client closed connection mid-stream |
| 502 | `upstream_error` | Worker returned unexpected error |
| 503 | `coordinator_disabled` | `RpcCoordinator.Enabled=false` |
| 503 | `coordinator_not_initialized` | Dispatcher coord is nil |
| 503 | `all_workers_unhealthy` | All workers have Open CB (Session 3.3) |
| 503 | `streaming_unsupported` | `http.Flusher` not available |
| 504 | `timeout` | Inference exceeded `requestTimeout`/`streamTimeout` |

OpenAI endpoints use the `{"error":{"message","type"}}` format; Ollama
uses `{"error":"<message>"}`.

Backend status → dispatcher response:

- 2xx → 200 + body passthrough (success).
- 4xx (except 408, 429) → passthrough: a client error, do not retry.
- 408, 429 → 502 (retryable).
- 5xx → 502 `upstream_error` (retryable — try the next candidate).
- Network error → 502 (connection refused, timeout).
- Timeout → 504 `request_timeout` (per `requestTimeout` / `streamTimeout`).
- All workers CB Open → 503 `all_workers_unhealthy` (degraded).
- Model not registered → 404 `model_not_distributed` (config error).
- Client disconnect → 499 `client_disconnected` (the user closed the connection).

## Auth

If `auth.enabled=true` in the config, the dispatcher checks the token before
processing the request:

```
Authorization: Bearer <token>
# or
X-API-Token: <token>
# or (for WebSocket-style clients)
?token=<token>
```

Auth applies to all `/api/*` and `/v1/*` rpc_coordinator paths.

- Enforced when `auth.enabled=true` in the balancer config
- Per-endpoint error format: Ollama `{"error": "..."}`, OpenAI `{"error": {"message": "...", "type": "unauthorized"}}`

## Circuit Breaker

Per-worker circuit breaker. It is created lazily in `getOrCreateCircuitBreaker`
on the first request.

State machine:
- **Closed** — normal operation. `RecordFailure` increments the counter;
  at `failureThreshold` → Open.
- **Open** — all requests are blocked. After `resetTimeout` → HalfOpen.
- **HalfOpen** — lets exactly 1 probe request through. Success → Closed,
  Failure → Open (the timeout restarts).

In short: **Closed** → (N failures) → **Open** → (cooldown) → **HalfOpen** → (success) → **Closed**.

Recording: after every `coordinator.Infer` / `coordinator.InferStream` the
dispatcher calls `recordCBFromStats(sliceStats)`:
- `slice.Success == true` → `cb.RecordSuccess()`
- `slice.Success == false` → `cb.RecordFailure()`

On a request the dispatcher records CB state from the `X-Worker-Stats` header and, for each
slice, picks the candidate via `selector.SelectWorkersForSlice(slice)`, skipping workers
whose CB is `Open`, and updates the CB from the response status afterwards.

`ShouldRoute` intercepts a request only if ALL of these are true:

1. `mode == rpc_coordinator` (operating mode is set)
2. `dispatcher != nil` (dispatcher is wired)
3. Path matches an Ollama or OpenAI inference path
4. Model is registered with the coordinator
5. **At least one** worker's CB is not `Open`

On a request, `ShouldRoute` decides:

- If the model is not registered → 404 `model_not_distributed`
- If all workers are Open → 503 `all_workers_unhealthy`
- Otherwise → the request goes through

Failover behavior in the dispatcher:

- Worker A returns 500 for slice 1 → try the next candidate for slice 1.
- Worker A times out (no response) → try the next candidate, 502 if all fail.
- Worker A disconnects mid-stream → the client sees a partial response, no retry (the stream has already started).
- Slice 1 fails on ALL workers → 502 `all_slices_failed`.
- Worker B returns 200 for slice 2 → aggregate with the slice 1 result.

The coordinator's `/rpc/health` endpoint (per worker) returns `200 OK` — healthy, or
`503 Service Unavailable` — unhealthy. The dispatcher queries each worker's health before
routing; if all workers are unhealthy it returns `503 all_workers_unhealthy`.

## Monitoring

Every request returns SliceStats in the `X-Rpc-Slice-Stats` header (or in
the response body for non-streaming):

```json
[
  {"slice_id": "1-16", "worker_id": "worker-1", "latency_ms": 234, "success": true},
  {"slice_id": "17-32", "worker_id": "worker-2", "latency_ms": 198, "success": true}
]
```

Circuit breaker state can be inspected via the `/api/v1/rpc/workers` API
(legacy endpoint, may require extension for CB visibility).

Per-slice Prometheus counters in `internal/rpccoordinator/metrics.go`:
- Total requests per slice
- Latency histogram per worker
- CB state transitions
- Streaming chunks sent
- Failover attempts

## Known limitations

- **Pipeline streaming is simplified**: slice 2 receives `"prompt + accumulated
  tokens"` as input, not a real `last_hidden_state` tensor. In production
  with a real llama.cpp a KV-cache merge is needed (deferred to P.3).
  It is suitable for models where downstream layers can be conditioned on text only.
- **Streaming in stub workers**: `WorkerClient.InferSliceStream` parses SSE
  via `bufio.Scanner`; for >1MB payloads it may be slow.
- **CB does not cover HealthCheck**: only Infer / InferStream errors
  trigger CB transitions. Worker-level health is a separate signal.
- **No chat template**: `flattenPrompt` joins messages as
  `"role: content\n"`. A real chat template from the coordinator is Phase 9.
- **Layout transport is not wired up** (see the next section): the Go side
  is ready, the C side does not read `rpc_servers` yet. (Updated 2026-10-11: the C side
  is wired up — see below.)
- **Single coordinator**: only one `ModelCoordinator` per balancer (no HA).
- **No automatic model distribution**: the model must be pre-registered via API or config.
- **Workers must be pre-registered**: no dynamic discovery (yet).

## What remains for the real transport (S1 of the 2026-10-07 plan)

This section is an exact map of the remaining work: it requires a CUDA build and two
machines, so it could be neither written nor verified in a session without a stand.

### What is already done (Go side, covered by tests)

| Layer | Artifact |
|---|---|
| Variables | `CPPWORKER_RPC_SERVERS` (`host:port,host:port`), `CPPWORKER_RPC_ENABLE`, `CPPWORKER_MAIN_GPU`, `CPPWORKER_LLAMA_BUILD` — parsing and validation: `cmd/cppworker/rpc_env_r101.go` |
| Config | `cppbackend.Config.RPCServers / RPCEnabled / LlamaBuild` |
| Bridge | `bridge.ModelConfig.RPCServers` is filled in `LoadModelWithOpts` (`applyRPCServersR105`) |
| Build version | `CPPWORKER_LLAMA_BUILD` → registration → `types.Backend.BuildInfo` → check of the RPC side compatibility in `POST /api/v1/placement/validate` (`version_mismatch`) |
| Balancer | logical backend (group), `strategy: sharded`, routing to the master, fit over the SUM of the nodes — all done and covered by tests |

Until the transport is built, an enabled RPC is **honestly logged as non-working**, both at
startup and on every model load: a silent setting would leave the operator guessing why the
second machine is idle.

### What has to be done in the C bridge (`c/bridge`) — DONE (2026-10-11)

1. **Structure.** ✅ `ModelConfig` in `bridge.h` got `rpc_endpoints` /
   `rpc_endpoints_len`; it is filled from `RPCServers` in `bridge.go`
   (`fillRPCEndpointsR108`).
2. **Node registration BEFORE the context is created.** ✅ `bridge_collect_rpc_devices`
   (bridge.c) registers every endpoint through the RPC registry
   (`ggml_backend_reg_get_proc_address(reg, "ggml_backend_rpc_add_server")`) and
   is called before `llama_model_load_from_file`.
3. **Devices.** ✅ The assembled NULL-terminated array (local non-CPU devices
   + devices of the RPC registries) is passed into `llama_model_params.devices`; the array
   is freed right after loading.
4. **Diagnostics.** ✅ Every node is logged separately ("connected / NOT
   connected"); a llama.cpp build without RPC and unreachable nodes produce a
   clear message on stderr instead of a model-load timeout.
5. **Version.** ⏳ Remaining: report the real llama.cpp commit from the runtime
   (`CPPWORKER_LLAMA_BUILD` is still set by the image, not by the build itself).

### What has to be done in the packaging — DONE (2026-10-11)

- **cppworker image:** ✅ `-DGGML_RPC=ON` in `Dockerfile.gpu` and `Dockerfile.gpu.86`,
  `rpc-server` is copied into the runtime layer (`/app/rpc-server`) by the same commit and
  with the same `CUDA_ARCHITECTURES` as `libllama`.
- **Compose:** ✅ `deployments/docker-compose.rpc-node.yml` — the node sidecar.
  An important detail: the image has `ENTRYPOINT=/entrypoint.sh`, which always starts
  `./cppworker`, so the sidecar overrides the **entrypoint**, not `command`.
  The old `docker-compose.rpc.yml` is marked as a historical scaffold of a different scheme.
- **Version:** ⏳ Remaining (see item 5 above).

### Build pitfalls with RPC (verified on a live build, 2026-10-11)

Three things, each of which kills the build AFTER the multi-hour CUDA stage has already
finished. Worth knowing before starting:

1. **The binary is called `ggml-rpc-server`, not `rpc-server`.** The target is declared in
   `c/llama.cpp/tools/rpc/CMakeLists.txt` (`set(TARGET ggml-rpc-server)`), although
   llama.cpp's documentation calls it rpc-server. It is copied into the image as
   `/app/rpc-server`; in COPY you must use the original name, otherwise `rpc-server: not found`.
2. **`-DGGML_RPC=ON` requires `-lggml-rpc`.** The backend registry inside `libggml.a`
   references `ggml_backend_rpc_reg`, and without the library linking fails:
   `undefined reference to 'ggml_backend_rpc_reg'`. The flag has been added to `CGO_LDFLAGS`.
3. **`libggml-rpc.a` lives in a subdirectory** `build/ggml/src/ggml-rpc/`, so an
   `-L` pointing at `build/ggml/src` is not enough — a separate `-L` for that subdirectory is
   needed, otherwise `cannot find -lggml-rpc`.

On top of that, a build of the **CPU variant** (`Dockerfile.cpu`) fails if the build is
started detached (WMI / no login session): Docker cannot fetch the credentials from the
Credential Manager (`A specified logon session does not exist`). The GPU image builds fine
meanwhile, and the CPU variant is not needed for an RPC node anyway.

Separately: `rpc-server` itself prints a warning on startup that the mode is experimental and
**insecure**, and asks not to expose it to an open network. That is why the firewall rule
`OllamaLegion-50052` is limited to its own subnet (`-RemoteAddress LocalSubnet`), and the same
should be kept in mind when moving a node outside the trusted network.

### Sequence of work and acceptance criteria

1. `iperf3 -c <node> -t 30 -P 4` ≥ 900 Mbit/s (everything further is counted from this
   figure, see §3 of the plan). ✅ **Done 2026-10-11, but not with iperf3** (the Cygwin build on
   Windows 26300 fails on the control message): measured with .NET sockets —
   `scripts/measure-network.ps1`, 92–110 MB/s.
2. Build the image with `rpc-server`, bring it up on the second machine; check that
   `POST /api/v1/placement/validate` for the group sees the open port (`rpc_unreachable`
   disappears). ⏳ The image builds (`-DGGML_RPC=ON`), the sidecar is `docker-compose.rpc-node.yml`.
3. Load a model that does not fit into 8 GB with `CPPWORKER_RPC_SERVERS=<second node>`
   and `CPPWORKER_SPLIT_MODE=1` (layer); record TTFT and tok/s. ⏳ **Done
   partially on 2026-10-13 — the transport works, the layout does not exist yet; details in
   the "Live probe of the RPC transport (2026-10-13)" section below.**
4. Compare three numbers on one model: `single`, `hybrid CPU+GPU` (one machine),
   `network layer split` — this is the acceptance criterion of §6 of the plan.
5. Check the build divergence: a node with an old commit → `version_mismatch` in a
   dry run (not a load timeout).

### Live probe of the RPC transport (2026-10-13)

Stand: master — this host (Ryzen 7 PRO 5750G, RTX 3070 8 GB, 24.5 GiB RAM),
donor — `192.0.2.10` (`CYBORG`, RTX 5060 Laptop 8 GB, sm_120). The master was started
with `CPPWORKER_RPC_ENABLE=true`, `CPPWORKER_RPC_SERVERS=192.0.2.10:50052`,
`CPPWORKER_SPLIT_MODE=1`; the model — `Qwen3.8-27B-UD-Q4_K_M.gguf` (15.33 GB, 64
layers, `qwen35` architecture, an attention/SSM hybrid), which does not fit into 8 GB.

**What was confirmed — the transport is alive, the node is visible to llama.cpp** (the
binary prints its diagnostics in Russian, so the log lines are quoted verbatim):

```
[bridge] RPC: узел 192.0.2.10:50052 подключён, его устройств: 1
[bridge] RPC: устройств в раскладке 2 (локальные + удалённые)
[bridge] RPC: список устройств передан в llama_model_load_from_file (устройств 2)
{"msg":"RPC-раскладка передана в мост","servers":["192.0.2.10:50052"]}
```

That is, the C part (`bridge_collect_rpc_devices` + passing the device array before
`llama_model_load_from_file`) works on a live build as well.

### Result of the live layout check (2026-10-13, after the fixes)

The donor `192.0.2.10` was unavailable by the time of the check (the machine does not
respond: ARP `Incomplete`, all ports closed), so the transport was verified against the
**local sidecar of the same image**:

```
cd deployments
CPPWORKER_GPU_TAG=rpc86-reasoning-r108e docker compose -f docker-compose.rpc-node.yml up -d
# cppworker: CPPWORKER_RPC_SERVERS=host.docker.internal:50052, CPPWORKER_SPLIT_MODE=1
```

This is a functional probe (both "devices" are the same card, so it shows no
performance), and it confirms the whole chain:

```
[bridge] RPC: узел host.docker.internal:50052 подключён, его устройств: 1
[bridge] RPC: устройств в раскладке 2 (локальные + удалённые)
llama_prepare_model_devices: using device CUDA0 (NVIDIA GeForce RTX 3070) - 7097 MiB free
llama_prepare_model_devices: using device RPC0 (host.docker.internal:50052) - 7097 MiB free
```

Layer layout (gemma-4-E4B-it-Q4_K_M, `gpuLayers=42`, ctx=4096, kv=q4_0):

| Device | Layers |
|---|---|
| CUDA0 (local RTX 3070) | 23 |
| RPC0 (node) | 19 |
| CPU | 1 |

The proportions matched the scheduler's calculation (`tensor split calculated (local + RPC):
localGPUs=1, rpcGPUs=1, ratios=[0.536, 0.464]`); `MemfitBudget: ёмкость RPC-узлов
добавлена в бюджет` and `EnvironmentProfile: ёмкость RPC-узлов добавлена`
confirm that both schedulers see the capacity of the node.

**What remains for full acceptance (§6):** bring the donor back into the network
(`CPPWORKER_RPC_SERVERS=192.0.2.10:50052`) and take three numbers on one model —
`single` (local card, 20 layers), `hybrid CPU+GPU`, `network layer split` —
by TTFT and tok/s. The probe is automated: `scripts/rpc-shard-load.ps1`.

### A live fact about node failure (2026-10-13)

Recreating the node (the `rpc-server` container) **while the master holds a live connection**
crashes the master: in the log `SIGABRT: abort` + `signal arrived during cgo execution` during
`bridge_load_model`, after which the cppworker restarts (policy
`restart: unless-stopped`) and the model ends up not loaded. This is llama.cpp RPC
behavior: a protocol break on the remote backend leads to `abort()`, not to a
load error that could be handled.

Practical takeaway for the stand: do not recreate a node under a live model —
unload the model first (`keep_alive=0` / `/api/models/unload`), then change the
node. What is worth doing next (a separate task): a health gate for the node before
loading and auto-unloading of the model when the node is lost, so that a node failure does not look
like a master crash.

**What is missing — the number of layers does not reach llama.cpp.** It is trimmed by TWO
Go schedulers, each counting free memory only on the LOCAL card:

| # | Where | What is visible in the log |
|---|---|---|
| 1 | `internal/cppbackend/backend.go:1651` (`checkVRAMForModel` → memfit) | `auto-adapting GPU layers for model (memfit): requestedGPULayers=48 optimalGPULayers=20` |
| 2 | `cmd/cppworker/lazyload_calc.go:271` (`calculateLazyLoadOpts`, with `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD=true`) | `applying partial offload: requested_gpu_layers=48, reduced_gpu_layers=16` |
| 3 | `internal/cppbackend/gpu_distribution.go:183` | `tensor split calculated (vram-ratio) gpuCount=1 ratios=[1] totalFreeMB=8191` |

The result in the bridge and in llama.cpp:

```
[bridge] loading model: .../Qwen3.8-27B-UD-Q4_K_M.gguf (ctx=8192, batch=512, threads=6, gpu_layers=20)
llama_prepare_model_devices: using device CUDA0 (NVIDIA GeForce RTX 3070) (0000:01:00.0) - 7097 MiB free
```

`llama_prepare_model_devices` sees a single device and there is nothing to lay out: 20
layers fit into the local 7 GB, so the remote node sits idle.
Setting `CPPWORKER_AUTO_OFFLOAD=false` does NOT affect this (it is a different
switch) — the trimming is done by memfit, not by auto-offload.

### The real cause of the "single device" (found 2026-10-13, fixed)

Separate from the schedulers there was a defect that makes the layout impossible **in
principle**: the layout mode never reached the bridge.

`CPPWORKER_SPLIT_MODE=1` was parsed in `cmd/cppworker/handlers_model.go`
(`defSplitMode = 1`) and put into `LoadModelOpts.SplitMode`, but in
`bridge.ModelConfig` that field was never assigned anywhere — the bridge always saw
`split_mode = 0` = `LLAMA_SPLIT_MODE_NONE`. In that mode llama.cpp
(`src/llama.cpp`, `llama_prepare_model_devices`) keeps **only `main_gpu`** out of the passed
device array — hence a single `using device CUDA0` line despite
"устройств в раскладке 2" (2 devices in the layout). A second trap of the same field: the value `-1` ("llama.cpp
default" = LAYER by the `c/bridge/bridge.h` convention) was translated into `0` = NONE,
and the default `defSplitMode := 0` meant NONE — that is, auto tensor split over
local GPUs did not work even without RPC (the proportions were computed, but llama.cpp
ignored them).

What was done:

| What | Where |
|---|---|
| `opts.SplitMode` is propagated to the bridge | `internal/cppbackend/backend.go` |
| Default is `-1` = LAYER; `-1` is not replaced with zero | `cmd/cppworker/handlers_model.go` |
| The bridge exposes the layout device list to the scheduler (`bridge_collect_devices`): name, type, "is it remote", free/total — in the order of `llama_model_params.devices` | `c/bridge/bridge.h`, `bridge.c`, `bridge.go` |
| memfit and the adaptive scheduler see the capacity of the nodes (`+rpc`) | `internal/cppbackend/memfit_adapter.go`, `cmd/cppworker/adaptive_loader.go` |
| tensor split accounts for remote devices, the master stays local | `internal/cppbackend/gpu_distribution.go` |

Regressions are closed by tests: `TestR108_SplitModeReachesBridge`,
`TestR108_SplitModeDefaultIsLayer` (static) and
`TestR108_TensorSplitIncludesRemoteDevices` (proportions 1/3 and 2/3 for 3 GB
local + 6 GB on the node).

**Next step (in increasing order of the amount of work):**

1. **Give the schedulers the VRAM of remote nodes.** The source of truth about which
   devices are actually available is the bridge: it already reports "устройств в раскладке 2".
   A query like `bridge_get_devices()` is needed (type, name, `memory_free`) → into
   `cppbackend` → into `memfit` as additional capacity and into
   `gpu_distribution`/`tensor_split`. Then the verdict will count 48-56 layers instead of 20.
2. **Verify that llama.cpp really takes the second device.** After (1)
   the log must contain two `using device ...` lines (CUDA0 and RPC0) and a distribution of
   layers between them; if the RPC device is still dropped — look at
   `llama_prepare_model_devices` in the `c/llama.cpp` commit in use (the device
   selection conditions and the `tensor_split` requirements).
3. **Compare three numbers** (§6 criterion): `single` (20 layers on the 3070),
   `hybrid CPU+GPU` (the same, but without RPC), `network layer split` (48+ layers on
   3070+5060). Measure TTFT and tok/s by the `prompt_eval_duration` /
   `eval_count` / `eval_duration` fields of a live response.

Useful for probes: the load-time scheduler is disabled by the variable
`CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD=false` (in compose it is now passed —
`deployments/docker-compose.stack.yml`), while memfit is not disabled by anything: it
is exactly the gate that has yet to learn about RPC.

Reference measurement (R108, single card, 20 layers on the 3070, ctx=8192):
`Qwen3.8-27B-UD-Q4_K_M` answers at ~1.1 tok/s, TTFT on prefill ~210 s — that is,
the gain from spreading onto a second GPU is measurable here and worth the work.

### Questions to settle BEFORE implementation (from §11 of the plan)

- Is `rpc-server` a mandatory part of the delivery or an optional compose profile?
- Heterogeneous GPUs (sm_86 + sm_120) — officially allowed or only with a
  warning?
- Worker crash = failure of the whole model: automatic rebuild of the layout, or an honest 503?
- Should N nodes (3–4 machines) be planned for right away instead of 2?

## Examples

### Distributed Llama-70B (2 workers, layers 1-40 + 41-80)

```bash
# 1. Start worker 1
cd worker-1 && ./rpcworker --worker-id=w1 --port=8001 --models-dir=/models/llama-70b
# 2. Start worker 2
cd worker-2 && ./rpcworker --worker-id=w2 --port=8001 --models-dir=/models/llama-70b

# 3. Configure balancer
cat > config.json <<EOF
{
  "balancing": {
    "operatingMode": "rpc_coordinator",
    "rpcCoordinator": {
      "enabled": true,
      "embedded": true,
      "failoverPolicy": "circuit_breaker"
    }
  }
}
EOF

# 4. Start balancer
./balancer --config=config.json

# 5. Register distributed model (via API or startup script)
curl -X POST http://localhost:8081/api/v1/rpc/models \
  -H "Content-Type: application/json" \
  -d '{
    "name": "llama-70b",
    "slices": [
      {"startLayer": 1, "endLayer": 40, "workerId": "w1"},
      {"startLayer": 41, "endLayer": 80, "workerId": "w2"}
    ]
  }'

# 6. Inference
curl -X POST http://localhost:8080/api/generate \
  -d '{"model":"llama-70b","prompt":"Hello"}'
```

### Streaming OpenAI

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model":"llama-70b",
    "messages":[{"role":"user","content":"Hello"}],
    "stream":true
  }'
```

## Migration / fallback

To disable `rpc_coordinator` and return to standard mode:

```json
{
  "balancing": {
    "operatingMode": "standard",
    "rpcCoordinator": {
      "enabled": false
    }
  }
}
```

The scaffold in `Proxy.ServeHTTP` checks `IsRpcCoordinatorMode`; if
`false`, the request flows through the normal proxy path unchanged.

## See also

- [`layer-sharding.md`](layer-sharding.md) — **how layers are actually laid out**
  across several GPUs/machines (llama.cpp RPC): the implementation level, containers,
  parameters, verification, pitfalls.
- `deployments/docker-compose.rpc-node.yml` — the `rpc-server` sidecar on the donor.
- `docs/archive/rounds/phase-8-p1-rpc-coordinator.md` — implementation plan / log.
- `docs/plans/2026-q3-production-ready-plan.md` — overall production-ready plan (§2 — the
  original P.1 plan).
- `CHANGELOG.md` — version history.
- `internal/balancer/rpc_coordinator_dispatcher.go` — implementation.
- `internal/rpccoordinator/coordinator.go` — coordinator.
- `internal/rpccoordinator/worker_client.go` — worker client.
- `internal/rpccoordinator/` — source code.
