# The link between layout nodes: how it is determined and where it is set (R166, 2026-10-17)

The document answers one question: **what bandwidth the balancer assumes
when it proposes a layer layout across hosts, and who determines it.**

## 1. Why this is a key parameter

When layers are laid out (`layer split`), every token crosses the boundary between machines:
the activations go to the remote node and come back. From the channel bandwidth the
following is computed:

* traffic per token (`hidden_size × bytes` for layer; `× layers` for row);
* traffic per prefill (`prefill_tokens ×` the same);
* the overhead share of token time (`tokenOverheadPercent`).

The formulas are in `internal/composition/traffic.go`. If the channel is underestimated by a factor of 10 (for example,
assuming 1 Gbps on a 10 Gbps link), the dry run will show “the network cannot keep up” and
the composition will be rejected for nothing.

## 2. The ladder of sources (top to bottom)

The resolver is `internal/balancer/link_bandwidth_r166.go`, function `LinkBandwidthInfo`.

| Priority | Source (`source`) | Where it comes from | When it is used |
|---|---|---|---|
| 1 | `request` | request body `bandwidthMBps` (**MB/s**) | the operator set the value in the WebUI or via the API |
| 2 | `auto` | **the balancer's auto-measurement**: downloading 8 MiB from the node's `/api/rpc-link-probe`, Mbps | the measurement is fresh (younger than 3 periods), the node has the `main`/`donor` role |
| 3 | `env` | `LB_RPC_LINK_MBPS` (Mbps) | there is no auto-measurement, but the operator supplied a number from `scripts/measure-network.ps1` |
| 4 | `default` | 112 MB/s (1 Gbps, the historical value from §3) | nothing has been measured |

Important properties:

* **the auto-measurement outranks `.env`**: it measures the same path that the layers take
  (`cppworker ↔ cppworker`, the container network), whereas `.env` is the host↔host path;
* with several participants the **slowest** channel is taken: the layout is synchronous,
  and it sets the overall pace;
* **a stale measurement is not used** (an honest `env`/`default` is better than an old
  figure passed off as a measurement);
* nodes without the `main`/`donor` role are not involved — they have no `rpc-server`.

The API response (and the panel) always contains the `bandwidth` field:

```json
{"mbps": 1064, "mbps_bits": 8515, "source": "auto",
 "backendId": "cppworker-gpu-bundled-agent",
 "detail": "auto-measurement by the balancer to node … (path cppworker↔cppworker, same as the layers themselves)"}
```

## 3. Automatic determination at initialization

### Balancer

* `StartRPCLinkMeasurer` (`internal/balancer/rpc_link_measure_r118.go`) starts at
  startup: **the first measurement after 30 s** (nodes have time to register), then once every
  `LB_RPC_LINK_MEASURE_SEC` (default 900 s; `0` disables it).
* Measurements are made only to nodes with the `main`/`donor` role.
* A failed measurement **does not overwrite** the previous successful figure.

### Backend (cppworker)

`/api/info` returns a `net` block (R166, `cmd/cppworker/netlink_r166.go`):

```json
"net": {"interface": "eth0", "mtu": 1500, "source": "detected",
        "note": "MTU 1500: jumbo is not enabled — fragmentation possible on large packets"}
```

This is **detection, not measurement**: in a container the interface is usually `veth`, and its
`speed` from `/sys` is either absent or equal to the speed of the host link. Therefore the field
is marked `source=detected` and does not substitute for the balancer's measurement. In the panel these numbers differ
in meaning, and they must not be confused.

## 4. Where to set it in the WebUI

Page **“Backends” → “Backend merge”**, block **“Inter-node link”**:

* the **Mbps** field — an explicit operator value (saved in the browser);
* **an empty field = “auto”**: the balancer will substitute its own measurement;
* the **“Measure”** button — POST `/api/v1/rpc/link/measure`, measure right now;
* the hint under the field shows what actually went into the traffic estimate and with which
  source: “used in traffic estimate: 8515 Mbps (1064 MB/s), source: balancer
  auto-measurement”.

## 5. API

| Method and path | Purpose |
|---|---|
| `POST /api/v1/rpc/link/measure` | measure right now. Body `{}` — all layout participants; `{"backendId": "..."}` — a single node. Response: `results[]` (per node) + `effective` |
| `POST /api/v1/placement/validate` | dry run of the composition; accepts `bandwidthMBps` (MB/s), returns `bandwidth` with its source |
| `POST /api/v1/placement/plan` | the proposed plan; it also accepts `bandwidthMBps` and returns `bandwidth` |
| `GET /api/v1/backends` | in the backend record: `rpcLink` (the operator's measurement from `.env`) and `rpcLinkAuto` (the balancer's measurement) — **separately and with their sources** |
| `GET /api/info` (worker) | the `net` block: interface, MTU, link speed (if the kernel reports it) |

## 6. Configuration

| Variable | Meaning | Default |
|---|---|---|
| `LB_RPC_LINK_MEASURE_SEC` | auto-measurement period, `0` disables it | `900` |
| `LB_RPC_LINK_MBPS` | operator measurement (**Mbps**), fallback | empty |
| `LB_RPC_LINK_MEASURED_AT`, `LB_RPC_LINK_SOURCE` | annotation for the operator measurement | empty |

The measured volume is 8 MiB per node (`rpcLinkProbeBytes`). The `/api/rpc-link-probe`
handler allows 64 KiB…64 MiB.

## 7. Reference points: what a fast link gives

For a model of the 180B MoE class (Qwen3.8-Flash-Next, 48 layers, a stream of 10 240 because of
hyper-connections):

| Quantity | 1 Gbps | 10 Gbps |
|---|---|---|
| Transferring 24 GB of weights to a remote GPU | ~3.6 min | ~22 s |
| Overhead per token (layer) | 183 µs | 19 µs |
| Prefill of 512 tokens | 94 ms | 10 ms |

That is, at 10 Gbps the network stops being the bottleneck; memory (weights/KV) and
GPU VRAM remain. A detailed calculation for two A10s is in `docs/plans/2026-10-17-qwen38-flash-next-a10.md`.

## 8. If something is wrong

| Symptom | Cause | What to do |
|---|---|---|
| `409 no layout nodes` on `/rpc/link/measure` | no backend has the `main`/`donor` role | assign the roles (WebUI → “Backend merge”, or `PUT /api/v1/backends/{id}/role`) |
| `source: default` in the response | the auto-measurement never ran, `LB_RPC_LINK_MBPS` is empty | press “Measure”; check that `/api/rpc-link-probe` answers on the node (cppworker image r118-probe1+) |
| The auto-measurement is noticeably lower than the host one | the balancer's measurement goes through Docker Desktop container networks (NAT/WSL2) | this is expected: the layers take the same path, so go by the auto-measurement, not by host↔host |
| The measurement “goes stale” (falls back to `env`/`default`) | the freshness filter: anything older than 3 periods is not used | reduce `LB_RPC_LINK_MEASURE_SEC` or press “Measure” |
| MTU 1500 at 10 Gbps | jumbo is not enabled | enable jumbo (9000) on both ends and on the switch; check with `ping -f -l 8972` |
