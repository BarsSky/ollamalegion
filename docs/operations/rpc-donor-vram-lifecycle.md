# VRAM on layout nodes: when it is released and what stays

This document answers the question "is the model unloaded on the donor when the
master fails, when a load fails or when a load is cancelled". Below: what exactly
holds memory on the donor, in which scenarios it is released and in which it is
**not** released automatically.

## Who holds memory on the donor

The donor knows nothing about the model: it runs `rpc-server` (the same cppworker
image, a child process of the supervisor, `CPPWORKER_RPC_SERVER_ENABLED=true`). The
master connects to it as the ggml device `RPC0` and loads part of the layers into
it. For those layers `rpc-server` allocates buffers (`RPC_CMD_ALLOC_BUFFER`) and
keeps them in the `rpc_server` object (the `buffers` set).

They are released in exactly two places: the `RPC_CMD_FREE_BUFFER` command from the
master, or the `rpc_server` destructor - when `recv()` on the socket returns an
error, that is, when the connection is closed (including by the death of the
master).

Releasing a buffer in llama.cpp is `cudaFree` (`ggml-rpc.cpp`:
`rpc_server::~rpc_server` -> `ggml_backend_buffer_free` ->
`~ggml_backend_cuda_buffer_context`), so the memory goes back to the driver, not to
the shared pool cache. Small scratch compute buffers live in the CUDA pool of the
`rpc-server` process and may be visible in `nvidia-smi` between loads - they are not
weights.

## When donor VRAM is released

| Scenario | What happens | Donor VRAM |
| --- | --- | --- |
| Normal model unload, "Apply profile", idle-unload | master does `llama_model_free` -> `FREE_BUFFER` | released immediately |
| Load failed (missing file, OOM, memfit refusal, RPC error) | the bridge frees the model/context on its error path | released immediately |
| Load cancelled (cancel/unload `force=true`/client disconnect) | R215: llama.cpp progress-callback returns false and weights reading stops; llama.cpp frees the partially loaded model | **within one tensor**, see below |
| Master died/killed (SIGKILL, SIGABRT, container restart) | the kernel closes the socket, `rpc-server` leaves the `recv` loop and runs the destructor | released immediately |
| `rpc-server` died on the donor (OOM, segfault) | the supervisor restarts the process; memory dies with the process | released by process death |
| Connection lost WITHOUT the socket being closed (master power loss, unplugged cable) | before R214: `recv()` hangs without timeouts | **up to ~1.5-2 minutes**, see below |
| Node hung (for example a stuck CUDA kernel) | the socket is alive, keepalive is answered by the kernel | **not released automatically**, see below |

### What R214 changed

`rpc-server` and the layout client now set `SO_KEEPALIVE` with aggressive parameters
(60 s idle, 3 probes of 15 s): a half-dead session is detected in ~1.5-2 minutes, the
destructor frees the weights and the node accepts the next client again. Before the
fix such a session could hang for tens of minutes, and the node would refuse new
loads (it serves one connection at a time), which looked like "the layout broke, the
layers went to the local card".

The patch lives in `docker/cppworker/patches/0001-rpc-tcp-keepalive.patch` and is
applied at build time to the `c/llama.cpp` submodule (pin - `CPPWORKER_LLAMA_BUILD`).

### What R215 changed

1. **Cancellation no longer waits for the end of the load, it stops the load.** The
   bridge passes a `progress_callback` to llama.cpp; if a cancel request arrives
   (`unload?force=true`), the callback returns `false`, weights reading stops, and
   `llama_model_load_from_file` frees the partially loaded model itself (including
   buffers on the nodes). Measured on the stand (gemma-4-E4B-it-Q4_K_M, 4.2 GB):
   cancelling with `force=true` returned `{"status":"load_cancelled"}` in **1.2 s**,
   the log showed `load cancelled at 52%`, VRAM went 3667 MiB -> 1242 MiB (baseline).
2. **Progress is visible to the operator.** Through the same callback the bridge
   records the percentage of loaded weights; it is served in
   `/api/models/load/progress` (`loadingProgressPct`) and in the list of loading
   models. The percentage is monotonic (llama.cpp counts it separately per buffer
   group, and without normalisation the UI would see it go backwards).
3. **A busy node is waited for, not bypassed.** The node readiness gate waits
   `CPPWORKER_RPC_READY_WAIT_SEC` (default 30 s, 0 = do not wait, maximum 600 s)
   instead of 1.2 s: a node that is freeing itself after a master failure has time to
   answer and the layout is not lost.
4. **A hung node is named out loud.** Two failed gate attempts in a row with a
   node reachable over TCP produce `rpc_layout.stuck_suspected=true` in `/api/models`
   and an ERROR in the log: this is the only case that needs a manual restart of the
   donor `rpc-server`.

### What R221 changed: the node disappeared mid-session

Before R221 the death of a node with an already loaded model crashed the master: the
client thread of the RPC dispatcher called `GGML_ABORT` on the first exchange
failure (`ggml-rpc.cpp:567/570 Remote RPC server crashed`), the container restarted,
and the client got a broken stream in the middle of a response. The patch
`docker/cppworker/patches/0003-rpc-session-loss.patch`:

1. marks the session as lost on an exchange failure (log:
   `RPC session lost, remote endpoint is gone`) instead of killing the process;
2. the command queue keeps completing `completion` - otherwise the caller would hang
   forever in `future.wait()`, that is, the abort would turn into a deadlock;
3. `graph_compute` returns `GGML_STATUS_FAILED`, so `llama_decode` returns an error:
   the client gets an explicit answer instead of garbage from zero logits;
4. a handshake failure also became an error return (a node that accepted TCP and
   died before `HELLO` no longer crashes the master when the device is added).

The detector works on the fact of an I/O failure, without polling or timers: silence
on the network means nothing by itself (a long prompt), while a broken socket is
visible immediately.

What happens next (R224): an inference failure is recognised as a lost session - the
client gets `rpc_session_lost` with the plain text "this is not OOM and not n_ctx",
and the master unloads the model itself. The next request loads it again: the layout
is assembled from scratch and the node is checked with a TCP probe before connecting
(R218). Auto-unload can be disabled: `CPPWORKER_RPC_SESSION_AUTO_UNLOAD=0`
(then the model stays loaded and the operator unloads it).

Live check: `scripts/probe-rpc-session-recovery-r224.js` - kills the node in the
middle of generation and requires: the master is alive, the diagnosis is honest, the
model is unloaded, and after the node returns the next request works.

## A donor does not keep its own model copies (R304)

The `donor` role is not "a node that also helps the master". It is a node whose VRAM
is **handed over** to the layers of the master's model through `rpc-server`. A donor
therefore cannot hold a model of its own: it takes exactly the gigabytes the donor
was assigned for and the master's load fails on memory.

**What was wrong before R304.** The role lived only in the balancer and no backend
selection path checked it: the only filter for "this node does not hold the model
whole" was the logical-backend worker (`balancing.backendGroups[]`), which is not
configured in a normal deployment. Observed live: a load went to a node that had just
been made a donor; a donor with a leftover copy served requests while the master was
loading layers into its memory ("the second agent tore the model away from the
first"); an n_ctx reload picked the donor as the target and wasted its VRAM; session
evacuation from a failed node landed on the donor.

**What now.** The invariant is expressed in one predicate on the whole hot path
(`cannotHostModelR304`: layout worker **or** donor) and is enforced at three
independent levels:

| Level | What it does | Where it is visible |
| --- | --- | --- |
| Balancer (selection) | the donor is excluded from every executor-selection path: serving a request, placement over reload, loading from scratch by the file on disk, window reload, session evacuation, replication | steward test `TestR304_SelectionSitesAskSharedPredicate` - a selection path that forgets the predicate breaks the test build |
| cppworker (refusal) | the worker knows its role (the balancer sends it together with the order about its own node) and answers **409 `donor_cannot_host_models`** to a load/import | `role`, `donor`, `can_load_own_models`, `donor_reason`, `own_copies` in `GET /api/models` |
| WebUI (mark) | a "donor (master: X)" badge, load buttons disabled with an explanation, the action itself refuses | `GET /api/v1/backends`: `donor`, `donorFor`, `canLoadOwnModels`, `ownCopies`, `donorNote` |

If the donor does end up with a copy (left over from a previous role, loaded before
this fix, or the operator enabled the exception), it is not "silently fixed": the
balancer logs a warning with the copy name (at most once per 5 minutes per node) and
reports it in `ownCopies`. Unloading someone else's work in the background is not
allowed - it would break a running stream; the decision belongs to the operator.

**Exception.** `LB_DONOR_ALLOW_OWN_COPIES=1` (hot setting) restores the old behaviour
wholly, through the same predicate: needed when a node is made a donor while streams
are still running on it. Default is `0`.

**How to check.** `GET /api/v1/backends` -> the donor has `donor: true`,
`donorFor.backendId` = master, `canLoadOwnModels: false`, `ownCopies: []`; a load
request to that node is refused with `donor_cannot_host_models`. On the worker
itself: `GET /api/models` -> `role: "donor"`, `can_load_own_models: false`;
`POST /api/models/load` -> 409.

## Limitations you should know about

1. **Model initialisation is not interrupted and shows no progress.** Measured on
   the stand: the first progress callback arrived **70 s** after the load started
   (gemma 4.2 GB on a bind-mount) - all that time llama.cpp parses the GGUF and
   creates tensors. The callback is only called in the weights loading phase,
   therefore: a cancel in that phase waits for the first callback (tens of seconds),
   and the percentage only appears towards the end. A cancel in the weights phase
   takes ~1.2 s.
2. **Keepalive does not cure a hung node.** If the donor process is alive but does
   not compute, the socket answers probes and the session stays. The sign is: the
   master sees the node as reachable but gets no device from it (`rpcDeviceSnapshot`
   is empty with `reachable=true`), and `/api/models` reports
   `rpc_layout.stuck_suspected=true`. The cure is a node restart: turn
   `rpcServerEnabled` off and on again in the donor worker settings (or restart the
   container).
3. **One client per node.** While a dead master's session hangs, a new master gets a
   refusal. R214 shortened that window to ~1.5-2 minutes and R215 lets the gate wait
   it out, but the "one client per node" design cannot be bypassed.
4. **The RAM part is not on the donor.** Layers that did not fit on the GPU live in
   the master's RAM and have nothing to do with the donor.

## Reproducing the measurement

`scripts/test-cancel-inflight-r215.js` - starts a load in a disposable container,
waits for the first progress callback, checks `loadingProgressPct` in the API,
cancels the load with `unload?force=true` and compares VRAM before/after. The script
returns 0 only when the cancellation is confirmed by the log and the memory is
released.

## How to check

- Donor: `docker exec <container> nvidia-smi` - memory held by the `rpc-server`
  process while no model is loaded on the master means a hanging session.
- Node: `GET /api/v1/cppworker/rpc-server/log?lines=200` (or in the WebUI, worker
  settings page) - process state, `restarts`, `lastExit`, server output.
- Master: `GET /api/models` - `rpc_servers_active`/`rpc_server` show whether the node
  is up and listening; `layout_degraded_reason` explains why the layers did not go to
  the node.
