# Distributing layers across multiple backends (llama.cpp RPC layer split)

> **Version:** 1.0 (2026-10-13)
> **Applies to:** cppworker + C bridge `c/bridge`, image `ollama-legion/cppworker:gpu-*`
> **Related documents:** [`rpc-coordinator.md`](rpc-coordinator.md) (history and live probes),
> [`deployment-stack.md`](../operations/deployment-stack.md), [`ENV_VARS.md`](../api/env-vars.md),
> [`cppworker-model-params.md`](../features/cppworker-model-params.md)

This document answers four questions:

1. [At what level this is implemented — is another container needed?](#1-at-what-level-this-is-implemented)
2. [Is this a separate mode or a backend property?](#2-is-this-a-mode-or-a-backend-plane)
3. [How it works inside (the chain from ENV to layers)](#3-how-it-works-inside)
4. [What is needed, how to run, which parameters, how to verify](#4-what-is-needed-how-to-run-and-verify)

---

## 1. At what level this is implemented

**The split is done by the cppworker ITSELF, the one that loads the model.** There is no
separate image and no separate container "for the split" on the master machine, and none is
required: it is the same process that usually serves `/api/chat` and
`/api/generate`, just with remote ggml devices attached.

```
Машина-мастер (например 192.0.2.20)          Машина-донор (например 192.0.2.10)
┌───────────────────────────────────────┐      ┌──────────────────────────────┐
│ контейнер ol-stack-cppworker-gpu      │      │ контейнер ol-rpc-server      │
│  ./cppworker                          │      │  /app/rpc-server -H 0.0.0.0  │
│   ├─ C-мост c/bridge                  │      │  -p 50052                    │
│   │   └─ ggml_backend_rpc_add_server ─┼─────►│  (отдаёт свои GPU как        │
│   ├─ llama.cpp (этот же процесс)      │ TCP  │   ggml-устройство RPC0)      │
│   │   └─ llama_model_load_from_file   │50052 └──────────────────────────────┘
│   │       devices=[CUDA0, RPC0]       │
│   │       split_mode=LAYER            │
│   └─ HTTP :18091/:18092 (Ollama API)  │
└───────────────────────────────────────┘
```

In total:

| Where | What is needed | Required? |
|---|---|---|
| Master (the one that loads the model) | **nothing extra** — an already running cppworker container + two ENVs | — |
| Donor (whose GPU is used) | the **`rpc-server`** container from the same cppworker image | yes, if someone else's GPU is needed |
| Local probe without a second machine | the same sidecar next to the master | only for a functional check |

Why a sidecar and not "just open the port": llama.cpp can use someone else's
GPU **only** through its own `rpc-server` protocol (`ggml_backend_rpc_*`). Neither
CUDA, nor NCCL, nor docker "GPU sharing" participates here. That is why exactly
`rpc-server` is brought up on every donor machine — the binary lives in the same
cppworker image, so that both sides of the protocol come from the same llama.cpp
commit (the protocol is version-sensitive).

Starting the sidecar: [`deployments/docker-compose.rpc-node.yml`](../../deployments/docker-compose.rpc-node.yml).

---

## 2. Is this a mode or a backend plane?

**A backend plane.** The split is a property of a particular cppworker
("which devices I see and how I distribute layers across them"), not of the
balancer's operating mode. The balancer knows nothing about the split:
it sees one ordinary llama.cpp backend that can do more than fits into one card.

Hence the practical consequences:

* `operatingMode = rpc_coordinator` (mode B) is **not needed** for the split. That is
  a different function — coordinating external workers with a distributed model
  (see [`rpc-coordinator.md`](rpc-coordinator.md)); enabling it for the sake of layer split
  is a common mistake.
* `balancing.rpcCoordinator.enabled`, split groups and
  `POST /api/v1/placement/validate` are about placement planning, not about the
  `rpc-server` transport.
* Switching `operatingMode` **neither enables nor disables** the split
  (since R112 the mode does not control backend types or the engine at all: see
  [`backend-type-isolation.md`](backend-type-isolation.md)).

---

## 3. How it works inside

The "ENV → devices → layers" chain:

| # | Step | Where | What happens |
|---|---|---|---|
| 1 | ENV | `cmd/cppworker/main.go:302-327` | `CPPWORKER_RPC_ENABLE`, `CPPWORKER_RPC_SERVERS`, `CPPWORKER_SPLIT_MODE`, `CPPWORKER_TENSOR_SPLIT`, `CPPWORKER_MAIN_GPU` land in `ModelConfig` |
| 2 | Connecting nodes | `c/bridge/bridge.c` → `bridge_collect_rpc_devices` | for each `host:port` `ggml_backend_rpc_add_server` is called; the device is cached by endpoint (a repeated load does not reconnect) |
| 3 | Collecting devices | `bridge_collect_devices` | one list: `[CUDA0, CUDA1, …, RPC0, RPC1, …]` (`is_remote` = 1 for RPC) |
| 4 | Planning | `internal/cppbackend/gpu_distribution.go` → `CalculateTensorSplitWithRPC` | proportions by the **free** memory of all devices (min 5 % per device), `main_gpu = 0` |
| 5 | Passing to llama.cpp | `internal/cppbackend/backend.go` → `bridge_load_model` | `llama_model_params.devices` = local (except CPU) + RPC; `split_mode = LAYER` |
| 6 | Distribution | inside llama.cpp | layers are cut by `tensor_split`; the expected log is `llama_prepare_model_devices: using device …` × N |

A live log of a successful split (gemma-4-E4B, `gpuLayers=42`, ctx=4096, kv=q4_0,
two "devices" — a local RTX 3070 and the sidecar):

```
[bridge] RPC: узел host.docker.internal:50052 подключён, его устройств: 1
[bridge] RPC: устройств в раскладке 2 (локальные + удалённые)
tensor split calculated (local + RPC): localGPUs=1, rpcGPUs=1, ratios=[0.536, 0.464]
llama_prepare_model_devices: using device CUDA0 (NVIDIA GeForce RTX 3070) - 7097 MiB free
llama_prepare_model_devices: using device RPC0 (host.docker.internal:50052) - 7097 MiB free
```

| Device | Layers |
|---|---|
| CUDA0 (local RTX 3070) | 23 |
| RPC0 (node) | 19 |
| CPU | 1 |

The capacity planners also see remote memory: `MemfitBudget: ёмкость
RPC-узлов добавлена в бюджет`, `EnvironmentProfile: ёмкость RPC-узлов
добавлена`, the device snapshot is cached for `rpcDeviceTTL = 30s`
(`internal/cppbackend/memfit_adapter.go`).

---

## 4. What is needed, how to run and verify

### 4.1 What is needed

* **A cppworker image built with RPC** (`GGML_RPC=ON`). An ordinary CUDA image without
  RPC will build, but there will be no `rpc-server` in it; the check is `docker run --rm
  <image> ls /app/rpc-server`.
* **The same image tag on the master and the donor.** The RPC protocol is sensitive to
  the llama.cpp commit; different tags = `version_mismatch` (a dry run of
  `POST /api/v1/placement/validate` shows this).
* **Network master → donor, TCP 50052** (on the donor the port is published by compose; the
  firewall must let it through). Check from the master:
  `powershell -File scripts\stand-ports.ps1 -Action Probe -Target <ip> -Ports 50052`
  → expect `CONNECTED`.
* **A model that does NOT fit entirely into the local card.** Otherwise there is nothing
  to distribute (see pitfalls 4.4).

### 4.2 Running on the donor

```powershell
# on the donor machine, from the deployments directory
printf 'CPPWORKER_GPU_TAG=rpc86-retry-r111\nRPC_PORT=50052\n' > .env.rpc
docker compose -f docker-compose.rpc-node.yml --env-file .env.rpc up -d
docker logs ol-rpc-server --tail 20   # expect a line about listening on 0.0.0.0:50052
```

### 4.3 Running on the master

In `deployments/.env` (the values are examples):

```dotenv
CPPWORKER_RPC_ENABLE=true
CPPWORKER_RPC_SERVERS=192.0.2.10:50052      # csv: host:port[,host:port]
CPPWORKER_SPLIT_MODE=1                          # 1 = LAYER (splitting into layers)
CPPWORKER_MAIN_GPU=0
CPPWORKER_GPU_LAYERS=99                         # not less than the model's total layers
# CPPWORKER_TENSOR_SPLIT=0.6,0.4                # usually NOT needed: computed from free VRAM
```

```powershell
cd deployments
docker compose -f docker-compose.stack.yml up -d cppworker-gpu
docker logs -f ol-stack-cppworker-gpu   # look for "RPC: узел … подключён" and "using device RPC0"
```

### 4.4 Parameters

| Variable | Value | Meaning |
|---|---|---|
| `CPPWORKER_RPC_ENABLE` | `true` / `false` | Enables the RPC transport in the bridge. Without it `CPPWORKER_RPC_SERVERS` is ignored |
| `CPPWORKER_RPC_SERVERS` | `host:port[,host:port]` | Donor nodes. Each one = a separate ggml device `RPC0`, `RPC1`, … |
| `CPPWORKER_SPLIT_MODE` | `-1` auto / `0` none / `1` layer / `2` row | Splitting across devices needs `1` (layer). `0` = everything on one device |
| `CPPWORKER_MAIN_GPU` | `0` (index among `devices`) | The main device; with `devices=[CUDA0, RPC0]` it is the local card |
| `CPPWORKER_TENSOR_SPLIT` | `0.6,0.4` | Explicit proportions. Empty = computed from the free VRAM of all devices (min 5 %) |
| `CPPWORKER_GPU_LAYERS` | a number | How many layers to give to the GPU. **Must exceed what fits locally** — otherwise memfit will cut it down to the local volume and there will be nothing to split |
| `CPPWORKER_AUTO_OFFLOAD` | `true` / `false` | A different switch (auto offload); does not affect the split |
| `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD` | `true` / `false` | Tuning the context to the memory; it counts capacity with RPC taken into account, but may reduce n_ctx |
| `RPC_PORT` (donor) | `50052` | The published port of the sidecar |

The port `50052` is not a contract but a default value in compose and in the stand's
firewall rules; it must be changed **in pairs** (the donor's compose and `CPPWORKER_RPC_SERVERS`).

### 4.5 Checking that the split really happened

```powershell
# 1. the whole functional probe (loads the model and prints the layout)
powershell -File scripts\rpc-shard-load.ps1 -Model gemma-4-E4B-it-Q4_K_M.gguf
```

The signs of success — all four at once:

1. `[bridge] RPC: узел <host:port> подключён, его устройств: N`;
2. `llama_prepare_model_devices: using device RPC0 …` (the device really
   participates, not just connected);
3. `tensor split calculated (local + RPC): … ratios=[…]` — proportions with a non-zero
   share for each device;
4. in the layer table (`CUDA0 / RPC0 / CPU`) the RPC device has a **non-zero** number of
   layers.

If `RPC0` is connected but has 0 layers — see the pitfalls below.

### 4.6 Pitfalls (all verified on a live stand)

| Symptom | Cause | What to do |
|---|---|---|
| `using device CUDA0` and that is all, although the RPC node is connected | the model (or the reduced `gpu_layers`) fits into the local card: `checkVRAMForModel`/`calculateLazyLoadOpts` reduced `gpu_layers` to the local volume | raise `CPPWORKER_GPU_LAYERS`, take a bigger model, check `AUTO_TUNE_NCTX_ON_LOAD` |
| Earlier: there was no split "in principle" | `bridge.ModelConfig.SplitMode` was not filled (mode `NONE`), llama.cpp saw one device | fixed (`cfg.SplitMode = opts.SplitMode`), protected by the test `rpc_split_mode_wiring_r108_test.go` |
| Earlier: `panic: cgo argument has Go pointer to unpinned Go pointer` on load | the `tensor_split` array was not pinned before being passed to C | fixed (`runtime.Pinner`), test `TestR108_TensorSplitArrayIsPinned` |
| **The load is cancelled with the text "RPC-узел раскладки … недоступен"** | R114: the gate before loading found no TCP connection to the node | this is the designed behaviour (better than the `SIGABRT` below). Bring the node up: on the donor `docker compose -f docker-compose.rpc-node.yml up -d`, check `scripts/stand-ports.ps1 -Action Probe -Target <ip> -Ports 50052` |
| **In the log "RPC-узел потерян: модель будет выгружена в простое"** | R114: the node disappeared AFTER loading; the watchdog unloads the model so that the next request does not bring the master down | the node came back → the next request will load the model again (the gate will check the nodes). Nothing needs to be done |
| The master crashes with `SIGABRT: abort` + `signal arrived during cgo execution` | the node was recreated/killed **under a live model**: an RPC protocol break in llama.cpp = `abort()`; the abort arrives at the moment the socket breaks, BEFORE the watchdog tick (verified: the container restarts) | first unload the model (`keep_alive=0`, `/api/models/unload`), then touch the node. R114 removes the neighbouring class of crashes (see below) |
| The sidecar shows `unhealthy` in `docker ps` while RPC works | the image's inherited HEALTHCHECK probes HTTP 18092 | in the donor's compose `healthcheck: disable: true` (already set) |
| The split "works" but it did not get faster | the probe was on a **local** sidecar: both "devices" are the same card, this is a functional check | the §6 acceptance needs a real donor and a TTFT/tok/s measurement |
| **The stand complains `version_unknown` on group nodes** | the node did not report the llama.cpp version (`buildInfo` is empty) | R114: the version is baked into the image (`ARG LLAMA_BUILD`) and goes into registration; in `deployments/.env` keep `CPPWORKER_LLAMA_BUILD` = `git -C c/llama.cpp rev-parse --short HEAD`. Different values on the master and the donor → `version_mismatch` (LevelError) in `POST /api/v1/placement/validate` |

### 4.7 What remains

* **§6 acceptance (performance):** donor `192.0.2.10` on the network, three numbers
  on one model — `single` (local card), `hybrid CPU+GPU`, `network layer
  split` by TTFT and tok/s. Automated in `scripts/rpc-shard-load.ps1`.
* **`nctx_feasibility`:** `max_vram_n_ctx=0` in the report — the RPC capacity is counted in
  the budget, but not in this calculation.
* **The window between the check and the connection.** R114 removes two failure scenarios
  (the node is unavailable before loading — the gate; the node disappeared after loading — the watchdog), but
  if the node dies EXACTLY at the moment of connection inside `bridge_load_model`, `abort()`
  still comes from llama.cpp: it cannot be handled in the C bridge, because
  it is not an error but an `abort()`. The practical conclusion: do not touch the node under a live
  model.


## Pitfalls: bundled node (R123) versus a separate container (R170, 2026-10-17)

A live analysis after a llama.cpp upgrade (pin `0eadefeb`, qwen4exp). Symptom:

```
sched_reserve: RPC0[192.0.2.10:50052] compute buffer size = 542.79 MiB
ggml-rpc.cpp:566: Remote RPC server crashed or returned malformed response
Aborted
```

Checked step by step (loading with `gpuLayers=10`, `ctx=4096`, `kv=q4_0`):

| Configuration | Load | Chat |
|---|---|---|
| bundled node, client and node on ONE machine | ok | **200 in 2.2 s** |
| a separate `rpc-server` container on another machine | ok | **200 in 4.4 s** |
| a separate `rpc-server` with the worker's env file | ok | **200 in 5.0 s** |
| a **bundled** node on another machine | ok | client crash (3/3) |

Conclusion: llama.cpp and the network are fine; what breaks is the bundled node (the worker's
child process, R123) when the client is on another machine. The node itself is healthy: it listens on the port,
`RestartCount=0`, and the log contains only `Accepted client connection / Client connection closed`.

**The working recipe (verified live)**

```powershell
# on the donor node: a separate container, not the bundled node
docker run -d --name rpc-node --runtime nvidia --entrypoint /app/rpc-server `
  -p 50055:50055 <cppworker image> -H 0.0.0.0 -p 50055

# on the master: enable the split onto this node
PUT /api/v1/cppworker/config/update {"rpcEnabled":true,"rpcServers":["192.0.2.10:50055"]}

# load with a limited number of layers on the GPU (otherwise the model will not fit into 2×8 GB)
POST /api/models/load-with-params {"name":"…","contextSize":4096,"gpuLayers":10,"kvCacheType":"q4_0"}
```

Checking the fact — `device_layers_exact` in `/api/models`: the device `RPC0/RPC1` with the node's
address must appear there.

**Why the bundled node is still needed and what to do with it.** It is more convenient (no second
container needed, the port is published by compose, the balancer gives the order itself — R133/R164), but with a
remote client it is currently unusable. Its flags are the same (`-H … -p …`) as for the standalone one,
so one must look not at the arguments but at the process context: cgroup/PID namespace,
the inherited CUDA context of the parent, file descriptors. Until it is fixed, use a standalone
node for splitting between machines.


## Managing an external node (R171, 2026-10-17)

The working recipe from the section above ("standalone node") is now managed properly, not by hand.

### What is enabled where

| Where | What | Why |
|---|---|---|
| node (donor) | `docker compose --profile rpc-node up -d rpc-node` | a separate node container, port `${RPC_NODE_PORT:-50055}` |
| node worker env | `CPPWORKER_RPC_EXTERNAL_ADDR=<host>:${RPC_NODE_PORT:-50055}` | the worker does not spawn its own process but checks the external one |
| balancer | backend role `donor` | the order "the node must be running" (R133/R164) |
| master | `PUT /api/v1/cppworker/config/update {"rpcEnabled":true,"rpcServers":["<host>:<port>"]}` | enable the split onto this node |

An example of updating a node with a single command (image + env in one pass):

```powershell
scripts/upgrade-donor-node.ps1 -NewImage ollama-legion/cppworker:gpu-r171-external-node `
  -TransferImage `
  -ExtraEnv 'CPPWORKER_RPC_EXTERNAL_ADDR=192.0.2.10:50055'
```

### What is visible in the status

`/api/models` on the node:

```json
"rpc_server": {"enabled":true,"running":true,"listening":true,
               "external":true,"addr":"192.0.2.10:50055",
               "notes":["внешний узел доступен (192.0.2.10:50055)"]}
```

`GET /api/v1/backends` on the balancer:

```json
"rpcNode": {"role":"donor","wanted":true,"enabled":true,"running":true,"listening":true,
            "addr":"192.0.2.10:50055","port":50055,"reachable":true}
```

The rule hard-wired here: **the address from the worker's report takes precedence over the balancer's guess**.
Before R171 the balancer probed `host:50052` (its own `rpcNodePort`) and wrote `reachable=false` while
the node was working on 50055, and the master received a wrong `rpcServers` list.

### Boundaries

* The worker **does not start** someone else's container: it has no docker socket, and giving it one
  is the wrong trade. `Stop()` in external mode only clears the wish and writes to the log that
  the operator can shut it down.
* The port is published by the node container, not by the balancer (a property of the host, as in R118).
* The RPC protocol is sensitive to the llama.cpp commit: the node and the master must be from the same
  build. The check is `llama_build`/`llama_build_env`/`llama_build_mismatch` in `/api/info`
  (R167): a mismatch is now visible instead of dissolving into "the versions seem to match".

### Live check (2026-10-17)

Role `donor` → the balancer's order → the worker reported `external=true`,
`addr=192.0.2.10:50055`; the balancer — `reachable=true`; the master loaded the model with the
split (`CPU:33 CUDA0:6 RPC0:4` — 4 layers on the donor card) and **answered 200 in 5.83 s
without crashing** (`RestartCount=0`).


## Node diagnostics: exit status, log, debug mode, auto-restart (R172, 2026-10-18)

### Why it was needed: the client's message lies

During a stand failure the master wrote:

```
ggml-rpc.cpp:566: Remote RPC server crashed or returned malformed response
Aborted
```

This is a **client-side** message, and it covers two different diagnoses. In `ggml-rpc.cpp:35`
the macro `RPC_STATUS_ASSERT` raises it on **any** server response with status false, and the
server closes the session on the very first failed operation (`if (!server.op(...)) return;`,
`ggml-rpc.cpp:1857+`). That is, the following look identical:

| What actually happened | What the operator sees |
|---|---|
| the server crashed (segfault / OOM / abort) | `Remote RPC server crashed…` |
| the operation failed: tensor region outside the buffer, unknown type, allocation refusal | `Remote RPC server crashed…` |

The real cause is printed by the **server** — through `GGML_LOG_ERROR` to stderr. Before R172 this
output went only to the container's `docker logs`, so: (a) when the image is updated the
container is recreated and the log is lost, (b) on a host without access nobody reads it.

### What exists now

| What | Where | Why |
|---|---|---|
| exit status of the previous process (`lastExit`, `lastExitPid`, `lastExitAt`) | node status (`/api/models` → `rpc_server`, `/api/v1/cppworker/config` → `rpcServer`) | distinguishes "the server crashed" from "the server responded false" |
| the last lines of the node's output (`logTail` in the status; the full buffer — in the handler) | `GET /api/v1/cppworker/rpc-server/log?lines=200` | the server-side cause of the failure, with the time of each line |
| `logLines` / `logDropped` | in the same place | "the log was truncated" is a fact, not silence |
| protocol debug mode | `CPPWORKER_RPC_SERVER_DEBUG=1` | `GGML_RPC_DEBUG=1` in the child process: a line-by-line log of every operation |
| auto-restart | `CPPWORKER_RPC_SERVER_RESTART` (**enabled** by default) | parity with `restart: unless-stopped` of a separate container |
| `restarting` / `restartInMs` | node status | "the node is down" ≠ "the node is coming up in 1.5 s" |

### How to enable the bundled node (without a second container)

In the bundled compose the node settings are now passed through from `deployments/.env` (before R172 they
were not there at all, so it was impossible to enable the node from `.env`):

```dotenv
# deployments/.env — the split node inside this same worker
CPPWORKER_RPC_SERVER_ENABLED=true
CPPWORKER_RPC_SERVER_PORT=50052      # must match the port publication in compose
# CPPWORKER_RPC_SERVER_DEBUG=true    # temporarily, for analysis: a line-by-line protocol log
# CPPWORKER_RPC_SERVER_RESTART=0     # disable auto-restart while analysing
```

The port is published by the line `"${CPPWORKER_RPC_SERVER_PORT:-50052}:${CPPWORKER_RPC_SERVER_PORT:-50052}"`
— it is always there, but without an enabled node there is simply no process behind it (the port is
listened to by docker-proxy, not by `rpc-server`: that is not a sign of a working node; the truth is
`running`/`listening` in the status).

Example:

```powershell
curl.exe -s -H "X-API-Token: $token" "http://192.0.2.10:18092/api/v1/cppworker/rpc-server/log?lines=50"
```
```json
{"enabled":true,"running":false,"external":false,"pid":0,"addr":"0.0.0.0:50052",
 "lastExit":"signal: segmentation fault (core dumped)","lastExitPid":4242,
 "lastExitAt":"2026-10-18T07:41:52Z","restartEnabled":true,"restarting":true,
 "restartInMs":1500,"debug":false,"logLines":812,"logDropped":0,
 "lines":[{"at":"2026-10-18T07:41:51.9Z","text":"Accepted client connection"},
          {"at":"2026-10-18T07:41:52.1Z","text":"ggml-rpc: [server] tensor region out of buffer bounds"}]}
```

How to read the result:

* `lastExit` = `signal: …` → the process **died**; look at the signal (`segmentation fault` —
  a crash in the kernel/backend, `killed` — the OOM killer, `abort` — `GGML_ABORT`);
* `lastExit` = `exit status N` → the process terminated by itself (`rpc-server` exits if it could not
  create the socket/bind);
* `lastExit` is empty and the client still crashed → the server is **alive**, the operation returned `false`;
  the cause is in `lines` (tensor region outside the buffer = protocol desync/incompatible build;
  allocation refusal = not enough VRAM).

### Boundaries and what is honestly unknown

* The capture duplicates output to the container's stdout, so `docker logs` works as
  before; the buffer is an addition, not a replacement.
* The log tail gets into the **status** only when there is something to look at (the node is down,
  it has already crashed, or debug is enabled) — otherwise every balancer poll would drag the log along.
* Auto-restart is limited to five times per hour with a growing pause (0.5 → 1 → 2 → 4 s, maximum 30 s):
  a cycle of endless crashes on an 8-gigabyte card itself becomes a cause of failures.
  The cause is not hidden by this — `lastExit` and `logTail` remain in the status.
* `GGML_RPC_DEBUG` is voluminous: during model loading it is tens of thousands of lines, so it is
  off by default.
* The restart itself **does not cure** the failure: if an operation fails because of a lack of VRAM,
  the node will come up and crash again — the budget is what limits this.


## Diagnosing the split after reload (R176-R179, 2026-10-19)

**What it was.** "Apply profile" from the WebUI reloaded the model **without the node**: the layers
moved to the local card. The real cause was that reload did not pass `SplitMode`,
so the bridge received `0 = LLAMA_SPLIT_MODE_NONE`, and with NONE llama.cpp leaves only
`main_gpu` from the device list and discards the RPC node. A fresh load did pass the mode
— that is why the split worked there and disappeared on reload.

**How the fact is now checked (not a guess).**

| Field / message | Where | Meaning |
|---|---|---|
| `layout_degraded`, `layout_degraded_reason` | `GET /api/models` (model) | the nodes were passed but none became a model device: there is NO split, the reason is named |
| `раскладка: какие устройства видит планировщик` | worker log | `local_gpus`, `remote_devices`, `active_servers`, `admitted_servers`, `snapshot_devices` |
| `узел раскладки готов принять загрузку` | worker log | the node answered a real protocol command (not a TCP-connect) before loading |
| `using device RPC0` | llama.cpp log | the ONLY proof that the node participates in loading |

The reading rule: if `layout_degraded=true` — look at `layout_degraded_reason`; if
there is none and the speed dropped — look for `using device RPC0` in the log: its absence with
servers passed is exactly the loss of the split.

**Boundaries.** The node list and the split proportions on reload are taken from the previous instance
(recalculated when the number of devices changes). A node serves one connection —
a short pause and a readiness check are set before loading, which reduces but does not
eliminate the race when a client connects at the same time.
