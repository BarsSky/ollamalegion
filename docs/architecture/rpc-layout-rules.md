# Layer layout rules (RPC): one commit, its own architecture, an explicit flag

This document records the rules obtained from live runs on two machines
(master `192.0.2.20`, node `192.0.2.10`). This is not theory: every rule
cost a process crash with `SIGABRT`, and every one has an observable symptom.

## 1. A node is admitted only if both the commit AND the architecture match

The layout works on top of `ggml-rpc`: the master and the node exchange
`ggml_tensor` structures and backend commands. Required at the same time:

* **one and the same llama.cpp commit** on both machines;
* **CUDA kernels for each machine's video card** — the node's binary must contain
  its `sm_XX`, the master's binary its own.

Check (both commands must be > 0 and refer to *its own* card):

```bash
docker exec <container> grep -a -c sm_86  /app/rpc-server   # master, RTX 3070
docker exec <container> grep -a -c sm_120 /app/rpc-server   # node,   RTX 5060
```

**Live symptoms of a violation.**

| What was done | What was seen |
|---|---|
| The master's binary was installed on the node (build with only `sm_86`) | Loading gets further (`offloaded 16/66`, buffers `CUDA0 2116 MiB`, `RPC0 2246 MiB`), then `ggml-rpc.cpp:823: Remote RPC server crashed or returned malformed response` → `SIGABRT`. The node itself stays alive and silent. |
| The node with its own build, but of a **different commit** (`gpu-rpc120` from Oct 11 versus `1d2869c6e`) | The same thing: the buffers are allocated, the very first RPC operation is not parsed, the master crashes, and the node has `RestartCount=0`. |

`RPC_STATUS_ASSERT` (`ggml-rpc.cpp:30`) is not "the node crashed" but "the response
was not parsed": llama.cpp answers this with `abort()`, so a predictable failure
without intervention is impossible — the node has to be cut off BEFORE the layout.

**Practice: one image for all machines.** `CUDA_ARCH=86;120` and
`CUDA_VERSION=12.8.0` in `deployments/.env`, a single build that runs on both
the master and the node (`deployments/docker-compose.rpc-node.yml` takes the same
`CPPWORKER_GPU_TAG`). Then both the commit and (given both sets of
kernels are present) the architecture match, and the node starts carrying
`CPPWORKER_LLAMA_BUILD`; otherwise it returns an empty `buildInfo` and the version check is blind.

Two build traps, both of which have already cost time:

* the default `CUDA_ARCH=all` in `docker/cppworker/Dockerfile.gpu` is
  `50;61;70;75;80;86;89;90`, **without `120`**; "all" does not help here;
* the default `CUDA_VERSION=12.2.0` does not know `sm_120` at all; `12.8.0`
  is needed (driver 617.42 on both machines can do this);
* the default mirror `dockerhub.timeweb.cloud` with the `12.8-devel` image (~6 GB)
  practically stalls the build (50+ minutes, almost zero CPU).
  Workaround: `CUDA_BASE=mirror.gcr.io/nvidia/cuda` (the build-arg appeared in R115).

## 2. Turning the layout off is done with a flag, and the flag decides

```bash
CPPWORKER_RPC_ENABLE=false      # no layout: not a single dial to the nodes
CPPWORKER_RPC_SERVERS=          # the list need not be erased — it will be cleared
```

Before R115 a non-empty `CPPWORKER_RPC_SERVERS` **force-enabled** the mode, so
`RPC_ENABLE=false` did not work: the master kept dialing the nodes and
planning by their memory. Now an explicit `false` wins and clears the list;
if the flag is not set, the behaviour is as before (a non-empty list enables the layout).

"Off" means exactly that: the nodes end up neither in the bridge parameters, nor in the
memory budget, nor in the watchdog checks; planning runs on the local card.
The only way to turn the mode off is not to "erase the node list" but to set
the flag.

## 3. Layout capacity: the total and its composition

`available_vram_mb` in `/api/models` is the **total** (local card + nodes), that is how
`memfit` computes it. A single number cannot tell where the gigabytes come from, so
R115 returns the composition in separate fields:

```
rpc_enabled, rpc_servers_active, vram_remote_nodes,
vram_local_free_mb, vram_local_total_mb,
vram_remote_free_mb, vram_remote_total_mb,
vram_layout_free_mb, vram_layout_total_mb
```

They are read by the balancer (into backend metrics) and by the WebUI. An unreachable node is
excluded from the budget automatically: `available_vram_mb` drops to the local card, that is,
the "nominal capacity" does not survive a node failure.

A node with **zero free memory** is not taken into the layout at all: previously it
was forcibly given a minimum of 5% ("the node must not drop out"), and llama.cpp tried
to place layers there that the card cannot hold.

## 4. More than two machines

The code is not limited to two seats: the node list holds up to 16 addresses (`CPPWORKER_RPC_SERVERS`,
`host:port` separated by commas), the planner reads up to 32 devices, the proportions
are computed across all devices (master + N nodes) with a sum of 1.0, `main_gpu`
stays local. The device order for llama.cpp: local devices first, then the
nodes — in the same order in which `tensor_split` was computed.

## 5. Configuration from the WebUI

The backend parameters tab (GGUF) → the "Layer layout between machines" block:
a mode checkbox and a node list (one `host:port` per line). The values
go to `PUT /api/v1/cppworker/config/update` as `rpcEnabled` / `rpcServers`
and are **applied on the next model load** — the response carries
`rpc_reload_required`, and the WebUI tells the operator so directly. Changing the layout does not
silently reload the model: that would be minutes of downtime.

## 6. Who assigns donors and who brings the node up (R132/R133, 2026-10-16)

Section 5 described manual configuration of the node list. Starting with R133 the source of truth is
**the backend's role in the panel**, not `.env` and not the list in the form:

| What | Who decides | Where it is visible |
|---|---|---|
| who is a donor, who is the main one, who is self-sufficient | the operator in the panel (`PUT /api/v1/backends/{id}/role`) | the role selector in the backend row |
| the composition of the node list on the main backend | the balancer (assembles it from the roles, checks liveness, turns the layout off if there are none alive) | `rpcLayout` in `/api/v1/backends` |
| whether `rpc-server` is up ON THE DONOR ITSELF | the balancer sends the donor an order (`rpcServerEnabled=true`) | `rpcNode` in `/api/v1/backends` |
| whether the decision will survive container recreation | the worker's operator overrides file (takes priority over env) | `/api/v1/cppworker/config` |
| whether the balancer uses the node at all (R250) | the operator in the panel (`POST /api/v1/backends/{id}/enabled` with `{"enabled":false,"reason":"..."}`) | `disabled` / `disabledReason` / `disabledSince` in `/api/v1/backends`, the "In use" toggle in the node table and the "Excluded" badge in the backends list |

Why so. Before R133 the `donor` role worked one-way: the node list
was sent to the main backend, but the donor itself received nothing — its `rpc-server` was brought up
by hand (`CPPWORKER_RPC_SERVER_ENABLED=true` on the donor machine or a toggle on site).
On top of that, an `.env` with `CPPWORKER_RPC_ENABLE=true` and the donor's address brought the master back to the OLD node on every
container recreation, including a disabled one.
The operator saw "the role exists, the node does not work" and did not understand which parameter
was mandatory.

The rules that follow from this:

* `CPPWORKER_RPC_ENABLE` / `CPPWORKER_RPC_SERVERS` in `.env` are **not needed** for normal
  operation: they remain an emergency bootstrap for a stand without roles.
* The balancer manages the node **only** on backends with the `donor` role and shuts the node down
  **only where it brought it up itself**: the operator's manual toggle on the remaining
  backends is not trampled.
* "Up" and "available" are different statements. A node may be listening on the port INSIDE
  the container and yet be unreachable from the balancer (the port was not published when the
  container was started). That is why the status carries three fields (`running`, `listening`,
  `reachable`) and a reason in words; port publication cannot be changed from the panel —
  it is a property of the container launch.
* The reference endpoints (`GET /api/models`, `GET /api/models/effective`) **do not
  dial** the nodes at all (R132): the nodes' contribution is taken from the last
  collected snapshot, and the `vram_remote_source` field distinguishes `none` ("on this
  endpoint the nodes were not measured"), `cache` and `probed`. Dialing remains only on the
  decision path — there the donor's capacity is needed as the basis.

## 6b. "Do not use this backend" without stopping the node (R250, 2026-10-28)

Live complaint. To take a node out of service the operator had to `docker stop` it or drop
its role. Both are crude: a stopped node disappears from the panel (and it has to be tuned
while it is out), while dropping the role breaks the layout the node belongs to.

What it is now. `POST /api/v1/backends/{id}/enabled` with `{"enabled":false,"reason":"..."}`
sets a flag in the backend record (`Disabled` + `DisabledReason` + `DisabledAt`). The flag is
read by the **selector only**:

* a disabled backend is not a candidate (`expandCandidates`, `selectBackend`,
  `selectByResources`, `selectBackendExcluding` and the "manual" searches for a node under a
  model or for a relocation), it does not become the layout master and is not handed out by
  replication;
* it **stays** in `GET /api/v1/backends`, in the cluster snapshot (the `disabled` /
  `disabledSince` fields of `BackendMetrics`), in metrics and in the layout — the operator
  must see the node to switch it back on;
* requests already running on it are **not interrupted**: the flag affects selection only,
  and cutting a live generation because of a checkbox is worse than finishing it;
* the response also reports `loadedModels`: a model that is running on the node stays loaded,
  and the panel says so instead of leaving the operator with "I disabled it, why is the GPU
  busy".

Persistence is the same as for roles: the record is rebuilt by agent self-registration and
heartbeat, so the decision also lives in the sidecar
`runtime-overrides/backend-disabled.json`, and the balancer periodically returns the flag into
records that lost it. "Enabled" is a decision too, and it overrides stale "disabled" in
`state.json`. The cause of "no candidates" is now distinguishable in the log: all matching
backends excluded by the operator is a separate message, not silence.

## 7. The live cause of the crashes is a SECOND connection to the node (not versions)

Section 1 asserts the requirement "one commit + its own architecture", and it remains
a valid requirement for the deliverable. But **the observed crash was not explained by it**, and
this was verified by a direct experiment: the master and the node were switched to ONE image
(commit `1d2869c6e`, architectures `86;120`), and the crash repeated with the same
stack. The version explanation had to be withdrawn.

What it actually turned out to be. The line on which the process crashed:

```c
static void get_device_memory(...) {
    bool status = send_rpc_cmd(sock, RPC_CMD_GET_DEVICE_MEMORY, ...);
    RPC_STATUS_ASSERT(status);        // ggml-rpc.cpp:823
```

This is a **device memory request**, that is, our own background budget check,
and not a weight transfer:

```
load_tensors:   CPU_Mapped model buffer size = 10993.98 MiB
load_tensors:        CUDA0 model buffer size =  2116.47 MiB
load_tensors: RPC0[192.0.2.10:50052] model buffer size =  2246.03 MiB
ggml-rpc.cpp:823: Remote RPC server crashed or returned malformed response
```

The buffers are allocated, yet the very next request crashes — one made not by the loader.
`rpc-server` serves connections one at a time; while the master was loading the model (holding
its own connection), the background opened a second one and got an unparsed response. llama.cpp
answers this with `RPC_STATUS_ASSERT` -> `GGML_ABORT`, that is, a process crash,
while the node stays alive and silent (`RestartCount=0`).

**Rule.** While a model is loading or inference is running, the layout nodes
**must not be dialed** — neither for memory nor for anything else. Implemented in
`layoutBusy()` + `rpcDeviceSnapshot` (`internal/cppbackend/`): during operation
only the already collected snapshot is used, and if there is none — local devices without
RPC (the budget is more conservative, but the process stays alive). Verified by a test with a
listener that counts incoming connections: during load and during active
inference the count is zero.

**Practical conclusion.** A plain TCP connect to the node's port (the reachability check) is harmless:
it is a `connect`, not an RPC command. What is dangerous are exactly the calls that speak to
`rpc-server` over the protocol — device memory collection and everything that goes through the bridge.

## 8. Operational note: the CUDA build cache is evicted

Rebuilding the image after a Go-only change still compiles `ggml-cuda`
from scratch (observed: 29% and ~1.5 h), because the CUDA layers weighing tens of
gigabytes are evicted from the Docker cache. If the change does not touch C/CUDA, it is cheaper not to
rebuild the whole image; but `rpc-server` and `cppworker` are linked from one
tree, so there is no separate "fast" path for a Go change in this image.

## 9. Node admission by version (R115): a mismatched commit is not "let's try" but a refusal

The rule from section 1 stopped being advice and became a check in the code
(`internal/cppbackend/rpc_version_gate_r115.go`).

**Where the master learns the node's version from.** `rpc-server` does not expose HTTP, so
the **node's cppworker surface** is queried: `GET /api/v1/cppworker/config` ->
`config.llamaBuild` (the same value that is baked into the image as
`CPPWORKER_LLAMA_BUILD`). Timeout 1.5 s, cache 120 s: the version changes only when the
node image is updated, while the device snapshot is taken once every 30 s.

**What happens:**

| Node commit | Decision |
|---|---|
| matched the master's commit | the node is in the layout (`version_match`) |
| did not match | the node is EXCLUDED, the log shows `version_mismatch: <node> != <master>` |
| unknown (the node is unreachable or did not return the field) | excluded; lifted by an explicit `CPPWORKER_RPC_ALLOW_VERSION_UNKNOWN=true` |
| the master itself does not know its version | no judgement (otherwise all nodes would have been refused at once) |

Important: **a mismatched commit is never admitted**, even with
`ALLOW_VERSION_UNKNOWN` - the exception is made only for "unknown".

**The node's port** is set by `CPPWORKER_RPC_NODE_HTTP_PORT` (default 18092), because
the node's cppworker surface may listen on a non-standard port.

**Why this.** Previously a commit mismatch led to `GGML_ABORT` - a crash of the
master while the node was alive and silent (section 1). Now such a node simply does not
take part in the layout, and the reason is visible in the log as a single line. The order of the remaining
nodes is preserved: it is also the device order for `tensor_split`.


## 10. Is there a gain from the layout? A measurement on qwen3.8 (R231, 2026-10-24)

The rules above answer "can the layers be laid out". This section answers "is it worth it": a live
measurement on one model, an identical request and two modes (`rpc-layout=off`/`on`).

Bench setup: master 8 GB VRAM + donor 8 GB VRAM, link ≈8.9 Gbit/s, model
`Qwen3.8-27B-UD-Q4_K_M` (15.33 GiB), `ctx=16384`, KV `q8_0`, a 96-token request.

| Metric | off | on | Δ |
|---|---|---|---|
| Layout | CUDA0 20/66, CPU 46 | CUDA0 26 + RPC0 22 = 48/66, CPU 18 | +28 layers on GPU |
| Load | 278 s | 285 s | +2.5 % |
| 96 tokens | 126–147 s | 45–49 s | — |
| Speed | 0.65–0.77 tok/s | 1.95–2.15 tok/s | **≈ ×2.9** |
| Latency | 136 s | **46 s** | **−66 %** |

Conclusion: on a fast link the layout gives **≈2.9× on generation** with almost no
increase in load time; on a slow link (hundreds of Mbit/s) a regression is possible, so
measure the link before enabling it (the "Measure" button / `POST .../measure-link`).
When the node is turned off or lost, the guard falls back to local loading (section 2).

Methodology, reproduction and limitations — [rpc-benchmark-qwen38.md](../features/rpc-benchmark-qwen38.md);
script — `scripts/bench-rpc-qwen38.ps1 -Mode both`.
